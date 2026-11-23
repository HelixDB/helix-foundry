-- +goose Up
-- Review advice: Founder's model's suggestion for a review card the rules
-- leave undecided (a name between candidates, a value documents split on),
-- written by the graph worker's advisor (workers/foundry-graph-worker
-- review_advice.go, backend/reviewadvice) and shown by the walk only while
-- the card still offers what the model was shown. Advice is paid for, so it
-- is kept until its load is no longer live; it is never batched or measured.
--
-- foundry_review_advice_loads is one row per load owed advice: owed again in
-- place (owed_at) whenever it becomes current or its index stores cards, due
-- at due_at (NULL: nothing to do until it is owed again), claimed by a worker
-- (claimed_by) for a lease that due_at ends, its back-offs since its last
-- store (attempts, which set the next wait), the claims in a row its
-- model's provider failed (failures: the third stops it, failed), and why
-- its last claim stopped (stopped).
--
-- foundry_review_advice is one row per card of a load: what came of it
-- (outcome: advised, none, rules or skipped), the choices the model was
-- offered and the one it picked, its rationale (plain text, sanitized) and
-- the quotes it cites, and the model and prompt that wrote it.
--
-- foundry_review_advice_spend is each workspace's monthly spend on advice,
-- apart from Founder's, questions' and loads' budgets.
CREATE TABLE foundry_review_advice_loads (
  load_id CHAR(26) NOT NULL PRIMARY KEY,
  workspace_id CHAR(26) NOT NULL,
  ontology_id CHAR(26) NOT NULL,
  owed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  due_at DATETIME(6) NULL,
  claimed_by VARCHAR(64) NULL,
  attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  failures SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  stopped VARCHAR(12) CHARACTER SET ascii COLLATE ascii_bin NULL,
  INDEX foundry_review_advice_loads_due_idx (due_at),
  CONSTRAINT foundry_review_advice_loads_chk CHECK (stopped IN ('done', 'limit', 'budget', 'plan', 'no_model', 'failed'))
);

CREATE TABLE foundry_review_advice (
  load_id CHAR(26) NOT NULL,
  card_id BINARY(32) NOT NULL,
  outcome VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  offered JSON NULL,
  choice JSON NULL,
  rationale VARCHAR(300) NULL,
  cites JSON NULL,
  model VARCHAR(255) NULL,
  prompt_version VARCHAR(32) CHARACTER SET ascii NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (load_id, card_id),
  CONSTRAINT foundry_review_advice_shape_chk CHECK (outcome IN ('advised', 'none', 'rules', 'skipped')
    AND (outcome = 'advised') = (choice IS NOT NULL)
    AND (choice IS NULL) = (rationale IS NULL)
    AND (choice IS NULL) = (cites IS NULL)
    AND (outcome IN ('advised', 'none')) = (model IS NOT NULL)
    AND (model IS NULL) = (prompt_version IS NULL)
    AND (model IS NULL) = (offered IS NULL))
);

CREATE TABLE foundry_review_advice_spend (
  workspace_id CHAR(26) NOT NULL,
  month DATE NOT NULL,
  usd_micros BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (workspace_id, month),
  CONSTRAINT foundry_review_advice_spend_chk CHECK (usd_micros >= 0 AND DAY(month) = 1)
);

-- +goose Down
DROP TABLE foundry_review_advice_spend;
DROP TABLE foundry_review_advice;
DROP TABLE foundry_review_advice_loads;
