-- +goose Up
-- 20261130100000 widened foundry_ontology_generations' status check by
-- dropping the old one by the prefix foundry_ontology_generations_status_chk.
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so the old one there is
-- foundry_ontology_generations_status_ch_<hash>: the prefix missed it, it
-- stayed beside the wide one, and it refused needs_input and cancelled. Drop
-- every status check, whatever the host named it, and add the wide one again.
-- Nothing here changes a row; a host where 20261130100000 already dropped
-- the old check ends the same.
SET @drop_generation_status_chks = (
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
    AND constraint_name LIKE 'foundry_ontology_generations_status_ch%'
);
PREPARE drop_generation_status_chks FROM @drop_generation_status_chks;
EXECUTE drop_generation_status_chks;
DEALLOCATE PREPARE drop_generation_status_chks;

ALTER TABLE foundry_ontology_generations
  ADD CONSTRAINT foundry_ontology_generations_status_chk
    CHECK (status IN ('queued', 'running', 'needs_input', 'succeeded', 'failed', 'cancelled'));

-- +goose Down
-- The wide check is 20261130100000's schema, which its own Down narrows, so
-- there is nothing to undo here.
SELECT 1;
