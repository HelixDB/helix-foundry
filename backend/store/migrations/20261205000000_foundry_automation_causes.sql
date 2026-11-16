-- +goose Up
-- What an automation's run started, so its triggers don't loop and its
-- action steps wait for their work. Columns only, every one nullable: no row
-- is changed, and a binary from before writes and reads these tables as it
-- did, leaving them NULL.
--
-- A sync or a load an automation's run started (an action step, or an action
-- its Assistant step ran as its owner approved) names that run
-- (started_by_automation_run_id) and how many automation runs led to it, that
-- one included (automation_chain_depth: 1 for a run a person, a schedule or
-- other work set off, one more for each run set off by work another run
-- started). An event trigger skips work its own automation started, directly
-- or through other automations, and work past the depth a chain may reach.
-- NULL is work a person or Foundry itself started.
ALTER TABLE connector_syncs
  ADD COLUMN started_by_automation_run_id CHAR(26) NULL,
  ADD COLUMN automation_chain_depth       INT      NULL;

ALTER TABLE foundry_loads
  ADD COLUMN started_by_automation_run_id CHAR(26) NULL,
  ADD COLUMN automation_chain_depth       INT      NULL;

-- A refresh a run's sync owed is that run's too: the sync's cause is kept
-- with the refresh it owes (refresh_cause_*, while refresh_due_at is set;
-- NULL once syncs of different causes owe it) and given to the refresh load
-- queued for it, so a load_ready trigger whose own sync changed the tables
-- doesn't fire on the load that change queued.
ALTER TABLE foundry_ontologies
  ADD COLUMN refresh_cause_run_id CHAR(26) NULL,
  ADD COLUMN refresh_cause_depth  INT      NULL;

-- The sync or load an action step started and waits for (running), by id;
-- the step's action says which kind. A run picked up again after its worker
-- stopped reads it and waits on, never starting another.
ALTER TABLE foundry_automation_run_nodes
  ADD COLUMN job_id CHAR(26) NULL;

-- +goose Down
ALTER TABLE foundry_automation_run_nodes
  DROP COLUMN job_id;

ALTER TABLE foundry_ontologies
  DROP COLUMN refresh_cause_depth,
  DROP COLUMN refresh_cause_run_id;

ALTER TABLE foundry_loads
  DROP COLUMN automation_chain_depth,
  DROP COLUMN started_by_automation_run_id;

ALTER TABLE connector_syncs
  DROP COLUMN automation_chain_depth,
  DROP COLUMN started_by_automation_run_id;
