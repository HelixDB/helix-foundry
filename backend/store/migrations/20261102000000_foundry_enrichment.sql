-- +goose Up
-- People enrichment: a load whose
-- version holds an [enrichments] item looks the version's people up on
-- context.dev and applies the matches with its tables. New tables only.
-- None holds personal data: emails, LinkedIn URLs, names and profiles live
-- only in S3, under ws/<workspace>/enrichment/<ontology>/loads/<load>/, and
-- these rows hold their keys and counts.

-- An owner's consent and monthly credit cap, append-only: the newest row of
-- a workspace is its setting, and every change keeps the notice it was given
-- under.
CREATE TABLE foundry_enrichment_consents (
  id                 CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id       CHAR(26)    NOT NULL,
  enabled            BOOLEAN     NOT NULL,
  monthly_credit_cap BIGINT      NOT NULL,
  notice_version     VARCHAR(32) NOT NULL,
  user_id            CHAR(26)    NOT NULL,
  created_at         DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  INDEX foundry_enrichment_consents_workspace_idx (workspace_id, created_at, id),
  CONSTRAINT foundry_enrichment_consents_cap_chk CHECK (monthly_credit_cap >= 0)
);

-- A load's enrichment step: at most one per load (its id is the load's).
-- The graph worker inserts it preparing, then queues it for the enrichment
-- worker or ends it; the enrichment worker claims a queued row before
-- claim_by, runs its batches (batch_cursor is the next one, and
-- inflight_credits what the one in flight may charge) and publishes.
-- applied_key is the file the load's Ingest reads, written once: '' is the
-- zero-byte file, and content_sha256 the SHA-256 of the applied object's
-- bytes. stop_requested is the user who asked to stop; stopped_by why a
-- partial row stopped.
CREATE TABLE foundry_load_enrichments (
  load_id          CHAR(26)      NOT NULL PRIMARY KEY,
  workspace_id     CHAR(26)      NOT NULL,
  ontology_id      CHAR(26)      NOT NULL,
  job_id           CHAR(26)      NOT NULL,
  status           VARCHAR(16)   NOT NULL,
  stopped_by       VARCHAR(16)   NULL,
  claim_by         DATETIME(6)   NULL,
  deadline         DATETIME(6)   NULL,
  lease_owner      VARCHAR(64)   NULL,
  lease_until      DATETIME(6)   NULL,
  batch_cursor     INT           NOT NULL DEFAULT 0,
  reserved_credits BIGINT        NOT NULL DEFAULT 0,
  inflight_credits BIGINT        NOT NULL DEFAULT 0,
  counts           JSON          NULL,
  plan_key         VARCHAR(1024) NULL,
  carried_key      VARCHAR(1024) NULL,
  profiles_key     VARCHAR(1024) NULL,
  results_key      VARCHAR(1024) NULL,
  applied_key      VARCHAR(1024) NULL,
  content_sha256   CHAR(64)      NULL,
  stop_requested   CHAR(26)      NULL,
  cancel_requested BOOLEAN       NOT NULL DEFAULT FALSE,
  purged_at        DATETIME(6)   NULL,
  created_at       DATETIME(6)   NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  claimed_at       DATETIME(6)   NULL,
  ended_at         DATETIME(6)   NULL,
  published_at     DATETIME(6)   NULL,
  calls_deleted_at DATETIME(6)   NULL,
  INDEX foundry_load_enrichments_ontology_idx (ontology_id, created_at),
  INDEX foundry_load_enrichments_claim_idx (status, claim_by),
  CONSTRAINT foundry_load_enrichments_status_chk CHECK (status IN ('preparing', 'queued', 'running', 'done', 'partial', 'off', 'failed', 'cancelled')),
  CONSTRAINT foundry_load_enrichments_stopped_chk CHECK ((status = 'partial') = (stopped_by IS NOT NULL)),
  CONSTRAINT foundry_load_enrichments_stopped_by_chk CHECK (stopped_by IS NULL OR stopped_by IN ('cap', 'provider_credits', 'provider', 'time', 'busy', 'stopped', 'pro', 'flag')),
  CONSTRAINT foundry_load_enrichments_ended_chk CHECK ((status IN ('preparing', 'queued', 'running')) = (ended_at IS NULL)),
  CONSTRAINT foundry_load_enrichments_queued_chk CHECK (status <> 'queued' OR claim_by IS NOT NULL),
  CONSTRAINT foundry_load_enrichments_running_chk CHECK (status <> 'running' OR (deadline IS NOT NULL AND lease_owner IS NOT NULL AND lease_until IS NOT NULL)),
  CONSTRAINT foundry_load_enrichments_applied_chk CHECK ((applied_key IS NULL) = (content_sha256 IS NULL)),
  CONSTRAINT foundry_load_enrichments_credits_chk CHECK (batch_cursor >= 0 AND inflight_credits >= 0 AND inflight_credits <= reserved_credits)
);

-- What each batch charged, once: the primary key refuses a second charge of
-- a batch. A month's credits are those charged in it.
CREATE TABLE foundry_enrichment_charges (
  load_id           CHAR(26)    NOT NULL,
  batch             INT         NOT NULL,
  workspace_id      CHAR(26)    NOT NULL,
  credits           BIGINT      NOT NULL,
  uncertain_credits BIGINT      NOT NULL,
  charged_at        DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (load_id, batch),
  INDEX foundry_enrichment_charges_workspace_idx (workspace_id, charged_at),
  CONSTRAINT foundry_enrichment_charges_credits_chk CHECK (batch >= 0 AND credits >= 0 AND uncertain_credits >= 0)
);

-- A provider's halt after it refused the key or the credits (402, 401, 403):
-- no lookups run before halted_until. Its row is also the lock that
-- serializes claims and credit reservations.
CREATE TABLE foundry_enrichment_provider (
  provider     VARCHAR(32) NOT NULL PRIMARY KEY,
  halted_until DATETIME(6) NULL,
  halt_code    VARCHAR(32) NULL
);

-- An ontology whose current load applied profiles when consent was turned
-- off: the sweeper queues a clean load (last_load_id) once no job is live,
-- retries a failed one daily up to three attempts, and sets done_at once a
-- load that applied no profiles is current. requested_at is when the latest
-- clean load was asked for.
CREATE TABLE foundry_enrichment_purges (
  ontology_id  CHAR(26)    NOT NULL PRIMARY KEY,
  workspace_id CHAR(26)    NOT NULL,
  requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  attempts     INT         NOT NULL DEFAULT 0,
  last_load_id CHAR(26)    NULL,
  done_at      DATETIME(6) NULL
);

-- +goose Down
DROP TABLE foundry_enrichment_purges;
DROP TABLE foundry_enrichment_provider;
DROP TABLE foundry_enrichment_charges;
DROP TABLE foundry_load_enrichments;
DROP TABLE foundry_enrichment_consents;
