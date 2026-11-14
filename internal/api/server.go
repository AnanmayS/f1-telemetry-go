// Package api is the HTTP layer: JSON endpoints over the store, caching
// headers, per-IP rate limiting, Prometheus metrics and static files.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/lru"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

// Options configures a Server.
type Options struct {
	Store      *store.Store
	Log        *slog.Logger
	StaticDir  string  // built frontend; "" disables static serving
	RateRPS    float64 // per-IP requests per second; 0 disables the limit
	RateBurst  int
	TrustProxy bool // take the client address from CF-Connecting-IP
	// RespCacheBytes bounds the cache of finished, gzip-compressed lap and
	// compare responses (0 = 64 MiB, negative = disabled).
	RespCacheBytes int64
}

// Server implements http.Handler.
type Server struct {
	store   *store.Store
	log     *slog.Logger
	mux     *http.ServeMux
	limiter *ipLimiter
	static  string
	metrics *metrics
	visits  *visitCounter

	// responses holds gzip bodies keyed by ETag. The ETag covers the segment
	// build, so entries of a rebuilt session simply stop being asked for.
	responses            *lru.Cache[string, []byte]
	respHits, respMisses atomic.Int64
}

const (
	defaultStep   = 5.0
	maxGridPoints = 20000
	// mismatchTolS is the PRD's bound on |final delta - official difference|.
	mismatchTolS = 0.05
)

var defaultChannels = []model.Channel{model.ChSpeed, model.ChThrottle, model.ChBrake, model.ChGear, model.ChRPM, model.ChDRS}

func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	s := &Server{store: o.Store, log: o.Log, mux: http.NewServeMux(), static: o.StaticDir, visits: newVisitCounter()}
	if o.RateRPS > 0 {
		s.limiter = newIPLimiter(o.RateRPS, max(o.RateBurst, 1), o.TrustProxy)
	} else {
		s.limiter = &ipLimiter{trustProxy: o.TrustProxy}
	}
	switch {
	case o.RespCacheBytes == 0:
		s.responses = lru.New[string, []byte](64 << 20)
	case o.RespCacheBytes > 0:
		s.responses = lru.New[string, []byte](o.RespCacheBytes)
	}
	s.metrics = newMetrics(o.Store, s.visits, s)

	s.route("GET /api/seasons", "seasons", s.handleSeasons)
	s.route("GET /api/sessions", "sessions", s.handleSessions)
	s.route("GET /api/sessions/{key}/drivers", "drivers", s.handleDrivers)
	s.route("GET /api/sessions/{key}/laps", "laps", s.handleLaps)
	s.route("GET /api/sessions/{key}/laps/{driver}/{lap}", "lap", s.handleLap)
	s.route("GET /api/compare", "compare", s.handleCompare)
	s.route("GET /api/health", "health", s.handleHealth)
	s.route("/api/", "api_unknown", func(w http.ResponseWriter, r *http.Request) *apiError {
		if r.Method != http.MethodGet {
			return &apiError{http.StatusMethodNotAllowed, "method not allowed", "method_not_allowed"}
		}
		return notFound("no such endpoint")
	})
	s.mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	s.mux.HandleFunc("/", s.handleStatic)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Run sweeps idle rate-limit buckets and rolls the daily visitor set until ctx is done.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if s.limiter.rps > 0 {
				s.limiter.sweep(now, 10*time.Minute)
			}
			if day, n, rolled := s.visits.roll(now); rolled {
				s.log.Info("daily visitors", "day", day, "unique_visitors", n)
			}
		}
	}
}

type handler func(w http.ResponseWriter, r *http.Request) *apiError

