// Command server serves the ghostlap HTTP API and the built web app.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/api"
	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/live"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

func main() {
	var (
		addr       = flag.String("addr", ":8080", "listen address")
		dataDir    = flag.String("data", "data", "data directory (segments, MANIFEST)")
		static     = flag.String("static", "web/dist", "built frontend directory (empty to disable)")
		cacheMB    = flag.Int64("cache-mb", 256, "decoded-lap cache size in MiB")
		respMB     = flag.Int64("resp-cache-mb", 64, "finished-response cache size in MiB (0 disables)")
		reload     = flag.Duration("reload", 30*time.Second, "how often to check the manifest for new segments")
		rateRPS    = flag.Float64("rate", 20, "per-IP API requests per second (0 disables the limit)")
		rateBurst  = flag.Int("burst", 120, "per-IP burst")
		replayKey  = flag.Int("replay-session", 0, "replay this recorded session through the live path while serving (needs -replay-year)")
		replayYear = flag.Int("replay-year", 0, "season of -replay-session")
		replaySpd  = flag.Float64("replay-speed", 10, "replay speed as a multiple of real time")
		trustProxy = flag.Bool("trust-proxy", false, "read the client address from CF-Connecting-IP (only behind Cloudflare)")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	st, err := store.Open(*dataDir, *cacheMB<<20, log)
	if err != nil {
		log.Error("cannot open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if *static != "" {
		if _, err := os.Stat(*static); err != nil {
			log.Warn("static directory not found; serving the API only", "dir", *static)
			*static = ""
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go st.Watch(ctx, *reload)

	respBytes := *respMB << 20
	if respBytes == 0 {
		respBytes = -1
	}
	if *replayKey != 0 {
		go replay(ctx, log, st, *dataDir, *replayYear, *replayKey, *replaySpd)
	}

	app := api.New(api.Options{Store: st, Log: log, StaticDir: *static, RateRPS: *rateRPS, RateBurst: *rateBurst, TrustProxy: *trustProxy, RespCacheBytes: respBytes})
	go app.Run(ctx)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", *addr, "segments", st.Stats().Segments, "static", *static)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
	<-drained // ListenAndServe returns as soon as Shutdown starts; wait for in-flight requests
}

// replay feeds a recorded session from the raw cache through the live path:
// WAL, memtable, snapshots visible to queries, then a sealed segment. It is
// how the live path is exercised without a (paid) live feed.
func replay(ctx context.Context, log *slog.Logger, st *store.Store, dataDir string, year, key int, speed float64) {
	client := openf1.New(filepath.Join(dataDir, "raw"), 0, 0)
	client.Offline, client.Log = true, log
	in := &ingest.Ingester{Client: client, DataDir: dataDir, Workers: 4, Log: log}
	sessions, err := in.Discover(ctx, year)
	if err != nil {
		log.Error("replay: cannot list sessions", "err", err)
		return
	}
	for _, s := range sessions {
		if s.Key != key {
			continue
		}
		raw, err := in.Fetch(ctx, s)
		if err != nil {
			log.Error("replay: cannot load session", "session", key, "err", err)
			return
		}
		r := &live.Runner{Store: st, DataDir: dataDir, LiveDir: filepath.Join(dataDir, "live"), SyncEvery: 100 * time.Millisecond, Refresh: time.Second, Log: log}
		if err := r.Replay(ctx, raw, speed); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("replay failed", "session", key, "err", err)
		}
		return
	}
	log.Error("replay: session not found in the raw cache", "session", key, "year", year)
}
