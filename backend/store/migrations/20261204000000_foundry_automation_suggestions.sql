-- +goose Up
-- Suggestions for a new automation, written once per ontology version by the
-- Assistant's worker (foundry-agent-worker) on the workspace's cheapest task
-- model, and read by the new automation page. A read queues the row
-- (queued), the worker claims it under a lease (running) and ends it with
-- the suggestions it kept (ready) or why it couldn't (failed). A failed row
-- is queued again by a read an hour after it failed. usd_micros is what
-- every attempt spent, charged_at when it was last charged: the Assistant's
-- monthly budget counts it beside people's turns. The table is new: nothing
-- existing changes. Rows go with their ontology, and with the workspace's
-- work when it is deleted.
CREATE TABLE foundry_automation_suggestions (
  ontology_id  CHAR(26)     NOT NULL,
  version_id   CHAR(26)     NOT NULL,
  workspace_id CHAR(26)     NOT NULL,
  status       VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- [{title, prompt, icon}], at most 4, as package suggest kept them.
  suggestions  JSON         NULL,
  failure      VARCHAR(255) NULL,
  usd_micros   BIGINT       NOT NULL DEFAULT 0,
  attempts     INT          NOT NULL DEFAULT 0,
  lease_owner  VARCHAR(64)  NULL,
  lease_until  DATETIME(6)  NULL,
  requested_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  charged_at   DATETIME(6)  NULL,
  PRIMARY KEY (ontology_id, version_id),
  INDEX foundry_automation_suggestions_workspace_idx (workspace_id, requested_at),
  INDEX foundry_automation_suggestions_queue_idx (status, lease_until),
  CONSTRAINT foundry_automation_suggestions_status_chk CHECK (status IN ('queued', 'running', 'ready', 'failed')),
  CONSTRAINT foundry_automation_suggestions_ready_chk CHECK ((status = 'ready') = (suggestions IS NOT NULL)),
  CONSTRAINT foundry_automation_suggestions_spend_chk CHECK (usd_micros >= 0)
);

-- +goose Down
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so they are dropped by the prefix they share.
SET @drop_automation_suggestion_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_automation_suggestions ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_automation_suggestions'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_automation_suggestions%'
);
PREPARE drop_automation_suggestion_chks FROM @drop_automation_suggestion_chks;
EXECUTE drop_automation_suggestion_chks;
DEALLOCATE PREPARE drop_automation_suggestion_chks;
DROP TABLE foundry_automation_suggestions;
