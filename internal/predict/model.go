package predict

import (
	"math"
	"sort"
)

// Params are the simulation settings that are not measured directly from
// past races. They are tuned on 2023-2024 and evaluated on later seasons
// (see Backtest).
type Params struct {
	GridGap   float64 `json:"grid_gap"`   // seconds between consecutive cars after lap 1
	StartSD   float64 `json:"start_sd"`   // seconds of random shuffle over the first lap
	FollowGap float64 `json:"follow_gap"` // closest a car can sit behind another, seconds
	PassDelta float64 `json:"pass_delta"` // lap-time advantage giving a 50% chance to pass per lap, seconds
	PassScale float64 `json:"pass_scale"` // how sharply that chance rises with the advantage, seconds
	PaceScale float64 `json:"pace_scale"` // multiplier on the measured race-pace uncertainty
	SCWindow  float64 `json:"sc_window"`  // a stop planned within this fraction of the race is taken under a safety car
}

// DefaultParams is the starting point for tuning.
var DefaultParams = Params{GridGap: 0.35, StartSD: 1.0, FollowGap: 0.4, PassDelta: 0.6, PassScale: 0.3, PaceScale: 1.0, SCWindow: 0.25}

// Tyre is the fitted behaviour of one dry compound, as fractions of a lap time.
type Tyre struct {
	Offset float64 `json:"offset"` // pace on a new set relative to a new medium
	Slope  float64 `json:"slope"`  // loss per lap of age
}

// DriverIn is one car's starting state for the simulation.
type DriverIn struct {
	Number  int     `json:"number"`
	Acronym string  `json:"acronym"`
	Name    string  `json:"name"`
	Team    string  `json:"team"`
	Colour  string  `json:"colour,omitempty"`
	Grid    int     `json:"grid"`
	Pace    float64 `json:"pace"` // expected race pace relative to the field median, fraction of a lap
}

// PlanStint is one stint of a strategy, as a share of the race distance.
type PlanStint struct {
	Compound string  `json:"c"`
	Frac     float64 `json:"f"`
}

// NeutralPat is one neutralisation in a past race, positioned by race fraction.
type NeutralPat struct {
	Kind string  `json:"k"`
	Frac float64 `json:"f"`
	Laps int     `json:"n"`
}

// Inputs is everything Simulate needs for one race. It is built only from
// races that happened before it, plus its own qualifying result and grid.
type Inputs struct {
	Key      int             `json:"session_key"`
	Laps     int             `json:"laps"`
	RefSec   float64         `json:"ref_sec"`  // a typical racing lap, seconds
	PitLoss  float64         `json:"pit_loss"` // time lost by one stop, seconds
	SCPit    float64         `json:"sc_pit"`   // share of PitLoss a stop costs under a safety car
	Tyres    map[string]Tyre `json:"tyres"`
	LapSD    float64         `json:"lap_sd"`  // lap-to-lap scatter, fraction of a lap
	PaceSD   float64         `json:"pace_sd"` // uncertainty of a driver's race pace, fraction of a lap
	Drivers  []DriverIn      `json:"drivers"`
	Plans    [][]PlanStint   `json:"plans"`
	Neutrals [][]NeutralPat  `json:"neutrals"`
	DNFRate  float64         `json:"dnf_rate"`
	DNFFracs []float64       `json:"dnf_fracs"`
	Params   Params          `json:"params"`
	// Prior[i][p] is how often the grid slot of Drivers[i] finished in
	// position p+1 in earlier races; PriorWeight is its share of the final
	// answer (0 = simulation only).
	Prior       [][]float64 `json:"prior,omitempty"`
	PriorWeight float64     `json:"prior_weight,omitempty"`
	Trained     int         `json:"trained_on"` // races the fit used
}

func dry(c string) bool { return c == "SOFT" || c == "MEDIUM" || c == "HARD" }

