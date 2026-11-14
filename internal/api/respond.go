package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Cache lifetimes. Lap data is immutable for a given segment build, and the
// ETag changes when a session is rebuilt, so a week is safe; listings grow
// as sessions are ingested.
const (
	cacheData = "public, max-age=604800, s-maxage=604800, stale-while-revalidate=86400"
	cacheList = "public, max-age=300, s-maxage=300"
	// cacheLive is for sessions still being ingested: their laps change every
	// second or so, so clients must come back.
	cacheLive = "public, max-age=2, s-maxage=2"
	cacheNone = "no-store"
)

type apiError struct {
	Status int    `json:"-"`
	Msg    string `json:"error"`
	Code   string `json:"code"`
}

func (e *apiError) Error() string { return e.Msg }

func badRequest(msg string) *apiError { return &apiError{http.StatusBadRequest, msg, "bad_request"} }
func notFound(msg string) *apiError   { return &apiError{http.StatusNotFound, msg, "not_found"} }

func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheNone)
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(e)
}

// etag derives a strong validator from the parts that determine a response.
func etag(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// notModified answers a conditional request and reports whether it did.
func notModified(w http.ResponseWriter, r *http.Request, tag, cache string) bool {
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("Vary", "Accept-Encoding")
	for _, c := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		c = strings.TrimPrefix(strings.TrimSpace(c), "W/")
		if c == tag || c == "*" {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}
	return false
}

var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return zw
}}

// writeJSON sends body, gzip-compressed when the client accepts it. Cache
// headers must already be set (see notModified).
func writeJSON(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || len(body) < 512 {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	zw := gzipPool.Get().(*gzip.Writer)
	zw.Reset(w)
	zw.Write(body)
	zw.Close()
	gzipPool.Put(zw)
}

func writeObject(w http.ResponseWriter, r *http.Request, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, &apiError{http.StatusInternalServerError, "internal error", "internal"})
		return
	}
	writeJSON(w, r, body)
}

// Number arrays are written by hand: the big responses are almost entirely
// numbers, and this avoids reflection and intermediate []any values.

// appendFloats writes vals with the given number of decimals; NaN becomes null.
func appendFloats(b []byte, vals []float64, decimals int) []byte {
	b = append(b, '[')
	for i, v := range vals {
		if i > 0 {
			b = append(b, ',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b = append(b, "null"...)
			continue
		}
		b = appendFloat(b, v, decimals)
	}
	return append(b, ']')
}

func appendFloat(b []byte, v float64, decimals int) []byte {
	if decimals == 0 {
		return strconv.AppendInt(b, int64(math.Round(v)), 10)
	}
	start := len(b)
	b = strconv.AppendFloat(b, v, 'f', decimals, 64)
	// Trim trailing zeros: 250.0 -> 250, 0.250 -> 0.25.
	end := len(b)
	for end > start && b[end-1] == '0' {
		end--
	}
	if end > start && b[end-1] == '.' {
		end--
	}
	b = b[:end]
	if string(b[start:]) == "-0" {
		b = append(b[:start], '0')
	}
	return b
}

// appendInts writes vals scaled by 1/div; missing entries become null.
func appendInts(b []byte, vals []int64, valid []bool, offset int64, div float64, decimals int) []byte {
	b = append(b, '[')
	for i, v := range vals {
		if i > 0 {
			b = append(b, ',')
		}
		switch {
		case valid != nil && !valid[i]:
			b = append(b, "null"...)
		case div == 1:
			b = strconv.AppendInt(b, v-offset, 10)
		default:
			b = appendFloat(b, float64(v-offset)/div, decimals)
		}
	}
	return append(b, ']')
}

// appendScalar writes one number, or null for NaN.
func appendScalar(b []byte, v float64, decimals int) []byte {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return append(b, "null"...)
	}
	return appendFloat(b, v, decimals)
}

// appendObject appends v's JSON encoding. v must be marshalable.
func appendObject(b []byte, v any) []byte {
	j, err := json.Marshal(v)
	if err != nil {
		return append(b, "null"...)
	}
	return append(b, j...)
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

func writeGzipped(w http.ResponseWriter, gz []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
	w.Write(gz)
}

// serveCached answers from the response cache and reports whether it did.
// Cache headers must already be set.
func (s *Server) serveCached(w http.ResponseWriter, r *http.Request, tag string) bool {
	if s.responses == nil || !acceptsGzip(r) {
		return false
	}
	gz, ok := s.responses.Get(tag)
	if !ok {
		s.respMisses.Add(1)
		return false
	}
	s.respHits.Add(1)
	writeGzipped(w, gz)
	return true
}

// writeAndCache sends body and keeps its gzip form for the next request.
func (s *Server) writeAndCache(w http.ResponseWriter, r *http.Request, tag string, body []byte) {
	if s.responses == nil || !acceptsGzip(r) || len(body) < 512 {
		writeJSON(w, r, body)
		return
	}
	var buf bytes.Buffer
	buf.Grow(len(body) / 3)
	zw := gzipPool.Get().(*gzip.Writer)
	zw.Reset(&buf)
	zw.Write(body)
	zw.Close()
	gzipPool.Put(zw)
	s.responses.Put(tag, buf.Bytes(), int64(buf.Len()))
	writeGzipped(w, buf.Bytes())
}

// cacheFor picks the cache lifetime for data from the given segment builds.
func cacheFor(gens ...string) string {
	for _, g := range gens {
		if strings.HasPrefix(g, "live-") {
			return cacheLive
		}
	}
	return cacheData
}
