package store_test

import (
	"context"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleOntology writes a live ontology of workspaceID on a tenant of its
// own, and the workspace when it has none. The ontology gets a document set
// of the workspace, generalSetID(o.ID) named "General <tag>", which the
// fixtures' versions read.
func sampleOntology(t *testing.T, db store.Store, workspaceID, tag string) *model.FoundryOntology {
	t.Helper()
	ctx := context.Background()
	createWorkspace(t, workspaceID)
	o := &model.FoundryOntology{
		ID: id26("FO-" + tag), WorkspaceID: workspaceID, Name: "Ontology " + tag, ProjectID: id26("FP-" + tag),
		TenantID: id26("FT-" + tag), ClientToken: "ontology-" + tag,
	}
	require.NoError(t, db.CreateFoundryOntology(ctx, o))
	require.NoError(t, db.CreateFoundryDocumentSet(ctx, &model.FoundryDocumentSet{
		ID: generalSetID(o.ID), WorkspaceID: workspaceID, Name: "General " + tag,
	}, 1000))
	stored, err := db.GetFoundryOntology(ctx, workspaceID, o.ID)
	require.NoError(t, err)
	return stored
}

func TestFoundryOntologies(t *testing.T) {
	workspaceID := id26("FO-WS")

	// A retried create finds the first; a second ontology can't share a database.
	t.Run("binds each live ontology to its own tenant", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		first := sampleOntology(t, db, workspaceID, "A")
		assert.Equal(t, "Ontology A", first.Name)
		assert.Nil(t, first.CurrentVersionID)
		assert.Nil(t, first.CurrentLoadID)
		assert.Nil(t, first.DeletedAt)

		sameToken := *first
		sameToken.ID, sameToken.TenantID = id26("FO-A2"), id26("FT-A2")
		assert.ErrorIs(t, db.CreateFoundryOntology(ctx, &sameToken), store.ErrConflict)
		byToken, err := db.GetFoundryOntologyByClientToken(ctx, workspaceID, first.ClientToken)
		require.NoError(t, err)
		assert.Equal(t, first.ID, byToken.ID)

		sameTenant := *first
		sameTenant.ID, sameTenant.ClientToken = id26("FO-A3"), "ontology-A3"
		assert.ErrorIs(t, db.CreateFoundryOntology(ctx, &sameTenant), store.ErrConflict, "one live ontology per database")

		_, err = db.GetFoundryOntology(ctx, id26("FO-WS-other"), first.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "ontologies are scoped to their workspace")
	})

	// Deleting keeps the row for history; an ontology that never loaded
	// frees its database for a new ontology at once.
	t.Run("frees the tenant of an ontology without loads on delete", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		first := sampleOntology(t, db, workspaceID, "B")
		require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, first.ID, id26("FN-D-B")))
		assert.ErrorIs(t, db.DeleteFoundryOntology(ctx, workspaceID, first.ID, id26("FN-D-B2")), store.ErrNotFound, "deleted once")
		deleted, err := db.GetFoundryOntology(ctx, workspaceID, first.ID)
		require.NoError(t, err)
		require.NotNil(t, deleted.DeletedAt)
		_, err = db.GetActiveFoundryGraphJob(ctx, first.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "nothing to delete from the database")
		_, err = db.GetFoundryOntologyByTenant(ctx, first.TenantID)
		assert.ErrorIs(t, err, store.ErrNotFound, "nothing holds the database")

		again := *first
		again.ID, again.ClientToken = id26("FO-B2"), "ontology-B2"
		require.NoError(t, db.CreateFoundryOntology(ctx, &again), "the database takes a new ontology")
		live, _, err := db.ListFoundryOntologies(ctx, workspaceID, "", 10)
		require.NoError(t, err)
		require.Len(t, live, 1, "lists leave deleted ontologies out")
		assert.Equal(t, again.ID, live[0].ID)
		holder, err := db.GetFoundryOntologyByTenant(ctx, first.TenantID)
		require.NoError(t, err)
		assert.Equal(t, again.ID, holder.ID)
	})

	// A live ontology always holds its database; the schema refuses one that doesn't.
	t.Run("never lets a live ontology go of its database", func(t *testing.T) {
		db := newTestStore(t)
		o := sampleOntology(t, db, workspaceID, "C")
		_, err := testDB.Exec(`UPDATE foundry_ontologies SET live_key=NULL WHERE id=?`, o.ID)
		assert.Error(t, err)
		_, err = testDB.Exec(`UPDATE foundry_ontologies SET deleted_at=NOW(6), current_load_id=? WHERE id=?`, id26("FJ-x"), o.ID)
		assert.Error(t, err, "a deleted ontology has no current load")
		assert.Nil(t, o.CurrentLoadID)
		assert.False(t, o.ExtractOwed)
	})

	// The list pages newest first.
	t.Run("pages the workspace's ontologies", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		for _, tag := range []string{"E1", "E2", "E3"} {
			sampleOntology(t, db, workspaceID, tag)
			time.Sleep(2 * time.Millisecond)
		}
		sampleOntology(t, db, id26("FO-WS-other"), "E4")
		page, next, err := db.ListFoundryOntologies(ctx, workspaceID, "", 2)
		require.NoError(t, err)
		require.Len(t, page, 2)
		assert.Equal(t, id26("FO-E3"), page[0].ID)
		rest, last, err := db.ListFoundryOntologies(ctx, workspaceID, next, 2)
		require.NoError(t, err)
		require.Len(t, rest, 1)
		assert.Equal(t, id26("FO-E1"), rest[0].ID)
		assert.Empty(t, last)
		_, _, err = db.ListFoundryOntologies(ctx, workspaceID, "not-a-token", 2)
		assert.ErrorIs(t, err, store.ErrInvalidPageToken)
	})
}
