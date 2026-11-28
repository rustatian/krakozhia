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
	"github.com/pgvector/pgvector-go"
)

// GitHubRepo represents one tracked repository under an allowlisted GitHub owner.
type GitHubRepo struct {
	FullName       string     `json:"full_name"`
	Owner          string     `json:"owner"`
	Name           string     `json:"name"`
	Project        string     `json:"project"`
	Description    string     `json:"description"`
	IsPrivate      bool       `json:"is_private"`
	IsArchived     bool       `json:"is_archived"`
	HasDiscussions bool       `json:"has_discussions"`
	PushedAt       *time.Time `json:"pushed_at,omitempty"`
	LastSyncedAt   *time.Time `json:"last_synced_at,omitempty"`
	SyncHash       string     `json:"sync_hash"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ProjectRepoInput binds a local krakozhia project name to a GitHub owner/repo slug.
type ProjectRepoInput struct {
	Project      string `json:"project"`
	RepoFullName string `json:"repo_full_name"`
}

// GitHubMilestone represents one repository milestone synced from GitHub.
type GitHubMilestone struct {
	RepoFullName string     `json:"repo_full_name"`
	Number       int        `json:"number"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	State        string     `json:"state"`
	DueOn        *time.Time `json:"due_on,omitempty"`
	OpenIssues   int        `json:"open_issues"`
	ClosedIssues int        `json:"closed_issues"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// GitHubItem represents a synced GitHub issue, pull request, or discussion.
type GitHubItem struct {
	ID                 string    `json:"id"`
	RepoFullName       string    `json:"repo_full_name"`
	Owner              string    `json:"owner"`
	Project            string    `json:"project"`
	Kind               string    `json:"kind"` // issue, pr, discussion
	Number             int       `json:"number"`
	State              string    `json:"state"`
	IssueType          string    `json:"issue_type"`
	Title              string    `json:"title"`
	Body               string    `json:"body"`
	Labels             []string  `json:"labels"`
	MilestoneTitle     string    `json:"milestone_title"`
	ParentNumber       *int      `json:"parent_number,omitempty"`
	SubIssuesTotal     int       `json:"sub_issues_total"`
	SubIssuesCompleted int       `json:"sub_issues_completed"`
	URL                string    `json:"url"`
	ContentHash        string    `json:"content_hash"`
	GitHubCreatedAt    time.Time `json:"github_created_at"`
	GitHubUpdatedAt    time.Time `json:"github_updated_at"`
	SyncedAt           time.Time `json:"synced_at"`
}

// GitHubItemFilter narrows ListGitHubItems queries.
type GitHubItemFilter struct {
	Owner        string
	RepoFullName string
	Project      string
	Kind         string
	State        string
	Limit        int
}

// ParseGitHubRepo validates and splits a GitHub "owner/repo" slug.
func ParseGitHubRepo(fullName string) (owner, name string, ok bool) {
	s := strings.TrimSpace(fullName)
	s = strings.TrimSuffix(s, ".git")
	s = strings.Trim(s, "/")
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return "", "", false
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// UpsertProjectRepo binds a krakozhia project name to a GitHub owner/repo slug.
func (s *Store) UpsertProjectRepo(ctx context.Context, project, repoFullName string) error {
	project = strings.TrimSpace(project)
	owner, name, ok := ParseGitHubRepo(repoFullName)
	if project == "" || !ok {
		return errors.New("valid project and owner/repo full_name are required")
	}
	full := owner + "/" + name
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_repos (project, repo_full_name, owner, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (project, repo_full_name) DO UPDATE SET
			owner = EXCLUDED.owner,
			updated_at = NOW()`, project, full, owner)
	return err
}

func upsertProjectRepoTx(ctx context.Context, tx pgx.Tx, project, repoFullName string) error {
	project = strings.TrimSpace(project)
	owner, name, ok := ParseGitHubRepo(repoFullName)
	if project == "" || !ok {
		return nil
	}
	full := owner + "/" + name
	_, err := tx.Exec(ctx, `
		INSERT INTO project_repos (project, repo_full_name, owner, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (project, repo_full_name) DO UPDATE SET
			owner = EXCLUDED.owner,
			updated_at = NOW()`, project, full, owner)
	return err
}

