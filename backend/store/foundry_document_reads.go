package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
	"unicode/utf8"
)

// foundryFileReadColumns are a read's columns under the alias r; through
// foundryDocuments' LEFT JOIN they are all NULL for a document with no read.
const foundryFileReadColumns = `r.document_id, r.mode, r.status, r.attempts, r.next_attempt_at, r.claim_token, r.claimed_until, r.job_id,
       r.page_count, r.row_count, r.usd_micros, r.text_sha256, r.artifact_key, r.error_code, r.last_error, r.requested_by_user_id,
       r.created_at, r.finished_at`

// maxFileReadErrorChars bounds last_error, a VARCHAR(1024).
const maxFileReadErrorChars = 1024

// fileReadScan scans foundryFileReadColumns, which may all be NULL.
type fileReadScan struct {
	documentID, mode, status, claimToken, jobID, textSHA256, artifactKey, errorCode, lastError, requestedBy sql.NullString
	attempts, pageCount, rowCount, usdMicros                                                                sql.NullInt64
	nextAttemptAt, claimedUntil, createdAt, finishedAt                                                      sql.NullTime
}

func (f *fileReadScan) dest() []any {
	return []any{&f.documentID, &f.mode, &f.status, &f.attempts, &f.nextAttemptAt, &f.claimToken, &f.claimedUntil, &f.jobID,
		&f.pageCount, &f.rowCount, &f.usdMicros, &f.textSHA256, &f.artifactKey, &f.errorCode, &f.lastError, &f.requestedBy,
		&f.createdAt, &f.finishedAt}
}

// read is the scanned read of a document of workspaceID; nil for none.
func (f *fileReadScan) read(workspaceID string) *model.FoundryFileRead {
	if !f.documentID.Valid {
		return nil
	}
	r := &model.FoundryFileRead{
		DocumentID: f.documentID.String, WorkspaceID: workspaceID, Mode: model.FoundryFileReadMode(f.mode.String),
		Status: model.FoundryFileReadStatus(f.status.String), Attempts: int(f.attempts.Int64), NextAttemptAt: f.nextAttemptAt.Time,
		USDMicros: f.usdMicros.Int64, CreatedAt: f.createdAt.Time,
	}
	for _, s := range []struct {
		src sql.NullString
		dst **string
	}{{f.claimToken, &r.ClaimToken}, {f.jobID, &r.JobID}, {f.textSHA256, &r.TextSHA256}, {f.artifactKey, &r.ArtifactKey},
		{f.lastError, &r.LastError}, {f.requestedBy, &r.RequestedByUserID}} {
		if s.src.Valid {
			v := s.src.String
			*s.dst = &v
		}
	}
	if f.claimedUntil.Valid {
		r.ClaimedUntil = &f.claimedUntil.Time
	}
	if f.finishedAt.Valid {
		r.FinishedAt = &f.finishedAt.Time
	}
	if f.pageCount.Valid {
		pages := int(f.pageCount.Int64)
		r.PageCount = &pages
	}
	if f.rowCount.Valid {
		r.RowCount = &f.rowCount.Int64
	}
	if f.errorCode.Valid {
		r.Error = &model.FoundryFileReadError{Code: model.FoundryFileReadErrorCode(f.errorCode.String), Message: f.lastError.String}
	}
	return r
}

// readAsText are the types that join a load's membership: text documents,
// and PDFs, read as the text their read stored. Table files never do: loads
// read them as tables.
func readAsText() []model.FoundryMediaType {
	return model.FoundryMediaTypesRead(model.FoundryReadText, model.FoundryReadPDF)
}

// pdfReadEnded keeps, of documents joined with their standard read (r),
// every one but a PDF (model.FoundryReadPDF, application/pdf alone) whose
// read has not ended. A load sends only these: a PDF still being read is
// neither sent nor unrecorded, so it requeues nothing, until its read's end
// wakes the loads that hold it (wakeFoundryDocument); one whose read failed
// is sent, to be recorded failed.
const pdfReadEnded = ` (d.media_type<>'` + string(model.FoundryMediaPDF) + `' OR r.status IN ('succeeded', 'failed'))`

// pdfReadSucceeded keeps every one but a PDF whose read has not succeeded:
// those whose text is there to read (model.FoundryDocument.TextKey).
const pdfReadSucceeded = ` (d.media_type<>'` + string(model.FoundryMediaPDF) + `' OR r.status='succeeded')`

