package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryHealFindingColumns = `ontology_id, fingerprint, kind, status, load_id, base_version_id, finding, support, sources,
       dismissed_by, support_at_dismissal`

const foundryHealRunColumns = `id, ontology_id, load_id, status, pages, cuts, rows_measured, created_at, finished_at`

// HealPublishBatch is the most findings one of a publish's transactions
// writes, in one statement: a dismissal waits for one batch at most, and
// a batch of findings with their examples stays well inside MySQL's
// packet.
const HealPublishBatch = 200

// ErrStale is a heal run's load changed under it: it is no longer the
// ontology's current load, or a graph job is live on the ontology or
// started after the run did.
var ErrStale = errors.New("store: the load changed under the heal run")

// afterHealPublishBatch runs after each batch a publish commits; tests
// stop a publish between batches with it.
var afterHealPublishBatch = func() {}

// ListFoundryHealFindings is an ontology's shown findings: its open ones
// backed by FoundryHealMinSources documents or by rows (sources 0), the
// FoundryHealShownLimit with the most support, ties by fingerprint.
func (s *mysqlDB) ListFoundryHealFindings(ctx context.Context, ontologyID string) ([]*model.FoundryHealFinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryHealFindingColumns+` FROM foundry_heal_findings
WHERE ontology_id=? AND status='open' AND (sources=0 OR sources>=?) ORDER BY support DESC, fingerprint LIMIT ?`,
		ontologyID, model.FoundryHealMinSources, model.FoundryHealShownLimit)
	if err != nil {
		return nil, fmt.Errorf("list heal findings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*model.FoundryHealFinding
	for rows.Next() {
		f, err := scanFoundryHealFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFoundryHealFinding is one of an ontology's findings by fingerprint.
func (s *mysqlDB) GetFoundryHealFinding(ctx context.Context, ontologyID, fingerprint string) (*model.FoundryHealFinding, error) {
	return scanFoundryHealFinding(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFindingColumns+` FROM foundry_heal_findings
WHERE ontology_id=? AND fingerprint=?`, ontologyID, fingerprint))
}

// DismissFoundryHealFinding dismisses an open finding for userID at its
// support now, keeping its finding as it is: ErrNotFound when the ontology
// has no such finding or it isn't open.
func (s *mysqlDB) DismissFoundryHealFinding(ctx context.Context, ontologyID, fingerprint, userID string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_heal_findings SET status='dismissed', dismissed_by=?, support_at_dismissal=support
WHERE ontology_id=? AND fingerprint=? AND status='open'`, userID, ontologyID, fingerprint)
}

// GetLatestFoundryHealRun is an ontology's newest run: ErrNotFound for none.
func (s *mysqlDB) GetLatestFoundryHealRun(ctx context.Context, ontologyID string) (*model.FoundryHealRun, error) {
	return scanFoundryHealRun(s.db.QueryRowContext(ctx, `SELECT `+foundryHealRunColumns+` FROM foundry_heal_runs
WHERE ontology_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, ontologyID))
}

// GetLatestCompleteFoundryHealRun is an ontology's newest complete run, of
// whichever load: ErrNotFound for none. It reads back at most the
// FoundryHealRunsKept runs an ontology keeps.
func (s *mysqlDB) GetLatestCompleteFoundryHealRun(ctx context.Context, ontologyID string) (*model.FoundryHealRun, error) {
	return scanFoundryHealRun(s.db.QueryRowContext(ctx, `SELECT `+foundryHealRunColumns+` FROM foundry_heal_runs
WHERE ontology_id=? AND status='complete' ORDER BY created_at DESC, id DESC LIMIT 1`, ontologyID))
}

