-- imgsync v1 phase 1.5: transfer_jobs.status 단일 b-tree 인덱스
-- 목적: imgsync_jobs_in_status scrape SQL
--   SELECT status, COUNT(*) FROM transfer_jobs GROUP BY status
-- 가 succeeded/skipped 누적 행에서 풀 heap scan 으로 떨어지는 것을 방지.
-- 단일 컬럼 b-tree 면 PG 가 index-only scan + HashAggregate 로 처리한다.
-- Spec: docs/superpowers/specs/2026-05-05-monitoring-stack-integration-design.md §4

BEGIN;

CREATE INDEX transfer_jobs_status_idx ON transfer_jobs (status);

INSERT INTO schema_migrations (version) VALUES ('0003_jobs_status_index');

COMMIT;
