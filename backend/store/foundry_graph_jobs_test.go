package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foundryFixture is an ontology on its own tenant, ready for documents and loads.
type foundryFixture struct {
	db       store.Store
	ontology *model.FoundryOntology
}

func newFoundryFixture(t *testing.T, tag string) foundryFixture {
	t.Helper()
	db := newTestStore(t)
	return foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-"+tag), tag)}
}

// version writes an agent draft of the fixture's ontology, FV-<tag>.
func (f foundryFixture) version(t *testing.T, tag string) string {
	t.Helper()
	v := &model.OntologyVersion{
		ID: id26("FV-" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), DocumentSetIDs: &[]string{generalSetID(f.ontology.ID)},
	}
	require.NoError(t, f.db.CreateOntologyVersion(context.Background(), v))
	return v.ID
}

// loadJob is a load job of o through version FV-<tag>, creating load FJ-<tag>.
func loadJob(o *model.FoundryOntology, tag string) *model.FoundryGraphJob {
	token := "load-" + tag
	return &model.FoundryGraphJob{
		ID: id26("FJ-" + tag), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, VersionID: id26("FV-" + tag), LoadID: id26("FJ-" + tag),
		Kind: model.FoundryGraphJobLoad, Phase: model.FoundryGraphJobProvisioning, ClientToken: &token,
	}
}

// load writes version FV-<tag> and queues a load of it, returning the load's id.
func (f foundryFixture) load(t *testing.T, tag string) string {
	t.Helper()
	f.version(t, tag)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, tag)))
	return id26("FJ-" + tag)
}

// uploaded writes a Markdown document of the ontology's set whose bytes are in.
func (f foundryFixture) uploaded(t *testing.T, tag string) string {
	t.Helper()
	return f.uploadedAs(t, tag, model.FoundryMediaMarkdown)
}

// uploadedAs writes a document of the ontology's set, of mediaType, whose
// bytes are in.
func (f foundryFixture) uploadedAs(t *testing.T, tag string, mediaType model.FoundryMediaType) string {
	t.Helper()
	ctx := context.Background()
	id := id26("FD-" + tag)
	d := foundryDocument(f.ontology, tag)
	d.MediaType = mediaType
	_, _, err := f.db.CreateFoundryDocument(ctx, d, noCap)
	require.NoError(t, err)
	_, _, err = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, id, f.fan(tag))
	require.NoError(t, err)
	return id
}

// fan is what a caller who may write every database hands a completion or
// a move: extracts FX-<tag>, then FX-<tag>-2, FX-<tag>-3, …, no monthly
// limit.
func (f foundryFixture) fan(tag string) model.FoundryDocumentFanOut {
	n := 0
	return model.FoundryDocumentFanOut{
		NewJobID: func() string {
			n++
			if n == 1 {
				return id26("FX-" + tag)
			}
			return id26(fmt.Sprintf("FX-%s-%d", tag, n))
		},
		Budget:   noBudget,
		CanWrite: func(string) bool { return true },
	}
}

// next is what a worker hands a finish: ids for the jobs it may queue after
// (FN-X-<tag>, FN-D-<tag>), no monthly limit, and five loads kept.
func next(tag string) model.FoundryGraphNext {
	return model.FoundryGraphNext{ExtractID: id26("FN-X-" + tag), DeleteID: id26("FN-D-" + tag), Budget: noBudget, RetainLoads: 5}
}

// finish claims the due job as worker, records the documents as extracted
// and ends the attempt with status; tag names the jobs it may queue after.
func (f foundryFixture) finish(t *testing.T, worker, tag string, status model.FoundryGraphJobStatus, documents ...string) (
	*model.FoundryGraphJob, model.FoundryGraphJobFinish) {
	t.Helper()
	ctx := context.Background()
	job, err := f.db.ClaimFoundryGraphJob(ctx, worker, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job, "a job is due")
	for _, doc := range documents {
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, worker, extracted(doc), false))
	}
	finish, err := f.db.FinishFoundryGraphJob(ctx, job.ID, worker, model.FoundryGraphJobOutcome{
		Status: status, IngestDone: job.Kind == model.FoundryGraphJobLoad, Next: next(tag),
	})
	require.NoError(t, err)
	return job, finish
}

// loaded claims the queued load job, records the documents extracted and
// ends it succeeded: its load is ready and current.
func (f foundryFixture) loaded(t *testing.T, worker string, documents ...string) {
	t.Helper()
	job, finish := f.finish(t, worker, "loaded-"+worker, model.FoundryGraphJobSucceeded, documents...)
	require.Equal(t, model.FoundryGraphJobLoad, job.Kind)
	require.Equal(t, model.FoundryGraphJobEnded, finish)
}

// ontologyNow re-reads the fixture's ontology.
func (f foundryFixture) ontologyNow(t *testing.T) *model.FoundryOntology {
	t.Helper()
	o, err := f.db.GetFoundryOntology(context.Background(), f.ontology.WorkspaceID, f.ontology.ID)
	require.NoError(t, err)
	return o
}

// loadStatus reads a load's status.
func (f foundryFixture) loadStatus(t *testing.T, loadID string) model.FoundryLoadStatus {
	t.Helper()
	l, err := f.db.GetFoundryLoad(context.Background(), f.ontology.ID, loadID)
	require.NoError(t, err)
	return l.Status
}

