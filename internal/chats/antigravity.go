package chats

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type agyMetadataCache struct {
	Conversations map[string]struct {
		IsInternal bool `json:"is_internal"`
	} `json:"conversations"`
}

type agyTranscriptLine struct {
	Source    string `json:"source"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
}

// ScanAntigravity parses top-level user conversations from ~/.gemini/antigravity-cli.
func ScanAntigravity(agyDir, homeDir string, opts ScanOptions) ([]Session, error) {
	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	internalIDs := loadAgyInternalIDs(filepath.Join(agyDir, "cache", "conversation_metadata.json"))

	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT conversation_id, coalesce(title, ''), coalesce(workspace_uris, ''), coalesce(last_modified_time, '')
		FROM conversation_summaries
		WHERE coalesce(nesting_depth, 0) = 0
		  AND coalesce(parent_conversation_id, '') = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var convID, title, wsRaw, modRaw string
		if err := rows.Scan(&convID, &title, &wsRaw, &modRaw); err != nil {
			continue
		}
		if convID == "" || internalIDs[convID] {
			continue
		}
		updated, ok := parseTimeFlex(modRaw)
		if !ok {
			continue
		}
		if !opts.inWindow(updated) {
			continue
		}
		tPath := filepath.Join(agyDir, "brain", convID, ".system_generated", "logs", "transcript.jsonl")
		turns, started, lastStep := parseAgyTranscript(tPath)
		excerpt, ok := FormatTurns(turns, defaultMaxExcerptRunes)
		if !ok {
			continue
		}
		if started.IsZero() {
			started = updated
		}
		if !lastStep.IsZero() && lastStep.After(updated) {
			updated = lastStep
		}
		if !opts.inWindow(updated) {
			continue
		}

		wsPath := firstWorkspaceURI(wsRaw)
		out = append(out, Session{
			ID:             "antigravity:" + convID,
			SourceAgent:    "antigravity",
			ExternalID:     convID,
			Project:        ResolveProject(wsPath, homeDir),
			WorkspacePath:  wsPath,
			RepoFullName:   ResolveGitRemote(wsPath, homeDir),
			Title:          deriveTitle(title, turns),
			StartedAt:      started,
			UpdatedAt:      updated,
			TranscriptHash: hashExcerpt(excerpt),
			RawExcerpt:     excerpt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func loadAgyInternalIDs(path string) map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var cache agyMetadataCache
	if err := json.Unmarshal(raw, &cache); err != nil {
		return out
	}
	for id, meta := range cache.Conversations {
		if meta.IsInternal {
			out[id] = true
		}
	}
	return out
}

func firstWorkspaceURI(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) == nil && len(uris) > 0 {
		return strings.TrimPrefix(uris[0], "file://")
	}
	return strings.TrimPrefix(raw, "file://")
}

func parseAgyTranscript(path string) ([]Turn, time.Time, time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, time.Time{}
	}
	defer f.Close()

	var (
		turns   []Turn
		started time.Time
		updated time.Time
	)
	r := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var entry agyTranscriptLine
			if json.Unmarshal(line, &entry) == nil {
				if ts, ok := parseTimeFlex(entry.CreatedAt); ok {
					if started.IsZero() || ts.Before(started) {
						started = ts
					}
					if ts.After(updated) {
						updated = ts
					}
				}
				content := strings.TrimSpace(entry.Content)
				if content == "" {
					continue
				}
				switch {
				case entry.Type == "USER_INPUT" || entry.Source == "USER_EXPLICIT":
					turns = append(turns, Turn{Role: "user", Text: content})
				case entry.Type == "PLANNER_RESPONSE":
					turns = append(turns, Turn{Role: "assistant", Text: content})
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
	return turns, started, updated
}
