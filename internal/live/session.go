package live

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
	"github.com/AnanmayS/f1-telemetry-go/internal/wal"
)

// Session is one live session: a WAL plus the memtable built from it.
// Safe for concurrent use.
type Session struct {
	meta    segment.Meta // Session and Drivers
	walPath string
	w       *wal.Writer

	mu      sync.Mutex
	lastSeq uint64 // highest event sequence applied
	// recSeq[i] is the event sequence held by the (i+1)-th record appended
	// since Open; with the WAL's durable record count it gives the highest
	// durable event sequence.
	recSeq    []uint64
	recovered uint64 // lastSeq right after WAL replay; all of it is on disk
	car       map[int][]laps.Car
	loc       map[int][]laps.Loc
	timing    map[int]map[int]laps.Timing
	version   uint64 // bumped by every applied event
}

// sourceMeta keeps what describes the source (session, drivers, raw sizes)
// and drops what Build derives (circuit length, stored sample count).
func sourceMeta(m segment.Meta) segment.Meta {
	return segment.Meta{Session: m.Session, Drivers: m.Drivers, RawSamples: m.RawSamples, RawJSONBytes: m.RawJSONBytes}
}

// WALPath is where a session's log lives.
func WALPath(liveDir string, sessionKey int) string {
	return filepath.Join(liveDir, fmt.Sprintf("%d.wal", sessionKey))
}

