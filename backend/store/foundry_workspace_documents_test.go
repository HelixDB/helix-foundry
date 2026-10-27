package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"sort"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sharedFixture is two ontologies of one workspace that may read one set.
type sharedFixture struct {
	a, b   foundryFixture
	shared string
}

func newSharedFixture(t *testing.T, tag string) sharedFixture {
	t.Helper()
	a := newFoundryFixture(t, tag+"A")
	b := foundryFixture{db: a.db, ontology: sampleOntology(t, a.db, a.ontology.WorkspaceID, tag+"B")}
	set := a.documentSet(tag + "-shared")
	require.NoError(t, a.db.CreateFoundryDocumentSet(context.Background(), set, 100))
	return sharedFixture{a: a, b: b, shared: set.ID}
}

// readsByOntology are reads keyed by ontology, for order-free comparison.
func readsByOntology(reads []model.FoundryDocumentRead) map[string]model.FoundryDocumentReadOutcome {
	out := map[string]model.FoundryDocumentReadOutcome{}
	for _, r := range reads {
		out[r.OntologyID] = r.Outcome
	}
	return out
}

func TestFoundryVersionScope(t *testing.T) {
	// A version's scope names only live sets of its workspace, once each and
	// in the order given: a set deleted since it was picked holds nothing,
	// and an id a revision's routes made up names nothing of the workspace.
	t.Run("stores only live sets of the workspace", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SCOPE1")
		general := generalSetID(f.ontology.ID)
		deleted := f.documentSet("deleted")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, deleted, 20))
		require.NoError(t, f.db.DeleteFoundryDocumentSet(ctx, f.ontology.WorkspaceID, deleted.ID))
		foreign := sampleOntology(t, f.db, id26("FWS-SCOPE1-other"), "SCOPE1-other")
		hidden, _ := f.hidden(t, "SCOPE1-h")
		picked := []string{general, deleted.ID, generalSetID(foreign.ID), "made-up", hidden, general}
		v := &model.OntologyVersion{
			ID: id26("FV-SCOPE1"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
			Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), DocumentSetIDs: &picked,
		}
		require.NoError(t, f.db.CreateOntologyVersion(ctx, v))
		assert.Equal(t, []string{general}, *v.DocumentSetIDs, "the caller sees what is stored")
		stored, err := f.db.GetOntologyVersion(ctx, f.ontology.ID, v.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.DocumentSetIDs)
		assert.Equal(t, []string{general}, *stored.DocumentSetIDs)
		listed, err := f.db.ListOntologyVersions(ctx, f.ontology.ID, 10)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, []string{general}, *listed[0].DocumentSetIDs, "lists carry the scope")

		none := &model.OntologyVersion{
			ID: id26("FV-SCOPE1-none"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceManual,
			Format: model.OntologyFormatFoundryV1, Definition: json.RawMessage(`{}`), DocumentSetIDs: &[]string{},
		}
		require.NoError(t, f.db.CreateOntologyVersion(ctx, none))
		scope, err := f.db.GetOntologyVersionScope(ctx, f.ontology.ID, none.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{}, scope, "an empty scope reads none")

		legacyless := &model.OntologyVersion{
			ID: id26("FV-SCOPE1-nil"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceManual,
			Format: model.OntologyFormatFoundryV1, Definition: json.RawMessage(`{}`),
		}
		assert.ErrorContains(t, f.db.CreateOntologyVersion(ctx, legacyless), "no document scope", "every new version has a scope")
	})

	// A version from before scopes reads its ontology's live sets, the ones
	// still naming it, oldest first: what it read before release 1.
	t.Run("reads a legacy version as its ontology's live sets", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SCOPE2")
		v := f.version(t, "legacy")
		_, err := testDB.Exec(`UPDATE foundry_ontology_versions SET document_set_ids=NULL WHERE id=?`, v)
		require.NoError(t, err)
		stored, err := f.db.GetOntologyVersion(ctx, f.ontology.ID, v)
		require.NoError(t, err)
		assert.Nil(t, stored.DocumentSetIDs, "legacy")
		scope, err := f.db.GetOntologyVersionScope(ctx, f.ontology.ID, v)
		require.NoError(t, err)
		assert.Empty(t, scope, "the fixture's set is the workspace's: it names no ontology")

		for _, tag := range []string{"old1", "old2"} {
			_, err := testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, ontology_id, name, live_key) VALUES (?, ?, ?, ?, 1)`,
				id26("FS-"+tag), f.ontology.WorkspaceID, f.ontology.ID, tag)
			require.NoError(t, err)
			time.Sleep(2 * time.Millisecond)
		}
		_, err = testDB.Exec(`UPDATE foundry_document_sets SET deleted_at=NOW(6), live_key=NULL WHERE id=?`, id26("FS-old2"))
		require.NoError(t, err)
		_, err = testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, ontology_id, name, live_key) VALUES (?, ?, ?, 'old3', 1)`,
			id26("FS-old3"), f.ontology.WorkspaceID, f.ontology.ID)
		require.NoError(t, err)
		scope, err = f.db.GetOntologyVersionScope(ctx, f.ontology.ID, v)
		require.NoError(t, err)
		assert.Equal(t, []string{id26("FS-old1"), id26("FS-old3")}, scope)
		_, err = f.db.GetOntologyVersionScope(ctx, f.ontology.ID, id26("FV-none"))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// Reading what a version routes needs only its mappings, not the
	// megabytes of its draft.
	t.Run("reads a version's mappings alone", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SCOPE3")
		v := f.versionReading(t, "routes", generalSetID(f.ontology.ID))
		mappings, err := f.db.GetOntologyVersionMappings(ctx, f.ontology.ID, v)
		require.NoError(t, err)
		assert.Contains(t, mappings, "[extractions.docs]")
		manual := &model.OntologyVersion{
			ID: id26("FV-SCOPE3-v1"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceManual,
			Format: model.OntologyFormatFoundryV1, Definition: json.RawMessage(`{"mappings_toml":"x"}`), DocumentSetIDs: &[]string{},
		}
		require.NoError(t, f.db.CreateOntologyVersion(ctx, manual))
		mappings, err = f.db.GetOntologyVersionMappings(ctx, f.ontology.ID, manual.ID)
		require.NoError(t, err)
		assert.Empty(t, mappings, "a Definition v1 routes nothing")
		bare := f.version(t, "bare")
		mappings, err = f.db.GetOntologyVersionMappings(ctx, f.ontology.ID, bare)
		require.NoError(t, err)
		assert.Empty(t, mappings, "a draft without mappings routes nothing")
		_, err = f.db.GetOntologyVersionMappings(ctx, f.ontology.ID, id26("FV-none"))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestFoundryLoadScope(t *testing.T) {
	// A load's documents are the uploaded documents of its version's scope,
	// whatever set or ontology they came from; nothing else joins it.
	t.Run("snapshots the uploaded documents of the version's scope", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "LS1")
		inShared := s.a.documentIn("shared", s.shared)
		s.a.created(t, inShared)
		_, _, err := s.a.db.CompleteFoundryDocumentUpload(ctx, s.a.ontology.WorkspaceID, inShared.ID, s.a.fan("shared"))
		require.NoError(t, err)
		ownA := s.a.uploaded(t, "ownA")
		ownB := s.b.uploaded(t, "ownB")
		s.a.created(t, s.a.documentIn("pending", s.shared))
		load := s.a.loadReading(t, "L1", s.shared, generalSetID(s.a.ontology.ID))
		pending, err := s.a.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		var ids []string
		for _, d := range pending {
			ids = append(ids, d.ID)
		}
		sort.Strings(ids)
		want := []string{inShared.ID, ownA}
		sort.Strings(want)
		assert.Equal(t, want, ids, "not %s, of a set it doesn't read, nor one waiting for its bytes", ownB)

		empty := s.b.loadReading(t, "L2")
		pending, err = s.b.db.ListPendingFoundryDocuments(ctx, empty)
		require.NoError(t, err)
		assert.Empty(t, pending, "a version with no sets reads no document")
	})

	// A job writes only the documents of its load, so a document of another
	// set can't land in it by a worker's mistake.
	t.Run("records only a document of the job's load", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "LS2")
		ownB := s.b.uploaded(t, "ownB")
		job := s.a.load(t, "L1")
		ownA := s.a.uploaded(t, "ownA")
		_, err := s.a.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.a.db.RecordFoundryDocumentExtraction(ctx, job, "w1", extracted(ownA), false))
		assert.ErrorContains(t, s.a.db.RecordFoundryDocumentExtraction(ctx, job, "w1", extracted(ownB), false), "did not join")
		holds, err := s.a.db.FoundryLoadHoldsDocument(ctx, job, ownA)
		require.NoError(t, err)
		assert.True(t, holds)
		holds, err = s.a.db.FoundryLoadHoldsDocument(ctx, job, ownB)
		require.NoError(t, err)
		assert.False(t, holds)
	})
}

