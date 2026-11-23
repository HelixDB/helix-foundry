-- +goose Up
-- The Slack data connector on Helix's Slack app (foundry.slack.feed). No
-- message text lives here but in foundry_slack_feed.payload, deleted once
-- folded into the day mirror (object store, indexed by foundry_slack_feed_days).
--
-- A connector's binding to its Slack team is a feed, with an id of its own:
-- unbinding marks it deleting and the sweep deletes what it holds later, so
-- the same connector may bind again (another team, or the same one after a
-- switch away) at once without its rows meeting the old feed's. Every table
-- below the feeds is keyed by the feed.
--
-- Rollback. A deploy request carries schema only; nothing older reads the
-- new tables or columns, so they stay. The Down is for a local database.
ALTER TABLE foundry_slack_installs
  -- auth.test's bot_id: a message of Helix's own bot carries it.
  ADD COLUMN bot_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' AFTER bot_user_id,
  -- The installer is a Slack workspace admin or owner (users.info): a feed binds only then.
  ADD COLUMN installer_admin BOOL NOT NULL DEFAULT FALSE,
  -- The last minute Slack dropped this team's events (app_rate_limited).
  ADD COLUMN events_limited_at DATETIME(6) NULL;
-- Where the site goes once Slack hands the install back.
ALTER TABLE foundry_slack_install_states ADD COLUMN return_path VARCHAR(255) NULL;
-- When the bot last posted in the thread, and when anyone but the bot and
-- slack_user_id last did: an unmentioned reply is taken while bot_at is
-- newer. A thread's mappings are read by foundry_slack_threads_uq's prefix.
ALTER TABLE foundry_slack_threads
  ADD COLUMN bot_at DATETIME(6) NULL,
  ADD COLUMN others_at DATETIME(6) NULL;
-- A pushed connector syncs when its source says so; the scheduler re-dues one
-- whose live sync may have missed what made it due.
ALTER TABLE connectors ADD COLUMN pushed BOOL NOT NULL DEFAULT FALSE;
UPDATE connectors SET pushed = TRUE WHERE kind = 'events';

-- Tier-1 slots per install and method (history, replies), shared by every
-- replica: no call before next_at. Made at install for every install.
CREATE TABLE foundry_slack_slots (
  install_id CHAR(26)    NOT NULL,
  method     VARCHAR(16) NOT NULL,
  next_at    DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (install_id, method)
);

-- The app-credential connector reading a Slack team: one live feed per
-- connector, per install and per Enterprise Grid org (the live_* keys).
-- install_id follows the team's live install (re-pointed by a reinstall).
-- Buffer rows are numbered under this row's lock; folded_seq is how far the
-- mirror holds them.
CREATE TABLE foundry_slack_feeds (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  connector_id    CHAR(26)    NOT NULL,
  workspace_id    CHAR(26)    NOT NULL,
  team_id         VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  enterprise_id   VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
  install_id      CHAR(26)    NOT NULL,
  last_seq        BIGINT      NOT NULL DEFAULT 0,
  folded_seq      BIGINT      NOT NULL DEFAULT 0,
  notice_version  INT         NOT NULL,
  bound_by        VARCHAR(64) NOT NULL,
  bound_at        DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- Set by unbind: the sweep deletes the buffer, moves the index's object
  -- keys to connector_replaced_snapshots, then deletes this row.
  deleting_at     DATETIME(6) NULL,
  live_connector  CHAR(26)    AS (IF(deleting_at IS NULL, connector_id, NULL)) STORED,
  live_install    CHAR(26)    AS (IF(deleting_at IS NULL, install_id, NULL)) STORED,
  live_enterprise VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin AS (IF(deleting_at IS NULL AND enterprise_id <> '', enterprise_id, NULL)) STORED,
  UNIQUE KEY foundry_slack_feeds_connector_uq (live_connector),
  UNIQUE KEY foundry_slack_feeds_install_uq (live_install),
  UNIQUE KEY foundry_slack_feeds_enterprise_uq (live_enterprise),
  KEY foundry_slack_feeds_workspace_idx (workspace_id, team_id),
  KEY foundry_slack_feeds_connector_idx (connector_id),
  KEY foundry_slack_feeds_deleting_idx (deleting_at)
);

