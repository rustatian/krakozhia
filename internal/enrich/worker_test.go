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
			name:      "string tags normalized",
			raw:       `{"summary":"String tags summary.","tags":"Go, Postgres - pgvector"}`,
			wantSum:   "String tags summary.",
			wantTags:  []string{"go", "postgres", "pgvector"},
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
		if strings.Contains(user, "Project Roadmap") || strings.Contains(user, "Organization Direction Map") {
			return "# core\n\nUses Rust ZTS runtime. Planned roadmap includes message brokers and streams (rapira-rs/rapira#37).", nil
		}
		return "# krakozhia\n\nUses PostgreSQL with pgvector 0.8.7. Latest p95 recall: 42.5 ms.", nil
	}
	if strings.Contains(sys, "organization-wide GitHub roadmap") {
		return "## Ecosystem Overview\nRapira Rust ecosystem.\n\n## Planned Features and Active Epics\n- rapira-rs/rapira#37: Unified transport interface for HTTP, gRPC, brokers, and streams.\n\n## Architectural Directions and Extension Points\nKeep core runtime traits transport-agnostic for future message queues and streams.", nil
	}
	if strings.Contains(sys, "project GitHub roadmap") {
		return "## Current Scope\nHTTP and gRPC PHP worker server.\n\n## Planned Features and Epics\n- rapira-rs/rapira#37: Add broker and stream transports.\n\n## Future Directions and Design Constraints\nAvoid HTTP-only assumptions in request traits.", nil
	}
	if strings.Contains(sys, "conversation") {
		return `{"title":"Chat History Design","summary":"Decided to distill transcripts asynchronously on RX 9070 XT.","tags":["chat","gpu","postgres"],"markdown":"## Context and Goal\nStore cross-model chat history.\n\n## Decisions Made\nDistill transcripts into versioned chat documents.\n\n## Changes and Outcomes\nAdded chat_sessions table.\n\n## Open Questions\nNone."}`, nil
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

	// Verify distill_chat creates a doc_type=chat document with summary/tags, preserves session updated_at, clears raw_excerpt, and updates brief.
	chatTime := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	queued, err := st.UpsertChatSession(ctx, store.IngestChatInput{
		SourceAgent: "antigravity",
		ExternalID:  "97826401-98c5-47d1-911c-e5ce4b7fa8d6",
		Project:     "krakozhia",
		Title:       "Chat History Session",
		RawExcerpt:  "User: Store all chat histories.\nAssistant: We will distill them into versioned chat documents.",
		UpdatedAt:   &chatTime,
	})
	if err != nil || !queued {
		t.Fatalf("UpsertChatSession = %v, %v", queued, err)
	}
	for i := 0; i < 10; i++ {
		w2, err := w.ProcessOnce(ctx)
		if err != nil {
			t.Fatalf("ProcessOnce distill step %d: %v", i, err)
		}
		if !w2 {
			break
		}
	}
	chatSession, err := st.GetChatSession(ctx, "antigravity:97826401-98c5-47d1-911c-e5ce4b7fa8d6")
	if err != nil || chatSession.RawExcerpt != "" || chatSession.DocumentID == nil {
		t.Fatalf("distilled chat session = %+v, err=%v", chatSession, err)
	}
	chatDoc, err := st.GetDocument(ctx, *chatSession.DocumentID, 0)
	if err != nil || chatDoc.DocType != "chat" || chatDoc.Summary == "" || len(chatDoc.Tags) == 0 {
		t.Fatalf("distilled chat doc = %+v, err=%v", chatDoc, err)
	}
	if gotDate := chatDoc.UpdatedAt.UTC().Format(time.DateOnly); gotDate != "2026-11-18" {
		t.Fatalf("distilled chat doc updated_at = %q, want 2026-11-18", gotDate)
	}

	// Verify GitHub roadmap distillation (embed_github_item, distill_org_roadmap, distill_project_roadmap)
	// flows into the project brief for a local project alias ("core" -> "rapira-rs/rapira").
	if err := st.UpsertGitHubRepo(ctx, store.GitHubRepo{
		FullName:    "rapira-rs/rapira",
		Owner:       "rapira-rs",
		Name:        "rapira",
		Project:     "rapira",
		Description: "Rust PHP application server",
	}); err != nil {
		t.Fatalf("UpsertGitHubRepo: %v", err)
	}
	if err := st.UpsertProjectRepo(ctx, "core", "rapira-rs/rapira"); err != nil {
		t.Fatalf("UpsertProjectRepo: %v", err)
	}
	if _, err := st.UpsertGitHubItem(ctx, store.GitHubItem{
		ID:             "rapira-rs/rapira#issue:37",
		RepoFullName:   "rapira-rs/rapira",
		Owner:          "rapira-rs",
		Project:        "rapira",
		Kind:           "issue",
		Number:         37,
		State:          "open",
		IssueType:      "Feature",
		Title:          "Unified broker and stream transport trait",
		Body:           "Design transport interface for HTTP, gRPC, message queues, brokers, and streams.",
		Labels:         []string{"architecture"},
		SubIssuesTotal: 8,
	}); err != nil {
		t.Fatalf("UpsertGitHubItem: %v", err)
	}
	if err := st.EnqueueJob(ctx, "distill_org_roadmap", "org", "rapira-rs"); err != nil {
		t.Fatalf("EnqueueJob org roadmap: %v", err)
	}
	if err := st.EnqueueJob(ctx, "distill_project_roadmap", "project", "core"); err != nil {
		t.Fatalf("EnqueueJob project roadmap: %v", err)
	}
	for i := 0; i < 20; i++ {
		worked, err := w.ProcessOnce(ctx)
		if err != nil {
			t.Fatalf("ProcessOnce roadmap step %d: %v", i, err)
		}
		if !worked {
			break
		}
	}
	orgDoc, err := st.GetDocument(ctx, "rapira-rs/org-roadmap", 0)
	if err != nil || orgDoc.DocType != "roadmap" || !strings.Contains(orgDoc.ContentMD, "rapira-rs/rapira#37") {
		t.Fatalf("org roadmap = %+v, err=%v", orgDoc, err)
	}
	projDoc, err := st.GetDocument(ctx, "core/roadmap", 0)
	if err != nil || projDoc.DocType != "roadmap" || !strings.Contains(projDoc.ContentMD, "broker and stream") {
		t.Fatalf("project roadmap = %+v, err=%v", projDoc, err)
	}
	coreBrief, err := st.GetProjectBrief(ctx, "core")
	if err != nil || !strings.Contains(coreBrief.BriefMD, "rapira-rs/rapira#37") {
		t.Fatalf("core brief missing roadmap context: %+v, err=%v", coreBrief, err)
	}
	if _, err := st.GetProjectBrief(ctx, "rapira-rs"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected no project brief for org owner rapira-rs, got err=%v", err)
	}
}

