-- +goose Up
-- Dashboards: tiles of typed queries over ontologies' loads, which every
-- member of the workspace sees and each reads as themselves, and their public
-- snapshots. Every table is new: nothing existing changes.

-- A dashboard of a workspace. Its spec is the dashboard package's canonical
-- JSON; revision counts its edits, so an edit or a publish names the one it
-- was made from. Pinned, it shows in every member's sidebar.
CREATE TABLE foundry_dashboards (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  name               VARCHAR(120) NOT NULL,
  description        TEXT         NOT NULL,
  spec               JSON         NOT NULL,
  revision           BIGINT       NOT NULL DEFAULT 1,
  created_by_user_id VARCHAR(64)  NOT NULL,
  updated_by_user_id VARCHAR(64)  NOT NULL,
  client_token       VARCHAR(64)  NULL,
  pinned_at          DATETIME(6)  NULL,
  pinned_by_user_id  VARCHAR(64)  NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_dashboards_client_token_uq (workspace_id, created_by_user_id, client_token),
  INDEX foundry_dashboards_list_idx (workspace_id, updated_at, id),
  CONSTRAINT foundry_dashboards_revision_chk CHECK (revision > 0),
  CONSTRAINT foundry_dashboards_pinned_chk CHECK ((pinned_at IS NULL) = (pinned_by_user_id IS NULL))
);

-- A dashboard's public snapshot: its tiles' outputs as its publisher read
-- them, served to anyone holding the link's token. Only the token's SHA-256
-- is looked up; the token itself is sealed with the connectors' keyring, so
-- owners and admins can be shown the link again and the table alone yields
-- no link. Republishing keeps the token; unpublishing deletes the row.
CREATE TABLE foundry_dashboard_shares (
  dashboard_id         CHAR(26)       NOT NULL PRIMARY KEY,
  workspace_id         CHAR(26)       NOT NULL,
  token_sha256         CHAR(64)       NOT NULL,
  token_sealed         VARBINARY(512) NOT NULL,
  token_key_id         VARCHAR(64)    NOT NULL,
  snapshot             JSON           NOT NULL,
  -- The dashboard's revision the snapshot shows.
  spec_revision        BIGINT         NOT NULL,
  -- The ontologies its tiles read: deleting one deletes the snapshot.
  ontology_ids         JSON           NOT NULL,
  published_by_user_id VARCHAR(64)    NOT NULL,
  published_at         DATETIME(6)    NOT NULL,
  first_published_at   DATETIME(6)    NOT NULL,
  UNIQUE KEY foundry_dashboard_shares_token_uq (token_sha256),
  INDEX foundry_dashboard_shares_workspace_idx (workspace_id)
);

-- +goose Down
DROP TABLE foundry_dashboard_shares;
DROP TABLE foundry_dashboards;
