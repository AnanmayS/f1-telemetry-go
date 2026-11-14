// Package wal is an append-only write-ahead log: length-prefixed records
// with a CRC, group-committed by a background fsync, replayed on startup
// with torn tails truncated.
//
// Record layout (little-endian):
//
//	length u32 | crc32(payload) u32 | payload
package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	headerSize = 8
	// MaxRecord bounds one record. Live events are tens of bytes; the limit
	// stops a corrupt length field from asking for gigabytes.
	MaxRecord = 1 << 20
	// DefaultSyncEvery is the default group-commit interval.
	DefaultSyncEvery = 100 * time.Millisecond
)

var ErrClosed = errors.New("wal: closed")

// Replay calls fn for every intact record in the file at path, in order. It
// stops at the first torn or corrupt record and truncates the file there, so
// the log ends on a record boundary and appends can continue. A missing file
// is an empty log. It returns the number of records and the bytes kept.
func Replay(path string, fn func(payload []byte) error) (records int, size int64, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	records, size, err = scan(bufio.NewReaderSize(f, 1<<20), fn)
	if err != nil {
		return records, size, err
	}
	if size < st.Size() {
		// Everything past the last intact record is a torn write.
		if err := f.Truncate(size); err != nil {
			return records, size, err
		}
		if err := f.Sync(); err != nil {
			return records, size, err
		}
	}
	return records, size, nil
}

// scan reads records until EOF or the first damaged one. An error is
// returned only when fn fails; damage just ends the scan.
func scan(r io.Reader, fn func([]byte) error) (records int, good int64, err error) {
	var hdr [headerSize]byte
	buf := make([]byte, 0, 4096)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return records, good, nil // clean EOF or a torn header
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n == 0 || n > MaxRecord {
			return records, good, nil
		}
		if cap(buf) < int(n) {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return records, good, nil // torn payload
		}
		if crc32.ChecksumIEEE(buf) != binary.LittleEndian.Uint32(hdr[4:]) {
			return records, good, nil
		}
		if err := fn(buf); err != nil {
			return records, good, err
		}
		records++
		good += headerSize + int64(n)
	}
}

// Writer appends records. Appends are buffered and made durable by a
// background fsync every SyncEvery (group commit), or by Sync.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	buf     *bufio.Writer
	written uint64 // records appended
	durable uint64 // records known to be on disk
	err     error  // first write or sync error; the log is unusable after it
	closed  bool

	syncEvery time.Duration
	stop      chan struct{}
	done      chan struct{}
}

// Open opens (creating if needed) the log at path for appending. Call Replay
// first: Open does not validate existing contents. syncEvery <= 0 makes
// every Append fsync before returning.
func Open(path string, syncEvery time.Duration) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// Make the file's directory entry durable too; fsync on the file alone
	// does not guarantee the file still exists after a power loss.
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		d.Sync()
		d.Close()
	}
	w := &Writer{f: f, buf: bufio.NewWriterSize(f, 256<<10), syncEvery: syncEvery}
	if syncEvery > 0 {
		w.stop, w.done = make(chan struct{}), make(chan struct{})
		go w.syncLoop()
	}
	return w, nil
}

func (w *Writer) syncLoop() {
	defer close(w.done)
	t := time.NewTicker(w.syncEvery)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.Sync()
		}
	}
}

// Append adds one record and returns its sequence number (1 for the first
// record appended by this Writer). The record is durable once Durable()
// reaches that number.
func (w *Writer) Append(payload []byte) (uint64, error) {
	if len(payload) == 0 || len(payload) > MaxRecord {
		return 0, fmt.Errorf("wal: record size %d out of range", len(payload))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	var hdr [headerSize]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:], crc32.ChecksumIEEE(payload))
	if _, err := w.buf.Write(hdr[:]); err != nil {
		w.err = err
		return 0, err
	}
	if _, err := w.buf.Write(payload); err != nil {
		w.err = err
		return 0, err
	}
	w.written++
	if w.syncEvery <= 0 {
		if err := w.syncLocked(); err != nil {
			return 0, err
		}
	}
	return w.written, nil
}

// Sync flushes buffered records and fsyncs. When it returns nil, every
// record appended before the call is durable.
func (w *Writer) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	return w.syncLocked()
}

func (w *Writer) syncLocked() error {
	if w.err != nil {
		return w.err
	}
	if w.durable == w.written {
		return nil
	}
	target := w.written
	if err := w.buf.Flush(); err != nil {
		w.err = err
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.err = err
		return err
	}
	w.durable = target
	return nil
}

// Durable is the sequence number of the last record known to be on disk.
func (w *Writer) Durable() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.durable
}

// Close syncs and closes the log.
func (w *Writer) Close() error {
	if w.stop != nil {
		close(w.stop)
		<-w.done
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	err := w.syncLocked()
	w.closed = true
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}
