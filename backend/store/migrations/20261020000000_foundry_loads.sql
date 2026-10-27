-- +goose Up
-- Load versions. A load is one write
-- of an approved version into an ontology's database. Its id is the id of the
-- job that creates it, and every node and edge it writes carries that id as
-- foundry_load, so loads of several versions sit side by side in the
-- database. The ontology points at the load reads go to.
--
-- Jobs and extractions written before loads existed belong to no load, so
-- both tables are recreated empty. A deploy that applies schema only and
-- keeps the rows would have the new checks refuse them: empty both tables
-- first.
CREATE TABLE foundry_loads (
  id           CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  ontology_id  CHAR(26)    NOT NULL,
  version_id   CHAR(26)    NOT NULL,
  status       VARCHAR(16) NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- When writing ended: the load job's end, or the ontology's delete.
  finished_at  DATETIME(6) NULL,
  deleted_at   DATETIME(6) NULL,
  INDEX foundry_loads_list_idx (ontology_id, created_at, id),
  CONSTRAINT foundry_loads_status_chk CHECK (status IN ('writing', 'ready', 'partial', 'failed', 'deleting', 'delete_failed', 'deleted')),
  CONSTRAINT foundry_loads_finished_chk CHECK ((status = 'writing') = (finished_at IS NULL)),
  CONSTRAINT foundry_loads_deleted_chk CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
);

-- A deleted ontology keeps its database (live_key) until its loads are
-- deleted from it, so a live ontology always holds one and a deleted one may.
-- A Vitess-based MySQL host may rename CHECK constraints, so the old one is
-- dropped by prefix.
SET @drop_ontology_live_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_ontologies ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_ontologies'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_ontologies_live_chk%'
);
PREPARE drop_ontology_live_chk FROM @drop_ontology_live_chk;
EXECUTE drop_ontology_live_chk;
DEALLOCATE PREPARE drop_ontology_live_chk;

ALTER TABLE foundry_ontologies
  -- The load reads go to; NULL before a load finished writing, and once deleted.
  ADD COLUMN current_load_id CHAR(26) NULL AFTER current_version_id,
  -- An upload, or a change of current load, came while a job was live: the
  -- job's end queues an extract of the current load.
  ADD COLUMN extract_owed BOOLEAN NOT NULL DEFAULT FALSE AFTER current_load_id,
  ADD CONSTRAINT foundry_ontologies_bound_chk CHECK (deleted_at IS NOT NULL OR live_key IS NOT NULL),
  ADD CONSTRAINT foundry_ontologies_current_chk CHECK (deleted_at IS NULL OR current_load_id IS NULL);

-- What loading a document into one load made of it. No row means the load
-- has not extracted it yet.
DROP TABLE foundry_document_extractions;
CREATE TABLE foundry_load_extractions (
  load_id         CHAR(26)      NOT NULL,
  document_id     CHAR(26)      NOT NULL,
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
  PRIMARY KEY (load_id, document_id),
  CONSTRAINT foundry_load_extractions_status_chk CHECK (status IN ('extracted', 'duplicate', 'failed')),
  CONSTRAINT foundry_load_extractions_shape_chk CHECK (
    (status = 'duplicate') = (duplicate_of IS NOT NULL) AND
    (status = 'failed') = (error_code IS NOT NULL AND error_retryable IS NOT NULL)
  ),
  CONSTRAINT foundry_load_extractions_code_chk CHECK (error_code IS NULL OR error_code IN ('too_large', 'unsupported', 'model', 'store'))
);

-- Work on an ontology's database: a load job creates a load (load_id is its
-- own id) and fills it from the tables, then the documents; an extract adds
-- documents to a load; a delete removes one. One live job per ontology, so
-- one writer per database.
DROP TABLE foundry_graph_jobs;
CREATE TABLE foundry_load_jobs (
  id                   CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id         CHAR(26)    NOT NULL,
  ontology_id          CHAR(26)    NOT NULL,
  version_id           CHAR(26)    NOT NULL,
  load_id              CHAR(26)    NOT NULL,
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
  UNIQUE KEY foundry_load_jobs_active_uq (ontology_id, active_key),
  UNIQUE KEY foundry_load_jobs_client_token_uq (ontology_id, client_token),
  INDEX foundry_load_jobs_claim_idx (status, next_attempt_at),
  INDEX foundry_load_jobs_history_idx (ontology_id, kind, created_at, id),
  INDEX foundry_load_jobs_load_idx (load_id, created_at, id),
  INDEX foundry_load_jobs_spend_idx (workspace_id, created_at),
  CONSTRAINT foundry_load_jobs_kind_chk CHECK (kind IN ('load', 'extract', 'delete')),
  CONSTRAINT foundry_load_jobs_load_chk CHECK (kind <> 'load' OR load_id = id),
  CONSTRAINT foundry_load_jobs_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'budget_exhausted', 'failed', 'cancelled')),
  CONSTRAINT foundry_load_jobs_phase_chk CHECK (phase IN ('provisioning', 'ingest', 'documents', 'finishing', 'deleting')),
  CONSTRAINT foundry_load_jobs_delete_phase_chk CHECK ((kind = 'delete') = (phase = 'deleting')),
  CONSTRAINT foundry_load_jobs_live_chk CHECK ((status IN ('queued', 'running')) = (active_key IS NOT NULL))
);

-- +goose Down
DROP TABLE foundry_load_jobs;
CREATE TABLE foundry_graph_jobs (
  id                   CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id         CHAR(26)    NOT NULL,
  ontology_id          CHAR(26)    NOT NULL,
  version_id           CHAR(26)    NOT NULL,
  kind                 VARCHAR(16) NOT NULL,
  status               VARCHAR(16) NOT NULL,
  phase                VARCHAR(16) NOT NULL,
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

DROP TABLE foundry_load_extractions;
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

SET @drop_ontology_load_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_ontologies ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_ontologies'
    AND constraint_type = 'CHECK'
    AND (constraint_name LIKE 'foundry_ontologies_bound_chk%' OR constraint_name LIKE 'foundry_ontologies_current_chk%')
);
PREPARE drop_ontology_load_chks FROM @drop_ontology_load_chks;
EXECUTE drop_ontology_load_chks;
DEALLOCATE PREPARE drop_ontology_load_chks;

-- A deleted ontology still holding its database fails this check: let its
-- loads drain first.
ALTER TABLE foundry_ontologies
  DROP COLUMN extract_owed,
  DROP COLUMN current_load_id,
  ADD CONSTRAINT foundry_ontologies_live_chk CHECK ((deleted_at IS NULL) = (live_key IS NOT NULL));

DROP TABLE foundry_loads;
