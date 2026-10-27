-- +goose Up
-- Questions asked of one load of a Foundry ontology.
-- The foundry-ask-worker claims a queued question, reserving its max_usd
-- against the workspace's monthly questions budget in the same transaction,
-- runs the ontology agent's read-only Ask over the load, and settles the
-- spend with the result. reserved_micros is held only while a question runs.
-- A question counts against the month its latest claim fell in (charged_at),
-- not the month it was asked in, so the month's commitment is
-- SUM(usd_micros + reserved_micros) of the questions charged in it.
CREATE TABLE foundry_questions (
  id                CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id      CHAR(26)    NOT NULL,
  ontology_id       CHAR(26)    NOT NULL,
  load_id           CHAR(26)    NOT NULL,
  -- The load's version, whose definition the agent is given.
  version_id        CHAR(26)    NOT NULL,
  -- The first question of the thread; a new thread's is its own id.
  thread_id         CHAR(26)    NOT NULL,
  question          TEXT        NOT NULL,
  focus             JSON        NULL,
  status            VARCHAR(16) NOT NULL,
  result            JSON        NULL,
  error             TEXT        NULL,
  client_token      VARCHAR(64) NULL,
  lease_owner       VARCHAR(64) NULL,
  lease_until       DATETIME(6) NULL,
  attempts          INT         NOT NULL DEFAULT 0,
  next_attempt_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  max_usd_micros    BIGINT      NOT NULL,
  reserved_micros   BIGINT      NOT NULL DEFAULT 0,
  usd_micros        BIGINT      NOT NULL DEFAULT 0,
  input_tokens      BIGINT      NOT NULL DEFAULT 0,
  output_tokens     BIGINT      NOT NULL DEFAULT 0,
  -- The seq of the newest event; events are numbered from 1.
  last_seq          INT         NOT NULL DEFAULT 0,
  asked_by_user_id  CHAR(26)    NULL,
  -- When the latest claim reserved: the question's spend and reservation are
  -- charged to the budget month this falls in. A question queued before a
  -- month and run in it is charged to the month that runs it, carrying what
  -- earlier attempts spent with it. NULL until a claim first reserves.
  charged_at        DATETIME(6) NULL,
  created_at        DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at        DATETIME(6) NULL,
  finished_at       DATETIME(6) NULL,
  UNIQUE KEY foundry_questions_client_token_uq (ontology_id, client_token),
  INDEX foundry_questions_claim_idx (status, next_attempt_at),
  INDEX foundry_questions_list_idx (ontology_id, created_at, id),
  INDEX foundry_questions_thread_idx (thread_id, created_at, id),
  INDEX foundry_questions_hour_idx (workspace_id, created_at),
  INDEX foundry_questions_spend_idx (workspace_id, charged_at),
  INDEX foundry_questions_running_idx (workspace_id, status),
  CONSTRAINT foundry_questions_status_chk CHECK (status IN ('queued', 'running', 'answered', 'cannot_answer', 'failed', 'cancelled')),
  CONSTRAINT foundry_questions_finished_chk CHECK ((status IN ('queued', 'running')) = (finished_at IS NULL)),
  CONSTRAINT foundry_questions_reserved_chk CHECK (reserved_micros >= 0 AND (status = 'running' OR reserved_micros = 0)),
  CONSTRAINT foundry_questions_spend_chk CHECK (max_usd_micros > 0 AND usd_micros >= 0),
  CONSTRAINT foundry_questions_charged_chk CHECK (charged_at IS NOT NULL OR (status <> 'running' AND usd_micros = 0))
);

-- What a question's run streamed, in order: the agent's events as it sent
-- them, numbered per question so a reader polls those after a seq.
CREATE TABLE foundry_question_events (
  question_id CHAR(26)    NOT NULL,
  seq         INT         NOT NULL,
  event       JSON        NOT NULL,
  created_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (question_id, seq)
);

-- +goose Down
DROP TABLE foundry_question_events;
DROP TABLE foundry_questions;
