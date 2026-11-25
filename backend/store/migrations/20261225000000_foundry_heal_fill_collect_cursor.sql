-- +goose Up
-- How far a moved fill's collect listed the load's reviewed documents (the
-- heal worker's pages of an Audit with documents_for), so a collect that
-- outlasts its time limit, or fails partway, resumes where it stopped
-- instead of starting over, and a load too large for one window is
-- collected across several: collect_after is the last document of the last
-- page it owed (pages go in document id order), collect_version_id the
-- version the load recorded when it listed it (a later move in place
-- starts the collect over), and collect_owed and collect_left what it owed
-- and left past model.FoundryFillMaxDocuments so far. Each page's documents
-- are owed ('link' rows) and its cursor saved in one transaction, so the
-- rows the load owes are always what the cursor counts. Cleared when the
-- fill starts, when a collect starts over (its own, or another of the
-- load's, which ends what the load owed), when the collect gives up, when
-- its finding is dismissed or superseded, and when another load becomes
-- current.
--
-- Four columns and one CHECK, none of which an older binary reads or
-- writes: their defaults hold the CHECK, and nothing here changes or
-- deletes a row.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the columns stay. The rollback floor (the
-- binary before them) never reads them; its fresh collect ends what a
-- cursor's pages owed, and a cursor it leaves then names rows no longer
-- owed, which this binary's start refuses (the load owes fewer than the
-- cursor counts), starting the collect over. The Down is for a local
-- database.
ALTER TABLE foundry_heal_fills
  ADD COLUMN collect_after      CHAR(26) NULL,
  ADD COLUMN collect_version_id CHAR(26) NULL,
  ADD COLUMN collect_owed       BIGINT   NOT NULL DEFAULT 0,
  ADD COLUMN collect_left       BIGINT   NOT NULL DEFAULT 0,
  ADD CONSTRAINT foundry_heal_fills_collect_chk CHECK ((collect_after IS NULL) = (collect_version_id IS NULL)
    AND (collect_after IS NOT NULL OR (collect_owed = 0 AND collect_left = 0)) AND collect_owed >= 0 AND collect_left >= 0);

-- +goose Down
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so the cursor's is dropped by its prefix.
SET @drop_heal_fill_collect_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_heal_fills ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_heal_fills'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_heal_fills_collect%'
);
PREPARE drop_heal_fill_collect_chk FROM @drop_heal_fill_collect_chk;
EXECUTE drop_heal_fill_collect_chk;
DEALLOCATE PREPARE drop_heal_fill_collect_chk;
ALTER TABLE foundry_heal_fills
  DROP COLUMN collect_left,
  DROP COLUMN collect_owed,
  DROP COLUMN collect_version_id,
  DROP COLUMN collect_after;
