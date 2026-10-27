-- +goose Up
-- A Foundry ontology: a workspace's model of its data, bound to one Helix
-- database (a tenant) the user chose or created with the tenant routes.
-- Deleting one removes these rows' liveness, never the database.
CREATE TABLE foundry_ontologies (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  name               VARCHAR(120) NOT NULL,
  project_id         CHAR(26)     NOT NULL,
  tenant_id          CHAR(26)     NOT NULL,
  current_version_id CHAR(26)     NULL,
  -- The version the database's tables were last loaded through: documents
  -- uploaded later are extracted against it.
  loaded_version_id  CHAR(26)     NULL,
  client_token       VARCHAR(64)  NOT NULL,
  created_by_user_id CHAR(26)     NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  deleted_at         DATETIME(6)  NULL,
  -- 1 while not deleted, NULL after: one live ontology per database, so a
  -- database has one Foundry writer.
  live_key           TINYINT      NULL,
  UNIQUE KEY foundry_ontologies_tenant_uq (tenant_id, live_key),
  UNIQUE KEY foundry_ontologies_client_token_uq (workspace_id, client_token),
  INDEX foundry_ontologies_list_idx (workspace_id, created_at, id),
  CONSTRAINT foundry_ontologies_live_chk CHECK ((deleted_at IS NULL) = (live_key IS NOT NULL)),
  CONSTRAINT foundry_ontologies_live_key_chk CHECK (live_key IS NULL OR live_key = 1)
);

-- An ontology's immutable versions, numbered 1, 2, 3 per ontology.
CREATE TABLE foundry_ontology_versions (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  ontology_id        CHAR(26)     NOT NULL,
  version            INT UNSIGNED NOT NULL,
  -- foundry_v1 or agent_bundle, checked in code.
  format             VARCHAR(16)  NOT NULL,
  definition         JSON         NOT NULL,
  source             VARCHAR(16)  NOT NULL,
  generation_id      CHAR(26)     NULL,
  created_by_user_id CHAR(26)     NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_ontology_versions_number_uq (ontology_id, version),
  CONSTRAINT foundry_ontology_versions_source_chk CHECK (source IN ('manual', 'generated'))
);

-- Drafts of an ontology the foundry-ontology-worker runs.
CREATE TABLE foundry_ontology_generations (
  id                   CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id         CHAR(26)    NOT NULL,
  ontology_id          CHAR(26)    NOT NULL,
  status               VARCHAR(16) NOT NULL,
  -- 1 while queued or running, NULL after: at most one live generation per ontology.
  active_key           TINYINT     NULL,
  goal                 TEXT        NULL,
  input                JSON        NOT NULL,
  lease_owner          VARCHAR(64) NULL,
  lease_until          DATETIME(6) NULL,
  attempts             INT         NOT NULL DEFAULT 0,
  next_attempt_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  error                TEXT        NULL,
  result_version_id    CHAR(26)    NULL,
  model                VARCHAR(64) NULL,
  input_tokens         INT         NULL,
  output_tokens        INT         NULL,
  requested_by_user_id CHAR(26)    NULL,
  created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at           DATETIME(6) NULL,
  finished_at          DATETIME(6) NULL,
  UNIQUE KEY foundry_ontology_generations_active_uq (ontology_id, active_key),
  INDEX foundry_ontology_generations_claim_idx (status, next_attempt_at),
  INDEX foundry_ontology_generations_history_idx (ontology_id, created_at, id),
  CONSTRAINT foundry_ontology_generations_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'failed'))
);

-- A document uploaded to an ontology: the row is written before the bytes,
-- which the browser PUTs to the presigned s3_key; :complete checks them.
CREATE TABLE foundry_documents (
  id                 CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)      NOT NULL,
  ontology_id        CHAR(26)      NOT NULL,
  name               VARCHAR(255)  NOT NULL,
  media_type         VARCHAR(32)   NOT NULL,
  size_bytes         BIGINT        NOT NULL,
  sha256             CHAR(64)      NOT NULL,
  client_token       VARCHAR(64)   NOT NULL,
  status             VARCHAR(16)   NOT NULL,
  s3_key             VARCHAR(1024) NOT NULL,
  created_by_user_id CHAR(26)      NULL,
  created_at         DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  uploaded_at        DATETIME(6)   NULL,
  deleted_at         DATETIME(6)   NULL,
  UNIQUE KEY foundry_documents_client_token_uq (ontology_id, client_token),
  INDEX foundry_documents_sha256_idx (ontology_id, sha256),
  INDEX foundry_documents_list_idx (ontology_id, created_at, id),
  INDEX foundry_documents_workspace_idx (workspace_id, status),
  CONSTRAINT foundry_documents_status_chk CHECK (status IN ('pending_upload', 'uploaded', 'deleted'))
);

