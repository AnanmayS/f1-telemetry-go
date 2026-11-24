package api

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
)

func predFile() *predict.File {
	in := &predict.Inputs{Key: 900, Laps: 40, RefSec: 90, PitLoss: 22, SCPit: 0.5, LapSD: 0.003, PaceSD: 0.002,
		Tyres:   map[string]predict.Tyre{"MEDIUM": {Slope: 0.0005}, "HARD": {Offset: 0.004, Slope: 0.0002}},
		Plans:   [][]predict.PlanStint{{{Compound: "MEDIUM", Frac: 0.4}, {Compound: "HARD", Frac: 0.6}}},
		DNFRate: 0.1, DNFFracs: []float64{0.5}, Params: predict.DefaultParams, Trained: 12}
	var actual []predict.Actual
	for i := 1; i <= 10; i++ {
		in.Drivers = append(in.Drivers, predict.DriverIn{Number: i, Acronym: "D" + string(rune('A'+i)), Grid: i, Pace: float64(i) * 0.001})
		actual = append(actual, predict.Actual{Number: i, Pos: i % 10, DNF: i == 10})
	}
	future := *in
	future.Key = 901
	return &predict.File{Generated: time.Unix(1_700_000_000, 0), HoldoutFrom: 2025,
		Holdout: predict.Metrics{Races: 30, WinnerHit: 0.5}, HoldoutBase: predict.Metrics{Races: 30, WinnerHit: 0.4},
		Races: []predict.RaceFile{
			{Key: 900, Year: 2025, Name: "Test Grand Prix", Circuit: "Test", Date: time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC), Run: true, Inputs: in, Actual: actual},
			{Key: 901, Year: 2025, Name: "Next Grand Prix", Circuit: "Next", Date: time.Date(2025, 3, 8, 0, 0, 0, 0, time.UTC), Inputs: &future},
		}}
}

func TestPredictions(t *testing.T) {
	s, _, _ := fixture(t, Options{Predictions: predFile()})

	r := get(t, s, "/api/predictions")
	if r.code != 200 {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	var list struct {
		Races    []predRaceJSON `json:"races"`
		Backtest struct {
			FromYear int             `json:"from_year"`
			Model    predict.Metrics `json:"model"`
		} `json:"backtest"`
	}
	if err := json.Unmarshal(r.body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Races) != 2 || list.Races[0].Key != 901 || list.Races[0].Run || !list.Races[1].Run {
		t.Fatalf("races, newest first: %+v", list.Races)
	}
	if list.Backtest.FromYear != 2025 || list.Backtest.Model.WinnerHit != 0.5 {
		t.Errorf("backtest: %+v", list.Backtest)
	}

	r = get(t, s, "/api/predictions/900")
	if r.code != 200 {
		t.Fatalf("race: %d %s", r.code, r.body)
	}
	var one struct {
		Runs    int              `json:"runs"`
		Drivers []predDriverJSON `json:"drivers"`
	}
	if err := json.Unmarshal(r.body, &one); err != nil {
		t.Fatal(err)
	}
	var win float64
	for _, d := range one.Drivers {
		win += d.Win
	}
	if one.Runs != predictRuns || len(one.Drivers) != 10 || math.Abs(win-1) > 1e-6 {
		t.Fatalf("runs %d, drivers %d, win chances sum %v", one.Runs, len(one.Drivers), win)
	}
	top := one.Drivers[0]
	if top.Number != 1 || top.Actual == nil || *top.Actual != 1 {
		t.Errorf("favourite should be the fastest car on pole and carry its result: %+v", top)
	}
	for _, d := range one.Drivers {
		if d.Number == 10 && (d.Actual != nil || !d.Out) {
			t.Errorf("retired driver: %+v", d)
		}
	}
	// Same answer every time, and cacheable.
	if again := get(t, s, "/api/predictions/900"); string(again.body) != string(r.body) {
		t.Error("prediction changed between requests")
	}
	if tag := r.header.Get("ETag"); tag == "" || get(t, s, "/api/predictions/900", "If-None-Match", tag).code != 304 {
		t.Error("no usable ETag")
	}

	if r := get(t, s, "/api/predictions/12345"); r.code != 404 {
		t.Errorf("unknown race: %d", r.code)
	}
	if r := get(t, s, "/api/predictions/abc"); r.code != 400 {
		t.Errorf("bad key: %d", r.code)
	}
}

func TestPredictionsAbsent(t *testing.T) {
	s, _, _ := fixture(t, Options{})
	r := get(t, s, "/api/predictions")
	if r.code != 200 || string(r.body) != `{"races":[]}`+"\n" && string(r.body) != `{"races":[]}` {
		t.Fatalf("no file: %d %q", r.code, r.body)
	}
	if r := get(t, s, "/api/predictions/900"); r.code != 404 {
		t.Errorf("no file, one race: %d", r.code)
	}
}
