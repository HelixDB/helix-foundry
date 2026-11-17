-- +goose Up
-- Why the model failed a document: the engine's model failure class (quota,
-- rate_limited, unavailable, timeout, auth, ...) on a row failed with code
-- model, so a load says why its documents went unread, and Foundry reads
-- again on its own those whose failure passes (a provider's quota or a busy
-- provider). NULL on every other row, and on a model failure recorded before
-- the column: its class was not kept.
--
-- One nullable column and a check that holds of every existing row (all are
-- NULL): nothing here changes or deletes a row, and an older binary neither
-- reads nor writes it.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column stays. The Down is for a local
-- database.
ALTER TABLE foundry_load_extractions
  ADD COLUMN model_failure VARCHAR(24) NULL AFTER error_retryable,
  ADD CONSTRAINT foundry_load_extractions_model_failure_chk CHECK (model_failure IS NULL OR error_code = 'model');

-- +goose Down
ALTER TABLE foundry_load_extractions
  DROP CHECK foundry_load_extractions_model_failure_chk,
  DROP COLUMN model_failure;
