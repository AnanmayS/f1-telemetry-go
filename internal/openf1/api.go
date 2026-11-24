package openf1

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Raw row types. Numeric fields are pointers because the feed uses null.

type Session struct {
	SessionKey  int       `json:"session_key"`
	MeetingKey  int       `json:"meeting_key"`
	SessionName string    `json:"session_name"`
	SessionType string    `json:"session_type"`
	DateStart   time.Time `json:"date_start"`
	DateEnd     time.Time `json:"date_end"`
	Circuit     string    `json:"circuit_short_name"`
	Country     string    `json:"country_name"`
	Year        int       `json:"year"`
	IsCancelled bool      `json:"is_cancelled"`
}

type Meeting struct {
	MeetingKey  int    `json:"meeting_key"`
	MeetingName string `json:"meeting_name"`
}

type Driver struct {
	DriverNumber int     `json:"driver_number"`
	NameAcronym  string  `json:"name_acronym"`
	FullName     string  `json:"full_name"`
	TeamName     *string `json:"team_name"`
	TeamColour   *string `json:"team_colour"`
}

type Lap struct {
	DriverNumber int        `json:"driver_number"`
	LapNumber    int        `json:"lap_number"`
	DateStart    *time.Time `json:"date_start"`
	LapDuration  *float64   `json:"lap_duration"`
	Sector1      *float64   `json:"duration_sector_1"`
	Sector2      *float64   `json:"duration_sector_2"`
	Sector3      *float64   `json:"duration_sector_3"`
	IsPitOutLap  *bool      `json:"is_pit_out_lap"`
}

type CarData struct {
	Date     *time.Time `json:"date"`
	Speed    *float64   `json:"speed"`
	RPM      *float64   `json:"rpm"`
	Gear     *float64   `json:"n_gear"`
	Throttle *float64   `json:"throttle"`
	Brake    *float64   `json:"brake"`
	DRS      *float64   `json:"drs"`
}

type Location struct {
	Date *time.Time `json:"date"`
	X    *float64   `json:"x"`
	Y    *float64   `json:"y"`
	Z    *float64   `json:"z"`
}

// listMaxAge is how long season listings are trusted; new sessions appear
// during a season, unlike per-session data which never changes.
const listMaxAge = 6 * time.Hour

func fetch[T any](ctx context.Context, c *Client, cachePath, query string, maxAge time.Duration) ([]T, int, error) {
	body, err := c.Fetch(ctx, cachePath, query, maxAge)
	if err != nil {
		return nil, 0, err
	}
	var rows []T
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, 0, fmt.Errorf("openf1: decoding %s: %w", cachePath, err)
	}
	return rows, len(body), nil
}

// Each accessor returns the rows and the size of the raw JSON in bytes.

func (c *Client) Sessions(ctx context.Context, year int) ([]Session, int, error) {
	return fetch[Session](ctx, c, fmt.Sprintf("_meta/sessions_%d.json.gz", year), fmt.Sprintf("sessions?year=%d", year), listMaxAge)
}

func (c *Client) Meetings(ctx context.Context, year int) ([]Meeting, int, error) {
	return fetch[Meeting](ctx, c, fmt.Sprintf("_meta/meetings_%d.json.gz", year), fmt.Sprintf("meetings?year=%d", year), listMaxAge)
}

func (c *Client) Drivers(ctx context.Context, session int) ([]Driver, int, error) {
	return fetch[Driver](ctx, c, fmt.Sprintf("%d/drivers_all.json.gz", session), fmt.Sprintf("drivers?session_key=%d", session), 0)
}

// Laps fetches every driver's laps for a session in one request.
func (c *Client) Laps(ctx context.Context, session int) ([]Lap, int, error) {
	return fetch[Lap](ctx, c, fmt.Sprintf("%d/laps_all.json.gz", session), fmt.Sprintf("laps?session_key=%d", session), 0)
}

// Window is the time range a session's telemetry is split over when one
// response would be too large. It is generous on both sides: sessions start
// late, get red-flagged and overrun.
type Window struct{ From, To time.Time }

// SessionWindow covers a session from an hour before its scheduled start to
// four hours after its scheduled end.
func SessionWindow(start, end time.Time) Window {
	return Window{start.Add(-time.Hour), end.Add(4 * time.Hour)}
}

