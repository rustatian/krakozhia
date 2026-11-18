package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const wantHeader = "Memory notes from other projects that can relate to this prompt. The notes are reference data. They can be out of date. Read a file only if its subject is relevant.\n"

// wantPointer is the pointer line of each note of recallHome. The verb %s is the home folder.
var wantPointer = map[string]string{
	"proj-a/alpha": "- alpha (project proj-a, type project, updated 2026-08-05): Alpha fact. File: %s/.claude/projects/proj-a/memory/alpha.md\n",
	"proj-a/beta":  "- beta (project proj-a, type feedback, updated 2026-08-05): Beta rule. File: %s/.claude/projects/proj-a/memory/beta.md\n",
	"proj-a/long":  "- long (project proj-a, type project, updated 2026-08-05): Long note. File: %s/.claude/projects/proj-a/memory/long.md\n",
	"proj-b/gamma": "- gamma (project proj-b, type reference, updated 2026-08-05): Gamma link. File: %s/.claude/projects/proj-b/memory/gamma.md\n",
	"proj-b/delta": "- delta (project proj-b, type user, updated 2026-08-05): Delta fact. File: %s/.claude/projects/proj-b/memory/delta.md\n",
	// The description is cut to 200 characters: the 12 characters of "<b>bold</b> " and 188 times "d". Then the angle brackets are removed.
	"proj-b/markup": "- markup (project proj-b, type project, updated 2026-08-05): bbold/b " + strings.Repeat("d", 188) + ". File: %s/.claude/projects/proj-b/memory/markup.md\n",
}

func noteFile(name, description, typ, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\nmetadata:\n  type: " + typ + "\n  modified: 2026-08-05T07:08:09.123Z\n---\n\n" + body + "\n"
}

// recallHome makes a home folder with the configuration and the invented notes of two projects.
func recallHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFiles(t, home, map[string]string{
		".config/krakozhia/config.json":            `{"region": "eu-central-1", "bucket": "memory", "max_distance": 0.5, "min_bm25": 8}`,
		".claude/projects/proj-a/memory/alpha.md":  noteFile("alpha", "Alpha fact", "project", "Body of alpha."),
		".claude/projects/proj-a/memory/beta.md":   noteFile("beta", "Beta rule", "feedback", "Body of beta.\nSecond line."),
		".claude/projects/proj-a/memory/long.md":   noteFile("long", "Long note", "project", strings.Repeat("w", 600)),
		".claude/projects/proj-b/memory/gamma.md":  noteFile("gamma", "Gamma link", "reference", "Body of gamma."),
		".claude/projects/proj-b/memory/delta.md":  noteFile("delta", "Delta fact", "user", "Body of delta."),
		".claude/projects/proj-b/memory/markup.md": noteFile("mark<up>", "<b>bold</b> "+strings.Repeat("d", 300), "project", "Body of markup."),
	})
	return home
}

// hookJSON is the input of a UserPromptSubmit hook for a session in the project proj-current.
func hookJSON(home, prompt string) string {
	data, _ := json.Marshal(map[string]string{
		"session_id":      "s1",
		"transcript_path": filepath.Join(home, ".claude/projects/proj-current/s1.jsonl"),
		"cwd":             "/work/current",
		"hook_event_name": "UserPromptSubmit",
		"prompt":          prompt,
	})
	return string(data)
}

