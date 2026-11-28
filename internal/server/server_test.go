package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rustatian/krakozhia/internal/inference"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

type fakeInference struct {
	healthErr error
	embedErr  error
	rerankErr error
}

func (f *fakeInference) Health(context.Context) error { return f.healthErr }

func (f *fakeInference) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return f.vecFor(text), nil
}

func (f *fakeInference) EmbedDocument(_ context.Context, text string) ([]float32, error) {
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return f.vecFor(text), nil
}

func (f *fakeInference) Rerank(_ context.Context, _ string, docs []string) ([]inference.RerankResult, error) {
	if f.rerankErr != nil {
		return nil, f.rerankErr
	}
	out := make([]inference.RerankResult, 0, len(docs))
	for i := len(docs) - 1; i >= 0; i-- {
		out = append(out, inference.RerankResult{Index: i, Score: float64(i + 1)})
	}
	return out, nil
}

func (f *fakeInference) vecFor(text string) []float32 {
	v := make([]float32, 1024)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "pgvector") {
		v[0] = 1.0
	} else {
		v[1] = 1.0
	}
	return v
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header.Set("Authorization", "Bearer "+b.token)
	rt := b.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(cloned)
}

func openServerTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("KRAKOZHIA_TEST_DSN")
	if dsn == "" {
		t.Skip("KRAKOZHIA_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "krakozhia_srv_" + hex.EncodeToString(raw[:])

	adminStore, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open admin: %v", err)
	}
	if err := adminStore.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		adminStore.Close()
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanCancel()
		_ = adminStore.Exec(cleanCtx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
		adminStore.Close()
	})

	s, err := store.OpenWithSearchPath(ctx, dsn, schema+",public")
	if err != nil {
		t.Fatalf("OpenWithSearchPath: %v", err)
	}
	t.Cleanup(s.Close)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestBearerAuth(t *testing.T) {
	t.Parallel()

	srv := server.New(server.Config{
		Inference: &fakeInference{},
		Token:     "secret-token-123",
		Version:   "test",
	})
	h := srv.Handler()

	cases := []struct {
		name       string
		method     string
		path       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "healthz requires no auth",
			method:     http.MethodGet,
			path:       "/healthz",
			wantStatus: http.StatusServiceUnavailable, // store is nil, proves auth did not block
		},
		{
			name:       "missing authorization header rejected",
			method:     http.MethodGet,
			path:       "/api/v1/documents",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong token rejected",
			method:     http.MethodGet,
			path:       "/api/v1/documents",
			authHeader: "Bearer wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "basic scheme rejected",
			method:     http.MethodPost,
			path:       "/mcp",
			authHeader: "Basic dXNlcjpwYXNz",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d (body=%s)", tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("expected WWW-Authenticate header on 401")
			}
		})
	}
}

