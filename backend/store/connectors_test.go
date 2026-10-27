package store_test

import (
	"context"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"slices"
	"testing"
	"time"
)

func TestConnectorLifecycle(t *testing.T) {
	newConnector := func(workspaceID, suffix string) *model.Connector {
		return &model.Connector{
			ID: id26("CONN-" + suffix), WorkspaceID: workspaceID, Kind: "stripe", Name: "Billing",
			Config: json.RawMessage(`{}`), SecretCiphertext: []byte{0, 1, 2, 255}, SecretKeyID: "k1",
			SecretHint: "sk_test…4f2a",
		}
	}

	// Credentials and config round-trip byte for byte; the ciphertext is binary.
	t.Run("stores and reads a connector", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		workspaceID := id26("CONN-WS-A")
		c := newConnector(workspaceID, "A")
		c.Config = json.RawMessage(`{"region":"us"}`)
		require.NoError(t, db.CreateConnector(ctx, c))

		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, "stripe", stored.Kind)
		assert.JSONEq(t, `{"region":"us"}`, string(stored.Config))
		assert.Equal(t, c.SecretCiphertext, stored.SecretCiphertext)
		assert.Equal(t, "k1", stored.SecretKeyID)
		assert.Nil(t, stored.CreatedByUserID)
	})

	// Every read is scoped to the workspace that owns the connector.
	t.Run("hides a connector from another workspace", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		c := newConnector(id26("CONN-WS-B"), "B")
		require.NoError(t, db.CreateConnector(ctx, c))

		_, err := db.GetConnector(ctx, id26("CONN-WS-OTHER"), c.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.ErrorIs(t, db.DeleteConnector(ctx, id26("CONN-WS-OTHER"), c.ID), store.ErrNotFound)
	})

	// New credentials replace the old ones together with the config they were tested with.
	t.Run("replaces config and credentials together", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		workspaceID := id26("CONN-WS-C")
		c := newConnector(workspaceID, "C")
		require.NoError(t, db.CreateConnector(ctx, c))
		name := "Renamed"

		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{
			Name: &name, Config: json.RawMessage(`{"region":"eu"}`), SecretCiphertext: []byte{9}, SecretKeyID: "k2", SecretHint: "phx_…99",
		}))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, "Renamed", stored.Name)
		assert.JSONEq(t, `{"region":"eu"}`, string(stored.Config))
		assert.Equal(t, []byte{9}, stored.SecretCiphertext)
		assert.Equal(t, "k2", stored.SecretKeyID)
		assert.Equal(t, "phx_…99", stored.SecretHint)
	})

	// A rename alone keeps the stored credentials.
	t.Run("renames without touching credentials", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		workspaceID := id26("CONN-WS-D")
		c := newConnector(workspaceID, "D")
		require.NoError(t, db.CreateConnector(ctx, c))
		name := "Renamed"

		require.NoError(t, db.UpdateConnector(ctx, workspaceID, c.ID, model.ConnectorUpdate{Name: &name}))
		stored, err := db.GetConnector(ctx, workspaceID, c.ID)
		require.NoError(t, err)
		assert.Equal(t, c.SecretCiphertext, stored.SecretCiphertext)
		assert.Equal(t, "k1", stored.SecretKeyID)
	})

	// A deleted connector disappears from reads and cannot be deleted twice.
	t.Run("soft deletes", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		workspaceID := id26("CONN-WS-E")
		kept, gone := newConnector(workspaceID, "E1"), newConnector(workspaceID, "E2")
		require.NoError(t, db.CreateConnector(ctx, kept))
		require.NoError(t, db.CreateConnector(ctx, gone))

		require.NoError(t, db.DeleteConnector(ctx, workspaceID, gone.ID))
		listed, err := db.ListConnectors(ctx, workspaceID)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, kept.ID, listed[0].ID)
		_, err = db.GetConnector(ctx, workspaceID, gone.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, db.DeleteConnector(ctx, workspaceID, gone.ID), store.ErrNotFound)
	})
}

