package server_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rustatian/krakozhia/internal/server"
)

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()

	srv := server.New(server.Config{
		Inference: &fakeInference{},
		Token:     "tok",
		Version:   "test",
	})
	h := srv.Handler()

	// Trigger one 200 (/healthz -> 503 since store is nil) and one 401.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/search", bytes.NewBufferString(`{"query":"x"}`)))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"krakozhia_http_requests_total",
		`route="GET /healthz"`,
		`route="POST /api/v1/search"`,
		"krakozhia_search_requests_total",
		"krakozhia_records_total",
		"krakozhia_enrichment_jobs",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q:\n%s", want, body)
		}
	}
}
