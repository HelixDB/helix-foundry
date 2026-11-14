-- +goose Up
-- When a graph job's attempts began finding an index still building. Kept
-- across those uncounted retries and cleared by any other outcome, so the
-- worker gives up on a build still unfinished 2 hours after it was first
-- found building (foundry-graph-worker's indexWait), however long the job had
-- waited or retried before its indexes existed. NULL while none is pending.
--
-- One nullable column: nothing here changes or deletes a row, and an older
-- binary neither reads nor writes it.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column stays. The Down is for a local
-- database.
ALTER TABLE foundry_load_jobs
  ADD COLUMN indexes_pending_since DATETIME(6) NULL;

-- +goose Down
ALTER TABLE foundry_load_jobs
  DROP COLUMN indexes_pending_since;
