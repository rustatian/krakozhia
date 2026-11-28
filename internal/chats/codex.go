package chats

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	Cwd          string          `json:"cwd"`
	Source       json.RawMessage `json:"source"`
	ThreadSource string          `json:"thread_source"`
}

type codexResponseItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// ScanCodex parses top-level Codex rollout JSONL sessions under <codexDir>/sessions.
func ScanCodex(codexDir, homeDir string, opts ScanOptions) ([]Session, error) {
	root := filepath.Join(codexDir, "sessions")
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	var out []Session
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mtime := info.ModTime().UTC()
		if !opts.inWindow(mtime) {
			return nil
		}
		if sess, ok := parseCodexFile(p, mtime, homeDir); ok {
			out = append(out, sess)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func parseCodexFile(path string, mtime time.Time, homeDir string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()

	var (
		sessionID string
		cwd       string
		started   time.Time
		updated   time.Time
		turns     []Turn
	)

	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var cl codexLine
			if json.Unmarshal(line, &cl) == nil {
				if ts, ok := parseTimeFlex(cl.Timestamp); ok {
					if started.IsZero() || ts.Before(started) {
						started = ts
					}
					if ts.After(updated) {
						updated = ts
					}
				}
				switch cl.Type {
				case "session_meta":
					var meta codexMeta
					if json.Unmarshal(cl.Payload, &meta) == nil {
						if isCodexSubagent(meta) {
							return Session{}, false
						}
						if sessionID == "" {
							sessionID = meta.ID
							if sessionID == "" {
								sessionID = meta.SessionID
							}
						}
						if cwd == "" {
							cwd = meta.Cwd
						}
					}
				case "response_item":
					var item codexResponseItem
					if json.Unmarshal(cl.Payload, &item) == nil && item.Type == "message" &&
						(item.Role == "user" || item.Role == "assistant") {
						var parts []string
						for _, c := range item.Content {
							if (c.Type == "input_text" || c.Type == "output_text" || c.Type == "text") &&
								strings.TrimSpace(c.Text) != "" {
								parts = append(parts, strings.TrimSpace(c.Text))
							}
						}
						if len(parts) > 0 {
							turns = append(turns, Turn{Role: item.Role, Text: strings.Join(parts, "\n")})
						}
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
	}

	excerpt, ok := FormatTurns(turns, defaultMaxExcerptRunes)
	if !ok {
		return Session{}, false
	}
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if started.IsZero() {
		started = mtime
	}
	if updated.IsZero() {
		updated = mtime
	}
	return Session{
		ID:             "codex:" + sessionID,
		SourceAgent:    "codex",
		ExternalID:     sessionID,
		Project:        ResolveProject(cwd, homeDir),
		WorkspacePath:  cwd,
		Title:          deriveTitle("", turns),
		StartedAt:      started,
		UpdatedAt:      updated,
		TranscriptHash: hashExcerpt(excerpt),
		RawExcerpt:     excerpt,
	}, true
}

func isCodexSubagent(meta codexMeta) bool {
	if strings.EqualFold(meta.ThreadSource, "subagent") {
		return true
	}
	trimmed := strings.TrimSpace(string(meta.Source))
	return strings.HasPrefix(trimmed, "{")
}
