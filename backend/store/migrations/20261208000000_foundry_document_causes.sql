-- +goose Up
-- A connector's document carries the cause of the sync that wrote it, as
-- that sync's row keeps it (connector_syncs.started_by_automation_run_id and
-- automation_chain_depth): the automation run that started the sync and how
-- many automation runs led to it. A document_arrived trigger skips documents
-- its own automation's sync wrote, directly or through other automations,
-- and those past the depth a chain may reach, as sync_finished and
-- load_ready triggers skip syncs and loads. NULL is a document a person
-- uploaded, one a sync no automation started wrote, or one from before.
-- Columns only, both nullable: no row is changed, and a binary from before
-- writes and reads documents as it did, leaving them NULL.
ALTER TABLE foundry_documents
  ADD COLUMN started_by_automation_run_id CHAR(26) NULL,
  ADD COLUMN automation_chain_depth       INT      NULL;

-- +goose Down
ALTER TABLE foundry_documents
  DROP COLUMN automation_chain_depth,
  DROP COLUMN started_by_automation_run_id;
