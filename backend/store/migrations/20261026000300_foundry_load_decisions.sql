-- +goose Up
-- Independent of live decisions: pruning must not change an existing load.
CREATE TABLE foundry_load_decisions (
  load_id CHAR(26) NOT NULL,
  id CHAR(26) NOT NULL,
  ontology_id CHAR(26) NOT NULL,
  kind VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  type VARCHAR(120) COLLATE utf8mb4_bin NOT NULL,
  doc_id CHAR(26) NULL,
  norm_name VARCHAR(512) COLLATE utf8mb4_bin NULL,
  entity_key VARCHAR(512) COLLATE utf8mb4_bin NULL,
  prop VARCHAR(120) COLLATE utf8mb4_bin NULL,
  value JSON NULL,
  decided_by_user_id CHAR(26) NOT NULL,
  decided_at DATETIME(6) NOT NULL,
  PRIMARY KEY (load_id, id)
);

-- +goose Down
DROP TABLE foundry_load_decisions;
