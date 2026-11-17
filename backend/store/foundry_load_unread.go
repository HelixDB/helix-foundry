package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ListFoundryLoadUnread returns, for each of loadIDs, what its documents came
// to when the model failed some (model.FoundryLoadUnread). Every load gets an
// entry, its Read counted; only a load the model failed documents of is
// counted further, so a list of loads reads little more than their rows.
func (s *mysqlDB) ListFoundryLoadUnread(ctx context.Context, loadIDs []string) (map[string]model.FoundryLoadUnread, error) {
	out := make(map[string]model.FoundryLoadUnread, len(loadIDs))
	if len(loadIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT load_id, status, COALESCE(model_failure, ''), COUNT(*) FROM foundry_load_extractions
WHERE load_id IN (`+idPlaceholders(len(loadIDs))+`) AND (status IN ('extracted', 'duplicate') OR status='failed' AND error_code='model')
GROUP BY load_id, status, model_failure`, toAnySlice(loadIDs)...)
	if err != nil {
		return nil, fmt.Errorf("count loads' unread documents: %w", err)
	}
	for rows.Next() {
		var loadID, status, failure string
		var n int
		if err := rows.Scan(&loadID, &status, &failure, &n); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan a load's unread documents: %w", err)
		}
		u := out[loadID]
		if status == string(model.FoundryExtractionFailed) {
			if u.Failed == nil {
				u.Failed = map[string]int{}
			}
			u.Failed[failure] += n
		} else {
			u.Read += n
		}
		out[loadID] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("count loads' unread documents: %w", err)
	}
	for _, loadID := range loadIDs {
		u := out[loadID]
		if u.Failures() == 0 {
			out[loadID] = u
			continue
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+foundryDocuments+` JOIN foundry_load_documents m ON m.document_id=d.id AND m.load_id=?
WHERE d.status='uploaded' AND`+pdfReadEnded+`
  AND NOT EXISTS (SELECT 1 FROM foundry_load_extractions e WHERE e.load_id=m.load_id AND e.document_id=d.id)`, loadID).Scan(&u.Unrecorded); err != nil {
			return nil, fmt.Errorf("count load %s's unrecorded documents: %w", loadID, err)
		}
		if u.Live, u.Rereads, u.LastEnd, err = s.foundryRereads(ctx, loadID); err != nil {
			return nil, err
		}
		out[loadID] = u
	}
	return out, nil
}

// foundryRereads reads a load's newest jobs: whether one is live, how many
// re-reads Foundry queued on its own in a row since the newest job anything
// else queued (model.FoundryRereadToken), and when the newest ended. It
// reads at most model.FoundryRereadLimit+1 jobs, newest first
// (foundry_load_jobs_load_idx), whatever the load's history.
func (s *mysqlDB) foundryRereads(ctx context.Context, loadID string) (live bool, rereads int, lastEnd *time.Time, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT active_key IS NOT NULL, COALESCE(client_token, ''), finished_at FROM foundry_load_jobs
WHERE load_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, loadID, model.FoundryRereadLimit+1)
	if err != nil {
		return false, 0, nil, fmt.Errorf("list load %s's jobs: %w", loadID, err)
	}
	defer rows.Close()
	first, counting := true, true
	for rows.Next() {
		var active bool
		var token string
		var finished sql.NullTime
		if err := rows.Scan(&active, &token, &finished); err != nil {
			return false, 0, nil, fmt.Errorf("scan load %s's job: %w", loadID, err)
		}
		if first {
			first, live = false, active
			if finished.Valid {
				lastEnd = &finished.Time
			}
		}
		if !strings.HasPrefix(token, model.FoundryRereadToken) {
			counting = false
		}
		if counting {
			rereads++
		}
	}
	if err := rows.Err(); err != nil {
		return false, 0, nil, fmt.Errorf("list load %s's jobs: %w", loadID, err)
	}
	return live, rereads, lastEnd, nil
}

// ListFoundryRereads pages the live ontologies whose current load, ready or
// partial, holds a document the model failed with one of classes (or with
// a class recorded before Foundry kept it), retryable, while no job of the
// ontology is live: those a re-read may be due for (QueueFoundryReread).
// Ordered by id, after afterID.
func (s *mysqlDB) ListFoundryRereads(ctx context.Context, classes []string, afterID string, limit int) ([]*model.FoundryOntology, error) {
	if len(classes) == 0 {
		return nil, nil
	}
	// The loads holding such rows come from their own index
	// (foundry_load_extractions_failed_idx), so a poll reads the failures,
	// not every document of every current load.
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryOntologyColumns+` FROM foundry_ontologies
WHERE deleted_at IS NULL AND id > ?
  AND current_load_id IN (SELECT e.load_id FROM foundry_load_extractions e
                          WHERE e.status='failed' AND e.error_code='model' AND e.error_retryable
                            AND (e.model_failure IS NULL OR e.model_failure IN (`+idPlaceholders(len(classes))+`)))
  AND EXISTS (SELECT 1 FROM foundry_loads l WHERE l.id=foundry_ontologies.current_load_id AND l.status IN ('ready', 'partial'))
  AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id=foundry_ontologies.id AND j.active_key IS NOT NULL)
ORDER BY id LIMIT ?`, append(append([]any{afterID}, toAnySlice(classes)...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("list ontologies owed a re-read: %w", err)
	}
	return scanAll(rows, scanFoundryOntology)
}

// QueueFoundryReread queues job, an extract of the ontology's current load,
// when the load holds a document the model failed with one of classes (or
// with a class recorded before Foundry kept it) and its re-read is due
// (model.FoundryLoadUnread.RereadAt at now): the documents the model failed
// are read again, with every other document the load waits for. Its client
// token marks it a re-read (model.FoundryRereadToken), so the next one waits
// twice as long. Under the ontology's lock it rechecks what
// ListFoundryRereads found; no job may be live, and the month must have
// budget for work no one started (queueFoundryExtract). It reports whether
// it queued.
func (s *mysqlDB) QueueFoundryReread(ctx context.Context, workspaceID, ontologyID string, classes []string, job *model.FoundryGraphJob,
	budget model.FoundryGraphBudget, now time.Time) (bool, error) {
	if len(classes) == 0 {
		return false, nil
	}
	// Not yet due, or past the limit, as most polls find a load: said
	// without the ontology's lock, which uploads and job ends take.
	var current sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT current_load_id FROM foundry_ontologies WHERE id=? AND workspace_id=? AND deleted_at IS NULL`,
		ontologyID, workspaceID).Scan(&current); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if !current.Valid {
		return false, nil
	}
	var due model.FoundryLoadUnread
	var err error
	if due.Live, due.Rereads, due.LastEnd, err = s.foundryRereads(ctx, current.String); err != nil {
		return false, err
	}
	if at, ok := due.RereadAt(); !ok || at.After(now) {
		return false, nil
	}
	queued := false
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil || o.CurrentLoadID == nil {
			return err
		}
		live, err := tx.liveFoundryGraphJob(ctx, o.ID)
		if err != nil || live != nil {
			return err
		}
		var owed bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_loads l JOIN foundry_load_extractions e ON e.load_id=l.id
