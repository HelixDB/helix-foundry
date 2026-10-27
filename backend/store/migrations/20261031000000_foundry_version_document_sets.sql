-- +goose Up
-- A version's scope: the ids of the document sets its loads read, as a JSON
-- array, written once with the version. NULL is a version written before
-- scopes, which reads its ontology's live sets until the
-- workspace-foundry-documents data step stores them; an empty array reads
-- none.
ALTER TABLE foundry_ontology_versions ADD COLUMN document_set_ids JSON NULL;

-- +goose Down
ALTER TABLE foundry_ontology_versions DROP COLUMN document_set_ids;
