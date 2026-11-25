-- +goose Up
-- Company research for a new ontology's goal (ResearchFoundryCompany and
-- DraftFoundryGoal): one row per model call, made before the call, under the
-- caller's hourly cap and the workspace's monthly budget, and given what the
-- call spent once it ends, whatever came of it. A research row holds the
-- research (result), which a goal's draft reads; a goal row holds none: it
-- records the spend. The table is new: nothing existing changes.
CREATE TABLE foundry_company_research (
  id           CHAR(26)    NOT NULL,
  workspace_id CHAR(26)    NOT NULL,
  user_id      VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  kind         VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- The registrable domain read; a goal's, its research's.
  domain       VARCHAR(253) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  -- A research's, as package companyresearch kept it; NULL until it ends,
  -- and for a goal or a call that failed.
  result       JSON        NULL,
  usd_micros   BIGINT      NOT NULL DEFAULT 0,
  created_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (id),
  INDEX foundry_company_research_user_idx (user_id, created_at),
  INDEX foundry_company_research_workspace_idx (workspace_id, created_at),
  CONSTRAINT foundry_company_research_kind_chk CHECK (kind IN ('research', 'goal')),
  CONSTRAINT foundry_company_research_result_chk CHECK (kind = 'research' OR result IS NULL),
  CONSTRAINT foundry_company_research_spend_chk CHECK (usd_micros >= 0)
);

-- +goose Down
-- A Vitess-based MySQL host (PlanetScale) names a CHECK by its first 38
-- characters and a hash, so the table's are dropped by the prefix they
-- share.
SET @drop_company_research_chks = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_company_research ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_company_research'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_company_research%'
);
PREPARE drop_company_research_chks FROM @drop_company_research_chks;
EXECUTE drop_company_research_chks;
DEALLOCATE PREPARE drop_company_research_chks;
DROP TABLE foundry_company_research;
