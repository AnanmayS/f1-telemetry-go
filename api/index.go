// Package handler is the Vercel Function that serves /api/*. Vercel builds
// every .go file in the top-level api directory into one function; the
// rewrite in vercel.json sends all API paths here.
//
// The function is the same HTTP layer cmd/server runs, over the segments
// bundled into the deployment (see deploy/vercel/publish.sh). Caches live
// for as long as the instance stays warm.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/AnanmayS/f1-telemetry-go/internal/api"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

var (
	once sync.Once
	app  http.Handler
)

// Handler is the function entry point.
func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(setup)
	// /metrics is per-instance and would be public here; it is not served.
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	app.ServeHTTP(w, r)
}

func setup() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	dir := os.Getenv("GHOSTLAP_DATA")
	if dir == "" {
		dir = "data" // bundled next to the function by includeFiles
	}
	st, err := store.Open(dir, 128<<20, log)
	if err != nil {
		log.Error("cannot open store", "dir", dir, "err", err)
		app = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "data is not available", "code": "unavailable"})
		})
		return
	}
	app = api.New(api.Options{
		Store: st, Log: log,
		RateRPS: 20, RateBurst: 120,
		// Vercel's proxy sets X-Real-Ip and clients cannot reach the function around it.
		ClientIPHeader: "X-Real-Ip",
		RespCacheBytes: 32 << 20,
	})
}
