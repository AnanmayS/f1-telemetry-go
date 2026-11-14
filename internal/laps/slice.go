// Package laps turns a driver's telemetry stream into per-lap sample sets
// (slicing, distance integration and normalisation) and aligns two laps on a
// common distance grid for comparison.
package laps

import (
	"math"
	"slices"
	"sort"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

// Missing marks an absent throttle or brake reading in a Car row.
const Missing = -1

// sentinel is the feed's "no data" value for throttle and brake.
const sentinel = 104

// Car is one car_data row; T is ms since the session epoch.
type Car struct {
	T                                      int64
	Speed, RPM, Gear, Throttle, Brake, DRS int64
}

// Loc is one location row.
type Loc struct{ T, X, Y, Z int64 }

// Timing is one row of the laps endpoint.
type Timing struct {
	Number     int
	StartMs    int64
	HasStart   bool
	DurationMs int32 // model.NoTime when null
	SectorMs   [3]int32
	PitOut     bool
}

const (
	// maxPadGapMs bounds how far outside the lap window a padding sample may lie.
	maxPadGapMs = 2000
	// maxLocGapMs bounds the location gap across which x/y/z are interpolated.
	maxLocGapMs = 2000
	// startTolMs is how far lap_duration may disagree with the gap between
	// consecutive date_start values and still count as an official lap time.
	startTolMs = 1500
)

// CleanCar sorts rows by time, drops duplicate timestamps (keeping the first
// row seen) and maps feed sentinels to Missing / boolean brake.
func CleanCar(rows []Car) []Car {
	out := slices.Clone(rows)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	out = slices.CompactFunc(out, func(a, b Car) bool { return a.T == b.T })
	for i := range out {
		r := &out[i]
		if r.Throttle < 0 || r.Throttle > 100 {
			r.Throttle = Missing
		}
		switch {
		case r.Brake == 0:
		case r.Brake > 0 && r.Brake <= 100:
			r.Brake = 1
		default:
			r.Brake = Missing
		}
	}
	return out
}

// CleanLoc sorts rows by time and drops duplicate timestamps.
func CleanLoc(rows []Loc) []Loc {
	out := slices.Clone(rows)
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return slices.CompactFunc(out, func(a, b Loc) bool { return a.T == b.T })
}

// Built is a sliced lap whose distance channel is not yet normalised.
type Built struct {
	Data *model.LapData
	// dist is the integrated distance (cm) of each sample from the lap start.
	dist []float64
	// RawLenCm is the integrated distance at StartMs+OfficialMs; 0 unless the
	// lap has an official time and telemetry covering it.
	RawLenCm float64
}

// Slice cuts one driver's cleaned telemetry into laps. Every timing row
// yields a lap, with zero samples when no telemetry falls inside it.
func Slice(driver int, timing []Timing, car []Car, loc []Loc) []*Built {
	timing = slices.Clone(timing)
	sort.SliceStable(timing, func(i, j int) bool { return timing[i].Number < timing[j].Number })
	timing = slices.CompactFunc(timing, func(a, b Timing) bool { return a.Number == b.Number })

	// A lap without date_start (seen on race lap 1) is anchored backwards from
	// the next lap when its duration is known.
	for i := len(timing) - 2; i >= 0; i-- {
		t, next := &timing[i], timing[i+1]
		if !t.HasStart && next.HasStart && t.DurationMs > 0 && next.Number == t.Number+1 {
			t.StartMs, t.HasStart = next.StartMs-int64(t.DurationMs), true
		}
	}

	out := make([]*Built, 0, len(timing))
	for i, t := range timing {
		lap := model.Lap{Driver: driver, Number: t.Number, OfficialMs: t.DurationMs, SectorMs: t.SectorMs}
		if t.PitOut {
			lap.Flags |= model.FlagPitOut
		}
		var next *Timing
		if i+1 < len(timing) && timing[i+1].Number == t.Number+1 {
			next = &timing[i+1]
			if next.PitOut {
				lap.Flags |= model.FlagPitIn
			}
		}
		b := &Built{Data: &model.LapData{Lap: lap}}
		out = append(out, b)
		if !t.HasStart {
			fillEmpty(b.Data)
			continue
		}
		lap.StartMs = t.StartMs
		hasDur := t.DurationMs > 0
		switch {
		case next != nil && next.PitOut && hasDur && !t.PitOut:
			// In-lap: the next lap's date_start is its pit exit, possibly long
			// after this lap ended at the line. lap_duration is line-to-line.
			lap.EndMs = t.StartMs + int64(t.DurationMs)
			lap.Flags |= model.FlagValidTime
		case next != nil && next.HasStart && next.StartMs > t.StartMs:
			lap.EndMs = next.StartMs
			if hasDur && !t.PitOut && abs64(next.StartMs-t.StartMs-int64(t.DurationMs)) <= startTolMs {
				lap.Flags |= model.FlagValidTime
			}
		case hasDur && !t.PitOut:
			lap.EndMs = t.StartMs + int64(t.DurationMs)
			lap.Flags |= model.FlagValidTime
		case len(car) > 0 && car[len(car)-1].T > t.StartMs:
			lap.EndMs = car[len(car)-1].T
		default:
			lap.EndMs = t.StartMs
		}
		cover := lap.EndMs
		if lap.ValidTime() {
			cover = max(cover, lap.StartMs+int64(lap.OfficialMs))
		}

		// Samples in [start, end), plus one padding sample on each side.
		lo := sort.Search(len(car), func(k int) bool { return car[k].T >= lap.StartMs })
		hi := sort.Search(len(car), func(k int) bool { return car[k].T >= cover })
		inside := hi - lo
		if lo > 0 && lap.StartMs-car[lo-1].T <= maxPadGapMs && (lo == len(car) || car[lo].T != lap.StartMs) {
			lo--
		}
		if hi < len(car) && car[hi].T-cover <= maxPadGapMs {
			hi++
		}
		b.Data.Lap = lap
		if inside <= 0 {
			fillEmpty(b.Data)
			continue
		}
		fill(b, car[lo:hi], loc)
	}
	return out
}

func fillEmpty(d *model.LapData) {
	for c := range d.Values {
		d.Values[c] = []int64{}
	}
}

func fill(b *Built, car []Car, loc []Loc) {
	d := b.Data
	n := len(car)
	d.Lap.Samples = n
	for c := range d.Values {
		d.Values[c] = make([]int64, n)
	}
	var thrValid, brkValid, locValid []bool
	missing := func(v *[]bool, i int) {
		if *v == nil {
			*v = make([]bool, n)
			for k := range *v {
				(*v)[k] = true
			}
		}
		(*v)[i] = false
	}
	cum := make([]float64, n) // cm from the first sample
	for i, r := range car {
		d.Values[model.ChT][i] = r.T
		d.Values[model.ChSpeed][i] = r.Speed
		d.Values[model.ChRPM][i] = r.RPM
		d.Values[model.ChGear][i] = r.Gear
		d.Values[model.ChDRS][i] = r.DRS
		if r.Throttle == Missing {
			missing(&thrValid, i)
		} else {
			d.Values[model.ChThrottle][i] = r.Throttle
		}
		if r.Brake == Missing {
			missing(&brkValid, i)
		} else {
			d.Values[model.ChBrake][i] = r.Brake
		}
		if x, y, z, ok := locAt(loc, r.T); ok {
			d.Values[model.ChX][i], d.Values[model.ChY][i], d.Values[model.ChZ][i] = x, y, z
		} else {
			missing(&locValid, i)
		}
		if i > 0 {
			// trapezoid; km/h * ms / 36 = cm
			cum[i] = cum[i-1] + float64(car[i-1].Speed+r.Speed)/2*float64(r.T-car[i-1].T)/36
		}
	}
	d.Valid[model.ChThrottle], d.Valid[model.ChBrake] = thrValid, brkValid
	d.Valid[model.ChX], d.Valid[model.ChY], d.Valid[model.ChZ] = locValid, locValid, locValid

	ts := d.Values[model.ChT]
	d0 := cumAt(ts, cum, car, d.Lap.StartMs)
	b.dist = make([]float64, n)
	for i := range cum {
		b.dist[i] = cum[i] - d0
	}
	if d.Lap.ValidTime() {
		end := d.Lap.StartMs + int64(d.Lap.OfficialMs)
		// Only trust the length when telemetry actually reaches the lap end.
		if ts[n-1] >= end-maxPadGapMs {
			b.RawLenCm = cumAt(ts, cum, car, end) - d0
		}
	}
}

// cumAt interpolates cumulative distance at time t, extrapolating at the
// edges with the nearest sample's speed.
func cumAt(ts []int64, cum []float64, car []Car, t int64) float64 {
	n := len(ts)
	i := sort.Search(n, func(k int) bool { return ts[k] >= t })
	switch {
	case i == 0:
		return cum[0] - float64(car[0].Speed)*float64(ts[0]-t)/36
	case i == n:
		return cum[n-1] + float64(car[n-1].Speed)*float64(t-ts[n-1])/36
	}
	f := float64(t-ts[i-1]) / float64(ts[i]-ts[i-1])
	return cum[i-1] + f*(cum[i]-cum[i-1])
}

// locAt linearly interpolates the car position at time t.
func locAt(loc []Loc, t int64) (x, y, z int64, ok bool) {
	i := sort.Search(len(loc), func(k int) bool { return loc[k].T >= t })
	if i < len(loc) && loc[i].T == t {
		return loc[i].X, loc[i].Y, loc[i].Z, true
	}
	if i == 0 || i == len(loc) {
		return 0, 0, 0, false
	}
	a, b := loc[i-1], loc[i]
	if b.T-a.T > maxLocGapMs {
		return 0, 0, 0, false
	}
	f := float64(t-a.T) / float64(b.T-a.T)
	lerp := func(p, q int64) int64 { return int64(math.Round(float64(p) + f*float64(q-p))) }
	return lerp(a.X, b.X), lerp(a.Y, b.Y), lerp(a.Z, b.Z), true
}

// Finalize computes the session's circuit length (metres) as the median
// integrated length of clean laps, then writes every lap's distance channel
// in centimetres. Laps with an official time are scaled so that the distance
// at StartMs+OfficialMs equals the circuit length exactly; that makes a
// lap's elapsed time at full distance equal to its official time.
func Finalize(all []*Built) float64 {
	fastest := int32(math.MaxInt32)
	for _, b := range all {
		if b.RawLenCm > 0 && b.Data.Lap.OfficialMs < fastest {
			fastest = b.Data.Lap.OfficialMs
		}
	}
	var clean []float64
	for _, b := range all {
		l := b.Data.Lap
		// Clean: official time within 110% of the session best, no pit entry,
		// and at least 2.5 Hz of telemetry.
		if b.RawLenCm > 0 && l.Flags&model.FlagPitIn == 0 &&
			float64(l.OfficialMs) <= 1.10*float64(fastest) &&
			float64(l.Samples) >= 2.5*float64(l.OfficialMs)/1000 {
			clean = append(clean, b.RawLenCm)
		}
	}
	var lengthCm float64
	if len(clean) > 0 {
		sort.Float64s(clean)
		lengthCm = clean[len(clean)/2]
		if len(clean)%2 == 0 {
			lengthCm = (clean[len(clean)/2-1] + clean[len(clean)/2]) / 2
		}
	}
	for _, b := range all {
		scale := 1.0
		// Laps whose integrated length is wildly off (telemetry gaps) are left
		// unscaled rather than stretched.
		if lengthCm > 0 && b.RawLenCm > 0.8*lengthCm && b.RawLenCm < 1.25*lengthCm {
			scale = lengthCm / b.RawLenCm
			b.Data.Lap.Flags |= model.FlagDistanceOK
		}
		dst := b.Data.Values[model.ChDistance]
		for i, v := range b.dist {
			dst[i] = int64(math.Round(v * scale))
		}
		b.dist = nil
	}
	return lengthCm / 100
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
