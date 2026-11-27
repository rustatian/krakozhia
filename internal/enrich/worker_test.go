package enrich_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/chunk"
	"github.com/rustatian/krakozhia/internal/enrich"
	"github.com/rustatian/krakozhia/internal/inference"
	"github.com/rustatian/krakozhia/internal/store"
)

func TestParseDocMeta(t *testing.T) {
	t.Parallel()

	valid := map[string]bool{
		"krakozhia/old-spec": true,
		"krakozhia/self-doc": true,
	}

	cases := []struct {
		name      string
		raw       string
		wantSum   string
		wantTags  []string
		wantLinks int
		wantErr   bool
	}{
		{
			name:      "raw json normalizes tags and filters hallucinated supersedes",
			raw:       `{"summary":"  pgvector design  ","tags":["Postgres","pgvector","postgres","  Go "],"possible_supersedes":[{"id":"krakozhia/old-spec","reason":"replaces S3 Vectors"},{"id":"krakozhia/self-doc","reason":"self"},{"id":"krakozhia/invented","reason":"fake"}]}`,
			wantSum:   "pgvector design",
			wantTags:  []string{"postgres", "pgvector", "go"},
			wantLinks: 1,
		},
		{
			name:      "fenced json block accepted",
			raw:       "```json\n{\"summary\":\"Brief summary.\",\"tags\":[\"k8s\"]}\n```",
			wantSum:   "Brief summary.",
			wantTags:  []string{"k8s"},
			wantLinks: 0,
		},
		{
			name:    "invalid json fails",
			raw:     "not json at all",
			wantErr: true,
		},
		{
			name:    "empty summary fails",
			raw:     `{"summary":"   ","tags":["a"]}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := enrich.ParseDocMeta(tc.raw, valid, "krakozhia/self-doc")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDocMeta() = %v", err)
			}
			if got.Summary != tc.wantSum || !slices.Equal(got.Tags, tc.wantTags) || len(got.PossibleSupersedes) != tc.wantLinks {
				t.Fatalf("got %+v, want sum=%q tags=%v links=%d", got, tc.wantSum, tc.wantTags, tc.wantLinks)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		attempt int
		want    time.Duration
	}{
		{name: "first attempt", attempt: 1, want: 30 * time.Second},
		{name: "second attempt", attempt: 2, want: 60 * time.Second},
		{name: "fourth attempt", attempt: 4, want: 4 * time.Minute},
		{name: "capped at 15m", attempt: 10, want: 15 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := enrich.Backoff(tc.attempt); got != tc.want {
				t.Fatalf("Backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

type mockInference struct {
	chatCalls  int
	embedCalls int
	failChat   bool
}

func (m *mockInference) EmbedDocument(_ context.Context, _ string) ([]float32, error) {
	m.embedCalls++
	v := make([]float32, 1024)
	v[0] = 1.0
	return v, nil
}

func (m *mockInference) Chat(_ context.Context, sys, user string, _ int) (string, error) {
	m.chatCalls++
	if m.failChat {
		return "", errors.New("gpu offline")
	}
	if strings.Contains(sys, "contextual") {
		return "This section covers storage details in Vector Design.", nil
	}
	if strings.Contains(sys, "project brief") {
		return "# krakozhia\n\nUses PostgreSQL with pgvector 0.8.7. Latest p95 recall: 42.5 ms.", nil
	}
	if strings.Contains(user, "Existing documents") {
		return `{"summary":"Vector storage architecture.","tags":["PostgreSQL","pgvector"],"possible_supersedes":[{"id":"krakozhia/old-spec","reason":"replaces old spec"}]}`, nil
	}
	return `{"summary":"GPU note summary.","tags":["gpu","vulkan"]}`, nil
}

func openEnrichTestStore(t *testing.T) *store.Store {
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
	schema := "krakozhia_enr_" + hex.EncodeToString(raw[:])

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

func TestWorkerIntegrationAndIdempotency(t *testing.T) {
	st := openEnrichTestStore(t)
	ctx := context.Background()

	// Seed an older document that the new doc will supersede.
	if _, err := st.SaveDocument(ctx, store.SaveDocumentInput{
		Project: "krakozhia",
		Name:    "old-spec",
		DocType: "spec",
		Title:   "Old S3 Spec",
		Content: "# Old S3 Spec\n\nUsed S3 Vectors.\n",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Old S3 Spec", Content: "Used S3 Vectors."}},
		},
	}); err != nil {
		t.Fatalf("SaveDocument old-spec: %v", err)
	}
	if _, err := st.SaveDocument(ctx, store.SaveDocumentInput{
		Project: "krakozhia",
		Name:    "pg-spec",
		DocType: "spec",
		Title:   "Vector Design",
		Content: "# Vector Design\n\n## Storage\nUses pgvector.\n",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Vector Design > Storage", Content: "Uses pgvector."}},
		},
	}); err != nil {
		t.Fatalf("SaveDocument pg-spec: %v", err)
	}
	if err := st.UpsertNote(ctx, store.UpsertNoteInput{
		ID:          "krakozhia/gpu",
		Project:     "krakozhia",
		NoteType:    "reference",
		Name:        "GPU setup",
		Description: "RX 9070 XT on wodka-3",
		Content:     "llama-server Vulkan router mode.",
	}); err != nil {
		t.Fatalf("UpsertNote: %v", err)
	}
	if _, err := st.RecordBenchmark(ctx, store.RecordBenchmarkInput{
		Project: "krakozhia",
		Suite:   "recall",
		Metric:  "p95_ms",
		Value:   42.5,
		Unit:    "ms",
		Status:  "baseline",
	}); err != nil {
		t.Fatalf("RecordBenchmark: %v", err)
	}

	inf := &mockInference{failChat: true}
	w := enrich.New(enrich.Config{
		Store:        st,
		Inference:    inf,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// First job fails while GPU is offline without blocking writes.
	didWork, err := w.ProcessOnce(ctx)
	if !didWork || err == nil {
		t.Fatalf("expected failed job while GPU offline, got didWork=%v err=%v", didWork, err)
	}

	// Recover GPU, make retry immediately eligible, and drain all queued jobs.
	inf.failChat = false
	if err := st.Exec(ctx, `UPDATE enrichment_jobs SET run_after = NOW()`); err != nil {
		t.Fatalf("reset run_after: %v", err)
	}
	for i := 0; i < 20; i++ {
		worked, err := w.ProcessOnce(ctx)
		if err != nil {
			t.Fatalf("ProcessOnce step %d: %v", i, err)
		}
		if !worked {
			break
		}
	}

	doc, err := st.GetDocument(ctx, "krakozhia/pg-spec", 0)
	if err != nil || doc.Summary != "Vector storage architecture." || !slices.Equal(doc.Tags, []string{"postgresql", "pgvector"}) {
		t.Fatalf("enriched doc = %+v, err=%v", doc, err)
	}
	chunks, err := st.ListChunks(ctx, "krakozhia/pg-spec")
	if err != nil || len(chunks) != 1 || chunks[0].ContextPrefix == "" {
		t.Fatalf("enriched chunks = %+v, err=%v", chunks, err)
	}
	links, err := st.ListRecordLinks(ctx, "doc", "krakozhia/old-spec")
	if err != nil || len(links) != 1 || links[0].SourceID != "krakozhia/pg-spec" {
		t.Fatalf("record links = %+v, err=%v", links, err)
	}
	note, err := st.GetNote(ctx, "krakozhia/gpu")
	if err != nil || note.Summary != "GPU note summary." {
		t.Fatalf("enriched note = %+v, err=%v", note, err)
	}
	brief, err := st.GetProjectBrief(ctx, "krakozhia")
	if err != nil || !strings.Contains(brief.BriefMD, "42.5 ms") {
		t.Fatalf("brief = %+v, err=%v", brief, err)
	}

	// Refresh once so the brief reflects all newly enriched summaries, then verify a repeat refresh is a no-op.
	if err := st.Exec(ctx, `INSERT INTO enrichment_jobs (job_type, target_kind, target_id) VALUES ('refresh_brief', 'project', 'krakozhia')`); err != nil {
		t.Fatalf("insert refresh_brief 1: %v", err)
	}
	if _, err := w.ProcessOnce(ctx); err != nil {
		t.Fatalf("ProcessOnce refresh_brief 1: %v", err)
	}
	chatsBefore := inf.chatCalls
	if err := st.Exec(ctx, `INSERT INTO enrichment_jobs (job_type, target_kind, target_id) VALUES ('refresh_brief', 'project', 'krakozhia')`); err != nil {
		t.Fatalf("insert refresh_brief 2: %v", err)
	}
	worked, err := w.ProcessOnce(ctx)
	if err != nil || !worked {
		t.Fatalf("idempotent refresh_brief = worked:%v err:%v", worked, err)
	}
	if inf.chatCalls != chatsBefore {
		t.Fatalf("expected 0 chat calls when source_hash matches, got %d", inf.chatCalls-chatsBefore)
	}
}

func TestWorkerLiveGPU(t *testing.T) {
	infURL := os.Getenv("KRAKOZHIA_TEST_INFERENCE_URL")
	if infURL == "" {
		t.Skip("KRAKOZHIA_TEST_INFERENCE_URL is not set")
	}
	st := openEnrichTestStore(t)
	ctx := context.Background()

	if _, err := st.SaveDocument(ctx, store.SaveDocumentInput{
		Project: "krakozhia",
		Name:    "gpu-design",
		DocType: "spec",
		Title:   "Krakozhia Knowledge Base",
		Content: "# Krakozhia Knowledge Base\n\n## Storage\nUses PostgreSQL 18.7 with pgvector 0.8.7 on CloudNativePG.\n",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Krakozhia Knowledge Base > Storage", Content: "Uses PostgreSQL 18.7 with pgvector 0.8.7 on CloudNativePG."}},
		},
	}); err != nil {
		t.Fatalf("SaveDocument: %v", err)
	}

	inf := inference.New(inference.Config{
		BaseURL:     infURL,
		EmbedModel:  "qwen3-embedding-0.6b",
		ChatModel:   "qwen3-4b-instruct",
		RerankModel: "bge-reranker-v2-m3",
		Timeout:     30 * time.Second,
	})
	w := enrich.New(enrich.Config{
		Store:        st,
		Inference:    inf,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	start := time.Now()
	for i := 0; i < 10; i++ {
		worked, err := w.ProcessOnce(ctx)
		if err != nil {
			t.Fatalf("live ProcessOnce step %d: %v", i, err)
		}
		if !worked {
			break
		}
	}
	elapsed := time.Since(start)

	doc, err := st.GetDocument(ctx, "krakozhia/gpu-design", 0)
	if err != nil || doc.Summary == "" || len(doc.Tags) == 0 {
		t.Fatalf("live enriched doc = %+v, err=%v", doc, err)
	}
	chunks, err := st.ListChunks(ctx, "krakozhia/gpu-design")
	if err != nil || len(chunks) != 1 || chunks[0].ContextPrefix == "" {
		t.Fatalf("live enriched chunks = %+v, err=%v", chunks, err)
	}
	brief, err := st.GetProjectBrief(ctx, "krakozhia")
	if err != nil || brief.BriefMD == "" {
		t.Fatalf("live brief = %+v, err=%v", brief, err)
	}
	t.Logf("live GPU enrichment elapsed=%s summary=%q tags=%v prefix=%q", elapsed, doc.Summary, doc.Tags, chunks[0].ContextPrefix)
}
