-- +goose Up
-- A self-healing suggestion's "Add this link" adds the link to a new version
-- and moves the current load to it in place, when the version only adds
-- (contract 4.32.0's `fold`): the load stops being pinned to the version it
-- was created with, and foundry_loads.version_id follows it, with the running
-- job's foundry_load_jobs.version_id, in one transaction. A later layer then
-- fills the documents behind the suggestion's reviews with the new link only.
--
-- foundry_heal_fills is one row per load and suggestion (fingerprint) a
-- person pressed it for: the move (from_version_id to to_version_id) and,
-- later, the fill. state is 'moving' (a resend job owes the move: a
-- foundry_load_resends row of reason 'move' and document ''), 'needs_load'
-- (the version adds more than a link; why says what, and nothing moved),
-- 'moved' (links_added are the links the load gained), then 'applying',
-- 'filled' or 'stopped' (stop_reason) for the fill. active_key is the load's
-- id while a fill applies, so a load fills one suggestion at a time. The
-- counters and spend are the fill's; a move spends nothing.
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the table stays. The rollback floor (the binary
-- that brought this migration) reads it to put back a load whose marker moved
-- without its row (reconcileFoundryLoadMarker), and never ends, lists or sends
-- a resend row of a reason it doesn't know ('move', 'link'). So nothing must
-- be deleted before going back. The Down is for a local database.
CREATE TABLE foundry_heal_fills (
  id              CHAR(26)    NOT NULL,
  ontology_id     CHAR(26)    NOT NULL,
  load_id         CHAR(26)    NOT NULL,
  fingerprint     CHAR(64)    CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  from_version_id CHAR(26)    NOT NULL,
  to_version_id   CHAR(26)    NOT NULL,
  state           VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  why             JSON        NULL,
  links_added     JSON        NULL,
  stop_reason     VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  active_key      CHAR(26)    NULL,
  docs_owed       BIGINT      NOT NULL DEFAULT 0,
  docs_filled     BIGINT      NOT NULL DEFAULT 0,
  docs_nothing    BIGINT      NOT NULL DEFAULT 0,
  docs_refused    BIGINT      NOT NULL DEFAULT 0,
  docs_native     BIGINT      NOT NULL DEFAULT 0,
  docs_left       BIGINT      NOT NULL DEFAULT 0,
  reviews_taken   BIGINT      NOT NULL DEFAULT 0,
  facts_added     BIGINT      NOT NULL DEFAULT 0,
  spent_usd       DECIMAL(12,6) NOT NULL DEFAULT 0,
  cap_usd         DECIMAL(12,6) NOT NULL DEFAULT 0,
  paused_until    DATETIME(6) NULL,
  failures        INT         NOT NULL DEFAULT 0,
  last_error      VARCHAR(512) NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  UNIQUE KEY foundry_heal_fills_load_uq (load_id, fingerprint),
  UNIQUE KEY foundry_heal_fills_active_uq (active_key),
  INDEX foundry_heal_fills_ontology_idx (ontology_id, state),
  CONSTRAINT foundry_heal_fills_state_chk CHECK (state IN ('moving', 'needs_load', 'moved', 'applying', 'filled', 'stopped')),
  CONSTRAINT foundry_heal_fills_stop_chk CHECK ((stop_reason IS NULL) = (state <> 'stopped')),
  CONSTRAINT foundry_heal_fills_active_chk CHECK ((active_key IS NOT NULL) = (state = 'applying') AND (active_key IS NULL OR active_key = load_id)),
  CONSTRAINT foundry_heal_fills_why_chk CHECK (state <> 'needs_load' OR why IS NOT NULL),
  CONSTRAINT foundry_heal_fills_counts_chk CHECK (docs_owed >= 0 AND docs_filled >= 0 AND docs_nothing >= 0 AND docs_refused >= 0
    AND docs_native >= 0 AND docs_left >= 0 AND reviews_taken >= 0 AND facts_added >= 0 AND spent_usd >= 0 AND cap_usd >= 0)
);

-- +goose Down
DROP TABLE foundry_heal_fills;
