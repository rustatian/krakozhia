package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ChatSession tracks an ingested client chat transcript and its distilled chat document.
type ChatSession struct {
	ID             string    `json:"id"`
	SourceAgent    string    `json:"source_agent"`
	ExternalID     string    `json:"external_id"`
	Project        string    `json:"project"`
	WorkspacePath  string    `json:"workspace_path"`
	Title          string    `json:"title"`
	TranscriptHash string    `json:"transcript_hash"`
	RawExcerpt     string    `json:"raw_excerpt,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	DocumentID     *string   `json:"document_id,omitempty"`
	DistilledHash  string    `json:"distilled_hash"`
}

// IngestChatInput specifies a chat session transcript ready for async GPU distillation.
type IngestChatInput struct {
	ID             string     `json:"id,omitempty"`
	SourceAgent    string     `json:"source_agent"`
	ExternalID     string     `json:"external_id"`
	Project        string     `json:"project"`
	WorkspacePath  string     `json:"workspace_path,omitempty"`
	RepoFullName   string     `json:"repo_full_name,omitempty"`
	Title          string     `json:"title,omitempty"`
	TranscriptHash string     `json:"transcript_hash,omitempty"`
	RawExcerpt     string     `json:"raw_excerpt"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
}

// UpsertChatSession stores a pending chat excerpt after secret scanning and queues distill_chat when changed.
// It returns true when a distillation job was queued and false when the transcript hash is unchanged.
func (s *Store) UpsertChatSession(ctx context.Context, in IngestChatInput) (bool, error) {
	agent := strings.TrimSpace(in.SourceAgent)
	extID := strings.TrimSpace(in.ExternalID)
	project := strings.TrimSpace(in.Project)
	if agent == "" || extID == "" || project == "" || strings.TrimSpace(in.RawExcerpt) == "" {
		return false, errors.New("source_agent, external_id, project, and raw_excerpt are required")
	}
	if err := checkSecrets(in.RawExcerpt); err != nil {
		return false, err
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = agent + ":" + extID
	}
	hash := strings.TrimSpace(in.TranscriptHash)
	if hash == "" {
		hash = hashContent(in.RawExcerpt)
	}
	now := time.Now().UTC()
	startedAt := now
	if in.StartedAt != nil && !in.StartedAt.IsZero() {
		startedAt = in.StartedAt.UTC()
	}
	updatedAt := now
	if in.UpdatedAt != nil && !in.UpdatedAt.IsZero() {
		updatedAt = in.UpdatedAt.UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prevHash, prevDistilled, prevExcerpt string
	err = tx.QueryRow(ctx, `
		SELECT transcript_hash, distilled_hash, raw_excerpt
		FROM chat_sessions
		WHERE id = $1
		FOR UPDATE`, id).Scan(&prevHash, &prevDistilled, &prevExcerpt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil && prevHash == hash && (prevDistilled == hash || prevExcerpt != "") {
		if err := upsertProjectRepoTx(ctx, tx, project, in.RepoFullName); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO chat_sessions (
			id, source_agent, external_id, project, workspace_path, title,
			transcript_hash, raw_excerpt, started_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			project = EXCLUDED.project,
			workspace_path = EXCLUDED.workspace_path,
			title = CASE WHEN EXCLUDED.title <> '' THEN EXCLUDED.title ELSE chat_sessions.title END,
			transcript_hash = EXCLUDED.transcript_hash,
			raw_excerpt = EXCLUDED.raw_excerpt,
			updated_at = EXCLUDED.updated_at`,
		id, agent, extID, project, in.WorkspacePath, in.Title, hash, in.RawExcerpt, startedAt, updatedAt); err != nil {
		return false, err
	}
	if err := upsertProjectRepoTx(ctx, tx, project, in.RepoFullName); err != nil {
		return false, err
	}
	if err := enqueueJobTx(ctx, tx, "distill_chat", "chat", id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ListChatHashes returns stored chat session IDs mapped to their transcript hashes.
func (s *Store) ListChatHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, transcript_hash FROM chat_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			return nil, err
		}
		out[id] = h
	}
	return out, rows.Err()
}

// GetChatSession fetches one chat session by ID.
func (s *Store) GetChatSession(ctx context.Context, id string) (ChatSession, error) {
	var cs ChatSession
	err := s.pool.QueryRow(ctx, `
		SELECT id, source_agent, external_id, project, workspace_path, title,
		       transcript_hash, raw_excerpt, started_at, updated_at, document_id, distilled_hash
		FROM chat_sessions
		WHERE id = $1`, id).Scan(
		&cs.ID, &cs.SourceAgent, &cs.ExternalID, &cs.Project, &cs.WorkspacePath, &cs.Title,
		&cs.TranscriptHash, &cs.RawExcerpt, &cs.StartedAt, &cs.UpdatedAt, &cs.DocumentID, &cs.DistilledHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatSession{}, ErrNotFound
	}
	return cs, err
}

// MarkChatDistilled links a distilled chat document and clears raw_excerpt so PostgreSQL stays compact.
func (s *Store) MarkChatDistilled(ctx context.Context, id, documentID, distilledHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE chat_sessions
		SET document_id = $2, distilled_hash = $3, raw_excerpt = ''
		WHERE id = $1`, id, documentID, distilledHash)
	return err
}

// ListNotesForBrief returns notes matching project directly or matching a Claude Code directory slug suffix.
func (s *Store) ListNotesForBrief(ctx context.Context, project string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, project, note_type, name, description, summary, tags, content, content_hash, source_agent, updated_at
		FROM notes
		WHERE project = $1
		   OR ($1 NOT LIKE '-%' AND length($1) >= 3 AND project LIKE '%-' || $1)
		ORDER BY updated_at DESC, id ASC
		LIMIT $2`, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(
			&n.ID, &n.Project, &n.NoteType, &n.Name, &n.Description, &n.Summary,
			&n.Tags, &n.Content, &n.ContentHash, &n.SourceAgent, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
