package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestOntologyGenerationQuestions(t *testing.T) {
	workspaceID := id26("GQ-WS")
	newStore := func(t *testing.T) (store.Store, string) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		return db, sampleOntology(t, db, workspaceID, "GQ-ONT").ID
	}
	queue := func(t *testing.T, db store.Store, ontologyID, suffix string) *model.OntologyGeneration {
		gen := &model.OntologyGeneration{ID: id26("GQ-GEN-" + suffix), WorkspaceID: workspaceID, OntologyID: ontologyID,
			Input: model.OntologyGenerationInput{Tables: []model.OntologyGenerationTable{{ConnectorID: id26("GQ-CONN"), Table: "customers"}}}}
		return startedGeneration(t, db, gen)
	}
	questions := []model.OntologyQuestion{
		{Question: agentquestion.Question{ID: "q1", Text: "Is a customer an account?", Blocking: true}, Items: []string{"type.Customer"}},
		{Question: agentquestion.Question{ID: "q2", Header: "Renewals", Text: "How often do they renew?", Choice: &agentquestion.Choice{
			Options: []agentquestion.Option{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}}, Multiple: true, Recommended: "Monthly"}},
			Items: []string{}},
	}
	state := json.RawMessage(`{"draft":{"rev":3},"excerpt":"Acme Corp"}`)
	asked := model.OntologyGenerationOutcome{Status: model.OntologyGenerationNeedsInput, Model: "claude-opus-5-5", InputTokens: 100, OutputTokens: 10,
		Asked: &model.OntologyGenerationAsked{State: state, DraftRev: 3, Questions: questions}}
	storedState := func(t *testing.T, id string) []byte {
		var raw []byte
		require.NoError(t, testDB.QueryRow(`SELECT turn_state FROM foundry_ontology_generations WHERE id=?`, id).Scan(&raw))
		return raw
	}

	// A turn that asked waits, still the ontology's live generation, until
	// someone answers; its answers carry on from the state it kept, which
	// only the claim reads and the end drops.
	t.Run("waits for answers, then carries on from the state", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked))

		waiting, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationNeedsInput, waiting.Status)
		require.NotNil(t, waiting.Questions)
		assert.Equal(t, uint64(3), waiting.Questions.DraftRev)
		assert.Equal(t, 1, waiting.Questions.Round)
		assert.Equal(t, questions, waiting.Questions.Asked)
		assert.Nil(t, waiting.Questions.Answers)
		assert.False(t, waiting.Questions.AskedAt.IsZero())
		assert.Nil(t, waiting.TurnState, "the state is read by the claim alone")
		assert.Nil(t, waiting.FinishedAt)
		assert.Equal(t, 100, *waiting.InputTokens)
		active, err := db.GetActiveOntologyGeneration(ctx, ontologyID)
		require.NoError(t, err)
		assert.Equal(t, gen.ID, active.ID, "it stays the ontology's live generation")
		assert.ErrorIs(t, db.EnqueueOntologyGeneration(ctx, &model.OntologyGeneration{ID: id26("GQ-GEN-B"), WorkspaceID: workspaceID, OntologyID: ontologyID}, 10),
			store.ErrConflict)
		none, err := db.ClaimOntologyGeneration(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		assert.Nil(t, none, "a generation waiting for answers is never due")
		listed, err := db.ListOntologyGenerations(ctx, ontologyID, 10)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, questions, listed[0].Questions.Asked, "lists carry the questions")
		assert.Nil(t, listed[0].TurnState)

		answers := []agentquestion.Answer{{QuestionID: "q1", Options: []string{}, Text: "Yes"}, {QuestionID: "q2", Options: []string{"Monthly", "Yearly"}, Text: "and weekly"}}
		assert.ErrorIs(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 2, answers), store.ErrNotFound, "answers to another revision")
		assert.ErrorIs(t, db.AnswerOntologyGeneration(ctx, id26("GQ-OTHER"), gen.ID, 3, answers), store.ErrNotFound, "another ontology's")
		require.NoError(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, answers))
		assert.ErrorIs(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, answers), store.ErrNotFound, "answered once")

		resumed, err := db.ClaimOntologyGeneration(ctx, "worker-2", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, resumed)
		assert.Equal(t, gen.ID, resumed.ID)
		assert.Equal(t, 1, resumed.Attempts, "the answering turn's attempts start again")
		assert.JSONEq(t, string(state), string(resumed.TurnState))
		assert.Equal(t, answers, resumed.Questions.Answers)

		v := &model.OntologyVersion{ID: id26("GQ-V-A"), WorkspaceID: workspaceID, OntologyID: ontologyID, Source: model.OntologySourceGenerated,
			Format: model.OntologyFormatAgentBundle, GenerationID: &gen.ID, Definition: json.RawMessage(`{"format":"agent_bundle"}`), DocumentSetIDs: &[]string{}}
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-2", model.OntologyGenerationOutcome{
			Status: model.OntologyGenerationSucceeded, Result: v, InputTokens: 50,
		}))
		done, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationSucceeded, done.Status)
		assert.Equal(t, 150, *done.InputTokens, "both turns are billed")
		assert.Equal(t, answers, done.Questions.Answers, "the round's questions and answers stay")
		assert.Nil(t, storedState(t, gen.ID), "the end drops the state")
	})

	// A turn that asks again replaces the round before; a retry of the
	// answering turn keeps the state and answers for the next attempt.
	t.Run("asks again and retries an answering turn", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked))
		require.NoError(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, []agentquestion.Answer{{QuestionID: "q1", Text: "Yes"}}))
		claimed, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		due := time.Now().Add(-time.Minute)
		require.NoError(t, db.FinishOntologyGeneration(ctx, claimed.ID, "worker-1", model.OntologyGenerationOutcome{RetryAt: &due, Error: "unavailable"}))
		retried, err := db.ClaimOntologyGeneration(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, retried)
		assert.JSONEq(t, string(state), string(retried.TurnState), "a retry carries on from the same state")
		assert.Len(t, retried.Questions.Answers, 1)
		assert.Equal(t, 1, retried.Questions.Round, "a retry is the same round")

		again := asked
		again.Asked = &model.OntologyGenerationAsked{State: json.RawMessage(`{"draft":{"rev":5}}`), DraftRev: 5,
			Questions: []model.OntologyQuestion{{Question: agentquestion.Question{ID: "q3", Text: "Which currency?", Blocking: true}, Items: []string{}}}}
		require.NoError(t, db.FinishOntologyGeneration(ctx, retried.ID, "worker-1", again))
		waiting, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationNeedsInput, waiting.Status)
		assert.Equal(t, uint64(5), waiting.Questions.DraftRev)
		assert.Equal(t, 2, waiting.Questions.Round, "the rounds count up")
		assert.Equal(t, "q3", waiting.Questions.Asked[0].ID)
		assert.Nil(t, waiting.Questions.Answers, "a new round has no answers yet")
		assert.JSONEq(t, `{"draft":{"rev":5}}`, string(storedState(t, gen.ID)))
	})

	// A cancelled generation stores nothing, holds the ontology no longer,
	// and drops its state; a worker running it loses its lease.
	t.Run("cancels a live generation", func(t *testing.T) {
		for name, setup := range map[string]func(t *testing.T, db store.Store, gen *model.OntologyGeneration){
			"running": func(*testing.T, store.Store, *model.OntologyGeneration) {},
			"waiting": func(t *testing.T, db store.Store, gen *model.OntologyGeneration) {
				require.NoError(t, db.FinishOntologyGeneration(context.Background(), gen.ID, "worker-1", asked))
			},
		} {
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				db, ontologyID := newStore(t)
				gen := queue(t, db, ontologyID, "A")
				setup(t, db, gen)
				assert.ErrorIs(t, db.CancelOntologyGeneration(ctx, id26("GQ-OTHER"), gen.ID), store.ErrNotFound, "another ontology's")
				require.NoError(t, db.CancelOntologyGeneration(ctx, ontologyID, gen.ID))
				assert.ErrorIs(t, db.CancelOntologyGeneration(ctx, ontologyID, gen.ID), store.ErrNotFound, "it ended")

				cancelled, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
				require.NoError(t, err)
				assert.Equal(t, model.OntologyGenerationCancelled, cancelled.Status)
				assert.NotNil(t, cancelled.FinishedAt)
				assert.Nil(t, storedState(t, gen.ID))
				_, err = db.GetActiveOntologyGeneration(ctx, ontologyID)
				assert.ErrorIs(t, err, store.ErrNotFound)
				assert.ErrorIs(t, db.RenewOntologyGenerationLease(ctx, gen.ID, "worker-1", time.Minute), store.ErrNotFound)
				assert.ErrorIs(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked), store.ErrNotFound)
				assert.ErrorIs(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, nil), store.ErrNotFound)
				require.NoError(t, db.EnqueueOntologyGeneration(ctx, &model.OntologyGeneration{ID: id26("GQ-GEN-B"), WorkspaceID: workspaceID, OntologyID: ontologyID}, 10),
					"the ontology may generate again")
			})
		}
	})

	t.Run("lists those asked before a time, oldest first", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked))
		other := sampleOntology(t, db, workspaceID, "GQ-ONT-2").ID
		later := queue(t, db, other, "B")
		require.NoError(t, db.FinishOntologyGeneration(ctx, later.ID, "worker-1", asked))
		_, err := testDB.Exec(`UPDATE foundry_ontology_generations SET asked_at=NOW(6) - INTERVAL 8 DAY WHERE id=?`, gen.ID)
		require.NoError(t, err)

		due, err := db.ListOntologyGenerationsAskedBefore(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		require.Len(t, due, 1)
		assert.Equal(t, gen.ID, due[0].ID)
		assert.Equal(t, questions, due[0].Questions.Asked)
		all, err := db.ListOntologyGenerationsAskedBefore(ctx, time.Now().Add(time.Minute), 10)
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, []string{gen.ID, later.ID}, []string{all[0].ID, all[1].ID})
		require.NoError(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, nil))
		due, err = db.ListOntologyGenerationsAskedBefore(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		assert.Empty(t, due, "an answered one waits no longer")
	})

	// An older binary never clears turn_state, so the checks let an ended
	// generation keep it: such a binary's end, or its ontology's delete,
	// mid-deploy never fails.
	t.Run("lets an older binary end a generation that kept its state", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked))
		require.NoError(t, db.AnswerOntologyGeneration(ctx, ontologyID, gen.ID, 3, []agentquestion.Answer{{QuestionID: "q1", Text: "Yes"}}))
		_, err := testDB.Exec(`UPDATE foundry_ontology_generations SET status='failed', error='old', active_key=NULL, finished_at=NOW(6) WHERE id=?`, gen.ID)
		require.NoError(t, err)
		assert.NotNil(t, storedState(t, gen.ID))
	})

	t.Run("ends with its ontology's delete, dropping the state", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", asked))
		require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, ontologyID, id26("GQ-DEL")))
		ended, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationFailed, ended.Status)
		assert.Nil(t, storedState(t, gen.ID))
	})

	// The outcome says what was asked exactly when the generation waits,
	// on state and a question that blocks it.
	t.Run("refuses an outcome that waits on nothing", func(t *testing.T) {
		ctx := context.Background()
		db, ontologyID := newStore(t)
		gen := queue(t, db, ontologyID, "A")
		due := time.Now()
		for name, outcome := range map[string]model.OntologyGenerationOutcome{
			"needs input, nothing asked": {Status: model.OntologyGenerationNeedsInput},
			"asked, but failed":          {Status: model.OntologyGenerationFailed, Asked: asked.Asked},
			"asked, and retried":         {Status: model.OntologyGenerationNeedsInput, Asked: asked.Asked, RetryAt: &due},
			"no state": {Status: model.OntologyGenerationNeedsInput, Asked: &model.OntologyGenerationAsked{DraftRev: 1,
				Questions: questions}},
			"nothing blocking": {Status: model.OntologyGenerationNeedsInput, Asked: &model.OntologyGenerationAsked{State: state, DraftRev: 1,
				Questions: questions[1:]}},
		} {
			assert.Error(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome), name)
		}
		running, err := db.GetOntologyGeneration(ctx, ontologyID, gen.ID)
		require.NoError(t, err)
		assert.Equal(t, model.OntologyGenerationRunning, running.Status, "nothing was stored")
	})
}
