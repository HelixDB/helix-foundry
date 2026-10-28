package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
)

// FoundryLoadRefusal is why a load can't be made current or deleted now,
// worded for the customer.
type FoundryLoadRefusal string

const (
	// FoundryLoadNotServable is a load that is not ready or partial.
	FoundryLoadNotServable FoundryLoadRefusal = "only a ready or partial load can be made current"
	// FoundryLoadIsCurrent is the load reads go to.
	FoundryLoadIsCurrent FoundryLoadRefusal = "the current load can't be deleted; make another load current first"
	// FoundryLoadBusy is a load being written, or one a live job targets.
	FoundryLoadBusy FoundryLoadRefusal = "a job is writing this load; delete it once the job ends"
	// FoundryLoadPurged is a load that applied People enrichment profiles
	// the workspace has since withdrawn consent for.
	FoundryLoadPurged FoundryLoadRefusal = "this load holds People enrichment profiles the workspace no longer allows; load the version again"
)

func (r FoundryLoadRefusal) Error() string { return string(r) }

const foundryLoadColumns = `l.id, l.workspace_id, l.ontology_id, l.version_id, v.version, l.status, l.ingest_violations, l.finished_at,
       l.created_at, l.deleted_at, l.origin, l.refreshes_load_id, l.applied_at`

// foundryLoadFrom joins each load to its version for the version's number.
const foundryLoadFrom = ` FROM foundry_loads l JOIN foundry_ontology_versions v ON v.id = l.version_id`

// CreateFoundryLoad queues job, a load job, with the load it creates (whose
// id is the job's), in one transaction under the ontology's lock. A live
// delete gives way: it is cancelled, its load stays deleting, and the load
// job's end queues it again. A live refresh gives way to a user's load too:
// its load is deleted with its People enrichment step, and the owed refresh
// is forgotten, since the new load reads the newest data. ErrConflict means
// a load or extract is live, or the ontology already used the job's client
// token; ErrNotFound that the ontology is not live.
func (s *mysqlDB) CreateFoundryLoad(ctx context.Context, job *model.FoundryGraphJob) error {
	if job.Kind != model.FoundryGraphJobLoad || job.Phase != model.FoundryGraphJobProvisioning || job.LoadID != job.ID {
		return errors.New("a load job starts at provisioning and creates the load with its own id")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockLiveFoundryOntology(ctx, job.WorkspaceID, job.OntologyID); err != nil {
			return err
		}
		return tx.createFoundryLoad(ctx, job)
	})
}

