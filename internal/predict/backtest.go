package predict

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// Metrics scores a set of predictions against what happened.
type Metrics struct {
	Races         int     `json:"races"`
	WinnerHit     float64 `json:"winner_hit"`     // share of races where the top pick won
	WinnerProb    float64 `json:"winner_prob"`    // mean probability given to the actual winner
	PodiumOverlap float64 `json:"podium_overlap"` // mean number of the three podium finishers among the top three picks
	BrierWin      float64 `json:"brier_win"`      // mean squared error of the probabilities, per driver; lower is better
	BrierPodium   float64 `json:"brier_podium"`
	BrierPoints   float64 `json:"brier_points"`
	MAE           float64 `json:"mae"` // mean |expected - actual| finishing position, classified drivers
	// Coverage is the share of classified drivers who finished inside their
	// 10th-90th percentile range; a well calibrated range gives about 0.8.
	Coverage float64 `json:"coverage"`
}

// Score is the sum the tuner minimises.
func (m Metrics) Score() float64 { return m.BrierWin + m.BrierPodium + m.BrierPoints }

// Bucket is one row of a calibration table.
type Bucket struct {
	Lo, Hi    float64
	N         int
	Predicted float64 // mean predicted probability in the bucket
	Observed  float64 // share that actually happened
}

// forecast is the common shape of the model and the baseline.
type forecast struct {
	number                    int
	win, podium, points, mean float64
	lo, hi                    int // likely range; 0 = none
}

type scorer struct {
	m        Metrics
	drivers  int
	finished int
	podium   []([2]float64) // (predicted podium probability, 1 if it happened)
}

func (s *scorer) add(r *Race, fc []forecast) {
	actual := map[int]*Entry{}
	for i := range r.Drivers {
		actual[r.Drivers[i].Number] = &r.Drivers[i]
	}
	sq := func(p float64, hit bool) float64 {
		if hit {
			return (1 - p) * (1 - p)
		}
		return p * p
	}
	byWin := append([]forecast(nil), fc...)
	sort.SliceStable(byWin, func(a, b int) bool { return byWin[a].win > byWin[b].win })
	byPod := append([]forecast(nil), fc...)
	sort.SliceStable(byPod, func(a, b int) bool { return byPod[a].podium > byPod[b].podium })
	if len(fc) == 0 {
		return
	}
	s.m.Races++
	if e := actual[byWin[0].number]; e != nil && e.Pos == 1 {
		s.m.WinnerHit++
	}
	for i := 0; i < 3 && i < len(byPod); i++ {
		if e := actual[byPod[i].number]; e != nil && e.Pos >= 1 && e.Pos <= 3 {
			s.m.PodiumOverlap++
		}
	}
	for _, f := range fc {
		e := actual[f.number]
		if e == nil {
			continue
		}
		pos := e.Pos
		s.drivers++
		s.m.BrierWin += sq(f.win, pos == 1)
		s.m.BrierPodium += sq(f.podium, pos >= 1 && pos <= 3)
		s.m.BrierPoints += sq(f.points, pos >= 1 && pos <= 10)
		if pos == 1 {
			s.m.WinnerProb += f.win
		}
		hit := 0.0
		if pos >= 1 && pos <= 3 {
			hit = 1
		}
		s.podium = append(s.podium, [2]float64{f.podium, hit})
		if pos > 0 {
			s.finished++
			s.m.MAE += math.Abs(f.mean - float64(pos))
			if f.lo > 0 && pos >= f.lo && pos <= f.hi {
				s.m.Coverage++
			}
		}
	}
}

func (s *scorer) result() Metrics {
	m := s.m
	if m.Races == 0 {
		return m
	}
	fr, fd := float64(m.Races), float64(max(1, s.drivers))
	m.WinnerHit /= fr
	m.WinnerProb /= fr
	m.PodiumOverlap /= fr
	m.BrierWin /= fd
	m.BrierPodium /= fd
	m.BrierPoints /= fd
	m.MAE /= float64(max(1, s.finished))
	m.Coverage /= float64(max(1, s.finished))
	return m
}

