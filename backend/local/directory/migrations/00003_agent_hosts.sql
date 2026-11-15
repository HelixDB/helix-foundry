-- +goose Up
-- The helpers paired with the install: processes on the owner's machine,
-- outside Docker, that run the Assistant's turns on the Claude Code or Codex
-- CLI the owner signed in to. Foundry keeps only each token's SHA-256.

-- A helper. Its hxh_ token claims turns and reports on them, nothing else.
CREATE TABLE foundry_local_agent_hosts (
  id           CHAR(26)                    NOT NULL PRIMARY KEY,
  name         VARCHAR(128)                NOT NULL,
  token_hash   CHAR(64)                    NOT NULL,
  -- What it offers, and what their CLIs are signed in to, as of its last
  -- call: [{runtime, state, subscription}] (agent.HostAccount).
  runtimes     SET('claude_code', 'codex') NOT NULL DEFAULT '',
  accounts     JSON                        NULL,
  version      VARCHAR(64)                 NOT NULL DEFAULT '',
  platform     VARCHAR(64)                 NOT NULL DEFAULT '',
  created_at   DATETIME(6)                 NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- Its last call: NULL until its first after pairing, which starts nothing.
  last_seen_at DATETIME(6)                 NULL,
  revoked_at   DATETIME(6)                 NULL,
  UNIQUE KEY foundry_local_agent_hosts_token_uq (token_hash)
);

-- A one-time code the owner made in Foundry, which pairs one helper within
-- ten minutes.
CREATE TABLE foundry_local_agent_pairings (
  code_hash  CHAR(64)    NOT NULL PRIMARY KEY,
  expires_at DATETIME(6) NOT NULL,
  used_at    DATETIME(6) NULL,
  KEY foundry_local_agent_pairings_expiry (expires_at)
);

-- An hxr_ token: what one claimed turn's CLI calls Foundry's tools with, in
-- that turn's workspace alone, while its claim's lease lives, a bounded
-- number of times.
CREATE TABLE foundry_local_agent_turn_tokens (
  token_hash   CHAR(64)    NOT NULL PRIMARY KEY,
  host_id      CHAR(26)    NOT NULL,
  turn_id      CHAR(26)    NOT NULL,
  workspace_id CHAR(26)    NOT NULL,
  -- The claim's lease owner, under which the turn's writes are made.
  lease_owner  VARCHAR(64) NOT NULL,
  -- When it stops working: the claim's lease, renewed with it.
  expires_at   DATETIME(6) NOT NULL,
  -- When the turn's wall time runs out, whatever the lease.
  deadline     DATETIME(6) NOT NULL,
  calls        INT         NOT NULL DEFAULT 0,
  asks         INT         NOT NULL DEFAULT 0,
  KEY foundry_local_agent_turn_tokens_turn (turn_id),
  KEY foundry_local_agent_turn_tokens_host (host_id, lease_owner),
  KEY foundry_local_agent_turn_tokens_expiry (expires_at),
  CONSTRAINT foundry_local_agent_turn_tokens_calls_chk CHECK (calls >= 0 AND asks >= 0 AND asks <= calls)
);

-- Where a workspace's new Assistant threads run; a workspace without a row
-- runs them on Foundry's own worker.
CREATE TABLE foundry_local_agent_settings (
  workspace_id CHAR(26)    NOT NULL PRIMARY KEY,
  runtime      VARCHAR(16) NOT NULL,
  updated_at   DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT foundry_local_agent_settings_runtime_chk CHECK (runtime IN ('api', 'claude_code', 'codex'))
);

-- +goose Down
DROP TABLE foundry_local_agent_settings;
DROP TABLE foundry_local_agent_turn_tokens;
DROP TABLE foundry_local_agent_pairings;
DROP TABLE foundry_local_agent_hosts;
