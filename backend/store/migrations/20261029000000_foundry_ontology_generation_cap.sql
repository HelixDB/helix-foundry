-- +goose Up
-- Serves the hourly cap on a workspace's ontology generations: the backend
-- counts WHERE workspace_id=? AND created_at >= NOW(6) - INTERVAL 1 HOUR
-- before it queues another.
CREATE INDEX foundry_ontology_generations_workspace_idx ON foundry_ontology_generations (workspace_id, created_at);

-- +goose Down
DROP INDEX foundry_ontology_generations_workspace_idx ON foundry_ontology_generations;
