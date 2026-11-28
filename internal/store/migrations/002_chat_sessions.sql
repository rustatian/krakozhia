CREATE TABLE IF NOT EXISTS chat_sessions (
    id TEXT PRIMARY KEY,
    source_agent TEXT NOT NULL,
    external_id TEXT NOT NULL,
    project TEXT NOT NULL,
    workspace_path TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    transcript_hash TEXT NOT NULL,
    raw_excerpt TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    document_id TEXT REFERENCES documents(id) ON DELETE SET NULL,
    distilled_hash TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS chat_sessions_project_idx ON chat_sessions (project, updated_at DESC);
