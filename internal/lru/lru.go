// Package lru is a byte-bounded LRU cache split into shards, so readers of
// different keys do not contend on one lock.
package lru

import (
	"container/list"
	"hash/maphash"
	"sync"
	"sync/atomic"
)

const shards = 16

// Cache maps K to V, evicting least-recently-used entries once the total
// size reported for its values exceeds the limit. Safe for concurrent use.
type Cache[K comparable, V any] struct {
	seed   maphash.Seed
	shards [shards]shard[K, V]
	bytes  atomic.Int64
}

type shard[K comparable, V any] struct {
	mu    sync.Mutex
	limit int64
	used  int64
	ll    *list.List // front = most recently used
	items map[K]*list.Element
}

type item[K comparable, V any] struct {
	key  K
	val  V
	size int64
}

// New returns a cache holding at most limitBytes, divided evenly over shards.
func New[K comparable, V any](limitBytes int64) *Cache[K, V] {
	c := &Cache[K, V]{seed: maphash.MakeSeed()}
	for i := range c.shards {
		c.shards[i] = shard[K, V]{limit: limitBytes / shards, ll: list.New(), items: map[K]*list.Element{}}
	}
	return c
}

func (c *Cache[K, V]) shard(k K) *shard[K, V] {
	return &c.shards[maphash.Comparable(c.seed, k)%shards]
}

// Get returns the value for k and marks it recently used.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[k]; ok {
		s.ll.MoveToFront(e)
		return e.Value.(*item[K, V]).val, true
	}
	var zero V
	return zero, false
}

// Put stores v under k with the given size. A value larger than one shard's
// budget is not stored. An existing entry is kept and marked recently used.
func (c *Cache[K, V]) Put(k K, v V, size int64) {
	s := c.shard(k)
	s.mu.Lock()
	defer s.mu.Unlock()
	if size > s.limit {
		return
	}
	if e, ok := s.items[k]; ok {
		s.ll.MoveToFront(e)
		return
	}
	s.items[k] = s.ll.PushFront(&item[K, V]{key: k, val: v, size: size})
	s.used += size
	c.bytes.Add(size)
	for s.used > s.limit {
		e := s.ll.Back()
		it := e.Value.(*item[K, V])
		s.ll.Remove(e)
		delete(s.items, it.key)
		s.used -= it.size
		c.bytes.Add(-it.size)
	}
}

// Bytes is the total size of the stored values.
func (c *Cache[K, V]) Bytes() int64 { return c.bytes.Load() }

// Len is the number of entries.
func (c *Cache[K, V]) Len() int {
	n := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		n += len(s.items)
		s.mu.Unlock()
	}
	return n
}
