-- +goose Up
-- The key a resolve decision's name is looked up by (contract 4.23.0): a
-- hash of its first words, as pkg/namewords reads them (namewords.Key). An
-- alias acts only on a document whose text writes its name, so each Extract
-- sends the aliases its documents write, found through
-- foundry_load_decisions_names_idx by the keys of the documents' words
-- (namewords.Text.Keys), at a cost bounded by the documents rather than by
-- the ontology. The settle writes it on foundry_review_decisions for a
-- resolve that chose an entity (an alias), and a load's copy takes it from
-- there. NULL for "not in data", for a decision of another kind, and for a
-- resolve settled before this; the graph worker fills a load's NULLs, and
-- the saved decisions they copy, before it reads the load's aliases
-- (store.FillFoundryLoadNameKeys), so no data step is needed. A change to
-- how namewords reads words sets the column NULL again in a migration of its
-- own.
--
-- Additive: one nullable column on each table and one index. Nothing here
-- changes or deletes a row; an older binary neither reads nor writes either
-- column (its load copy names its columns, so a copy it makes is NULL and
-- filled by the worker).
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the columns and index stay. The Down is for a
-- local database.
ALTER TABLE foundry_review_decisions
  ADD COLUMN name_key BIGINT NULL;
ALTER TABLE foundry_load_decisions
  ADD COLUMN name_key BIGINT NULL,
  ADD INDEX foundry_load_decisions_names_idx (load_id, kind, name_key);

-- +goose Down
ALTER TABLE foundry_load_decisions
  DROP INDEX foundry_load_decisions_names_idx,
  DROP COLUMN name_key;
ALTER TABLE foundry_review_decisions
  DROP COLUMN name_key;
