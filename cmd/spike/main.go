// Command spike is the milestone-0 data spike: it hits every OpenF1 endpoint
// the project depends on for one session, saves the raw samples, and prints
// the facts recorded in docs/DATA.md (field names, sampling rate, quirks).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const userAgent = "ghostlap-spike/0.1 (+https://github.com/AnanmayS/f1-telemetry-go)"

func main() {
	session := flag.Int("session", 9492, "session_key to probe")
	driver := flag.Int("driver", 1, "driver_number to probe")
	year := flag.Int("year", 2024, "season to list")
	out := flag.String("out", "data/spike", "directory for saved samples")
	flag.Parse()
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fatal(err)
	}
	base := "https://api.openf1.org/v1/"
	sd := fmt.Sprintf("session_key=%d&driver_number=%d", *session, *driver)
	endpoints := []struct{ name, query string }{
		{"sessions", fmt.Sprintf("sessions?year=%d", *year)},
		{"meetings", fmt.Sprintf("meetings?year=%d", *year)},
		{"drivers", fmt.Sprintf("drivers?session_key=%d", *session)},
		{"laps", "laps?" + sd},
		{"car_data", "car_data?" + sd},
		{"location", "location?" + sd},
	}
	rows := map[string][]map[string]any{}
	for _, e := range endpoints {
		start := time.Now()
		body, status, err := get(base + e.query)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, e.name+".json"), body, 0o644); err != nil {
			fatal(err)
		}
		var r []map[string]any
		if err := json.Unmarshal(body, &r); err != nil {
			fatal(fmt.Errorf("%s: %w", e.name, err))
		}
		rows[e.name] = r
		fmt.Printf("%-9s http=%d rows=%d bytes=%d took=%s\n", e.name, status, len(r), len(body), time.Since(start).Round(time.Millisecond))
		if len(r) > 0 {
			fmt.Printf("          fields=%v\n", keys(r[0]))
		}
		time.Sleep(time.Second) // politeness
	}
	for _, name := range []string{"car_data", "location"} {
		timing(name, rows[name])
	}
	for _, f := range []string{"throttle", "brake", "drs", "n_gear"} {
		fmt.Printf("car_data.%s distinct=%v\n", f, distinct(rows["car_data"], f))
	}
	nullDur := 0
	for _, l := range rows["laps"] {
		if l["lap_duration"] == nil {
			nullDur++
		}
	}
	fmt.Printf("laps: %d, null lap_duration: %d\n", len(rows["laps"]), nullDur)
	cancelled := 0
	for _, s := range rows["sessions"] {
		if s["is_cancelled"] == true {
			cancelled++
		}
	}
	fmt.Printf("sessions %d: %d, cancelled: %d\n", *year, len(rows["sessions"]), cancelled)

	// Sources the PRD lists as unavailable (section 4.4).
	for _, u := range []string{
		"https://api.openf1.org/v1/sessions?year=2022",
		"https://livetiming.formula1.com/static/2024/Index.json",
	} {
		_, status, err := get(u)
		fmt.Printf("probe %s -> http=%d err=%v\n", u, status, err)
		time.Sleep(time.Second)
	}
}

func get(url string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

func keys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func distinct(rows []map[string]any, field string) map[float64]int {
	out := map[float64]int{}
	for _, r := range rows {
		if v, ok := r[field].(float64); ok {
			out[v]++
		}
	}
	return out
}

func timing(name string, rows []map[string]any) {
	var ts []time.Time
	for _, r := range rows {
		s, _ := r["date"].(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			fmt.Printf("%s: unparseable date %q\n", name, s)
			continue
		}
		ts = append(ts, t)
	}
	if len(ts) < 2 {
		return
	}
	var dts []float64
	ooo, dup := 0, 0
	for i := 1; i < len(ts); i++ {
		d := ts[i].Sub(ts[i-1]).Seconds()
		switch {
		case d < 0:
			ooo++
		case d == 0:
			dup++
		}
		dts = append(dts, d)
	}
	sort.Float64s(dts)
	span := ts[len(ts)-1].Sub(ts[0]).Seconds()
	fmt.Printf("%s: %.2f Hz over %.0fs, dt median=%.3fs p10=%.3fs p90=%.3fs max=%.3fs, out-of-order=%d, duplicate-timestamps=%d\n",
		name, float64(len(ts))/span, span, dts[len(dts)/2], dts[len(dts)/10], dts[len(dts)*9/10], dts[len(dts)-1], ooo, dup)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "spike:", err)
	os.Exit(1)
}
