-- +goose Up
-- How many violation records a load's ingest left open, as its report
-- counted them: NULL when the report did not count every chunk (a retry that
-- skipped chunks an earlier attempt wrote) or the load predates the column.
-- Nullable with no default, so a schema-only deploy adds it with nothing to
-- backfill and an old binary keeps working.
ALTER TABLE foundry_loads ADD COLUMN ingest_violations BIGINT UNSIGNED NULL;

-- +goose Down
ALTER TABLE foundry_loads DROP COLUMN ingest_violations;
