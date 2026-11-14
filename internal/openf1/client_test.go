package openf1

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func testClient(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int64) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(t.TempDir(), 1, 2)
	c.BaseURL = srv.URL + "/v1/"
	c.Limiter = rate.NewLimiter(rate.Inf, 1)
	c.BaseDelay = time.Millisecond
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return c, &hits
}

func TestFetchCachesOnce(t *testing.T) {
	var ua string
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		w.Write([]byte(`[{"driver_number":1,"name_acronym":"VER","team_colour":null}]`))
	})
	for i := 0; i < 3; i++ {
		d, n, err := c.Drivers(context.Background(), 9492)
		if err != nil || len(d) != 1 || d[0].NameAcronym != "VER" || d[0].TeamColour != nil || n == 0 {
			t.Fatalf("drivers: %+v n=%d err=%v", d, n, err)
		}
	}
	if hits.Load() != 1 || c.CacheHits.Load() != 2 {
		t.Fatalf("hits=%d cacheHits=%d, want 1 and 2", hits.Load(), c.CacheHits.Load())
	}
	if ua != DefaultUserAgent {
		t.Errorf("User-Agent = %q", ua)
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, "9492", "drivers_all.json.gz")); err != nil {
		t.Error(err)
	}
	if left, _ := filepath.Glob(filepath.Join(c.CacheDir, "9492", ".tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
}

func TestNoResultsIsCachedAsEmpty(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"detail":"No results found."}`))
	})
	for i := 0; i < 2; i++ {
		rows, _, err := c.CarData(context.Background(), 1, 2)
		if err != nil || len(rows) != 0 {
			t.Fatalf("rows=%v err=%v", rows, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestRetriesThenSucceeds(t *testing.T) {
	var n atomic.Int64
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(503)
		default:
			w.Write([]byte(`[]`))
		}
	})
	if _, err := c.Fetch(context.Background(), "x.json.gz", "laps?session_key=1", 0); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}
}

func TestRespectsRetryAfter(t *testing.T) {
	var n atomic.Int64
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`[]`))
	})
	start := time.Now()
	if _, err := c.Fetch(context.Background(), "x.json.gz", "q", 0); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("retried after %s, Retry-After said 1s", el)
	}
}

func TestGivesUpAndDoesNotCacheFailures(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	c.MaxRetries = 2
	if _, err := c.Fetch(context.Background(), "x.json.gz", "q", 0); err == nil {
		t.Fatal("expected error")
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", hits.Load())
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, "x.json.gz")); !os.IsNotExist(err) {
		t.Fatal("failure was cached")
	}
}

func TestRestrictedAndClientErrorsAreNotRetried(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"Session in progress, access is restricted to authenticated users"}`))
	})
	if _, err := c.Fetch(context.Background(), "x.json.gz", "q", 0); !errors.Is(err, ErrRestricted) {
		t.Fatalf("err = %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
	c2, hits2 := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) })
	if _, err := c2.Fetch(context.Background(), "x.json.gz", "q", 0); err == nil || hits2.Load() != 1 {
		t.Fatalf("422: err=%v hits=%d", err, hits2.Load())
	}
}

func TestOfflineAndMaxAge(t *testing.T) {
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[]`)) })
	c.Offline = true
	if _, err := c.Fetch(context.Background(), "x.json.gz", "q", 0); !errors.Is(err, ErrOffline) {
		t.Fatalf("offline miss: %v", err)
	}
	c.Offline = false
	c.Fetch(context.Background(), "x.json.gz", "q", 0)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(c.CacheDir, "x.json.gz"), old, old)
	c.Fetch(context.Background(), "x.json.gz", "q", time.Minute) // stale -> refetch
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
	c.Offline = true
	if _, err := c.Fetch(context.Background(), "x.json.gz", "q", time.Nanosecond); err != nil {
		t.Fatalf("offline must serve stale cache: %v", err)
	}
}

func TestRateLimitAndConcurrencyAreGlobal(t *testing.T) {
	var cur, peak atomic.Int64
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
		w.Write([]byte(`[]`))
	})
	c.Limiter = rate.NewLimiter(rate.Every(50*time.Millisecond), 1)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := c.CarData(context.Background(), 1, i); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if el := time.Since(start); el < 330*time.Millisecond { // 8 requests need 7 intervals of 50 ms
		t.Fatalf("8 requests finished in %s; limiter not shared", el)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2", peak.Load())
	}
	if hits.Load() != 8 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestContextCancelStopsWaiting(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	c.BaseDelay = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Fetch(ctx, "x.json.gz", "q", 0); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel did not interrupt backoff")
	}
}
