// Package predict estimates race outcomes by Monte Carlo simulation.
//
// It works from a compact record of every past Grand Prix (grid, result, lap
// times, tyre stints, safety cars) built from OpenF1, fits a small pace and
// tyre model on races before the one being predicted, and simulates the race
// many times. See docs/PREDICTIONS.md for the model and its measured accuracy.
package predict

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
)

// RacesFile is where the race records live inside the data directory.
const RacesFile = "races.json.gz"

// Race is everything the model knows about one Grand Prix.
type Race struct {
	Key     int       `json:"session_key"`
	Meeting int       `json:"meeting_key"`
	Year    int       `json:"year"`
	Name    string    `json:"name"`
	Circuit string    `json:"circuit"`
	Date    time.Time `json:"date"`
	// Laps is the race distance actually run (the winner's lap count), or the
	// expected distance for a race that has not been run.
	Laps    int       `json:"laps"`
	Run     bool      `json:"run"` // false: grid known, no result yet
	Drivers []Entry   `json:"drivers"`
	Neutral []Neutral `json:"neutral,omitempty"`
}

// Neutral is a run of laps under safety car, virtual safety car or red flag.
type Neutral struct {
	Kind string `json:"kind"` // SC, VSC, RED
	From int    `json:"from"` // first affected lap
	To   int    `json:"to"`   // last affected lap
}

// Stint is a run of laps on one set of tyres.
type Stint struct {
	From     int    `json:"from"`
	To       int    `json:"to"`
	Compound string `json:"compound"` // SOFT, MEDIUM, HARD, INTERMEDIATE, WET
	Age      int    `json:"age"`      // laps already on the set at From
}

// Entry is one driver in one race.
type Entry struct {
	Number  int    `json:"number"`
	Acronym string `json:"acronym"`
	Name    string `json:"name"`
	Team    string `json:"team"`
	Colour  string `json:"colour,omitempty"`

	Grid    int `json:"grid"`     // 0 = unknown
	QualiMs int `json:"quali_ms"` // best qualifying lap, 0 = none
	// QualiSegMs is the best lap in each qualifying segment (Q1, Q2, Q3),
	// 0 where the driver set none.
	QualiSegMs []int `json:"quali_seg_ms,omitempty"`

	Pos      int     `json:"pos"` // classified finishing position, 0 = none
	DNF      bool    `json:"dnf,omitempty"`
	DNS      bool    `json:"dns,omitempty"`
	DSQ      bool    `json:"dsq,omitempty"`
	LapsDone int     `json:"laps_done"`
	Points   float64 `json:"points,omitempty"`

	LapMs   []int32 `json:"lap_ms,omitempty"`   // index = lap-1, 0 = missing
	StartMs []int64 `json:"start_ms,omitempty"` // lap start since the race's first timed lap, 0 = missing
	Stints  []Stint `json:"stints,omitempty"`
}

// Wet reports whether a meaningful part of the race was run on wet tyres.
func (r *Race) Wet() bool {
	var wet, all int
	for _, d := range r.Drivers {
		for _, s := range d.Stints {
			n := s.To - s.From + 1
			all += n
			if s.Compound == "INTERMEDIATE" || s.Compound == "WET" {
				wet += n
			}
		}
	}
	return all > 0 && wet*10 > all
}

// neutralLap reports whether lap was run under a neutralisation, or is the
// lap right after one (restart laps are not representative either).
func (r *Race) neutralLap(lap int) bool {
	for _, n := range r.Neutral {
		if lap >= n.From && lap <= n.To+1 {
			return true
		}
	}
	return false
}

