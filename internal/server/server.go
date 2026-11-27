// Package server exposes the krakozhia HTTP REST API and Streamable HTTP MCP server.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rustatian/krakozhia/internal/chunk"
	"github.com/rustatian/krakozhia/internal/inference"
	"github.com/rustatian/krakozhia/internal/secrets"
	"github.com/rustatian/krakozhia/internal/store"
)

// InferenceClient abstracts embedding, chat, reranking, and health checks.
type InferenceClient interface {
	Health(ctx context.Context) error
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocument(ctx context.Context, text string) ([]float32, error)
	Chat(ctx context.Context, systemPrompt, userPrompt string, maxTokens int) (string, error)
	Rerank(ctx context.Context, query string, docs []string) ([]inference.RerankResult, error)
}

// Config configures a krakozhia Server.
type Config struct {
	Store     *store.Store
	Inference InferenceClient
	Token     string
	Version   string
	WebUI     http.Handler
}

// Server serves REST, MCP, and optional read-only UI endpoints.
type Server struct {
	store     *store.Store
	inf       InferenceClient
	token     string
	version   string
	webUI     http.Handler
	mcpServer *mcp.Server
}

type badRequestError struct {
	msg string
}

func (e badRequestError) Error() string { return e.msg }

// New constructs a Server with its MCP tool registry.
func New(cfg Config) *Server {
	ver := cfg.Version
	if ver == "" {
		ver = "dev"
	}
	s := &Server{
		store:   cfg.Store,
		inf:     cfg.Inference,
		token:   cfg.Token,
		version: ver,
		webUI:   cfg.WebUI,
	}
	s.mcpServer = s.newMCPServer()
	return s
}

// Handler returns the configured HTTP router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.mcpServer
	}, &mcp.StreamableHTTPOptions{
		Stateless: true,
	})
	mux.Handle("/mcp", s.requireAuth(mcpHandler))

	mux.Handle("POST /api/v1/search", s.requireAuthFunc(s.handleSearch))
	mux.Handle("POST /api/v1/documents", s.requireAuthFunc(s.handleSaveDocument))
	mux.Handle("GET /api/v1/documents", s.requireAuthFunc(s.handleListDocuments))
	mux.Handle("GET /api/v1/documents/get", s.requireAuthFunc(s.handleGetDocument))
	mux.Handle("DELETE /api/v1/documents", s.requireAuthFunc(s.handleDeleteDocument))
	mux.Handle("GET /api/v1/notes/hashes", s.requireAuthFunc(s.handleListNoteHashes))
	mux.Handle("POST /api/v1/notes", s.requireAuthFunc(s.handleSaveNote))
	mux.Handle("POST /api/v1/notes/sync", s.requireAuthFunc(s.handleSyncNotes))
	mux.Handle("GET /api/v1/notes/get", s.requireAuthFunc(s.handleGetNote))
	mux.Handle("POST /api/v1/benchmarks", s.requireAuthFunc(s.handleRecordBenchmark))
	mux.Handle("GET /api/v1/benchmarks", s.requireAuthFunc(s.handleQueryBenchmarks))
	mux.Handle("GET /api/v1/benchmarks/compare", s.requireAuthFunc(s.handleCompareBenchmarks))
	mux.Handle("GET /api/v1/briefs", s.requireAuthFunc(s.handleGetBrief))

	if s.webUI != nil {
		mux.Handle("/", s.webUI)
	}
	return mux
}

