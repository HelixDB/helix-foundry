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
	Suggestions: json.RawMessage(`[{"kind":"workflow","title":"Renewal radar","prompt":"Every Monday, list renewals.","topic":"Renewals","icon":"watch"}]`),
}

// suggestionQuestions are the competency questions a row is queued with.
var suggestionQuestions = []string{"Which contracts renew this quarter?", "How many invoices are overdue?"}

func suggestionVersion(n int) string { return id26(fmt.Sprintf("SV-%03d-", n)) }

// queueSuggestions queues ontology's version n's suggestions, 20 an hour,
// retried an hour after a failure.
func queueSuggestions(db store.Store, workspaceID, ontologyID string, n int) (*model.FoundryAutomationSuggestions, error) {
	return db.QueueFoundryAutomationSuggestions(context.Background(), workspaceID, ontologyID, suggestionVersion(n), suggestionQuestions, 20, time.Hour)
}

// ledgerEntries counts the workspace's ledger entries of kind.
func ledgerEntries(t *testing.T, workspaceID, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_automation_suggestion_ledger WHERE workspace_id=? AND kind=?`,
		workspaceID, kind).Scan(&n))
	return n
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
	assert.Equal(t, 1, ledgerEntries(t, ws, "queued"), "queued once")

	got, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(1))
	require.NoError(t, err)
	assert.Equal(t, ws, got.WorkspaceID)
	assert.Zero(t, got.Attempts)
	assert.Nil(t, got.Suggestions)
	assert.Equal(t, suggestionQuestions, got.Questions, "with the version's questions")
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

	// A failure stands for retryAfter, then a read queues it again.
	_, err = queueSuggestions(db, ws, ontology, 2)
	require.NoError(t, err)
	claimed, err = db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, suggestionVersion(2), claimed.VersionID)
	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, claimed.VersionID, "w1",
		model.FoundryAutomationSuggestionsEnd{Failure: "invalid answer"}))
	failed, err := queueSuggestions(db, ws, ontology, 2)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsFailed, failed.Status)
	assert.Equal(t, "invalid answer", *failed.Failure)
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET updated_at = NOW(6) - INTERVAL 61 MINUTE,
requested_at = NOW(6) - INTERVAL 62 MINUTE WHERE version_id=?`, suggestionVersion(2))
	require.NoError(t, err)
	requeued, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(2), nil, 20, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsQueued, requeued.Status)
	assert.Nil(t, requeued.Failure)
	assert.Zero(t, requeued.Attempts)
	assert.Equal(t, []string{}, requeued.Questions, "no questions is an empty list")
	assert.True(t, requeued.RequestedAt.After(failed.RequestedAt))
	assert.Equal(t, 3, ledgerEntries(t, ws, "queued"))

	for name, call := range map[string]func() error{
		"no workspace": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, "", ontology, suggestionVersion(3), nil, 20, time.Hour)
			return err
		},
		"no limit": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), nil, 0, time.Hour)
			return err
		},
		"no retry": func() error {
			_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(3), nil, 20, 0)
			return err
		},
	} {
		assert.Error(t, call(), name)
	}
	_, err = db.QueueFoundryAutomationSuggestions(ctx, id26("SW-none"), ontology, suggestionVersion(3), nil, 20, time.Hour)
	assert.ErrorIs(t, err, store.ErrNotFound, "a workspace that isn't there")
}