func (s *scorer) calibration() []Bucket {
	edges := []float64{0, 0.05, 0.2, 0.5, 0.8, 1.0001}
	out := make([]Bucket, len(edges)-1)
	for i := range out {
		out[i].Lo, out[i].Hi = edges[i], math.Min(1, edges[i+1])
	}
	for _, p := range s.podium {
		for i := range out {
			if p[0] >= edges[i] && p[0] < edges[i+1] {
				out[i].N++
				out[i].Predicted += p[0]
				out[i].Observed += p[1]
			}
		}
	}
	for i := range out {
		if out[i].N > 0 {
			out[i].Predicted /= float64(out[i].N)
			out[i].Observed /= float64(out[i].N)
		}
	}
	return out
}

func fromPrediction(p *Prediction) []forecast {
	out := make([]forecast, len(p.Drivers))
	for i, d := range p.Drivers {
		out[i] = forecast{d.Number, d.Win, d.Podium, d.Points, d.Mean, d.P10, d.P90}
	}
	return out
}

// gridBaseline predicts from the grid slot alone: how often that slot won,
// reached the podium or scored in earlier races, and "finishes where it starts".
func gridBaseline(past []*Race, r *Race) []forecast {
	var win, pod, pts, starts [40]float64
	for _, p := range past {
		if !p.Run {
			continue
		}
		for _, e := range p.Drivers {
			if e.Grid < 1 || e.Grid >= len(starts) || e.DNS {
				continue
			}
			starts[e.Grid]++
			if e.Pos == 1 {
				win[e.Grid]++
			}
			if e.Pos >= 1 && e.Pos <= 3 {
				pod[e.Grid]++
			}
			if e.Pos >= 1 && e.Pos <= 10 {
				pts[e.Grid]++
			}
		}
	}
	var out []forecast
	for _, e := range r.Drivers {
		if e.DNS {
			continue
		}
		g := e.Grid
		if g < 1 || g >= len(starts) {
			g = 20
		}
		n := starts[g] + 1
		out = append(out, forecast{number: e.Number, win: win[g] / n, podium: pod[g] / n, points: pts[g] / n, mean: float64(g)})
	}
	return out
}

// MinTrain is how many run races the model needs before it predicts.
const MinTrain = 8

// Report is the outcome of a walk-forward backtest.
type Report struct {
	Params      Params
	TuneYears   string
	Tune        Metrics // seasons the parameters were tuned on
	TuneBase    Metrics
	Holdout     Metrics // later seasons, untouched by tuning
	HoldoutSim  Metrics // the simulation alone, without the grid-slot history
	PriorWeight float64 // share of the grid-slot history in the newest prediction
	HoldoutBase Metrics
	Calibration []Bucket // podium probabilities on the holdout
	ByYear      map[int][2]Metrics
	Inputs      map[int]*Inputs // by session key, with the tuned parameters
	Coef        []float64
	Tyres       map[string]Tyre
	PaceSD      float64
	LapSD       float64
}

// tuneUntil is the last season used to choose Params.
const tuneUntil = 2024