func (s *Server) requireAuthFunc(fn http.HandlerFunc) http.Handler {
	return s.requireAuth(fn)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.token == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="krakozhia"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": "unconfigured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "database": err.Error()})
		return
	}
	infStatus := "unconfigured"
	if s.inf != nil {
		if err := s.inf.Health(ctx); err != nil {
			infStatus = "degraded"
		} else {
			infStatus = "ok"
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok", "inference": infStatus})
}

// SearchRequest configures a hybrid search over documents and notes.
type SearchRequest struct {
	Form               string `json:"form,omitempty"`
	Query              string `json:"query"`
	Kind               string `json:"kind,omitempty"`
	Project            string `json:"project,omitempty"`
	ExcludeNoteProject string `json:"exclude_note_project,omitempty"`
	Type               string `json:"type,omitempty"`
	Tag                string `json:"tag,omitempty"`
	Limit              int    `json:"limit,omitempty"`
	DedupeDocs         bool   `json:"dedupe_docs,omitempty"`
	Rerank             bool   `json:"rerank,omitempty"`
}

// SearchResponse returns ranked hits and step latencies.
type SearchResponse struct {
	Hits     []store.SearchHit `json:"hits"`
	EmbedMs  float64           `json:"embed_ms"`
	QueryMs  float64           `json:"query_ms"`
	RerankMs float64           `json:"rerank_ms"`
	Degraded bool              `json:"degraded"`
}

func (s *Server) searchKnowledge(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return SearchResponse{}, badRequestError{msg: "query is required"}
	}
	form := req.Form
	if form == "" {
		form = store.FormHybrid
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	var (
		vec      []float32
		embedMs  float64
		degraded bool
	)
	if form != store.FormText && s.inf != nil {
		start := time.Now()
		emb, err := s.inf.EmbedQuery(ctx, req.Query)
		embedMs = msSince(start)
		if err != nil {
			if form == store.FormVector {
				return SearchResponse{}, fmt.Errorf("embed query: %w", err)
			}
			form = store.FormText
			degraded = true
		} else {
			vec = emb
		}
	} else if form == store.FormHybrid && s.inf == nil {
		form = store.FormText
		degraded = true
	}

	start := time.Now()
	hits, err := s.store.SearchKnowledge(ctx, store.SearchQuery{
		Form:               form,
		Text:               req.Query,
		QueryVector:        vec,
		Kind:               req.Kind,
		Project:            req.Project,
		ExcludeNoteProject: req.ExcludeNoteProject,
		Type:               req.Type,
		Tag:                req.Tag,
		Limit:              limit,
		DedupeDocs:         req.DedupeDocs,
	})
	queryMs := msSince(start)
	if err != nil {
		return SearchResponse{}, err
	}

	var rerankMs float64
	if req.Rerank && len(hits) > 1 && s.inf != nil {
		passages := make([]string, len(hits))
		for i, h := range hits {
			passages[i] = h.Title + " " + h.HeadingPath + "\n" + h.Snippet
		}
		rStart := time.Now()
		ranked, err := s.inf.Rerank(ctx, req.Query, passages)
		rerankMs = msSince(rStart)
		if err != nil {
			degraded = true
		} else {
			reordered := make([]store.SearchHit, 0, len(hits))
			for _, item := range ranked {
				if item.Index >= 0 && item.Index < len(hits) {
					reordered = append(reordered, hits[item.Index])
				}
			}
			if len(reordered) > 0 {
				hits = reordered
			}
		}
	}
	if hits == nil {
		hits = []store.SearchHit{}
	}
	return SearchResponse{
		Hits:     hits,
		EmbedMs:  embedMs,
		QueryMs:  queryMs,
		RerankMs: rerankMs,
		Degraded: degraded,
	}, nil
}

// SaveDocumentRequest specifies a document save payload.
type SaveDocumentRequest struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	DocType     string `json:"doc_type"`
	Title       string `json:"title,omitempty"`
	Content     string `json:"content"`
	SourceAgent string `json:"source_agent,omitempty"`
}

