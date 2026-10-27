-- +goose Up
-- The owner: a standalone install has one user, written at every start.
CREATE TABLE users (
  id           CHAR(26)     NOT NULL PRIMARY KEY,
  email        VARCHAR(320) NOT NULL,
  display_name VARCHAR(255) NOT NULL,
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_users_email (email)
);

-- The owner's workspaces. Foundry's store reads id, status and deleted_at
-- (store.Workspaces).
CREATE TABLE workspaces (
  id           CHAR(26)     NOT NULL PRIMARY KEY,
  slug         VARCHAR(64)  NOT NULL,
  display_name VARCHAR(255) NOT NULL,
  region       VARCHAR(32)  NOT NULL,
  status       VARCHAR(16)  NOT NULL DEFAULT 'active',
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at   DATETIME(6)  NULL,
  UNIQUE KEY uq_workspaces_slug (slug)
);

CREATE TABLE projects (
  id           CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)     NOT NULL,
  slug         VARCHAR(64)  NOT NULL,
  display_name VARCHAR(512) NOT NULL,
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at   DATETIME(6)  NULL,
  UNIQUE KEY uq_projects_ws_slug (workspace_id, slug)
);

-- The databases ontologies load into: every one is queried at the install's
-- one HelixDB, so a slug is unique across the install.
CREATE TABLE tenants (
  id           CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)     NOT NULL,
  project_id   CHAR(26)     NOT NULL,
  name         VARCHAR(128) NOT NULL,
  slug         VARCHAR(64)  NOT NULL,
  status       VARCHAR(16)  NOT NULL DEFAULT 'active',
  created_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at   DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  deleted_at   DATETIME(6)  NULL,
  UNIQUE KEY uq_tenants_slug (slug),
  KEY idx_tenants_project (project_id)
);

-- +goose Down
DROP TABLE tenants;
DROP TABLE projects;
DROP TABLE workspaces;
DROP TABLE users;
