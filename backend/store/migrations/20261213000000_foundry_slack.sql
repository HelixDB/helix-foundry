-- +goose Up
-- Helix Foundry for Slack (L1): installs of Helix's "Helix Foundry" app, the
-- Slack users linked to Foundry users, which Founder thread a Slack thread
-- continues, what Slack sent (intake) and the Founder turns shown in Slack
-- (replies). Every table is new; foundry_agent_turns gains one column with a
-- default, which an older binary neither reads nor writes (its turn queries
-- name their columns).
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the tables and the column stay; nothing older
-- reads them. The Down is for a local database.

-- Slack installs: one live install per Slack team (live_team) and per
-- Foundry workspace (live_workspace), both keys. Identity mode is derived:
-- bot_credential_id (L2) NULL is per_person.
CREATE TABLE foundry_slack_installs (
  id                         CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id               CHAR(26)      NOT NULL,
  team_id                    VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  enterprise_id              VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  team_name                  VARCHAR(255)  NOT NULL,
  app_id                     VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  bot_user_id                VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- The bot token's scopes, comma-separated as oauth.v2.access names them.
  scopes                     VARCHAR(1024) NOT NULL,
  installed_by_user_id       VARCHAR(64)   NOT NULL,
  -- oauth.v2.access authed_user.id: who in Slack installed it (L3 owner link).
  installed_by_slack_user_id VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at                 DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- The last install or reinstall: an uninstall event from before it is stale.
  updated_at                 DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  revoked_at                 DATETIME(6)   NULL,
  -- When Slack was told (apps.uninstall) or told us. A team with a revoked,
  -- not-yet-uninstalled row can't be installed again.
  uninstalled_at             DATETIME(6)   NULL,
  live_team                  VARCHAR(32)   CHARACTER SET ascii COLLATE ascii_bin AS (IF(revoked_at IS NULL, team_id, NULL)) STORED,
  live_workspace             CHAR(26)      AS (IF(revoked_at IS NULL, workspace_id, NULL)) STORED,
  UNIQUE KEY foundry_slack_installs_team_uq (live_team),
  UNIQUE KEY foundry_slack_installs_workspace_uq (live_workspace),
  KEY foundry_slack_installs_sweep_idx (revoked_at, uninstalled_at),
  KEY foundry_slack_installs_team_idx (team_id, uninstalled_at),
  KEY foundry_slack_installs_workspace_idx (workspace_id),
  -- A click from another workspace of a Grid org finds the org's install.
  KEY foundry_slack_installs_enterprise_idx (enterprise_id, revoked_at)
);

-- A hosted install's bot token, sealed with the connectors' keyring under the
-- install id. A live hosted install always has one (the one insert path
-- writes both); it is deleted once Slack is told of the uninstall.
CREATE TABLE foundry_slack_install_tokens (
  install_id   CHAR(26)       NOT NULL PRIMARY KEY,
  token_sealed VARBINARY(512) NOT NULL,
  key_id       VARCHAR(64)    NOT NULL
);

-- An OAuth install in flight: its state's SHA-256, bound to the member who
-- started it and the workspace, for ten minutes, once.
CREATE TABLE foundry_slack_install_states (
  state_hash   CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  user_id      VARCHAR(64) NOT NULL,
  expires_at   DATETIME(6) NOT NULL,
  used_at      DATETIME(6) NULL,
  KEY foundry_slack_install_states_expiry_idx (expires_at),
  KEY foundry_slack_install_states_workspace_idx (workspace_id)
);

-- A Slack user of an install linked to a Foundry user, who the bot acts as.
CREATE TABLE foundry_slack_links (
  install_id    CHAR(26)    NOT NULL,
  slack_user_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  user_id       VARCHAR(64) NOT NULL,
  created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (install_id, slack_user_id),
  KEY foundry_slack_links_user_idx (install_id, user_id)
);

-- A one-time link the bot sent one Slack user, ephemerally: its token's SHA-256.
CREATE TABLE foundry_slack_link_tokens (
  token_hash    CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
  install_id    CHAR(26)    NOT NULL,
  slack_user_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  expires_at    DATETIME(6) NOT NULL,
  used_at       DATETIME(6) NULL,
  KEY foundry_slack_link_tokens_expiry_idx (expires_at),
  KEY foundry_slack_link_tokens_user_idx (install_id, slack_user_id)
);

