package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var suggestionsReady = model.FoundryAutomationSuggestionsEnd{
	Suggestions: json.RawMessage(`[{"title":"Renewal radar","prompt":"Every Monday, list renewals.","icon":"watch"}]`), USDMicros: 1200,
}

func suggestionVersion(n int) string { return id26(fmt.Sprintf("SV-%03d-", n)) }

// queueSuggestions queues ontology's version n's suggestions, 20 an hour,
// retried an hour after a failure.
func queueSuggestions(db store.Store, workspaceID, ontologyID string, n int) (*model.FoundryAutomationSuggestions, error) {
	return db.QueueFoundryAutomationSuggestions(context.Background(), workspaceID, ontologyID, suggestionVersion(n), 20, time.Hour)
}

// Concurrent first reads queue once; a queued, running or ready row is
// returned as it is; a failure is queued again only after retryAfter.
func TestFoundryAutomationSuggestionsQueue(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, ontology := id26("SW-queue"), id26("SO-queue")
	createWorkspace(t, ws)

	var wg sync.WaitGroup
	rows := make([]*model.FoundryAutomationSuggestions, 8)
	errs := make([]error, len(rows))
	for i := range rows {
		wg.Go(func() { rows[i], errs[i] = queueSuggestions(db, ws, ontology, 1) })
	}
	wg.Wait()
	for i := range rows {
		require.NoError(t, errs[i])
		assert.Equal(t, model.FoundryAutomationSuggestionsQueued, rows[i].Status)
		assert.Equal(t, rows[0].RequestedAt, rows[i].RequestedAt, "one row, queued once")
	}
	var count int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_automation_suggestions WHERE workspace_id=?`, ws).Scan(&count))
	assert.Equal(t, 1, count)

	got, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(1))
	require.NoError(t, err)
	assert.Equal(t, ws, got.WorkspaceID)
	assert.Zero(t, got.Attempts)
	assert.Nil(t, got.Suggestions)
	_, err = db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2))
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Claimed and ready, a read gets it as it is.
	claimed, err := db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, claimed.VersionID, "w1", suggestionsReady))
	again, err := queueSuggestions(db, ws, ontology, 1)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsReady, again.Status)
	assert.JSONEq(t, string(suggestionsReady.Suggestions), string(again.Suggestions))

	// A failure stands for retryAfter, then a read queues it again, its spend kept.
	_, err = queueSuggestions(db, ws, ontology, 2)
	require.NoError(t, err)
	claimed, err = db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, suggestionVersion(2), claimed.VersionID)
	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, claimed.VersionID, "w1",
		model.FoundryAutomationSuggestionsEnd{Failure: "invalid answer", USDMicros: 300}))
	failed, err := queueSuggestions(db, ws, ontology, 2)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsFailed, failed.Status)
	assert.Equal(t, "invalid answer", *failed.Failure)
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET updated_at = NOW(6) - INTERVAL 61 MINUTE,
requested_at = NOW(6) - INTERVAL 62 MINUTE WHERE version_id=?`, suggestionVersion(2))
	require.NoError(t, err)
	requeued, err := queueSuggestions(db, ws, ontology, 2)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsQueued, requeued.Status)
	assert.Nil(t, requeued.Failure)
	assert.Zero(t, requeued.Attempts)
	assert.EqualValues(t, 300, requeued.USDMicros, "what it spent stays charged")
	assert.True(t, requeued.RequestedAt.After(failed.RequestedAt))

	for name, call := range map[string]func() error{
		"no workspace": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, "", ontology, suggestionVersion(3), 20, time.Hour)
			return err
		},
		"no limit": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), 0, time.Hour)
			return err
		},
		"no retry": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), 20, 0)
			return err
		},
	} {
		assert.Error(t, call(), name)
	}
	_, err = db.QueueFoundryAutomationSuggestions(ctx, id26("SW-none"), ontology, suggestionVersion(3), 20, time.Hour)
	assert.ErrorIs(t, err, store.ErrNotFound, "a workspace that isn't there")
}

