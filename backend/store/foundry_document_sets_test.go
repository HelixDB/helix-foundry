package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// documentSet is a set FS-<tag> of the fixture's workspace.
func (f foundryFixture) documentSet(tag string) *model.FoundryDocumentSet {
	return &model.FoundryDocumentSet{ID: id26("FS-" + tag), WorkspaceID: f.ontology.WorkspaceID, Name: "Set " + tag}
}

// documentIn is a document FD-<tag> of the fixture's workspace in set setID.
func (f foundryFixture) documentIn(tag, setID string) *model.FoundryDocument {
	d := foundryDocument(f.ontology, tag)
	d.SetID = setID
	return d
}

// created writes d, which must be new, and returns it as stored.
func (f foundryFixture) created(t *testing.T, d *model.FoundryDocument) *model.FoundryDocument {
	t.Helper()
	stored, existing, err := f.db.CreateFoundryDocument(context.Background(), d, noCap)
	require.NoError(t, err)
	require.False(t, existing, "a new document")
	return stored
}

// setNames lists the workspace's live sets as name=documents, oldest first.
func (f foundryFixture) setNames(t *testing.T) []string {
	t.Helper()
	sets, err := f.db.ListFoundryDocumentSets(context.Background(), f.ontology.WorkspaceID)
	require.NoError(t, err)
	var out []string
	for _, s := range sets {
		out = append(out, fmt.Sprintf("%s=%d", s.Name, s.Documents))
	}
	return out
}

// hidden writes, as rows from before documents were the workspace's, a live
// set FS-<tag> and an uploaded document FD-<tag> in it of an ontology of the
// fixture's workspace that is then deleted, without the delete that release
// 1 does: the rows still name the ontology.
func (f foundryFixture) hidden(t *testing.T, tag string) (setID, docID string) {
	t.Helper()
	gone := sampleOntology(t, f.db, f.ontology.WorkspaceID, tag+"-gone")
	setID, docID = id26("FS-"+tag), id26("FD-"+tag)
	_, err := testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, ontology_id, name, live_key) VALUES (?, ?, ?, ?, 1)`,
		setID, f.ontology.WorkspaceID, gone.ID, "Hidden "+tag)
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_documents (id, workspace_id, ontology_id, set_id, name, media_type, size_bytes, sha256, client_token,
  status, s3_key, uploaded_at) VALUES (?, ?, ?, ?, 'hidden.md', 'text/markdown', 1, ?, ?, 'uploaded', 'k', NOW(6))`,
		docID, f.ontology.WorkspaceID, gone.ID, setID, fmt.Sprintf("%064x", 999), "doc-"+tag)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET deleted_at=NOW(6), live_key=NULL WHERE id=?`, gone.ID)
	require.NoError(t, err)
	return setID, docID
}

// connectorSet writes connector CONN-<tag> of the fixture's workspace, named
// name, and its set FS-<tag> of stream "notes", named Notes, as the
// connector's sync makes it.
func (f foundryFixture) connectorSet(t *testing.T, tag, name string) *model.FoundryDocumentSet {
	t.Helper()
	c := &model.Connector{
		ID: id26("CONN-" + tag), WorkspaceID: f.ontology.WorkspaceID, Kind: "granola", Name: name, Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte{1}, SecretKeyID: "k1", SecretHint: "grn_…1",
	}
	require.NoError(t, f.db.CreateConnector(context.Background(), c))
	set := &model.FoundryDocumentSet{
		ID: id26("FS-" + tag), WorkspaceID: f.ontology.WorkspaceID, Name: "Notes", Description: "Meeting notes",
		Owner: &model.FoundryDocumentSetOwner{ConnectorID: c.ID, ConnectorKind: c.Kind, ConnectorName: name, Stream: "notes"},
	}
	_, err := testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, connector_id, stream, name, description, live_key)
VALUES (?, ?, ?, ?, ?, ?, 1)`, set.ID, set.WorkspaceID, c.ID, set.Owner.Stream, set.Name, set.Description)
	require.NoError(t, err)
	return set
}

// connectorDocument writes an uploaded document FD-<tag> of a connector's
// set for its row externalID, created at the source at, as the connector's
// sync writes it, with the bytes of foundryDocument's FD-<tag>.
func (f foundryFixture) connectorDocument(t *testing.T, tag, setID, externalID string, at time.Time) string {
	t.Helper()
	d := foundryDocument(f.ontology, tag)
	_, err := testDB.Exec(`INSERT INTO foundry_documents (id, workspace_id, set_id, external_id, source_version, source_at, name, media_type,
  size_bytes, sha256, status, s3_key, uploaded_at) VALUES (?, ?, ?, ?, '1|v', ?, ?, ?, ?, ?, 'uploaded', ?, NOW(6))`,
		d.ID, d.WorkspaceID, setID, externalID, at.UTC(), d.Name, string(d.MediaType), d.SizeBytes, d.SHA256, d.S3Key)
	require.NoError(t, err)
	return d.ID
}

