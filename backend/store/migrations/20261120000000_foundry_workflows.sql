-- +goose Up
-- Workflows: an agent's task over one ontology. Each run of one records the
-- agent's request (input), the calls it made with the run's id (accesses:
-- its reads and questions of the ontology's loads, its sends to API sources
-- and the documents it uploaded) and its answer (output), so the runs show
-- what the ontology gave the agent. An ontology has at most 100 workflows;
-- deleting it, or a workflow, deletes their runs and accesses.
CREATE TABLE foundry_workflows (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)     NOT NULL,
  ontology_id        CHAR(26)     NOT NULL,
  name               VARCHAR(100) NOT NULL,
  description        TEXT         NOT NULL,
  client_token       VARCHAR(64)  NOT NULL,
  created_by_user_id CHAR(26)     NOT NULL,
  created_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_workflows_client_token_uq (ontology_id, client_token),
  INDEX foundry_workflows_list_idx (ontology_id, created_at, id),
  CONSTRAINT foundry_workflows_name_chk CHECK (name <> '')
);

-- A run is open, taking accesses, until it is finished with its output and
-- input (which it may have had since it started). started_by is a user or an
-- agent's service credential, never both. The counts sum its accesses as
-- they are recorded, at most 1,000 (foundrymodel.FoundryWorkflowRunMaxAccesses);
-- result_bytes is the read results it stores, at most 4 MiB. A workflow
-- keeps its newest 1,000 runs.
CREATE TABLE foundry_workflow_runs (
  id                               CHAR(26)    NOT NULL PRIMARY KEY,
  workflow_id                      CHAR(26)    NOT NULL,
  workspace_id                     CHAR(26)    NOT NULL,
  ontology_id                      CHAR(26)    NOT NULL,
  status                           VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  input                            MEDIUMTEXT  NULL,
  output                           MEDIUMTEXT  NULL,
  client_token                     VARCHAR(64) NOT NULL,
  started_by_user_id               CHAR(26)    NULL,
  started_by_service_credential_id CHAR(26)    NULL,
  started_at                       DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at                      DATETIME(6) NULL,
  accesses                         INT         NOT NULL DEFAULT 0,
  reads_count                      INT         NOT NULL DEFAULT 0,
  questions                        INT         NOT NULL DEFAULT 0,
  sends                            INT         NOT NULL DEFAULT 0,
  uploads                          INT         NOT NULL DEFAULT 0,
  pieces                           INT         NOT NULL DEFAULT 0,
  results_omitted                  INT         NOT NULL DEFAULT 0,
  result_bytes                     BIGINT      NOT NULL DEFAULT 0,
  UNIQUE KEY foundry_workflow_runs_client_token_uq (workflow_id, client_token),
  INDEX foundry_workflow_runs_list_idx (workflow_id, started_at, id),
  INDEX foundry_workflow_runs_ontology_idx (ontology_id),
  CONSTRAINT foundry_workflow_runs_status_chk CHECK (status IN ('open', 'finished')),
  CONSTRAINT foundry_workflow_runs_finished_chk CHECK (
    (status = 'finished') = (finished_at IS NOT NULL) AND
    (status = 'open' OR (input IS NOT NULL AND output IS NOT NULL)) AND
    (status = 'finished' OR output IS NULL)
  ),
  CONSTRAINT foundry_workflow_runs_started_by_chk CHECK (
    (started_by_user_id IS NULL) <> (started_by_service_credential_id IS NULL)
  ),
  CONSTRAINT foundry_workflow_runs_counts_chk CHECK (
    accesses BETWEEN 0 AND 1000 AND accesses = reads_count + questions + sends + uploads AND
    pieces >= 0 AND results_omitted BETWEEN 0 AND reads_count AND result_bytes BETWEEN 0 AND 4194304
  )
);

-- One call a run recorded, by its place in the run (seq, from 1). kind says
-- what detail holds (foundrymodel.FoundryWorkflowAccess): a read's load and
-- reads, with its result in result unless it was left out; a question's load
-- and id; a send's source, seq and piece ids; an upload's document and set.
-- dedupe_key is what a retried call is recorded under once (a question, a
-- send, a document); a read has none, as each is its own.
CREATE TABLE foundry_workflow_run_accesses (
  run_id     CHAR(26)     NOT NULL,
  seq        INT          NOT NULL,
  kind       VARCHAR(16)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  dedupe_key VARCHAR(200) CHARACTER SET ascii COLLATE ascii_bin NULL,
  detail     JSON         NOT NULL,
  result     MEDIUMTEXT   NULL,
  PRIMARY KEY (run_id, seq),
  UNIQUE KEY foundry_workflow_run_accesses_dedupe_uq (run_id, dedupe_key),
  CONSTRAINT foundry_workflow_run_accesses_kind_chk CHECK (kind IN ('read', 'question', 'send', 'upload')),
  CONSTRAINT foundry_workflow_run_accesses_seq_chk CHECK (seq BETWEEN 1 AND 1000),
  CONSTRAINT foundry_workflow_run_accesses_shape_chk CHECK (
    (kind = 'read') = (dedupe_key IS NULL) AND (kind = 'read' OR result IS NULL)
  )
);

-- +goose Down
DROP TABLE foundry_workflow_run_accesses;
DROP TABLE foundry_workflow_runs;
DROP TABLE foundry_workflows;