func TestFoundryDocumentFanOut(t *testing.T) {
	// An upload into a set two ontologies read is read by both, each into
	// its own current load, as one upload into one ontology always was.
	t.Run("reads an upload into every ontology that reads its set", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "FAN1")
		ws := s.a.ontology.WorkspaceID
		loadA := s.a.loadReading(t, "LA", s.shared)
		s.a.loaded(t, "w1")
		loadB := s.b.loadReading(t, "LB", s.shared, generalSetID(s.b.ontology.ID))
		s.b.loaded(t, "w1")
		third := foundryFixture{db: s.a.db, ontology: sampleOntology(t, s.a.db, ws, "FAN1C")}
		third.loadReading(t, "LC", generalSetID(third.ontology.ID))
		third.loaded(t, "w1")

		doc := s.a.documentIn("shared", s.shared)
		s.a.created(t, doc)
		completed, reads, err := s.a.db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, s.a.fan("FAN1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, completed.Status)
		assert.Equal(t, map[string]model.FoundryDocumentReadOutcome{
			s.a.ontology.ID: model.FoundryDocumentReadQueued, s.b.ontology.ID: model.FoundryDocumentReadQueued,
		}, readsByOntology(reads), "not the third, whose load doesn't read the set")
		jobA, jobB := s.a.liveJob(t), s.b.liveJob(t)
		require.NotNil(t, jobA)
		require.NotNil(t, jobB)
		assert.Equal(t, loadA, jobA.LoadID)
		assert.Equal(t, loadB, jobB.LoadID)
		assert.NotEqual(t, jobA.ID, jobB.ID, "an extract each, against the one monthly budget")
		assert.Nil(t, third.liveJob(t))
		for _, load := range []string{loadA, loadB} {
			pending, err := s.a.db.ListPendingFoundryDocuments(ctx, load)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, doc.ID, pending[0].ID)
		}
	})

	// A load being written whose version reads the set takes the upload,
	// even when the current load's version doesn't; and a current load that
	// reads it while another version is being loaded is owed it.
	t.Run("targets the live job's load or the current load, each by its own version", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "FAN2")
		ws := s.a.ontology.WorkspaceID
		// A: current load A1 reads only its own set; A2, being written, reads the shared one.
		// B: current load B1 reads the shared set; B2, of a version that doesn't, is being written.
		// Both current loads end before either load being written is queued, so neither is claimed.
		s.a.loadReading(t, "A1", generalSetID(s.a.ontology.ID))
		s.a.loaded(t, "w1")
		currentB := s.b.loadReading(t, "B1", s.shared)
		s.b.loaded(t, "w1")
		writing := s.a.loadReading(t, "A2", s.shared)
		s.b.loadReading(t, "B2", generalSetID(s.b.ontology.ID))

		doc := s.a.documentIn("shared", s.shared)
		s.a.created(t, doc)
		_, reads, err := s.a.db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, s.a.fan("FAN2"))
		require.NoError(t, err)
		assert.Equal(t, map[string]model.FoundryDocumentReadOutcome{
			s.a.ontology.ID: model.FoundryDocumentReadOwed, s.b.ontology.ID: model.FoundryDocumentReadOwed,
		}, readsByOntology(reads))
		pending, err := s.a.db.ListPendingFoundryDocuments(ctx, writing)
		require.NoError(t, err)
		require.Len(t, pending, 1, "A's load being written takes it")
		assert.True(t, s.a.ontologyNow(t).ExtractOwed)
		assert.True(t, s.b.ontologyNow(t).ExtractOwed, "B's live job's end extracts it into the current load")
		pending, err = s.b.db.ListPendingFoundryDocuments(ctx, currentB)
		require.NoError(t, err)
		assert.Empty(t, pending, "it joins the load being written; the owed extract snapshots the current load")

		// B's load fails, so B1 stays current, and its owed extract reads the document.
		job, err := s.b.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
		require.NoError(t, err)
		for job != nil && job.OntologyID != s.b.ontology.ID {
			job, err = s.b.db.ClaimFoundryGraphJob(ctx, "w2", time.Minute)
			require.NoError(t, err)
		}
		require.NotNil(t, job)
		_, err = s.b.db.FinishFoundryGraphJob(ctx, job.ID, "w2", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobFailed, IngestDone: true, Next: next("FAN2B"),
		})
		require.NoError(t, err)
		extract := s.b.liveJob(t)
		require.NotNil(t, extract)
		assert.Equal(t, currentB, extract.LoadID)
		pending, err = s.b.db.ListPendingFoundryDocuments(ctx, currentB)
		require.NoError(t, err)
		require.Len(t, pending, 1)
	})

	// The caller may write some ontologies' databases and not others: those
	// are skipped, not owed, so no write is queued on their behalf.
	t.Run("skips an ontology whose database the caller can't write", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "FAN3")
		ws := s.a.ontology.WorkspaceID
		s.a.loadReading(t, "LA", s.shared)
		s.a.loaded(t, "w1")
		s.b.loadReading(t, "LB1", s.shared)
		s.b.loaded(t, "w1")
		s.b.loadReading(t, "LB2", s.shared)
		doc := s.a.documentIn("shared", s.shared)
		s.a.created(t, doc)
		fan := s.a.fan("FAN3")
		fan.CanWrite = func(projectID string) bool { return projectID == s.a.ontology.ProjectID }
		_, reads, err := s.a.db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, fan)
		require.NoError(t, err)
		assert.Equal(t, map[string]model.FoundryDocumentReadOutcome{
			s.a.ontology.ID: model.FoundryDocumentReadQueued, s.b.ontology.ID: model.FoundryDocumentReadSkipped,
		}, readsByOntology(reads))
		assert.False(t, s.b.ontologyNow(t).ExtractOwed, "skipped, not owed")
		pending, err := s.b.db.ListPendingFoundryDocuments(ctx, id26("FJ-LB2"))
		require.NoError(t, err)
		assert.Empty(t, pending, "nor joined to its load being written")
	})

	// An ontology deleted before the upload completes reads nothing of it,
	// and one deleted while it waits for the workspace's lock is skipped.
	t.Run("skips a deleted ontology", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "FAN4")
		ws := s.a.ontology.WorkspaceID
		s.a.loadReading(t, "LA", s.shared)
		s.a.loaded(t, "w1")
		s.b.loadReading(t, "LB", s.shared)
		s.b.loaded(t, "w1")
		doc := s.a.documentIn("shared", s.shared)
		s.a.created(t, doc)

		// Hold B's row as a delete does, then complete: it waits for B, which is then gone.
		tx, err := testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var locked string
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM foundry_ontologies WHERE id=? FOR UPDATE`, s.b.ontology.ID).Scan(&locked))
		done := make(chan []model.FoundryDocumentRead, 1)
		go func() {
			_, reads, err := s.a.db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, s.a.fan("FAN4"))
			assert.NoError(t, err)
			done <- reads
		}()
		time.Sleep(200 * time.Millisecond)
		_, err = tx.ExecContext(ctx, `UPDATE foundry_ontologies SET deleted_at=NOW(6), current_load_id=NULL, live_key=NULL WHERE id=?`, s.b.ontology.ID)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		reads := <-done
		assert.Equal(t, map[string]model.FoundryDocumentReadOutcome{s.a.ontology.ID: model.FoundryDocumentReadQueued}, readsByOntology(reads))
		assert.Nil(t, s.b.liveJob(t))
	})

	// A move is an upload into the destination set: the ontologies that
	// read it get the document, unless the load already holds it.
	t.Run("reads a moved document as an upload into its new set", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "FAN5")
		ws := s.a.ontology.WorkspaceID
		s.a.loadReading(t, "LA", s.shared, generalSetID(s.a.ontology.ID))
		s.a.loaded(t, "w1")
		s.b.loadReading(t, "LB", s.shared)
		s.b.loaded(t, "w1")
		doc := s.a.uploaded(t, "doc")
		extract := s.a.liveJob(t)
		require.NotNil(t, extract)
		job, _ := s.a.finish(t, "w1", "FAN5A", model.FoundryGraphJobSucceeded, doc)
		require.Equal(t, extract.ID, job.ID)
		require.Nil(t, s.a.liveJob(t))

		_, reads, err := s.a.db.MoveFoundryDocument(ctx, ws, doc, s.shared, s.a.fan("FAN5"))
		require.NoError(t, err)
		assert.Equal(t, map[string]model.FoundryDocumentReadOutcome{
			s.a.ontology.ID: model.FoundryDocumentReadHeld, s.b.ontology.ID: model.FoundryDocumentReadQueued,
		}, readsByOntology(reads), "A's load holds it already; B's reads it now")
		assert.Nil(t, s.a.liveJob(t))
		require.NotNil(t, s.b.liveJob(t))
	})
}

func TestFoundryDocumentFanOutByID(t *testing.T) {
	// A fan-out joins what its transaction added, not its whole scope: a
	// document an earlier caller could not send waits for the end of a job
	// that owes an extract, which joins the rest of the scope once.
	t.Run("joins only the documents it adds, and the scope at a job's end", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FAN6")
		ws := f.ontology.WorkspaceID
		load := f.load(t, "L1")
		f.loaded(t, "w1")
		pendingIDs := func() []string {
			t.Helper()
			pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
			require.NoError(t, err)
			return documentIDs(pending)
		}

		skipped := f.created(t, foundryDocument(f.ontology, "skip"))
		fan := f.fan("skip")
		fan.CanWrite = func(string) bool { return false }
		_, reads, err := f.db.CompleteFoundryDocumentUpload(ctx, ws, skipped.ID, fan)
		require.NoError(t, err)
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: f.ontology.ID, Outcome: model.FoundryDocumentReadSkipped}}, reads)
		assert.Empty(t, pendingIDs(), "a skipped document joins nothing")

		a := f.uploaded(t, "a")
		assert.Equal(t, []string{a}, pendingIDs(), "the next upload joins alone")
		require.Equal(t, id26("FX-a"), f.liveJob(t).ID)
		bb := f.uploaded(t, "bb")
		assert.True(t, f.ontologyNow(t).ExtractOwed)

		_, finish := f.finish(t, "w2", "FAN6", model.FoundryGraphJobSucceeded, a, bb)
		assert.Equal(t, model.FoundryGraphJobEnded, finish)
		owed := f.liveJob(t)
		require.NotNil(t, owed)
		assert.Equal(t, id26("FN-X-FAN6"), owed.ID)
		assert.Nil(t, owed.RequestedByUserID, "an owed extract is work no one started")
		assert.Equal(t, []string{skipped.ID}, pendingIDs(), "the job's end joins the rest of the scope")
	})
}

// Completing an upload locks every live ontology of the workspace before
// the document, as CreateFoundryLoad locks its ontology before reading
// documents: run together, neither waits for what the other holds, so no
// deadlock ends either.
func TestFoundryDocumentFanOutLockOrder(t *testing.T) {
	ctx := context.Background()
	s := newSharedFixture(t, "LOCK")
	ws := s.a.ontology.WorkspaceID
	s.a.loadReading(t, "LA", s.shared)
	s.a.loaded(t, "w1")
	s.b.loadReading(t, "LB", s.shared)
	s.b.loaded(t, "w1")
	for round := range 8 {
		doc := s.a.documentIn(fmt.Sprintf("r%d", round), s.shared)
		s.a.created(t, doc)
		// Leave no job live, so both loads can be created.
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET status='cancelled', active_key=NULL, finished_at=NOW(6) WHERE workspace_id=? AND active_key=1`, ws)
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_loads SET status='failed', finished_at=NOW(6) WHERE workspace_id=? AND status='writing'`, ws)
		require.NoError(t, err)
		loads := make([]*model.FoundryGraphJob, 2)
		for i, f := range []foundryFixture{s.a, s.b} {
			tag := fmt.Sprintf("LOCK%d-%d", round, i)
			f.versionReading(t, tag, s.shared)
			loads[i] = loadJob(f.ontology, tag)
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _, errs[0] = s.a.db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, s.a.fan(fmt.Sprintf("LOCK%d", round)))
		}()
		for i, f := range []foundryFixture{s.a, s.b} {
			go func() {
				defer wg.Done()
				errs[i+1] = f.db.CreateFoundryLoad(ctx, loads[i])
			}()
		}
		wg.Wait()
		for _, err := range errs {
			var me *gomysql.MySQLError
			if errors.As(err, &me) {
				assert.NotEqual(t, uint16(1213), me.Number, "round %d: deadlock", round)
			}
			if err != nil && !errors.Is(err, store.ErrConflict) {
				assert.NoError(t, err, "round %d", round)
			}
		}
	}
}

func TestFoundryOntologyDocuments(t *testing.T) {
	// An ontology lists what it reads: its current load's documents, and the
	// uploaded documents its reading version's scope holds; filters keep
	// those its current load holds so.
	t.Run("lists the current load's documents and the reading version's", func(t *testing.T) {
		ctx := context.Background()
		s := newSharedFixture(t, "OD1")
		ws := s.a.ontology.WorkspaceID
		general := generalSetID(s.a.ontology.ID)
		read, failed, review := s.a.uploaded(t, "read"), s.a.uploaded(t, "failed"), s.a.uploaded(t, "review")
		load := s.a.loadReading(t, "L1", general)
		job, err := s.a.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.a.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(read), false))
		e := extracted(failed)
		e.Status, e.Error = model.FoundryExtractionFailed, &model.FoundryExtractionError{Code: model.FoundryExtractionModel}
		require.NoError(t, s.a.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", e, false))
		withReviews := extracted(review)
		withReviews.Counts.Reviews = 2
		require.NoError(t, s.a.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", withReviews, false))
		s.a.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("OD1"))
		// Moved out of scope, it stays in the load; uploaded later into
		// another set, it is no one's; uploaded in scope, it waits.
		_, _, err = s.a.db.MoveFoundryDocument(ctx, ws, read, s.shared, s.a.fan("OD1-move"))
		require.NoError(t, err)
		outside := s.a.documentIn("outside", s.shared)
		s.a.created(t, outside)
		_, _, err = s.a.db.CompleteFoundryDocumentUpload(ctx, ws, outside.ID, s.a.fan("OD1-out"))
		require.NoError(t, err)
		waiting := s.a.uploaded(t, "waiting")
		s.a.created(t, s.a.documentIn("pending", general))

		list := func(filter model.FoundryDocumentFilter) []string {
			t.Helper()
			docs, _, err := s.a.db.ListFoundryOntologyDocuments(ctx, ws, load, []string{general}, filter, "", 50)
			require.NoError(t, err)
			var ids []string
			for _, d := range docs {
				ids = append(ids, d.ID)
			}
			sort.Strings(ids)
			return ids
		}
		sorted := func(ids ...string) []string { sort.Strings(ids); return ids }
		assert.Equal(t, sorted(read, failed, review, waiting), list(model.FoundryDocumentFilterAll))
		assert.Equal(t, sorted(waiting), list(model.FoundryDocumentFilterWaiting))
		assert.Equal(t, sorted(read, review), list(model.FoundryDocumentFilterExtracted))
		assert.Equal(t, sorted(review), list(model.FoundryDocumentFilterReview))
		assert.Equal(t, sorted(failed), list(model.FoundryDocumentFilterFailed))
		assert.Empty(t, list(model.FoundryDocumentFilterDuplicate))
		_, _, err = s.a.db.ListFoundryOntologyDocuments(ctx, ws, load, nil, "bogus", "", 50)
		assert.Error(t, err)

		// With no load current, the reading version's scope alone.
		docs, _, err := s.b.db.ListFoundryOntologyDocuments(ctx, ws, "", []string{s.shared}, model.FoundryDocumentFilterAll, "", 50)
		require.NoError(t, err)
		var ids []string
		for _, d := range docs {
			ids = append(ids, d.ID)
		}
		assert.Equal(t, sorted(read, outside.ID), sorted(ids...))
		docs, _, err = s.b.db.ListFoundryOntologyDocuments(ctx, ws, "", nil, model.FoundryDocumentFilterAll, "", 50)
		require.NoError(t, err)
		assert.Empty(t, docs, "nothing reads its uploads yet")

		// Pages newest first.
		page, next, err := s.a.db.ListFoundryOntologyDocuments(ctx, ws, load, []string{general}, model.FoundryDocumentFilterAll, "", 2)
		require.NoError(t, err)
		require.Len(t, page, 2)
		assert.Equal(t, waiting, page[0].ID)
		rest, last, err := s.a.db.ListFoundryOntologyDocuments(ctx, ws, load, []string{general}, model.FoundryDocumentFilterAll, next, 2)
		require.NoError(t, err)
		assert.Len(t, rest, 2)
		assert.Empty(t, last)
	})
}

func TestFoundryOntologyRelease(t *testing.T) {
	// An ontology comes with no sets: they are the workspace's.
	t.Run("creates an ontology with no set", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, id26("FWS-REL1"))
		o := &model.FoundryOntology{ID: id26("FO-REL1"), WorkspaceID: id26("FWS-REL1"), Name: "x", ProjectID: "p", TenantID: id26("FT-REL1"),
			ClientToken: "rel1"}
		require.NoError(t, db.CreateFoundryOntology(ctx, o))
		sets, err := db.ListFoundryDocumentSets(ctx, o.WorkspaceID)
		require.NoError(t, err)
		assert.Empty(t, sets)
	})

	// Deleting an ontology keeps its sets and documents, the workspace's
	// now: they name it no more, so nothing hides them.
	t.Run("releases its sets and documents on delete", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "REL2")
		doc := f.uploaded(t, "a")
		_, err := testDB.Exec(`UPDATE foundry_document_sets SET ontology_id=? WHERE id=?`, f.ontology.ID, generalSetID(f.ontology.ID))
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_documents SET ontology_id=? WHERE id=?`, f.ontology.ID, doc)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FN-D-REL2")))
		var owners int
		require.NoError(t, testDB.QueryRow(`SELECT (SELECT COUNT(*) FROM foundry_document_sets WHERE ontology_id=?) +
  (SELECT COUNT(*) FROM foundry_documents WHERE ontology_id=?)`, f.ontology.ID, f.ontology.ID).Scan(&owners))
		assert.Zero(t, owners)
		got, err := f.db.GetFoundryDocument(ctx, f.ontology.WorkspaceID, doc)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, got.Status)
		_, err = f.db.GetFoundryDocumentSet(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID))
		require.NoError(t, err)
	})
}