WHERE l.id=? AND l.status IN ('ready', 'partial') AND e.status='failed' AND e.error_code='model' AND e.error_retryable
  AND (e.model_failure IS NULL OR e.model_failure IN (`+idPlaceholders(len(classes))+`)))`,
			append([]any{*o.CurrentLoadID}, toAnySlice(classes)...)...).Scan(&owed); err != nil || !owed {
			return err
		}
		var u model.FoundryLoadUnread
		if u.Live, u.Rereads, u.LastEnd, err = tx.foundryRereads(ctx, *o.CurrentLoadID); err != nil {
			return err
		}
		at, ok := u.RereadAt()
		if !ok || at.After(now) {
			return nil
		}
		token := model.FoundryRereadToken + job.ID
		job.ClientToken = &token
		queued, err = tx.queueFoundryExtract(ctx, o, job, budget)
		return err
	})
	return queued, err
}

// completeFoundryLoad makes a partial load ready once a job into it
// (an extract, a re-read among them, or an apply) succeeded and it waits for
// no document: every one the model failed, or a spent budget left out, is
// read now. A load still waiting for one stays as it is. The caller holds the
// ontology's lock.
func (s *mysqlDB) completeFoundryLoad(ctx context.Context, loadID string, status model.FoundryGraphJobStatus) error {
	if status != model.FoundryGraphJobSucceeded {
		return nil
	}
	waiting, err := s.countWaitingFoundryDocuments(ctx, loadID)
	if err != nil || waiting > 0 {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_loads SET status='ready' WHERE id=? AND status='partial'`, loadID)
	return err
}
