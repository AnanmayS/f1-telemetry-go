// Package openf1 is a polite client for the OpenF1 REST API: one global rate
// limiter, bounded concurrency, retries with backoff, and a write-once gzip
// cache of every raw response.
package openf1

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

const (
	DefaultBaseURL   = "https://api.openf1.org/v1/"
	DefaultUserAgent = "ghostlap/0.1 (+https://github.com/AnanmayS/f1-telemetry-go)"
	// DefaultPerMinute stays under OpenF1's published free limit of 30
	// requests per minute (and far under 3 per second). See docs/DATA.md.
	DefaultPerMinute = 28.5
	DefaultInFlight  = 2
)

var (
	// ErrRestricted means the API refused an unauthenticated request, which
	// OpenF1 does for all queries while a session is live.
	ErrRestricted = errors.New("openf1: access restricted (a session is probably live; retry later)")
	// ErrTooLarge means the API refused to build a response that big (HTTP
	// 422 on very long sessions). Callers split the query by time window.
	ErrTooLarge = errors.New("openf1: response too large for one request")
	// ErrOffline is returned for a cache miss when the client is offline.
	ErrOffline = errors.New("openf1: not in cache and client is offline")
)

// Client fetches and caches API responses. Safe for concurrent use.
type Client struct {
	BaseURL    string
	CacheDir   string
	UserAgent  string
	HTTP       *http.Client
	Limiter    *rate.Limiter
	MaxRetries int
	BaseDelay  time.Duration // first backoff step
	Offline    bool          // serve from cache only
	Log        *slog.Logger

	sem chan struct{}

	Requests  atomic.Int64 // HTTP requests sent
	CacheHits atomic.Int64
}

// New returns a client limited to perMinute requests per minute and inFlight
// concurrent requests.
func New(cacheDir string, perMinute float64, inFlight int) *Client {
	if perMinute <= 0 {
		perMinute = DefaultPerMinute
	}
	if inFlight <= 0 {
		inFlight = DefaultInFlight
	}
	return &Client{
		BaseURL:    DefaultBaseURL,
		CacheDir:   cacheDir,
		UserAgent:  DefaultUserAgent,
		HTTP:       &http.Client{Timeout: 3 * time.Minute},
		Limiter:    rate.NewLimiter(rate.Limit(perMinute/60), 1),
		MaxRetries: 6,
		BaseDelay:  2 * time.Second,
		Log:        slog.Default(),
		sem:        make(chan struct{}, inFlight),
	}
}

// Fetch returns the JSON body for query (e.g. "laps?session_key=9492"),
// reading CacheDir/cachePath when present and no older than maxAge
// (0 = never expires). A "no results" 404 is cached as an empty array.
func (c *Client) Fetch(ctx context.Context, cachePath, query string, maxAge time.Duration) ([]byte, error) {
	if body, ok, err := c.cached(cachePath, maxAge); ok || err != nil {
		return body, err
	}
	body, err := c.get(ctx, query)
	if err != nil {
		return nil, err
	}
	if err := checkArray(query, body); err != nil {
		return nil, err
	}
	return body, writeGzip(filepath.Join(c.CacheDir, cachePath), body)
}

// checkArray rejects a 200 response that is not a JSON array (a proxy error
// page, say) before it can reach the write-once cache and poison every rerun.
func checkArray(query string, body []byte) error {
	if t := bytes.TrimSpace(body); len(t) == 0 || t[0] != '[' || !json.Valid(t) {
		return fmt.Errorf("openf1: %s: response is not a JSON array: %s", query, snippet(body))
	}
	return nil
}

// cached returns the cache entry if it is usable. In offline mode a miss is
// an error.
func (c *Client) cached(cachePath string, maxAge time.Duration) (body []byte, ok bool, err error) {
	full := filepath.Join(c.CacheDir, cachePath)
	if st, err := os.Stat(full); err == nil && (maxAge == 0 || c.Offline || time.Since(st.ModTime()) < maxAge) {
		if body, err := readGzip(full); err == nil {
			c.CacheHits.Add(1)
			return body, true, nil
		} else {
			c.Log.Warn("unreadable cache file, refetching", "path", full, "err", err)
		}
	}
	if c.Offline {
		return nil, false, fmt.Errorf("%w: %s", ErrOffline, cachePath)
	}
	return nil, false, nil
}

// minWindow is the smallest time slice a too-large query is split into.
const minWindow = 5 * time.Minute

