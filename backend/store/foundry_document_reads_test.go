package store_test

import (
	"context"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tableTypes are the table files the document worker claims; pdfTypes its
// PDFs.
var (
	tableTypes = model.FoundryMediaTypesRead(model.FoundryReadTable)
	pdfTypes   = model.FoundryMediaTypesRead(model.FoundryReadPDF)
)

// wake is what the document worker hands a read: extract ids of its own, no
// monthly limit.
var wake = model.FoundryFileReadWake{NewJobID: id.New, Budget: noBudget}

// uploadedBy writes a PDF of the ontology's set that user uploaded, whose
// bytes are in.
func (f foundryFixture) uploadedBy(t *testing.T, tag, user string) string {
	t.Helper()
	ctx := context.Background()
	d := foundryDocument(f.ontology, tag)
	d.MediaType, d.CreatedByUserID = model.FoundryMediaPDF, &user
	_, _, err := f.db.CreateFoundryDocument(ctx, d, noCap)
	require.NoError(t, err)
	_, _, err = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, d.ID, f.fan(tag))
	require.NoError(t, err)
	return d.ID
}

// lapse makes a running read's claim run out, as a killed worker leaves it.
func lapse(t *testing.T, documentID string) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_document_reads SET claimed_until=NOW(6) - INTERVAL 1 SECOND WHERE document_id=?`, documentID)
	require.NoError(t, err)
}

func TestFoundryFileReads(t *testing.T) {
	ctx := context.Background()

	// Completing the upload of a table file or a PDF queues its read in the
	// same transaction, so a file is never uploaded without one; text is
	// extracted as it is and gets none.
	t.Run("queues the read of a table file or PDF when its upload completes", func(t *testing.T) {
		f := newFoundryFixture(t, "FR1")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		md := f.uploaded(t, "md")
		for _, id := range []string{csv, pdf} {
			d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, id)
			require.NoError(t, err)
			require.NotNil(t, d.Read, "%s has a read", id)
			assert.Equal(t, model.FoundryFileReadQueued, d.Read.Status)
			assert.Equal(t, model.FoundryFileReadStandard, d.Read.Mode)
			assert.Equal(t, f.ontology.WorkspaceID, d.Read.WorkspaceID)
			assert.False(t, d.Read.Retrying())
		}
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, md)
		require.NoError(t, err)
		assert.Nil(t, d.Read)
		assert.Equal(t, model.FoundryFileReadReady, d.ReadState())
	})

	// The worker claims a due table file with a fresh token, and records
	// what it read; the read never changes after.
	t.Run("claims a table file and records its rows", func(t *testing.T) {
		f := newFoundryFixture(t, "FR2")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, claim)
		assert.Equal(t, csv, claim.Document.ID)
		assert.Equal(t, model.FoundryMediaCSV, claim.Document.MediaType)
		assert.Equal(t, model.FoundryFileReadRunning, claim.Read.Status)
		assert.Equal(t, 1, claim.Read.Attempts)
		require.NotNil(t, claim.Read.ClaimToken)
		require.NotNil(t, claim.Read.ClaimedUntil)
		next, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, next, "a claimed read is not claimed twice")

		key := "ws/" + f.ontology.WorkspaceID + "/documents/" + csv + "/read/rows.jsonl.gz"
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken,
			model.FoundryTableRead{ArtifactKey: key, Rows: 42}, wake))
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, csv)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadSucceeded, d.Read.Status)
		assert.Equal(t, int64(42), *d.Read.RowCount)
		assert.Equal(t, key, *d.Read.ArtifactKey)
		assert.Nil(t, d.Read.ClaimToken)
		assert.NotNil(t, d.Read.FinishedAt)
		assert.Equal(t, model.FoundryFileReadReady, d.ReadState())
		err = f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken, model.FoundryTableRead{ArtifactKey: "other", Rows: 1}, wake)
		assert.ErrorIs(t, err, store.ErrNotFound, "a finished read is final")
	})

	// The worker claims only the types it has a reader for: asked for table
	// files, a PDF's read waits, queued.
	t.Run("claims only the types asked for", func(t *testing.T) {
		f := newFoundryFixture(t, "FR3")
		f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, claim)
		claim, err = f.db.ClaimFoundryFileRead(ctx, nil, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, claim, "no type claims nothing")
	})

	// Only the claim's holder ends it: a worker whose claim lapsed and was
	// taken over records nothing.
	t.Run("refuses a token that no longer holds the read", func(t *testing.T) {
		f := newFoundryFixture(t, "FR4")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		first, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		lapse(t, csv)
		second, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, second)
		assert.Equal(t, 2, second.Read.Attempts)
		assert.NotEqual(t, *first.Read.ClaimToken, *second.Read.ClaimToken)
		err = f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *first.Read.ClaimToken, model.FoundryTableRead{ArtifactKey: "k", Rows: 1}, wake)
		assert.ErrorIs(t, err, store.ErrNotFound)
		err = f.db.RenewFoundryFileReadLease(ctx, csv, model.FoundryFileReadStandard, *first.Read.ClaimToken, time.Minute)
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, f.db.RenewFoundryFileReadLease(ctx, csv, model.FoundryFileReadStandard, *second.Read.ClaimToken, time.Minute))
	})

	// A file that kills the worker would restart the pod forever: the third
	// lapsed claim in a row fails the read as unsupported, and the worker
	// goes on to the next.
	t.Run("fails a read whose claims lapse three times", func(t *testing.T) {
		f := newFoundryFixture(t, "FR5")
		poison := f.uploadedAs(t, "poison", model.FoundryMediaParquet)
		for range model.MaxFoundryFileReadLapses {
			claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
			require.NoError(t, err)
			require.NotNil(t, claim)
			lapse(t, poison)
		}
		next := f.uploadedAs(t, "next", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, claim)
		assert.Equal(t, next, claim.Document.ID)
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, poison)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadFailed, d.Read.Status)
		require.NotNil(t, d.Read.Error)
		assert.Equal(t, model.FoundryFileReadUnsupported, d.Read.Error.Code)
		assert.Contains(t, d.Read.Error.Message, "in 3 tries")
		assert.Equal(t, model.FoundryFileReadFailedState, d.ReadState())
	})

	// A failure that is not the file's (storage, the database) ends the
	// claim in order: the read waits until its time, says why, and starts
	// its lapse count again.
	t.Run("retries a read after a failure that is not the file's", func(t *testing.T) {
		f := newFoundryFixture(t, "FR6")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken,
			model.FoundryFileReadRetry{At: time.Now().Add(time.Hour), Message: "storage is unavailable"}, wake))
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, csv)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadQueued, d.Read.Status)
		assert.Equal(t, 0, d.Read.Attempts)
		assert.True(t, d.Read.Retrying())
		assert.Equal(t, "storage is unavailable", *d.Read.LastError)
		assert.Equal(t, model.FoundryFileReadReading, d.ReadState())
		none, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, none, "not before its time")
		_, err = testDB.Exec(`UPDATE foundry_document_reads SET next_attempt_at=NOW(6) - INTERVAL 1 SECOND WHERE document_id=?`, csv)
		require.NoError(t, err)
		again, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, again)
		assert.Equal(t, 1, again.Read.Attempts)
	})

	// A worker that stops gives its read back as it was: due now, and the
	// claim not counted.
	t.Run("takes a read back from a stopping worker", func(t *testing.T) {
		f := newFoundryFixture(t, "FR7")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken, model.FoundryFileReadReleased{}, wake))
		again, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, again)
		assert.Equal(t, 1, again.Read.Attempts)
		assert.Nil(t, again.Read.LastError)
	})

	// A file that is not what its type says fails for good, with the code
	// an extraction would record and what to fix.
	t.Run("fails an unreadable file for good", func(t *testing.T) {
		f := newFoundryFixture(t, "FR8")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		bad := model.FoundryFileUnreadable{Error: model.FoundryFileReadError{Code: "model", Message: "x"}}
		assert.Error(t, f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken, bad, wake), "only file codes")
		long := string(make([]rune, 2000))
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken,
			model.FoundryFileUnreadable{Error: model.FoundryFileReadError{Code: model.FoundryFileReadTooLarge, Message: long}}, wake))
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, csv)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadFailed, d.Read.Status)
		assert.Equal(t, model.FoundryFileReadTooLarge, d.Read.Error.Code)
		assert.Len(t, []rune(d.Read.Error.Message), 1024, "the message is clipped to its column")
		none, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, none)
	})

	// The table holds only the shapes the design allows: a succeeded read
	// has its artifact, a failed one its code, and no read counts both rows
	// and pages.
	t.Run("refuses a read of the wrong shape", func(t *testing.T) {
		newTestStore(t)
		for name, insert := range map[string]string{
			"succeeded without an artifact": `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status) VALUES ('d', 'standard', 'w', 'succeeded')`,
			"failed without a code":         `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status) VALUES ('d', 'standard', 'w', 'failed')`,
			"submitted without a job":       `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status) VALUES ('d', 'standard', 'w', 'submitted')`,
			"rows and pages": `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status, row_count, page_count)