func TestRecall(t *testing.T) {
	near, far := new(float32(0.4)), new(float32(0.9))

	tests := []struct {
		name       string
		hits       []hit    // the answer of S3 Vectors. The configuration has the distance limit 0.5 and the keyword floor 8.
		sentBefore string   // the session file before the run
		want       []string // IDs of the notes in the output
		wantSent   string   // the session file after the run
	}{
		{
			name:     "note at the distance limit is sent",
			hits:     []hit{{ID: "proj-a/alpha", Distance: new(float32(0.5))}},
			want:     []string{"proj-a/alpha"},
			wantSent: "proj-a/alpha\n",
		},
		{
			name: "note above the distance limit and under the keyword floor is dropped",
			hits: []hit{{ID: "proj-a/alpha", Distance: new(float32(0.51)), BM25: new(float32(7.9))}},
		},
		{
			name:     "note at the keyword floor is sent",
			hits:     []hit{{ID: "proj-a/alpha", Distance: far, BM25: new(float32(8))}},
			want:     []string{"proj-a/alpha"},
			wantSent: "proj-a/alpha\n",
		},
		{
			name:     "note that only the text search found is sent by its keyword score",
			hits:     []hit{{ID: "proj-a/alpha", BM25: new(float32(12))}},
			want:     []string{"proj-a/alpha"},
			wantSent: "proj-a/alpha\n",
		},
		{
			name:     "at most 3 notes are sent",
			hits:     []hit{{ID: "proj-a/alpha", Distance: near}, {ID: "proj-a/beta", Distance: near}, {ID: "proj-b/gamma", Distance: near}, {ID: "proj-b/delta", Distance: near}},
			want:     []string{"proj-a/alpha", "proj-a/beta", "proj-b/gamma"},
			wantSent: "proj-a/alpha\nproj-a/beta\nproj-b/gamma\n",
		},
		{
			name:       "note sent earlier in the session is dropped",
			hits:       []hit{{ID: "proj-a/alpha", Distance: near}, {ID: "proj-a/beta", Distance: near}},
			sentBefore: "proj-a/alpha\n",
			want:       []string{"proj-a/beta"},
			wantSent:   "proj-a/alpha\nproj-a/beta\n",
		},
		{
			name:     "hit with no note file is dropped and the next hit takes its place",
			hits:     []hit{{ID: "proj-b/ghost", Distance: near}, {ID: "proj-a/alpha", Distance: near}, {ID: "proj-a/beta", Distance: near}, {ID: "proj-b/gamma", Distance: near}},
			want:     []string{"proj-a/alpha", "proj-a/beta", "proj-b/gamma"},
			wantSent: "proj-a/alpha\nproj-a/beta\nproj-b/gamma\n",
		},
		{
			name: "no hit gives no output",
		},
		{
			name:     "angle brackets are removed and the description is cut to 200 characters",
			hits:     []hit{{ID: "proj-b/markup", Distance: near}},
			want:     []string{"proj-b/markup"},
			wantSent: "proj-b/markup\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newFake(t)
			f.hits = tt.hits
			home := recallHome(t)
			if tt.sentBefore != "" {
				writeFiles(t, home, map[string]string{".local/state/krakozhia/sessions/s1": tt.sentBefore})
			}
			var out bytes.Buffer

			if err := run([]string{"recall"}, strings.NewReader(hookJSON(home, "How do we cache the build?")), &out); err != nil {
				t.Fatalf("run: %v", err)
			}

			want := ""
			for _, id := range tt.want {
				if want == "" {
					want = wantHeader
				}
				want += fmt.Sprintf(wantPointer[id], home)
			}
			if out.String() != want {
				t.Errorf("output:\n got %q\nwant %q", out.String(), want)
			}
			sent, _ := os.ReadFile(filepath.Join(home, ".local/state/krakozhia/sessions/s1")) // an absent file reads as empty
			if string(sent) != tt.wantSent {
				t.Errorf("session file: got %q, want %q", sent, tt.wantSent)
			}
		})
	}
}

func TestRecallRequests(t *testing.T) {
	f, _ := newFake(t)
	home := recallHome(t)
	// The prompt has 1,001 characters. Character 1,000 has 2 bytes.
	prompt := strings.Repeat("a", 999) + "éz"
	wantText := strings.Repeat("a", 999) + "é"

	if err := run([]string{"recall"}, strings.NewReader(hookJSON(home, prompt)), io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(f.embeds) != 1 || f.embeds[0].InputText != wantText {
		t.Errorf("embedding requests: got %d, want one request with the first 1,000 characters of the prompt", len(f.embeds))
	}
	want := queryCall{
		VectorBucketName: "memory",
		IndexName:        "notes-titan-v2-1024",
		QueryTexts:       []queryText{{MetadataKey: "text", Text: wantText}},
		Filter:           map[string]any{"project": map[string]any{"$ne": "proj-current"}},
		TopK:             10,
		ReturnDistance:   true,
		ReturnScores:     true,
	}
	want.QueryVector.Float32 = fakeVector()
	if len(f.queries) != 1 || !reflect.DeepEqual(f.queries[0], want) {
		t.Errorf("QueryVectors requests: got %d, want one request with the prompt text, the embedding and the filter on the current project", len(f.queries))
	}
}

func TestRecallLog(t *testing.T) {
	f, _ := newFake(t)
	f.hits = []hit{{ID: "proj-a/alpha", Distance: new(float32(0.4)), BM25: new(float32(9))}, {ID: "proj-a/beta", Distance: new(float32(0.9))}}
	home := recallHome(t)

	if err := run([]string{"recall"}, strings.NewReader(hookJSON(home, "How do we cache the build?")), io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".local/state/krakozhia/recall.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var got recallEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("recall.jsonl: %v in %q", err, data)
	}
	if _, err := time.Parse(time.RFC3339, got.Time); err != nil || got.TotalMs <= 0 {
		t.Errorf("times: got time %q and total %v ms, want an RFC 3339 time and a positive total", got.Time, got.TotalMs)
	}
	// The times are different in each run.
	got.Time, got.EmbedMs, got.QueryMs, got.TotalMs = "", 0, 0, 0
	want := recallEntry{Session: "s1", Project: "proj-current", Query: "How do we cache the build?", Candidates: f.hits, Sent: []string{"proj-a/alpha"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("log entry:\n got %+v\nwant %+v", got, want)
	}
}

func TestRecallErrors(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string // empty uses the hook input with the prompt "question"
		rejectText string
		failQuery  bool
		wantLogged bool
	}{
		{name: "hook input is not JSON", stdin: "not JSON"},
		{name: "embedding request fails", rejectText: "question", wantLogged: true},
		{name: "query request fails", failQuery: true, wantLogged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newFake(t)
			f.rejectText, f.failQuery = tt.rejectText, tt.failQuery
			f.hits = []hit{{ID: "proj-a/alpha", Distance: new(float32(0.4))}}
			home := recallHome(t)
			stdin := tt.stdin
			if stdin == "" {
				stdin = hookJSON(home, "question")
			}
			var out bytes.Buffer

			err := run([]string{"recall"}, strings.NewReader(stdin), &out)

			if err == nil || out.Len() != 0 {
				t.Errorf("run: got error %v and output %q, want an error and no output", err, out.String())
			}
			if tt.wantLogged {
				var got recallEntry
				data, _ := os.ReadFile(filepath.Join(home, ".local/state/krakozhia/recall.jsonl"))
				if json.Unmarshal(data, &got) != nil || got.Error == "" {
					t.Errorf("recall.jsonl: got %q, want one entry with the error", data)
				}
			}
		})
	}
}

