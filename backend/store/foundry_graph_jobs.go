package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryGraphJobColumns = `id, workspace_id, ontology_id, version_id, load_id, kind, status, phase, client_token, lease_owner,
       lease_until, attempts, next_attempt_at, error, report, chunks_done, documents_done, documents_total, usd_micros,
       input_tokens, output_tokens, requested_by_user_id, created_at, started_at, finished_at, sources`

// insertFoundryGraphJob inserts job as queued. The caller holds the
// ontology's lock and has checked no job of it is live. ErrConflict means
// the ontology has a live job, or one with the same client token.
func (s *mysqlDB) insertFoundryGraphJob(ctx context.Context, job *model.FoundryGraphJob) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_jobs
  (id, workspace_id, ontology_id, version_id, load_id, kind, status, phase, active_key, client_token, requested_by_user_id)
VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, 1, ?, ?)`,
		job.ID, job.WorkspaceID, job.OntologyID, job.VersionID, job.LoadID, string(job.Kind), string(job.Phase), job.ClientToken,
		job.RequestedByUserID)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// liveFoundryGraphJob returns the ontology's queued or running job, locked,
// or nil when none is. The caller holds the ontology's lock.
func (s *mysqlDB) liveFoundryGraphJob(ctx context.Context, ontologyID string) (*model.FoundryGraphJob, error) {
	job, err := scanFoundryGraphJob(s.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs WHERE ontology_id=? AND active_key=1 FOR UPDATE`, ontologyID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return job, err
}

// cancelLiveFoundryGraphJob ends the ontology's live job, if it has one, as
// cancelled, saying why. A worker running it loses its lease: its next
// renewal stops the attempt, which then records nothing. The caller holds
// the ontology's lock.
func (s *mysqlDB) cancelLiveFoundryGraphJob(ctx context.Context, ontologyID, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_jobs
SET status='cancelled', active_key=NULL, lease_owner=NULL, lease_until=NULL, error=?, finished_at=NOW(6)
WHERE ontology_id=? AND active_key=1`, why, ontologyID)
	return err
}

func (s *mysqlDB) GetActiveFoundryGraphJob(ctx context.Context, ontologyID string) (*model.FoundryGraphJob, error) {
	return scanFoundryGraphJob(s.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs WHERE ontology_id=? AND active_key=1`, ontologyID))
}

// GetLatestFoundryGraphJob returns the ontology's newest job of a kind.
func (s *mysqlDB) GetLatestFoundryGraphJob(ctx context.Context, ontologyID string, kind model.FoundryGraphJobKind) (*model.FoundryGraphJob, error) {
	return scanFoundryGraphJob(s.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs WHERE ontology_id=? AND kind=? ORDER BY created_at DESC, id DESC LIMIT 1`, ontologyID, string(kind)))
}

func (s *mysqlDB) GetFoundryGraphJob(ctx context.Context, ontologyID, id string) (*model.FoundryGraphJob, error) {
	return scanFoundryGraphJob(s.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs WHERE id=? AND ontology_id=?`, id, ontologyID))
}

func (s *mysqlDB) GetFoundryGraphJobByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryGraphJob, error) {
	return scanFoundryGraphJob(s.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs WHERE ontology_id=? AND client_token=?`, ontologyID, clientToken))
}

// SumFoundryGraphJobSpend adds up what the workspace's jobs, across its
// ontologies, created since since spent, including what live attempts
// recorded so far.
func (s *mysqlDB) SumFoundryGraphJobSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error) {
	var micros int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_load_jobs
WHERE workspace_id=? AND created_at >= ?`, workspaceID, since.UTC()).Scan(&micros)
	return micros, err
}

