-- +goose Up
-- A connector syncs by itself: next_sync_at is set when a sync ends, from
-- the plan's interval or the owner's longer one (sync_every_seconds; 0 means
-- manual only). A sync that fails for good pauses the schedule with its reason.
-- change_cursor is where the next change read starts, in the kind's own JSON.
ALTER TABLE connectors
  ADD COLUMN sync_every_seconds INT          NULL,
  ADD COLUMN next_sync_at       DATETIME(6)  NULL,
  ADD COLUMN schedule_paused    VARCHAR(255) NULL,
  ADD COLUMN change_cursor      JSON         NULL,
  ADD INDEX connectors_due_idx (next_sync_at);

-- origin is manual, schedule or settings; mode is full or changes; both checked in code.
ALTER TABLE connector_syncs
  ADD COLUMN origin VARCHAR(16) NOT NULL DEFAULT 'manual',
  ADD COLUMN mode   VARCHAR(16) NOT NULL DEFAULT 'changes';

-- A table's digest says whether a sync changed it; change_seq is its newest
-- batch; reconciled_at is its last full read.
ALTER TABLE connector_tables
  ADD COLUMN content_digest BINARY(32)  NULL,
  ADD COLUMN change_seq     BIGINT      NOT NULL DEFAULT 0,
  ADD COLUMN reconciled_at  DATETIME(6) NULL;

-- One sync's change to one table. kind is changes (a file of upserts and
-- deletes by key), replace (a keyless or first snapshot) or drop, checked in code.
-- A retried sync records a second batch for a table it wrote before failing:
-- what changed since its first attempt.
CREATE TABLE connector_change_batches (
  id           CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)      NOT NULL,
  connector_id CHAR(26)      NOT NULL,
  table_name   VARCHAR(255)  NOT NULL,
  seq          BIGINT        NOT NULL,
  sync_id      CHAR(26)      NOT NULL,
  kind         VARCHAR(16)   NOT NULL,
  s3_key       VARCHAR(1024) NULL,
  inserts      BIGINT        NOT NULL,
  updates      BIGINT        NOT NULL,
  deletes      BIGINT        NOT NULL,
  created_at   DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY connector_change_batches_seq_uq (connector_id, table_name, seq),
  INDEX connector_change_batches_sync_idx (sync_id, table_name),
  INDEX connector_change_batches_age_idx (created_at)
);

-- Snapshots a sync replaced or dropped. A sweep deletes each once it is past
-- the grace and no live load job pins it.
CREATE TABLE connector_replaced_snapshots (
  id           CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)      NOT NULL,
  connector_id CHAR(26)      NOT NULL,
  s3_key       VARCHAR(1024) NOT NULL,
  replaced_at  DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX connector_replaced_snapshots_age_idx (replaced_at)
);

-- The snapshot keys a load job's load reads, recorded on its first attempt so
-- every retry reads the same data.
ALTER TABLE foundry_load_jobs
  ADD COLUMN sources JSON NULL;

-- refresh_due_at is the earliest time an owed refresh may be queued.
ALTER TABLE foundry_ontologies
  ADD COLUMN auto_refresh   BOOLEAN     NOT NULL DEFAULT TRUE,
  ADD COLUMN refresh_due_at DATETIME(6) NULL,
  ADD INDEX foundry_ontologies_refresh_idx (refresh_due_at);

-- origin is user or refresh, checked in code; a refresh records the current
-- load it replaces.
ALTER TABLE foundry_loads
  ADD COLUMN origin            VARCHAR(16) NOT NULL DEFAULT 'user',
  ADD COLUMN refreshes_load_id CHAR(26)    NULL;

-- +goose Down
ALTER TABLE foundry_loads
  DROP COLUMN refreshes_load_id,
  DROP COLUMN origin;
ALTER TABLE foundry_ontologies
  DROP INDEX foundry_ontologies_refresh_idx,
  DROP COLUMN refresh_due_at,
  DROP COLUMN auto_refresh;
ALTER TABLE foundry_load_jobs DROP COLUMN sources;
DROP TABLE connector_replaced_snapshots;
DROP TABLE connector_change_batches;
ALTER TABLE connector_tables
  DROP COLUMN reconciled_at,
  DROP COLUMN change_seq,
  DROP COLUMN content_digest;
ALTER TABLE connector_syncs
  DROP COLUMN mode,
  DROP COLUMN origin;
ALTER TABLE connectors
  DROP INDEX connectors_due_idx,
  DROP COLUMN change_cursor,
  DROP COLUMN schedule_paused,
  DROP COLUMN next_sync_at,
  DROP COLUMN sync_every_seconds;
