-- +goose Up
-- Source permissions (plans/SOURCE_ACLS.md): who a connector's source shares
-- each of its items with (a Slack channel), read at the end of every
-- successful sync of a kind that reports them (connector.Audienced), and
-- replaced whole at each read. Every table is new: nothing existing changes.
-- Emails are stored as connector.ParseEmail writes them (trimmed and
-- lowercased) and compared byte for byte, so no collation matches two
-- addresses that differ.

-- A connector's last read of its audiences: when the last one that worked
-- was (NULL until one has), and why the latest failed, if it did and no read
-- has worked since.
CREATE TABLE foundry_source_audiences (
  connector_id CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)      NOT NULL,
  read_at      DATETIME(6)   NULL,
  problem      VARCHAR(1024) NULL,
  problem_at   DATETIME(6)   NULL,
  KEY foundry_source_audiences_workspace_idx (workspace_id),
  CONSTRAINT foundry_source_audiences_problem_chk CHECK ((problem IS NULL) = (problem_at IS NULL)),
  CONSTRAINT foundry_source_audiences_read_chk CHECK (read_at IS NOT NULL OR problem IS NOT NULL)
);

-- The source's people at that read, by email; a guest reads only the items
-- that list them.
CREATE TABLE foundry_source_people (
  connector_id CHAR(26)     NOT NULL,
  email        VARCHAR(320) COLLATE utf8mb4_bin NOT NULL,
  guest        BOOLEAN      NOT NULL,
  PRIMARY KEY (connector_id, email)
);

-- The source's items at that read: shared with everyone (no guest), or with
-- the people foundry_source_item_members lists.
CREATE TABLE foundry_source_items (
  connector_id CHAR(26)     NOT NULL,
  item_key     VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
  everyone     BOOLEAN      NOT NULL,
  PRIMARY KEY (connector_id, item_key)
);

-- The people a members item lists.
CREATE TABLE foundry_source_item_members (
  connector_id CHAR(26)     NOT NULL,
  item_key     VARCHAR(255) COLLATE utf8mb4_bin NOT NULL,
  email        VARCHAR(320) COLLATE utf8mb4_bin NOT NULL,
  PRIMARY KEY (connector_id, item_key, email),
  KEY foundry_source_item_members_email_idx (connector_id, email)
);

-- +goose Down
DROP TABLE foundry_source_item_members;
DROP TABLE foundry_source_items;
DROP TABLE foundry_source_people;
DROP TABLE foundry_source_audiences;