func (s *Server) saveDocument(ctx context.Context, req SaveDocumentRequest) (store.SaveDocumentResult, error) {
	if strings.TrimSpace(req.Project) == "" || strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.DocType) == "" {
		return store.SaveDocumentResult{}, badRequestError{msg: "project, name, and doc_type are required"}
	}
	if strings.Contains(req.Name, "#") {
		return store.SaveDocumentResult{}, badRequestError{msg: "document name cannot contain #"}
	}
	if findings := secrets.Scan(req.Content); len(findings) > 0 {
		return store.SaveDocumentResult{}, &secrets.FindingsError{Findings: findings}
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = chunk.Title(req.Content)
	}
	if title == "" {
		title = req.Name
	}

	sections := chunk.Split(req.Content, title)
	chunkInputs := make([]store.ChunkInput, len(sections))
	for i, sec := range sections {
		chunkInputs[i] = store.ChunkInput{Section: sec}
		if s.inf != nil {
			if vec, err := s.inf.EmbedDocument(ctx, sec.EmbedText()); err == nil {
				chunkInputs[i].Embedding = vec
			}
		}
	}
	return s.store.SaveDocument(ctx, store.SaveDocumentInput{
		Project:     req.Project,
		Name:        req.Name,
		DocType:     req.DocType,
		Title:       title,
		Content:     req.Content,
		SourceAgent: req.SourceAgent,
		Chunks:      chunkInputs,
	})
}

// GetDocumentResponse wraps either a full document or a single section chunk.
type GetDocumentResponse struct {
	Kind     string          `json:"kind"`
	Document *store.Document `json:"document,omitempty"`
	Chunk    *store.Chunk    `json:"chunk,omitempty"`
}

func (s *Server) getDocument(ctx context.Context, id string, revision int) (GetDocumentResponse, error) {
	if strings.TrimSpace(id) == "" {
		return GetDocumentResponse{}, badRequestError{msg: "id is required"}
	}
	if strings.Contains(id, "#") {
		ch, err := s.store.GetChunk(ctx, id)
		if err != nil {
			return GetDocumentResponse{}, err
		}
		return GetDocumentResponse{Kind: "chunk", Chunk: &ch}, nil
	}
	doc, err := s.store.GetDocument(ctx, id, revision)
	if err != nil {
		return GetDocumentResponse{}, err
	}
	return GetDocumentResponse{Kind: "document", Document: &doc}, nil
}

// SaveNoteRequest specifies one memory note upsert payload.
type SaveNoteRequest struct {
	ID          string     `json:"id"`
	Project     string     `json:"project"`
	NoteType    string     `json:"note_type"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Content     string     `json:"content"`
	SourceAgent string     `json:"source_agent,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

func (s *Server) saveNote(ctx context.Context, req SaveNoteRequest) error {
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Project) == "" ||
		strings.TrimSpace(req.NoteType) == "" || strings.TrimSpace(req.Name) == "" {
		return badRequestError{msg: "id, project, note_type, and name are required"}
	}
	if findings := secrets.Scan(req.Content); len(findings) > 0 {
		return &secrets.FindingsError{Findings: findings}
	}
	var vec []float32
	if s.inf != nil {
		if emb, err := s.inf.EmbedDocument(ctx, req.Content); err == nil {
			vec = emb
		}
	}
	return s.store.UpsertNote(ctx, store.UpsertNoteInput{
		ID:          req.ID,
		Project:     req.Project,
		NoteType:    req.NoteType,
		Name:        req.Name,
		Description: req.Description,
		Content:     req.Content,
		SourceAgent: req.SourceAgent,
		UpdatedAt:   req.UpdatedAt,
		Embedding:   vec,
	})
}

// SyncNotesRequest batches note upserts and deletions.
type SyncNotesRequest struct {
	Notes     []SaveNoteRequest `json:"notes"`
	DeleteIDs []string          `json:"delete_ids"`
}

// SyncNotesResponse reports batch upsert and delete counts.
type SyncNotesResponse struct {
	Written int      `json:"written"`
	Deleted int      `json:"deleted"`
	Errors  []string `json:"errors,omitempty"`
}

func (s *Server) syncNotes(ctx context.Context, req SyncNotesRequest) SyncNotesResponse {
	var res SyncNotesResponse
	for _, n := range req.Notes {
		if err := s.saveNote(ctx, n); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", n.ID, err))
			continue
		}
		res.Written++
	}
	if len(req.DeleteIDs) > 0 {
		if err := s.store.DeleteNotes(ctx, req.DeleteIDs); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("delete: %v", err))
		} else {
			res.Deleted = len(req.DeleteIDs)
		}
	}
	return res
}

