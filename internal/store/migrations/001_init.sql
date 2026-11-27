CREATE TABLE IF NOT EXISTS documents (
    id TEXT PRIMARY KEY,
    project TEXT NOT NULL,
    name TEXT NOT NULL,
    doc_type TEXT NOT NULL,
    title TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    current_revision INTEGER NOT NULL DEFAULT 1,
    source_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project, name)
);

CREATE INDEX IF NOT EXISTS documents_project_idx ON documents (project);
CREATE INDEX IF NOT EXISTS documents_tags_idx ON documents USING GIN (tags);

CREATE TABLE IF NOT EXISTS document_revisions (
    id BIGSERIAL PRIMARY KEY,
    document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    revision INTEGER NOT NULL,
    content_md TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    source_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (document_id, revision)
);

CREATE TABLE IF NOT EXISTS chunks (
    id TEXT PRIMARY KEY,
    document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    section_index INTEGER NOT NULL,
    heading_path TEXT NOT NULL,
    content TEXT NOT NULL,
    context_prefix TEXT NOT NULL DEFAULT '',
    embedding vector(1024),
    tsv_en tsvector GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(context_prefix, '') || ' ' || heading_path || ' ' || content)
    ) STORED,
    tsv_simple tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', coalesce(context_prefix, '') || ' ' || heading_path || ' ' || content)
    ) STORED,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (document_id, section_index)
);

CREATE INDEX IF NOT EXISTS chunks_document_id_idx ON chunks (document_id);
CREATE INDEX IF NOT EXISTS chunks_tsv_en_idx ON chunks USING GIN (tsv_en);
CREATE INDEX IF NOT EXISTS chunks_tsv_simple_idx ON chunks USING GIN (tsv_simple);

CREATE TABLE IF NOT EXISTS notes (
    id TEXT PRIMARY KEY,
    project TEXT NOT NULL,
    note_type TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    tags TEXT[] NOT NULL DEFAULT '{}',
    content TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    source_agent TEXT NOT NULL DEFAULT '',
    embedding vector(1024),
    tsv_en tsvector GENERATED ALWAYS AS (
        to_tsvector('english', name || ' ' || description || ' ' || coalesce(summary, '') || ' ' || content)
    ) STORED,
    tsv_simple tsvector GENERATED ALWAYS AS (
        to_tsvector('simple', name || ' ' || description || ' ' || coalesce(summary, '') || ' ' || content)
    ) STORED,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS notes_project_idx ON notes (project);
CREATE INDEX IF NOT EXISTS notes_type_idx ON notes (note_type);
CREATE INDEX IF NOT EXISTS notes_tags_idx ON notes USING GIN (tags);
CREATE INDEX IF NOT EXISTS notes_tsv_en_idx ON notes USING GIN (tsv_en);
CREATE INDEX IF NOT EXISTS notes_tsv_simple_idx ON notes USING GIN (tsv_simple);

CREATE TABLE IF NOT EXISTS benchmarks (
    id BIGSERIAL PRIMARY KEY,
    project TEXT NOT NULL,
    suite TEXT NOT NULL,
    metric TEXT NOT NULL,
    value DOUBLE PRECISION NOT NULL,
    unit TEXT NOT NULL,
    baseline_id BIGINT REFERENCES benchmarks(id) ON DELETE SET NULL,
    delta_pct DOUBLE PRECISION,
    git_commit TEXT NOT NULL DEFAULT '',
    branch TEXT NOT NULL DEFAULT '',
    environment TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'info',
    notes TEXT NOT NULL DEFAULT '',
    document_id TEXT REFERENCES documents(id) ON DELETE SET NULL,
    source_agent TEXT NOT NULL DEFAULT '',
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS benchmarks_project_metric_idx ON benchmarks (project, suite, metric, recorded_at DESC);
CREATE INDEX IF NOT EXISTS benchmarks_commit_idx ON benchmarks (git_commit);

CREATE TABLE IF NOT EXISTS project_briefs (
    project TEXT PRIMARY KEY,
    brief_md TEXT NOT NULL,
    source_hash TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS record_links (
    id BIGSERIAL PRIMARY KEY,
    source_kind TEXT NOT NULL,
    source_id TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    target_id TEXT NOT NULL,
    relation TEXT NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (source_kind, source_id, target_kind, target_id, relation)
);

CREATE INDEX IF NOT EXISTS record_links_target_idx ON record_links (target_kind, target_id);

CREATE TABLE IF NOT EXISTS enrichment_jobs (
    id BIGSERIAL PRIMARY KEY,
    job_type TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    target_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    run_after TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS enrichment_jobs_poll_idx ON enrichment_jobs (status, run_after, id);
CREATE UNIQUE INDEX IF NOT EXISTS enrichment_jobs_active_uniq
    ON enrichment_jobs (job_type, target_kind, target_id)
    WHERE status IN ('pending', 'running');
