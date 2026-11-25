package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// foundryResendLive is the SQL condition that request w (a foundry_load_resends
// row) is not a fill's, or is one whose load's fill applies and isn't
// paused: a paused or stopped fill's rows queue no job and are sent by none.
// Nor is a move's request live while its move backs off after a failed job
// (failFoundryLoadMove): no job meets it before the backoff ends, when the
// heal worker sends it again (ResumeFoundryFill).
const foundryResendLive = `((w.reason<>'link' OR EXISTS (SELECT 1 FROM foundry_heal_fills lf WHERE lf.load_id=w.load_id
  AND lf.state='applying' AND (lf.paused_until IS NULL OR lf.paused_until<=NOW(6))))
  AND (w.reason<>'move' OR EXISTS (SELECT 1 FROM foundry_heal_fills mf WHERE mf.load_id=w.load_id
  AND mf.state='moving' AND (mf.paused_until IS NULL OR mf.paused_until<=NOW(6)))))`

// stopFoundryFill stops load loadID's applying fill for reason, ending the
// rows it owes; with fillID, only if that fill is the one applying. The
// caller holds the ontology's lock, or the job's lease.
func (s *mysqlDB) stopFoundryFill(ctx context.Context, loadID, fillID, reason, jobID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='stopped', stop_reason=?, active_key=NULL, paused_until=NULL
WHERE load_id=? AND state='applying' AND (?='' OR id=?)`, reason, loadID, fillID, fillID)
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
		return s.stopFoundryFill(ctx, job.LoadID, "", model.FoundryFillModelUnavailable, job.ID)
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

// FoundryFillApplying is load loadID's applying fill, nil for none, with
// its suggestion's link and spellings (Link, Spellings), read from the
// finding as stored: a fill asks that link alone and takes only reviews of
// those spellings. A finding gone, no longer open (dismissed, or superseded
// by a later audit), or naming no relationship, reads none of them, so the
// fill stops (model.FoundryFillFindingGone) before it spends more.
func (s *mysqlDB) FoundryFillApplying(ctx context.Context, loadID string) (*model.FoundryHealFill, error) {
	var finding, status sql.NullString
	fill, err := scanFoundryHealFillWith(s.db.QueryRowContext(ctx, `SELECT `+foundryHealFillColumns+`, h.finding, h.status`+foundryHealFillFrom+`
LEFT JOIN foundry_heal_findings h ON h.ontology_id=f.ontology_id AND h.fingerprint=f.fingerprint
WHERE f.load_id=? AND f.state='applying'`, loadID), &finding, &status)
	if err == ErrNotFound {
		return nil, nil
	}
	if err != nil || !finding.Valid || status.String != "open" {
		return fill, err
	}
	var read struct {
		Signal struct {
			Kind      string   `json:"kind"`
			Canonical string   `json:"canonical"`
			Spellings []string `json:"spellings"`
		} `json:"signal"`
	}
	if json.Unmarshal([]byte(finding.String), &read) == nil && read.Signal.Kind == "unmatched" {
		fill.Link, fill.Spellings = read.Signal.Canonical, read.Signal.Spellings
	}
	return fill, nil
}

// ListFoundryLinkResends is up to limit of the documents load loadID owes a
// fill (model.FoundryResendLink) while its fill applies unpaused, in
// document order: each with its document where the load holds it uploaded
// and extracted, else Unsendable (model.FoundryReleaseDocumentGone, or
// model.FoundryReleaseDocumentUnread for one it holds unextracted).
func (s *mysqlDB) ListFoundryLinkResends(ctx context.Context, loadID string, limit int) ([]*model.FoundryLoadResend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.document_id, w.requests FROM foundry_load_resends w
WHERE w.load_id=? AND w.reason=? AND w.status='owed' AND `+foundryResendLive+` ORDER BY w.document_id LIMIT ?`,
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
// document it sent to be filled for fill d.FillID, while workerID holds the
// job's lease (ErrNotFound when it lost it, and nothing is written), in one
// transaction. What it spent is the job's, that fill's, and its set's month
// allowance's, whatever else: a call made is paid. The document's request
// ends if it is still the one the job read, and only then are its numbers
// counted, on that fill while it applies (never on another that applies
// since) and on the document's row of the load (a fact more for each fact its
// fill added, a review more for each claim it held and one fewer for each
// it took), and its review cards owed a rebuild. So an event sent again, by
// a retry after one was lost, counts once: the engine says the same numbers
// each time, and a fill sent again after its transaction landed, whose
// answer this never recorded (a crash between the engine's write and this
// record), is "landed" with what that transaction took (contract 4.35.0),
// counted as filled. The fill ends filled once it owes no document. An
// outcome the binary doesn't know counts as refused.
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
		if err := tx.spendFoundryFill(ctx, jobID, loadID, workspaceID, d); err != nil {
			return err
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
		case "written", "landed":
			column = "docs_filled"
		case "nothing":
			column = "docs_nothing"
		case "native":
			column = "docs_native"
		}
		facts, held, taken := d.Facts, d.Held, d.ReviewsTaken
		if d.Outcome != "written" && d.Outcome != "landed" && d.Outcome != "nothing" {
			// A native document's commit counted what it states; a refused one
			// wrote nothing.
			facts, held, taken = 0, 0, 0
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET `+column+`=`+column+`+1, facts_added=facts_added+?,
  reviews_taken=reviews_taken+?, failures=0, last_error=NULL WHERE id=? AND load_id=? AND state='applying'`,
			facts, taken, d.FillID, loadID); err != nil {
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

// spendFoundryFill adds what document d's calls spent to job jobID's row, to
// the fill the job sent it for (d.FillID, of load loadID, whatever state it
// is in now: what a job spent after its fill stopped is still that fill's,
// never another's), and to the document's set's month allowance: a fill is
// no one's start, so its spend counts there as a sync's does. The caller
// holds the job's lease.
func (s *mysqlDB) spendFoundryFill(ctx context.Context, jobID, loadID, workspaceID string, d model.FoundryFillDocument) error {
	if d.FillID == "" {
		return fmt.Errorf("document %s was sent to be filled for no fill", d.DocumentID)
	}
	if d.USDMicros <= 0 && d.InputTokens <= 0 && d.OutputTokens <= 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=usd_micros+?, input_tokens=input_tokens+?,
  output_tokens=output_tokens+? WHERE id=?`, d.USDMicros, d.InputTokens, d.OutputTokens, jobID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET spent_usd=spent_usd+? WHERE id=? AND load_id=?`,
		float64(d.USDMicros)/1e6, d.FillID, loadID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_document_set_spend (set_id, month, workspace_id, usd_micros)
SELECT m.set_id, ?, ?, ? FROM foundry_load_documents m WHERE m.load_id=? AND m.document_id=?
ON DUPLICATE KEY UPDATE usd_micros=usd_micros+VALUES(usd_micros)`, utcMonth(time.Now()), workspaceID, d.USDMicros, loadID, d.DocumentID)
	return err
}

