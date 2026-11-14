package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	reloadMu sync.Mutex // serialises snapshot changes; never taken by readers
	// live holds in-memory segments of sessions still being ingested. They are
	// served like any other segment until the session is sealed into the manifest.
	live map[int]*openSegment

	cache  *lru.Cache[lapKey, *model.LapData]
	flight flight

	hits, misses, shared, decodes, decodeErrors atomic.Int64
}

type snapshot struct {
	version  int64  // manifest version
	rev      string // identifies the set of served segment builds
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
	failed := false
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
			failed = true
			if cur, ok := old.segs[e.SessionKey]; ok {
				next.segs[e.SessionKey] = cur // keep serving the previous build
			}
			continue
		}
		next.segs[e.SessionKey] = &openSegment{entry: e, seg: seg}
	}
	for key, o := range s.live {
		if _, sealed := next.segs[key]; sealed {
			delete(s.live, key) // sealed: the file replaces the in-memory segment (released below)
		} else {
			next.segs[key] = o
		}
	}
	if failed {
		// Keep the old version so the next Reload tries the failed files again
		// (the cause may be transient, such as running out of file descriptors).
		next.version = old.version
	}
	s.publish(next, old)
	s.log.Info("manifest loaded", "version", man.Version, "segments", len(next.segs))
	return true, nil
}

// publish finishes next (listings, revision), swaps it in and releases the
// segments of old that it no longer serves. Callers hold reloadMu.
func (s *Store) publish(next, old *snapshot) {
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
	// The revision is derived from content (which build of which session is
	// served), so it means the same thing across restarts and replicas.
	keys := make([]int, 0, len(next.segs))
	for k := range next.segs {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d=%s;", k, next.segs[k].entry.SHA256)
	}
	next.rev = hex.EncodeToString(h.Sum(nil)[:12])
	s.snap.Store(next)
	for key, o := range old.segs {
		if next.segs[key] != o {
			o.seg.Release()
		}
	}
}

// SetLive serves seg (an in-memory segment of a session being ingested)
// under its session key, replacing any earlier live segment for it. gen must
// differ for every call; it keys the caches. A session already sealed into
// the manifest is not overridden. The store takes ownership of seg.
func (s *Store) SetLive(seg *segment.Segment, gen string) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	old := s.snap.Load()
	if cur, ok := old.segs[seg.SessionKey]; ok && s.live[seg.SessionKey] != cur {
		seg.Release() // sealed already
		return
	}
	if s.live == nil {
		s.live = map[int]*openSegment{}
	}
	o := &openSegment{entry: Entry{SessionKey: seg.SessionKey, Year: seg.Meta.Session.Year, SHA256: gen}, seg: seg}
	s.live[seg.SessionKey] = o
	next := &snapshot{version: old.version, segs: make(map[int]*openSegment, len(old.segs)+1)}
	for k, v := range old.segs {
		next.segs[k] = v
	}
	next.segs[seg.SessionKey] = o
	s.publish(next, old)
}

// ClearLive stops serving a live segment (after its session was sealed and
// reloaded, or abandoned).
func (s *Store) ClearLive(key int) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	o, ok := s.live[key]
	if !ok {
		return
	}
	delete(s.live, key)
	old := s.snap.Load()
	next := &snapshot{version: old.version, segs: make(map[int]*openSegment, len(old.segs))}
	for k, v := range old.segs {
		if v != o {
			next.segs[k] = v
		}
	}
	s.publish(next, old)
}

// Rev identifies the set of served segment builds (sealed and live). It
// changes whenever that set changes and is stable across restarts, so
// listings use it as their cache validator.
func (s *Store) Rev() string { return s.snap.Load().rev }

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
	s.live = nil
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