-- What loading a document into its ontology's database made of it. No row
-- means not extracted yet.
CREATE TABLE foundry_document_extractions (
  document_id     CHAR(26)      NOT NULL PRIMARY KEY,
  ontology_id     CHAR(26)      NOT NULL,
  workspace_id    CHAR(26)      NOT NULL,
  job_id          CHAR(26)      NOT NULL,
  status          VARCHAR(16)   NOT NULL,
  duplicate_of    CHAR(26)      NULL,
  error_code      VARCHAR(16)   NULL,
  error_retryable BOOLEAN       NULL,
  sections        INT           NOT NULL DEFAULT 0,
  entities        INT           NOT NULL DEFAULT 0,
  mentions        INT           NOT NULL DEFAULT 0,
  facts           INT           NOT NULL DEFAULT 0,
  reviews         INT           NOT NULL DEFAULT 0,
  unresolved      INT           NOT NULL DEFAULT 0,
  rejected        INT           NOT NULL DEFAULT 0,
  usd_micros      BIGINT        NOT NULL DEFAULT 0,
  input_tokens    BIGINT        NOT NULL DEFAULT 0,
  output_tokens   BIGINT        NOT NULL DEFAULT 0,
  artifact_key    VARCHAR(1024) NULL,
  finished_at     DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX foundry_document_extractions_ontology_idx (ontology_id),
  CONSTRAINT foundry_document_extractions_status_chk CHECK (status IN ('extracted', 'duplicate', 'failed')),
  CONSTRAINT foundry_document_extractions_shape_chk CHECK (
    (status = 'duplicate') = (duplicate_of IS NOT NULL) AND
    (status = 'failed') = (error_code IS NOT NULL AND error_retryable IS NOT NULL)
  ),
  CONSTRAINT foundry_document_extractions_code_chk CHECK (error_code IS NULL OR error_code IN ('too_large', 'unsupported', 'model', 'store'))
);

-- Loads of an ontology's database: a load ingests a version's tables, then
-- extracts the documents; an extract, queued by an upload, only extracts.
-- One live job per ontology, so one writer per database.
CREATE TABLE foundry_graph_jobs (
  id                   CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id         CHAR(26)    NOT NULL,
  ontology_id          CHAR(26)    NOT NULL,
  version_id           CHAR(26)    NOT NULL,
  kind                 VARCHAR(16) NOT NULL,
  status               VARCHAR(16) NOT NULL,
  phase                VARCHAR(16) NOT NULL,
  -- 1 while queued or running, NULL after.
  active_key           TINYINT     NULL,
  client_token         VARCHAR(64) NULL,
  lease_owner          VARCHAR(64) NULL,
  lease_until          DATETIME(6) NULL,
  attempts             INT         NOT NULL DEFAULT 0,
  next_attempt_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  error                TEXT        NULL,
  report               JSON        NULL,
  chunks_done          INT         NOT NULL DEFAULT 0,
  documents_done       INT         NOT NULL DEFAULT 0,
  documents_total      INT         NOT NULL DEFAULT 0,
  usd_micros           BIGINT      NOT NULL DEFAULT 0,
  input_tokens         BIGINT      NOT NULL DEFAULT 0,
  output_tokens        BIGINT      NOT NULL DEFAULT 0,
  requested_by_user_id CHAR(26)    NULL,
  created_at           DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  started_at           DATETIME(6) NULL,
  finished_at          DATETIME(6) NULL,
  UNIQUE KEY foundry_graph_jobs_active_uq (ontology_id, active_key),
  UNIQUE KEY foundry_graph_jobs_client_token_uq (ontology_id, client_token),
  INDEX foundry_graph_jobs_claim_idx (status, next_attempt_at),
  INDEX foundry_graph_jobs_history_idx (ontology_id, created_at, id),
  INDEX foundry_graph_jobs_spend_idx (workspace_id, created_at),
  CONSTRAINT foundry_graph_jobs_kind_chk CHECK (kind IN ('load', 'extract')),
  CONSTRAINT foundry_graph_jobs_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'budget_exhausted', 'failed')),
  CONSTRAINT foundry_graph_jobs_phase_chk CHECK (phase IN ('provisioning', 'ingest', 'documents', 'finishing')),
  CONSTRAINT foundry_graph_jobs_live_chk CHECK ((status IN ('queued', 'running')) = (active_key IS NOT NULL))
);

-- +goose Down
DROP TABLE foundry_graph_jobs;
DROP TABLE foundry_document_extractions;
DROP TABLE foundry_documents;
DROP TABLE foundry_ontology_generations;
DROP TABLE foundry_ontology_versions;
DROP TABLE foundry_ontologies;
