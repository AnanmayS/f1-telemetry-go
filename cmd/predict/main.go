// Command predict builds and evaluates the race prediction model.
//
//	predict fetch     download race metadata and write data/races.json.gz
//	predict backtest  walk forward over past races and print accuracy (Markdown)
//	predict picks     list favourite, pole sitter and winner per race (Markdown)
//	predict build     write data/predictions.json.gz for the server
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dataDir := fs.String("data", "data", "data directory")
	offline := fs.Bool("offline", false, "fetch: never touch the network")
	runs := fs.Int("runs", 2000, "simulated races per prediction during tuning and scoring")
	fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	var err error
	switch cmd {
	case "fetch":
		err = fetch(ctx, log, *dataDir, *offline)
	case "backtest":
		err = backtest(*dataDir, *runs)
	case "picks":
		err = picks(*dataDir, *runs)
	case "build":
		err = build(*dataDir, *runs)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "predict:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: predict fetch|backtest|picks|build [-data dir]")
	os.Exit(2)
}

func fetch(ctx context.Context, log *slog.Logger, dataDir string, offline bool) error {
	c := openf1.New(filepath.Join(dataDir, "raw"), openf1.DefaultPerMinute, openf1.DefaultInFlight)
	c.Offline, c.Log = offline, log
	now := time.Now()
	var years []int
	for y := 2023; y <= now.Year(); y++ {
		years = append(years, y)
	}
	races, err := predict.Fetch(ctx, c, years, now, func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) })
	if err != nil {
		return err
	}
	if err := predict.SaveRaces(dataDir, races); err != nil {
		return err
	}
	log.Info("done", "races", len(races), "http_requests", c.Requests.Load())
	return nil
}
