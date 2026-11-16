-- +goose Up
-- Automations' workflows: Foundry makes one workflow of each ontology an
-- automation uses (automation_id), the first time a run of it uses the
-- ontology or when its owner approves steps that name it, and starts one run
-- of it for each of the automation's runs that uses the ontology
-- (automation_run_id, its client token too), finished when that run ends.
-- Deleting the automation keeps them as history: automation_id then names
-- no automation.
ALTER TABLE foundry_workflows
  ADD COLUMN automation_id CHAR(26) NULL,
  ADD UNIQUE KEY foundry_workflows_automation_uq (ontology_id, automation_id),
  ADD INDEX foundry_workflows_automation_idx (automation_id);

-- A run Foundry started for an automation is its owner's, and records only
-- the calls that name it with its key (automation_key), which Foundry gives
-- the automation's Assistant steps and never shows anyone.
ALTER TABLE foundry_workflow_runs
  ADD COLUMN automation_run_id CHAR(26) NULL,
  ADD COLUMN automation_key CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  ADD INDEX foundry_workflow_runs_automation_run_idx (automation_run_id),
  ADD CONSTRAINT foundry_workflow_runs_automation_chk CHECK (automation_run_id IS NULL OR started_by_user_id IS NOT NULL),
  ADD CONSTRAINT foundry_workflow_runs_automation_key_chk CHECK ((automation_run_id IS NULL) = (automation_key IS NULL));

-- An ontology an automation's run used that Foundry couldn't make the
-- automation's workflow of, and why: workflows_full, the ontology held as
-- many workflows as it may. The run went on untracked. The row goes once the
-- workflow is made, and with the automation.
CREATE TABLE foundry_automation_untracked (
  automation_id CHAR(26)    NOT NULL,
  ontology_id   CHAR(26)    NOT NULL,
  workspace_id  CHAR(26)    NOT NULL,
  reason        VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  at            DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (automation_id, ontology_id),
  CONSTRAINT foundry_automation_untracked_reason_chk CHECK (reason IN ('workflows_full'))
);

-- +goose Down
DROP TABLE foundry_automation_untracked;
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so the two are dropped by the prefix they share.
SET @drop_workflow_run_automation_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_workflow_runs ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_workflow_runs'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_workflow_runs_automation%'
);
PREPARE drop_workflow_run_automation_chks FROM @drop_workflow_run_automation_chks;
EXECUTE drop_workflow_run_automation_chks;
DEALLOCATE PREPARE drop_workflow_run_automation_chks;
ALTER TABLE foundry_workflow_runs
  DROP INDEX foundry_workflow_runs_automation_run_idx,
  DROP COLUMN automation_key,
  DROP COLUMN automation_run_id;
ALTER TABLE foundry_workflows
  DROP INDEX foundry_workflows_automation_idx,
  DROP INDEX foundry_workflows_automation_uq,
  DROP COLUMN automation_id;
