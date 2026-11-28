package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

// Client calls the krakozhia REST API with bearer authentication.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient constructs a REST API client for baseURL and bearer token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, dst any) error {
	var body io.Reader
	if reqBody != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var apiErr struct {
			Error    string `json:"error"`
			Findings []any  `json:"findings"`
		}
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
			if len(apiErr.Findings) > 0 {
				return fmt.Errorf("http %d: %s (%v)", resp.StatusCode, apiErr.Error, apiErr.Findings)
			}
			return fmt.Errorf("http %d: %s", resp.StatusCode, apiErr.Error)
		}
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// Search queries /api/v1/search.
func (c *Client) Search(ctx context.Context, req server.SearchRequest) (server.SearchResponse, error) {
	var res server.SearchResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/search", req, &res)
	return res, err
}

// SaveDocument calls POST /api/v1/documents.
func (c *Client) SaveDocument(ctx context.Context, req server.SaveDocumentRequest) (store.SaveDocumentResult, error) {
	var res store.SaveDocumentResult
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/documents", req, &res)
	return res, err
}

// ListDocuments calls GET /api/v1/documents.
func (c *Client) ListDocuments(ctx context.Context, project, docType string, limit int) ([]store.Document, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if docType != "" {
		q.Set("type", docType)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Documents []store.Document `json:"documents"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/documents?"+q.Encode(), nil, &out)
	return out.Documents, err
}

// GetDocument calls GET /api/v1/documents/get.
func (c *Client) GetDocument(ctx context.Context, id string, revision int) (server.GetDocumentResponse, error) {
	q := url.Values{}
	q.Set("id", id)
	if revision > 0 {
		q.Set("revision", strconv.Itoa(revision))
	}
	var out server.GetDocumentResponse
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/documents/get?"+q.Encode(), nil, &out)
	return out, err
}

// GetNote calls GET /api/v1/notes/get.
func (c *Client) GetNote(ctx context.Context, id string) (store.Note, error) {
	q := url.Values{}
	q.Set("id", id)
	var out store.Note
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/notes/get?"+q.Encode(), nil, &out)
	return out, err
}

// ListNoteHashes calls GET /api/v1/notes/hashes.
func (c *Client) ListNoteHashes(ctx context.Context) (map[string]string, error) {
	var out struct {
		Hashes map[string]string `json:"hashes"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/notes/hashes", nil, &out)
	if out.Hashes == nil {
		out.Hashes = map[string]string{}
	}
	return out.Hashes, err
}

// SyncNotes calls POST /api/v1/notes/sync.
func (c *Client) SyncNotes(ctx context.Context, req server.SyncNotesRequest) (server.SyncNotesResponse, error) {
	var out server.SyncNotesResponse
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/notes/sync", req, &out)
	return out, err
}

// RecordBenchmark calls POST /api/v1/benchmarks.
func (c *Client) RecordBenchmark(ctx context.Context, in store.RecordBenchmarkInput) (store.Benchmark, error) {
	var out store.Benchmark
	err := c.doJSON(ctx, http.MethodPost, "/api/v1/benchmarks", in, &out)
	return out, err
}

// QueryBenchmarks calls GET /api/v1/benchmarks.
func (c *Client) QueryBenchmarks(ctx context.Context, f store.BenchmarkFilter) ([]store.Benchmark, error) {
	q := url.Values{}
	if f.Project != "" {
		q.Set("project", f.Project)
	}
	if f.Suite != "" {
		q.Set("suite", f.Suite)
	}
	if f.Metric != "" {
		q.Set("metric", f.Metric)
	}
	if f.Branch != "" {
		q.Set("branch", f.Branch)
	}
	if f.GitCommit != "" {
		q.Set("commit", f.GitCommit)
	}
	if f.Status != "" {
		q.Set("status", f.Status)
	}
	if f.LatestOnly {
		q.Set("latest", "true")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out struct {
		Benchmarks []store.Benchmark `json:"benchmarks"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/benchmarks?"+q.Encode(), nil, &out)
	return out.Benchmarks, err
}

// CompareBenchmarks calls GET /api/v1/benchmarks/compare.
func (c *Client) CompareBenchmarks(ctx context.Context, project, suite, metric string, limit int) ([]store.Benchmark, error) {
	q := url.Values{}
	q.Set("project", project)
	q.Set("suite", suite)
	q.Set("metric", metric)
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Benchmarks []store.Benchmark `json:"benchmarks"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/benchmarks/compare?"+q.Encode(), nil, &out)
	return out.Benchmarks, err
}

// GetBrief calls GET /api/v1/briefs.
func (c *Client) GetBrief(ctx context.Context, project string) (store.ProjectBrief, error) {
	q := url.Values{}
	q.Set("project", project)
	var out store.ProjectBrief
	err := c.doJSON(ctx, http.MethodGet, "/api/v1/briefs?"+q.Encode(), nil, &out)
	return out, err
}
