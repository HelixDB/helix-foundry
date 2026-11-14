-- +goose Up
-- The jobs of a load that were cancelled while an attempt ran, whose attempt
-- may still be writing until its lease ends (the worker stops at its next
-- renewal, the engine at its next commit). A take-back reads them by load,
-- status and lease end, as the in-flight rule of a load's decisions
-- (store.foundryInFlight); cancelling a running job now keeps its lease end.
-- An index only: nothing here changes or deletes a row.
CREATE INDEX foundry_load_jobs_writing_idx ON foundry_load_jobs (load_id, status, lease_until);

-- +goose Down
DROP INDEX foundry_load_jobs_writing_idx ON foundry_load_jobs;
