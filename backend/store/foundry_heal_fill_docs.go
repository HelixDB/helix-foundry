package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A fill writes the link a suggestion's "Add this link" added, by a move in
// place, for the documents behind the suggestion's reviews (contract
// 4.33.0), through the self-healing that found it and the resend job that
// applies approvals: no other pipeline. The heal worker collects the
// documents (DueFoundryHealFills, OweFoundryFillDocuments) and starts the
// fill (StartFoundryFill): state applying, each document owed as a 'link'
// resend row. Resend jobs send them, bounded, under the fill's budget, and
// record each document's word (RecordFoundryFillDocument), which ends its
// row and counts it; the last one ends the fill, filled. A failed job backs
// the fill off and finally stops it (failFoundryFill); a cap reached, a
// dismissal or a person's stop end its rows with it (stopFoundryFill).

// foundryLinkLive is the SQL condition that request w (a foundry_load_resends
// row) is not a fill's, or is one whose load's fill applies and isn't
// paused: a paused or stopped fill's rows queue no job and are sent by none.
const foundryLinkLive = `(w.reason<>'link' OR EXISTS (SELECT 1 FROM foundry_heal_fills lf WHERE lf.load_id=w.load_id
  AND lf.state='applying' AND (lf.paused_until IS NULL OR lf.paused_until<=NOW(6))))`

// stopFoundryFill stops load loadID's applying fill for reason, ending the
// rows it owes. The caller holds the ontology's lock, or the job's lease.
func (s *mysqlDB) stopFoundryFill(ctx context.Context, loadID, reason, jobID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='stopped', stop_reason=?, active_key=NULL, paused_until=NULL
WHERE load_id=? AND state='applying'`, reason, loadID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=NULLIF(?, '')
WHERE load_id=? AND reason=? AND status='owed'`, jobID, loadID, string(model.FoundryResendLink))
	return err
}

