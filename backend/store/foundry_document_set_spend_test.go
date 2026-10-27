package store_test

import (
	"context"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spendRows lists foundry_document_set_spend as "<set> <month> <workspace>
// <µ$> <skipped>", by set and month.
func spendRows(t *testing.T) []string {
	t.Helper()
	rows, err := testDB.Query(`SELECT set_id, month, workspace_id, usd_micros, skipped FROM foundry_document_set_spend ORDER BY set_id, month`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var set, ws string
		var month time.Time
		var usd int64
		var skipped bool
		require.NoError(t, rows.Scan(&set, &month, &ws, &usd, &skipped))
		out = append(out, fmt.Sprintf("%s %s %s %d %t", set, month.Format(time.DateOnly), ws, usd, skipped))
	}
	require.NoError(t, rows.Err())
	return out
}

// months are the first days of this UTC month and the one before, as stored.
func months() (this, last string) {
	now := time.Now().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.Format(time.DateOnly), first.AddDate(0, -1, 0).Format(time.DateOnly)
}

// insertSpend writes a set's month as a recording or a skipped sync would.
func insertSpend(t *testing.T, setID, month, workspaceID string, usd int64, skipped bool) {
	t.Helper()
	_, err := testDB.Exec(`INSERT INTO foundry_document_set_spend (set_id, month, workspace_id, usd_micros, skipped) VALUES (?, ?, ?, ?, ?)`,
		setID, month, workspaceID, usd, skipped)
	require.NoError(t, err)
}

func TestRecordFoundryDocumentSetSpend(t *testing.T) {
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	person := id26("U-RSP")
	this, last := months()
	n1, n2 := id26("FD-n1"), id26("FD-n2")
	// claimed is the ontology's due job, leased to w1, and a recorder of
	// its documents at a price.
	claimed := func(t *testing.T, db store.Store) (*model.FoundryGraphJob, func(doc string, usd int64, keepExisting bool) error) {
		t.Helper()
		ctx := context.Background()
		job, err := db.ClaimFoundryGraphJob(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, job)
		return job, func(doc string, usd int64, keepExisting bool) error {
			e := extracted(doc)
			e.USDMicros = usd
			return db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", e, keepExisting)
		}
	}
	jobSpend := func(t *testing.T, f foundryFixture, jobID string) int64 {
		t.Helper()
		job, err := f.db.GetFoundryGraphJob(context.Background(), f.ontology.ID, jobID)
		require.NoError(t, err)
		return job.Spend.USDMicros
	}

	// A job no one requested adds what each document cost to its set's
	// month, as it adds it to the job: a replay at $0 adds nothing, and a
	// document the load already held still spent what it spent.
	t.Run("adds work no one requested to its set's month as to the job", func(t *testing.T) {
		c := newConnectorSync(t, newFoundryFixture(t, "RSP1"), "RSP1")
		ws := c.ontology.WorkspaceID
		c.published(t, c.publish("RSP1", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day)}, nil))
		c.loadReading(t, "L1", c.setID)
		job, record := claimed(t, c.db)
		require.Nil(t, job.RequestedByUserID)
		require.NoError(t, record(n1, 250_000, false))
		require.NoError(t, record(n2, 0, false))
		assert.Equal(t, []string{fmt.Sprintf("%s %s %s 250000 false", c.setID, this, ws)}, spendRows(t), "not the replay at $0")
		require.NoError(t, record(n1, 100_000, true))
		assert.Equal(t, []string{fmt.Sprintf("%s %s %s 350000 false", c.setID, this, ws)}, spendRows(t), "a kept row's spend counts")
		assert.Equal(t, int64(350_000), jobSpend(t, c.foundryFixture, job.ID))
	})

	// A person's job spends from the month's budget alone.
	t.Run("adds nothing for a person's job", func(t *testing.T) {
		c := newConnectorSync(t, newFoundryFixture(t, "RSP2"), "RSP2")
		c.published(t, c.publish("RSP2", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		c.versionReading(t, "L1", c.setID)
		load := loadJob(c.ontology, "L1")
		load.RequestedByUserID = &person
		require.NoError(t, c.db.CreateFoundryLoad(context.Background(), load))
		job, record := claimed(t, c.db)
		require.Equal(t, &person, job.RequestedByUserID)
		require.NoError(t, record(n1, 250_000, false))
		assert.Empty(t, spendRows(t))
		assert.Equal(t, int64(250_000), jobSpend(t, c.foundryFixture, job.ID))
	})

	// The spend is the set's the document joined the load in: a move after
	// that applies from the next load.
	t.Run("keys by the set the document joined the load with", func(t *testing.T) {
		ctx := context.Background()
		f := newFoundryFixture(t, "RSP3")
		ws, general := f.ontology.WorkspaceID, generalSetID(f.ontology.ID)
		other := f.documentSet("RSP3-other")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, other, 10))
		doc := f.uploaded(t, "RSP3-doc")
		f.loadReading(t, "L1", general, other.ID)
		_, _, err := f.db.MoveFoundryDocument(ctx, ws, doc, other.ID, f.fan("RSP3-move"))
		require.NoError(t, err)
		_, record := claimed(t, f.db)
		require.NoError(t, record(doc, 250_000, false))
		assert.Equal(t, []string{fmt.Sprintf("%s %s %s 250000 false", general, this, ws)}, spendRows(t))
	})

	// Each UTC month has a row of its own: last month's spend and skip stay
	// as they were.
	t.Run("writes a new month's row", func(t *testing.T) {
		c := newConnectorSync(t, newFoundryFixture(t, "RSP4"), "RSP4")
		ws := c.ontology.WorkspaceID
		c.published(t, c.publish("RSP4", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		c.loadReading(t, "L1", c.setID)
		insertSpend(t, c.setID, last, ws, 4_900_000, true)
		_, record := claimed(t, c.db)
		require.NoError(t, record(n1, 250_000, false))
		assert.Equal(t, []string{
			fmt.Sprintf("%s %s %s 4900000 true", c.setID, last, ws),
			fmt.Sprintf("%s %s %s 250000 false", c.setID, this, ws),
		}, spendRows(t))
	})

	// An attempt that lost its lease records nothing, spend included.
	t.Run("writes nothing for a lost lease", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "RSP5"), "RSP5")
		c.published(t, c.publish("RSP5", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		c.loadReading(t, "L1", c.setID)
		job, _ := claimed(t, c.db)
		assert.ErrorIs(t, c.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w2", extracted(n1), false), store.ErrNotFound)
		assert.Empty(t, spendRows(t))
	})
}

func TestFoundryDocumentSetSpend(t *testing.T) {
	this, last := months()
	// fixture is a workspace with two API sources and one deleted, a
	// Granola connection, its General set, and an API source of another
	// workspace, each having spent this month; one source also spent last
	// month and skipped pieces then.
	type fixture struct {
		foundryFixture
		a, b, gone, notes, elsewhere *model.FoundryDocumentSet
	}
	newFixture := func(t *testing.T, tag string) fixture {
		t.Helper()
		f := fixture{foundryFixture: newFoundryFixture(t, tag)}
		ws := f.ontology.WorkspaceID
		f.a, f.b = f.connectorSet(t, tag+"-a", "Shop orders"), f.connectorSet(t, tag+"-b", "Support tickets")
		f.gone, f.notes = f.connectorSet(t, tag+"-gone", "Old source"), f.connectorSet(t, tag+"-notes", "Team Granola")
		other := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, id26("FWS-"+tag+"-2"), tag+"-2")}
		f.elsewhere = other.connectorSet(t, tag+"-x", "Elsewhere")
		for _, s := range []*model.FoundryDocumentSet{f.a, f.b, f.gone, f.elsewhere} {
			_, err := testDB.Exec(`UPDATE connectors SET kind=? WHERE id=?`, model.ConnectorKindEvents, s.Owner.ConnectorID)
			require.NoError(t, err)
		}
		require.NoError(t, f.db.DeleteConnector(context.Background(), ws, f.gone.Owner.ConnectorID))
		insertSpend(t, f.a.ID, this, ws, 1_000_000, false)
		insertSpend(t, f.a.ID, last, ws, 3_000_000, true)
		insertSpend(t, f.b.ID, this, ws, 2_000_000, false)
		insertSpend(t, f.gone.ID, this, ws, 1_500_000, false)
		insertSpend(t, f.notes.ID, this, ws, 7_000_000, false)
		insertSpend(t, generalSetID(f.ontology.ID), this, ws, 500_000, false)
		insertSpend(t, f.elsewhere.ID, this, other.ontology.WorkspaceID, 9_000_000, false)
		return f
	}

	// Each asked set a connector owns, of the workspace, with its month,
	// the month of all the workspace's sets of its kind (a deleted
	// source's too, so deleting one frees nothing) and whether any month
	// skipped; a person's set, another workspace's and an unknown id are
	// left out.
	t.Run("reads the asked sets of the workspace", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t, "SSR1")
		ws := f.ontology.WorkspaceID
		spend, err := f.db.FoundryDocumentSetSpend(ctx, ws,
			[]string{f.a.ID, f.b.ID, f.gone.ID, f.notes.ID, generalSetID(f.ontology.ID), f.elsewhere.ID, id26("FS-none")}, time.Now())
		require.NoError(t, err)
		assert.Equal(t, map[string]model.FoundryDocumentSetSpend{
			f.a.ID: {SetID: f.a.ID, ConnectorKind: model.ConnectorKindEvents, ConnectorName: "Shop orders", USDMicros: 1_000_000,
				WorkspaceUSDMicros: 4_500_000, Skipped: true},
			f.b.ID: {SetID: f.b.ID, ConnectorKind: model.ConnectorKindEvents, ConnectorName: "Support tickets", USDMicros: 2_000_000,
				WorkspaceUSDMicros: 4_500_000},
			f.gone.ID: {SetID: f.gone.ID, ConnectorKind: model.ConnectorKindEvents, ConnectorName: "Old source", USDMicros: 1_500_000,
				WorkspaceUSDMicros: 4_500_000},
			f.notes.ID: {SetID: f.notes.ID, ConnectorKind: "granola", ConnectorName: "Team Granola", USDMicros: 7_000_000,
				WorkspaceUSDMicros: 7_000_000},
		}, spend)

		for name, ids := range map[string][]string{
			"no sets":               nil,
			"another workspace's":   {f.elsewhere.ID},
			"a person's set":        {generalSetID(f.ontology.ID)},
			"unknown":               {id26("FS-none")},
			"this one's, elsewhere": {f.a.ID},
		} {
			asker := ws
			if name == "this one's, elsewhere" {
				asker = f.elsewhere.WorkspaceID
			}
			spend, err := f.db.FoundryDocumentSetSpend(ctx, asker, ids, time.Now())
			require.NoError(t, err)
			assert.Empty(t, spend, name)
		}
	})

	// Any time in a month reads that UTC month: last month's spend, or a
	// month with none at all.
	t.Run("reads the UTC month the time is in", func(t *testing.T) {
		ctx := context.Background()
		f := newFixture(t, "SSR2")
		ws := f.ontology.WorkspaceID
		insertSpend(t, f.a.ID, "2030-10-01", ws, 111, false)
		insertSpend(t, f.a.ID, "2030-11-01", ws, 222, false)
		lastMonth, err := time.Parse(time.DateOnly, last)
		require.NoError(t, err)
		for _, tc := range []struct {
			name      string
			at        time.Time
			set, kind int64
		}{
			{"mid last month", lastMonth.Add(14*24*time.Hour + 13*time.Hour), 3_000_000, 3_000_000},
			{"a zone ahead of UTC, still October in UTC", time.Date(2030, 11, 1, 1, 0, 0, 0, time.FixedZone("", 5*3600)), 111, 111},
			{"a zone behind UTC, already November in UTC", time.Date(2030, 10, 31, 20, 0, 0, 0, time.FixedZone("", -5*3600)), 222, 222},
			{"a month with no spend", time.Date(2031, 1, 10, 0, 0, 0, 0, time.UTC), 0, 0},
		} {
			spend, err := f.db.FoundryDocumentSetSpend(ctx, ws, []string{f.a.ID, f.b.ID}, tc.at)
			require.NoError(t, err)
			assert.Equal(t, tc.set, spend[f.a.ID].USDMicros, tc.name)
			assert.Equal(t, tc.kind, spend[f.a.ID].WorkspaceUSDMicros, tc.name)
			assert.True(t, spend[f.a.ID].Skipped, "%s: a skip of any month", tc.name)
			assert.False(t, spend[f.b.ID].Skipped, tc.name)
		}
	})

	// A paused sync marks its set's month skipped, once however often it
	// says so; only a sync of the set's connector, holding its lease,
	// clears it, in every month, leaving the spend as it was.
	t.Run("marks a set skipped, and only its own sync clears it", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "SSK"), "SSK")
		ws := c.ontology.WorkspaceID
		now := time.Now()
		lastMonth, err := time.Parse(time.DateOnly, last)
		require.NoError(t, err)
		insertSpend(t, c.setID, this, ws, 1_200_000, false)
		for _, at := range []time.Time{now, now, lastMonth.Add(36 * time.Hour)} {
			require.NoError(t, c.db.MarkFoundryDocumentSetSkipped(ctx, ws, c.setID, at))
		}
		marked := []string{fmt.Sprintf("%s %s %s 0 true", c.setID, last, ws), fmt.Sprintf("%s %s %s 1200000 true", c.setID, this, ws)}
		assert.Equal(t, marked, spendRows(t))
		other := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, id26("FWS-SSK-2"), "SSK-2")}
		for name, mark := range map[string]struct{ ws, set string }{
			"another workspace's set": {other.ontology.WorkspaceID, c.setID},
			"a person's set":          {ws, generalSetID(c.ontology.ID)},
			"an unknown set":          {ws, id26("FS-none")},
		} {
			assert.ErrorIs(t, c.db.MarkFoundryDocumentSetSkipped(ctx, mark.ws, mark.set, now), store.ErrNotFound, name)
		}
		assert.Equal(t, marked, spendRows(t), "a refused mark writes nothing")

		// An API source of the workspace, whose sync runs beside the other's (another class of sync).
		source := c.connectorSet(t, "SSK-source", "Shop orders")
		require.NoError(t, c.db.EnqueueConnectorSync(ctx, &model.ConnectorSync{
			ID: id26("CS-SSK-source"), ConnectorID: source.Owner.ConnectorID, WorkspaceID: ws, Origin: model.ConnectorSyncEvents,
		}))
		sourceSync, err := c.db.ClaimConnectorSync(ctx, syncWorker, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, sourceSync)
		for name, clear := range map[string]struct{ ws, sync, worker string }{
			"a lost lease":              {ws, c.sync.ID, "sync-w2"},
			"another connector's sync":  {ws, sourceSync.ID, syncWorker},
			"another workspace's asker": {other.ontology.WorkspaceID, c.sync.ID, syncWorker},
		} {
			assert.ErrorIs(t, c.db.ClearFoundryDocumentSetSkipped(ctx, clear.ws, c.setID, clear.sync, clear.worker), store.ErrNotFound, name)
		}
		assert.Equal(t, marked, spendRows(t), "a refused clear writes nothing")
		spend, err := c.db.FoundryDocumentSetSpend(ctx, ws, []string{c.setID}, now)
		require.NoError(t, err)
		assert.True(t, spend[c.setID].Skipped)

		require.NoError(t, c.db.ClearFoundryDocumentSetSkipped(ctx, ws, c.setID, c.sync.ID, syncWorker))
		assert.Equal(t, []string{fmt.Sprintf("%s %s %s 0 false", c.setID, last, ws), fmt.Sprintf("%s %s %s 1200000 false", c.setID, this, ws)},
			spendRows(t))
		require.NoError(t, c.db.ClearFoundryDocumentSetSkipped(ctx, ws, c.setID, c.sync.ID, syncWorker), "clearing again changes nothing")
		spend, err = c.db.FoundryDocumentSetSpend(ctx, ws, []string{c.setID}, now)
		require.NoError(t, err)
		assert.False(t, spend[c.setID].Skipped)
	})
}
