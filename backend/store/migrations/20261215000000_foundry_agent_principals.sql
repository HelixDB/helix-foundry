-- +goose Up
-- Founder's threads, turns and actions belong to a principal: a person
-- (principal_kind 'user', user_id their Helix id) or a Slack install's bot
-- ('service_credential', user_id the credential's id). Every row before
-- this is a person's. A bot's action holds no confirmation: an owner or
-- admin who approves it runs it as themselves (decided_by, which the table
-- has had since it was made), so its confirmation columns are NULL.
-- principal_kind has no CHECK: the store parses it and refuses any other.
--
-- Rollback. A deploy request carries schema only; an older binary names its
-- columns and never writes a bot's row, so the columns stay. The Down is for
-- a local database.
ALTER TABLE foundry_agent_threads
  ADD COLUMN principal_kind VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'user' AFTER user_id;
ALTER TABLE foundry_agent_turns
  ADD COLUMN principal_kind VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'user' AFTER user_id;
ALTER TABLE foundry_agent_actions
  ADD COLUMN principal_kind VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'user' AFTER user_id,
  MODIFY confirmation_id VARCHAR(64) NULL,
  MODIFY confirmation_token_sealed VARBINARY(512) NULL,
  MODIFY confirmation_key_id VARCHAR(64) NULL;
-- Bot mode: the workspace service credential the install's bot acts as,
-- whose token no one holds; NULL is per_person. Hosted's credential pages
-- read it by id (IsFoundrySlackBotCredential).
ALTER TABLE foundry_slack_installs
  ADD COLUMN bot_credential_id CHAR(26) NULL AFTER scopes,
  ADD KEY foundry_slack_installs_bot_idx (bot_credential_id);

-- Who has spoken to the bot in a bot-mode Slack thread (thread_id is the
-- foundry_slack_threads mapping's id, slack_user_id ''): an unmentioned
-- reply reaches the bot only from one of them. Deleted with the mapping.
CREATE TABLE foundry_slack_thread_people (
  thread_id     CHAR(26)    NOT NULL,
  slack_user_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  created_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (thread_id, slack_user_id)
);

-- +goose Down
DROP TABLE foundry_slack_thread_people;
ALTER TABLE foundry_slack_installs DROP KEY foundry_slack_installs_bot_idx, DROP COLUMN bot_credential_id;
-- The confirmation columns stay NULL-able: a bot's action has none.
ALTER TABLE foundry_agent_actions DROP COLUMN principal_kind;
ALTER TABLE foundry_agent_turns DROP COLUMN principal_kind;
ALTER TABLE foundry_agent_threads DROP COLUMN principal_kind;