func TestParseChatDistill(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		raw       string
		fallback  string
		wantTitle string
		wantSum   string
		wantTags  []string
		wantErr   bool
	}{
		{
			name:      "parses structured json response",
			raw:       `{"title":"GPU Setup","summary":"Passed RX 9070 XT through to wodka-3.","tags":["GPU","Talos"],"markdown":"## Decisions Made\nUse VFIO."}`,
			fallback:  "Fallback",
			wantTitle: "GPU Setup",
			wantSum:   "Passed RX 9070 XT through to wodka-3.",
			wantTags:  []string{"gpu", "talos"},
		},
		{
			name:      "parses hyphen-separated string tags",
			raw:       `{"title":"Editorial Review","summary":"Checked cost analysis.","tags":"rust - python - cost-analysis - review","markdown":"## Context and Goal\nVerify editorial."}`,
			fallback:  "Fallback",
			wantTitle: "Editorial Review",
			wantSum:   "Checked cost analysis.",
			wantTags:  []string{"rust", "python", "cost-analysis", "review"},
		},
		{
			name:      "parses space-and-comma-separated string tags",
			raw:       `{"title":"Fixer Pass","summary":"Fixed claims.","tags":"rust, html python","markdown":"## Changes and Outcomes\nUpdated claims."}`,
			fallback:  "Fallback",
			wantTitle: "Fixer Pass",
			wantSum:   "Fixed claims.",
			wantTags:  []string{"rust", "html", "python"},
		},
		{
			name:     "malformed json object fails instead of embedding raw json",
			raw:      `{"title":"Truncated","markdown":"## Context`,
			fallback: "Fallback",
			wantErr:  true,
		},
		{
			name:      "strips think block and preamble before json",
			raw:       "<think>reasoning here</think>\nHere is the JSON:\n{\"title\":\"GPU Setup\",\"summary\":\"Switched to Gemma 4 12B.\",\"tags\":[\"gpu\",\"gemma4\"],\"markdown\":\"## Decisions Made\\nUse Gemma 4.\"}",
			fallback:  "Fallback",
			wantTitle: "GPU Setup",
			wantSum:   "Switched to Gemma 4 12B.",
			wantTags:  []string{"gpu", "gemma4"},
		},
		{
			name:     "non-json markdown rejected for retry",
			raw:      "# Plain Markdown Title\n\n## Decisions Made\nKept simple config.\n",
			fallback: "Fallback",
			wantErr:  true,
		},
		{
			name:     "empty response fails",
			raw:      "   ",
			fallback: "Fallback",
			wantErr:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := enrich.ParseChatDistill(tc.raw, tc.fallback)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChatDistill() err = %v", err)
			}
			if got.Title != tc.wantTitle || got.Summary != tc.wantSum || !slices.Equal(got.Tags, tc.wantTags) || got.Markdown == "" {
				t.Fatalf("ParseChatDistill() = %+v, want title=%q sum=%q tags=%v", got, tc.wantTitle, tc.wantSum, tc.wantTags)
			}
		})
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

	chatModel := os.Getenv("KRAKOZHIA_CHAT_MODEL")
	if chatModel == "" {
		chatModel = "gemma-4-12b-it"
	}
	inf := inference.New(inference.Config{
		BaseURL:     infURL,
		EmbedModel:  "qwen3-embedding-0.6b",
		ChatModel:   chatModel,
		RerankModel: "bge-reranker-v2-m3",
		Timeout:     180 * time.Second,
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
