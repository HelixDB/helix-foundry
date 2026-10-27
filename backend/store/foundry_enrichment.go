package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
)

// People enrichment's rows (foundry_load_enrichments and its companions).
// A load's step is prepared and applied by the graph worker, fenced on the
// load job's lease, and run by the enrichment worker, fenced on the step's
// own lease. Writes that wake a parked job, or must see a park whole, take
// the ontology's lock first, then the step's row.

const foundryLoadEnrichmentColumns = `e.load_id, e.workspace_id, e.ontology_id, e.job_id, e.status, e.stopped_by, e.claim_by, e.deadline,
       e.lease_owner, e.lease_until, e.batch_cursor, e.reserved_credits, e.inflight_credits, e.counts, e.plan_key, e.carried_key,
       e.profiles_key, e.results_key, e.applied_key, e.content_sha256, e.stop_requested, e.cancel_requested, e.purged_at,
       e.created_at, e.claimed_at, e.ended_at, e.published_at, e.calls_deleted_at`

// ownedFoundryLoadEnrichment fences a write on the enrichment worker's lease.
const ownedFoundryLoadEnrichment = ` WHERE e.load_id=? AND e.status='running' AND e.lease_owner=?`

// jobOwnedFoundryLoadEnrichment fences a write on the load job's lease: the
// graph worker's attempt that prepares or applies the step.
const jobOwnedFoundryLoadEnrichment = ` WHERE e.load_id=? AND EXISTS (SELECT 1 FROM foundry_load_jobs j
  WHERE j.id=e.job_id AND j.status='running' AND j.lease_owner=?)`

// foundryEnrichmentEnded is the SQL list of ended statuses.
const foundryEnrichmentEnded = `('done', 'partial', 'off', 'failed', 'cancelled')`

// keepIngestCounts is the counts JSON ? with the applied and unmatched the
// load's Ingest recorded on the row kept: the enrichment worker never counts
// them, and a worker past its deadline may still write after the Ingest.
const keepIngestCounts = `JSON_SET(CAST(? AS JSON),
  '$.applied', COALESCE(CAST(JSON_EXTRACT(counts, '$.applied') AS SIGNED), 0),
  '$.unmatched', COALESCE(CAST(JSON_EXTRACT(counts, '$.unmatched') AS SIGNED), 0))`

// GetFoundryEnrichmentConsent returns the workspace's newest consent change;
// ErrNotFound means it was never set, which is off.
func (s *mysqlDB) GetFoundryEnrichmentConsent(ctx context.Context, workspaceID string) (*model.FoundryEnrichmentConsent, error) {
	var c model.FoundryEnrichmentConsent
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, enabled, monthly_credit_cap, notice_version, user_id, created_at
FROM foundry_enrichment_consents WHERE workspace_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, workspaceID).
		Scan(&c.ID, &c.WorkspaceID, &c.Enabled, &c.MonthlyCreditCap, &c.NoticeVersion, &c.UserID, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// GetFoundryEnrichmentSettings reads what a workspace's People enrichment
// settings show: the newest change, the change that turned it on while it
// is on (the first since it was last off), the month's ledger since since,
// and how many of its ontologies a purge still has to clean.
func (s *mysqlDB) GetFoundryEnrichmentSettings(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentSettings, error) {
	var out model.FoundryEnrichmentSettings
	consent, err := s.GetFoundryEnrichmentConsent(ctx, workspaceID)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return out, err
	default:
		out.Consent = consent
	}
	if out.Consent != nil && out.Consent.Enabled {
		var c model.FoundryEnrichmentConsent
		if err := s.db.QueryRowContext(ctx, `SELECT c.id, c.workspace_id, c.enabled, c.monthly_credit_cap, c.notice_version, c.user_id, c.created_at
FROM foundry_enrichment_consents c
WHERE c.workspace_id=? AND c.enabled AND NOT EXISTS (SELECT 1 FROM foundry_enrichment_consents d
  WHERE d.workspace_id=c.workspace_id AND NOT d.enabled AND (d.created_at > c.created_at OR d.created_at = c.created_at AND d.id > c.id))
ORDER BY c.created_at, c.id LIMIT 1`, workspaceID).
			Scan(&c.ID, &c.WorkspaceID, &c.Enabled, &c.MonthlyCreditCap, &c.NoticeVersion, &c.UserID, &c.CreatedAt); err != nil {
			return out, err
		}
		out.Enabled = &c
	}
	if out.Ledger, err = s.GetFoundryEnrichmentLedger(ctx, workspaceID, since); err != nil {
		return out, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_enrichment_purges WHERE workspace_id=? AND done_at IS NULL`, workspaceID).
		Scan(&out.PurgesPending)
	return out, err
}

// SetFoundryEnrichmentConsent records a change of the workspace's setting.
// Turning consent off, in the same transaction and under the locks of the
// workspace's ontologies: ends its queued steps cancelled and wakes their
// jobs; asks its preparing and running steps to cancel, which publishes
// nothing; marks every step purged, so its files are swept once no writing
// load applies them; and queues a purge for each live ontology with a load,
// current or not, that applied profiles: the purge's clean load becomes
// current and the others are deleted. A load still writing counts, so
// profiles it chose just before are removed once it ends.
func (s *mysqlDB) SetFoundryEnrichmentConsent(ctx context.Context, c *model.FoundryEnrichmentConsent) error {
	if c.ID == "" || c.WorkspaceID == "" || c.UserID == "" || c.NoticeVersion == "" || c.MonthlyCreditCap < 0 {
		return fmt.Errorf("invalid foundry enrichment consent")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_enrichment_consents
  (id, workspace_id, enabled, monthly_credit_cap, notice_version, user_id) VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.WorkspaceID, c.Enabled, c.MonthlyCreditCap, c.NoticeVersion, c.UserID); err != nil {
			return err
		}
		if c.Enabled {
			return nil
		}
		// Every step that wakes a job holds its ontology's lock first.
		ontologies, err := tx.lockWorkspaceFoundryOntologies(ctx, c.WorkspaceID)
		if err != nil {
			return err
		}
		for _, ontologyID := range ontologies {
			if err := tx.cancelFoundryLoadEnrichments(ctx, ontologyID, false); err != nil {
				return err
			}
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_enrichment_purges (ontology_id, workspace_id)
SELECT DISTINCT o.id, o.workspace_id FROM foundry_ontologies o
JOIN foundry_load_enrichments e ON e.ontology_id = o.id JOIN foundry_loads l ON l.id = e.load_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND e.applied_key <> '' AND l.status NOT IN ('deleting', 'deleted')
ON DUPLICATE KEY UPDATE requested_at=NOW(6), attempts=0, last_load_id=NULL, done_at=NULL`, c.WorkspaceID)
		return err
	})
}

