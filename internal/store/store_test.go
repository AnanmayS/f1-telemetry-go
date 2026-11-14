package store

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func mkLap(driver, number, n int, official int32, speed int64) *model.LapData {
	ld := &model.LapData{Lap: model.Lap{Driver: driver, Number: number, Samples: n, OfficialMs: official,
		StartMs: int64(number) * 100_000, EndMs: int64(number+1) * 100_000}}
	if official > 0 {
		ld.Lap.Flags = model.FlagValidTime | model.FlagDistanceOK
	}
	for c := range ld.Values {
		ld.Values[c] = make([]int64, n)
	}
	for i := 0; i < n; i++ {
		ld.Values[model.ChT][i] = ld.Lap.StartMs + int64(i)*250
		ld.Values[model.ChSpeed][i] = speed
		ld.Values[model.ChDistance][i] = int64(i) * 1000
	}
	return ld
}

// writeSession builds a segment for session key and registers it in the manifest.
func writeSession(t testing.TB, dir string, key, year int, speed int64) Entry {
	t.Helper()
	meta := segment.Meta{
		Session: model.Session{Key: key, Year: year, Name: "Qualifying", Start: time.Date(year, 3, key%28+1, 0, 0, 0, 0, time.UTC)},
		Drivers: []model.Driver{{Number: 1, Acronym: "VER"}, {Number: 4, Acronym: "NOR"}},
	}
	laps := []*model.LapData{
		mkLap(1, 1, 300, model.NoTime, speed), mkLap(1, 2, 300, 90_000, speed), mkLap(1, 3, 300, 88_000, speed),
		mkLap(1, 4, 0, 80_000, speed), // fastest on paper, but no telemetry
		mkLap(4, 1, 300, 89_000, speed),
	}
	data, err := segment.Build(meta, laps)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, SegmentDir), 0o755)
	name, sum := SegmentFileName(key, data)
	if err := segment.WriteFile(filepath.Join(dir, SegmentDir, name), data); err != nil {
		t.Fatal(err)
	}
	man, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{SessionKey: key, Year: year, File: name, Size: int64(len(data)), SHA256: sum}
	man.Put(e)
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestManifestAtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManifest(dir)
	if err != nil || m.Version != 0 || len(m.Segments) != 0 {
		t.Fatalf("empty manifest: %+v %v", m, err)
	}
	m.Put(Entry{SessionKey: 2, File: "2-a.seg"})
	m.Put(Entry{SessionKey: 1, File: "1-a.seg"})
	m.Skip(3, "no telemetry")
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	if old := m.Put(Entry{SessionKey: 2, File: "2-b.seg"}); old != "2-a.seg" {
		t.Fatalf("replaced = %q", old)
	}
	m.Put(Entry{SessionKey: 3, File: "3-a.seg"}) // a skipped session that later gets data
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 2 || len(got.Segments) != 3 || got.Segments[0].SessionKey != 1 || got.Segments[1].File != "2-b.seg" || len(got.Skipped) != 0 {
		t.Fatalf("manifest: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestName+".tmp")); !os.IsNotExist(err) {
		t.Fatal("MANIFEST.tmp left behind")
	}
	os.WriteFile(filepath.Join(dir, ManifestName), []byte("{half"), 0o644)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("corrupt manifest accepted")
	}
}

func TestListingAndFastest(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, 10, 2023, 100)
	writeSession(t, dir, 20, 2024, 100)
	writeSession(t, dir, 21, 2024, 100)
	s, err := Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if y := s.Years(); len(y) != 2 || y[0] != 2024 || y[1] != 2023 {
		t.Fatalf("years = %v", y)
	}
	if got := s.Sessions(2024); len(got) != 2 || got[0].Key != 21 {
		t.Fatalf("sessions(2024) = %+v", got)
	}
	if len(s.Sessions(0)) != 3 || len(s.Sessions(1999)) != 0 {
		t.Fatal("session filters")
	}
	if _, err := s.Acquire(999); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing session: %v", err)
	}
	sess, _ := s.Acquire(10)
	defer sess.Release()
	if l, ok := Fastest(sess.Segment, 1); !ok || l.Number != 3 {
		t.Fatalf("fastest = %+v (lap 4 has no telemetry, lap 1 no official time)", l)
	}
	if _, ok := Fastest(sess.Segment, 99); ok {
		t.Fatal("fastest for unknown driver")
	}
	if _, err := s.Lap(sess, 1, 77); !errors.Is(err, ErrNoLap) {
		t.Fatalf("unknown lap: %v", err)
	}
	if _, err := s.Lap(sess, 1, 4); !errors.Is(err, ErrNoData) {
		t.Fatalf("empty lap: %v", err)
	}
}