func TestFoundryDataLayout(t *testing.T) {
	// A workspace with no layout reads {} at revision 0; each write names
	// the revision it read and moves it on by one.
	t.Run("writes over the revision read", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("FWS-LAYOUT1")
		l, err := db.GetFoundryDataLayout(ctx, ws)
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(l.Layout))
		assert.Zero(t, l.Revision)
		user := id26("FU-LAYOUT")
		first := &model.FoundryDataLayout{WorkspaceID: ws, Layout: json.RawMessage(`{"v":1,"order":{"root":["a"]}}`), UpdatedByUserID: &user}
		require.NoError(t, db.PutFoundryDataLayout(ctx, first, 0))
		assert.Equal(t, int64(1), first.Revision)
		second := &model.FoundryDataLayout{WorkspaceID: ws, Layout: json.RawMessage(`{"v":1,"order":{"root":["b"]}}`)}
		require.NoError(t, db.PutFoundryDataLayout(ctx, second, 1))
		assert.Equal(t, int64(2), second.Revision)
		assert.ErrorIs(t, db.PutFoundryDataLayout(ctx, second, 1), store.ErrConflict, "a stale revision")
		assert.ErrorIs(t, db.PutFoundryDataLayout(ctx, second, 0), store.ErrConflict, "a first write over one")
		l, err = db.GetFoundryDataLayout(ctx, ws)
		require.NoError(t, err)
		assert.JSONEq(t, `{"v":1,"order":{"root":["b"]}}`, string(l.Layout))
		assert.Equal(t, int64(2), l.Revision)
		assert.Nil(t, l.UpdatedByUserID, "the last writer")
		other, err := db.GetFoundryDataLayout(ctx, id26("FWS-LAYOUT2"))
		require.NoError(t, err)
		assert.Zero(t, other.Revision, "layouts are per workspace")
	})

	// Two tabs writing the same revision at once: one wins, the other is
	// told to read again, first write included.
	t.Run("lets one of two racing writes in", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ws := id26("FWS-LAYOUT3")
		for _, expected := range []int64{0, 1} {
			var wg sync.WaitGroup
			errs := make([]error, 4)
			for i := range errs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errs[i] = db.PutFoundryDataLayout(ctx, &model.FoundryDataLayout{WorkspaceID: ws, Layout: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))},
						expected)
				}()
			}
			wg.Wait()
			won := 0
			for _, err := range errs {
				if err == nil {
					won++
					continue
				}
				assert.ErrorIs(t, err, store.ErrConflict)
			}
			assert.Equal(t, 1, won, "from revision %d", expected)
		}
		l, err := db.GetFoundryDataLayout(ctx, ws)
		require.NoError(t, err)
		assert.Equal(t, int64(2), l.Revision)
	})
}
