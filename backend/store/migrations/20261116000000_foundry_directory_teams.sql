-- +goose Up
-- A team the host keeps in step with a group of the workspace's directory
-- (SCIM, through WorkOS Directory Sync): its name and members are the
-- group's, and only its grants are the workspace's to change. NULL for a
-- team an owner or admin made. Releasing a directory sets them NULL, so its
-- teams stay, grants and all, as teams made by hand.
ALTER TABLE foundry_teams
  ADD COLUMN directory_id       VARCHAR(64) NULL,
  ADD COLUMN directory_group_id VARCHAR(64) NULL,
  -- The directory's provider as people know it ("Okta"), for the words
  -- that say where to change a synced team.
  ADD COLUMN directory_provider VARCHAR(64) NULL,
  ADD UNIQUE KEY foundry_teams_directory_group_uq (workspace_id, directory_group_id);

-- Members of a synced team's group the team leaves out: a member with raw
-- access, whom the team's grants can't confine, while the team holds grants.
-- The next sync puts them back once it holds none or they lose raw access.
CREATE TABLE foundry_team_left_out (
  team_id      CHAR(26) NOT NULL,
  workspace_id CHAR(26) NOT NULL,
  user_id      CHAR(26) NOT NULL,
  PRIMARY KEY (team_id, user_id),
  KEY foundry_team_left_out_user_idx (workspace_id, user_id)
);

-- +goose Down
DROP TABLE foundry_team_left_out;
ALTER TABLE foundry_teams
  DROP INDEX foundry_teams_directory_group_uq,
  DROP COLUMN directory_provider,
  DROP COLUMN directory_group_id,
  DROP COLUMN directory_id;