// queueFoundryFileRead queues the standard read of document id when it is a
// table file or a PDF, which the document worker reads once; a read already
// there stays as it is.
func (s *mysqlDB) queueFoundryFileRead(ctx context.Context, id string) error {
	types := model.FoundryMediaTypesRead(model.FoundryReadTable, model.FoundryReadPDF)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status)
SELECT id, 'standard', workspace_id, 'queued' FROM foundry_documents WHERE id=? AND media_type IN (`+idPlaceholders(len(types))+`)
ON DUPLICATE KEY UPDATE document_id=foundry_document_reads.document_id`, append([]any{id}, mediaTypeArgs(types)...)...)
	return err
}

// ClaimFoundryFileRead claims the read due first of an uploaded document of
// one of mediaTypes, for lease: a queued read whose time has come, or a
// running one whose claim lapsed. Each claim counts an attempt and holds the
// read under token, a new one per claim, which only its holder can renew or
// end it with. A lapsed read whose claims lapsed
// model.MaxFoundryFileReadLapses times in a row is failed as unsupported
// instead (the file keeps killing the worker), and the next is claimed. A
// failed PDF wakes the loads holding it (wake, as FinishFoundryFileRead) in
// the transaction that fails it, so a worker that stops in between leaves
// the read lapsed, for the next claim to fail and wake again. nil means none
// is due.
func (s *mysqlDB) ClaimFoundryFileRead(ctx context.Context, mediaTypes []model.FoundryMediaType, token string, lease time.Duration,
	wake model.FoundryFileReadWake) (*model.FoundryFileReadClaim, error) {
	if s.root == nil || wake.NewJobID == nil {
		return nil, fmt.Errorf("foundry file read claim requires a root database connection and a whole wake")
	}
	if len(mediaTypes) == 0 {
		return nil, nil
	}
	for {
		claim, spent, err := s.claimFoundryFileRead(ctx, mediaTypes, token, lease)
		if err != nil || spent == nil {
			return claim, err
		}
		if err := s.failLapsedFoundryFileRead(ctx, spent, wake); err != nil {
			return nil, err
		}
	}
}

// lapsedFileRead is a running read whose claims lapsed
// model.MaxFoundryFileReadLapses times in a row, and its document.
type lapsedFileRead struct {
	document *model.FoundryDocument
	mode     string
	attempts int
}

// claimFoundryFileRead claims one read in one transaction; spent is a read
// it found lapsed too often to claim again, which it leaves as it is for
// failLapsedFoundryFileRead.
func (s *mysqlDB) claimFoundryFileRead(ctx context.Context, mediaTypes []model.FoundryMediaType, token string, lease time.Duration) (
	_ *model.FoundryFileReadClaim, spent *lapsedFileRead, err error) {
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin foundry file read claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// The locking clause locks only the outer query's rows: Vitess cannot
	// parse FOR UPDATE OF, so the documents are read in a subquery. A running
	// read keeps the next_attempt_at it was due at, so both kinds fall in the
	// index's range.
	var documentID, mode, status string
	var attempts int
	err = tx.QueryRowContext(ctx, `
SELECT r.document_id, r.mode, r.status, r.attempts FROM foundry_document_reads r
WHERE r.status IN ('queued', 'running') AND r.next_attempt_at <= NOW(6) AND (r.status='queued' OR r.claimed_until < NOW(6))
  AND EXISTS (SELECT 1 FROM foundry_documents d WHERE d.id=r.document_id AND d.status='uploaded'
              AND d.media_type IN (`+idPlaceholders(len(mediaTypes))+`))
ORDER BY r.next_attempt_at, r.document_id
LIMIT 1 FOR UPDATE SKIP LOCKED`, mediaTypeArgs(mediaTypes)...).Scan(&documentID, &mode, &status, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("select foundry file read claim: %w", err)
	}
	doc, err := scanFoundryDocument(tx.QueryRowContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+` WHERE d.id=?`, documentID))
	if err != nil {
		return nil, nil, fmt.Errorf("read claimed foundry document: %w", err)
	}
	if model.FoundryFileReadStatus(status) == model.FoundryFileReadRunning && attempts >= model.MaxFoundryFileReadLapses {
		_ = tx.Rollback()
		return nil, &lapsedFileRead{document: doc, mode: mode, attempts: attempts}, nil
	}
	if _, err = tx.ExecContext(ctx, `