// failFoundryFill counts a failed resend job against its load's applying
// fill, when it owes documents: the fill pauses for the next
// model.FoundryFillBackoff, so its rows queue no job meanwhile (the heal
// worker resumes it, ResumeFoundryFill), and stops as model_unavailable once
// they're spent. The caller holds the ontology's lock.
func (s *mysqlDB) failFoundryFill(ctx context.Context, job *model.FoundryGraphJob) error {
	var failures int
	err := s.db.QueryRowContext(ctx, `SELECT f.failures FROM foundry_heal_fills f WHERE f.load_id=? AND f.state='applying'
  AND EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=f.load_id AND w.reason=? AND w.status='owed') FOR UPDATE`,
		job.LoadID, string(model.FoundryResendLink)).Scan(&failures)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if failures >= len(model.FoundryFillBackoff) {
		return s.stopFoundryFill(ctx, job.LoadID, model.FoundryFillModelUnavailable, job.ID)
	}
	why := ""
	if job.Error != nil {
		why = *job.Error
	}
	for len(why) > 500 || !utf8.ValidString(why) {
		why = why[:min(len(why), 500)]
		for !utf8.ValidString(why) {
			why = why[:len(why)-1]
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET failures=failures+1, last_error=NULLIF(?, ''),
  paused_until=NOW(6) + INTERVAL ? MICROSECOND WHERE load_id=? AND state='applying'`,
		why, model.FoundryFillBackoff[failures].Microseconds(), job.LoadID)
	return err
}

// FoundryFillApplying is load loadID's applying fill, nil for none.
func (s *mysqlDB) FoundryFillApplying(ctx context.Context, loadID string) (*model.FoundryHealFill, error) {
	fill, err := scanFoundryHealFill(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFillColumns+foundryHealFillFrom+`
WHERE f.load_id=? AND f.state='applying'`, loadID))
	if err == ErrNotFound {
		return nil, nil
	}
	return fill, err
}

// ListFoundryLinkResends is up to limit of the documents load loadID owes a
// fill (model.FoundryResendLink) while its fill applies unpaused, in
// document order: each with its document where the load holds it uploaded
// and extracted, else Unsendable (model.FoundryReleaseDocumentGone, or
// model.FoundryReleaseDocumentUnread for one it holds unextracted).
func (s *mysqlDB) ListFoundryLinkResends(ctx context.Context, loadID string, limit int) ([]*model.FoundryLoadResend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.document_id, w.requests FROM foundry_load_resends w
WHERE w.load_id=? AND w.reason=? AND w.status='owed' AND `+foundryLinkLive+` ORDER BY w.document_id LIMIT ?`,
		loadID, string(model.FoundryResendLink), limit)
	if err != nil {
		return nil, err
	}
	var owed []*model.FoundryLoadResend
	for rows.Next() {
		r := &model.FoundryLoadResend{LoadID: loadID, Reason: model.FoundryResendLink}
		if err := rows.Scan(&r.DocumentID, &r.Requests); err != nil {
			_ = rows.Close()
			return nil, err
		}
		owed = append(owed, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(owed) == 0 {
		return nil, err
	}
	ids := make([]any, len(owed))
	for i, r := range owed {
		ids[i] = r.DocumentID
	}
	docRows, err := s.db.QueryContext(ctx, "SELECT "+strings.Replace(foundryDocumentColumns, "d.set_id", "m.set_id", 1)+
		foundryLoadDocuments+`
 JOIN foundry_load_extractions e ON e.load_id=m.load_id AND e.document_id=d.id AND e.status='extracted'
 WHERE d.status='uploaded' AND d.id IN (`+idPlaceholders(len(ids))+`)`, append([]any{loadID}, ids...)...)
	if err != nil {
		return nil, err
	}
	docs, err := scanAll(docRows, scanFoundryDocument)
	if err != nil {
		return nil, err
	}
	byID := map[string]*model.FoundryDocument{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	var unsendable []any
	for _, r := range owed {
		if r.Document = byID[r.DocumentID]; r.Document == nil {
			unsendable = append(unsendable, r.DocumentID)
		}
	}
	if len(unsendable) == 0 {
		return owed, nil
	}
	heldRows, err := s.db.QueryContext(ctx, `SELECT d.id FROM foundry_documents d
JOIN foundry_load_documents m ON m.document_id=d.id AND m.load_id=?
WHERE d.status='uploaded' AND d.id IN (`+idPlaceholders(len(unsendable))+`)`, append([]any{loadID}, unsendable...)...)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for heldRows.Next() {
		var id string
		if err := heldRows.Scan(&id); err != nil {
			_ = heldRows.Close()
			return nil, err
		}
		held[id] = true
	}
	_ = heldRows.Close()
	if err := heldRows.Err(); err != nil {
		return nil, err
	}
	for _, r := range owed {
		if r.Document != nil {
			continue
		}
		r.Unsendable = model.FoundryReleaseDocumentGone
		if held[r.DocumentID] {
			r.Unsendable = model.FoundryReleaseDocumentUnread
		}
	}
	return owed, nil
}

// RecordFoundryFillDocument records what an Extract of job jobID said of a
// document it sent to be filled, while workerID holds the job's lease
// (ErrNotFound when it lost it, and nothing is written), in one transaction.
// What it spent is the job's, the fill's, and its set's month allowance's,
// whatever else: a call made is paid. The document's request ends if it is
// still the one the job read, and only then are its numbers counted, on the
// fill and on the document's row of the load (a fact more for each fact its
// fill added, a review more for each claim it held and one fewer for each
// it took), and its review cards owed a rebuild. So an event sent again, by
// a retry after one was lost, counts once: the engine says the same numbers
// each time. The fill ends filled once it owes no document. An outcome the
// binary doesn't know counts as refused.
func (s *mysqlDB) RecordFoundryFillDocument(ctx context.Context, jobID, workerID string, d model.FoundryFillDocument) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var loadID, ontologyID, workspaceID string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id, ontology_id, workspace_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`,
			jobID, workerID).Scan(&loadID, &ontologyID, &workspaceID); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if d.USDMicros > 0 || d.InputTokens > 0 || d.OutputTokens > 0 {
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=usd_micros+?, input_tokens=input_tokens+?,
  output_tokens=output_tokens+? WHERE id=?`, d.USDMicros, d.InputTokens, d.OutputTokens, jobID); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET spent_usd=spent_usd+? WHERE load_id=? AND state='applying'`,
				float64(d.USDMicros)/1e6, loadID); err != nil {
				return err
			}
			// A fill is no one's start: its spend counts against its set's
			// month allowance, as a sync's does.
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_document_set_spend (set_id, month, workspace_id, usd_micros)
SELECT m.set_id, ?, ?, ? FROM foundry_load_documents m WHERE m.load_id=? AND m.document_id=?
ON DUPLICATE KEY UPDATE usd_micros=usd_micros+VALUES(usd_micros)`, utcMonth(time.Now()), workspaceID, d.USDMicros, loadID, d.DocumentID); err != nil {
				return err
			}
		}
		ended, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=?
WHERE load_id=? AND document_id=? AND reason=? AND requests=? AND status='owed'`,
			jobID, loadID, d.DocumentID, string(model.FoundryResendLink), d.Requests)
		if err != nil {
			return err
		}
		if n, err := ended.RowsAffected(); err != nil || n == 0 {
			return err
		}
		column := "docs_refused"
		switch d.Outcome {
		case "written":
			column = "docs_filled"
		case "nothing":
			column = "docs_nothing"
		case "native":
			column = "docs_native"
		}
		facts, held, taken := d.Facts, d.Held, d.ReviewsTaken
		if d.Outcome != "written" && d.Outcome != "nothing" {
			// A native document's commit counted what it states; a refused one
			// wrote nothing.
			facts, held, taken = 0, 0, 0
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET `+column+`=`+column+`+1, facts_added=facts_added+?,
  reviews_taken=reviews_taken+?, failures=0, last_error=NULL WHERE load_id=? AND state='applying'`,
			facts, taken, loadID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_extractions SET facts=facts+?, reviews=GREATEST(reviews+?-?, 0),
  usd_micros=usd_micros+? WHERE load_id=? AND document_id=?`, facts, held, taken, d.USDMicros, loadID, d.DocumentID); err != nil {
			return err
		}
		// Its reviews changed: those taken go from the review page.
		if facts+held+taken > 0 {
			if err := tx.oweFoundryReviewDocument(ctx, ontologyID, loadID, d.DocumentID); err != nil {
				return err
			}
		}
		return tx.endFoundryFillIfDone(ctx, loadID)
	})
}

// endFoundryFillIfDone ends load loadID's applying fill, filled, once it owes
// no document.
func (s *mysqlDB) endFoundryFillIfDone(ctx context.Context, loadID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills f SET f.state='filled', f.active_key=NULL, f.paused_until=NULL
WHERE f.load_id=? AND f.state='applying' AND NOT EXISTS (SELECT 1 FROM foundry_load_resends w
  WHERE w.load_id=f.load_id AND w.reason=? AND w.status='owed')`, loadID, string(model.FoundryResendLink))
	return err
}

