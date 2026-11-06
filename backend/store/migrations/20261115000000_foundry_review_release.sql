-- +goose Up
-- A release decision: a person's word that a claim a document's review holds
-- back holds, which the next load writes as a fact. It names one review: the
-- document, the claim's key as the engine stores it (held::ClaimKey), 64
-- hexadecimal digits, and where the quote is (the engine's Span), as one claim
-- may be stated in several places and by statements that differ. It names no
-- type. identity_key hashes the key and the place, so the column is generated
-- again and the table rebuilt. A load's snapshot takes them with the rest.
--
-- A binary before this one fails on any row of kind release, as its Validate
-- knows only resolve and prop: rolling back takes this migration's Down, which
-- deletes those rows, before the older binary starts.
ALTER TABLE foundry_review_decisions
  DROP CHECK foundry_review_decisions_shape_chk,
  DROP INDEX foundry_review_decisions_identity_uq,
  DROP COLUMN identity_key,
  ADD COLUMN claim_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER value,
  ADD COLUMN claim_section INT UNSIGNED NULL AFTER claim_key,
  ADD COLUMN claim_start INT UNSIGNED NULL AFTER claim_section,
  ADD COLUMN claim_end INT UNSIGNED NULL AFTER claim_start,
  ADD COLUMN identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, doc_id, norm_name, IF(kind='prop', entity_key, NULL), prop,
    claim_key, claim_section, claim_start, claim_end
  ) AS CHAR), 256))) STORED,
  ADD UNIQUE KEY foundry_review_decisions_identity_uq (ontology_id, identity_key),
  ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL) OR
    (kind='release' AND type='' AND doc_id IS NOT NULL AND claim_key IS NOT NULL AND claim_section IS NOT NULL AND claim_start IS NOT NULL AND claim_end IS NOT NULL AND claim_start < claim_end AND norm_name IS NULL AND entity_key IS NULL AND prop IS NULL AND value IS NULL)
  );
ALTER TABLE foundry_load_decisions
  ADD COLUMN claim_key CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER value,
  ADD COLUMN claim_section INT UNSIGNED NULL AFTER claim_key,
  ADD COLUMN claim_start INT UNSIGNED NULL AFTER claim_section,
  ADD COLUMN claim_end INT UNSIGNED NULL AFTER claim_start;

-- +goose Down
DELETE FROM foundry_review_decisions WHERE kind='release';
DELETE FROM foundry_load_decisions WHERE kind='release';
ALTER TABLE foundry_load_decisions
  DROP COLUMN claim_key,
  DROP COLUMN claim_section,
  DROP COLUMN claim_start,
  DROP COLUMN claim_end;
ALTER TABLE foundry_review_decisions
  DROP CHECK foundry_review_decisions_shape_chk,
  DROP INDEX foundry_review_decisions_identity_uq,
  DROP COLUMN identity_key,
  DROP COLUMN claim_key,
  DROP COLUMN claim_section,
  DROP COLUMN claim_start,
  DROP COLUMN claim_end,
  ADD COLUMN identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, doc_id, norm_name, IF(kind='prop', entity_key, NULL), prop
  ) AS CHAR), 256))) STORED,
  ADD UNIQUE KEY foundry_review_decisions_identity_uq (ontology_id, identity_key),
  ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL)
  );
