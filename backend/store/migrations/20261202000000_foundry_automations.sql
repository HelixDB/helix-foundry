-- +goose Up
-- Automations: a person's graph of steps (package automation) that Foundry
-- runs as that person, its owner, when its trigger fires: a schedule, or the
-- end of Foundry's work it finds (a sync, a load, a document's arrival, a
-- generation). Three tables are new, and one CHECK of foundry_notifications
-- is widened for the two emails automations send; nothing else changes, and
-- no row is changed or deleted.

-- An automation of a workspace. spec is the automation package's canonical
-- JSON. revision counts every edit, so an edit names the one it was made
-- from; spec_revision counts the spec's own. An automation runs only while
-- its owner approved the spec it has (approved_spec_revision =
-- spec_revision): a spec edited by anyone or anything but the owner's own
-- click in Foundry waits for that click (needs_approval).
--
-- trigger_kind and trigger_target repeat the spec's trigger, for the worker:
-- an active schedule fires at next_fire_at; an active event trigger finds
-- the work that ended after its cursor (cursor_at, cursor_id) on its target,
-- checked_at saying when it last looked. failures counts the runs that
-- failed in a row: past foundry.automations.pause_after_failures it is
-- paused (paused_reason failures) and its owner emailed (notice_id, the
-- email's subject); while backoff_until is ahead it starts no run.
CREATE TABLE foundry_automations (
  id                     CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id           CHAR(26)     NOT NULL,
  owner_user_id          VARCHAR(64)  NOT NULL,
  name                   VARCHAR(120) NOT NULL,
  description            TEXT         NOT NULL,
  spec                   JSON         NOT NULL,
  revision               BIGINT       NOT NULL DEFAULT 1,
  spec_revision          BIGINT       NOT NULL DEFAULT 1,
  approved_spec_revision BIGINT       NULL,
  approved_at            DATETIME(6)  NULL,
  trigger_kind           VARCHAR(32)  NOT NULL,
  trigger_target         VARCHAR(128) NULL,
  status                 VARCHAR(16)  NOT NULL,
  paused_reason          VARCHAR(32)  NULL,
  paused_by_user_id      VARCHAR(64)  NULL,
  paused_at              DATETIME(6)  NULL,
  failures               INT          NOT NULL DEFAULT 0,
  backoff_until          DATETIME(6)  NULL,
  next_fire_at           DATETIME(6)  NULL,
  cursor_at              DATETIME(6)  NULL,
  cursor_id              VARCHAR(26)  NOT NULL DEFAULT '',
  checked_at             DATETIME(6)  NULL,
  notice_id              CHAR(26)     NULL,
  client_token           VARCHAR(64)  NULL,
  created_at             DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at             DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_automations_client_token_uq (workspace_id, owner_user_id, client_token),
  INDEX foundry_automations_list_idx (workspace_id, updated_at, id),
  INDEX foundry_automations_fire_idx (status, next_fire_at),
  INDEX foundry_automations_watch_idx (status, trigger_kind, checked_at),
  CONSTRAINT foundry_automations_revision_chk CHECK (revision > 0 AND spec_revision > 0 AND spec_revision <= revision AND failures >= 0),
  CONSTRAINT foundry_automations_status_chk CHECK (status IN ('needs_approval', 'active', 'paused')),
  CONSTRAINT foundry_automations_trigger_chk CHECK (
    trigger_kind IN ('schedule', 'sync_finished', 'load_ready', 'document_arrived', 'generation_finished', 'manual') AND
    (trigger_target IS NULL) = (trigger_kind IN ('schedule', 'manual'))
  ),
  CONSTRAINT foundry_automations_approved_chk CHECK (
    (status IN ('active', 'paused')) = (approved_spec_revision IS NOT NULL AND approved_spec_revision = spec_revision) AND
    (approved_spec_revision IS NULL) = (approved_at IS NULL)
  ),
  CONSTRAINT foundry_automations_paused_chk CHECK (
    (status = 'paused') = (paused_reason IS NOT NULL) AND (paused_reason IS NULL) = (paused_at IS NULL) AND
    (paused_reason IS NULL OR paused_reason IN ('person', 'failures', 'owner_lost_access')) AND
    (paused_reason = 'person') = (paused_by_user_id IS NOT NULL)
  ),
  CONSTRAINT foundry_automations_fire_chk CHECK ((next_fire_at IS NOT NULL) = (status = 'active' AND trigger_kind = 'schedule')),
  CONSTRAINT foundry_automations_cursor_chk CHECK (
    (cursor_at IS NOT NULL) = (status = 'active' AND trigger_kind IN ('sync_finished', 'load_ready', 'document_arrived', 'generation_finished'))
  )
);

-- One run of an automation: the spec it runs (its owner's approved spec when
-- it started; none for a skipped run, which runs nothing) and what fired it. trigger_key is unique per automation, so a
-- trigger delivered twice (a retried poll, a double click on Run now) makes
-- one run: schedule:<fire time>, <event kind>:<the row that ended>, or
-- manual:<client token>. An automation has at most one live run
-- (live_automation); an event found while one is live waits, and a schedule
-- that fires meanwhile records a skipped run. The foundry-automation-worker
-- claims a queued run under a lease and walks it a step at a time (package
-- automation's Spec.Next), handing a step that waits (an Assistant turn) back
-- to the queue until next_attempt_at. thread_id is the owner's Assistant
-- thread its Assistant steps run in; usd_micros what they spent. The
-- worker deletes runs that ended long ago (finished_at), a batch at a time.
-- The list of automations reads each one's newest run (history_idx) and
-- newest failure (outcome_idx), and the workspace's runs of the last days
-- (day_idx).
CREATE TABLE foundry_automation_runs (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  automation_id   CHAR(26)    NOT NULL,
  workspace_id    CHAR(26)    NOT NULL,
  owner_user_id   VARCHAR(64) NOT NULL,
  spec            JSON        NULL,
  spec_revision   BIGINT      NOT NULL,
  trigger_key     VARCHAR(96) NOT NULL,
  trigger_event   JSON        NOT NULL,
  status          VARCHAR(16) NOT NULL,
  error           TEXT        NULL,
  thread_id       CHAR(26)    NULL,
  lease_owner     VARCHAR(64) NULL,
  lease_until     DATETIME(6) NULL,
  attempts        INT         NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  usd_micros      BIGINT      NOT NULL DEFAULT 0,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at      DATETIME(6) NULL,
  finished_at     DATETIME(6) NULL,
  live_automation CHAR(26) AS (IF(status IN ('queued', 'running'), automation_id, NULL)) STORED,
  UNIQUE KEY foundry_automation_runs_live_uq (live_automation),
  UNIQUE KEY foundry_automation_runs_trigger_uq (automation_id, trigger_key),
  INDEX foundry_automation_runs_claim_idx (status, next_attempt_at),
  INDEX foundry_automation_runs_history_idx (automation_id, created_at, id),
  INDEX foundry_automation_runs_outcome_idx (automation_id, status, created_at),
  INDEX foundry_automation_runs_day_idx (workspace_id, created_at),
  INDEX foundry_automation_runs_ended_idx (finished_at),
  CONSTRAINT foundry_automation_runs_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'skipped')),
  CONSTRAINT foundry_automation_runs_finished_chk CHECK ((status IN ('queued', 'running')) = (finished_at IS NULL)),
  CONSTRAINT foundry_automation_runs_why_chk CHECK (status NOT IN ('failed', 'skipped', 'cancelled') OR error IS NOT NULL),
  CONSTRAINT foundry_automation_runs_spec_chk CHECK ((status = 'skipped') = (spec IS NULL)),
  CONSTRAINT foundry_automation_runs_lease_chk CHECK ((lease_owner IS NULL) = (lease_until IS NULL) AND (status = 'running' OR lease_owner IS NULL)),
  CONSTRAINT foundry_automation_runs_spend_chk CHECK (usd_micros >= 0 AND attempts >= 0)
);