// connectorTable writes table name of connector connectorID with its
// snapshot at key, and the change batch CB-<key> of it, as a sync's publish
// does.
func connectorTable(t *testing.T, workspaceID, connectorID, name, key string) {
	t.Helper()
	_, err := testDB.Exec(`INSERT INTO connector_tables (id, connector_id, workspace_id, table_name, columns, primary_key, foreign_keys,
  s3_key, row_count, sync_id, synced_at, change_seq) VALUES (?, ?, ?, ?, '[]', '[]', '[]', ?, 1, ?, NOW(6), 1)`,
		id26("CT-"+key), connectorID, workspaceID, name, key, id26("CS-"+key))
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO connector_change_batches (id, workspace_id, connector_id, table_name, seq, sync_id, kind, s3_key,
  inserts, updates, deletes) VALUES (?, ?, ?, ?, 1, ?, 'changes', ?, 1, 0, 0)`,
		id26("CB-"+key), workspaceID, connectorID, name, id26("CS-"+key), key+".changes")
	require.NoError(t, err)
}

// currentReading writes version FV-<tag> of f's ontology, which reads sets
// and whose generation (GN-<tag>) drafted from tables, and makes a load of
// it current with documents extracted.
func (f foundryFixture) currentReading(t *testing.T, tag string, tables []model.OntologyGenerationTable, sets []string, documents ...string) string {
	t.Helper()
	versionID := f.versionReading(t, tag, sets...)
	input, err := json.Marshal(model.OntologyGenerationInput{Tables: tables})
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input)
VALUES (?, ?, ?, 'succeeded', ?)`, id26("GN-"+tag), f.ontology.WorkspaceID, f.ontology.ID, string(input))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontology_versions SET generation_id=? WHERE id=?`, id26("GN-"+tag), versionID)
	require.NoError(t, err)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, tag)))
	f.loaded(t, "w-"+tag, documents...)
	return id26("FJ-" + tag)
}

// replacedKeys are the snapshot keys waiting for the sweep, by key.
func replacedKeys(t *testing.T, db store.Store) []string {
	t.Helper()
	replaced, err := db.ListReplacedSnapshots(context.Background(), time.Now().Add(time.Minute), 100)
	require.NoError(t, err)
	keys := []string{}
	for _, r := range replaced {
		keys = append(keys, r.S3Key)
	}
	slices.Sort(keys)
	return keys
}

// Deleting a connector deletes what its syncs wrote: its tables' snapshots,
// its documents and its change batches wait for the sweep, and what they put
// in the graph goes with the refresh the delete owes.
func TestDeleteConnector(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	c := newConnectorSync(t, newFoundryFixture(t, "DEL"), "DEL")
	ws := c.ontology.WorkspaceID
	n1, n2 := id26("FD-n1"), id26("FD-n2")
	c.published(t, c.publish("DEL-a", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day)}, nil))
	c.published(t, c.publish("DEL-b", nil, nil, n2))
	require.NoError(t, c.db.FinishConnectorSync(ctx, c.sync.ID, syncWorker, model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded}))
	connectorTable(t, ws, c.connector.ID, "notes", "DEL-notes")
	// Another connection of the workspace, and an upload, which stay.
	kept := c.connectorSet(t, "DEL-kept", "Kept")
	keptDoc := c.connectorDocument(t, "kept", kept.ID, "not_1", day)
	connectorTable(t, ws, kept.Owner.ConnectorID, "notes", "DEL-kept-notes")
	upload := c.uploaded(t, "DEL-upload")

	notes := []model.OntologyGenerationTable{{ConnectorID: c.connector.ID, Table: "notes"}}
	// A's current load reads the connector's set, B's its table; C reads
	// both with refresh off, and D reads only the other connection.
	readsSet := c.currentReading(t, "A", nil, []string{c.setID}, n1)
	readsTable := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "DELB")}
	readsTable.currentReading(t, "B", notes, []string{generalSetID(readsTable.ontology.ID)})
	off := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "DELC")}
	off.currentReading(t, "C", notes, []string{c.setID}, n1)
	require.NoError(t, c.db.SetFoundryOntologyAutoRefresh(ctx, ws, off.ontology.ID, false))
	other := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "DELD")}
	other.currentReading(t, "D", []model.OntologyGenerationTable{{ConnectorID: kept.Owner.ConnectorID, Table: "notes"}}, []string{kept.ID}, keptDoc)
	// A refreshed ten minutes ago; a delete owes the next one now all the same.
	_, err := testDB.Exec(`UPDATE foundry_loads SET origin='refresh', created_at=NOW(6) - INTERVAL 10 MINUTE WHERE id=?`, readsSet)
	require.NoError(t, err)

	start := time.Now().UTC()
	require.NoError(t, c.db.DeleteConnector(ctx, ws, c.connector.ID))

	_, err = c.db.GetConnector(ctx, ws, c.connector.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = c.db.GetFoundryDocumentSet(ctx, ws, c.setID)
	assert.ErrorIs(t, err, store.ErrNotFound, "its set goes with it")
	stored, err := c.db.GetFoundryDocument(ctx, ws, n1)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryDocumentDeleted, stored.Status)
	assert.NotNil(t, stored.DeletedAt)
	assert.Equal(t, []string{n1, n2}, purgeRows(t), "each document waits for the sweep once, the one removed before too")
	tables, err := c.db.ListConnectorTables(ctx, ws, c.connector.ID)
	require.NoError(t, err)
	assert.Empty(t, tables)
	assert.Equal(t, []string{"DEL-notes"}, replacedKeys(t, c.db), "its snapshot waits for the sweep as a replaced one")
	batches, err := c.db.ListExpiredChangeBatches(ctx, time.Now().Add(-time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, batches, 1, "its batches go at the sweep's next tick, the other connection's stay")
	assert.Equal(t, id26("CB-DEL-notes"), batches[0].ID)

	due := func(f foundryFixture) *time.Time { return f.ontologyNow(t).RefreshDueAt }
	require.NotNil(t, due(c.foundryFixture))
	assert.WithinDuration(t, start, *due(c.foundryFixture), 5*time.Second, "its current load reads the set")
	require.NotNil(t, due(readsTable))
	assert.WithinDuration(t, start, *due(readsTable), 5*time.Second, "its current load reads the table")
	assert.Nil(t, due(off), "refresh is off")
	assert.Nil(t, due(other), "it reads neither")

	keptTables, err := c.db.ListConnectorTables(ctx, ws, kept.Owner.ConnectorID)
	require.NoError(t, err)
	assert.Len(t, keptTables, 1)
	_, err = c.db.GetFoundryDocumentSet(ctx, ws, kept.ID)
	require.NoError(t, err)
	for _, id := range []string{keptDoc, upload} {
		d, err := c.db.GetFoundryDocument(ctx, ws, id)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, d.Status, id)
	}
	assert.ErrorIs(t, c.db.DeleteConnector(ctx, ws, c.connector.ID), store.ErrNotFound)
}

// A workspace being deleted takes its connectors and what their syncs wrote
// with it, once no sync of them can still write.
func TestDeleteWorkspaceConnectors(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f := newFoundryFixture(t, "DWS")
	ws := f.ontology.WorkspaceID
	a, b := f.connectorSet(t, "DWS-a", "Granola"), f.connectorSet(t, "DWS-b", "Notion")
	docA, docB := f.connectorDocument(t, "a", a.ID, "not_1", day), f.connectorDocument(t, "b", b.ID, "not_1", day)
	connectorTable(t, ws, a.Owner.ConnectorID, "notes", "DWS-a-notes")
	connectorTable(t, ws, b.Owner.ConnectorID, "pages", "DWS-b-pages")
	f.currentReading(t, "L1", []model.OntologyGenerationTable{{ConnectorID: a.Owner.ConnectorID, Table: "notes"}}, []string{a.ID, b.ID},
		docA, docB)
	elsewhere := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, id26("FWS-DWS-OTHER"), "DWSO")}
	elsewhere.connectorSet(t, "DWS-o", "Elsewhere")

	// A sync still live may write a table or document until it ends.
	require.NoError(t, f.db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CS-DWS"), ConnectorID: a.Owner.ConnectorID, WorkspaceID: ws}))
	assert.ErrorIs(t, f.db.DeleteWorkspaceConnectors(ctx, ws), store.ErrConflict)
	live, err := f.db.ListConnectors(ctx, ws)
	require.NoError(t, err)
	assert.Len(t, live, 2, "nothing is deleted while it is live")
	claimed, err := f.db.ClaimConnectorSync(ctx, syncWorker, time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.db.FinishConnectorSync(ctx, claimed.ID, syncWorker,
		model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed, Error: "the workspace was deleted"}))

	require.NoError(t, f.db.DeleteWorkspaceConnectors(ctx, ws))
	live, err = f.db.ListConnectors(ctx, ws)
	require.NoError(t, err)
	assert.Empty(t, live)
	for _, set := range []string{a.ID, b.ID} {
		_, err := f.db.GetFoundryDocumentSet(ctx, ws, set)
		assert.ErrorIs(t, err, store.ErrNotFound)
	}
	for _, id := range []string{docA, docB} {
		d, err := f.db.GetFoundryDocument(ctx, ws, id)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentDeleted, d.Status)
	}
	assert.Equal(t, []string{docA, docB}, purgeRows(t))
	for _, connectorID := range []string{a.Owner.ConnectorID, b.Owner.ConnectorID} {
		tables, err := f.db.ListConnectorTables(ctx, ws, connectorID)
		require.NoError(t, err)
		assert.Empty(t, tables)
	}
	assert.Equal(t, []string{"DWS-a-notes", "DWS-b-pages"}, replacedKeys(t, f.db))
	assert.Nil(t, f.ontologyNow(t).RefreshDueAt, "the ontologies go with the workspace")
	others, err := f.db.ListConnectors(ctx, elsewhere.ontology.WorkspaceID)
	require.NoError(t, err)
	assert.Len(t, others, 1, "another workspace's connector stays")
	require.NoError(t, f.db.DeleteWorkspaceConnectors(ctx, ws), "a retry finds nothing left to delete")
}