UPDATE foundry_document_reads
SET status='running', attempts=attempts+1, claim_token=?, claimed_until=NOW(6) + INTERVAL ? MICROSECOND
WHERE document_id=? AND mode=?`, token, lease.Microseconds(), documentID, mode); err != nil {
		return nil, nil, fmt.Errorf("claim foundry file read: %w", err)
	}
	txDB := &mysqlDB{db: tx, workspaces: s.workspaces}
	read, err := txDB.foundryFileRead(ctx, doc.WorkspaceID, documentID, model.FoundryFileReadMode(mode))
	if err != nil {
		return nil, nil, err
	}
	doc.Read = read
	if err = tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit foundry file read claim: %w", err)
	}
	return &model.FoundryFileReadClaim{Read: read, Document: doc}, nil, nil
}

// failLapsedFoundryFileRead fails spent as unsupported and, for a PDF,
// wakes the loads holding it, in one transaction that locks in
// FinishFoundryFileRead's order: the workspace's ontologies, the document,
// then the read. A read no longer lapsed (another worker failed it, or its
// holder renewed it) or whose document is no longer uploaded is left alone.
func (s *mysqlDB) failLapsedFoundryFileRead(ctx context.Context, spent *lapsedFileRead, wake model.FoundryFileReadWake) error {
	doc := spent.document
	pdf := doc.MediaType.ReadKind() == model.FoundryReadPDF
	message := fmt.Sprintf("Helix could not finish reading this file in %d tries: it stopped the reader or ran past its time; "+
		"check the file and upload it again", spent.attempts)
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var ontologies []*model.FoundryOntology
		if pdf {
			var err error
			if ontologies, err = tx.lockLiveFoundryOntologies(ctx, doc.WorkspaceID); err != nil {
				return err
			}
		}
		var status string
		var uploader sql.NullString
		err := tx.db.QueryRowContext(ctx, `SELECT status, created_by_user_id FROM foundry_documents WHERE id=? FOR SHARE`, doc.ID).
			Scan(&status, &uploader)
		if errors.Is(err, sql.ErrNoRows) || err == nil && model.FoundryDocumentStatus(status) != model.FoundryDocumentUploaded {
			return nil
		}
		if err != nil {
			return err
		}
		res, err := tx.db.ExecContext(ctx, `
UPDATE foundry_document_reads
SET status='failed', error_code=?, last_error=?, claim_token=NULL, claimed_until=NULL, finished_at=NOW(6)
WHERE document_id=? AND mode=? AND status='running' AND claimed_until < NOW(6) AND attempts >= ?`,
			string(model.FoundryFileReadUnsupported), message, doc.ID, spent.mode, model.MaxFoundryFileReadLapses)
		if err != nil {
			return err
		}
		failed, err := res.RowsAffected()
		if err != nil || failed == 0 || !pdf {
			return err
		}
		return tx.wakeFoundryDocument(ctx, ontologies, doc.ID, stringPtr(uploader), wake)
	})
	if err != nil {
		return fmt.Errorf("fail lapsed foundry file read: %w", err)
	}
	return nil
}

// foundryFileRead reads one read of a document of workspaceID.
func (s *mysqlDB) foundryFileRead(ctx context.Context, workspaceID, documentID string, mode model.FoundryFileReadMode) (*model.FoundryFileRead, error) {
	var scan fileReadScan
	err := s.db.QueryRowContext(ctx, "SELECT "+foundryFileReadColumns+` FROM foundry_document_reads r WHERE r.document_id=? AND r.mode=?`,
		documentID, string(mode)).Scan(scan.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read foundry file read: %w", err)
	}
	return scan.read(workspaceID), nil
}

// RenewFoundryFileReadLease extends a running read's claim; ErrNotFound
// means token no longer holds it.
func (s *mysqlDB) RenewFoundryFileReadLease(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `
UPDATE foundry_document_reads SET claimed_until=NOW(6) + INTERVAL ? MICROSECOND
WHERE document_id=? AND mode=? AND status='running' AND claim_token=?`, lease.Microseconds(), documentID, string(mode), token)
}

// FinishFoundryFileRead ends token's claim of a read with outcome: read, for
// good; unreadable, for good; back in the queue after a failure that was not
// the file's (the claim ended in order, so attempts starts again); or given
// back as it was. ErrNotFound means token no longer holds it: its claim
// lapsed and another took it, or its document was deleted.
//
// A PDF's read that ends, read or unreadable, wakes the loads that hold its
// document in the same transaction (wakeFoundryDocument, handed wake): they
// waited for it. That transaction locks the workspace's live ontologies,
// then the document, then the read, the order an upload and a delete take
// them in.
func (s *mysqlDB) FinishFoundryFileRead(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string,
	outcome model.FoundryFileReadOutcome, wake model.FoundryFileReadWake) error {
	const claimed = ` WHERE document_id=? AND mode=? AND status='running' AND claim_token=?`
	const ended = `claim_token=NULL, claimed_until=NULL`
	key := []any{documentID, string(mode), token}
	var query string
	var args []any
	ends := true
	switch o := outcome.(type) {
	case model.FoundryTableRead:
		if o.ArtifactKey == "" || o.Rows < 0 {
			return fmt.Errorf("a table read needs its artifact and a row count; got %q and %d", o.ArtifactKey, o.Rows)
		}
		query = `UPDATE foundry_document_reads