// SpendFoundryFillDocument records what a document sent to be filled spent
// when its fill ended in no word, as when the model failed it, while
// workerID holds job jobID's lease (ErrNotFound when it lost it): the job's,
// the fill's and its set's allowance's (spendFoundryFill). Its request stays
// owed, and nothing is counted.
func (s *mysqlDB) SpendFoundryFillDocument(ctx context.Context, jobID, workerID string, d model.FoundryFillDocument) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var loadID, workspaceID string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id, workspace_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`,
			jobID, workerID).Scan(&loadID, &workspaceID); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		return tx.spendFoundryFill(ctx, jobID, loadID, workspaceID, d)
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

// EndFoundryFillDocument ends a document fill fillID owes that can't be sent
// (gone, or not extracted in the load), refused for reason, while workerID
// holds job jobID's lease: counted once, as RecordFoundryFillDocument
// counts any refusal.
func (s *mysqlDB) EndFoundryFillDocument(ctx context.Context, jobID, workerID, fillID, docID string, requests int, reason string) error {
	return s.RecordFoundryFillDocument(ctx, jobID, workerID, model.FoundryFillDocument{
		FillID: fillID, DocumentID: docID, Requests: requests, Outcome: "refused", Reason: reason,
	})
}

// StopFoundryFill stops fill fillID of load loadID for reason
// (model.FoundryFillCapReached), ending what it owes, while workerID holds
// job jobID's lease (ErrNotFound when it lost it): only while it applies, so
// a job that outlived its fill never stops the one that applies since.
func (s *mysqlDB) StopFoundryFill(ctx context.Context, jobID, workerID, loadID, fillID, reason string) error {
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
		if fillID == "" {
			return fmt.Errorf("a job stops the fill it sends, by its id")
		}
		return tx.stopFoundryFill(ctx, loadID, fillID, reason, jobID)
	})
}

// PauseFoundryFill pauses fill fillID of load loadID until until, while
// workerID holds job jobID's lease, as when the ontology's fills spent their
// day's cap, or add_in_place is off: only while it applies. Its rows queue
// no job meanwhile, and the heal worker resumes it (ResumeFoundryFill).
func (s *mysqlDB) PauseFoundryFill(ctx context.Context, jobID, workerID, loadID, fillID string, until time.Time) error {
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
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET paused_until=? WHERE id=? AND load_id=? AND state='applying'`,
			until.UTC(), fillID, loadID)
		return err
	})
}