func fetchWindowed[T any](ctx context.Context, c *Client, cachePath, query string, w Window) ([]T, int, error) {
	body, err := c.FetchWindowed(ctx, cachePath, query, w.From, w.To)
	if err != nil {
		return nil, 0, err
	}
	var rows []T
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, 0, fmt.Errorf("openf1: decoding %s: %w", cachePath, err)
	}
	return rows, len(body), nil
}

func (c *Client) CarData(ctx context.Context, session, driver int, w Window) ([]CarData, int, error) {
	return fetchWindowed[CarData](ctx, c, fmt.Sprintf("%d/car_data_%d.json.gz", session, driver), fmt.Sprintf("car_data?session_key=%d&driver_number=%d", session, driver), w)
}

func (c *Client) Location(ctx context.Context, session, driver int, w Window) ([]Location, int, error) {
	return fetchWindowed[Location](ctx, c, fmt.Sprintf("%d/location_%d.json.gz", session, driver), fmt.Sprintf("location?session_key=%d&driver_number=%d", session, driver), w)
}

// Race metadata, used by the prediction model. One request per session each.

type Stint struct {
	DriverNumber int    `json:"driver_number"`
	StintNumber  int    `json:"stint_number"`
	LapStart     *int   `json:"lap_start"`
	LapEnd       *int   `json:"lap_end"`
	Compound     string `json:"compound"`
	TyreAge      *int   `json:"tyre_age_at_start"`
}

type RaceControl struct {
	Date      *time.Time `json:"date"`
	LapNumber *int       `json:"lap_number"`
	Category  string     `json:"category"`
	Flag      *string    `json:"flag"`
	Message   string     `json:"message"`
}

// Result is one row of session_result. In a race Duration is a number; in
// qualifying it is an array with one entry per segment, hence RawMessage.
type Result struct {
	DriverNumber int             `json:"driver_number"`
	Position     Pos             `json:"position"`
	Laps         *int            `json:"number_of_laps"`
	Points       *float64        `json:"points"`
	DNF          bool            `json:"dnf"`
	DNS          bool            `json:"dns"`
	DSQ          bool            `json:"dsq"`
	Duration     json.RawMessage `json:"duration"`
}

// Segments returns the time of each qualifying segment in seconds (0 where
// the driver set none), or nil for a race result.
func (r Result) Segments() []float64 {
	var many []*float64
	if json.Unmarshal(r.Duration, &many) != nil {
		return nil
	}
	out := make([]float64, len(many))
	for i, v := range many {
		if v != nil && *v > 0 {
			out[i] = *v
		}
	}
	return out
}

// Pos is a classified position. The feed sends a number, null, or a string
// such as "NC" or "DQ" for a driver without one; anything but a number is 0.
type Pos int

func (p *Pos) UnmarshalJSON(b []byte) error {
	var n int
	if json.Unmarshal(b, &n) == nil {
		*p = Pos(n)
	} else {
		*p = 0
	}
	return nil
}

// BestSeconds is the race duration, or the best qualifying segment time.
// It returns 0 when there is none.
func (r Result) BestSeconds() float64 {
	var one float64
	if json.Unmarshal(r.Duration, &one) == nil {
		return one
	}
	var many []*float64
	best := 0.0
	if json.Unmarshal(r.Duration, &many) == nil {
		for _, v := range many {
			if v != nil && *v > 0 && (best == 0 || *v < best) {
				best = *v
			}
		}
	}
	return best
}

type Position struct {
	Date         *time.Time `json:"date"`
	DriverNumber int        `json:"driver_number"`
	Position     int        `json:"position"`
}

func (c *Client) Stints(ctx context.Context, session int) ([]Stint, int, error) {
	return fetch[Stint](ctx, c, fmt.Sprintf("%d/stints_all.json.gz", session), fmt.Sprintf("stints?session_key=%d", session), 0)
}

func (c *Client) RaceControl(ctx context.Context, session int) ([]RaceControl, int, error) {
	return fetch[RaceControl](ctx, c, fmt.Sprintf("%d/race_control_all.json.gz", session), fmt.Sprintf("race_control?session_key=%d", session), 0)
}

func (c *Client) Results(ctx context.Context, session int) ([]Result, int, error) {
	return fetch[Result](ctx, c, fmt.Sprintf("%d/session_result_all.json.gz", session), fmt.Sprintf("session_result?session_key=%d", session), 0)
}

func (c *Client) Positions(ctx context.Context, session int) ([]Position, int, error) {
	return fetch[Position](ctx, c, fmt.Sprintf("%d/position_all.json.gz", session), fmt.Sprintf("position?session_key=%d", session), 0)
}
