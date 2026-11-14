package api

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// lap builds a synthetic lap of lengthM metres driven at constant speed in
// durMs, sampled every 250 ms with one padding sample either side.
func lap(driver, number int, startMs int64, durMs int32, lengthM float64, valid bool) *model.LapData {
	l := model.Lap{Driver: driver, Number: number, StartMs: startMs, EndMs: startMs + int64(durMs), OfficialMs: model.NoTime,
		SectorMs: [3]int32{model.NoTime, model.NoTime, model.NoTime}}
	if valid {
		l.OfficialMs, l.Flags = durMs, model.FlagValidTime|model.FlagDistanceOK
		l.SectorMs = [3]int32{durMs / 3, durMs / 3, durMs - 2*(durMs/3)}
	}
	ld := &model.LapData{Lap: l}
	cmPerMs := lengthM * 100 / float64(durMs)
	speed := int64(math.Round(cmPerMs * 36))
	for t := startMs - 250; t <= startMs+int64(durMs)+250; t += 250 {
		i := len(ld.Values[model.ChT])
		ld.Values[model.ChT] = append(ld.Values[model.ChT], t)
		ld.Values[model.ChDistance] = append(ld.Values[model.ChDistance], int64(math.Round(float64(t-startMs)*cmPerMs)))
		ld.Values[model.ChSpeed] = append(ld.Values[model.ChSpeed], speed)
		ld.Values[model.ChRPM] = append(ld.Values[model.ChRPM], 11000)
		ld.Values[model.ChGear] = append(ld.Values[model.ChGear], int64(1+i/50%8))
		ld.Values[model.ChThrottle] = append(ld.Values[model.ChThrottle], 100)
		ld.Valid[model.ChThrottle] = append(ld.Valid[model.ChThrottle], i != 3) // one missing reading
		ld.Values[model.ChBrake] = append(ld.Values[model.ChBrake], int64(i/20%2))
		drs := int64(8)
		if i > 100 {
			drs = 12
		}
		ld.Values[model.ChDRS] = append(ld.Values[model.ChDRS], drs)
		ld.Values[model.ChX] = append(ld.Values[model.ChX], int64(i*10))
		ld.Values[model.ChY] = append(ld.Values[model.ChY], int64(-i*10))
		ld.Values[model.ChZ] = append(ld.Values[model.ChZ], 5)
	}
	ld.Lap.Samples = len(ld.Values[model.ChT])
	return ld
}

func emptyLap(driver, number int) *model.LapData {
	ld := &model.LapData{Lap: model.Lap{Driver: driver, Number: number, OfficialMs: 70_000, Flags: model.FlagValidTime}}
	for c := range ld.Values {
		ld.Values[c] = []int64{}
	}
	return ld
}

func addSession(t testing.TB, dir string, key, year int, circuit string, lengthM float64, laps []*model.LapData) store.Entry {
	t.Helper()
	meta := segment.Meta{
		Session: model.Session{Key: key, MeetingKey: key / 10, Year: year, MeetingName: circuit + " Grand Prix", Name: "Qualifying", Type: "Qualifying",
			Circuit: circuit, Country: "Japan", Start: time.Date(year, 4, 6, 6, 0, 0, 0, time.UTC), End: time.Date(year, 4, 6, 7, 0, 0, 0, time.UTC)},
		Drivers: []model.Driver{
			{Number: 1, Acronym: "VER", Name: "Max VERSTAPPEN", Team: "Red Bull Racing", Colour: "3671C6"},
			{Number: 4, Acronym: "NOR", Name: "Lando NORRIS", Team: "McLaren", Colour: "FF8000"},
			{Number: 99, Acronym: "RES", Name: "Reserve DRIVER"},
		},
		CircuitLengthM: lengthM,
	}
	data, err := segment.Build(meta, laps)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, store.SegmentDir), 0o755)
	name, sum := store.SegmentFileName(key, data)
	if err := segment.WriteFile(filepath.Join(dir, store.SegmentDir, name), data); err != nil {
		t.Fatal(err)
	}
	man, _ := store.LoadManifest(dir)
	e := store.Entry{SessionKey: key, Year: year, File: name, Size: int64(len(data)), SHA256: sum}
	man.Put(e)
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
	return e
}

