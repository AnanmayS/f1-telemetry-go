package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func collect(t testing.TB, path string) [][]byte {
	t.Helper()
	var got [][]byte
	if _, _, err := Replay(path, func(p []byte) error {
		got = append(got, bytes.Clone(p))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func writeN(t testing.TB, path string, from, n int) {
	t.Helper()
	w, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := from; i < from+n; i++ {
		if _, err := w.Append([]byte(fmt.Sprintf("record-%04d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendReplayRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	if n, size, err := Replay(path, func([]byte) error { return nil }); err != nil || n != 0 || size != 0 {
		t.Fatalf("missing file: n=%d size=%d err=%v", n, size, err)
	}
	writeN(t, path, 0, 100)
	writeN(t, path, 100, 50) // reopening appends
	got := collect(t, path)
	if len(got) != 150 {
		t.Fatalf("records = %d", len(got))
	}
	for i, p := range got {
		if string(p) != fmt.Sprintf("record-%04d", i) {
			t.Fatalf("record %d = %q", i, p)
		}
	}
}

// Every possible truncation point must replay to a clean prefix, and the
// file must be cut back so later appends follow the last good record.
func TestTornTailIsTruncatedAtEveryOffset(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "full.wal")
	writeN(t, full, 0, 5)
	data, _ := os.ReadFile(full)
	recLen := len(data) / 5
	for cut := 0; cut <= len(data); cut++ {
		path := filepath.Join(dir, "cut.wal")
		os.WriteFile(path, data[:cut], 0o644)
		got := collect(t, path)
		if want := cut / recLen; len(got) != want {
			t.Fatalf("cut at %d: %d records, want %d", cut, len(got), want)
		}
		if st, _ := os.Stat(path); st.Size() != int64(len(got)*recLen) {
			t.Fatalf("cut at %d: file is %d bytes after replay, want %d", cut, st.Size(), len(got)*recLen)
		}
		writeN(t, path, 100, 1)
		after := collect(t, path)
		if len(after) != len(got)+1 || string(after[len(after)-1]) != "record-0100" {
			t.Fatalf("cut at %d: append after recovery gave %d records", cut, len(after))
		}
	}
}

func TestCorruptRecordEndsReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	writeN(t, path, 0, 10)
	data, _ := os.ReadFile(path)
	recLen := len(data) / 10
	bad := bytes.Clone(data)
	bad[6*recLen+headerSize+2] ^= 0x01 // flip a payload bit in record 6
	os.WriteFile(path, bad, 0o644)
	if got := collect(t, path); len(got) != 6 {
		t.Fatalf("records = %d, want 6 (everything before the damage)", len(got))
	}
	// A length field pointing past the limit, or zero, also ends the log.
	for _, n := range []uint32{0, MaxRecord + 1, 0xffffffff} {
		b := bytes.Clone(data[:recLen])
		b = binary.LittleEndian.AppendUint32(b, n)
		b = binary.LittleEndian.AppendUint32(b, 0)
		b = append(b, "garbage"...)
		os.WriteFile(path, b, 0o644)
		if got := collect(t, path); len(got) != 1 {
			t.Fatalf("length %d: records = %d", n, len(got))
		}
	}
}

func TestDurabilityAndGroupCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	w, err := Open(path, time.Hour) // background sync effectively off
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		seq, err := w.Append([]byte("x"))
		if err != nil || seq != uint64(i+1) {
			t.Fatalf("append %d: seq=%d err=%v", i, seq, err)
		}
	}
	if d := w.Durable(); d != 0 {
		t.Fatalf("durable = %d before any sync", d)
	}
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Fatal("records reached the file before a sync (buffering is off)")
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if d := w.Durable(); d != 10 {
		t.Fatalf("durable = %d after sync", d)
	}
	if got := collect(t, path); len(got) != 10 {
		t.Fatalf("synced records on disk = %d", len(got))
	}
	w.Close()

	// With a short interval the background syncer makes appends durable on its own.
	w, _ = Open(path, 5*time.Millisecond)
	seq, _ := w.Append([]byte("y"))
	deadline := time.Now().Add(2 * time.Second)
	for w.Durable() < seq && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w.Durable() < seq {
		t.Fatal("background sync never made the record durable")
	}
	w.Close()

	// syncEvery = 0: every append is durable when it returns.
	w, _ = Open(path, 0)
	seq, _ = w.Append([]byte("z"))
	if w.Durable() != seq {
		t.Fatal("synchronous append not durable on return")
	}
	w.Close()
	if _, err := w.Append([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestRejectsBadRecords(t *testing.T) {
	w, _ := Open(filepath.Join(t.TempDir(), "s.wal"), 0)
	defer w.Close()
	if _, err := w.Append(nil); err == nil {
		t.Error("empty record accepted")
	}
	if _, err := w.Append(make([]byte, MaxRecord+1)); err == nil {
		t.Error("oversized record accepted")
	}
}

func TestReplayStopsOnCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	writeN(t, path, 0, 5)
	before, _ := os.Stat(path)
	boom := errors.New("boom")
	n, _, err := Replay(path, func(p []byte) error {
		if string(p) == "record-0002" {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if after, _ := os.Stat(path); after.Size() != before.Size() {
		t.Fatal("a callback error must not truncate the log")
	}
}

func TestConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.wal")
	w, _ := Open(path, time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				if _, err := w.Append([]byte(fmt.Sprintf("%d-%d", g, i))); err != nil {
					t.Error(err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range collect(t, path) {
		if seen[string(p)] {
			t.Fatalf("duplicate record %q", p)
		}
		seen[string(p)] = true
	}
	if len(seen) != 4000 {
		t.Fatalf("records = %d, want 4000", len(seen))
	}
}

// FuzzReplay: arbitrary file contents never panic, and replay always leaves
// a file that is exactly the records it reported.
func FuzzReplay(f *testing.F) {
	rec := func(p string) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(len(p)))
		b = binary.LittleEndian.AppendUint32(b, crc32.ChecksumIEEE([]byte(p)))
		return append(b, p...)
	}
	f.Add(append(rec("hello"), rec("world")...))
	f.Add(append(rec("hello"), 0xff, 0xff, 0xff))
	f.Add([]byte{})
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "fuzz.wal")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		var total int64
		n, size, err := Replay(path, func(p []byte) error {
			total += headerSize + int64(len(p))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if total != size || size > int64(len(data)) {
			t.Fatalf("size %d, records sum to %d, input %d", size, total, len(data))
		}
		if st, _ := os.Stat(path); st.Size() != size {
			t.Fatalf("file is %d bytes, replay kept %d", st.Size(), size)
		}
		n2, size2, _ := Replay(path, func([]byte) error { return nil })
		if n2 != n || size2 != size {
			t.Fatal("second replay disagrees with the first")
		}
	})
}