// documentIDs are the ids of docs, in order.
func documentIDs(docs []*model.FoundryDocument) []string {
	ids := make([]string, len(docs))
	for i, d := range docs {
		ids[i] = d.ID
	}
	return ids
}

func TestFoundryDocumentSets(t *testing.T) {
	// Names tell a workspace's folders apart to the user and to the agent,
	// so live ones never share one, in the column's collation.
	t.Run("creates, renames and counts sets with names unique per workspace", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET2")
		contracts := f.documentSet("contracts")
		contracts.Name, contracts.Description = "Contracts", "Signed customer contracts"
		userID := id26("FU-SET2")
		contracts.CreatedByUserID = &userID
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, contracts, 20))
		got, err := f.db.GetFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID)
		require.NoError(t, err)
		assert.Equal(t, "Signed customer contracts", got.Description)
		assert.Equal(t, f.ontology.WorkspaceID, got.WorkspaceID)
		require.NotNil(t, got.CreatedByUserID)
		assert.Equal(t, userID, *got.CreatedByUserID)
		var owner *string
		require.NoError(t, testDB.QueryRow(`SELECT ontology_id FROM foundry_document_sets WHERE id=?`, contracts.ID).Scan(&owner))
		assert.Nil(t, owner, "a new set names no ontology")

		for _, name := range []string{"CONTRACTS", "Cóntracts"} {
			clash := f.documentSet("clash")
			clash.Name = name
			assert.ErrorIs(t, f.db.CreateFoundryDocumentSet(ctx, clash, 20), store.ErrConflict, "%q takes Contracts' name", name)
		}
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID),
			model.FoundryDocumentSetUpdate{Name: ptr("contracts")}), store.ErrConflict)
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID,
			model.FoundryDocumentSetUpdate{Name: ptr("CONTRACTS")}), "a set may change its own name's case")
		other := sampleOntology(t, f.db, id26("FWS-SET2-other"), "SET2-other")
		elsewhere := *contracts
		elsewhere.ID, elsewhere.WorkspaceID = id26("FS-elsewhere"), other.WorkspaceID
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, &elsewhere, 20), "names are unique per workspace")
		second := sampleOntology(t, f.db, f.ontology.WorkspaceID, "SET2-second")
		_, err = f.db.GetFoundryDocumentSet(ctx, f.ontology.WorkspaceID, generalSetID(second.ID))
		require.NoError(t, err, "every ontology of the workspace shares its sets")

		// A nil field is left as it is; a set the workspace doesn't have is not found.
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID,
			model.FoundryDocumentSetUpdate{Description: ptr("")}))
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID,
			model.FoundryDocumentSetUpdate{Name: ptr("Agreements")}))
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID, model.FoundryDocumentSetUpdate{}),
			"an update that changes nothing still finds the set")
		got, err = f.db.GetFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID)
		require.NoError(t, err)
		assert.Equal(t, "Agreements", got.Name)
		assert.Empty(t, got.Description)
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, other.WorkspaceID, contracts.ID, model.FoundryDocumentSetUpdate{Name: ptr("x")}),
			store.ErrNotFound)

		// Before the data step two legacy sets may share a name: an edit that
		// sends a set's own name is no rename, while taking the name is.
		_, err = testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, ontology_id, name, live_key) VALUES (?, ?, ?, 'Agreements', 1)`,
			id26("FS-SET2-legacy"), f.ontology.WorkspaceID, second.ID)
		require.NoError(t, err)
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID,
			model.FoundryDocumentSetUpdate{Name: ptr("Agreements"), Description: ptr("signed")}))
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, contracts.ID,
			model.FoundryDocumentSetUpdate{Name: ptr("agreements")}), store.ErrConflict, "a change of case is a rename")
		_, err = testDB.Exec(`DELETE FROM foundry_document_sets WHERE id=?`, id26("FS-SET2-legacy"))
		require.NoError(t, err)

		// Counts leave deleted documents out; the list is oldest first.
		f.created(t, f.documentIn("a", contracts.ID))
		f.created(t, f.documentIn("bb", contracts.ID))
		f.created(t, f.documentIn("ccc", generalSetID(f.ontology.ID)))
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, id26("FD-bb")))
		assert.Equal(t, []string{"General SET2=1", "Agreements=1", "General SET2-second=0"}, f.setNames(t))
	})

	// Each set is a source a generation may give the agent, so a create may
	// bring a workspace to a bounded number; names hold across creates at once.
	t.Run("caps a workspace's live sets, across concurrent creates", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET3")
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = f.db.CreateFoundryDocumentSet(ctx, f.documentSet(fmt.Sprint(i)), 3)
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
		assert.Equal(t, 2, created, "the fixture's set holds one of the three places")
		sets, err := f.db.ListFoundryDocumentSets(ctx, f.ontology.WorkspaceID)
		require.NoError(t, err)
		require.Len(t, sets, 3)
		require.NoError(t, f.db.DeleteFoundryDocumentSet(ctx, f.ontology.WorkspaceID, sets[2].ID))
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, f.documentSet("again"), 3), "a deleted set gives its place back")
		// A workspace whose ontologies' sets became its own may be over the
		// cap: it renames and deletes, and creates no more.
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, f.ontology.WorkspaceID, sets[0].ID,
			model.FoundryDocumentSetUpdate{Name: ptr("Renamed")}))
		assert.ErrorIs(t, f.db.CreateFoundryDocumentSet(ctx, f.documentSet("over"), 2), store.ErrLimitExceeded)

		// Two creates of one name at once: the workspace's lock lets one in.
		errs = make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				set := f.documentSet(fmt.Sprintf("same-%d", i))
				set.Name = "Same name"
				errs[i] = f.db.CreateFoundryDocumentSet(ctx, set, 20)
			}()
		}
		wg.Wait()
		created = 0
		for _, err := range errs {
			if err == nil {
				created++
				continue
			}
			assert.ErrorIs(t, err, store.ErrConflict)
		}
		assert.Equal(t, 1, created)
	})

	// A set needs its workspace.
	t.Run("refuses a set of a workspace that is gone", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET4")
		stranger := f.documentSet("stranger")
		stranger.WorkspaceID = id26("FWS-stranger")
		assert.ErrorIs(t, f.db.CreateFoundryDocumentSet(ctx, stranger, 20), store.ErrNotFound)
	})

	// A set is deleted only once its documents are gone, and then takes
	// no document again; its name is free for a new set.
	t.Run("deletes only an empty set", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET5")
		ws := f.ontology.WorkspaceID
		set := f.documentSet("old")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, set, 20))
		f.created(t, f.documentIn("a", set.ID))
		assert.ErrorIs(t, f.db.DeleteFoundryDocumentSet(ctx, ws, set.ID), store.ErrNotEmpty)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, ws, id26("FD-a")))
		require.NoError(t, f.db.DeleteFoundryDocumentSet(ctx, ws, set.ID), "a deleted document doesn't hold its set")
		assert.ErrorIs(t, f.db.DeleteFoundryDocumentSet(ctx, ws, set.ID), store.ErrNotFound, "deleted once")
		_, err := f.db.GetFoundryDocumentSet(ctx, ws, set.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, ws, set.ID, model.FoundryDocumentSetUpdate{Name: ptr("x")}), store.ErrNotFound)
		_, _, err = f.db.CreateFoundryDocument(ctx, f.documentIn("bb", set.ID), noCap)
		assert.ErrorIs(t, err, store.ErrNotFound)
		f.created(t, f.documentIn("ccc", generalSetID(f.ontology.ID)))
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, id26("FD-ccc"), set.ID, f.fan("SET5"))
		assert.ErrorIs(t, err, store.ErrNotFound)
		reuse := f.documentSet("new")
		reuse.Name = set.Name
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, reuse, 20))
		assert.Equal(t, []string{"General SET5=1", "Set old=0"}, f.setNames(t))
	})

	// Deleting a set while documents are uploaded into it never strands one:
	// either the delete sees them and is refused, or they find no set.
	t.Run("never leaves a document in a deleted set", func(t *testing.T) {
		ctx := context.Background()
		for round := range 5 {
			f := newFoundryFixture(t, fmt.Sprintf("SET6-%d", round))
			set := f.documentSet("racy")
			require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, set, 20))
			var wg sync.WaitGroup
			created := make([]error, 4)
			var deleted error
			for i := range created {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _, created[i] = f.db.CreateFoundryDocument(ctx, f.documentIn(fmt.Sprintf("d%d", i), set.ID), noCap)
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				deleted = f.db.DeleteFoundryDocumentSet(ctx, f.ontology.WorkspaceID, set.ID)
			}()
			wg.Wait()
			docs, _, err := f.db.ListFoundryDocuments(ctx, f.ontology.WorkspaceID, set.ID, "", 10)
			require.NoError(t, err)
			if deleted == nil {
				assert.Empty(t, docs, "round %d: a deleted set holds no document", round)
				for _, err := range created {
					if err != nil {
						assert.ErrorIs(t, err, store.ErrNotFound)
					}
				}
				continue
			}
			assert.ErrorIs(t, deleted, store.ErrNotEmpty)
			assert.NotEmpty(t, docs)
		}
	})

	// A move changes the set a document is listed under; only a live
	// document of the workspace moves, and only into its live sets.
	t.Run("moves a document to another set", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET7")
		ws, general := f.ontology.WorkspaceID, generalSetID(f.ontology.ID)
		contracts := f.documentSet("contracts")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, contracts, 20))
		f.created(t, f.documentIn("a", general))
		time.Sleep(2 * time.Millisecond)
		f.created(t, f.documentIn("bb", general))

		moved, reads, err := f.db.MoveFoundryDocument(ctx, ws, id26("FD-a"), contracts.ID, f.fan("SET7"))
		require.NoError(t, err)
		assert.Equal(t, contracts.ID, moved.SetID)
		assert.Empty(t, reads, "a document waiting for its bytes is read when they come")
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, id26("FD-a"), contracts.ID, f.fan("SET7"))
		require.NoError(t, err, "moving again is no error")
		inContracts, _, err := f.db.ListFoundryDocuments(ctx, ws, contracts.ID, "", 10)
		require.NoError(t, err)
		require.Len(t, inContracts, 1)
		assert.Equal(t, id26("FD-a"), inContracts[0].ID)
		all, _, err := f.db.ListFoundryDocuments(ctx, ws, "", "", 10)
		require.NoError(t, err)
		assert.Len(t, all, 2, "no set lists every set's documents")

		other := sampleOntology(t, f.db, id26("FWS-SET7-other"), "SET7-other")
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, id26("FD-bb"), generalSetID(other.ID), f.fan("SET7"))
		assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's set")
		_, _, err = f.db.MoveFoundryDocument(ctx, other.WorkspaceID, id26("FD-bb"), generalSetID(other.ID), f.fan("SET7"))
		assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's document")
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, ws, id26("FD-bb")))
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, id26("FD-bb"), contracts.ID, f.fan("SET7"))
		assert.ErrorIs(t, err, store.ErrNotFound, "a deleted document")
	})

	// A generation shows the agent each set's newest uploaded text
	// documents: Markdown, MDX and plain text. Table files and PDFs never
	// take one of the limit's places.
	t.Run("lists a set's uploaded text documents", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET8")
		contracts := f.documentSet("contracts")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, contracts, 20))
		inGeneral := f.uploaded(t, "a")
		mdx := f.uploadedAs(t, "mdx", model.FoundryMediaMDX)
		txt := f.uploadedAs(t, "txt", model.FoundryMediaPlainText)
		f.uploadedAs(t, "csv", model.FoundryMediaCSV)
		f.uploadedAs(t, "pdf", model.FoundryMediaPDF)
		_, _, err := f.db.MoveFoundryDocument(ctx, f.ontology.WorkspaceID, f.uploaded(t, "bb"), contracts.ID, f.fan("SET8"))
		require.NoError(t, err)
		docs, err := f.db.ListUploadedFoundryDocuments(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID), 10)
		require.NoError(t, err)
		ids := make([]string, len(docs))
		for i, d := range docs {
			ids[i] = d.ID
			assert.Equal(t, generalSetID(f.ontology.ID), d.SetID)
		}
		assert.ElementsMatch(t, []string{inGeneral, mdx, txt}, ids)
		docs, err = f.db.ListUploadedFoundryDocuments(ctx, f.ontology.WorkspaceID, generalSetID(f.ontology.ID), 2)
		require.NoError(t, err)
		assert.Len(t, docs, 2, "the limit counts text documents only")
	})

	// A document no route of the load's version reads is recorded, so the
	// load doesn't wait for it, and a later load reads it afresh.
	t.Run("records a document out of scope", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET9")
		load := f.load(t, "L1")
		doc := f.uploaded(t, "a")
		job, err := f.db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, job)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", &model.FoundryDocumentExtraction{
			DocumentID: doc, Status: model.FoundryExtractionOutOfScope, Counts: model.FoundryExtractionCounts{Sections: 3},
		}, false))
		e, err := f.db.GetFoundryDocumentExtraction(ctx, load, doc)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionOutOfScope, e.Status)
		assert.Equal(t, 3, e.Counts.Sections)
		assert.Nil(t, e.Error)
		assert.Nil(t, e.DuplicateOf)
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.Empty(t, pending)
		f.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("SET9"))
		second := f.load(t, "L2")
		pending, err = f.db.ListPendingFoundryDocuments(ctx, second)
		require.NoError(t, err)
		require.Len(t, pending, 1, "another load extracts it afresh")
	})

	// Until the workspace-foundry-documents step removes them, the sets and
	// documents of an ontology deleted before they were the workspace's are
	// gone to every read, count and write, and to the names and the cap.
	// A connector's set says whose it is, by the connector's name as it is
	// now; its documents say which row of the source they are the text of.
	t.Run("reads a connector's set and documents with their owner and source", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET11")
		ws := f.ontology.WorkspaceID
		notes := f.connectorSet(t, "SET11-granola", "Team Granola")
		at := time.Date(2026, 10, 1, 9, 30, 0, 123000, time.UTC)
		linked := f.connectorDocument(t, "SET11-linked", notes.ID, "not_Abc", at)
		_, err := testDB.Exec(`UPDATE foundry_documents SET source_url='https://notes.granola.ai/d/abc' WHERE id=?`, linked)
		require.NoError(t, err)
		unlinked := f.connectorDocument(t, "SET11-unlinked", notes.ID, "not_abc", at)
		upload := f.created(t, f.documentIn("SET11-upload", generalSetID(f.ontology.ID)))

		got, err := f.db.GetFoundryDocumentSet(ctx, ws, notes.ID)
		require.NoError(t, err)
		assert.Equal(t, notes.Owner, got.Owner)
		assert.Nil(t, got.CreatedByUserID)
		assert.Equal(t, 2, got.Documents)
		assert.Equal(t, "Team Granola / Notes", got.DisplayName())
		sets, err := f.db.ListFoundryDocumentSets(ctx, ws)
		require.NoError(t, err)
		require.Len(t, sets, 2)
		assert.Nil(t, sets[0].Owner, "a person's set has no owner")
		assert.Equal(t, "General SET11", sets[0].DisplayName())
		assert.Equal(t, notes.Owner, sets[1].Owner)

		// Renaming the connection renames the set where it is shown: nothing is written to the set.
		require.NoError(t, f.db.UpdateConnector(ctx, ws, notes.Owner.ConnectorID, model.ConnectorUpdate{Name: ptr("Sales Granola")}))
		got, err = f.db.GetFoundryDocumentSet(ctx, ws, notes.ID)
		require.NoError(t, err)
		assert.Equal(t, "Sales Granola / Notes", got.DisplayName())

		docs, err := f.db.ListFoundryDocumentsByID(ctx, ws, []string{linked, unlinked, upload.ID})
		require.NoError(t, err)
		require.Len(t, docs, 3)
		require.NotNil(t, docs[linked].Source)
		assert.Equal(t, "not_Abc", docs[linked].Source.ExternalID)
		assert.Equal(t, "1|v", docs[linked].Source.Version)
		assert.True(t, at.Equal(docs[linked].Source.At), "source time %v, want %v", docs[linked].Source.At, at)
		require.NotNil(t, docs[linked].Source.URL)
		assert.Equal(t, "https://notes.granola.ai/d/abc", *docs[linked].Source.URL)
		assert.Nil(t, docs[linked].ClientToken, "a connector's document has no client token")
		assert.Nil(t, docs[linked].CreatedByUserID)
		require.NotNil(t, docs[unlinked].Source)
		assert.Equal(t, "not_abc", docs[unlinked].Source.ExternalID, "external ids are case-sensitive")
		assert.Nil(t, docs[unlinked].Source.URL)
		assert.Nil(t, docs[upload.ID].Source, "an upload has no source")
		require.NotNil(t, docs[upload.ID].ClientToken)
		assert.Equal(t, "doc-SET11-upload", *docs[upload.ID].ClientToken)

		_, _, err = f.db.CreateFoundryDocument(ctx, &model.FoundryDocument{ID: id26("FD-SET11-x"), WorkspaceID: ws, SetID: notes.ID}, noCap)
		assert.ErrorContains(t, err, "an upload has a client token and no source")
	})

	// A connector's set and its documents are its syncs' to write: no
	// person's change goes through, and a refused one changes nothing. They
	// take no place among the workspace's sets, its names, its document cap
	// or its upload list, and an upload is no repeat of their bytes; what a
	// version reads counts them.
	t.Run("leaves a connector's set and documents to its syncs", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET12")
		ws, general := f.ontology.WorkspaceID, generalSetID(f.ontology.ID)
		notes := f.connectorSet(t, "SET12-granola", "Team Granola")
		note := f.connectorDocument(t, "SET12-note", notes.ID, "not_1", time.Now())
		upload := f.created(t, f.documentIn("SET12-upload", general))
		_, err := testDB.Exec(`UPDATE foundry_documents SET status='uploaded', uploaded_at=NOW(6) WHERE id=?`, upload.ID)
		require.NoError(t, err)

		_, _, err = f.db.CreateFoundryDocument(ctx, f.documentIn("SET12-into", notes.ID), noCap)
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "an upload into the set")
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, upload.ID, notes.ID, f.fan("SET12-in"))
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "a move into the set")
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, note, general, f.fan("SET12-out"))
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "a move out of the set")
		assert.ErrorIs(t, f.db.RenameFoundryDocument(ctx, ws, note, "Mine.md"), store.ErrConnectorOwned, "a rename of a document")
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, ws, notes.ID, model.FoundryDocumentSetUpdate{Name: ptr("Meetings")}),
			store.ErrConnectorOwned, "a rename of the set")
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, ws, notes.ID, model.FoundryDocumentSetUpdate{Description: ptr("mine")}),
			store.ErrConnectorOwned, "a description of the set")
		assert.ErrorIs(t, f.db.DeleteFoundryDocumentSet(ctx, ws, notes.ID), store.ErrConnectorOwned, "a delete of the set")
		got, err := f.db.GetFoundryDocumentSet(ctx, ws, notes.ID)
		require.NoError(t, err)
		assert.Equal(t, "Notes", got.Name)
		assert.Equal(t, "Meeting notes", got.Description)
		assert.Equal(t, 1, got.Documents)
		stored, err := f.db.GetFoundryDocument(ctx, ws, note)
		require.NoError(t, err)
		assert.Equal(t, notes.ID, stored.SetID)
		assert.Equal(t, "SET12-note.md", stored.Name)
		_, err = f.db.GetFoundryDocument(ctx, ws, id26("FD-SET12-into"))
		assert.ErrorIs(t, err, store.ErrNotFound)
		moved, err := f.db.GetFoundryDocument(ctx, ws, upload.ID)
		require.NoError(t, err)
		assert.Equal(t, general, moved.SetID)

		// Its name and place are free for a person's set.
		mine := f.documentSet("SET12-mine")
		mine.Name = "notes"
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, mine, 2), "the connector's set takes no place and no name")
		assert.ErrorIs(t, f.db.CreateFoundryDocumentSet(ctx, f.documentSet("SET12-more"), 2), store.ErrLimitExceeded)
		require.NoError(t, f.db.UpdateFoundryDocumentSet(ctx, ws, mine.ID, model.FoundryDocumentSetUpdate{Name: ptr("NOTES")}))

		// The cap, the bytes and the workspace's list are uploads'; a set's list and a version's count are both origins'.
		n, err := f.db.CountFoundryDocuments(ctx, ws, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Equal(t, 1, n, "the cap counts the upload only")
		n, err = f.db.CountFoundryDocumentsInSets(ctx, ws, []string{general, notes.ID})
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		same := f.documentIn("SET12-same", general)
		same.SHA256 = foundryDocument(f.ontology, "SET12-note").SHA256
		repeat, existing, err := f.db.CreateFoundryDocument(ctx, same, noCap)
		require.NoError(t, err)
		assert.False(t, existing, "a connector's document is no repeat of an upload")
		assert.Equal(t, same.ID, repeat.ID)
		docs, _, err := f.db.ListFoundryDocuments(ctx, ws, "", "", 10)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{upload.ID, same.ID}, documentIDs(docs), "the workspace's list is its uploads")
		docs, _, err = f.db.ListFoundryDocuments(ctx, ws, notes.ID, "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{note}, documentIDs(docs), "a connector's set lists its documents")

		// The graph worker still deletes one whose original is gone; a deleted one is not found, not refused.
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, ws, note))
		assert.ErrorIs(t, f.db.RenameFoundryDocument(ctx, ws, note, "Mine.md"), store.ErrNotFound)
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, note, general, f.fan("SET12-deleted"))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("hides the rows of an ontology deleted before release 1", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "SET10")
		ws := f.ontology.WorkspaceID
		setID, docID := f.hidden(t, "SET10-h")
		_, err := f.db.GetFoundryDocumentSet(ctx, ws, setID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.GetFoundryDocument(ctx, ws, docID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, []string{"General SET10=0", "General SET10-h-gone=0"}, f.setNames(t))
		docs, _, err := f.db.ListFoundryDocuments(ctx, ws, "", "", 10)
		require.NoError(t, err)
		assert.Empty(t, docs)
		n, err := f.db.CountFoundryDocuments(ctx, ws, time.Unix(0, 0))
		require.NoError(t, err)
		assert.Zero(t, n, "the cap counts none of them")
		n, err = f.db.CountFoundryDocumentsInSets(ctx, ws, []string{setID})
		require.NoError(t, err)
		assert.Zero(t, n)
		uploaded, err := f.db.ListUploadedFoundryDocuments(ctx, ws, setID, 10)
		require.NoError(t, err)
		assert.Empty(t, uploaded)

		reuse := f.documentSet("reuse")
		reuse.Name = "Hidden SET10-h"
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, reuse, 3), "its name is free, and it takes no place")
		_, _, err = f.db.CreateFoundryDocument(ctx, f.documentIn("into", setID), noCap)
		assert.ErrorIs(t, err, store.ErrNotFound, "no document goes into it")
		same := f.documentIn("same", generalSetID(f.ontology.ID))
		same.SHA256 = fmt.Sprintf("%064x", 999)
		stored, existing, err := f.db.CreateFoundryDocument(ctx, same, noCap)
		require.NoError(t, err)
		assert.False(t, existing, "its bytes are no repeat")
		assert.Equal(t, same.ID, stored.ID)
		assert.ErrorIs(t, f.db.DeleteFoundryDocumentSet(ctx, ws, setID), store.ErrNotFound)
		assert.ErrorIs(t, f.db.UpdateFoundryDocumentSet(ctx, ws, setID, model.FoundryDocumentSetUpdate{Name: ptr("x")}), store.ErrNotFound)
	})
}

// The worker sends ListPendingFoundryDocuments' SetID to Extract. A move
// after joining a load must change only the next load, including on retry.
// A connector's documents are new by their time at the source, which a
// backfill publishes newest first; an upload by its creation.
func TestFoundryConnectorDocumentOrder(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "ORD")
	ws := f.ontology.WorkspaceID
	general := generalSetID(f.ontology.ID)
	notes := f.connectorSet(t, "ORD-granola", "Team Granola")
	day := time.Now().UTC().Truncate(time.Second).Add(-72 * time.Hour)
	// Published newest first, as a full read offers them; the source's own times say otherwise.
	newest := f.connectorDocument(t, "ORD-newest", notes.ID, "not_3", day.Add(48*time.Hour))
	oldest := f.connectorDocument(t, "ORD-oldest", notes.ID, "not_1", day)
	middle := f.connectorDocument(t, "ORD-middle", notes.ID, "not_2", day.Add(24*time.Hour))
	firstUpload := f.uploaded(t, "ORD-u1")
	time.Sleep(2 * time.Millisecond)
	secondUpload := f.uploaded(t, "ORD-u2")
	load := f.loadReading(t, "L1", general, notes.ID)

	// Uploads first, oldest first, so a connector's backlog never holds one
	// back; then the connector's, newest at the source first, so a budget
	// that runs out spends on the newest.
	t.Run("sends uploads first, then the newest of a connector's", func(t *testing.T) {
		pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.Equal(t, []string{firstUpload, secondUpload, newest, middle, oldest}, documentIDs(pending))
	})

	// A set's list, its page tokens and a generation's sample all go by the source's time.
	t.Run("lists a connector's newest at the source first", func(t *testing.T) {
		page, next, err := f.db.ListFoundryDocuments(ctx, ws, notes.ID, "", 2)
		require.NoError(t, err)
		assert.Equal(t, []string{newest, middle}, documentIDs(page))
		rest, last, err := f.db.ListFoundryDocuments(ctx, ws, notes.ID, next, 2)
		require.NoError(t, err)
		assert.Equal(t, []string{oldest}, documentIDs(rest))
		assert.Empty(t, last)
		sample, err := f.db.ListUploadedFoundryDocuments(ctx, ws, notes.ID, 2)
		require.NoError(t, err)
		assert.Equal(t, []string{newest, middle}, documentIDs(sample))
	})

	// An ontology's list mixes both origins, each by its own time, and pages across them.
	t.Run("pages an ontology's documents of both origins by time", func(t *testing.T) {
		var got []string
		token := ""
		for range 5 {
			page, next, err := f.db.ListFoundryOntologyDocuments(ctx, ws, "", []string{general, notes.ID}, model.FoundryDocumentFilterAll, token, 2)
			require.NoError(t, err)
			got = append(got, documentIDs(page)...)
			if token = next; token == "" {
				break
			}
		}
		assert.Equal(t, []string{secondUpload, firstUpload, newest, middle, oldest}, got)
	})
}

func TestFoundryLoadDocumentSets(t *testing.T) {
	for _, timing := range []string{"before load", "during load", "after load"} {
		t.Run(timing, func(t *testing.T) {
			ctx := context.Background()
			f := newFoundryFixture(t, "SETSNAP")
			general := generalSetID(f.ontology.ID)
			other := f.documentSet("other")
			require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, other, 20))
			var doc string
			if timing == "before load" {
				doc = f.uploaded(t, "snap")
			}
			load := f.loadReading(t, "L1", general, other.ID)
			if timing == "after load" {
				f.loaded(t, "w1")
			}
			if doc == "" {
				doc = f.uploaded(t, "snap")
			}
			_, _, err := f.db.MoveFoundryDocument(ctx, f.ontology.WorkspaceID, doc, other.ID, f.fan("SETSNAP"))
			require.NoError(t, err)
			// The source set can disappear; membership must not depend on it
			// remaining live, or on the document's mutable set_id.
			require.NoError(t, f.db.DeleteFoundryDocumentSet(ctx, f.ontology.WorkspaceID, general))
			pending, err := f.db.ListPendingFoundryDocuments(ctx, load)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, general, pending[0].SetID)
			job := f.liveJob(t)
			require.NotNil(t, job)
			f.claimed(t, "w1", job.ID)
			failed := extracted(doc)
			failed.Status = model.FoundryExtractionFailed
			failed.Error = &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true}
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", failed, false))
			pending, err = f.db.ListPendingFoundryDocuments(ctx, load)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, general, pending[0].SetID, "retries retain the captured set")
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(doc), false))
			f.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("SETSNAP"))
			second := f.loadReading(t, "L2", other.ID)
			pending, err = f.db.ListPendingFoundryDocuments(ctx, second)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, other.ID, pending[0].SetID, "the next load uses the moved set")
			f.loaded(t, "w1", doc)
			require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, load, id26("FN-D-snapshot")))
			deleting := f.claimed(t, "w1", id26("FN-D-snapshot"))
			f.ended(t, "w1", deleting.ID, model.FoundryGraphJobSucceeded, next("SNAPDELETE"))
			var remaining int
			require.NoError(t, testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_documents WHERE load_id=?`, load).Scan(&remaining))
			assert.Zero(t, remaining, "deleting a load removes its membership")
			require.NoError(t, testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_documents WHERE load_id=?`, second).Scan(&remaining))
			assert.Equal(t, 1, remaining, "other loads retain their membership")
		})
	}
}

// versionReading writes an agent draft FV-<tag> of the fixture's ontology
// that reads sets, with mappings that route them.
func (f foundryFixture) versionReading(t *testing.T, tag string, sets ...string) string {
	t.Helper()
	routes, err := json.Marshal(sets)
	require.NoError(t, err)
	def, err := json.Marshal(map[string]string{"format": "agent_bundle", "mappings_toml": "[extractions.docs]\ndocuments = " + string(routes) + "\n"})
	require.NoError(t, err)
	v := &model.OntologyVersion{
		ID: id26("FV-" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: def, DocumentSetIDs: &sets,
	}
	require.NoError(t, f.db.CreateOntologyVersion(context.Background(), v))
	return v.ID
}

// loadReading writes a version that reads sets and queues a load of it,
// returning the load's id.
func (f foundryFixture) loadReading(t *testing.T, tag string, sets ...string) string {
	t.Helper()
	f.versionReading(t, tag, sets...)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, tag)))
	return id26("FJ-" + tag)
}

// A connector's set counts its documents through its live index, never
// reading the deleted rows its stream's turnover leaves behind, and a
// person's set counts by status, its pending uploads included: either way
// the count is of the documents that are not deleted, whatever the mix
// (seeded rounds, one adding 10,000 deleted rows to one set).
func TestFoundryDocumentSetCounts(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "CNT")
	ws := f.ontology.WorkspaceID
	pieces, notes := f.connectorSet(t, "CNT-pieces", "Shop orders"), f.connectorSet(t, "CNT-notes", "Team Granola")
	general := generalSetID(f.ontology.ID)
	rng := rand.New(rand.NewPCG(7, 2026))
	n := 0
	// insert writes count documents into a set: into a connector's,
	// uploaded ones (each its own row) or deleted ones (what replacements
	// left, sharing external ids); into a person's, uploads in any status.
	insert := func(setID string, connector bool, count int, deleted bool) {
		t.Helper()
		for count > 0 {
			batch := min(count, 1000)
			count -= batch
			values, args := make([]string, 0, batch), make([]any, 0, 6*batch)
			for range batch {
				n++
				id, sha := id26(fmt.Sprintf("CNT%08d", n)), fmt.Sprintf("%064d", n)
				if !connector {
					values = append(values, "(?, ?, ?, NULL, NULL, NULL, 'up.md', 'text/markdown', 1, ?, ?, ?, 'k', NULL)")
					args = append(args, id, ws, setID, sha, fmt.Sprintf("tok-%d", n), []string{"pending_upload", "uploaded", "deleted"}[rng.IntN(3)])
					continue
				}
				external, status := fmt.Sprintf("live-%d", n), "uploaded"
				if deleted {
					external, status = fmt.Sprintf("ext-%d", rng.IntN(50)), "deleted"
				}
				values = append(values, "(?, ?, ?, ?, '1|v', NOW(6), 'piece.md', 'text/markdown', 1, ?, NULL, ?, 'k', NOW(6))")
				args = append(args, id, ws, setID, external, sha, status)
			}
			_, err := testDB.Exec(`INSERT INTO foundry_documents (id, workspace_id, set_id, external_id, source_version, source_at, name, media_type,
  size_bytes, sha256, client_token, status, s3_key, uploaded_at) VALUES `+strings.Join(values, ", "), args...)
			require.NoError(t, err)
		}
	}
	for round, deleted := range []int{10_000, 0, 37, 500} {
		insert(pieces.ID, true, rng.IntN(40), false)
		insert(pieces.ID, true, deleted, true)
		insert(notes.ID, true, rng.IntN(40), false)
		insert(notes.ID, true, rng.IntN(200), true)
		insert(general, false, rng.IntN(60), false)
		// Some of every set's documents go, as a sync's removal or a person's delete does.
		_, err := testDB.Exec(`UPDATE foundry_documents SET status='deleted', deleted_at=NOW(6)
WHERE workspace_id=? AND status<>'deleted' AND MOD(CRC32(id), 5)=?`, ws, round)
		require.NoError(t, err)

		want := map[string]int{}
		rows, err := testDB.Query(`SELECT set_id, COUNT(*) FROM foundry_documents WHERE workspace_id=? AND status<>'deleted' GROUP BY set_id`, ws)
		require.NoError(t, err)
		for rows.Next() {
			var set string
			var count int
			require.NoError(t, rows.Scan(&set, &count))
			want[set] = count
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		sets, err := f.db.ListFoundryDocumentSets(ctx, ws)
		require.NoError(t, err)
		got := map[string]int{}
		for _, s := range sets {
			got[s.ID] = s.Documents
		}
		for _, id := range []string{pieces.ID, notes.ID, general} {
			assert.Equal(t, want[id], got[id], "round %d: set %s", round, id)
			one, err := f.db.GetFoundryDocumentSet(ctx, ws, id)
			require.NoError(t, err)
			assert.Equal(t, want[id], one.Documents, "round %d: set %s read alone", round, id)
		}
	}

	// The connector's count reads one range of the live index, so a set
	// whose stream turned over 10,000 times costs what its live documents do.
	_, err := testDB.Exec(`ANALYZE TABLE foundry_documents`)
	require.NoError(t, err)
	rows, err := testDB.Query("EXPLAIN "+store.FoundryDocumentSetSelect+` WHERE s.id=? AND s.workspace_id=? AND s.live_key=1`, pieces.ID, ws)
	require.NoError(t, err)
	defer rows.Close()
	columns, err := rows.Columns()
	require.NoError(t, err)
	keys := map[string]string{}
	for rows.Next() {
		values, dest := make([]sql.NullString, len(columns)), make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		require.NoError(t, rows.Scan(dest...))
		keys[values[slices.Index(columns, "table")].String] = values[slices.Index(columns, "key")].String
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, "foundry_documents_live_external_uq", keys["cd"], "the plan: %v", keys)
}