// Open starts or resumes a live session. An existing WAL is replayed (its
// torn tail, if any, is cut off) and its metadata record wins over meta.
func Open(liveDir string, meta segment.Meta, syncEvery time.Duration) (*Session, error) {
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		return nil, err
	}
	s := &Session{
		meta: sourceMeta(meta), walPath: WALPath(liveDir, meta.Session.Key),
		car: map[int][]laps.Car{}, loc: map[int][]laps.Loc{}, timing: map[int]map[int]laps.Timing{},
	}
	sawMeta := false
	records, _, err := wal.Replay(s.walPath, func(p []byte) error {
		if p[0] == recMeta {
			var m segment.Meta
			if err := json.Unmarshal(p[1:], &m); err != nil {
				return fmt.Errorf("live: metadata record: %w", err)
			}
			if m.Session.Key != meta.Session.Key {
				return fmt.Errorf("live: WAL %s belongs to session %d", s.walPath, m.Session.Key)
			}
			s.meta, sawMeta = sourceMeta(m), true
			return nil
		}
		ev, err := decodeEvent(p)
		if err != nil {
			return err
		}
		s.applyLocked(ev)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.recovered = s.lastSeq
	if s.w, err = wal.Open(s.walPath, syncEvery); err != nil {
		return nil, err
	}
	if records == 0 || !sawMeta {
		// The first record makes the log self-describing.
		j, err := json.Marshal(s.meta)
		if err != nil {
			return nil, err
		}
		if _, err := s.w.Append(append([]byte{recMeta}, j...)); err != nil {
			return nil, err
		}
		if err := s.w.Sync(); err != nil {
			return nil, err
		}
		s.recSeq = append(s.recSeq, s.lastSeq)
	}
	return s, nil
}

// Meta returns the session and driver list.
func (s *Session) Meta() segment.Meta { return s.meta }

// Apply logs and applies one event. An event whose Seq is not above the last
// applied one is a resend and is ignored (reported as false).
func (s *Session) Apply(ev Event) (applied bool, err error) {
	if ev.Driver < 0 || ev.Driver > 255 || ev.Kind < KindCar || ev.Kind > KindLap {
		return false, errBadEvent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Seq <= s.lastSeq {
		return false, nil
	}
	// Log first: an event is never visible to queries without being in the WAL buffer.
	if _, err := s.w.Append(ev.append(nil)); err != nil {
		return false, err
	}
	s.recSeq = append(s.recSeq, ev.Seq)
	s.applyLocked(ev)
	return true, nil
}

func (s *Session) applyLocked(ev Event) {
	if ev.Seq <= s.lastSeq {
		return
	}
	s.lastSeq = ev.Seq
	s.version++
	switch ev.Kind {
	case KindCar:
		s.car[ev.Driver] = append(s.car[ev.Driver], ev.Car)
	case KindLoc:
		s.loc[ev.Driver] = append(s.loc[ev.Driver], ev.Loc)
	case KindLap:
		if s.timing[ev.Driver] == nil {
			s.timing[ev.Driver] = map[int]laps.Timing{}
		}
		s.timing[ev.Driver][ev.Lap.Number] = ev.Lap
	}
}

// LastSeq is the highest event sequence applied (durable or not).
func (s *Session) LastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// DurableSeq is the highest event sequence known to be on disk: an event at
// or below it survives a crash. This is what a source may acknowledge.
func (s *Session) DurableSeq() uint64 {
	n := s.w.Durable()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n == 0 {
		return s.recovered
	}
	return s.recSeq[min(int(n), len(s.recSeq))-1]
}

// Sync forces buffered events to disk.
func (s *Session) Sync() error { return s.w.Sync() }

// Version changes whenever an event is applied.
func (s *Session) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Build serialises the session's current contents exactly as batch ingest
// would. data is nil while there is no telemetry inside a lap.
func (s *Session) Build() (data []byte, meta segment.Meta, version uint64, err error) {
	s.mu.Lock()
	timing := make(map[int][]laps.Timing, len(s.timing))
	for d, m := range s.timing {
		rows := make([]laps.Timing, 0, len(m))
		for _, t := range m {
			rows = append(rows, t)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Number < rows[j].Number })
		timing[d] = rows
	}
	car := make(map[int][]laps.Car, len(s.car))
	var rawSamples int64
	for d, rows := range s.car {
		car[d] = rows[:len(rows):len(rows)] // appends after unlock cannot touch what we read
		rawSamples += int64(len(rows))
	}
	loc := make(map[int][]laps.Loc, len(s.loc))
	for d, rows := range s.loc {
		loc[d] = rows[:len(rows):len(rows)]
	}
	version = s.version
	m := s.meta
	s.mu.Unlock()

	// Cleaning copies, sorts and de-duplicates, as batch ingest does.
	for d := range car {
		car[d] = laps.CleanCar(car[d])
	}
	for d := range loc {
		loc[d] = laps.CleanLoc(loc[d])
	}
	if m.RawSamples == 0 {
		m.RawSamples = rawSamples // a real feed has no "rows fetched" figure; count what arrived
	}
	data, meta, _, err = ingest.BuildSegment(m, timing, car, loc)
	return data, meta, version, err
}

// Seal writes the session as a normal segment, registers it in the manifest
// and removes the WAL. After Seal the session must not be used.
func (s *Session) Seal(dataDir string) (store.Entry, error) {
	if err := s.w.Close(); err != nil {
		return store.Entry{}, err
	}
	data, meta, _, err := s.Build()
	if err != nil {
		return store.Entry{}, err
	}
	if data == nil {
		return store.Entry{}, errors.New("live: nothing to seal (no telemetry inside a lap)")
	}
	segDir := filepath.Join(dataDir, store.SegmentDir)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return store.Entry{}, err
	}
	name, sum := store.SegmentFileName(meta.Session.Key, data)
	if err := segment.WriteFile(filepath.Join(segDir, name), data); err != nil {
		return store.Entry{}, err
	}
	e := store.Entry{SessionKey: meta.Session.Key, Year: meta.Session.Year, File: name, Size: int64(len(data)), SHA256: sum, Samples: meta.Samples}
	var old string
	if err := store.Update(dataDir, func(m *store.Manifest) error {
		old = m.Put(e)
		return nil
	}); err != nil {
		return store.Entry{}, err
	}
	if old != "" {
		os.Remove(filepath.Join(segDir, old))
	}
	// Only now is the log redundant. A crash before this line leaves both the
	// segment and the WAL; sealing again produces the same segment.
	return e, os.Remove(s.walPath)
}

// Close syncs and closes the WAL without sealing.
func (s *Session) Close() error { return s.w.Close() }

// Snapshot parses the current contents into an in-memory segment for the
// store. It returns nil when there is nothing to serve yet.
func (s *Session) Snapshot() (*segment.Segment, uint64, error) {
	data, _, version, err := s.Build()
	if err != nil || data == nil {
		return nil, version, err
	}
	seg, err := segment.Parse(data)
	return seg, version, err
}
