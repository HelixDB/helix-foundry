-- +goose Up
-- Whether a withdraw takes back an approval its load had recorded written
-- (contract 4.16.0). The engine takes back every withdraw it is sent before it
-- reads any document, at no cost, and says what it did only in that
-- document's event: a run that stops first says nothing, and the withdraw
-- sent again reads not_written. With this the backend knows such a
-- not_written of an approval the load had written is the take-back done
-- (withdrawn), and moves the
-- document's counts back, a fact fewer and a review more, as it moved them
-- forward when the approval was written. NULL for every other decision.
--
-- One nullable column: nothing here changes or deletes a row, and an older
-- binary neither reads nor writes it.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column stays. The rollback past 4.16.0 is
-- unchanged (20261125000000's note: delete the withdraw rows first). The
-- Down is for a local database.
ALTER TABLE foundry_load_decisions
  ADD COLUMN was_written BOOLEAN NULL;

-- +goose Down
ALTER TABLE foundry_load_decisions
  DROP COLUMN was_written;
