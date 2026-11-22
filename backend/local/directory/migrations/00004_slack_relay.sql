-- +goose Up
-- The install's link to Helix, which it opts in to: Slack reaches it through
-- Helix's relay once linked. Its hx_rl_ token is sealed with the install's
-- connector key (context "foundry-relay"). One row, id 1, while linked.
CREATE TABLE foundry_local_relay (
  id              TINYINT        NOT NULL PRIMARY KEY,
  hosted_origin   VARCHAR(255)   NOT NULL,
  link_id         CHAR(26)       NOT NULL,
  token_sealed    VARBINARY(512) NOT NULL,
  key_id          VARCHAR(64)    NOT NULL,
  -- The Helix account that authorized the link, as Settings shows it.
  account_email   VARCHAR(320)   NOT NULL,
  -- Whether the owner's Slack account may approve actions through Slack
  -- (off: the relay only reads and asks).
  slack_approvals BOOL           NOT NULL DEFAULT TRUE,
  linked_at       DATETIME(6)    NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- The last claim Helix answered.
  last_ok_at      DATETIME(6)    NULL,
  CONSTRAINT foundry_local_relay_one_chk CHECK (id = 1)
);

-- A link the owner started: its state's SHA-256 and its PKCE verifier,
-- sealed, until Helix hands the code back, once, before it expires.
CREATE TABLE foundry_local_relay_pending (
  state_hash      CHAR(64)       NOT NULL PRIMARY KEY,
  verifier_sealed VARBINARY(512) NOT NULL,
  key_id          VARCHAR(64)    NOT NULL,
  expires_at      DATETIME(6)    NOT NULL,
  used_at         DATETIME(6)    NULL,
  KEY foundry_local_relay_pending_expiry (expires_at)
);

-- +goose Down
DROP TABLE foundry_local_relay_pending;
DROP TABLE foundry_local_relay;
