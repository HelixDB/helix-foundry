-- +goose Up
-- The Assistant: a person's conversations with Foundry's own agent, which
-- reads the workspace through Foundry's tools and prepares actions the person
-- runs with a click. Every table is new: nothing existing changes.

-- A conversation, private to the user who started it: a person's chat, or
-- the thread an automation's run (foundry_automation_runs.thread_id) runs
-- its Assistant steps in, as its owner, which the person's list leaves out.
CREATE TABLE foundry_agent_threads (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  user_id            VARCHAR(64)  NOT NULL,
  kind               VARCHAR(16)  NOT NULL DEFAULT 'chat',
  title              VARCHAR(200) NOT NULL,
  -- Who runs its turns: Foundry's own worker ('api'), or a helper on the
  -- owner's machine driving a CLI the person signed in to.
  runtime            VARCHAR(16)  NOT NULL DEFAULT 'api',
  -- The helper's session of that CLI (Claude Code's session, Codex's
  -- thread), which its next turn resumes; NULL for 'api' and until the
  -- helper's first turn ends.
  runtime_session_id VARCHAR(128) NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  deleted_at         DATETIME(6)  NULL,
  INDEX foundry_agent_threads_list_idx (workspace_id, user_id, kind, updated_at, id),
  CONSTRAINT foundry_agent_threads_runtime_chk CHECK (runtime IN ('api', 'claude_code', 'codex')),
  CONSTRAINT foundry_agent_threads_kind_chk CHECK (kind IN ('chat', 'automation'))
);

