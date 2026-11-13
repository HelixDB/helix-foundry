-- +goose Up
-- A person takes back an approval a load may have written (contract 4.16.0):
-- the load's copy of the release becomes a withdraw (foundry_load_decisions
-- kind 'withdraw', the release as it was sent), and a resend job sends it
-- with its document. The engine answers each withdraw 'withdrawn' or
-- 'not_written' (or refused), which the copy keeps in applied_status, as a
-- release's copy keeps 'written'. Those words are longer than the column's
-- eight characters, so it widens to sixteen. kind (VARCHAR(8)) takes
-- 'withdraw' as it is, and neither column has a CHECK. Nothing here changes
-- or deletes a row, and every value stored fits both widths.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the width stays, which an older binary reads
-- as before. It can't read a decision of kind withdraw: its Validate knows
-- none, so every Extract of a load whose list holds one fails, as does an
-- older engine sent one. So before going back to a binary older than this
-- one, once anyone has taken an approval back, a person must run on the
-- database:
--
--   DELETE FROM foundry_load_decisions WHERE kind='withdraw';
--
-- and rebuild every load an approval was taken back from, or make a load
-- built before it current: a withdraw that had not run leaves its approval
-- written there, with no decision left to take it back. The Down is for a
-- local database and deletes nothing: a status longer than eight characters
-- stops it until a person has run that DELETE.
ALTER TABLE foundry_load_decisions
  MODIFY COLUMN applied_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL;

-- +goose Down
ALTER TABLE foundry_load_decisions
  MODIFY COLUMN applied_status VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NULL;