// CancelWorkspaceFoundryWork stops a workspace's Foundry work as it is
// marked deleting, in that transaction, as deleting each of its ontologies
// does. Under the ontologies' locks: each live graph job is cancelled (a
// worker running it loses its lease and stops) and the load it was writing
// ends failed, so no sweep waits on it; queued enrichment steps end
// cancelled, preparing and running ones are asked to cancel, and every step
// is purged, so its files are swept; its pending purges end, as its
// databases go with it; and its model providers drop their keys, a pending
// one superseded.
func (s *mysqlDB) CancelWorkspaceFoundryWork(ctx context.Context, workspaceID string) error {
	ontologies, err := s.lockWorkspaceFoundryOntologies(ctx, workspaceID)
	if err != nil {
		return err
	}
	for _, ontologyID := range ontologies {
		if err := s.cancelLiveFoundryGraphJob(ctx, ontologyID, "the workspace was deleted"); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='failed', finished_at=NOW(6)
WHERE ontology_id=? AND status='writing'`, ontologyID); err != nil {
			return err
		}
		if err := s.cancelFoundryLoadEnrichments(ctx, ontologyID, true); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_enrichment_purges SET done_at=NOW(6) WHERE workspace_id=? AND done_at IS NULL`,
		workspaceID); err != nil {
		return err
	}
	return s.dropFoundryAIConfigs(ctx, workspaceID)
}

// lockWorkspaceFoundryOntologies locks every ontology of the workspace, in id
// order, until the transaction ends, and returns their ids.
func (s *mysqlDB) lockWorkspaceFoundryOntologies(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_ontologies WHERE workspace_id=? ORDER BY id FOR UPDATE`, workspaceID)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// cancelFoundryLoadEnrichments ends the ontology's queued step cancelled,
// waking its job, asks a running one to cancel, and marks every step of the
// ontology purged. A preparing step is asked to cancel too, which its job's
// attempt does; with jobsCancelled (the ontology or its workspace is being
// deleted, and the caller cancels its live job) no attempt comes again, so
// it ends cancelled at once. An attempt already preparing it may still
// upload its plan, carried and results until its lease renewal stops it,
// unrecorded; the sweep deletes its whole layout once that attempt has
// stopped (ListFoundryEnrichmentSweeps' wholeBefore). The caller holds the
// ontology's lock.
func (s *mysqlDB) cancelFoundryLoadEnrichments(ctx context.Context, ontologyID string, jobsCancelled bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id FROM foundry_load_enrichments WHERE ontology_id=? AND status='queued' FOR UPDATE`, ontologyID)
	if err != nil {
		return err
	}
	jobs, err := scanStrings(rows)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status='cancelled', ended_at=NOW(6) WHERE ontology_id=? AND (status='queued' OR status='preparing' AND ?)`, ontologyID, jobsCancelled); err != nil {
		return err
	}
	for _, jobID := range jobs {
		if err := s.wakeFoundryGraphJob(ctx, jobID); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET cancel_requested = cancel_requested OR status IN ('preparing', 'running'), purged_at=COALESCE(purged_at, NOW(6))
WHERE ontology_id=?`, ontologyID)
	return err
}

// cancelFoundryLoadEnrichment ends the step of one load whose job the
// caller cancelled (a refresh a user's load went before), as deleting its
// ontology ends every step: queued or preparing, it ends cancelled at once,
// since no attempt comes again; running, it is asked to cancel, which
// publishes nothing. It is purged, so nothing of it is carried forward and
// the sweep removes its whole layout once an attempt that was preparing it
// has stopped (ListFoundryEnrichmentSweeps' wholeBefore). A step that ended
// is left as it is: its results are the next load's carry-forward. MySQL
// sets the columns in order, so each reads the status before the update.
// The caller holds the ontology's lock.
func (s *mysqlDB) cancelFoundryLoadEnrichment(ctx context.Context, loadID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET cancel_requested = cancel_requested OR status = 'running', purged_at = COALESCE(purged_at, NOW(6)),
  ended_at = IF(status = 'running', ended_at, NOW(6)), status = IF(status = 'running', status, 'cancelled')
WHERE load_id=? AND status IN ('preparing', 'queued', 'running')`, loadID)
	return err
}

// wakeFoundryGraphJob makes a job parked on its enrichment step due now. A
// job that is not parked (running, or past provisioning) is left alone: an
// attempt running it parks after reading the step whole. The caller holds
// the ontology's lock.
func (s *mysqlDB) wakeFoundryGraphJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET next_attempt_at=NOW(6)
WHERE id=? AND status='queued' AND phase='provisioning'`, jobID)
	return err
}

// GetFoundryLoadEnrichment returns a load's enrichment step; ErrNotFound means
// it has none.
func (s *mysqlDB) GetFoundryLoadEnrichment(ctx context.Context, loadID string) (*model.FoundryLoadEnrichment, error) {
	return scanFoundryLoadEnrichment(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+
		" FROM foundry_load_enrichments e WHERE e.load_id=?", loadID))
}

// ListFoundryLoadEnrichments returns the steps of the loads that have one,
// by load id.
func (s *mysqlDB) ListFoundryLoadEnrichments(ctx context.Context, loadIDs []string) (map[string]*model.FoundryLoadEnrichment, error) {
	out := make(map[string]*model.FoundryLoadEnrichment, len(loadIDs))
	if len(loadIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+" FROM foundry_load_enrichments e WHERE e.load_id IN ("+
		idPlaceholders(len(loadIDs))+")", toAnySlice(loadIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		row, err := scanFoundryLoadEnrichment(rows)
		if err != nil {
			return nil, err
		}
		out[row.LoadID] = row
	}
	return out, rows.Err()
}

// GetLatestFoundryEnrichmentResults returns the ontology's newest step with
// results that are not purged, whatever became of its load: carry-forward
// reads its results. ErrNotFound means none has.
func (s *mysqlDB) GetLatestFoundryEnrichmentResults(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error) {
	return scanFoundryLoadEnrichment(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+`
FROM foundry_load_enrichments e WHERE e.ontology_id=? AND e.results_key IS NOT NULL AND e.purged_at IS NULL
ORDER BY e.created_at DESC, e.load_id DESC LIMIT 1`, ontologyID))
}

// GetLatestFoundryEnrichmentApplied returns the ontology's newest step that
// applied profiles of its own (its carried or published file), not purged,
// whose load is not being deleted: a load whose own step failed applies the
// same file. A step that applied another load's file (its own prepare
// failed) is passed over: that file is kept only while its own load exists,
// which nothing ties to the step. ErrNotFound means none did.
func (s *mysqlDB) GetLatestFoundryEnrichmentApplied(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error) {
	return scanFoundryLoadEnrichment(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+`
FROM foundry_load_enrichments e JOIN foundry_loads l ON l.id = e.load_id
WHERE e.ontology_id=? AND e.applied_key IN (e.carried_key, e.profiles_key) AND e.purged_at IS NULL
  AND l.status NOT IN ('deleting', 'deleted')
ORDER BY e.created_at DESC, e.load_id DESC LIMIT 1`, ontologyID))
}

// GetFoundryEnrichmentLedger returns the month's credits since since: those
// charged, and those reserved by running steps, for the workspace and
// across all.
func (s *mysqlDB) GetFoundryEnrichmentLedger(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentLedger, error) {
	return s.foundryEnrichmentLedger(ctx, workspaceID, "", since)
}