-- One turn of a thread: a person's message, the result of an action the
-- person ran, which the agent goes on from, the person's answers to the
-- questions a turn asked them (helix_foundry_ask_person), or an automation's
-- Assistant step (automation_run_id, automation_node_id), whose input is its
-- instruction.
-- The foundry-agent-worker claims a queued turn, reserving its max_usd
-- against the workspace's monthly budget in the same transaction, as foundry_questions are claimed; a thread has at
-- most one live turn (live_thread). A turn has its thread's runtime, so each
-- runtime's worker claims only its own. A helper's turn runs on its owner's
-- subscription and spends nothing: its max_usd is 0, and only an 'api' turn's
-- is above it. An automation's turns are claimed under the workspace's
-- automations budget and running cap, apart from people's: unattended work
-- never spends the Assistant people use.
CREATE TABLE foundry_agent_turns (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id    CHAR(26)    NOT NULL,
  thread_id       CHAR(26)    NOT NULL,
  user_id         VARCHAR(64) NOT NULL,
  runtime         VARCHAR(16) NOT NULL,
  kind            VARCHAR(16) NOT NULL,
  -- The person's text, for a message; the action's id, for an action result;
  -- the asking turn's id, for answers; the step's instruction, with what
  -- fired the run, for an automation's.
  input           TEXT        NOT NULL,
  -- What the turn asked the person and ended waiting on: {questions, and
  -- generation {ontology_id, generation_id, round} when they are a
  -- generation's, relayed}; set once, by the turn's worker. NULL for a turn
  -- that asked nothing, and always for an automation's (no one is there).
  asked           JSON        NULL,
  -- An answers turn's answers, each {question_id, options, text}, checked
  -- against the asking turn's questions; [] leaves every one to the agent.
  answers         JSON        NULL,
  automation_run_id  CHAR(26)    NULL,
  automation_node_id VARCHAR(32) NULL,
  -- What the person was looking at: ids only.
  page_context    JSON        NULL,
  status          VARCHAR(16) NOT NULL,
  error           TEXT        NULL,
  client_token    VARCHAR(64) NULL,
  lease_owner     VARCHAR(64) NULL,
  lease_until     DATETIME(6) NULL,
  attempts        INT         NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  max_usd_micros  BIGINT      NOT NULL,
  reserved_micros BIGINT      NOT NULL DEFAULT 0,
  usd_micros      BIGINT      NOT NULL DEFAULT 0,
  -- What usd_micros holds of reservations whose attempt died unsettled: the
  -- month pays them, but they aren't the turn's own spend against its cap.
  lost_micros     BIGINT      NOT NULL DEFAULT 0,
  input_tokens    BIGINT      NOT NULL DEFAULT 0,
  output_tokens   BIGINT      NOT NULL DEFAULT 0,
  steps           INT         NOT NULL DEFAULT 0,
  -- The seq of the newest event; events are numbered from 1.
  last_seq        INT         NOT NULL DEFAULT 0,
  charged_at      DATETIME(6) NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at      DATETIME(6) NULL,
  finished_at     DATETIME(6) NULL,
  live_thread     CHAR(26) AS (IF(status IN ('queued', 'running'), thread_id, NULL)) STORED,
  -- The turn an answers turn answers: questions are answered once.
  answered_turn   CHAR(26) AS (IF(kind = 'answers', input, NULL)) STORED,
  UNIQUE KEY foundry_agent_turns_live_uq (live_thread),
  UNIQUE KEY foundry_agent_turns_answered_uq (answered_turn),
  UNIQUE KEY foundry_agent_turns_client_token_uq (workspace_id, user_id, client_token),
  INDEX foundry_agent_turns_claim_idx (runtime, status, next_attempt_at),
  INDEX foundry_agent_turns_thread_idx (thread_id, created_at, id),
  INDEX foundry_agent_turns_hour_idx (workspace_id, user_id, created_at),
  INDEX foundry_agent_turns_spend_idx (workspace_id, charged_at),
  INDEX foundry_agent_turns_running_idx (workspace_id, status),
  CONSTRAINT foundry_agent_turns_runtime_chk CHECK (runtime IN ('api', 'claude_code', 'codex')),
  CONSTRAINT foundry_agent_turns_kind_chk CHECK (kind IN ('message', 'action_result', 'answers', 'automation') AND
    (kind = 'automation') = (automation_run_id IS NOT NULL) AND (automation_run_id IS NULL) = (automation_node_id IS NULL)),
  CONSTRAINT foundry_agent_turns_asked_chk CHECK ((kind = 'answers') = (answers IS NOT NULL) AND (asked IS NULL OR kind <> 'automation')),
  CONSTRAINT foundry_agent_turns_status_chk CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
  CONSTRAINT foundry_agent_turns_finished_chk CHECK ((status IN ('queued', 'running')) = (finished_at IS NULL)),
  CONSTRAINT foundry_agent_turns_reserved_chk CHECK (reserved_micros >= 0 AND (status = 'running' OR reserved_micros = 0)),
  CONSTRAINT foundry_agent_turns_spend_chk CHECK (max_usd_micros >= 0 AND (runtime = 'api') = (max_usd_micros > 0) AND usd_micros >= lost_micros AND lost_micros >= 0),
  CONSTRAINT foundry_agent_turns_charged_chk CHECK (charged_at IS NOT NULL OR (status <> 'running' AND usd_micros = 0))
);

-- What a turn shows the person, in order, numbered per turn so a reader
-- polls those after a seq.
CREATE TABLE foundry_agent_events (
  turn_id    CHAR(26)    NOT NULL,
  seq        INT         NOT NULL,
  event      JSON        NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (turn_id, seq)
);

-- The transcript the model is sent, provider-neutral, numbered per thread.
CREATE TABLE foundry_agent_messages (
  thread_id  CHAR(26)    NOT NULL,
  seq        INT         NOT NULL,
  turn_id    CHAR(26)    NOT NULL,
  role       VARCHAR(16) NOT NULL,
  content    JSON        NOT NULL,
  est_tokens INT         NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (thread_id, seq),
  INDEX foundry_agent_messages_turn_idx (turn_id),
  CONSTRAINT foundry_agent_messages_role_chk CHECK (role IN ('user', 'assistant'))
);

-- An action the agent prepared and only the person who asked may run: its
-- canonical payload and hash, the names the server resolved for it, and its
-- one-time confirmation's token, sealed with the connectors' keyring. A
-- turn prepares the same payload once (its replays find the same action).
CREATE TABLE foundry_agent_actions (
  id                        CHAR(26)       NOT NULL PRIMARY KEY,
  turn_id                   CHAR(26)       NOT NULL,
  thread_id                 CHAR(26)       NOT NULL,
  workspace_id              CHAR(26)       NOT NULL,
  user_id                   VARCHAR(64)    NOT NULL,
  operation                 VARCHAR(64)    NOT NULL,
  target                    VARCHAR(255)   NOT NULL,
  payload                   JSON           NOT NULL,
  payload_hash              CHAR(64)       NOT NULL,
  display                   JSON           NOT NULL,
  target_named_by_user      BOOL           NOT NULL,
  confirmation_id           VARCHAR(64)    NOT NULL,
  confirmation_token_sealed VARBINARY(512) NOT NULL,
  confirmation_key_id       VARCHAR(64)    NOT NULL,
  status                    VARCHAR(16)    NOT NULL,
  result                    JSON           NULL,
  error                     TEXT           NULL,
  expires_at                DATETIME(6)    NOT NULL,
  decided_by                VARCHAR(64)    NULL,
  decided_at                DATETIME(6)    NULL,
  created_at                DATETIME(6)    NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_agent_actions_payload_uq (turn_id, payload_hash),
  INDEX foundry_agent_actions_thread_idx (thread_id, created_at, id),
  CONSTRAINT foundry_agent_actions_status_chk CHECK (status IN ('prepared', 'executing', 'succeeded', 'failed', 'unknown', 'expired', 'dismissed')),
  CONSTRAINT foundry_agent_actions_decided_chk CHECK ((status = 'prepared') = (decided_at IS NULL))
);

-- +goose Down
DROP TABLE foundry_agent_actions;
DROP TABLE foundry_agent_messages;
DROP TABLE foundry_agent_events;
DROP TABLE foundry_agent_turns;
DROP TABLE foundry_agent_threads;