// DueFoundryHealOntologies is up to limit live ontologies, of live
// workspaces, whose current load is ready or partial, of an agent bundle,
// with no graph job live on them (an extract, apply or delete would change
// the pages under the audit) and no run held, and due by the load's newest
// run: none; a complete one that started every ago or more (nightly); an
// incomplete or failed one that ended retryAfter ago or more; or a running
// one whose lease ended retryAfter ago or more. The stalest come first: a
// load no run has audited, then by its newest run's start.
//
// The worker calls it every tick. It reads every live ontology's current
// load by its keys, and their runs by (ontology_id, load_id): a few rows
// a day each.
func (s *mysqlDB) DueFoundryHealOntologies(ctx context.Context, every, retryAfter time.Duration, limit int) ([]model.FoundryHealDue, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryLoadColumns+foundryLoadFrom+`
JOIN foundry_ontologies o ON o.current_load_id = l.id AND o.id = l.ontology_id
JOIN `+s.workspaces.Table()+` w ON w.id = o.workspace_id
WHERE o.deleted_at IS NULL AND w.deleted_at IS NULL AND w.status NOT IN ('deleting', 'deleted')
  AND l.status IN ('ready', 'partial') AND v.format = ?
  AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id = o.id AND j.active_key IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM foundry_heal_runs r WHERE r.ontology_id = o.id AND r.active_key IS NOT NULL AND r.lease_until > NOW(6))
  AND COALESCE((SELECT CASE r.status
      WHEN 'complete' THEN r.created_at <= NOW(6) - INTERVAL ? MICROSECOND
      WHEN 'running' THEN r.lease_until <= NOW(6) - INTERVAL ? MICROSECOND
      ELSE r.finished_at <= NOW(6) - INTERVAL ? MICROSECOND END
    FROM foundry_heal_runs r WHERE r.ontology_id = o.id AND r.load_id = l.id ORDER BY r.created_at DESC, r.id DESC LIMIT 1), TRUE)
ORDER BY (SELECT MAX(r.created_at) FROM foundry_heal_runs r WHERE r.ontology_id = o.id AND r.load_id = l.id), o.id LIMIT ?`,
		model.OntologyFormatAgentBundle, every.Microseconds(), retryAfter.Microseconds(), retryAfter.Microseconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("find ontologies due an audit: %w", err)
	}
	var due []model.FoundryHealDue
	for rows.Next() {
		l, err := scanFoundryLoad(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		due = append(due, model.FoundryHealDue{WorkspaceID: l.WorkspaceID, OntologyID: l.OntologyID, Load: l})
	}
	_ = rows.Close()
	return due, rows.Err()
}

// StartFoundryHealRun holds run (its ID, OntologyID and LoadID) as the
// ontology's one running run until lease from now, while LoadID is still
// the live ontology's current load and no graph job is live on it:
// ErrConflict while another run holds it, ErrStale once the load isn't
// due. A running run whose lease has ended is failed first, so a worker
// that died never holds an ontology, and the ontology's runs past its
// FoundryHealRunsKept newest are deleted.
func (s *mysqlDB) StartFoundryHealRun(ctx context.Context, run *model.FoundryHealRun, lease time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_runs SET status='failed', active_key=NULL, lease_until=NULL, finished_at=NOW(6)
WHERE ontology_id=? AND active_key IS NOT NULL AND lease_until <= NOW(6)`, run.OntologyID); err != nil {
		return fmt.Errorf("end a lapsed heal run: %w", err)
	}
	if err := s.pruneFoundryHealRuns(ctx, run.OntologyID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO foundry_heal_runs (id, ontology_id, load_id, status, active_key, lease_until)
SELECT ?, o.id, o.current_load_id, 'running', 1, NOW(6) + INTERVAL ? MICROSECOND FROM foundry_ontologies o
WHERE o.id=? AND o.current_load_id=? AND o.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id = o.id AND j.active_key IS NOT NULL)`,
		run.ID, lease.Microseconds(), run.OntologyID, run.LoadID)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("start heal run: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return errors.Join(ErrStale, err)
	}
	run.Status = model.FoundryHealRunning
	return nil
}

