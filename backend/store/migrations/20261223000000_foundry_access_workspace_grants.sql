-- +goose Up
-- A workspace grant (target_kind 'workspace'): everything in the workspace
-- but People enrichment, now and later, naming no connector, table or set.
-- Drop foundry_access_grants' target check whatever the host named it (a
-- Vitess-based host, PlanetScale, names a CHECK by its first 38 characters
-- and a hash: foundry_access_grants_target_ch_<hash>) and add it back with
-- the workspace case. No row changes.
SET @drop_access_target_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_access_grants ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_access_grants'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_access_grants_target_ch%'
);
PREPARE drop_access_target_chks FROM @drop_access_target_chks;
EXECUTE drop_access_target_chks;
DEALLOCATE PREPARE drop_access_target_chks;

ALTER TABLE foundry_access_grants
  ADD CONSTRAINT foundry_access_grants_target_chk CHECK (
    (target_kind = 'connector' AND connector_id <> '' AND table_name = '' AND document_set_id = '') OR
    (target_kind = 'table' AND connector_id <> '' AND table_name <> '' AND document_set_id = '') OR
    (target_kind = 'document_set' AND connector_id = '' AND table_name = '' AND document_set_id <> '') OR
    (target_kind = 'enrichment' AND connector_id = '' AND table_name = '' AND document_set_id = '') OR
    (target_kind = 'workspace' AND connector_id = '' AND table_name = '' AND document_set_id = ''));

-- +goose Down
-- Workspace grants go: a principal that held only one then holds nothing,
-- and stays restricted (taking grants away never lifts that).
DELETE FROM foundry_access_grants WHERE target_kind = 'workspace';

SET @drop_access_target_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_access_grants ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_access_grants'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_access_grants_target_ch%'
);
PREPARE drop_access_target_chks FROM @drop_access_target_chks;
EXECUTE drop_access_target_chks;
DEALLOCATE PREPARE drop_access_target_chks;

ALTER TABLE foundry_access_grants
  ADD CONSTRAINT foundry_access_grants_target_chk CHECK (
    (target_kind = 'connector' AND connector_id <> '' AND table_name = '' AND document_set_id = '') OR
    (target_kind = 'table' AND connector_id <> '' AND table_name <> '' AND document_set_id = '') OR
    (target_kind = 'document_set' AND connector_id = '' AND table_name = '' AND document_set_id <> '') OR
    (target_kind = 'enrichment' AND connector_id = '' AND table_name = '' AND document_set_id = ''));
