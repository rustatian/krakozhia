// Package note discovers and parses Claude Code Markdown memory notes.
package note

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Note represents a parsed Markdown memory note on local disk.
type Note struct {
	ID          string
	Project     string
	NoteType    string
	Name        string
	Description string
	Content     string
	ContentHash string
	FilePath    string
	UpdatedAt   time.Time
}

// ScanDir scans <projectsDir>/*/memory/*.md, parses frontmatter, and returns notes sorted by ID.
func ScanDir(projectsDir string) ([]Note, []string, error) {
	pattern := filepath.Join(projectsDir, "*", "memory", "*.md")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, nil, err
	}

	var notes []Note
	var warnings []string
	for _, p := range paths {
		if filepath.Base(p) == "MEMORY.md" {
			continue
		}
		project := filepath.Base(filepath.Dir(filepath.Dir(p)))
		stem := strings.TrimSuffix(filepath.Base(p), ".md")
		id := project + "/" + stem

		raw, err := os.ReadFile(p)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: read: %v", id, err))
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: stat: %v", id, err))
			continue
		}
		content := string(raw)
		name, desc, noteType, updatedAt, ok := parseFrontmatter(content, info.ModTime().UTC())
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: missing or invalid frontmatter", id))
			continue
		}
		sum := sha256.Sum256(raw)
		notes = append(notes, Note{
			ID:          id,
			Project:     project,
			NoteType:    noteType,
			Name:        name,
			Description: desc,
			Content:     content,
			ContentHash: hex.EncodeToString(sum[:]),
			FilePath:    p,
			UpdatedAt:   updatedAt,
		})
	}
	slices.SortFunc(notes, func(a, b Note) int {
		return strings.Compare(a.ID, b.ID)
	})
	return notes, warnings, nil
}

// ProjectSlug converts a directory path to the folder name convention used by Claude Code.
func ProjectSlug(dir string) string {
	clean := filepath.Clean(dir)
	r := strings.NewReplacer("/", "-", ".", "-")
	return r.Replace(clean)
}

// LocalPath returns the workstation file path for a note ID, or "" if the ID is unsafe.
func LocalPath(projectsDir, project, noteID string) string {
	if projectsDir == "" || !isSafeSegment(project) {
		return ""
	}
	stem, ok := strings.CutPrefix(noteID, project+"/")
	if !ok || !isSafeSegment(stem) {
		return ""
	}
	return filepath.Join(projectsDir, project, "memory", stem+".md")
}

func isSafeSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, "/\\")
}

func parseFrontmatter(content string, fallbackTime time.Time) (name, desc, noteType string, updated time.Time, ok bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", "", time.Time{}, false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", "", "", time.Time{}, false
	}

	var modifiedRaw string
	inMetadata := false
	for _, rawLine := range lines[1:end] {
		if strings.TrimSpace(rawLine) == "" {
			continue
		}
		indented := strings.HasPrefix(rawLine, " ") || strings.HasPrefix(rawLine, "\t")
		trimmed := strings.TrimSpace(rawLine)
		if !indented {
			inMetadata = false
			if trimmed == "metadata:" {
				inMetadata = true
				continue
			}
		} else if !inMetadata {
			continue
		}

		k, v, hasColon := strings.Cut(trimmed, ":")
		if !hasColon {
			continue
		}
		key := strings.TrimSpace(k)
		val := unquoteYAML(strings.TrimSpace(v))
		switch key {
		case "name":
			if !indented {
				name = val
			}
		case "description":
			if !indented {
				desc = val
			}
		case "type":
			noteType = val
		case "modified":
			modifiedRaw = val
		}
	}

	if name == "" || !slices.Contains([]string{"user", "feedback", "project", "reference"}, noteType) {
		return "", "", "", time.Time{}, false
	}
	updated = parseTimestamp(modifiedRaw, fallbackTime)
	return name, desc, noteType, updated, true
}

func unquoteYAML(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if u, err := strconv.Unquote(v); err == nil {
			return u
		}
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

func parseTimestamp(raw string, fallback time.Time) time.Time {
	if raw == "" {
		return fallback
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, time.DateOnly} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return fallback
}