// A workspace queues perHour writes an hour; rows queued before the hour,
// and other workspaces', don't count.
func TestFoundryAutomationSuggestionsRateLimit(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other, ontology := id26("SW-rate"), id26("SW-rate-other"), id26("SO-rate")
	createWorkspace(t, ws)
	createWorkspace(t, other)
	for n := range 3 {
		_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(n), 3, time.Hour)
		require.NoError(t, err)
	}
	_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), 3, time.Hour)
	assert.ErrorIs(t, err, store.ErrLimitExceeded)
	_, err = db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(0), 3, time.Hour)
	require.NoError(t, err, "a row already queued is read, not queued again")
	_, err = db.QueueFoundryAutomationSuggestions(ctx, other, id26("SO-rate-other"), suggestionVersion(0), 3, time.Hour)
	require.NoError(t, err, "another workspace's writes are its own")

	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET requested_at = NOW(6) - INTERVAL 61 MINUTE WHERE version_id=? AND workspace_id=?`,
		suggestionVersion(0), ws)
	require.NoError(t, err)
	_, err = db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), 3, time.Hour)
	require.NoError(t, err, "an hour on, a write leaves the count")
}

// A claim takes the oldest due row, once; a lease that ran out makes it due
// again, counting the attempt; only the lease holder finishes it.
func TestFoundryAutomationSuggestionsClaimAndFinish(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, ontology := id26("SW-claim"), id26("SO-claim")
	createWorkspace(t, ws)

	none, err := db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	assert.Nil(t, none, "nothing queued")
	_, err = db.ClaimFoundryAutomationSuggestions(ctx, "", time.Minute)
	assert.Error(t, err)

	for n := 1; n <= 2; n++ {
		_, err := queueSuggestions(db, ws, ontology, n)
		require.NoError(t, err)
	}
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET requested_at = NOW(6) - INTERVAL 1 MINUTE WHERE version_id=?`, suggestionVersion(2))
	require.NoError(t, err)

	first, err := db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, suggestionVersion(2), first.VersionID, "the oldest first")
	assert.Equal(t, model.FoundryAutomationSuggestionsRunning, first.Status)
	assert.Equal(t, 1, first.Attempts)
	assert.Equal(t, "w1", *first.LeaseOwner)
	second, err := db.ClaimFoundryAutomationSuggestions(ctx, "w2", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, suggestionVersion(1), second.VersionID)
	third, err := db.ClaimFoundryAutomationSuggestions(ctx, "w3", time.Minute)
	require.NoError(t, err)
	assert.Nil(t, third, "a leased row isn't due")

	// w1's lease runs out: w3 takes it, and w1 can't finish it.
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET lease_until = NOW(6) - INTERVAL 1 SECOND WHERE version_id=?`, suggestionVersion(2))
	require.NoError(t, err)
	retaken, err := db.ClaimFoundryAutomationSuggestions(ctx, "w3", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retaken)
	assert.Equal(t, suggestionVersion(2), retaken.VersionID)
	assert.Equal(t, 2, retaken.Attempts, "a lost lease counts its attempt")
	assert.ErrorIs(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2), "w1", suggestionsReady), store.ErrNotFound)
	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2), "w3", suggestionsReady))
	ready, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsReady, ready.Status)
	assert.EqualValues(t, 1200, ready.USDMicros)
	assert.NotNil(t, ready.ChargedAt)
	assert.Nil(t, ready.LeaseOwner)
	assert.ErrorIs(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2), "w3", suggestionsReady), store.ErrNotFound,
		"ended once")

	for name, end := range map[string]model.FoundryAutomationSuggestionsEnd{
		"neither":  {},
		"both":     {Suggestions: suggestionsReady.Suggestions, Failure: "x"},
		"negative": {Failure: "x", USDMicros: -1},
		"long":     {Failure: string(make([]rune, 256))},
	} {
		assert.Error(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(1), "w2", end), name)
	}
	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(1), "w2",
		model.FoundryAutomationSuggestionsEnd{Failure: "the Assistant's monthly budget is spent"}))
	failed, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(1))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsFailed, failed.Status)
	assert.Nil(t, failed.Suggestions)
}

// What suggestions spent counts in the Assistant's people's pool this month:
// SumFoundryAssistantSpend, and the budget a person's turn is sent and
// claimed under; never automations' pool.
func TestFoundryAutomationSuggestionsSpendTheAssistantsBudget(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "suggest")
	ontology := id26("SO-spend")
	since := f.limits().Since

	spent, err := f.db.SumFoundryAssistantSpend(ctx, f.workspaceID, since)
	require.NoError(t, err)
	assert.Zero(t, spent)

	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	f.settle(t, turn) // 1,000
	_, err = queueSuggestions(f.db, f.workspaceID, ontology, 1)
	require.NoError(t, err)
	claimed, err := f.db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.db.FinishFoundryAutomationSuggestions(ctx, ontology, claimed.VersionID, "w1",
		model.FoundryAutomationSuggestionsEnd{Failure: "invalid answer", USDMicros: 600_000}))
	spent, err = f.db.SumFoundryAssistantSpend(ctx, f.workspaceID, since)
	require.NoError(t, err)
	assert.EqualValues(t, 601_000, spent)
	later, err := f.db.SumFoundryAssistantSpend(ctx, f.workspaceID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Zero(t, later, "only what was charged since")

	// $0.60 of suggestions and a $0.50 cap pass the $1 month: refused.
	assert.Equal(t, model.FoundryAgentBudget, refusal(t, f.send(2, 0)))

	// A turn already queued is failed at its claim, not run.
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET usd_micros = 0`)
	require.NoError(t, err)
	require.NoError(t, f.send(3, 0))
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET usd_micros = 600000`)
	require.NoError(t, err)
	none, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", f.claim())
	require.NoError(t, err)
	assert.Nil(t, none)
	failed, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(3))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnFailed, failed.Status)
	assert.Contains(t, *failed.Error, "spent this month's")
}

// Suggestions go with their ontology, and with the workspace's work.
func TestFoundryAutomationSuggestionsGoWithTheirOntologyAndWorkspace(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other := id26("SW-gone"), id26("SW-gone-other")
	o, kept := sampleOntology(t, db, ws, "SG"), sampleOntology(t, db, ws, "SG-kept")
	elsewhere := sampleOntology(t, db, other, "SG-else")
	for _, row := range []struct{ ws, ontology string }{{ws, o.ID}, {ws, kept.ID}, {other, elsewhere.ID}} {
		_, err := queueSuggestions(db, row.ws, row.ontology, 1)
		require.NoError(t, err)
	}

	require.NoError(t, db.DeleteFoundryOntology(ctx, ws, o.ID, id26("SG-del")))
	_, err := db.GetFoundryAutomationSuggestions(ctx, o.ID, suggestionVersion(1))
	assert.ErrorIs(t, err, store.ErrNotFound, "deleting an ontology deletes its suggestions")
	_, err = db.GetFoundryAutomationSuggestions(ctx, kept.ID, suggestionVersion(1))
	require.NoError(t, err, "and no other")

	require.NoError(t, db.WithTx(ctx, func(tx store.Store) error { return tx.CancelWorkspaceFoundryWork(ctx, ws) }))
	_, err = db.GetFoundryAutomationSuggestions(ctx, kept.ID, suggestionVersion(1))
	assert.ErrorIs(t, err, store.ErrNotFound, "deleting the workspace deletes them")
	_, err = db.GetFoundryAutomationSuggestions(ctx, elsewhere.ID, suggestionVersion(1))
	require.NoError(t, err, "another workspace's stay")
}
