CREATE INDEX transfer_jobs_trace_id_idx ON transfer_jobs (trace_id);
DELETE FROM schema_migrations WHERE version = '0004_drop_trace_id_index';