// ClaimFoundryGraphJob leases the oldest due job to workerID: a queued one
// whose retry time has come, or a running one whose lease expired. Nil means
// none is due.
func (s *mysqlDB) ClaimFoundryGraphJob(ctx context.Context, workerID string, lease time.Duration) (_ *model.FoundryGraphJob, err error) {
	if s.root == nil {
		return nil, fmt.Errorf("foundry graph job claim requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin foundry graph job claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// A user's job goes before a refresh's, an apply being one, so a refresh
	// never delays a load someone waits for by more than the job running. The
	// subquery takes no lock: a locking clause locks only the outer query's rows.
	job, err := scanFoundryGraphJob(tx.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+`
FROM foundry_load_jobs j
WHERE (status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6))
ORDER BY j.kind = 'apply' OR (SELECT l.origin = 'refresh' FROM foundry_loads l WHERE l.id = j.load_id), next_attempt_at, id
LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if err == ErrNotFound {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select foundry graph job claim: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	if _, err = tx.ExecContext(ctx, `
UPDATE foundry_load_jobs
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, started_at=COALESCE(started_at, NOW(6))
WHERE id=?`, workerID, leaseUntil, job.ID); err != nil {
		return nil, fmt.Errorf("claim foundry graph job: %w", err)
	}
	var origin, refreshes sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT origin, refreshes_load_id FROM foundry_loads WHERE id=?`, job.LoadID).
		Scan(&origin, &refreshes); err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("read foundry load %s origin: %w", job.LoadID, err)
	}
	job.Origin = model.FoundryLoadOrigin(origin.String)
	if refreshes.Valid {
		job.RefreshesLoadID = &refreshes.String
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit foundry graph job claim: %w", err)
	}
	job.Status, job.LeaseOwner, job.LeaseUntil = model.FoundryGraphJobRunning, &workerID, &leaseUntil
	job.Attempts++
	if job.StartedAt == nil {
		job.StartedAt = &now
	}
	return job, nil
}

const ownedFoundryGraphJob = ` WHERE id=? AND status='running' AND lease_owner=?`

// SetFoundryGraphJobSources pins the snapshots a load job reads, while
// workerID holds its lease; ErrNotFound means it lost it.
func (s *mysqlDB) SetFoundryGraphJobSources(ctx context.Context, id, workerID string, sources []model.FoundryLoadSource) error {
	raw, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_load_jobs SET sources=CAST(? AS JSON)`+ownedFoundryGraphJob, string(raw), id, workerID)
}

// RenewFoundryGraphJobLease extends the lease; ErrNotFound means workerID lost it.
func (s *mysqlDB) RenewFoundryGraphJobLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_load_jobs SET lease_until=?`+ownedFoundryGraphJob,
		time.Now().UTC().Add(lease), id, workerID)
}

