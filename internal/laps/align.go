package laps

import (
	"math"
	"sort"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

// Resampled is a lap interpolated onto a distance grid. Missing values and
// grid points outside the lap's telemetry are NaN.
type Resampled struct {
	TimeS  []float64 // seconds since the lap start
	Values map[model.Channel][]float64
}

// Grid returns distances 0, step, 2*step, ... and finally lengthM itself, so
// the last point is exactly the end of the lap.
func Grid(lengthM, step float64) []float64 {
	if lengthM <= 0 || step <= 0 {
		return nil
	}
	n := int(math.Floor(lengthM / step))
	g := make([]float64, 0, n+2)
	for i := 0; i <= n; i++ {
		g = append(g, float64(i)*step)
	}
	if lengthM-g[len(g)-1] > 1e-6 {
		g = append(g, lengthM)
	}
	return g
}

// Resample interpolates ld at the given positions (metres along this lap's
// own distance channel). Smooth channels are linear; gear, DRS and brake hold
// the previous sample's value.
func Resample(ld *model.LapData, positions []float64, channels []model.Channel) *Resampled {
	out := &Resampled{TimeS: make([]float64, len(positions)), Values: make(map[model.Channel][]float64, len(channels))}
	for _, c := range channels {
		out.Values[c] = make([]float64, len(positions))
	}
	dist, ts := ld.Values[model.ChDistance], ld.Values[model.ChT]
	n := len(dist)
	start := float64(ld.Lap.StartMs)
	nan := math.NaN()
	for p, posM := range positions {
		pos := posM * 100
		// i is the first sample at or beyond pos; interpolate between i-1 and i.
		i := sort.Search(n, func(k int) bool { return float64(dist[k]) >= pos })
		var lo, hi int
		var f float64
		switch {
		case n == 0 || i == n || (i == 0 && float64(dist[0]) > pos):
			out.TimeS[p] = nan
			for _, c := range channels {
				out.Values[c][p] = nan
			}
			continue
		case i == 0:
			lo, hi, f = 0, 0, 0
		default:
			lo, hi = i-1, i
			f = (pos - float64(dist[lo])) / float64(dist[hi]-dist[lo]) // dist[hi] > dist[lo] by the search
		}
		out.TimeS[p] = (float64(ts[lo]) + f*float64(ts[hi]-ts[lo]) - start) / 1000
		for _, c := range channels {
			v, valid := ld.Values[c], ld.Valid[c]
			okLo := valid == nil || valid[lo]
			okHi := valid == nil || valid[hi]
			switch {
			case c.Step():
				// Hold the earlier sample until the later one is reached.
				src, ok := lo, okLo
				if f >= 1 {
					src, ok = hi, okHi
				}
				if ok {
					out.Values[c][p] = float64(v[src])
				} else {
					out.Values[c][p] = nan
				}
			case okLo && okHi:
				out.Values[c][p] = float64(v[lo]) + f*float64(v[hi]-v[lo])
			default:
				out.Values[c][p] = nan
			}
		}
	}
	return out
}

// Comparison is two laps on one distance grid.
type Comparison struct {
	DistanceM []float64
	A, B      *Resampled
	DeltaS    []float64 // time_B(d) - time_A(d); NaN where either lap has no data
	// FinalDeltaS is the delta at the end of the lap, NaN if unavailable.
	FinalDeltaS float64
}

// Compare resamples both laps onto a grid over lap A's circuit length. Lap B
// is mapped proportionally (its own circuit length corresponds to A's), so
// laps from different sessions at the same track line up.
func Compare(a, b *model.LapData, lengthA, lengthB, step float64, channels []model.Channel) *Comparison {
	grid := Grid(lengthA, step)
	posB := make([]float64, len(grid))
	for i, d := range grid {
		posB[i] = d * lengthB / lengthA
	}
	c := &Comparison{DistanceM: grid, A: Resample(a, grid, channels), B: Resample(b, posB, channels), FinalDeltaS: math.NaN()}
	// The last grid point is the timing line. A normalised lap reaches it at
	// exactly its official time; pin that, because interpolation is ambiguous
	// when a car is stationary there (distance stops increasing).
	if n := len(grid); n > 0 {
		if a.Lap.DistanceOK() {
			c.A.TimeS[n-1] = float64(a.Lap.OfficialMs) / 1000
		}
		if b.Lap.DistanceOK() {
			c.B.TimeS[n-1] = float64(b.Lap.OfficialMs) / 1000
		}
	}
	c.DeltaS = make([]float64, len(grid))
	for i := range grid {
		c.DeltaS[i] = c.B.TimeS[i] - c.A.TimeS[i]
	}
	if len(grid) > 0 {
		c.FinalDeltaS = c.DeltaS[len(grid)-1]
	}
	return c
}
