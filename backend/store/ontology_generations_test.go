package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync"
	"testing"
	"time"
)

func TestOntologyGenerationQueue(t *testing.T) {
	workspaceID, ontologyID := id26("GEN-WS"), id26("GEN-ONT")
	goal, title := "customer billing health", "Customer billing health"
	newGeneration := func(suffix string) *model.OntologyGeneration {
		return &model.OntologyGeneration{
			ID: id26("GEN-" + suffix), WorkspaceID: workspaceID, OntologyID: ontologyID, Goal: &goal, Title: &title,
			Input: model.OntologyGenerationInput{
				Tables:        []model.OntologyGenerationTable{{ConnectorID: id26("GEN-CONN"), Table: "customers", SyncID: id26("GEN-SYNC")}},
				BaseVersionID: id26("GEN-BASE"),
			},
		}
	}
	draft := func(suffix string) *model.OntologyVersion {
		genID := id26("GEN-" + suffix)
		return &model.OntologyVersion{
			ID: id26("GEN-V-" + suffix), WorkspaceID: workspaceID, OntologyID: ontologyID, Source: model.OntologySourceGenerated,
			Format: model.OntologyFormatAgentBundle, GenerationID: &genID, Definition: json.RawMessage(`{"format":"agent_bundle"}`),
			DocumentSetIDs: &[]string{},
		}
	}
	// Every generation queues under the workspace's lock, so its workspace exists.
	const perHour = 10
	newStore := func(t *testing.T) store.Store {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		return db
	}
	succeeded := func(suffix string) model.OntologyGenerationOutcome {
		return model.OntologyGenerationOutcome{
			Status: model.OntologyGenerationSucceeded, Result: draft(suffix), Model: "claude-opus-5-5", InputTokens: 1000, OutputTokens: 100,
		}
	}

	// Two generations of one ontology would draft from the same base and race;
	// another ontology of the workspace drafts on its own.
	t.Run("keeps one live generation per ontology", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, newGeneration("A"), perHour))
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, newGeneration("B"), perHour), store.ErrConflict)

		other := newGeneration("O")
		other.OntologyID, other.Goal, other.Title = sampleOntology(t, db, workspaceID, "GEN-ONT-OTHER").ID, nil, nil
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, other, perHour))

		active, err := db.GetActiveOntologyGeneration(ctx, ontologyID)
		require.NoError(t, err)
		assert.Equal(t, id26("GEN-A"), active.ID)
		assert.Equal(t, goal, *active.Goal)
		assert.Equal(t, title, *active.Title)
		untitled, err := db.GetActiveOntologyGeneration(ctx, other.OntologyID)
		require.NoError(t, err)
		assert.Nil(t, untitled.Goal)
		assert.Nil(t, untitled.Title, "a generation without a goal has no title")
		assert.Equal(t, newGeneration("A").Input, active.Input)

		claimed, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.Equal(t, id26("GEN-A"), claimed.ID)
		require.NoError(t, db.FinishOntologyGeneration(ctx, claimed.ID, "worker-1", succeeded("A")))
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, newGeneration("B"), perHour), "a finished generation no longer blocks the next")
	})

	// Generations spend on the model whatever becomes of them, so every one
	// started in the last hour counts, across the workspace's ontologies.
	t.Run("caps the generations a workspace starts in an hour", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		of := func(ontology string) *model.OntologyGeneration {
			gen := newGeneration(ontology)
			gen.OntologyID = sampleOntology(t, db, workspaceID, "GEN-ONT-"+ontology).ID
			return gen
		}
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, of("1"), 2))
		claimed, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.FinishOntologyGeneration(ctx, claimed.ID, "worker-1", model.OntologyGenerationOutcome{
			Status: model.OntologyGenerationFailed, Error: "not valid",
		}))
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, of("2"), 2))
		third := of("3")
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, third, 2), store.ErrLimitExceeded, "a failed generation still counts")
		_, err = db.GetActiveOntologyGeneration(ctx, third.OntologyID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a refused generation is not queued")

		otherWorkspace := newGeneration("4")
		otherWorkspace.WorkspaceID = id26("GEN-WS-OTHER")
		createWorkspace(t, otherWorkspace.WorkspaceID)
		otherWorkspace.OntologyID = sampleOntology(t, db, otherWorkspace.WorkspaceID, "GEN-ONT-4").ID
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, otherWorkspace, 2), "another workspace has its own count")

		_, err = testDB.Exec(`UPDATE foundry_ontology_generations SET created_at=NOW(6) - INTERVAL 2 HOUR WHERE workspace_id=?`, workspaceID)
		require.NoError(t, err)
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, third, 2), "an hour later the count starts again")
	})

	// A revision runs the engine, not the model: it queues past the cap and
	// leaves the count alone.
	t.Run("leaves revisions outside the cap", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		of := func(suffix string, revise bool) *model.OntologyGeneration {
			gen := newGeneration(suffix)
			gen.OntologyID = sampleOntology(t, db, workspaceID, "GEN-ONT-"+suffix).ID
			if revise {
				gen.Input.Revise = &model.OntologyRevision{ParentVersionID: id26("GEN-PARENT"), Edits: model.OntologyEdits{model.OntologyRemove{ItemID: "rule.old"}}}
			}
			return gen
		}
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, of("REV-1", true), 1))
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, of("1", false), 1), "a revision doesn't count")
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, of("REV-2", true), 1), "nor is it capped")
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, of("2", false), 1), store.ErrLimitExceeded)
	})

	// The count and the insert share the workspace's lock, so racing requests
	// can't all see room under the cap.
	t.Run("holds the cap under concurrent requests", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		const requests, limit = 8, 3
		gens := make([]*model.OntologyGeneration, requests)
		for i := range gens {
			gens[i] = newGeneration(fmt.Sprintf("R%d", i))
			gens[i].OntologyID = sampleOntology(t, db, workspaceID, fmt.Sprintf("GEN-ONT-R%d", i)).ID
		}
		errs := make(chan error, requests)
		var wait sync.WaitGroup
		for _, gen := range gens {
			wait.Add(1)
			go func() {
				defer wait.Done()
				errs <- db.EnqueueOntologyGeneration(ctx, gen, limit)
			}()
		}
		wait.Wait()
		close(errs)
		queued := 0
		for err := range errs {
			if err == nil {
				queued++
				continue
			}
			assert.ErrorIs(t, err, store.ErrLimitExceeded)
		}
		assert.Equal(t, limit, queued)
	})

	t.Run("refuses a workspace that is gone and a cap below one", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		gone := newGeneration("A")
		gone.WorkspaceID = id26("GEN-WS-GONE")
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, gone, perHour), store.ErrNotFound)
		assert.Error(t, db.EnqueueOntologyGeneration(ctx, newGeneration("A"), 0))
		_, err := db.GetActiveOntologyGeneration(ctx, ontologyID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// GenerateOntology titles a generation once the cap has admitted it.
	t.Run("titles a queued generation", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		gen := newGeneration("A")
		gen.Title = nil
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, gen, perHour))
		require.NoError(t, db.SetOntologyGenerationTitle(ctx, gen.ID, "Billing health"))
		got, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, "Billing health", *got.Title)
		assert.ErrorIs(t, db.SetOntologyGenerationTitle(ctx, id26("GEN-NONE"), "Billing health"), store.ErrNotFound)
	})

	// The draft and the generation's result are written together, and the draft is not made current.
	t.Run("stores the draft with the result", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		ontology := sampleOntology(t, db, workspaceID, "GEN")
		gen := newGeneration("A")
		gen.OntologyID = ontology.ID
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, gen, perHour))
		claimed, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		result := succeeded("A")
		result.Result.OntologyID = ontology.ID
		require.NoError(t, db.FinishOntologyGeneration(ctx, claimed.ID, "worker-1", result))

		stored, err := db.GetOntologyGeneration(ctx, ontology.ID, claimed.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationSucceeded, stored.Status)
		assert.Equal(t, id26("GEN-V-A"), *stored.ResultVersionID)
		assert.Equal(t, "claude-opus-5-5", *stored.Model)
		assert.NotNil(t, stored.FinishedAt)
		version, err := db.GetOntologyVersion(ctx, ontology.ID, id26("GEN-V-A"))
		require.NoError(t, err)
		assert.Equal(t, 1, version.Version)
		assert.Equal(t, claimed.ID, *version.GenerationID)
		assert.Equal(t, model.OntologyFormatAgentBundle, version.Format, "the draft keeps the format its generator wrote")
		_, err = db.GetCurrentOntologyVersion(ctx, ontology.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a draft is never made current")
	})

	t.Run("stores nothing for a worker that lost the lease", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, newGeneration("A"), perHour))
		first, err := db.ClaimOntologyGeneration(ctx, "worker-1", -time.Second)
		require.NoError(t, err)
		second, err := db.ClaimOntologyGeneration(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, second, "an expired lease is claimable")
		assert.Equal(t, 2, second.Attempts)

		assert.ErrorIs(t, db.FinishOntologyGeneration(ctx, first.ID, "worker-1", succeeded("A")), store.ErrNotFound)
		_, err = db.GetOntologyVersion(ctx, ontologyID, id26("GEN-V-A"))
		assert.ErrorIs(t, err, store.ErrNotFound, "the lost attempt's draft was rolled back")
		again, err := db.ClaimOntologyGeneration(ctx, "worker-3", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, again, "a live lease is not claimable")
	})

	// A generation of an ontology deleted while it is queued or running can't
	// store its draft. Deleting ends it, so no worker claims it and pays for
	// its turn again; the one running loses its lease and stores nothing.
	t.Run("ends with its ontology's delete", func(t *testing.T) {
		for _, running := range []bool{false, true} {
			t.Run(map[bool]string{false: "queued", true: "running"}[running], func(t *testing.T) {
				ctx := context.Background()
				db := newStore(t)
				ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
				require.NoError(t, db.EnqueueOntologyGeneration(ctx, newGeneration("A"), perHour))
				if running {
					_, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
					require.NoError(t, err)
				}
				require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, ontologyID, id26("GEN-DEL")))
				gen, err := db.GetOntologyGeneration(ctx, ontologyID, id26("GEN-A"))
				require.NoError(t, err)
				assert.Equal(t, model.OntologyGenerationFailed, gen.Status)
				assert.Equal(t, "the ontology was deleted", *gen.Error)
				assert.Nil(t, gen.LeaseOwner)
				_, err = db.GetActiveOntologyGeneration(ctx, ontologyID)
				assert.ErrorIs(t, err, store.ErrNotFound)
				assert.ErrorIs(t, db.RenewOntologyGenerationLease(ctx, gen.ID, "worker-1", time.Minute), store.ErrNotFound)
				assert.ErrorIs(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", succeeded("A")), store.ErrNotFound)
				_, err = db.GetOntologyVersion(ctx, ontologyID, id26("GEN-V-A"))
				assert.ErrorIs(t, err, store.ErrNotFound, "no draft for a deleted ontology")
				again, err := db.ClaimOntologyGeneration(ctx, "worker-2", -time.Second)
				require.NoError(t, err)
				assert.Nil(t, again, "nothing left to run")
			})
		}
	})

	// A generation asked for as its ontology is deleted (GenerateOntology
	// reads it live, then the document sets and tables) can't be queued once
	// the delete went first: it could never store its draft, and workers
	// would claim it, and pay for its turn, forever. A revision skips the
	// workspace's lock, not the ontology's.
	t.Run("refuses an ontology deleted first", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, ontologyID, id26("GEN-DEL")))
		revision := newGeneration("R")
		revision.Input.Revise = &model.OntologyRevision{ParentVersionID: id26("GEN-PARENT"), Edits: model.OntologyEdits{model.OntologyRemove{ItemID: "rule.old"}}}
		for _, gen := range []*model.OntologyGeneration{newGeneration("A"), revision} {
			assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, gen, perHour), store.ErrNotFound)
			_, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
			assert.ErrorIs(t, err, store.ErrNotFound, "nothing queued")
		}
		elsewhere := newGeneration("B")
		elsewhere.OntologyID = sampleOntology(t, db, id26("GEN-WS-OTHER"), "GEN-ONT-ELSEWHERE").ID
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, elsewhere, perHour), store.ErrNotFound, "another workspace's ontology")
		again, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, again, "nothing to run")
	})

	// Every attempt is billed, so retries add to the totals rather than replace them.
	t.Run("adds up tokens across a retry and a failure", func(t *testing.T) {
		ctx := context.Background()
		db := newStore(t)
		ontologyID = sampleOntology(t, db, workspaceID, "GEN-ONT").ID
		require.NoError(t, db.EnqueueOntologyGeneration(ctx, newGeneration("A"), perHour))
		claimed, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		// Due a minute ago, so a test database whose clock lags the host's still finds it due.
		due := time.Now().Add(-time.Minute)
		require.NoError(t, db.FinishOntologyGeneration(ctx, claimed.ID, "worker-1", model.OntologyGenerationOutcome{
			RetryAt: &due, Error: "Claude is unavailable", Model: "claude-opus-5-5", ModelProvider: "anthropic", InputTokens: 300, OutputTokens: 30,
		}))
		retried, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, retried)
		require.NoError(t, db.FinishOntologyGeneration(ctx, retried.ID, "worker-1", model.OntologyGenerationOutcome{
			Status: model.OntologyGenerationFailed, Error: "not valid", InputTokens: 500, OutputTokens: 50,
		}))

		list, err := db.ListOntologyGenerations(ctx, ontologyID, 10)
		require.NoError(t, err)
		require.Len(t, list, 1)
		got := list[0]
		assert.Equal(t, model.OntologyGenerationFailed, got.Status)
		assert.Equal(t, "not valid", *got.Error)
		assert.Equal(t, 800, *got.InputTokens)
		assert.Equal(t, 80, *got.OutputTokens)
		assert.Equal(t, "claude-opus-5-5", *got.Model, "a failure without a model keeps the one recorded")
		var provider sql.NullString
		require.NoError(t, testDB.QueryRowContext(ctx, `SELECT model_provider FROM foundry_ontology_generations WHERE id=?`, got.ID).Scan(&provider))
		assert.Equal(t, "anthropic", provider.String, "and the provider kind that served it")
		other, err := db.ListOntologyGenerations(ctx, id26("GEN-OTHER"), 10)
		require.NoError(t, err)
		assert.Empty(t, other, "generations are scoped to their ontology")
	})
}

