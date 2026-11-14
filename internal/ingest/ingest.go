// Package ingest builds segments from OpenF1 data: discover sessions, fetch
// (through the raw cache), normalise, slice laps and write segment + manifest.
package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

// FirstSeason is the first season OpenF1 serves.
const FirstSeason = 2023

// settleTime is how long after a session ends before it is ingested, so the
// feed is complete and the API is no longer restricted.
const settleTime = 3 * time.Hour

// Ingester ingests sessions into DataDir.
type Ingester struct {
	Client  *openf1.Client
	DataDir string
	Workers int // concurrent per-driver fetch+slice jobs
	Force   bool
	Log     *slog.Logger
	Now     func() time.Time
}

// Result summarises one session.
type Result struct {
	Session      model.Session
	Skipped      string // non-empty reason when no segment was written
	Unchanged    bool   // already in the manifest
	Laps         int
	Samples      int64
	RawJSONBytes int64
	SegmentBytes int64
	Elapsed      time.Duration
}

// Discover lists the ingestible sessions of a season in date order: not
// cancelled and finished long enough ago.
func (in *Ingester) Discover(ctx context.Context, year int) ([]model.Session, error) {
	raw, _, err := in.Client.Sessions(ctx, year)
	if err != nil {
		return nil, err
	}
	meetings, _, err := in.Client.Meetings(ctx, year)
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	for _, m := range meetings {
		names[m.MeetingKey] = m.MeetingName
	}
	var out []model.Session
	for _, s := range raw {
		if s.IsCancelled || s.DateEnd.IsZero() || in.now().Before(s.DateEnd.Add(settleTime)) {
			continue
		}
		out = append(out, model.Session{
			Key: s.SessionKey, MeetingKey: s.MeetingKey, Year: s.Year,
			MeetingName: names[s.MeetingKey], Name: s.SessionName, Type: s.SessionType,
			Circuit: s.Circuit, Country: s.Country, Start: s.DateStart.UTC(), End: s.DateEnd.UTC(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

func (in *Ingester) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

// Session ingests one session and updates the manifest. It is idempotent:
// a session already in the manifest is left alone unless Force is set.
func (in *Ingester) Session(ctx context.Context, s model.Session) (Result, error) {
	start := time.Now()
	res := Result{Session: s}
	man, err := store.LoadManifest(in.DataDir)
	if err != nil {
		return res, err
	}
	if !in.Force {
		if _, ok := man.Find(s.Key); ok {
			res.Unchanged = true
			return res, nil
		}
		if reason, ok := man.Skipped[s.Key]; ok {
			res.Unchanged, res.Skipped = true, reason
			return res, nil
		}
	}
	data, meta, nLaps, err := in.Build(ctx, s)
	if err != nil {
		return res, err
	}
	res.Laps, res.Samples, res.RawJSONBytes = nLaps, meta.Samples, meta.RawJSONBytes
	if meta.Samples == 0 {
		res.Skipped = "no telemetry"
		if len(meta.Drivers) == 0 {
			res.Skipped = "no drivers"
		}
		man.Skip(s.Key, res.Skipped)
		res.Elapsed = time.Since(start)
		return res, man.Save(in.DataDir)
	}
	segDir := filepath.Join(in.DataDir, store.SegmentDir)
	if err := os.MkdirAll(segDir, 0o755); err != nil {
		return res, err
	}
	name, sum := store.SegmentFileName(s.Key, data)
	if err := segment.WriteFile(filepath.Join(segDir, name), data); err != nil {
		return res, err
	}
	// The segment is durable before the manifest names it, so a crash here
	// leaves at worst an unreferenced file, never a visible half-written one.
	old := man.Put(store.Entry{SessionKey: s.Key, Year: s.Year, File: name, Size: int64(len(data)), SHA256: sum, Samples: meta.Samples})
	if err := man.Save(in.DataDir); err != nil {
		return res, err
	}
	if old != "" {
		// Unlinking is safe: servers holding the old file keep their mapping.
		os.Remove(filepath.Join(segDir, old))
	}
	res.SegmentBytes = int64(len(data))
	res.Elapsed = time.Since(start)
	return res, nil
}

// Raw is one session's cleaned input: everything a segment is built from.
type Raw struct {
	Meta   segment.Meta // Session, Drivers, RawJSONBytes, RawSamples filled in
	Timing map[int][]laps.Timing
	Car    map[int][]laps.Car // cleaned (sorted, de-duplicated)
	Loc    map[int][]laps.Loc
}

// Fetch downloads (or reads from the raw cache) everything for one session.
// Drivers without laps are listed in Meta.Drivers but have no telemetry.
func (in *Ingester) Fetch(ctx context.Context, s model.Session) (*Raw, error) {
	raw := &Raw{Timing: map[int][]laps.Timing{}, Car: map[int][]laps.Car{}, Loc: map[int][]laps.Loc{}}
	raw.Meta.Session = s
	drivers, n, err := in.Client.Drivers(ctx, s.Key)
	if err != nil {
		return nil, err
	}
	raw.Meta.RawJSONBytes += int64(n)
	if len(drivers) == 0 {
		return raw, nil
	}
	rawLaps, n, err := in.Client.Laps(ctx, s.Key)
	if err != nil {
		return nil, err
	}
	raw.Meta.RawJSONBytes += int64(n)
	for _, l := range rawLaps {
		raw.Timing[l.DriverNumber] = append(raw.Timing[l.DriverNumber], convertLap(s.Start, l))
	}

	seen := map[int]bool{}
	for _, d := range drivers {
		if seen[d.DriverNumber] || d.DriverNumber < 0 || d.DriverNumber > 255 {
			continue
		}
		seen[d.DriverNumber] = true
		md := model.Driver{Number: d.DriverNumber, Acronym: d.NameAcronym, Name: d.FullName}
		if d.TeamName != nil {
			md.Team = *d.TeamName
		}
		if d.TeamColour != nil {
			md.Colour = *d.TeamColour
		}
		raw.Meta.Drivers = append(raw.Meta.Drivers, md)
	}
	sort.Slice(raw.Meta.Drivers, func(i, j int) bool { return raw.Meta.Drivers[i].Number < raw.Meta.Drivers[j].Number })

	var mu sync.Mutex
	window := openf1.SessionWindow(s.Start, s.End)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(in.Workers, 1))
	for _, d := range raw.Meta.Drivers {
		if len(raw.Timing[d.Number]) == 0 {
			continue // no laps, so nothing to slice; do not spend requests on it
		}
		g.Go(func() error {
			car, nCar, err := in.Client.CarData(gctx, s.Key, d.Number, window)
			if err != nil {
				return fmt.Errorf("driver %d car_data: %w", d.Number, err)
			}
			loc, nLoc, err := in.Client.Location(gctx, s.Key, d.Number, window)
			if err != nil {
				return fmt.Errorf("driver %d location: %w", d.Number, err)
			}
			cleanCar, cleanLoc := laps.CleanCar(convertCar(s.Start, car)), laps.CleanLoc(convertLoc(s.Start, loc))
			mu.Lock()
			raw.Car[d.Number], raw.Loc[d.Number] = cleanCar, cleanLoc
			raw.Meta.RawJSONBytes += int64(nCar + nLoc)
			raw.Meta.RawSamples += int64(len(car))
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return raw, nil
}

// BuildSegment slices every driver's laps, normalises distance and
// serialises the session. It returns nil data when there is no telemetry.
// The live path uses the same function, so a sealed live session and a
// batch-ingested one are built identically.
func BuildSegment(meta segment.Meta, timing map[int][]laps.Timing, car map[int][]laps.Car, loc map[int][]laps.Loc) (data []byte, out segment.Meta, nLaps int, err error) {
	var built []*laps.Built
	for _, d := range meta.Drivers {
		if len(timing[d.Number]) == 0 {
			continue
		}
		built = append(built, laps.Slice(d.Number, timing[d.Number], car[d.Number], loc[d.Number])...)
	}
	meta.CircuitLengthM = math.Round(laps.Finalize(built)*100) / 100
	meta.Samples = 0
	lapData := make([]*model.LapData, len(built))
	for i, b := range built {
		lapData[i] = b.Data
		meta.Samples += int64(b.Data.Lap.Samples)
	}
	if meta.Samples == 0 {
		return nil, meta, len(built), nil
	}
	data, err = segment.Build(meta, lapData)
	return data, meta, len(built), err
}

// Build fetches a session and returns its serialised segment.
func (in *Ingester) Build(ctx context.Context, s model.Session) (data []byte, meta segment.Meta, nLaps int, err error) {
	raw, err := in.Fetch(ctx, s)
	if err != nil {
		return nil, segment.Meta{Session: s}, 0, err
	}
	return BuildSegment(raw.Meta, raw.Timing, raw.Car, raw.Loc)
}

func ms(epoch, t time.Time) int64 { return t.Sub(epoch).Milliseconds() }

func secMs(v *float64) int32 {
	if v == nil || *v <= 0 || *v > 86_400 {
		return model.NoTime
	}
	return int32(math.Round(*v * 1000))
}

func convertLap(epoch time.Time, l openf1.Lap) laps.Timing {
	t := laps.Timing{
		Number:     l.LapNumber,
		DurationMs: secMs(l.LapDuration),
		SectorMs:   [3]int32{secMs(l.Sector1), secMs(l.Sector2), secMs(l.Sector3)},
		PitOut:     l.IsPitOutLap != nil && *l.IsPitOutLap,
	}
	if l.DateStart != nil {
		t.StartMs, t.HasStart = ms(epoch, *l.DateStart), true
	}
	return t
}

func round(v *float64) int64 { return int64(math.Round(*v)) }

func convertCar(epoch time.Time, rows []openf1.CarData) []laps.Car {
	out := make([]laps.Car, 0, len(rows))
	for _, r := range rows {
		// A row without a timestamp or a core reading cannot be placed or plotted.
		if r.Date == nil || r.Speed == nil || r.RPM == nil || r.Gear == nil {
			continue
		}
		c := laps.Car{T: ms(epoch, *r.Date), Speed: round(r.Speed), RPM: round(r.RPM), Gear: round(r.Gear), Throttle: laps.Missing, Brake: laps.Missing}
		if r.Throttle != nil {
			c.Throttle = round(r.Throttle)
		}
		if r.Brake != nil {
			c.Brake = round(r.Brake)
		}
		if r.DRS != nil {
			c.DRS = round(r.DRS)
		}
		out = append(out, c)
	}
	return out
}

func convertLoc(epoch time.Time, rows []openf1.Location) []laps.Loc {
	out := make([]laps.Loc, 0, len(rows))
	for _, r := range rows {
		if r.Date == nil || r.X == nil || r.Y == nil || r.Z == nil {
			continue
		}
		out = append(out, laps.Loc{T: ms(epoch, *r.Date), X: round(r.X), Y: round(r.Y), Z: round(r.Z)})
	}
	return out
}
