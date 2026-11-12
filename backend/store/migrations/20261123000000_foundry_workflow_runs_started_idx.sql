-- +goose Up
-- A window of an ontology's runs by when they started, as workflow insights
-- read it (ListFoundryWorkflowInsightRuns).
CREATE INDEX foundry_workflow_runs_started_idx ON foundry_workflow_runs (ontology_id, started_at);

-- +goose Down
DROP INDEX foundry_workflow_runs_started_idx ON foundry_workflow_runs;
