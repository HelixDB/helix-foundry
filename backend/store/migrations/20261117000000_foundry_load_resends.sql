-- +goose Up
-- A person's approval of a held claim applies to the current load at once: the
-- decision joins that load's list (foundry_load_decisions, which so far only a
-- load's creation wrote), and a resend job sends the claim's document again
-- with its saved extraction, so the engine replays it for nothing and writes
-- what the approval releases. Nothing here changes or deletes a row.
--
-- foundry_load_resends is the documents of a load a resend job owes the
-- engine, each for a reason. 'approval' is the only one so far: a decision of
-- the document waits to be applied, and its resend spends nothing. requests
-- counts how often the row was owed again, so a job ends only the request it
-- read: an approval that lands while its document's resend runs stays owed.
-- reason has no CHECK: the backend's type holds it (model.FoundryResendReason),
-- and a later reason needs no migration.
--
-- applied_status is what became of a release decision in its load, as the
-- engine said when a job sent its document: NULL until a job reports it,
-- 'pending' (settled against a review of this load, and a resend owes it),
-- 'carried' (saved before this load became current and not in its list until
-- then; a resend owes it), 'written', 'refused' (applied_reason says why, in
-- the engine's word or one of the backend's) or 'missed' (this load holds no
-- review at its key and place).
--
-- Rollback. A deploy request carries schema only and a rollback restores the
-- binary, not the schema, so the table and the columns stay, and an older
-- binary neither reads nor writes them: a decision saved meanwhile applies
-- from the next load again, as before, and one left 'pending' stays in its
-- load's list, which that binary sends with its document as any other. A
-- queued or running job of kind 'resend' is claimed by an older binary as an
-- extract of its load, which sends only the documents the load waits for and
-- ends: the rows it owed stay owed, and nothing reads them. So nothing must be
-- deleted before going back. The Down is for a local database: it deletes the
-- resend jobs, which the old CHECK has no arm for, and then the rest.
--
-- A Vitess-based MySQL host may rename CHECK constraints, so the old one is
-- dropped by prefix.
SET @drop_load_jobs_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_jobs ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_jobs'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_jobs_kind_chk%'
);
PREPARE drop_load_jobs_kind_chk FROM @drop_load_jobs_kind_chk;
EXECUTE drop_load_jobs_kind_chk;
DEALLOCATE PREPARE drop_load_jobs_kind_chk;

ALTER TABLE foundry_load_jobs
  ADD CONSTRAINT foundry_load_jobs_kind_chk CHECK (kind IN ('load', 'extract', 'delete', 'apply', 'resend'));

CREATE TABLE foundry_load_resends (
  load_id      CHAR(26)    NOT NULL,
  document_id  CHAR(26)    NOT NULL,
  reason       VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  status       VARCHAR(8)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'owed',
  requests     INT         NOT NULL DEFAULT 1,
  requested_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  -- When a job ended the request, and which; NULL while owed.
  ended_at     DATETIME(6) NULL,
  job_id       CHAR(26)    NULL,
  PRIMARY KEY (load_id, document_id, reason),
  INDEX foundry_load_resends_owed_idx (load_id, status),
  CONSTRAINT foundry_load_resends_status_chk CHECK (status IN ('owed', 'done')),
  CONSTRAINT foundry_load_resends_ended_chk CHECK ((status = 'done') = (ended_at IS NOT NULL))
);

-- The index finds a document's decisions in a load: a job reads them for the
-- documents it sends, and an approval carried over to a load is matched to
-- the claims its list already has a word on.
ALTER TABLE foundry_load_decisions
  ADD COLUMN applied_status VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NULL,
  ADD COLUMN applied_reason VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL,
  ADD COLUMN applied_at DATETIME(6) NULL,
  ADD INDEX foundry_load_decisions_docs_idx (load_id, kind, doc_id);

-- +goose Down
ALTER TABLE foundry_load_decisions
  DROP INDEX foundry_load_decisions_docs_idx,
  DROP COLUMN applied_status,
  DROP COLUMN applied_reason,
  DROP COLUMN applied_at;

DROP TABLE foundry_load_resends;

-- A resend job fails the old check: delete those rows first.
DELETE FROM foundry_load_jobs WHERE kind = 'resend';

SET @drop_load_jobs_kind_chk = (
  SELECT COALESCE(
    CONCAT(
      'ALTER TABLE foundry_load_jobs ',
      GROUP_CONCAT(CONCAT('DROP CHECK `', REPLACE(constraint_name, '`', '``'), '`') SEPARATOR ', ')
    ),
    'SELECT 1'
  )
  FROM information_schema.table_constraints
  WHERE constraint_schema = DATABASE()
    AND table_name = 'foundry_load_jobs'
    AND constraint_type = 'CHECK'
    AND constraint_name LIKE 'foundry_load_jobs_kind_chk%'
);
PREPARE drop_load_jobs_kind_chk FROM @drop_load_jobs_kind_chk;
EXECUTE drop_load_jobs_kind_chk;
DEALLOCATE PREPARE drop_load_jobs_kind_chk;

ALTER TABLE foundry_load_jobs
  ADD CONSTRAINT foundry_load_jobs_kind_chk CHECK (kind IN ('load', 'extract', 'delete', 'apply'));
