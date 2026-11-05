-- +goose Up
-- A connector connected by signing in at its provider reads with an account
-- its host's broker keeps (hosted: WorkOS Pipes), asking it for a token each
-- time, and seals an empty secret, as a secretless kind does. account_id is
-- the broker's id for the account; account_user_id the member it acts as,
-- whom the broker says made it; account_label who the provider says signed
-- in, for people, and account_identity its stable id there, which tells
-- whether signing in again changed who it reads as (labels repeat). account_generation counts the connector's sign-ins: a sync breaks the
-- account only while the generation it asked with is current.
-- account_broken is why it needs signing in again (member_left,
-- access_revoked, expired or account_missing, checked in code), NULL while
-- connected: nothing syncs it until someone signs it in again.
-- account_checked_at is when the broker last vended a token for it, which a
-- weekly check keeps from going stale (a check that gets none moves it on,
-- so the next comes an hour later); account_connected_at when it was last
-- signed in.
ALTER TABLE connectors
  ADD COLUMN account_id           VARCHAR(64)  NULL,
  ADD COLUMN account_user_id      CHAR(26)     NULL,
  ADD COLUMN account_label        VARCHAR(255) NULL,
  ADD COLUMN account_identity     VARCHAR(255) NULL,
  ADD COLUMN account_generation   INT          NOT NULL DEFAULT 0,
  ADD COLUMN account_broken       VARCHAR(32)  NULL,
  ADD COLUMN account_broken_at    DATETIME(6)  NULL,
  ADD COLUMN account_checked_at   DATETIME(6)  NULL,
  ADD COLUMN account_connected_at DATETIME(6)  NULL,
  ADD INDEX connectors_account_user_idx (workspace_id, account_user_id),
  ADD INDEX connectors_account_checked_idx (account_checked_at);

-- One connector's sign-in: started by a user or their agent with the
-- connector's kind, name and settings (never a credential), authorized and
-- finished in that user's browser. connector_id is the connector it signs
-- in again, or the one it made once connected. A row reads expired once
-- past expires_at, unless finishing or connected; authorizing again moves
-- it. The PKCE verifier is sealed under the connector keyring.
-- account_id is the broker's account, written as soon as the broker saves
-- it, so one a failed or abandoned finish leaves behind is purged.
CREATE TABLE foundry_connector_sign_ins (
  id                  CHAR(26)       NOT NULL PRIMARY KEY,
  workspace_id        CHAR(26)       NOT NULL,
  kind                VARCHAR(32)    NOT NULL,
  name                VARCHAR(120)   NOT NULL,
  config              JSON           NOT NULL,
  connector_id        CHAR(26)       NULL,
  status              VARCHAR(16)    NOT NULL,
  failure             VARCHAR(32)    NULL,
  discovery_problem   VARCHAR(512)   NULL,
  identity_changed    BOOL           NOT NULL DEFAULT 0,
  verifier_ciphertext VARBINARY(256) NULL,
  verifier_key_id     VARCHAR(32)    NULL,
  account_id          VARCHAR(64)    NULL,
  created_by_user_id  CHAR(26)       NOT NULL,
  client_token        VARCHAR(64)    NULL,
  created_at          DATETIME(6)    NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at          DATETIME(6)    NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  expires_at          DATETIME(6)    NOT NULL,
  UNIQUE KEY foundry_connector_sign_ins_client_token_uq (workspace_id, created_by_user_id, client_token),
  INDEX foundry_connector_sign_ins_workspace_idx (workspace_id, created_at),
  INDEX foundry_connector_sign_ins_user_idx (created_by_user_id, created_at),
  INDEX foundry_connector_sign_ins_connector_idx (connector_id),
  INDEX foundry_connector_sign_ins_expiry_idx (expires_at),
  CONSTRAINT foundry_connector_sign_ins_status_chk CHECK (status IN ('pending', 'authorizing', 'finishing', 'connected', 'declined', 'failed')),
  CONSTRAINT foundry_connector_sign_ins_failure_chk CHECK ((status = 'failed') = (failure IS NOT NULL)),
  CONSTRAINT foundry_connector_sign_ins_connected_chk CHECK (status <> 'connected' OR connector_id IS NOT NULL)
);

-- Broker accounts to delete, each written in the transaction that lets it
-- go: a connector's delete, a workspace's connectors' delete, or a sign-in
-- that ended without using its account. The sync worker deletes each
-- through the broker, as user_id, backing off from next_attempt_at.
CREATE TABLE foundry_connector_account_purges (
  id              CHAR(26)     NOT NULL PRIMARY KEY,
  workspace_id    CHAR(26)     NOT NULL,
  kind            VARCHAR(32)  NOT NULL,
  account_id      VARCHAR(64)  NOT NULL,
  user_id         CHAR(26)     NOT NULL,
  attempts        INT          NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_error      VARCHAR(512) NULL,
  created_at      DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY foundry_connector_account_purges_account_uq (workspace_id, kind, account_id),
  INDEX foundry_connector_account_purges_due_idx (next_attempt_at)
);

-- +goose Down
DROP TABLE foundry_connector_account_purges;
DROP TABLE foundry_connector_sign_ins;
ALTER TABLE connectors
  DROP INDEX connectors_account_checked_idx,
  DROP INDEX connectors_account_user_idx,
  DROP COLUMN account_connected_at,
  DROP COLUMN account_checked_at,
  DROP COLUMN account_broken_at,
  DROP COLUMN account_broken,
  DROP COLUMN account_generation,
  DROP COLUMN account_identity,
  DROP COLUMN account_label,
  DROP COLUMN account_user_id,
  DROP COLUMN account_id;