// cleanLap is one representative racing lap.
type cleanLap struct {
	y        float64 // lap time / race reference - 1
	lapFrac  float64
	compound string
	age      float64
}

// raceFit is what one past race contributes to the model.
type raceFit struct {
	race    *Race
	ref     float64            // median clean lap, ms
	clean   map[int][]cleanLap // by driver number
	pace    map[int]float64    // tyre- and fuel-corrected pace relative to the field median
	quali   map[int]float64    // qualifying deficit to the field median
	pitLoss []float64          // seconds, green-flag stops
	scRatio []float64          // safety-car stop cost / green stop cost
	plans   [][]PlanStint
	neutral []NeutralPat
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

// robustSD estimates a standard deviation from the median absolute
// deviation, so a few laps in traffic or one ruined race do not inflate it.
func robustSD(res []float64) float64 {
	med := median(res)
	dev := make([]float64, len(res))
	for i, r := range res {
		dev[i] = math.Abs(r - med)
	}
	return 1.4826 * median(dev)
}

// stintAt returns the stint a lap belongs to and its index.
func (e *Entry) stintAt(lap int) (Stint, int) {
	for i, s := range e.Stints {
		if lap >= s.From && lap <= s.To {
			return s, i
		}
	}
	return Stint{}, -1
}

// qualiDeficits returns each driver's qualifying gap as a fraction of a lap.
//
// Times from different segments are not comparable (the track gets faster
// through the session), so each driver is measured against the best time of
// the last segment they set a lap in. Without segment times it falls back to
// the gap between best laps.
func qualiDeficits(r *Race) map[int]float64 {
	out := map[int]float64{}
	clip := func(v float64) float64 { return math.Max(0, math.Min(0.05, v)) }
	var best []int
	for _, e := range r.Drivers {
		for i, ms := range e.QualiSegMs {
			for len(best) <= i {
				best = append(best, 0)
			}
			if ms > 0 && (best[i] == 0 || ms < best[i]) {
				best[i] = ms
			}
		}
	}
	for _, e := range r.Drivers {
		for i := len(e.QualiSegMs) - 1; i >= 0; i-- {
			if e.QualiSegMs[i] > 0 && best[i] > 0 {
				out[e.Number] = clip(float64(e.QualiSegMs[i])/float64(best[i]) - 1)
				break
			}
		}
	}
	if len(out) >= 5 {
		return out
	}
	out = map[int]float64{}
	fastest := 0
	for _, e := range r.Drivers {
		if e.QualiMs > 0 && (fastest == 0 || e.QualiMs < fastest) {
			fastest = e.QualiMs
		}
	}
	for _, e := range r.Drivers {
		if e.QualiMs > 0 {
			out[e.Number] = clip(float64(e.QualiMs)/float64(fastest) - 1)
		}
	}
	if len(out) < 5 {
		return map[int]float64{}
	}
	return out
}

// prepare extracts the clean laps and the measured quantities of a past race.
func prepare(r *Race) *raceFit {
	f := &raceFit{race: r, clean: map[int][]cleanLap{}, quali: qualiDeficits(r)}
	for _, n := range r.Neutral {
		if r.Laps > 0 {
			f.neutral = append(f.neutral, NeutralPat{Kind: n.Kind, Frac: float64(n.From) / float64(r.Laps), Laps: n.To - n.From + 1})
		}
	}
	if !r.Run || r.Laps < 10 || r.Wet() {
		return f
	}
	type cand struct {
		ms       float64
		lap      int
		compound string
		age      int
	}
	cands := map[int][]cand{}
	var all []float64
	for i := range r.Drivers {
		e := &r.Drivers[i]
		for lap := 2; lap <= len(e.LapMs); lap++ {
			ms := float64(e.LapMs[lap-1])
			st, si := e.stintAt(lap)
			if ms <= 0 || si < 0 || !dry(st.Compound) || r.neutralLap(lap) {
				continue
			}
			if (lap == st.To && si < len(e.Stints)-1) || (lap == st.From && si > 0) {
				continue // in-lap or out-lap
			}
			cands[e.Number] = append(cands[e.Number], cand{ms, lap, st.Compound, st.Age + lap - st.From})
			all = append(all, ms)
		}
	}
	if len(all) < 100 {
		return f
	}
	f.ref = median(all)
	for n, cs := range cands {
		for _, c := range cs {
			if c.ms > f.ref*1.07 {
				continue // traffic, damage, a mistake
			}
			f.clean[n] = append(f.clean[n], cleanLap{c.ms/f.ref - 1, float64(c.lap) / float64(r.Laps), c.compound, float64(c.age)})
		}
	}

	// Pit loss: time across the in-lap and out-lap against two clean laps.
	span := func(e *Entry, lap int) float64 { // start of lap -> start of lap+2, ms
		if lap < 1 || lap+1 >= len(e.StartMs) || e.StartMs[lap-1] == 0 || e.StartMs[lap+1] == 0 {
			return 0
		}
		return float64(e.StartMs[lap+1] - e.StartMs[lap-1])
	}
	pitsAt := func(e *Entry, lap int) bool {
		for i := 0; i < len(e.Stints)-1; i++ {
			if e.Stints[i].To == lap || e.Stints[i].To == lap+1 || e.Stints[i].To == lap-1 {
				return true
			}
		}
		return false
	}
	var green []float64
	var sc []float64
	for i := range r.Drivers {
		e := &r.Drivers[i]
		var own []float64
		for _, c := range cands[e.Number] {
			own = append(own, c.ms)
		}
		if len(own) < 8 {
			continue
		}
		base := median(own)
		for si := 0; si < len(e.Stints)-1; si++ {
			lap := e.Stints[si].To
			d := span(e, lap)
			if d == 0 || lap < 2 {
				continue
			}
			if !r.neutralLap(lap) && !r.neutralLap(lap+1) {
				if loss := (d - 2*base) / 1000; loss > 10 && loss < 45 {
					green = append(green, loss)
				}
				continue
			}
			// Under a neutralisation: compare with cars that stayed out.
			var others []float64
			for j := range r.Drivers {
				o := &r.Drivers[j]
				if j != i && !pitsAt(o, lap) {
					if od := span(o, lap); od > 0 {
						others = append(others, od)
					}
				}
			}
			if len(others) >= 5 {
				if loss := (d - median(others)) / 1000; loss > 0 && loss < 45 {
					sc = append(sc, loss)
				}
			}
		}
	}
	f.pitLoss = green
	if g := median(green); len(green) >= 5 && g > 0 {
		for _, l := range sc {
			f.scRatio = append(f.scRatio, l/g)
		}
	}

	// Strategies of classified finishers.
	for i := range r.Drivers {
		e := &r.Drivers[i]
		if e.Pos == 0 || len(e.Stints) < 2 || len(e.Stints) > 4 {
			continue
		}
		var plan []PlanStint
		ok := e.Stints[0].From <= 1 && e.Stints[len(e.Stints)-1].To >= e.LapsDone-1
		for _, s := range e.Stints {
			frac := float64(s.To-s.From+1) / float64(r.Laps)
			if !dry(s.Compound) || frac < 0.08 {
				ok = false
			}
			plan = append(plan, PlanStint{s.Compound, math.Round(frac*1000) / 1000})
		}
		if ok {
			f.plans = append(f.plans, plan)
		}
	}
	return f
}

// solve solves the linear system a·x = b in place (Gaussian elimination with
// partial pivoting). It returns false for a singular system.
func solve(a [][]float64, b []float64) bool {
	n := len(b)
	for c := 0; c < n; c++ {
		p := c
		for r := c + 1; r < n; r++ {
			if math.Abs(a[r][c]) > math.Abs(a[p][c]) {
				p = r
			}
		}
		if math.Abs(a[p][c]) < 1e-12 {
			return false
		}
		a[c], a[p] = a[p], a[c]
		b[c], b[p] = b[p], b[c]
		for r := c + 1; r < n; r++ {
			m := a[r][c] / a[c][c]
			for k := c; k < n; k++ {
				a[r][k] -= m * a[c][k]
			}
			b[r] -= m * b[c]
		}
	}
	for c := n - 1; c >= 0; c-- {
		for k := c + 1; k < n; k++ {
			b[c] -= a[c][k] * b[k]
		}
		b[c] /= a[c][c]
	}
	return true
}

// ols fits y = x·beta by least squares with a small ridge term.
func ols(xs [][]float64, ys []float64, ridge float64) []float64 {
	if len(xs) == 0 {
		return nil
	}
	n := len(xs[0])
	a := make([][]float64, n)
	for i := range a {
		a[i] = make([]float64, n)
		a[i][i] = ridge
	}
	b := make([]float64, n)
	for r, x := range xs {
		for i := 0; i < n; i++ {
			b[i] += x[i] * ys[r]
			for j := 0; j < n; j++ {
				a[i][j] += x[i] * x[j]
			}
		}
	}
	if !solve(a, b) {
		return nil
	}
	return b
}

// tyreFeatures: race fraction (fuel and track evolution), compound offsets
// relative to medium, and wear per lap of age for each compound.
func tyreFeatures(c cleanLap) []float64 {
	x := make([]float64, 6)
	x[0] = c.lapFrac
	switch c.compound {
	case "SOFT":
		x[1], x[3] = 1, c.age
	case "MEDIUM":
		x[4] = c.age
	case "HARD":
		x[2], x[5] = 1, c.age
	}
	return x
}

// Model is fitted on a set of past races and produces Inputs for the next.
type Model struct {
	fits    []*raceFit
	beta    []float64 // tyre regression
	lapSD   float64
	coef    []float64 // pace regression: intercept, quali, driver form, team form, grid slot
	paceSD  float64
	refQ    float64 // typical race lap / median qualifying lap
	dnfRate float64
	dnfFrac []float64
	scRatio float64
	trained int
}

const (
	formDecay = 0.7
	formRaces = 8
	minClean  = 8
)

// form is the recency-weighted mean of the last few values (newest last).
func form(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	var sum, wsum float64
	w := 1.0
	for i := len(v) - 1; i >= 0 && i >= len(v)-formRaces; i-- {
		sum += w * v[i]
		wsum += w
		w *= formDecay
	}
	return sum / wsum, true
}

// paceFeatures builds the regression row for one driver from what is known
// before the race. Missing pieces fall back to the next best thing.
func paceFeatures(q float64, hasQ bool, drv []float64, team []float64, gridFrac float64) []float64 {
	df, hasD := form(drv)
	tf, hasT := form(team)
	if !hasD && hasT {
		df = tf
	}
	if !hasT && hasD {
		tf = df
	}
	if !hasQ {
		q = 0.012 // a typical gap, for a driver who set no time
	}
	return []float64{1, q, df, tf, gridFrac}
}

// frontWeight is the regression weight of a row by grid slot (0 = pole).
func frontWeight(gridFrac float64) float64 {
	if gridFrac <= 0.5 {
		return 1
	}
	return 0.2
}

// gridFracs places each driver's grid slot on a 0 (pole) to 1 (last) scale.
// Unlike the qualifying time it is comparable across the whole field: times
// set in different qualifying segments are not.
func gridFracs(r *Race) map[int]float64 {
	out := map[int]float64{}
	n := 0
	for _, e := range r.Drivers {
		n = max(n, e.Grid)
	}
	for _, e := range r.Drivers {
		g := e.Grid
		if g == 0 {
			g = n
		}
		if n > 1 {
			out[e.Number] = float64(g-1) / float64(n-1)
		}
	}
	return out
}

// Fit builds a model from past races (oldest first). Prepared fits may be
// reused between calls through the cache.
func Fit(past []*Race, cache map[int]*raceFit) *Model {
	m := &Model{scRatio: 0.5, refQ: 1.06, lapSD: 0.006, paceSD: 0.004}
	for _, r := range past {
		if !r.Run {
			continue
		}
		f := cache[r.Key]
		if f == nil {
			f = prepare(r)
			if cache != nil {
				cache[r.Key] = f
			}
		}
		m.fits = append(m.fits, f)
	}
	m.trained = len(m.fits)

	// 1. Tyre and fuel regression with a fixed effect per driver and race.
	var xs [][]float64
	var ys []float64
	for _, f := range m.fits {
		for _, laps := range f.clean {
			if len(laps) < minClean {
				continue
			}
			mean := make([]float64, 6)
			var my float64
			rows := make([][]float64, len(laps))
			for i, c := range laps {
				rows[i] = tyreFeatures(c)
				for k, v := range rows[i] {
					mean[k] += v
				}
				my += c.y
			}
			for i, c := range laps {
				for k := range rows[i] {
					rows[i][k] -= mean[k] / float64(len(laps))
				}
				xs = append(xs, rows[i])
				ys = append(ys, c.y-my/float64(len(laps)))
			}
		}
	}
	m.beta = ols(xs, ys, 1e-6)
	if m.beta == nil {
		m.beta = make([]float64, 6)
	}
	if len(ys) > 100 {
		res := make([]float64, len(ys))
		for i, x := range xs {
			res[i] = ys[i]
			for k, v := range x {
				res[i] -= m.beta[k] * v
			}
		}
		m.lapSD = robustSD(res)
	}

	// 2. Each driver's corrected race pace, relative to the field median.
	var ratios, scs []float64
	for _, f := range m.fits {
		f.pace = map[int]float64{}
		var all []float64
		for n, laps := range f.clean {
			if len(laps) < minClean {
				continue
			}
			res := make([]float64, len(laps))
			for i, c := range laps {
				res[i] = c.y
				for k, v := range tyreFeatures(c) {
					res[i] -= m.beta[k] * v
				}
			}
			f.pace[n] = median(res) // the median shrugs off laps lost in traffic
			all = append(all, f.pace[n])
		}
		med := median(all)
		for n := range f.pace {
			f.pace[n] -= med
		}
		var qs []float64
		for _, e := range f.race.Drivers {
			if e.QualiMs > 0 {
				qs = append(qs, float64(e.QualiMs))
			}
		}
		if f.ref > 0 && len(qs) >= 5 {
			ratios = append(ratios, f.ref/median(qs))
		}
		scs = append(scs, f.scRatio...)
	}
	if len(ratios) >= 3 {
		m.refQ = median(ratios)
	}
	if len(scs) >= 10 {
		m.scRatio = math.Max(0.2, math.Min(1, median(scs)))
	}

	// 3. Pace regression, walking forward so each row only sees earlier races.
	drv, team := map[string][]float64{}, map[string][]float64{}
	var px [][]float64
	var py, pw []float64
	for _, f := range m.fits {
		gf := gridFracs(f.race)
		for _, e := range f.race.Drivers {
			p, ok := f.pace[e.Number]
			if !ok {
				continue
			}
			q, hasQ := f.quali[e.Number]
			if len(drv[e.Acronym]) == 0 && len(team[e.Team]) == 0 && !hasQ {
				continue
			}
			// The front of the grid decides wins and podiums, and its qualifying
			// times are the comparable ones, so those rows count for more.
			w := math.Sqrt(frontWeight(gf[e.Number]))
			x := paceFeatures(q, hasQ, drv[e.Acronym], team[e.Team], gf[e.Number])
			for k := range x {
				x[k] *= w
			}
			px = append(px, x)
			py = append(py, p*w)
			pw = append(pw, w)
		}
		m.observe(f, drv, team)
	}
	m.coef = []float64{0, 0.5, 0.5, 0, 0}
	if len(py) >= 60 {
		if c := ols(px, py, 1e-7); c != nil {
			m.coef = c
			res := make([]float64, len(py))
			for i, x := range px {
				res[i] = py[i]
				for k, v := range x {
					res[i] -= c[k] * v
				}
				res[i] /= pw[i] // back to unweighted units
			}
			m.paceSD = robustSD(res)
		}
	}

	// 4. Retirements.
	var starts, dnfs int
	for _, f := range m.fits {
		for _, e := range f.race.Drivers {
			if e.DNS {
				continue
			}
			starts++
			if e.DNF && f.race.Laps > 0 {
				dnfs++
				m.dnfFrac = append(m.dnfFrac, math.Round(math.Min(1, float64(e.LapsDone)/float64(f.race.Laps))*1000)/1000)
			}
		}
	}
	m.dnfRate = 0.1
	if starts >= 100 {
		m.dnfRate = float64(dnfs) / float64(starts)
	}
	return m
}

// observe adds one race's measured pace to the form histories.
// Form follows the driver (by acronym), not the car number: numbers change
// hands between seasons, and the champion may switch to number 1.
func (m *Model) observe(f *raceFit, drv map[string][]float64, team map[string][]float64) {
	sum, cnt := map[string]float64{}, map[string]int{}
	for _, e := range f.race.Drivers {
		if p, ok := f.pace[e.Number]; ok {
			drv[e.Acronym] = append(drv[e.Acronym], p)
			sum[e.Team] += p
			cnt[e.Team]++
		}
	}
	for t, n := range cnt {
		team[t] = append(team[t], sum[t]/float64(n))
	}
}

// Inputs prepares the simulation of race r, which must be later than every
// race the model was fitted on.
func (m *Model) Inputs(r *Race, p Params) *Inputs {
	in := &Inputs{Key: r.Key, Laps: r.Laps, SCPit: m.scRatio, LapSD: m.lapSD, PaceSD: m.paceSD,
		DNFRate: m.dnfRate, DNFFracs: m.dnfFrac, Params: p, Trained: m.trained, Tyres: map[string]Tyre{}}
	in.Tyres["SOFT"] = Tyre{m.beta[1], m.beta[3]}
	in.Tyres["MEDIUM"] = Tyre{0, m.beta[4]}
	in.Tyres["HARD"] = Tyre{m.beta[2], m.beta[5]}

	drv, team := map[string][]float64{}, map[string][]float64{}
	var pitHere, pitAll []float64
	var plansHere, plansAll [][]PlanStint
	for _, f := range m.fits {
		m.observe(f, drv, team)
		pitAll = append(pitAll, f.pitLoss...)
		plansAll = append(plansAll, f.plans...)
		if f.race.Circuit == r.Circuit {
			pitHere = append(pitHere, f.pitLoss...)
			plansHere = append(plansHere, f.plans...)
			if f.ref > 0 {
				in.RefSec = f.ref / 1000
			}
		}
		if len(f.neutral) > 0 || f.race.Run {
			in.Neutrals = append(in.Neutrals, f.neutral)
			if f.race.Circuit == r.Circuit { // the circuit's own history counts three times
				in.Neutrals = append(in.Neutrals, f.neutral, f.neutral)
			}
		}
	}
	in.PitLoss = 22
	if len(pitHere) >= 5 {
		in.PitLoss = median(pitHere)
	} else if len(pitAll) >= 5 {
		in.PitLoss = median(pitAll)
	}
	in.Plans = plansHere
	if len(plansHere) < 12 {
		in.Plans = plansAll
		if len(in.Plans) > 150 {
			in.Plans = in.Plans[len(in.Plans)-150:]
		}
	}
	if len(in.Plans) == 0 {
		in.Plans = [][]PlanStint{{{"MEDIUM", 0.45}, {"HARD", 0.55}}}
	}
	if len(in.DNFFracs) > 200 {
		in.DNFFracs = in.DNFFracs[len(in.DNFFracs)-200:]
	}

	quali, gf := qualiDeficits(r), gridFracs(r)
	var qs []float64
	for _, e := range r.Drivers {
		if e.QualiMs > 0 {
			qs = append(qs, float64(e.QualiMs))
		}
	}
	if in.RefSec == 0 && len(qs) > 0 {
		in.RefSec = median(qs) / 1000 * m.refQ
	}
	if in.RefSec == 0 {
		in.RefSec = 90
	}
	back := 0
	for _, e := range r.Drivers {
		back = max(back, e.Grid)
	}
	for _, e := range r.Drivers {
		if e.DNS {
			continue
		}
		q, hasQ := quali[e.Number]
		x := paceFeatures(q, hasQ, drv[e.Acronym], team[e.Team], gf[e.Number])
		var pace float64
		for k, v := range x {
			pace += m.coef[k] * v
		}
		grid := e.Grid
		if grid == 0 { // pit lane start or unknown: behind everyone
			back++
			grid = back
		}
		in.Drivers = append(in.Drivers, DriverIn{e.Number, e.Acronym, e.Name, e.Team, e.Colour, grid, math.Round(pace*1e6) / 1e6})
	}
	sort.Slice(in.Drivers, func(i, j int) bool { return in.Drivers[i].Grid < in.Drivers[j].Grid })
	return in
}

// finishRanks orders every starter of a run race as a classification does:
// classified finishers by position, then the rest by laps completed.
func finishRanks(r *Race) map[int]int {
	var es []*Entry
	for i := range r.Drivers {
		if !r.Drivers[i].DNS {
			es = append(es, &r.Drivers[i])
		}
	}
	sort.SliceStable(es, func(a, b int) bool {
		x, y := es[a], es[b]
		if (x.Pos > 0) != (y.Pos > 0) {
			return x.Pos > 0
		}
		if x.Pos > 0 {
			return x.Pos < y.Pos
		}
		return x.LapsDone > y.LapsDone
	})
	out := map[int]int{}
	for i, e := range es {
		out[e.Number] = i + 1
	}
	return out
}

// gridPrior returns, for each driver, how the cars that started from the same
// grid slot finished in past races. Neighbouring slots are pooled a little to
// steady the small samples, and the table is balanced so that every finishing
// position is handed out about once.
func gridPrior(past []*Race, drivers []DriverIn) [][]float64 {
	n := len(drivers)
	const slots = 40
	var hist [slots][slots]float64
	for _, r := range past {
		if !r.Run {
			continue
		}
		rank := finishRanks(r)
		for _, e := range r.Drivers {
			if k := rank[e.Number]; k > 0 && e.Grid > 0 && e.Grid < slots {
				hist[e.Grid][min(k, slots-1)]++
			}
		}
	}
	out := make([][]float64, n)
	for i, d := range drivers {
		g := max(1, min(d.Grid, slots-2))
		row := make([]float64, n)
		for k := 1; k < slots; k++ {
			v := hist[g][k] + 0.5*hist[g-1][k] + 0.5*hist[g+1][k]
			row[min(k, n)-1] += v
		}
		for p := range row {
			row[p] += 0.2
		}
		out[i] = row
	}
	for it := 0; it < 20; it++ {
		for p := 0; p < n; p++ { // each position is taken once
			var sum float64
			for i := range out {
				sum += out[i][p]
			}
			for i := range out {
				out[i][p] /= sum
			}
		}
		for i := range out { // each driver finishes somewhere
			var sum float64
			for _, v := range out[i] {
				sum += v
			}
			for p := range out[i] {
				out[i][p] /= sum
			}
		}
	}
	for i := range out {
		for p := range out[i] {
			out[i][p] = math.Round(out[i][p]*1e4) / 1e4
		}
	}
	return out
}