func TestLapCacheAndSingleflight(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, 10, 2024, 100)
	s, err := Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sess, _ := s.Acquire(10)
	defer sess.Release()

	const n = 64
	var wg sync.WaitGroup
	results := make([]*model.LapData, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			d, err := s.Lap(sess, 1, 2)
			if err != nil {
				t.Error(err)
			}
			results[i] = d
		}(i)
	}
	close(start)
	wg.Wait()
	st := s.Stats()
	if st.Decodes != 1 {
		t.Fatalf("decodes = %d, want exactly 1 for %d concurrent requests", st.Decodes, n)
	}
	for _, d := range results {
		if d != results[0] {
			t.Fatal("requests did not share one decoded lap")
		}
	}
	if st.CacheHits+st.CacheMisses != n || st.CacheBytes <= 0 {
		t.Fatalf("stats: %+v", st)
	}
	s.Lap(sess, 1, 2)
	if got := s.Stats(); got.Decodes != 1 || got.CacheHits != st.CacheHits+1 {
		t.Fatalf("warm read decoded again: %+v", got)
	}
}

func TestCacheEvictsByBytes(t *testing.T) {
	c := newLapCache(cacheShards * 3 * int64(mkLap(1, 1, 300, 0, 0).SizeBytes()))
	for lap := 0; lap < 400; lap++ {
		c.put(lapKey{session: 1, driver: 1, lap: lap}, mkLap(1, lap, 300, 0, 0))
	}
	var used int64
	for i := range c.shards {
		sh := &c.shards[i]
		if sh.used > sh.limit {
			t.Fatalf("shard %d over budget: %d > %d", i, sh.used, sh.limit)
		}
		if len(sh.items) != sh.ll.Len() {
			t.Fatal("map and list disagree")
		}
		used += sh.used
	}
	if used != c.bytes.Load() || used == 0 {
		t.Fatalf("byte accounting: shards=%d total=%d", used, c.bytes.Load())
	}
	// LRU order: touching an entry protects it from the next eviction.
	lru := newLapCache(cacheShards * 2 * int64(mkLap(1, 1, 300, 0, 0).SizeBytes()))
	k := func(l int) lapKey { return lapKey{session: 16 * l} } // same shard for every key
	lru.put(k(1), mkLap(1, 1, 300, 0, 0))
	lru.put(k(2), mkLap(1, 2, 300, 0, 0))
	lru.get(k(1))
	lru.put(k(3), mkLap(1, 3, 300, 0, 0))
	if _, ok := lru.get(k(1)); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := lru.get(k(2)); ok {
		t.Fatal("least recently used entry survived")
	}
	// An entry bigger than a shard is not cached.
	lru.put(k(9), mkLap(1, 9, 100_000, 0, 0))
	if _, ok := lru.get(k(9)); ok {
		t.Fatal("oversized entry cached")
	}
}

