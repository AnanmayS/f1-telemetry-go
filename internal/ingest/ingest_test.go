package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

var epoch = time.Date(2024, 4, 6, 6, 0, 0, 0, time.UTC)

func ts(ms int64) string {
	return epoch.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.000000+00:00")
}

type row = map[string]any

// fakeAPI serves a tiny season: session 100 (two drivers, three laps each,
// 5 km laps at 180 km/h), session 101 (drivers but no telemetry), session 102
// (cancelled), session 103 (not finished yet).
func fakeAPI(t *testing.T, failCar *atomic.Bool) (*httptest.Server, *atomic.Int64) {
	var hits atomic.Int64
	write := func(w http.ResponseWriter, rows []row) {
		if len(rows) == 0 {
			w.WriteHeader(404)
			w.Write([]byte(`{"detail":"No results found."}`))
			return
		}
		json.NewEncoder(w).Encode(rows)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		s := func(key int, name string, start time.Time, cancelled bool) row {
			return row{"session_key": key, "meeting_key": 7, "session_name": name, "session_type": name, "year": 2024,
				"date_start": start.Format(time.RFC3339), "date_end": start.Add(time.Hour).Format(time.RFC3339),
				"circuit_short_name": "Suzuka", "country_name": "Japan", "is_cancelled": cancelled}
		}
		write(w, []row{
			s(101, "Practice 1", epoch.Add(-24*time.Hour), false),
			s(100, "Qualifying", epoch, false),
			s(102, "Sprint", epoch.Add(24*time.Hour), true),
			s(103, "Race", time.Now().Add(-time.Hour), false),
		})
	})
	mux.HandleFunc("/v1/meetings", func(w http.ResponseWriter, r *http.Request) {
		write(w, []row{{"meeting_key": 7, "meeting_name": "Japanese Grand Prix"}})
	})
	mux.HandleFunc("/v1/drivers", func(w http.ResponseWriter, r *http.Request) {
		write(w, []row{
			{"driver_number": 4, "name_acronym": "NOR", "full_name": "Lando NORRIS", "team_name": "McLaren", "team_colour": "FF8000"},
			{"driver_number": 1, "name_acronym": "VER", "full_name": "Max VERSTAPPEN", "team_name": "Red Bull Racing", "team_colour": "3671C6"},
			{"driver_number": 1, "name_acronym": "VER", "full_name": "duplicate row"},
			{"driver_number": 99, "name_acronym": "RES", "full_name": "Reserve DRIVER", "team_name": nil, "team_colour": nil}, // no laps
		})
	})
	mux.HandleFunc("/v1/laps", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("session_key") != "100" {
			write(w, nil)
			return
		}
		var rows []row
		for _, d := range []int{1, 4} {
			off := int64(d) * 137
			rows = append(rows,
				row{"driver_number": d, "lap_number": 1, "date_start": ts(60_000 + off), "lap_duration": nil, "is_pit_out_lap": true, "duration_sector_1": nil, "duration_sector_2": 40.0, "duration_sector_3": 20.0},
				row{"driver_number": d, "lap_number": 2, "date_start": ts(160_000 + off), "lap_duration": 100.0 + float64(d)/10, "is_pit_out_lap": false, "duration_sector_1": 30.0, "duration_sector_2": 40.0, "duration_sector_3": 30.0},
				row{"driver_number": d, "lap_number": 3, "date_start": ts(260_000 + off + int64(d)*100), "lap_duration": 100.0, "is_pit_out_lap": false},
			)
		}
		write(w, rows)
	})
	mux.HandleFunc("/v1/car_data", func(w http.ResponseWriter, r *http.Request) {
		if failCar != nil && failCar.Load() {
			w.WriteHeader(500)
			return
		}
		if r.URL.Query().Get("session_key") != "100" {
			write(w, nil)
			return
		}
		var rows []row
		for t := int64(0); t <= 400_000; t += 250 {
			thr, brk := 100, 0
			if t < 30_000 {
				thr, brk = 104, 104 // stationary sentinel
			}
			rows = append(rows, row{"date": ts(t), "speed": 180, "rpm": 11000, "n_gear": 7, "throttle": thr, "brake": brk, "drs": 8})
		}
		rows = append(rows, rows[10], rows[5]) // a duplicate and an out-of-order row
		write(w, rows)
	})
	mux.HandleFunc("/v1/location", func(w http.ResponseWriter, r *http.Request) {
		var rows []row
		for t := int64(100); t <= 400_000; t += 250 {
			rows = append(rows, row{"date": ts(t), "x": t / 10, "y": -t / 10, "z": 40})
		}
		write(w, rows)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newIngester(t *testing.T, srv *httptest.Server, dir string) *Ingester {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := openf1.New(filepath.Join(dir, "raw"), 1, 2)
	c.BaseURL = srv.URL + "/v1/"
	c.Limiter = rate.NewLimiter(rate.Inf, 1)
	c.BaseDelay, c.MaxRetries, c.Log = time.Millisecond, 1, log
	return &Ingester{Client: c, DataDir: dir, Workers: 2, Log: log}
}

func TestEndToEnd(t *testing.T) {
	srv, hits := fakeAPI(t, nil)
	dir := t.TempDir()
	in := newIngester(t, srv, dir)
	ctx := context.Background()

	sessions, err := in.Discover(ctx, 2024)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Key != 101 || sessions[1].Key != 100 {
		t.Fatalf("discover should drop cancelled and unfinished sessions and sort by date: %+v", sessions)
	}
	if sessions[1].MeetingName != "Japanese Grand Prix" || sessions[1].Circuit != "Suzuka" {
		t.Fatalf("session metadata: %+v", sessions[1])
	}

	res, err := in.Session(ctx, sessions[0])
	if err != nil || res.Skipped != "no telemetry" {
		t.Fatalf("empty session: %+v err=%v", res, err)
	}
	res, err = in.Session(ctx, sessions[1])
	if err != nil {
		t.Fatal(err)
	}
	if res.Laps != 6 || res.Samples == 0 || res.SegmentBytes == 0 || res.RawJSONBytes <= res.SegmentBytes {
		t.Fatalf("result: %+v", res)
	}

	man, _ := store.LoadManifest(dir)
	e, ok := man.Find(100)
	if !ok || man.Skipped[101] == "" || len(man.Segments) != 1 {
		t.Fatalf("manifest: %+v", man)
	}
	seg, err := segment.Open(filepath.Join(dir, store.SegmentDir, e.File))
	if err != nil {
		t.Fatal(err)
	}
	defer seg.Close()
	if err := seg.Verify(); err != nil {
		t.Fatal(err)
	}
	if len(seg.Meta.Drivers) != 3 || seg.Meta.Drivers[0].Acronym != "VER" || seg.Meta.Drivers[0].Colour != "3671C6" || seg.Meta.Drivers[2].Team != "" {
		t.Fatalf("drivers (deduplicated, sorted, null-safe): %+v", seg.Meta.Drivers)
	}
	if l := seg.Meta.CircuitLengthM; l < 4990 || l > 5010 {
		t.Fatalf("circuit length = %.1f, want ~5000 (100 s at 50 m/s)", l)
	}
	if len(seg.Laps(1)) != 3 || len(seg.Laps(4)) != 3 || len(seg.Laps(99)) != 0 {
		t.Fatal("lap counts")
	}
	lap2, _ := seg.Lap(4, 2)
	if !lap2.ValidTime() || !lap2.DistanceOK() || lap2.OfficialMs != 100_400 || lap2.SectorMs != [3]int32{30_000, 40_000, 30_000} {
		t.Fatalf("lap 2: %+v", lap2)
	}
	lap1, _ := seg.Lap(4, 1)
	if lap1.ValidTime() || lap1.Flags&model.FlagPitOut == 0 || lap1.SectorMs[0] != model.NoTime {
		t.Fatalf("lap 1: %+v", lap1)
	}
	ld, err := seg.ReadLap(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	tsCh := ld.Values[model.ChT]
	for i := 1; i < len(tsCh); i++ {
		if tsCh[i] <= tsCh[i-1] {
			t.Fatal("timestamps not strictly increasing (duplicates or disorder survived)")
		}
	}
	if ld.Valid[model.ChThrottle] != nil {
		t.Error("lap 2 has no sentinel values, so it must not carry a validity bitmap")
	}

	// Idempotent: a rerun does no work and sends no requests.
	before := hits.Load()
	for _, s := range sessions {
		res, err := in.Session(ctx, s)
		if err != nil || !res.Unchanged {
			t.Fatalf("rerun of %d: %+v err=%v", s.Key, res, err)
		}
	}
	if hits.Load() != before {
		t.Fatalf("rerun sent %d requests", hits.Load()-before)
	}

	// Force rebuild offline: identical bytes, same file, nothing fetched.
	in.Force, in.Client.Offline = true, true
	if _, err := in.Session(ctx, sessions[1]); err != nil {
		t.Fatal(err)
	}
	man2, _ := store.LoadManifest(dir)
	if e2, _ := man2.Find(100); e2.File != e.File || e2.SHA256 != e.SHA256 {
		t.Fatal("rebuild from cache is not byte-identical")
	}
	files, _ := os.ReadDir(filepath.Join(dir, store.SegmentDir))
	if len(files) != 1 {
		t.Fatalf("segment dir holds %d files", len(files))
	}
}

func TestResumeAfterFailure(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv, hits := fakeAPI(t, &fail)
	dir := t.TempDir()
	in := newIngester(t, srv, dir)
	ctx := context.Background()
	sessions, _ := in.Discover(ctx, 2024)
	s := sessions[1]

	if _, err := in.Session(ctx, s); err == nil {
		t.Fatal("expected failure while car_data is down")
	}
	if man, _ := store.LoadManifest(dir); len(man.Segments) != 0 {
		t.Fatal("failed session reached the manifest")
	}
	if files, _ := os.ReadDir(filepath.Join(dir, store.SegmentDir)); len(files) != 0 {
		t.Fatal("failed session left a segment file")
	}

	fail.Store(false)
	before := hits.Load()
	if _, err := in.Session(ctx, s); err != nil {
		t.Fatal(err)
	}
	// drivers and laps were cached by the failed run; only telemetry is fetched now.
	if n := hits.Load() - before; n > 4 {
		t.Fatalf("resume sent %d requests, want at most 4 (2 drivers x car_data+location)", n)
	}
}

func TestCancelledContextStops(t *testing.T) {
	srv, _ := fakeAPI(t, nil)
	in := newIngester(t, srv, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	sessions, _ := in.Discover(ctx, 2024)
	cancel()
	if _, err := in.Session(ctx, sessions[1]); err == nil {
		t.Fatal("expected context error")
	}
	if man, _ := store.LoadManifest(in.DataDir); len(man.Segments) != 0 || len(man.Skipped) != 0 {
		t.Fatalf("interrupted run changed the manifest: %+v", man)
	}
}
