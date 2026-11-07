-- +goose Up
-- A release decision: a person's word that a claim a document's review holds
-- back holds, which the next load writes as a fact. It names one review: the
-- document, the claim's key as the engine stores it (held::ClaimKey), 64
-- hexadecimal digits, and where the quote is (the engine's Span), as one claim
-- may be stated in several places and by statements that differ. It names no
-- type. identity_key hashes the key and the place, so the column is generated
-- again and the table rebuilt. A load's snapshot takes them with the rest. A
-- Vitess-based MySQL host may rename CHECK constraints, so the shape's is
-- dropped by prefix.
-- statement_hash is what the person read (held::StatementHash): the engine
-- refuses a claim that reads otherwise, and it's no part of the identity, as a
-- second approval of what the claim says now replaces the first.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so nothing here deletes a row, and the columns stay:
-- an older binary neither reads nor writes them. It can't read a row of kind
-- release, though: its Validate knows only resolve and prop, so its
-- ListFoundryReviewDecisions, the prune in SetCurrentOntologyVersion and every
-- Extract of that ontology fail. So before going back to a binary older than
-- this one, once anyone has approved a claim, a person must run on the database:
--
--   DELETE FROM foundry_load_decisions WHERE kind='release';
--   DELETE FROM foundry_review_decisions WHERE kind='release';
--
-- That takes the approvals, not what they wrote: a load keeps its facts, and a
-- later load holds those claims again until someone approves them. The Down is
-- for a local database and refuses while such a row is left, as the old CHECK
-- has no arm for one.
SET @drop_review_decisions_shape_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_review_decisions ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_review_decisions'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_review_decisions_shape_chk%'
);
PREPARE drop_review_decisions_shape_chk FROM @drop_review_decisions_shape_chk;
EXECUTE drop_review_decisions_shape_chk;
DEALLOCATE PREPARE drop_review_decisions_shape_chk;

ALTER TABLE foundry_review_decisions
  DROP INDEX foundry_review_decisions_identity_uq,
  DROP COLUMN identity_key,
  ADD COLUMN claim_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER value,
  ADD COLUMN claim_section INT UNSIGNED NULL AFTER claim_key,
  ADD COLUMN claim_start INT UNSIGNED NULL AFTER claim_section,
  ADD COLUMN claim_end INT UNSIGNED NULL AFTER claim_start,
  ADD COLUMN statement_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER claim_end,
  ADD COLUMN identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, doc_id, norm_name, IF(kind='prop', entity_key, NULL), prop,
    claim_key, claim_section, claim_start, claim_end
  ) AS CHAR), 256))) STORED,
  ADD UNIQUE KEY foundry_review_decisions_identity_uq (ontology_id, identity_key),
  ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR
    (kind='release' AND type='' AND doc_id IS NOT NULL AND claim_key IS NOT NULL AND claim_section IS NOT NULL AND claim_start IS NOT NULL AND claim_end IS NOT NULL AND statement_hash IS NOT NULL AND claim_start < claim_end AND norm_name IS NULL AND entity_key IS NULL AND prop IS NULL AND value IS NULL)
  );
ALTER TABLE foundry_load_decisions
  ADD COLUMN claim_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER value,
  ADD COLUMN claim_section INT UNSIGNED NULL AFTER claim_key,
  ADD COLUMN claim_start INT UNSIGNED NULL AFTER claim_section,
  ADD COLUMN claim_end INT UNSIGNED NULL AFTER claim_start,
  ADD COLUMN statement_hash CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER claim_end;

-- +goose Down
-- Deletes nothing: see the rollback note above. The new CHECK goes first, by
-- prefix, and foundry_load_decisions last, so a release row left stops this at
-- the old CHECK with both tables' columns in place, and it runs again once the
-- rows are gone.
SET @drop_review_decisions_shape_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_review_decisions ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_review_decisions'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_review_decisions_shape_chk%'
);
PREPARE drop_review_decisions_shape_chk FROM @drop_review_decisions_shape_chk;
EXECUTE drop_review_decisions_shape_chk;
DEALLOCATE PREPARE drop_review_decisions_shape_chk;

ALTER TABLE foundry_review_decisions
  DROP INDEX foundry_review_decisions_identity_uq,
  DROP COLUMN identity_key,
  DROP COLUMN claim_key,
  DROP COLUMN claim_section,
  DROP COLUMN claim_start,
  DROP COLUMN claim_end,
  DROP COLUMN statement_hash,
  ADD COLUMN identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, doc_id, norm_name, IF(kind='prop', entity_key, NULL), prop
  ) AS CHAR), 256))) STORED,
  ADD UNIQUE KEY foundry_review_decisions_identity_uq (ontology_id, identity_key),
  ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL)
  );
ALTER TABLE foundry_load_decisions
  DROP COLUMN claim_key,
  DROP COLUMN claim_section,
  DROP COLUMN claim_start,
  DROP COLUMN claim_end,
  DROP COLUMN statement_hash;
