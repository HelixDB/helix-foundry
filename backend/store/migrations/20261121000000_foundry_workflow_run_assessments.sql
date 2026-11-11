-- +goose Up
-- A finished workflow run's assessment: the ontology agent's Assess of how
-- much what the agent read of the ontology made its answer, run by the
-- foundry-ask-worker on the workspace's question model. Finishing a run
-- queues it; an owner or admin may queue it again. It is queued until a
-- worker claims it (running, leased to lease_owner until lease_until), then
-- done with the engine's Assessment (assessment, its verdict apart for
-- counting, and left_out, the calls whose results didn't fit the model),
-- failed (failure, the engine's ModelFailure, or error alone for a failure
-- of Foundry's), or skipped when the workspace's month of assessments is
-- spent (error says so). usd_micros is what its attempts spent, counted
-- against foundry.workflows.assess.monthly_usd from charged_at. It goes
-- with its run.
CREATE TABLE foundry_workflow_run_assessments (
  run_id          CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id    CHAR(26)     NOT NULL,
  ontology_id     CHAR(26)     NOT NULL,
  workflow_id     CHAR(26)     NOT NULL,
  status          VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  attempts        INT          NOT NULL DEFAULT 0,
  lease_owner     VARCHAR(255) NULL,
  lease_until     DATETIME(6)  NULL,
  next_attempt_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  verdict         VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NULL,
  assessment      JSON         NULL,
  left_out        JSON         NULL,
  failure         VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NULL,
  error           TEXT         NULL,
  usd_micros      BIGINT       NOT NULL DEFAULT 0,
  charged_at      DATETIME(6)  NULL,
  queued_at       DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at     DATETIME(6)  NULL,
  -- Claims read due ones in this order.
  INDEX foundry_workflow_run_assessments_due_idx (status, next_attempt_at),
  INDEX foundry_workflow_run_assessments_spend_idx (workspace_id, charged_at),
  INDEX foundry_workflow_run_assessments_workflow_idx (workflow_id, verdict),
  CONSTRAINT foundry_workflow_run_assessments_status_chk CHECK (status IN ('queued', 'running', 'done', 'failed', 'skipped')),
  CONSTRAINT foundry_workflow_run_assessments_shape_chk CHECK (
    (status = 'running') = (lease_owner IS NOT NULL AND lease_until IS NOT NULL) AND
    (status = 'done') = (assessment IS NOT NULL AND verdict IS NOT NULL) AND
    (status IN ('done', 'failed', 'skipped')) = (finished_at IS NOT NULL) AND
    (status NOT IN ('failed', 'skipped') OR error IS NOT NULL) AND
    (verdict IS NULL OR verdict IN ('essential', 'helpful', 'marginal', 'unused', 'misleading'))
  ),
  CONSTRAINT foundry_workflow_run_assessments_spend_chk CHECK (usd_micros >= 0 AND attempts >= 0)
);

-- +goose Down
DROP TABLE foundry_workflow_run_assessments;
