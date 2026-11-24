package predict

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
)

func ptr[T any](v T) *T { return &v }

func TestNeutralisations(t *testing.T) {
	msg := func(lap int, cat, text string, flag *string) openf1.RaceControl {
		return openf1.RaceControl{LapNumber: ptr(lap), Category: cat, Message: text, Flag: flag}
	}
	rc := []openf1.RaceControl{
		msg(1, "SafetyCar", "SAFETY CAR DEPLOYED", nil),
		msg(4, "SafetyCar", "SAFETY CAR IN THIS LAP", nil),
		msg(14, "SafetyCar", "VIRTUAL SAFETY CAR DEPLOYED", nil),
		msg(15, "SafetyCar", "SAFETY CAR DEPLOYED", nil), // upgraded
		msg(18, "SafetyCar", "SAFETY CAR IN THIS LAP", nil),
		msg(30, "Flag", "RED FLAG", ptr("RED")),
		msg(30, "Flag", "RED FLAG", ptr("RED")),
		msg(50, "SafetyCar", "SAFETY CAR DEPLOYED", nil), // race ends behind it
		msg(20, "Flag", "YELLOW IN SECTOR 2", ptr("YELLOW")),
	}
	got := neutralisations(rc, 53)
	want := []Neutral{{"SC", 1, 4}, {"VSC", 14, 15}, {"SC", 15, 18}, {"RED", 30, 31}, {"SC", 50, 53}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestBestSeconds(t *testing.T) {
	for in, want := range map[string]float64{
		`5458.421`:              5458.421,
		`[88.866, 88.74, null]`: 88.74,
		`[null, null, null]`:    0,
		`null`:                  0,
		``:                      0,
	} {
		if got := (openf1.Result{Duration: []byte(in)}).BestSeconds(); got != want {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func TestResultPositionTolerance(t *testing.T) {
	var rows []openf1.Result
	if err := json.Unmarshal([]byte(`[{"position":3},{"position":null},{"position":"NC"},{"position":"DQ"}]`), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].Position != 3 || rows[1].Position != 0 || rows[2].Position != 0 || rows[3].Position != 0 {
		t.Errorf("positions: %+v", rows)
	}
}

func TestOLS(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var xs [][]float64
	var ys []float64
	for i := 0; i < 500; i++ {
		a, b := rng.Float64(), rng.Float64()
		xs = append(xs, []float64{1, a, b})
		ys = append(ys, 2+3*a-4*b+rng.NormFloat64()*0.01)
	}
	got := ols(xs, ys, 0)
	for i, want := range []float64{2, 3, -4} {
		if math.Abs(got[i]-want) > 0.02 {
			t.Errorf("coef %d: got %v, want %v", i, got[i], want)
		}
	}
	if ols([][]float64{{1, 1}, {2, 2}}, []float64{1, 2}, 0) != nil {
		t.Error("singular system should not solve")
	}
}

// synthRace builds a dry race where each driver's lap time follows the same
// model the fit assumes, so the fit can be checked against known values.
func synthRace(key int, rng *rand.Rand, pace []float64, softSlope, hardSlope, hardOffset float64) *Race {
	const laps, ref = 50, 90000.0
	r := &Race{Key: key, Year: 2023, Name: "Synthetic", Circuit: "Test", Laps: laps, Run: true,
		Date: time.Date(2023, 1, key, 0, 0, 0, 0, time.UTC)}
	type row struct {
		n int
		t float64
	}
	var total []row
	for i, p := range pace {
		e := Entry{Number: i + 1, Acronym: string(rune('A' + i)), Team: string(rune('a' + i/2)), Grid: i + 1,
			QualiMs: int(ref * 0.94 * (1 + p)), QualiSegMs: []int{int(ref * 0.95 * (1 + p)), int(ref * 0.94 * (1 + p))}, LapsDone: laps}
		split := 15 + rng.IntN(15)
		e.Stints = []Stint{{1, split, "SOFT", 0}, {split + 1, laps, "HARD", 0}}
		var clock, sum float64
		for lap := 1; lap <= laps; lap++ {
			st, _ := e.stintAt(lap)
			y := p - 0.03*float64(lap)/laps + rng.NormFloat64()*0.002
			age := float64(lap - st.From)
			if st.Compound == "SOFT" {
				y += softSlope * age
			} else {
				y += hardOffset + hardSlope*age
			}
			ms := ref * (1 + y)
			if lap == split || lap == split+1 {
				ms += 11000 // 22 s lost over the in-lap and out-lap
			}
			e.StartMs = append(e.StartMs, int64(clock)+1)
			e.LapMs = append(e.LapMs, int32(ms))
			clock += ms
			sum += ms
		}
		total = append(total, row{i, sum})
		r.Drivers = append(r.Drivers, e)
	}
	for i := range r.Drivers {
		pos := 1
		for _, o := range total {
			if o.t < total[i].t {
				pos++
			}
		}
		r.Drivers[i].Pos = pos
	}
	return r
}

func TestFitRecoversKnownModel(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	pace := make([]float64, 20)
	for i := range pace {
		pace[i] = float64(i) * 0.0008
	}
	var races []*Race
	for k := 1; k <= 12; k++ {
		races = append(races, synthRace(k, rng, pace, 0.0006, 0.0002, 0.004))
	}
	m := Fit(races, map[int]*raceFit{})
	check := func(name string, got, want, tol float64) {
		t.Helper()
		if math.Abs(got-want) > tol {
			t.Errorf("%s: got %.5f, want %.5f", name, got, want)
		}
	}
	check("soft wear", m.beta[3], 0.0006, 0.0001)
	check("hard wear", m.beta[5], 0.0002, 0.0001)
	check("race trend", m.beta[0], -0.03, 0.004)
	check("lap scatter", m.lapSD, 0.002, 0.0004)

	next := synthRace(13, rng, pace, 0.0006, 0.0002, 0.004)
	in := m.Inputs(next, DefaultParams)
	check("pit loss", in.PitLoss, 22, 1.5)
	check("ref lap", in.RefSec, 90, 2)
	if len(in.Drivers) != 20 || len(in.Plans) == 0 {
		t.Fatalf("inputs: %d drivers, %d plans", len(in.Drivers), len(in.Plans))
	}
	// Predicted pace should preserve the true order: driver 1 fastest.
	for i := 1; i < len(in.Drivers); i++ {
		if in.Drivers[i].Pace < in.Drivers[i-1].Pace-0.0004 {
			t.Errorf("pace order broken at grid %d: %v then %v", i, in.Drivers[i-1].Pace, in.Drivers[i].Pace)
		}
	}
	check("pace spread", in.Drivers[19].Pace-in.Drivers[0].Pace, 19*0.0008, 0.003)
}

func testInputs(n int) *Inputs {
	in := &Inputs{Key: 1, Laps: 50, RefSec: 90, PitLoss: 22, SCPit: 0.5, LapSD: 0.003, PaceSD: 0.002,
		Tyres:    map[string]Tyre{"SOFT": {-0.004, 0.0008}, "MEDIUM": {0, 0.0005}, "HARD": {0.004, 0.0002}},
		Plans:    [][]PlanStint{{{"MEDIUM", 0.4}, {"HARD", 0.6}}, {{"SOFT", 0.3}, {"HARD", 0.7}}},
		Neutrals: [][]NeutralPat{nil, {{"SC", 0.3, 4}}, {{"VSC", 0.6, 2}}},
		DNFRate:  0.1, DNFFracs: []float64{0.02, 0.4, 0.9}, Params: DefaultParams}
	for i := 0; i < n; i++ {
		in.Drivers = append(in.Drivers, DriverIn{Number: i + 1, Grid: i + 1})
	}
	return in
}

func TestSimulate(t *testing.T) {
	in := testInputs(20)
	in.Drivers[5].Pace = -0.01 // a much faster car starting sixth
	p := Simulate(in, 4000, 42)
	if !reflect.DeepEqual(p, Simulate(in, 4000, 42)) {
		t.Fatal("same seed gave a different answer")
	}
	var win, podium, points float64
	by := map[int]DriverOut{}
	for _, d := range p.Drivers {
		win, podium, points = win+d.Win, podium+d.Podium, points+d.Points
		by[d.Number] = d
		var sum float64
		for _, v := range d.Dist {
			sum += v
		}
		if math.Abs(sum-1) > 0.01 {
			t.Errorf("driver %d: position chances sum to %v", d.Number, sum)
		}
		if d.P10 < 1 || d.P10 > d.P50 || d.P50 > d.P90 || d.P90 > 20 {
			t.Errorf("driver %d: percentiles %d %d %d", d.Number, d.P10, d.P50, d.P90)
		}
		if d.DNF < 0.06 || d.DNF > 0.14 {
			t.Errorf("driver %d: retirement chance %v, want about 0.10", d.Number, d.DNF)
		}
	}
	for name, got := range map[string][2]float64{"win": {win, 1}, "podium": {podium, 3}, "points": {points, 10}} {
		if math.Abs(got[0]-got[1]) > 1e-6 {
			t.Errorf("%s chances sum to %v, want %v", name, got[0], got[1])
		}
	}
	if p.Drivers[0].Number != 6 || by[6].Win < 0.5 {
		t.Errorf("the fast car should be favourite: top pick %d, its win chance %v", p.Drivers[0].Number, by[6].Win)
	}
	// With equal cars, starting further forward must help.
	if !(by[1].Mean < by[10].Mean && by[10].Mean < by[20].Mean) {
		t.Errorf("grid order lost: P1 %v, P10 %v, P20 %v", by[1].Mean, by[10].Mean, by[20].Mean)
	}
}

func TestSimulateDegenerate(t *testing.T) {
	if p := Simulate(&Inputs{Laps: 50}, 100, 1); len(p.Drivers) != 0 {
		t.Error("no drivers should give an empty prediction")
	}
	in := testInputs(2)
	in.Neutrals, in.DNFFracs = nil, nil
	if p := Simulate(in, 200, 1); math.Abs(p.Drivers[0].Win+p.Drivers[1].Win-1) > 1e-9 {
		t.Error("two cars: win chances must sum to 1")
	}
}

func TestBacktestBeatsGridOnSyntheticData(t *testing.T) {
	// Grids are random, so a forecast that knows pace must beat one that
	// only knows the grid.
	rng := rand.New(rand.NewPCG(3, 9))
	pace := make([]float64, 20)
	for i := range pace {
		pace[i] = float64(19-i) * 0.001
	}
	var races []*Race
	for k := 1; k <= 14; k++ {
		r := synthRace(k, rng, pace, 0.0006, 0.0002, 0.004)
		if k > 10 {
			r.Year = 2025
		}
		for i, g := range rng.Perm(20) { // the grid says nothing about pace
			r.Drivers[i].Grid = g + 1
		}
		races = append(races, r)
	}
	rep := Backtest(races, 300)
	if rep.Holdout.Races != 4 || rep.Tune.Races != 2 {
		t.Fatalf("holdout %d races, tuning %d", rep.Holdout.Races, rep.Tune.Races)
	}
	if rep.Holdout.Score() >= rep.HoldoutBase.Score() {
		t.Errorf("model %.4f should beat grid-only %.4f", rep.Holdout.Score(), rep.HoldoutBase.Score())
	}
	f := BuildFile(races, rep, time.Unix(0, 0))
	if len(f.Races) != 6 || len(f.Races[0].Actual) != 20 {
		t.Fatalf("file has %d races", len(f.Races))
	}
	dir := t.TempDir()
	if err := SaveFile(dir, f); err != nil {
		t.Fatal(err)
	}
	back, err := LoadFile(dir)
	if err != nil || len(back.Races) != 6 {
		t.Fatalf("round trip: %v", err)
	}
	if none, err := LoadFile(t.TempDir()); none != nil || err != nil {
		t.Errorf("missing file: got %v, %v", none, err)
	}
}

func BenchmarkSimulate(b *testing.B) {
	in := testInputs(20)
	in.Laps = 57
	for b.Loop() {
		Simulate(in, 1000, 1)
	}
}

func TestGridPriorAndBlend(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	pace := make([]float64, 20)
	for i := range pace {
		pace[i] = float64(i) * 0.001 // pole is fastest, so grid slot predicts the finish
	}
	var races []*Race
	for k := 1; k <= 10; k++ {
		races = append(races, synthRace(k, rng, pace, 0.0006, 0.0002, 0.004))
	}
	races[3].Drivers[0].Pos, races[3].Drivers[0].DNF, races[3].Drivers[0].LapsDone = 0, true, 10

	ranks := finishRanks(races[3])
	if ranks[1] != 20 {
		t.Errorf("a retired driver ranks last, got %d", ranks[1])
	}

	in := testInputs(20)
	prior := gridPrior(races, in.Drivers)
	for i, row := range prior {
		var sum float64
		for _, v := range row {
			sum += v
		}
		if math.Abs(sum-1) > 0.01 {
			t.Errorf("driver %d: prior sums to %v", i, sum)
		}
	}
	for p := 0; p < 20; p++ {
		var sum float64
		for i := range prior {
			sum += prior[i][p]
		}
		if math.Abs(sum-1) > 0.05 {
			t.Errorf("position %d handed out %v times", p+1, sum)
		}
	}
	if prior[0][0] < 0.5 || prior[19][0] > 0.05 {
		t.Errorf("pole should usually win here: pole %v, last %v", prior[0][0], prior[19][0])
	}

	// Weight 1 returns the prior itself; weight 0 ignores it.
	in.Prior, in.PriorWeight = prior, 1
	for _, d := range Simulate(in, 200, 1).Drivers {
		if math.Abs(d.Win-prior[d.Grid-1][0]) > 1e-3 {
			t.Errorf("grid %d: win %v, prior %v", d.Grid, d.Win, prior[d.Grid-1][0])
		}
	}
	in.PriorWeight = 0
	plain := *in
	plain.Prior = nil
	if !reflect.DeepEqual(Simulate(in, 200, 1), Simulate(&plain, 200, 1)) {
		t.Error("weight 0 should leave the simulation untouched")
	}
}
