-- +goose Up
-- What reading a document once, at upload, gave: a table file's rows or a
-- PDF's text. Completing the
-- upload of a table file or a PDF queues a standard read; the
-- foundry-document-worker claims it under a lease (attempts counts the
-- claims since the last one that ended in order, so a file that keeps
-- killing the worker fails as unsupported) and records what the file gave,
-- or why it could not be read. A deep read is a second row. The result is
-- written once and never changes, so loads and generations replay it.
-- The documents already uploaded get their queued read from the
-- backfill-foundry-document-reads data step.
CREATE TABLE foundry_document_reads (
  document_id          CHAR(26)      NOT NULL,
  mode                 VARCHAR(16)   NOT NULL,
  workspace_id         CHAR(26)      NOT NULL,
  status               VARCHAR(16)   NOT NULL,
  attempts             INT           NOT NULL DEFAULT 0,
  next_attempt_at      DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  claim_token          CHAR(26)      NULL,
  claimed_until        DATETIME(6)   NULL,
  job_id               VARCHAR(128)  NULL,
  page_count           INT           NULL,
  row_count            BIGINT        NULL,
  usd_micros           BIGINT        NOT NULL DEFAULT 0,
  text_sha256          CHAR(64)      NULL,
  artifact_key         VARCHAR(1024) NULL,
  error_code           VARCHAR(16)   NULL,
  last_error           VARCHAR(1024) NULL,
  requested_by_user_id CHAR(26)      NULL,
  created_at           DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at          DATETIME(6)   NULL,
  PRIMARY KEY (document_id, mode),
  KEY foundry_document_reads_due_idx (status, next_attempt_at),
  KEY foundry_document_reads_workspace_idx (workspace_id, created_at),
  CONSTRAINT foundry_document_reads_mode_chk CHECK (mode IN ('standard', 'deep')),
  CONSTRAINT foundry_document_reads_status_chk CHECK (status IN ('queued', 'running', 'submitted', 'succeeded', 'failed')),
  CONSTRAINT foundry_document_reads_shape_chk CHECK (
    (status <> 'submitted' OR job_id IS NOT NULL) AND
    (status = 'succeeded') = (artifact_key IS NOT NULL) AND
    (status = 'failed') = (error_code IS NOT NULL) AND
    (page_count IS NULL OR row_count IS NULL)
  ),
  CONSTRAINT foundry_document_reads_code_chk CHECK (error_code IS NULL OR error_code IN ('too_large', 'unsupported'))
);

-- +goose Down
DROP TABLE foundry_document_reads;
