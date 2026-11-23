-- +goose Up
-- The rebuilds of current loads this install owes: a release whose engine
-- writes the graph differently than the one before it owes one, added as a
-- row by its own migration with the time it ran, which is when this
-- install first started that release. At every start foundry queues a full
-- reload of each live ontology's current load created before owed_since
-- (local.RebuildOwedLoads, store.RebuildFoundryLoads), again every minute
-- while one is owed (local.Rebuilds), and marks the row done once none is
-- queued, reloading or held back by a live job.
CREATE TABLE foundry_local_rebuilds (
  name       VARCHAR(64) NOT NULL PRIMARY KEY,
  owed_since DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  done_at    DATETIME(6) NULL
);

-- The engine of this release keeps every datetime prop as Helix's typed
-- DateTime, to the millisecond, where loads before it wrote RFC 3339 text.
INSERT INTO foundry_local_rebuilds (name) VALUES ('typed_datetime');

-- +goose Down
DROP TABLE foundry_local_rebuilds;
