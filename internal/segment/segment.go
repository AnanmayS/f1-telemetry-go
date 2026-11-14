// Package segment reads and writes ghostlap segment files: one immutable,
// CRC-protected, mmap-friendly file per session. The byte layout is specified
// in docs/FORMAT.md.
package segment

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	"github.com/AnanmayS/f1-telemetry-go/internal/encoding"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

const (
	Magic   = "GLAP"
	Version = 1

	headerFixed   = 4 + 2 + 4 + 8 + 4 // magic, version, session_key, epoch, meta_len
	footerSize    = 8 + 4 + 4 + 4     // index_offset, index_length, crc32(index), magic
	indexEntry    = 1 + 2 + 1 + 8 + 4
	lapRecord     = 1 + 1 + 2 + 8 + 8 + 4*4 + 4
	blockOverhead = 1 + 4 + 4 // encoding, count, crc32
)

var (
	ErrCorrupt  = errors.New("segment: corrupt")
	ErrVersion  = errors.New("segment: unsupported format version")
	ErrNotFound = errors.New("segment: lap not found")
	ErrClosed   = errors.New("segment: closed")
)

// Meta is the small JSON document in the header.
type Meta struct {
	Session        model.Session  `json:"session"`
	Drivers        []model.Driver `json:"drivers"`
	CircuitLengthM float64        `json:"circuit_length_m"`
	Samples        int64          `json:"samples"`        // samples stored (inside laps)
	RawSamples     int64          `json:"raw_samples"`    // car_data rows fetched (includes time outside laps)
	RawJSONBytes   int64          `json:"raw_json_bytes"` // size of every raw API response for the session
}

// Build serialises a session. laps may be in any order; duplicate
// (driver, lap) pairs are an error.
func Build(meta Meta, laps []*model.LapData) ([]byte, error) {
	sorted := append([]*model.LapData(nil), laps...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Lap, sorted[j].Lap
		if a.Driver != b.Driver {
			return a.Driver < b.Driver
		}
		return a.Number < b.Number
	})
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	buf := make([]byte, 0, 1<<20)
	buf = append(buf, Magic...)
	buf = le.AppendUint16(buf, Version)
	buf = le.AppendUint32(buf, uint32(meta.Session.Key))
	buf = le.AppendUint64(buf, uint64(meta.Session.Start.UnixMilli()))
	buf = le.AppendUint32(buf, uint32(len(metaJSON)))
	buf = append(buf, metaJSON...)
	buf = le.AppendUint32(buf, crc32.ChecksumIEEE(buf))

	index := le.AppendUint32(nil, uint32(len(sorted))*uint32(model.NumChannels))
	lapTable := le.AppendUint32(nil, uint32(len(sorted)))
	for i, ld := range sorted {
		l := ld.Lap
		if l.Driver < 0 || l.Driver > 255 || l.Number < 0 || l.Number > 65535 {
			return nil, fmt.Errorf("segment: driver %d lap %d out of range", l.Driver, l.Number)
		}
		if i > 0 && sorted[i-1].Lap.Driver == l.Driver && sorted[i-1].Lap.Number == l.Number {
			return nil, fmt.Errorf("segment: duplicate driver %d lap %d", l.Driver, l.Number)
		}
		n := len(ld.Values[model.ChT])
		if n > encoding.MaxCount {
			return nil, fmt.Errorf("segment: driver %d lap %d has %d samples", l.Driver, l.Number, n)
		}
		for c := model.Channel(0); c < model.NumChannels; c++ {
			if len(ld.Values[c]) != n || (ld.Valid[c] != nil && len(ld.Valid[c]) != n) {
				return nil, fmt.Errorf("segment: driver %d lap %d channel %s length mismatch", l.Driver, l.Number, c)
			}
			enc, payload := encoding.EncodeBlock(c.Kind(), ld.Values[c], ld.Valid[c])
			off := len(buf)
			buf = append(buf, enc)
			buf = le.AppendUint32(buf, uint32(n))
			buf = append(buf, payload...)
			buf = le.AppendUint32(buf, crc32.ChecksumIEEE(buf[off:]))
			index = append(index, byte(l.Driver))
			index = le.AppendUint16(index, uint16(l.Number))
			index = append(index, byte(c))
			index = le.AppendUint64(index, uint64(off))
			index = le.AppendUint32(index, uint32(len(buf)-off))
		}
		lapTable = append(lapTable, byte(l.Driver), l.Flags)
		lapTable = le.AppendUint16(lapTable, uint16(l.Number))
		lapTable = le.AppendUint64(lapTable, uint64(l.StartMs))
		lapTable = le.AppendUint64(lapTable, uint64(l.EndMs))
		lapTable = le.AppendUint32(lapTable, uint32(l.OfficialMs))
		for _, s := range l.SectorMs {
			lapTable = le.AppendUint32(lapTable, uint32(s))
		}
		lapTable = le.AppendUint32(lapTable, uint32(n))
	}
	index = append(index, lapTable...)
	indexOff := len(buf)
	buf = append(buf, index...)
	buf = le.AppendUint64(buf, uint64(indexOff))
	buf = le.AppendUint32(buf, uint32(len(index)))
	buf = le.AppendUint32(buf, crc32.ChecksumIEEE(index))
	buf = append(buf, Magic...)
	return buf, nil
}

