-- +goose Up
-- Members an owner or admin restricted to what grants give: on every Foundry
-- call, while their role is member, they are grantees (package access).
-- Kept when the member leaves the workspace, so one let back in is still
-- restricted.
CREATE TABLE foundry_restricted_members (
  workspace_id       CHAR(26)    NOT NULL,
  user_id            CHAR(26)    NOT NULL,
  created_by_user_id CHAR(26)    NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (workspace_id, user_id)
);

-- Teams of a workspace's members: each member holds the team's grants.
CREATE TABLE foundry_teams (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  name               VARCHAR(120) NOT NULL,
  created_by_user_id CHAR(26)     NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_teams_name_uq (workspace_id, name)
);

CREATE TABLE foundry_team_members (
  team_id      CHAR(26) NOT NULL,
  workspace_id CHAR(26) NOT NULL,
  user_id      CHAR(26) NOT NULL,
  PRIMARY KEY (team_id, user_id),
  KEY foundry_team_members_user_idx (workspace_id, user_id)
);

-- +goose Down
DROP TABLE foundry_team_members;
DROP TABLE foundry_teams;
DROP TABLE foundry_restricted_members;
