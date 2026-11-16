-- +goose Up
-- Suggestions now have kinds (questions, dashboards, changes to the ontology
-- and workflows), written in one call per version and kept in the same JSON
-- column, each item with its kind and topic: no column changes. The rows
-- written before kinds hold workflows alone, which a read keeps nothing of,
-- so they are deleted and the next read of each version queues its write
-- again with every kind. The ledger stays, so the month's spend and the
-- hour's queues still count what was spent and queued.
DELETE FROM foundry_automation_suggestions;

-- +goose Down
-- Rows written with kinds read as before kinds wrote them, workflows among
-- the other kinds, so nothing is undone.
SELECT 1;