func doAuthedJSON(t *testing.T, baseURL, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	req, err := http.NewRequest(method, baseURL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var raw bytes.Buffer
	_, _ = raw.ReadFrom(resp.Body)
	return resp.StatusCode, raw.Bytes()
}

func TestRESTAndMCPIntegration(t *testing.T) {
	st := openServerTestStore(t)
	inf := &fakeInference{}
	const token = "integration-bearer-token"

	srv := server.New(server.Config{
		Store:     st,
		Inference: inf,
		Token:     token,
		Version:   "test",
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Health check healthy & degraded inference.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %v, %v", resp, err)
	}
	_ = resp.Body.Close()

	inf.healthErr = errors.New("gpu offline")
	resp, err = http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz degraded = %v, %v", resp, err)
	}
	var healthPayload map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&healthPayload)
	_ = resp.Body.Close()
	if healthPayload["inference"] != "degraded" {
		t.Fatalf("health inference = %q, want degraded", healthPayload["inference"])
	}
	inf.healthErr = nil

	// Secret detection returns 422.
	code, _ := doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/documents", map[string]any{
		"project":  "krakozhia",
		"name":     "bad-doc",
		"doc_type": "spec",
		"content":  "# Bad\n\n" + "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890\n",
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("secret doc status = %d, want 422", code)
	}

	// Save document, list, get full & chunk, and verify missing is 404.
	code, body := doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/documents", map[string]any{
		"project":      "krakozhia",
		"name":         "arch",
		"doc_type":     "spec",
		"content":      "# Architecture\n\n## Database\nUses PostgreSQL with pgvector 0.8.7.\n\n## GPU\nUses RX 9070 XT.\n",
		"source_agent": "claude",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/v1/documents = %d (%s)", code, body)
	}
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodGet, "/api/v1/documents/get?id=krakozhia/arch%230", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "pgvector 0.8.7") {
		t.Fatalf("GET chunk = %d (%s)", code, body)
	}
	code, _ = doAuthedJSON(t, ts.URL, token, http.MethodGet, "/api/v1/documents/get?id=krakozhia/missing", nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET missing doc = %d, want 404", code)
	}

	// Sync notes and verify hashes.
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/notes/sync", map[string]any{
		"notes": []map[string]any{
			{
				"id":           "infra/gpu-note",
				"project":      "infra",
				"note_type":    "reference",
				"name":         "RX 9070 XT",
				"description":  "Vulkan router mode on wodka-3",
				"content":      "Runs llama-server with pgvector embeddings.",
				"source_agent": "claude",
			},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("POST /api/v1/notes/sync = %d (%s)", code, body)
	}
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodGet, "/api/v1/notes/hashes", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "infra/gpu-note") {
		t.Fatalf("GET /api/v1/notes/hashes = %d (%s)", code, body)
	}

	// Chat session ingest and hashes.
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/chats/ingest", map[string]any{
		"chats": []map[string]any{
			{
				"source_agent": "claude",
				"external_id":  "sess-42",
				"project":      "krakozhia",
				"title":        "Architecture discussion",
				"raw_excerpt":  "User: Design chat sync.\nAssistant: Use async GPU distillation.",
			},
		},
	})
	if code != http.StatusOK || !strings.Contains(string(body), `"queued":1`) {
		t.Fatalf("POST /api/v1/chats/ingest = %d (%s)", code, body)
	}
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodGet, "/api/v1/chats/hashes", nil)
	if code != http.StatusOK || !strings.Contains(string(body), "claude:sess-42") {
		t.Fatalf("GET /api/v1/chats/hashes = %d (%s)", code, body)
	}

	// Search hybrid + graceful fallback when inference fails.
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/search", map[string]any{
		"form":        "hybrid",
		"query":       "pgvector",
		"dedupe_docs": true,
		"limit":       5,
	})
	if code != http.StatusOK || !strings.Contains(string(body), "krakozhia/arch#0") {
		t.Fatalf("POST /api/v1/search = %d (%s)", code, body)
	}

	inf.embedErr = errors.New("inference down")
	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/search", map[string]any{
		"form":  "hybrid",
		"query": "pgvector",
		"limit": 5,
	})
	if code != http.StatusOK || !strings.Contains(string(body), `"degraded":true`) {
		t.Fatalf("degraded POST /api/v1/search = %d (%s)", code, body)
	}
	inf.embedErr = nil

	// Benchmarks REST + validation + JSON round-trip.
	code, _ = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/benchmarks", map[string]any{
		"project": "krakozhia",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid benchmark = %d, want 400", code)
	}

	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/benchmarks", map[string]any{
		"project":      "krakozhia",
		"suite":        "recall",
		"metric":       "p95_ms",
		"value":        200.0,
		"unit":         "ms",
		"git_commit":   "abc1234",
		"branch":       "main",
		"environment":  "wodka",
		"status":       "baseline",
		"notes":        "initial run",
		"document_id":  "krakozhia/arch",
		"source_agent": "claude",
	})
	if code != http.StatusOK {
		t.Fatalf("POST baseline benchmark = %d (%s)", code, body)
	}
	var baseBench store.Benchmark
	if err := json.Unmarshal(body, &baseBench); err != nil {
		t.Fatalf("unmarshal baseline benchmark: %v", err)
	}
	if baseBench.GitCommit != "abc1234" || baseBench.Branch != "main" || baseBench.Environment != "wodka" ||
		baseBench.Status != "baseline" || baseBench.Notes != "initial run" ||
		baseBench.DocumentID == nil || *baseBench.DocumentID != "krakozhia/arch" ||
		baseBench.SourceAgent != "claude" {
		t.Fatalf("baseline benchmark fields lost: %+v", baseBench)
	}

	code, body = doAuthedJSON(t, ts.URL, token, http.MethodPost, "/api/v1/benchmarks", map[string]any{
		"project":      "krakozhia",
		"suite":        "recall",
		"metric":       "p95_ms",
		"value":        150.0,
		"unit":         "ms",
		"baseline_id":  baseBench.ID,
		"git_commit":   "def5678",
		"branch":       "main",
		"environment":  "wodka",
		"status":       "kept",
		"notes":        "tuned query",
		"source_agent": "claude",
	})
	if code != http.StatusOK {
		t.Fatalf("POST improved benchmark = %d (%s)", code, body)
	}
	var impBench store.Benchmark
	if err := json.Unmarshal(body, &impBench); err != nil {
		t.Fatalf("unmarshal improved benchmark: %v", err)
	}
	if impBench.BaselineID == nil || *impBench.BaselineID != baseBench.ID {
		t.Fatalf("baseline_id = %v, want %d", impBench.BaselineID, baseBench.ID)
	}
	if impBench.DeltaPct == nil || *impBench.DeltaPct != -25.0 {
		t.Fatalf("delta_pct = %v, want -25", impBench.DeltaPct)
	}

	// MCP Streamable HTTP client end-to-end.
	ctx := context.Background()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("MCP Connect: %v", err)
	}
	defer session.Close()

	toolsRes, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("MCP ListTools: %v", err)
	}
	gotTools := make([]string, 0, len(toolsRes.Tools))
	for _, tool := range toolsRes.Tools {
		gotTools = append(gotTools, tool.Name)
	}
	slices.Sort(gotTools)
	wantTools := []string{
		"compare_benchmarks",
		"get_document",
		"get_note",
		"get_project_brief",
		"list_documents",
		"query_benchmarks",
		"record_benchmark",
		"save_document",
		"save_note",
		"search_knowledge",
	}
	if !slices.Equal(gotTools, wantTools) {
		t.Fatalf("MCP tools = %v, want %v", gotTools, wantTools)
	}

	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "save_document",
		Arguments: map[string]any{
			"project":      "krakozhia",
			"name":         "mcp-doc",
			"doc_type":     "plan",
			"content":      "# MCP Plan\n\n## Tools\nExpose ten tools over Streamable HTTP.\n",
			"source_agent": "antigravity",
		},
	})
	if err != nil || callRes.IsError {
		t.Fatalf("MCP save_document = %+v, err=%v", callRes, err)
	}

	searchRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "search_knowledge",
		Arguments: map[string]any{
			"query": "Streamable HTTP",
			"limit": 5,
		},
	})
	if err != nil || searchRes.IsError {
		t.Fatalf("MCP search_knowledge = %+v, err=%v", searchRes, err)
	}

	benchRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "record_benchmark",
		Arguments: map[string]any{
			"project":      "krakozhia",
			"suite":        "inference",
			"metric":       "embed_ms",
			"value":        119.0,
			"unit":         "ms",
			"status":       "baseline",
			"source_agent": "antigravity",
		},
	})
	if err != nil || benchRes.IsError {
		t.Fatalf("MCP record_benchmark = %+v, err=%v", benchRes, err)
	}

	if err := st.UpsertProjectBrief(ctx, "krakozhia", "# Krakozhia\nReady.", "h1"); err != nil {
		t.Fatalf("UpsertProjectBrief: %v", err)
	}
	briefRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_project_brief",
		Arguments: map[string]any{"project": "krakozhia"},
	})
	if err != nil || briefRes.IsError {
		t.Fatalf("MCP get_project_brief = %+v, err=%v", briefRes, err)
	}
}
