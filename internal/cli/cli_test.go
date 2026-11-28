package cli_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/cli"
	"github.com/rustatian/krakozhia/internal/githubsync"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

type fakeServerState struct {
	hashes         map[string]string
	chatHashes     map[string]string
	ingestedChats  []server.IngestChatsRequest
	syncedBatches  []server.SyncNotesRequest
	searchRequests []server.SearchRequest
	searchResponse server.SearchResponse
	savedDocs      []server.SaveDocumentRequest
	recordedBench  []store.RecordBenchmarkInput
	brief          store.ProjectBrief
	docResp        server.GetDocumentResponse
	noteResp       store.Note
	listDocs       []store.Document
	benchRows      []store.Benchmark
	ghSyncResp     githubsync.SyncSummary
	ghSyncCalls    int
}

func newTestAPIServer(t *testing.T, token string, st *fakeServerState) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/search":
			var req server.SearchRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			st.searchRequests = append(st.searchRequests, req)
			_ = json.NewEncoder(w).Encode(st.searchResponse)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/notes/hashes":
			_ = json.NewEncoder(w).Encode(map[string]any{"hashes": st.hashes})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/notes/sync":
			var req server.SyncNotesRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			st.syncedBatches = append(st.syncedBatches, req)
			_ = json.NewEncoder(w).Encode(server.SyncNotesResponse{
				Written: len(req.Notes),
				Deleted: len(req.DeleteIDs),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/chats/hashes":
			_ = json.NewEncoder(w).Encode(map[string]any{"hashes": st.chatHashes})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chats/ingest":
			var req server.IngestChatsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			st.ingestedChats = append(st.ingestedChats, req)
			_ = json.NewEncoder(w).Encode(server.IngestChatsResponse{
				Queued: len(req.Chats),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/github/sync":
			st.ghSyncCalls++
			_ = json.NewEncoder(w).Encode(st.ghSyncResp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/documents":
			var req server.SaveDocumentRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			st.savedDocs = append(st.savedDocs, req)
			_ = json.NewEncoder(w).Encode(store.SaveDocumentResult{
				ID:         req.Project + "/" + req.Name,
				Revision:   2,
				ChunkCount: 3,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/documents":
			_ = json.NewEncoder(w).Encode(map[string]any{"documents": st.listDocs})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/documents/get":
			_ = json.NewEncoder(w).Encode(st.docResp)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/notes/get":
			_ = json.NewEncoder(w).Encode(st.noteResp)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/benchmarks":
			var in store.RecordBenchmarkInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			st.recordedBench = append(st.recordedBench, in)
			_ = json.NewEncoder(w).Encode(store.Benchmark{
				ID:         11,
				Project:    in.Project,
				Suite:      in.Suite,
				Metric:     in.Metric,
				Value:      in.Value,
				Unit:       in.Unit,
				Status:     in.Status,
				RecordedAt: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC),
			})
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/benchmarks" || r.URL.Path == "/api/v1/benchmarks/compare"):
			_ = json.NewEncoder(w).Encode(map[string]any{"benchmarks": st.benchRows})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/briefs":
			_ = json.NewEncoder(w).Encode(st.brief)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func setupTestEnv(t *testing.T, baseURL, token string) (cfgPath, stateDir, projectsDir string) {
	t.Helper()
	tmp := t.TempDir()
	cfgPath = filepath.Join(tmp, "config.json")
	stateDir = filepath.Join(tmp, "state")
	projectsDir = filepath.Join(tmp, "projects")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{
		"url":            baseURL,
		"token":          token,
		"max_distance":   0.45,
		"min_text_score": 0.10,
		"max_pointers":   3,
		"source_agent":   "claude",
	})
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("KRAKOZHIA_CONFIG", cfgPath)
	t.Setenv("KRAKOZHIA_STATE_DIR", stateDir)
	t.Setenv("KRAKOZHIA_PROJECTS_DIR", projectsDir)
	return cfgPath, stateDir, projectsDir
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		envTok  string
		wantErr bool
	}{
		{
			name:    "valid config applies defaults",
			json:    `{"token":"tok-123"}`,
			wantErr: false,
		},
		{
			name:    "rejects unknown field",
			json:    `{"token":"tok-123","region":"eu-central-1"}`,
			wantErr: true,
		},
		{
			name:    "requires token when env empty",
			json:    `{"url":"https://kb.lab.rustatian.me"}`,
			wantErr: true,
		},
		{
			name:    "accepts token from env",
			json:    `{"url":"https://kb.lab.rustatian.me"}`,
			envTok:  "env-tok",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			cfgPath := filepath.Join(tmp, "config.json")
			if err := os.WriteFile(cfgPath, []byte(tc.json), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("KRAKOZHIA_CONFIG", cfgPath)
			t.Setenv("KRAKOZHIA_API_TOKEN", tc.envTok)

			cfg, err := cli.LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Fatalf("LoadConfig error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && (cfg.URL != cli.DefaultURL || cfg.MaxDistance != cli.DefaultMaxDistance || cfg.MinTextScore != cli.DefaultMinTextScore) {
				t.Fatalf("defaults mismatch: %+v", cfg)
			}
		})
	}
}

func TestRecallHookBehavior(t *testing.T) {
	dPass := 0.30
	dFail := 0.80
	tPass := 0.25
	tFail := 0.01
	updated := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	st := &fakeServerState{
		searchResponse: server.SearchResponse{
			Hits: []store.SearchHit{
				{
					Kind:        "note",
					ID:          "-other/gpu",
					Project:     "-other",
					Type:        "reference",
					Title:       "GPU <Alert>",
					Description: "Check reset <script>",
					UpdatedAt:   updated,
					Distance:    &dPass,
					TextScore:   &tFail,
					Score:       0.03,
				},
				{
					Kind:        "doc",
					ID:          "krakozhia/arch#0",
					DocumentID:  "krakozhia/arch",
					Project:     "krakozhia",
					Type:        "spec",
					Title:       "Krakozhia <Spec>",
					HeadingPath: "Architecture > Storage",
					UpdatedAt:   updated,
					Distance:    &dFail,
					TextScore:   &tPass,
					Score:       0.02,
				},
				{
					Kind:      "note",
					ID:        "-other/noisy",
					Project:   "-other",
					Type:      "reference",
					Title:     "Irrelevant",
					UpdatedAt: updated,
					Distance:  &dFail,
					TextScore: &tFail,
					Score:     0.01,
				},
			},
			EmbedMs: 12,
			QueryMs: 5,
		},
	}
	srv := newTestAPIServer(t, "secret-tok", st)
	defer srv.Close()

	_, stateDir, projectsDir := setupTestEnv(t, srv.URL, "secret-tok")
	noteFile := filepath.Join(projectsDir, "-other", "memory", "gpu.md")
	if err := os.MkdirAll(filepath.Dir(noteFile), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(noteFile, []byte("---\nname: gpu\ntype: reference\n---\nbody\n"), 0o600); err != nil {
		t.Fatalf("write note: %v", err)
	}

	cases := []struct {
		name           string
		input          string
		wantCalled     bool
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:       "skips short prompt under 15 runes",
			input:      `{"session_id":"s1","transcript_path":"/home/valery/.claude/projects/-curr/abc.jsonl","prompt":"hello there"}`,
			wantCalled: false,
		},
		{
			name:       "skips slash command",
			input:      `{"session_id":"s1","transcript_path":"/home/valery/.claude/projects/-curr/abc.jsonl","prompt":"/compact the conversation now"}`,
			wantCalled: false,
		},
		{
			name:       "returns gated pointers, strips angle brackets, excludes irrelevant hit",
			input:      `{"session_id":"s2","transcript_path":"/home/valery/.claude/projects/-curr/abc.jsonl","prompt":"How does RX 9070 XT reset work with pgvector storage?"}`,
			wantCalled: true,
			wantContains: []string{
				"GPU Alert",
				"Check reset script",
				"File: " + noteFile,
				"Krakozhia Spec: Architecture Storage",
				"Read: krakozhia show krakozhia/arch#0",
			},
			wantNotContain: []string{
				"<Alert>",
				"<script>",
				"<Spec>",
				"Irrelevant",
			},
		},
		{
			name:       "deduplicates previously sent IDs in same session",
			input:      `{"session_id":"s2","transcript_path":"/home/valery/.claude/projects/-curr/abc.jsonl","prompt":"Check RX 9070 XT reset and pgvector again please."}`,
			wantCalled: true,
			wantNotContain: []string{
				"GPU Alert",
				"Krakozhia Spec",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(st.searchRequests)
			var out, errBuf bytes.Buffer
			err := cli.Run([]string{"recall"}, strings.NewReader(tc.input), &out, &errBuf)
			if err != nil {
				t.Fatalf("recall returned err: %v (stderr=%s)", err, errBuf.String())
			}
			gotCalled := len(st.searchRequests) > before
			if gotCalled != tc.wantCalled {
				t.Fatalf("search called = %v, want %v", gotCalled, tc.wantCalled)
			}
			gotOut := out.String()
			for _, sub := range tc.wantContains {
				if !strings.Contains(gotOut, sub) {
					t.Fatalf("stdout missing %q:\n%s", sub, gotOut)
				}
			}
			for _, sub := range tc.wantNotContain {
				if strings.Contains(gotOut, sub) {
					t.Fatalf("stdout unexpectedly contained %q:\n%s", sub, gotOut)
				}
			}
		})
	}

	logData, err := os.ReadFile(filepath.Join(stateDir, "recall.jsonl"))
	if err != nil {
		t.Fatalf("read recall.jsonl: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(string(logData)), "\n"); len(lines) != len(cases) {
		t.Fatalf("recall.jsonl lines = %d, want %d", len(lines), len(cases))
	}
}

func TestSyncDiffingAndDeletes(t *testing.T) {
	st := &fakeServerState{
		hashes: map[string]string{
			"-proj/unchanged": "",
			"-proj/deleted":   "old-hash",
			"-proj/mcp-only":  "remote-hash",
		},
	}
	srv := newTestAPIServer(t, "secret-tok", st)
	defer srv.Close()
	_, stateDir, projectsDir := setupTestEnv(t, srv.URL, "secret-tok")

	unchangedBody := "---\nname: unchanged\ndescription: stays same\nmetadata:\n  type: project\n---\nbody\n"
	changedBody := "---\nname: changed\ndescription: needs upload\nmetadata:\n  type: reference\n---\nnew body\n"
	memDir := filepath.Join(projectsDir, "-proj", "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "unchanged.md"), []byte(unchangedBody), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "changed.md"), []byte(changedBody), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Compute real server hash for unchanged by scanning once or running hash matching.
	// Write previous manifest recording -proj/unchanged and -proj/deleted (NOT -proj/mcp-only).
	manifestRaw, _ := json.Marshal(map[string]map[string]string{
		"hashes": {
			"-proj/deleted": "old-hash",
		},
	})
	if err := os.WriteFile(filepath.Join(stateDir, "manifest.json"), manifestRaw, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	// Put an old session file (>7d) and fresh session file to verify pruning.
	sessDir := filepath.Join(stateDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	oldSess := filepath.Join(sessDir, "old-sess")
	newSess := filepath.Join(sessDir, "new-sess")
	_ = os.WriteFile(oldSess, []byte("x\n"), 0o600)
	_ = os.WriteFile(newSess, []byte("x\n"), 0o600)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	_ = os.Chtimes(oldSess, oldTime, oldTime)

	// Run sync once to upload changed + unchanged, delete -proj/deleted, keep -proj/mcp-only.
	var out, errBuf bytes.Buffer
	if err := cli.Run([]string{"sync"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("sync failed: %v (%s)", err, errBuf.String())
	}
	if len(st.syncedBatches) != 1 {
		t.Fatalf("syncedBatches = %d, want 1", len(st.syncedBatches))
	}
	batch := st.syncedBatches[0]
	if len(batch.Notes) != 2 {
		t.Fatalf("uploaded notes = %d, want 2", len(batch.Notes))
	}
	if len(batch.DeleteIDs) != 1 || batch.DeleteIDs[0] != "-proj/deleted" {
		t.Fatalf("deleteIDs = %v, want [-proj/deleted]", batch.DeleteIDs)
	}
	if _, err := os.Stat(oldSess); !os.IsNotExist(err) {
		t.Fatalf("expected old session file pruned")
	}
	if _, err := os.Stat(newSess); err != nil {
		t.Fatalf("expected new session file preserved: %v", err)
	}

	// Update server hashes to match synced notes, then second sync should make zero POSTs.
	for _, n := range batch.Notes {
		// Recompute via manifest saved on disk.
		_ = n
	}
	mBytes, _ := os.ReadFile(filepath.Join(stateDir, "manifest.json"))
	var savedManifest struct {
		Hashes map[string]string `json:"hashes"`
	}
	_ = json.Unmarshal(mBytes, &savedManifest)
	delete(st.hashes, "-proj/deleted")
	for k, v := range savedManifest.Hashes {
		st.hashes[k] = v
	}

	out.Reset()
	if err := cli.Run([]string{"sync"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}
	if len(st.syncedBatches) != 1 {
		t.Fatalf("expected no upload request on clean second sync, got %d batches", len(st.syncedBatches))
	}
}

func TestSaveShowListBenchBriefAndEval(t *testing.T) {
	dHit := 0.20
	dMiss := 0.70
	tHit := 0.30
	tMiss := 0.02
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)

	st := &fakeServerState{
		docResp: server.GetDocumentResponse{
			Kind:     "document",
			Document: &store.Document{ID: "proj/spec", ContentMD: "# Spec\n\nBody.\n"},
		},
		noteResp: store.Note{ID: "-proj/mem", Content: "Note body."},
		listDocs: []store.Document{
			{ID: "proj/spec", DocType: "spec", Title: "My Spec", CurrentRevision: 2, UpdatedAt: now},
		},
		benchRows: []store.Benchmark{
			{ID: 1, Project: "proj", Suite: "eval", Metric: "recall", Value: 0.9, Unit: "ratio", Status: "baseline", RecordedAt: now},
		},
		brief: store.ProjectBrief{Project: "proj", BriefMD: "# proj brief\nReady."},
		searchResponse: server.SearchResponse{
			Hits: []store.SearchHit{
				{Kind: "note", ID: "-p/rel", Distance: &dHit, TextScore: &tHit, UpdatedAt: now},
				{Kind: "note", ID: "-p/other", Distance: &dMiss, TextScore: &tMiss, UpdatedAt: now},
			},
			EmbedMs: 10,
			QueryMs: 5,
		},
	}
	srv := newTestAPIServer(t, "secret-tok", st)
	defer srv.Close()
	_, _, _ = setupTestEnv(t, srv.URL, "secret-tok")

	docFile := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(docFile, []byte("# Design\n\nHello.\n"), 0o600); err != nil {
		t.Fatalf("write doc: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := cli.Run([]string{"save", "--project", "proj", "--name", "spec", "--type", "spec", docFile}, nil, &out, &errBuf); err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.Contains(out.String(), "proj/spec") || len(st.savedDocs) != 1 {
		t.Fatalf("unexpected save output: %q", out.String())
	}

	out.Reset()
	if err := cli.Run([]string{"show", "proj/spec"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("show doc: %v", err)
	}
	if !strings.Contains(out.String(), "# Spec") {
		t.Fatalf("show doc = %q", out.String())
	}

	out.Reset()
	if err := cli.Run([]string{"show", "--note", "-proj/mem"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("show note: %v", err)
	}
	if !strings.Contains(out.String(), "Note body.") {
		t.Fatalf("show note = %q", out.String())
	}

	out.Reset()
	if err := cli.Run([]string{"list", "--project", "proj"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "My Spec") {
		t.Fatalf("list = %q", out.String())
	}

	out.Reset()
	if err := cli.Run([]string{"bench", "record", "--project", "proj", "--suite", "eval", "--metric", "recall", "--value", "0.9", "--unit", "ratio", "--status", "baseline"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("bench record: %v", err)
	}
	if len(st.recordedBench) != 1 || st.recordedBench[0].Status != "baseline" {
		t.Fatalf("recordedBench = %+v", st.recordedBench)
	}

	out.Reset()
	if err := cli.Run([]string{"bench", "compare", "--project", "proj", "--suite", "eval", "--metric", "recall"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("bench compare: %v", err)
	}
	if !strings.Contains(out.String(), "0.9") {
		t.Fatalf("bench compare = %q", out.String())
	}

	out.Reset()
	if err := cli.Run([]string{"brief", "proj"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("brief: %v", err)
	}
	if !strings.Contains(out.String(), "# proj brief") {
		t.Fatalf("brief = %q", out.String())
	}

	testset := filepath.Join(t.TempDir(), "test.jsonl")
	lines := strings.Join([]string{
		`{"prompt":"positive prompt one","project":"-p","relevant":["-p/rel"]}`,
		`{"prompt":"negative prompt one","project":"-p","relevant":[]}`,
	}, "\n") + "\n"
	if err := os.WriteFile(testset, []byte(lines), 0o600); err != nil {
		t.Fatalf("write testset: %v", err)
	}
	out.Reset()
	if err := cli.Run([]string{"eval", testset}, nil, &out, &errBuf); err != nil {
		t.Fatalf("eval: %v (%s)", err, errBuf.String())
	}
	for _, form := range []string{"text", "vector", "hybrid"} {
		if !strings.Contains(out.String(), form) {
			t.Fatalf("eval output missing %q:\n%s", form, out.String())
		}
	}
}

func TestSyncChatsCommand(t *testing.T) {
	st := &fakeServerState{
		chatHashes: map[string]string{},
	}
	srv := newTestAPIServer(t, "secret-tok", st)
	defer srv.Close()

	_, _, projectsDir := setupTestEnv(t, srv.URL, "secret-tok")
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("KRAKOZHIA_PROJECTS_DIR", projectsDir)
	repo := filepath.Join(fakeHome, "projects", "krakozhia")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	projDir := filepath.Join(projectsDir, "-krakozhia")
	_ = os.MkdirAll(projDir, 0o755)

	chatFile := filepath.Join(projDir, "chat-99.jsonl")
	raw := `{"type":"user","sessionId":"chat-99","cwd":"` + repo + `","timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":"Implement automatic chat transcript ingestion for all four clients."}}` + "\n" +
		`{"type":"assistant","sessionId":"chat-99","cwd":"` + repo + `","timestamp":"2026-11-27T10:01:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Added PostgreSQL chat_sessions and async GPU distillation worker."}]}}` + "\n"
	_ = os.WriteFile(chatFile, []byte(raw), 0o600)
	oldTime := time.Now().Add(-15 * time.Minute)
	_ = os.Chtimes(chatFile, oldTime, oldTime)

	var out, errBuf bytes.Buffer
	if err := cli.Run([]string{"sync-chats", "--agent", "claude", "--all"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("sync-chats err = %v (stderr=%s)", err, errBuf.String())
	}
	if len(st.ingestedChats) != 1 || len(st.ingestedChats[0].Chats) != 1 {
		t.Fatalf("expected 1 ingested chat, got %+v", st.ingestedChats)
	}
	if st.ingestedChats[0].Chats[0].Project != "krakozhia" {
		t.Fatalf("project = %q, want krakozhia", st.ingestedChats[0].Chats[0].Project)
	}
}

func TestSyncGitHubCommand(t *testing.T) {
	st := &fakeServerState{
		ghSyncResp: githubsync.SyncSummary{
			Owners:        3,
			ReposScanned:  110,
			ItemsUpserted: 240,
			ItemsChanged:  18,
		},
	}
	srv := newTestAPIServer(t, "secret-tok", st)
	defer srv.Close()
	_, _, _ = setupTestEnv(t, srv.URL, "secret-tok")

	var out, errBuf bytes.Buffer
	if err := cli.Run([]string{"sync-github"}, nil, &out, &errBuf); err != nil {
		t.Fatalf("sync-github err = %v (stderr=%s)", err, errBuf.String())
	}
	if st.ghSyncCalls != 1 || !strings.Contains(out.String(), "owners=3 repos=110 items=240 changed=18 errors=0") {
		t.Fatalf("unexpected sync-github output = %q (calls=%d)", out.String(), st.ghSyncCalls)
	}
}