// pruneFoundryHealRuns deletes the ontology's ended runs past its
// FoundryHealRunsKept newest, keeping room for the one starting.
func (s *mysqlDB) pruneFoundryHealRuns(ctx context.Context, ontologyID string) error {
	var at time.Time
	var id string
	switch err := s.db.QueryRowContext(ctx, `SELECT created_at, id FROM foundry_heal_runs WHERE ontology_id=?
ORDER BY created_at DESC, id DESC LIMIT 1 OFFSET ?`, ontologyID, model.FoundryHealRunsKept-1).Scan(&at, &id); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("find an ontology's oldest kept heal run: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_heal_runs
WHERE ontology_id=? AND active_key IS NULL AND (created_at < ? OR (created_at = ? AND id <= ?))`, ontologyID, at, at, id); err != nil {
		return fmt.Errorf("prune heal runs: %w", err)
	}
	return nil
}

// RenewFoundryHealRun extends a running run's lease to lease from now:
// ErrNotFound once it is no longer running, as when it lapsed and another
// worker took the ontology.
func (s *mysqlDB) RenewFoundryHealRun(ctx context.Context, id string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_heal_runs SET lease_until=NOW(6) + INTERVAL ? MICROSECOND
WHERE id=? AND status='running'`, lease.Microseconds(), id)
}

