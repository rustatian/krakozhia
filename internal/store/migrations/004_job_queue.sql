DROP INDEX IF EXISTS enrichment_jobs_active_uniq;
CREATE UNIQUE INDEX IF NOT EXISTS enrichment_jobs_active_uniq
    ON enrichment_jobs (job_type, target_kind, target_id)
    WHERE status = 'pending';
