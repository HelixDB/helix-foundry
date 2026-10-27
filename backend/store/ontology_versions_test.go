package store_test

import (
	"context"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOntologyVersions(t *testing.T) {
	workspaceID := id26("ONT-WS")
	newVersion := func(ontologyID, suffix string) *model.OntologyVersion {
		return &model.OntologyVersion{
			ID: id26("ONT-" + suffix), WorkspaceID: workspaceID, OntologyID: ontologyID, Source: model.OntologySourceManual,
			Format: model.OntologyFormatFoundryV1, Definition: json.RawMessage(`{"entities":[]}`), DocumentSetIDs: &[]string{},
		}
	}

	// Each ontology counts its own versions from 1, even two of one workspace.
	t.Run("numbers versions per ontology", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		a, b := sampleOntology(t, db, workspaceID, "ONT-O-A").ID, sampleOntology(t, db, workspaceID, "ONT-O-B").ID
		first, second, other := newVersion(a, "A1"), newVersion(a, "A2"), newVersion(b, "B1")
		require.NoError(t, db.CreateOntologyVersion(ctx, first))
		require.NoError(t, db.CreateOntologyVersion(ctx, second))
		require.NoError(t, db.CreateOntologyVersion(ctx, other))
		assert.Equal(t, 1, first.Version)
		assert.Equal(t, 2, second.Version)
		assert.Equal(t, 1, other.Version)

		listed, err := db.ListOntologyVersions(ctx, a, 10)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		assert.Equal(t, 2, listed[0].Version, "newest first")
		assert.Nil(t, listed[0].Definition, "lists leave the definition out")
		stored, err := db.GetOntologyVersion(ctx, a, first.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"entities":[]}`, string(stored.Definition))
		_, err = db.GetOntologyVersion(ctx, b, first.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "versions are scoped to their ontology")
	})

	// An agent draft and a Definition v1 version live side by side; each reads back as its own format.
	t.Run("keeps each version's format", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ontologyID := sampleOntology(t, db, workspaceID, "F").ID
		manual, agent := newVersion(ontologyID, "F1"), newVersion(ontologyID, "F2")
		agent.Format, agent.Source, agent.Definition = model.OntologyFormatAgentBundle, model.OntologySourceGenerated, json.RawMessage(`{"format":"agent_bundle"}`)
		require.NoError(t, db.CreateOntologyVersion(ctx, manual))
		require.NoError(t, db.CreateOntologyVersion(ctx, agent))
		require.NoError(t, db.SetCurrentOntologyVersion(ctx, ontologyID, agent.ID, nil), "an agent draft can be made current")

		listed, err := db.ListOntologyVersions(ctx, ontologyID, 10)
		require.NoError(t, err)
		require.Len(t, listed, 2)
		assert.Equal(t, model.OntologyFormatAgentBundle, listed[0].Format)
		assert.Equal(t, model.OntologyFormatFoundryV1, listed[1].Format)
		agentVersion, err := db.GetOntologyVersion(ctx, ontologyID, agent.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"format":"agent_bundle"}`, string(agentVersion.Definition), "a single version keeps its definition")
		current, err := db.GetCurrentOntologyVersion(ctx, ontologyID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyFormatAgentBundle, current.Format)
		assert.JSONEq(t, `{"format":"agent_bundle"}`, string(current.Definition))

		unknown := newVersion(ontologyID, "F3")
		unknown.Format = ""
		assert.ErrorContains(t, db.CreateOntologyVersion(ctx, unknown), "format")
	})

	// Two editors choosing the current version cannot silently overwrite each other.
	t.Run("moves the current pointer only from the expected version", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		ontologyID := sampleOntology(t, db, workspaceID, "C").ID
		first, second := newVersion(ontologyID, "C1"), newVersion(ontologyID, "C2")
		require.NoError(t, db.CreateOntologyVersion(ctx, first))
		require.NoError(t, db.CreateOntologyVersion(ctx, second))

		_, err := db.GetCurrentOntologyVersion(ctx, ontologyID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a new ontology has no current version")
		require.NoError(t, db.SetCurrentOntologyVersion(ctx, ontologyID, first.ID, nil))
		assert.ErrorIs(t, db.SetCurrentOntologyVersion(ctx, ontologyID, second.ID, nil), store.ErrConflict,
			"the pointer moved since the caller read it")
		require.NoError(t, db.SetCurrentOntologyVersion(ctx, ontologyID, second.ID, &first.ID))

		current, err := db.GetCurrentOntologyVersion(ctx, ontologyID)
		require.NoError(t, err)
		assert.Equal(t, second.ID, current.ID)

		require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, ontologyID, id26("FN-D-ONT")))
		assert.ErrorIs(t, db.SetCurrentOntologyVersion(ctx, ontologyID, first.ID, &second.ID), store.ErrConflict,
			"a deleted ontology's pointer does not move")
	})
}
