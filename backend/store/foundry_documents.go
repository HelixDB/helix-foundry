package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"strings"
	"time"
)

const foundryDocumentColumns = `d.id, d.workspace_id, d.set_id, d.name, d.media_type, d.size_bytes, d.sha256, d.client_token,
       d.external_id, d.source_version, d.source_at, d.source_url, d.status, d.s3_key,
       d.created_by_user_id, d.created_at, d.uploaded_at, d.deleted_at, ` + foundryFileReadColumns

// foundryDocuments is the documents table with each document's standard
// read, which foundryDocumentColumns reads.
const foundryDocuments = ` FROM foundry_documents d LEFT JOIN foundry_document_reads r ON r.document_id=d.id AND r.mode='standard'`

// CreateFoundryDocument writes d, an upload waiting for its bytes, into a
// live set of its workspace, or returns the workspace's upload d repeats
// (existing): the one with d's client token, else the oldest one with d's
// bytes that is not deleted, in whatever set it is. A connector's document
// with the same bytes is no repeat: d could never leave the connector's set.
// All of it runs under the workspace's lock, which every create takes, so a
// retry racing its first try finds it, and a drop of several files can't
// pass the cap together; the insert also holds the set's, so the set isn't
// deleted under it. ErrLimitExceeded means the workspace holds limit.Max
// uploads and d, or an abandoned upload asked for again (one created before
// limit.PendingSince, which no longer counts), would pass that. ErrNotFound
// means the workspace or the set is gone; ErrConnectorOwned means a
// connector owns the set. ErrConflict means another row holds the client
// token: one of an ontology deleted before documents belonged to the
// workspace.
func (s *mysqlDB) CreateFoundryDocument(ctx context.Context, d *model.FoundryDocument, limit model.FoundryDocumentCap) (_ *model.FoundryDocument, existing bool, err error) {
	if d.ClientToken == nil || d.Source != nil {
		return nil, false, fmt.Errorf("create foundry document: an upload has a client token and no source")
	}
	var out *model.FoundryDocument
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, d.WorkspaceID); err != nil {
			return err
		}
		// Plain reads after the lock see every create that held it before.
		found, err := scanFoundryDocument(tx.db.QueryRowContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.workspace_id=? AND d.client_token=? AND`+foundryDocumentOwnerLive, d.WorkspaceID, d.ClientToken))
		if errors.Is(err, ErrNotFound) {
			found, err = scanFoundryDocument(tx.db.QueryRowContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.workspace_id=? AND d.sha256=? AND d.status<>'deleted' AND d.external_id IS NULL AND`+foundryDocumentOwnerLive+`
ORDER BY d.created_at, d.id LIMIT 1`, d.WorkspaceID, d.SHA256))
		}
		switch {
		case err == nil:
			out, existing = found, true
			if found.Status != model.FoundryDocumentPendingUpload || !found.CreatedAt.Before(limit.PendingSince) {
				return nil
			}
			// An abandoned upload no longer counts; asking for its URL again takes a place again.
			n, err := tx.CountFoundryDocuments(ctx, d.WorkspaceID, limit.PendingSince)
			if err == nil && n >= limit.Max {
				return ErrLimitExceeded
			}
			return err
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if err := tx.lockFoundryDocumentSet(ctx, d.WorkspaceID, d.SetID); err != nil {
			return err
		}
		n, err := tx.CountFoundryDocuments(ctx, d.WorkspaceID, limit.PendingSince)
		if err != nil {
			return err
		}
		if n >= limit.Max {
			return ErrLimitExceeded
		}
		if _, err := tx.db.ExecContext(ctx, `
INSERT INTO foundry_documents (id, workspace_id, set_id, name, media_type, size_bytes, sha256, client_token, status, s3_key,
  created_by_user_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending_upload', ?, ?)`,
			d.ID, d.WorkspaceID, d.SetID, d.Name, string(d.MediaType), d.SizeBytes, d.SHA256, d.ClientToken, d.S3Key, d.CreatedByUserID); err != nil {
			if isDuplicateKey(err) {
				return ErrConflict
			}
			return err
		}
		out, err = tx.GetFoundryDocument(ctx, d.WorkspaceID, d.ID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return out, existing, nil
}

// GetFoundryDocument returns a document of the workspace, deleted or not.
func (s *mysqlDB) GetFoundryDocument(ctx context.Context, workspaceID, id string) (*model.FoundryDocument, error) {
	return scanFoundryDocument(s.db.QueryRowContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.id=? AND d.workspace_id=? AND`+foundryDocumentOwnerLive, id, workspaceID))
}

// CountFoundryDocuments counts what the workspace's document cap holds: its
// uploads, once uploaded or while waiting for their bytes since
// pendingSince. Older pending uploads were abandoned and no longer count;
// connectors' documents take no place in it.
func (s *mysqlDB) CountFoundryDocuments(ctx context.Context, workspaceID string, pendingSince time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_documents d
WHERE d.workspace_id=? AND (d.status='uploaded' OR d.status='pending_upload' AND d.created_at >= ?) AND d.external_id IS NULL AND`+
		foundryDocumentOwnerLive,
		workspaceID, pendingSince.UTC()).Scan(&n)
	return n, err
}

// CountFoundryDocumentsInSets counts the workspace's uploaded documents in
// the given sets: what a version whose scope they are reads.
func (s *mysqlDB) CountFoundryDocumentsInSets(ctx context.Context, workspaceID string, setIDs []string) (int, error) {
	if len(setIDs) == 0 {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_documents d
WHERE d.workspace_id=? AND d.status='uploaded' AND d.set_id IN (`+idPlaceholders(len(setIDs))+`) AND`+foundryDocumentOwnerLive,
		append([]any{workspaceID}, toAnySlice(setIDs)...)...).Scan(&n)
	return n, err
}

// ListFoundryDocuments returns the documents that are not deleted of one
// set when setID is not empty, else of the workspace's uploads (a
// connector's documents are listed by its sets), newest first (by
// foundryDocumentNewest), a page at a time.
func (s *mysqlDB) ListFoundryDocuments(ctx context.Context, workspaceID, setID, pageToken string, limit int) ([]*model.FoundryDocument, string, error) {
	conds := []string{"d.workspace_id=?", "d.status<>'deleted'", foundryDocumentOwnerLive}
	args := []any{workspaceID}
	if setID != "" {
		conds = append(conds, "d.set_id=?")
		args = append(args, setID)
	} else {
		conds = append(conds, "d.external_id IS NULL")
	}
	return s.listFoundryDocuments(ctx, "", conds, args, pageToken, limit)
}

// ListFoundryOntologyDocuments returns what an ontology reads, newest first
// (by foundryDocumentNewest), a page at a time: the documents of its current
// load (currentLoadID, empty for none), and the uploaded documents of scope,
// the sets the version that reads its new uploads reads. Deleted documents
// are left out. filter keeps those the current load holds as it says.
func (s *mysqlDB) ListFoundryOntologyDocuments(ctx context.Context, workspaceID, currentLoadID string, scope []string,
	filter model.FoundryDocumentFilter, pageToken string, limit int) ([]*model.FoundryDocument, string, error) {
	if !filter.Valid() {
		return nil, "", fmt.Errorf("list foundry ontology documents: unknown filter %q", filter)
	}
	read := "EXISTS (SELECT 1 FROM foundry_load_documents m WHERE m.load_id=? AND m.document_id=d.id)"
	args := []any{currentLoadID, workspaceID, currentLoadID}
	if len(scope) > 0 {
		read = "(" + read + " OR d.status='uploaded' AND d.set_id IN (" + idPlaceholders(len(scope)) + "))"
		args = append(args, toAnySlice(scope)...)
	}
	conds := []string{"d.workspace_id=?", "d.status<>'deleted'", foundryDocumentOwnerLive, read}
	switch filter {
	case model.FoundryDocumentFilterWaiting:
		// A table file is never extracted, so it never waits.
		text := readAsText()
		conds = append(conds, "d.status='uploaded' AND e.document_id IS NULL AND d.media_type IN ("+idPlaceholders(len(text))+")")
		args = append(args, mediaTypeArgs(text)...)
	case model.FoundryDocumentFilterReview:
		conds = append(conds, "e.status='extracted' AND e.reviews > 0")
	case model.FoundryDocumentFilterExtracted, model.FoundryDocumentFilterDuplicate, model.FoundryDocumentFilterFailed,
		model.FoundryDocumentFilterOutOfScope:
		conds = append(conds, "e.status=?")
		args = append(args, string(filter))
	}
	return s.listFoundryDocuments(ctx, " LEFT JOIN foundry_load_extractions e ON e.load_id=? AND e.document_id=d.id", conds, args, pageToken, limit)
}

// foundryDocumentNewest is when a document is new from: a connector's, its
// time at the source (a backfill publishes the newest first); an upload's,
// its creation.
const foundryDocumentNewest = `COALESCE(d.source_at, d.created_at)`

// listFoundryDocuments reads a page of the documents conds keep, newest
// first (foundryDocumentNewest, then id), after join (whose arguments lead
// args). The page token holds the last one's time and id.
func (s *mysqlDB) listFoundryDocuments(ctx context.Context, join string, conds []string, args []any, pageToken string, limit int) (
	[]*model.FoundryDocument, string, error) {
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "("+foundryDocumentNewest+" < ? OR ("+foundryDocumentNewest+" = ? AND d.id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+join+whereClause(conds)+
		fmt.Sprintf(" ORDER BY %s DESC, d.id DESC LIMIT %d", foundryDocumentNewest, limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out, err := scanFoundryDocuments(rows)
	if err != nil {
		return nil, "", err
	}
	if len(out) <= limit {
		return out, "", nil
	}
	last := out[limit-1]
	newest := last.CreatedAt
	if last.Source != nil {
		newest = last.Source.At
	}
	next, err := encodeTimeIDCursor(newest, last.ID)
	return out[:limit], next, err
}

// RenameFoundryDocument names an upload that is not deleted anew; the name
// is the caller's to check. ErrNotFound means the workspace has no such
// document, or it is deleted; ErrConnectorOwned means it is a connector's,
// named by its source.
func (s *mysqlDB) RenameFoundryDocument(ctx context.Context, workspaceID, id, name string) error {
	err := execOrNotFound(ctx, s.db, `UPDATE foundry_documents SET name=?
WHERE id=? AND workspace_id=? AND status<>'deleted' AND external_id IS NULL`, name, id, workspaceID)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	var owned int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_documents
WHERE id=? AND workspace_id=? AND status<>'deleted' AND external_id IS NOT NULL`, id, workspaceID).Scan(&owned); err != nil {
		return err
	}
	if owned > 0 {
		return ErrConnectorOwned
	}
	return ErrNotFound
}

// CompleteFoundryDocumentUpload marks a pending document uploaded and, in
// the same transaction, queues its read when its type is read at upload (a
// table file or a PDF), and has every ontology that reads its set read it
// (fanOutFoundryDocuments): reads holds what it did for each. ErrNotFound
// means no pending document of the workspace has this id.
func (s *mysqlDB) CompleteFoundryDocumentUpload(ctx context.Context, workspaceID, id string, fan model.FoundryDocumentFanOut) (
	_ *model.FoundryDocument, reads []model.FoundryDocumentRead, err error) {
	if s.root == nil || fan.NewJobID == nil || fan.CanWrite == nil {
		return nil, nil, fmt.Errorf("foundry document upload requires a root database connection and a whole fan-out")
	}
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, workspaceID)
		if err != nil {
			return err
		}
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_documents SET status='uploaded', uploaded_at=NOW(6)
WHERE id=? AND workspace_id=? AND status='pending_upload'`, id, workspaceID); err != nil {
			return err
		}
		if err := tx.queueFoundryFileRead(ctx, id); err != nil {
			return err
		}
		var setID string
		if err := tx.db.QueryRowContext(ctx, `SELECT set_id FROM foundry_documents WHERE id=?`, id).Scan(&setID); err != nil {
			return err
		}
		reads, err = tx.fanOutFoundryDocuments(ctx, ontologies, setID, []string{id}, fan)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	d, err := s.GetFoundryDocument(ctx, workspaceID, id)
	return d, reads, err
}

// MoveFoundryDocument puts an upload that is not deleted in another live
// set of its workspace. An uploaded one is then read as an upload into that
// set is (fanOutFoundryDocuments): reads holds what that did for each
// ontology. A load keeps what it read, with the set it read it in: the move
// applies to it from the next load. Moving a document to the set it is in
// changes nothing and reads it again nowhere it is held. ErrNotFound means
// the workspace has no such document or no such live set;
// ErrConnectorOwned means a connector owns the set, or the document is a
// connector's, which never leaves its set.
func (s *mysqlDB) MoveFoundryDocument(ctx context.Context, workspaceID, id, setID string, fan model.FoundryDocumentFanOut) (
	_ *model.FoundryDocument, reads []model.FoundryDocumentRead, err error) {
	if s.root == nil || fan.NewJobID == nil || fan.CanWrite == nil {
		return nil, nil, fmt.Errorf("foundry document move requires a root database connection and a whole fan-out")
	}
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, workspaceID)
		if err != nil {
			return err
		}
		if err := tx.lockFoundryDocumentSet(ctx, workspaceID, setID); err != nil {
			return err
		}
		var status string
		var owned bool
		if err := tx.db.QueryRowContext(ctx, `SELECT status, external_id IS NOT NULL FROM foundry_documents
WHERE id=? AND workspace_id=? AND status<>'deleted' FOR UPDATE`, id, workspaceID).Scan(&status, &owned); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if owned {
			return ErrConnectorOwned
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_documents SET set_id=? WHERE id=?`, setID, id); err != nil {
			return err
		}
		if model.FoundryDocumentStatus(status) != model.FoundryDocumentUploaded {
			// Its upload's completion reads it.
			return nil
		}
		reads, err = tx.fanOutFoundryDocuments(ctx, ontologies, setID, []string{id}, fan)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	d, err := s.GetFoundryDocument(ctx, workspaceID, id)
	return d, reads, err
}

