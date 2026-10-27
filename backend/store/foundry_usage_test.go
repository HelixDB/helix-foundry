package store_test

import (
	"context"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three sums a workspace's month of AI spend reads (the backend's
// GetFoundryUsage), as the caps read them: loads by the jobs created since
// the month's first instant (SumFoundryGraphJobSpend, the graph budget's),
// API sources' extraction no person started by the UTC month of the spend
// table (SumFoundryDocumentSetSpend, the allowance's workspace side), and
// questions by the claims charged since it (SumFoundryQuestionSpend, the
// questions budget's: settled spend apart from what running ones reserve).
func TestFoundryMonthUsageSums(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonth := start.Add(-time.Microsecond)
	this, last := months()

	f := newFoundryFixture(t, "USE")
	ws := f.ontology.WorkspaceID
	other := sampleOntology(t, f.db, id26("FWS-USE-2"), "USE-2")
	empty := id26("FWS-USE-EMPTY")
	createWorkspace(t, empty)

	// API sources (connectors of kind events), one deleted, beside a
	// Granola connection, a person's set and another workspace's source.
	// The delete runs first: it owes the ontology a refresh, which reads its
	// live job's load, and the jobs written below have no load rows.
	a, b := f.connectorSet(t, "USE-a", "Shop orders"), f.connectorSet(t, "USE-b", "Support tickets")
	gone, notes := f.connectorSet(t, "USE-gone", "Old source"), f.connectorSet(t, "USE-notes", "Team Granola")
	elsewhere := foundryFixture{db: f.db, ontology: other}.connectorSet(t, "USE-x", "Elsewhere")
	for _, s := range []*model.FoundryDocumentSet{a, b, gone, elsewhere} {
		_, err := testDB.Exec(`UPDATE connectors SET kind=? WHERE id=?`, model.ConnectorKindEvents, s.Owner.ConnectorID)
		require.NoError(t, err)
	}
	require.NoError(t, f.db.DeleteConnector(ctx, ws, gone.Owner.ConnectorID))
	insertSpend(t, a.ID, this, ws, 1_000_000, false)
	insertSpend(t, a.ID, last, ws, 3_000_000, true)
	insertSpend(t, b.ID, this, ws, 2_000_000, false)
	insertSpend(t, gone.ID, this, ws, 1_500_000, false)
	insertSpend(t, notes.ID, this, ws, 7_000_000, false)
	insertSpend(t, generalSetID(f.ontology.ID), this, ws, 500_000, false)
	insertSpend(t, elsewhere.ID, this, other.WorkspaceID, 9_000_000, false)

	// Graph jobs: each spent what its documents' extraction cost.
	job := func(tag string, o *model.FoundryOntology, kind, status string, usd int64, created time.Time) {
		t.Helper()
		var active any
		if status == "running" {
			active = 1
		}
		phase, load := "documents", id26("FL-"+tag)
		if kind == "delete" {
			phase = "deleting"
		}
		if kind == "load" {
			load = id26("FJ-" + tag)
		}
		_, err := testDB.Exec(`INSERT INTO foundry_load_jobs
  (id, workspace_id, ontology_id, version_id, load_id, kind, status, phase, active_key, usd_micros, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id26("FJ-"+tag), o.WorkspaceID, o.ID, id26("FV-"+tag), load, kind, status, phase, active, usd, created)
		require.NoError(t, err)
	}
	job("first", f.ontology, "load", "succeeded", 2_000_000, start)
	job("live", f.ontology, "extract", "running", 750_000, now)
	job("delete", f.ontology, "delete", "succeeded", 0, now)
	job("last", f.ontology, "load", "succeeded", 5_000_000, lastMonth)
	job("other", other, "extract", "succeeded", 9_000_000, now)

	// Questions, each charged to the month its latest claim fell in.
	question := func(tag, workspace, status string, usd, reserved int64, charged *time.Time, created time.Time) {
		t.Helper()
		var finished any
		if status != "queued" && status != "running" {
			finished = created
		}
		_, err := testDB.Exec(`INSERT INTO foundry_questions
  (id, workspace_id, ontology_id, load_id, version_id, thread_id, question, status, max_usd_micros, reserved_micros, usd_micros,
   charged_at, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, 'who?', ?, 500000, ?, ?, ?, ?, ?)`,
			id26("FQ-"+tag), workspace, f.ontology.ID, id26("FL-q"), id26("FV-q"), id26("FQ-"+tag), status, reserved, usd,
			charged, created, finished)
		require.NoError(t, err)
	}
	question("settled", ws, "answered", 200_000, 0, &now, now)
	// A retry carries its first attempt's spend, and reserves its cap while it runs.
	question("running", ws, "running", 100_000, 500_000, &now, now)
	// A cancel charges the reservation it held as spent.
	question("cancelled", ws, "cancelled", 500_000, 0, &now, now)
	// Asked last month, charged to this one, which ran it.
	question("carried", ws, "answered", 300_000, 0, &now, lastMonth)
	question("last", ws, "answered", 900_000, 0, &lastMonth, lastMonth)
	question("queued", ws, "queued", 0, 0, nil, now)
	question("other", other.WorkspaceID, "answered", 700_000, 0, &now, now)

	t.Run("sums this month of the workspace's loads", func(t *testing.T) {
		loads, err := f.db.SumFoundryGraphJobSpend(ctx, ws, start)
		require.NoError(t, err)
		assert.Equal(t, int64(2_750_000), loads, "from the month's first instant, a live job's spend so far included")
	})

	t.Run("sums this month of the workspace's API sources", func(t *testing.T) {
		sources, err := f.db.SumFoundryDocumentSetSpend(ctx, ws, model.ConnectorKindEvents, start)
		require.NoError(t, err)
		assert.Equal(t, int64(4_500_000), sources, "a deleted source's included; Granola's, a person's set's and another workspace's not")
		granola, err := f.db.SumFoundryDocumentSetSpend(ctx, ws, "granola", now)
		require.NoError(t, err)
		assert.Equal(t, int64(7_000_000), granola, "any time in the month reads it")
		before, err := f.db.SumFoundryDocumentSetSpend(ctx, ws, model.ConnectorKindEvents, lastMonth)
		require.NoError(t, err)
		assert.Equal(t, int64(3_000_000), before, "last month is a month of its own")
	})

	t.Run("sums this month of the workspace's questions, settled apart from reserved", func(t *testing.T) {
		questions, err := f.db.SumFoundryQuestionSpend(ctx, ws, start)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 1_100_000, ReservedMicros: 500_000}, questions)
	})

	t.Run("reads a workspace with no spend as zero", func(t *testing.T) {
		loads, err := f.db.SumFoundryGraphJobSpend(ctx, empty, start)
		require.NoError(t, err)
		sources, err := f.db.SumFoundryDocumentSetSpend(ctx, empty, model.ConnectorKindEvents, start)
		require.NoError(t, err)
		questions, err := f.db.SumFoundryQuestionSpend(ctx, empty, start)
		require.NoError(t, err)
		assert.Zero(t, loads)
		assert.Zero(t, sources)
		assert.Zero(t, questions)
	})

	t.Run("reads another workspace's month apart", func(t *testing.T) {
		loads, err := f.db.SumFoundryGraphJobSpend(ctx, other.WorkspaceID, start)
		require.NoError(t, err)
		sources, err := f.db.SumFoundryDocumentSetSpend(ctx, other.WorkspaceID, model.ConnectorKindEvents, start)
		require.NoError(t, err)
		questions, err := f.db.SumFoundryQuestionSpend(ctx, other.WorkspaceID, start)
		require.NoError(t, err)
		assert.Equal(t, int64(9_000_000), loads)
		assert.Equal(t, int64(9_000_000), sources)
		assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 700_000}, questions)
	})
}