// A workspace queues perHour writes an hour; queues before the hour, and
// other workspaces', don't count, and deleting an ontology doesn't reset
// the count.
func TestFoundryAutomationSuggestionsRateLimit(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other := id26("SW-rate"), id26("SW-rate-other")
	o := sampleOntology(t, db, ws, "SR")
	elsewhere := sampleOntology(t, db, other, "SR-else")
	queue := func(ws, ontology string, n int) error {
		_, err := db.QueueFoundryAutomationSuggestions(ctx, ws, ontology, suggestionVersion(n), nil, 3, time.Hour)
		return err
	}
	for n := range 3 {
		require.NoError(t, queue(ws, o.ID, n))
	}
	assert.ErrorIs(t, queue(ws, o.ID, 3), store.ErrLimitExceeded)
	require.NoError(t, queue(ws, o.ID, 0), "a row already queued is read, not queued again")
	require.NoError(t, queue(other, elsewhere.ID, 0), "another workspace's writes are its own")

	require.NoError(t, db.DeleteFoundryOntology(ctx, ws, o.ID, id26("SR-del")))
	kept := sampleOntology(t, db, ws, "SR-kept")
	assert.ErrorIs(t, queue(ws, kept.ID, 3), store.ErrLimitExceeded, "deleting the ontology leaves its queues counted")

	_, err := testDB.Exec(`UPDATE foundry_automation_suggestion_ledger SET at = NOW(6) - INTERVAL 61 MINUTE WHERE workspace_id=? ORDER BY at LIMIT 1`, ws)
	require.NoError(t, err)
	require.NoError(t, queue(ws, kept.ID, 3), "an hour on, a queue leaves the count")
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

	// Only the holder renews the lease, for a write that takes longer.
	assert.ErrorIs(t, db.RenewFoundryAutomationSuggestionsLease(ctx, ontology, suggestionVersion(2), "w1", time.Hour), store.ErrNotFound)
	assert.Error(t, db.RenewFoundryAutomationSuggestionsLease(ctx, ontology, suggestionVersion(2), "w3", 0))
	require.NoError(t, db.RenewFoundryAutomationSuggestionsLease(ctx, ontology, suggestionVersion(2), "w3", time.Hour))
	renewed, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *renewed.LeaseUntil, time.Minute)

	require.NoError(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2), "w3", suggestionsReady))
	ready, err := db.GetFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAutomationSuggestionsReady, ready.Status)
	assert.Nil(t, ready.LeaseOwner)
	assert.Nil(t, ready.LeaseUntil)
	assert.ErrorIs(t, db.RenewFoundryAutomationSuggestionsLease(ctx, ontology, suggestionVersion(2), "w3", time.Hour), store.ErrNotFound,
		"an ended write has no lease")
	assert.ErrorIs(t, db.FinishFoundryAutomationSuggestions(ctx, ontology, suggestionVersion(2), "w3", suggestionsReady), store.ErrNotFound,
		"ended once")

	for name, end := range map[string]model.FoundryAutomationSuggestionsEnd{
		"neither": {},
		"both":    {Suggestions: suggestionsReady.Suggestions, Failure: "x"},
		"long":    {Failure: string(make([]rune, 256))},
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

// What suggestions spent counts in the Assistant's people's pool in the
// month it was charged: SumFoundryAssistantSpend, and the budget a person's
// turn is sent and claimed under; never automations' pool. Charges are
// kept apart from the rows: queueing a failed write again doesn't charge
// it twice, and deleting its ontology doesn't refund it.
func TestFoundryAutomationSuggestionsSpendTheAssistantsBudget(t *testing.T) {
	ctx := context.Background()
	f := newAgentFixture(t, "suggest")
	o := sampleOntology(t, f.db, f.workspaceID, "SP")
	since := f.limits().Since
	sum := func() int64 {
		t.Helper()
		spent, err := f.db.SumFoundryAssistantSpend(ctx, f.workspaceID, since)
		require.NoError(t, err)
		return spent
	}
	assert.Zero(t, sum())

	require.NoError(t, f.send(1, 0))
	turn := f.claimed(t)
	f.settle(t, turn) // 1,000
	_, err := queueSuggestions(f.db, f.workspaceID, o.ID, 1)
	require.NoError(t, err)
	claimed, err := f.db.ClaimFoundryAutomationSuggestions(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.db.ChargeFoundryAutomationSuggestions(ctx, f.workspaceID, 600_000))
	require.NoError(t, f.db.FinishFoundryAutomationSuggestions(ctx, o.ID, claimed.VersionID, "w1",
		model.FoundryAutomationSuggestionsEnd{Failure: "invalid answer"}))
	assert.EqualValues(t, 601_000, sum())
	later, err := f.db.SumFoundryAssistantSpend(ctx, f.workspaceID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Zero(t, later, "only what was charged since")

	// Nothing is no charge; less than nothing, or no workspace, is refused.
	require.NoError(t, f.db.ChargeFoundryAutomationSuggestions(ctx, f.workspaceID, 0))
	assert.Equal(t, 1, ledgerEntries(t, f.workspaceID, "charged"))
	assert.Error(t, f.db.ChargeFoundryAutomationSuggestions(ctx, f.workspaceID, -1))
	assert.Error(t, f.db.ChargeFoundryAutomationSuggestions(ctx, "", 1))

	// Queued again an hour on, the failed write's spend isn't charged again.
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestions SET updated_at = NOW(6) - INTERVAL 61 MINUTE WHERE ontology_id=?`, o.ID)
	require.NoError(t, err)
	requeued, err := queueSuggestions(f.db, f.workspaceID, o.ID, 1)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationSuggestionsQueued, requeued.Status)
	assert.EqualValues(t, 601_000, sum())

	// $0.60 of suggestions and a $0.50 cap pass the $1 month: refused.
	assert.Equal(t, model.FoundryAgentBudget, refusal(t, f.send(2, 0)))

	// A charge of last month counts in last month only.
	_, err = testDB.Exec(`UPDATE foundry_automation_suggestion_ledger SET at = ? WHERE workspace_id=? AND kind='charged'`,
		since.Add(-time.Hour), f.workspaceID)
	require.NoError(t, err)
	assert.EqualValues(t, 1_000, sum())

	// A turn already queued is failed at its claim, not run.
	require.NoError(t, f.send(3, 0))
	require.NoError(t, f.db.ChargeFoundryAutomationSuggestions(ctx, f.workspaceID, 600_000))
	none, err := f.db.ClaimFoundryAgentTurn(ctx, "w1", f.claim())
	require.NoError(t, err)
	assert.Nil(t, none)
	failed, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, f.turnID(3))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryAgentTurnFailed, failed.Status)
	assert.Contains(t, *failed.Error, "spent this month's")

	// Deleting the ontology refunds nothing.
	require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.workspaceID, o.ID, id26("SP-del")))
	assert.EqualValues(t, 601_000, sum())
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

// A version's sources are the generation that drafted it and its scope,
// read without its definition; a legacy version's scope is its ontology's
// live sets, as GetOntologyVersionScope reads it.
func TestFoundryOntologyVersionSources(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "VSRC")
	general := generalSetID(f.ontology.ID)
	generation := id26("FG-VSRC")
	v := &model.OntologyVersion{
		ID: id26("FV-VSRC"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{"huge":true}`), GenerationID: &generation, DocumentSetIDs: &[]string{general},
	}
	require.NoError(t, f.db.CreateOntologyVersion(ctx, v))
	got, err := f.db.GetOntologyVersionSources(ctx, f.ontology.ID, v.ID)
	require.NoError(t, err)
	assert.Equal(t, &model.OntologyVersionSources{GenerationID: &generation, DocumentSetIDs: []string{general}}, got)

	manual := f.version(t, "VSRC-manual")
	got, err = f.db.GetOntologyVersionSources(ctx, f.ontology.ID, manual)
	require.NoError(t, err)
	assert.Nil(t, got.GenerationID, "a version no generation drafted")
	assert.Equal(t, []string{general}, got.DocumentSetIDs)

	_, err = testDB.Exec(`UPDATE foundry_ontology_versions SET document_set_ids=NULL WHERE id=?`, manual)
	require.NoError(t, err)
	got, err = f.db.GetOntologyVersionSources(ctx, f.ontology.ID, manual)
	require.NoError(t, err)
	assert.Equal(t, []string{}, got.DocumentSetIDs, "legacy: the ontology's live sets, none here")
	scope, err := f.db.GetOntologyVersionScope(ctx, f.ontology.ID, manual)
	require.NoError(t, err)
	assert.Equal(t, scope, got.DocumentSetIDs)

	_, err = f.db.GetOntologyVersionSources(ctx, f.ontology.ID, id26("FV-VSRC-none"))
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.db.GetOntologyVersionSources(ctx, id26("FO-VSRC-other"), v.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "another ontology's version")
}
