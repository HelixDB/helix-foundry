-- +goose Up
-- Document sets (ontology v2, wave 1). Every document belongs to one set of
-- its ontology, and a version's extraction routes ([extractions.<name>] in
-- its mappings) name the sets whose documents fill which types, props and
-- links. An ontology starts with a set named General.
CREATE TABLE foundry_document_sets (
  id                 CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)      NOT NULL,
  ontology_id        CHAR(26)      NOT NULL,
  name               VARCHAR(120)  NOT NULL,
  -- What its documents hold, in the user's words; the ontology agent reads it.
  description        VARCHAR(1000) NOT NULL DEFAULT '',
  created_by_user_id CHAR(26)      NULL,
  created_at         DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  deleted_at         DATETIME(6)   NULL,
  -- 1 while not deleted, NULL after: names are unique among live sets.
  live_key           TINYINT       NULL,
  UNIQUE KEY foundry_document_sets_name_uq (ontology_id, name, live_key),
  CONSTRAINT foundry_document_sets_live_chk CHECK ((deleted_at IS NULL) = (live_key IS NOT NULL)),
  CONSTRAINT foundry_document_sets_live_key_chk CHECK (live_key IS NULL OR live_key = 1)
);

-- A document's set. Added nullable: a deploy request carries schema only, so
-- the backfill-foundry-document-sets data migration moves the documents
-- already there into their ontology's General set, and a later migration
-- makes the column NOT NULL. The binary reads it as NOT NULL, so stop the
-- old binary, migrate, run the backfill-foundry-document-sets step, then
-- deploy.
ALTER TABLE foundry_documents
  ADD COLUMN set_id CHAR(26) NULL AFTER ontology_id,
  ADD INDEX foundry_documents_set_idx (ontology_id, set_id, created_at);

-- A document joins a load with its current set. Moves must not change an
-- in-flight load or its retries. A new table is safe for schema-only deploys.
CREATE TABLE foundry_load_documents (
  load_id     CHAR(26) NOT NULL,
  document_id CHAR(26) NOT NULL,
  set_id      CHAR(26) NOT NULL,
  PRIMARY KEY (load_id, document_id)
);

-- A document no route of its load's version reads is stored with its
-- sections and makes no model call: out_of_scope. A Vitess-based MySQL host
-- may rename CHECK constraints, so the old one is dropped by prefix.
SET @drop_extraction_status_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_extractions ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_extractions'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_extractions_status_chk%'
);
PREPARE drop_extraction_status_chk FROM @drop_extraction_status_chk;
EXECUTE drop_extraction_status_chk;
DEALLOCATE PREPARE drop_extraction_status_chk;

ALTER TABLE foundry_load_extractions
  ADD CONSTRAINT foundry_load_extractions_status_chk CHECK (status IN ('extracted', 'duplicate', 'failed', 'out_of_scope'));

-- +goose Down
DROP TABLE foundry_load_documents;

-- An out_of_scope extraction fails the old check: delete those rows first.
SET @drop_extraction_status_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_extractions ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_extractions'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_extractions_status_chk%'
);
PREPARE drop_extraction_status_chk FROM @drop_extraction_status_chk;
EXECUTE drop_extraction_status_chk;
DEALLOCATE PREPARE drop_extraction_status_chk;

ALTER TABLE foundry_load_extractions
  ADD CONSTRAINT foundry_load_extractions_status_chk CHECK (status IN ('extracted', 'duplicate', 'failed'));

ALTER TABLE foundry_documents
  DROP INDEX foundry_documents_set_idx,
  DROP COLUMN set_id;

DROP TABLE foundry_document_sets;
