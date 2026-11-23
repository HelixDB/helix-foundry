package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
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
// A refresh load is started by the automation run whose sync owed the
// refresh, when one did (OweFoundryRefreshes), at that sync's depth.
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
		} else {
			if err := tx.queueFullFoundryRefresh(ctx, o, versionID, job); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads l JOIN foundry_ontologies o ON o.id = l.ontology_id
SET l.started_by_automation_run_id=o.refresh_cause_run_id, l.automation_chain_depth=o.refresh_cause_depth
WHERE l.id=? AND o.refresh_cause_run_id IS NOT NULL`, job.LoadID); err != nil {
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=NULL, refresh_cause_run_id=NULL, refresh_cause_depth=NULL WHERE id=?`,
			o.ID); err != nil {
			return err
		}
		queued = true
		return nil
	})
	return queued, err
}

// FoundryRebuild is what QueueFoundryRebuild did for one ontology.
type FoundryRebuild int

const (
	// FoundryRebuildQueued is a rebuild queued: a refresh load of the current
	// load's version, which replaces that load once it holds every document
	// the current load held.
	FoundryRebuildQueued FoundryRebuild = iota + 1
	// FoundryRebuildNotOwed is an ontology with no current load, or whose
	// current load was created at or after the cutover: nothing to rebuild.
	FoundryRebuildNotOwed
	// FoundryRebuildUnderWay is an ontology whose live job is a rebuild
	// already: a refresh load of its current load created at or after the
	// cutover, by this engine.
	FoundryRebuildUnderWay
	// FoundryRebuildBusy is an ontology with another live job: queue it
	// again once that ends.
	FoundryRebuildBusy
	// FoundryRebuildGaveUp is an ontology whose current load
	// MaxFoundryRebuilds rebuilds ended without replacing: none is queued
	// again. A load a person starts, or a refresh, still replaces it.
	FoundryRebuildGaveUp
	// FoundryRebuildSample is an ontology whose current load is the sample
	// company's (its version drafted from a sample, package sample): it is
	// never rebuilt. Its data is fixed, and every datetime in it is to the
	// minute, which Helix's own conversion types exactly, so a rebuild would
	// write the same graph again at a model cost. A rebuild of it an earlier
	// run queued that never started is withdrawn.
	FoundryRebuildSample
)

// MaxFoundryRebuilds is how many rebuilds of one current load may end
// without replacing it (failed, or partial without everything it held)
// before QueueFoundryRebuild queues no more: each loads every table again,
// and one that can't succeed (a database that refuses writes, documents
// with no budget left to read) would do so on every run.
const MaxFoundryRebuilds = 3