// liveJob is the ontology's live job, nil when it has none.
func (f foundryFixture) liveJob(t *testing.T) *model.FoundryGraphJob {
	t.Helper()
	job, err := f.db.GetActiveFoundryGraphJob(context.Background(), f.ontology.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return job
}

// generalSetID is the id fixtures give an ontology's General set: its own id
// with FG in place of FO.
func generalSetID(ontologyID string) string {
	return "FG" + ontologyID[2:]
}

// foundryDocument is a document of o's workspace in o's set.
func foundryDocument(o *model.FoundryOntology, tag string) *model.FoundryDocument {
	return &model.FoundryDocument{
		ID: id26("FD-" + tag), WorkspaceID: o.WorkspaceID, SetID: generalSetID(o.ID), Name: tag + ".md",
		MediaType: model.FoundryMediaMarkdown,
		SizeBytes: 120, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(tag))), ClientToken: new("doc-" + tag),
		S3Key: "ws/" + o.WorkspaceID + "/documents/" + id26("FD-"+tag) + "/original",
	}
}

func extractJob(tag string) *model.FoundryGraphJob {
	return &model.FoundryGraphJob{ID: id26("FX-" + tag)}
}

var noBudget = model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 1 << 62}

var noCap = model.FoundryDocumentCap{Max: 1 << 30, PendingSince: time.Unix(0, 0)}

func extracted(documentID string) *model.FoundryDocumentExtraction {
	return &model.FoundryDocumentExtraction{
		DocumentID: documentID, Status: model.FoundryExtractionExtracted,
		Counts: model.FoundryExtractionCounts{Sections: 2, Entities: 3, Mentions: 4, Facts: 1}, USDMicros: 250_000,
		InputTokens: 1000, OutputTokens: 100,
	}
}