// UpdateFoundryGraphJob records a running attempt's phase and progress and,
// with IngestViolations, its load's ingest count in the same statement;
// ErrNotFound means workerID lost the lease, and nothing was written.
func (s *mysqlDB) UpdateFoundryGraphJob(ctx context.Context, id, workerID string, update model.FoundryGraphJobUpdate) error {
	p := update.Progress
	if update.IngestViolations == nil {
		return execOrNotFound(ctx, s.db, `UPDATE foundry_load_jobs
SET phase=?, chunks_done=?, documents_done=?, documents_total=?`+ownedFoundryGraphJob,
			string(update.Phase), p.ChunksDone, p.DocumentsDone, p.DocumentsTotal, id, workerID)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_load_jobs j JOIN foundry_loads l ON l.id = j.load_id
SET j.phase=?, j.chunks_done=?, j.documents_done=?, j.documents_total=?, l.ingest_violations=?
WHERE j.id=? AND j.status='running' AND j.lease_owner=?`,
		string(update.Phase), p.ChunksDone, p.DocumentsDone, p.DocumentsTotal, *update.IngestViolations, id, workerID)
}

// RecordFoundryDocumentExtraction stores how a document ended in the job's
// load and adds what it spent to the job, in one transaction, while workerID
// holds the lease: an attempt that lost its lease writes nothing. Writing the
// same document into the same load again replaces its row; with keepExisting
// (the load already held the document) a row already there stays, unless it
// is a failure. Either way what it spent counts. A job no person requested
// adds the spend, in the same transaction, to the month's row of the set
// the document joined the load with (foundry_document_set_spend): the UTC
// month the spend is recorded in, the first day of which is
// config.FoundryGraphConfig.MonthBudget's Since. Progress is the attempt's
// to record, with UpdateFoundryGraphJob.
func (s *mysqlDB) RecordFoundryDocumentExtraction(ctx context.Context, jobID, workerID string, e *model.FoundryDocumentExtraction, keepExisting bool) error {
	switch {
	case (e.Status == model.FoundryExtractionDuplicate) != (e.DuplicateOf != nil),
		(e.Status == model.FoundryExtractionFailed) != (e.Error != nil):
		return fmt.Errorf("foundry document extraction %s has status %s with duplicate_of=%t error=%t",
			e.DocumentID, e.Status, e.DuplicateOf != nil, e.Error != nil)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var ontologyID, workspaceID, loadID, kind string
		var requestedBy sql.NullString
		if err := tx.db.QueryRowContext(ctx, `SELECT ontology_id, workspace_id, load_id, kind, requested_by_user_id FROM foundry_load_jobs`+
			ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).Scan(&ontologyID, &workspaceID, &loadID, &kind, &requestedBy); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if kind == string(model.FoundryGraphJobDelete) {
			return fmt.Errorf("foundry graph job %s deletes a load; it extracts no documents", jobID)
		}
		// A job writes only the documents of its load (foundryLoadDocuments).
		holds, err := tx.FoundryLoadHoldsDocument(ctx, loadID, e.DocumentID)
		if err != nil {
			return err
		}
		if !holds {
			return fmt.Errorf("foundry graph job %s writes load %s, which document %s did not join", jobID, loadID, e.DocumentID)
		}
		var code any
		var retryable any
		if e.Error != nil {
			code, retryable = string(e.Error.Code), e.Error.Retryable
		}
		c := e.Counts
		verb, onDuplicate := "INSERT", ` ON DUPLICATE KEY UPDATE job_id=VALUES(job_id), status=VALUES(status),
  duplicate_of=VALUES(duplicate_of), error_code=VALUES(error_code), error_retryable=VALUES(error_retryable),
  sections=VALUES(sections), entities=VALUES(entities), mentions=VALUES(mentions), facts=VALUES(facts),
  reviews=VALUES(reviews), unresolved=VALUES(unresolved), rejected=VALUES(rejected), usd_micros=VALUES(usd_micros),
  input_tokens=VALUES(input_tokens), output_tokens=VALUES(output_tokens),
  artifact_key=COALESCE(VALUES(artifact_key), artifact_key), attributes=VALUES(attributes), attribute_reviews=VALUES(attribute_reviews), prop_conflicts=VALUES(prop_conflicts), created=VALUES(created), identifiers_unverified=VALUES(identifiers_unverified), attributes_over_entries=VALUES(attributes_over_entries), resolved_user=VALUES(resolved_user), resolved_adjudicated=VALUES(resolved_adjudicated), resolved_identifier=VALUES(resolved_identifier), resolved_name=VALUES(resolved_name), resolved_alias=VALUES(resolved_alias), resolved_created=VALUES(resolved_created), finished_at=VALUES(finished_at)`
		if keepExisting {
			// A failed row is what the load held before, not what it holds now.
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_extractions WHERE load_id=? AND document_id=? AND status='failed'`,
				loadID, e.DocumentID); err != nil {
				return err
			}
			verb, onDuplicate = "INSERT IGNORE", ""
		}
		if _, err := tx.db.ExecContext(ctx, verb+` INTO foundry_load_extractions
  (load_id, document_id, ontology_id, workspace_id, job_id, status, duplicate_of, error_code, error_retryable, sections, entities,
   mentions, facts, reviews, unresolved, rejected, usd_micros, input_tokens, output_tokens, artifact_key, attributes, attribute_reviews, prop_conflicts, created, identifiers_unverified, attributes_over_entries, resolved_user, resolved_adjudicated, resolved_identifier, resolved_name, resolved_alias, resolved_created, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6))`+onDuplicate,
			loadID, e.DocumentID, ontologyID, workspaceID, jobID, string(e.Status), e.DuplicateOf, code, retryable, c.Sections,
			c.Entities, c.Mentions, c.Facts, c.Reviews, c.Unresolved, c.Rejected, e.USDMicros, e.InputTokens, e.OutputTokens,
			e.ArtifactKey, c.Attributes, c.AttributeReviews, c.PropConflicts, c.Created, c.IdentifiersUnverified, c.AttributesOverEntries, c.ResolvedUser, c.ResolvedAdjudicated, c.ResolvedIdentifier, c.ResolvedName, c.ResolvedAlias, c.ResolvedCreated,
		); err != nil {
			return fmt.Errorf("record foundry document extraction: %w", err)
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs
SET usd_micros=usd_micros+?, input_tokens=input_tokens+?, output_tokens=output_tokens+? WHERE id=?`,
			e.USDMicros, e.InputTokens, e.OutputTokens, jobID); err != nil {
			return err
		}
		if requestedBy.Valid || e.USDMicros <= 0 {
			return nil
		}
		// The membership holds the set (FoundryLoadHoldsDocument checked it).
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_document_set_spend (set_id, month, workspace_id, usd_micros)
SELECT m.set_id, ?, ?, ? FROM foundry_load_documents m WHERE m.load_id=? AND m.document_id=?
ON DUPLICATE KEY UPDATE usd_micros=usd_micros+VALUES(usd_micros)`, utcMonth(time.Now()), workspaceID, e.USDMicros, loadID, e.DocumentID)
		return err
	})
}

