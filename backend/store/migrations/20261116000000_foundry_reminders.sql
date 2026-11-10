-- +goose Up
-- The reminders a person asks for on Foundry's landing page, from a phone, to
-- try Foundry on a computer, one row per address (email, trimmed and
-- lowercased, compared byte for byte). The API takes them from the
-- foundry-events door, for the site; the foundry-notify-worker claims each
-- one due and sends it on its own subscription topic, foundry-reminder.
--
-- A row is scheduled while a reminder is due at next_send_at, and is done
-- once every reminder of its schedule went (sent counts them), stopped once
-- its person signed in (stop_reason), or failed once the worker gave up on
-- one (last_error says why). Asking again while it is scheduled changes
-- nothing; once it ended, a new schedule starts in place (requested_at), no
-- sooner than foundry.reminders.min_gap_days after last_sent_at, which
-- outlives each schedule. time_zone is the IANA zone the schedule keeps to,
-- and consent the words the person agreed to (remind-v1). claim_token holds
-- a scheduled row while a reminder is sent, until claimed_until; attempts
-- counts the claims of the reminder due.
CREATE TABLE foundry_reminders (
  id            CHAR(26)     NOT NULL PRIMARY KEY,
  email         VARCHAR(254) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  time_zone     VARCHAR(64)  NOT NULL,
  consent       VARCHAR(32)  NOT NULL,
  status        VARCHAR(16)  NOT NULL DEFAULT 'scheduled',
  sent          INT          NOT NULL DEFAULT 0,
  next_send_at  DATETIME(6)  NULL,
  attempts      INT          NOT NULL DEFAULT 0,
  claim_token   CHAR(26)     NULL,
  claimed_until DATETIME(6)  NULL,
  last_error    TEXT         NULL,
  requested_at  DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  created_at    DATETIME(6)  NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  last_sent_at  DATETIME(6)  NULL,
  stopped_at    DATETIME(6)  NULL,
  stop_reason   VARCHAR(16)  NULL,
  UNIQUE KEY foundry_reminders_email_uq (email),
  INDEX foundry_reminders_claim_idx (status, next_send_at),
  INDEX foundry_reminders_requested_idx (requested_at),
  CONSTRAINT foundry_reminders_status_chk CHECK (status IN ('scheduled', 'done', 'stopped', 'failed')),
  CONSTRAINT foundry_reminders_next_chk CHECK ((status = 'scheduled') = (next_send_at IS NOT NULL)),
  CONSTRAINT foundry_reminders_stopped_chk CHECK ((status = 'stopped') = (stopped_at IS NOT NULL) AND
    (stopped_at IS NULL) = (stop_reason IS NULL) AND (stop_reason IS NULL OR stop_reason IN ('signed_in'))),
  CONSTRAINT foundry_reminders_why_chk CHECK (status <> 'failed' OR last_error IS NOT NULL),
  CONSTRAINT foundry_reminders_sent_chk CHECK (sent >= 0 AND attempts >= 0 AND (sent = 0 OR last_sent_at IS NOT NULL)),
  CONSTRAINT foundry_reminders_claim_chk CHECK ((claim_token IS NULL) = (claimed_until IS NULL) AND
    (status = 'scheduled' OR claim_token IS NULL))
);

-- +goose Down
DROP TABLE foundry_reminders;
