// Package live is the live-ingest path: events are appended to a write-ahead
// log, applied to an in-memory table, exposed to queries as a periodically
// rebuilt in-memory segment, and sealed into a normal segment file when the
// session ends.
package live

import (
	"encoding/binary"
	"errors"

	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
)

// Kind is the type of a live event.
type Kind uint8

const (
	KindCar Kind = 1 // one car_data sample
	KindLoc Kind = 2 // one location sample
	KindLap Kind = 3 // a lap-timing row; later rows for the same lap replace earlier ones
)

// Event is one unit of the live feed. Seq is assigned by the source and
// strictly increases; it makes Apply idempotent, so a source that resends
// after a crash cannot create duplicates.
type Event struct {
	Seq    uint64
	Kind   Kind
	Driver int
	Car    laps.Car
	Loc    laps.Loc
	Lap    laps.Timing
}

var errBadEvent = errors.New("live: malformed event")

// recEvent and recMeta tag WAL records.
const (
	recMeta  byte = 0
	recEvent byte = 1
)

func (e Event) append(b []byte) []byte {
	b = append(b, recEvent, byte(e.Kind))
	b = binary.AppendUvarint(b, e.Seq)
	b = binary.AppendUvarint(b, uint64(e.Driver))
	switch e.Kind {
	case KindCar:
		for _, v := range []int64{e.Car.T, e.Car.Speed, e.Car.RPM, e.Car.Gear, e.Car.Throttle, e.Car.Brake, e.Car.DRS} {
			b = binary.AppendVarint(b, v)
		}
	case KindLoc:
		for _, v := range []int64{e.Loc.T, e.Loc.X, e.Loc.Y, e.Loc.Z} {
			b = binary.AppendVarint(b, v)
		}
	case KindLap:
		flags := int64(0)
		if e.Lap.HasStart {
			flags |= 1
		}
		if e.Lap.PitOut {
			flags |= 2
		}
		for _, v := range []int64{int64(e.Lap.Number), e.Lap.StartMs, flags, int64(e.Lap.DurationMs),
			int64(e.Lap.SectorMs[0]), int64(e.Lap.SectorMs[1]), int64(e.Lap.SectorMs[2])} {
			b = binary.AppendVarint(b, v)
		}
	}
	return b
}

func decodeEvent(b []byte) (Event, error) {
	var e Event
	if len(b) < 2 || b[0] != recEvent {
		return e, errBadEvent
	}
	e.Kind = Kind(b[1])
	b = b[2:]
	u := func() uint64 {
		v, n := binary.Uvarint(b)
		if n <= 0 {
			b = nil
			return 0
		}
		b = b[n:]
		return v
	}
	i := func() int64 {
		v, n := binary.Varint(b)
		if n <= 0 {
			b = nil
			return 0
		}
		b = b[n:]
		return v
	}
	e.Seq = u()
	e.Driver = int(u())
	switch e.Kind {
	case KindCar:
		e.Car = laps.Car{T: i(), Speed: i(), RPM: i(), Gear: i(), Throttle: i(), Brake: i(), DRS: i()}
	case KindLoc:
		e.Loc = laps.Loc{T: i(), X: i(), Y: i(), Z: i()}
	case KindLap:
		e.Lap.Number, e.Lap.StartMs = int(i()), i()
		flags := i()
		e.Lap.HasStart, e.Lap.PitOut = flags&1 != 0, flags&2 != 0
		e.Lap.DurationMs = int32(i())
		e.Lap.SectorMs = [3]int32{int32(i()), int32(i()), int32(i())}
	default:
		return e, errBadEvent
	}
	if b == nil || len(b) != 0 || e.Driver > 255 {
		return e, errBadEvent
	}
	return e, nil
}
