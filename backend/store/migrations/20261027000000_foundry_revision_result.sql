-- +goose Up
-- Revise input lives in the existing generation input JSON. This nullable
-- result holds cards/findings for previews and refusals without a version.
ALTER TABLE foundry_ontology_generations ADD COLUMN revise_result JSON NULL;

-- +goose Down
ALTER TABLE foundry_ontology_generations DROP COLUMN revise_result;
