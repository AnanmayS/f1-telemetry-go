// Command ingest discovers sessions, fetches them from OpenF1 (politely, via
// the raw cache) and builds segments. It is resumable and idempotent: rerun
// it after Ctrl-C or a failure and it continues where it stopped.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
)

func main() {
	var (
		years     = flag.String("year", "", `seasons to ingest: "2024", "2023,2024" or "all"`)
		sessions  = flag.String("session", "", "only these session keys (comma-separated); needs -year to locate them")
		dataDir   = flag.String("data", "data", "data directory (raw cache, segments, MANIFEST)")
		perMinute = flag.Float64("per-minute", openf1.DefaultPerMinute, "max API requests per minute (OpenF1 free limit: 30)")
		inFlight  = flag.Int("in-flight", openf1.DefaultInFlight, "max concurrent API requests")
		workers   = flag.Int("workers", 4, "concurrent per-driver build jobs")
		force     = flag.Bool("force", false, "rebuild sessions already in the manifest (uses the raw cache)")
		offline   = flag.Bool("offline", false, "never touch the network; fail on a cache miss")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	yearList, err := parseYears(*years)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ingest:", err)
		flag.Usage()
		os.Exit(2)
	}
	only := map[int]bool{}
	for _, f := range strings.Split(*sessions, ",") {
		if f = strings.TrimSpace(f); f != "" {
			k, err := strconv.Atoi(f)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ingest: bad -session value:", f)
				os.Exit(2)
			}
			only[k] = true
		}
	}

	client := openf1.New(filepath.Join(*dataDir, "raw"), *perMinute, *inFlight)
	client.Offline = *offline
	client.Log = log
	in := &ingest.Ingester{Client: client, DataDir: *dataDir, Workers: *workers, Force: *force, Log: log}

	start := time.Now()
	var built, unchanged, skipped, failed int
	var samples, rawBytes, segBytes int64
	for _, year := range yearList {
		list, err := in.Discover(ctx, year)
		if err != nil {
			exit(log, err)
		}
		var todo []model.Session
		for _, s := range list {
			if len(only) == 0 || only[s.Key] {
				todo = append(todo, s)
			}
		}
		log.Info("season", "year", year, "sessions", len(todo))
		for i, s := range todo {
			res, err := in.Session(ctx, s)
			switch {
			case errors.Is(err, context.Canceled):
				log.Info("interrupted; rerun to resume")
				os.Exit(130)
			case errors.Is(err, openf1.ErrRestricted):
				exit(log, err)
			case err != nil:
				// One bad session must not stop the corpus; it is retried on the next run.
				failed++
				log.Error("session failed", "session", s.Key, "name", s.MeetingName+" "+s.Name, "err", err)
				continue
			case res.Unchanged:
				unchanged++
				continue
			case res.Skipped != "":
				skipped++
				log.Info("skipped", "session", s.Key, "name", s.MeetingName+" "+s.Name, "reason", res.Skipped)
				continue
			}
			built++
			samples += res.Samples
			rawBytes += res.RawJSONBytes
			segBytes += res.SegmentBytes
			log.Info("built", "n", fmt.Sprintf("%d/%d", i+1, len(todo)), "session", s.Key, "name", s.MeetingName+" "+s.Name,
				"laps", res.Laps, "samples", res.Samples, "raw_mb", mb(res.RawJSONBytes), "seg_mb", mb(res.SegmentBytes),
				"ratio", fmt.Sprintf("%.1fx", float64(res.RawJSONBytes)/float64(res.SegmentBytes)),
				"took", res.Elapsed.Round(time.Millisecond), "http", client.Requests.Load())
		}
	}
	log.Info("done", "built", built, "unchanged", unchanged, "skipped", skipped, "failed", failed,
		"samples", samples, "raw_mb", mb(rawBytes), "seg_mb", mb(segBytes),
		"http_requests", client.Requests.Load(), "cache_hits", client.CacheHits.Load(), "took", time.Since(start).Round(time.Second))
	if failed > 0 {
		os.Exit(1)
	}
}

func mb(b int64) string { return fmt.Sprintf("%.2f", float64(b)/1e6) }

func exit(log *slog.Logger, err error) {
	if errors.Is(err, context.Canceled) {
		log.Info("interrupted; rerun to resume")
		os.Exit(130)
	}
	log.Error("ingest stopped", "err", err)
	os.Exit(1)
}

func parseYears(s string) ([]int, error) {
	if s == "" {
		return nil, errors.New("-year is required")
	}
	if s == "all" {
		var out []int
		for y := ingest.FirstSeason; y <= time.Now().Year(); y++ {
			out = append(out, y)
		}
		return out, nil
	}
	var out []int
	for _, f := range strings.Split(s, ",") {
		y, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || y < ingest.FirstSeason {
			return nil, fmt.Errorf("bad -year value %q (first season is %d)", f, ingest.FirstSeason)
		}
		out = append(out, y)
	}
	return out, nil
}