// Backtest predicts every race from the races before it. Model fits never
// see the race being predicted; Params are tuned on seasons up to tuneUntil
// and then frozen.
func Backtest(races []*Race, runs int) *Report {
	cache := map[int]*raceFit{}
	type item struct {
		race *Race
		in   *Inputs
		base []forecast
	}
	var items []item
	var last *Model
	done := 0
	for i, r := range races {
		if done >= MinTrain {
			m := Fit(races[:i], cache)
			last = m
			it := item{race: r, in: m.Inputs(r, DefaultParams)}
			it.in.Prior = gridPrior(races[:i], it.in.Drivers)
			if r.Run {
				it.base = gridBaseline(races[:i], r)
			}
			items = append(items, it)
		}
		if r.Run {
			done++
		}
	}

	eval := func(p Params, keep func(*Race) bool, n int) *scorer {
		// Races are independent: simulate them in parallel, score in order.
		preds := make([]*Prediction, len(items))
		var wg sync.WaitGroup
		sem := make(chan struct{}, runtime.GOMAXPROCS(0))
		for i, it := range items {
			if !it.race.Run || !keep(it.race) {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				in := *it.in
				in.Params, in.PriorWeight = p, 0
				preds[i] = Simulate(&in, n, uint64(it.race.Key))
			}()
		}
		wg.Wait()
		s := &scorer{}
		for i, it := range items {
			if preds[i] != nil {
				s.add(it.race, fromPrediction(preds[i]))
			}
		}
		return s
	}
	isTune := func(r *Race) bool { return r.Year <= tuneUntil }
	isHold := func(r *Race) bool { return r.Year > tuneUntil }

	// Coordinate descent over a small grid, two passes.
	best := DefaultParams
	bestScore := eval(best, isTune, runs).result().Score()
	grid := []struct {
		set  func(*Params, float64)
		vals []float64
	}{
		{func(p *Params, v float64) { p.StartSD = v }, []float64{0.25, 0.5, 1, 2, 3}},
		{func(p *Params, v float64) { p.PassDelta = v }, []float64{0.3, 0.6, 0.9, 1.3, 1.8, 2.5, 3.5}},
		{func(p *Params, v float64) { p.GridGap = v }, []float64{0.2, 0.35, 0.5, 0.75, 1.0, 1.5, 2.0, 3.0, 4.0}},
		{func(p *Params, v float64) { p.SCWindow = v }, []float64{0.1, 0.25, 0.4, 0.6}},
	}
	for pass := 0; pass < 2; pass++ {
		for _, g := range grid {
			for _, v := range g.vals {
				p := best
				g.set(&p, v)
				if p == best {
					continue
				}
				if sc := eval(p, isTune, runs).result().Score(); sc < bestScore {
					best, bestScore = p, sc
				}
			}
		}
	}

	// The share given to the grid-slot history is chosen race by race from
	// the races already predicted, so it never sees the race it is used on.
	weights := []float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 1}
	sums := make([]float64, len(weights)) // running Brier score per weight
	seen := 0
	final := make([]*Prediction, len(items))
	for i, it := range items {
		w := 0.5
		if seen >= 10 {
			bi := 0
			for k := range weights {
				if sums[k] < sums[bi] {
					bi = k
				}
			}
			w = weights[bi]
		}
		it.in.Params, it.in.PriorWeight = best, w
		final[i] = Simulate(it.in, runs, uint64(it.race.Key))
		if !it.race.Run {
			continue
		}
		for k, wk := range weights {
			in := *it.in
			in.PriorWeight = wk
			sc := &scorer{}
			sc.add(it.race, fromPrediction(Simulate(&in, runs, uint64(it.race.Key))))
			sums[k] += sc.result().Score()
		}
		seen++
	}
	score := func(keep func(*Race) bool, base bool) *scorer {
		s := &scorer{}
		for i, it := range items {
			if !it.race.Run || !keep(it.race) {
				continue
			}
			if base {
				s.add(it.race, it.base)
			} else {
				s.add(it.race, fromPrediction(final[i]))
			}
		}
		return s
	}
	simOnly := func(keep func(*Race) bool) Metrics {
		p := best
		return eval(p, keep, runs).result()
	}

	rep := &Report{Params: best, TuneYears: "2023-2024", ByYear: map[int][2]Metrics{}, Inputs: map[int]*Inputs{}}
	rep.Tune, rep.TuneBase = score(isTune, false).result(), score(isTune, true).result()
	hold := score(isHold, false)
	rep.Holdout, rep.HoldoutBase, rep.Calibration = hold.result(), score(isHold, true).result(), hold.calibration()
	rep.HoldoutSim = simOnly(isHold)
	years := map[int]bool{}
	for _, it := range items {
		if it.race.Run {
			years[it.race.Year] = true
		}
		rep.Inputs[it.race.Key] = it.in
		rep.PriorWeight = it.in.PriorWeight
	}
	for y := range years {
		only := func(r *Race) bool { return r.Year == y }
		rep.ByYear[y] = [2]Metrics{score(only, false).result(), score(only, true).result()}
	}
	if last != nil {
		rep.Coef, rep.PaceSD, rep.LapSD = last.coef, last.paceSD, last.lapSD
		rep.Tyres = map[string]Tyre{"SOFT": {last.beta[1], last.beta[3]}, "MEDIUM": {0, last.beta[4]}, "HARD": {last.beta[2], last.beta[5]}}
	}
	return rep
}

