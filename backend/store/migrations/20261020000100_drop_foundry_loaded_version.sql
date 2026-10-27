-- +goose Up
-- The current load (20261020000000) replaces the loaded version: a load
-- records its version, and uploads are extracted into the current load. Run
-- `make dev-scale-down` before migrating dev: the running binary reads the
-- dropped column.
ALTER TABLE foundry_ontologies DROP COLUMN loaded_version_id;

-- +goose Down
ALTER TABLE foundry_ontologies ADD COLUMN loaded_version_id CHAR(26) NULL AFTER current_version_id;
