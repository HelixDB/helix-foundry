-- +goose Up
-- The reviews_waiting trigger: a workflow that runs when an ontology's
-- current load has at least so many review cards open (foundry_review_loads'
-- count). It is an event trigger with a target and a cursor, so both of
-- foundry_automations' checks that name the kinds widen to take it.
--
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so each check is dropped by its prefix, whatever the
-- host named it, in one prepared ALTER, and added again wide. Up can be run
-- again. Nothing here changes a row.
SET @drop_automation_kind_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_automations ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_automations'
    AND constraint_type = 'CHECK'
    AND (constraint_name LIKE 'foundry_automations_trigger_ch%' OR constraint_name LIKE 'foundry_automations_cursor_ch%')
);
PREPARE drop_automation_kind_chks FROM @drop_automation_kind_chks;
EXECUTE drop_automation_kind_chks;
DEALLOCATE PREPARE drop_automation_kind_chks;

ALTER TABLE foundry_automations
  ADD CONSTRAINT foundry_automations_trigger_chk CHECK (
    trigger_kind IN ('schedule', 'sync_finished', 'load_ready', 'document_arrived', 'generation_finished', 'reviews_waiting', 'manual') AND
    (trigger_target IS NULL) = (trigger_kind IN ('schedule', 'manual'))
  ),
  ADD CONSTRAINT foundry_automations_cursor_chk CHECK (
    (cursor_at IS NOT NULL) = (status = 'active' AND trigger_kind IN ('sync_finished', 'load_ready', 'document_arrived', 'generation_finished', 'reviews_waiting'))
  );

-- +goose Down
-- The wide checks are harmless to an older binary, which never writes the
-- kind, and Up's drop by prefix can be run again, so there is nothing to undo.
SELECT 1;