// ModelVersion changes whenever the simulator's output for the same inputs
// changes, so cached responses are not reused across such a change.
const ModelVersion = "1"

// PredictionsFile is what the server loads.
const PredictionsFile = "predictions.json.gz"

// Actual is how a driver's race really ended.
type Actual struct {
	Number int  `json:"number"`
	Pos    int  `json:"pos"` // 0 = not classified
	DNF    bool `json:"dnf,omitempty"`
}

// RaceFile is one race in the predictions file.
type RaceFile struct {
	Key     int       `json:"session_key"`
	Year    int       `json:"year"`
	Name    string    `json:"name"`
	Circuit string    `json:"circuit"`
	Date    time.Time `json:"date"`
	Run     bool      `json:"run"`
	Wet     bool      `json:"wet,omitempty"`
	Inputs  *Inputs   `json:"inputs"`
	Actual  []Actual  `json:"actual,omitempty"`
}

// File is the predictions file: per-race simulation inputs plus the
// backtest that says how far to trust them.
type File struct {
	Generated   time.Time  `json:"generated"`
	Holdout     Metrics    `json:"holdout"`
	HoldoutBase Metrics    `json:"holdout_baseline"`
	HoldoutFrom int        `json:"holdout_from"`
	Races       []RaceFile `json:"races"`
}

// BuildFile turns a backtest into the file the server reads.
func BuildFile(races []*Race, rep *Report, now time.Time) *File {
	f := &File{Generated: now.UTC().Truncate(time.Second), Holdout: rep.Holdout, HoldoutBase: rep.HoldoutBase, HoldoutFrom: tuneUntil + 1}
	for _, r := range races {
		in := rep.Inputs[r.Key]
		if in == nil || len(in.Drivers) == 0 || in.Laps < 2 {
			continue
		}
		rf := RaceFile{Key: r.Key, Year: r.Year, Name: r.Name, Circuit: r.Circuit, Date: r.Date, Run: r.Run, Wet: r.Wet(), Inputs: in}
		if r.Run {
			for _, e := range r.Drivers {
				rf.Actual = append(rf.Actual, Actual{e.Number, e.Pos, e.DNF})
			}
		}
		f.Races = append(f.Races, rf)
	}
	return f
}

// SaveFile writes the predictions file atomically.
func SaveFile(dataDir string, f *File) error {
	return writeGzipJSON(filepath.Join(dataDir, PredictionsFile), f)
}

// LoadFile reads the predictions file. A missing file is not an error: it
// returns nil, and the server simply has no predictions.
func LoadFile(dataDir string) (*File, error) {
	var f File
	err := readGzipJSON(filepath.Join(dataDir, PredictionsFile), &f)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Keep only races that can actually be simulated.
	kept := f.Races[:0]
	for _, r := range f.Races {
		if in := r.Inputs; in != nil && len(in.Drivers) > 0 && len(in.Drivers) <= MaxDrivers && in.Laps >= 2 && in.Laps <= MaxLaps {
			kept = append(kept, r)
		}
	}
	f.Races = kept
	return &f, nil
}
