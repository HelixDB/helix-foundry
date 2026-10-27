-- +goose Up
-- What work no person started (refreshes, and extracts no one asked for)
-- spent extracting a document set's documents in one UTC month, and whether
-- a sync of the set's stream left pieces unwritten that month because the
-- set's allowance (foundry.events.max_system_usd_per_source_month) or the
-- workspace's share (max_system_usd_per_workspace_month, summed through
-- the workspace index) was spent; the next sync with room reads in full.
CREATE TABLE foundry_document_set_spend (
  set_id       CHAR(26) NOT NULL,
  month        DATE     NOT NULL, -- first day of the UTC month
  workspace_id CHAR(26) NOT NULL,
  usd_micros   BIGINT   NOT NULL DEFAULT 0,
  skipped      BOOLEAN  NOT NULL DEFAULT FALSE,
  PRIMARY KEY (set_id, month),
  INDEX foundry_document_set_spend_workspace_idx (workspace_id, month)
);

-- +goose Down
DROP TABLE foundry_document_set_spend;
