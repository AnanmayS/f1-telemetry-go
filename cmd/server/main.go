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
	"syscall"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/api"
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
	go func() {
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
}