// foundryEnrichmentLedger is GetFoundryEnrichmentLedger leaving out the
// reservation of step exceptLoad.
func (s *mysqlDB) foundryEnrichmentLedger(ctx context.Context, workspaceID, exceptLoad string, since time.Time) (model.FoundryEnrichmentLedger, error) {
	var l model.FoundryEnrichmentLedger
	err := s.db.QueryRowContext(ctx, `SELECT
  (SELECT COALESCE(SUM(credits + uncertain_credits), 0) FROM foundry_enrichment_charges WHERE workspace_id=? AND charged_at >= ?),
  (SELECT COALESCE(SUM(reserved_credits), 0) FROM foundry_load_enrichments WHERE status='running' AND workspace_id=? AND load_id<>?),
  (SELECT COALESCE(SUM(credits + uncertain_credits), 0) FROM foundry_enrichment_charges WHERE charged_at >= ?),
  (SELECT COALESCE(SUM(reserved_credits), 0) FROM foundry_load_enrichments WHERE status='running' AND load_id<>?)`,
		workspaceID, since.UTC(), workspaceID, exceptLoad, since.UTC(), exceptLoad).
		Scan(&l.WorkspaceCharged, &l.WorkspaceReserved, &l.GlobalCharged, &l.GlobalReserved)
	return l, err
}

// GetFoundryEnrichmentProvider returns the provider's halt; a provider never
// halted has none.
func (s *mysqlDB) GetFoundryEnrichmentProvider(ctx context.Context, provider string) (*model.FoundryEnrichmentProvider, error) {
	p := model.FoundryEnrichmentProvider{Provider: provider}
	var until sql.NullTime
	var code sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT halted_until, halt_code FROM foundry_enrichment_provider WHERE provider=?`, provider).
		Scan(&until, &code)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &p, nil
	case err != nil:
		return nil, err
	}
	p.HaltedUntil, p.HaltCode = timePtr(until), stringPtr(code)
	return &p, nil
}

// HaltFoundryEnrichmentProvider halts the provider's lookups until until,
// saying why; a longer halt already recorded stays.
func (s *mysqlDB) HaltFoundryEnrichmentProvider(ctx context.Context, provider string, until time.Time, code string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_enrichment_provider (provider, halted_until, halt_code) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE halt_code=IF(halted_until IS NULL OR halted_until < VALUES(halted_until), VALUES(halt_code), halt_code),
  halted_until=IF(halted_until IS NULL OR halted_until < VALUES(halted_until), VALUES(halted_until), halted_until)`,
		provider, until.UTC(), code)
	return err
}

// lockFoundryEnrichmentProvider locks the provider's row, creating it, until
// the transaction ends, and reports whether its halt is in force by the
// database's clock. It serializes claims and credit reservations, so the
// running limit and the caps hold across workers.
func (s *mysqlDB) lockFoundryEnrichmentProvider(ctx context.Context, provider string) (halted bool, err error) {
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_enrichment_provider (provider) VALUES (?)`, provider); err != nil {
		return false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(halted_until > NOW(6), FALSE) FROM foundry_enrichment_provider WHERE provider=? FOR UPDATE`,
		provider).Scan(&halted)
	return halted, err
}

// StartFoundryLoadEnrichment inserts the step of job's load as preparing,
// unless it has one, and returns the stored step. It is fenced on the job's
// lease: ErrNotFound means jobWorker no longer holds it.
func (s *mysqlDB) StartFoundryLoadEnrichment(ctx context.Context, jobWorker string, job *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error) {
	if job.Kind != model.FoundryGraphJobLoad {
		return nil, fmt.Errorf("foundry graph job %s is a %s; only a load enriches", job.ID, job.Kind)
	}
	res, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_load_enrichments (load_id, workspace_id, ontology_id, job_id, status)
SELECT load_id, workspace_id, ontology_id, id, 'preparing' FROM foundry_load_jobs WHERE id=? AND status='running' AND lease_owner=?`,
		job.ID, jobWorker)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		var owned int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM foundry_load_jobs WHERE id=? AND status='running' AND lease_owner=?`, job.ID, jobWorker).Scan(&owned)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
	}
	return s.GetFoundryLoadEnrichment(ctx, job.LoadID)
}

// QueueFoundryLoadEnrichment hands a preparing step to the enrichment worker
// until q.ClaimBy. A Stop or a cancel asked for while it was prepared ends it
// instead, partial(stopped) or cancelled. It returns the stored step; it is
// fenced on the job's lease (ErrNotFound), and refuses a step not preparing.
func (s *mysqlDB) QueueFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, q model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error) {
	if q.PlanKey == "" || q.CarriedKey == "" {
		return nil, fmt.Errorf("a queued foundry enrichment step needs its plan and carried files")
	}
	counts, err := json.Marshal(q.Counts)
	if err != nil {
		return nil, err
	}
	var e *model.FoundryLoadEnrichment
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		row, err := tx.lockPreparingFoundryLoadEnrichment(ctx, loadID, jobWorker)
		if err != nil {
			return err
		}
		status, stoppedBy := "queued", sql.NullString{}
		switch {
		case row.CancelRequested:
			status = string(model.FoundryEnrichmentCancelled)
		case row.StopRequested != nil:
			status, stoppedBy = string(model.FoundryEnrichmentPartial), sql.NullString{String: string(model.FoundryEnrichmentStopStopped), Valid: true}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status=?, stopped_by=?, plan_key=?, carried_key=?, counts=?, claim_by=?, ended_at=IF(?='queued', NULL, NOW(6)) WHERE load_id=?`,
			status, stoppedBy, q.PlanKey, q.CarriedKey, string(counts), q.ClaimBy.UTC(), status, loadID); err != nil {
			return err
		}
		e, err = tx.GetFoundryLoadEnrichment(ctx, loadID)
		return err
	})
	return e, err
}

// EndPreparedFoundryLoadEnrichment ends a preparing step with no lookups and
// chooses the file the load applies, in one statement. A cancel asked for
// while it was prepared ends it cancelled, applying no profiles. It is
// fenced on the job's lease (ErrNotFound), and refuses a step not preparing.
func (s *mysqlDB) EndPreparedFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, p model.FoundryEnrichmentPrepared) error {
	counts, err := json.Marshal(p.Counts)
	if err != nil {
		return err
	}
	if p.End.Status() == "" {
		return fmt.Errorf("a prepared foundry enrichment step needs its end")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		row, err := tx.lockPreparingFoundryLoadEnrichment(ctx, loadID, jobWorker)
		if err != nil {
			return err
		}
		end, applied := p.End, p.Applied
		if row.CancelRequested {
			end, applied = model.EndFoundryEnrichment(model.FoundryEnrichmentCancelled), model.FoundryEnrichmentNone
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status=?, stopped_by=?, plan_key=NULLIF(?, ''), carried_key=NULLIF(?, ''), results_key=NULLIF(?, ''),
  published_at=IF(? = '', NULL, NOW(6)), counts=?, applied_key=?, content_sha256=?, ended_at=NOW(6)
WHERE load_id=?`,
			string(end.Status()), stopName(end), p.PlanKey, p.CarriedKey, p.ResultsKey, p.ResultsKey, string(counts),
			applied.Key, applied.SHA256, loadID)
		return err
	})
}