// LoadRaces reads the race records, oldest first.
func LoadRaces(dataDir string) ([]*Race, error) {
	var out []*Race
	if err := readGzipJSON(filepath.Join(dataDir, RacesFile), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SaveRaces writes the race records atomically.
func SaveRaces(dataDir string, races []*Race) error {
	return writeGzipJSON(filepath.Join(dataDir, RacesFile), races)
}

func readGzipJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	if err := json.NewDecoder(zr).Decode(v); err != nil {
		return fmt.Errorf("predict: %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeGzipJSON(path string, v any) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	zw := gzip.NewWriter(tmp)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		tmp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Fetch builds the record of every Grand Prix in the given seasons. Sprints
// are left out: they are short, have no mandatory stop and score differently.
// A race whose qualifying has finished but which has not been run is included
// with Run=false so it can be predicted.
func Fetch(ctx context.Context, c *openf1.Client, years []int, now time.Time, logf func(string, ...any)) ([]*Race, error) {
	var out []*Race
	for _, year := range years {
		sessions, _, err := c.Sessions(ctx, year)
		if err != nil {
			return nil, err
		}
		meetings, _, err := c.Meetings(ctx, year)
		if err != nil {
			return nil, err
		}
		names := map[int]string{}
		for _, m := range meetings {
			names[m.MeetingKey] = m.MeetingName
		}
		quali := map[int]openf1.Session{}
		for _, s := range sessions {
			if s.SessionName == "Qualifying" && !s.IsCancelled {
				quali[s.MeetingKey] = s
			}
		}
		for _, s := range sessions {
			if s.SessionName != "Race" || s.IsCancelled || strings.Contains(names[s.MeetingKey], "Testing") {
				continue
			}
			q, ok := quali[s.MeetingKey]
			if !ok || now.Before(q.DateEnd.Add(2*time.Hour)) {
				continue // nothing to predict from yet
			}
			run := !now.Before(s.DateEnd.Add(3 * time.Hour))
			r, err := fetchRace(ctx, c, s, q, names[s.MeetingKey], run)
			if err != nil {
				return nil, fmt.Errorf("predict: session %d (%s): %w", s.SessionKey, names[s.MeetingKey], err)
			}
			if r == nil {
				logf("skipped %d %s %d: no usable data", s.SessionKey, names[s.MeetingKey], year)
				continue
			}
			logf("race %d %d %s: %d drivers, %d laps, run=%v", r.Key, r.Year, r.Name, len(r.Drivers), r.Laps, r.Run)
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	// An unrun race has no distance yet: use the last one run at the circuit.
	for i, r := range out {
		if r.Laps > 0 {
			continue
		}
		for j := i - 1; j >= 0; j-- {
			if out[j].Circuit == r.Circuit && out[j].Laps > 0 {
				r.Laps = out[j].Laps
				break
			}
		}
	}
	return out, nil
}

func fetchRace(ctx context.Context, c *openf1.Client, s, q openf1.Session, meeting string, run bool) (*Race, error) {
	r := &Race{Key: s.SessionKey, Meeting: s.MeetingKey, Year: s.Year, Name: meeting, Circuit: s.Circuit, Date: s.DateStart.UTC(), Run: run}

	qres, _, err := c.Results(ctx, q.SessionKey)
	if err != nil {
		return nil, err
	}
	if len(qres) == 0 {
		return nil, nil
	}
	// Driver names: the race entry list once it exists, else qualifying's.
	from := q.SessionKey
	if run {
		from = s.SessionKey
	}
	drivers, _, err := c.Drivers(ctx, from)
	if err != nil {
		return nil, err
	}
	byNum := map[int]*Entry{}
	entry := func(n int) *Entry {
		if e := byNum[n]; e != nil {
			return e
		}
		e := &Entry{Number: n, Acronym: fmt.Sprint(n)}
		byNum[n] = e
		return e
	}
	for _, d := range drivers {
		e := entry(d.DriverNumber)
		e.Acronym, e.Name = d.NameAcronym, d.FullName
		if d.TeamName != nil {
			e.Team = *d.TeamName
		}
		if d.TeamColour != nil {
			e.Colour = *d.TeamColour
		}
	}
	for _, x := range qres {
		e := entry(x.DriverNumber)
		e.QualiMs = int(math.Round(x.BestSeconds() * 1000))
		for _, v := range x.Segments() {
			e.QualiSegMs = append(e.QualiSegMs, int(math.Round(v*1000)))
		}
		if !run && x.Position > 0 {
			e.Grid = int(x.Position) // provisional: penalties are not known here
		}
	}

	if run {
		res, _, err := c.Results(ctx, s.SessionKey)
		if err != nil {
			return nil, err
		}
		if len(res) == 0 {
			return nil, nil
		}
		for _, x := range res {
			e := entry(x.DriverNumber)
			e.DNF, e.DNS, e.DSQ = x.DNF, x.DNS, x.DSQ
			if x.Position > 0 && !x.DNF && !x.DNS && !x.DSQ {
				e.Pos = int(x.Position)
			}
			if x.Laps != nil {
				e.LapsDone = *x.Laps
				r.Laps = max(r.Laps, *x.Laps)
			}
			if x.Points != nil {
				e.Points = *x.Points
			}
		}
		// The grid as it formed up: each driver's first position record.
		pos, _, err := c.Positions(ctx, s.SessionKey)
		if err != nil {
			return nil, err
		}
		first := map[int]openf1.Position{}
		for _, p := range pos {
			if p.Date == nil {
				continue
			}
			if old, ok := first[p.DriverNumber]; !ok || p.Date.Before(*old.Date) {
				first[p.DriverNumber] = p
			}
		}
		for n, p := range first {
			if e := byNum[n]; e != nil {
				e.Grid = p.Position
			}
		}
		if err := fillLaps(ctx, c, s.SessionKey, r, byNum); err != nil {
			return nil, err
		}
	}

	for _, e := range byNum {
		// Someone who only appears in qualifying did not start the race.
		if run && e.LapsDone == 0 && len(e.LapMs) == 0 && !e.DNF && !e.DNS && !e.DSQ {
			continue
		}
		r.Drivers = append(r.Drivers, *e)
	}
	sort.Slice(r.Drivers, func(i, j int) bool {
		a, b := r.Drivers[i], r.Drivers[j]
		if (a.Grid == 0) != (b.Grid == 0) {
			return b.Grid == 0
		}
		if a.Grid != b.Grid {
			return a.Grid < b.Grid
		}
		return a.Number < b.Number
	})
	return r, nil
}

func fillLaps(ctx context.Context, c *openf1.Client, session int, r *Race, byNum map[int]*Entry) error {
	laps, _, err := c.Laps(ctx, session)
	if err != nil {
		return err
	}
	var epoch time.Time
	for _, l := range laps {
		if l.DateStart != nil && (epoch.IsZero() || l.DateStart.Before(epoch)) {
			epoch = *l.DateStart
		}
	}
	for _, l := range laps {
		e := byNum[l.DriverNumber]
		if e == nil || l.LapNumber < 1 || l.LapNumber > 200 {
			continue
		}
		for len(e.LapMs) < l.LapNumber {
			e.LapMs = append(e.LapMs, 0)
			e.StartMs = append(e.StartMs, 0)
		}
		if l.LapDuration != nil && *l.LapDuration > 0 {
			e.LapMs[l.LapNumber-1] = int32(math.Round(*l.LapDuration * 1000))
		}
		if l.DateStart != nil {
			e.StartMs[l.LapNumber-1] = l.DateStart.Sub(epoch).Milliseconds() + 1 // +1 keeps 0 meaning "missing"
		}
	}

	stints, _, err := c.Stints(ctx, session)
	if err != nil {
		return err
	}
	sort.Slice(stints, func(i, j int) bool { return stints[i].StintNumber < stints[j].StintNumber })
	for _, st := range stints {
		e := byNum[st.DriverNumber]
		if e == nil || st.LapStart == nil || st.LapEnd == nil || *st.LapEnd < *st.LapStart {
			continue
		}
		age := 0
		if st.TyreAge != nil {
			age = *st.TyreAge
		}
		e.Stints = append(e.Stints, Stint{From: *st.LapStart, To: *st.LapEnd, Compound: strings.ToUpper(st.Compound), Age: age})
	}

	rc, _, err := c.RaceControl(ctx, session)
	if err != nil {
		return err
	}
	sort.SliceStable(rc, func(i, j int) bool {
		if rc[i].Date == nil || rc[j].Date == nil {
			return false
		}
		return rc[i].Date.Before(*rc[j].Date)
	})
	r.Neutral = neutralisations(rc, r.Laps)
	return nil
}

// neutralisations turns race control messages into lap ranges.
func neutralisations(rc []openf1.RaceControl, raceLaps int) []Neutral {
	var out []Neutral
	open := map[string]int{} // kind -> index in out
	start := func(kind string, lap int) {
		if _, ok := open[kind]; ok {
			return
		}
		open[kind] = len(out)
		out = append(out, Neutral{Kind: kind, From: lap, To: raceLaps})
	}
	end := func(kind string, lap int) {
		if i, ok := open[kind]; ok {
			out[i].To = max(lap, out[i].From)
			delete(open, kind)
		}
	}
	for _, m := range rc {
		if m.LapNumber == nil {
			continue
		}
		lap, msg := *m.LapNumber, strings.ToUpper(m.Message)
		switch {
		case m.Flag != nil && *m.Flag == "RED":
			end("SC", lap)
			end("VSC", lap)
			if n := len(out); n == 0 || out[n-1].Kind != "RED" || out[n-1].To < lap {
				out = append(out, Neutral{Kind: "RED", From: lap, To: lap + 1})
			}
		case m.Category != "SafetyCar":
		case strings.Contains(msg, "VIRTUAL SAFETY CAR DEPLOYED"):
			start("VSC", lap)
		case strings.Contains(msg, "VIRTUAL SAFETY CAR ENDING"):
			end("VSC", lap)
		case strings.Contains(msg, "SAFETY CAR DEPLOYED"):
			end("VSC", lap) // a VSC is often upgraded to a full safety car
			start("SC", lap)
		case strings.Contains(msg, "SAFETY CAR IN THIS LAP"):
			end("SC", lap)
		}
	}
	return out
}
