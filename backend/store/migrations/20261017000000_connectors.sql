-- +goose Up
CREATE TABLE connectors (
  id                 CHAR(26)        NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)        NOT NULL,
  kind               VARCHAR(32)     NOT NULL,
  name               VARCHAR(120)    NOT NULL,
  config             JSON            NOT NULL,
  secret_ciphertext  VARBINARY(4096) NOT NULL,
  secret_key_id      VARCHAR(32)     NOT NULL,
  secret_hint        VARCHAR(64)     NOT NULL,
  created_by_user_id CHAR(26)        NULL,
  created_at         DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at         DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at         DATETIME(6)     NULL,
  INDEX connectors_workspace_idx (workspace_id, deleted_at, created_at)
);

-- +goose Down
DROP TABLE connectors;
