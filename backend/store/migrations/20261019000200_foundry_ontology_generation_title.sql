-- +goose Up
-- A one-line title of the generation's goal, written when GenerateOntology
-- queues it. Code keeps it to 60 characters; NULL for a generation without a
-- goal and for those queued before titles.
ALTER TABLE foundry_ontology_generations ADD COLUMN title VARCHAR(120) NULL AFTER goal;

-- +goose Down
ALTER TABLE foundry_ontology_generations DROP COLUMN title;
