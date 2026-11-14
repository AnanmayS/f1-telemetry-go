package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/lru"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
)

// DefaultCacheBytes is the default decoded-lap cache budget.
const DefaultCacheBytes = 256 << 20

var (
	ErrNoSession = errors.New("store: session not found")
	ErrNoLap     = errors.New("store: lap not found")
	ErrNoData    = errors.New("store: lap has no telemetry")
)

// Store serves laps from the live segments named by the manifest. Reads take
// no store-wide lock: the segment set is an immutable snapshot behind an
// atomic pointer, and the lap cache is sharded.
type Store struct {
	dir string
	log *slog.Logger

	snap     atomic.Pointer[snapshot]
	reloadMu sync.Mutex // serialises Reload; never taken by readers

	cache  *lru.Cache[lapKey, *model.LapData]
	flight flight

	hits, misses, shared, decodes, decodeErrors atomic.Int64
}

type snapshot struct {
	version  int64
	segs     map[int]*openSegment
	sessions []model.Session // newest first
	years    []int           // newest first
}

type openSegment struct {
	entry Entry
	seg   *segment.Segment
}

// Stats is a point-in-time view for metrics.
type Stats struct {
	ManifestVersion int64
	Segments        int
	CacheHits       int64
	CacheMisses     int64
	SharedLoads     int64 // requests that waited on another request's decode
	Decodes         int64
	DecodeErrors    int64
	CacheBytes      int64
}

// Open loads the manifest in dir and opens its segments. A segment that
// fails to open is logged and left out; the rest are served.
func Open(dir string, cacheBytes int64, log *slog.Logger) (*Store, error) {
	if cacheBytes <= 0 {
		cacheBytes = DefaultCacheBytes
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Store{dir: dir, log: log, cache: lru.New[lapKey, *model.LapData](cacheBytes)}
	s.snap.Store(&snapshot{segs: map[int]*openSegment{}})
	if _, err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload re-reads the manifest and swaps in new or rebuilt segments. Segments
// no longer named are released; their mappings close once in-flight reads
// finish. It reports whether anything changed.
func (s *Store) Reload() (bool, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	man, err := LoadManifest(s.dir)
	if err != nil {
		return false, err
	}
	old := s.snap.Load()
	if man.Version == old.version {
		return false, nil
	}
	next := &snapshot{version: man.Version, segs: make(map[int]*openSegment, len(man.Segments))}
	for _, e := range man.Segments {
		if cur, ok := old.segs[e.SessionKey]; ok && cur.entry.File == e.File {
			next.segs[e.SessionKey] = cur
			continue
		}
		seg, err := segment.Open(filepath.Join(s.dir, SegmentDir, e.File))
		if err == nil && int64(seg.Size()) != e.Size {
			seg.Close()
			err = fmt.Errorf("size %d does not match manifest %d", seg.Size(), e.Size)
		}
		if err == nil && seg.SessionKey != e.SessionKey {
			seg.Close()
			err = fmt.Errorf("file holds session %d", seg.SessionKey)
		}
		if err != nil {
			s.log.Error("segment unavailable", "session", e.SessionKey, "file", e.File, "err", err)
			if cur, ok := old.segs[e.SessionKey]; ok {
				next.segs[e.SessionKey] = cur // keep serving the previous build
			}
			continue
		}
		next.segs[e.SessionKey] = &openSegment{entry: e, seg: seg}
	}
	seenYear := map[int]bool{}
	for _, os := range next.segs {
		sess := os.seg.Meta.Session
		next.sessions = append(next.sessions, sess)
		if !seenYear[sess.Year] {
			seenYear[sess.Year] = true
			next.years = append(next.years, sess.Year)
		}
	}
	sort.Slice(next.sessions, func(i, j int) bool { return next.sessions[i].Start.After(next.sessions[j].Start) })
	sort.Sort(sort.Reverse(sort.IntSlice(next.years)))
	s.snap.Store(next)
	for key, o := range old.segs {
		if next.segs[key] != o {
			o.seg.Release()
		}
	}
	s.log.Info("manifest loaded", "version", man.Version, "segments", len(next.segs))
	return true, nil
}

// Watch reloads the manifest every interval until ctx is done.
func (s *Store) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.Reload(); err != nil {
				s.log.Error("manifest reload failed", "err", err)
			}
		}
	}
}