// EndFoundryFillDocument ends a document a fill owes that can't be sent
// (gone, or not extracted in the load), refused for reason, while workerID
// holds job jobID's lease: counted once, as RecordFoundryFillDocument
// counts any refusal.
func (s *mysqlDB) EndFoundryFillDocument(ctx context.Context, jobID, workerID, docID string, requests int, reason string) error {
	return s.RecordFoundryFillDocument(ctx, jobID, workerID, model.FoundryFillDocument{
		DocumentID: docID, Requests: requests, Outcome: "refused", Reason: reason,
	})
}

// StopFoundryFill stops load loadID's applying fill for reason
// (model.FoundryFillCapReached), ending what it owes, while workerID holds
// job jobID's lease (ErrNotFound when it lost it).
func (s *mysqlDB) StopFoundryFill(ctx context.Context, jobID, workerID, loadID, reason string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var held string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).
			Scan(&held); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if held != loadID {
			return fmt.Errorf("job %s is of load %s, not %s", jobID, held, loadID)
		}
		return tx.stopFoundryFill(ctx, loadID, reason, jobID)
	})
}

// PauseFoundryFill pauses load loadID's applying fill until until, while
// workerID holds job jobID's lease, as when the ontology's fills spent their
// day's cap: its rows queue no job meanwhile, and the heal worker resumes
// it (ResumeFoundryFill).
func (s *mysqlDB) PauseFoundryFill(ctx context.Context, jobID, workerID, loadID string, until time.Time) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var held string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).
			Scan(&held); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if held != loadID {
			return fmt.Errorf("job %s is of load %s, not %s", jobID, held, loadID)
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET paused_until=? WHERE load_id=? AND state='applying'`, until.UTC(), loadID)
		return err
	})
}

// SumFoundryFillSpend is what ontology ontologyID's fills spent since since:
// its resend jobs' spend, as only a fill spends there (an approval, a name
// or a move replays, at no cost).
func (s *mysqlDB) SumFoundryFillSpend(ctx context.Context, ontologyID string, since time.Time) (int64, error) {
	var micros int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_load_jobs
WHERE ontology_id=? AND kind=? AND created_at >= ?`, ontologyID, string(model.FoundryGraphJobResend), since.UTC()).Scan(&micros)
	return micros, err
}