// QueueFoundryRebuild queues a rebuild of the ontology's current load, with
// job's id, when that load was created before `before`: a full refresh load
// of its version, as QueueFoundryRefresh queues with apply off, whatever the
// ontology's refresh setting or when its refresh is due. A refresh loads the
// tables again, replays the saved extractions of the documents the current
// load holds (at no model cost unless the engine, its prompts or its models
// changed), and replaces the current load only once it holds them all
// (FinishFoundryGraphJob), so a rebuild that fails leaves the load it
// rebuilds current. It is how loads an earlier engine wrote are written
// again by this one (RebuildFoundryLoads); its job's client token is
// model.FoundryRebuildToken and its id.
//
// A live apply gives way, as it does to a person's load: it is cancelled,
// the current load keeps what it wrote, and the ontology owes the refresh
// again, which runs once the rebuild ends (an apply to the rebuilt load, or
// to the old one if the rebuild failed). A live delete gives way as to any
// load (CreateFoundryLoad). Any other live job queues nothing: a refresh of
// the current load created at or after before is the rebuild
// (FoundryRebuildUnderWay), any other holds it back (FoundryRebuildBusy), an
// earlier engine's refresh included, since what it writes is that engine's.
// Nor does it queue one for a current load created at or after before
// (FoundryRebuildNotOwed, the rebuild's once it is current), or once
// MaxFoundryRebuilds rebuilds of the current load ended without replacing
// it (FoundryRebuildGaveUp; a cancelled one doesn't count), so running it
// again is safe. A refresh the ontology owes stays owed. ErrNotFound means
// the ontology is not live.
//
// A sample's load is never rebuilt (FoundryRebuildSample). A rebuild of it
// that is queued and never started is withdrawn: cancelled, and its load,
// of which nothing reached the database, deleted at once, so no job of it
// is left in the queue. What a job's end would have queued is queued then:
// a resend the current load owes, or a delete a load waits for. While the
// ontology owes an extract, whose budget only a worker knows, the load is
// left to a delete job instead, whose end queues it. One that started runs
// to its end (FoundryRebuildUnderWay).
func (s *mysqlDB) QueueFoundryRebuild(ctx context.Context, workspaceID, ontologyID string, before time.Time,
	job *model.FoundryGraphJob) (FoundryRebuild, error) {
	var done FoundryRebuild
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		done = FoundryRebuildNotOwed
		if o.CurrentLoadID == nil {
			return nil
		}
		var versionID string
		var createdAt time.Time
		var sample bool
		if err := tx.db.QueryRowContext(ctx, `SELECT l.version_id, l.created_at,`+sampleRun+`
FROM foundry_loads l JOIN foundry_ontology_versions v ON v.id = l.version_id
LEFT JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE l.id=?`, *o.CurrentLoadID).Scan(&versionID, &createdAt, &sample); err != nil {
			return fmt.Errorf("read current load %s: %w", *o.CurrentLoadID, err)
		}
		if !createdAt.Before(before) {
			return nil
		}
		live, err := tx.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		if sample {
			done = FoundryRebuildSample
			switch {
			case live == nil || live.ClientToken == nil || !strings.HasPrefix(*live.ClientToken, model.FoundryRebuildToken):
				return nil
			case live.Status != model.FoundryGraphJobQueued || live.StartedAt != nil:
				done = FoundryRebuildUnderWay
				return nil
			}
			if err := tx.cancelLiveFoundryGraphJob(ctx, o.ID, "a sample's load is not rebuilt"); err != nil {
				return err
			}
			// What arrived while it waited was left to its end
			// (queueAfterFoundryGraphJob), so it is queued now. An owed
			// extract waits on a budget only a worker knows: the load is
			// left to a delete job, whose end queues the extract.
			if o.ExtractOwed {
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting', finished_at=NOW(6)
WHERE id=? AND status='writing'`, live.LoadID); err != nil {
					return err
				}
				return tx.queueNextFoundryDelete(ctx, o, id.New())
			}
			// It never started, so nothing of its load reached the database:
			// the load is deleted here, with no delete job to wait for.
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleted', deleted_at=NOW(6), finished_at=NOW(6)
WHERE id=? AND status='writing'`, live.LoadID); err != nil {
				return err
			}
			if err := tx.dropFoundryLoadRows(ctx, live.LoadID); err != nil {
				return err
			}
			if queued, err := tx.queueFoundryResend(ctx, o, &model.FoundryGraphJob{ID: id.New()}, 0, true); err != nil || queued {
				return err
			}
			return tx.queueNextFoundryDelete(ctx, o, id.New())
		}
		switch {
		case live == nil, live.Kind == model.FoundryGraphJobDelete:
		case live.Kind == model.FoundryGraphJobApply:
			if err := tx.cancelLiveFoundryGraphJob(ctx, o.ID, "a rebuild of the load went first; the refresh runs after it"); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET refresh_due_at=COALESCE(refresh_due_at, NOW(6)) WHERE id=?`,
				o.ID); err != nil {
				return err
			}
		case live.Kind == model.FoundryGraphJobLoad:
			done = FoundryRebuildBusy
			var origin string
			var refreshes sql.NullString
			var liveCreated time.Time
			if err := tx.db.QueryRowContext(ctx, `SELECT origin, refreshes_load_id, created_at FROM foundry_loads WHERE id=?`, live.LoadID).
				Scan(&origin, &refreshes, &liveCreated); err != nil {
				return fmt.Errorf("read live load %s: %w", live.LoadID, err)
			}
			if origin == string(model.FoundryLoadByRefresh) && refreshes.String == *o.CurrentLoadID && !liveCreated.Before(before) {
				done = FoundryRebuildUnderWay
			}
			return nil
		default:
			done = FoundryRebuildBusy
			return nil
		}
		// Every rebuild of the current load that ended did so without
		// replacing it: one that did would have made a load created since
		// before current.
		var ended int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_jobs j JOIN foundry_loads l ON l.id = j.load_id
WHERE l.ontology_id=? AND l.refreshes_load_id=? AND j.id = l.id AND j.client_token LIKE ? AND j.active_key IS NULL AND j.status<>'cancelled'`,
			o.ID, *o.CurrentLoadID, model.FoundryRebuildToken+"%").Scan(&ended); err != nil {
			return fmt.Errorf("count rebuilds of %s: %w", *o.CurrentLoadID, err)
		}
		if ended >= MaxFoundryRebuilds {
			done = FoundryRebuildGaveUp
			return nil
		}
		token := model.FoundryRebuildToken + job.ID
		job.ClientToken = &token
		if err := tx.queueFullFoundryRefresh(ctx, o, versionID, job); err != nil {
			return err
		}
		done = FoundryRebuildQueued
		return nil
	})
	return done, err
}