// lockLiveFoundryOntologies locks the workspace's live ontologies, in
// created_at, id order: the order of its index, so InnoDB takes the locks in
// the order it returns the rows. Every write that changes what a document
// does in loads takes them before it touches the document, as
// CreateFoundryLoad, which locks an ontology and then reads its documents,
// does, so neither can hold what the other waits for.
func (s *mysqlDB) lockLiveFoundryOntologies(ctx context.Context, workspaceID string) ([]*model.FoundryOntology, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryOntologyColumns+`
FROM foundry_ontologies WHERE workspace_id=? AND deleted_at IS NULL ORDER BY created_at, id FOR UPDATE`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryOntology
	for rows.Next() {
		o, err := scanFoundryOntology(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// fanOutFoundryDocuments has every ontology that reads set setID read ids,
// documents the caller just made uploaded in it, under the locks of
// ontologies, the workspace's live ones, which the caller holds. An ontology
// reads the set when its live job's load (unless the job deletes one) or its
// current load is of a version whose scope holds it. For each, as for one
// ontology before documents belonged to the workspace:
//   - the documents join the load that is being written (the live job's)
//     or else the current one, if that load's scope holds the set. Only
//     they join: what the transaction does is the size of what it added,
//     not of the scope. The rest of the scope joins at the job's end or a
//     change of current load (snapshotFoundryLoadDocuments), once each;
//   - with fan.SystemOnly they never join a load a person's job is
//     writing: they join nothing, as when the live job's load doesn't read
//     the set, so that person's spend never pays for them;
//   - with a job live, the job's end owes an extract of the current load,
//     which then holds them, and which no one requests;
//   - otherwise an extract (fan.NewJobID, fan.RequestedByUserID) of the
//     current load is queued while one of them waits for it and the month
//     has budget for its requester (model.FoundryGraphBudget.CapFor).
//
// An ontology whose database the caller may not write (fan.CanWrite) is
// skipped, not owed: an owed extract with no job live would queue a write
// for them at the next job's end. One upload read by N ontologies queues N
// extracts against the one monthly budget. A table file joins no load and
// is extracted by none: loads read it as a table, through the mappings of
// the version whose generation drafted from it. A PDF whose read has not
// ended joins the load and stops there, reading: the read's end wakes the
// loads that hold it (wakeFoundryDocument). The outcome is reading when
// every document is such a PDF. A SystemOnly fan-out has no requester.
func (s *mysqlDB) fanOutFoundryDocuments(ctx context.Context, ontologies []*model.FoundryOntology, setID string, ids []string,
	fan model.FoundryDocumentFanOut) ([]model.FoundryDocumentRead, error) {
	if fan.SystemOnly && fan.RequestedByUserID != nil {
		return nil, fmt.Errorf("fan out foundry documents: work no person started has no requester")
	}
	reads := []model.FoundryDocumentRead{}
	if len(ids) == 0 {
		return reads, nil
	}
	text := readAsText()
	rows, err := s.db.QueryContext(ctx, `SELECT d.id, d.media_type, r.status`+foundryDocuments+`
WHERE d.id IN (`+idPlaceholders(len(ids))+`) AND d.set_id=? AND d.status='uploaded' AND d.media_type IN (`+idPlaceholders(len(text))+`)`,
		append(append(toAnySlice(ids), setID), mediaTypeArgs(text)...)...)
	if err != nil {
		return nil, err
	}
	var joining []string
	reading := true
	for rows.Next() {
		var id string
		var mediaType model.FoundryMediaType
		var readStatus sql.NullString
		if err := rows.Scan(&id, &mediaType, &readStatus); err != nil {
			_ = rows.Close()
			return nil, err
		}
		status := model.FoundryFileReadStatus(readStatus.String)
		joining = append(joining, id)
		reading = reading && mediaType.ReadKind() == model.FoundryReadPDF && status != model.FoundryFileReadSucceeded &&
			status != model.FoundryFileReadFailed
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(joining) == 0 {
		return reads, err
	}
	for _, o := range ontologies {
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		// The loads that may read it: the one being written, and the current one.
		var readers []string
		if live != nil && live.Kind != model.FoundryGraphJobDelete {
			readers = append(readers, live.LoadID)
		}
		if o.CurrentLoadID != nil {
			readers = append(readers, *o.CurrentLoadID)
		}
		// They join the first of them, as an upload into one ontology always
		// has, when its version reads the set, unless that is a load a
		// person's job is writing and no person asked for them.
		personWrites := live != nil && live.Kind != model.FoundryGraphJobDelete && live.RequestedByUserID != nil
		readsSet, joins := false, false
		for i, l := range readers {
			_, scope, err := s.foundryLoadScope(ctx, l)
			if err != nil {
				return nil, err
			}
			if readsSet = slices.Contains(scope, setID); readsSet {
				joins = i == 0 && !(fan.SystemOnly && personWrites)
				break
			}
		}
		if !readsSet {
			continue
		}
		read := model.FoundryDocumentRead{OntologyID: o.ID}
		if !fan.CanWrite(o.ProjectID) {
			read.Outcome = model.FoundryDocumentReadSkipped
			reads = append(reads, read)
			continue
		}
		if joins {
			// Membership already there, and the set it captured, survive a move.
			if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_documents (load_id, document_id, set_id)
SELECT ?, id, set_id FROM foundry_documents WHERE id IN (`+idPlaceholders(len(joining))+`)
ON DUPLICATE KEY UPDATE document_id=foundry_load_documents.document_id`, append([]any{readers[0]}, toAnySlice(joining)...)...); err != nil {
				return nil, err
			}
		}
		if reading {
			read.Outcome = model.FoundryDocumentReadReading
			reads = append(reads, read)
			continue
		}
		if live != nil {
			if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies SET extract_owed=TRUE WHERE id=?`, o.ID); err != nil {
				return nil, err
			}
			read.Outcome = model.FoundryDocumentReadOwed
			reads = append(reads, read)
			continue
		}
		// With no job live, the set is read because the current load reads it.
		var waiting int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+foundryLoadDocuments+waitingFoundryDocumentWhere+
			" AND d.id IN ("+idPlaceholders(len(joining))+")", append([]any{*o.CurrentLoadID}, toAnySlice(joining)...)...).Scan(&waiting); err != nil {
			return nil, err
		}
		if waiting == 0 {
			read.Outcome = model.FoundryDocumentReadHeld
			reads = append(reads, read)
			continue
		}
		queued, err := s.queueFoundryExtract(ctx, o, &model.FoundryGraphJob{ID: fan.NewJobID(), RequestedByUserID: fan.RequestedByUserID}, fan.Budget)
		if err != nil {
			return nil, err
		}
		read.Outcome = model.FoundryDocumentReadNoBudget
		if queued {
			read.Outcome = model.FoundryDocumentReadQueued
		}
		reads = append(reads, read)
	}
	return reads, nil
}

// DeleteFoundryDocument marks a document of the workspace deleted and, in
// the same transaction, removes its reads: a worker reading it can no longer
// record what it read, and removes what it wrote. Each ontology whose live
// or current load holds it takes it out of the graph with an extract
// (oweFoundryRemovals), so it locks the workspace's live ontologies first,
// as every writer of documents does. ErrNotFound means it is missing or
// already deleted. It deletes a connector's document too, for the graph
// worker, whose original is gone; the service refuses a person's delete of
// one.
func (s *mysqlDB) DeleteFoundryDocument(ctx context.Context, workspaceID, id string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, workspaceID)
		if err != nil {
			return err
		}
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_documents SET status='deleted', deleted_at=NOW(6)
WHERE id=? AND workspace_id=? AND status<>'deleted'`, id, workspaceID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_document_reads WHERE document_id=?`, id); err != nil {
			return err
		}
		return tx.oweFoundryRemovals(ctx, ontologies, false)
	})
}