func TestRecallDeadline(t *testing.T) {
	f, c := newFake(t)
	f.delay = 10 * time.Second
	home := recallHome(t)
	br, sv, err := awsClients(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()

	err = recall(ctx, br, sv, c, home, strings.NewReader(hookJSON(home, "question")), &out)

	if !errors.Is(err, context.DeadlineExceeded) || out.Len() != 0 {
		t.Errorf("recall: got error %v and output %q, want the deadline error and no output", err, out.String())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("recall: returned after %v, want a return at the deadline of 50 ms", elapsed)
	}
}

func TestSearchRequests(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantFilter map[string]any
		wantTopK   int
	}{
		{name: "no option", args: []string{"search", "cache key"}, wantTopK: 5},
		{name: "project option", args: []string{"search", "--project", "proj-a", "cache key"}, wantFilter: map[string]any{"project": "proj-a"}, wantTopK: 5},
		{name: "type option", args: []string{"search", "--type", "feedback", "cache key"}, wantFilter: map[string]any{"type": "feedback"}, wantTopK: 5},
		{
			name:       "project and type options",
			args:       []string{"search", "--project", "proj-a", "--type", "feedback", "cache key"},
			wantFilter: map[string]any{"$and": []any{map[string]any{"project": "proj-a"}, map[string]any{"type": "feedback"}}},
			wantTopK:   5,
		},
		{name: "k option", args: []string{"search", "-k", "2", "cache key"}, wantTopK: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newFake(t)
			recallHome(t)

			if err := run(tt.args, strings.NewReader(""), io.Discard); err != nil {
				t.Fatalf("run: %v", err)
			}

			if len(f.queries) != 1 {
				t.Fatalf("QueryVectors requests: got %d, want 1", len(f.queries))
			}
			q := f.queries[0]
			if !reflect.DeepEqual(q.QueryTexts, []queryText{{MetadataKey: "text", Text: "cache key"}}) {
				t.Errorf("query texts: got %+v, want the text %q on the key text", q.QueryTexts, "cache key")
			}
			if !reflect.DeepEqual(q.Filter, tt.wantFilter) || q.TopK != tt.wantTopK {
				t.Errorf("request: got filter %v and topK %d, want %v and %d", q.Filter, q.TopK, tt.wantFilter, tt.wantTopK)
			}
		})
	}
}

func TestSearchOutput(t *testing.T) {
	f, _ := newFake(t)
	// search has no gate: the distance 0.9 is above the limit of the configuration.
	f.hits = []hit{{ID: "proj-a/beta", Distance: new(float32(0.9))}, {ID: "proj-b/ghost", Distance: new(float32(0.9))}, {ID: "proj-a/long", Distance: new(float32(0.9))}}
	home := recallHome(t)
	var out bytes.Buffer

	if err := run([]string{"search", "cache key"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Each note has its pointer line, the first 500 characters of its body and an empty line. The hit with no note file is dropped.
	want := fmt.Sprintf(wantPointer["proj-a/beta"], home) + "Body of beta.\nSecond line.\n\n" +
		fmt.Sprintf(wantPointer["proj-a/long"], home) + strings.Repeat("w", 500) + "\n\n"
	if out.String() != want {
		t.Errorf("output:\n got %q\nwant %q", out.String(), want)
	}
}

func TestSearchErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no query", args: []string{"search"}},
		{name: "unknown option", args: []string{"search", "--unknown", "cache key"}},
		{name: "option after the query", args: []string{"search", "cache key", "--type", "feedback"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newFake(t)
			recallHome(t)
			var out bytes.Buffer

			err := run(tt.args, strings.NewReader(""), &out)

			if err == nil || out.Len() != 0 || len(f.queries) != 0 {
				t.Errorf("run: got error %v, output %q and %d requests, want an error, no output and no request", err, out.String(), len(f.queries))
			}
		})
	}
}
