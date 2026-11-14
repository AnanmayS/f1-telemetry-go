package api

import (
	"hash/maphash"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const limiterShards = 32

// ipLimiter is a per-client token bucket. Buckets live in shards so the
// request path never takes a server-wide lock.
type ipLimiter struct {
	rps        rate.Limit
	burst      int
	trustProxy bool
	seed       maphash.Seed
	shards     [limiterShards]limiterShard
}

type limiterShard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newIPLimiter(rps float64, burst int, trustProxy bool) *ipLimiter {
	l := &ipLimiter{rps: rate.Limit(rps), burst: burst, trustProxy: trustProxy, seed: maphash.MakeSeed()}
	for i := range l.shards {
		l.shards[i].buckets = map[string]*bucket{}
	}
	return l
}

// clientIP is the peer address, or the CDN-reported client address when the
// server is told it sits behind a trusted proxy.
func (l *ipLimiter) clientIP(r *http.Request) string {
	if l.trustProxy {
		if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	s := &l.shards[maphash.String(l.seed, ip)%limiterShards]
	s.mu.Lock()
	b, ok := s.buckets[ip]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.rps, l.burst)}
		s.buckets[ip] = b
	}
	b.seen = now
	s.mu.Unlock()
	return b.lim.AllowN(now, 1)
}

// sweep drops buckets idle for longer than maxIdle.
func (l *ipLimiter) sweep(now time.Time, maxIdle time.Duration) {
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		for ip, b := range s.buckets {
			if now.Sub(b.seen) > maxIdle {
				delete(s.buckets, ip)
			}
		}
		s.mu.Unlock()
	}
}
