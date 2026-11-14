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

func (c *Client) CarData(ctx context.Context, session, driver int) ([]CarData, int, error) {
	return fetch[CarData](ctx, c, fmt.Sprintf("%d/car_data_%d.json.gz", session, driver), fmt.Sprintf("car_data?session_key=%d&driver_number=%d", session, driver), 0)
}

func (c *Client) Location(ctx context.Context, session, driver int) ([]Location, int, error) {
	return fetch[Location](ctx, c, fmt.Sprintf("%d/location_%d.json.gz", session, driver), fmt.Sprintf("location?session_key=%d&driver_number=%d", session, driver), 0)
}
