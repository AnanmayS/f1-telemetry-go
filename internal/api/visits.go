package api

import (
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"
)

// visitCounter counts page views and distinct visitors per UTC day without
// storing anything that identifies a person. An address is hashed with a
// random salt that exists only in memory and is replaced every day, so the
// hashes cannot be reversed or linked across days, and nothing is written to
// disk. No cookies, no third parties.
type visitCounter struct {
	views, shareOpens atomic.Int64

	mu   sync.Mutex
	day  string
	salt [32]byte
	seen map[[8]byte]struct{}
}

func newVisitCounter() *visitCounter {
	v := &visitCounter{seen: map[[8]byte]struct{}{}, day: time.Now().UTC().Format(time.DateOnly)}
	rand.Read(v.salt[:])
	return v
}

func (v *visitCounter) pageView(ip string, shareLink bool, now time.Time) {
	v.views.Add(1)
	if shareLink {
		v.shareOpens.Add(1)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.rollLocked(now)
	h := sha256.New()
	h.Write(v.salt[:])
	h.Write([]byte(ip))
	var k [8]byte
	copy(k[:], h.Sum(nil))
	v.seen[k] = struct{}{}
}

func (v *visitCounter) uniqueToday() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.seen)
}

// roll starts a new day if the date changed, returning the finished day's count.
func (v *visitCounter) roll(now time.Time) (day string, unique int, rolled bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.rollLocked(now)
}

func (v *visitCounter) rollLocked(now time.Time) (string, int, bool) {
	today := now.UTC().Format(time.DateOnly)
	if today == v.day {
		return "", 0, false
	}
	day, n := v.day, len(v.seen)
	v.day, v.seen = today, map[[8]byte]struct{}{}
	rand.Read(v.salt[:])
	return day, n, true
}