// fixture: session 100 (2024, 5000 m) and session 200 (2023, same circuit
// measured as 5010 m), plus session 300 at another circuit.
func fixture(t testing.TB, o Options) (*Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	addSession(t, dir, 100, 2024, "Suzuka", 5000, []*model.LapData{
		lap(1, 1, 10_000, 120_000, 5000, false), // out-lap, no official time
		lap(1, 2, 200_000, 90_000, 5000, true),
		lap(1, 3, 300_000, 88_000, 5000, true), // VER fastest
		emptyLap(1, 4),
		lap(4, 1, 200_000, 88_500, 5000, true), // NOR fastest
	})
	addSession(t, dir, 200, 2023, "Suzuka", 5010, []*model.LapData{lap(1, 1, 50_000, 89_250, 5010, true)})
	addSession(t, dir, 300, 2024, "Monza", 5793, []*model.LapData{lap(1, 1, 50_000, 80_000, 5793, true)})
	st, err := store.Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	o.Store, o.Log = st, quiet
	return New(o), st, dir
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func get(t testing.TB, h http.Handler, url string, hdr ...string) resp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header(), rec.Body.Bytes()}
}

func decode[T any](t testing.TB, r resp) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(r.body, &v); err != nil {
		t.Fatalf("invalid JSON (%v): %.300s", err, r.body)
	}
	return v
}

type compareResp struct {
	A, B struct {
		SessionKey int `json:"session_key"`
		Driver     model.Driver
		Lap        lapJSON
	}
	StepM         float64   `json:"step_m"`
	LengthM       float64   `json:"length_m"`
	SameCircuit   bool      `json:"same_circuit"`
	Aligned       bool      `json:"aligned"`
	OfficialDelta *float64  `json:"official_delta_s"`
	FinalDelta    *float64  `json:"final_delta_s"`
	Distance      []float64 `json:"distance"`
	Delta         []*float64
	Time          struct{ A, B []*float64 }
	Channels      map[string]struct{ A, B []*float64 }
}

func TestListings(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	r := get(t, s, "/api/seasons")
	if r.code != 200 || string(r.body) != `{"seasons":[2024,2023]}` {
		t.Fatalf("seasons: %d %s", r.code, r.body)
	}
	sess := decode[struct{ Sessions []model.Session }](t, get(t, s, "/api/sessions?year=2024"))
	if len(sess.Sessions) != 2 || sess.Sessions[0].Circuit == "" || sess.Sessions[0].Year != 2024 {
		t.Fatalf("sessions: %+v", sess)
	}
	if all := decode[struct{ Sessions []model.Session }](t, get(t, s, "/api/sessions")); len(all.Sessions) != 3 {
		t.Fatalf("all sessions: %d", len(all.Sessions))
	}
	if r := get(t, s, "/api/sessions?year=1999"); string(r.body) != `{"sessions":[]}` {
		t.Fatalf("empty year must be an empty array: %s", r.body)
	}
	d := decode[struct {
		Session struct {
			CircuitLengthM float64 `json:"circuit_length_m"`
		}
		Drivers []driverJSON
	}](t, get(t, s, "/api/sessions/100/drivers"))
	if d.Session.CircuitLengthM != 5000 || len(d.Drivers) != 3 {
		t.Fatalf("drivers: %+v", d)
	}
	if v := d.Drivers[0]; v.Acronym != "VER" || v.Laps != 4 || v.FastestLap == nil || *v.FastestLap != 3 || *v.FastestMs != 88_000 {
		t.Fatalf("VER: %+v", v)
	}
	if v := d.Drivers[2]; v.Laps != 0 || v.FastestLap != nil || v.FastestMs != nil {
		t.Fatalf("driver without laps: %+v", v)
	}
	l := decode[struct{ Laps []lapJSON }](t, get(t, s, "/api/sessions/100/laps?driver=1"))
	if len(l.Laps) != 4 {
		t.Fatalf("laps: %+v", l)
	}
	if l.Laps[0].DurationMs != nil || l.Laps[0].ValidTime || !l.Laps[0].HasTelemetry {
		t.Errorf("out-lap: %+v", l.Laps[0])
	}
	if !l.Laps[2].IsFastest || l.Laps[1].IsFastest || *l.Laps[2].DurationMs != 88_000 || *l.Laps[2].SectorsMs[2] != 29_334 {
		t.Errorf("fastest flag / times: %+v", l.Laps[2])
	}
	if l.Laps[3].HasTelemetry || l.Laps[3].IsFastest {
		t.Errorf("lap without telemetry: %+v", l.Laps[3])
	}
	h := decode[map[string]any](t, get(t, s, "/api/health"))
	if h["status"] != "ok" || h["segments"] != float64(3) {
		t.Fatalf("health: %v", h)
	}
}

