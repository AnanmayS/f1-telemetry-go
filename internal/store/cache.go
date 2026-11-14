package store

import (
	"fmt"
	"sync"

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

	// Deferred so that a panic in fn still releases the waiters and the key;
	// otherwise every later request for this lap would block forever.
	defer func() {
		if r := recover(); r != nil {
			c.err = fmt.Errorf("store: lap load panicked: %v", r)
			d, err = nil, c.err
		}
		f.mu.Lock()
		delete(f.calls, k)
		f.mu.Unlock()
		c.wg.Done()
	}()
	c.data, c.err = fn()
	return c.data, c.err, false
}
