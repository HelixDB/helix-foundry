-- +goose Up
CREATE TABLE foundry_review_decisions (
  id CHAR(26) NOT NULL PRIMARY KEY,
  ontology_id CHAR(26) NOT NULL,
  kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  type VARCHAR(120) COLLATE utf8mb4_bin NOT NULL,
  doc_id CHAR(26) NULL,
  norm_name VARCHAR(512) COLLATE utf8mb4_bin NULL,
  entity_key VARCHAR(512) COLLATE utf8mb4_bin NULL,
  prop VARCHAR(120) COLLATE utf8mb4_bin NULL,
  value JSON NULL,
  decided_by_user_id CHAR(26) NOT NULL,
  decided_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- MySQL UNIQUE permits duplicate NULLs. Hash only the identity, never
  -- the chosen value (entity_key on resolve), for actual override uniqueness.
  identity_key BINARY(32) GENERATED ALWAYS AS (UNHEX(SHA2(CAST(JSON_ARRAY(
    kind, type, doc_id, norm_name, IF(kind='prop', entity_key, NULL), prop
  ) AS CHAR), 256))) STORED,
  UNIQUE KEY foundry_review_decisions_identity_uq (ontology_id, identity_key),
  INDEX foundry_review_decisions_docs_idx (ontology_id, kind, doc_id),
  CONSTRAINT foundry_review_decisions_shape_chk CHECK (
    (kind='resolve' AND doc_id IS NOT NULL AND norm_name IS NOT NULL AND prop IS NULL AND value IS NULL) OR
    (kind='prop' AND doc_id IS NULL AND norm_name IS NULL AND entity_key IS NOT NULL AND prop IS NOT NULL AND value IS NOT NULL)
  )
);

-- +goose Down
DROP TABLE foundry_review_decisions;
