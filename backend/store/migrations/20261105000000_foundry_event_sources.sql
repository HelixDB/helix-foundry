-- +goose Up
-- One send to an event source: its pieces in one gzipped NDJSON object at
-- s3_key, written before this row. seq counts from 1 per source and is taken
-- under the source's connectors row lock, so seq order is commit order.
-- client_token makes a retry of a send return the first send. events is how
-- many pieces it holds, bytes the object's uncompressed size, and first_at
-- and last_at the earliest and latest piece time (occurred, else received).
-- Kept as long as the source lives.
CREATE TABLE foundry_event_batches (
  id           CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)      NOT NULL,
  connector_id CHAR(26)      NOT NULL,
  seq          BIGINT        NOT NULL,
  client_token VARCHAR(64)   CHARACTER SET ascii COLLATE ascii_bin NULL,
  s3_key       VARCHAR(1024) NOT NULL,
  events       INT           NOT NULL,
  bytes        BIGINT        NOT NULL,
  first_at     DATETIME(6)   NOT NULL,
  last_at      DATETIME(6)   NOT NULL,
  created_at   DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_event_batches_seq_uq (connector_id, seq),
  UNIQUE KEY foundry_event_batches_token_uq (connector_id, client_token),
  INDEX foundry_event_batches_day_idx (workspace_id, created_at)
);

-- A send-only credential for one event source: the SHA-256 of the token,
-- never the token. prefix is its first 12 characters, to tell tokens apart.
CREATE TABLE foundry_event_source_tokens (
  id           CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  connector_id CHAR(26)    NOT NULL,
  hash         CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  prefix       VARCHAR(16) NOT NULL,
  created_by   CHAR(26)    NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  revoked_at   DATETIME(6) NULL,
  last_used_at DATETIME(6) NULL,
  UNIQUE KEY foundry_event_source_tokens_hash_uq (hash),
  INDEX foundry_event_source_tokens_source_idx (connector_id, created_at)
);

-- +goose Down
DROP TABLE foundry_event_source_tokens;
DROP TABLE foundry_event_batches;
