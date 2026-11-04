-- +goose Up
CREATE TABLE foundry_local_mcp_confirmations (
  id CHAR(26) NOT NULL PRIMARY KEY,
  token_hash CHAR(64) NOT NULL,
  owner_id CHAR(26) NOT NULL,
  operation VARCHAR(128) NOT NULL,
  target VARCHAR(255) NOT NULL,
  audience VARCHAR(255) NOT NULL,
  payload_hash CHAR(64) NOT NULL,
  expires_at DATETIME(6) NOT NULL,
  consumed_at DATETIME(6) NULL,
  KEY ix_local_confirmations_expiry (expires_at)
);

-- +goose Down
DROP TABLE foundry_local_mcp_confirmations;