// createFoundryLoad is CreateFoundryLoad under the ontology's lock.
func (s *mysqlDB) createFoundryLoad(ctx context.Context, job *model.FoundryGraphJob) error {
	tx := s
	live, err := tx.liveFoundryGraphJob(ctx, job.OntologyID)
	if err != nil {
		return err
	}
	if live != nil {
		refresh, err := tx.isFoundryRefreshJob(ctx, live)
		if err != nil {
			return err
		}
		userLoad := job.Origin != model.FoundryLoadByRefresh
		switch {
		case live.Kind == model.FoundryGraphJobDelete:
			if err := tx.cancelLiveFoundryGraphJob(ctx, job.OntologyID, "a new load went first; the delete resumes after it"); err != nil {
				return err
			}
		case refresh && userLoad:
			if err := tx.cancelLiveFoundryGraphJob(ctx, job.OntologyID, "a load the user started went first"); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting', finished_at=COALESCE(finished_at, NOW(6))
WHERE id=? AND status='writing'`, live.LoadID); err != nil {
				return err
			}
			if err := tx.cancelFoundryLoadEnrichment(ctx, live.LoadID); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NULL WHERE id=?`, job.OntologyID); err != nil {
				return err
			}
		case live.Kind == model.FoundryGraphJobApply && userLoad:
			// The user's load reads the newest data too. The current load keeps what
			// the apply wrote; a later apply brings it up to date if it stays current.
			if err := tx.cancelLiveFoundryGraphJob(ctx, job.OntologyID, "a load the user started went first"); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NULL WHERE id=?`, job.OntologyID); err != nil {
				return err
			}
		default:
			return ErrConflict
		}
	}
	return tx.insertFoundryLoad(ctx, job)
}

// isFoundryRefreshJob reports a load job that writes a refresh load.
func (s *mysqlDB) isFoundryRefreshJob(ctx context.Context, job *model.FoundryGraphJob) (bool, error) {
	if job.Kind != model.FoundryGraphJobLoad {
		return false, nil
	}
	var origin string
	if err := s.db.QueryRowContext(ctx, `SELECT origin FROM foundry_loads WHERE id=?`, job.LoadID).Scan(&origin); err != nil {
		return false, err
	}
	return origin == string(model.FoundryLoadByRefresh), nil
}

// QueueFoundryRefresh queues a refresh of the ontology, with job's id: an
// apply job that brings its current load up to date in place when job's
// kind asks for one and the current load is servable, or else a load of the
// current load's version that replaces it. It returns false without
// queueing while a job is live, leaving the refresh owed. It does not wait
// for budget: a refresh with none left loads the tables and replays what its
// documents' saved extractions hold, at $0. An ontology with refresh off or
// no current load owes nothing any more, and its owed refresh is forgotten.
func (s *mysqlDB) QueueFoundryRefresh(ctx context.Context, workspaceID, ontologyID string, job *model.FoundryGraphJob) (bool, error) {
	queued := false
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		if o.RefreshDueAt == nil || o.RefreshDueAt.After(time.Now()) {
			return nil
		}
		if !o.AutoRefresh || o.CurrentLoadID == nil {
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NULL WHERE id=?`, o.ID)
			return err
		}
		live, err := tx.liveFoundryGraphJob(ctx, o.ID)
		if err != nil || live != nil {
			return err
		}
		var versionID, status string
		if err := tx.db.QueryRowContext(ctx, `SELECT version_id, status FROM foundry_loads WHERE id=?`, *o.CurrentLoadID).
			Scan(&versionID, &status); err != nil {
			return fmt.Errorf("read current load %s: %w", *o.CurrentLoadID, err)
		}
		job.WorkspaceID, job.OntologyID, job.VersionID = o.WorkspaceID, o.ID, versionID
		if job.Kind == model.FoundryGraphJobApply && model.FoundryLoadStatus(status).Servable() {
			token := "apply:" + job.ID
			job.LoadID, job.Phase, job.ClientToken = *o.CurrentLoadID, model.FoundryGraphJobProvisioning, &token
			if err := tx.insertFoundryGraphJob(ctx, job); err != nil {
				return err
			}
		} else if err := tx.queueFullFoundryRefresh(ctx, o, versionID, job); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NULL WHERE id=?`, o.ID); err != nil {
			return err
		}
		queued = true
		return nil
	})
	return queued, err
}

// queueFullFoundryRefresh queues job as a refresh load of versionID that
// replaces the ontology's current load. The caller holds the ontology's lock.
func (s *mysqlDB) queueFullFoundryRefresh(ctx context.Context, o *model.FoundryOntology, versionID string, job *model.FoundryGraphJob) error {
	token := "refresh:" + job.ID
	job.WorkspaceID, job.OntologyID, job.VersionID, job.LoadID = o.WorkspaceID, o.ID, versionID, job.ID
	job.Kind, job.Phase, job.ClientToken = model.FoundryGraphJobLoad, model.FoundryGraphJobProvisioning, &token
	job.Origin, job.RefreshesLoadID = model.FoundryLoadByRefresh, o.CurrentLoadID
	return s.createFoundryLoad(ctx, job)
}

// insertFoundryLoad writes load job job's load, with the documents and
// review decisions it starts from, and queues the job. The caller holds the
// ontology's lock and has checked no other job is live.
func (s *mysqlDB) insertFoundryLoad(ctx context.Context, job *model.FoundryGraphJob) error {
	origin := job.Origin
	if origin == "" {
		origin = model.FoundryLoadByUser
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, origin, refreshes_load_id)
VALUES (?, ?, ?, ?, 'writing', ?, ?)`, job.ID, job.WorkspaceID, job.OntologyID, job.VersionID, string(origin), job.RefreshesLoadID); err != nil {
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	}
	if err := s.snapshotFoundryLoadDocuments(ctx, job.LoadID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`)
SELECT ?, `+foundryReviewDecisionColumns+` FROM foundry_review_decisions WHERE ontology_id=?`, job.LoadID, job.OntologyID); err != nil {
		return err
	}
	return s.insertFoundryGraphJob(ctx, job)
}

