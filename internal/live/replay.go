package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

// Events turns a recorded session into the time-ordered stream a live feed
// would deliver: every telemetry sample at its own timestamp, a lap row when
// the lap starts (start time only) and again when it completes (with its
// time and sectors). Sequence numbers are the position in that stream, so
// the same recording always yields the same numbering.
func Events(raw *ingest.Raw) []Event {
	type timed struct {
		at int64
		ev Event
	}
	var all []timed
	for d, rows := range raw.Car {
		for _, r := range rows {
			all = append(all, timed{r.T, Event{Kind: KindCar, Driver: d, Car: r}})
		}
	}
	for d, rows := range raw.Loc {
		for _, r := range rows {
			all = append(all, timed{r.T, Event{Kind: KindLoc, Driver: d, Loc: r}})
		}
	}
	none := [3]int32{model.NoTime, model.NoTime, model.NoTime}
	for d, rows := range raw.Timing {
		rows = append([]laps.Timing(nil), rows...)
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Number < rows[j].Number })
		var prevDone int64
		for i, t := range rows {
			if i > 0 && rows[i-1].Number == t.Number {
				continue // batch ingest keeps the first row of a duplicated lap
			}
			if t.HasStart {
				all = append(all, timed{t.StartMs, Event{Kind: KindLap, Driver: d,
					Lap: laps.Timing{Number: t.Number, StartMs: t.StartMs, HasStart: true, DurationMs: model.NoTime, SectorMs: none, PitOut: t.PitOut}}})
			}
			// The complete row is known when the car next crosses the line.
			done := prevDone
			switch {
			case t.HasStart && t.DurationMs > 0 && !t.PitOut:
				done = t.StartMs + int64(t.DurationMs)
			case i+1 < len(rows) && rows[i+1].HasStart:
				done = rows[i+1].StartMs
			case t.HasStart:
				done = t.StartMs
			}
			done = max(done, prevDone)
			prevDone = done
			all = append(all, timed{done, Event{Kind: KindLap, Driver: d, Lap: t}})
		}
	}
	// Ties are broken by driver and kind so the order never depends on map iteration.
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.at != b.at {
			return a.at < b.at
		}
		if a.ev.Driver != b.ev.Driver {
			return a.ev.Driver < b.ev.Driver
		}
		if a.ev.Kind != b.ev.Kind {
			return a.ev.Kind < b.ev.Kind
		}
		return a.ev.Lap.Number < b.ev.Lap.Number
	})
	out := make([]Event, len(all))
	for i, t := range all {
		t.ev.Seq = uint64(i + 1)
		out[i] = t.ev
	}
	return out
}

// eventTime is when the feed would deliver ev (ms since the session epoch).
func eventTime(ev Event, fallback int64) int64 {
	switch ev.Kind {
	case KindCar:
		return ev.Car.T
	case KindLoc:
		return ev.Loc.T
	}
	return fallback // lap rows ride along with the telemetry around them
}

// Runner drives one live session from a source of events to a sealed segment.
type Runner struct {
	Store     *store.Store  // optional: publish live snapshots for queries
	DataDir   string        // where the sealed segment and manifest go
	LiveDir   string        // where WALs live
	SyncEvery time.Duration // WAL group-commit interval
	Refresh   time.Duration // how often queries see new data (default 1s)
	Log       *slog.Logger

	// OnRecovered is called once with the last sequence found in the WAL.
	OnRecovered func(lastSeq uint64)
	// OnDurable is called as the durable sequence advances. Only events at or
	// below it may be acknowledged to the source.
	OnDurable func(seq uint64)
}

// Replay feeds a recorded session through the live path at speed times real
// time (speed <= 0: as fast as possible), resuming after the last event in
// the WAL, then seals it. If ctx is cancelled the WAL is kept for the next run.
func (r *Runner) Replay(ctx context.Context, raw *ingest.Raw, speed float64) error {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	key := raw.Meta.Session.Key
	if man, err := store.LoadManifest(r.DataDir); err != nil {
		return err
	} else if _, sealed := man.Find(key); sealed {
		// A crash after sealing but before the WAL was removed: the segment is
		// complete, the log is redundant.
		if err := os.Remove(WALPath(r.LiveDir, key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if r.OnRecovered != nil {
			r.OnRecovered(0)
		}
		return nil
	}
	sess, err := Open(r.LiveDir, raw.Meta, r.SyncEvery)
	if err != nil {
		return err
	}
	events := Events(raw)
	from := sess.LastSeq()
	if r.OnRecovered != nil {
		r.OnRecovered(from)
	}
	log.Info("live session open", "session", key, "events", len(events), "resume_after_seq", from)

	// Publish snapshots and report durability in the background.
	bgCtx, stopBG := context.WithCancel(ctx)
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		refresh := r.Refresh
		if refresh <= 0 {
			refresh = time.Second
		}
		ack := time.NewTicker(10 * time.Millisecond)
		pub := time.NewTicker(refresh)
		defer ack.Stop()
		defer pub.Stop()
		var acked, published uint64
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-ack.C:
				if d := sess.DurableSeq(); d > acked && r.OnDurable != nil {
					acked = d
					r.OnDurable(d)
				}
			case <-pub.C:
				if r.Store == nil || sess.Version() == published {
					continue
				}
				seg, version, err := sess.Snapshot()
				if err != nil {
					log.Error("live snapshot failed", "session", key, "err", err)
					continue
				}
				published = version
				if seg != nil {
					r.Store.SetLive(seg, fmt.Sprintf("live-%d-%d", key, version))
				}
			}
		}
	}()
	stop := func() {
		stopBG()
		<-bgDone
	}

	var wallStart time.Time
	var feedStart int64
	last := int64(0)
	for _, ev := range events {
		if ev.Seq <= from {
			last = eventTime(ev, last)
			continue
		}
		at := eventTime(ev, last)
		last = at
		if speed > 0 {
			if wallStart.IsZero() {
				wallStart, feedStart = time.Now(), at
			}
			due := wallStart.Add(time.Duration(float64(at-feedStart)/speed) * time.Millisecond)
			if d := time.Until(due); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
				}
			}
		}
		if ctx.Err() != nil {
			stop()
			sess.Close()
			return ctx.Err()
		}
		if _, err := sess.Apply(ev); err != nil {
			stop()
			sess.Close()
			return err
		}
	}
	if err := sess.Sync(); err != nil {
		stop()
		return err
	}
	if r.OnDurable != nil {
		r.OnDurable(sess.DurableSeq())
	}
	stop()
	entry, err := sess.Seal(r.DataDir)
	if err != nil {
		return err
	}
	log.Info("live session sealed", "session", key, "file", entry.File, "samples", entry.Samples)
	if r.Store != nil {
		// Serve the sealed file, then drop the in-memory segment.
		if _, err := r.Store.Reload(); err != nil {
			return err
		}
		r.Store.ClearLive(key)
	}
	return nil
}