SET status='succeeded', row_count=?, artifact_key=?, last_error=NULL, finished_at=NOW(6), ` + ended + claimed
		args = append([]any{o.Rows, o.ArtifactKey}, key...)
	case model.FoundryPDFRead:
		if o.ArtifactKey == "" || o.Pages < 1 || len(o.TextSHA256) != 64 {
			return fmt.Errorf("a PDF read needs its artifact, its pages and its text's SHA-256; got %q, %d and %q", o.ArtifactKey, o.Pages, o.TextSHA256)
		}
		query = `UPDATE foundry_document_reads
SET status='succeeded', page_count=?, text_sha256=?, artifact_key=?, last_error=NULL, finished_at=NOW(6), ` + ended + claimed
		args = append([]any{o.Pages, o.TextSHA256, o.ArtifactKey}, key...)
	case model.FoundryFileUnreadable:
		if o.Error.Code != model.FoundryFileReadTooLarge && o.Error.Code != model.FoundryFileReadUnsupported {
			return fmt.Errorf("a read fails as too_large or unsupported, not %q", o.Error.Code)
		}
		query = `UPDATE foundry_document_reads
SET status='failed', error_code=?, last_error=?, finished_at=NOW(6), ` + ended + claimed
		args = append([]any{string(o.Error.Code), clipChars(o.Error.Message, maxFileReadErrorChars)}, key...)
	case model.FoundryFileReadRetry:
		ends = false
		query = `UPDATE foundry_document_reads
SET status='queued', attempts=0, next_attempt_at=?, last_error=?, ` + ended + claimed
		args = append([]any{o.At.UTC(), clipChars(o.Message, maxFileReadErrorChars)}, key...)
	case model.FoundryFileReadReleased:
		ends = false
		query = `UPDATE foundry_document_reads
SET status='queued', attempts=GREATEST(attempts-1, 0), next_attempt_at=NOW(6), ` + ended + claimed
		args = key
	default:
		return fmt.Errorf("unknown foundry file read outcome %T", outcome)
	}
	if !ends {
		return execOrNotFound(ctx, s.db, query, args...)
	}
	var workspaceID string
	var mediaType model.FoundryMediaType
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id, media_type FROM foundry_documents WHERE id=?`, documentID).Scan(&workspaceID, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if mediaType.ReadKind() != model.FoundryReadPDF {
		return execOrNotFound(ctx, s.db, query, args...)
	}
	if s.root == nil || wake.NewJobID == nil {
		return fmt.Errorf("finishing a PDF's read requires a root database connection and a whole wake")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, workspaceID)
		if err != nil {
			return err
		}
		var status string
		var uploader sql.NullString
		err = tx.db.QueryRowContext(ctx, `SELECT status, created_by_user_id FROM foundry_documents WHERE id=? FOR SHARE`, documentID).
			Scan(&status, &uploader)
		if errors.Is(err, sql.ErrNoRows) || err == nil && model.FoundryDocumentStatus(status) != model.FoundryDocumentUploaded {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := execOrNotFound(ctx, tx.db, query, args...); err != nil {
			return err
		}
		return tx.wakeFoundryDocument(ctx, ontologies, documentID, stringPtr(uploader), wake)
	})
}

