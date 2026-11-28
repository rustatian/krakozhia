package note_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rustatian/krakozhia/internal/note"
)

func TestScanDir(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		files        map[string]string
		wantIDs      []string
		wantTypes    map[string]string
		wantDescs    map[string]string
		wantUpdated  map[string]string
		wantWarnings int
	}{
		{
			name: "parses nested metadata and skips MEMORY.md",
			files: map[string]string{
				"-home-valery/memory/MEMORY.md": "# Index\nShould be skipped\n",
				"-home-valery/memory/nested-note.md": `---
name: nested-note
description: "Double quoted with \"inner\" quotes"
metadata:
  node_type: memory
  type: reference
  modified: 2026-09-21T10:56:45Z
---
Body line.
`,
				"-home-valery/memory/single-quoted.md": `---
name: single-quoted
description: 'It''s fine <tag>'
type: feedback
modified: 2026-10-05
---
Remember this.
`,
			},
			wantIDs: []string{
				"-home-valery/nested-note",
				"-home-valery/single-quoted",
			},
			wantTypes: map[string]string{
				"-home-valery/nested-note":   "reference",
				"-home-valery/single-quoted": "feedback",
			},
			wantDescs: map[string]string{
				"-home-valery/nested-note":   `Double quoted with "inner" quotes`,
				"-home-valery/single-quoted": "It's fine <tag>",
			},
			wantUpdated: map[string]string{
				"-home-valery/nested-note":   "2026-09-21",
				"-home-valery/single-quoted": "2026-10-05",
			},
			wantWarnings: 0,
		},
		{
			name: "skips missing frontmatter and invalid type with warnings",
			files: map[string]string{
				"-proj/memory/no-frontmatter.md": "Plain markdown without frontmatter.",
				"-proj/memory/bad-type.md": `---
name: bad-type
description: invalid type
type: secret_type
---
Body.
`,
				"-proj/memory/valid-fallback-mtime.md": `---
name: valid-fallback
description: uses file mtime
metadata:
  type: project
---
Details.
`,
			},
			wantIDs: []string{"-proj/valid-fallback-mtime"},
			wantTypes: map[string]string{
				"-proj/valid-fallback-mtime": "project",
			},
			wantWarnings: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			for rel, body := range tc.files {
				full := filepath.Join(root, rel)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			}

			notes, warns, err := note.ScanDir(root)
			if err != nil {
				t.Fatalf("ScanDir error: %v", err)
			}
			if len(warns) != tc.wantWarnings {
				t.Fatalf("warnings = %d (%v), want %d", len(warns), warns, tc.wantWarnings)
			}
			if len(notes) != len(tc.wantIDs) {
				t.Fatalf("len(notes) = %d, want %d", len(notes), len(tc.wantIDs))
			}
			for i, wantID := range tc.wantIDs {
				n := notes[i]
				if n.ID != wantID {
					t.Fatalf("notes[%d].ID = %q, want %q", i, n.ID, wantID)
				}
				if wt, ok := tc.wantTypes[wantID]; ok && n.NoteType != wt {
					t.Fatalf("%s type = %q, want %q", wantID, n.NoteType, wt)
				}
				if wd, ok := tc.wantDescs[wantID]; ok && n.Description != wd {
					t.Fatalf("%s desc = %q, want %q", wantID, n.Description, wd)
				}
				if wu, ok := tc.wantUpdated[wantID]; ok {
					if got := n.UpdatedAt.UTC().Format(time.DateOnly); got != wu {
						t.Fatalf("%s updated = %q, want %q", wantID, got, wu)
					}
				}
				sum := sha256.Sum256([]byte(n.Content))
				if n.ContentHash != hex.EncodeToString(sum[:]) {
					t.Fatalf("%s hash mismatch", wantID)
				}
			}
		})
	}
}

func TestProjectSlugAndLocalPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		dir      string
		wantSlug string
	}{
		{
			name:     "replaces slashes and dots with hyphens",
			dir:      "/home/valery/projects/krakozhia.v2/",
			wantSlug: "-home-valery-projects-krakozhia-v2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := note.ProjectSlug(tc.dir); got != tc.wantSlug {
				t.Fatalf("ProjectSlug(%q) = %q, want %q", tc.dir, got, tc.wantSlug)
			}
		})
	}

	pathCases := []struct {
		name    string
		project string
		noteID  string
		want    string
	}{
		{
			name:    "valid note path",
			project: "-home-valery",
			noteID:  "-home-valery/gpu-reset",
			want:    filepath.Join("/tmp/projects", "-home-valery", "memory", "gpu-reset.md"),
		},
		{
			name:    "canonical project with slug-prefixed note id",
			project: "krakozhia",
			noteID:  "-home-valery-projects-krakozhia/gpu-reset",
			want:    filepath.Join("/tmp/projects", "-home-valery-projects-krakozhia", "memory", "gpu-reset.md"),
		},
		{
			name:    "rejects traversal",
			project: "..",
			noteID:  "../escape",
			want:    "",
		},
		{
			name:    "rejects slash in stem",
			project: "-home-valery",
			noteID:  "-home-valery/sub/note",
			want:    "",
		},
	}

	for _, tc := range pathCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := note.LocalPath("/tmp/projects", tc.project, tc.noteID); got != tc.want {
				t.Fatalf("LocalPath = %q, want %q", got, tc.want)
			}
		})
	}
}