// SumFoundryFillSpend is what ontology ontologyID's fills spent since since:
// its resend jobs' spend, as only a fill spends there (an approval, a name
// or a move replays, at no cost), of each job still under way or that
// finished since, whenever it was queued. A job's whole spend counts, some
// of it maybe from before since: a cap errs on spending less. Jobs queued
// more than foundryFillSpendQueued before since are not read, so the sum
// reads a bounded range of the ontology's jobs.
func (s *mysqlDB) SumFoundryFillSpend(ctx context.Context, ontologyID string, since time.Time) (int64, error) {
	var micros int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_load_jobs
WHERE ontology_id=? AND kind=? AND created_at >= ? AND (finished_at IS NULL OR finished_at >= ?)`, ontologyID,
		string(model.FoundryGraphJobResend), since.Add(-foundryFillSpendQueued).UTC(), since.UTC()).Scan(&micros)
	return micros, err
}

// foundryFillSpendQueued is how long before a day's window a resend job may
// have been queued and still spend in it (SumFoundryFillSpend): far longer
// than one waits behind other jobs and runs.
const foundryFillSpendQueued = 7 * 24 * time.Hour

// DueFoundryHealFills is up to limit fills the heal worker has to act on, of
// live ontologies' current loads, ready or partial, of agent bundles: a
// moved one of an open finding whose documents to collect, where the load
// has no fill applying, records the version it moved to or one a later move
// in place reached (each only adds, so the link stays: any move that gained
// links, whatever its fill came to), and no moved fill of the load is
// collected or backs off after a failed collect (paused_until: a load
// collects one fill at a time, ClaimFoundryFillCollect), the one with the
// least support first, with its suggestion as stored; an applying one to resume, whose pause ended or who owes
// documents while no job is live on the ontology (a failed or ended job
// queued none); and a moving one whose move's job failed (failFoundryLoadMove),
// once its backoff ended, with no job live, to send again (Resume). Of a
// load's moved fills, one whose collect saved a cursor comes first, so the
// documents it owed stay owed until it resumes (another's fresh collect
// would end them).
func (s *mysqlDB) DueFoundryHealFills(ctx context.Context, limit int) ([]model.FoundryFillDue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryHealFillColumns+`, o.workspace_id, h.finding, f.state='applying'`+foundryHealFillFrom+`
JOIN foundry_ontologies o ON o.id=f.ontology_id AND o.current_load_id=f.load_id
JOIN foundry_loads l ON l.id=f.load_id
JOIN `+s.workspaces.Table()+` ws ON ws.id=o.workspace_id
JOIN foundry_heal_findings h ON h.ontology_id=f.ontology_id AND h.fingerprint=f.fingerprint
WHERE o.deleted_at IS NULL AND ws.deleted_at IS NULL AND ws.status NOT IN ('deleting', 'deleted') AND l.status IN ('ready', 'partial')
  AND ((f.state='moved' AND h.status='open'
        AND NOT EXISTS (SELECT 1 FROM foundry_heal_fills p WHERE p.load_id=f.load_id AND p.state='moved' AND p.paused_until>NOW(6))
        AND (l.version_id=f.to_version_id OR EXISTS (SELECT 1 FROM foundry_heal_fills c WHERE c.load_id=f.load_id
          AND c.to_version_id=l.version_id AND c.links_added IS NOT NULL))
        AND NOT EXISTS (SELECT 1 FROM foundry_heal_fills a WHERE a.load_id=f.load_id AND a.state='applying'))
    OR (f.state='applying' AND (f.paused_until IS NULL OR f.paused_until<=NOW(6))
        AND EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=f.load_id AND w.reason=? AND w.status='owed')
        AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id=o.id AND j.active_key IS NOT NULL))
    OR (f.state='moving' AND f.failures>0 AND (f.paused_until IS NULL OR f.paused_until<=NOW(6))
        AND EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=f.load_id AND w.document_id='' AND w.reason=? AND w.status='owed')
        AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.ontology_id=o.id AND j.active_key IS NOT NULL)))