-- What Slack sent (event) or the backfill read (backfill), until folded.
-- dedupe_key: "e:"+event_id, "b:"+SHA-256 of (channel, ts, edited ts, kind),
-- or "g:"+SHA-256 of a gap's channel and span. day: the UTC day the row lands on.
-- kind: message | changed | deleted | replied | channel | joined | left |
-- span | complete | gap; source: event | backfill; both checked in code.
CREATE TABLE foundry_slack_feed (
  feed_id    CHAR(26)    NOT NULL,
  seq        BIGINT      NOT NULL,
  dedupe_key CHAR(66)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  channel_id VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  day        DATE        NOT NULL,
  kind       VARCHAR(16) NOT NULL,
  source     VARCHAR(8)  NOT NULL,
  payload    JSON        NOT NULL,
  bytes      INT         NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (feed_id, seq),
  UNIQUE KEY foundry_slack_feed_dedupe_uq (feed_id, dedupe_key)
);

-- Each readable channel's coverage: events hold everything from covered_from.
CREATE TABLE foundry_slack_feed_channels (
  feed_id      CHAR(26)    NOT NULL,
  channel_id   VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  covered_from DATETIME(6) NOT NULL,
  PRIMARY KEY (feed_id, channel_id)
);

-- The day mirror's index. object_key names an immutable object; a fold
-- writes a new one and records the old key as replaced. state: partial |
-- complete | lossy. documented_at: when the day's document was last offered.
CREATE TABLE foundry_slack_feed_days (
  feed_id       CHAR(26)     NOT NULL,
  channel_id    VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  day           DATE         NOT NULL,
  object_key    VARCHAR(512) NOT NULL,
  digest        CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  messages      INT          NOT NULL,
  bytes         INT          NOT NULL,
  state         VARCHAR(16)  NOT NULL,
  updated_seq   BIGINT       NOT NULL,
  documented_at DATETIME(6)  NULL,
  PRIMARY KEY (feed_id, channel_id, day),
  KEY foundry_slack_feed_days_updated_idx (feed_id, updated_seq),
  KEY foundry_slack_feed_days_day_idx (feed_id, day)
);

-- A window of a channel's history to read, newest first. reason: bound |
-- joined | gap; status: queued | running | done | cancelled | failed.
CREATE TABLE foundry_slack_backfill (
  feed_id    CHAR(26)     NOT NULL,
  channel_id VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  oldest     DATETIME(6)  NOT NULL,
  reason     VARCHAR(16)  NOT NULL,
  -- A bound window's place in the channel order the bind was given.
  place      INT          NOT NULL DEFAULT 0,
  status     VARCHAR(16)  NOT NULL DEFAULT 'queued',
  -- The next page ends here.
  latest     DATETIME(6)  NOT NULL,
  page_cursor VARCHAR(255) NULL,
  attempts   INT          NOT NULL DEFAULT 0,
  pages      INT          NOT NULL DEFAULT 0,
  messages   INT          NOT NULL DEFAULT 0,
  threads    INT          NOT NULL DEFAULT 0,
  covered_s  BIGINT       NOT NULL DEFAULT 0,
  error      VARCHAR(255) NULL,
  updated_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (feed_id, channel_id, oldest),
  KEY foundry_slack_backfill_next_idx (feed_id, status, place)
);

-- A thread whose replies the backfill reads. status: queued | done | gone.
CREATE TABLE foundry_slack_backfill_threads (
  feed_id    CHAR(26)     NOT NULL,
  channel_id VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  thread_ts  VARCHAR(32)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  day        DATE         NOT NULL,
  page_cursor VARCHAR(255) NULL,
  status     VARCHAR(16)  NOT NULL DEFAULT 'queued',
  PRIMARY KEY (feed_id, channel_id, thread_ts),
  KEY foundry_slack_backfill_threads_next_idx (feed_id, status, day)
);

-- Gap repair spent per feed per UTC day.
CREATE TABLE foundry_slack_gap_budget (
  feed_id CHAR(26) NOT NULL,
  day     DATE     NOT NULL,
  calls   INT      NOT NULL DEFAULT 0,
  PRIMARY KEY (feed_id, day)
);

-- +goose Down
DROP TABLE foundry_slack_gap_budget;
DROP TABLE foundry_slack_backfill_threads;
DROP TABLE foundry_slack_backfill;
DROP TABLE foundry_slack_feed_days;
DROP TABLE foundry_slack_feed_channels;
DROP TABLE foundry_slack_feed;
DROP TABLE foundry_slack_feeds;
DROP TABLE foundry_slack_slots;
ALTER TABLE connectors DROP COLUMN pushed;
ALTER TABLE foundry_slack_threads DROP COLUMN others_at, DROP COLUMN bot_at;
ALTER TABLE foundry_slack_install_states DROP COLUMN return_path;
ALTER TABLE foundry_slack_installs DROP COLUMN events_limited_at, DROP COLUMN installer_admin, DROP COLUMN bot_id;
