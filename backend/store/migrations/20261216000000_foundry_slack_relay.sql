-- +goose Up
-- Helix Foundry for Slack (L3): local Foundry installs linked to a Helix
-- workspace, which Slack's requests for their Slack teams are relayed to.
-- Hosted fills the relay's tables; a local install carries them empty, and
-- uses the columns its own Slack rows gain (who may use the bot, and turns
-- that only read and ask).
--
-- Rollback. A deploy request carries schema only; nothing older reads the
-- new tables or columns. foundry_slack_installs' live_workspace, which this
-- replaces with live_destination, is read only by binaries with Slack, which
-- ship with this migration (20261213000000 to here go out together). The
-- Down is for a local database.

-- A local install's link: its hx_rl_ token kept as a SHA-256, and the
-- loopback origin it was authorized for (a relayed install's return_to is
-- under it). Revoking it revokes its installs.
CREATE TABLE foundry_relay_links (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  created_by_user_id VARCHAR(64)  NOT NULL,
  name               VARCHAR(128) NOT NULL,
  token_hash         CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  redirect_origin    VARCHAR(255) NOT NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- Its last claim or call, written at most once a minute.
  last_seen_at       DATETIME(6)  NULL,
  last_seen_addr     VARCHAR(64)  NULL,
  revoked_at         DATETIME(6)  NULL,
  UNIQUE KEY foundry_relay_links_token_uq (token_hash),
  KEY foundry_relay_links_workspace_idx (workspace_id, revoked_at)
);

-- A one-time code the site gave the owner's browser for one install, spent
-- once with its PKCE verifier (S256 code_challenge) before it expires.
CREATE TABLE foundry_relay_codes (
  code_hash      CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
  workspace_id   CHAR(26)     NOT NULL,
  user_id        VARCHAR(64)  NOT NULL,
  code_challenge CHAR(43)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  redirect_uri   VARCHAR(255) NOT NULL,
  name           VARCHAR(128) NOT NULL,
  expires_at     DATETIME(6)  NOT NULL,
  used_at        DATETIME(6)  NULL,
  KEY foundry_relay_codes_expiry_idx (expires_at),
  KEY foundry_relay_codes_workspace_idx (workspace_id)
);

-- Slack's requests for a relayed install (what foundry_slack_intake holds
-- for a hosted one: the same dedupe key, kind, conversation, Slack user and
-- payload), until its local install claims and acks them, or they expire.
-- status: queued | leased | acked | expired, checked in code. payload is
-- NULL once acked or expired.
CREATE TABLE foundry_relay_deliveries (
  id                 CHAR(26)    NOT NULL PRIMARY KEY,
  link_id            CHAR(26)    NOT NULL,
  install_id         CHAR(26)    NOT NULL,
  relay_workspace_id CHAR(26)    NOT NULL,
  dedupe_key         CHAR(66)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  conversation       CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  slack_user_id      VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  kind               VARCHAR(16) NOT NULL,
  payload            JSON        NULL,
  status             VARCHAR(16) NOT NULL DEFAULT 'queued',
  attempts           INT         NOT NULL DEFAULT 0,
  lease_until        DATETIME(6) NULL,
  -- Set once the hosted worker told the conversation its install is offline.
  noticed_at         DATETIME(6) NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at         DATETIME(6) NOT NULL,
  finished_at        DATETIME(6) NULL,
  UNIQUE KEY foundry_relay_deliveries_dedupe_uq (dedupe_key),
  KEY foundry_relay_deliveries_claim_idx (link_id, status, created_at),
  KEY foundry_relay_deliveries_expiry_idx (status, expires_at),
  KEY foundry_relay_deliveries_conv_idx (conversation, noticed_at),
  KEY foundry_relay_deliveries_install_idx (install_id, status),
  KEY foundry_relay_deliveries_finished_idx (finished_at)
);

-- A relayed install's channel messages (a feed row, its fields those of
-- foundry_slack_feed) and the spans of them it may have missed (kind 'gap',
-- payload {"from", "to"}, channel_id '' for every channel), until its local
-- install claims and acks them. A message row expires into a gap.
CREATE TABLE foundry_relay_feed (
  id                 CHAR(26)    NOT NULL PRIMARY KEY,
  link_id            CHAR(26)    NOT NULL,
  install_id         CHAR(26)    NOT NULL,
  relay_workspace_id CHAR(26)    NOT NULL,
  dedupe_key         CHAR(66)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  channel_id         VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  day                DATE        NOT NULL,
  kind               VARCHAR(16) NOT NULL,
  payload            JSON        NOT NULL,
  bytes              INT         NOT NULL,
  lease_until        DATETIME(6) NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  expires_at         DATETIME(6) NOT NULL,
  UNIQUE KEY foundry_relay_feed_dedupe_uq (install_id, dedupe_key),
  KEY foundry_relay_feed_claim_idx (link_id, created_at),
  KEY foundry_relay_feed_expiry_idx (kind, expires_at),
  KEY foundry_relay_feed_gap_idx (install_id, kind, channel_id)
);

