package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/store"
	"github.com/rustatian/krakozhia/internal/webui"
)

type fakeReader struct{}

func (fakeReader) Stats(context.Context) (store.StoreStats, error) {
	return store.StoreStats{
		Documents: 1,
		Chunks:    1,
		Notes:     1,
		Benchmarks: 1,
		JobsByStatus: map[string]int64{"done": 2},
	}, nil
}

func (fakeReader) ListProjects(context.Context) ([]string, error) {
	return []string{"alpha"}, nil
}

func (fakeReader) ListDocuments(_ context.Context, project, _ string, _ int) ([]store.Document, error) {
	if project != "" && project != "alpha" {
		return nil, nil
	}
	return []store.Document{{
		ID:              "alpha/spec",
		Project:         "alpha",
		Name:            "spec",
		DocType:         "spec",
		Title:           "Alpha <script>alert(1)</script>",
		Summary:         "Doc summary",
		Tags:            []string{"design"},
		CurrentRevision: 1,
		Revision:        1,
		UpdatedAt:       time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func (fakeReader) GetDocument(_ context.Context, id string, _ int) (store.Document, error) {
	if id != "alpha/spec" {
		return store.Document{}, store.ErrNotFound
	}
	return store.Document{
		ID:              "alpha/spec",
		Project:         "alpha",
		Name:            "spec",
		DocType:         "spec",
		Title:           "Alpha <script>alert(1)</script>",
		Summary:         "Doc summary",
		Tags:            []string{"design"},
		CurrentRevision: 1,
		Revision:        1,
		ContentMD:       "# Heading\n<script>xss()</script>\n",
		UpdatedAt:       time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}, nil
}

func (fakeReader) ListChunks(context.Context, string) ([]store.Chunk, error) {
	return []store.Chunk{{
		ID:            "alpha/spec#0",
		DocumentID:    "alpha/spec",
		SectionIndex:  0,
		HeadingPath:   "Heading",
		Content:       "Chunk body",
		ContextPrefix: "Context prefix",
	}}, nil
}

func (fakeReader) ListNotes(_ context.Context, project, _ string, _ int) ([]store.Note, error) {
	if project != "" && project != "alpha" {
		return nil, nil
	}
	return []store.Note{{
		ID:          "alpha/mem",
		Project:     "alpha",
		NoteType:    "reference",
		Name:        "mem",
		Description: "Note desc",
		Summary:     "Note sum",
		Tags:        []string{"ops"},
		Content:     "Note content <img src=x onerror=alert(1)>",
		UpdatedAt:   time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func (fakeReader) GetNote(_ context.Context, id string) (store.Note, error) {
	if id != "alpha/mem" {
		return store.Note{}, store.ErrNotFound
	}
	return store.Note{
		ID:        "alpha/mem",
		Project:   "alpha",
		NoteType:  "reference",
		Name:      "mem",
		Summary:   "Note sum",
		Tags:      []string{"ops"},
		Content:   "Note content <img src=x onerror=alert(1)>",
		UpdatedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}, nil
}

func (fakeReader) QueryBenchmarks(context.Context, store.BenchmarkFilter) ([]store.Benchmark, error) {
	d := -12.5
	return []store.Benchmark{{
		ID:          1,
		Project:     "alpha",
		Suite:       "recall",
		Metric:      "p95_ms",
		Value:       42.5,
		Unit:        "ms",
		DeltaPct:    &d,
		Status:      "kept",
		GitCommit:   "abc1234",
		Branch:      "main",
		Environment: "wodka",
		Notes:       "good",
		RecordedAt:  time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}}, nil
}

func (fakeReader) GetProjectBrief(_ context.Context, project string) (store.ProjectBrief, error) {
	if project != "alpha" {
		return store.ProjectBrief{}, store.ErrNotFound
	}
	return store.ProjectBrief{
		Project:   "alpha",
		BriefMD:   "# Alpha\nBrief text",
		UpdatedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
	}, nil
}

func (fakeReader) ListRecordLinks(context.Context, string, string) ([]store.RecordLink, error) {
	return []store.RecordLink{{
		SourceKind: "doc",
		SourceID:   "alpha/new",
		TargetKind: "doc",
		TargetID:   "alpha/spec",
		Relation:   "possible_supersedes",
		Note:       "newer design",
	}}, nil
}

func TestWebUIRoutesAndReadOnlyEnforcement(t *testing.T) {
	t.Parallel()

	ui := webui.New(fakeReader{}, func(_ context.Context, p webui.SearchParams) ([]store.SearchHit, bool, error) {
		return []store.SearchHit{{
			Kind:       "doc",
			ID:         "alpha/spec#0",
			DocumentID: "alpha/spec",
			Project:    "alpha",
			Type:       "spec",
			Title:      "Alpha Search Hit",
			Snippet:    "Matched snippet for " + p.Query,
		}}, false, nil
	})

	cases := []struct {
		name           string
		method         string
		target         string
		wantStatus     int
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:       "home page lists projects docs benchmarks and escapes HTML",
			method:     http.MethodGet,
			target:     "/",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"krakozhia",
				"alpha/spec",
				"42.5 ms",
				"&lt;script&gt;alert(1)&lt;/script&gt;",
			},
			wantNotContain: []string{
				"<script>alert(1)</script>",
			},
		},
		{
			name:       "search page renders results",
			method:     http.MethodGet,
			target:     "/search?q=pgvector",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"Alpha Search Hit",
				"Matched snippet for pgvector",
			},
		},
		{
			name:       "project page renders brief docs notes and benchmarks",
			method:     http.MethodGet,
			target:     "/projects/alpha",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"Brief text",
				"alpha/mem",
				"42.5",
			},
		},
		{
			name:       "document page renders escaped markdown chunks and links",
			method:     http.MethodGet,
			target:     "/document?id=alpha/spec",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"Context prefix",
				"possible_supersedes",
				"&lt;script&gt;xss()&lt;/script&gt;",
			},
			wantNotContain: []string{
				"<script>xss()</script>",
			},
		},
		{
			name:       "note page escapes HTML in content",
			method:     http.MethodGet,
			target:     "/note?id=alpha/mem",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"Note sum",
				"&lt;img src=x onerror=alert(1)&gt;",
			},
			wantNotContain: []string{
				"<img src=x onerror=alert(1)>",
			},
		},
		{
			name:       "benchmarks page renders comparison table",
			method:     http.MethodGet,
			target:     "/benchmarks?project=alpha",
			wantStatus: http.StatusOK,
			wantContains: []string{
				"p95_ms",
				"-12.50%",
			},
		},
		{
			name:       "unknown route returns 404",
			method:     http.MethodGet,
			target:     "/unknown/path",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "POST rejected with 405",
			method:     http.MethodPost,
			target:     "/",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "DELETE rejected with 405",
			method:     http.MethodDelete,
			target:     "/document?id=alpha/spec",
			wantStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(tc.method, tc.target, nil)
			rec := httptest.NewRecorder()
			ui.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, want %d (body=%s)", tc.method, tc.target, rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			for _, w := range tc.wantContains {
				if !strings.Contains(body, w) {
					t.Fatalf("%s %s missing %q", tc.method, tc.target, w)
				}
			}
			for _, nw := range tc.wantNotContain {
				if strings.Contains(body, nw) {
					t.Fatalf("%s %s unexpectedly contained %q", tc.method, tc.target, nw)
				}
			}
		})
	}
}