// lockPreparingFoundryLoadEnrichment locks a preparing step, fenced on its
// job's lease: ErrNotFound when jobWorker lost it.
func (s *mysqlDB) lockPreparingFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string) (*model.FoundryLoadEnrichment, error) {
	row, err := scanFoundryLoadEnrichment(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+
		" FROM foundry_load_enrichments e"+jobOwnedFoundryLoadEnrichment+" FOR UPDATE", loadID, jobWorker))
	if err != nil {
		return nil, err
	}
	if row.Status != model.FoundryEnrichmentPreparing {
		return nil, fmt.Errorf("foundry enrichment step %s is %s, not preparing", loadID, row.Status)
	}
	return row, nil
}

// ApplyFoundryLoadEnrichment chooses the file the load's Ingest reads, once:
// a step that already chose keeps its choice, which it returns. A step
// purged meanwhile (consent was withdrawn) applies no profiles, whatever was
// asked. It is fenced on the job's lease (ErrNotFound).
func (s *mysqlDB) ApplyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, applied model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error) {
	if err := execOrNotFound(ctx, s.db, `UPDATE foundry_load_enrichments e
SET e.applied_key=COALESCE(e.applied_key, IF(e.purged_at IS NULL, ?, '')),
  e.content_sha256=COALESCE(e.content_sha256, IF(e.purged_at IS NULL, ?, ?))`+jobOwnedFoundryLoadEnrichment,
		applied.Key, applied.SHA256, model.FoundryEnrichmentNone.SHA256, loadID, jobWorker); err != nil {
		return model.FoundryEnrichmentApplied{}, err
	}
	var stored model.FoundryEnrichmentApplied
	err := s.db.QueryRowContext(ctx, `SELECT applied_key, content_sha256 FROM foundry_load_enrichments WHERE load_id=?`, loadID).
		Scan(&stored.Key, &stored.SHA256)
	return stored, err
}

// EndBusyFoundryLoadEnrichment ends a step no worker claimed before its
// claim_by partial(busy), only while it is still queued. It is fenced on the
// job's lease (ErrNotFound); a step claimed meanwhile is left as it is.
func (s *mysqlDB) EndBusyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string) error {
	var owned int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM foundry_load_enrichments e`+jobOwnedFoundryLoadEnrichment, loadID, jobWorker).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET status='partial', stopped_by='busy', ended_at=NOW(6)
WHERE load_id=? AND status='queued' AND claim_by <= NOW(6)`, loadID)
	return err
}

// RecordFoundryLoadEnrichmentIngest records what the load's Ingest did with
// the profiles it applied: applied bound a person, unmatched bound no one.
// It is fenced on the job's lease (ErrNotFound), and is written again by an
// Ingest a retry runs again.
func (s *mysqlDB) RecordFoundryLoadEnrichmentIngest(ctx context.Context, loadID, jobWorker string, applied, unmatched int64) error {
	if applied < 0 || unmatched < 0 {
		return fmt.Errorf("foundry enrichment step %s: Ingest applied %d and left %d unmatched", loadID, applied, unmatched)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_load_enrichments e
SET e.counts=JSON_SET(COALESCE(e.counts, JSON_OBJECT()), '$.applied', ?, '$.unmatched', ?)`+jobOwnedFoundryLoadEnrichment,
		applied, unmatched, loadID, jobWorker)
}

// ParkFoundryGraphJob ends workerID's attempt of a load job waiting for its
// enrichment step as an uncounted retry at until, at provisioning. A park is
// no failure, so an earlier attempt's error goes with it. Under the
// ontology's lock, which the step's end takes to wake the job, it reads the
// step: one that ended meanwhile makes the job due now (due), so no wake is
// lost. ErrNotFound means the attempt lost its lease.
func (s *mysqlDB) ParkFoundryGraphJob(ctx context.Context, jobID, workerID string, until time.Time) (due bool, err error) {
	var workspaceID, ontologyID string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id, ontology_id FROM foundry_load_jobs WHERE id=?`, jobID).
		Scan(&workspaceID, &ontologyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
			return err
		}
		job, err := scanFoundryGraphJob(tx.db.QueryRowContext(ctx, "SELECT "+foundryGraphJobColumns+
			" FROM foundry_load_jobs"+ownedFoundryGraphJob+" FOR UPDATE", jobID, workerID))
		if err != nil {
			return err
		}
		if job.Kind != model.FoundryGraphJobLoad || job.Phase != model.FoundryGraphJobProvisioning {
			return fmt.Errorf("foundry graph job %s is a %s at %s; only a load at provisioning parks", jobID, job.Kind, job.Phase)
		}
		var status string
		if err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_load_enrichments WHERE load_id=? FOR UPDATE`, job.LoadID).
			Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("foundry graph job %s has no enrichment step to wait for", jobID)
			}
			return err
		}
		due = model.FoundryEnrichmentStatus(status).Ended()
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs
SET status='queued', next_attempt_at=IF(?, NOW(6), ?), attempts=GREATEST(attempts-1, 0), error=NULL, lease_owner=NULL,
  lease_until=NULL
WHERE id=?`, due, until.UTC(), jobID)
		return err
	})
	return due, err
}