// GetFoundryLoad returns one of the ontology's loads, deleted or not.
func (s *mysqlDB) GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error) {
	return scanFoundryLoad(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadColumns+foundryLoadFrom+`
WHERE l.id=? AND l.ontology_id=?`, id, ontologyID))
}

// ListFoundryLoads returns the ontology's loads not yet deleted, newest
// first. Retention keeps them few: the current load and the newest others up
// to foundry.graph.retain_loads, the one being written, and those being or
// failing to be deleted.
func (s *mysqlDB) ListFoundryLoads(ctx context.Context, ontologyID string) ([]*model.FoundryLoad, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryLoadColumns+foundryLoadFrom+`
WHERE l.ontology_id=? AND l.status<>'deleted' ORDER BY l.created_at DESC, l.id DESC`, ontologyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryLoad
	for rows.Next() {
		l, err := scanFoundryLoad(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListFoundryLoadJobs returns, for each of the given loads that has jobs,
// its newest jobs (at most newest, newest first) and the count and spend of
// all of them. Each load's newest are one query on foundry_load_jobs_load_idx
// that reads no more than newest rows, and retention keeps the loads few; the
// totals are one grouped query, read last so they cover every job listed
// (jobs are never deleted and their spend only grows).
func (s *mysqlDB) ListFoundryLoadJobs(ctx context.Context, loadIDs []string, newest int) (map[string]model.FoundryLoadJobs, error) {
	if newest < 1 {
		return nil, fmt.Errorf("list foundry load jobs: newest must be positive, got %d", newest)
	}
	out := map[string]model.FoundryLoadJobs{}
	if len(loadIDs) == 0 {
		return out, nil
	}
	for _, loadID := range loadIDs {
		rows, err := s.db.QueryContext(ctx, "SELECT "+foundryGraphJobColumns+` FROM foundry_load_jobs
WHERE load_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, loadID, newest)
		if err != nil {
			return nil, err
		}
		var jobs []*model.FoundryGraphJob
		for rows.Next() {
			job, err := scanFoundryGraphJob(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			jobs = append(jobs, job)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(jobs) > 0 {
			out[loadID] = model.FoundryLoadJobs{Newest: jobs}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT load_id, COUNT(*), COALESCE(SUM(usd_micros), 0) FROM foundry_load_jobs
WHERE load_id IN (`+idPlaceholders(len(loadIDs))+`) GROUP BY load_id`, toAnySlice(loadIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var loadID string
		var count int
		var usdMicros int64
		if err := rows.Scan(&loadID, &count, &usdMicros); err != nil {
			return nil, err
		}
		jobs := out[loadID]
		jobs.Count, jobs.USDMicros = count, usdMicros
		out[loadID] = jobs
	}
	return out, rows.Err()
}

// SetCurrentFoundryLoad points the ontology's reads and uploads at a ready or
// partial load (FoundryLoadNotServable otherwise) that applied no purged
// People enrichment profiles (FoundryLoadPurged), under its lock. The load
// may lack documents uploaded since it was written, or hold retryable
// failures: with no job live, an extract (extract's id and requester) is
// queued for them within budget; with one live, its end owes it. queued
// reports an extract queued here. ErrNotFound means the ontology is not live
// or has no such load not yet deleted.
func (s *mysqlDB) SetCurrentFoundryLoad(ctx context.Context, workspaceID, ontologyID, loadID string, extract *model.FoundryGraphJob,
	budget model.FoundryGraphBudget) (queued bool, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		load, err := tx.GetFoundryLoad(ctx, ontologyID, loadID)
		switch {
		case err != nil:
			return err
		case load.Status == model.FoundryLoadDeleted:
			return ErrNotFound
		case !load.Status.Servable():
			return FoundryLoadNotServable
		}
		var purged bool
		switch err := tx.db.QueryRowContext(ctx, `SELECT COALESCE(applied_key, '') <> '' AND purged_at IS NOT NULL
FROM foundry_load_enrichments WHERE load_id=?`, loadID).Scan(&purged); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case purged:
			return FoundryLoadPurged
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET current_load_id=? WHERE id=?`, loadID, ontologyID); err != nil {
			return err
		}
		o.CurrentLoadID = &loadID
		// What the load waits for is what joined it: the uploads since it was
		// written join it now.
		if err := tx.snapshotFoundryLoadDocuments(ctx, loadID); err != nil {
			return err
		}
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		if live == nil {
			queued, err = tx.queueFoundryExtract(ctx, o, extract, budget)
			return err
		}
		waiting, err := tx.countWaitingFoundryDocuments(ctx, loadID)
		if err != nil || waiting == 0 {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET extract_owed=TRUE WHERE id=?`, ontologyID)
		return err
	})
	return queued, err
}

// DeleteFoundryLoad marks a ready, partial, failed or delete_failed load
// deleting, under the ontology's lock, and, with no job live, queues a delete
// job (deleteJobID) for the oldest load being deleted; a live job's end
// queues it otherwise. The current load is FoundryLoadIsCurrent; one being
// written or targeted by the live job FoundryLoadBusy. A load already being
// deleted is left as it is. ErrNotFound means the ontology is not live or
// has no such load not yet deleted.
func (s *mysqlDB) DeleteFoundryLoad(ctx context.Context, workspaceID, ontologyID, loadID, deleteJobID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		load, err := tx.GetFoundryLoad(ctx, ontologyID, loadID)
		switch {
		case err != nil:
			return err
		case load.Status == model.FoundryLoadDeleted:
			return ErrNotFound
		case load.Status == model.FoundryLoadDeleting:
			return nil
		case o.CurrentLoadID != nil && *o.CurrentLoadID == loadID:
			return FoundryLoadIsCurrent
		case !load.Status.Deletable():
			return FoundryLoadBusy
		}
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		if live != nil && live.LoadID == loadID {
			return FoundryLoadBusy
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting' WHERE id=?`, loadID); err != nil {
			return err
		}
		if live != nil {
			return nil
		}
		return tx.queueNextFoundryDelete(ctx, o, deleteJobID)
	})
}