-- Channels a relayed install was sent something from (never an externally
-- shared one): the only ones its proxied calls may name. Users likewise.
CREATE TABLE foundry_relay_channels (
  install_id CHAR(26)    NOT NULL,
  channel_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  last_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (install_id, channel_id),
  KEY foundry_relay_channels_last_idx (last_at)
);
CREATE TABLE foundry_relay_users (
  install_id    CHAR(26)    NOT NULL,
  slack_user_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  last_at       DATETIME(6) NOT NULL,
  PRIMARY KEY (install_id, slack_user_id),
  KEY foundry_relay_users_last_idx (last_at)
);

-- Counts only, per link per UTC day.
CREATE TABLE foundry_relay_usage (
  link_id    CHAR(26) NOT NULL,
  day        DATE     NOT NULL,
  deliveries BIGINT   NOT NULL DEFAULT 0,
  calls      BIGINT   NOT NULL DEFAULT 0,
  refused    BIGINT   NOT NULL DEFAULT 0,
  feed_rows  BIGINT   NOT NULL DEFAULT 0,
  feed_bytes BIGINT   NOT NULL DEFAULT 0,
  PRIMARY KEY (link_id, day)
);

-- Local only (empty on hosted): Slack users who asked to use a local
-- install, once each, until the owner allows them.
CREATE TABLE foundry_slack_access_requests (
  install_id    CHAR(26)     NOT NULL,
  slack_user_id VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  slack_name    VARCHAR(255) NOT NULL,
  created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (install_id, slack_user_id)
);

-- A relayed install serves its link's local workspace: one live install
-- per destination, (hosted workspace, local workspace), where a hosted one's
-- local workspace is ''. feed_on: its local install's Slack data connector
-- reads it (hosted keeps its channel messages); follow_on: its local
-- install takes unmentioned follow-ups (hosted keeps its thread replies);
-- relay_feed_rows: how many message rows it holds in foundry_relay_feed.
ALTER TABLE foundry_slack_installs
  ADD COLUMN relay_link_id CHAR(26) NULL AFTER workspace_id,
  ADD COLUMN relay_workspace_id CHAR(26) NULL AFTER relay_link_id,
  ADD COLUMN feed_on BOOL NOT NULL DEFAULT FALSE,
  ADD COLUMN follow_on BOOL NOT NULL DEFAULT FALSE,
  ADD COLUMN relay_feed_rows BIGINT NOT NULL DEFAULT 0,
  DROP INDEX foundry_slack_installs_workspace_uq,
  DROP COLUMN live_workspace,
  ADD COLUMN live_destination VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin
    AS (IF(revoked_at IS NULL, CONCAT(workspace_id, '/', IFNULL(relay_workspace_id, '')), NULL)) STORED,
  ADD UNIQUE KEY foundry_slack_installs_destination_uq (live_destination),
  ADD KEY foundry_slack_installs_relay_idx (relay_link_id, revoked_at);
-- An install a local install started through its link: the link, the local
-- workspace and where Slack's answer goes back to (the link's origin).
ALTER TABLE foundry_slack_install_states
  ADD COLUMN relay_link_id CHAR(26) NULL,
  ADD COLUMN relay_workspace_id CHAR(26) NULL,
  ADD COLUMN return_to VARCHAR(255) NULL;
-- 'full' (a person's own link; locally the app's installer, the owner) or
-- 'read' (locally, allowed by the owner): checked in code.
ALTER TABLE foundry_slack_links ADD COLUMN access VARCHAR(8) NOT NULL DEFAULT 'full';
-- A turn that only reads and asks, whatever its person may do.
ALTER TABLE foundry_agent_turns ADD COLUMN read_only BOOL NOT NULL DEFAULT FALSE AFTER surface;

-- +goose Down
ALTER TABLE foundry_agent_turns DROP COLUMN read_only;
ALTER TABLE foundry_slack_links DROP COLUMN access;
ALTER TABLE foundry_slack_install_states DROP COLUMN return_to, DROP COLUMN relay_workspace_id, DROP COLUMN relay_link_id;
-- A database holding several live installs of one workspace (relayed ones)
-- can't go down: revoke them first.
ALTER TABLE foundry_slack_installs
  DROP INDEX foundry_slack_installs_relay_idx,
  DROP INDEX foundry_slack_installs_destination_uq,
  DROP COLUMN live_destination,
  ADD COLUMN live_workspace CHAR(26) AS (IF(revoked_at IS NULL, workspace_id, NULL)) STORED,
  ADD UNIQUE KEY foundry_slack_installs_workspace_uq (live_workspace),
  DROP COLUMN relay_feed_rows,
  DROP COLUMN follow_on,
  DROP COLUMN feed_on,
  DROP COLUMN relay_workspace_id,
  DROP COLUMN relay_link_id;
DROP TABLE foundry_slack_access_requests;
DROP TABLE foundry_relay_usage;
DROP TABLE foundry_relay_users;
DROP TABLE foundry_relay_channels;
DROP TABLE foundry_relay_feed;
DROP TABLE foundry_relay_deliveries;
DROP TABLE foundry_relay_codes;
DROP TABLE foundry_relay_links;
