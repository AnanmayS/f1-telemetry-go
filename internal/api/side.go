package api

import "github.com/AnanmayS/f1-telemetry-go/internal/model"

type sideJSON struct {
	SessionKey int          `json:"session_key"`
	Session    sessionJSON  `json:"session"`
	Driver     model.Driver `json:"driver"`
	Lap        lapJSON      `json:"lap"`
}

// appendSide describes one lap of a comparison.
func appendSide(b []byte, s side) []byte {
	fastest := -1
	if s.row.ValidTime() {
		// is_fastest is filled in by the caller's lap table when needed; here
		// it reflects whether this lap is the driver's best.
		best := s.row
		for _, l := range s.sess.Laps(s.row.Driver) {
			if l.ValidTime() && l.Samples > 0 && l.OfficialMs < best.OfficialMs {
				best = l
			}
		}
		fastest = best.Number
	}
	return appendObject(b, sideJSON{
		SessionKey: s.sess.SessionKey,
		Session:    sessionJSON{Session: s.sess.Meta.Session, CircuitLengthM: s.sess.Meta.CircuitLengthM},
		Driver:     s.driver,
		Lap:        lapToJSON(s.row, fastest),
	})
}