// retainFoundryLoads marks deleting the ontology's ready, partial and failed
// loads past the newest keep, counting the current load, which is always
// kept. The caller holds the ontology's lock and has ended its live job, so
// no job targets them.
func (s *mysqlDB) retainFoundryLoads(ctx context.Context, o *model.FoundryOntology, keep int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_loads
WHERE ontology_id=? AND status IN ('ready', 'partial', 'failed') ORDER BY created_at DESC, id DESC`, o.ID)
	if err != nil {
		return err
	}
	var loads []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		loads = append(loads, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	kept := 0
	if o.CurrentLoadID != nil {
		kept = 1
	}
	var drop []string
	for _, id := range loads {
		switch {
		case o.CurrentLoadID != nil && id == *o.CurrentLoadID:
		case kept < keep:
			kept++
		default:
			drop = append(drop, id)
		}
	}
	if len(drop) == 0 {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting' WHERE id IN (`+idPlaceholders(len(drop))+`)`,
		toAnySlice(drop)...)
	return err
}

// queueFoundryExtract queues extract into the ontology's current load while
// documents of its membership wait for it and the workspace's month has
// budget for extract's requester (model.FoundryGraphBudget.CapFor), or it
// holds deleted documents to take out, whatever the budget; queued
// reports whether it did. The caller holds the ontology's lock, no job is
// live, and what the load should send has joined it: a fan-out's documents
// as they are added, the rest of its scope at a job's end or a change of
// current load (snapshotFoundryLoadDocuments).
func (s *mysqlDB) queueFoundryExtract(ctx context.Context, o *model.FoundryOntology, extract *model.FoundryGraphJob,
	budget model.FoundryGraphBudget) (queued bool, err error) {
	if o.CurrentLoadID == nil {
		return false, nil
	}
	loadID := *o.CurrentLoadID
	waiting, err := s.countWaitingFoundryDocuments(ctx, loadID)
	if err != nil {
		return false, err
	}
	removed, err := s.countRemovedFoundryDocuments(ctx, loadID)
	if err != nil || waiting == 0 && removed == 0 {
		return false, err
	}
	// A removal spends nothing, so only documents to extract wait for budget.
	if removed == 0 {
		spent, err := s.SumFoundryGraphJobSpend(ctx, o.WorkspaceID, budget.Since)
		if err != nil || spent >= budget.CapFor(extract.RequestedByUserID) {
			return false, err
		}
	}
	var versionID string
	if err := s.db.QueryRowContext(ctx, `SELECT version_id FROM foundry_loads WHERE id=?`, loadID).Scan(&versionID); err != nil {
		return false, err
	}
	job := &model.FoundryGraphJob{
		ID: extract.ID, WorkspaceID: o.WorkspaceID, OntologyID: o.ID, VersionID: versionID, LoadID: loadID,
		Kind: model.FoundryGraphJobExtract, Phase: model.FoundryGraphJobDocuments, RequestedByUserID: extract.RequestedByUserID,
	}
	return true, s.insertFoundryGraphJob(ctx, job)
}

