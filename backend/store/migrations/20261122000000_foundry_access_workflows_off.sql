-- +goose Up
-- The Workflows permission (package access): a principal on source grants
-- (a user, a team, or a service credential) may use the workflows of the
-- ontologies it reads unless an owner or admin turned it off. A row turns it
-- off; it is on for everyone else. A member is refused it while they or any
-- of their teams has it off.
CREATE TABLE foundry_access_workflows_off (
  workspace_id       CHAR(26)    NOT NULL,
  principal_kind     VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  principal_id       CHAR(26)    NOT NULL,
  created_by_user_id CHAR(26)    NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (workspace_id, principal_kind, principal_id),
  CONSTRAINT foundry_access_workflows_off_principal_chk CHECK (principal_kind IN ('user', 'service_credential', 'team') AND principal_id <> '')
);

-- +goose Down
DROP TABLE foundry_access_workflows_off;