// FinishFoundryHealRun ends a running run incomplete or failed, with the
// pages it read and the cuts among them: ErrNotFound once it is no longer
// running. A complete run ends with PublishFoundryHealRun.
func (s *mysqlDB) FinishFoundryHealRun(ctx context.Context, id string, status model.FoundryHealRunStatus, pages, cuts int) error {
	if status != model.FoundryHealRunIncomplete && status != model.FoundryHealRunFailed {
		return fmt.Errorf("a heal run ends incomplete or failed here, not %q", status)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_heal_runs SET status=?, pages=?, cuts=?, active_key=NULL, lease_until=NULL, finished_at=NOW(6)
WHERE id=? AND status='running'`, status, pages, cuts, id)
}

// ListFoundryHealDocuments is up to limit of a load's documents with
// reviews, by id after after (exclusive; "" for the first): an Audit page.
// It reads the load's extractions by their primary key.
func (s *mysqlDB) ListFoundryHealDocuments(ctx context.Context, loadID, after string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT document_id FROM foundry_load_extractions
WHERE load_id=? AND document_id>? AND status='extracted' AND reviews>0 ORDER BY document_id LIMIT ?`, loadID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("page a load's reviewed documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PublishFoundryHealRun stores a complete run's findings, then supersedes
// the open findings of the kinds it measured that it didn't find, then ends
// the run complete, recording whether it measured the rows. Each finding
// becomes what FoundryHealFinding.Next makes of its row, written in
// transactions of HealPublishBatch findings, each one statement, each of
// which locks its rows and the run's: a dismissal waits for one. Every
// transaction first checks that the run is still running (ErrNotFound once
// it isn't, as when it lapsed and another worker took the ontology) and
// its load still current (ErrStale otherwise); the first also that no
// graph job is live or started since the run did. A job queued later
// changes nothing the run read: every page was read by then. A publish
// that stops partway keeps the batches it committed and leaves the run
// running: the caller ends it.
func (s *mysqlDB) PublishFoundryHealRun(ctx context.Context, p model.FoundryHealPublish) error {
	found := make(map[string]bool, len(p.Findings))
	first := true
	tx := func(fn func(*mysqlDB) error) error {
		jobs := first
		first = false
		return s.healPublishTx(ctx, p, jobs, fn)
	}
	for batch := range slices.Chunk(p.Findings, HealPublishBatch) {
		err := tx(func(q *mysqlDB) error {
			fingerprints := make([]string, len(batch))
			for i, seen := range batch {
				fingerprints[i], found[seen.Fingerprint] = seen.Fingerprint, true
			}
			stored, err := q.lockFoundryHealFindings(ctx, p.OntologyID, fingerprints)
			if err != nil {
				return err
			}
			var next []*model.FoundryHealFinding
			for _, seen := range batch {
				if f := stored[seen.Fingerprint].Next(seen, p.LoadID, p.VersionID); f != nil {
					next = append(next, f)
				}
			}
			return q.writeFoundryHealFindings(ctx, p.OntologyID, next)
		})
		if err != nil {
			return err
		}
		afterHealPublishBatch()
	}
	return tx(func(q *mysqlDB) error {
		// Unmeasured, the rows' findings stand only for the load that
		// measured them: an earlier load's go.
		rows, err := q.db.QueryContext(ctx, `SELECT fingerprint FROM foundry_heal_findings
WHERE ontology_id=? AND status='open' AND (sources>0 OR ? OR load_id<>?) FOR UPDATE`, p.OntologyID, p.RowsMeasured, p.LoadID)
		if err != nil {
			return fmt.Errorf("read open heal findings: %w", err)
		}
		var gone []string
		for rows.Next() {
			var fingerprint string
			if err := rows.Scan(&fingerprint); err != nil {
				_ = rows.Close()
				return err
			}
			if !found[fingerprint] {
				gone = append(gone, fingerprint)
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// A superseded finding keeps no quote of a document, nor a row.
		for batch := range slices.Chunk(gone, HealPublishBatch) {
			if _, err := q.db.ExecContext(ctx, `UPDATE foundry_heal_findings
SET status='superseded', finding=JSON_REMOVE(finding, '$.signal.backing.examples', '$.signal.errors.examples')
WHERE ontology_id=? AND status='open' AND fingerprint IN (`+idPlaceholders(len(batch))+`)`,
				append([]any{p.OntologyID}, toAnySlice(batch)...)...); err != nil {
				return fmt.Errorf("supersede heal findings: %w", err)
			}
		}
		return execOrNotFound(ctx, q.db, `UPDATE foundry_heal_runs SET status='complete', pages=?, cuts=?, rows_measured=?, active_key=NULL,
  lease_until=NULL, finished_at=NOW(6) WHERE id=? AND status='running'`, p.Pages, p.Cuts, p.RowsMeasured, p.RunID)
	})
}

// healPublishTx is healTx for publishing p: fn runs only while p's load is
// still the ontology's current one, and, with jobs, no graph job is live
// or started since p's run did; ErrStale otherwise.
func (s *mysqlDB) healPublishTx(ctx context.Context, p model.FoundryHealPublish, jobs bool, fn func(*mysqlDB) error) error {
	return s.healTx(ctx, p.RunID, func(q *mysqlDB) error {
		switch changed, err := q.foundryHealLoadChanged(ctx, p.RunID, p.OntologyID, p.LoadID, jobs); {
		case err != nil:
			return err
		case changed:
			return ErrStale
		}
		return fn(q)
	})
}

// FoundryHealLoadChanged reports whether run runID's load changed under
// it: it is no longer the live ontology's current load, or a graph job is
// live on the ontology or started since the run did. The worker asks after
// each page, so a stale run stops within one.
func (s *mysqlDB) FoundryHealLoadChanged(ctx context.Context, runID, ontologyID, loadID string) (bool, error) {
	return s.foundryHealLoadChanged(ctx, runID, ontologyID, loadID, true)
}

// foundryHealLoadChanged is FoundryHealLoadChanged, asking about graph
// jobs only with jobs.
func (s *mysqlDB) foundryHealLoadChanged(ctx context.Context, runID, ontologyID, loadID string, jobs bool) (bool, error) {
	var changed bool
	if err := s.db.QueryRowContext(ctx, `SELECT NOT EXISTS (SELECT 1 FROM foundry_ontologies o
    WHERE o.id=? AND o.current_load_id=? AND o.deleted_at IS NULL)
  OR (? AND EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id=? AND (j.active_key IS NOT NULL OR
    j.created_at >= (SELECT r.created_at FROM foundry_heal_runs r WHERE r.id=?))))`,
		ontologyID, loadID, jobs, ontologyID, runID).Scan(&changed); err != nil {
		return false, fmt.Errorf("check the heal run's load: %w", err)
	}
	return changed, nil
}

// healTx runs fn in a transaction that first locks run runID while it is
// running: ErrNotFound once it isn't.
func (s *mysqlDB) healTx(ctx context.Context, runID string, fn func(*mysqlDB) error) error {
	return s.WithTx(ctx, func(tx Store) error {
		q := tx.(*mysqlDB)
		var status model.FoundryHealRunStatus
		switch err := q.db.QueryRowContext(ctx, `SELECT status FROM foundry_heal_runs WHERE id=? FOR UPDATE`, runID).Scan(&status); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("lock heal run: %w", err)
		case status != model.FoundryHealRunning:
			return ErrNotFound
		}
		return fn(q)
	})
}

// lockFoundryHealFindings locks and reads the ontology's rows of these
// fingerprints, by fingerprint.
func (s *mysqlDB) lockFoundryHealFindings(ctx context.Context, ontologyID string, fingerprints []string) (map[string]*model.FoundryHealFinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryHealFindingColumns+` FROM foundry_heal_findings
WHERE ontology_id=? AND fingerprint IN (`+idPlaceholders(len(fingerprints))+`) FOR UPDATE`,
		append([]any{ontologyID}, toAnySlice(fingerprints)...)...)
	if err != nil {
		return nil, fmt.Errorf("lock heal findings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	stored := make(map[string]*model.FoundryHealFinding, len(fingerprints))
	for rows.Next() {
		f, err := scanFoundryHealFinding(rows)
		if err != nil {
			return nil, err
		}
		stored[f.Fingerprint] = f
	}
	return stored, rows.Err()
}

// writeFoundryHealFindings writes fs as the ontology's rows of their
// fingerprints, in one statement.
func (s *mysqlDB) writeFoundryHealFindings(ctx context.Context, ontologyID string, fs []*model.FoundryHealFinding) error {
	if len(fs) == 0 {
		return nil
	}
	row := "(" + idPlaceholders(11) + ")"
	args := make([]any, 0, 11*len(fs))
	for _, f := range fs {
		var by, at any
		if f.Dismissal != nil {
			by, at = f.Dismissal.ByUserID, f.Dismissal.Support
		}
		args = append(args, ontologyID, f.Fingerprint, f.Kind, f.Status, f.LoadID, f.BaseVersionID, string(f.Finding), f.Support, f.Sources, by, at)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_heal_findings (`+foundryHealFindingColumns+`)
VALUES `+strings.Repeat(row+", ", len(fs)-1)+row+`
ON DUPLICATE KEY UPDATE kind=VALUES(kind), status=VALUES(status), load_id=VALUES(load_id), base_version_id=VALUES(base_version_id),
  finding=VALUES(finding), support=VALUES(support), sources=VALUES(sources), dismissed_by=VALUES(dismissed_by),
  support_at_dismissal=VALUES(support_at_dismissal)`, args...)
	if err != nil {
		return fmt.Errorf("write heal findings: %w", err)
	}
	return nil
}

func scanFoundryHealFinding(scanner rowScanner) (*model.FoundryHealFinding, error) {
	var f model.FoundryHealFinding
	var finding []byte
	var by sql.NullString
	var at sql.NullInt64
	if err := scanner.Scan(&f.OntologyID, &f.Fingerprint, &f.Kind, &f.Status, &f.LoadID, &f.BaseVersionID, &finding, &f.Support, &f.Sources,
		&by, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.Finding = json.RawMessage(finding)
	if by.Valid && at.Valid {
		f.Dismissal = &model.FoundryHealDismissal{ByUserID: by.String, Support: at.Int64}
	}
	return &f, nil
}

func scanFoundryHealRun(scanner rowScanner) (*model.FoundryHealRun, error) {
	var r model.FoundryHealRun
	var finishedAt sql.NullTime
	if err := scanner.Scan(&r.ID, &r.OntologyID, &r.LoadID, &r.Status, &r.Pages, &r.Cuts, &r.RowsMeasured, &r.CreatedAt, &finishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if finishedAt.Valid {
		r.FinishedAt = &finishedAt.Time
	}
	return &r, nil
}
