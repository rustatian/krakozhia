package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPlanSync(t *testing.T) {
	a := note{ID: "proj/a", Text: "text of a"}
	b := note{ID: "proj/b", Text: "text of b"}

	tests := []struct {
		name        string
		notes       []note
		manifest    manifest
		wantPuts    []note
		wantDeletes []string
	}{
		{
			name:     "new note is written",
			notes:    []note{a},
			manifest: manifest{Index: "idx", Hashes: map[string]string{}},
			wantPuts: []note{a},
		},
		{
			name:     "changed note is written",
			notes:    []note{a},
			manifest: manifest{Index: "idx", Hashes: map[string]string{"proj/a": hashText("old text of a")}},
			wantPuts: []note{a},
		},
		{
			name:     "unchanged note is not written",
			notes:    []note{a},
			manifest: manifest{Index: "idx", Hashes: map[string]string{"proj/a": hashText("text of a")}},
		},
		{
			name:     "only the changed note of two is written",
			notes:    []note{a, b},
			manifest: manifest{Index: "idx", Hashes: map[string]string{"proj/a": hashText("text of a"), "proj/b": hashText("old text of b")}},
			wantPuts: []note{b},
		},
		{
			name:        "note with no file is deleted",
			notes:       []note{a},
			manifest:    manifest{Index: "idx", Hashes: map[string]string{"proj/a": hashText("text of a"), "proj/gone": hashText("x"), "proj/also-gone": hashText("y")}},
			wantDeletes: []string{"proj/also-gone", "proj/gone"},
		},
		{
			name:     "manifest of another index writes all notes and deletes nothing",
			notes:    []note{a, b},
			manifest: manifest{Index: "old-idx", Hashes: map[string]string{"proj/a": hashText("text of a"), "proj/b": hashText("text of b"), "proj/gone": hashText("x")}},
			wantPuts: []note{a, b},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			puts, deletes := planSync(tt.notes, tt.manifest, "idx")
			if !slices.Equal(puts, tt.wantPuts) {
				t.Errorf("puts: got %+v, want %+v", puts, tt.wantPuts)
			}
			if !slices.Equal(deletes, tt.wantDeletes) {
				t.Errorf("deletes: got %v, want %v", deletes, tt.wantDeletes)
			}
		})
	}
}

func TestReadManifest(t *testing.T) {
	tests := []struct {
		name    string
		content string // "" writes no file
		want    manifest
	}{
		{
			name:    "manifest file",
			content: `{"index": "idx", "hashes": {"proj/a": "abc"}}`,
			want:    manifest{Index: "idx", Hashes: map[string]string{"proj/a": "abc"}},
		},
		{
			name: "no file gives an empty manifest",
			want: manifest{Hashes: map[string]string{}},
		},
		{
			name:    "damaged file gives an empty manifest",
			content: `{"index": "idx", "hashes": {"proj/a"`,
			want:    manifest{Hashes: map[string]string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if tt.content != "" {
				if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := readManifest(path)
			if got.Index != tt.want.Index || !maps.Equal(got.Hashes, tt.want.Hashes) {
				t.Errorf("readManifest: got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestWriteManifest(t *testing.T) {
	// The state folder does not exist before the first sync.
	path := filepath.Join(t.TempDir(), "state", "krakozhia", "manifest.json")
	want := manifest{Index: "idx", Hashes: map[string]string{"proj/a": "abc", "proj/b": "def"}}

	if err := writeManifest(path, want); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	got := readManifest(path)
	if got.Index != want.Index || !maps.Equal(got.Hashes, want.Hashes) {
		t.Errorf("manifest after write: got %+v, want %+v", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("state folder: got %d files, want only the manifest", len(entries))
	}
}
