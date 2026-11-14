// Command replay feeds a recorded session through the live-ingest path
// (WAL, memtable, seal) at a multiple of real time. It is the stand-in for a
// live feed, which is not freely available (see docs/DATA.md), and the
// process the crash test kills.
//
// It prints one line per state change on stdout:
//
//	RECOVERED <seq>   last event found in the WAL at startup
//	ACK <seq>         every event up to seq is on disk
//	SEALED            the session was written as a segment
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
	"syscall"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/live"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
)

func main() {
	var (
		dataDir = flag.String("data", "data", "data directory (raw cache in, segment out)")
		liveDir = flag.String("live-dir", "", "WAL directory (default <data>/live)")
		year    = flag.Int("year", 0, "season of the session")
		session = flag.Int("session", 0, "session key to replay; it must be in the raw cache")
		speed   = flag.Float64("speed", 10, "replay speed as a multiple of real time (0 = as fast as possible)")
		sync    = flag.Duration("sync", 100*time.Millisecond, "WAL fsync interval (0 = fsync every event)")
	)
	flag.Parse()
	if *year == 0 || *session == 0 {
		fmt.Fprintln(os.Stderr, "replay: -year and -session are required")
		os.Exit(2)
	}
	if *liveDir == "" {
		*liveDir = filepath.Join(*dataDir, "live")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := openf1.New(filepath.Join(*dataDir, "raw"), 0, 0)
	client.Offline = true
	client.Log = log
	in := &ingest.Ingester{Client: client, DataDir: *dataDir, Workers: 4, Log: log}
	sessions, err := in.Discover(ctx, *year)
	if err != nil {
		fatal(err)
	}
	var raw *ingest.Raw
	for _, s := range sessions {
		if s.Key == *session {
			if raw, err = in.Fetch(ctx, s); err != nil {
				fatal(err)
			}
		}
	}
	if raw == nil {
		fatal(fmt.Errorf("session %d not found in season %d", *session, *year))
	}
	r := &live.Runner{
		DataDir: *dataDir, LiveDir: *liveDir, SyncEvery: *sync, Log: log,
		OnRecovered: func(seq uint64) { fmt.Println("RECOVERED", seq) },
		OnDurable:   func(seq uint64) { fmt.Println("ACK", seq) },
	}
	if err := r.Replay(ctx, raw, *speed); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fatal(err)
	}
	fmt.Println("SEALED")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "replay:", err)
	os.Exit(1)
}
