package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// note is one memory note file of Claude Code.
type note struct {
	ID          string // <project folder>/<file stem>
	Project     string
	Type        string
	Name        string
	Description string
	Updated     int64  // unix seconds
	Text        string // the full file content
}

// parseNote reads the front matter that Claude Code writes: one line for each key, and the nested keys of metadata on indented lines.
// A plain description can contain a colon and a space, so a value is all text after the first colon of its line.
func parseNote(path string, data []byte, fileTime time.Time) (note, error) {
	text := string(data)
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return note{}, errors.New("no front matter")
	}
	frontMatter, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return note{}, errors.New("front matter has no closing line")
	}

	// path is <projects>/<project folder>/memory/<file stem>.md
	project := filepath.Base(filepath.Dir(filepath.Dir(path)))
	n := note{
		ID:      project + "/" + strings.TrimSuffix(filepath.Base(path), ".md"),
		Project: project,
		Updated: fileTime.Unix(),
		Text:    text,
	}
	for line := range strings.SplitSeq(frontMatter, "\n") {
		key, value, _ := strings.Cut(line, ":")
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			n.Name = value
		case "description":
			if strings.HasPrefix(value, `"`) {
				if unquoted, err := strconv.Unquote(value); err == nil {
					value = unquoted
				}
			}
			n.Description = value
		case "type":
			n.Type = value
		case "modified":
			// time.Parse accepts the milliseconds that Claude Code writes after the seconds.
			if t, err := time.Parse(time.RFC3339, value); err == nil {
				n.Updated = t.Unix()
			} else if t, err := time.Parse(time.DateOnly, value); err == nil {
				n.Updated = t.Unix()
			}
		}
	}
	if n.Name == "" || n.Description == "" || n.Type == "" {
		return note{}, errors.New("front matter needs name, description and metadata.type")
	}
	return n, nil
}

// loadNotes reads the notes of all projects: <root>/<project folder>/memory/<file stem>.md.
// A MEMORY.md file is the index of a project and is not a note.
func loadNotes(root string) ([]note, []error) {
	paths, err := filepath.Glob(filepath.Join(root, "*", "memory", "*.md"))
	if err != nil {
		return nil, []error{err}
	}
	var notes []note
	var errs []error
	for _, path := range paths {
		if filepath.Base(path) == "MEMORY.md" {
			continue
		}
		n, err := readNote(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
			continue
		}
		notes = append(notes, n)
	}
	return notes, errs
}

func readNote(path string) (note, error) {
	info, err := os.Stat(path)
	if err != nil {
		return note{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return note{}, err
	}
	return parseNote(path, data, info.ModTime())
}
