package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/chunk"
	"github.com/rustatian/krakozhia/internal/secrets"
	"github.com/rustatian/krakozhia/internal/store"
)

func TestComputeRRF(t *testing.T) {
	t.Parallel()

	// RRF(rank) = 1 / (60 + rank), 1-based rank.
	wantBoth := 1.0/61.0 + 1.0/62.0
	wantVecOnly := 1.0 / 63.0
	wantTextOnly := 1.0 / 61.0

	cases := []struct {
		name     string
		vecRank  int
		textRank int
		want     float64
	}{
		{name: "both ranks present", vecRank: 1, textRank: 2, want: wantBoth},
		{name: "vector rank only", vecRank: 3, textRank: 0, want: wantVecOnly},
		{name: "text rank only", vecRank: 0, textRank: 1, want: wantTextOnly},
		{name: "neither rank", vecRank: 0, textRank: 0, want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := store.ComputeRRF(tc.vecRank, tc.textRank)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("ComputeRRF(%d, %d) = %.12f, want %.12f", tc.vecRank, tc.textRank, got, tc.want)
			}
		})
	}
}

func TestValidateBenchmarkInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      store.RecordBenchmarkInput
		wantErr bool
	}{
		{
			name: "valid baseline",
			in: store.RecordBenchmarkInput{
				Project: "krakozhia",
				Suite:   "recall",
				Metric:  "p95_ms",
				Value:   42.5,
				Unit:    "ms",
				Status:  "baseline",
			},
		},
		{
			name: "missing metric",
			in: store.RecordBenchmarkInput{
				Project: "krakozhia",
				Suite:   "recall",
				Value:   10,
				Unit:    "ms",
				Status:  "info",
			},
			wantErr: true,
		},
		{
			name: "invalid status",
			in: store.RecordBenchmarkInput{
				Project: "krakozhia",
				Suite:   "recall",
				Metric:  "p95_ms",
				Value:   10,
				Unit:    "ms",
				Status:  "broken",
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := store.ValidateBenchmarkInput(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateBenchmarkInput() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func unitVec(dim, hot int, val float32) []float32 {
	v := make([]float32, dim)
	v[hot] = val
	return v
}

func openTestStore(t *testing.T) *store.Store {
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
	schema := "krakozhia_test_" + hex.EncodeToString(raw[:])

	adminStore, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open admin: %v", err)
	}
	if err := adminStore.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		adminStore.Close()
		t.Fatalf("create schema %s: %v", schema, err)
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
	// Verify second Migrate call is idempotent.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	return s
}

func TestStoreDocumentsNotesBenchmarksSearchAndJobs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// 1. Secret scanner blocks document save before writing.
	_, err := s.SaveDocument(ctx, store.SaveDocumentInput{
		Project: "krakozhia",
		Name:    "leaked",
		DocType: "spec",
		Content: "# Secret\n\ntoken=" + "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890\n",
	})
	var secErr *secrets.FindingsError
	if !errors.As(err, &secErr) || len(secErr.Findings) != 1 {
		t.Fatalf("expected FindingsError, got %v", err)
	}

	// 2. Save document revision 1 with two sections.
	v0 := unitVec(1024, 0, 1.0)
	v1 := unitVec(1024, 1, 1.0)
	docRes, err := s.SaveDocument(ctx, store.SaveDocumentInput{
		Project:     "krakozhia",
		Name:        "vector-design",
		DocType:     "spec",
		Title:       "Vector Design",
		Content:     "# Vector Design\n\n## Storage\nPostgreSQL with pgvector extension 0.8.7.\n\n## Retrieval\nHybrid exact cosine and tsvector.\n",
		SourceAgent: "claude",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Vector Design > Storage", Content: "PostgreSQL with pgvector extension 0.8.7."}, Embedding: v0},
			{Section: chunk.Section{Index: 1, HeadingPath: "Vector Design > Retrieval", Content: "Hybrid exact cosine and tsvector."}, Embedding: v1},
		},
	})
	if err != nil {
		t.Fatalf("SaveDocument rev1: %v", err)
	}
	if docRes.Revision != 1 || docRes.ChunkCount != 2 {
		t.Fatalf("unexpected SaveDocument rev1 result: %+v", docRes)
	}

	// Unchanged content skips new revision.
	docSame, err := s.SaveDocument(ctx, store.SaveDocumentInput{
		Project:     "krakozhia",
		Name:        "vector-design",
		DocType:     "spec",
		Title:       "Vector Design",
		Content:     "# Vector Design\n\n## Storage\nPostgreSQL with pgvector extension 0.8.7.\n\n## Retrieval\nHybrid exact cosine and tsvector.\n",
		SourceAgent: "claude",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Vector Design > Storage", Content: "PostgreSQL with pgvector extension 0.8.7."}, Embedding: v0},
			{Section: chunk.Section{Index: 1, HeadingPath: "Vector Design > Retrieval", Content: "Hybrid exact cosine and tsvector."}, Embedding: v1},
		},
	})
	if err != nil || docSame.Revision != 1 {
		t.Fatalf("unchanged SaveDocument = %+v, %v; want Revision 1", docSame, err)
	}

	// Revision 2 shrinks to 1 section; stale chunk 1 must be removed.
	docRev2, err := s.SaveDocument(ctx, store.SaveDocumentInput{
		Project:     "krakozhia",
		Name:        "vector-design",
		DocType:     "spec",
		Title:       "Vector Design",
		Content:     "# Vector Design\n\n## Storage\nPostgreSQL 18.7 with pgvector 0.8.7 and exact scan.\n",
		SourceAgent: "codex",
		Chunks: []store.ChunkInput{
			{Section: chunk.Section{Index: 0, HeadingPath: "Vector Design > Storage", Content: "PostgreSQL 18.7 with pgvector 0.8.7 and exact scan."}, Embedding: v0},
		},
	})
	if err != nil || docRev2.Revision != 2 || docRev2.ChunkCount != 1 {
		t.Fatalf("SaveDocument rev2 = %+v, %v", docRev2, err)
	}

	gotRev1, err := s.GetDocument(ctx, "krakozhia/vector-design", 1)
	if err != nil || gotRev1.Revision != 1 {
		t.Fatalf("GetDocument rev1 = %+v, %v", gotRev1, err)
	}
	gotCur, err := s.GetDocument(ctx, "krakozhia/vector-design", 0)
	if err != nil || gotCur.Revision != 2 {
		t.Fatalf("GetDocument current = %+v, %v", gotCur, err)
	}
	docs, err := s.ListDocuments(ctx, "krakozhia", "spec", 10)
	if err != nil || len(docs) != 1 || docs[0].ID != "krakozhia/vector-design" {
		t.Fatalf("ListDocuments = %+v, %v", docs, err)
	}

	// 3. Notes upsert, list hashes, and delete.
	if err := s.UpsertNote(ctx, store.UpsertNoteInput{
		ID:          "infra/pgvector-note",
		Project:     "infra",
		NoteType:    "reference",
		Name:        "pgvector on app-db",
		Description: "app-db bundles pgvector 0.8.7",
		Content:     "Database krakozhia enables vector 0.8.7 on CloudNativePG.",
		SourceAgent: "claude",
		Embedding:   unitVec(1024, 2, 1.0),
	}); err != nil {
		t.Fatalf("UpsertNote: %v", err)
	}
	hashes, err := s.ListNoteHashes(ctx)
	if err != nil || len(hashes["infra/pgvector-note"]) != 64 {
		t.Fatalf("ListNoteHashes = %+v, %v", hashes, err)
	}

	// 4. Hybrid search across docs and notes, with project exclusion and doc deduplication.
	hits, err := s.SearchKnowledge(ctx, store.SearchQuery{
		Form:               store.FormHybrid,
		Text:               "pgvector 0.8.7",
		QueryVector:        v0,
		ExcludeNoteProject: "other-project",
		Limit:              10,
		DedupeDocs:         true,
	})
	if err != nil {
		t.Fatalf("SearchKnowledge: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits (1 doc chunk + 1 note), got %d: %+v", len(hits), hits)
	}
	if hits[0].Kind != "doc" || hits[0].ID != "krakozhia/vector-design#0" {
		t.Fatalf("expected top hit to be krakozhia/vector-design#0, got %+v", hits[0])
	}

	// Excluding infra notes hides infra/pgvector-note.
	docOnlyHits, err := s.SearchKnowledge(ctx, store.SearchQuery{
		Form:               store.FormText,
		Text:               "pgvector",
		ExcludeNoteProject: "infra",
		Limit:              10,
	})
	if err != nil || len(docOnlyHits) != 1 || docOnlyHits[0].Kind != "doc" {
		t.Fatalf("SearchKnowledge ExcludeNoteProject = %+v, %v", docOnlyHits, err)
	}

	// 5. Benchmarks: baseline + derived delta_pct + compare + latest.
	baseBench, err := s.RecordBenchmark(ctx, store.RecordBenchmarkInput{
		Project:   "krakozhia",
		Suite:     "recall",
		Metric:    "p95_ms",
		Value:     500.0,
		Unit:      "ms",
		GitCommit: "abc1111",
		Branch:    "main",
		Status:    "baseline",
	})
	if err != nil {
		t.Fatalf("RecordBenchmark baseline: %v", err)
	}
	improvedBench, err := s.RecordBenchmark(ctx, store.RecordBenchmarkInput{
		Project:    "krakozhia",
		Suite:      "recall",
		Metric:     "p95_ms",
		Value:      50.0,
		Unit:       "ms",
		BaselineID: &baseBench.ID,
		GitCommit:  "def2222",
		Branch:     "feat/pgvector",
		Status:     "kept",
		DocumentID: &docRes.ID,
	})
	if err != nil {
		t.Fatalf("RecordBenchmark kept: %v", err)
	}
	if improvedBench.DeltaPct == nil || math.Abs(*improvedBench.DeltaPct-(-90.0)) > 1e-9 {
		t.Fatalf("DeltaPct = %v, want -90.0", improvedBench.DeltaPct)
	}

	latest, err := s.QueryBenchmarks(ctx, store.BenchmarkFilter{Project: "krakozhia", LatestOnly: true, Limit: 10})
	if err != nil || len(latest) != 1 || latest[0].ID != improvedBench.ID {
		t.Fatalf("QueryBenchmarks latest = %+v, %v", latest, err)
	}
	cmp, err := s.CompareBenchmarks(ctx, "krakozhia", "recall", "p95_ms", 10)
	if err != nil || len(cmp) != 2 {
		t.Fatalf("CompareBenchmarks = %+v, %v", cmp, err)
	}

	// 6. Enrichment job lifecycle & updates.
	job, err := s.ClaimNextJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("ClaimNextJob = %+v, %v", job, err)
	}
	if err := s.CompleteJob(ctx, job.ID); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	if err := s.UpdateDocumentEnrichment(ctx, "krakozhia/vector-design", "Summary text.", []string{"postgres", "pgvector"}); err != nil {
		t.Fatalf("UpdateDocumentEnrichment: %v", err)
	}
	if err := s.UpdateChunkEnrichment(ctx, "krakozhia/vector-design#0", "Context prefix.", v0); err != nil {
		t.Fatalf("UpdateChunkEnrichment: %v", err)
	}
	if err := s.UpsertProjectBrief(ctx, "krakozhia", "# Brief\nUses pgvector.", "hash123"); err != nil {
		t.Fatalf("UpsertProjectBrief: %v", err)
	}
	brief, err := s.GetProjectBrief(ctx, "krakozhia")
	if err != nil || brief.BriefMD != "# Brief\nUses pgvector." {
		t.Fatalf("GetProjectBrief = %+v, %v", brief, err)
	}
	if err := s.AddRecordLink(ctx, store.RecordLink{
		SourceKind: "doc",
		SourceID:   "krakozhia/vector-design",
		TargetKind: "note",
		TargetID:   "infra/pgvector-note",
		Relation:   "supersedes",
		Note:       "Doc replaces initial note.",
	}); err != nil {
		t.Fatalf("AddRecordLink: %v", err)
	}

	gotEnriched, err := s.GetDocument(ctx, "krakozhia/vector-design", 0)
	if err != nil || gotEnriched.Summary != "Summary text." || !slices.Equal(gotEnriched.Tags, []string{"postgres", "pgvector"}) {
		t.Fatalf("enriched doc = %+v, %v", gotEnriched, err)
	}

	if err := s.DeleteNotes(ctx, []string{"infra/pgvector-note"}); err != nil {
		t.Fatalf("DeleteNotes: %v", err)
	}
	if err := s.DeleteDocument(ctx, "krakozhia/vector-design"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
}
