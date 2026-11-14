package store

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

// lapKey identifies a decoded lap. gen is the segment's content checksum, so
// a rebuilt session never serves laps decoded from its previous file.
type lapKey struct {
	session int
	gen     string
	driver  int
	lap     int
}

const cacheShards = 16

// lapCache is a byte-bounded LRU of decoded laps, split into shards so
// readers of different laps do not contend on one lock.
type lapCache struct {
	shards [cacheShards]cacheShard
	bytes  atomic.Int64
}

type cacheShard struct {
	mu    sync.Mutex
	limit int64
	used  int64
	ll    *list.List // front = most recently used
	items map[lapKey]*list.Element
}

type cacheItem struct {
	key  lapKey
	data *model.LapData
	size int64
}

func newLapCache(limitBytes int64) *lapCache {
	c := &lapCache{}
	for i := range c.shards {
		c.shards[i] = cacheShard{limit: limitBytes / cacheShards, ll: list.New(), items: map[lapKey]*list.Element{}}
	}
	return c
}

func (c *lapCache) shard(k lapKey) *cacheShard {
	h := uint32(k.session)*2654435761 ^ uint32(k.driver)*40503 ^ uint32(k.lap)*97
	return &c.shards[h%cacheShards]
}

func (c *lapCache) get(k lapKey) (*model.LapData, bool) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[k]; ok {
		s.ll.MoveToFront(e)
		return e.Value.(*cacheItem).data, true
	}
	return nil, false
}

func (c *lapCache) put(k lapKey, d *model.LapData) {
	size := int64(d.SizeBytes())
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if size > s.limit {
		return // larger than a whole shard: not worth evicting everything for
	}
	if e, ok := s.items[k]; ok {
		s.ll.MoveToFront(e)
		return
	}
	s.items[k] = s.ll.PushFront(&cacheItem{key: k, data: d, size: size})
	s.used += size
	c.bytes.Add(size)
	for s.used > s.limit {
		e := s.ll.Back()
		it := e.Value.(*cacheItem)
		s.ll.Remove(e)
		delete(s.items, it.key)
		s.used -= it.size
		c.bytes.Add(-it.size)
	}
}

// flight collapses concurrent loads of the same key into one.
type flight struct {
	mu    sync.Mutex
	calls map[lapKey]*call
}

type call struct {
	wg   sync.WaitGroup
	data *model.LapData
	err  error
}

func (f *flight) do(k lapKey, fn func() (*model.LapData, error)) (d *model.LapData, err error, shared bool) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[lapKey]*call{}
	}
	if c, ok := f.calls[k]; ok {
		f.mu.Unlock()
		c.wg.Wait()
		return c.data, c.err, true
	}
	c := &call{}
	c.wg.Add(1)
	f.calls[k] = c
	f.mu.Unlock()

	c.data, c.err = fn()
	f.mu.Lock()
	delete(f.calls, k)
	f.mu.Unlock()
	c.wg.Done()
	return c.data, c.err, false
}
