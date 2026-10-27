-- +goose Up
CREATE TABLE connector_syncs (
  id                   CHAR(26)    NOT NULL PRIMARY KEY,
  connector_id         CHAR(26)    NOT NULL,
  workspace_id         CHAR(26)    NOT NULL,
  status               VARCHAR(16) NOT NULL,
  -- 1 while queued or running, NULL after: at most one live sync per connector.
  active_key           TINYINT     NULL,
  lease_owner          VARCHAR(64) NULL,
  lease_until          DATETIME(6) NULL,
  attempts             INT         NOT NULL DEFAULT 0,
  next_attempt_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  error                TEXT        NULL,
  rows_synced          BIGINT      NULL,
  requested_by_user_id CHAR(26)    NULL,
  created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at           DATETIME(6) NULL,
  finished_at          DATETIME(6) NULL,
  UNIQUE KEY connector_syncs_active_uq (connector_id, active_key),
  INDEX connector_syncs_claim_idx (status, next_attempt_at),
  INDEX connector_syncs_history_idx (connector_id, created_at),
  CONSTRAINT connector_syncs_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'failed'))
);

CREATE TABLE connector_tables (
  id           CHAR(26)      NOT NULL PRIMARY KEY,
  connector_id CHAR(26)      NOT NULL,
  workspace_id CHAR(26)      NOT NULL,
  table_name   VARCHAR(255)  NOT NULL,
  columns      JSON          NOT NULL,
  primary_key  JSON          NOT NULL,
  foreign_keys JSON          NOT NULL,
  s3_key       VARCHAR(1024) NOT NULL,
  row_count    BIGINT        NOT NULL,
  sync_id      CHAR(26)      NOT NULL,
  synced_at    DATETIME(6)   NOT NULL,
  UNIQUE KEY connector_tables_name_uq (connector_id, table_name)
);

-- +goose Down
DROP TABLE connector_tables;
DROP TABLE connector_syncs;