func TestFoundryDocuments(t *testing.T) {
	// A retried POST must not make a second document, and the same bytes
	// again find the first, in whatever set of the workspace it is.
	t.Run("finds a repeat upload by client token, then content, per workspace", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC1")
		first := f.created(t, foundryDocument(f.ontology, "a"))
		assert.Equal(t, model.FoundryDocumentPendingUpload, first.Status)
		var owner *string
		require.NoError(t, testDB.QueryRow(`SELECT ontology_id FROM foundry_documents WHERE id=?`, first.ID).Scan(&owner))
		assert.Nil(t, owner, "a new document names no ontology")

		again := foundryDocument(f.ontology, "a")
		again.ID = id26("FD-other")
		found, existing, err := f.db.CreateFoundryDocument(ctx, again, noCap)
		require.NoError(t, err)
		assert.True(t, existing)
		assert.Equal(t, first.ID, found.ID, "the same client token")

		contracts := f.documentSet("contracts")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, contracts, 20))
		sameBytes := f.documentIn("copy", contracts.ID)
		sameBytes.SHA256 = first.SHA256
		found, existing, err = f.db.CreateFoundryDocument(ctx, sameBytes, noCap)
		require.NoError(t, err)
		assert.True(t, existing)
		assert.Equal(t, first.ID, found.ID, "the same bytes, though asked for in another set")
		assert.Equal(t, generalSetID(f.ontology.ID), found.SetID, "in the set it is in")

		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, first.ID))
		found, existing, err = f.db.CreateFoundryDocument(ctx, sameBytes, noCap)
		require.NoError(t, err)
		assert.False(t, existing, "a deleted document's bytes are no repeat")
		assert.Equal(t, sameBytes.ID, found.ID)

		other := sampleOntology(t, f.db, id26("FWS-DOC1-other"), "DOC1-other")
		elsewhere := foundryDocument(other, "a")
		elsewhere.ID = id26("FD-elsewhere")
		found, existing, err = f.db.CreateFoundryDocument(ctx, elsewhere, noCap)
		require.NoError(t, err)
		assert.False(t, existing, "client tokens and bytes repeat only within a workspace")
		_, err = f.db.GetFoundryDocument(ctx, other.WorkspaceID, first.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "documents are scoped to their workspace")
		assert.Equal(t, elsewhere.ID, found.ID)
	})

	// A double click sends one create twice at once: both see one document,
	// as the lookups run under the workspace's lock.
	t.Run("finds a repeat racing its first create", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC9")
		var wg sync.WaitGroup
		ids := make([]string, 6)
		existing := make([]bool, 6)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d := foundryDocument(f.ontology, "racy")
				d.ID = id26(fmt.Sprintf("FD-racy-%d", i))
				if i%2 == 1 {
					// The same file dropped again, with a new token.
					d.ClientToken = new(fmt.Sprintf("doc-racy-%d", i))
				}
				stored, found, err := f.db.CreateFoundryDocument(ctx, d, noCap)
				assert.NoError(t, err)
				if err == nil {
					ids[i], existing[i] = stored.ID, found
				}
			}()
		}
		wg.Wait()
		created := 0
		for i := range ids {
			assert.Equal(t, ids[0], ids[i], "one document")
			if !existing[i] {
				created++
			}
		}
		assert.Equal(t, 1, created)
	})

	// The list pages newest first and leaves deleted documents out, as do
	// the counts the cap and a scope use.
	t.Run("lists and counts live documents", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC2")
		ws := f.ontology.WorkspaceID
		for _, tag := range []string{"a", "bb", "ccc"} {
			f.created(t, foundryDocument(f.ontology, tag))
			time.Sleep(2 * time.Millisecond)
		}
		other := sampleOntology(t, f.db, ws, "DOC2-other")
		f.created(t, foundryDocument(other, "dddd"))
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, ws, id26("FD-bb")))
		assert.ErrorIs(t, f.db.DeleteFoundryDocument(ctx, ws, id26("FD-bb")), store.ErrNotFound)
		n, err := f.db.CountFoundryDocuments(ctx, ws, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Equal(t, 3, n, "the workspace cap counts every set's documents")
		gone := sampleOntology(t, f.db, ws, "DOC2-gone")
		f.created(t, foundryDocument(gone, "eeeee"))
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, gone.WorkspaceID, gone.ID, id26("FN-D-gone")))
		n, err = f.db.CountFoundryDocuments(ctx, ws, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Equal(t, 4, n, "a deleted ontology's set and documents stay the workspace's")
		f.uploaded(t, "uploaded")
		n, err = f.db.CountFoundryDocumentsInSets(ctx, ws, []string{generalSetID(f.ontology.ID)})
		require.NoError(t, err)
		assert.Equal(t, 1, n, "a scope's count is its uploaded documents")
		n, err = f.db.CountFoundryDocumentsInSets(ctx, ws, nil)
		require.NoError(t, err)
		assert.Zero(t, n, "an empty scope holds none")

		page, next, err := f.db.ListFoundryDocuments(ctx, ws, generalSetID(f.ontology.ID), "", 2)
		require.NoError(t, err)
		require.Len(t, page, 2)
		assert.Equal(t, []string{id26("FD-uploaded"), id26("FD-ccc")}, []string{page[0].ID, page[1].ID})
		rest, last, err := f.db.ListFoundryDocuments(ctx, ws, generalSetID(f.ontology.ID), next, 10)
		require.NoError(t, err)
		require.Len(t, rest, 1)
		assert.Equal(t, id26("FD-a"), rest[0].ID)
		assert.Empty(t, last)
		all, _, err := f.db.ListFoundryDocuments(ctx, ws, "", "", 10)
		require.NoError(t, err)
		assert.Len(t, all, 5, "every set's, the deleted ontology's included")
		_, _, err = f.db.ListFoundryDocuments(ctx, ws, "", "not-a-token", 10)
		assert.ErrorIs(t, err, store.ErrInvalidPageToken)
	})

	// A document can be renamed until it is deleted; its name is the
	// caller's to check.
	t.Run("renames a document", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC10")
		doc := f.uploaded(t, "a")
		require.NoError(t, f.db.RenameFoundryDocument(ctx, f.ontology.WorkspaceID, doc, "Contract.md"))
		got, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, doc)
		require.NoError(t, err)
		assert.Equal(t, "Contract.md", got.Name)
		assert.ErrorIs(t, f.db.RenameFoundryDocument(ctx, id26("FWS-elsewhere"), doc, "x.md"), store.ErrNotFound)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, doc))
		assert.ErrorIs(t, f.db.RenameFoundryDocument(ctx, f.ontology.WorkspaceID, doc, "x.md"), store.ErrNotFound)
	})

	// A generation drafts for the documents whose bytes are in, newest first
	// and bounded; one waiting for its bytes, deleted, or of a type
	// ontologies don't read is not one of them.
	t.Run("lists the uploaded documents a generation drafts for", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC8")
		older := f.uploaded(t, "a")
		time.Sleep(2 * time.Millisecond)
		f.created(t, foundryDocument(f.ontology, "bb"))
		time.Sleep(2 * time.Millisecond)
		deleted := f.uploaded(t, "ccc")
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, deleted))
		time.Sleep(2 * time.Millisecond)
		newer := f.uploaded(t, "dddd")
		time.Sleep(2 * time.Millisecond)
		pdf := foundryDocument(f.ontology, "fffff")
		pdf.Name, pdf.MediaType = "fffff.pdf", model.FoundryMediaPDF
		_, _, err := f.db.CreateFoundryDocument(ctx, pdf, noCap)
		require.NoError(t, err)
		_, _, err = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, pdf.ID, f.fan("fffff"))
		require.NoError(t, err)
		other := sampleOntology(t, f.db, f.ontology.WorkspaceID, "DOC8-other")
		f.created(t, foundryDocument(other, "eeeee"))

		docs, err := f.db.ListUploadedFoundryDocuments(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID), 10)
		require.NoError(t, err)
		var ids []string
		for _, d := range docs {
			ids = append(ids, d.ID)
		}
		assert.Equal(t, []string{newer, older}, ids)
		first, err := f.db.ListUploadedFoundryDocuments(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID), 1)
		require.NoError(t, err)
		require.Len(t, first, 1)
		assert.Equal(t, newer, first[0].ID)
	})

	// Several files dropped at once create their documents concurrently; the
	// cap must hold across them, not just for each alone.
	t.Run("holds the workspace cap across concurrent creates", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC6")
		other := sampleOntology(t, f.db, f.ontology.WorkspaceID, "DOC6-other")
		f.created(t, foundryDocument(f.ontology, "a"))
		f.created(t, foundryDocument(other, "bb"))
		limit := model.FoundryDocumentCap{Max: 3, PendingSince: time.Unix(0, 0)}
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o := f.ontology
				if i%2 == 1 {
					o = other
				}
				_, _, errs[i] = f.db.CreateFoundryDocument(ctx, foundryDocument(o, fmt.Sprintf("c%d", i)), limit)
			}()
		}
		wg.Wait()
		created := 0
		for _, err := range errs {
			if err == nil {
				created++
				continue
			}
			assert.ErrorIs(t, err, store.ErrLimitExceeded)
		}
		assert.Equal(t, 1, created, "one place was left")
		n, err := f.db.CountFoundryDocuments(ctx, f.ontology.WorkspaceID, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Equal(t, 3, n)
	})

	// An upload whose URL expired before its bytes came was abandoned and
	// gives its place back; one still within its URL's life holds it. Asking
	// for an abandoned one again takes a place again.
	t.Run("counts abandoned uploads out of the cap", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC7")
		f.created(t, foundryDocument(f.ontology, "a"))
		f.created(t, foundryDocument(f.ontology, "bb"))
		f.uploaded(t, "ccc")
		_, err := testDB.Exec(`UPDATE foundry_documents SET created_at=NOW(6) - INTERVAL 1 HOUR WHERE id IN (?, ?)`, id26("FD-a"), id26("FD-ccc"))
		require.NoError(t, err)
		n, err := f.db.CountFoundryDocuments(ctx, f.ontology.WorkspaceID, time.Now().Add(-15*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, 2, n, "the fresh pending upload and the old uploaded document count; the abandoned one does not")
		limit := model.FoundryDocumentCap{Max: 3, PendingSince: time.Now().Add(-15 * time.Minute)}
		_, _, err = f.db.CreateFoundryDocument(ctx, foundryDocument(f.ontology, "dddd"), limit)
		require.NoError(t, err)
		_, _, err = f.db.CreateFoundryDocument(ctx, foundryDocument(f.ontology, "eeeee"), limit)
		assert.ErrorIs(t, err, store.ErrLimitExceeded)
		_, _, err = f.db.CreateFoundryDocument(ctx, foundryDocument(f.ontology, "a"), limit)
		assert.ErrorIs(t, err, store.ErrLimitExceeded, "the abandoned upload asked for again")
		found, existing, err := f.db.CreateFoundryDocument(ctx, foundryDocument(f.ontology, "bb"), limit)
		require.NoError(t, err, "a fresh pending upload holds its place")
		assert.True(t, existing)
		assert.Equal(t, id26("FD-bb"), found.ID)
	})

	// Before a load is current a document waits for the next load, which
	// extracts every document of its scope; after, an upload is queued into
	// the current load, or owed by the job that is live.
	t.Run("queues extraction of an upload into the current load", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC3")
		ws := f.ontology.WorkspaceID
		f.created(t, foundryDocument(f.ontology, "a"))
		doc, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, ws, id26("FD-a"), f.fan("a"))
		require.NoError(t, err)
		assert.Empty(t, reads, "no load reads it yet: it waits for a load")
		assert.False(t, f.ontologyNow(t).ExtractOwed, "no job is live to owe it")
		assert.Equal(t, model.FoundryDocumentUploaded, doc.Status)
		require.NotNil(t, doc.UploadedAt)
		_, _, err = f.db.CompleteFoundryDocumentUpload(ctx, ws, id26("FD-a"), f.fan("a2"))
		assert.ErrorIs(t, err, store.ErrNotFound, "an upload completes once")

		load := f.load(t, "L1")
		f.loaded(t, "w1", id26("FD-a"))
		require.NotNil(t, f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, load, *f.ontologyNow(t).CurrentLoadID)

		f.created(t, foundryDocument(f.ontology, "b"))
		_, reads, err = f.db.CompleteFoundryDocumentUpload(ctx, ws, id26("FD-b"), f.fan("b"))
		require.NoError(t, err)
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: f.ontology.ID, Outcome: model.FoundryDocumentReadQueued}}, reads)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, id26("FX-b"), job.ID)
		assert.Equal(t, load, job.LoadID, "extracted into the current load")
		assert.Equal(t, id26("FV-L1"), job.VersionID, "against the load's version")
		assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
		assert.Equal(t, model.FoundryGraphJobDocuments, job.Phase)
		assert.Nil(t, job.ClientToken)

		f.created(t, foundryDocument(f.ontology, "cc"))
		_, reads, err = f.db.CompleteFoundryDocumentUpload(ctx, ws, id26("FD-cc"), f.fan("cc"))
		require.NoError(t, err)
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: f.ontology.ID, Outcome: model.FoundryDocumentReadOwed}}, reads,
			"the live job's end sees to it")
		assert.True(t, f.ontologyNow(t).ExtractOwed)
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.Len(t, pending, 2, "the load extracted the first; the extract job owes two")
	})

	// A deleted ontology reads no upload: its documents are the workspace's.
	t.Run("completes an upload of a deleted ontology's set for no ontology", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC4")
		f.load(t, "L1")
		f.loaded(t, "w1")
		f.created(t, foundryDocument(f.ontology, "a"))
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-D-DOC4")))
		doc, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, id26("FD-a"), f.fan("a"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, doc.Status)
		assert.Empty(t, reads)
	})

	// A workspace past its monthly cap queues no paid work, whichever of its ontologies spent it.
	t.Run("queues nothing past the monthly budget", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "DOC5")
		f.load(t, "L1")
		doc := f.uploaded(t, "a")
		f.loaded(t, "w1", doc)
		spent, err := f.db.SumFoundryGraphJobSpend(ctx, f.ontology.WorkspaceID, time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.Equal(t, int64(250_000), spent)

		f.created(t, foundryDocument(f.ontology, "bb"))
		fan := f.fan("bb")
		fan.Budget = model.FoundryGraphBudget{Since: time.Now().Add(-time.Hour), CapMicros: 250_000}
		_, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, id26("FD-bb"), fan)
		require.NoError(t, err)
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: f.ontology.ID, Outcome: model.FoundryDocumentReadNoBudget}}, reads)
		assert.Nil(t, f.liveJob(t))
	})

	// Work no person started stops a job's cap short of the month: with
	// prod's $50 month and $10 jobs, an extract no one asked for is queued
	// at $39.99 spent and not at $40, and a person's still is, up to $50.
	t.Run("keeps the month's last job's worth for a person", func(t *testing.T) {
		prod := model.FoundryGraphBudget{Since: time.Now().Add(-time.Hour), CapMicros: 50_000_000, ReserveMicros: 10_000_000}
		for i, tc := range []struct {
			spent  int64
			person bool
			want   model.FoundryDocumentReadOutcome
		}{
			{39_990_000, false, model.FoundryDocumentReadQueued},
			{40_000_000, false, model.FoundryDocumentReadNoBudget},
			{40_000_000, true, model.FoundryDocumentReadQueued},
			{49_990_000, true, model.FoundryDocumentReadQueued},
			{50_000_000, true, model.FoundryDocumentReadNoBudget},
		} {
			t.Run(fmt.Sprintf("$%.2f spent, a person %t", float64(tc.spent)/1e6, tc.person), func(t *testing.T) {
				ctx := context.Background()
				f := newFoundryFixture(t, fmt.Sprintf("DOC9-%d", i))
				load := f.load(t, "L1")
				f.loaded(t, "w1")
				_, err := testDB.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=? WHERE id=?`, tc.spent, load)
				require.NoError(t, err)
				f.created(t, foundryDocument(f.ontology, "a"))
				fan := f.fan("a")
				fan.Budget = prod
				if tc.person {
					fan.RequestedByUserID = new(id26("U-1"))
				}
				_, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, id26("FD-a"), fan)
				require.NoError(t, err)
				assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: f.ontology.ID, Outcome: tc.want}}, reads)
				if job := f.liveJob(t); job != nil {
					assert.Equal(t, fan.RequestedByUserID, job.RequestedByUserID, "the extract keeps its requester")
				}
			})
		}
	})
}

func TestFoundryGraphJobQueue(t *testing.T) {
	// Two writers of one database at once would interleave; another ontology's job is no conflict.
	t.Run("keeps one live job per ontology", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q1")
		loadID := f.load(t, "L1")
		f.version(t, "L2")
		second := loadJob(f.ontology, "L2")
		assert.ErrorIs(t, f.db.CreateFoundryLoad(ctx, second), store.ErrConflict)
		_, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, second.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a refused load leaves no load behind")
		other := sampleOntology(t, f.db, f.ontology.WorkspaceID, "Q1-other")
		otherFixture := foundryFixture{db: f.db, ontology: other}
		otherFixture.load(t, "L3")

		byToken, err := f.db.GetFoundryGraphJobByClientToken(ctx, f.ontology.ID, "load-L1")
		require.NoError(t, err)
		assert.Equal(t, loadID, byToken.ID)
		assert.Equal(t, loadID, byToken.LoadID, "a load job's load has its id")
		assert.Equal(t, model.FoundryGraphJobLoad, byToken.Kind)
		assert.Equal(t, model.FoundryGraphJobProvisioning, byToken.Phase)
		_, err = f.db.GetFoundryGraphJobByClientToken(ctx, other.ID, "load-L1")
		assert.ErrorIs(t, err, store.ErrNotFound, "client tokens are per ontology")
		assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, loadID))

		f.finish(t, "w1", "Q1a", model.FoundryGraphJobFailed)
		otherFixture.finish(t, "w1", "Q1b", model.FoundryGraphJobFailed)
		assert.Nil(t, f.liveJob(t))
		latest, err := f.db.GetLatestFoundryGraphJob(ctx, f.ontology.ID, model.FoundryGraphJobLoad)
		require.NoError(t, err)
		assert.Equal(t, loadID, latest.ID)
		assert.Equal(t, model.FoundryGraphJobFailed, latest.Status)
		_, err = f.db.GetLatestFoundryGraphJob(ctx, f.ontology.ID, model.FoundryGraphJobExtract)
		assert.ErrorIs(t, err, store.ErrNotFound, "the latest job is the latest of its kind")
		require.NoError(t, f.db.CreateFoundryLoad(ctx, second), "an ended job no longer blocks the next")
		assert.Error(t, f.db.CreateFoundryLoad(ctx, &model.FoundryGraphJob{ID: id26("FJ-bad"), Kind: model.FoundryGraphJobExtract}),
			"only a load job creates a load")
	})

	// A worker that lost its lease can't renew, record or finish: the new owner does.
	t.Run("refuses an attempt that lost its lease", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q2")
		jobID := f.load(t, "L1")
		job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, 1, job.Attempts)
		time.Sleep(10 * time.Millisecond)
		again, err := f.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
		require.NoError(t, err)
		require.Equal(t, jobID, again.ID, "an expired lease is claimed again")
		assert.Equal(t, 2, again.Attempts)

		doc := f.uploaded(t, "a")
		assert.ErrorIs(t, f.db.RenewFoundryGraphJobLease(ctx, jobID, "w1", time.Minute), store.ErrNotFound)
		assert.ErrorIs(t, f.db.UpdateFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobUpdate{Phase: model.FoundryGraphJobIngest}), store.ErrNotFound)
		assert.ErrorIs(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", extracted(doc), false), store.ErrNotFound)
		_, err = f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobFailed, Next: next("Q2")})
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.GetFoundryDocumentExtraction(ctx, jobID, doc)
		assert.ErrorIs(t, err, store.ErrNotFound, "the stale attempt wrote nothing")
		assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, jobID), "nor ended the load")

		require.NoError(t, f.db.RenewFoundryGraphJobLease(ctx, jobID, "w2", time.Minute))
		require.NoError(t, f.db.UpdateFoundryGraphJob(ctx, jobID, "w2", model.FoundryGraphJobUpdate{
			Phase: model.FoundryGraphJobIngest, Progress: model.FoundryGraphJobProgress{ChunksDone: 3},
		}))
		stored, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, jobID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobIngest, stored.Phase)
		assert.Equal(t, 3, stored.Progress.ChunksDone)
	})

	// A retry rewrites what an earlier attempt recorded rather than adding a
	// second row; another load of the same document has a row of its own.
	t.Run("records each document once per load", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q3")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		doc := f.uploaded(t, "a")
		key := "ws/k.json.gz"
		first := extracted(doc)
		first.ArtifactKey = &key
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", first, false))
		failed := extracted(doc)
		failed.Status, failed.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", failed, false))
		stored, err := f.db.ListFoundryDocumentExtractions(ctx, jobID, []string{doc, id26("FD-none")})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		got := stored[doc]
		assert.Equal(t, model.FoundryExtractionFailed, got.Status, "a retry rewrites the row")
		assert.Equal(t, model.FoundryExtractionModel, got.Error.Code)
		assert.True(t, got.Error.Retryable)
		assert.Equal(t, key, *got.ArtifactKey, "a later write without an artifact keeps the saved one")
		assert.Equal(t, jobID, got.LoadID)

		// The load already holding the document replaces a failure, and keeps a success.
		held := extracted(doc)
		held.Counts = model.FoundryExtractionCounts{Sections: 9}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", held, true))
		again := extracted(doc)
		again.Counts = model.FoundryExtractionCounts{}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", again, true))
		got, err = f.db.GetFoundryDocumentExtraction(ctx, jobID, doc)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionExtracted, got.Status, "already ingested replaces the failed row")
		assert.Nil(t, got.Error)
		assert.Equal(t, 9, got.Counts.Sections, "already ingested keeps a row that is not a failure")
		assert.Equal(t, jobID, got.JobID)
		assert.Equal(t, f.ontology.ID, got.OntologyID)

		dup := extracted(doc)
		dup.Status = model.FoundryExtractionDuplicate
		assert.Error(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", dup, false), "a duplicate must name its original")
		foreign := sampleOntology(t, f.db, f.ontology.WorkspaceID, "Q3-other")
		f.created(t, foundryDocument(foreign, "bb"))
		assert.ErrorContains(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", extracted(id26("FD-bb")), false), "did not join",
			"a job records only its load's documents")
		job, err := f.db.GetFoundryGraphJob(ctx, f.ontology.ID, jobID)
		require.NoError(t, err)
		assert.Equal(t, int64(1_000_000), job.Spend.USDMicros, "every attempt's spend counts")
		assert.Equal(t, 0, job.Progress.DocumentsDone, "progress is the attempt's to record")

		// The next load extracts the document afresh, into a row of its own.
		_, err = f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobSucceeded, Next: next("Q3")})
		require.NoError(t, err)
		second := f.load(t, "L2")
		pending, err := f.db.ListPendingFoundryDocuments(ctx, second)
		require.NoError(t, err)
		require.Len(t, pending, 1, "a new load owes every document")
		_, err = f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, second, "w1", failed, false))
		got, err = f.db.GetFoundryDocumentExtraction(ctx, jobID, doc)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionExtracted, got.Status, "the first load's row stays as it was")
		got, err = f.db.GetFoundryDocumentExtraction(ctx, second, doc)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionFailed, got.Status)
	})

	// A job sends what was never extracted into its load and what failed
	// there retryably; extracted and lost causes stay out.
	t.Run("lists the documents a load owes", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q4")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		done, retry, lost, fresh := f.uploaded(t, "a"), f.uploaded(t, "bb"), f.uploaded(t, "ccc"), f.uploaded(t, "dddd")
		f.created(t, foundryDocument(f.ontology, "eeeee"))
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", extracted(done), false))
		for doc, retryable := range map[string]bool{retry: true, lost: false} {
			e := extracted(doc)
			e.Status, e.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionTooLarge, Retryable: retryable}
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", e, false))
		}
		pending, err := f.db.ListPendingFoundryDocuments(ctx, jobID)
		require.NoError(t, err)
		var ids []string
		for _, d := range pending {
			ids = append(ids, d.ID)
		}
		assert.Equal(t, []string{retry, fresh}, ids, "oldest first; the pending upload is not in yet")
	})

	// A failed attempt waits and keeps the job live; a load whose tables are in skips Ingest next time.
	t.Run("requeues a retry with its phase", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q5")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		retryAt := time.Now().Add(time.Hour)
		finish, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{
			RetryAt: &retryAt, Error: "the agent is unavailable", IngestDone: true,
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobRetried, finish)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, model.FoundryGraphJobQueued, job.Status)
		assert.Equal(t, model.FoundryGraphJobDocuments, job.Phase)
		assert.Equal(t, "the agent is unavailable", *job.Error)
		assert.Equal(t, 1, job.Attempts, "a failed attempt counts against the job's attempts")
		assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, jobID), "the load is written until its job ends")
		none, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none, "a retry waits for its time")
	})

	// A deploy or a restart stops attempts that did nothing wrong, as does an
	// index the database is still building; counting them would fail a job
	// after a few of them.
	t.Run("gives back an uncounted attempt", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q6")
		jobID := f.load(t, "L1")
		claimed, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.Equal(t, 1, claimed.Attempts)
		retryAt := time.Now().Add(-time.Second)
		finish, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{
			RetryAt: &retryAt, Uncounted: true, Error: "the load was stopped; Helix will retry",
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobRetried, finish)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, 0, job.Attempts)
		again, err := f.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
		require.NoError(t, err)
		require.Equal(t, jobID, again.ID)
		assert.Equal(t, 1, again.Attempts, "the next attempt is the job's first again")
	})

	// An outcome the provider can't act on is refused before anything is written.
	t.Run("refuses an outcome it can't act on", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "Q7")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		for name, outcome := range map[string]model.FoundryGraphJobOutcome{
			"neither a retry nor an end": {Status: model.FoundryGraphJobQueued, Next: next("Q7")},
			"a cancel":                   {Status: model.FoundryGraphJobCancelled, Next: next("Q7")},
			"uncounted, but no retry":    {Status: model.FoundryGraphJobFailed, Uncounted: true, Next: next("Q7")},
			"an end with nothing next":   {Status: model.FoundryGraphJobFailed},
			"an end keeping no loads":    {Status: model.FoundryGraphJobFailed, Next: model.FoundryGraphNext{ExtractID: "x", DeleteID: "d"}},
		} {
			_, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", outcome)
			assert.Error(t, err, name)
		}
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, model.FoundryGraphJobRunning, job.Status)
	})
}

func TestFoundryGraphJobFinish(t *testing.T) {
	// Uploads during a job wait for it; finishing hands them to the next attempt, never leaving one behind.
	t.Run("requeues a job at its documents while uploads it has not recorded wait", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "F2")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		early := f.uploaded(t, "a")
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", extracted(early), false))
		late := f.uploaded(t, "bb")

		finish, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("F2a"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobMerged, finish)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, jobID, job.ID)
		assert.Equal(t, model.FoundryGraphJobLoad, job.Kind, "the user's load stays a load")
		assert.Equal(t, model.FoundryGraphJobDocuments, job.Phase, "the next attempt skips the tables")
		assert.Equal(t, 0, job.Attempts, "a merge is not a failed attempt")
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, jobID), "the load is ready once its tables and documents are in")
		assert.Equal(t, jobID, *f.ontologyNow(t).CurrentLoadID)

		claimed, err := f.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
		require.NoError(t, err)
		require.Equal(t, jobID, claimed.ID)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w2", extracted(late), false))
		finish, err = f.db.FinishFoundryGraphJob(ctx, jobID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, Next: next("F2b"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, jobID), "a load leaves writing once; a later end of its job changes nothing")
		assert.Nil(t, f.liveJob(t), "the uploads were merged, so nothing is owed")
	})

	// A load written before memberships, or by an old binary, has none: its
	// job sends none of the uploads, so it must not requeue for them either,
	// or it would be merged and sent nothing for ever. Making the load
	// current again has them join it, and an extract sends them.
	t.Run("ends a job whose load has no membership of its uploads", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "F2m")
		doc := f.uploaded(t, "a")
		jobID := f.load(t, "L1")
		_, err := testDB.Exec(`DELETE FROM foundry_load_documents WHERE load_id=?`, jobID)
		require.NoError(t, err)
		for attempt := range 2 {
			job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
			require.NoError(t, err)
			require.NotNil(t, job, "attempt %d", attempt)
			pending, err := f.db.ListPendingFoundryDocuments(ctx, jobID)
			require.NoError(t, err)
			require.Empty(t, pending, "a load sends only its members")
			if attempt == 0 {
				retryAt := time.Now().Add(-time.Second)
				_, err = f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{RetryAt: &retryAt, IngestDone: true})
				require.NoError(t, err)
				continue
			}
			finish, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{
				Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("F2m"),
			})
			require.NoError(t, err)
			assert.Equal(t, model.FoundryGraphJobEnded, finish, "nothing it could send is unrecorded")
		}
		assert.Nil(t, f.liveJob(t))
		queued, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, jobID, extractJob("F2m"), noBudget)
		require.NoError(t, err)
		require.True(t, queued, "the upload joins the load it waits for")
		pending, err := f.db.ListPendingFoundryDocuments(ctx, jobID)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, doc, pending[0].ID)
	})

	// A document the job recorded as a retryable failure waits for a later
	// job; requeueing for it would loop the job on a document that keeps failing.
	t.Run("ends a job whose documents all have a state", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "F3")
		jobID := f.load(t, "L1")
		_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		doc := f.uploaded(t, "a")
		e := extracted(doc)
		e.Status, e.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, jobID, "w1", e, false))
		finish, err := f.db.FinishFoundryGraphJob(ctx, jobID, "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next("F3"),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
	})

	// An upload owed by a job that ends without extracting it is queued into
	// the current load: else it waits for ever. The extract it queues owes
	// nothing itself, so a failing extract does not chain.
	t.Run("queues the extract a job owes, once", func(t *testing.T) {
		for _, status := range []model.FoundryGraphJobStatus{model.FoundryGraphJobFailed, model.FoundryGraphJobBudgetExhausted} {
			t.Run(string(status), func(t *testing.T) {
				f := newFoundryFixture(t, "F5")
				current := f.load(t, "L1")
				f.loaded(t, "w1")
				f.load(t, "L2")
				_, err := f.db.ClaimFoundryGraphJob(context.Background(), "w1", time.Minute)
				require.NoError(t, err)
				doc := f.uploaded(t, "a")
				require.True(t, f.ontologyNow(t).ExtractOwed)
				_, err = f.db.FinishFoundryGraphJob(context.Background(), id26("FJ-L2"), "w1", model.FoundryGraphJobOutcome{
					Status: status, Error: "stopped", Next: next("F5a"),
				})
				require.NoError(t, err)
				extract := f.liveJob(t)
				require.NotNil(t, extract)
				assert.Equal(t, id26("FN-X-F5a"), extract.ID)
				assert.Equal(t, model.FoundryGraphJobExtract, extract.Kind)
				assert.Equal(t, current, extract.LoadID, "into the current load, not the one that stopped")
				assert.Equal(t, id26("FV-L1"), extract.VersionID)
				assert.False(t, f.ontologyNow(t).ExtractOwed, "queueing it pays the debt")

				f.finish(t, "w1", "F5b", model.FoundryGraphJobFailed)
				assert.Nil(t, f.liveJob(t), "a failing extract queues no other")
				pending, err := f.db.ListPendingFoundryDocuments(context.Background(), current)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				assert.Equal(t, doc, pending[0].ID, "it waits for the next upload, change of current load or load")
			})
		}
	})

	// The owed extract is queued only where it can run: into a current load, within the month's budget.
	t.Run("owes nothing it can't queue", func(t *testing.T) {
		ctx := context.Background()
		t.Run("no current load", func(t *testing.T) {
			f := newFoundryFixture(t, "F6a")
			f.load(t, "L1")
			_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
			require.NoError(t, err)
			f.uploaded(t, "a")
			_, err = f.db.FinishFoundryGraphJob(ctx, id26("FJ-L1"), "w1", model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobFailed, Next: next("F6a")})
			require.NoError(t, err)
			assert.Nil(t, f.liveJob(t), "the next load extracts every document")
			assert.False(t, f.ontologyNow(t).ExtractOwed, "the debt is dropped, not carried")
		})
		t.Run("a month spent", func(t *testing.T) {
			f := newFoundryFixture(t, "F6b")
			f.load(t, "L1")
			f.loaded(t, "w1")
			f.load(t, "L2")
			_, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
			require.NoError(t, err)
			f.uploaded(t, "a")
			spent := next("F6b")
			spent.Budget = model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 0}
			_, err = f.db.FinishFoundryGraphJob(ctx, id26("FJ-L2"), "w1", model.FoundryGraphJobOutcome{Status: model.FoundryGraphJobFailed, Next: spent})
			require.NoError(t, err)
			assert.Nil(t, f.liveJob(t))
		})
	})

	// Completing an upload and finishing the job race: whichever commits first, the document is extracted.
	t.Run("leaves no upload stranded by a concurrent finish", func(t *testing.T) {
		for round := range 4 {
			ctx := context.Background()
			f := newFoundryFixture(t, fmt.Sprintf("R%d", round))
			f.load(t, "L1")
			f.loaded(t, "w1")
			first := f.uploaded(t, "a")
			job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
			require.NoError(t, err)
			require.NotNil(t, job, "the upload queued an extract")
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(first), false))
			f.created(t, foundryDocument(f.ontology, "bb"))

			var wg sync.WaitGroup
			var finish model.FoundryGraphJobFinish
			var reads []model.FoundryDocumentRead
			var finishErr, uploadErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				finish, finishErr = f.db.FinishFoundryGraphJob(ctx, job.ID, "w1", model.FoundryGraphJobOutcome{
					Status: model.FoundryGraphJobSucceeded, Next: next(fmt.Sprintf("R%d", round)),
				})
			}()
			go func() {
				defer wg.Done()
				_, reads, uploadErr = f.db.CompleteFoundryDocumentUpload(ctx, f.ontology.WorkspaceID, id26("FD-bb"), f.fan("bb"))
			}()
			wg.Wait()
			require.NoError(t, finishErr)
			require.NoError(t, uploadErr)
			require.Len(t, reads, 1)
			queued := reads[0].Outcome == model.FoundryDocumentReadQueued
			require.NotNil(t, f.liveJob(t), "round %d: finish %s, read %s: a job must be left for the upload", round, finish, reads[0].Outcome)
			assert.True(t, finish == model.FoundryGraphJobMerged || queued, "round %d: finish %s, read %s", round, finish, reads[0].Outcome)
		}
	})
}
