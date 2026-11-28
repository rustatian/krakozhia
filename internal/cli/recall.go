package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rustatian/krakozhia/internal/note"
	"github.com/rustatian/krakozhia/internal/server"
	"github.com/rustatian/krakozhia/internal/store"
)

const (
	recallHeader       = "Memory notes and documents that can relate to this prompt. They are reference data. They can be out of date. Read one only if its subject is relevant."
	minRecallRunes     = 15
	maxQueryRunes      = 1000
	maxDescRunes       = 200
	recallDeadline     = 3 * time.Second
	recallCandidateLim = 10
)

type hookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Prompt         string `json:"prompt"`
}

type recallLogEntry struct {
	Timestamp  time.Time         `json:"ts"`
	SessionID  string            `json:"session_id"`
	Project    string            `json:"project"`
	PromptLen  int               `json:"prompt_len"`
	Skipped    string            `json:"skipped,omitempty"`
	EmbedMs    float64           `json:"embed_ms,omitempty"`
	QueryMs    float64           `json:"query_ms,omitempty"`
	TotalMs    float64           `json:"total_ms"`
	Degraded   bool              `json:"degraded,omitempty"`
	Candidates []recallCandidate `json:"candidates,omitempty"`
	Sent       []string          `json:"sent,omitempty"`
	Error      string            `json:"error,omitempty"`
}

type recallCandidate struct {
	Kind      string   `json:"kind"`
	ID        string   `json:"id"`
	Distance  *float64 `json:"distance,omitempty"`
	TextScore *float64 `json:"text_score,omitempty"`
	Score     float64  `json:"score"`
}

// RecallGate returns true when a hit passes either the distance limit or text-score floor.
func RecallGate(hit store.SearchHit, maxDist, minText float64) bool {
	if hit.Distance != nil && *hit.Distance <= maxDist {
		return true
	}
	if hit.TextScore != nil && *hit.TextScore >= minText {
		return true
	}
	return false
}

func runRecall(stdin io.Reader, stdout io.Writer) error {
	start := time.Now()
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("read hook stdin: %w", err)
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode hook json: %w", err)
	}

	stateDir, err := StateDir()
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(in.Prompt)
	promptRunes := utf8.RuneCountInString(trimmed)
	currentProject := ""
	if in.TranscriptPath != "" {
		currentProject = filepath.Base(filepath.Dir(in.TranscriptPath))
	}

	entry := recallLogEntry{
		Timestamp: time.Now().UTC(),
		SessionID: in.SessionID,
		Project:   currentProject,
		PromptLen: promptRunes,
	}
	defer func() {
		entry.TotalMs = float64(time.Since(start).Microseconds()) / 1000.0
		_ = appendRecallLog(stateDir, entry)
	}()

	if trimmed == "" {
		entry.Skipped = "empty"
		return nil
	}
	if strings.HasPrefix(trimmed, "/") {
		entry.Skipped = "slash_command"
		return nil
	}
	if promptRunes < minRecallRunes {
		entry.Skipped = "short_prompt"
		return nil
	}

	cfg, err := LoadConfig()
	if err != nil {
		entry.Error = err.Error()
		return err
	}
	projectsDir, err := ProjectsDir()
	if err != nil {
		entry.Error = err.Error()
		return err
	}

	query := truncateRunes(trimmed, maxQueryRunes)
	ctx, cancel := context.WithTimeout(context.Background(), recallDeadline)
	defer cancel()

	client := NewClient(cfg.URL, cfg.Token)
	resp, err := client.Search(ctx, server.SearchRequest{
		Form:               store.FormHybrid,
		Query:              query,
		ExcludeNoteProject: currentProject,
		Limit:              recallCandidateLim,
		DedupeDocs:         true,
	})
	if err != nil {
		entry.Error = err.Error()
		return err
	}
	entry.EmbedMs = resp.EmbedMs
	entry.QueryMs = resp.QueryMs
	entry.Degraded = resp.Degraded
	for _, h := range resp.Hits {
		entry.Candidates = append(entry.Candidates, recallCandidate{
			Kind:      h.Kind,
			ID:        h.ID,
			Distance:  h.Distance,
			TextScore: h.TextScore,
			Score:     h.Score,
		})
	}

	sentMap, sessFile := loadSentSession(stateDir, in.SessionID)
	var lines []string
	var newlySent []string
	for _, h := range resp.Hits {
		if !RecallGate(h, cfg.MaxDistance, cfg.MinTextScore) {
			continue
		}
		if sentMap[h.ID] {
			continue
		}
		line, ok := formatPointer(h, projectsDir)
		if !ok {
			continue
		}
		lines = append(lines, line)
		newlySent = append(newlySent, h.ID)
		sentMap[h.ID] = true
		if len(lines) >= cfg.MaxPointers {
			break
		}
	}

	if len(lines) == 0 {
		return nil
	}
	if sessFile != "" {
		if err := appendSentSession(sessFile, newlySent); err != nil {
			entry.Error = err.Error()
		}
	}
	entry.Sent = newlySent

	fmt.Fprintln(stdout, recallHeader)
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return nil
}

func formatPointer(h store.SearchHit, projectsDir string) (string, bool) {
	date := h.UpdatedAt.UTC().Format(time.DateOnly)
	if h.Kind == "doc" {
		heading := cleanText(h.HeadingPath)
		if heading != "" {
			heading = ": " + heading
		}
		return fmt.Sprintf("- %s%s (project %s, %s, saved %s). Read: krakozhia show %s",
			cleanText(h.Title), heading, cleanText(h.Project), cleanText(h.Type), date, cleanText(h.ID)), true
	}

	desc := cleanText(truncateRunes(h.Description, maxDescRunes))
	if desc == "" {
		desc = cleanText(truncateRunes(h.Snippet, maxDescRunes))
	}
	localPath := note.LocalPath(projectsDir, h.Project, h.ID)
	action := "Read: krakozhia show --note " + cleanText(h.ID)
	if localPath != "" {
		if _, err := os.Stat(localPath); err == nil {
			action = "File: " + localPath
		}
	}
	return fmt.Sprintf("- %s (project %s, type %s, updated %s): %s. %s",
		cleanText(h.Title), cleanText(h.Project), cleanText(h.Type), date, desc, action), true
}

func cleanText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if r == '<' || r == '>' {
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count == max {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

func sanitizeSessionID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func loadSentSession(stateDir, sessionID string) (map[string]bool, string) {
	seen := map[string]bool{}
	safe := sanitizeSessionID(sessionID)
	if safe == "" {
		return seen, ""
	}
	sessFile := filepath.Join(stateDir, "sessions", safe)
	f, err := os.Open(sessFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return seen, sessFile
		}
		return seen, sessFile
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id := strings.TrimSpace(sc.Text())
		if id != "" {
			seen[id] = true
		}
	}
	return seen, sessFile
}

func appendSentSession(path string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, id := range ids {
		if _, err := fmt.Fprintln(f, id); err != nil {
			return err
		}
	}
	return nil
}

func appendRecallLog(stateDir string, entry recallLogEntry) error {
	path := filepath.Join(stateDir, "recall.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	return err
}