ORDER BY f.state IN ('applying', 'moving') DESC, f.collect_after IS NOT NULL DESC, h.support, f.id LIMIT ?`, string(model.FoundryResendLink), string(model.FoundryResendMove), limit)
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
		if fill != nil && fill.State == model.FoundryFillMoving {
			d.Resume = true
		}
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

// RestartFoundryFillCollect starts fill's collect over, under the
// ontology's lock, while the fill is still moved, its collect's lease the one
// this collect claimed (fill.PausedUntil, ClaimFoundryFillCollect), its
// finding open, and its load current: it ends every 'link' row the load
// still owes, which only a collect that stopped partway leaves (no fill
// applies while one collects), and clears every cursor of the load's fills,
// whose rows those were. So a fill sends only the documents its own collect
// listed. ok is false when the fill, its lease, its finding or its load
// changed, and nothing changes.
func (s *mysqlDB) RestartFoundryFillCollect(ctx context.Context, fill *model.FoundryHealFill, workspaceID string) (ok bool, err error) {
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if ok, err = tx.collectsFoundryFill(ctx, fill, workspaceID); err != nil || !ok {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends w SET w.status='done', w.ended_at=NOW(6)
WHERE w.load_id=? AND w.reason=? AND w.status='owed'
  AND NOT EXISTS (SELECT 1 FROM foundry_heal_fills a WHERE a.load_id=w.load_id AND a.state='applying')`,
			fill.LoadID, string(model.FoundryResendLink)); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET `+clearFoundryFillCursor+` WHERE load_id=?`, fill.LoadID)
		return err
	})
	if err == nil && ok {
		fill.Collect = nil
	}
	return ok, err
}

// OweFoundryFillDocuments owes fill's load a fill of each of documentIDs
// ('link' rows), one page of its collect, and saves at as how far the
// collect got, in one transaction, so the rows owed are always what the
// cursor counts and a collect cut off after it resumes past the page: under
// the ontology's lock, while the fill is still moved, its collect's lease
// the one this collect claimed, its finding open, and its load current. A
// row that exists is owed again, so a fill after an earlier one sends each.
// The rows count as owed only once the fill applies (StartFoundryFill). A
// page saved is progress: the fill's collect failures start again from
// none. ok is false when the fill, its lease, its finding or its load
// changed, and nothing is owed or saved.
func (s *mysqlDB) OweFoundryFillDocuments(ctx context.Context, fill *model.FoundryHealFill, workspaceID string, documentIDs []string,
	at model.FoundryFillCursor) (ok bool, err error) {
	if at.After == "" || at.VersionID == "" || at.Owed < 0 || at.Left < 0 {
		return false, fmt.Errorf("a collect's cursor names its last document and version, and counts no less than nothing: %+v", at)
	}
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if ok, err = tx.collectsFoundryFill(ctx, fill, workspaceID); err != nil || !ok {
			return err
		}
		if len(documentIDs) > 0 {
			if err := tx.oweFoundryResends(ctx, fill.LoadID, model.FoundryResendLink, documentIDs); err != nil {
				return err
			}
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET collect_after=?, collect_version_id=?, collect_owed=?, collect_left=?,
  failures=0, last_error=NULL WHERE id=? AND state='moved'`, at.After, at.VersionID, at.Owed, at.Left, fill.ID)
		return err
	})
	if err == nil && ok {
		fill.Collect, fill.Failures = &at, 0
	}
	return ok, err
}

