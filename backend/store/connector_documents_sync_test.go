package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// notesKind is a connector kind whose one table, notes, carries the notes
// stream: each row's document is its text. notes are id, version and text,
// newest first.
type notesKind struct{ notes [][3]string }

func (*notesKind) Name() string                                          { return "fake" }
func (*notesKind) DisplayName() string                                   { return "Fake" }
func (*notesKind) Prepare(src connector.Source) (json.RawMessage, error) { return src.Settings, nil }
func (*notesKind) Hint(string) string                                    { return "" }

func (*notesKind) Discover(context.Context, connector.Source) (connector.Discovery, error) {
	return connector.Discovery{Tables: []connector.Table{{Name: "notes", PrimaryKey: []string{"id"}}}}, nil
}

func (*notesKind) Sync(context.Context, connector.Source, connector.Table, connector.RowWriter) (connector.Table, error) {
	return connector.Table{}, errors.New("notes carries a stream: the syncer reads it with SyncDocuments")
}

func (*notesKind) Streams(connector.Source) []connector.Stream {
	return []connector.Stream{{Name: "notes", Table: "notes", Title: "Notes", Description: "Meeting notes", Format: connector.Markdown}}
}

func (k *notesKind) SyncDocuments(ctx context.Context, _ connector.Source, table connector.Table, rows connector.RowWriter,
	docs connector.DocumentWriter) (connector.Table, error) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for _, n := range k.notes {
		if err := rows.Write(map[string]any{"id": n[0], "version": n[1]}); err != nil {
			return table, err
		}
		if docs.Want("notes", n[0], n[1]) {
			if err := docs.Put(ctx, "notes", connector.Document{ExternalID: n[0], Version: n[1], At: at, Title: n[0], Text: []byte(n[2])}); err != nil {
				return table, err
			}
		}
	}
	return table, nil
}

// syncBucket holds a sync's snapshots and documents' originals.
type syncBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *syncBucket) Put(_ context.Context, key string, body io.ReadSeeker) error {
	return b.PutOnce(context.Background(), key, "", body)
}

func (b *syncBucket) PutOnce(_ context.Context, key, _ string, body io.ReadSeeker) error {
	data, err := io.ReadAll(body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
	return err
}

func (b *syncBucket) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return io.NopCloser(bytes.NewReader(b.objects[key])), nil
}

func (b *syncBucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	return nil
}

// TestSyncerConnectorDocuments runs the syncer against MySQL: what a sync's
// documents publish is what the provider keeps, reads and fans out.
func TestSyncerConnectorDocuments(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "SYN")
	ws, connectorID := f.ontology.WorkspaceID, id26("CONN-SYN")
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)})
	require.NoError(t, err)
	sealed, keyID, err := keys.Seal([]byte("secret"), connector.SecretContext(connectorID))
	require.NoError(t, err)
	require.NoError(t, f.db.CreateConnector(ctx, &model.Connector{ID: connectorID, WorkspaceID: ws, Kind: "fake", Name: "Notes app",
		Config: json.RawMessage(`{}`), SecretCiphertext: sealed, SecretKeyID: keyID, SecretHint: "…"}))
	plans := config.DefaultPlans()
	for i := range plans {
		plans[i].Kinds = append(plans[i].Kinds, "fake")
	}
	planCatalog, err := foundryplan.NewCatalog(plans, nil)
	require.NoError(t, err)
	kind := &notesKind{notes: [][3]string{{"n2", "1|a", "second"}, {"n1", "1|a", "first"}}}
	bucket := &syncBucket{objects: map[string][]byte{}}
	s := &syncer.Syncer{
		DB: f.db, Catalog: connector.NewCatalogOf(kind), Keys: keys, PlanCatalog: planCatalog, Refresh: f.db, Snapshots: bucket,
		MaxRows: 100, MaxBytes: 1 << 20, TempDir: t.TempDir(), Documents: bucket, MaxDocumentBytes: 1 << 20, MaxPerStream: 1000,
		PurgeGrace: time.Hour, MonthBudget: func(time.Time) model.FoundryGraphBudget { return noBudget },
	}
	rows := int64(0)
	// run claims a sync of the connector and runs it as worker, which may not
	// hold the lease; the claim is finished either way.
	run := func(tag, worker string) error {
		t.Helper()
		require.NoError(t, f.db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CS-" + tag), ConnectorID: connectorID, WorkspaceID: ws}))
		claimed, err := f.db.ClaimConnectorSync(ctx, syncWorker, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		claimed.LeaseOwner = &worker
		_, runErr := s.Run(ctx, claimed)
		require.NoError(t, f.db.FinishConnectorSync(ctx, claimed.ID, syncWorker, model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows}))
		return runErr
	}
	// index is the set's live index as "<external id>@<version>=<text>".
	index := func(setID string) []string {
		t.Helper()
		page, err := f.db.ListConnectorDocuments(ctx, ws, setID, "", 100)
		require.NoError(t, err)
		var out []string
		for _, d := range page {
			out = append(out, d.ExternalID+"@"+d.Version+"="+string(bucket.objects["ws/"+ws+"/documents/"+d.ID+"/original"]))
		}
		return out
	}

	require.NoError(t, run("1", syncWorker))
	sets, err := f.db.ListFoundryDocumentSets(ctx, ws)
	require.NoError(t, err)
	var set *model.FoundryDocumentSet
	for _, s := range sets {
		if s.Owner != nil {
			set = s
		}
	}
	require.NotNil(t, set)
	assert.Equal(t, "Notes app / Notes", set.DisplayName())
	assert.Equal(t, 2, set.Documents)
	assert.Equal(t, []string{"n1@1|a=first", "n2@1|a=second"}, index(set.ID))
	assert.Empty(t, purgeRows(t), "publishing claimed the reservations")
	first := index(set.ID)

	// An ontology's current load reads the set: new documents fan out to it,
	// and a replaced or removed one owes it a refresh.
	held, err := f.db.ListConnectorDocuments(ctx, ws, set.ID, "", 100)
	require.NoError(t, err)
	load := f.loadReading(t, "L1", set.ID)
	f.loaded(t, "w1", held[0].ID, held[1].ID)
	require.NoError(t, run("2", syncWorker))
	assert.Equal(t, first, index(set.ID), "an unchanged sync writes nothing")
	assert.Nil(t, f.ontologyNow(t).RefreshDueAt)

	kind.notes = [][3]string{{"n3", "1|a", "third"}, {"n2", "2|b", "second, edited"}}
	require.NoError(t, run("3", syncWorker))
	assert.Equal(t, []string{"n2@2|b=second, edited", "n3@1|a=third"}, index(set.ID))
	assert.Len(t, purgeRows(t), 2, "the replaced and the removed document wait for the sweep")
	assert.NotNil(t, f.ontologyNow(t).RefreshDueAt)
	job := f.liveJob(t)
	require.NotNil(t, job, "the new documents queued an extract into the current load")
	assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	assert.Len(t, members(t, load), 4, "the load holds the two it read and the two added")

	// A sync that lost its lease writes nothing.
	kind.notes = [][3]string{{"n4", "1|a", "fourth"}}
	assert.Error(t, run("4", "sync-w2"))
	assert.Equal(t, []string{"n2@2|b=second, edited", "n3@1|a=third"}, index(set.ID))
	assert.Len(t, purgeRows(t), 2)
}
