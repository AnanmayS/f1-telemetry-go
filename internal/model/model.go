// Package model holds the domain types shared by ingest, storage and the API.
package model

import (
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/encoding"
)

// Channel identifies one stored series of a lap.
type Channel uint8

const (
	ChT        Channel = iota // ms since session epoch
	ChSpeed                   // km/h
	ChRPM                     //
	ChGear                    // 0 = neutral
	ChThrottle                // percent
	ChBrake                   // 0 or 1
	ChDRS                     // raw feed code
	ChX                       // track position, feed units
	ChY                       //
	ChZ                       //
	ChDistance                // centimetres from the lap start
	NumChannels
)

var channelNames = [NumChannels]string{"t", "speed", "rpm", "gear", "throttle", "brake", "drs", "x", "y", "z", "distance"}

func (c Channel) String() string {
	if c < NumChannels {
		return channelNames[c]
	}
	return "unknown"
}

// ParseChannel maps an API name to its channel.
func ParseChannel(name string) (Channel, bool) {
	for i, n := range channelNames {
		if n == name {
			return Channel(i), true
		}
	}
	return 0, false
}

// Kind reports which codec family suits the channel.
func (c Channel) Kind() encoding.Kind {
	switch c {
	case ChGear, ChDRS:
		return encoding.KindStep
	case ChBrake:
		return encoding.KindBool
	}
	return encoding.KindSmooth
}

// Step reports whether the channel is resampled with step (previous-value)
// interpolation instead of linear interpolation.
func (c Channel) Step() bool { return c.Kind() != encoding.KindSmooth }

// DRSOpen derives "flap open" from the raw feed code (see docs/DATA.md).
func DRSOpen(code int64) bool { return code == 10 || code == 12 || code == 14 }

// Session describes one track session.
type Session struct {
	Key         int       `json:"session_key"`
	MeetingKey  int       `json:"meeting_key"`
	Year        int       `json:"year"`
	MeetingName string    `json:"meeting_name"`
	Name        string    `json:"session_name"`
	Type        string    `json:"session_type"`
	Circuit     string    `json:"circuit"`
	Country     string    `json:"country"`
	Start       time.Time `json:"date_start"`
	End         time.Time `json:"date_end"`
}

// Driver is a driver as entered in one session.
type Driver struct {
	Number  int    `json:"number"`
	Acronym string `json:"acronym"`
	Name    string `json:"name"`
	Team    string `json:"team"`
	Colour  string `json:"colour"` // RRGGBB, no leading '#'
}

// Lap flags.
const (
	FlagPitOut     uint8 = 1 << iota // lap started in the pit lane
	FlagPitIn                        // lap ended in the pit lane (next lap is a pit-out lap)
	FlagValidTime                    // OfficialMs is an official, line-to-line lap time
	FlagDistanceOK                   // distance channel is normalised to the circuit length
)

// NoTime marks a missing duration.
const NoTime int32 = -1

// Lap is one row of a segment's lap table. Times are ms; StartMs and EndMs
// are relative to the session epoch.
type Lap struct {
	Driver     int
	Number     int
	Flags      uint8
	StartMs    int64
	EndMs      int64
	OfficialMs int32 // NoTime when the feed has no lap time
	SectorMs   [3]int32
	Samples    int
}

// ValidTime reports whether the lap has an official lap time.
func (l Lap) ValidTime() bool { return l.Flags&FlagValidTime != 0 }

// DistanceOK reports whether the lap's distance channel reaches exactly the
// circuit length at StartMs+OfficialMs, which is what makes a comparison's
// final delta equal the official lap-time difference.
func (l Lap) DistanceOK() bool { return l.Flags&FlagDistanceOK != 0 }

// LapData is a lap's decoded samples. Valid[c] is nil when every value of
// channel c is present.
type LapData struct {
	Lap    Lap
	Values [NumChannels][]int64
	Valid  [NumChannels][]bool
}

// SizeBytes estimates the heap held by the decoded lap, for cache accounting.
func (d *LapData) SizeBytes() int {
	n := 256
	for c := range d.Values {
		n += 8*cap(d.Values[c]) + cap(d.Valid[c])
	}
	return n
}