// collectsFoundryFill locks fill's ontology (live, in workspaceID) and
// reports whether its collect may write: the load still current, and the
// fill still moved under this collect's lease with its finding open.
func (s *mysqlDB) collectsFoundryFill(ctx context.Context, fill *model.FoundryHealFill, workspaceID string) (bool, error) {
	o, err := s.lockLiveFoundryOntology(ctx, workspaceID, fill.OntologyID)
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if o.CurrentLoadID == nil || *o.CurrentLoadID != fill.LoadID {
		return false, nil
	}
	return s.holdsFoundryFillCollect(ctx, fill)
}

// clearFoundryFillCursor is the SET of a fill row with no collect cursor.
const clearFoundryFillCursor = `collect_after=NULL, collect_version_id=NULL, collect_owed=0, collect_left=0`

// StartFoundryFill starts fill, moved, once its documents are owed: owed
// documents, left past model.FoundryFillMaxDocuments, under
// model.FoundryFillCapUSD, applying, its collect's cursor cleared, and
// resend queued behind the live job.
// A fill that owes none is filled at once: no review of it is left to fill.
// Under the ontology's lock, only while the fill is still moved, its
// collect's lease this collect's, its load current, the load owes exactly
// owed 'link' rows (what this collect owed, no more and no fewer), and no
// other fill of the load applies (ErrConflict otherwise, and nothing
// changes).
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
		if held, err := tx.holdsFoundryFillCollect(ctx, fill); err != nil || !held {
			if err == nil {
				err = ErrConflict
			}
			return err
		}
		var links int64
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason=? AND status='owed'`,
			fill.LoadID, string(model.FoundryResendLink)).Scan(&links); err != nil {
			return err
		}
		if links != owed {
			return fmt.Errorf("%w: the load owes %d documents to fill, its collect %d", ErrConflict, links, owed)
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
  failures=0, last_error=NULL, paused_until=NULL, `+clearFoundryFillCursor+` WHERE id=? AND state='moved'
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
// whose documents wait with no job live, or moving, whose move's job failed
// and whose backoff ended (the move is sent again): the pause lifted, under
// the ontology's lock, while the load is current. queued reports a job
// queued.
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
WHERE id=? AND state IN ('applying', 'moving') AND paused_until<=NOW(6)`, fill.ID); err != nil {
			return err
		}
		queued, err = tx.queueFoundryResend(ctx, o, resend, 0, true)
		return err
	})
	return queued, err
}

// FailFoundryFillCollect counts a collect of fill, moved, that failed for
// why, saving nothing more: it backs off by model.FoundryFillBackoff
// (paused_until, which the due list reads, and which ends its lease), and
// after as many failures in a row (a page saved starts them again from
// none, OweFoundryFillDocuments), one more needs a load (why collect_failed,
// the error), so a collect that keeps failing never runs at every tick. What
// its pages owed, and its cursor, stay for the next attempt to resume,
// unless restart (a start refused as not what the load owes, so the cursor
// can't be trusted) or it needs a load: then what the load owes ends
// ('link' rows, while no fill of the load applies) and the cursor goes. The
// links its move added stay the load's: its Extracts still ask them apart
// (FoundryAskedApart). Only while the fill is still moved and its collect's
// lease this collect's.
func (s *mysqlDB) FailFoundryFillCollect(ctx context.Context, fill *model.FoundryHealFill, why string, restart bool) error {
	for len(why) > 200 || !utf8.ValidString(why) {
		why = why[:min(len(why), 200)]
		for !utf8.ValidString(why) {
			why = why[:len(why)-1]
		}
	}
	if fill.PausedUntil == nil {
		return nil
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var failures int
		err := tx.db.QueryRowContext(ctx, `SELECT failures FROM foundry_heal_fills WHERE id=? AND state='moved' AND paused_until=? FOR UPDATE`,
			fill.ID, *fill.PausedUntil).Scan(&failures)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		given := failures >= len(model.FoundryFillBackoff)
		if restart || given {
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends w SET w.status='done', w.ended_at=NOW(6)
WHERE w.load_id=? AND w.reason=? AND w.status='owed'
  AND NOT EXISTS (SELECT 1 FROM foundry_heal_fills a WHERE a.load_id=w.load_id AND a.state='applying')`,
				fill.LoadID, string(model.FoundryResendLink)); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET `+clearFoundryFillCursor+` WHERE load_id=?`, fill.LoadID); err != nil {
				return err
			}
		}
		if given {
			given, err := json.Marshal([]model.FoundryFoldWhy{{Kind: model.FoundryWhyCollectFailed, Item: why}})
			if err != nil {
				return err
			}
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET state='needs_load', why=?, failures=failures+1,
  last_error=NULLIF(?, ''), paused_until=NULL WHERE id=? AND state='moved'`, string(given), why, fill.ID)
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET failures=failures+1, last_error=NULLIF(?, ''),
  paused_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=? AND state='moved'`, why, model.FoundryFillBackoff[failures].Microseconds(), fill.ID)
		return err
	})
}

