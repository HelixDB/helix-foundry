-- +goose Up
-- Public dashboard links an owner or admin controls: a link may expire, and
-- an owner may turn public links off for the whole workspace.

-- When a link stops showing its snapshot; NULL: never, as every link from
-- before. An expired link is served as an unpublished one, and publishing
-- again makes a new link. A nullable column only: no row is changed, and a
-- binary from before publishes and serves links as it did, never expiring.
ALTER TABLE foundry_dashboard_shares ADD COLUMN expires_at DATETIME(6) NULL;

-- A workspace's dashboard settings, changed by its owners. No row is the
-- default: public links on, as every workspace had them before. Off, no link
-- of the workspace is served and none is published, until they are turned on
-- again. A new table: nothing existing changes.
CREATE TABLE foundry_dashboard_settings (
  workspace_id       CHAR(26)    NOT NULL PRIMARY KEY,
  public_links       BOOLEAN     NOT NULL,
  updated_by_user_id VARCHAR(64) NOT NULL,
  updated_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
);

-- +goose Down
DROP TABLE foundry_dashboard_settings;
ALTER TABLE foundry_dashboard_shares DROP COLUMN expires_at;