// DueFoundryHealFills is up to limit fills the heal worker has to act on, of
// live ontologies' current loads, ready or partial, of agent bundles: a
// moved one whose documents to collect, where the load has no fill applying
// (the one with the least support first), with its suggestion as stored;
// and an applying one to resume, whose pause ended or who owes documents
// while no job is live on the ontology (a failed or ended job queued none).
func (s *mysqlDB) DueFoundryHealFills(ctx context.Context, limit int) ([]model.FoundryFillDue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryHealFillColumns+`, o.workspace_id, h.finding, f.state='applying'`+foundryHealFillFrom+`
JOIN foundry_ontologies o ON o.id=f.ontology_id AND o.current_load_id=f.load_id
JOIN foundry_loads l ON l.id=f.load_id
JOIN `+s.workspaces.Table()+` ws ON ws.id=o.workspace_id
JOIN foundry_heal_findings h ON h.ontology_id=f.ontology_id AND h.fingerprint=f.fingerprint
WHERE o.deleted_at IS NULL AND ws.deleted_at IS NULL AND ws.status NOT IN ('deleting', 'deleted') AND l.status IN ('ready', 'partial')
  AND ((f.state='moved' AND l.version_id=f.to_version_id
        AND NOT EXISTS (SELECT 1 FROM foundry_heal_fills a WHERE a.load_id=f.load_id AND a.state='applying'))
    OR (f.state='applying' AND (f.paused_until IS NULL OR f.paused_until<=NOW(6))
        AND EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=f.load_id AND w.reason=? AND w.status='owed')
        AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id=o.id AND j.active_key IS NOT NULL)))
ORDER BY f.state='applying' DESC, h.support, f.id LIMIT ?`, string(model.FoundryResendLink), limit)
	if err != nil {
		return nil, fmt.Errorf("find fills due: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var due []model.FoundryFillDue
	seen := map[string]bool{}
	for rows.Next() {
		var d model.FoundryFillDue
		var workspaceID string
		fill, err := scanFoundryHealFillWith(rows, &workspaceID, &d.Finding, &d.Resume)
		if err != nil {
			return nil, err
		}
		// One fill a load at a time: the first, by the order above.
		if seen[fill.LoadID] {
			continue
		}
		seen[fill.LoadID] = true
		d.Fill = fill
		d.Due = model.FoundryHealDue{WorkspaceID: workspaceID, OntologyID: fill.OntologyID}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range due {
		load, err := s.GetFoundryLoad(ctx, due[i].Fill.OntologyID, due[i].Fill.LoadID)
		if err != nil {
			return nil, err
		}
		due[i].Due.Load = load
	}
	return due, nil
}

// OweFoundryFillDocuments owes fill's load, under the ontology's lock, a
// fill of each of documentIDs ('link' rows), while the fill is still moved
// and its load current: owed again where a row exists, so a collect run
// again after a crash, or a fill after an earlier one, sends each. The rows
// count as owed only once the fill applies (StartFoundryFill). ok is false
// when the fill or its load changed, and nothing is owed.
func (s *mysqlDB) OweFoundryFillDocuments(ctx context.Context, fill *model.FoundryHealFill, workspaceID string, documentIDs []string) (ok bool, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, fill.OntologyID)
		if err == ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != fill.LoadID {
			return nil
		}
		var state string
		if err := tx.db.QueryRowContext(ctx, `SELECT state FROM foundry_heal_fills WHERE id=?`, fill.ID).Scan(&state); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		if state != string(model.FoundryFillMoved) {
			return nil
		}
		ok = true
		return tx.oweFoundryResends(ctx, fill.LoadID, model.FoundryResendLink, documentIDs)
	})
	return ok, err
}