// FinishFoundryGraphJob ends workerID's attempt. ErrNotFound means it lost the
// lease (or the job was cancelled) and nothing was written. Under the
// ontology's lock, which every write that queues a job takes, it either:
//   - requeues the job for RetryAt (FoundryGraphJobRetried), at documents once
//     a load job's tables are in; an uncounted attempt is given back;
//   - or ends it, recording what the job did to its load: a load job's
//     load leaves writing as ready (succeeded, and it becomes current),
//     partial (budget_exhausted, current when the ontology has none, or
//     when it refreshes the current load and holds what that load held:
//     of the sets with an allowance its budget stop left unfinished,
//     outcome.HeldSets, every document that load recorded, deleted since
//     or not, else it is deleted) or failed; a delete job's load becomes
//     deleted, its extraction rows removed, or delete_failed, never
//     requeued by itself. Then:
//   - a succeeded load or extract whose load is current, with uploads it has
//     not recorded, is requeued at once at documents, keeping its kind
//     (FoundryGraphJobMerged): the next attempt extracts them. A document it
//     recorded as a retryable failure waits for a later job, so a failing
//     document can't requeue it forever;
//   - otherwise it ends (FoundryGraphJobEnded) and queues what comes next
//     (outcome.Next): the ontology keeps its newest Next.RetainLoads loads;
//     an owed extract is queued into the current load, once the uploaded
//     documents of its scope have joined it, while documents wait for it
//     and the month has budget for work no one started (and is owed no
//     more either way);
//     failing that, a delete of the oldest load being deleted. A deleted
//     ontology's job only queues the next delete, and with none left the
//     ontology lets go of its database, unless a load whose delete failed
//     is still in it.
func (s *mysqlDB) FinishFoundryGraphJob(ctx context.Context, jobID, workerID string, outcome model.FoundryGraphJobOutcome) (model.FoundryGraphJobFinish, error) {
	switch {
	case len(outcome.HeldSets) > 0 && (outcome.RetryAt != nil || outcome.Status != model.FoundryGraphJobBudgetExhausted):
		return "", fmt.Errorf("foundry graph job outcome %q holds sets back, which only a budget stop does", outcome.Status)
	case outcome.RetryAt != nil:
	case outcome.Uncounted:
		return "", fmt.Errorf("foundry graph job outcome %q is uncounted but not a retry", outcome.Status)
	case outcome.Status != model.FoundryGraphJobSucceeded && outcome.Status != model.FoundryGraphJobBudgetExhausted &&
		outcome.Status != model.FoundryGraphJobFailed:
		return "", fmt.Errorf("foundry graph job outcome %q is neither a retry nor terminal", outcome.Status)
	case outcome.Next.ExtractID == "" || outcome.Next.DeleteID == "" || outcome.Next.RetainLoads < 1:
		return "", fmt.Errorf("an ended foundry graph job needs the next jobs' ids and how many loads to keep")
	}
	if s.root == nil {
		return "", fmt.Errorf("foundry graph job finish requires a root database connection")
	}
	var workspaceID, ontologyID string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id, ontology_id FROM foundry_load_jobs WHERE id=?`, jobID).
		Scan(&workspaceID, &ontologyID); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", err
	}
	var finish model.FoundryGraphJobFinish
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		job, err := scanFoundryGraphJob(tx.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+
			" FROM foundry_load_jobs"+ownedFoundryGraphJob+" FOR UPDATE", jobID, workerID))
		if err != nil {
			return err
		}
		if job.Kind == model.FoundryGraphJobDelete && outcome.Status == model.FoundryGraphJobBudgetExhausted {
			return fmt.Errorf("foundry graph job %s deletes a load; it spends no budget", jobID)
		}
		p := outcome.Progress
		attempt := `error=NULLIF(?, ''), report=COALESCE(CAST(? AS JSON), report), chunks_done=?, documents_done=?,
  documents_total=?, lease_owner=NULL, lease_until=NULL`
		attemptArgs := []any{outcome.Error, nullJSON(outcome.Report), p.ChunksDone, p.DocumentsDone, p.DocumentsTotal}
		if outcome.RetryAt != nil {
			finish = model.FoundryGraphJobRetried
			// A retry resumes where the tables are.
			phase := job.Phase
			if outcome.IngestDone && (job.Kind == model.FoundryGraphJobLoad || job.Kind == model.FoundryGraphJobApply) {
				phase = model.FoundryGraphJobDocuments
			}
			givenBack := 0
			if outcome.Uncounted {
				givenBack = 1
			}
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET status='queued', next_attempt_at=?, phase=?,
  attempts=GREATEST(attempts-?, 0), `+attempt+` WHERE id=?`,
				append(append([]any{outcome.RetryAt.UTC(), string(phase), givenBack}, attemptArgs...), jobID)...)
			return err
		}
		if err := tx.endFoundryLoadWork(ctx, o, job, outcome.Status, outcome.IngestDone, outcome.HeldSets); err != nil {
			return err
		}
		if outcome.Status == model.FoundryGraphJobSucceeded && job.Kind != model.FoundryGraphJobDelete &&
			o.CurrentLoadID != nil && *o.CurrentLoadID == job.LoadID {
			unrecorded, err := tx.countUnrecordedFoundryDocuments(ctx, job.LoadID)
			if err != nil {
				return err
			}
			if unrecorded > 0 {
				// Not a failure: the next attempt starts afresh on the documents.
				finish = model.FoundryGraphJobMerged
				_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET status='queued', next_attempt_at=NOW(6), attempts=0,
  phase='documents', `+attempt+` WHERE id=?`, append(attemptArgs, jobID)...)
				return err
			}
		}
		finish = model.FoundryGraphJobEnded
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET status=?, active_key=NULL, finished_at=NOW(6), `+
			attempt+` WHERE id=?`, append(append([]any{string(outcome.Status)}, attemptArgs...), jobID)...); err != nil {
			return err
		}
		// An apply that left the load's tables behind hands the refresh to a full
		// load, while the load it was for is still current; that load's own end
		// then queues what follows.
		if job.Kind == model.FoundryGraphJobApply && outcome.Status == model.FoundryGraphJobFailed && !outcome.IngestDone &&
			o.DeletedAt == nil && o.CurrentLoadID != nil && *o.CurrentLoadID == job.LoadID {
			if outcome.Next.RefreshID == "" {
				return fmt.Errorf("an apply that ends failed needs the full refresh's id")
			}
			return tx.queueFullFoundryRefresh(ctx, o, job.VersionID, &model.FoundryGraphJob{ID: outcome.Next.RefreshID})
		}
		if job.Kind == model.FoundryGraphJobApply && outcome.Status == model.FoundryGraphJobSucceeded && outcome.IngestDone &&
			o.DeletedAt == nil && o.CurrentLoadID != nil && *o.CurrentLoadID == job.LoadID && !outcome.Next.RebuildBefore.IsZero() {
			rebuilt, err := tx.queueFoundryRebuild(ctx, o, job, outcome.Next)
			if err != nil || rebuilt {
				return err
			}
		}
		return tx.queueAfterFoundryGraphJob(ctx, o, outcome.Next)
	})
	return finish, err
}