func TestLapSamples(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	type lapResp struct {
		Lap      lapJSON
		Samples  int
		T        []int64
		Distance []float64
		Channels map[string][]*float64
	}
	r := decode[lapResp](t, get(t, s, "/api/sessions/100/laps/1/fastest?channels=speed,throttle,drs,brake"))
	if r.Lap.Lap != 3 || r.Samples != len(r.T) || len(r.Distance) != r.Samples {
		t.Fatalf("lap: %+v samples=%d t=%d", r.Lap, r.Samples, len(r.T))
	}
	if r.T[0] != -250 || r.T[1] != 0 || r.Distance[1] != 0 {
		t.Errorf("t/distance must be relative to the lap start: t=%v d=%v", r.T[:2], r.Distance[:2])
	}
	if len(r.Channels) != 4 || r.Channels["throttle"][3] != nil || *r.Channels["throttle"][4] != 100 {
		t.Errorf("missing value must be null: %v", r.Channels["throttle"][:5])
	}
	if *r.Channels["drs"][0] != 0 || *r.Channels["drs"][150] != 1 {
		t.Error("drs must be derived open/closed, not the raw code")
	}
	byNumber := decode[lapResp](t, get(t, s, "/api/sessions/100/laps/1/3"))
	if byNumber.Lap.Lap != 3 || len(byNumber.Channels) != len(defaultChannels) {
		t.Errorf("default channels: %d", len(byNumber.Channels))
	}
}

func TestCompare(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	c := decode[compareResp](t, get(t, s, "/api/compare?a=100:1:fastest&b=100:4:fastest"))
	n := len(c.Distance)
	if n != 1001 || c.Distance[0] != 0 || c.Distance[n-1] != 5000 || c.StepM != 5 || c.LengthM != 5000 {
		t.Fatalf("grid: n=%d last=%v step=%v", n, c.Distance[n-1], c.StepM)
	}
	if c.A.Driver.Acronym != "VER" || c.A.Lap.Lap != 3 || c.B.Driver.Acronym != "NOR" || c.B.Lap.Lap != 1 || !c.A.Lap.IsFastest {
		t.Fatalf("sides: %+v / %+v", c.A, c.B)
	}
	if !c.Aligned || !c.SameCircuit || *c.OfficialDelta != 0.5 || *c.FinalDelta != 0.5 {
		t.Fatalf("delta: aligned=%v official=%v final=%v", c.Aligned, *c.OfficialDelta, *c.FinalDelta)
	}
	if len(c.Delta) != n || len(c.Time.A) != n || math.Abs(*c.Delta[500]-0.25) > 0.002 || *c.Time.A[n-1] != 88 || *c.Time.B[n-1] != 88.5 {
		t.Fatalf("delta series: mid=%v", *c.Delta[500])
	}
	if len(c.Channels) != len(defaultChannels) {
		t.Fatalf("channels: %d", len(c.Channels))
	}
	for name, ch := range c.Channels {
		if len(ch.A) != n || len(ch.B) != n {
			t.Fatalf("%s: lengths %d/%d", name, len(ch.A), len(ch.B))
		}
	}
	if v := *c.Channels["drs"].A[n-1]; v != 1 {
		t.Errorf("drs at lap end = %v, want open (1)", v)
	}

	// Different sessions at the same circuit: B is mapped proportionally and
	// the final delta still equals the official difference.
	x := decode[compareResp](t, get(t, s, "/api/compare?a=100:1:3&b=200:1:1&channels=speed,x,y&step=10"))
	if !x.SameCircuit || !x.Aligned || math.Abs(*x.FinalDelta-1.25) > 1e-9 || len(x.Distance) != 501 || len(x.Channels) != 3 {
		t.Fatalf("cross-session: same=%v final=%v n=%d", x.SameCircuit, *x.FinalDelta, len(x.Distance))
	}
	if d := decode[compareResp](t, get(t, s, "/api/compare?a=100:1:3&b=300:1:1")); d.SameCircuit {
		t.Error("different circuits reported as the same")
	}
	// A lap without an official time is still comparable, but not "aligned".
	u := decode[compareResp](t, get(t, s, "/api/compare?a=100:1:1&b=100:1:3"))
	if u.Aligned || u.OfficialDelta != nil || u.FinalDelta == nil {
		t.Fatalf("unofficial lap: aligned=%v official=%v", u.Aligned, u.OfficialDelta)
	}
}

