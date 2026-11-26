-- +goose Up
-- What a Slack install's shared bot reads (model.SlackBotScope): 'workspace',
-- everything an unrestricted member reads, or 'sources', only what Settings ›
-- Access grants its credential. Meaningful only while bot_credential_id is
-- set; per_person rows keep the default. Every bot from before reads as
-- 'workspace'. No CHECK: the store parses it and refuses any other value.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column stays. The rollback floor never
-- reads it and serves every bot as a grantee of its source grants, which
-- only narrows what it reads. The Down is for a local database.
ALTER TABLE foundry_slack_installs
  ADD COLUMN bot_scope VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'workspace' AFTER bot_credential_id;

-- +goose Down
ALTER TABLE foundry_slack_installs DROP COLUMN bot_scope;