func TestReloadSwapsRebuiltSegment(t *testing.T) {
	dir := t.TempDir()
	first := writeSession(t, dir, 10, 2024, 100)
	s, err := Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if changed, _ := s.Reload(); changed {
		t.Fatal("reload with an unchanged manifest reported a change")
	}
	oldSess, _ := s.Acquire(10) // an in-flight reader
	if d, _ := s.Lap(oldSess, 1, 2); d.Values[model.ChSpeed][0] != 100 {
		t.Fatal("initial read")
	}

	second := writeSession(t, dir, 10, 2024, 222) // rebuild with different content
	writeSession(t, dir, 11, 2024, 100)           // and a brand-new session
	if first.File == second.File {
		t.Fatal("rebuilt segment reused the file name")
	}
	os.Remove(filepath.Join(dir, SegmentDir, first.File)) // ingest unlinks the replaced file
	if changed, err := s.Reload(); err != nil || !changed {
		t.Fatalf("reload: changed=%v err=%v", changed, err)
	}

	// The in-flight reader still reads the old file's bytes.
	if v, _, err := oldSess.ReadChannel(1, 3, model.ChSpeed); err != nil || v[0] != 100 {
		t.Fatalf("in-flight read after swap: %v", err)
	}
	oldSess.Release()
	if oldSess.Segment.Acquire() {
		t.Fatal("old segment still open after its last reader released it")
	}

	newSess, err := s.Acquire(10)
	if err != nil {
		t.Fatal(err)
	}
	defer newSess.Release()
	if d, err := s.Lap(newSess, 1, 2); err != nil || d.Values[model.ChSpeed][0] != 222 {
		t.Fatalf("read after swap returned stale data (err=%v)", err)
	}
	if _, err := s.Acquire(11); err != nil {
		t.Fatalf("new session not picked up: %v", err)
	}
	if s.Stats().Segments != 2 {
		t.Fatalf("segments = %d", s.Stats().Segments)
	}
}

func TestCorruptionIsIsolated(t *testing.T) {
	dir := t.TempDir()
	good := writeSession(t, dir, 10, 2024, 100)
	bad := writeSession(t, dir, 11, 2024, 100)
	blk := writeSession(t, dir, 12, 2024, 100)

	// Session 11: truncated file. Session 12: one flipped byte inside a block.
	path := filepath.Join(dir, SegmentDir, bad.File)
	b, _ := os.ReadFile(path)
	os.WriteFile(path, b[:len(b)/2], 0o644)
	path = filepath.Join(dir, SegmentDir, blk.File)
	b, _ = os.ReadFile(path)
	seg, _ := segment.Parse(b)
	for _, bi := range seg.Blocks() {
		if bi.Driver == 1 && bi.Lap == 2 && bi.Channel == model.ChRPM {
			b[bi.Offset+6] ^= 0xff
		}
	}
	os.WriteFile(path, b, 0o644)

	s, err := Open(dir, 0, quiet)
	if err != nil {
		t.Fatalf("a corrupt segment must not stop the store: %v", err)
	}
	defer s.Close()
	if _, err := s.Acquire(11); !errors.Is(err, ErrNoSession) {
		t.Fatalf("truncated segment: %v", err)
	}
	sess, err := s.Acquire(good.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Release()
	if _, err := s.Lap(sess, 1, 2); err != nil {
		t.Fatalf("healthy session: %v", err)
	}
	sess12, _ := s.Acquire(12)
	defer sess12.Release()
	if _, err := s.Lap(sess12, 1, 2); !errors.Is(err, segment.ErrCorrupt) {
		t.Fatalf("corrupt lap: %v", err)
	}
	if _, err := s.Lap(sess12, 1, 3); err != nil {
		t.Fatalf("neighbouring lap in the damaged file: %v", err)
	}
	if st := s.Stats(); st.DecodeErrors != 1 {
		t.Fatalf("decode errors = %d", st.DecodeErrors)
	}
}

func TestConcurrentReadsDuringReloads(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, 10, 2024, 1)
	s, err := Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sess, err := s.Acquire(10)
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Lap(sess, 1, 2+g%2); err != nil {
					t.Error(err)
				}
				sess.Release()
			}
		}()
	}
	for i := int64(2); i < 12; i++ {
		e := writeSession(t, dir, 10, 2024, i)
		if _, err := s.Reload(); err != nil {
			t.Fatal(err)
		}
		_ = e
	}
	close(stop)
	wg.Wait()
}