// route wraps a handler with panic recovery, rate limiting and metrics.
func (s *Server) route(pattern, name string, h handler) {
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				// Log the stack server-side; the client only sees a generic error.
				s.log.Error("panic in handler", "route", name, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
				if !sw.wrote {
					writeError(sw, &apiError{http.StatusInternalServerError, "internal error", "internal"})
				}
			}
			s.metrics.observe(name, sw.status, time.Since(start))
		}()
		if s.limiter.rps > 0 && !s.limiter.allow(s.limiter.clientIP(r), start) {
			sw.Header().Set("Retry-After", "1")
			writeError(sw, &apiError{http.StatusTooManyRequests, "rate limit exceeded", "rate_limited"})
			return
		}
		if err := h(sw, r); err != nil {
			writeError(sw, err)
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// storeError maps store and segment errors to API errors. Internal detail is
// logged by the store, never returned.
func storeError(err error) *apiError {
	switch {
	case errors.Is(err, store.ErrNoSession):
		return notFound("session not found")
	case errors.Is(err, store.ErrNoLap), errors.Is(err, segment.ErrNotFound):
		return notFound("lap not found")
	case errors.Is(err, store.ErrNoData):
		return &apiError{http.StatusNotFound, "lap has no telemetry", "no_telemetry"}
	case errors.Is(err, segment.ErrCorrupt):
		return &apiError{http.StatusInternalServerError, "stored data for this lap is damaged", "corrupt_data"}
	}
	return &apiError{http.StatusInternalServerError, "internal error", "internal"}
}

func pathInt(r *http.Request, name string, lo, hi int) (int, *apiError) {
	v, err := strconv.Atoi(r.PathValue(name))
	if err != nil || v < lo || v > hi {
		return 0, badRequest("invalid " + name)
	}
	return v, nil
}

// ---- listings ----

type sessionJSON struct {
	model.Session
	CircuitLengthM float64 `json:"circuit_length_m,omitempty"`
}

func (s *Server) handleSeasons(w http.ResponseWriter, r *http.Request) *apiError {
	if notModified(w, r, etag("seasons", fmt.Sprint(s.store.Version())), cacheList) {
		return nil
	}
	years := s.store.Years()
	if years == nil {
		years = []int{}
	}
	writeObject(w, r, map[string]any{"seasons": years})
	return nil
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) *apiError {
	year := 0
	if q := r.URL.Query().Get("year"); q != "" {
		y, err := strconv.Atoi(q)
		if err != nil || y < 1950 || y > 2200 {
			return badRequest("invalid year")
		}
		year = y
	}
	if notModified(w, r, etag("sessions", fmt.Sprint(s.store.Version()), fmt.Sprint(year)), cacheList) {
		return nil
	}
	list := s.store.Sessions(year)
	out := make([]sessionJSON, len(list))
	for i, x := range list {
		out[i] = sessionJSON{Session: x}
	}
	writeObject(w, r, map[string]any{"sessions": out})
	return nil
}

type driverJSON struct {
	model.Driver
	Laps       int  `json:"laps"`
	FastestLap *int `json:"fastest_lap"`
	FastestMs  *int `json:"fastest_ms"`
}

func (s *Server) handleDrivers(w http.ResponseWriter, r *http.Request) *apiError {
	key, aerr := pathInt(r, "key", 0, math.MaxInt32)
	if aerr != nil {
		return aerr
	}
	sess, err := s.store.Acquire(key)
	if err != nil {
		return storeError(err)
	}
	defer sess.Release()
	if notModified(w, r, etag("drivers", sess.Gen), cacheData) {
		return nil
	}
	out := make([]driverJSON, 0, len(sess.Meta.Drivers))
	for _, d := range sess.Meta.Drivers {
		dj := driverJSON{Driver: d, Laps: len(sess.Laps(d.Number))}
		if f, ok := store.Fastest(sess.Segment, d.Number); ok {
			n, ms := f.Number, int(f.OfficialMs)
			dj.FastestLap, dj.FastestMs = &n, &ms
		}
		out = append(out, dj)
	}
	writeObject(w, r, map[string]any{
		"session": sessionJSON{Session: sess.Meta.Session, CircuitLengthM: sess.Meta.CircuitLengthM},
		"drivers": out,
	})
	return nil
}

type lapJSON struct {
	Lap          int       `json:"lap"`
	DurationMs   *int32    `json:"duration_ms"`
	SectorsMs    [3]*int32 `json:"sectors_ms"`
	PitOut       bool      `json:"pit_out"`
	PitIn        bool      `json:"pit_in"`
	ValidTime    bool      `json:"valid_time"` // false = "unofficial time"
	HasTelemetry bool      `json:"has_telemetry"`
	IsFastest    bool      `json:"is_fastest"`
}

func lapToJSON(l model.Lap, fastest int) lapJSON {
	j := lapJSON{
		Lap: l.Number, PitOut: l.Flags&model.FlagPitOut != 0, PitIn: l.Flags&model.FlagPitIn != 0,
		ValidTime: l.ValidTime(), HasTelemetry: l.Samples > 0, IsFastest: l.Number == fastest,
	}
	if l.OfficialMs != model.NoTime && l.ValidTime() {
		v := l.OfficialMs
		j.DurationMs = &v
	}
	for i, sec := range l.SectorMs {
		if sec != model.NoTime {
			v := sec
			j.SectorsMs[i] = &v
		}
	}
	return j
}

func (s *Server) handleLaps(w http.ResponseWriter, r *http.Request) *apiError {
	key, aerr := pathInt(r, "key", 0, math.MaxInt32)
	if aerr != nil {
		return aerr
	}
	driver, err := strconv.Atoi(r.URL.Query().Get("driver"))
	if err != nil || driver < 0 || driver > 255 {
		return badRequest("driver is required and must be a driver number")
	}
	sess, err := s.store.Acquire(key)
	if err != nil {
		return storeError(err)
	}
	defer sess.Release()
	rows := sess.Laps(driver)
	if len(rows) == 0 {
		return notFound("driver has no laps in this session")
	}
	if notModified(w, r, etag("laps", sess.Gen, fmt.Sprint(driver)), cacheData) {
		return nil
	}
	fastest := -1
	if f, ok := store.Fastest(sess.Segment, driver); ok {
		fastest = f.Number
	}
	out := make([]lapJSON, len(rows))
	for i, l := range rows {
		out[i] = lapToJSON(l, fastest)
	}
	writeObject(w, r, map[string]any{"session_key": key, "driver": driver, "laps": out})
	return nil
}

// ---- lap data ----

// parseChannels reads a comma-separated channel list. "t" and "distance" are
// always part of a response and are not valid selections.
func parseChannels(q string) ([]model.Channel, string, *apiError) {
	if q == "" {
		return defaultChannels, "default", nil
	}
	var out []model.Channel
	seen := map[model.Channel]bool{}
	for _, name := range strings.Split(q, ",") {
		c, ok := model.ParseChannel(strings.TrimSpace(name))
		if !ok || c == model.ChT || c == model.ChDistance {
			return nil, "", badRequest("unknown channel " + strconv.Quote(name))
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	names := make([]string, len(out))
	for i, c := range out {
		names[i] = c.String()
	}
	return out, strings.Join(names, ","), nil
}

// resolveLap turns "fastest" or a number into a lap row.
func resolveLap(seg *segment.Segment, driver int, spec string) (model.Lap, *apiError) {
	if spec == "fastest" {
		l, ok := store.Fastest(seg, driver)
		if !ok {
			return l, &apiError{http.StatusNotFound, "driver has no timed lap with telemetry in this session", "no_fastest_lap"}
		}
		return l, nil
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 0 || n > 65535 {
		return model.Lap{}, badRequest(`lap must be a number or "fastest"`)
	}
	l, ok := seg.Lap(driver, n)
	if !ok {
		return l, notFound("lap not found")
	}
	return l, nil
}

func (s *Server) handleLap(w http.ResponseWriter, r *http.Request) *apiError {
	key, aerr := pathInt(r, "key", 0, math.MaxInt32)
	if aerr != nil {
		return aerr
	}
	driver, aerr := pathInt(r, "driver", 0, 255)
	if aerr != nil {
		return aerr
	}
	channels, chKey, aerr := parseChannels(r.URL.Query().Get("channels"))
	if aerr != nil {
		return aerr
	}
	sess, err := s.store.Acquire(key)
	if err != nil {
		return storeError(err)
	}
	defer sess.Release()
	row, aerr := resolveLap(sess.Segment, driver, r.PathValue("lap"))
	if aerr != nil {
		return aerr
	}
	tag := etag("lap", sess.Gen, fmt.Sprint(driver, row.Number), chKey)
	if notModified(w, r, tag, cacheData) || s.serveCached(w, r, tag) {
		return nil
	}
	ld, err := s.store.Lap(sess, driver, row.Number)
	if err != nil {
		return storeError(err)
	}
	fastest := -1
	if f, ok := store.Fastest(sess.Segment, driver); ok {
		fastest = f.Number
	}
	b := make([]byte, 0, 16<<10)
	b = append(b, `{"session_key":`...)
	b = strconv.AppendInt(b, int64(key), 10)
	b = append(b, `,"driver":`...)
	b = strconv.AppendInt(b, int64(driver), 10)
	b = append(b, `,"lap":`...)
	b = appendObject(b, lapToJSON(row, fastest))
	b = append(b, `,"samples":`...)
	b = strconv.AppendInt(b, int64(row.Samples), 10)
	// t: ms since the lap start. distance: metres from the lap start.
	b = append(b, `,"t":`...)
	b = appendInts(b, ld.Values[model.ChT], nil, row.StartMs, 1, 0)
	b = append(b, `,"distance":`...)
	b = appendInts(b, ld.Values[model.ChDistance], nil, 0, 100, 2)
	b = append(b, `,"channels":{`...)
	for i, c := range channels {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendQuote(b, c.String())
		b = append(b, ':')
		if c == model.ChDRS {
			b = appendDRS(b, ld.Values[c])
		} else {
			b = appendInts(b, ld.Values[c], ld.Valid[c], 0, 1, 0)
		}
	}
	b = append(b, "}}"...)
	s.writeAndCache(w, r, tag, b)
	return nil
}

// appendDRS derives "open" (1) or "closed" (0) from the raw feed code.
func appendDRS(b []byte, codes []int64) []byte {
	b = append(b, '[')
	for i, c := range codes {
		if i > 0 {
			b = append(b, ',')
		}
		if model.DRSOpen(c) {
			b = append(b, '1')
		} else {
			b = append(b, '0')
		}
	}
	return append(b, ']')
}

// ---- compare ----

type lapRef struct {
	session, driver int
	lap             string
}

func parseRef(q string) (lapRef, *apiError) {
	parts := strings.Split(q, ":")
	if len(parts) != 3 {
		return lapRef{}, badRequest(`lap reference must be "session:driver:lap"`)
	}
	sk, err1 := strconv.Atoi(parts[0])
	dr, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || sk < 0 || dr < 0 || dr > 255 {
		return lapRef{}, badRequest(`lap reference must be "session:driver:lap"`)
	}
	return lapRef{sk, dr, parts[2]}, nil
}

type side struct {
	sess   store.Session
	row    model.Lap
	driver model.Driver
	data   *model.LapData
}

func (s *Server) loadSide(ref lapRef) (side, *apiError) {
	sess, err := s.store.Acquire(ref.session)
	if err != nil {
		return side{}, storeError(err)
	}
	row, aerr := resolveLap(sess.Segment, ref.driver, ref.lap)
	if aerr != nil {
		sess.Release()
		return side{}, aerr
	}
	sd := side{sess: sess, row: row, driver: model.Driver{Number: ref.driver}}
	for _, d := range sess.Meta.Drivers {
		if d.Number == ref.driver {
			sd.driver = d
		}
	}
	return sd, nil
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) *apiError {
	q := r.URL.Query()
	refA, aerr := parseRef(q.Get("a"))
	if aerr != nil {
		return aerr
	}
	refB, aerr := parseRef(q.Get("b"))
	if aerr != nil {
		return aerr
	}
	channels, chKey, aerr := parseChannels(q.Get("channels"))
	if aerr != nil {
		return aerr
	}
	step := defaultStep
	if v := q.Get("step"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || f < 1 || f > 500 {
			return badRequest("step must be between 1 and 500 metres")
		}
		step = f
	}
	a, aerr := s.loadSide(refA)
	if aerr != nil {
		return aerr
	}
	defer a.sess.Release()
	b, aerr := s.loadSide(refB)
	if aerr != nil {
		return aerr
	}
	defer b.sess.Release()

	tag := etag("compare", a.sess.Gen, fmt.Sprint(refA.driver, a.row.Number), b.sess.Gen, fmt.Sprint(refB.driver, b.row.Number), chKey, strconv.FormatFloat(step, 'g', -1, 64))
	if notModified(w, r, tag, cacheData) || s.serveCached(w, r, tag) {
		return nil
	}
	var err error
	if a.data, err = s.store.Lap(a.sess, refA.driver, a.row.Number); err != nil {
		return storeError(err)
	}
	if b.data, err = s.store.Lap(b.sess, refB.driver, b.row.Number); err != nil {
		return storeError(err)
	}
	lenA, lenB := a.sess.Meta.CircuitLengthM, b.sess.Meta.CircuitLengthM
	if lenA <= 0 || lenB <= 0 {
		return &apiError{http.StatusUnprocessableEntity, "a session has no measured circuit length, so its laps cannot be aligned", "no_circuit_length"}
	}
	if lenA/step > maxGridPoints {
		return badRequest("step is too small for this circuit")
	}
	cmp := laps.Compare(a.data, b.data, lenA, lenB, step, channels)

	// The final delta of two normalised laps must match official timing.
	official := math.NaN()
	if a.row.ValidTime() && b.row.ValidTime() {
		official = float64(b.row.OfficialMs-a.row.OfficialMs) / 1000
		if a.row.DistanceOK() && b.row.DistanceOK() && !(math.Abs(cmp.FinalDeltaS-official) <= mismatchTolS) {
			s.metrics.mismatch.Inc()
			s.log.Warn("lap-time mismatch", "a", q.Get("a"), "b", q.Get("b"), "final_delta_s", cmp.FinalDeltaS, "official_delta_s", official)
		}
	}

	out := make([]byte, 0, 160<<10)
	out = append(out, `{"a":`...)
	out = appendSide(out, a)
	out = append(out, `,"b":`...)
	out = appendSide(out, b)
	out = append(out, `,"step_m":`...)
	out = appendFloat(out, step, 3)
	out = append(out, `,"length_m":`...)
	out = appendFloat(out, lenA, 2)
	out = append(out, `,"same_circuit":`...)
	out = strconv.AppendBool(out, a.sess.Meta.Session.Circuit == b.sess.Meta.Session.Circuit)
	// aligned: both laps are normalised, so the delta is anchored to official timing.
	out = append(out, `,"aligned":`...)
	out = strconv.AppendBool(out, a.row.DistanceOK() && b.row.DistanceOK())
	out = append(out, `,"official_delta_s":`...)
	out = appendScalar(out, official, 3)
	out = append(out, `,"final_delta_s":`...)
	out = appendScalar(out, cmp.FinalDeltaS, 3)
	out = append(out, `,"distance":`...)
	out = appendFloats(out, cmp.DistanceM, 2)
	out = append(out, `,"delta":`...)
	out = appendFloats(out, cmp.DeltaS, 3)
	out = append(out, `,"time":{"a":`...)
	out = appendFloats(out, cmp.A.TimeS, 3)
	out = append(out, `,"b":`...)
	out = appendFloats(out, cmp.B.TimeS, 3)
	out = append(out, `},"channels":{`...)
	for i, c := range channels {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendQuote(out, c.String())
		va, vb := cmp.A.Values[c], cmp.B.Values[c]
		decimals := 1
		switch {
		case c == model.ChDRS:
			va, vb = drsOpen(va), drsOpen(vb)
			decimals = 0
		case c.Step(), c == model.ChX, c == model.ChY, c == model.ChZ, c == model.ChRPM:
			decimals = 0
		}
		out = append(out, `:{"a":`...)
		out = appendFloats(out, va, decimals)
		out = append(out, `,"b":`...)
		out = appendFloats(out, vb, decimals)
		out = append(out, '}')
	}
	out = append(out, "}}"...)
	s.writeAndCache(w, r, tag, out)
	return nil
}

func drsOpen(codes []float64) []float64 {
	out := make([]float64, len(codes))
	for i, c := range codes {
		switch {
		case math.IsNaN(c):
			out[i] = c
		case model.DRSOpen(int64(c)):
			out[i] = 1
		}
	}
	return out
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) *apiError {
	st := s.store.Stats()
	w.Header().Set("Cache-Control", cacheNone)
	writeObject(w, r, map[string]any{"status": "ok", "manifest_version": st.ManifestVersion, "segments": st.Segments})
	return nil
}

// ---- static files ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.static == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	clean := filepath.Clean("/" + r.URL.Path)
	full := filepath.Join(s.static, clean)
	if st, err := os.Stat(full); err == nil && !st.IsDir() && clean != "/index.html" {
		if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed by the build
		}
		http.ServeFile(w, r, full)
		return
	}
	if filepath.Ext(clean) != "" && clean != "/index.html" {
		http.NotFound(w, r)
		return
	}
	// Everything else is the single-page app. Count the visit here: this is
	// the only request every page view makes exactly once.
	q := r.URL.Query()
	s.visits.pageView(s.limiter.clientIP(r), q.Get("a") != "" && q.Get("b") != "", time.Now())
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filepath.Join(s.static, "index.html"))
}
