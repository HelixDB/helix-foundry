-- +goose Up
-- A workspace's model provider (Settings > AI models). A config is written
-- once (only its probe columns change) and holds its API key sealed until it
-- is neither active nor pending; the ask worker probes queued configs; the
-- newest activation is the setting, and an activation with no config is the
-- deployment's default. New tables, plus an expand-only change of the
-- generations table so a generation can say which provider drafted it.
CREATE TABLE foundry_ai_configs (
  id                CHAR(26)        NOT NULL PRIMARY KEY,
  workspace_id      CHAR(26)        NOT NULL,
  kind              VARCHAR(32)     NOT NULL,
  -- {"v":1,"provider":{...},"models":{...}}; never a secret.
  settings          JSON            NOT NULL,
  secret_ciphertext VARBINARY(4096) NULL,
  secret_key_id     VARCHAR(32)     NULL,
  secret_hint       VARCHAR(64)     NULL,
  probe_state       VARCHAR(16)     NOT NULL,
  probe_attempts    INT             NOT NULL DEFAULT 0,
  probe_owner       VARCHAR(64)     NULL,
  probe_until       DATETIME(6)     NULL,
  probe_failure     VARCHAR(32)     NULL,
  probe_detail      VARCHAR(32)     NULL,
  -- Per check: role, outcome, reasoning, seconds; Ollama's digest and
  -- context. Never a model's text.
  probe_report      JSON            NULL,
  created_by        CHAR(26)        NOT NULL,
  created_at        DATETIME(6)     NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  probed_at         DATETIME(6)     NULL,
  INDEX foundry_ai_configs_workspace_idx (workspace_id, created_at, id),
  INDEX foundry_ai_configs_probe_idx (probe_state, probe_until, created_at),
  CONSTRAINT foundry_ai_configs_secret_chk CHECK ((secret_ciphertext IS NULL) = (secret_key_id IS NULL)),
  CONSTRAINT foundry_ai_configs_state_chk CHECK (probe_state IN ('queued', 'running', 'passed', 'failed', 'superseded')),
  CONSTRAINT foundry_ai_configs_running_chk CHECK (probe_state <> 'running' OR (probe_owner IS NOT NULL AND probe_until IS NOT NULL)),
  CONSTRAINT foundry_ai_configs_failed_chk CHECK ((probe_state = 'failed') = (probe_failure IS NOT NULL)),
  CONSTRAINT foundry_ai_configs_attempts_chk CHECK (probe_attempts >= 0)
);

-- Append-only: a workspace's newest row (created_at, id) is its setting.
-- config_id NULL is the deployment's default.
CREATE TABLE foundry_ai_activations (
  id           CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  config_id    CHAR(26)    NULL,
  user_id      CHAR(26)    NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX foundry_ai_activations_workspace_idx (workspace_id, created_at, id)
);

-- The provider kind that drafted a generation, and room for model names
-- longer than Claude's. NULL for every generation before.
ALTER TABLE foundry_ontology_generations
  ADD COLUMN model_provider VARCHAR(32) NULL AFTER model,
  MODIFY model VARCHAR(255) NULL;

-- +goose Down
ALTER TABLE foundry_ontology_generations
  DROP COLUMN model_provider,
  MODIFY model VARCHAR(64) NULL;
DROP TABLE foundry_ai_activations;
DROP TABLE foundry_ai_configs;