-- Which Founder thread a Slack thread continues: per person (slack_user_id)
-- in per_person mode, one per Slack thread ('') in bot mode. Never overwritten
-- while its Founder thread lives. last_at is the last message: the thread's
-- Slack-born text is purged 30 days after it.
CREATE TABLE foundry_slack_threads (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  install_id      CHAR(26)    NOT NULL,
  channel_id      VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  thread_ts       VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slack_user_id   VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  agent_thread_id CHAR(26)    NOT NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_slack_threads_uq (install_id, channel_id, thread_ts, slack_user_id),
  KEY foundry_slack_threads_agent_idx (agent_thread_id),
  KEY foundry_slack_threads_last_idx (last_at)
);

-- What Slack sent, once each (dedupe_key: "e:"+event_id, or "i:"+SHA-256 of
-- team, user, action_ts and action_id). conversation: SHA-256 of (install,
-- channel, thread_ts[, user in per_person]) for messages, '' for clicks; the
-- claim takes only the oldest open row of a conversation. payload is the
-- parsed event (plus, after the first attempt, the rendered turn input, so a
-- retry sends the same message); NULL once done or failed.
CREATE TABLE foundry_slack_intake (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  install_id      CHAR(26)    NOT NULL,
  dedupe_key      CHAR(66)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind            VARCHAR(16) NOT NULL,
  conversation    CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  slack_user_id   VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  payload         JSON        NULL,
  status          VARCHAR(16) NOT NULL DEFAULT 'queued',
  attempts        INT         NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- A fresh random token per claim; every later write is conditional on it.
  lease_owner     CHAR(32)    CHARACTER SET ascii COLLATE ascii_bin NULL,
  lease_until     DATETIME(6) NULL,
  -- Waiting on a busy thread until this turn ends.
  waits_on_turn   CHAR(26)    NULL,
  error           TEXT        NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at     DATETIME(6) NULL,
  UNIQUE KEY foundry_slack_intake_dedupe_uq (dedupe_key),
  KEY foundry_slack_intake_claim_idx (status, next_attempt_at),
  KEY foundry_slack_intake_conv_idx (conversation, status, id),
  KEY foundry_slack_intake_user_idx (install_id, slack_user_id, created_at),
  KEY foundry_slack_intake_wait_idx (waits_on_turn),
  KEY foundry_slack_intake_created_idx (created_at)
);

-- A Founder turn the worker shows in Slack: where, how far (after_seq), the
-- message it updates, and what else it posted ({"q:<question>": ts,
-- "a:<action>": ts, "k:<asking turn>": ts}; "pending" before the post).
-- Held by one worker for the whole follow (lease renewed); every write fenced on lease_owner.
CREATE TABLE foundry_slack_replies (
  turn_id        CHAR(26)    NOT NULL PRIMARY KEY,
  install_id     CHAR(26)    NOT NULL,
  channel_id     VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  thread_ts      VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slack_user_id  VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  message_ts     VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  after_seq      INT         NOT NULL DEFAULT 0,
  posted         JSON        NULL,
  status         VARCHAR(16) NOT NULL DEFAULT 'following',
  lease_owner    CHAR(32)    CHARACTER SET ascii COLLATE ascii_bin NULL,
  lease_until    DATETIME(6) NULL,
  next_poll_at   DATETIME(6) NOT NULL,
  -- The reply message's next chat.update may not come before this (across replicas and restarts).
  next_update_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  created_at     DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at    DATETIME(6) NULL,
  KEY foundry_slack_replies_poll_idx (status, next_poll_at),
  KEY foundry_slack_replies_conv_idx (install_id, channel_id, thread_ts),
  KEY foundry_slack_replies_finished_idx (finished_at)
);

-- Where a turn's person reads it: the site, or Slack (its prompt and auto-runs differ).
ALTER TABLE foundry_agent_turns ADD COLUMN surface VARCHAR(16) NOT NULL DEFAULT 'site' AFTER kind;

-- +goose Down
ALTER TABLE foundry_agent_turns DROP COLUMN surface;
DROP TABLE foundry_slack_replies;
DROP TABLE foundry_slack_intake;
DROP TABLE foundry_slack_threads;
DROP TABLE foundry_slack_link_tokens;
DROP TABLE foundry_slack_links;
DROP TABLE foundry_slack_install_states;
DROP TABLE foundry_slack_install_tokens;
DROP TABLE foundry_slack_installs;
