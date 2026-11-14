// Command gentargets writes a vegeta targets file for /api/compare with a
// realistic mix: most requests hit a small set of popular comparisons
// (fastest laps of recent sessions), the rest compare random laps from
// anywhere in the corpus. It discovers sessions and laps through the API.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
)

type session struct {
	Key int `json:"session_key"`
}

type driver struct {
	Number    int  `json:"number"`
	FastestMs *int `json:"fastest_ms"`
}

type lap struct {
	Lap          int  `json:"lap"`
	HasTelemetry bool `json:"has_telemetry"`
}

func getJSON(url string, v any) {
	resp, err := http.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gentargets:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, "gentargets:", url, resp.Status)
		os.Exit(1)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		fmt.Fprintln(os.Stderr, "gentargets:", url, err)
		os.Exit(1)
	}
}

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "server base URL")
	n := flag.Int("n", 20000, "number of targets")
	popularShare := flag.Float64("popular", 0.8, "share of requests that hit popular comparisons")
	popularSessions := flag.Int("popular-sessions", 4, "most recent sessions that count as popular")
	randomSessions := flag.Int("random-sessions", 60, "sessions sampled for random laps")
	seed := flag.Int64("seed", 1, "random seed")
	flag.Parse()
	rng := rand.New(rand.NewSource(*seed))

	var sl struct{ Sessions []session }
	getJSON(*base+"/api/sessions", &sl)
	if len(sl.Sessions) == 0 {
		fmt.Fprintln(os.Stderr, "gentargets: no sessions")
		os.Exit(1)
	}

	drivers := func(key int) []driver {
		var d struct{ Drivers []driver }
		getJSON(fmt.Sprintf("%s/api/sessions/%d/drivers", *base, key), &d)
		var out []driver
		for _, x := range d.Drivers {
			if x.FastestMs != nil {
				out = append(out, x)
			}
		}
		sort.Slice(out, func(i, j int) bool { return *out[i].FastestMs < *out[j].FastestMs })
		return out
	}

	// Popular: fastest-lap comparisons among the top 5 drivers of the newest sessions.
	var popular []string
	for _, s := range sl.Sessions[:min(*popularSessions, len(sl.Sessions))] {
		top := drivers(s.Key)
		top = top[:min(5, len(top))]
		for i := range top {
			for j := range top {
				if i != j {
					popular = append(popular, fmt.Sprintf("a=%d:%d:fastest&b=%d:%d:fastest", s.Key, top[i].Number, s.Key, top[j].Number))
				}
			}
		}
	}

	// Random: any two laps with telemetry from the same session, sampled across the corpus.
	type ref struct{ session, driver, lap int }
	bySession := map[int][]ref{}
	var keys []int
	perm := rng.Perm(len(sl.Sessions))
	for _, i := range perm[:min(*randomSessions, len(perm))] {
		key := sl.Sessions[i].Key
		for _, d := range drivers(key) {
			var l struct{ Laps []lap }
			getJSON(fmt.Sprintf("%s/api/sessions/%d/laps?driver=%d", *base, key, d.Number), &l)
			for _, x := range l.Laps {
				if x.HasTelemetry {
					bySession[key] = append(bySession[key], ref{key, d.Number, x.Lap})
				}
			}
		}
		if len(bySession[key]) >= 2 {
			keys = append(keys, key)
		}
	}
	if len(popular) == 0 || len(keys) == 0 {
		fmt.Fprintln(os.Stderr, "gentargets: not enough data for a target mix")
		os.Exit(1)
	}

	nPopular := 0
	for i := 0; i < *n; i++ {
		var q string
		if rng.Float64() < *popularShare {
			q = popular[rng.Intn(len(popular))]
			nPopular++
		} else {
			laps := bySession[keys[rng.Intn(len(keys))]]
			a, b := laps[rng.Intn(len(laps))], laps[rng.Intn(len(laps))]
			q = fmt.Sprintf("a=%d:%d:%d&b=%d:%d:%d", a.session, a.driver, a.lap, b.session, b.driver, b.lap)
		}
		fmt.Printf("GET %s/api/compare?%s\nAccept-Encoding: gzip\n\n", *base, q)
	}
	nLaps := 0
	for _, k := range keys {
		nLaps += len(bySession[k])
	}
	fmt.Fprintf(os.Stderr, "gentargets: %d targets: %d popular (%d distinct comparisons), %d random (pool of %d laps in %d sessions)\n",
		*n, nPopular, len(popular), *n-nPopular, nLaps, len(keys))
}
