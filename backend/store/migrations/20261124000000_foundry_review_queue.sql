-- +goose Up
-- The review queue: one list of an ontology's open reviews, in the order of
-- the facts each decision writes, with a slice of its cards drawn at random
-- from every score band to measure precision. Nothing here changes or deletes
-- a row.
--
-- foundry_review_decisions.drawn is how the queue showed the card a decision
-- was made on: 'ranked' (in its order) or 'random' (its random slice). NULL is
-- a decision made anywhere else, which is every decision saved before this.
-- Only the review's own table has it: a load's snapshot (foundry_load_decisions)
-- copies the columns it names, and the engine is never sent it.
--
-- foundry_review_labels is a person's word on a claim at its place, kept to
-- measure precision and nothing else: no load, decision or Extract reads it.
-- A label is of a held claim (its key and the hash of the statement read,
-- kept as its decision of the random slice is saved), or of an accepted
-- fact's quote (its fact's key), at its document, section and byte range.
-- drawn 'random' is a card of the random slice; 'recheck' a second person's
-- word on a decision or a label someone else made, whose verdict it checks is
-- `against`. confidence is the verifier's score as the claim was read, so a
-- label keeps its band when the review or the decision is gone. One label per
-- person and claim: saying it again replaces the verdict.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the column and the table stay. An older binary
-- names its columns, so it neither reads nor writes drawn (a decision it
-- saves anew keeps the value a row held), and it never reads the table. So
-- nothing must be deleted before going back. The Down is for a local
-- database: it drops the table and the column, and the labels and draws with
-- them.
ALTER TABLE foundry_review_decisions
  ADD COLUMN drawn VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NULL,
  ADD CONSTRAINT foundry_review_decisions_drawn_chk CHECK (drawn IS NULL OR drawn IN ('ranked', 'random'));

CREATE TABLE foundry_review_labels (
  id                 CHAR(26)     NOT NULL PRIMARY KEY,
  ontology_id        CHAR(26)     NOT NULL,
  load_id            CHAR(26)     NOT NULL,
  drawn              VARCHAR(8)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  doc_id             CHAR(26)     NOT NULL,
  claim_section      INT UNSIGNED NOT NULL,
  claim_start        INT UNSIGNED NOT NULL,
  claim_end          INT UNSIGNED NOT NULL,
  claim_key          CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  statement_hash     CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NULL,
  fact_key           VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
  link               VARCHAR(255) COLLATE utf8mb4_bin NULL,
  confidence         DOUBLE       NULL,
  verdict            VARCHAR(8)   CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  decision_id        CHAR(26)     NULL,
  against            VARCHAR(8)   CHARACTER SET ascii COLLATE ascii_bin NULL,
  decided_by_user_id CHAR(26)     NOT NULL,
  decided_at         DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- The claim: a held claim's key or an accepted fact's, at its place.
  identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    doc_id, claim_section, claim_start, claim_end, claim_key, fact_key
  ) AS CHAR), 256))) STORED,
  UNIQUE KEY foundry_review_labels_identity_uq (ontology_id, identity_key, decided_by_user_id),
  -- Lists page an ontology's labels by id.
  INDEX foundry_review_labels_ontology_idx (ontology_id, id),
  CONSTRAINT foundry_review_labels_shape_chk CHECK (
    drawn IN ('random', 'recheck') AND
    verdict IN ('right', 'wrong') AND
    claim_start < claim_end AND
    ((claim_key IS NOT NULL AND statement_hash IS NOT NULL AND fact_key IS NULL) OR
     (claim_key IS NULL AND statement_hash IS NULL AND fact_key IS NOT NULL)) AND
    (drawn = 'recheck') = (against IS NOT NULL) AND
    (against IS NULL OR against IN ('right', 'wrong')) AND
    (confidence IS NULL OR (confidence >= 0 AND confidence <= 1))
  )
);

-- +goose Down
-- For a local database: drops what the Up added, labels and draws included.
-- Vitess may rename a CHECK, so drawn's goes by prefix before its column.
DROP TABLE foundry_review_labels;
SET @drop_review_decisions_drawn_chk = (
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
    AND constraint_name LIKE 'foundry_review_decisions_drawn_chk%'
);
PREPARE drop_review_decisions_drawn_chk FROM @drop_review_decisions_drawn_chk;
EXECUTE drop_review_decisions_drawn_chk;
DEALLOCATE PREPARE drop_review_decisions_drawn_chk;
ALTER TABLE foundry_review_decisions DROP COLUMN drawn;