// ListFoundryRebuilds returns up to limit live ontologies whose current load
// was created before `before`, by id after afterID (empty starts at the
// first): those QueueFoundryRebuild would rebuild, and the samples it
// skips. One a live job holds back stays listed, so a caller pages past it.
func (s *mysqlDB) ListFoundryRebuilds(ctx context.Context, before time.Time, afterID string, limit int) ([]*model.FoundryOntology, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryOntologyColumns+` FROM foundry_ontologies o
WHERE o.deleted_at IS NULL AND o.id > ?
  AND EXISTS (SELECT 1 FROM foundry_loads l WHERE l.id = o.current_load_id AND l.created_at < ?)
ORDER BY o.id LIMIT ?`, afterID, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryOntology)
}

// FoundryRebuilds is what RebuildFoundryLoads did, by ontology: the
// rebuilds it queued, those an earlier run queued that are still live,
// those another live job held back, which a later run queues once it ends,
// those it gave up on (FoundryRebuildGaveUp), whose earlier-engine load
// stays current until a person loads the ontology again, and the samples it
// skipped (FoundryRebuildSample). Once Queued, UnderWay and Busy are all
// empty, nothing is left to queue or to wait for.
type FoundryRebuilds struct {
	Queued   []string
	UnderWay []string
	Busy     []string
	GaveUp   []string
	Samples  []string
}

// rebuildPage is how many ontologies RebuildFoundryLoads reads at a time.
const rebuildPage = 100

// RebuildFoundryLoads queues a rebuild (QueueFoundryRebuild) of the current
// load of every live ontology whose current load was created before
// `before`: the cutover to an engine that writes the graph differently, such
// as the one that writes Helix's typed DateTime. Hosted runs it once per
// stamp after that engine is deployed; a standalone install runs it at its
// first start of such a release, and `foundry rebuild-loads` runs it on
// demand. It reads the ontologies a page at a time by id, and goes on past
// one it can't queue, returning every such failure joined. Running it again
// queues the rebuilds a live job held back (Busy) and those of rebuilds that
// failed, up to MaxFoundryRebuilds a load (GaveUp after), none for a rebuild
// still live (UnderWay), and none for a load a rebuild already replaced.
// It rebuilds no sample's load, and withdraws a rebuild of one an earlier
// run queued that hasn't started (Samples), so the customers' loads are
// the only rebuilds it leaves queued.
func RebuildFoundryLoads(ctx context.Context, db Store, before time.Time) (FoundryRebuilds, error) {
	var out FoundryRebuilds
	var failed []error
	for after := ""; ; {
		owed, err := db.ListFoundryRebuilds(ctx, before, after, rebuildPage)
		if err != nil {
			return out, errors.Join(append(failed, fmt.Errorf("list ontologies to rebuild: %w", err))...)
		}
		for _, o := range owed {
			done, err := db.QueueFoundryRebuild(ctx, o.WorkspaceID, o.ID, before, &model.FoundryGraphJob{ID: id.New()})
			switch {
			case errors.Is(err, ErrNotFound):
				// Deleted since it was listed.
			case err != nil:
				failed = append(failed, fmt.Errorf("rebuild ontology %s: %w", o.ID, err))
			case done == FoundryRebuildQueued:
				out.Queued = append(out.Queued, o.ID)
			case done == FoundryRebuildUnderWay:
				out.UnderWay = append(out.UnderWay, o.ID)
			case done == FoundryRebuildBusy:
				out.Busy = append(out.Busy, o.ID)
			case done == FoundryRebuildGaveUp:
				out.GaveUp = append(out.GaveUp, o.ID)
			case done == FoundryRebuildSample:
				out.Samples = append(out.Samples, o.ID)
			}
		}
		if len(owed) < rebuildPage {
			return out, errors.Join(failed...)
		}
		after = owed[len(owed)-1].ID
	}
}

// queueFullFoundryRefresh queues job as a refresh load of versionID that
// replaces the ontology's current load, with job's client token, else
// "refresh:" and its id. The caller holds the ontology's lock.
func (s *mysqlDB) queueFullFoundryRefresh(ctx context.Context, o *model.FoundryOntology, versionID string, job *model.FoundryGraphJob) error {
	if job.ClientToken == nil {
		token := "refresh:" + job.ID
		job.ClientToken = &token
	}
	job.WorkspaceID, job.OntologyID, job.VersionID, job.LoadID = o.WorkspaceID, o.ID, versionID, job.ID
	job.Kind, job.Phase = model.FoundryGraphJobLoad, model.FoundryGraphJobProvisioning
	job.Origin, job.RefreshesLoadID = model.FoundryLoadByRefresh, o.CurrentLoadID
	return s.createFoundryLoad(ctx, job)
}