// ClaimFoundryLoadEnrichment leases a step to workerID: first a running one
// whose lease ran out (its worker died; it resumes where the row says), then,
// while fewer than maxRunning run and none of its workspace does, the
// queued one closest to its claim_by, before it. A fresh claim sets the
// deadline loadWait from now. A step past its deadline and the grace runs
// no lookup and its load no longer waits for it (only its late publish is
// left, for the next load's carry-forward), so it holds no running slot,
// however long that publish takes. Nil means none is due. Claims are
// serialized on the provider's row.
//
// first is whether the claim is the step's first: a fresh claim of a queued
// step, which sets claimed_at. A reclaim of a running step keeps it.
func (s *mysqlDB) ClaimFoundryLoadEnrichment(ctx context.Context, workerID, provider string, lease, loadWait, grace time.Duration, maxRunning int) (*model.FoundryLoadEnrichment, bool, error) {
	if s.root == nil {
		return nil, false, fmt.Errorf("foundry enrichment claim requires a root database connection")
	}
	var claimed *model.FoundryLoadEnrichment
	var first bool
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockFoundryEnrichmentProvider(ctx, provider); err != nil {
			return err
		}
		leaseUntil := time.Now().UTC().Add(lease)
		row, err := scanFoundryLoadEnrichment(tx.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+`
FROM foundry_load_enrichments e WHERE e.status='running' AND e.lease_until < NOW(6)
ORDER BY e.lease_until, e.load_id LIMIT 1 FOR UPDATE SKIP LOCKED`))
		switch {
		case err == nil:
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET lease_owner=?, lease_until=? WHERE load_id=?`,
				workerID, leaseUntil, row.LoadID); err != nil {
				return err
			}
			claimed, err = tx.GetFoundryLoadEnrichment(ctx, row.LoadID)
			return err
		case !errors.Is(err, ErrNotFound):
			return err
		}
		var running int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_enrichments
WHERE status='running' AND deadline > NOW(6) - INTERVAL ? MICROSECOND`, grace.Microseconds()).Scan(&running); err != nil {
			return err
		}
		if running >= maxRunning {
			return nil
		}
		row, err = scanFoundryLoadEnrichment(tx.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+`
FROM foundry_load_enrichments e
WHERE e.status='queued' AND e.claim_by > NOW(6) AND NOT e.cancel_requested AND e.stop_requested IS NULL
  AND NOT EXISTS (SELECT 1 FROM foundry_load_enrichments r WHERE r.workspace_id=e.workspace_id AND r.status='running'
    AND r.deadline > NOW(6) - INTERVAL ? MICROSECOND)
ORDER BY e.claim_by, e.load_id LIMIT 1 FOR UPDATE SKIP LOCKED`, grace.Microseconds()))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status='running', lease_owner=?, lease_until=?, claimed_at=NOW(6), deadline=NOW(6) + INTERVAL ? MICROSECOND WHERE load_id=?`,
			workerID, leaseUntil, loadWait.Microseconds(), row.LoadID); err != nil {
			return err
		}
		claimed, err = tx.GetFoundryLoadEnrichment(ctx, row.LoadID)
		first = err == nil
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return claimed, first, nil
}

// foundryEnrichmentHalted is whether the provider ? is halted now, by the
// database's clock, as an SQL expression.
const foundryEnrichmentHalted = `COALESCE((SELECT p.halted_until > NOW(6) FROM foundry_enrichment_provider p WHERE p.provider=?), FALSE)`

// RenewFoundryLoadEnrichmentLease extends workerID's lease and reads whether
// a Stop or a cancel was asked for, and whether provider is halted, so a
// batch already running stops sending at once; ErrNotFound means it lost
// the lease.
func (s *mysqlDB) RenewFoundryLoadEnrichmentLease(ctx context.Context, loadID, workerID, provider string, lease time.Duration) (model.FoundryEnrichmentSignals, error) {
	if err := execOrNotFound(ctx, s.db, `UPDATE foundry_load_enrichments e SET e.lease_until=?`+ownedFoundryLoadEnrichment,
		time.Now().UTC().Add(lease), loadID, workerID); err != nil {
		return model.FoundryEnrichmentSignals{}, err
	}
	var sig model.FoundryEnrichmentSignals
	err := s.db.QueryRowContext(ctx, `SELECT stop_requested IS NOT NULL, cancel_requested, `+foundryEnrichmentHalted+`
FROM foundry_load_enrichments WHERE load_id=?`, provider, loadID).Scan(&sig.Stop, &sig.Cancel, &sig.Halted)
	return sig, err
}

// ReleaseFoundryLoadEnrichment lets workerID's lease run out now, keeping the
// step running where it is: a worker stopping for a deploy hands it to the
// next claim at once.
func (s *mysqlDB) ReleaseFoundryLoadEnrichment(ctx context.Context, loadID, workerID string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_load_enrichments e SET e.lease_until=NOW(6) - INTERVAL 1 MICROSECOND`+
		ownedFoundryLoadEnrichment, loadID, workerID)
}

// CheckFoundryLoadEnrichment reads what a running step re-checks: a Stop, a
// cancel, provider's halt, the workspace's consent, and whether its
// ontology or workspace is deleted or being deleted.
func (s *mysqlDB) CheckFoundryLoadEnrichment(ctx context.Context, loadID, provider string) (model.FoundryEnrichmentChecks, error) {
	var c model.FoundryEnrichmentChecks
	var workspaceID string
	err := s.db.QueryRowContext(ctx, `SELECT e.workspace_id, e.stop_requested IS NOT NULL, e.cancel_requested, `+foundryEnrichmentHalted+`,
  o.id IS NULL OR o.deleted_at IS NOT NULL OR COALESCE(w.status IN ('deleting', 'deleted'), FALSE)
FROM foundry_load_enrichments e
LEFT JOIN foundry_ontologies o ON o.id = e.ontology_id
LEFT JOIN `+s.workspaces.Table()+` w ON w.id = e.workspace_id
WHERE e.load_id=?`, provider, loadID).Scan(&workspaceID, &c.Stop, &c.Cancel, &c.Halted, &c.Gone)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.Consent, err = s.GetFoundryEnrichmentConsent(ctx, workspaceID)
	if errors.Is(err, ErrNotFound) {
		return c, nil
	}
	return c, err
}

// ReserveFoundryEnrichmentCredits reserves up to want credits for workerID's
// step, as much as the caps leave after the month's charges and the other
// running steps' reservations, and returns what it reserved. It replaces
// what the step held, so it runs with no batch in flight. Reservations are
// serialized on the provider's row.
func (s *mysqlDB) ReserveFoundryEnrichmentCredits(ctx context.Context, loadID, workerID, provider string, want int64, caps model.FoundryEnrichmentCaps) (int64, error) {
	var reserved int64
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockFoundryEnrichmentProvider(ctx, provider); err != nil {
			return err
		}
		row, err := tx.lockOwnedFoundryLoadEnrichment(ctx, loadID, workerID)
		if err != nil {
			return err
		}
		if row.InflightCredits > 0 {
			return fmt.Errorf("foundry enrichment step %s has batch %d in flight; settle it first", loadID, row.BatchCursor)
		}
		ledger, err := tx.foundryEnrichmentLedger(ctx, row.WorkspaceID, loadID, caps.Since)
		if err != nil {
			return err
		}
		reserved = max(min(want, ledger.Headroom(caps)), 0)
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET reserved_credits=? WHERE load_id=?`, reserved, loadID)
		return err
	})
	return reserved, err
}

// BeginFoundryEnrichmentBatch records batch, the step's next, as in flight
// with as many of its want calls as the step's reservation and the caps
// leave (headroom counted from the month's charges, as each batch is charged
// in the month it runs), and returns that number. It reads, before every
// batch, what ends a running step at once: a Stop, a cancel and the
// provider's halt, which another step or an operator may have recorded. With
// any of them, or the caps reached, nothing is in flight (N is 0).
func (s *mysqlDB) BeginFoundryEnrichmentBatch(ctx context.Context, loadID, workerID, provider string, batch, want int,
	caps model.FoundryEnrichmentCaps) (model.FoundryEnrichmentBegun, error) {
	var begun model.FoundryEnrichmentBegun
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		halted, err := tx.lockFoundryEnrichmentProvider(ctx, provider)
		if err != nil {
			return err
		}
		row, err := tx.lockOwnedFoundryLoadEnrichment(ctx, loadID, workerID)
		if err != nil {
			return err
		}
		if row.InflightCredits > 0 || row.BatchCursor != batch {
			return fmt.Errorf("foundry enrichment step %s is at batch %d (in flight: %t), not ready for batch %d",
				loadID, row.BatchCursor, row.InflightCredits > 0, batch)
		}
		begun = model.FoundryEnrichmentBegun{
			FoundryEnrichmentSignals: model.FoundryEnrichmentSignals{Stop: row.StopRequested != nil, Cancel: row.CancelRequested, Halted: halted},
		}
		if begun.Stop || begun.Cancel || begun.Halted {
			return nil
		}
		ledger, err := tx.foundryEnrichmentLedger(ctx, row.WorkspaceID, loadID, caps.Since)
		if err != nil {
			return err
		}
		credits := min(row.ReservedCredits, ledger.Headroom(caps))
		begun.N = int(max(min(int64(want), credits/model.FoundryEnrichmentCredits), 0))
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET inflight_credits=? WHERE load_id=?`,
			int64(begun.N)*model.FoundryEnrichmentCredits, loadID)
		return err
	})
	return begun, err
}

