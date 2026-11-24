// Package vercelapp is the HTTP handler behind the Vercel Function in
// api/index.go. It is a public package on purpose: Vercel compiles files in
// the top-level api directory as a separate module, which may not import
// this module's internal packages, so the function imports this wrapper.
//
// It serves the same HTTP layer cmd/server runs, over the segments bundled
// into the deployment (see deploy/vercel/publish.sh). Caches live for as
// long as the function instance stays warm.
package vercelapp

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/AnanmayS/f1-telemetry-go/internal/api"
	"github.com/AnanmayS/f1-telemetry-go/internal/predict"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

var (
	once sync.Once
	app  http.Handler
)

// Handler serves /api/* requests.
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
	dir := findData(log)
	var st *store.Store
	err := errors.New("no MANIFEST found in any candidate directory")
	if dir != "" {
		st, err = store.Open(dir, 128<<20, log)
	}
	if err != nil {
		// A function without its data bundle is a broken deployment: say so
		// instead of serving an empty catalogue.
		log.Error("cannot open store", "dir", dir, "err", err)
		app = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "data is not available", "code": "unavailable"})
		})
		return
	}
	log.Info("store open", "dir", dir, "segments", st.Stats().Segments)
	preds, err := predict.LoadFile(dir)
	if err != nil {
		log.Warn("predictions unavailable", "err", err)
	}
	app = api.New(api.Options{
		Store: st, Log: log, Predictions: preds,
		RateRPS: 20, RateBurst: 120,
		// Vercel's proxy sets X-Real-Ip and clients cannot reach the function around it.
		ClientIPHeader: "X-Real-Ip",
		RespCacheBytes: 32 << 20,
	})
}

// findData returns the first directory that holds a MANIFEST. Where the
// platform unpacks included files relative to the running binary is not
// something to rely on, so several places are tried.
func findData(log *slog.Logger) string {
	cwd, _ := os.Getwd()
	exe, _ := os.Executable()
	root := os.Getenv("LAMBDA_TASK_ROOT")
	if root == "" {
		root = "/var/task"
	}
	candidates := []string{os.Getenv("GHOSTLAP_DATA"), "data", filepath.Join("api", "data"),
		filepath.Join(filepath.Dir(exe), "data"), filepath.Join(filepath.Dir(exe), "api", "data"),
		filepath.Join(root, "data"), filepath.Join(root, "api", "data"), filepath.Join(cwd, "..", "data")}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(c, store.ManifestName)); err == nil {
			return c
		}
	}
	// Leave enough in the log to see where the files actually went.
	var seen []string
	for _, d := range []string{cwd, filepath.Dir(exe), "/var/task"} {
		if entries, err := os.ReadDir(d); err == nil {
			for _, e := range entries {
				seen = append(seen, filepath.Join(d, e.Name()))
			}
		}
	}
	log.Error("data bundle not found", "cwd", cwd, "exe", exe, "tried", candidates, "listing", seen)
	return ""
}
