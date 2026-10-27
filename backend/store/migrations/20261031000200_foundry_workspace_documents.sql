-- +goose Up
-- Document sets and documents belong to the workspace (release 1 of the
-- three that move them). ontology_id no longer owns them: the
-- binary writes it on no new row, and the workspace-foundry-documents data
-- step nulls it on the old ones; release 3 drops it. A set's names are unique
-- per workspace among live sets, checked under the workspace's lock until
-- release 2 makes the index unique; a document's client token is unique per
-- workspace (the preflight, migrations.Preflight, checks that no workspace
-- of the destination repeats one).
-- The documents' ALTER goes first: it is the one rows can refuse (a
-- repeated client token), and one ALTER applies whole or not at all, so a
-- refused run leaves nothing applied and runs again once fixed.
ALTER TABLE foundry_documents
  MODIFY COLUMN ontology_id CHAR(26) NULL,
  ADD UNIQUE KEY foundry_documents_workspace_client_token_uq (workspace_id, client_token),
  ADD INDEX foundry_documents_workspace_sha256_idx (workspace_id, sha256),
  ADD INDEX foundry_documents_workspace_set_idx (workspace_id, set_id, created_at);

ALTER TABLE foundry_document_sets
  MODIFY COLUMN ontology_id CHAR(26) NULL,
  ADD INDEX foundry_document_sets_workspace_name_idx (workspace_id, name, live_key);

-- +goose Down
-- Refused while a row has no ontology: the binary that needs ontology_id
-- can't read those rows anyway. Roll back the binary, not the schema.
ALTER TABLE foundry_document_sets
  DROP INDEX foundry_document_sets_workspace_name_idx,
  MODIFY COLUMN ontology_id CHAR(26) NOT NULL;

ALTER TABLE foundry_documents
  DROP INDEX foundry_documents_workspace_set_idx,
  DROP INDEX foundry_documents_workspace_sha256_idx,
  DROP INDEX foundry_documents_workspace_client_token_uq,
  MODIFY COLUMN ontology_id CHAR(26) NOT NULL;
