-- +goose Up
-- An apply job brings an ontology's current load up to its tables' newest
-- snapshots in place, instead of a refresh building a new load. A
-- Vitess-based MySQL host may rename CHECK constraints, so the old one is
-- dropped by prefix.
SET @drop_load_jobs_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_jobs ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_jobs'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_jobs_kind_chk%'
);
PREPARE drop_load_jobs_kind_chk FROM @drop_load_jobs_kind_chk;
EXECUTE drop_load_jobs_kind_chk;
DEALLOCATE PREPARE drop_load_jobs_kind_chk;

ALTER TABLE foundry_load_jobs
  ADD CONSTRAINT foundry_load_jobs_kind_chk CHECK (kind IN ('load', 'extract', 'delete', 'apply'));

-- When an apply last brought the load's tables up to date; NULL until one has.
ALTER TABLE foundry_loads ADD COLUMN applied_at DATETIME(6) NULL;

-- The snapshot of each table a load holds: written when a load or an apply
-- has its tables in, from the snapshots its job pinned. A pin from before
-- syncs were recorded has no sync_id ('') or synced_at (NULL).
CREATE TABLE foundry_load_sources (
  load_id      CHAR(26)      NOT NULL,
  connector_id CHAR(26)      NOT NULL,
  table_name   VARCHAR(255)  NOT NULL,
  s3_key       VARCHAR(1024) NOT NULL,
  sync_id      CHAR(26)      NOT NULL,
  change_seq   BIGINT        NOT NULL,
  synced_at    DATETIME(6)   NULL,
  updated_at   DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (load_id, connector_id, table_name)
);

-- +goose Down
DROP TABLE foundry_load_sources;

ALTER TABLE foundry_loads DROP COLUMN applied_at;

-- An apply job fails the old check: delete those rows first.
DELETE FROM foundry_load_jobs WHERE kind = 'apply';

SET @drop_load_jobs_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_jobs ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_jobs'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_jobs_kind_chk%'
);
PREPARE drop_load_jobs_kind_chk FROM @drop_load_jobs_kind_chk;
EXECUTE drop_load_jobs_kind_chk;
DEALLOCATE PREPARE drop_load_jobs_kind_chk;

ALTER TABLE foundry_load_jobs
  ADD CONSTRAINT foundry_load_jobs_kind_chk CHECK (kind IN ('load', 'extract', 'delete'));
