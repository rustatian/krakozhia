package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"

	"github.com/rustatian/krakozhia/internal/chunk"
	"github.com/rustatian/krakozhia/internal/secrets"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ChunkInput holds one document section and its optional embedding vector.
type ChunkInput struct {
	Section   chunk.Section
	Embedding []float32
}

// SaveDocumentInput specifies a document save request.
type SaveDocumentInput struct {
	Project          string
	Name             string
	DocType          string
	Title            string
	Summary          string
	Tags             []string
	Content          string
	SourceAgent      string
	SkipEnrichDoc    bool
	SkipRefreshBrief bool
	Chunks           []ChunkInput
}

// SaveDocumentResult summarizes a saved document revision.
type SaveDocumentResult struct {
	ID         string `json:"id"`
	Revision   int    `json:"revision"`
	ChunkCount int    `json:"chunk_count"`
	Unchanged  bool   `json:"unchanged"`
}

// Document represents a stored document and its requested revision body.
type Document struct {
	ID              string    `json:"id"`
	Project         string    `json:"project"`
	Name            string    `json:"name"`
	DocType         string    `json:"doc_type"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	Tags            []string  `json:"tags"`
	CurrentRevision int       `json:"current_revision"`
	Revision        int       `json:"revision"`
	ContentMD       string    `json:"content_md"`
	SourceAgent     string    `json:"source_agent"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Chunk holds one document section stored in PostgreSQL.
type Chunk struct {
	ID            string    `json:"id"`
	DocumentID    string    `json:"document_id"`
	SectionIndex  int       `json:"section_index"`
	HeadingPath   string    `json:"heading_path"`
	Content       string    `json:"content"`
	ContextPrefix string    `json:"context_prefix"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UpsertNoteInput specifies a memory note upsert request.
type UpsertNoteInput struct {
	ID          string
	Project     string
	NoteType    string
	Name        string
	Description string
	Content     string
	SourceAgent string
	UpdatedAt   *time.Time
	Embedding   []float32
}

// Note represents one stored memory note.
type Note struct {
	ID          string    `json:"id"`
	Project     string    `json:"project"`
	NoteType    string    `json:"note_type"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Summary     string    `json:"summary"`
	Tags        []string  `json:"tags"`
	Content     string    `json:"content"`
	ContentHash string    `json:"content_hash"`
	SourceAgent string    `json:"source_agent"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RecordBenchmarkInput specifies a structured benchmark or improvement measurement.
type RecordBenchmarkInput struct {
	Project     string     `json:"project"`
	Suite       string     `json:"suite"`
	Metric      string     `json:"metric"`
	Value       float64    `json:"value"`
	Unit        string     `json:"unit"`
	BaselineID  *int64     `json:"baseline_id,omitempty"`
	DeltaPct    *float64   `json:"delta_pct,omitempty"`
	GitCommit   string     `json:"git_commit,omitempty"`
	Branch      string     `json:"branch,omitempty"`
	Environment string     `json:"environment,omitempty"`
	Status      string     `json:"status,omitempty"`
	Notes       string     `json:"notes,omitempty"`
	DocumentID  *string    `json:"document_id,omitempty"`
	SourceAgent string     `json:"source_agent,omitempty"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`
}

// Benchmark represents one stored benchmark row.
type Benchmark struct {
	ID          int64     `json:"id"`
	Project     string    `json:"project"`
	Suite       string    `json:"suite"`
	Metric      string    `json:"metric"`
	Value       float64   `json:"value"`
	Unit        string    `json:"unit"`
	BaselineID  *int64    `json:"baseline_id,omitempty"`
	DeltaPct    *float64  `json:"delta_pct,omitempty"`
	GitCommit   string    `json:"git_commit"`
	Branch      string    `json:"branch"`
	Environment string    `json:"environment"`
	Status      string    `json:"status"`
	Notes       string    `json:"notes"`
	DocumentID  *string   `json:"document_id,omitempty"`
	SourceAgent string    `json:"source_agent"`
	RecordedAt  time.Time `json:"recorded_at"`
}

// BenchmarkFilter narrows benchmark queries.
type BenchmarkFilter struct {
	Project    string
	Suite      string
	Metric     string
	Branch     string
	GitCommit  string
	Status     string
	LatestOnly bool
	Limit      int
}

// ProjectBrief holds an auto-generated Markdown summary for a project.
type ProjectBrief struct {
	Project    string    `json:"project"`
	BriefMD    string    `json:"brief_md"`
	SourceHash string    `json:"source_hash"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// RecordLink links two knowledge records with an advisory relation.
type RecordLink struct {
	ID         int64     `json:"id"`
	SourceKind string    `json:"source_kind"`
	SourceID   string    `json:"source_id"`
	TargetKind string    `json:"target_kind"`
	TargetID   string    `json:"target_id"`
	Relation   string    `json:"relation"`
	Note       string    `json:"note"`
	CreatedAt  time.Time `json:"created_at"`
}

// EnrichmentJob represents one queued GPU enrichment task.
type EnrichmentJob struct {
	ID         int64     `json:"id"`
	JobType    string    `json:"job_type"`
	TargetKind string    `json:"target_kind"`
	TargetID   string    `json:"target_id"`
	Status     string    `json:"status"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error"`
	CreatedAt  time.Time `json:"created_at"`
}

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func checkSecrets(text string) error {
	if findings := secrets.Scan(text); len(findings) > 0 {
		return &secrets.FindingsError{Findings: findings}
	}
	return nil
}

func optionalVector(v []float32) any {
	if len(v) == 0 {
		return nil
	}
	return pgvector.NewVector(v)
}

type jobExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func enqueueJobTx(ctx context.Context, tx pgx.Tx, jobType, targetKind, targetID string) error {
	return enqueueJob(ctx, tx, jobType, targetKind, targetID)
}

func enqueueJob(ctx context.Context, execer jobExecer, jobType, targetKind, targetID string) error {
	_, err := execer.Exec(ctx, `
		INSERT INTO enrichment_jobs (job_type, target_kind, target_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (job_type, target_kind, target_id) WHERE status = 'pending'
		DO NOTHING`, jobType, targetKind, targetID)
	return err
}

// SaveDocument stores a document, creates a new revision when changed, replaces chunks, and queues enrichment.
func (s *Store) SaveDocument(ctx context.Context, in SaveDocumentInput) (SaveDocumentResult, error) {
	if in.Project == "" || in.Name == "" || in.DocType == "" {
		return SaveDocumentResult{}, errors.New("project, name, and doc_type are required")
	}
	if err := checkSecrets(in.Content); err != nil {
		return SaveDocumentResult{}, err
	}
	id := in.Project + "/" + in.Name
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = chunk.Title(in.Content)
	}
	if title == "" {
		title = in.Name
	}
	hash := hashContent(in.Content)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SaveDocumentResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var curRev int
	var curHash string
	var curChunks int
	err = tx.QueryRow(ctx, `
		SELECT d.current_revision, coalesce(r.content_hash, ''),
		       (SELECT count(*) FROM chunks c WHERE c.document_id = d.id)
		FROM documents d
		LEFT JOIN document_revisions r ON r.document_id = d.id AND r.revision = d.current_revision
		WHERE d.id = $1
		FOR UPDATE OF d`, id).Scan(&curRev, &curHash, &curChunks)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SaveDocumentResult{}, err
	}
	if err == nil && curHash == hash && curChunks > 0 {
		if err := tx.Commit(ctx); err != nil {
			return SaveDocumentResult{}, err
		}
		return SaveDocumentResult{ID: id, Revision: curRev, ChunkCount: curChunks, Unchanged: true}, nil
	}

	tags := in.Tags
	if tags == nil {
		tags = []string{}
	}
	nextRev := curRev + 1
	if errors.Is(err, pgx.ErrNoRows) {
		nextRev = 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO documents (id, project, name, doc_type, title, summary, tags, current_revision, source_agent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, in.Project, in.Name, in.DocType, title, in.Summary, tags, nextRev, in.SourceAgent); err != nil {
			return SaveDocumentResult{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE documents
			SET doc_type = $2,
			    title = $3,
			    summary = CASE WHEN $4 <> '' THEN $4 ELSE summary END,
			    tags = CASE WHEN cardinality($5::text[]) > 0 THEN $5 ELSE tags END,
			    current_revision = $6,
			    source_agent = $7,
			    updated_at = NOW()
			WHERE id = $1`,
			id, in.DocType, title, in.Summary, tags, nextRev, in.SourceAgent); err != nil {
			return SaveDocumentResult{}, err
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO document_revisions (document_id, revision, content_md, content_hash, source_agent)
		VALUES ($1, $2, $3, $4, $5)`,
		id, nextRev, in.Content, hash, in.SourceAgent); err != nil {
		return SaveDocumentResult{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE document_id = $1`, id); err != nil {
		return SaveDocumentResult{}, err
	}
	for _, ch := range in.Chunks {
		chunkID := fmt.Sprintf("%s#%d", id, ch.Section.Index)
		if _, err := tx.Exec(ctx, `
			INSERT INTO chunks (id, document_id, section_index, heading_path, content, embedding)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			chunkID, id, ch.Section.Index, ch.Section.HeadingPath, ch.Section.Content, optionalVector(ch.Embedding)); err != nil {
			return SaveDocumentResult{}, err
		}
	}

	if !in.SkipEnrichDoc {
		if err := enqueueJobTx(ctx, tx, "enrich_doc", "doc", id); err != nil {
			return SaveDocumentResult{}, err
		}
	}
	if !in.SkipRefreshBrief {
		if err := enqueueJobTx(ctx, tx, "refresh_brief", "project", in.Project); err != nil {
			return SaveDocumentResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SaveDocumentResult{}, err
	}
	return SaveDocumentResult{ID: id, Revision: nextRev, ChunkCount: len(in.Chunks)}, nil
}

// GetDocument returns a document and the Markdown body for revision (0 selects current_revision).
func (s *Store) GetDocument(ctx context.Context, id string, revision int) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.project, d.name, d.doc_type, d.title, d.summary, d.tags,
		       d.current_revision, r.revision, r.content_md, d.source_agent, d.created_at, d.updated_at
		FROM documents d
		JOIN document_revisions r
		  ON r.document_id = d.id
		 AND r.revision = CASE WHEN $2 > 0 THEN $2 ELSE d.current_revision END
		WHERE d.id = $1`, id, revision).Scan(
		&d.ID, &d.Project, &d.Name, &d.DocType, &d.Title, &d.Summary, &d.Tags,
		&d.CurrentRevision, &d.Revision, &d.ContentMD, &d.SourceAgent, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// GetChunk returns a single document section chunk by ID (<project>/<name>#<index>).
func (s *Store) GetChunk(ctx context.Context, chunkID string) (Chunk, error) {
	var ch Chunk
	err := s.pool.QueryRow(ctx, `
		SELECT id, document_id, section_index, heading_path, content, context_prefix, updated_at
		FROM chunks
		WHERE id = $1`, chunkID).Scan(
		&ch.ID, &ch.DocumentID, &ch.SectionIndex, &ch.HeadingPath, &ch.Content, &ch.ContextPrefix, &ch.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Chunk{}, ErrNotFound
	}
	return ch, err
}

// ListDocuments returns document metadata filtered by optional project and docType.
func (s *Store) ListDocuments(ctx context.Context, project, docType string, limit int) ([]Document, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, project, name, doc_type, title, summary, tags, current_revision, current_revision, '', source_agent, created_at, updated_at
		FROM documents
		WHERE ($1 = '' OR project = $1)
		  AND ($2 = '' OR doc_type = $2)
		ORDER BY updated_at DESC, id ASC
		LIMIT $3`, project, docType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(
			&d.ID, &d.Project, &d.Name, &d.DocType, &d.Title, &d.Summary, &d.Tags,
			&d.CurrentRevision, &d.Revision, &d.ContentMD, &d.SourceAgent, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDocument removes a document and cascades to revisions and chunks.
func (s *Store) DeleteDocument(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertNote creates or updates a memory note after secret scanning and queues enrichment.
func (s *Store) UpsertNote(ctx context.Context, in UpsertNoteInput) error {
	if in.ID == "" || in.Project == "" || in.NoteType == "" || in.Name == "" {
		return errors.New("id, project, note_type, and name are required")
	}
	if err := checkSecrets(in.Content); err != nil {
		return err
	}
	hash := hashContent(in.Content)
	updatedAt := time.Now().UTC()
	if in.UpdatedAt != nil && !in.UpdatedAt.IsZero() {
		updatedAt = in.UpdatedAt.UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO notes (id, project, note_type, name, description, content, content_hash, source_agent, embedding, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			project = EXCLUDED.project,
			note_type = EXCLUDED.note_type,
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			content = EXCLUDED.content,
			content_hash = EXCLUDED.content_hash,
			source_agent = EXCLUDED.source_agent,
			embedding = EXCLUDED.embedding,
			updated_at = EXCLUDED.updated_at`,
		in.ID, in.Project, in.NoteType, in.Name, in.Description, in.Content, hash, in.SourceAgent, optionalVector(in.Embedding), updatedAt); err != nil {
		return err
	}
	if err := enqueueJobTx(ctx, tx, "enrich_note", "note", in.ID); err != nil {
		return err
	}
	if err := enqueueJobTx(ctx, tx, "refresh_brief", "project", in.Project); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetNote returns a stored note by ID.
func (s *Store) GetNote(ctx context.Context, id string) (Note, error) {
	var n Note
	err := s.pool.QueryRow(ctx, `
		SELECT id, project, note_type, name, description, summary, tags, content, content_hash, source_agent, updated_at
		FROM notes
		WHERE id = $1`, id).Scan(
		&n.ID, &n.Project, &n.NoteType, &n.Name, &n.Description, &n.Summary, &n.Tags, &n.Content, &n.ContentHash, &n.SourceAgent, &n.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Note{}, ErrNotFound
	}
	return n, err
}

// ListNoteHashes returns a map of stored note ID to SHA-256 content hash.
func (s *Store) ListNoteHashes(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, content_hash FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hashes := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, err
		}
		hashes[id] = hash
	}
	return hashes, rows.Err()
}

// DeleteNotes deletes notes by ID.
func (s *Store) DeleteNotes(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM notes WHERE id = ANY($1)`, ids)
	return err
}

// ValidateBenchmarkInput verifies required fields and allowed status values.
func ValidateBenchmarkInput(in RecordBenchmarkInput) error {
	if strings.TrimSpace(in.Project) == "" || strings.TrimSpace(in.Suite) == "" ||
		strings.TrimSpace(in.Metric) == "" || strings.TrimSpace(in.Unit) == "" {
		return errors.New("project, suite, metric, and unit are required")
	}
	status := in.Status
	if status == "" {
		status = "info"
	}
	if !slices.Contains([]string{"baseline", "kept", "reverted", "info"}, status) {
		return fmt.Errorf("invalid benchmark status %q", status)
	}
	return nil
}

// RecordBenchmark inserts a typed benchmark run and computes delta_pct from baseline_id when omitted.
func (s *Store) RecordBenchmark(ctx context.Context, in RecordBenchmarkInput) (Benchmark, error) {
	if err := ValidateBenchmarkInput(in); err != nil {
		return Benchmark{}, err
	}
	if err := checkSecrets(in.Notes); err != nil {
		return Benchmark{}, err
	}
	status := in.Status
	if status == "" {
		status = "info"
	}
	recordedAt := time.Now().UTC()
	if in.RecordedAt != nil && !in.RecordedAt.IsZero() {
		recordedAt = in.RecordedAt.UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Benchmark{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	delta := in.DeltaPct
	if in.BaselineID != nil && delta == nil {
		var baseVal float64
		err := tx.QueryRow(ctx, `SELECT value FROM benchmarks WHERE id = $1`, *in.BaselineID).Scan(&baseVal)
		if errors.Is(err, pgx.ErrNoRows) {
			return Benchmark{}, fmt.Errorf("baseline %d: %w", *in.BaselineID, ErrNotFound)
		}
		if err != nil {
			return Benchmark{}, err
		}
		if baseVal != 0 {
			d := ((in.Value - baseVal) / baseVal) * 100.0
			delta = &d
		}
	}

	var b Benchmark
	err = tx.QueryRow(ctx, `
		INSERT INTO benchmarks (
			project, suite, metric, value, unit, baseline_id, delta_pct,
			git_commit, branch, environment, status, notes, document_id, source_agent, recorded_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING id, project, suite, metric, value, unit, baseline_id, delta_pct,
		          git_commit, branch, environment, status, notes, document_id, source_agent, recorded_at`,
		in.Project, in.Suite, in.Metric, in.Value, in.Unit, in.BaselineID, delta,
		in.GitCommit, in.Branch, in.Environment, status, in.Notes, in.DocumentID, in.SourceAgent, recordedAt).Scan(
		&b.ID, &b.Project, &b.Suite, &b.Metric, &b.Value, &b.Unit, &b.BaselineID, &b.DeltaPct,
		&b.GitCommit, &b.Branch, &b.Environment, &b.Status, &b.Notes, &b.DocumentID, &b.SourceAgent, &b.RecordedAt)
	if err != nil {
		return Benchmark{}, err
	}
	if err := enqueueJobTx(ctx, tx, "refresh_brief", "project", in.Project); err != nil {
		return Benchmark{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Benchmark{}, err
	}
	return b, nil
}

// QueryBenchmarks returns structured benchmark rows matching filter.
func (s *Store) QueryBenchmarks(ctx context.Context, filter BenchmarkFilter) ([]Benchmark, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	query := `
		SELECT id, project, suite, metric, value, unit, baseline_id, delta_pct,
		       git_commit, branch, environment, status, notes, document_id, source_agent, recorded_at
		FROM benchmarks
		WHERE ($1 = '' OR project = $1)
		  AND ($2 = '' OR suite = $2)
		  AND ($3 = '' OR metric = $3)
		  AND ($4 = '' OR branch = $4)
		  AND ($5 = '' OR git_commit = $5)
		  AND ($6 = '' OR status = $6)
		ORDER BY recorded_at DESC, id DESC
		LIMIT $7`
	if filter.LatestOnly {
		query = `
			SELECT DISTINCT ON (project, suite, metric)
			       id, project, suite, metric, value, unit, baseline_id, delta_pct,
			       git_commit, branch, environment, status, notes, document_id, source_agent, recorded_at
			FROM benchmarks
			WHERE ($1 = '' OR project = $1)
			  AND ($2 = '' OR suite = $2)
			  AND ($3 = '' OR metric = $3)
			  AND ($4 = '' OR branch = $4)
			  AND ($5 = '' OR git_commit = $5)
			  AND ($6 = '' OR status = $6)
			ORDER BY project, suite, metric, recorded_at DESC, id DESC
			LIMIT $7`
	}
	rows, err := s.pool.Query(ctx, query, filter.Project, filter.Suite, filter.Metric, filter.Branch, filter.GitCommit, filter.Status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchmarks(rows)
}

// CompareBenchmarks returns chronological benchmark runs for one project, suite, and metric.
func (s *Store) CompareBenchmarks(ctx context.Context, project, suite, metric string, limit int) ([]Benchmark, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, project, suite, metric, value, unit, baseline_id, delta_pct,
		       git_commit, branch, environment, status, notes, document_id, source_agent, recorded_at
		FROM benchmarks
		WHERE project = $1 AND suite = $2 AND metric = $3
		ORDER BY recorded_at ASC, id ASC
		LIMIT $4`, project, suite, metric, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBenchmarks(rows)
}

func scanBenchmarks(rows pgx.Rows) ([]Benchmark, error) {
	var out []Benchmark
	for rows.Next() {
		var b Benchmark
		if err := rows.Scan(
			&b.ID, &b.Project, &b.Suite, &b.Metric, &b.Value, &b.Unit, &b.BaselineID, &b.DeltaPct,
			&b.GitCommit, &b.Branch, &b.Environment, &b.Status, &b.Notes, &b.DocumentID, &b.SourceAgent, &b.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetProjectBrief returns the stored brief for project.
func (s *Store) GetProjectBrief(ctx context.Context, project string) (ProjectBrief, error) {
	var b ProjectBrief
	err := s.pool.QueryRow(ctx, `
		SELECT project, brief_md, source_hash, updated_at
		FROM project_briefs
		WHERE project = $1`, project).Scan(&b.Project, &b.BriefMD, &b.SourceHash, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectBrief{}, ErrNotFound
	}
	return b, err
}

// UpsertProjectBrief stores or replaces a project brief.
func (s *Store) UpsertProjectBrief(ctx context.Context, project, briefMD, sourceHash string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_briefs (project, brief_md, source_hash, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (project) DO UPDATE SET
			brief_md = EXCLUDED.brief_md,
			source_hash = EXCLUDED.source_hash,
			updated_at = NOW()`, project, briefMD, sourceHash)
	return err
}

// ReclaimRunningJobs resets orphaned running jobs left behind by a prior server shutdown or crash.
func (s *Store) ReclaimRunningJobs(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Mark obsolete running rows done when a newer pending or running job exists for the same target.
	if _, err := tx.Exec(ctx, `
		UPDATE enrichment_jobs r
		SET status = 'done', updated_at = NOW()
		WHERE r.status = 'running'
		  AND EXISTS (
			SELECT 1 FROM enrichment_jobs p
			WHERE p.id <> r.id
			  AND p.job_type = r.job_type
			  AND p.target_kind = r.target_kind
			  AND p.target_id = r.target_id
			  AND (p.status = 'pending' OR (p.status = 'running' AND p.id > r.id))
		  )`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE enrichment_jobs
		SET status = 'pending', run_after = NOW(), updated_at = NOW()
		WHERE status = 'running'`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimNextJob claims one ready pending enrichment job (or stale running job >15m) using SKIP LOCKED.
func (s *Store) ClaimNextJob(ctx context.Context) (*EnrichmentJob, error) {
	var j EnrichmentJob
	err := s.pool.QueryRow(ctx, `
		WITH next_job AS (
			SELECT id
			FROM enrichment_jobs
			WHERE (status = 'pending' AND run_after <= NOW())
			   OR (status = 'running' AND updated_at < NOW() - INTERVAL '15 minutes')
			ORDER BY run_after ASC, id ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE enrichment_jobs e
		SET status = 'running', attempts = e.attempts + 1, updated_at = NOW()
		FROM next_job
		WHERE e.id = next_job.id
		RETURNING e.id, e.job_type, e.target_kind, e.target_id, e.status, e.attempts, e.last_error, e.created_at`).Scan(
		&j.ID, &j.JobType, &j.TargetKind, &j.TargetID, &j.Status, &j.Attempts, &j.LastError, &j.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// CompleteJob marks an enrichment job done.
func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE enrichment_jobs SET status = 'done', last_error = '', updated_at = NOW() WHERE id = $1`, id)
	return err
}

// FailJob schedules a retry or marks a job failed when maxAttempts is reached.
func (s *Store) FailJob(ctx context.Context, id int64, errMsg string, retryDelay time.Duration) error {
	const maxAttempts = 5
	_, err := s.pool.Exec(ctx, `
		UPDATE enrichment_jobs e
		SET status = CASE
				WHEN EXISTS (
					SELECT 1 FROM enrichment_jobs p
					WHERE p.id <> e.id
					  AND p.job_type = e.job_type
					  AND p.target_kind = e.target_kind
					  AND p.target_id = e.target_id
					  AND p.status = 'pending'
				) THEN 'done'
				WHEN e.attempts >= $4 THEN 'failed'
				ELSE 'pending'
			END,
		    last_error = $2,
		    run_after = NOW() + ($3 * INTERVAL '1 millisecond'),
		    updated_at = NOW()
		WHERE e.id = $1`, id, errMsg, retryDelay.Milliseconds(), maxAttempts)
	return err
}

// UpdateDocumentEnrichment writes derived summary and tags onto documents.
func (s *Store) UpdateDocumentEnrichment(ctx context.Context, docID, summary string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE documents SET summary = $2, tags = $3, updated_at = NOW() WHERE id = $1`, docID, summary, tags)
	return err
}

// UpdateChunkEnrichment writes a contextual prefix and updated embedding onto one chunk.
func (s *Store) UpdateChunkEnrichment(ctx context.Context, chunkID, prefix string, emb []float32) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE chunks SET context_prefix = $2, embedding = $3, updated_at = NOW() WHERE id = $1`,
		chunkID, prefix, optionalVector(emb))
	return err
}

// UpdateNoteEnrichment writes derived summary and tags onto a memory note.
func (s *Store) UpdateNoteEnrichment(ctx context.Context, noteID, summary string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE notes SET summary = $2, tags = $3, updated_at = NOW() WHERE id = $1`, noteID, summary, tags)
	return err
}

// AddRecordLink records an advisory link between two knowledge records.
func (s *Store) AddRecordLink(ctx context.Context, link RecordLink) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO record_links (source_kind, source_id, target_kind, target_id, relation, note)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (source_kind, source_id, target_kind, target_id, relation)
		DO UPDATE SET note = EXCLUDED.note`,
		link.SourceKind, link.SourceID, link.TargetKind, link.TargetID, link.Relation, link.Note)
	return err
}

// ListChunks returns all chunks of a document ordered by section_index.
func (s *Store) ListChunks(ctx context.Context, docID string) ([]Chunk, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, document_id, section_index, heading_path, content, context_prefix, updated_at
		FROM chunks
		WHERE document_id = $1
		ORDER BY section_index ASC`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Chunk
	for rows.Next() {
		var ch Chunk
		if err := rows.Scan(&ch.ID, &ch.DocumentID, &ch.SectionIndex, &ch.HeadingPath, &ch.Content, &ch.ContextPrefix, &ch.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, rows.Err()
}

// ListNotes returns notes filtered by optional project and noteType ordered newest first.
func (s *Store) ListNotes(ctx context.Context, project, noteType string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, project, note_type, name, description, summary, tags, content, content_hash, source_agent, updated_at
		FROM notes
		WHERE ($1 = '' OR project = $1)
		  AND ($2 = '' OR note_type = $2)
		ORDER BY updated_at DESC, id ASC
		LIMIT $3`, project, noteType, limit)
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

// ListProjects returns distinct project names sorted alphabetically, excluding org-level roadmaps.
func (s *Store) ListProjects(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT project FROM documents WHERE name <> 'org-roadmap'
		UNION
		SELECT project FROM notes
		UNION
		SELECT project FROM benchmarks
		ORDER BY 1 ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListSupersededDocs returns a map of target document ID to the newest superseding document ID.
func (s *Store) ListSupersededDocs(ctx context.Context, docIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(docIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (target_id) target_id, source_id
		FROM record_links
		WHERE target_kind = 'doc'
		  AND relation IN ('possible_supersedes', 'supersedes')
		  AND target_id = ANY($1)
		ORDER BY target_id, created_at DESC, id DESC`, docIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var targetID, sourceID string
		if err := rows.Scan(&targetID, &sourceID); err != nil {
			return nil, err
		}
		out[targetID] = sourceID
	}
	return out, rows.Err()
}

// ListRecordLinks returns advisory links pointing to targetKind and targetID.
func (s *Store) ListRecordLinks(ctx context.Context, targetKind, targetID string) ([]RecordLink, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, source_kind, source_id, target_kind, target_id, relation, note, created_at
		FROM record_links
		WHERE target_kind = $1 AND target_id = $2
		ORDER BY created_at DESC, id DESC`, targetKind, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RecordLink
	for rows.Next() {
		var l RecordLink
		if err := rows.Scan(&l.ID, &l.SourceKind, &l.SourceID, &l.TargetKind, &l.TargetID, &l.Relation, &l.Note, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpdateNoteEmbedding writes an embedding vector onto an existing memory note.
func (s *Store) UpdateNoteEmbedding(ctx context.Context, noteID string, emb []float32) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE notes SET embedding = $2, updated_at = NOW() WHERE id = $1`, noteID, optionalVector(emb))
	return err
}

// StoreStats reports row counts across knowledge base tables and job statuses.
type StoreStats struct {
	Documents    int64
	Chunks       int64
	Notes        int64
	Benchmarks   int64
	GitHubRepos  int64
	GitHubItems  int64
	JobsByStatus map[string]int64
}

// Stats returns aggregate counts of records and enrichment jobs.
func (s *Store) Stats(ctx context.Context) (StoreStats, error) {
	stats := StoreStats{
		JobsByStatus: map[string]int64{
			"pending": 0,
			"running": 0,
			"done":    0,
			"failed":  0,
		},
	}
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM documents),
			(SELECT count(*) FROM chunks),
			(SELECT count(*) FROM notes),
			(SELECT count(*) FROM benchmarks),
			(SELECT count(*) FROM github_repos WHERE NOT is_archived),
			(SELECT count(*) FROM github_items)`).Scan(
		&stats.Documents, &stats.Chunks, &stats.Notes, &stats.Benchmarks,
		&stats.GitHubRepos, &stats.GitHubItems)
	if err != nil {
		return stats, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT status, count(*)
		FROM enrichment_jobs
		GROUP BY status`)
	if err != nil {
		return stats, err
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var cnt int64
		if err := rows.Scan(&status, &cnt); err != nil {
			return stats, err
		}
		stats.JobsByStatus[status] = cnt
	}
	return stats, rows.Err()
}
