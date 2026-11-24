package predict

import (
	"math"
	"math/rand/v2"
	"sort"
)

// DriverOut is the simulated outlook for one driver.
type DriverOut struct {
	Number  int     `json:"number"`
	Acronym string  `json:"acronym"`
	Name    string  `json:"name"`
	Team    string  `json:"team"`
	Colour  string  `json:"colour,omitempty"`
	Grid    int     `json:"grid"`
	Win     float64 `json:"win"`
	Podium  float64 `json:"podium"`
	Points  float64 `json:"points"` // top ten
	DNF     float64 `json:"dnf"`
	Mean    float64 `json:"mean"` // mean finishing position
	P10     int     `json:"p10"`  // finishing position percentiles, given that the car finishes
	P50     int     `json:"p50"`
	P90     int     `json:"p90"`
	// Dist[i] is the chance of finishing in position i+1 (retirements are
	// placed behind the finishers, as in a real classification).
	Dist []float64 `json:"dist"`
}

// Prediction is the result of simulating one race many times.
type Prediction struct {
	Key     int         `json:"session_key"`
	Runs    int         `json:"runs"`
	Drivers []DriverOut `json:"drivers"` // most likely winner first
}

type car struct {
	pace    float64
	t       float64 // race time so far, seconds
	lapTime float64
	plan    []PlanStint
	tyres   []Tyre // per stint
	ends    []int  // last lap of each stint
	stint   int
	age     int
	dnfLap  int // 0 = finishes
	out     bool
	pitted  bool
	laps    int
}

const (
	scLap  = 1.45 // lap time behind the safety car, relative to a racing lap
	vscLap = 1.30
	scGap  = 0.6 // seconds between cars once bunched up
)

