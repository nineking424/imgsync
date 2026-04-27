-- imgsync v1 initial schema
-- Spec: design doc rev 4, section "Database Schema"

BEGIN;

CREATE TYPE job_status AS ENUM (
    'pending',
    'leased',
    'succeeded',
    'skipped',
    'dead'
);

CREATE TABLE transfer_jobs (
    id            BIGSERIAL PRIMARY KEY,
    trace_id      TEXT        NOT NULL,
    src           TEXT        NOT NULL,
    dst           TEXT        NOT NULL,
    src_protocol  TEXT        NOT NULL,
    dst_protocol  TEXT        NOT NULL,
    payload       JSONB       NOT NULL DEFAULT '{}'::JSONB,
    status        job_status  NOT NULL DEFAULT 'pending',
    attempts      INT         NOT NULL DEFAULT 0,
    max_attempts  INT         NOT NULL DEFAULT 5,
    locked_at     TIMESTAMPTZ,
    locked_by     TEXT,
    next_run_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT transfer_jobs_trace_id_dst_key UNIQUE (trace_id, dst)
);

CREATE INDEX transfer_jobs_pending_idx
    ON transfer_jobs (next_run_at, id)
    WHERE status = 'pending';

CREATE INDEX transfer_jobs_leased_idx
    ON transfer_jobs (locked_at)
    WHERE status = 'leased';

CREATE INDEX transfer_jobs_trace_id_idx
    ON transfer_jobs (trace_id);

CREATE TABLE transfer_events (
    id        BIGSERIAL PRIMARY KEY,
    trace_id  TEXT        NOT NULL,
    job_id    BIGINT      NOT NULL REFERENCES transfer_jobs(id) ON DELETE CASCADE,
    ts        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    status    TEXT        NOT NULL,
    detail    JSONB       NOT NULL DEFAULT '{}'::JSONB,
    CONSTRAINT transfer_events_status_check
        CHECK (status IN ('enqueue','lease','success','skip','fail','expire','dead'))
);

CREATE INDEX transfer_events_job_id_idx ON transfer_events (job_id);
CREATE INDEX transfer_events_trace_id_ts_idx ON transfer_events (trace_id, ts);

CREATE TABLE schema_migrations (
    version    TEXT        PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO schema_migrations (version) VALUES ('0001_initial');

COMMIT;
