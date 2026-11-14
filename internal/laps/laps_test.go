package laps

import (
	"math"
	"testing"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

// synth builds a car stream sampled every dtMs at constant speed.
func synth(fromMs, toMs, dtMs, speed int64) (car []Car, loc []Loc) {
	for t := fromMs; t <= toMs; t += dtMs {
		car = append(car, Car{T: t, Speed: speed, RPM: 10000, Gear: 7, Throttle: 100, Brake: 0, DRS: 8})
		loc = append(loc, Loc{T: t + 100, X: t, Y: -t, Z: 5})
	}
	return
}

func TestCleanCar(t *testing.T) {
	rows := []Car{
		{T: 300, Throttle: 50, Brake: 100},
		{T: 100, Throttle: 104, Brake: 104},
		{T: 300, Throttle: 99, Brake: 0}, // duplicate timestamp, dropped
		{T: 200, Throttle: 0, Brake: 0},
	}
	got := CleanCar(rows)
	if len(got) != 3 || got[0].T != 100 || got[1].T != 200 || got[2].T != 300 {
		t.Fatalf("order/dedupe: %+v", got)
	}
	if got[0].Throttle != Missing || got[0].Brake != Missing {
		t.Errorf("sentinel 104 not mapped to missing: %+v", got[0])
	}
	if got[2].Throttle != 50 || got[2].Brake != 1 {
		t.Errorf("brake not boolean or wrong duplicate kept: %+v", got[2])
	}
	if rows[0].T != 300 {
		t.Error("input was modified")
	}
}

func TestSliceWindowsAndFlags(t *testing.T) {
	car, loc := synth(0, 400_000, 250, 180) // 180 km/h = 50 m/s
	timing := []Timing{
		{Number: 1, StartMs: 10_000, HasStart: true, DurationMs: model.NoTime, PitOut: true},
		{Number: 2, StartMs: 110_000, HasStart: true, DurationMs: 100_020}, // 20 ms off the next start
		{Number: 3, StartMs: 210_000, HasStart: true, DurationMs: 100_000},
		{Number: 4, StartMs: 390_000, HasStart: true, DurationMs: 500_000, PitOut: true}, // pit exit 80 s after lap 3 ended; duration is not line-to-line
	}
	built := Slice(44, timing, CleanCar(car), CleanLoc(loc))
	if len(built) != 4 {
		t.Fatalf("laps = %d", len(built))
	}
	l1, l2, l3, l4 := built[0].Data.Lap, built[1].Data.Lap, built[2].Data.Lap, built[3].Data.Lap
	if l1.ValidTime() || l1.Flags&model.FlagPitOut == 0 || l1.EndMs != 110_000 {
		t.Errorf("lap 1: %+v", l1)
	}
	if !l2.ValidTime() || l2.EndMs != 210_000 || l2.OfficialMs != 100_020 {
		t.Errorf("lap 2: %+v", l2)
	}
	if !l3.ValidTime() || l3.Flags&model.FlagPitIn == 0 || l3.EndMs != 310_000 {
		t.Errorf("lap 3 should be valid and a pit-in lap: %+v", l3)
	}
	if l4.ValidTime() {
		t.Errorf("pit-out lap must not have a valid time: %+v", l4)
	}
	// A sample exactly on the lap start needs no leading padding sample...
	ts := built[1].Data.Values[model.ChT]
	if ts[0] != 110_000 {
		t.Errorf("lap 2 leading samples: %v", ts[:2])
	}
	// ...but a start between samples gets one sample before it.
	off := Slice(44, []Timing{{Number: 1, StartMs: 110_100, HasStart: true, DurationMs: 50_000}}, CleanCar(car), CleanLoc(loc))
	if got := off[0].Data.Values[model.ChT][0]; got != 110_000 {
		t.Errorf("leading padding sample = %d, want 110000", got)
	}
	if last := ts[len(ts)-1]; last != 210_250 {
		t.Errorf("lap 2 trailing sample = %d, want 210250 (covers start+official)", last)
	}
	// x is interpolated onto the car clock: loc rows sit 100 ms later and carry X = T-100.
	if x := built[1].Data.Values[model.ChX][0]; x != 109_900 {
		t.Errorf("interpolated x = %d", x)
	}
}

func TestSliceMissingStartAndGaps(t *testing.T) {
	car, loc := synth(100_000, 300_000, 250, 180)
	timing := []Timing{
		{Number: 1, HasStart: false, DurationMs: 95_000}, // anchored back from lap 2
		{Number: 2, StartMs: 200_000, HasStart: true, DurationMs: 90_000},
		{Number: 3, StartMs: 900_000, HasStart: true, DurationMs: 90_000}, // no telemetry
		{Number: 5, HasStart: false, DurationMs: model.NoTime},            // nothing to anchor to
	}
	built := Slice(1, timing, CleanCar(car), CleanLoc(loc))
	if l := built[0].Data.Lap; l.StartMs != 105_000 || !l.ValidTime() || l.Samples == 0 {
		t.Errorf("lap 1 not anchored: %+v", l)
	}
	for _, i := range []int{2, 3} {
		d := built[i].Data
		if d.Lap.Samples != 0 || len(d.Values[model.ChT]) != 0 || d.Values[model.ChSpeed] == nil {
			t.Errorf("lap %d should be present but empty: %+v", d.Lap.Number, d.Lap)
		}
	}
	Finalize(built) // must not panic on empty laps
}

func TestMissingValuesAndLocationGaps(t *testing.T) {
	car, _ := synth(0, 20_000, 250, 100)
	car[10].Throttle, car[10].Brake = 104, 104
	loc := []Loc{{T: 0, X: 0}, {T: 1000, X: 1000}, {T: 9000, X: 9000}, {T: 21_000, X: 21_000}}
	built := Slice(1, []Timing{{Number: 1, StartMs: 0, HasStart: true, DurationMs: 20_000}}, CleanCar(car), CleanLoc(loc))
	d := built[0].Data
	if d.Valid[model.ChThrottle] == nil || d.Valid[model.ChThrottle][10] || !d.Valid[model.ChThrottle][9] {
		t.Error("throttle sentinel not flagged missing")
	}
	if d.Valid[model.ChBrake][10] {
		t.Error("brake sentinel not flagged missing")
	}
	if d.Valid[model.ChSpeed] != nil {
		t.Error("speed should have no validity bitmap")
	}
	// t=500 lies inside a 1 s location gap (interpolated); t=5000 inside an 8 s gap (missing).
	if !d.Valid[model.ChX][2] || d.Values[model.ChX][2] != 500 {
		t.Errorf("x at 500ms: valid=%v v=%d", d.Valid[model.ChX][2], d.Values[model.ChX][2])
	}
	if d.Valid[model.ChX][20] {
		t.Error("x interpolated across an 8 s gap")
	}
}

func TestFinalizeNormalisesDistance(t *testing.T) {
	// Two laps at 180 km/h (50 m/s). The first is 100 s, the second reads 2%
	// fast on the speed sensor; both must end at the same circuit length.
	carA, loc := synth(0, 120_000, 240, 180)
	a := Slice(1, []Timing{{Number: 1, StartMs: 5_010, HasStart: true, DurationMs: 100_000}}, CleanCar(carA), CleanLoc(loc))
	carB, _ := synth(0, 120_000, 240, 184)
	b := Slice(2, []Timing{{Number: 1, StartMs: 7_030, HasStart: true, DurationMs: 100_000}}, CleanCar(carB), CleanLoc(loc))
	carC, _ := synth(0, 120_000, 240, 180)
	c := Slice(3, []Timing{{Number: 1, StartMs: 6_000, HasStart: true, DurationMs: 100_000}}, CleanCar(carC), CleanLoc(loc))
	all := append(append(a, b...), c...)
	length := Finalize(all)
	if math.Abs(length-5000) > 0.5 {
		t.Fatalf("circuit length = %.2f m, want 5000", length)
	}
	for _, bl := range all {
		d := bl.Data
		if !d.Lap.DistanceOK() {
			t.Fatalf("driver %d not normalised", d.Lap.Driver)
		}
		r := Resample(d, []float64{0, length}, nil)
		if math.Abs(r.TimeS[0]) > 0.002 || math.Abs(r.TimeS[1]-100) > 0.002 {
			t.Errorf("driver %d: time at 0 = %.4f, at full distance = %.4f; want 0 and 100", d.Lap.Driver, r.TimeS[0], r.TimeS[1])
		}
	}
}

func TestGrid(t *testing.T) {
	g := Grid(22, 5)
	want := []float64{0, 5, 10, 15, 20, 22}
	if len(g) != len(want) {
		t.Fatalf("grid = %v", g)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("grid = %v", g)
		}
	}
	if g := Grid(20, 5); len(g) != 5 || g[4] != 20 {
		t.Fatalf("exact multiple: %v", g)
	}
	if Grid(0, 5) != nil || Grid(100, 0) != nil {
		t.Fatal("degenerate grids must be empty")
	}
}