// WriteFile writes data durably: temp file, fsync, atomic rename, fsync dir.
func WriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

type blockRef struct {
	key    uint32 // driver<<24 | lap<<8 | channel
	offset uint64
	length uint32
}

func blockKey(driver, lap int, ch model.Channel) uint32 {
	return uint32(driver)<<24 | uint32(lap)<<8 | uint32(ch)
}

// Segment is an open, immutable segment. All methods are safe for concurrent use.
type Segment struct {
	Meta       Meta
	SessionKey int
	EpochMs    int64

	data   []byte
	unmap  func() error
	blocks []blockRef  // sorted by key
	laps   []model.Lap // sorted by (driver, number)

	refs atomic.Int64 // starts at 1 (the opener's reference)
}

// Open maps the file at path and validates its header, footer and index.
func Open(path string) (*Segment, error) {
	data, unmap, err := mapFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		unmap()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.unmap = unmap
	return s, nil
}

// Parse validates data as a segment. The returned segment aliases data.
func Parse(data []byte) (*Segment, error) {
	le := binary.LittleEndian
	if len(data) < headerFixed+4+footerSize {
		return nil, fmt.Errorf("%w: too short", ErrCorrupt)
	}
	if string(data[:4]) != Magic || string(data[len(data)-4:]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	if v := le.Uint16(data[4:]); v != Version {
		return nil, fmt.Errorf("%w: %d", ErrVersion, v)
	}
	s := &Segment{data: data}
	s.SessionKey = int(le.Uint32(data[6:]))
	s.EpochMs = int64(le.Uint64(data[10:]))
	metaLen := uint64(le.Uint32(data[18:]))
	headerEnd := uint64(headerFixed) + metaLen
	if headerEnd+4 > uint64(len(data)-footerSize) {
		return nil, fmt.Errorf("%w: header overruns file", ErrCorrupt)
	}
	if crc32.ChecksumIEEE(data[:headerEnd]) != le.Uint32(data[headerEnd:]) {
		return nil, fmt.Errorf("%w: header checksum", ErrCorrupt)
	}
	if err := json.Unmarshal(data[headerFixed:headerEnd], &s.Meta); err != nil {
		return nil, fmt.Errorf("%w: metadata: %v", ErrCorrupt, err)
	}
	blocksStart := headerEnd + 4

	foot := data[len(data)-footerSize:]
	indexOff, indexLen := le.Uint64(foot), uint64(le.Uint32(foot[8:]))
	if indexOff < blocksStart || indexOff > uint64(len(data)-footerSize) || indexOff+indexLen != uint64(len(data)-footerSize) {
		return nil, fmt.Errorf("%w: index bounds", ErrCorrupt)
	}
	index := data[indexOff : indexOff+indexLen]
	if crc32.ChecksumIEEE(index) != le.Uint32(foot[12:]) {
		return nil, fmt.Errorf("%w: index checksum", ErrCorrupt)
	}

	if len(index) < 4 {
		return nil, fmt.Errorf("%w: index truncated", ErrCorrupt)
	}
	nBlocks := uint64(le.Uint32(index))
	index = index[4:]
	if nBlocks*indexEntry > uint64(len(index)) {
		return nil, fmt.Errorf("%w: index truncated", ErrCorrupt)
	}
	s.blocks = make([]blockRef, nBlocks)
	for i := range s.blocks {
		e := index[i*indexEntry:]
		b := blockRef{
			key:    blockKey(int(e[0]), int(le.Uint16(e[1:])), model.Channel(e[3])),
			offset: le.Uint64(e[4:]),
			length: le.Uint32(e[12:]),
		}
		if b.length < blockOverhead || b.offset < blocksStart || b.offset+uint64(b.length) > indexOff {
			return nil, fmt.Errorf("%w: block %d bounds", ErrCorrupt, i)
		}
		if i > 0 && s.blocks[i-1].key >= b.key {
			return nil, fmt.Errorf("%w: index not sorted", ErrCorrupt)
		}
		s.blocks[i] = b
	}
	index = index[nBlocks*indexEntry:]
	if len(index) < 4 {
		return nil, fmt.Errorf("%w: lap table truncated", ErrCorrupt)
	}
	nLaps := uint64(le.Uint32(index))
	index = index[4:]
	if nLaps*lapRecord != uint64(len(index)) {
		return nil, fmt.Errorf("%w: lap table size", ErrCorrupt)
	}
	s.laps = make([]model.Lap, nLaps)
	for i := range s.laps {
		r := index[i*lapRecord:]
		l := model.Lap{
			Driver:     int(r[0]),
			Flags:      r[1],
			Number:     int(le.Uint16(r[2:])),
			StartMs:    int64(le.Uint64(r[4:])),
			EndMs:      int64(le.Uint64(r[12:])),
			OfficialMs: int32(le.Uint32(r[20:])),
			Samples:    int(le.Uint32(r[36:])),
		}
		for k := range l.SectorMs {
			l.SectorMs[k] = int32(le.Uint32(r[24+4*k:]))
		}
		if i > 0 {
			p := s.laps[i-1]
			if p.Driver > l.Driver || (p.Driver == l.Driver && p.Number >= l.Number) {
				return nil, fmt.Errorf("%w: lap table not sorted", ErrCorrupt)
			}
		}
		s.laps[i] = l
	}
	s.refs.Store(1)
	return s, nil
}

// Acquire takes a reference; it fails once the segment has been fully released.
func (s *Segment) Acquire() bool {
	for {
		n := s.refs.Load()
		if n <= 0 {
			return false
		}
		if s.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Release drops a reference and unmaps the file when the last one is gone.
func (s *Segment) Release() {
	if s.refs.Add(-1) == 0 && s.unmap != nil {
		s.unmap()
	}
}

// Close releases the opener's reference.
func (s *Segment) Close() { s.Release() }

// Size is the file size in bytes.
func (s *Segment) Size() int { return len(s.data) }

// AllLaps returns the lap table sorted by (driver, lap). Do not modify.
func (s *Segment) AllLaps() []model.Lap { return s.laps }

// Laps returns one driver's laps in lap order. Do not modify.
func (s *Segment) Laps(driver int) []model.Lap {
	lo := sort.Search(len(s.laps), func(i int) bool { return s.laps[i].Driver >= driver })
	hi := sort.Search(len(s.laps), func(i int) bool { return s.laps[i].Driver > driver })
	return s.laps[lo:hi]
}

// Lap looks up one lap-table row.
func (s *Segment) Lap(driver, lap int) (model.Lap, bool) {
	i := sort.Search(len(s.laps), func(i int) bool {
		l := s.laps[i]
		return l.Driver > driver || (l.Driver == driver && l.Number >= lap)
	})
	if i < len(s.laps) && s.laps[i].Driver == driver && s.laps[i].Number == lap {
		return s.laps[i], true
	}
	return model.Lap{}, false
}

// ReadChannel decodes one block. It touches only that block's bytes.
func (s *Segment) ReadChannel(driver, lap int, ch model.Channel) ([]int64, []bool, error) {
	if driver < 0 || driver > 255 || lap < 0 || lap > 65535 || ch >= model.NumChannels {
		return nil, nil, ErrNotFound
	}
	if !s.Acquire() {
		return nil, nil, ErrClosed
	}
	defer s.Release()
	key := blockKey(driver, lap, ch)
	i := sort.Search(len(s.blocks), func(i int) bool { return s.blocks[i].key >= key })
	if i == len(s.blocks) || s.blocks[i].key != key {
		return nil, nil, ErrNotFound
	}
	return s.decode(s.blocks[i])
}

func (s *Segment) decode(b blockRef) ([]int64, []bool, error) {
	raw := s.data[b.offset : b.offset+uint64(b.length)]
	body, sum := raw[:len(raw)-4], binary.LittleEndian.Uint32(raw[len(raw)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return nil, nil, fmt.Errorf("%w: block checksum at offset %d", ErrCorrupt, b.offset)
	}
	count := binary.LittleEndian.Uint32(body[1:])
	if count > encoding.MaxCount {
		return nil, nil, fmt.Errorf("%w: block count %d", ErrCorrupt, count)
	}
	v, valid, err := encoding.DecodeBlock(body[0], int(count), body[5:])
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return v, valid, nil
}

// ReadLap decodes every channel of one lap.
func (s *Segment) ReadLap(driver, lap int) (*model.LapData, error) {
	l, ok := s.Lap(driver, lap)
	if !ok {
		return nil, ErrNotFound
	}
	ld := &model.LapData{Lap: l}
	for c := model.Channel(0); c < model.NumChannels; c++ {
		v, valid, err := s.ReadChannel(driver, lap, c)
		if err != nil {
			return nil, fmt.Errorf("driver %d lap %d channel %s: %w", driver, lap, c, err)
		}
		if len(v) != l.Samples {
			return nil, fmt.Errorf("%w: driver %d lap %d channel %s has %d samples, lap table says %d", ErrCorrupt, driver, lap, c, len(v), l.Samples)
		}
		ld.Values[c], ld.Valid[c] = v, valid
	}
	return ld, nil
}

// BlockInfo describes one block for tooling.
type BlockInfo struct {
	Driver, Lap int
	Channel     model.Channel
	Offset      uint64
	Length      uint32
	Encoding    byte
	Count       uint32
}

// Blocks lists every block in index order.
func (s *Segment) Blocks() []BlockInfo {
	out := make([]BlockInfo, len(s.blocks))
	for i, b := range s.blocks {
		out[i] = BlockInfo{
			Driver: int(b.key >> 24), Lap: int(b.key >> 8 & 0xffff), Channel: model.Channel(b.key & 0xff),
			Offset: b.offset, Length: b.length,
			Encoding: s.data[b.offset], Count: binary.LittleEndian.Uint32(s.data[b.offset+1:]),
		}
	}
	return out
}

// Verify decodes every block and returns the first error found.
func (s *Segment) Verify() error {
	for _, b := range s.blocks {
		if _, _, err := s.decode(b); err != nil {
			return fmt.Errorf("driver %d lap %d channel %s: %w", b.key>>24, b.key>>8&0xffff, model.Channel(b.key&0xff), err)
		}
	}
	return nil
}
