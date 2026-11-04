-- +goose Up
-- Self-healing findings: what the ontology agent's Audit found in an
-- ontology's current load, summed over the load's pages by the
-- foundry-heal-worker, one row per finding. A finding's fingerprint is the
-- engine's (the SHA-256 of its signal's kind and target, as hex), the same
-- across versions, loads and pages, so a later run's finding updates the
-- same row.
--
-- status is open while the latest complete run found it, superseded once a
-- complete run of a measured kind no longer does (its target went), and
-- dismissed from when a person dismisses it (dismissed_by) until a run finds
-- twice the support it had then (support_at_dismissal), or, for a finding
-- that lists the spellings it gathers, a spelling the dismissed finding
-- didn't hold. A dismissed finding keeps finding as it was dismissed, so its
-- spellings are the ones it was dismissed with. drafting, checking, approved
-- and failed are for the fixes a later change drafts and checks.
--
-- finding is the engine's Finding (audit_result.json) with its backing
-- summed over the load's pages; support and sources are that backing's
-- reviews and documents (a row finding's support is its rows, its sources
-- 0), counted over load_id, a load of base_version_id. A superseded
-- finding keeps no examples, so no quote of a document outlives what the
-- audits find; the ontology's deletion deletes its rows.
CREATE TABLE foundry_heal_findings (
  ontology_id          CHAR(26)    NOT NULL,
  fingerprint          CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind                 VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status               VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  base_version_id      CHAR(26)    NOT NULL,
  load_id              CHAR(26)    NOT NULL,
  finding              JSON        NOT NULL,
  support              BIGINT      NOT NULL,
  sources              BIGINT      NOT NULL,
  support_at_dismissal BIGINT      NULL,
  dismissed_by         CHAR(26)    NULL,
  PRIMARY KEY (ontology_id, fingerprint),
  -- The list reads an ontology's open findings in this order.
  INDEX foundry_heal_findings_list_idx (ontology_id, status, support DESC, fingerprint),
  CONSTRAINT foundry_heal_findings_status_chk CHECK (status IN ('open', 'dismissed', 'superseded', 'drafting', 'checking', 'approved', 'failed')),
  CONSTRAINT foundry_heal_findings_counts_chk CHECK (support >= 0 AND sources >= 0),
  CONSTRAINT foundry_heal_findings_dismissal_chk CHECK (
    (dismissed_by IS NULL) = (support_at_dismissal IS NULL) AND (status <> 'dismissed' OR dismissed_by IS NOT NULL)
  )
);

-- One audit of an ontology's load by the foundry-heal-worker: pages is how
-- many Audit calls it made, cuts how many of them the agent's 60 s wall cut
-- short, rows_measured whether a complete run audited the load's rows (its
-- load's report held them), false for any other run. A run is running while a worker holds it (active_key 1, renewed
-- until lease_until), one per ontology at a time; complete once every page
-- was read whole and its findings were stored; incomplete when even a page
-- of one document was cut, the run ran out of time, or a graph job changed
-- the load under it, which stores nothing, so a later run audits the load
-- again; failed when it stopped otherwise, its worker's lease ran out, or
-- storing its findings failed partway. An ontology keeps its newest runs
-- only (FoundryHealRunsKept).
CREATE TABLE foundry_heal_runs (
  id          CHAR(26)    NOT NULL PRIMARY KEY,
  ontology_id CHAR(26)    NOT NULL,
  load_id     CHAR(26)    NOT NULL,
  status      VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  pages       INT         NOT NULL DEFAULT 0,
  cuts        INT         NOT NULL DEFAULT 0,
  rows_measured BOOLEAN   NOT NULL DEFAULT FALSE,
  active_key  TINYINT     NULL,
  lease_until DATETIME(6) NULL,
  created_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  finished_at DATETIME(6) NULL,
  UNIQUE KEY foundry_heal_runs_active_uq (ontology_id, active_key),
  -- A load's newest run, and an ontology's.
  INDEX foundry_heal_runs_load_idx (ontology_id, load_id, created_at, id),
  INDEX foundry_heal_runs_newest_idx (ontology_id, created_at, id),
  CONSTRAINT foundry_heal_runs_status_chk CHECK (status IN ('running', 'complete', 'incomplete', 'failed')),
  CONSTRAINT foundry_heal_runs_live_chk CHECK (
    (status = 'running') = (active_key IS NOT NULL) AND (status = 'running') = (lease_until IS NOT NULL) AND
    (status = 'running') = (finished_at IS NULL) AND (active_key IS NULL OR active_key = 1)
  ),
  CONSTRAINT foundry_heal_runs_counts_chk CHECK (pages >= 0 AND cuts >= 0 AND cuts <= pages)
);

-- +goose Down
DROP TABLE foundry_heal_runs;
DROP TABLE foundry_heal_findings;
