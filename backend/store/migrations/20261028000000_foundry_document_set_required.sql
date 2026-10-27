-- +goose Up
-- foundry_documents.set_id (20261025000000) becomes NOT NULL once
-- backfill-foundry-document-sets has given every document a set. MySQL
-- refuses this while a row has none; a deploy that applies the schema to a
-- copy with no rows has nothing to refuse, so it runs the preflight
-- (migrations.Preflight) on the destination first. Numbered after every
-- migration that deploys before it: goose refuses a lower one.
ALTER TABLE foundry_documents MODIFY COLUMN set_id CHAR(26) NOT NULL;

-- +goose Down
ALTER TABLE foundry_documents MODIFY COLUMN set_id CHAR(26) NULL;
