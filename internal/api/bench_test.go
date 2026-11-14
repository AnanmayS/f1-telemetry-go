package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

// BenchmarkCompareWarm measures one /api/compare request with both laps in the
// lap cache but the response cache off, end to end
// (resample, JSON, gzip) on real data. It skips when the golden session is
// not ingested. Run: go test ./internal/api -bench Compare -run xxx
func BenchmarkCompareWarm(b *testing.B) {
	dir := os.Getenv("GHOSTLAP_DATA")
	if dir == "" {
		dir = filepath.Join("..", "..", "data")
	}
	st, err := store.Open(dir, 0, quiet)
	if err != nil {
		b.Skip(err)
	}
	defer st.Close()
	if _, err := st.Acquire(9492); err != nil {
		b.Skip("session 9492 not ingested")
	}
	s := New(Options{Store: st, Log: quiet, RespCacheBytes: -1}) // measure the uncached path
	req := httptest.NewRequest(http.MethodGet, "/api/compare?a=9492:1:fastest&b=9492:4:fastest", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != 200 {
				b.Fatal(rec.Code)
			}
		}
	})
}
