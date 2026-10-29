-- +goose Up
-- The emails Foundry sends people, one row each. The foundry-notify-worker
-- finds each in the work it tells of, then claims it and sends it once.
-- subject_id is what an email is about, unique per kind, so finding one
-- again inserts nothing: the generation, for ontology_ready (how a first
-- draft ended); the user, for sample_follow_up, so a user is followed up
-- once ever. ontology_id is the ontology_ready email's. A row is pending
-- until it is sent, skipped for good (its user, ontology or workspace is
-- gone, or it has nothing to say: last_error says why) or failed (the
-- worker's last attempt failed); claim_token holds a pending row while it
-- is sent, until claimed_until.
CREATE TABLE foundry_notifications (
  id              CHAR(26)    NOT NULL PRIMARY KEY,
  kind            VARCHAR(32) NOT NULL,
  subject_id      CHAR(26)    NOT NULL,
  workspace_id    CHAR(26)    NOT NULL,
  user_id         CHAR(26)    NOT NULL,
  ontology_id     CHAR(26)    NULL,
  status          VARCHAR(16) NOT NULL DEFAULT 'pending',
  attempts        INT         NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  claim_token     CHAR(26)    NULL,
  claimed_until   DATETIME(6) NULL,
  last_error      TEXT        NULL,
  created_at      DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  sent_at         DATETIME(6) NULL,
  UNIQUE KEY foundry_notifications_subject_uq (kind, subject_id),
  INDEX foundry_notifications_claim_idx (status, next_attempt_at),
  CONSTRAINT foundry_notifications_kind_chk CHECK (kind IN ('ontology_ready', 'sample_follow_up')),
  CONSTRAINT foundry_notifications_status_chk CHECK (status IN ('pending', 'sent', 'skipped', 'failed')),
  CONSTRAINT foundry_notifications_ontology_chk CHECK ((kind = 'ontology_ready') = (ontology_id IS NOT NULL)),
  CONSTRAINT foundry_notifications_sent_chk CHECK ((status = 'sent') = (sent_at IS NOT NULL)),
  CONSTRAINT foundry_notifications_why_chk CHECK (status NOT IN ('skipped', 'failed') OR last_error IS NOT NULL),
  CONSTRAINT foundry_notifications_claim_chk CHECK ((claim_token IS NULL) = (claimed_until IS NULL) AND
    (status = 'pending' OR claim_token IS NULL))
);

-- The worker finds its emails, every tick, among the generations that
-- finished in a window of days: this index reads only those.
ALTER TABLE foundry_ontology_generations ADD INDEX foundry_ontology_generations_finished_idx (finished_at);

-- +goose Down
ALTER TABLE foundry_ontology_generations DROP INDEX foundry_ontology_generations_finished_idx;

DROP TABLE foundry_notifications;
