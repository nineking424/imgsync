-- imgsync: drop redundant single-column index transfer_jobs_trace_id_idx.
-- transfer_jobs_trace_id_idx (trace_id) is a leading-column prefix of the
-- UNIQUE(trace_id, dst) constraint index transfer_jobs_trace_id_dst_key, so
-- the only production lookup (WHERE trace_id=$1 AND dst=$2) is already fully
-- covered by the composite. The single-column index just adds write cost.
-- See GitHub issue #34.

BEGIN;

DROP INDEX IF EXISTS transfer_jobs_trace_id_idx;

INSERT INTO schema_migrations (version) VALUES ('0004_drop_trace_id_index');

COMMIT;