// FetchWindowed is Fetch for time-series endpoints. If the API refuses the
// whole response as too large, the query is split into time windows over
// [from, to) (halving until each piece is accepted) and the rows are joined.
// The cache holds the joined result, so the split happens once.
func (c *Client) FetchWindowed(ctx context.Context, cachePath, query string, from, to time.Time) ([]byte, error) {
	if body, ok, err := c.cached(cachePath, 0); ok || err != nil {
		return body, err
	}
	body, err := c.get(ctx, query)
	if errors.Is(err, ErrTooLarge) && to.After(from) {
		c.Log.Info("response too large, splitting by time", "query", query)
		var rows []json.RawMessage
		if rows, err = c.getWindow(ctx, query, from, to); err == nil {
			if rows == nil {
				rows = []json.RawMessage{}
			}
			body, err = json.Marshal(rows)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := checkArray(query, body); err != nil {
		return nil, err
	}
	return body, writeGzip(filepath.Join(c.CacheDir, cachePath), body)
}

func (c *Client) getWindow(ctx context.Context, query string, from, to time.Time) ([]json.RawMessage, error) {
	const layout = "2006-01-02T15:04:05Z"
	q := fmt.Sprintf("%s&date%%3E=%s&date%%3C%s", query, from.UTC().Format(layout), to.UTC().Format(layout))
	body, err := c.get(ctx, q)
	if errors.Is(err, ErrTooLarge) && to.Sub(from) > minWindow {
		mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		a, err := c.getWindow(ctx, query, from, mid)
		if err != nil {
			return nil, err
		}
		b, err := c.getWindow(ctx, query, mid, to)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}
	if err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("openf1: decoding %s: %w", q, err)
	}
	return rows, nil
}

func (c *Client) get(ctx context.Context, query string) ([]byte, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := c.BaseDelay << (attempt - 1)
			delay = delay/2 + rand.N(delay/2+1) // full backoff with jitter in the upper half
			var ra retryAfter
			if errors.As(lastErr, &ra) && time.Duration(ra) > delay {
				delay = time.Duration(ra)
			}
			c.Log.Warn("retrying", "query", query, "attempt", attempt, "delay", delay.Round(time.Millisecond), "err", lastErr)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err := c.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
		body, retry, err := c.once(ctx, query)
		if err == nil {
			return body, nil
		}
		if !retry || ctx.Err() != nil {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("openf1: %s: giving up after %d attempts: %w", query, c.MaxRetries+1, lastErr)
}

// retryAfter is a retryable error carrying the server's Retry-After hint.
type retryAfter time.Duration

func (r retryAfter) Error() string {
	return fmt.Sprintf("rate limited, retry after %s", time.Duration(r))
}

func (c *Client) once(ctx context.Context, query string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+query, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	c.Requests.Add(1)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, false, nil
	case resp.StatusCode == http.StatusNotFound && bytes.Contains(body, []byte("No results found")):
		return []byte("[]"), false, nil
	case resp.StatusCode == http.StatusUnprocessableEntity && bytes.Contains(body, []byte("too much data")):
		return nil, false, fmt.Errorf("%w (%s)", ErrTooLarge, query)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("%w (http %d: %s)", ErrRestricted, resp.StatusCode, snippet(body))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, perr := strconv.Atoi(strings.TrimSpace(s)); perr == nil && secs > 0 {
				return nil, true, retryAfter(time.Duration(secs) * time.Second)
			}
			if at, perr := http.ParseTime(s); perr == nil && time.Until(at) > 0 {
				return nil, true, retryAfter(time.Until(at))
			}
		}
		return nil, true, fmt.Errorf("openf1: http %d: %s", resp.StatusCode, snippet(body))
	default:
		return nil, false, fmt.Errorf("openf1: %s: http %d: %s", query, resp.StatusCode, snippet(body))
	}
}

func snippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return strings.TrimSpace(string(b))
}

func readGzip(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// writeGzip writes atomically so an interrupted run never leaves a partial
// cache entry behind.
func writeGzip(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	_, err = zw.Write(body)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// WriteCache stores body as the cache entry for cachePath, as if it had been
// fetched. It exists for tools and tests that seed a raw cache.
func (c *Client) WriteCache(cachePath string, body []byte) error {
	return writeGzip(filepath.Join(c.CacheDir, cachePath), body)
}