-- One step of a run, from when it started. executing is an action step whose
-- call is in flight: found so by a later attempt, it ended unknown and is
-- never run again (failed, saying so). turn_id is an Assistant step's turn;
-- notification_id the email a notify step queued (foundry_notifications'
-- subject_id, kind automation_run).
CREATE TABLE foundry_automation_run_nodes (
  run_id          CHAR(26)    NOT NULL,
  node_id         VARCHAR(32) NOT NULL,
  status          VARCHAR(16) NOT NULL,
  output          JSON        NULL,
  error           TEXT        NULL,
  turn_id         CHAR(26)    NULL,
  notification_id CHAR(26)    NULL,
  usd_micros      BIGINT      NOT NULL DEFAULT 0,
  started_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at     DATETIME(6) NULL,
  PRIMARY KEY (run_id, node_id),
  UNIQUE KEY foundry_automation_run_nodes_notification_uq (notification_id),
  CONSTRAINT foundry_automation_run_nodes_status_chk CHECK (status IN ('running', 'executing', 'succeeded', 'failed', 'skipped', 'blocked')),
  CONSTRAINT foundry_automation_run_nodes_finished_chk CHECK ((status IN ('running', 'executing')) = (finished_at IS NULL)),
  CONSTRAINT foundry_automation_run_nodes_why_chk CHECK (status <> 'failed' OR error IS NOT NULL),
  CONSTRAINT foundry_automation_run_nodes_spend_chk CHECK (usd_micros >= 0)
);

-- The emails: automation_run, a notify step's (subject_id its
-- foundry_automation_run_nodes.notification_id), and automation_paused, the
-- owner's word that their automation paused itself after failing (subject_id
-- its foundry_automations.notice_id). ontology_questions stays, which
-- 20261129000000 adds. A Vitess-based MySQL host may rename CHECK
-- constraints, so the old one is dropped by prefix.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema. An older binary's notify worker skips an email of
-- a kind it doesn't know, saying so, and never claims an automation's run.
SET @drop_notifications_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_notifications ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_notifications'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_notifications_kind_chk%'
);
PREPARE drop_notifications_kind_chk FROM @drop_notifications_kind_chk;
EXECUTE drop_notifications_kind_chk;
DEALLOCATE PREPARE drop_notifications_kind_chk;

ALTER TABLE foundry_notifications
  ADD CONSTRAINT foundry_notifications_kind_chk CHECK (
    kind IN ('ontology_ready', 'sample_follow_up', 'ontology_questions', 'automation_run', 'automation_paused')
  );

-- +goose Down
DELETE FROM foundry_notifications WHERE kind IN ('automation_run', 'automation_paused');

SET @drop_notifications_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_notifications ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_notifications'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_notifications_kind_chk%'
);
PREPARE drop_notifications_kind_chk FROM @drop_notifications_kind_chk;
EXECUTE drop_notifications_kind_chk;
DEALLOCATE PREPARE drop_notifications_kind_chk;

ALTER TABLE foundry_notifications
  ADD CONSTRAINT foundry_notifications_kind_chk CHECK (kind IN ('ontology_ready', 'sample_follow_up', 'ontology_questions'));

DROP TABLE foundry_automation_run_nodes;
DROP TABLE foundry_automation_runs;
DROP TABLE foundry_automations;
