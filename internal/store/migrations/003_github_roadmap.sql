CREATE TABLE IF NOT EXISTS github_repos (
    full_name TEXT PRIMARY KEY,
    owner TEXT NOT NULL,
    name TEXT NOT NULL,
    project TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    is_private BOOLEAN NOT NULL DEFAULT FALSE,
    is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    has_discussions BOOLEAN NOT NULL DEFAULT FALSE,
    pushed_at TIMESTAMPTZ,
    last_synced_at TIMESTAMPTZ,
    sync_hash TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS github_repos_owner_idx ON github_repos (owner, is_archived);
CREATE INDEX IF NOT EXISTS github_repos_project_idx ON github_repos (project);

CREATE TABLE IF NOT EXISTS project_repos (
    project TEXT NOT NULL,
    repo_full_name TEXT NOT NULL,
    owner TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project, repo_full_name)
);

CREATE INDEX IF NOT EXISTS project_repos_repo_idx ON project_repos (repo_full_name);
CREATE INDEX IF NOT EXISTS project_repos_owner_idx ON project_repos (owner, project);

CREATE TABLE IF NOT EXISTS github_milestones (
    repo_full_name TEXT NOT NULL REFERENCES github_repos(full_name) ON DELETE CASCADE,
    number INTEGER NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'open',
    due_on TIMESTAMPTZ,
    open_issues INTEGER NOT NULL DEFAULT 0,
    closed_issues INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (repo_full_name, number)
);

CREATE TABLE IF NOT EXISTS github_items (
    id TEXT PRIMARY KEY,
    repo_full_name TEXT NOT NULL REFERENCES github_repos(full_name) ON DELETE CASCADE,
    owner TEXT NOT NULL,
    project TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('issue', 'pr', 'discussion')),
    number INTEGER NOT NULL,
    state TEXT NOT NULL,
    issue_type TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    labels TEXT[] NOT NULL DEFAULT '{}',
    labels_text TEXT NOT NULL DEFAULT '',
    milestone_title TEXT NOT NULL DEFAULT '',
    parent_number INTEGER,
    sub_issues_total INTEGER NOT NULL DEFAULT 0,
    sub_issues_completed INTEGER NOT NULL DEFAULT 0,
    url TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL,
    embedding vector(1024),
    tsv_en tsvector GENERATED ALWAYS AS (
        to_tsvector('english', repo_full_name || ' ' || issue_type || ' ' || milestone_title || ' ' || labels_text || ' ' || title || ' ' || body)
    ) STORED,
    tsv_simple tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', repo_full_name || ' ' || issue_type || ' ' || milestone_title || ' ' || labels_text || ' ' || title || ' ' || body)
    ) STORED,
    github_created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    github_updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    synced_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS github_items_repo_state_idx ON github_items (repo_full_name, state, github_updated_at DESC);
CREATE INDEX IF NOT EXISTS github_items_owner_state_idx ON github_items (owner, state, github_updated_at DESC);
CREATE INDEX IF NOT EXISTS github_items_project_state_idx ON github_items (project, state, github_updated_at DESC);
CREATE INDEX IF NOT EXISTS github_items_labels_idx ON github_items USING GIN (labels);
CREATE INDEX IF NOT EXISTS github_items_tsv_en_idx ON github_items USING GIN (tsv_en);
CREATE INDEX IF NOT EXISTS github_items_tsv_simple_idx ON github_items USING GIN (tsv_simple);