// Simulate runs the race `runs` times. The same inputs and seed always give
// the same answer.
func Simulate(in *Inputs, runs int, seed uint64) *Prediction {
	n := len(in.Drivers)
	out := &Prediction{Key: in.Key, Runs: runs, Drivers: make([]DriverOut, n)}
	if n == 0 || in.Laps < 2 || runs < 1 {
		return out
	}
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	counts := make([][]int, n)
	for i := range counts {
		counts[i] = make([]int, n)
	}
	dnfs := make([]int, n)
	finished := make([][]int, n) // positions in the runs the car finished
	for i := range finished {
		finished[i] = make([]int, n)
	}
	cars := make([]car, n)
	order := make([]int, n)
	kind := make([]byte, in.Laps+2) // per lap: 0 green, 'S' safety car, 'V' virtual
	p := in.Params
	window := int(math.Round(p.SCWindow * float64(in.Laps)))
	planTyres := make([][]Tyre, len(in.Plans))
	for i, plan := range in.Plans {
		for _, s := range plan {
			planTyres[i] = append(planTyres[i], in.Tyres[s.Compound])
		}
	}
	// The order barely changes from lap to lap, so an insertion sort is
	// close to linear here and avoids sort.Slice's overhead.
	byTime := func(live []int) {
		for a := 1; a < len(live); a++ {
			v := live[a]
			b := a
			for b > 0 && cars[live[b-1]].t > cars[v].t {
				live[b] = live[b-1]
				b--
			}
			live[b] = v
		}
	}

	for run := 0; run < runs; run++ {
		// Neutralisations: replay the pattern of one past race.
		for i := range kind {
			kind[i] = 0
		}
		if len(in.Neutrals) > 0 {
			for _, nt := range in.Neutrals[rng.IntN(len(in.Neutrals))] {
				from := max(1, int(math.Round(nt.Frac*float64(in.Laps))))
				k := byte('S')
				if nt.Kind == "VSC" {
					k = 'V'
				}
				for l := from; l < from+nt.Laps && l <= in.Laps; l++ {
					if kind[l] != 'S' {
						kind[l] = k
					}
				}
			}
		}

		for i := range cars {
			d := in.Drivers[i]
			c := &cars[i]
			pi := rng.IntN(len(in.Plans))
			*c = car{pace: d.Pace + rng.NormFloat64()*in.PaceSD*p.PaceScale, plan: in.Plans[pi], tyres: planTyres[pi], ends: c.ends[:0]}
			var acc float64
			for _, s := range c.plan {
				acc += s.Frac
				c.ends = append(c.ends, int(math.Round(acc*float64(in.Laps))))
			}
			c.ends[len(c.ends)-1] = in.Laps
			if rng.Float64() < in.DNFRate && len(in.DNFFracs) > 0 {
				c.dnfLap = max(1, int(math.Ceil(in.DNFFracs[rng.IntN(len(in.DNFFracs))]*float64(in.Laps))))
				if c.dnfLap >= in.Laps {
					c.dnfLap = in.Laps - 1
				}
			}
			// Lap 1: the grid spread out, plus the shuffle of the start.
			c.t = float64(d.Grid-1)*p.GridGap + rng.NormFloat64()*p.StartSD
			c.age, c.laps = 1, 1
			if c.dnfLap == 1 {
				c.out, c.laps = true, 0
			}
		}

		for lap := 2; lap <= in.Laps; lap++ {
			// Running order before this lap.
			m := 0
			for i := range cars {
				if !cars[i].out {
					order[m] = i
					m++
				}
			}
			live := order[:m]
			byTime(live)

			k := kind[lap]
			neutralStart := k != 0 && kind[lap-1] == 0
			for _, i := range live {
				c := &cars[i]
				c.pitted = false
				switch k {
				case 'S':
					c.lapTime = in.RefSec * scLap
				case 'V':
					c.lapTime = in.RefSec * vscLap
				default:
					ty := c.tyres[c.stint]
					c.lapTime = in.RefSec * (1 + c.pace + ty.Offset + ty.Slope*float64(c.age) + rng.NormFloat64()*in.LapSD)
				}
				c.t += c.lapTime
				c.age++
				c.laps = lap
				// Stop at the end of this lap: as planned, or early because the
				// field is neutralised and the stop is cheap.
				if c.stint < len(c.plan)-1 && lap < in.Laps-1 {
					due := lap >= c.ends[c.stint]
					early := neutralStart && c.ends[c.stint]-lap <= window
					if due || early {
						loss := in.PitLoss
						if k != 0 {
							loss *= in.SCPit
						}
						c.t += loss
						c.stint++
						c.age = 0
						c.pitted = true
					}
				}
			}

			if k == 0 {
				// Cars that catch the one ahead must pass it or sit behind it.
				for pos := 1; pos < m; pos++ {
					c, ahead := &cars[live[pos]], &cars[live[pos-1]]
					if c.pitted || ahead.pitted || c.t >= ahead.t+p.FollowGap {
						continue
					}
					adv := ahead.lapTime - c.lapTime
					if rng.Float64() < 1/(1+math.Exp(-(adv-p.PassDelta)/p.PassScale)) {
						// Through, but no further than the next car up the road.
						if pos >= 2 {
							if next := &cars[live[pos-2]]; !next.pitted && c.t < next.t+p.FollowGap {
								c.t = next.t + p.FollowGap
							}
						}
						continue
					}
					c.t = ahead.t + p.FollowGap
				}
			} else if k == 'S' && neutralStart {
				// The field closes up behind the safety car.
				byTime(live)
				lead := cars[live[0]].t
				for pos, i := range live {
					if gap := float64(pos) * scGap; cars[i].t > lead+gap {
						cars[i].t = lead + gap
					}
				}
			}

			for _, i := range live {
				if cars[i].dnfLap == lap {
					cars[i].out = true
				}
			}
		}

		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(a, b int) bool {
			x, y := &cars[order[a]], &cars[order[b]]
			if x.out != y.out {
				return y.out
			}
			if x.out {
				return x.laps > y.laps
			}
			return x.t < y.t
		})
		for pos, i := range order {
			counts[i][pos]++
			if cars[i].out {
				dnfs[i]++
			} else {
				finished[i][pos]++
			}
		}
	}

	fr := float64(runs)
	blend := in.PriorWeight > 0 && len(in.Prior) == n
	for i, d := range in.Drivers {
		o := DriverOut{Number: d.Number, Acronym: d.Acronym, Name: d.Name, Team: d.Team, Colour: d.Colour, Grid: d.Grid, Dist: make([]float64, n)}
		for pos, c := range counts[i] {
			o.Dist[pos] = float64(c) / fr
			if blend && len(in.Prior[i]) == n {
				o.Dist[pos] = (1-in.PriorWeight)*o.Dist[pos] + in.PriorWeight*in.Prior[i][pos]
			}
		}
		o.DNF = float64(dnfs[i]) / fr
		o.summarise(finished[i])
		out.Drivers[i] = o
	}
	sort.SliceStable(out.Drivers, func(a, b int) bool {
		x, y := out.Drivers[a], out.Drivers[b]
		if x.Win != y.Win {
			return x.Win > y.Win
		}
		return x.Mean < y.Mean
	})
	return out
}

// summarise fills the headline numbers from the position distribution, and
// the percentiles from the positions of the runs the car finished.
func (o *DriverOut) summarise(finished []int) {
	var mean float64
	o.Win, o.Podium, o.Points = 0, 0, 0
	for pos, pr := range o.Dist {
		mean += pr * float64(pos+1)
		if pos < 1 {
			o.Win += pr
		}
		if pos < 3 {
			o.Podium += pr
		}
		if pos < 10 {
			o.Points += pr
		}
		o.Dist[pos] = math.Round(pr*1e4) / 1e4
	}
	o.Mean = math.Round(mean*100) / 100

	total := 0
	for _, c := range finished {
		total += c
	}
	o.P10, o.P50, o.P90 = o.Grid, o.Grid, o.Grid
	if total == 0 {
		return
	}
	at := func(q float64) int {
		need, cum := q*float64(total), 0.0
		for pos, c := range finished {
			cum += float64(c)
			if cum >= need {
				return pos + 1
			}
		}
		return len(finished)
	}
	o.P10, o.P50, o.P90 = at(0.1), at(0.5), at(0.9)
}
