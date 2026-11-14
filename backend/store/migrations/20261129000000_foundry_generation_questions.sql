-- +goose Up
-- A generation the ontology agent asked questions of waits for the answers
-- (needs_input), still the ontology's live generation (active_key 1), and
-- carries on from where the agent asked once they come. One may also be
-- cancelled. What the asking turn left:
--   turn_state  the agent's session state, opaque, at most about 1 MB; it
--               may quote the data, so only the worker reads it, and the
--               generation's end drops it;
--   draft_rev   the draft revision the questions were asked of, which the
--               answers' feedback names;
--   questions   the questions, as the agent wrote them;
--   asked_at    when, from which the worker answers with the agent's
--               defaults after foundry.ontology.answer_days;
--   answers     the answers, one per question at most, NULL while it waits.
-- draft_rev, questions and asked_at are set together; the questions and
-- answers of the last round stay once the generation ends.
--
-- Five nullable columns, and the status check widened: nothing here changes
-- or deletes a row.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the columns and checks stay. An older binary
-- never claims a needs_input generation, which holds its ontology's live
-- generation until cancelled: before rolling back, fail them
-- (UPDATE foundry_ontology_generations SET status='failed',
-- error='the generation was stopped', active_key=NULL, turn_state=NULL,
-- finished_at=NOW(6) WHERE status='needs_input'). The Down does that, and
-- turns cancelled ones into failed ones, for a local database.
ALTER TABLE foundry_ontology_generations
  ADD COLUMN turn_state MEDIUMBLOB      NULL,
  ADD COLUMN draft_rev  BIGINT UNSIGNED NULL,
  ADD COLUMN questions  JSON            NULL,
  ADD COLUMN asked_at   DATETIME(6)     NULL,
  ADD COLUMN answers    JSON            NULL;

-- A Vitess-based MySQL host may rename CHECK constraints, so the old one is
-- dropped by prefix.
SET @drop_generation_status_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_ontology_generations ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_ontology_generations'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_ontology_generations_status_chk%'
);
PREPARE drop_generation_status_chk FROM @drop_generation_status_chk;
EXECUTE drop_generation_status_chk;
DEALLOCATE PREPARE drop_generation_status_chk;

-- needs_input holds state and no answers; a live generation with answers
-- holds the state they carry on from; an ended one holds no state.
ALTER TABLE foundry_ontology_generations
  ADD CONSTRAINT foundry_ontology_generations_status_chk
    CHECK (status IN ('queued', 'running', 'needs_input', 'succeeded', 'failed', 'cancelled')),
  ADD CONSTRAINT foundry_ontology_generations_asked_chk CHECK (
    (draft_rev IS NULL) = (questions IS NULL) AND (questions IS NULL) = (asked_at IS NULL)
    AND (turn_state IS NULL OR questions IS NOT NULL)
    AND (answers IS NULL OR questions IS NOT NULL)
    AND (status <> 'needs_input' OR (turn_state IS NOT NULL AND answers IS NULL))
    AND (status NOT IN ('queued', 'running') OR answers IS NULL OR turn_state IS NOT NULL)
    AND (status IN ('queued', 'running', 'needs_input') OR turn_state IS NULL)
  );

-- +goose Down
UPDATE foundry_ontology_generations
SET status='failed', error=COALESCE(error, 'the generation was stopped'), active_key=NULL, turn_state=NULL, finished_at=COALESCE(finished_at, NOW(6))
WHERE status IN ('needs_input', 'cancelled');

SET @drop_generation_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_ontology_generations ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_ontology_generations'
    AND constraint_type = 'CHECK'
    AND (constraint_name LIKE 'foundry_ontology_generations_status_chk%' OR constraint_name LIKE 'foundry_ontology_generations_asked_chk%')
);
PREPARE drop_generation_chks FROM @drop_generation_chks;
EXECUTE drop_generation_chks;
DEALLOCATE PREPARE drop_generation_chks;

ALTER TABLE foundry_ontology_generations
  ADD CONSTRAINT foundry_ontology_generations_status_chk CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
  DROP COLUMN answers,
  DROP COLUMN asked_at,
  DROP COLUMN questions,
  DROP COLUMN draft_rev,
  DROP COLUMN turn_state;
