package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestFoundryDocumentRemovals(t *testing.T) {
	// A document gone at the source leaves the graph through an extract into
	// each load that holds it: queued now, or owed after a live job. An
	// ontology with refresh off keeps it, as it keeps a table's old rows.
	t.Run("a sync's removal owes an extract of each load holding it", func(t *testing.T) {
		ctx := context.Background()
		day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		c := newConnectorSync(t, newFoundryFixture(t, "RMS"), "RMS")
		ws := c.ontology.WorkspaceID
		c.published(t, c.publish("RMS-a", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day)}, nil))
		notes := []string{id26("FD-n1"), id26("FD-n2")}
		current := c.loadReading(t, "LA", c.setID)
		c.loaded(t, "w1", notes...)
		elsewhere := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "RMSB")}
		elsewhere.loadReading(t, "LB", generalSetID(elsewhere.ontology.ID))
		elsewhere.loaded(t, "w1")
		off := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "RMSC")}
		off.loadReading(t, "LC", c.setID)
		off.loaded(t, "w1", notes...)
		require.NoError(t, c.db.SetFoundryOntologyAutoRefresh(ctx, ws, off.ontology.ID, false))
		writing := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "RMSD")}
		writing.loadReading(t, "LD", c.setID)

		c.published(t, c.publish("RMS-b", nil, nil, id26("FD-n2")))
		live := c.liveJob(t)
		require.NotNil(t, live, "an extract takes the document out")
		assert.Equal(t, model.FoundryGraphJobExtract, live.Kind)
		assert.Equal(t, current, live.LoadID)
		assert.Nil(t, c.ontologyNow(t).RefreshDueAt, "no refresh is owed")
		removed, err := c.db.ListRemovedFoundryDocuments(ctx, current)
		require.NoError(t, err)
		assert.Equal(t, []string{id26("FD-n2")}, removed)
		assert.Nil(t, elsewhere.liveJob(t), "its loads don't hold the document")
		assert.Nil(t, off.liveJob(t), "refresh is off")
		assert.True(t, writing.ontologyNow(t).ExtractOwed, "the load being written holds it")
	})

	// A person who deletes a document wants its facts gone, refresh or not.
	t.Run("a person's delete takes the document out", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RMP")
		doc := f.uploaded(t, "d1")
		load := f.load(t, "L1")
		f.loaded(t, "w1", doc)
		require.NoError(t, f.db.SetFoundryOntologyAutoRefresh(ctx, f.ontology.WorkspaceID, f.ontology.ID, false))
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, doc))
		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, load, live.LoadID)
		removed, err := f.db.ListRemovedFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.Equal(t, []string{doc}, removed)
	})

	// Taking a document out spends nothing, so a spent month holds none back.
	t.Run("queues a removal with the month spent", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RMB")
		doc := f.uploaded(t, "d1")
		load := f.load(t, "L1")
		f.loaded(t, "w1", doc)
		_, err := testDB.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=? WHERE id=?`, int64(1)<<40, load)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, doc))
		require.NotNil(t, f.liveJob(t))
	})

	// The agent's confirmation forgets the document in the load, under the
	// job's lease, and only a document that is deleted.
	t.Run("forgets a removed document once the agent took it out", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RMF")
		doc, kept := f.uploaded(t, "d1"), f.uploaded(t, "d2")
		load := f.load(t, "L1")
		f.loaded(t, "w1", doc, kept)
		require.NoError(t, f.db.DeleteFoundryDocument(ctx, f.ontology.WorkspaceID, doc))
		job := f.claimed(t, "w2", f.liveJob(t).ID)
		assert.ErrorIs(t, f.db.RecordFoundryDocumentRemoval(ctx, job.ID, "w9", doc), store.ErrNotFound, "only the lease holder records")
		assert.Error(t, f.db.RecordFoundryDocumentRemoval(ctx, job.ID, "w2", kept), "a document that is not deleted stays")
		require.NoError(t, f.db.RecordFoundryDocumentRemoval(ctx, job.ID, "w2", doc))
		removed, err := f.db.ListRemovedFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.Empty(t, removed)
		extractions, err := f.db.ListFoundryDocumentExtractions(ctx, load, []string{doc, kept})
		require.NoError(t, err)
		assert.Contains(t, extractions, kept)
		assert.NotContains(t, extractions, doc)
	})
}

func TestFoundryDocumentExpiry(t *testing.T) {
	day := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	// A document that left its source's window leaves every load: the
	// current one through an extract even with refresh off, the one being
	// written once its job ends, and every other one that can still be read
	// or made current is deleted. A load that doesn't hold it stays.
	t.Run("leaves every load of every ontology", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "EXP"), "EXP")
		ws := c.ontology.WorkspaceID
		c.published(t, c.publish("EXP-a", []*model.FoundryDocument{
			c.note("e1", "day_1", "1|a", day), c.note("e2", "day_2", "1|a", day.AddDate(0, 0, 1)),
		}, nil))
		notes := []string{id26("FD-e1"), id26("FD-e2")}
		older := c.loadReading(t, "EXP-L1", c.setID)
		c.loaded(t, "exp1", notes...)
		current := c.loadReading(t, "EXP-L2", c.setID)
		c.loaded(t, "exp2", notes...)
		require.NoError(t, c.db.SetFoundryOntologyAutoRefresh(ctx, ws, c.ontology.ID, false))

		moved := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "EXPB")}
		held := moved.loadReading(t, "EXPB-L1", c.setID)
		moved.loaded(t, "expb1", notes...)
		elsewhere := moved.loadReading(t, "EXPB-L2", generalSetID(moved.ontology.ID))
		moved.loaded(t, "expb2")

		failed := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "EXPD")}
		broken := failed.loadReading(t, "EXPD-L1", c.setID)
		failed.finish(t, "expd1", "EXPD-f", model.FoundryGraphJobFailed)
		require.Equal(t, model.FoundryLoadFailed, failed.loadStatus(t, broken))

		writing := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "EXPC")}
		written := writing.loadReading(t, "EXPC-L1", c.setID)

		p := c.publish("EXP-b", nil, nil)
		p.Expire = []string{id26("FD-e1")}
		c.published(t, p)

		assert.Equal(t, []string{"day_2=" + id26("FD-e2") + "@1|a"}, c.index(t))
		gone, err := c.db.GetFoundryDocument(ctx, ws, id26("FD-e1"))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentDeleted, gone.Status)
		assert.Equal(t, []string{id26("FD-e1")}, purgeRows(t), "its bytes wait for the sweep")

		live := c.liveJob(t)
		require.NotNil(t, live, "refresh off takes it out all the same")
		assert.Equal(t, model.FoundryGraphJobExtract, live.Kind)
		assert.Equal(t, current, live.LoadID)
		assert.Equal(t, model.FoundryLoadReady, c.loadStatus(t, current))
		assert.Equal(t, model.FoundryLoadDeleting, c.loadStatus(t, older), "deleted once the extract ends")

		live = moved.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, model.FoundryGraphJobDelete, live.Kind, "nothing current holds it, so the delete goes now")
		assert.Equal(t, held, live.LoadID)
		assert.Equal(t, model.FoundryLoadReady, moved.loadStatus(t, elsewhere))

		live = writing.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, written, live.LoadID, "the load being written goes on")
		assert.Equal(t, model.FoundryLoadWriting, writing.loadStatus(t, written))
		assert.True(t, writing.ontologyNow(t).ExtractOwed, "and takes it out after")

		live = failed.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, model.FoundryGraphJobDelete, live.Kind, "a failed load is still read")
		assert.Equal(t, broken, live.LoadID)
	})

	// Deleting a connector read through Helix's Slack app (its orphan
	// sweep's, or a person's) keeps nothing: its documents leave every
	// load, as days leaving the window do.
	t.Run("a deleted Slack app connector's documents leave every load", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "EXS"), "EXS")
		ws := c.ontology.WorkspaceID
		c.published(t, c.publish("EXS-a", []*model.FoundryDocument{c.note("s1", "C1:2026-08-01", "1|a", day)}, nil))
		older := c.loadReading(t, "EXS-L1", c.setID)
		c.loaded(t, "exs1", id26("FD-s1"))
		current := c.loadReading(t, "EXS-L2", c.setID)
		c.loaded(t, "exs2", id26("FD-s1"))
		require.NoError(t, c.db.SetFoundryOntologyAutoRefresh(ctx, ws, c.ontology.ID, false))
		dbExec(t, `INSERT INTO foundry_slack_feeds (id, connector_id, workspace_id, team_id, install_id, notice_version, bound_by)
VALUES (?, ?, ?, 'T1', ?, 1, 'u')`, id26("FEED-EXS"), c.connector.ID, ws, id26("INST-EXS"))

		require.NoError(t, c.db.DeleteConnector(ctx, ws, c.connector.ID))
		live := c.liveJob(t)
		require.NotNil(t, live, "refresh off takes it out all the same")
		assert.Equal(t, model.FoundryGraphJobExtract, live.Kind)
		assert.Equal(t, current, live.LoadID)
		assert.Equal(t, model.FoundryLoadDeleting, c.loadStatus(t, older), "deleted once the extract ends")
	})

	// Its other loads go with any deleted document of the set as old at the
	// source as what expires, such as one a load being written kept when an
	// earlier day left; a load holding only newer deleted ones stays.
	t.Run("deletes the loads holding a day before the window", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "EXO"), "EXO")
		ws := c.ontology.WorkspaceID
		old, mid, recent := id26("FD-o1"), id26("FD-o2"), id26("FD-o3")
		c.published(t, c.publish("EXO-a", []*model.FoundryDocument{
			c.note("o1", "day_1", "1|a", day), c.note("o2", "day_2", "1|a", day.AddDate(0, 0, 1)),
			c.note("o3", "day_4", "1|a", day.AddDate(0, 0, 3)),
		}, nil))
		kept := c.loadReading(t, "EXO-L1", c.setID)
		c.loaded(t, "exo1", old, mid, recent)
		retried := c.loadReading(t, "EXO-L2", c.setID)
		c.loaded(t, "exo2", old, mid, recent)
		current := c.loadReading(t, "EXO-L3", c.setID)
		c.loaded(t, "exo3", old, mid, recent)
		for load, out := range map[string][]string{kept: {old, mid}, retried: {mid}} {
			_, err := testDB.Exec(`DELETE FROM foundry_load_documents WHERE load_id=? AND document_id IN (`+
				strings.TrimSuffix(strings.Repeat("?, ", len(out)), ", ")+`)`, append([]any{load}, toAny(out)...)...)
			require.NoError(t, err)
		}
		_, err := testDB.Exec(`UPDATE foundry_loads SET status='delete_failed' WHERE id=?`, retried)
		require.NoError(t, err)
		require.NoError(t, c.db.SetFoundryOntologyAutoRefresh(ctx, ws, c.ontology.ID, false))
		c.published(t, c.publish("EXO-b", nil, nil, old, recent))
		require.Nil(t, c.liveJob(t), "refresh is off: a sync's removals stay")

		p := c.publish("EXO-c", nil, nil)
		p.Expire = []string{mid}
		c.published(t, p)
		assert.Equal(t, model.FoundryLoadReady, c.loadStatus(t, kept), "it holds only a newer day")
		assert.Equal(t, model.FoundryLoadDeleting, c.loadStatus(t, retried), "its failed delete is tried again")
		live := c.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, model.FoundryGraphJobExtract, live.Kind)
		assert.Equal(t, current, live.LoadID)
		removed, err := c.db.ListRemovedFoundryDocuments(ctx, current)
		require.NoError(t, err)
		assert.Equal(t, []string{old, mid, recent}, removed, "the extract takes out every deleted document")

		c.claimed(t, "exo4", live.ID)
		c.ended(t, "exo4", live.ID, model.FoundryGraphJobSucceeded, next("EXO"))
		live = c.liveJob(t)
		require.NotNil(t, live, "the delete follows the extract")
		assert.Equal(t, id26("FN-D-EXO"), live.ID)
		assert.Equal(t, retried, live.LoadID)
	})

	// A document the graph worker already deleted still expires: the loads
	// it is left in go.
	t.Run("expires a document already deleted", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "EXD"), "EXD")
		ws := c.ontology.WorkspaceID
		doc := id26("FD-d1")
		c.published(t, c.publish("EXD-a", []*model.FoundryDocument{c.note("d1", "day_1", "1|a", day)}, nil))
		older := c.loadReading(t, "EXD-L1", c.setID)
		c.loaded(t, "exd1", doc)
		c.loadReading(t, "EXD-L2", c.setID)
		c.loaded(t, "exd2", doc)
		require.NoError(t, c.db.DeleteFoundryDocument(ctx, ws, doc))
		extract := c.liveJob(t)
		require.NotNil(t, extract)

		p := c.publish("EXD-b", nil, nil)
		p.Expire = []string{doc}
		c.published(t, p)
		assert.Empty(t, purgeRows(t), "nothing is removed again")
		assert.Equal(t, model.FoundryLoadDeleting, c.loadStatus(t, older))
		assert.Equal(t, extract.ID, c.liveJob(t).ID, "the delete waits for the live job")
	})

	t.Run("refuses a document both removed and expired; expires nothing outside the set", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "EXR"), "EXR")
		ws := c.ontology.WorkspaceID
		doc := id26("FD-r1")
		c.published(t, c.publish("EXR-a", []*model.FoundryDocument{c.note("r1", "day_1", "1|a", day)}, nil))
		older := c.loadReading(t, "EXR-L1", c.setID)
		c.loaded(t, "exr1", doc)
		c.loadReading(t, "EXR-L2", c.setID)
		c.loaded(t, "exr2", doc)

		p := c.publish("EXR-b", nil, nil, doc)
		p.Expire = []string{doc}
		_, err := c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, syncWorker)
		assert.ErrorContains(t, err, "is both removed and expired")

		p = c.publish("EXR-c", nil, nil)
		p.Expire = []string{doc}
		_, err = c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, "sync-w2")
		assert.ErrorIs(t, err, store.ErrNotFound, "an expiry alone is no empty publish")

		upload := c.uploaded(t, "EXR-upload")
		p.Expire = []string{upload, id26("FD-unknown")}
		c.published(t, p)
		assert.Equal(t, []string{"day_1=" + doc + "@1|a"}, c.index(t))
		kept, err := c.db.GetFoundryDocument(ctx, ws, upload)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, kept.Status, "another set's document stays")
		assert.Equal(t, model.FoundryLoadReady, c.loadStatus(t, older))
		assert.Nil(t, c.liveJob(t))
	})
}
