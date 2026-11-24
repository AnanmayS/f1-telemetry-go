package api

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
)

// predictRuns is how many races are simulated per prediction. The result is
// deterministic (the seed is the session key) and kept once computed.
const predictRuns = 10000

type predictions struct {
	file  *predict.File
	byKey map[int]*predict.RaceFile
	tag   string
	mu    sync.Mutex
	once  map[int]*sync.Once
	done  map[int]*predict.Prediction
}

func newPredictions(f *predict.File) *predictions {
	p := &predictions{file: f, byKey: map[int]*predict.RaceFile{}, once: map[int]*sync.Once{}, done: map[int]*predict.Prediction{}}
	if f != nil {
		p.tag = strconv.FormatInt(f.Generated.Unix(), 36)
		for i := range f.Races {
			p.byKey[f.Races[i].Key] = &f.Races[i]
		}
	}
	return p
}

func (p *predictions) simulate(rf *predict.RaceFile) *predict.Prediction {
	p.mu.Lock()
	o := p.once[rf.Key]
	if o == nil {
		o = new(sync.Once)
		p.once[rf.Key] = o
	}
	p.mu.Unlock()
	o.Do(func() {
		res := predict.Simulate(rf.Inputs, predictRuns, uint64(rf.Key))
		p.mu.Lock()
		p.done[rf.Key] = res
		p.mu.Unlock()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.done[rf.Key]
}

type predRaceJSON struct {
	Key     int    `json:"session_key"`
	Year    int    `json:"year"`
	Name    string `json:"name"`
	Circuit string `json:"circuit"`
	Date    string `json:"date"`
	Run     bool   `json:"run"`
	Wet     bool   `json:"wet"`
}

func predRace(rf *predict.RaceFile) predRaceJSON {
	return predRaceJSON{rf.Key, rf.Year, rf.Name, rf.Circuit, rf.Date.Format("2006-01-02"), rf.Run, rf.Wet}
}

func (s *Server) handlePredictions(w http.ResponseWriter, r *http.Request) *apiError {
	p := s.predictions
	if notModified(w, r, etag("predictions", p.tag), cacheList) {
		return nil
	}
	races := []predRaceJSON{}
	out := map[string]any{"races": races}
	if p.file != nil {
		for i := len(p.file.Races) - 1; i >= 0; i-- { // newest first
			races = append(races, predRace(&p.file.Races[i]))
		}
		out["races"] = races
		out["backtest"] = map[string]any{"from_year": p.file.HoldoutFrom, "model": p.file.Holdout, "grid_only": p.file.HoldoutBase}
		out["generated"] = p.file.Generated
	}
	writeObject(w, r, out)
	return nil
}

type predDriverJSON struct {
	predict.DriverOut
	Pace   float64 `json:"pace"`             // expected race pace vs the field median, percent of a lap
	Actual *int    `json:"actual,omitempty"` // classified finishing position
	Out    bool    `json:"retired,omitempty"`
}

func (s *Server) handlePrediction(w http.ResponseWriter, r *http.Request) *apiError {
	key, e := pathInt(r, "key", 1, 1<<30)
	if e != nil {
		return e
	}
	rf := s.predictions.byKey[key]
	if rf == nil {
		return notFound("no prediction for this session")
	}
	if notModified(w, r, etag("prediction", s.predictions.tag, strconv.Itoa(key)), cacheList) {
		return nil
	}
	res := s.predictions.simulate(rf)
	actual := map[int]predict.Actual{}
	for _, a := range rf.Actual {
		actual[a.Number] = a
	}
	pace := map[int]float64{}
	for _, d := range rf.Inputs.Drivers {
		pace[d.Number] = d.Pace * 100
	}
	drivers := make([]predDriverJSON, 0, len(res.Drivers))
	for _, d := range res.Drivers {
		j := predDriverJSON{DriverOut: d, Pace: pace[d.Number]}
		if a, ok := actual[d.Number]; ok {
			if a.Pos > 0 {
				pos := a.Pos
				j.Actual = &pos
			}
			j.Out = a.DNF
		}
		drivers = append(drivers, j)
	}
	in := rf.Inputs
	writeObject(w, r, map[string]any{
		"race": predRace(rf), "runs": res.Runs, "drivers": drivers,
		"inputs": map[string]any{"laps": in.Laps, "pit_loss_s": in.PitLoss, "lap_s": in.RefSec, "trained_on": in.Trained,
			"retirement_rate": in.DNFRate, "strategies": len(in.Plans)},
	})
	return nil
}
