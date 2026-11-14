package store

import (
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

	c.data, c.err = fn()
	f.mu.Lock()
	delete(f.calls, k)
	f.mu.Unlock()
	c.wg.Done()
	return c.data, c.err, false
}