// ChargeFoundryEnrichmentBatch settles workerID's batch in flight, in one
// transaction fenced on its lease: it charges the batch once (a batch
// already charged keeps its charge), moves the cursor past it, frees what it
// had in flight, takes the charge off the reservation, which it then cuts to
// what the step's calls after the batch may cost (charge.Remaining), and
// records counts. ErrNotFound means the lease is lost and nothing was
// written.
func (s *mysqlDB) ChargeFoundryEnrichmentBatch(ctx context.Context, loadID, workerID string, charge model.FoundryEnrichmentCharge, counts model.FoundryEnrichmentCounts) error {
	raw, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		row, err := tx.lockOwnedFoundryLoadEnrichment(ctx, loadID, workerID)
		if err != nil {
			return err
		}
		total := charge.Credits + charge.UncertainCredits
		switch {
		case charge.Batch != row.BatchCursor:
			return fmt.Errorf("foundry enrichment step %s is at batch %d, not %d", loadID, row.BatchCursor, charge.Batch)
		case charge.Credits < 0 || charge.UncertainCredits < 0 || total > row.InflightCredits:
			return fmt.Errorf("foundry enrichment batch %d of %s charges %d credits, more than the %d in flight", charge.Batch, loadID, total,
				row.InflightCredits)
		case charge.Remaining < 0:
			return fmt.Errorf("foundry enrichment batch %d of %s leaves %d credits to reserve", charge.Batch, loadID, charge.Remaining)
		}
		res, err := tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_enrichment_charges (load_id, batch, workspace_id, credits, uncertain_credits)
VALUES (?, ?, ?, ?, ?)`, loadID, charge.Batch, row.WorkspaceID, charge.Credits, charge.UncertainCredits)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			total = 0
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET batch_cursor=?, inflight_credits=0, reserved_credits=LEAST(GREATEST(reserved_credits-?, 0), ?), counts=`+keepIngestCounts+` WHERE load_id=?`,
			charge.Batch+1, total, charge.Remaining, string(raw), loadID)
		return err
	})
}

// lockOwnedFoundryLoadEnrichment locks workerID's running step: ErrNotFound
// when it lost the lease.
func (s *mysqlDB) lockOwnedFoundryLoadEnrichment(ctx context.Context, loadID, workerID string) (*model.FoundryLoadEnrichment, error) {
	return scanFoundryLoadEnrichment(s.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+
		" FROM foundry_load_enrichments e"+ownedFoundryLoadEnrichment+" FOR UPDATE", loadID, workerID))
}

// EndFoundryLoadEnrichment ends workerID's step with no batch in flight,
// with what it published (nil for a cancelled step, which publishes
// nothing), and wakes its parked job, under the ontology's lock. A step the
// graph worker stopped waiting for still ends here: its files feed the next
// load's carry-forward. ErrNotFound means the lease is lost.
func (s *mysqlDB) EndFoundryLoadEnrichment(ctx context.Context, loadID, workerID string, end model.FoundryEnrichmentEnd,
	published *model.FoundryEnrichmentPublished, counts model.FoundryEnrichmentCounts) error {
	if end.Status() == "" || (published == nil) != (end.Status() == model.FoundryEnrichmentCancelled || end.Status() == model.FoundryEnrichmentFailed) {
		return fmt.Errorf("a foundry enrichment step ending %q publishes exactly unless cancelled or failed", end.Status())
	}
	raw, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	var workspaceID, ontologyID string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id, ontology_id FROM foundry_load_enrichments WHERE load_id=?`, loadID).
		Scan(&workspaceID, &ontologyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var profiles, results any
	if published != nil {
		profiles, results = published.ProfilesKey, published.ResultsKey
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
			return err
		}
		row, err := tx.lockOwnedFoundryLoadEnrichment(ctx, loadID, workerID)
		if err != nil {
			return err
		}
		if row.InflightCredits > 0 {
			return fmt.Errorf("foundry enrichment step %s has batch %d in flight; settle it first", loadID, row.BatchCursor)
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status=?, stopped_by=?, profiles_key=COALESCE(?, profiles_key), results_key=COALESCE(?, results_key),
  published_at=IF(? IS NULL, published_at, NOW(6)), counts=`+keepIngestCounts+`, ended_at=NOW(6), lease_owner=NULL, lease_until=NULL,
  reserved_credits=0
WHERE load_id=?`, string(end.Status()), stopName(end), profiles, results, results, string(raw), loadID); err != nil {
			return err
		}
		return tx.wakeFoundryGraphJob(ctx, row.JobID)
	})
}

// StopFoundryLoadEnrichment is a user's Stop of a load's lookups: a queued
// step ends partial(stopped) and wakes its job; a preparing or running one
// is asked to stop, and settles its batch and publishes; an ended one is
// left as it is. ErrNotFound means the live ontology has no such step.
func (s *mysqlDB) StopFoundryLoadEnrichment(ctx context.Context, workspaceID, ontologyID, loadID, userID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
			return err
		}
		row, err := scanFoundryLoadEnrichment(tx.db.QueryRowContext(ctx, "SELECT "+foundryLoadEnrichmentColumns+
			" FROM foundry_load_enrichments e WHERE e.load_id=? AND e.ontology_id=? FOR UPDATE", loadID, ontologyID))
		if err != nil {
			return err
		}
		switch row.Status {
		case model.FoundryEnrichmentQueued:
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments
SET status='partial', stopped_by='stopped', stop_requested=?, ended_at=NOW(6) WHERE load_id=?`, userID, loadID); err != nil {
				return err
			}
			return tx.wakeFoundryGraphJob(ctx, row.JobID)
		case model.FoundryEnrichmentPreparing, model.FoundryEnrichmentRunning:
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET stop_requested=COALESCE(stop_requested, ?) WHERE load_id=?`,
				userID, loadID)
			return err
		}
		return nil
	})
}

