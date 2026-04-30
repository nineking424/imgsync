BEGIN;

CREATE TABLE sniffer_state (
  source_id   TEXT PRIMARY KEY,
  last_run_ts TIMESTAMPTZ NOT NULL,
  last_run_pk TEXT,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE sniffer_state IS
  'Watermark + tie-break key per polled source. One row per source_id. v1 single sniffer pod, no advisory lock.';

COMMENT ON COLUMN sniffer_state.last_run_pk IS
  'Tie-break key from the most recent row at last_run_ts. NULL on first poll.';

INSERT INTO schema_migrations (version) VALUES ('0002_sniffer_state');

COMMIT;
