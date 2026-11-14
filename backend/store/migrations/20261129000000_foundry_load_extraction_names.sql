-- +goose Up
-- What a load's runs did with each name decision that met a document
-- (contract 4.22.0): the document event's names, one entry per resolve
-- decision of the document and per alias that met one of its names, as JSON
-- [{decision, type, norm_name, decision_id, outcome, reason}] (NULL for
-- none); and resolved_carried, the document's entities an alias linked. An
-- event of a document the load holds already adds to names rather than
-- replace them (model.MergeFoundryDocumentNames).
--
-- Additive: one nullable column and one with a default. Nothing here
-- changes or deletes a row, and an older binary neither reads nor writes
-- either.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the columns stay. The Down is for a local
-- database.
ALTER TABLE foundry_load_extractions
  ADD COLUMN resolved_carried INT NOT NULL DEFAULT 0,
  ADD COLUMN names JSON NULL;

-- +goose Down
ALTER TABLE foundry_load_extractions
  DROP COLUMN resolved_carried,
  DROP COLUMN names;
