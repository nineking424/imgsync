DROP INDEX IF EXISTS transfer_jobs_status_idx;
DELETE FROM schema_migrations WHERE version = '0003_jobs_status_index';