// ListFoundryEnrichmentSweeps returns, oldest first, ended steps with files
// to delete now, each with what to delete:
//   - the plan and the batch results, once the step ended;
//   - the raw calls, once the step ended before callsBefore, or is purged;
//   - carried and profiles, once the load is deleted, or the step purged;
//   - the results, once purged, or once a newer step of the ontology
//     published results before supersededBefore (carry-forward reads only
//     the newest; the margin lets a prepare reading the older one finish);
//   - every file of its layout, named on its row or not, once purged, or
//     once it failed before any worker claimed it, and it ended before
//     wholeBefore: an attempt may have written its plan, carried or results
//     before the row named them, and its load applies none of them. A step
//     ended under a running attempt (its ontology deleted while it was
//     preparing) may get them after, until the attempt's lease renewal
//     stops it: wholeBefore, a graph job's lease ago, waits that out. Such
//     a step is listed once (its calls, none, are then recorded deleted).
//
// Carried and profiles a writing load applies, and the files of a purged
// step it applies, wait for that load to end.
//
// Rows the sweep has finished with (no key left, calls deleted), which all
// rows become but those of the loads still kept, are passed over row by
// row. The subqueries run only for the rest, and only where they decide
// something: whether a writing load applies a step's carried or profiles
// only once its load is gone or it is purged or failed unclaimed, whether
// its results are superseded only while it has some; and only within the
// step's ontology (a load applies only its ontology's files, under
// ws/<ws>/enrichment/<ontology>/), on its index. So a sweep reads the ended
// rows once each, not each against every load.
func (s *mysqlDB) ListFoundryEnrichmentSweeps(ctx context.Context, callsBefore, supersededBefore, wholeBefore time.Time, limit int) ([]model.FoundryEnrichmentSweep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT load_id, workspace_id, ontology_id, batch_cursor, plan_key, carried_key, profiles_key,
  results_key, whole, calls_expired, load_gone, applied_by_writing, superseded
FROM (
  SELECT e.load_id, e.workspace_id, e.ontology_id, e.batch_cursor, e.plan_key, e.carried_key, e.profiles_key, e.results_key,
    e.created_at, (e.purged_at IS NOT NULL OR e.status = 'failed' AND e.claimed_at IS NULL) AND e.ended_at < ? AS whole,
    e.calls_deleted_at IS NULL AND e.ended_at < ? AS calls_expired,
    COALESCE(l.status = 'deleted', TRUE) AS load_gone,
    (e.purged_at IS NOT NULL OR e.status = 'failed' AND e.claimed_at IS NULL OR COALESCE(l.status = 'deleted', TRUE))
      AND (e.carried_key IS NOT NULL OR e.profiles_key IS NOT NULL) AND EXISTS (SELECT 1 FROM foundry_load_enrichments x
      JOIN foundry_loads xl ON xl.id = x.load_id
      WHERE x.ontology_id = e.ontology_id AND xl.status = 'writing' AND x.applied_key IN (e.carried_key, e.profiles_key)) AS applied_by_writing,
    e.results_key IS NOT NULL AND EXISTS (SELECT 1 FROM foundry_load_enrichments n WHERE n.ontology_id = e.ontology_id
      AND n.created_at > e.created_at AND n.purged_at IS NULL AND n.results_key IS NOT NULL AND n.published_at < ?) AS superseded,
    e.calls_deleted_at IS NULL AS calls_left
  FROM foundry_load_enrichments e LEFT JOIN foundry_loads l ON l.id = e.load_id
  WHERE e.status IN `+foundryEnrichmentEnded+`
    AND (e.calls_deleted_at IS NULL OR e.plan_key IS NOT NULL OR e.carried_key IS NOT NULL OR e.profiles_key IS NOT NULL
      OR e.results_key IS NOT NULL)
) c
WHERE plan_key IS NOT NULL
   OR calls_expired
   OR (whole AND NOT applied_by_writing AND (calls_left OR carried_key IS NOT NULL OR profiles_key IS NOT NULL OR results_key IS NOT NULL))
   OR (load_gone AND NOT applied_by_writing AND (carried_key IS NOT NULL OR profiles_key IS NOT NULL))
   OR (superseded AND results_key IS NOT NULL)
ORDER BY created_at, load_id LIMIT ?`, wholeBefore.UTC(), callsBefore.UTC(), supersededBefore.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryEnrichmentSweep
	for rows.Next() {
		var sw model.FoundryEnrichmentSweep
		var plan, carried, profiles, results sql.NullString
		var whole, callsExpired, loadGone, appliedByWriting, superseded bool
		if err := rows.Scan(&sw.LoadID, &sw.WorkspaceID, &sw.OntologyID, &sw.Batches, &plan, &carried, &profiles, &results,
			&whole, &callsExpired, &loadGone, &appliedByWriting, &superseded); err != nil {
			return nil, err
		}
		whole = whole && !appliedByWriting
		sw.Plan, sw.WholeLayout = stringPtr(plan), whole
		sw.Calls = callsExpired || whole
		if whole || loadGone && !appliedByWriting {
			sw.Carried, sw.Profiles = stringPtr(carried), stringPtr(profiles)
		}
		if whole || superseded {
			sw.Results = stringPtr(results)
		}
		out = append(out, sw)
	}
	return out, rows.Err()
}

// ClearFoundryEnrichmentFiles records what the sweep deleted: each key it
// names is cleared if the row still holds it, and the raw calls as gone.
func (s *mysqlDB) ClearFoundryEnrichmentFiles(ctx context.Context, sw model.FoundryEnrichmentSweep) error {
	key := func(k *string) any {
		if k == nil {
			return nil
		}
		return *k
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_enrichments SET
  plan_key=IF(plan_key <=> ?, NULL, plan_key), carried_key=IF(carried_key <=> ?, NULL, carried_key),
  profiles_key=IF(profiles_key <=> ?, NULL, profiles_key), results_key=IF(results_key <=> ?, NULL, results_key),
  calls_deleted_at=IF(?, COALESCE(calls_deleted_at, NOW(6)), calls_deleted_at)
WHERE load_id=?`, key(sw.Plan), key(sw.Carried), key(sw.Profiles), key(sw.Results), sw.Calls, sw.LoadID)
	return err
}

