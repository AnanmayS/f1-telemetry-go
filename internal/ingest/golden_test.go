package ingest

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

// Golden sessions: one qualifying and one race per season (Japanese GP).
// The tests compare built segments with the official timing in the raw
// cache. They need real data, so they skip unless the sessions have been
// ingested:
//
//	go run ./cmd/ingest -year all -session 9169,9173,9492,9496,10002,10006,11249,11253
//	go test ./internal/ingest -run Golden -v
//
// Set GHOSTLAP_DATA to use a data directory other than ./data, and
// GHOSTLAP_GOLDEN=strict to fail instead of skip when a session is missing.
var goldenSessions = []struct {
	key  int
	name string
}{
	{9169, "2023 Japan Qualifying"}, {9173, "2023 Japan Race"},
	{9492, "2024 Japan Qualifying"}, {9496, "2024 Japan Race"},
	{10002, "2025 Japan Qualifying"}, {10006, "2025 Japan Race"},
	{11249, "2026 Japan Qualifying"}, {11253, "2026 Japan Race"},
}

func goldenDataDir() string {
	if d := os.Getenv("GHOSTLAP_DATA"); d != "" {
		return d
	}
	return filepath.Join("..", "..", "data")
}

func TestGolden(t *testing.T) {
	dir := goldenDataDir()
	man, err := store.LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	client := openf1.New(filepath.Join(dir, "raw"), 1, 1)
	client.Offline = true
	client.Log = slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, g := range goldenSessions {
		t.Run(g.name, func(t *testing.T) {
			e, ok := man.Find(g.key)
			if !ok {
				if os.Getenv("GHOSTLAP_GOLDEN") == "strict" {
					t.Fatalf("session %d not ingested", g.key)
				}
				t.Skipf("session %d not ingested in %s", g.key, dir)
			}
			seg, err := segment.Open(filepath.Join(dir, store.SegmentDir, e.File))
			if err != nil {
				t.Fatal(err)
			}
			defer seg.Close()
			if err := seg.Verify(); err != nil {
				t.Fatal(err)
			}
			official, _, err := client.Laps(context.Background(), g.key)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, seg, official)
		})
	}
}

