-- +goose Up
-- A reject decision: a person's word that a claim a document's review holds
-- back is wrong. It names one review as a release does: the document, the
-- claim's key and where the quote is, with the hash of the statement the
-- person read. It writes nothing and is sent to no engine: the review stays in
-- the graph, where no read sees it, and the site leaves its card out of this
-- load's Needs review and every later load's. Removing the decision brings the
-- card back. A claim has a release or a reject, never both: settling one
-- removes the other (the backend, under the ontology's lock).
--
-- Only the shape's CHECK changes: kind is one more value, with a release's
-- columns. identity_key already hashes kind with the claim's key and place, so
-- the column and its unique key stay as they are and no table is rebuilt for
-- them. Nothing here changes or deletes a row. A load's snapshot
-- (foundry_load_decisions) never holds a reject: a load copies every kind but
-- this one.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the CHECK stays. An older binary can't read a
-- row of kind reject: its Validate knows only resolve, prop and release, so
-- its ListFoundryReviewDecisions and the prune in SetCurrentOntologyVersion
-- fail on one, a load it creates copies the row into its snapshot, and any
-- Extract that sends the row's document then fails too. So before going back
-- to a binary older than this one, once anyone has rejected a claim, a person
-- must run on the database:
--
--   DELETE FROM foundry_load_decisions WHERE kind='reject';
--   DELETE FROM foundry_review_decisions WHERE kind='reject';
--
-- That takes the rejections and nothing else: the graph never held them, and
-- the claims are listed for review again. The first DELETE finds rows only
-- where an older binary made a load while reject rows existed. The Down is
-- for a local database and deletes nothing: a reject row left in
-- foundry_review_decisions stops it at the old CHECK, which has no arm for
-- one, and it runs again once the rows are gone.
--
-- A Vitess-based MySQL host may rename CHECK constraints, so the old one is
-- dropped by prefix.
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
  ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR
    (kind IN ('release', 'reject') AND type='' AND doc_id IS NOT NULL AND claim_key IS NOT NULL AND claim_section IS NOT NULL AND claim_start IS NOT NULL AND claim_end IS NOT NULL AND statement_hash IS NOT NULL AND claim_start < claim_end AND norm_name IS NULL AND entity_key IS NULL AND prop IS NULL AND value IS NULL)
  );

-- +goose Down
-- Deletes nothing: see the rollback note above. The old CHECK goes back in
-- the statement that drops the new one, by prefix, so a reject row left in
-- foundry_review_decisions stops it with the new CHECK still in place, and it
-- runs again once a person has run the two DELETEs.
SET @drop_review_decisions_shape_chk = (
  SELECT CONCAT(
    'ALTER TABLE foundry_review_decisions ',
    COALESCE(CONCAT(GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', '), ', '), ''),
    'ADD CONSTRAINT foundry_review_decisions_shape_chk CHECK (',
    '(kind=''resolve'' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR ',
    '(kind=''prop'' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL AND claim_key IS NULL AND claim_section IS NULL AND claim_start IS NULL AND claim_end IS NULL AND statement_hash IS NULL) OR ',
    '(kind=''release'' AND type='''' AND doc_id IS NOT NULL AND claim_key IS NOT NULL AND claim_section IS NOT NULL AND claim_start IS NOT NULL AND claim_end IS NOT NULL AND statement_hash IS NOT NULL AND claim_start < claim_end AND norm_name IS NULL AND entity_key IS NULL AND prop IS NULL AND value IS NULL))'
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