// insertFoundryLoad writes load job job's load, with the documents and
// review decisions it starts from and its mark that grants may cover it,
// and queues the job. The caller holds the
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
	// Every decision an Extract takes: a reject is the site's to read, and is
	// sent to no engine, and neither is an approval of a claim it rejects
	// (foundryRejected), as only an older binary leaves both.
	// Each with its name's key (name_key), by which an Extract finds the
	// aliases its documents write.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, name_key)
SELECT ?, `+foundryReviewDecisionColumns+`, name_key FROM foundry_review_decisions r WHERE r.ontology_id=? AND r.kind<>'reject'
  AND NOT (r.kind='release' AND `+foundryRejected("r", "r.ontology_id")+`)`, job.LoadID, job.OntologyID); err != nil {
		return err
	}
	// Made since grants could cover a load: the decisions it copied had
	// their documents recorded when they were settled.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_access (load_id) VALUES (?)`, job.LoadID); err != nil {
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
	return scanAll(rows, scanFoundryLoad)
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
		jobs, err := scanAll(rows, scanFoundryGraphJob)
		if err != nil {
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

// ListFoundryLoadDocumentSets counts what a load made of its documents, by
// the set each joined it with, in set id order: the load's members that are
// still uploaded (a deleted document is on its way out of the load), by how
// extracting each into the load ended, with what those extractions wrote. A
// member with no extraction row waits for one, and one whose failed row a
// later job replaced counts as that job left it. A document moved to another
// set since counts under the set it joined with. It reads the load's
// membership, documents and extractions by their primary keys: three row
// reads a document, so a caller that polls keeps the answer a while
// (api.loadDocumentSets). A load with no membership returns none: one that
// holds no documents, or one made before membership was recorded.
func (s *mysqlDB) ListFoundryLoadDocumentSets(ctx context.Context, loadID string) ([]model.FoundryLoadDocumentSet, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.set_id, COUNT(*),
  COALESCE(SUM(e.status='extracted'), 0), COALESCE(SUM(e.status='duplicate'), 0), COALESCE(SUM(e.status='failed'), 0),
  COALESCE(SUM(e.status='out_of_scope'), 0), COALESCE(SUM(e.document_id IS NULL), 0),
  COALESCE(SUM(e.facts), 0), COALESCE(SUM(e.attributes), 0), COALESCE(SUM(e.reviews), 0), COALESCE(SUM(e.unresolved), 0)
FROM foundry_load_documents m
JOIN foundry_documents d ON d.id=m.document_id AND d.status='uploaded'
LEFT JOIN foundry_load_extractions e ON e.load_id=m.load_id AND e.document_id=m.document_id
WHERE m.load_id=? GROUP BY m.set_id ORDER BY m.set_id`, loadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sets []model.FoundryLoadDocumentSet
	for rows.Next() {
		var set model.FoundryLoadDocumentSet
		if err := rows.Scan(&set.SetID, &set.Documents, &set.Read, &set.Duplicate, &set.Failed, &set.NotRead, &set.Waiting,
			&set.Facts, &set.Attributes, &set.Reviews, &set.Unresolved); err != nil {
			return nil, err
		}
		sets = append(sets, set)
	}
	return sets, rows.Err()
}

// SetCurrentFoundryLoad points the ontology's reads and uploads at a ready or
// partial load (FoundryLoadNotServable otherwise) that applied no purged
// People enrichment profiles (FoundryLoadPurged), under its lock. The load
// may lack documents uploaded since it was written, or hold retryable
// failures: with no job live, an extract (extract's id and requester) is
// queued for them within budget; with one live, its end owes it. queued
// reports an extract queued here. The load's list also lacks the approvals
// saved since it copied its decisions: with no job live and no extract
// queued, they are carried to it and a resend (resend's id and requester)
// queued for their documents, as a job's end does (queueFoundryResend); a
// live or queued job's end carries them otherwise. The resolves saved since
// join it at once (makeFoundryLoadCurrent), their documents owed, sent by
// that resend or the job's end. ErrNotFound means the ontology is not live
// or has no such load not yet deleted.
func (s *mysqlDB) SetCurrentFoundryLoad(ctx context.Context, workspaceID, ontologyID, loadID string, extract, resend *model.FoundryGraphJob,
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
		// The live job's row before any decision's (the carry writes them), as
		// a worker recording a decision's outcome locks them.
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		if err := tx.makeFoundryLoadCurrent(ctx, o, loadID); err != nil {
			return err
		}
		// What the load waits for is what joined it: the uploads since it was
		// written join it now.
		if err := tx.snapshotFoundryLoadDocuments(ctx, loadID); err != nil {
			return err
		}
		if live == nil {
			queued, err = tx.queueFoundryExtract(ctx, o, extract, budget)
			if err != nil || queued {
				return err
			}
			_, err = tx.queueFoundryResend(ctx, o, resend, 0, true)
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
		ClientToken: extract.ClientToken,
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