// ClaimFoundryFillCollect claims fill's collect for a heal worker for lease:
// only while the fill is moved, not backing off, and no other moved fill of
// its load is collected or backs off (a load collects one fill at a time,
// as only one of its fills applies at a time). The lease is the fill's
// paused_until, which keeps it off the due list (DueFoundryHealFills) for
// every other worker; fill.PausedUntil holds it then, and every write of the
// collect names it (RestartFoundryFillCollect, OweFoundryFillDocuments,
// StartFoundryFill, FailFoundryFillCollect), so a collect that outlived its
// lease writes nothing. A collect that dies leaves it to run out, its cursor
// kept for the next. claimed is false when
// another holds it, or the fill changed.
func (s *mysqlDB) ClaimFoundryFillCollect(ctx context.Context, fill *model.FoundryHealFill, lease time.Duration) (claimed bool, err error) {
	until := time.Now().UTC().Add(lease).Truncate(time.Microsecond)
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills f SET f.paused_until=?
WHERE f.id=? AND f.state='moved' AND (f.paused_until IS NULL OR f.paused_until<=NOW(6))
  AND NOT EXISTS (SELECT 1 FROM (SELECT id FROM foundry_heal_fills WHERE load_id=? AND id<>? AND state='moved' AND paused_until>NOW(6)) b)`,
		until, fill.ID, fill.LoadID, fill.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	fill.PausedUntil = &until
	return true, nil
}

// ReleaseFoundryFillCollect gives up fill's collect lease
// (ClaimFoundryFillCollect) without counting a failure, as a heal worker
// that stops mid-collect does, or one whose collect saved pages before it
// ran out of time: the fill is due again at once, and its next collect
// resumes from the cursor its pages saved. Only while it holds the lease.
func (s *mysqlDB) ReleaseFoundryFillCollect(ctx context.Context, fill *model.FoundryHealFill) error {
	if fill.PausedUntil == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_heal_fills SET paused_until=NULL WHERE id=? AND state='moved' AND paused_until=?`,
		fill.ID, *fill.PausedUntil)
	return err
}

// holdsFoundryFillCollect reports whether fill is still moved under the
// collect lease this collect claimed (ClaimFoundryFillCollect), its finding
// still open: one dismissed or superseded meanwhile has its cursor cleared,
// which a collect still running must not save again.
func (s *mysqlDB) holdsFoundryFillCollect(ctx context.Context, fill *model.FoundryHealFill) (bool, error) {
	if fill.PausedUntil == nil {
		return false, nil
	}
	var held bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_heal_fills f
  JOIN foundry_heal_findings h ON h.ontology_id=f.ontology_id AND h.fingerprint=f.fingerprint AND h.status='open'
  WHERE f.id=? AND f.state='moved' AND f.paused_until=?)`, fill.ID, *fill.PausedUntil).Scan(&held)
	return held, err
}
