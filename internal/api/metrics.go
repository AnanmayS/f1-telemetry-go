package api

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	mismatch prometheus.Counter
}

// latencyBuckets are fine-grained around the 50 ms p99 target.
var latencyBuckets = []float64{.0005, .001, .002, .005, .01, .02, .03, .04, .05, .075, .1, .2, .5, 1, 2}

func newMetrics(st *store.Store, v *visitCounter) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ghostlap_http_requests_total", Help: "API requests by route and status code.",
		}, []string{"route", "code"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "ghostlap_http_request_duration_seconds", Help: "Server-side API request duration by route.", Buckets: latencyBuckets,
		}, []string{"route"}),
		mismatch: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ghostlap_lap_time_mismatch_total", Help: "Comparisons whose final delta differed from the official lap-time difference by more than 0.05 s.",
		}),
	}
	counter := func(name, help string, f func() float64) prometheus.Collector {
		return prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, f)
	}
	gauge := func(name, help string, f func() float64) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f)
	}
	stat := func(f func(store.Stats) int64) func() float64 {
		return func() float64 { return float64(f(st.Stats())) }
	}
	m.registry.MustRegister(
		m.requests, m.latency, m.mismatch,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		counter("ghostlap_lap_cache_hits_total", "Lap lookups served from the decoded-lap cache.", stat(func(s store.Stats) int64 { return s.CacheHits })),
		counter("ghostlap_lap_cache_misses_total", "Lap lookups that were not in the cache. Hit ratio = hits / (hits + misses).", stat(func(s store.Stats) int64 { return s.CacheMisses })),
		counter("ghostlap_lap_decodes_total", "Laps decoded from segment files.", stat(func(s store.Stats) int64 { return s.Decodes })),
		counter("ghostlap_lap_shared_loads_total", "Lap lookups that waited on another request's decode (singleflight).", stat(func(s store.Stats) int64 { return s.SharedLoads })),
		counter("ghostlap_decode_errors_total", "Laps that failed to decode (corrupt blocks).", stat(func(s store.Stats) int64 { return s.DecodeErrors })),
		gauge("ghostlap_lap_cache_bytes", "Bytes held by the decoded-lap cache.", stat(func(s store.Stats) int64 { return s.CacheBytes })),
		gauge("ghostlap_segments", "Live segments (sessions) being served.", stat(func(s store.Stats) int64 { return int64(s.Segments) })),
		gauge("ghostlap_manifest_version", "Version of the loaded manifest.", stat(func(s store.Stats) int64 { return s.ManifestVersion })),
		counter("ghostlap_page_views_total", "Page loads of the web app.", func() float64 { return float64(v.views.Load()) }),
		counter("ghostlap_share_link_opens_total", "Page loads that arrived with a comparison in the URL.", func() float64 { return float64(v.shareOpens.Load()) }),
		gauge("ghostlap_unique_visitors_today", "Distinct visitors since 00:00 UTC (salted hashes, never stored).", func() float64 { return float64(v.uniqueToday()) }),
	)
	return m
}

func (m *metrics) observe(route string, status int, d time.Duration) {
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Inc()
	m.latency.WithLabelValues(route).Observe(d.Seconds())
}
