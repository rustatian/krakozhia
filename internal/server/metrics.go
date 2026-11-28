package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rustatian/krakozhia/internal/store"
)

type metricsRegistry struct {
	mu           sync.Mutex
	httpRequests map[string]int64
	searches     map[string]int64
}

func newMetricsRegistry() *metricsRegistry {
	return &metricsRegistry{
		httpRequests: map[string]int64{},
		searches:     map[string]int64{},
	}
}

func (m *metricsRegistry) incHTTP(route string, status int) {
	key := fmt.Sprintf("%s\x00%d", route, status)
	m.mu.Lock()
	m.httpRequests[key]++
	m.mu.Unlock()
}

func (m *metricsRegistry) incSearch(form string, degraded bool) {
	key := fmt.Sprintf("%s\x00%t", form, degraded)
	m.mu.Lock()
	m.searches[key]++
	m.mu.Unlock()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) wrapRoute(route string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(rec, r)
		s.metrics.incHTTP(route, rec.status)
	})
}

func (s *Server) wrapRouteFunc(route string, fn http.HandlerFunc) http.Handler {
	return s.wrapRoute(route, fn)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	var b strings.Builder
	b.WriteString("# HELP krakozhia_http_requests_total Total HTTP requests handled by route and status code.\n")
	b.WriteString("# TYPE krakozhia_http_requests_total counter\n")
	s.metrics.mu.Lock()
	httpKeys := make([]string, 0, len(s.metrics.httpRequests))
	for k := range s.metrics.httpRequests {
		httpKeys = append(httpKeys, k)
	}
	slices.Sort(httpKeys)
	for _, k := range httpKeys {
		route, code, _ := strings.Cut(k, "\x00")
		fmt.Fprintf(&b, "krakozhia_http_requests_total{route=%q,status=%q} %d\n", route, code, s.metrics.httpRequests[k])
	}

	b.WriteString("# HELP krakozhia_search_requests_total Total knowledge search operations.\n")
	b.WriteString("# TYPE krakozhia_search_requests_total counter\n")
	searchKeys := make([]string, 0, len(s.metrics.searches))
	for k := range s.metrics.searches {
		searchKeys = append(searchKeys, k)
	}
	slices.Sort(searchKeys)
	for _, k := range searchKeys {
		form, deg, _ := strings.Cut(k, "\x00")
		fmt.Fprintf(&b, "krakozhia_search_requests_total{form=%q,degraded=%q} %d\n", form, deg, s.metrics.searches[k])
	}
	s.metrics.mu.Unlock()

	var stats store.StoreStats
	stats.JobsByStatus = map[string]int64{"pending": 0, "running": 0, "done": 0, "failed": 0}
	if s.store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		st, err := s.store.Stats(ctx)
		cancel()
		if err == nil {
			stats = st
		}
	}

	b.WriteString("# HELP krakozhia_records_total Current stored rows by kind.\n")
	b.WriteString("# TYPE krakozhia_records_total gauge\n")
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"documents\"} %d\n", stats.Documents)
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"chunks\"} %d\n", stats.Chunks)
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"notes\"} %d\n", stats.Notes)
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"benchmarks\"} %d\n", stats.Benchmarks)
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"github_repos\"} %d\n", stats.GitHubRepos)
	fmt.Fprintf(&b, "krakozhia_records_total{kind=\"github_items\"} %d\n", stats.GitHubItems)

	b.WriteString("# HELP krakozhia_enrichment_jobs Number of GPU enrichment jobs by status.\n")
	b.WriteString("# TYPE krakozhia_enrichment_jobs gauge\n")
	jobKeys := make([]string, 0, len(stats.JobsByStatus))
	for k := range stats.JobsByStatus {
		jobKeys = append(jobKeys, k)
	}
	slices.Sort(jobKeys)
	for _, k := range jobKeys {
		fmt.Fprintf(&b, "krakozhia_enrichment_jobs{status=%q} %d\n", k, stats.JobsByStatus[k])
	}

	_, _ = io.WriteString(w, b.String())
}
