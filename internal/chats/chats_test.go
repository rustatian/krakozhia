package chats_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rustatian/krakozhia/internal/chats"
	"github.com/rustatian/krakozhia/internal/secrets"
)

func TestResolveProject(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "krakozhia")
	sub := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "rr965-work"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "rr965-work")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "rr965-work")+"\n"), 0o600); err != nil {
		t.Fatalf("write worktree .git: %v", err)
	}

	cases := []struct {
		name string
		ws   string
		want string
	}{
		{name: "git repo root", ws: repo, want: "krakozhia"},
		{name: "nested subdir resolves to git root", ws: sub, want: "krakozhia"},
		{name: "git worktree resolves to main repo root", ws: wt, want: "krakozhia"},
		{name: "file uri resolves to git root", ws: "file://" + repo, want: "krakozhia"},
		{name: "home dir maps to home", ws: home, want: "home"},
		{name: "missing path falls back to basename", ws: "/nonexistent/path/rapira-core", want: "rapira-core"},
		{name: "empty maps to general", ws: "", want: "general"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chats.ResolveProject(tc.ws, home); got != tc.want {
				t.Fatalf("ResolveProject(%q) = %q, want %q", tc.ws, got, tc.want)
			}
		})
	}
}

func TestParseGitHubRemoteURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want string
	}{
		{name: "ssh scp syntax", url: "git@github.com:rapira-rs/rapira.git", want: "rapira-rs/rapira"},
		{name: "https with .git", url: "https://github.com/rustatian/krakozhia.git", want: "rustatian/krakozhia"},
		{name: "https without .git", url: "https://github.com/roadrunner-server/roadrunner", want: "roadrunner-server/roadrunner"},
		{name: "ssh scheme syntax", url: "ssh://git@github.com/roadrunner-server/redis.git", want: "roadrunner-server/redis"},
		{name: "https with embedded credentials stripped", url: "https://user:secret@github.com/rapira-rs/contract.git", want: "rapira-rs/contract"},
		{name: "dot github repo", url: "git@github.com:rapira-rs/.github.git", want: "rapira-rs/.github"},
		{name: "non-github host ignored", url: "git@gitlab.com:group/project.git", want: ""},
		{name: "empty url ignored", url: "", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chats.ParseGitHubRemoteURL(tc.url); got != tc.want {
				t.Fatalf("ParseGitHubRemoteURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestResolveGitRemote(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "core")
	sub := filepath.Join(repo, "src", "runtime")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt-core"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	gitCfg := "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = git@github.com:rapira-rs/rapira.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte(gitCfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	wt := filepath.Join(t.TempDir(), "wt-core")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt-core")+"\n"), 0o600); err != nil {
		t.Fatalf("write wt .git: %v", err)
	}

	cases := []struct {
		name string
		ws   string
		want string
	}{
		{name: "repo root reads origin remote", ws: repo, want: "rapira-rs/rapira"},
		{name: "nested subdir resolves repo origin", ws: sub, want: "rapira-rs/rapira"},
		{name: "worktree follows main repo config", ws: wt, want: "rapira-rs/rapira"},
		{name: "file uri handled", ws: "file://" + repo, want: "rapira-rs/rapira"},
		{name: "home dir returns empty", ws: home, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := chats.ResolveGitRemote(tc.ws, home); got != tc.want {
				t.Fatalf("ResolveGitRemote(%q) = %q, want %q", tc.ws, got, tc.want)
			}
		})
	}
}

func TestFormatTurnsRedactsAndTrims(t *testing.T) {
	t.Parallel()

	secretTok := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	turns := []chats.Turn{
		{Role: "user", Text: "<local-command-caveat>Caveat: local command output</local-command-caveat>"},
		{Role: "user", Text: "<task-notification><status>completed</status></task-notification>"},
		{Role: "user", Text: "# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>follow these rules</INSTRUCTIONS>"},
		{Role: "user", Text: "<environment_context><cwd>/repo</cwd></environment_context>"},
		{Role: "user", Text: "<USER_REQUEST><pasted_content id=\"a296\">Please help debug</pasted_content id=\"a296\"> <system-reminder>ignore me</system-reminder> token " + secretTok + " on wodka-3</USER_REQUEST>"},
		{Role: "assistant", Text: "Checked wodka-3 and fixed the configuration cleanly with verified GPU resets."},
	}
	out, ok := chats.FormatTurns(turns, 20000)
	if !ok {
		t.Fatalf("FormatTurns returned ok=false")
	}
	for _, banned := range []string{"ignore me", "<USER_REQUEST>", "local-command-caveat", "task-notification", "follow these rules", "environment_context", "pasted_content"} {
		if strings.Contains(out, banned) {
			t.Fatalf("wrapper/system content %q not stripped: %q", banned, out)
		}
	}
	if !strings.Contains(out, "Please help debug") {
		t.Fatalf("unwrapped pasted content missing: %q", out)
	}
	if findings := secrets.Scan(out); len(findings) != 0 {
		t.Fatalf("FormatTurns leaked secrets: %+v in %q", findings, out)
	}
	if !strings.Contains(out, "[REDACTED:github-token]") {
		t.Fatalf("expected redaction marker in %q", out)
	}

	// Short/empty sessions are rejected.
	if _, ok := chats.FormatTurns([]chats.Turn{{Role: "user", Text: "hi"}}, 20000); ok {
		t.Fatalf("expected short single-word chat to be skipped")
	}
}

func TestScanClaude(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "krakozhia")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	claudeProj := filepath.Join(home, ".claude", "projects", "-home-projects-krakozhia")
	_ = os.MkdirAll(filepath.Join(claudeProj, "subagents"), 0o755)

	lines := []string{
		`{"type":"ai-title","sessionId":"sess-1","title":"PGVector Setup"}`,
		`{"type":"user","isMeta":true,"isSidechain":false,"sessionId":"sess-1","cwd":` + mustJSON(t, repo) + `,"timestamp":"2026-11-27T09:59:59Z","message":{"role":"user","content":"<local-command-caveat>injected meta caveat</local-command-caveat>"}}`,
		`{"type":"user","isSidechain":false,"sessionId":"sess-1","cwd":` + mustJSON(t, repo) + `,"timestamp":"2026-11-27T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"How do we deploy PostgreSQL with pgvector on the wodka cluster?"}]}}`,
		`{"type":"assistant","isSidechain":false,"sessionId":"sess-1","cwd":` + mustJSON(t, repo) + `,"timestamp":"2026-11-27T10:01:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"internal"},{"type":"text","text":"Enable the vector 0.8.7 extension on the krakozhia CNPG Database resource."}]}}`,
		`{"type":"user","isSidechain":true,"sessionId":"sess-1","timestamp":"2026-11-27T10:02:00Z","message":{"role":"user","content":"sidechain should be skipped"}}`,
	}
	mainFile := filepath.Join(claudeProj, "sess-1.jsonl")
	_ = os.WriteFile(mainFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	// Subagent file must be ignored.
	_ = os.WriteFile(filepath.Join(claudeProj, "subagents", "sub-1.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	// Active file modified just now must be skipped when MinIdle=2m.
	activeFile := filepath.Join(claudeProj, "sess-active.jsonl")
	_ = os.WriteFile(activeFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(mainFile, oldTime, oldTime)

	sessions, err := chats.ScanClaude(filepath.Join(home, ".claude", "projects"), home, chats.ScanOptions{
		MinIdle: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("ScanClaude: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1 (%+v)", len(sessions), sessions)
	}
	s := sessions[0]
	if s.ID != "claude:sess-1" || s.Project != "krakozhia" || s.Title != "PGVector Setup" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if strings.Contains(s.RawExcerpt, "sidechain") || strings.Contains(s.RawExcerpt, "internal") || strings.Contains(s.RawExcerpt, "injected meta caveat") {
		t.Fatalf("sidechain, meta, or thinking leaked: %s", s.RawExcerpt)
	}
	if got := chats.ResolveClaudeFolderProject(filepath.Join(home, ".claude", "projects"), "-home-projects-krakozhia", home); got != "krakozhia" {
		t.Fatalf("ResolveClaudeFolderProject() = %q, want krakozhia", got)
	}
}

func TestScanAntigravity(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "infrastructure")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	agyDir := filepath.Join(home, ".gemini", "antigravity-cli")
	_ = os.MkdirAll(filepath.Join(agyDir, "cache"), 0o755)

	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT, title TEXT, workspace_uris TEXT,
		last_modified_time TEXT, parent_conversation_id TEXT, nesting_depth INTEGER
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	ts := time.Now().UTC().Add(-15 * time.Minute).Format("2006-01-02 15:04:05+00:00")
	wsJSON := mustJSON(t, []string{"file://" + repo})
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES ('conv-top', 'RX 9070 XT Passthrough', ?, ?, '', 0)`, wsJSON, ts)
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES ('conv-internal', 'Internal', ?, ?, '', 0)`, wsJSON, ts)
	_, _ = db.Exec(`INSERT INTO conversation_summaries VALUES ('conv-sub', 'Subagent', ?, ?, 'conv-top', 1)`, wsJSON, ts)
	_ = db.Close()

	meta := `{"conversations":{"conv-top":{"is_internal":false},"conv-internal":{"is_internal":true}}}`
	_ = os.WriteFile(filepath.Join(agyDir, "cache", "conversation_metadata.json"), []byte(meta), 0o600)

	logDir := filepath.Join(agyDir, "brain", "conv-top", ".system_generated", "logs")
	_ = os.MkdirAll(logDir, 0o755)
	transcript := strings.Join([]string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","created_at":"2026-11-27T20:00:00Z","content":"<USER_REQUEST>Let us pass through the RX 9070 XT to wodka-3 via VFIO.</USER_REQUEST>"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","created_at":"2026-11-27T20:01:00Z","thinking":"hidden","content":"Attached PCI 0000:03:00 with vfio-rx9070xt.sh and verified 3 clean VM resets."}`,
	}, "\n") + "\n"
	_ = os.WriteFile(filepath.Join(logDir, "transcript.jsonl"), []byte(transcript), 0o600)

	sessions, err := chats.ScanAntigravity(agyDir, home, chats.ScanOptions{MinIdle: 2 * time.Minute})
	if err != nil {
		t.Fatalf("ScanAntigravity: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1 (%+v)", len(sessions), sessions)
	}
	s := sessions[0]
	if s.ID != "antigravity:conv-top" || s.Project != "infrastructure" || s.Title != "RX 9070 XT Passthrough" {
		t.Fatalf("unexpected session: %+v", s)
	}
	if !strings.Contains(s.RawExcerpt, "vfio-rx9070xt.sh") || strings.Contains(s.RawExcerpt, "hidden") {
		t.Fatalf("unexpected excerpt: %s", s.RawExcerpt)
	}
}

func TestScanCodex(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "rapira-core")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	sessDir := filepath.Join(home, ".codex", "sessions", "2026", "11", "27")
	_ = os.MkdirAll(sessDir, 0o755)

	userRollout := strings.Join([]string{
		`{"timestamp":"2026-11-27T12:00:00Z","type":"session_meta","payload":{"id":"codex-1","cwd":` + mustJSON(t, repo) + `,"source":"vscode","thread_source":"user"}}`,
		`{"timestamp":"2026-11-27T12:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"system developer prompt"}]}}`,
		`{"timestamp":"2026-11-27T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>boilerplate rules</INSTRUCTIONS>"}]}}`,
		`{"timestamp":"2026-11-27T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/repo</cwd></environment_context>"}]}}`,
		`{"timestamp":"2026-11-27T12:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Optimize gRPC frame allocation in rapira-core worker pool."}]}}`,
		`{"timestamp":"2026-11-27T12:00:10Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Reused per-worker frame buffers and cut allocations per request by 40 percent."}]}}`,
	}, "\n") + "\n"
	subRollout := `{"timestamp":"2026-11-27T12:00:00Z","type":"session_meta","payload":{"id":"codex-sub","cwd":` + mustJSON(t, repo) + `,"source":{"subagent":"x"},"thread_source":"subagent"}}` + "\n"

	f1 := filepath.Join(sessDir, "rollout-2026-11-27-codex-1.jsonl")
	f2 := filepath.Join(sessDir, "rollout-2026-11-27-codex-sub.jsonl")
	_ = os.WriteFile(f1, []byte(userRollout), 0o600)
	_ = os.WriteFile(f2, []byte(subRollout), 0o600)
	oldTime := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(f1, oldTime, oldTime)
	_ = os.Chtimes(f2, oldTime, oldTime)

	sessions, err := chats.ScanCodex(filepath.Join(home, ".codex"), home, chats.ScanOptions{MinIdle: 2 * time.Minute})
	if err != nil {
		t.Fatalf("ScanCodex: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "codex:codex-1" || sessions[0].Project != "rapira-core" {
		t.Fatalf("unexpected codex sessions: %+v", sessions)
	}
	if sessions[0].Title != "Optimize gRPC frame allocation in rapira-core worker pool." {
		t.Fatalf("unexpected codex title: %q", sessions[0].Title)
	}
	if strings.Contains(sessions[0].RawExcerpt, "system developer prompt") || strings.Contains(sessions[0].RawExcerpt, "boilerplate rules") {
		t.Fatalf("developer or AGENTS.md boilerplate leaked: %s", sessions[0].RawExcerpt)
	}
}

func TestScanOpenCode(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	repo := filepath.Join(home, "projects", "krakozhia")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	ocDir := filepath.Join(home, ".local", "share", "opencode")
	_ = os.MkdirAll(ocDir, 0o755)

	db, err := sql.Open("sqlite", filepath.Join(ocDir, "opencode.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE session_v2 (id TEXT, parent_id TEXT, directory TEXT, title TEXT, time_created INTEGER, time_updated INTEGER);
		CREATE TABLE session_message (id TEXT, session_id TEXT, type TEXT, seq INTEGER, data TEXT);
		CREATE TABLE session_part (message_id TEXT, session_id TEXT, ordinal INTEGER, type TEXT, data TEXT);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	createdMs := time.Now().Add(-20 * time.Minute).UnixMilli()
	updatedMs := time.Now().Add(-10 * time.Minute).UnixMilli()
	_, _ = db.Exec(`INSERT INTO session_v2 VALUES ('oc-1', '', ?, 'OpenCode Review', ?, ?)`, repo, createdMs, updatedMs)
	_, _ = db.Exec(`INSERT INTO session_v2 VALUES ('oc-child', 'oc-1', ?, 'Subtask', ?, ?)`, repo, createdMs, updatedMs)
	_, _ = db.Exec(`INSERT INTO session_message VALUES ('m1', 'oc-1', 'user', 1, '{"text":"Review the OR lexeme query in internal/store/search.go for stopword issues."}')`)
	// Modern OpenCode: empty data.content, visible text in session_part.
	_, _ = db.Exec(`INSERT INTO session_message VALUES ('m2', 'oc-1', 'assistant', 2, '{"content":[]}')`)
	_, _ = db.Exec(`INSERT INTO session_part VALUES ('m2', 'oc-1', 0, 'text', '{"type":"text","text":"Filtered English stopwords before building the simple OR tsquery."}')`)
	_ = db.Close()

	sessions, err := chats.ScanOpenCode(ocDir, home, chats.ScanOptions{MinIdle: 2 * time.Minute})
	if err != nil {
		t.Fatalf("ScanOpenCode: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "opencode:oc-1" || sessions[0].Project != "krakozhia" {
		t.Fatalf("unexpected opencode sessions: %+v", sessions)
	}
	if !strings.Contains(sessions[0].RawExcerpt, "Filtered English stopwords") {
		t.Fatalf("missing session_part assistant text: %s", sessions[0].RawExcerpt)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
