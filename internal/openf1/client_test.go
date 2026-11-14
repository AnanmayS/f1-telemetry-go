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
	"strconv"
	"strings"
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
		rows, _, err := c.CarData(context.Background(), 1, 2, Window{})
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
			if _, _, err := c.CarData(context.Background(), 1, i, Window{}); err != nil {
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

// A session too long for one response is fetched in time windows, joined in
// order, and cached as a single entry.
func TestTooLargeIsSplitByTime(t *testing.T) {
	start := time.Date(2023, 2, 23, 7, 0, 0, 0, time.UTC)
	var windows []string
	var mu sync.Mutex
	c, hits := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		parse := func(marker string) time.Time {
			i := strings.Index(raw, marker)
			if i < 0 {
				return time.Time{}
			}
			v := raw[i+len(marker):]
			if j := strings.IndexByte(v, '&'); j >= 0 {
				v = v[:j]
			}
			ts, err := time.Parse("2006-01-02T15:04:05Z", v)
			if err != nil {
				t.Errorf("bad date %q", v)
			}
			return ts
		}
		from, to := parse("date%3E="), parse("date%3C")
		// Refuse anything wider than 3 hours, including the unbounded query.
		if from.IsZero() || to.Sub(from) > 3*time.Hour {
			w.WriteHeader(422)
			w.Write([]byte(`{"detail":"Failed to retrieve information. You're likely asking for too much data at once."}`))
			return
		}
		mu.Lock()
		windows = append(windows, from.Format("15:04")+"-"+to.Format("15:04"))
		mu.Unlock()
		// One row per hour inside the window; empty windows are a 404.
		var rows []string
		for ts := from; ts.Before(to); ts = ts.Add(time.Hour) {
			if ts.Hour() >= 7 && ts.Hour() < 16 {
				rows = append(rows, `{"date":"`+ts.Format(time.RFC3339)+`","x":`+strconv.Itoa(ts.Hour())+`,"y":0,"z":0}`)
			}
		}
		if len(rows) == 0 {
			w.WriteHeader(404)
			w.Write([]byte(`{"detail":"No results found."}`))
			return
		}
		w.Write([]byte("[" + strings.Join(rows, ",") + "]"))
	})
	win := Window{start.Add(-time.Hour), start.Add(11 * time.Hour)} // 12 h -> 6 h -> 3 h pieces
	rows, _, err := c.Location(context.Background(), 9222, 10, win)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 9 {
		t.Fatalf("rows = %d, want 9 (07:00..15:00)", len(rows))
	}
	for i, r := range rows {
		if int(*r.X) != 7+i {
			t.Fatalf("rows out of order or duplicated at %d: x=%v", i, *r.X)
		}
	}
	if len(windows) != 4 || hits.Load() != 8 { // refused: unbounded, 12 h, two 6 h halves; then four 3 h quarters
		t.Fatalf("windows=%v hits=%d", windows, hits.Load())
	}
	before := hits.Load()
	if again, _, err := c.Location(context.Background(), 9222, 10, win); err != nil || len(again) != 9 || hits.Load() != before {
		t.Fatalf("joined result not cached: rows=%d err=%v extra hits=%d", len(again), err, hits.Load()-before)
	}
}

func TestTooLargeGivesUpAtMinimumWindow(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"detail":"You're likely asking for too much data at once."}`))
	})
	start := time.Date(2023, 2, 23, 7, 0, 0, 0, time.UTC)
	if _, _, err := c.CarData(context.Background(), 1, 1, Window{start, start.Add(15 * time.Minute)}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, "1", "car_data_1.json.gz")); !os.IsNotExist(err) {
		t.Fatal("failure was cached")
	}
}

// A 200 that is not a JSON array must fail without being cached, so the
// next run can succeed.
func TestNonArrayBodyIsNotCached(t *testing.T) {
	var n atomic.Int64
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.Write([]byte("<html>gateway error</html>"))
		case 2:
			w.Write([]byte(`{"detail":"something else"}`))
		default:
			w.Write([]byte(` [{"driver_number":1,"name_acronym":"VER"}]`))
		}
	})
	for i := 0; i < 2; i++ {
		if _, _, err := c.Drivers(context.Background(), 5); err == nil {
			t.Fatalf("attempt %d: bad body accepted", i)
		}
		if _, err := os.Stat(filepath.Join(c.CacheDir, "5", "drivers_all.json.gz")); !os.IsNotExist(err) {
			t.Fatal("bad body was cached")
		}
	}
	if d, _, err := c.Drivers(context.Background(), 5); err != nil || len(d) != 1 {
		t.Fatalf("recovery: %v %v", d, err)
	}
}