// wakeFoundryDocument has every ontology of ontologies, the workspace's live
// ones, whose load holds docID read it, now that its read has ended. As an
// upload's completion does (fanOutFoundryDocuments): with a job live, the
// job's end owes an extract (the job may be past listing its documents);
// otherwise an extract of the current load is queued (wake.NewJobID) when
// it waits for documents and the month has budget for uploader, the
// person who uploaded it (nil for none), whom the extract is requested by,
// as the upload's own would have been (model.FoundryGraphBudget.CapFor). A
// load holds the document when it joined its membership, which an upload
// into a set the load reads, or the load's own start, makes; an ontology
// whose loads don't hold it reads it at its next load. The caller holds
// the ontologies' locks.
func (s *mysqlDB) wakeFoundryDocument(ctx context.Context, ontologies []*model.FoundryOntology, docID string, uploader *string,
	wake model.FoundryFileReadWake) error {
	for _, o := range ontologies {
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		var loads []string
		if live != nil && live.Kind != model.FoundryGraphJobDelete {
			loads = append(loads, live.LoadID)
		}
		if o.CurrentLoadID != nil {
			loads = append(loads, *o.CurrentLoadID)
		}
		holds := false
		for _, l := range loads {
			if holds, err = s.FoundryLoadHoldsDocument(ctx, l, docID); err != nil || holds {
				break
			}
		}
		switch {
		case err != nil:
			return err
		case !holds:
			continue
		case live != nil:
			if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET extract_owed=TRUE WHERE id=?`, o.ID); err != nil {
				return err
			}
			continue
		}
		extract := &model.FoundryGraphJob{ID: wake.NewJobID(), RequestedByUserID: uploader}
		if _, err := s.queueFoundryExtract(ctx, o, extract, wake.Budget); err != nil {
			return err
		}
	}
	return nil
}

// clipChars cuts s to at most n characters.
func clipChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ListReadFoundryTableDocuments returns the workspace's uploaded table files
// in the given sets whose read succeeded, oldest first: what a generation
// drafts from beside the synced tables.
func (s *mysqlDB) ListReadFoundryTableDocuments(ctx context.Context, workspaceID string, setIDs []string) ([]*model.FoundryDocument, error) {
	if len(setIDs) == 0 {
		return nil, nil
	}
	tables := model.FoundryMediaTypesRead(model.FoundryReadTable)
	args := append(append([]any{workspaceID}, toAnySlice(setIDs)...), mediaTypeArgs(tables)...)
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.workspace_id=? AND d.status='uploaded' AND d.set_id IN (`+idPlaceholders(len(setIDs))+`)
  AND d.media_type IN (`+idPlaceholders(len(tables))+`) AND r.status='succeeded' AND`+foundryDocumentOwnerLive+`
ORDER BY d.created_at, d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFoundryDocuments(rows)
}

// CountReadableFoundryTextDocuments counts the workspace's uploaded documents
// in the given sets that a generation stages as text: text documents, and
// PDFs whose read succeeded, as ListUploadedFoundryDocuments lists them. The
// two must agree, or a generation would be accepted with nothing to stage.
func (s *mysqlDB) CountReadableFoundryTextDocuments(ctx context.Context, workspaceID string, setIDs []string) (int, error) {
	if len(setIDs) == 0 {
		return 0, nil
	}
	text := readAsText()
	args := append(append([]any{workspaceID}, toAnySlice(setIDs)...), mediaTypeArgs(text)...)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+foundryDocuments+`
WHERE d.workspace_id=? AND d.status='uploaded' AND d.set_id IN (`+idPlaceholders(len(setIDs))+`)
  AND d.media_type IN (`+idPlaceholders(len(text))+`) AND`+pdfReadSucceeded+` AND`+foundryDocumentOwnerLive, args...).Scan(&n)
	return n, err
}

// ListFoundryDocumentsByID returns the workspace's documents with the given
// ids, deleted or not, by id; ids it has no document for are absent.
func (s *mysqlDB) ListFoundryDocumentsByID(ctx context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error) {
	out := map[string]*model.FoundryDocument{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.workspace_id=? AND d.id IN (`+idPlaceholders(len(ids))+`) AND`+foundryDocumentOwnerLive,
		append([]any{workspaceID}, toAnySlice(ids)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs, err := scanFoundryDocuments(rows)
	for _, d := range docs {
		out[d.ID] = d
	}
	return out, err
}
