-- +goose Up
-- Connector-owned documents. A connector kind gives each row of a table one
-- text document, which its sync writes into one read-only set per connector
-- and stream. Such a document has the row's key (external_id), the source's
-- version and time of it, maybe a link (source_url), and no client token or
-- uploader: it is never pending, and its set is the owner. An upload keeps
-- its token and has none of the source fields. live_external_id is
-- external_id while the document is uploaded, so a set holds at most one live
-- document per row: a replacement deletes the old one and inserts the new
-- one in a transaction. Every row already here is an upload and passes the
-- CHECK. ascii_bin: source ids are case-sensitive.
ALTER TABLE foundry_documents
  MODIFY COLUMN client_token VARCHAR(64) NULL,
  ADD COLUMN external_id    VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER set_id,
  ADD COLUMN source_version VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER external_id,
  ADD COLUMN source_at      DATETIME(6) NULL AFTER source_version,
  ADD COLUMN source_url     VARCHAR(2048) NULL AFTER source_at,
  ADD COLUMN live_external_id VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin
      GENERATED ALWAYS AS (IF(status = 'uploaded', external_id, NULL)) STORED,
  ADD UNIQUE KEY foundry_documents_live_external_uq (set_id, live_external_id),
  ADD CONSTRAINT foundry_documents_source_chk CHECK (
    (external_id IS NULL) = (source_version IS NULL) AND
    (external_id IS NULL) = (source_at IS NULL) AND
    (external_id IS NULL) = (client_token IS NOT NULL) AND
    (external_id IS NULL OR (status <> 'pending_upload' AND created_by_user_id IS NULL)) AND
    (source_url IS NULL OR external_id IS NOT NULL));

-- A set a connector owns: one per connector and stream, for as long as the
-- connector lives. No person made it, and its name is the stream's title.
ALTER TABLE foundry_document_sets
  ADD COLUMN connector_id CHAR(26) NULL AFTER workspace_id,
  ADD COLUMN stream VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER connector_id,
  ADD UNIQUE KEY foundry_document_sets_stream_uq (connector_id, stream),
  ADD CONSTRAINT foundry_document_sets_owner_chk CHECK (
    (connector_id IS NULL) = (stream IS NULL) AND (connector_id IS NULL OR created_by_user_id IS NULL));

-- Document prefixes for the sync worker's sweep to delete after the grace: a
-- flush's reservation (claimed when it publishes) or a removed connector document.
CREATE TABLE foundry_document_purges (
  document_id  CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX foundry_document_purges_age_idx (created_at)
);

-- +goose Down
-- Refused while a connector document exists, deleted or not: client_token
-- can't be NOT NULL again. A binary that writes them has run, so roll back to
-- the reader release's binary, not the schema. The documents' ALTER goes
-- first and applies whole or not at all, so a refused run leaves everything
-- in place. A Vitess-based MySQL host may rename CHECK constraints, so they
-- are dropped by prefix. A set a connector made and left empty stays, as a set
-- no person made.
SET @down_foundry_documents = (
  SELECT CONCAT(
    'ALTER TABLE foundry_documents ',
    COALESCE(GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`, ') SEPARATOR ''), ''),
    'DROP INDEX foundry_documents_live_external_uq, DROP COLUMN live_external_id, DROP COLUMN source_url, ',
    'DROP COLUMN source_at, DROP COLUMN source_version, DROP COLUMN external_id, ',
    'MODIFY COLUMN client_token VARCHAR(64) NOT NULL'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_documents'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_documents_source_chk%'
);
PREPARE down_foundry_documents FROM @down_foundry_documents;
EXECUTE down_foundry_documents;
DEALLOCATE PREPARE down_foundry_documents;

SET @down_foundry_document_sets = (
  SELECT CONCAT(
    'ALTER TABLE foundry_document_sets ',
    COALESCE(GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`, ') SEPARATOR ''), ''),
    'DROP INDEX foundry_document_sets_stream_uq, DROP COLUMN stream, DROP COLUMN connector_id'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_document_sets'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_document_sets_owner_chk%'
);
PREPARE down_foundry_document_sets FROM @down_foundry_document_sets;
EXECUTE down_foundry_document_sets;
DEALLOCATE PREPARE down_foundry_document_sets;

DROP TABLE foundry_document_purges;
