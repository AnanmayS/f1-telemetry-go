package vercelapp

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Without bundled data the function must answer with a clean 503, and it
// must never serve anything outside /api/.
func TestHandlerWithoutData(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MANIFEST"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOSTLAP_DATA", dir)
	cases := []struct {
		path string
		code int
	}{{"/metrics", 404}, {"/", 404}, {"/api/health", 503}}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		Handler(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.code {
			t.Errorf("%s: %d, want %d (%s)", c.path, rec.Code, c.code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	Handler(rec, httptest.NewRequest("GET", "/api/seasons", nil))
	if !strings.Contains(rec.Body.String(), `"code":"unavailable"`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unavailable response: %s %v", rec.Body, rec.Header())
	}
}

func TestFindData(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MANIFEST"), []byte("{}"), 0o644)
	t.Setenv("GHOSTLAP_DATA", dir)
	if got := findData(slog.New(slog.NewTextHandler(io.Discard, nil))); got != dir {
		t.Fatalf("findData = %q, want %q", got, dir)
	}
	t.Setenv("GHOSTLAP_DATA", filepath.Join(dir, "nope"))
	t.Chdir(t.TempDir()) // no ./data here
	if got := findData(slog.New(slog.NewTextHandler(io.Discard, nil))); got != "" {
		t.Fatalf("findData found %q where there is no bundle", got)
	}
}
