package chats

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type claudeLine struct {
	Type        string          `json:"type"`
	IsSidechain bool            `json:"isSidechain"`
	IsMeta      bool            `json:"isMeta"`
	SessionID   string          `json:"sessionId"`
	Cwd         string          `json:"cwd"`
	Title       string          `json:"title"`
	Timestamp   string          `json:"timestamp"`
	Message     json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ScanClaude parses top-level Claude Code session JSONL files under <claudeProjectsDir>/*/*.jsonl.
func ScanClaude(claudeProjectsDir, homeDir string, opts ScanOptions) ([]Session, error) {
	if _, err := os.Stat(claudeProjectsDir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(claudeProjectsDir, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}

	var out []Session
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			continue
		}
		mtime := info.ModTime().UTC()
		if !opts.inWindow(mtime) {
			continue
		}
		sess, ok := parseClaudeFile(p, mtime, homeDir)
		if !ok {
			continue
		}
		out = append(out, sess)
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func parseClaudeFile(path string, mtime time.Time, homeDir string) (Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false
	}
	defer f.Close()

	var (
		sessionID string
		cwd       string
		title     string
		started   time.Time
		updated   time.Time
		turns     []Turn
	)

	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var cl claudeLine
			if json.Unmarshal(line, &cl) == nil {
				if sessionID == "" && cl.SessionID != "" {
					sessionID = cl.SessionID
				}
				if cwd == "" && cl.Cwd != "" {
					cwd = cl.Cwd
				}
				if cl.Type == "ai-title" && cl.Title != "" {
					title = cl.Title
				}
				if ts, ok := parseTimeFlex(cl.Timestamp); ok {
					if started.IsZero() || ts.Before(started) {
						started = ts
					}
					if ts.After(updated) {
						updated = ts
					}
				}
				if !cl.IsSidechain && !cl.IsMeta && (cl.Type == "user" || cl.Type == "assistant") && len(cl.Message) > 0 {
					if text := extractClaudeMessageText(cl.Message); text != "" {
						turns = append(turns, Turn{Role: cl.Type, Text: text})
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
	project := ResolveProject(cwd, homeDir)
	if cwd == "" {
		folder := filepath.Base(filepath.Dir(path))
		project = sanitizeProjectName(strings.TrimLeft(folder, "-"))
	}
	return Session{
		ID:             "claude:" + sessionID,
		SourceAgent:    "claude",
		ExternalID:     sessionID,
		Project:        project,
		WorkspacePath:  cwd,
		Title:          deriveTitle(title, turns),
		StartedAt:      started,
		UpdatedAt:      updated,
		TranscriptHash: hashExcerpt(excerpt),
		RawExcerpt:     excerpt,
	}, true
}

func extractClaudeMessageText(raw json.RawMessage) string {
	var msg claudeMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var asStr string
	if err := json.Unmarshal(msg.Content, &asStr); err == nil {
		return strings.TrimSpace(asStr)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}
