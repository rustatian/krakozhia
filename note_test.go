package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseNote(t *testing.T) {
	fileTime := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	const path = "/home/u/.claude/projects/-home-u-proj/memory/build-cache.md"

	tests := []struct {
		name            string
		content         string
		wantType        string
		wantDescription string
		wantUpdated     int64
	}{
		{
			name:            "plain description",
			content:         "---\nname: build-cache\ndescription: The CI cache key uses the hash of go.sum\nmetadata:\n  type: project\n---\n\nBody.\n",
			wantType:        "project",
			wantDescription: "The CI cache key uses the hash of go.sum",
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "double-quoted description with an escaped quote",
			content:         "---\nname: build-cache\ndescription: \"Run \\\"go vet\\\" before each commit\"\nmetadata:\n  type: feedback\n---\n\nBody.\n",
			wantType:        "feedback",
			wantDescription: `Run "go vet" before each commit`,
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "plain description with a colon and a space",
			content:         "---\nname: build-cache\ndescription: Result of the cache test: the warm build takes 4 minutes\nmetadata:\n  type: project\n---\n\nBody.\n",
			wantType:        "project",
			wantDescription: "Result of the cache test: the warm build takes 4 minutes",
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "plain description with a space and a hash",
			content:         "---\nname: build-cache\ndescription: The fix is in build 12 #flaky\nmetadata:\n  type: project\n---\n\nBody.\n",
			wantType:        "project",
			wantDescription: "The fix is in build 12 #flaky",
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "metadata line with a trailing space",
			content:         "---\nname: build-cache\ndescription: A rule\nmetadata: \n  type: user\n---\n\nBody.\n",
			wantType:        "user",
			wantDescription: "A rule",
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "modified with milliseconds",
			content:         "---\nname: build-cache\ndescription: A fact\nmetadata:\n  node_type: memory\n  type: reference\n  originSessionId: 0b0e7c1a\n  modified: 2026-08-05T07:08:09.123Z\n---\n\nBody.\n",
			wantType:        "reference",
			wantDescription: "A fact",
			wantUpdated:     time.Date(2026, 8, 5, 7, 8, 9, 0, time.UTC).Unix(),
		},
		{
			name:            "modified with a date only",
			content:         "---\nname: build-cache\ndescription: A fact\nmetadata:\n  type: project\n  modified: 2026-07-14\n---\n\nBody.\n",
			wantType:        "project",
			wantDescription: "A fact",
			wantUpdated:     time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC).Unix(),
		},
		{
			name:            "modified in an unknown format uses the file time",
			content:         "---\nname: build-cache\ndescription: A fact\nmetadata:\n  type: project\n  modified: yesterday\n---\n\nBody.\n",
			wantType:        "project",
			wantDescription: "A fact",
			wantUpdated:     fileTime.Unix(),
		},
		{
			name:            "key in the body is not front matter",
			content:         "---\nname: build-cache\ndescription: A fact\nmetadata:\n  type: project\n---\n\ndescription: body text\n  type: user\n",
			wantType:        "project",
			wantDescription: "A fact",
			wantUpdated:     fileTime.Unix(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := note{
				ID:          "-home-u-proj/build-cache",
				Project:     "-home-u-proj",
				Type:        tt.wantType,
				Name:        "build-cache",
				Description: tt.wantDescription,
				Updated:     tt.wantUpdated,
				Text:        tt.content,
			}
			got, err := parseNote(path, []byte(tt.content), fileTime)
			if err != nil {
				t.Fatalf("parseNote: %v", err)
			}
			if got != want {
				t.Errorf("parseNote:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestParseNoteErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty file", content: ""},
		{name: "no front matter", content: "# Title\n\nText.\n"},
		{name: "front matter with no closing line", content: "---\nname: a\ndescription: b\nmetadata:\n  type: project\n\nBody.\n"},
		{name: "no name", content: "---\ndescription: b\nmetadata:\n  type: project\n---\n\nBody.\n"},
		{name: "no description", content: "---\nname: a\nmetadata:\n  type: project\n---\n\nBody.\n"},
		{name: "no type", content: "---\nname: a\ndescription: b\nmetadata:\n  node_type: memory\n---\n\nBody.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNote("/p/proj/memory/a.md", []byte(tt.content), time.Unix(0, 0))
			if err == nil {
				t.Errorf("parseNote: got %+v, want an error", got)
			}
		})
	}
}

func TestLoadNotes(t *testing.T) {
	root := t.TempDir()
	const valid = "---\nname: one\ndescription: A fact\nmetadata:\n  type: project\n---\n\nBody.\n"
	files := map[string]string{
		"proj-a/memory/one.md":     valid,
		"proj-a/memory/MEMORY.md":  "- [One](one.md)\n",
		"proj-b/memory/broken.md":  "# No front matter\n",
		"proj-b/memory/notes.txt":  valid,
		"proj-b/other/ignored.md":  valid,
		"proj-b/memory/sub/two.md": valid,
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fileTime := time.Date(2026, 11, 2, 8, 30, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "proj-a/memory/one.md"), fileTime, fileTime); err != nil {
		t.Fatal(err)
	}

	notes, errs := loadNotes(root)

	want := note{ID: "proj-a/one", Project: "proj-a", Type: "project", Name: "one", Description: "A fact", Updated: fileTime.Unix(), Text: valid}
	if len(notes) != 1 || notes[0] != want {
		t.Errorf("notes:\n got %+v\nwant [%+v]", notes, want)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.md") {
		t.Errorf("errors: got %v, want one error that names broken.md", errs)
	}
}