// queueNextFoundryDelete queues a delete job (jobID) for the ontology's
// oldest load being deleted. A deleted ontology with none left lets go of
// its database, which another ontology may then be bound to, unless a load
// whose delete failed is still in it: a new ontology would take that load
// for its own data, so the database stays held until a retry deletes it
// (ResumeFoundryOntologyDrain). The caller holds the ontology's lock and no
// job is live.
func (s *mysqlDB) queueNextFoundryDelete(ctx context.Context, o *model.FoundryOntology, jobID string) error {
	var loadID, versionID string
	err := s.db.QueryRowContext(ctx, `SELECT id, version_id FROM foundry_loads
WHERE ontology_id=? AND status='deleting' ORDER BY created_at, id LIMIT 1`, o.ID).Scan(&loadID, &versionID)
	switch {
	case errors.Is(err, sql.ErrNoRows) && o.DeletedAt != nil:
		_, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET live_key=NULL
WHERE id=? AND NOT EXISTS (SELECT 1 FROM foundry_loads WHERE ontology_id=? AND status='delete_failed')`, o.ID, o.ID)
		return err
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return s.insertFoundryGraphJob(ctx, &model.FoundryGraphJob{
		ID: jobID, WorkspaceID: o.WorkspaceID, OntologyID: o.ID, VersionID: versionID, LoadID: loadID,
		Kind: model.FoundryGraphJobDelete, Phase: model.FoundryGraphJobDeleting,
	})
}

func scanFoundryLoad(scanner rowScanner) (*model.FoundryLoad, error) {
	var l model.FoundryLoad
	var status string
	var finishedAt, deletedAt, appliedAt sql.NullTime
	var origin string
	var refreshes sql.NullString
	if err := scanner.Scan(&l.ID, &l.WorkspaceID, &l.OntologyID, &l.VersionID, &l.Version, &status, &l.IngestViolations, &finishedAt,
		&l.CreatedAt, &deletedAt, &origin, &refreshes, &appliedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l.Status, l.Origin = model.FoundryLoadStatus(status), model.FoundryLoadOrigin(origin)
	if refreshes.Valid {
		l.RefreshesLoadID = &refreshes.String
	}
	if finishedAt.Valid {
		l.FinishedAt = &finishedAt.Time
	}
	if deletedAt.Valid {
		l.DeletedAt = &deletedAt.Time
	}
	if appliedAt.Valid {
		l.AppliedAt = &appliedAt.Time
	}
	return &l, nil
}
