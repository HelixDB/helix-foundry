package store_test

import (
	"context"
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