// ListUploadedFoundryDocuments returns at most limit of a set's uploaded
// documents that read as text, newest first (foundryDocumentNewest): those a
// generation shows the agent for the set. They are its text documents, and
// its PDFs whose read succeeded (model.FoundryDocument.TextKey); table
// files, and PDFs not read or unreadable, never take one of the limit's
// places.
func (s *mysqlDB) ListUploadedFoundryDocuments(ctx context.Context, workspaceID, setID string, limit int) ([]*model.FoundryDocument, error) {
	text := readAsText()
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryDocumentColumns+foundryDocuments+`
WHERE d.workspace_id=? AND d.set_id=? AND d.status='uploaded' AND d.media_type IN (`+idPlaceholders(len(text))+`) AND`+
		pdfReadSucceeded+` AND`+foundryDocumentOwnerLive+`
ORDER BY `+foundryDocumentNewest+` DESC, d.id DESC LIMIT ?`, append(append([]any{workspaceID, setID}, mediaTypeArgs(text)...), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFoundryDocuments(rows)
}

// mediaTypeArgs are types as query arguments.
func mediaTypeArgs(types []model.FoundryMediaType) []any {
	out := make([]any, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

// foundryLoadDocuments are a load's documents: those that joined it, each
// with the set it joined with (snapshotFoundryLoadDocuments). Sending,
// waiting and the merged requeue all read this one membership, so a document
// no row puts in a load is none of them, and can't requeue a job that will
// never send it.
const foundryLoadDocuments = foundryDocuments + ` JOIN foundry_load_documents m ON m.document_id=d.id AND m.load_id=?`

// waitingFoundryDocumentWhere keeps, of a load's documents, the uploaded ones
// it still waits for and may send: never extracted into it, or failed there
// in a way a later job may get past, and not a PDF still being read
// (pdfReadEnded). A job sends these.
const waitingFoundryDocumentWhere = ` WHERE d.status='uploaded' AND` + pdfReadEnded + `
  AND NOT EXISTS (SELECT 1 FROM foundry_load_extractions e WHERE e.load_id=m.load_id AND e.document_id=d.id
                  AND (e.status<>'failed' OR e.error_retryable=FALSE))`

// foundryLoadScope returns a load's workspace and the sets its version reads
// (foundryVersionScope).
func (s *mysqlDB) foundryLoadScope(ctx context.Context, loadID string) (workspaceID string, scope []string, err error) {
	var ontologyID, versionID string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace_id, ontology_id, version_id FROM foundry_loads WHERE id=?`, loadID).
		Scan(&workspaceID, &ontologyID, &versionID); err != nil {
		return "", nil, err
	}
	scope, err = s.GetOntologyVersionScope(ctx, ontologyID, versionID)
	return workspaceID, scope, err
}

// snapshotFoundryLoadDocuments captures uploaded documents when they join a
// load: those of the sets its version reads that loads read as text (text
// documents and PDFs; never table files, which loads read as tables). It
// reads the whole scope, so it runs once a job: at the load's start, at the
// end of a job that owes an extract, and when the load is made current.
// Callers hold the ontology's lock; existing membership, and the set it
// captured, survive retries and moves.
func (s *mysqlDB) snapshotFoundryLoadDocuments(ctx context.Context, loadID string) error {
	workspaceID, scope, err := s.foundryLoadScope(ctx, loadID)
	if err != nil || len(scope) == 0 {
		return err
	}
	text := readAsText()
	_, err = s.db.ExecContext(ctx, `INSERT INTO foundry_load_documents (load_id, document_id, set_id)
SELECT ?, id, set_id FROM foundry_documents WHERE workspace_id=? AND status='uploaded' AND set_id IN (`+idPlaceholders(len(scope))+`)
  AND media_type IN (`+idPlaceholders(len(text))+`)
ON DUPLICATE KEY UPDATE document_id=foundry_load_documents.document_id`,
		append(append([]any{loadID, workspaceID}, toAnySlice(scope)...), mediaTypeArgs(text)...)...)
	return err
}

// ListPendingFoundryDocuments returns the documents a job sends to a load:
// uploads first, oldest first, then connectors' documents, newest at the
// source first, so a backlog of them never holds back an upload and a
// budget that runs out spends on the newest. SetID is the set captured when
// the document joined this load.
func (s *mysqlDB) ListPendingFoundryDocuments(ctx context.Context, loadID string) ([]*model.FoundryDocument, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+strings.Replace(foundryDocumentColumns, "d.set_id", "m.set_id", 1)+
		foundryLoadDocuments+waitingFoundryDocumentWhere+" ORDER BY d.external_id IS NOT NULL, d.source_at DESC, d.created_at, d.id", loadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFoundryDocuments(rows)
}

// countWaitingFoundryDocuments counts the documents a load still waits for.
func (s *mysqlDB) countWaitingFoundryDocuments(ctx context.Context, loadID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+foundryLoadDocuments+waitingFoundryDocumentWhere, loadID).Scan(&n)
	return n, err
}

// countUnrecordedFoundryDocuments counts a load's uploaded documents no job
// recorded in it yet: those uploaded while a job wrote it. A retryable
// failure is recorded, so it waits for a later job rather than requeueing
// this one, and a PDF still being read is not counted: its read's end
// wakes the load (pdfReadEnded).
func (s *mysqlDB) countUnrecordedFoundryDocuments(ctx context.Context, loadID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)`+foundryLoadDocuments+` WHERE d.status='uploaded' AND`+pdfReadEnded+`
  AND NOT EXISTS (SELECT 1 FROM foundry_load_extractions e WHERE e.load_id=m.load_id AND e.document_id=d.id)`, loadID).Scan(&n)
	return n, err
}

const foundryExtractionColumns = `load_id, document_id, ontology_id, workspace_id, job_id, status, duplicate_of, error_code, error_retryable,
       sections, entities, mentions, facts, reviews, unresolved, rejected, usd_micros, input_tokens, output_tokens,
       artifact_key, finished_at, attributes, attribute_reviews, prop_conflicts, created, identifiers_unverified, attributes_over_entries, resolved_user, resolved_adjudicated, resolved_identifier, resolved_name, resolved_alias, resolved_created`

// GetFoundryDocumentExtraction returns what extracting a document into a
// load made of it.
func (s *mysqlDB) GetFoundryDocumentExtraction(ctx context.Context, loadID, documentID string) (*model.FoundryDocumentExtraction, error) {
	return scanFoundryExtraction(s.db.QueryRowContext(ctx, "SELECT "+foundryExtractionColumns+`
FROM foundry_load_extractions WHERE load_id=? AND document_id=?`, loadID, documentID))
}

// ListFoundryDocumentExtractions returns the extractions of the given
// documents into a load, by document id; documents not extracted into it are
// absent.
func (s *mysqlDB) ListFoundryDocumentExtractions(ctx context.Context, loadID string, documentIDs []string) (map[string]*model.FoundryDocumentExtraction, error) {
	out := map[string]*model.FoundryDocumentExtraction{}
	if len(documentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryExtractionColumns+`
FROM foundry_load_extractions WHERE load_id=? AND document_id IN (`+idPlaceholders(len(documentIDs))+`)`,
		append([]any{loadID}, toAnySlice(documentIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanFoundryExtraction(rows)
		if err != nil {
			return nil, err
		}
		out[e.DocumentID] = e
	}
	return out, rows.Err()
}

// FoundryLoadHoldsDocument reports whether a document joined a load.
func (s *mysqlDB) FoundryLoadHoldsDocument(ctx context.Context, loadID, documentID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_documents WHERE load_id=? AND document_id=?`, loadID, documentID).Scan(&n)
	return n > 0, err
}

func scanFoundryDocuments(rows *sql.Rows) ([]*model.FoundryDocument, error) {
	var out []*model.FoundryDocument
	for rows.Next() {
		d, err := scanFoundryDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanFoundryDocument(scanner rowScanner) (*model.FoundryDocument, error) {
	var d model.FoundryDocument
	var mediaType, status string
	var clientToken, externalID, sourceVersion, sourceURL, createdBy sql.NullString
	var sourceAt, uploadedAt, deletedAt sql.NullTime
	var read fileReadScan
	if err := scanner.Scan(append([]any{&d.ID, &d.WorkspaceID, &d.SetID, &d.Name, &mediaType, &d.SizeBytes, &d.SHA256, &clientToken,
		&externalID, &sourceVersion, &sourceAt, &sourceURL, &status, &d.S3Key, &createdBy, &d.CreatedAt, &uploadedAt, &deletedAt},
		read.dest()...)...); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.MediaType, d.Status = model.FoundryMediaType(mediaType), model.FoundryDocumentStatus(status)
	d.Read = read.read(d.WorkspaceID)
	// foundry_documents_source_chk: a connector's document has every source
	// field but its link, and an upload has its token and none of them.
	if externalID.Valid {
		d.Source = &model.FoundryDocumentSource{ExternalID: externalID.String, Version: sourceVersion.String, At: sourceAt.Time}
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{clientToken, &d.ClientToken}, {createdBy, &d.CreatedByUserID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	if d.Source != nil && sourceURL.Valid {
		d.Source.URL = &sourceURL.String
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{uploadedAt, &d.UploadedAt}, {deletedAt, &d.DeletedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &d, nil
}

func scanFoundryExtraction(scanner rowScanner) (*model.FoundryDocumentExtraction, error) {
	var e model.FoundryDocumentExtraction
	var status string
	var duplicateOf, errorCode, artifactKey sql.NullString
	var retryable sql.NullBool
	c := &e.Counts
	if err := scanner.Scan(&e.LoadID, &e.DocumentID, &e.OntologyID, &e.WorkspaceID, &e.JobID, &status, &duplicateOf, &errorCode, &retryable,
		&c.Sections, &c.Entities, &c.Mentions, &c.Facts, &c.Reviews, &c.Unresolved, &c.Rejected,
		&e.USDMicros, &e.InputTokens, &e.OutputTokens, &artifactKey, &e.FinishedAt, &c.Attributes, &c.AttributeReviews, &c.PropConflicts, &c.Created, &c.IdentifiersUnverified, &c.AttributesOverEntries, &c.ResolvedUser, &c.ResolvedAdjudicated, &c.ResolvedIdentifier, &c.ResolvedName, &c.ResolvedAlias, &c.ResolvedCreated); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.Status = model.FoundryExtractionStatus(status)
	if duplicateOf.Valid {
		e.DuplicateOf = &duplicateOf.String
	}
	if errorCode.Valid {
		e.Error = &model.FoundryExtractionError{Code: model.FoundryExtractionErrorCode(errorCode.String), Retryable: retryable.Bool}
	}
	if artifactKey.Valid {
		e.ArtifactKey = &artifactKey.String
	}
	return &e, nil
}
