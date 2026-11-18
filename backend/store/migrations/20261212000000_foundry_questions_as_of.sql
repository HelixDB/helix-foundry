-- +goose Up
-- The day a question is answered as of (contract 4.28.0's `valid_at`): the
-- ask worker sends it, and the engine reads entities as they were then by
-- valid time, history and changes to it, and leaves out documents dated after
-- it, refusing the reads that can only show the load as it is now. NULL for a
-- question answered as of now, as every question asked before the column is.
--
-- One nullable column: nothing here changes or deletes a row, and an older
-- binary neither reads nor writes it (its question queries name their
-- columns).
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column stays. A question queued with a day
-- and run by an older worker is answered as of now: finish or cancel those
-- first (SELECT id FROM foundry_questions WHERE status IN ('queued',
-- 'running') AND as_of IS NOT NULL). The Down is for a local database.
ALTER TABLE foundry_questions
  ADD COLUMN as_of DATE NULL AFTER focus;

-- +goose Down
ALTER TABLE foundry_questions
  DROP COLUMN as_of;