// A first draft's success (outcome.Load) makes the draft current and queues
// its load in the transaction that stores it, as set-current then
// create-load would. Whatever stops either, the generation still succeeds.
func TestFirstDraftLoad(t *testing.T) {
	user := id26("FDL-USER")
	// started queues and claims a first draft's generation of f's ontology.
	started := func(t *testing.T, f foundryFixture, tag string) *model.OntologyGeneration {
		t.Helper()
		ctx := context.Background()
		gen := &model.OntologyGeneration{ID: id26("FDL-GEN-" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID,
			RequestedByUserID: &user, Input: model.OntologyGenerationInput{LoadWhenReady: true}}
		require.NoError(t, f.db.EnqueueOntologyGeneration(ctx, gen, 10))
		claimed, err := f.db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.Equal(t, gen.ID, claimed.ID)
		return claimed
	}
	// drafted is the outcome the worker hands a first draft that loads.
	drafted := func(f foundryFixture, gen *model.OntologyGeneration, tag string) model.OntologyGenerationOutcome {
		v := &model.OntologyVersion{
			ID: id26("FDL-V-" + tag), WorkspaceID: gen.WorkspaceID, OntologyID: gen.OntologyID, Source: model.OntologySourceGenerated,
			Format: model.OntologyFormatAgentBundle, GenerationID: &gen.ID, Definition: json.RawMessage(`{"format":"agent_bundle"}`),
			DocumentSetIDs: &[]string{generalSetID(f.ontology.ID)}, CreatedByUserID: gen.RequestedByUserID,
		}
		token := model.FirstDraftLoadTokenPrefix + gen.ID
		return model.OntologyGenerationOutcome{Status: model.OntologyGenerationSucceeded, Result: v, Load: &model.FoundryGraphJob{
			ID: id26("FDL-L-" + tag), LoadID: id26("FDL-L-" + tag), WorkspaceID: gen.WorkspaceID, OntologyID: gen.OntologyID, VersionID: v.ID,
			Kind: model.FoundryGraphJobLoad, Phase: model.FoundryGraphJobProvisioning, ClientToken: &token, RequestedByUserID: gen.RequestedByUserID,
		}}
	}
	// succeeded checks the generation succeeded with its draft stored.
	succeeded := func(t *testing.T, f foundryFixture, gen *model.OntologyGeneration, versionID string) {
		t.Helper()
		ctx := context.Background()
		got, err := f.db.GetOntologyGeneration(ctx, f.ontology.ID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationSucceeded, got.Status)
		require.NotNil(t, got.ResultVersionID)
		assert.Equal(t, versionID, *got.ResultVersionID)
		_, err = f.db.GetOntologyVersion(ctx, f.ontology.ID, versionID)
		require.NoError(t, err, "the draft is stored")
	}

	t.Run("makes the draft current and queues its load", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL1")
		gen := started(t, f, "1")
		outcome := drafted(f, gen, "1")
		require.NoError(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
		succeeded(t, f, gen, outcome.Result.ID)
		o := f.ontologyNow(t)
		require.NotNil(t, o.CurrentVersionID)
		assert.Equal(t, outcome.Result.ID, *o.CurrentVersionID)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, outcome.Load.ID, job.ID)
		assert.Equal(t, job.ID, job.LoadID)
		assert.Equal(t, model.FoundryGraphJobLoad, job.Kind)
		assert.Equal(t, model.FoundryGraphJobQueued, job.Status)
		assert.Equal(t, model.FoundryGraphJobProvisioning, job.Phase)
		assert.Equal(t, outcome.Result.ID, job.VersionID)
		require.NotNil(t, job.RequestedByUserID)
		assert.Equal(t, user, *job.RequestedByUserID)
		byToken, err := f.db.GetFoundryGraphJobByClientToken(ctx, f.ontology.ID, "first-draft:"+gen.ID)
		require.NoError(t, err)
		assert.Equal(t, job.ID, byToken.ID, "the load is found by its generation's token")
		assert.Equal(t, model.FoundryLoadWriting, f.loadStatus(t, job.LoadID))

		f.loaded(t, "w1")
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, job.LoadID))
		require.NotNil(t, f.ontologyNow(t).CurrentLoadID)
		assert.Equal(t, job.LoadID, *f.ontologyNow(t).CurrentLoadID, "the first load serves reads once it ends")
	})

	// A version made current since the request is the user's choice: the
	// draft is stored for review, and nothing is loaded.
	t.Run("leaves a version made current meanwhile", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL2")
		gen := started(t, f, "2")
		chosen := f.version(t, "CHOSEN")
		require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, chosen, nil))
		outcome := drafted(f, gen, "2")
		require.NoError(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
		succeeded(t, f, gen, outcome.Result.ID)
		assert.Equal(t, chosen, *f.ontologyNow(t).CurrentVersionID)
		assert.Nil(t, f.liveJob(t), "no load is queued")
		_, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, outcome.Load.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// One writer per database: the live job keeps it, and the draft is left
	// current for the user to load once it ends.
	t.Run("leaves the draft current, unloaded, while a load is live", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL3")
		live := f.load(t, "LIVE")
		gen := started(t, f, "3")
		outcome := drafted(f, gen, "3")
		require.NoError(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
		succeeded(t, f, gen, outcome.Result.ID)
		assert.Equal(t, outcome.Result.ID, *f.ontologyNow(t).CurrentVersionID)
		job := f.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, live, job.ID, "the live load keeps the database")
		_, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, outcome.Load.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "no load of the draft is written")
		_, err = f.db.GetFoundryGraphJobByClientToken(ctx, f.ontology.ID, "first-draft:"+gen.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// A token is the user's to send, so a load already holding the draft's
	// is left alone, and no half-written load of the draft is kept.
	t.Run("leaves a load that took the draft's token", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL4")
		gen := started(t, f, "4")
		f.version(t, "TOKEN")
		taken := loadJob(f.ontology, "TOKEN")
		token := "first-draft:" + gen.ID
		taken.ClientToken = &token
		require.NoError(t, f.db.CreateFoundryLoad(ctx, taken))
		f.finish(t, "w1", "token", model.FoundryGraphJobFailed)
		outcome := drafted(f, gen, "4")
		require.NoError(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
		succeeded(t, f, gen, outcome.Result.ID)
		assert.Equal(t, outcome.Result.ID, *f.ontologyNow(t).CurrentVersionID)
		assert.Nil(t, f.liveJob(t))
		_, err := f.db.GetFoundryLoad(ctx, f.ontology.ID, outcome.Load.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "no load of the draft is written")
		var loads int
		require.NoError(t, testDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_loads WHERE ontology_id=?`, f.ontology.ID).Scan(&loads))
		assert.Equal(t, 1, loads, "only the user's load")
	})

	// A draft the worker didn't load waits for review, as before.
	t.Run("stores a draft without a load as before", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL5")
		gen := started(t, f, "5")
		outcome := drafted(f, gen, "5")
		outcome.Load = nil
		require.NoError(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
		succeeded(t, f, gen, outcome.Result.ID)
		assert.Nil(t, f.ontologyNow(t).CurrentVersionID)
		assert.Nil(t, f.liveJob(t))
	})

	// A load that isn't the draft's, or one beside no draft, is the worker's
	// mistake: nothing is stored, and the attempt keeps its lease.
	t.Run("refuses a load that is not the draft's", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "FDL6")
		gen := started(t, f, "6")
		for name, change := range map[string]func(*model.OntologyGenerationOutcome){
			"another version":      func(o *model.OntologyGenerationOutcome) { o.Load.VersionID = id26("FDL-OTHER") },
			"another ontology":     func(o *model.OntologyGenerationOutcome) { o.Load.OntologyID = id26("FDL-OTHER") },
			"an extract":           func(o *model.OntologyGenerationOutcome) { o.Load.Kind = model.FoundryGraphJobExtract },
			"a load of another id": func(o *model.OntologyGenerationOutcome) { o.Load.LoadID = id26("FDL-OTHER") },
			"no token":             func(o *model.OntologyGenerationOutcome) { o.Load.ClientToken = nil },
			"a failure":            func(o *model.OntologyGenerationOutcome) { o.Status, o.Result = model.OntologyGenerationFailed, nil },
			"a retry": func(o *model.OntologyGenerationOutcome) {
				at := time.Now()
				o.RetryAt = &at
			},
		} {
			outcome := drafted(f, gen, "6")
			change(&outcome)
			assert.Error(t, f.db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome), name)
		}
		got, err := f.db.GetOntologyGeneration(ctx, f.ontology.ID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationRunning, got.Status)
		assert.Nil(t, f.ontologyNow(t).CurrentVersionID)
		assert.Nil(t, f.liveJob(t))
	})
}