// UpsertGitHubRepo stores repository metadata and registers its default project mapping.
func (s *Store) UpsertGitHubRepo(ctx context.Context, repo GitHubRepo) error {
	owner, name, ok := ParseGitHubRepo(repo.FullName)
	if !ok {
		return fmt.Errorf("invalid github repo %q", repo.FullName)
	}
	full := owner + "/" + name
	project := strings.TrimSpace(repo.Project)
	if project == "" {
		project = name
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO github_repos (
			full_name, owner, name, project, description, is_private, is_archived,
			has_discussions, pushed_at, last_synced_at, sync_hash, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NOW())
		ON CONFLICT (full_name) DO UPDATE SET
			owner = EXCLUDED.owner,
			name = EXCLUDED.name,
			project = EXCLUDED.project,
			description = EXCLUDED.description,
			is_private = EXCLUDED.is_private,
			is_archived = EXCLUDED.is_archived,
			has_discussions = EXCLUDED.has_discussions,
			pushed_at = COALESCE(EXCLUDED.pushed_at, github_repos.pushed_at),
			last_synced_at = COALESCE(EXCLUDED.last_synced_at, github_repos.last_synced_at),
			sync_hash = CASE WHEN EXCLUDED.sync_hash <> '' THEN EXCLUDED.sync_hash ELSE github_repos.sync_hash END,
			updated_at = NOW()`,
		full, owner, name, project, repo.Description, repo.IsPrivate, repo.IsArchived,
		repo.HasDiscussions, repo.PushedAt, repo.LastSyncedAt, repo.SyncHash); err != nil {
		return err
	}
	if err := upsertProjectRepoTx(ctx, tx, project, full); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetGitHubRepo fetches one tracked repository by owner/repo full name.
func (s *Store) GetGitHubRepo(ctx context.Context, fullName string) (GitHubRepo, error) {
	var r GitHubRepo
	err := s.pool.QueryRow(ctx, `
		SELECT full_name, owner, name, project, description, is_private, is_archived,
		       has_discussions, pushed_at, last_synced_at, sync_hash, updated_at
		FROM github_repos
		WHERE full_name = $1`, fullName).Scan(
		&r.FullName, &r.Owner, &r.Name, &r.Project, &r.Description, &r.IsPrivate, &r.IsArchived,
		&r.HasDiscussions, &r.PushedAt, &r.LastSyncedAt, &r.SyncHash, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubRepo{}, ErrNotFound
	}
	return r, err
}

// ListGitHubReposByOwner returns tracked non-archived repositories for an owner.
func (s *Store) ListGitHubReposByOwner(ctx context.Context, owner string) ([]GitHubRepo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT full_name, owner, name, project, description, is_private, is_archived,
		       has_discussions, pushed_at, last_synced_at, sync_hash, updated_at
		FROM github_repos
		WHERE owner = $1 AND NOT is_archived
		ORDER BY pushed_at DESC NULLS LAST, full_name ASC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGitHubRepos(rows)
}

// ListReposForProject returns tracked GitHub repositories linked to a krakozhia project.
func (s *Store) ListReposForProject(ctx context.Context, project string) ([]GitHubRepo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.full_name, r.owner, r.name, r.project, r.description, r.is_private,
		       r.is_archived, r.has_discussions, r.pushed_at, r.last_synced_at, r.sync_hash, r.updated_at
		FROM github_repos r
		LEFT JOIN project_repos pr ON pr.repo_full_name = r.full_name
		WHERE (pr.project = $1 OR r.project = $1 OR r.name = $1)
		  AND NOT r.is_archived
		ORDER BY r.full_name ASC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGitHubRepos(rows)
}

// ListProjectsForRepo returns all distinct krakozhia project names mapped to repoFullName.
func (s *Store) ListProjectsForRepo(ctx context.Context, repoFullName string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT project
		FROM (
			SELECT project FROM project_repos WHERE repo_full_name = $1
			UNION
			SELECT project FROM github_repos WHERE full_name = $1
		) p
		ORDER BY project ASC`, repoFullName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// ListProjectsForOwner returns all distinct krakozhia project names mapped to repositories under owner.
func (s *Store) ListProjectsForOwner(ctx context.Context, owner string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT project
		FROM (
			SELECT project FROM project_repos WHERE owner = $1
			UNION
			SELECT project FROM github_repos WHERE owner = $1 AND NOT is_archived
		) p
		ORDER BY project ASC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// ListOwnersForProject returns distinct GitHub owners associated with project.
func (s *Store) ListOwnersForProject(ctx context.Context, project string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT owner
		FROM (
			SELECT owner FROM project_repos WHERE project = $1
			UNION
			SELECT owner FROM github_repos WHERE (project = $1 OR name = $1) AND NOT is_archived
		) o
		ORDER BY owner ASC`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// ListGitHubOwners returns all distinct owners present in github_repos.
func (s *Store) ListGitHubOwners(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT owner
		FROM github_repos
		WHERE NOT is_archived
		ORDER BY owner ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanStrings(rows)
}

// UpsertGitHubMilestones replaces stored milestones for repoFullName.
func (s *Store) UpsertGitHubMilestones(ctx context.Context, repoFullName string, milestones []GitHubMilestone) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM github_milestones WHERE repo_full_name = $1`, repoFullName); err != nil {
		return err
	}
	for _, m := range milestones {
		updated := m.UpdatedAt
		if updated.IsZero() {
			updated = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO github_milestones (
				repo_full_name, number, title, description, state, due_on,
				open_issues, closed_issues, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			repoFullName, m.Number, m.Title, m.Description, m.State, m.DueOn,
			m.OpenIssues, m.ClosedIssues, updated); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListGitHubMilestones returns milestones for repoFullName ordered by state and due date.
func (s *Store) ListGitHubMilestones(ctx context.Context, repoFullName string) ([]GitHubMilestone, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT repo_full_name, number, title, description, state, due_on,
		       open_issues, closed_issues, updated_at
		FROM github_milestones
		WHERE repo_full_name = $1
		ORDER BY (state = 'open') DESC, due_on ASC NULLS LAST, number DESC`, repoFullName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GitHubMilestone
	for rows.Next() {
		var m GitHubMilestone
		if err := rows.Scan(
			&m.RepoFullName, &m.Number, &m.Title, &m.Description, &m.State, &m.DueOn,
			&m.OpenIssues, &m.ClosedIssues, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpsertGitHubItem inserts or updates one GitHub issue, pull request, or discussion.
// It enqueues embed_github_item and returns true whenever the item content changes.
func (s *Store) UpsertGitHubItem(ctx context.Context, item GitHubItem) (bool, error) {
	owner, name, ok := ParseGitHubRepo(item.RepoFullName)
	if !ok {
		return false, fmt.Errorf("invalid repo_full_name %q", item.RepoFullName)
	}
	full := owner + "/" + name
	kind := strings.ToLower(strings.TrimSpace(item.Kind))
	if kind != "issue" && kind != "pr" && kind != "discussion" {
		return false, fmt.Errorf("invalid github item kind %q", item.Kind)
	}
	if item.Number <= 0 || strings.TrimSpace(item.Title) == "" {
		return false, errors.New("number and title are required")
	}
	id := strings.TrimSpace(item.ID)
	if id == "" {
		id = fmt.Sprintf("%s#%s:%d", full, kind, item.Number)
	}
	project := strings.TrimSpace(item.Project)
	if project == "" {
		project = name
	}
	labels := cleanGitHubLabels(item.Labels)
	labelsText := strings.Join(labels, " ")
	hash := strings.TrimSpace(item.ContentHash)
	if hash == "" {
		hash = hashGitHubItem(item, labelsText)
	}
	createdAt := item.GitHubCreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	updatedAt := item.GitHubUpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prevHash string
	var hasEmbedding bool
	err = tx.QueryRow(ctx, `
		SELECT content_hash, (embedding IS NOT NULL)
		FROM github_items
		WHERE id = $1
		FOR UPDATE`, id).Scan(&prevHash, &hasEmbedding)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil && prevHash == hash {
		if !hasEmbedding {
			if err := enqueueJobTx(ctx, tx, "embed_github_item", "github", id); err != nil {
				return false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO github_items (
			id, repo_full_name, owner, project, kind, number, state, issue_type,
			title, body, labels, labels_text, milestone_title, parent_number,
			sub_issues_total, sub_issues_completed, url, content_hash,
			github_created_at, github_updated_at, synced_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, NOW())
		ON CONFLICT (id) DO UPDATE SET
			owner = EXCLUDED.owner,
			project = EXCLUDED.project,
			state = EXCLUDED.state,
			issue_type = EXCLUDED.issue_type,
			title = EXCLUDED.title,
			body = EXCLUDED.body,
			labels = EXCLUDED.labels,
			labels_text = EXCLUDED.labels_text,
			milestone_title = EXCLUDED.milestone_title,
			parent_number = EXCLUDED.parent_number,
			sub_issues_total = EXCLUDED.sub_issues_total,
			sub_issues_completed = EXCLUDED.sub_issues_completed,
			url = EXCLUDED.url,
			content_hash = EXCLUDED.content_hash,
			github_updated_at = EXCLUDED.github_updated_at,
			synced_at = NOW()`,
		id, full, owner, project, kind, item.Number, item.State, item.IssueType,
		item.Title, item.Body, labels, labelsText, item.MilestoneTitle, item.ParentNumber,
		item.SubIssuesTotal, item.SubIssuesCompleted, item.URL, hash,
		createdAt.UTC(), updatedAt.UTC()); err != nil {
		return false, err
	}
	if err := enqueueJobTx(ctx, tx, "embed_github_item", "github", id); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// GetGitHubItem retrieves one GitHub item by ID.
func (s *Store) GetGitHubItem(ctx context.Context, id string) (GitHubItem, error) {
	var it GitHubItem
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo_full_name, owner, project, kind, number, state, issue_type,
		       title, body, labels, milestone_title, parent_number,
		       sub_issues_total, sub_issues_completed, url, content_hash,
		       github_created_at, github_updated_at, synced_at
		FROM github_items
		WHERE id = $1`, id).Scan(
		&it.ID, &it.RepoFullName, &it.Owner, &it.Project, &it.Kind, &it.Number, &it.State, &it.IssueType,
		&it.Title, &it.Body, &it.Labels, &it.MilestoneTitle, &it.ParentNumber,
		&it.SubIssuesTotal, &it.SubIssuesCompleted, &it.URL, &it.ContentHash,
		&it.GitHubCreatedAt, &it.GitHubUpdatedAt, &it.SyncedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubItem{}, ErrNotFound
	}
	return it, err
}

// UpdateGitHubItemEmbedding stores the 1024-dim vector embedding for one GitHub item.
func (s *Store) UpdateGitHubItemEmbedding(ctx context.Context, id string, vec []float32) error {
	if len(vec) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE github_items
		SET embedding = $2
		WHERE id = $1`, id, pgvector.NewVector(vec))
	return err
}

// ListGitHubItems returns filtered GitHub items ordered by recency.
func (s *Store) ListGitHubItems(ctx context.Context, filter GitHubItemFilter) ([]GitHubItem, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.repo_full_name, i.owner, i.project, i.kind, i.number, i.state, i.issue_type,
		       i.title, i.body, i.labels, i.milestone_title, i.parent_number,
		       i.sub_issues_total, i.sub_issues_completed, i.url, i.content_hash,
		       i.github_created_at, i.github_updated_at, i.synced_at
		FROM github_items i
		WHERE ($1 = '' OR i.owner = $1)
		  AND ($2 = '' OR i.repo_full_name = $2)
		  AND ($3 = '' OR i.project = $3 OR EXISTS (
				SELECT 1 FROM project_repos pr
				WHERE pr.project = $3 AND pr.repo_full_name = i.repo_full_name
		      ))
		  AND ($4 = '' OR i.kind = $4)
		  AND ($5 = '' OR i.state = $5)
		ORDER BY (i.state IN ('open', 'draft')) DESC, i.sub_issues_total DESC, i.github_updated_at DESC, i.id ASC
		LIMIT $6`,
		filter.Owner, filter.RepoFullName, filter.Project, filter.Kind, filter.State, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GitHubItem
	for rows.Next() {
		var it GitHubItem
		if err := rows.Scan(
			&it.ID, &it.RepoFullName, &it.Owner, &it.Project, &it.Kind, &it.Number, &it.State, &it.IssueType,
			&it.Title, &it.Body, &it.Labels, &it.MilestoneTitle, &it.ParentNumber,
			&it.SubIssuesTotal, &it.SubIssuesCompleted, &it.URL, &it.ContentHash,
			&it.GitHubCreatedAt, &it.GitHubUpdatedAt, &it.SyncedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// EnqueueJob inserts a pending enrichment job unless an identical pending or running job exists.
func (s *Store) EnqueueJob(ctx context.Context, jobType, targetKind, targetID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO enrichment_jobs (job_type, target_kind, target_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (job_type, target_kind, target_id)
		WHERE status IN ('pending', 'running')
		DO NOTHING`, jobType, targetKind, targetID)
	return err
}

func scanGitHubRepos(rows pgx.Rows) ([]GitHubRepo, error) {
	var out []GitHubRepo
	for rows.Next() {
		var r GitHubRepo
		if err := rows.Scan(
			&r.FullName, &r.Owner, &r.Name, &r.Project, &r.Description, &r.IsPrivate, &r.IsArchived,
			&r.HasDiscussions, &r.PushedAt, &r.LastSyncedAt, &r.SyncHash, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanStrings(rows pgx.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func cleanGitHubLabels(in []string) []string {
	var out []string
	for _, l := range in {
		v := strings.ToLower(strings.TrimSpace(l))
		if v == "" || slices.Contains(out, v) {
			continue
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

func hashGitHubItem(it GitHubItem, labelsText string) string {
	h := sha256.New()
	parent := 0
	if it.ParentNumber != nil {
		parent = *it.ParentNumber
	}
	fmt.Fprintf(h, "%s|%s|%d|%s|%s|%s|%s|%s|%d|%d|%d|%s",
		it.RepoFullName, it.Kind, it.Number, it.State, it.IssueType,
		it.Title, labelsText, it.MilestoneTitle, parent,
		it.SubIssuesTotal, it.SubIssuesCompleted, it.Body)
	return hex.EncodeToString(h.Sum(nil))
}