// Close releases every segment.
func (s *Store) Close() {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	old := s.snap.Swap(&snapshot{segs: map[int]*openSegment{}})
	for _, o := range old.segs {
		o.seg.Release()
	}
}

func (s *Store) Version() int64 { return s.snap.Load().version }

// Years lists seasons with data, newest first. Do not modify.
func (s *Store) Years() []int { return s.snap.Load().years }

// Sessions lists sessions newest first; year 0 means all. Do not modify.
func (s *Store) Sessions(year int) []model.Session {
	all := s.snap.Load().sessions
	if year == 0 {
		return all
	}
	var out []model.Session
	for _, x := range all {
		if x.Year == year {
			out = append(out, x)
		}
	}
	return out
}

// Session is a referenced view of one session's segment. Call Release when done.
type Session struct {
	*segment.Segment
	Gen string // content checksum; changes when the session is rebuilt
}

// Acquire returns the session's segment with a reference held.
func (s *Store) Acquire(key int) (Session, error) {
	// A segment can be retired between loading the snapshot and taking the
	// reference; the fresh snapshot then holds its replacement.
	for range 3 {
		o, ok := s.snap.Load().segs[key]
		if !ok {
			return Session{}, ErrNoSession
		}
		if o.seg.Acquire() {
			return Session{Segment: o.seg, Gen: o.entry.SHA256}, nil
		}
	}
	return Session{}, ErrNoSession
}

// Fastest returns the driver's fastest lap with an official time and telemetry.
func Fastest(seg *segment.Segment, driver int) (model.Lap, bool) {
	var best model.Lap
	found := false
	for _, l := range seg.Laps(driver) {
		if l.ValidTime() && l.Samples > 0 && (!found || l.OfficialMs < best.OfficialMs) {
			best, found = l, true
		}
	}
	return best, found
}

// Lap returns a decoded lap, from cache when possible. Concurrent requests
// for the same uncached lap decode it once. The result is shared: do not modify.
func (s *Store) Lap(sess Session, driver, lap int) (*model.LapData, error) {
	k := lapKey{session: sess.SessionKey, gen: sess.Gen, driver: driver, lap: lap}
	if d, ok := s.cache.Get(k); ok {
		s.hits.Add(1)
		return d, nil
	}
	s.misses.Add(1)
	d, err, shared := s.flight.do(k, func() (*model.LapData, error) {
		if d, ok := s.cache.Get(k); ok { // filled while we waited for the flight slot
			return d, nil
		}
		row, ok := sess.Segment.Lap(driver, lap)
		if !ok {
			return nil, ErrNoLap
		}
		if row.Samples == 0 {
			return nil, ErrNoData
		}
		s.decodes.Add(1)
		d, err := sess.ReadLap(driver, lap)
		if err != nil {
			s.decodeErrors.Add(1)
			s.log.Error("lap decode failed", "session", sess.SessionKey, "driver", driver, "lap", lap, "err", err)
			return nil, err
		}
		s.cache.Put(k, d, int64(d.SizeBytes()))
		return d, nil
	})
	if shared {
		s.shared.Add(1)
	}
	return d, err
}

func (s *Store) Stats() Stats {
	snap := s.snap.Load()
	return Stats{
		ManifestVersion: snap.version, Segments: len(snap.segs),
		CacheHits: s.hits.Load(), CacheMisses: s.misses.Load(), SharedLoads: s.shared.Load(),
		Decodes: s.decodes.Load(), DecodeErrors: s.decodeErrors.Load(), CacheBytes: s.cache.Bytes(),
	}
}