// StartFoundryFill starts fill, moved, once its documents are owed: owed
// documents, left past model.FoundryFillMaxDocuments, under
// model.FoundryFillCapUSD, applying, and resend queued behind the live job.
// A fill that owes none is filled at once: no review of it is left to fill.
// Under the ontology's lock, only while the fill is still moved and its
// load current, and no other fill of the load applies (ErrConflict
// otherwise, and nothing changes).
func (s *mysqlDB) StartFoundryFill(ctx context.Context, fill *model.FoundryHealFill, workspaceID string, owed, left int64,
	resend *model.FoundryGraphJob) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, fill.OntologyID)
		if err == ErrNotFound {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != fill.LoadID {
			return ErrConflict
		}
		live, err := tx.liveFoundryGraphJob(ctx, fill.OntologyID)
		if err != nil {
			return err
		}
		state, active := model.FoundryFillApplying, any(fill.LoadID)
		if owed == 0 {
			state, active = model.FoundryFillFilled, nil
		}
		res, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state=?, active_key=?, docs_owed=?, docs_left=?, cap_usd=?,
  failures=0, last_error=NULL, paused_until=NULL WHERE id=? AND state='moved'
  AND NOT EXISTS (SELECT 1 FROM (SELECT load_id FROM foundry_heal_fills WHERE load_id=? AND state='applying') a)`,
			string(state), active, owed, left, model.FoundryFillCapUSD, fill.ID, fill.LoadID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		if state != model.FoundryFillApplying {
			return nil
		}
		_, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		return err
	})
}

// ResumeFoundryFill queues resend for fill, applying, whose pause ended or
// whose documents wait with no job live: the pause lifted, under the
// ontology's lock, while the load is current. queued reports a job queued.
func (s *mysqlDB) ResumeFoundryFill(ctx context.Context, fill *model.FoundryHealFill, workspaceID string, resend *model.FoundryGraphJob) (queued bool, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, fill.OntologyID)
		if err == ErrNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != fill.LoadID {
			return nil
		}
		live, err := tx.liveFoundryGraphJob(ctx, fill.OntologyID)
		if err != nil || live != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET paused_until=NULL
WHERE id=? AND state='applying' AND paused_until<=NOW(6)`, fill.ID); err != nil {
			return err
		}
		queued, err = tx.queueFoundryResend(ctx, o, resend, 0, true)
		return err
	})
	return queued, err
}
