package lru

import (
	"sync"
	"testing"
)

func TestEvictsLeastRecentlyUsedWithinBudget(t *testing.T) {
	c := New[int, string](shards * 100)
	for i := 0; i < 5000; i++ {
		c.Put(i, "v", 10)
	}
	var used int64
	for i := range c.shards {
		s := &c.shards[i]
		if s.used > s.limit {
			t.Fatalf("shard %d over budget: %d > %d", i, s.used, s.limit)
		}
		if len(s.items) != s.ll.Len() {
			t.Fatal("map and list disagree")
		}
		used += s.used
	}
	if used != c.Bytes() || used == 0 || c.Len() != int(used/10) {
		t.Fatalf("accounting: shards=%d total=%d len=%d", used, c.Bytes(), c.Len())
	}
}

func TestRecencyAndOversize(t *testing.T) {
	// Find three keys that share a shard so the order of eviction is observable.
	c := New[int, int](shards * 20)
	var keys []int
	for k := 0; len(keys) < 3; k++ {
		if c.shard(k) == c.shard(0) {
			keys = append(keys, k)
		}
	}
	c.Put(keys[0], 0, 10)
	c.Put(keys[1], 1, 10)
	c.Get(keys[0])
	c.Put(keys[2], 2, 10)
	if _, ok := c.Get(keys[0]); !ok {
		t.Fatal("recently used entry was evicted")
	}
	if _, ok := c.Get(keys[1]); ok {
		t.Fatal("least recently used entry survived")
	}
	c.Put(keys[0], 99, 10) // existing key keeps its value
	if v, _ := c.Get(keys[0]); v != 0 {
		t.Fatalf("existing entry overwritten: %d", v)
	}
	c.Put(1_000_000, 7, 21)
	if _, ok := c.Get(1_000_000); ok {
		t.Fatal("entry larger than a shard was stored")
	}
}

func TestConcurrent(t *testing.T) {
	c := New[int, int](shards * 1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				c.Put(i%700, i, 16)
				c.Get((i + g) % 700)
			}
		}(g)
	}
	wg.Wait()
	if c.Bytes() < 0 || c.Bytes() > shards*1000 {
		t.Fatalf("bytes = %d", c.Bytes())
	}
}