// ListFoundryEnrichmentPurges returns the purges not done, oldest first,
// those that queued maxAttempts clean loads (exhausted: an operator must
// look) after every other, so however many pile up they never keep a newer
// purge from its clean load.
func (s *mysqlDB) ListFoundryEnrichmentPurges(ctx context.Context, maxAttempts, limit int) ([]*model.FoundryEnrichmentPurge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ontology_id, workspace_id, requested_at, attempts, last_load_id, done_at
FROM foundry_enrichment_purges WHERE done_at IS NULL ORDER BY attempts >= ?, requested_at, ontology_id LIMIT ?`, maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryEnrichmentPurge
	for rows.Next() {
		var p model.FoundryEnrichmentPurge
		var last sql.NullString
		var done sql.NullTime
		if err := rows.Scan(&p.OntologyID, &p.WorkspaceID, &p.RequestedAt, &p.Attempts, &last, &done); err != nil {
			return nil, err
		}
		p.LastLoadID, p.DoneAt = stringPtr(last), timePtr(done)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// AdvanceFoundryEnrichmentPurge moves an ontology's purge on, under its lock:
//   - a deleted ontology ends the purge (done);
//   - with a job live, it waits: the job may write a load that applied
//     purged profiles, which becomes current when it ends;
//   - the purge's own clean load, once it ended ready or partial (its
//     Extract ran out of budget, and a partial load never replaces a
//     current one), becomes current: it holds no purged profiles, and the
//     withdrawn ones must not wait for the budget;
//   - a current load that applied no purged profiles, or none, ends the
//     purge (done), and the ontology's other loads that applied purged
//     profiles are deleted (a delete job, deleteJobID, is queued);
//   - after maxAttempts clean loads it gives up (exhausted);
//   - a clean load queued less than retryAfter ago waits;
//   - otherwise it queues load (a load job of the current load's version,
//     whose own step applies no profiles while consent is off), skipping the
//     monthly spend check a user's load gets.
//
// ErrNotFound means the ontology has no purge pending.
func (s *mysqlDB) AdvanceFoundryEnrichmentPurge(ctx context.Context, ontologyID string, load *model.FoundryGraphJob, deleteJobID string,
	retryAfter time.Duration, maxAttempts int) (model.FoundryEnrichmentPurgeStep, error) {
	var step model.FoundryEnrichmentPurgeStep
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var workspaceID string
		if err := tx.db.QueryRowContext(ctx, `SELECT workspace_id FROM foundry_enrichment_purges WHERE ontology_id=? AND done_at IS NULL`,
			ontologyID).Scan(&workspaceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		o, err := tx.lockFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		var p model.FoundryEnrichmentPurge
		var last sql.NullString
		if err := tx.db.QueryRowContext(ctx, `SELECT requested_at, attempts, last_load_id FROM foundry_enrichment_purges
WHERE ontology_id=? FOR UPDATE`, ontologyID).Scan(&p.RequestedAt, &p.Attempts, &last); err != nil {
			return err
		}
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		if o.DeletedAt != nil {
			step = model.FoundryEnrichmentPurgeDone
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_enrichment_purges SET done_at=NOW(6) WHERE ontology_id=?`, ontologyID)
			return err
		}
		// A live job may write a load that applied purged profiles, which
		// becomes current when it ends: whether the current load is clean
		// is known only once no job is live.
		if live != nil {
			step = model.FoundryEnrichmentPurgeWaiting
			return nil
		}
		clean := o.CurrentLoadID == nil
		if !clean {
			var applied bool
			err := tx.db.QueryRowContext(ctx, `SELECT applied_key <> '' AND purged_at IS NOT NULL FROM foundry_load_enrichments WHERE load_id=?`,
				*o.CurrentLoadID).Scan(&applied)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			clean = !applied
		}
		if !clean && last.Valid {
			var servable bool
			err := tx.db.QueryRowContext(ctx, `SELECT l.status IN ('ready', 'partial') AND NOT COALESCE(e.applied_key <> '' AND e.purged_at IS NOT NULL, FALSE)
FROM foundry_loads l LEFT JOIN foundry_load_enrichments e ON e.load_id = l.id WHERE l.id=? AND l.ontology_id=?`, last.String, ontologyID).
				Scan(&servable)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if servable {
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET current_load_id=? WHERE id=?`, last.String, ontologyID); err != nil {
					return err
				}
				o.CurrentLoadID, clean = &last.String, true
			}
		}
		switch {
		case clean:
			step = model.FoundryEnrichmentPurgeDone
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_enrichment_purges SET done_at=NOW(6) WHERE ontology_id=?`, ontologyID); err != nil {
				return err
			}
			current := ""
			if o.CurrentLoadID != nil {
				current = *o.CurrentLoadID
			}
			// The loads that applied purged profiles go.
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads l JOIN foundry_load_enrichments e ON e.load_id = l.id
SET l.status='deleting'
WHERE l.ontology_id=? AND l.status IN ('ready', 'partial', 'failed', 'delete_failed') AND e.applied_key <> '' AND e.purged_at IS NOT NULL
  AND l.id <> ?`, ontologyID, current); err != nil {
				return err
			}
			return tx.queueNextFoundryDelete(ctx, o, deleteJobID)
		case p.Attempts >= maxAttempts:
			step = model.FoundryEnrichmentPurgeExhausted
			return nil
		case last.Valid && time.Since(p.RequestedAt) < retryAfter:
			step = model.FoundryEnrichmentPurgeWaiting
			return nil
		}
		var versionID string
		if err := tx.db.QueryRowContext(ctx, `SELECT version_id FROM foundry_loads WHERE id=?`, *o.CurrentLoadID).Scan(&versionID); err != nil {
			return err
		}
		job := *load
		job.WorkspaceID, job.OntologyID, job.VersionID, job.LoadID = workspaceID, ontologyID, versionID, load.ID
		job.Kind, job.Phase, job.ClientToken = model.FoundryGraphJobLoad, model.FoundryGraphJobProvisioning, nil
		if err := tx.insertFoundryLoad(ctx, &job); err != nil {
			return err
		}
		step = model.FoundryEnrichmentPurgeQueued
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_enrichment_purges SET attempts=attempts+1, last_load_id=?, requested_at=NOW(6)
WHERE ontology_id=?`, job.ID, ontologyID)
		return err
	})
	return step, err
}

func stopName(end model.FoundryEnrichmentEnd) any {
	if why := end.StoppedBy(); why != nil {
		return string(*why)
	}
	return nil
}

func stringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func timePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	return &v.Time
}

// scanStrings reads one string column of every row and closes rows.
func scanStrings(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanFoundryLoadEnrichment(scanner rowScanner) (*model.FoundryLoadEnrichment, error) {
	var e model.FoundryLoadEnrichment
	var status string
	var stoppedBy, leaseOwner, plan, carried, profiles, results, applied, sha, stop sql.NullString
	var claimBy, deadline, leaseUntil, purged, claimed, ended, published, callsDeleted sql.NullTime
	var counts []byte
	if err := scanner.Scan(&e.LoadID, &e.WorkspaceID, &e.OntologyID, &e.JobID, &status, &stoppedBy, &claimBy, &deadline, &leaseOwner,
		&leaseUntil, &e.BatchCursor, &e.ReservedCredits, &e.InflightCredits, &counts, &plan, &carried, &profiles, &results, &applied,
		&sha, &stop, &e.CancelRequested, &purged, &e.CreatedAt, &claimed, &ended, &published, &callsDeleted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.Status = model.FoundryEnrichmentStatus(status)
	if stoppedBy.Valid {
		why := model.FoundryEnrichmentStop(stoppedBy.String)
		e.StoppedBy = &why
	}
	if len(counts) > 0 {
		if err := json.Unmarshal(counts, &e.Counts); err != nil {
			return nil, fmt.Errorf("foundry enrichment step %s counts: %w", e.LoadID, err)
		}
	}
	if applied.Valid {
		e.Applied = &model.FoundryEnrichmentApplied{Key: applied.String, SHA256: sha.String}
	}
	e.LeaseOwner, e.PlanKey, e.CarriedKey, e.ProfilesKey, e.ResultsKey, e.StopRequested =
		stringPtr(leaseOwner), stringPtr(plan), stringPtr(carried), stringPtr(profiles), stringPtr(results), stringPtr(stop)
	e.ClaimBy, e.Deadline, e.LeaseUntil, e.PurgedAt = timePtr(claimBy), timePtr(deadline), timePtr(leaseUntil), timePtr(purged)
	e.ClaimedAt, e.EndedAt, e.PublishedAt, e.CallsDeletedAt = timePtr(claimed), timePtr(ended), timePtr(published), timePtr(callsDeleted)
	return &e, nil
}