VALUES ('d', 'standard', 'w', 'queued', 1, 1)`,
			"an unknown mode":  `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status) VALUES ('d', 'quick', 'w', 'queued')`,
			"a model failure":  `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status, error_code) VALUES ('d', 'standard', 'w', 'failed', 'model')`,
			"queued with code": `INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status, error_code) VALUES ('d', 'standard', 'w', 'queued', 'unsupported')`,
		} {
			_, err := testDB.Exec(insert)
			assert.Error(t, err, name)
		}
	})

	// Deleting a document removes its reads with it: the worker reading it
	// can no longer record what it read, and is told so.
	t.Run("removes a deleted document's read", func(t *testing.T) {
		f := newFoundryFixture(t, "FR9")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, csv))
		err = f.db.FinishFoundryFileRead(ctx, csv, model.FoundryFileReadStandard, *claim.Read.ClaimToken, model.FoundryTableRead{ArtifactKey: "k", Rows: 1}, wake)
		assert.ErrorIs(t, err, store.ErrNotFound)
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, csv)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentDeleted, d.Status)
		assert.Nil(t, d.Read)
		assert.ErrorIs(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, csv), store.ErrNotFound)
	})
}

func TestFoundryTableFilesInLoads(t *testing.T) {
	ctx := context.Background()

	// A load reads table files as tables, never as text: they join no
	// load's membership, so no job sends them and none waits for them.
	t.Run("leaves table files out of a load's documents", func(t *testing.T) {
		f := newFoundryFixture(t, "TL1")
		md := f.uploaded(t, "md")
		csv := f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		load := f.load(t, "L1")
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, md, pending[0].ID)
		holds, err := f.db.FoundryLoadHoldsDocument(ctx, load, csv)
		require.NoError(t, err)
		assert.False(t, holds)
	})

	// An upload into a set a current load reads queues an extract for text,
	// and nothing for a table file, which only a revised version maps.
	t.Run("reads no table file into the current load", func(t *testing.T) {
		f := newFoundryFixture(t, "TL2")
		f.load(t, "L1")
		f.loaded(t, "w1")
		ctx := context.Background()
		doc := foundryDocument(f.ontology, "csv")
		doc.MediaType = model.FoundryMediaCSV
		_, _, err := f.db.CreateFoundryDocument(ctx, doc, noCap)
		require.NoError(t, err)
		_, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, doc.ID, f.fan("TL2"))
		require.NoError(t, err)
		assert.Empty(t, reads)
		assert.Nil(t, f.liveJob(t), "no extract is queued for a table file")
		md := foundryDocument(f.ontology, "md")
		_, _, err = f.db.CreateFoundryDocument(ctx, md, noCap)
		require.NoError(t, err)
		_, reads, err = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, md.ID, f.fan("TL2-md"))
		require.NoError(t, err)
		require.Len(t, reads, 1)
		assert.Equal(t, model.FoundryDocumentReadQueued, reads[0].Outcome)
	})

	// An ontology's documents waiting for its load are the text it will
	// extract; a table file never waits.
	t.Run("never lists a table file as waiting", func(t *testing.T) {
		f := newFoundryFixture(t, "TL3")
		md := f.uploaded(t, "md")
		f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		docs, _, err := f.db.ListFoundryOntologyDocuments(ctx, f.ontology.WorkspaceID, "", []string{generalSetID(f.ontology.ID)},
			model.FoundryDocumentFilterWaiting, "", 10)
		require.NoError(t, err)
		require.Len(t, docs, 1)
		assert.Equal(t, md, docs[0].ID)
		all, _, err := f.db.ListFoundryOntologyDocuments(ctx, f.ontology.WorkspaceID, "", []string{generalSetID(f.ontology.ID)},
			model.FoundryDocumentFilterAll, "", 10)
		require.NoError(t, err)
		assert.Len(t, all, 2, "the ontology still lists it")
	})
}

func TestFoundryGenerationSources(t *testing.T) {
	ctx := context.Background()
	// read finishes the worker's next claim as rows.
	read := func(t *testing.T, f foundryFixture) string {
		t.Helper()
		claim, err := f.db.ClaimFoundryFileRead(ctx, tableTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, claim)
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, claim.Document.ID, model.FoundryFileReadStandard, *claim.Read.ClaimToken,
			model.FoundryTableRead{ArtifactKey: "ws/x/documents/" + claim.Document.ID + "/read/rows.jsonl.gz", Rows: 2}, wake))
		return claim.Document.ID
	}

	// A generation drafts from the table files of its sets whose read
	// succeeded, in upload order; one still being read is left out.
	t.Run("lists the table files read in the sets", func(t *testing.T) {
		f := newFoundryFixture(t, "GS1")
		set := []string{generalSetID(f.ontology.ID)}
		f.uploadedAs(t, "a", model.FoundryMediaCSV)
		f.uploadedAs(t, "b", model.FoundryMediaJSON)
		f.uploadedAs(t, "c", model.FoundryMediaParquet)
		f.uploaded(t, "md")
		first, second := read(t, f), read(t, f)
		docs, err := f.db.ListReadFoundryTableDocuments(ctx, f.ontology.WorkspaceID, set)
		require.NoError(t, err)
		require.Len(t, docs, 2)
		assert.Equal(t, first, docs[0].ID)
		assert.Equal(t, second, docs[1].ID)
		assert.Equal(t, model.FoundryFileReadSucceeded, docs[0].Read.Status)
		none, err := f.db.ListReadFoundryTableDocuments(ctx, f.ontology.WorkspaceID, []string{"another-set"})
		require.NoError(t, err)
		assert.Empty(t, none)
		none, err = f.db.ListReadFoundryTableDocuments(ctx, f.ontology.WorkspaceID, nil)
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	// A set counts toward a generation's sources when it holds text the
	// generation stages: Markdown, MDX and plain text.
	t.Run("counts the text a set holds", func(t *testing.T) {
		f := newFoundryFixture(t, "GS2")
		set := []string{generalSetID(f.ontology.ID)}
		n, err := f.db.CountReadableFoundryTextDocuments(ctx, f.ontology.WorkspaceID, set)
		require.NoError(t, err)
		assert.Zero(t, n)
		f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		n, err = f.db.CountReadableFoundryTextDocuments(ctx, f.ontology.WorkspaceID, set)
		require.NoError(t, err)
		assert.Zero(t, n, "a table file, and a PDF still being read, hold no text a generation stages")
		_, err = testDB.Exec(`UPDATE foundry_document_reads SET status='failed', error_code='unsupported' WHERE document_id=?`, pdf)
		require.NoError(t, err)
		n, err = f.db.CountReadableFoundryTextDocuments(ctx, f.ontology.WorkspaceID, set)
		require.NoError(t, err)
		assert.Zero(t, n, "a PDF whose read failed has no text")
		_, err = testDB.Exec(`UPDATE foundry_document_reads SET status='succeeded', error_code=NULL, artifact_key='k' WHERE document_id=?`, pdf)
		require.NoError(t, err)
		f.uploadedAs(t, "mdx", model.FoundryMediaMDX)
		f.uploaded(t, "md")
		n, err = f.db.CountReadableFoundryTextDocuments(ctx, f.ontology.WorkspaceID, set)
		require.NoError(t, err)
		assert.Equal(t, 3, n, "Markdown, MDX and the read PDF")
	})

	// A load names the files its version maps by id, deleted or not.
	t.Run("reads documents by id", func(t *testing.T) {
		f := newFoundryFixture(t, "GS3")
		a := f.uploadedAs(t, "a", model.FoundryMediaCSV)
		b := f.uploaded(t, "b")
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, b))
		docs, err := f.db.ListFoundryDocumentsByID(ctx, f.ontology.WorkspaceID, []string{a, b, "missing"})
		require.NoError(t, err)
		require.Len(t, docs, 2)
		assert.Equal(t, model.FoundryDocumentDeleted, docs[b].Status)
		assert.NotNil(t, docs[a].Read)
		other, err := f.db.ListFoundryDocumentsByID(ctx, "another-workspace", []string{a})
		require.NoError(t, err)
		assert.Empty(t, other)
	})
}

// pdfRead is a PDF's read as the worker records it, for document docID.
func pdfRead(docID string) model.FoundryPDFRead {
	return model.FoundryPDFRead{ArtifactKey: "ws/x/documents/" + docID + "/read/standard-" + strings.Repeat("a", 64) + ".txt", Pages: 3,
		TextSHA256: strings.Repeat("a", 64)}
}

// readPDF claims the due PDF read, which must be of docID, and ends it with
// outcome.
func readPDF(t *testing.T, f foundryFixture, docID string, outcome model.FoundryFileReadOutcome) {
	t.Helper()
	ctx := context.Background()
	claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
	require.NoError(t, err)
	require.NotNil(t, claim, "a PDF read is due")
	require.Equal(t, docID, claim.Document.ID)
	require.NoError(t, f.db.FinishFoundryFileRead(ctx, docID, model.FoundryFileReadStandard, *claim.Read.ClaimToken, outcome, wake))
}

// pendingIDs are the documents a job of load would send now.
func pendingIDs(t *testing.T, f foundryFixture, load string) []string {
	t.Helper()
	pending, err := f.db.ListPendingFoundryDocuments(context.Background(), load)
	require.NoError(t, err)
	ids := []string{}
	for _, d := range pending {
		ids = append(ids, d.ID)
	}
	return ids
}

func TestFoundryPDFReads(t *testing.T) {
	ctx := context.Background()

	// A PDF's read records its pages, its text's hash and where the text
	// is, once: the document then reads as that text.
	t.Run("records a PDF's text", func(t *testing.T) {
		f := newFoundryFixture(t, "PR1")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		readPDF(t, f, pdf, pdfRead(pdf))
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, pdf)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadSucceeded, d.Read.Status)
		assert.Equal(t, 3, *d.Read.PageCount)
		assert.Nil(t, d.Read.RowCount)
		assert.Equal(t, strings.Repeat("a", 64), *d.Read.TextSHA256)
		assert.Equal(t, model.FoundryFileReadReady, d.ReadState())
		key, ok := d.TextKey()
		assert.True(t, ok)
		assert.Equal(t, pdfRead(pdf).ArtifactKey, key)
		none, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, none, "a read PDF is never read again")
	})

	// A PDF read is recorded whole or not at all.
	t.Run("refuses a PDF read without its text", func(t *testing.T) {
		f := newFoundryFixture(t, "PR2")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		for _, bad := range []model.FoundryPDFRead{
			{Pages: 1, TextSHA256: strings.Repeat("a", 64)},
			{ArtifactKey: "k", TextSHA256: strings.Repeat("a", 64)},
			{ArtifactKey: "k", Pages: 1, TextSHA256: "abc"},
		} {
			assert.Error(t, f.db.FinishFoundryFileRead(ctx, pdf, model.FoundryFileReadStandard, *claim.Read.ClaimToken, bad, wake), "%+v", bad)
		}
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, pdf)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadRunning, d.Read.Status)
	})

	// A document has one read, however often its worker restarts: a
	// restart mid-read leaves the claim to lapse and the same row is claimed
	// again, a stop gives it back, and completing the upload again adds
	// none.
	t.Run("reads a PDF once across worker restarts", func(t *testing.T) {
		f := newFoundryFixture(t, "PR3")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		first, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, first)
		lapse(t, pdf)
		second, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NotNil(t, second)
		assert.Equal(t, pdf, second.Document.ID)
		assert.Equal(t, 2, second.Read.Attempts)
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, pdf, model.FoundryFileReadStandard, *second.Read.ClaimToken,
			model.FoundryFileReadReleased{}, wake))
		readPDF(t, f, pdf, pdfRead(pdf))
		assert.ErrorIs(t, f.db.FinishFoundryFileRead(ctx, pdf, model.FoundryFileReadStandard, *first.Read.ClaimToken, pdfRead(pdf), wake),
			store.ErrNotFound, "the first claim records nothing")
		_, _, err = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, pdf, f.fan("PR3-again"))
		assert.ErrorIs(t, err, store.ErrNotFound, "an upload completes once")
		var reads int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_document_reads WHERE document_id=?`, pdf).Scan(&reads))
		assert.Equal(t, 1, reads)
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, pdf)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadSucceeded, d.Read.Status)
	})

	// A PDF deleted while it was read records nothing: its read is gone.
	t.Run("records nothing for a PDF deleted under its read", func(t *testing.T) {
		f := newFoundryFixture(t, "PR4")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, pdf))
		err = f.db.FinishFoundryFileRead(ctx, pdf, model.FoundryFileReadStandard, *claim.Read.ClaimToken, pdfRead(pdf), wake)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// A generation shows the agent a set's newest documents that read as
	// text: a PDF counts once its read succeeded, and never before or after
	// a failure.
	t.Run("lists read PDFs among a set's text", func(t *testing.T) {
		f := newFoundryFixture(t, "PR5")
		set := generalSetID(f.ontology.ID)
		md := f.uploaded(t, "md")
		time.Sleep(2 * time.Millisecond)
		read := f.uploadedAs(t, "read", model.FoundryMediaPDF)
		time.Sleep(2 * time.Millisecond)
		failed := f.uploadedAs(t, "failed", model.FoundryMediaPDF)
		time.Sleep(2 * time.Millisecond)
		f.uploadedAs(t, "reading", model.FoundryMediaPDF)
		readPDF(t, f, read, pdfRead(read))
		readPDF(t, f, failed, model.FoundryFileUnreadable{Error: model.FoundryFileReadError{Code: model.FoundryFileReadUnsupported, Message: "Scanned PDF: no text to read"}})
		docs, err := f.db.ListUploadedFoundryDocuments(ctx, f.ontology.WorkspaceID, set, 10)
		require.NoError(t, err)
		ids := []string{}
		for _, d := range docs {
			ids = append(ids, d.ID)
		}
		assert.Equal(t, []string{read, md}, ids)
		n, err := f.db.CountReadableFoundryTextDocuments(ctx, f.ontology.WorkspaceID, []string{set})
		require.NoError(t, err)
		assert.Equal(t, len(ids), n, "the count agrees with the list")
	})
}

