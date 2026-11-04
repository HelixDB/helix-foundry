-- +goose Up
-- Source grants (package access): an owner or admin gives a principal (a
-- user, a team, or a service credential: an agent's key) one of the
-- workspace's sources. A grantee reads an ontology's current load only while
-- every source it holds is granted. Unused targets are ''.
CREATE TABLE foundry_access_grants (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  principal_kind     VARCHAR(24)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  principal_id       CHAR(26)     NOT NULL,
  target_kind        VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  connector_id       CHAR(26)     NOT NULL DEFAULT '',
  table_name         VARCHAR(255) NOT NULL DEFAULT '',
  document_set_id    CHAR(26)     NOT NULL DEFAULT '',
  created_by_user_id CHAR(26)     NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_access_grants_uq (workspace_id, principal_kind, principal_id, target_kind, connector_id, table_name, document_set_id),
  KEY foundry_access_grants_connector_idx (workspace_id, connector_id),
  KEY foundry_access_grants_set_idx (workspace_id, document_set_id),
  CONSTRAINT foundry_access_grants_principal_chk CHECK (principal_kind IN ('user', 'service_credential', 'team') AND principal_id <> ''),
  CONSTRAINT foundry_access_grants_target_chk CHECK (
    (target_kind = 'connector' AND connector_id <> '' AND table_name = '' AND document_set_id = '') OR
    (target_kind = 'table' AND connector_id <> '' AND table_name <> '' AND document_set_id = '') OR
    (target_kind = 'document_set' AND connector_id = '' AND table_name = '' AND document_set_id <> '') OR
    (target_kind = 'enrichment' AND connector_id = '' AND table_name = '' AND document_set_id = ''))
);

-- The questions a grantee asked: only these are its own, and only these
-- read as a grantee's (history, catalog and a check of its grants when the
-- question runs). Questions anyone else asked have no row.
CREATE TABLE foundry_question_grantees (
  question_id    CHAR(26)    NOT NULL PRIMARY KEY,
  ontology_id    CHAR(26)    NOT NULL,
  principal_kind VARCHAR(24) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  principal_id   CHAR(26)    NOT NULL,
  -- What the grantee held of the load when it ran, if not all of it: its answer
  -- reads only while its grants still hold that.
  seen           JSON        NULL,
  KEY foundry_question_grantees_principal_idx (ontology_id, principal_kind, principal_id),
  KEY foundry_question_grantees_asker_idx (principal_kind, principal_id),
  CONSTRAINT foundry_question_grantees_principal_chk CHECK (principal_kind IN ('user', 'service_credential'))
);

-- +goose Down
DROP TABLE foundry_question_grantees;
DROP TABLE foundry_access_grants;