func checkGolden(t *testing.T, seg *segment.Segment, official []openf1.Lap) {
	type key struct{ driver, lap int }
	wantCount := map[int]int{}
	wantFastest := map[int]int32{}
	rows := map[key]openf1.Lap{}
	for _, l := range official {
		k := key{l.DriverNumber, l.LapNumber}
		if _, dup := rows[k]; !dup {
			wantCount[l.DriverNumber]++
		}
		rows[k] = l
		// Official fastest: the best line-to-line time (pit-out laps are timed
		// from the previous crossing, see docs/DATA.md).
		if l.LapDuration != nil && (l.IsPitOutLap == nil || !*l.IsPitOutLap) {
			ms := int32(math.Round(*l.LapDuration * 1000))
			if cur, ok := wantFastest[l.DriverNumber]; !ok || ms < cur {
				wantFastest[l.DriverNumber] = ms
			}
		}
	}

	// 1. Lap count per driver matches the laps endpoint.
	for driver, want := range wantCount {
		if got := len(seg.Laps(driver)); got != want {
			t.Errorf("driver %d: %d laps in segment, laps endpoint has %d", driver, got, want)
		}
	}

	var valid, withTelemetry, distOK, coverageOK, timeExact int
	var sumSliced, sumOfficial int64
	var worstEnd float64
	for _, l := range seg.AllLaps() {
		if !l.ValidTime() {
			continue
		}
		valid++
		// 2. Sliced window vs official duration.
		sumSliced += l.EndMs - l.StartMs
		sumOfficial += int64(l.OfficialMs)
		if d := math.Abs(float64(l.EndMs-l.StartMs-int64(l.OfficialMs))) / 1000; d > 1.5 {
			t.Errorf("driver %d lap %d: sliced %.3fs vs official %.3fs", l.Driver, l.Number, float64(l.EndMs-l.StartMs)/1000, float64(l.OfficialMs)/1000)
		}
		if l.Samples == 0 {
			continue
		}
		withTelemetry++
		if float64(l.Samples) >= 2.5*float64(l.OfficialMs)/1000 {
			coverageOK++
		}
		if !l.DistanceOK() {
			continue
		}
		distOK++
		// 3. The distance channel reaches the circuit length exactly at the
		// official lap time (this is what pins the final delta of a comparison).
		ld, err := seg.ReadLap(l.Driver, l.Number)
		if err != nil {
			t.Errorf("driver %d lap %d: %v", l.Driver, l.Number, err)
			continue
		}
		diff := math.Abs(distanceAt(ld, l.StartMs+int64(l.OfficialMs))/100 - seg.Meta.CircuitLengthM)
		if !(diff <= 0.05) {
			t.Errorf("driver %d lap %d: distance at the official lap time is %.2f m from the circuit length", l.Driver, l.Number, diff)
		} else {
			timeExact++
		}
		worstEnd = math.Max(worstEnd, diff)
	}
	if valid == 0 {
		t.Fatal("no laps with an official time")
	}
	if rel := math.Abs(float64(sumSliced-sumOfficial)) / float64(sumOfficial); rel > 0.001 {
		t.Errorf("sum of sliced lap durations %.1fs vs official %.1fs (%.3f%% apart)", float64(sumSliced)/1000, float64(sumOfficial)/1000, rel*100)
	}
	if frac := float64(coverageOK) / float64(max(withTelemetry, 1)); frac < 0.95 {
		t.Errorf("only %.1f%% of timed laps have >= 2.5 Hz telemetry", frac*100)
	}
	if frac := float64(distOK) / float64(max(withTelemetry, 1)); frac < 0.95 {
		t.Errorf("only %.1f%% of timed laps with telemetry have a normalised distance", frac*100)
	}

	// 4. Fastest lap per driver matches the official fastest lap.
	var sessionBest model.Lap
	fastestOK := 0
	for driver, want := range wantFastest {
		got, ok := store.Fastest(seg, driver)
		switch {
		case !ok:
			t.Errorf("driver %d: official fastest %.3fs but no fastest lap in segment", driver, float64(want)/1000)
			continue
		case got.OfficialMs != want:
			t.Errorf("driver %d: fastest lap %d = %.3fs, official fastest %.3fs", driver, got.Number, float64(got.OfficialMs)/1000, float64(want)/1000)
			continue
		}
		fastestOK++
		if sessionBest.OfficialMs == 0 || got.OfficialMs < sessionBest.OfficialMs {
			sessionBest = got
		}
	}

	// 5. Comparing each driver's fastest lap with the session's fastest lap:
	// the final delta equals the official lap-time difference within 0.05 s.
	channels := []model.Channel{model.ChSpeed}
	compared := 0
	var worstDelta float64
	if sessionBest.DistanceOK() {
		a, err := seg.ReadLap(sessionBest.Driver, sessionBest.Number)
		if err != nil {
			t.Fatal(err)
		}
		for driver := range wantFastest {
			l, ok := store.Fastest(seg, driver)
			if !ok || !l.DistanceOK() {
				continue
			}
			b, err := seg.ReadLap(l.Driver, l.Number)
			if err != nil {
				t.Error(err)
				continue
			}
			c := laps.Compare(a, b, seg.Meta.CircuitLengthM, seg.Meta.CircuitLengthM, 5, channels)
			want := float64(l.OfficialMs-sessionBest.OfficialMs) / 1000
			diff := math.Abs(c.FinalDeltaS - want)
			if !(diff <= 0.05) {
				t.Errorf("driver %d vs %d: final delta %.3fs, official difference %.3fs", driver, sessionBest.Driver, c.FinalDeltaS, want)
			}
			worstDelta = math.Max(worstDelta, diff)
			compared++
			for i, d := range c.DeltaS {
				if math.IsNaN(d) {
					t.Errorf("driver %d vs %d: no delta at %.0f m", driver, sessionBest.Driver, c.DistanceM[i])
					break
				}
			}
		}
	}
	if compared < len(wantFastest)*9/10 {
		t.Errorf("only %d of %d drivers' fastest laps were comparable", compared, len(wantFastest))
	}
	t.Logf("circuit %.1f m | drivers %d | laps %d | timed laps %d (telemetry %d, dist-ok %d) | distance exact at lap end on %d (worst %.3f m) | fastest matched %d/%d | deltas checked %d (worst error %.4fs)",
		seg.Meta.CircuitLengthM, len(wantCount), len(seg.AllLaps()), valid, withTelemetry, distOK, timeExact, worstEnd, fastestOK, len(wantFastest), compared, worstDelta)
}

// distanceAt interpolates a lap's distance channel (cm) at session time t.
func distanceAt(ld *model.LapData, t int64) float64 {
	ts, dist := ld.Values[model.ChT], ld.Values[model.ChDistance]
	for i := 1; i < len(ts); i++ {
		if ts[i] >= t {
			f := float64(t-ts[i-1]) / float64(ts[i]-ts[i-1])
			return float64(dist[i-1]) + f*float64(dist[i]-dist[i-1])
		}
	}
	return math.NaN()
}
