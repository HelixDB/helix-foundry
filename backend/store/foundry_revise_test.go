package store_test

import (
	"context"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/require"
)

func TestFoundryRevisionQueue(t *testing.T) {
	for _, mode := range []string{"version", "preview", "refused", "lost lease"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := newTestStore(t)
			o := sampleOntology(t, db, id26("REV-WS"), "REV")
			gen := &model.OntologyGeneration{ID: id26("REV-GEN"), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, Input: model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{{ConnectorID: "c", Table: "items"}}, Revise: &model.OntologyRevision{ParentVersionID: id26("REV-PARENT"), DryRun: mode == "preview", Edits: model.OntologyEdits{model.OntologyRemove{ItemID: "rule.old"}}}}}
			require.NoError(t, db.EnqueueOntologyGeneration(ctx, gen, 1))
			claimed, err := db.ClaimOntologyGeneration(ctx, "worker", time.Minute)
			require.NoError(t, err)
			require.Equal(t, gen.ID, claimed.ID)
			require.Equal(t, gen.Input, claimed.Input)
			report := json.RawMessage(`{"status":"revised","findings":[],"cards":[{"item_id":"rule.old"}]}`)
			outcome := model.OntologyGenerationOutcome{Status: model.OntologyGenerationSucceeded, ReviseResult: report}
			if mode == "version" || mode == "lost lease" {
				outcome.Result = &model.OntologyVersion{ID: id26("REV-VERSION"), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, Source: model.OntologySourceManual, Format: model.OntologyFormatAgentBundle, GenerationID: &gen.ID, Definition: json.RawMessage(`{"parent_version_id":"parent","status":"done"}`), DocumentSetIDs: &[]string{}}
			}
			if mode == "refused" {
				outcome.Status = model.OntologyGenerationFailed
				outcome.Error = "edits refused"
				outcome.ReviseResult = json.RawMessage(`{"status":"refused","findings":[{"kind":"EditNotAllowed"}],"cards":[]}`)
			}
			if mode == "lost lease" {
				require.ErrorIs(t, db.FinishOntologyGeneration(ctx, gen.ID, "wrong-worker", outcome), store.ErrNotFound)
				_, err := db.GetOntologyVersion(ctx, o.ID, outcome.Result.ID)
				require.ErrorIs(t, err, store.ErrNotFound)
				return
			}
			require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker", outcome))
			got, err := db.GetOntologyGeneration(ctx, o.ID, gen.ID)
			require.NoError(t, err)
			require.JSONEq(t, string(outcome.ReviseResult), string(got.ReviseResult))
			require.Equal(t, outcome.Status, got.Status)
			require.Equal(t, gen.Input, got.Input)
			// Lists, which poll a hundred at a time, leave out what can run to
			// megabytes: the result and the edits. The rest is as it was.
			listed, err := db.ListOntologyGenerations(ctx, o.ID, 10)
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Nil(t, listed[0].ReviseResult)
			require.Nil(t, listed[0].Input.Revise.Edits)
			listedInput := listed[0].Input
			listedInput.Revise = &model.OntologyRevision{ParentVersionID: listed[0].Input.Revise.ParentVersionID, DryRun: listed[0].Input.Revise.DryRun, Edits: gen.Input.Revise.Edits}
			require.Equal(t, gen.Input, listedInput)
			require.Equal(t, got.Status, listed[0].Status)
			versions, err := db.ListOntologyVersions(ctx, o.ID, 10)
			require.NoError(t, err)
			if mode == "version" {
				require.Len(t, versions, 1)
				require.Equal(t, model.OntologySourceManual, versions[0].Source)
				require.Equal(t, gen.ID, *versions[0].GenerationID)
			} else {
				require.Empty(t, versions)
				require.Nil(t, got.ResultVersionID)
			}
			require.ErrorIs(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker", outcome), store.ErrNotFound)
		})
	}
	t.Run("normal generation cannot finish without version", func(t *testing.T) {
		db := newTestStore(t)
		ctx := context.Background()
		createWorkspace(t, id26("NOV-WS"))
		o := sampleOntology(t, db, id26("NOV-WS"), "NOV")
		gen := &model.OntologyGeneration{ID: id26("NOV-GEN"), WorkspaceID: o.WorkspaceID, OntologyID: o.ID}
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, gen, 1))
		_, err := db.ClaimOntologyGeneration(ctx, "w", time.Minute)
		require.NoError(t, err)
		require.Error(t, db.FinishOntologyGeneration(ctx, gen.ID, "w", model.OntologyGenerationOutcome{Status: model.OntologyGenerationSucceeded}))
	})
}