func TestFoundryPDFsInLoads(t *testing.T) {
	ctx := context.Background()

	// A load holds a PDF from the start but neither sends it nor counts it
	// unrecorded while it is read, so its job ends rather than requeueing
	// itself. The read's end queues the extract that sends it.
	t.Run("waits for a PDF's read, then extracts it", func(t *testing.T) {
		f := newFoundryFixture(t, "PL1")
		md := f.uploaded(t, "md")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		load := f.load(t, "L1")
		holds, err := f.db.FoundryLoadHoldsDocument(ctx, load, pdf)
		require.NoError(t, err)
		assert.True(t, holds)
		assert.Equal(t, []string{md}, pendingIDs(t, f, load))
		f.loaded(t, "w1", md)
		assert.Nil(t, f.liveJob(t), "nothing requeued for the PDF being read")

		readPDF(t, f, pdf, pdfRead(pdf))
		job := f.liveJob(t)
		require.NotNil(t, job, "the read's end queued an extract")
		assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
		assert.Equal(t, load, job.LoadID)
		assert.Nil(t, job.RequestedByUserID)
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, pdf, pending[0].ID)
		key, ok := pending[0].TextKey()
		assert.True(t, ok)
		assert.Equal(t, pdfRead(pdf).ArtifactKey, key)
	})

	// A read that ends while a job writes the load is owed by that job,
	// which may be past listing its documents. The PDF is then unrecorded
	// in the load, so the job's end goes on to send it.
	t.Run("owes the extract while a job is live", func(t *testing.T) {
		f := newFoundryFixture(t, "PL2")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		f.load(t, "L1")
		readPDF(t, f, pdf, pdfRead(pdf))
		assert.True(t, f.ontologyNow(t).ExtractOwed)
		job, finish := f.finish(t, "w1", "PL2", model.FoundryGraphJobSucceeded)
		require.Equal(t, model.FoundryGraphJobLoad, job.Kind)
		assert.Equal(t, model.FoundryGraphJobMerged, finish, "the job goes on to send the PDF")
		assert.Equal(t, []string{pdf}, pendingIDs(t, f, job.LoadID))
		_, finish = f.finish(t, "w1", "PL2-2", model.FoundryGraphJobSucceeded, pdf)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
		assert.False(t, f.ontologyNow(t).ExtractOwed, "the debt is paid")
		assert.Nil(t, f.liveJob(t), "nothing is left to extract")
	})

	// A PDF that can't be read is sent once its read fails, so the load
	// records it failed with the read's code instead of waiting forever.
	t.Run("sends a PDF whose read failed, to be recorded", func(t *testing.T) {
		f := newFoundryFixture(t, "PL3")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		load := f.load(t, "L1")
		f.loaded(t, "w1")
		readPDF(t, f, pdf, model.FoundryFileUnreadable{Error: model.FoundryFileReadError{Code: model.FoundryFileReadTooLarge, Message: "151 pages"}})
		job := f.liveJob(t)
		require.NotNil(t, job)
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, model.FoundryFileReadTooLarge, pending[0].Read.Error.Code)
	})

	// A PDF uploaded into a set the current load reads joins it and waits:
	// the upload says reading, and queues nothing until the read ends.
	t.Run("says a PDF uploaded into a load is being read", func(t *testing.T) {
		f := newFoundryFixture(t, "PL4")
		load := f.load(t, "L1")
		f.loaded(t, "w1")
		doc := foundryDocument(f.ontology, "pdf")
		doc.Name, doc.MediaType = "pdf.pdf", model.FoundryMediaPDF
		_, _, err := f.db.CreateFoundryDocument(ctx, doc, noCap)
		require.NoError(t, err)
		_, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, doc.ID, f.fan("PL4"))
		require.NoError(t, err)
		require.Len(t, reads, 1)
		assert.Equal(t, model.FoundryDocumentRead{OntologyID: f.ontology.ID, Outcome: model.FoundryDocumentReadReading}, reads[0])
		assert.Nil(t, f.liveJob(t))
		holds, err := f.db.FoundryLoadHoldsDocument(ctx, load, doc.ID)
		require.NoError(t, err)
		assert.True(t, holds)
		readPDF(t, f, doc.ID, pdfRead(doc.ID))
		require.NotNil(t, f.liveJob(t), "the read's end queued its extract")
	})

	// A read PDF moved into a set the current load reads is read as text
	// is, at once.
	t.Run("reads a read PDF moved into a load at once", func(t *testing.T) {
		f := newFoundryFixture(t, "PL5")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		readPDF(t, f, pdf, pdfRead(pdf))
		f.load(t, "L1")
		f.loaded(t, "w1", pdf)
		other := f.documentSet("other")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, other, 10))
		_, _, err := f.db.MoveFoundryDocument(ctx, f.ontology.WorkspaceID, pdf, other.ID, f.fan("PL5-out"))
		require.NoError(t, err)
		_, reads, err := f.db.MoveFoundryDocument(ctx, f.ontology.WorkspaceID, pdf, generalSetID(f.ontology.ID), f.fan("PL5-in"))
		require.NoError(t, err)
		require.Len(t, reads, 1)
		assert.Equal(t, model.FoundryDocumentReadHeld, reads[0].Outcome, "the load already holds what it read")
	})

	// A read that ends in a workspace with no load holding the PDF wakes
	// nothing: the next load reads it.
	t.Run("wakes no load that does not hold the PDF", func(t *testing.T) {
		f := newFoundryFixture(t, "PL6")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		readPDF(t, f, pdf, pdfRead(pdf))
		assert.Nil(t, f.liveJob(t))
		assert.False(t, f.ontologyNow(t).ExtractOwed)
		load := f.load(t, "L1")
		assert.Equal(t, []string{pdf}, pendingIDs(t, f, load), "the next load sends it")
	})

	// A PDF that kills the reader fails after three lapses, and the load
	// holding it is woken to record it, as a read that failed in order is.
	t.Run("wakes the load of a PDF whose reads lapsed", func(t *testing.T) {
		f := newFoundryFixture(t, "PL7")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		load := f.load(t, "L1")
		f.loaded(t, "w1")
		for range model.MaxFoundryFileReadLapses {
			claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
			require.NoError(t, err)
			require.NotNil(t, claim)
			lapse(t, pdf)
		}
		none, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, none)
		require.NotNil(t, f.liveJob(t), "the failure queued an extract to record it")
		assert.Equal(t, []string{pdf}, pendingIDs(t, f, load))
	})

	// Failing a lapsed PDF and waking its load are one transaction: a wake
	// that fails leaves the read lapsed, and the next claim fails and wakes
	// it again.
	t.Run("keeps a lapsed PDF for the next claim when its wake fails", func(t *testing.T) {
		f := newFoundryFixture(t, "PL9")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		load := f.load(t, "L1")
		f.loaded(t, "w1")
		for range model.MaxFoundryFileReadLapses {
			claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
			require.NoError(t, err)
			require.NotNil(t, claim)
			lapse(t, pdf)
		}
		var taken string
		require.NoError(t, testDB.QueryRow(`SELECT id FROM foundry_load_jobs WHERE ontology_id=? LIMIT 1`, f.ontology.ID).Scan(&taken))
		clash := model.FoundryFileReadWake{NewJobID: func() string { return taken }, Budget: noBudget}
		_, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, clash)
		require.Error(t, err, "the wake's job id is taken")
		d, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, pdf)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadRunning, d.Read.Status, "the failure rolled back with its wake")
		assert.Nil(t, f.liveJob(t))

		none, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		assert.Nil(t, none)
		d, err = f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, pdf)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryFileReadFailed, d.Read.Status)
		require.NotNil(t, f.liveJob(t), "the retried failure queued an extract to record it")
		assert.Equal(t, []string{pdf}, pendingIDs(t, f, load))
	})

	// The extract a read's end queues is the uploader's, as the upload's own
	// would have been: it may spend the share of the month kept for people,
	// which an extract no one asked for may not. A PDF with no uploader
	// stops at the system's share.
	t.Run("extracts a person's PDF on their share of the month", func(t *testing.T) {
		f := newFoundryFixture(t, "PL10")
		nobody := f.uploadedAs(t, "pdf-nobody", model.FoundryMediaPDF)
		person := f.uploadedBy(t, "pdf-person", "user-PL10")
		f.load(t, "L1")
		f.loaded(t, "w1")
		// The month has $1 left for a person and none for work no one asked for.
		reserved := model.FoundryFileReadWake{NewJobID: id.New, Budget: model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 1, ReserveMicros: 1}}
		finish := func(doc string) {
			claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
			require.NoError(t, err)
			require.Equal(t, doc, claim.Document.ID)
			require.NoError(t, f.db.FinishFoundryFileRead(ctx, doc, model.FoundryFileReadStandard, *claim.Read.ClaimToken, pdfRead(doc), reserved))
		}
		finish(nobody)
		assert.Nil(t, f.liveJob(t), "with no uploader, the extract stops at the system's share")
		finish(person)
		job := f.liveJob(t)
		require.NotNil(t, job, "the uploader's extract may spend the reserve")
		require.NotNil(t, job.RequestedByUserID)
		assert.Equal(t, "user-PL10", *job.RequestedByUserID)
	})

	// A PDF failed for lapsing wakes its loads for its uploader too.
	t.Run("wakes a lapsed PDF's load for its uploader", func(t *testing.T) {
		f := newFoundryFixture(t, "PL11")
		pdf := f.uploadedBy(t, "pdf", "user-PL11")
		f.load(t, "L1")
		f.loaded(t, "w1")
		for range model.MaxFoundryFileReadLapses {
			claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
			require.NoError(t, err)
			require.NotNil(t, claim)
			lapse(t, pdf)
		}
		reserved := model.FoundryFileReadWake{NewJobID: id.New, Budget: model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 1, ReserveMicros: 1}}
		none, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, reserved)
		require.NoError(t, err)
		assert.Nil(t, none)
		job := f.liveJob(t)
		require.NotNil(t, job, "the failure queued the uploader's extract to record it")
		require.NotNil(t, job.RequestedByUserID)
		assert.Equal(t, "user-PL11", *job.RequestedByUserID)
	})

	// The wake spends from the same monthly budget as an upload: with none
	// left it queues nothing, and the next load reads the PDF.
	t.Run("queues nothing past the month's budget", func(t *testing.T) {
		f := newFoundryFixture(t, "PL8")
		pdf := f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		f.load(t, "L1")
		f.loaded(t, "w1")
		claim, err := f.db.ClaimFoundryFileRead(ctx, pdfTypes, id.New(), time.Minute, wake)
		require.NoError(t, err)
		spent := model.FoundryFileReadWake{NewJobID: id.New, Budget: model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 0}}
		require.NoError(t, f.db.FinishFoundryFileRead(ctx, pdf, model.FoundryFileReadStandard, *claim.Read.ClaimToken, pdfRead(pdf), spent))
		assert.Nil(t, f.liveJob(t))
	})
}