// endFoundryLoadWork records on its load what an ended job did, and keeps o
// in step. A load job's load leaves writing once, at its first end: a later
// end of the same job, merged meanwhile, changes nothing; its People
// enrichment step, if still preparing or queued, ends with it. held are the
// sets with an allowance the job's budget stop left unfinished
// (FoundryGraphJobOutcome.HeldSets). The caller holds the ontology's lock.
func (s *mysqlDB) endFoundryLoadWork(ctx context.Context, o *model.FoundryOntology, job *model.FoundryGraphJob, status model.FoundryGraphJobStatus,
	ingestDone bool, held []string) error {
	switch job.Kind {
	case model.FoundryGraphJobApply:
		// What the apply brought the load's tables up to is what it pinned.
		if !ingestDone {
			return nil
		}
		if err := s.recordFoundryLoadSources(ctx, job.LoadID, job.Sources); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET applied_at=NOW(6) WHERE id=?`, job.LoadID)
		return err
	case model.FoundryGraphJobLoad:
		// No attempt comes back to a step its job left preparing or queued:
		// it ends with the job (cancelled if consent was withdrawn), so the
		// sweep deletes what an attempt wrote of it.
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status=IF(cancel_requested, 'cancelled', 'failed'), ended_at=NOW(6) WHERE load_id=? AND status IN ('preparing', 'queued')`,
			job.LoadID); err != nil {
			return err
		}
		load := map[model.FoundryGraphJobStatus]model.FoundryLoadStatus{
			model.FoundryGraphJobSucceeded:       model.FoundryLoadReady,
			model.FoundryGraphJobBudgetExhausted: model.FoundryLoadPartial,
			model.FoundryGraphJobFailed:          model.FoundryLoadFailed,
		}[status]
		res, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status=?, finished_at=NOW(6) WHERE id=? AND status='writing'`,
			string(load), job.LoadID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		if o.DeletedAt != nil || load == model.FoundryLoadFailed {
			return nil
		}
		if err := s.recordFoundryLoadSources(ctx, job.LoadID, job.Sources); err != nil {
			return err
		}
		var origin string
		var refreshes sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT origin, refreshes_load_id FROM foundry_loads WHERE id=?`, job.LoadID).
			Scan(&origin, &refreshes); err != nil {
			return err
		}
		replaced := o.CurrentLoadID
		refresh := origin == string(model.FoundryLoadByRefresh)
		// A refresh replaces only the load it refreshed.
		refreshesCurrent := refresh && replaced != nil && refreshes.Valid && *replaced == refreshes.String
		becomesCurrent := load == model.FoundryLoadReady || replaced == nil
		if load == model.FoundryLoadPartial && refreshesCurrent {
			// A partial refresh (one whose budget ran out) replaces the load it
			// refreshed once it holds what that load held: it recorded every
			// document that load recorded and that is still uploaded and of
			// its own membership. One that lost a document's replay waits for
			// a run with budget; an empty one never replaces a full one.
			var missing bool
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_extractions p
  JOIN foundry_load_documents m ON m.load_id=? AND m.document_id=p.document_id
  JOIN foundry_documents d ON d.id=p.document_id AND d.status='uploaded'
  WHERE p.load_id=? AND p.status IN ('extracted', 'duplicate', 'out_of_scope')
    AND NOT EXISTS (SELECT 1 FROM foundry_load_extractions e WHERE e.load_id=? AND e.document_id=p.document_id
                    AND e.status IN ('extracted', 'duplicate', 'out_of_scope')))`,
				job.LoadID, refreshes.String, job.LoadID).Scan(&missing); err != nil {
				return err
			}
			becomesCurrent = !missing
			if becomesCurrent && len(held) > 0 {
				// Nor does one drop what that load extracted of a set with an
				// allowance that it left unfinished: those documents'
				// entities, a window's trimmed pieces among them, stay until
				// a run with budget extracts what replaced them. Such a
				// refresh never becomes current, so it is deleted rather
				// than kept among the loads.
				var dropped bool
				if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_extractions p
  JOIN foundry_documents d ON d.id=p.document_id AND d.set_id IN (`+idPlaceholders(len(held))+`)
  WHERE p.load_id=? AND p.status IN ('extracted', 'duplicate', 'out_of_scope')
    AND NOT EXISTS (SELECT 1 FROM foundry_load_extractions e WHERE e.load_id=? AND e.document_id=p.document_id
                    AND e.status IN ('extracted', 'duplicate', 'out_of_scope')))`,
					append(toAnySlice(held), refreshes.String, job.LoadID)...).Scan(&dropped); err != nil {
					return err
				}
				if dropped {
					_, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting' WHERE id=?`, job.LoadID)
					return err
				}
			}
		}
		if !becomesCurrent {
			return nil
		}
		if refresh && !refreshesCurrent {
			// Someone moved the current load meanwhile.
			_, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting' WHERE id=?`, job.LoadID)
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET current_load_id=? WHERE id=?`, job.LoadID, o.ID); err != nil {
			return err
		}
		o.CurrentLoadID = &job.LoadID
		// A refresh that replaced a refresh deletes it now, so refreshes never crowd out a customer's loads.
		if refresh && replaced != nil {
			if _, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting'
WHERE id=? AND origin='refresh' AND status IN ('ready', 'partial')`, *replaced); err != nil {
				return err
			}
		}
	case model.FoundryGraphJobDelete:
		if status == model.FoundryGraphJobFailed {
			_, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='delete_failed' WHERE id=? AND status='deleting'`, job.LoadID)
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleted', deleted_at=NOW(6) WHERE id=? AND status='deleting'`,
			job.LoadID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions WHERE load_id=?`, job.LoadID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_documents WHERE load_id=?`, job.LoadID); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_sources WHERE load_id=?`, job.LoadID); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_extractions WHERE load_id=?`, job.LoadID)
		return err
	}
	return nil
}

