-- +goose Up
-- What a restricted member's Founder thread ran under: the grant targets
-- they held when its turns ran, as JSON [{kind, connector_id, table,
-- set_id}], which they must all still hold to read it. NULL for a thread
-- made or run with the workspace's whole view, which a restricted member
-- never reads: every thread before this column.
ALTER TABLE foundry_agent_threads ADD COLUMN granted_targets JSON NULL AFTER runtime_session_id;

-- +goose Down
ALTER TABLE foundry_agent_threads DROP COLUMN granted_targets;