func (s *Server) recordBenchmark(ctx context.Context, in store.RecordBenchmarkInput) (store.Benchmark, error) {
	if err := store.ValidateBenchmarkInput(in); err != nil {
		return store.Benchmark{}, badRequestError{msg: err.Error()}
	}
	return s.store.RecordBenchmark(ctx, in)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.searchKnowledge(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSaveDocument(w http.ResponseWriter, r *http.Request) {
	var req SaveDocumentRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.saveDocument(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntQuery(q.Get("limit"), 50)
	docs, err := s.store.ListDocuments(r.Context(), q.Get("project"), q.Get("type"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if docs == nil {
		docs = []store.Document{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rev := parseIntQuery(q.Get("revision"), 0)
	res, err := s.getDocument(r.Context(), q.Get("id"), rev)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, badRequestError{msg: "id is required"})
		return
	}
	if err := s.store.DeleteDocument(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (s *Server) handleListNoteHashes(w http.ResponseWriter, r *http.Request) {
	hashes, err := s.store.ListNoteHashes(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hashes": hashes})
}

func (s *Server) handleSaveNote(w http.ResponseWriter, r *http.Request) {
	var req SaveNoteRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.saveNote(r.Context(), req); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID})
}

func (s *Server) handleSyncNotes(w http.ResponseWriter, r *http.Request) {
	var req SyncNotesRequest
	if err := decodeJSON(r.Body, &req); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.syncNotes(r.Context(), req))
}

func (s *Server) handleGetNote(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if strings.TrimSpace(id) == "" {
		writeError(w, badRequestError{msg: "id is required"})
		return
	}
	n, err := s.store.GetNote(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleRecordBenchmark(w http.ResponseWriter, r *http.Request) {
	var in store.RecordBenchmarkInput
	if err := decodeJSON(r.Body, &in); err != nil {
		writeError(w, err)
		return
	}
	b, err := s.recordBenchmark(r.Context(), in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleQueryBenchmarks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.store.QueryBenchmarks(r.Context(), store.BenchmarkFilter{
		Project:    q.Get("project"),
		Suite:      q.Get("suite"),
		Metric:     q.Get("metric"),
		Branch:     q.Get("branch"),
		GitCommit:  q.Get("commit"),
		Status:     q.Get("status"),
		LatestOnly: q.Get("latest") == "true",
		Limit:      parseIntQuery(q.Get("limit"), 50),
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == nil {
		rows = []store.Benchmark{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": rows})
}

func (s *Server) handleCompareBenchmarks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project, suite, metric := q.Get("project"), q.Get("suite"), q.Get("metric")
	if project == "" || suite == "" || metric == "" {
		writeError(w, badRequestError{msg: "project, suite, and metric are required"})
		return
	}
	rows, err := s.store.CompareBenchmarks(r.Context(), project, suite, metric, parseIntQuery(q.Get("limit"), 50))
	if err != nil {
		writeError(w, err)
		return
	}
	if rows == nil {
		rows = []store.Benchmark{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"benchmarks": rows})
}

func (s *Server) handleGetBrief(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if strings.TrimSpace(project) == "" {
		writeError(w, badRequestError{msg: "project is required"})
		return
	}
	brief, err := s.store.GetProjectBrief(r.Context(), project)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, brief)
}

func decodeJSON(body io.Reader, dst any) error {
	dec := json.NewDecoder(io.LimitReader(body, 4<<20))
	if err := dec.Decode(dst); err != nil {
		return badRequestError{msg: fmt.Sprintf("invalid json: %v", err)}
	}
	return nil
}

func parseIntQuery(raw string, def int) int {
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func writeError(w http.ResponseWriter, err error) {
	var secErr *secrets.FindingsError
	if errors.As(err, &secErr) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    secErr.Error(),
			"findings": secErr.Findings,
		})
		return
	}
	var badReq badRequestError
	if errors.As(err, &badReq) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": badReq.Error()})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}