func TestErrors(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	cases := []struct {
		url  string
		code int
		api  string
	}{
		{"/api/sessions?year=abc", 400, "bad_request"},
		{"/api/sessions/999/drivers", 404, "not_found"},
		{"/api/sessions/abc/drivers", 400, "bad_request"},
		{"/api/sessions/100/laps", 400, "bad_request"},
		{"/api/sessions/100/laps?driver=77", 404, "not_found"},
		{"/api/sessions/100/laps/1/999", 404, "not_found"},
		{"/api/sessions/100/laps/1/4", 404, "no_telemetry"},
		{"/api/sessions/100/laps/99/fastest", 404, "no_fastest_lap"},
		{"/api/sessions/100/laps/1/2?channels=speed,nope", 400, "bad_request"},
		{"/api/sessions/100/laps/1/2?channels=t", 400, "bad_request"},
		{"/api/sessions/100/laps/300/2", 400, "bad_request"},
		{"/api/compare", 400, "bad_request"},
		{"/api/compare?a=100:1:3", 400, "bad_request"},
		{"/api/compare?a=100:1:3&b=100:4", 400, "bad_request"},
		{"/api/compare?a=100:1:3&b=100:4:x", 400, "bad_request"},
		{"/api/compare?a=100:1:3&b=999:4:1", 404, "not_found"},
		{"/api/compare?a=100:1:3&b=100:1:4", 404, "no_telemetry"},
		{"/api/compare?a=100:1:3&b=100:4:1&step=0.01", 400, "bad_request"},
		{"/api/compare?a=100:1:3&b=100:4:1&step=NaN", 400, "bad_request"},
		{"/api/compare?a=-1:1:3&b=100:4:1", 400, "bad_request"},
		{"/api/nope", 404, "not_found"},
	}
	for _, c := range cases {
		r := get(t, s, c.url)
		e := decode[apiError](t, r)
		if r.code != c.code || e.Code != c.api || e.Msg == "" {
			t.Errorf("%s: %d %s, want %d %s", c.url, r.code, r.body, c.code, c.api)
		}
		if r.header.Get("Cache-Control") != cacheNone {
			t.Errorf("%s: error responses must not be cached", c.url)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/compare", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestCachingAndGzip(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	url := "/api/compare?a=100:1:fastest&b=100:4:fastest"
	plain := get(t, s, url)
	tag := plain.header.Get("ETag")
	if tag == "" || !strings.Contains(plain.header.Get("Cache-Control"), "max-age=604800") || plain.header.Get("Content-Encoding") != "" {
		t.Fatalf("headers: %v", plain.header)
	}
	zipped := get(t, s, url, "Accept-Encoding", "gzip, br")
	if zipped.header.Get("Content-Encoding") != "gzip" || zipped.header.Get("Vary") != "Accept-Encoding" {
		t.Fatalf("gzip headers: %v", zipped.header)
	}
	zr, err := gzip.NewReader(strings.NewReader(string(zipped.body)))
	if err != nil {
		t.Fatal(err)
	}
	inflated, _ := io.ReadAll(zr)
	if string(inflated) != string(plain.body) || len(zipped.body) >= len(plain.body)/2 {
		t.Fatalf("gzip body: %d bytes vs %d plain", len(zipped.body), len(plain.body))
	}
	if r := get(t, s, url, "If-None-Match", tag); r.code != http.StatusNotModified || len(r.body) != 0 {
		t.Fatalf("conditional: %d", r.code)
	}
	if r := get(t, s, url, "If-None-Match", `"other", W/`+tag); r.code != http.StatusNotModified {
		t.Fatalf("conditional with list and weak prefix: %d", r.code)
	}
	// "fastest" and the explicit lap number are the same resource.
	if got := get(t, s, "/api/compare?a=100:1:3&b=100:4:1").header.Get("ETag"); got != tag {
		t.Error("equivalent comparisons have different ETags")
	}
	if got := get(t, s, url+"&step=10").header.Get("ETag"); got == tag {
		t.Error("different step, same ETag")
	}
	if cc := get(t, s, "/api/seasons").header.Get("Cache-Control"); cc != cacheList {
		t.Errorf("listing cache-control = %q", cc)
	}
}

func TestETagChangesWhenSessionIsRebuilt(t *testing.T) {
	s, st, dir := fixture(t, Options{})
	url := "/api/compare?a=100:1:3&b=100:4:1"
	before := get(t, s, url)
	addSession(t, dir, 100, 2024, "Suzuka", 5000, []*model.LapData{
		lap(1, 3, 300_000, 87_000, 5000, true), lap(4, 1, 200_000, 88_500, 5000, true),
	})
	if _, err := st.Reload(); err != nil {
		t.Fatal(err)
	}
	after := get(t, s, url, "If-None-Match", before.header.Get("ETag"))
	if after.code != 200 {
		t.Fatalf("stale ETag still validated after rebuild: %d", after.code)
	}
	if c := decode[compareResp](t, after); *c.FinalDelta != 1.5 {
		t.Fatalf("served stale data after rebuild: final delta %v", *c.FinalDelta)
	}
}

func TestCorruptLapFailsAloneAndIsCounted(t *testing.T) {
	dir := t.TempDir()
	e := addSession(t, dir, 100, 2024, "Suzuka", 5000, []*model.LapData{
		lap(1, 2, 200_000, 90_000, 5000, true), lap(1, 3, 300_000, 88_000, 5000, true), lap(4, 1, 200_000, 88_500, 5000, true)})
	path := filepath.Join(dir, store.SegmentDir, e.File)
	b, _ := os.ReadFile(path)
	seg, _ := segment.Parse(b)
	for _, bi := range seg.Blocks() {
		if bi.Driver == 1 && bi.Lap == 2 && bi.Channel == model.ChSpeed {
			b[bi.Offset+7] ^= 0xff
		}
	}
	os.WriteFile(path, b, 0o644)
	st, err := store.Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(Options{Store: st, Log: quiet})
	r := get(t, s, "/api/sessions/100/laps/1/2")
	if e := decode[apiError](t, r); r.code != 500 || e.Code != "corrupt_data" || strings.Contains(string(r.body), "offset") || strings.Contains(string(r.body), "goroutine") {
		t.Fatalf("corrupt lap: %d %s", r.code, r.body)
	}
	if r := get(t, s, "/api/compare?a=100:1:3&b=100:4:1"); r.code != 200 {
		t.Fatalf("healthy laps in the same file: %d", r.code)
	}
	if m := string(get(t, s, "/metrics").body); !strings.Contains(m, "ghostlap_decode_errors_total 1") {
		t.Error("decode error not counted")
	}
}

func TestRateLimitIsPerIP(t *testing.T) {
	s, _, _ := fixture(t, Options{RateRPS: 1, RateBurst: 3})
	hit := func(addr string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if c := hit("10.0.0.1:1234"); c != 200 {
			t.Fatalf("request %d within burst: %d", i, c)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "10.0.0.1:999"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if e := decode[apiError](t, resp{rec.Code, rec.Header(), rec.Body.Bytes()}); rec.Code != 429 || e.Code != "rate_limited" || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over limit: %d %s", rec.Code, rec.Body)
	}
	if c := hit("10.0.0.2:1234"); c != 200 {
		t.Fatalf("another client was limited: %d", c)
	}
	// CF-Connecting-IP is ignored unless the proxy is trusted.
	req = httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.RemoteAddr = "10.0.0.1:5"
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 429 {
		t.Fatalf("spoofed header bypassed the limit: %d", rec.Code)
	}
	trusted, _, _ := fixture(t, Options{RateRPS: 1, RateBurst: 1, TrustProxy: true})
	for i, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.1"} {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.RemoteAddr = "172.16.0.1:1"
		req.Header.Set("CF-Connecting-IP", ip)
		rec := httptest.NewRecorder()
		trusted.ServeHTTP(rec, req)
		if want := []int{200, 200, 429}[i]; rec.Code != want {
			t.Fatalf("trusted proxy request %d: %d, want %d", i, rec.Code, want)
		}
	}
	l := newIPLimiter(1, 1, false)
	l.allow("a", time.Unix(0, 0))
	l.sweep(time.Unix(3600, 0), time.Minute)
	for i := range l.shards {
		if len(l.shards[i].buckets) != 0 {
			t.Fatal("idle bucket not swept")
		}
	}
}

func TestMetricsAndMismatch(t *testing.T) {
	dir := t.TempDir()
	// Lap B claims an official time 1 s away from what its telemetry shows.
	bad := lap(4, 1, 200_000, 88_500, 5000, true)
	bad.Lap.OfficialMs = 89_500
	addSession(t, dir, 100, 2024, "Suzuka", 5000, []*model.LapData{lap(1, 3, 300_000, 88_000, 5000, true), bad})
	st, err := store.Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(Options{Store: st, Log: quiet})
	get(t, s, "/api/compare?a=100:1:3&b=100:4:1")
	get(t, s, "/api/compare?a=100:1:3&b=100:4:1")
	get(t, s, "/api/nope")
	m := string(get(t, s, "/metrics").body)
	for _, want := range []string{
		`ghostlap_http_requests_total{code="200",route="compare"} 2`,
		`ghostlap_http_requests_total{code="404",route="api_unknown"} 1`,
		`ghostlap_http_request_duration_seconds_bucket{route="compare",le="0.05"} 2`,
		"ghostlap_lap_cache_hits_total 2",
		"ghostlap_lap_cache_misses_total 2",
		"ghostlap_lap_decodes_total 2",
		"ghostlap_segments 1",
		"ghostlap_decode_errors_total 0",
		"ghostlap_lap_time_mismatch_total 0", // pinned to official time, so no mismatch
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestStaticAndVisitCounting(t *testing.T) {
	static := t.TempDir()
	os.WriteFile(filepath.Join(static, "index.html"), []byte("<html>app</html>"), 0o644)
	os.MkdirAll(filepath.Join(static, "assets"), 0o755)
	os.WriteFile(filepath.Join(static, "assets", "app-abc123.js"), []byte("console.log(1)"), 0o644)
	s, _, _ := fixture(t, Options{StaticDir: static})

	if r := get(t, s, "/"); r.code != 200 || !strings.Contains(string(r.body), "app") || r.header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("index: %d %v", r.code, r.header)
	}
	if r := get(t, s, "/assets/app-abc123.js"); r.code != 200 || !strings.Contains(r.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", r.code, r.header)
	}
	if r := get(t, s, "/assets/missing.js"); r.code != 404 {
		t.Fatalf("missing asset: %d", r.code)
	}
	// The router canonicalises ".." away (redirect); nothing outside the static dir is ever read.
	if r := get(t, s, "/../../etc/passwd"); r.code != http.StatusTemporaryRedirect || strings.Contains(string(r.body), "root:") {
		t.Fatalf("path traversal: %d %s", r.code, r.body)
	}
	if r := get(t, s, "/etc/passwd"); r.code != 200 || !strings.Contains(string(r.body), "app") {
		t.Fatalf("unknown path should serve the app: %d", r.code)
	}
	s.visits = newVisitCounter() // discard the probe above
	s.metrics = newMetrics(s.store, s.visits, s)
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/", s.handleStatic)
	metricsOf := func() string {
		mfs, err := s.metrics.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				v := m.GetCounter().GetValue() + m.GetGauge().GetValue()
				b.WriteString(mf.GetName() + " " + strconv.FormatFloat(v, 'f', -1, 64) + "\n")
			}
		}
		return b.String()
	}
	get(t, s, "/")
	get(t, s, "/?a=100:1:fastest&b=100:4:fastest") // a share link, same client
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:1"
	s.ServeHTTP(httptest.NewRecorder(), req)

	m := metricsOf()
	for _, want := range []string{"ghostlap_page_views_total 3", "ghostlap_share_link_opens_total 1", "ghostlap_unique_visitors_today 2"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
	// A new day resets the visitor set and its salt.
	oldSalt := s.visits.salt
	day, n, rolled := s.visits.roll(time.Now().Add(48 * time.Hour))
	if !rolled || n != 2 || day == "" || s.visits.uniqueToday() != 0 || s.visits.salt == oldSalt {
		t.Fatalf("roll: day=%q n=%d rolled=%v", day, n, rolled)
	}

	noStatic, _, _ := fixture(t, Options{})
	if r := get(t, noStatic, "/"); r.code != 404 {
		t.Fatalf("static disabled: %d", r.code)
	}
}

func TestPanicIsContained(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	s.route("GET /api/boom", "boom", func(w http.ResponseWriter, r *http.Request) *apiError { panic("secret internal detail") })
	r := get(t, s, "/api/boom")
	if e := decode[apiError](t, r); r.code != 500 || e.Code != "internal" || strings.Contains(string(r.body), "secret") || strings.Contains(string(r.body), "goroutine") {
		t.Fatalf("panic leaked: %d %s", r.code, r.body)
	}
	if r := get(t, s, "/api/health"); r.code != 200 {
		t.Fatal("server unusable after a panic")
	}
}

func TestConcurrentRequests(t *testing.T) {
	s, _, _ := fixture(t, Options{RateRPS: 1e6, RateBurst: 1e6})
	urls := []string{
		"/api/compare?a=100:1:fastest&b=100:4:fastest", "/api/compare?a=100:1:2&b=200:1:1&step=20",
		"/api/sessions/100/laps/1/3", "/api/sessions/100/laps?driver=1", "/api/sessions/100/drivers", "/api/seasons", "/metrics",
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				if r := get(t, s, urls[(g+i)%len(urls)], "Accept-Encoding", "gzip"); r.code != 200 {
					t.Errorf("%s: %d", urls[(g+i)%len(urls)], r.code)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestNumberFormatting(t *testing.T) {
	got := string(appendFloats(nil, []float64{250, 0.25, -0.0001, math.NaN(), 12.345, 1e6, math.Inf(1)}, 2))
	if want := "[250,0.25,0,null,12.35,1000000,null]"; got != want {
		t.Errorf("floats: %s, want %s", got, want)
	}
	if got := string(appendFloats(nil, []float64{7.6, -0.4}, 0)); got != "[8,0]" {
		t.Errorf("zero decimals: %s", got)
	}
	if got := string(appendInts(nil, []int64{1500, 2000, 3000}, []bool{true, false, true}, 1000, 1, 0)); got != "[500,null,2000]" {
		t.Errorf("ints: %s", got)
	}
	if got := string(appendInts(nil, []int64{-800, 1418}, nil, 0, 100, 2)); got != "[-8,14.18]" {
		t.Errorf("scaled ints: %s", got)
	}
}

func TestResponseCache(t *testing.T) {
	s, st, _ := fixture(t, Options{})
	url := "/api/compare?a=100:1:fastest&b=100:4:fastest"
	first := get(t, s, url, "Accept-Encoding", "gzip")
	lookups := st.Stats().CacheHits + st.Stats().CacheMisses
	second := get(t, s, "/api/compare?a=100:1:3&b=100:4:1", "Accept-Encoding", "gzip") // same resource, other spelling
	if second.code != 200 || string(second.body) != string(first.body) || second.header.Get("Content-Encoding") != "gzip" ||
		second.header.Get("ETag") != first.header.Get("ETag") || second.header.Get("Cache-Control") != first.header.Get("Cache-Control") {
		t.Fatalf("cached response differs: %d %v", second.code, second.header)
	}
	if got := st.Stats().CacheHits + st.Stats().CacheMisses; got != lookups {
		t.Fatal("a cached response still touched the lap store")
	}
	if s.respHits.Load() != 1 || s.respMisses.Load() != 1 || s.responses.Len() != 1 {
		t.Fatalf("hits=%d misses=%d entries=%d", s.respHits.Load(), s.respMisses.Load(), s.responses.Len())
	}
	// Clients without gzip are served uncompressed and bypass the cache.
	plain := get(t, s, url)
	if plain.header.Get("Content-Encoding") != "" || !json.Valid(plain.body) {
		t.Fatal("plain client got a compressed or invalid body")
	}
	// Errors are never cached.
	get(t, s, "/api/compare?a=100:1:3&b=100:1:4", "Accept-Encoding", "gzip")
	if s.responses.Len() != 1 {
		t.Fatal("an error response was cached")
	}
	off, _, _ := fixture(t, Options{RespCacheBytes: -1})
	get(t, off, url, "Accept-Encoding", "gzip")
	if r := get(t, off, url, "Accept-Encoding", "gzip"); r.code != 200 || off.responses != nil {
		t.Fatal("disabled cache")
	}
}