// queueAfterFoundryGraphJob queues what follows a job that ended, as
// FinishFoundryGraphJob says. The caller holds the ontology's lock and no job
// of it is live.
func (s *mysqlDB) queueAfterFoundryGraphJob(ctx context.Context, o *model.FoundryOntology, next model.FoundryGraphNext) error {
	if o.DeletedAt != nil {
		return s.queueNextFoundryDelete(ctx, o, next.DeleteID)
	}
	if err := s.retainFoundryLoads(ctx, o, next.RetainLoads); err != nil {
		return err
	}
	if o.ExtractOwed {
		// What the current load should send joins it once a job ends, not as each document comes.
		if o.CurrentLoadID != nil {
			if err := s.snapshotFoundryLoadDocuments(ctx, *o.CurrentLoadID); err != nil {
				return err
			}
		}
		queued, err := s.queueFoundryExtract(ctx, o, &model.FoundryGraphJob{ID: next.ExtractID}, next.Budget)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET extract_owed=FALSE WHERE id=?`, o.ID); err != nil {
			return err
		}
		if queued {
			return nil
		}
	}
	return s.queueNextFoundryDelete(ctx, o, next.DeleteID)
}

func scanFoundryGraphJob(scanner rowScanner) (*model.FoundryGraphJob, error) {
	var job model.FoundryGraphJob
	var kind, status, phase string
	var clientToken, leaseOwner, errText, report, requestedBy, sources sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	p, spend := &job.Progress, &job.Spend
	if err := scanner.Scan(&job.ID, &job.WorkspaceID, &job.OntologyID, &job.VersionID, &job.LoadID, &kind, &status, &phase, &clientToken,
		&leaseOwner, &leaseUntil, &job.Attempts, &job.NextAttemptAt, &errText, &report, &p.ChunksDone, &p.DocumentsDone,
		&p.DocumentsTotal, &spend.USDMicros, &spend.InputTokens, &spend.OutputTokens, &requestedBy, &job.CreatedAt, &startedAt,
		&finishedAt, &sources,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	job.Kind, job.Status, job.Phase = model.FoundryGraphJobKind(kind), model.FoundryGraphJobStatus(status), model.FoundryGraphJobPhase(phase)
	if report.Valid {
		job.Report = json.RawMessage(report.String)
	}
	if sources.Valid {
		if err := json.Unmarshal([]byte(sources.String), &job.Sources); err != nil {
			return nil, fmt.Errorf("decode load job %s sources: %w", job.ID, err)
		}
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{clientToken, &job.ClientToken}, {leaseOwner, &job.LeaseOwner}, {errText, &job.Error}, {requestedBy, &job.RequestedByUserID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{leaseUntil, &job.LeaseUntil}, {startedAt, &job.StartedAt}, {finishedAt, &job.FinishedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &job, nil
}

// queueFoundryRebuild queues a full refresh load (next.RefreshID) of the
// load an apply just brought up to date, when that load was built before
// next.RebuildBefore. The rebuild reads the snapshots the apply pinned, so
// what it holds unlike the applied load is drift, not newer data. It reports
// whether it queued one. The caller holds the ontology's lock.
func (s *mysqlDB) queueFoundryRebuild(ctx context.Context, o *model.FoundryOntology, apply *model.FoundryGraphJob,
	next model.FoundryGraphNext) (bool, error) {
	var finished sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT finished_at FROM foundry_loads WHERE id=?`, apply.LoadID).Scan(&finished); err != nil {
		return false, err
	}
	if !finished.Valid || !finished.Time.Before(next.RebuildBefore) {
		return false, nil
	}
	if next.RefreshID == "" {
		return false, fmt.Errorf("a rebuild needs the full refresh's id")
	}
	if err := s.queueFullFoundryRefresh(ctx, o, apply.VersionID, &model.FoundryGraphJob{ID: next.RefreshID}); err != nil {
		return false, err
	}
	if len(apply.Sources) == 0 {
		return true, nil
	}
	pins, err := json.Marshal(apply.Sources)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET sources=CAST(? AS JSON) WHERE id=?`, string(pins), next.RefreshID)
	return true, err
}

// recordFoundryLoadSources sets the snapshot of each table the load holds to
// those of sources, the snapshots a job of it pinned; a table not among them
// is no longer the load's. A job that pinned nothing (one before pins, or a
// load of documents only) leaves them as they are.
func (s *mysqlDB) recordFoundryLoadSources(ctx context.Context, loadID string, sources []model.FoundryLoadSource) error {
	if len(sources) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_sources WHERE load_id=?`, loadID); err != nil {
		return err
	}
	rows := make([]string, 0, len(sources))
	args := make([]any, 0, 7*len(sources))
	for _, src := range sources {
		var syncedAt sql.NullTime
		if !src.SyncedAt.IsZero() {
			syncedAt = sql.NullTime{Time: src.SyncedAt.UTC(), Valid: true}
		}
		rows = append(rows, "(?, ?, ?, ?, ?, ?, ?)")
		args = append(args, loadID, src.ConnectorID, src.Table, src.S3Key, src.SyncID, src.ChangeSeq, syncedAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_sources (load_id, connector_id, table_name, s3_key, sync_id, change_seq, synced_at)
VALUES `+strings.Join(rows, ", "), args...)
	return err
}

// ListFoundryLoadSources returns the snapshot of each table the loads hold,
// by load, each load's by connector and table.
func (s *mysqlDB) ListFoundryLoadSources(ctx context.Context, loadIDs []string) (map[string][]model.FoundryLoadSource, error) {
	out := make(map[string][]model.FoundryLoadSource, len(loadIDs))
	if len(loadIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT load_id, connector_id, table_name, s3_key, sync_id, change_seq, synced_at
FROM foundry_load_sources WHERE load_id IN (`+idPlaceholders(len(loadIDs))+`) ORDER BY load_id, connector_id, table_name`,
		toAnySlice(loadIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var loadID string
		var src model.FoundryLoadSource
		var syncedAt sql.NullTime
		if err := rows.Scan(&loadID, &src.ConnectorID, &src.Table, &src.S3Key, &src.SyncID, &src.ChangeSeq, &syncedAt); err != nil {
			return nil, err
		}
		src.SyncedAt = syncedAt.Time
		out[loadID] = append(out[loadID], src)
	}
	return out, rows.Err()
}
