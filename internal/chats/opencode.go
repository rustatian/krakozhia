package chats

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type ocUserData struct {
	Text string `json:"text"`
}

type ocAssistantData struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type ocPartData struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ScanOpenCode parses top-level OpenCode sessions from <opencodeDir>/opencode.db.
func ScanOpenCode(opencodeDir, homeDir string, opts ScanOptions) ([]Session, error) {
	dbPath := filepath.Join(opencodeDir, "opencode.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	dsn := "file:" + filepath.ToSlash(dbPath) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id, coalesce(directory, ''), coalesce(title, ''), coalesce(time_created, 0), coalesce(time_updated, 0)
		FROM session_v2
		WHERE coalesce(parent_id, '') = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type metaRow struct {
		id        string
		dir       string
		title     string
		createdAt time.Time
		updatedAt time.Time
	}
	var candidates []metaRow
	for rows.Next() {
		var r metaRow
		var createdMs, updatedMs int64
		if err := rows.Scan(&r.id, &r.dir, &r.title, &createdMs, &updatedMs); err != nil {
			continue
		}
		r.createdAt = epochToTime(createdMs)
		r.updatedAt = epochToTime(updatedMs)
		if r.updatedAt.IsZero() {
			r.updatedAt = r.createdAt
		}
		if !opts.inWindow(r.updatedAt) {
			continue
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []Session
	for _, c := range candidates {
		turns, err := loadOpenCodeTurns(db, c.id)
		if err != nil {
			continue
		}
		excerpt, ok := FormatTurns(turns, defaultMaxExcerptRunes)
		if !ok {
			continue
		}
		out = append(out, Session{
			ID:             "opencode:" + c.id,
			SourceAgent:    "opencode",
			ExternalID:     c.id,
			Project:        ResolveProject(c.dir, homeDir),
			WorkspacePath:  c.dir,
			RepoFullName:   ResolveGitRemote(c.dir, homeDir),
			Title:          deriveTitle(c.title, turns),
			StartedAt:      c.createdAt,
			UpdatedAt:      c.updatedAt,
			TranscriptHash: hashExcerpt(excerpt),
			RawExcerpt:     excerpt,
		})
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func loadOpenCodeTurns(db *sql.DB, sessionID string) ([]Turn, error) {
	// Preload session_part text items keyed by message_id (used by newer OpenCode builds).
	partsByMsg := map[string][]string{}
	partRows, err := db.Query(`
		SELECT message_id, data
		FROM session_part
		WHERE session_id = ? AND type = 'text'
		ORDER BY ordinal ASC`, sessionID)
	if err == nil {
		defer partRows.Close()
		for partRows.Next() {
			var msgID, raw string
			if err := partRows.Scan(&msgID, &raw); err != nil {
				continue
			}
			var pd ocPartData
			if json.Unmarshal([]byte(raw), &pd) == nil && strings.TrimSpace(pd.Text) != "" {
				partsByMsg[msgID] = append(partsByMsg[msgID], strings.TrimSpace(pd.Text))
			}
		}
	}

	msgRows, err := db.Query(`
		SELECT id, type, data
		FROM session_message
		WHERE session_id = ? AND type IN ('user', 'assistant')
		ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer msgRows.Close()

	var turns []Turn
	for msgRows.Next() {
		var msgID, typ, raw string
		if err := msgRows.Scan(&msgID, &typ, &raw); err != nil {
			continue
		}
		switch typ {
		case "user":
			var u ocUserData
			if json.Unmarshal([]byte(raw), &u) == nil && strings.TrimSpace(u.Text) != "" {
				turns = append(turns, Turn{Role: "user", Text: strings.TrimSpace(u.Text)})
			}
		case "assistant":
			var parts []string
			var a ocAssistantData
			if json.Unmarshal([]byte(raw), &a) == nil {
				for _, c := range a.Content {
					if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
						parts = append(parts, strings.TrimSpace(c.Text))
					}
				}
			}
			if len(parts) == 0 {
				parts = partsByMsg[msgID]
			}
			if len(parts) > 0 {
				turns = append(turns, Turn{Role: "assistant", Text: strings.Join(parts, "\n")})
			}
		}
	}
	return turns, msgRows.Err()
}

func epochToTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	if v > 1e12 {
		return time.UnixMilli(v).UTC()
	}
	return time.Unix(v, 0).UTC()
}