func lapOf(startMs int64, ts, dist, speed, gear []int64) *model.LapData {
	ld := &model.LapData{Lap: model.Lap{StartMs: startMs, Samples: len(ts)}}
	for c := range ld.Values {
		ld.Values[c] = make([]int64, len(ts))
	}
	ld.Values[model.ChT], ld.Values[model.ChDistance] = ts, dist
	ld.Values[model.ChSpeed], ld.Values[model.ChGear] = speed, gear
	return ld
}

func TestResampleInterpolation(t *testing.T) {
	ld := lapOf(1000,
		[]int64{1000, 2000, 3000, 4000},
		[]int64{0, 10_000, 10_000, 30_000}, // stationary between samples 1 and 2
		[]int64{100, 200, 0, 300},
		[]int64{3, 4, 4, 6})
	ld.Valid[model.ChSpeed] = []bool{true, true, false, true}
	chans := []model.Channel{model.ChSpeed, model.ChGear}
	r := Resample(ld, []float64{0, 50, 100, 200, 300, 301}, chans)
	eq := func(name string, got, want float64) {
		t.Helper()
		if math.IsNaN(want) != math.IsNaN(got) || (!math.IsNaN(want) && math.Abs(got-want) > 1e-9) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("time@0", r.TimeS[0], 0)
	eq("time@50", r.TimeS[1], 0.5)
	eq("time@100", r.TimeS[2], 1)
	eq("time@200", r.TimeS[3], 2.5)
	eq("time@300", r.TimeS[4], 3)
	eq("time@301 (beyond data)", r.TimeS[5], math.NaN())
	eq("speed@50 linear", r.Values[model.ChSpeed][1], 150)
	eq("speed@200 next to a missing sample", r.Values[model.ChSpeed][3], math.NaN())
	eq("gear@50 holds previous", r.Values[model.ChGear][1], 3)
	eq("gear@100 switches at the sample", r.Values[model.ChGear][2], 4)
	eq("gear@200 holds previous", r.Values[model.ChGear][3], 4)
	eq("gear@300", r.Values[model.ChGear][4], 6)

	empty := Resample(lapOf(0, nil, nil, nil, nil), []float64{0, 5}, chans)
	eq("empty lap", empty.TimeS[0], math.NaN())
}

func TestCompareFinalDeltaEqualsLapTimeDifference(t *testing.T) {
	// A: 5000 m in 100 s. B: a different session whose circuit length came
	// out as 5010 m, driven in 101.5 s. Final delta must be +1.5 s.
	mk := func(lengthCm, durMs int64) *model.LapData {
		n := int64(400)
		ts, dist, sp, g := make([]int64, n+1), make([]int64, n+1), make([]int64, n+1), make([]int64, n+1)
		for i := int64(0); i <= n; i++ {
			ts[i], dist[i] = 50_000+durMs*i/n, lengthCm*i/n
		}
		return lapOf(50_000, ts, dist, sp, g)
	}
	c := Compare(mk(500_000, 100_000), mk(501_000, 101_500), 5000, 5010, 5, []model.Channel{model.ChSpeed})
	if len(c.DistanceM) != 1001 || c.DistanceM[1000] != 5000 {
		t.Fatalf("grid: n=%d last=%v", len(c.DistanceM), c.DistanceM[len(c.DistanceM)-1])
	}
	if math.Abs(c.FinalDeltaS-1.5) > 0.001 {
		t.Fatalf("final delta = %.4f, want 1.5", c.FinalDeltaS)
	}
	if math.Abs(c.DeltaS[500]-0.75) > 0.001 {
		t.Fatalf("mid-lap delta = %.4f, want 0.75", c.DeltaS[500])
	}
}
