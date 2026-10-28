package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncWorker is the sync worker holding a connectorSync's lease.
const syncWorker = "sync-w1"

// connectorSync is a sync, leased to syncWorker, of connector CONN-<tag> of
// a fixture's workspace, which has ensured the connector's notes set.
type connectorSync struct {
	foundryFixture
	connector *model.Connector
	sync      *model.ConnectorSync
	setID     string
}

// newConnectorSync writes connector CONN-<tag> of f's workspace, claims a
// sync of it and ensures its notes set (new id FS-<tag>), as a sync's
// start does.
func newConnectorSync(t *testing.T, f foundryFixture, tag string) connectorSync {
	t.Helper()
	ctx := context.Background()
	c := &model.Connector{
		ID: id26("CONN-" + tag), WorkspaceID: f.ontology.WorkspaceID, Kind: "granola", Name: "Granola " + tag, Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte{1}, SecretKeyID: "k1", SecretHint: "grn_…1",
	}
	require.NoError(t, f.db.CreateConnector(ctx, c))
	require.NoError(t, f.db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("CS-" + tag), ConnectorID: c.ID, WorkspaceID: c.WorkspaceID}))
	sync, err := f.db.ClaimConnectorSync(ctx, syncWorker, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, sync)
	setID, err := f.db.EnsureConnectorDocumentSet(ctx, notesSet(c, tag), sync.ID, syncWorker)
	require.NoError(t, err)
	return connectorSync{foundryFixture: f, connector: c, sync: sync, setID: setID}
}

// notesSet is connector c's notes set as its sync asks for it, under new id
// FS-<tag>.
func notesSet(c *model.Connector, tag string) *model.FoundryDocumentSet {
	return &model.FoundryDocumentSet{
		ID: id26("FS-" + tag), WorkspaceID: c.WorkspaceID, Name: "Notes", Description: "Meeting notes",
		Owner: &model.FoundryDocumentSetOwner{ConnectorID: c.ID, Stream: "notes"},
	}
}

// note is FD-<tag>, the document of row externalID of the set at version,
// created at the source at, as a sync puts it.
func (c connectorSync) note(tag, externalID, version string, at time.Time) *model.FoundryDocument {
	d := foundryDocument(c.ontology, tag)
	d.SetID, d.ClientToken = c.setID, nil
	d.Source = &model.FoundryDocumentSource{ExternalID: externalID, Version: version, At: at}
	return d
}

// publish is a flush into the set, with an hour's grace and refresh interval
// and a sync's fan-out (extracts FX-<tag>…, no requester).
func (c connectorSync) publish(tag string, add []*model.FoundryDocument, update []model.ConnectorDocumentUpdate, remove ...string) model.ConnectorDocumentsPublish {
	return model.ConnectorDocumentsPublish{
		WorkspaceID: c.ontology.WorkspaceID, ConnectorID: c.connector.ID, SetID: c.setID, Add: add, Update: update, Remove: remove,
		Grace: time.Hour, Fan: c.fan(tag),
	}
}

// published reserves p's adds, then publishes p, as a flush does.
func (c connectorSync) published(t *testing.T, p model.ConnectorDocumentsPublish) []model.FoundryDocumentRead {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, c.db.ReserveConnectorDocuments(ctx, p.WorkspaceID, documentIDs(p.Add), c.sync.ID, syncWorker))
	reads, err := c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, syncWorker)
	require.NoError(t, err)
	return reads
}

// index is the set's live index, read two at a time, as
// "<external id>=<id>@<version>".
func (c connectorSync) index(t *testing.T) []string {
	t.Helper()
	var out []string
	after := ""
	for {
		page, err := c.db.ListConnectorDocuments(context.Background(), c.ontology.WorkspaceID, c.setID, after, 2)
		require.NoError(t, err)
		for _, d := range page {
			out = append(out, fmt.Sprintf("%s=%s@%s", d.ExternalID, d.ID, d.Version))
		}
		if len(page) < 2 {
			return out
		}
		after = page[len(page)-1].ExternalID
	}
}

// purgeRows are the documents with a purge row, by id.
func purgeRows(t *testing.T) []string {
	t.Helper()
	rows, err := testDB.Query(`SELECT document_id FROM foundry_document_purges ORDER BY document_id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

// members are the documents that joined a load, by id.
func members(t *testing.T, loadID string) []string {
	t.Helper()
	rows, err := testDB.Query(`SELECT document_id FROM foundry_load_documents WHERE load_id=? ORDER BY document_id`, loadID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestEnsureConnectorDocumentSet(t *testing.T) {
	// A connector has one set per stream for as long as it lives: every
	// sync asks for it and gets the id it has had since, named and
	// described as the stream is now.
	t.Run("keeps one set per connector and stream", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "ENS1"), "ENS1")
		ws := c.ontology.WorkspaceID
		assert.Equal(t, id26("FS-ENS1"), c.setID)
		again := notesSet(c.connector, "ENS1-again")
		again.Name, again.Description = "Meetings", "Summaries of meetings"
		id, err := c.db.EnsureConnectorDocumentSet(ctx, again, c.sync.ID, syncWorker)
		require.NoError(t, err)
		assert.Equal(t, c.setID, id, "the set it has had since")
		set, err := c.db.GetFoundryDocumentSet(ctx, ws, id)
		require.NoError(t, err)
		assert.Equal(t, "Meetings", set.Name)
		assert.Equal(t, "Summaries of meetings", set.Description)
		assert.Equal(t, &model.FoundryDocumentSetOwner{ConnectorID: c.connector.ID, ConnectorKind: "granola", ConnectorName: "Granola ENS1",
			Stream: "notes"}, set.Owner)
		assert.Nil(t, set.CreatedByUserID)

		transcripts := notesSet(c.connector, "ENS1-transcripts")
		transcripts.Name, transcripts.Owner.Stream = "Transcripts", "transcripts"
		id, err = c.db.EnsureConnectorDocumentSet(ctx, transcripts, c.sync.ID, syncWorker)
		require.NoError(t, err)
		assert.Equal(t, transcripts.ID, id, "another stream has a set of its own")
		assert.Equal(t, []string{"General ENS1=0", "Meetings=0", "Transcripts=0"}, c.setNames(t))
	})

	// A worker that lost the lease writes nothing, and a deleted connector
	// gets no set.
	t.Run("refuses a lost lease and a deleted connector", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "ENS2"), "ENS2")
		ws := c.ontology.WorkspaceID
		transcripts := notesSet(c.connector, "ENS2-transcripts")
		transcripts.Name, transcripts.Owner.Stream = "Transcripts", "transcripts"
		_, err := c.db.EnsureConnectorDocumentSet(ctx, transcripts, c.sync.ID, "sync-w2")
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, c.db.DeleteConnector(ctx, ws, c.connector.ID))
		_, err = c.db.EnsureConnectorDocumentSet(ctx, transcripts, c.sync.ID, syncWorker)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = c.db.GetFoundryDocumentSet(ctx, ws, transcripts.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("takes only a connector's set", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "ENS3"), "ENS3")
		_, err := c.db.EnsureConnectorDocumentSet(ctx, c.documentSet("ENS3-mine"), c.sync.ID, syncWorker)
		assert.ErrorContains(t, err, "a connector's set has its owner and no creator")
		made := notesSet(c.connector, "ENS3-made")
		made.CreatedByUserID = new(id26("U-ENS3"))
		_, err = c.db.EnsureConnectorDocumentSet(ctx, made, c.sync.ID, syncWorker)
		assert.ErrorContains(t, err, "a connector's set has its owner and no creator")
	})
}

func TestPublishConnectorDocuments(t *testing.T) {
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	// A flush adds new rows' documents, changes in place one whose bytes
	// did not change, replaces one whose bytes did and removes one gone at
	// the source. The live index the next sync reads follows, and every
	// object in S3 keeps a row that owns it.
	t.Run("adds, updates, replaces and removes in one publish", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB1"), "PUB1")
		ws := c.ontology.WorkspaceID
		n1, n2, n3 := id26("FD-n1"), id26("FD-n2"), id26("FD-n3")
		reads := c.published(t, c.publish("PUB1-a", []*model.FoundryDocument{
			c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day.Add(time.Hour)), c.note("n3", "not_3", "1|a", day.Add(2*time.Hour)),
		}, nil))
		assert.Empty(t, reads, "no ontology reads the set")
		assert.Equal(t, []string{"not_1=" + n1 + "@1|a", "not_2=" + n2 + "@1|a", "not_3=" + n3 + "@1|a"}, c.index(t))
		at := func() []time.Time {
			listed, err := c.db.ListConnectorDocuments(ctx, ws, c.setID, "", 10)
			require.NoError(t, err)
			out := make([]time.Time, len(listed))
			for i, d := range listed {
				out[i] = d.At.UTC()
			}
			return out
		}
		assert.Equal(t, []time.Time{day, day.Add(time.Hour), day.Add(2 * time.Hour)}, at(), "each with its time at the source")
		assert.Empty(t, purgeRows(t), "publishing claims the reservations")
		stored, err := c.db.GetFoundryDocument(ctx, ws, n1)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentUploaded, stored.Status)
		require.NotNil(t, stored.UploadedAt)
		assert.Nil(t, stored.ClientToken)
		assert.Nil(t, stored.Source.URL)

		link := "https://notes.granola.ai/d/1"
		update := model.ConnectorDocumentUpdate{ID: n1, Name: "Kickoff.md", Version: "1|b", At: day.Add(-time.Hour), URL: &link}
		reads = c.published(t, c.publish("PUB1-b", []*model.FoundryDocument{c.note("n2b", "not_2", "1|b", day.Add(time.Hour))},
			[]model.ConnectorDocumentUpdate{update}, n2, n3))
		assert.Empty(t, reads)
		n2b := id26("FD-n2b")
		assert.Equal(t, []string{"not_1=" + n1 + "@1|b", "not_2=" + n2b + "@1|b"}, c.index(t))
		assert.Equal(t, []time.Time{day.Add(-time.Hour), day.Add(time.Hour)}, at(), "an update moves its time")
		stored, err = c.db.GetFoundryDocument(ctx, ws, n1)
		require.NoError(t, err)
		assert.Equal(t, "Kickoff.md", stored.Name)
		assert.Equal(t, &model.FoundryDocumentSource{ExternalID: "not_1", Version: "1|b", At: day.Add(-time.Hour), URL: &link}, stored.Source)
		for _, gone := range []string{n2, n3} {
			d, err := c.db.GetFoundryDocument(ctx, ws, gone)
			require.NoError(t, err)
			assert.Equal(t, model.FoundryDocumentDeleted, d.Status)
			assert.NotNil(t, d.DeletedAt)
		}
		assert.Equal(t, []string{n2, n3}, purgeRows(t), "what was replaced or removed waits for the sweep")
		docs, _, err := c.db.ListFoundryDocuments(ctx, ws, c.setID, "", 10)
		require.NoError(t, err)
		assert.Equal(t, []string{n2b, n1}, documentIDs(docs), "newest at the source first")
	})

	// Nothing of a refused publish is written: not its removals, updates,
	// adds or reservations' claims, nor what a load joins.
	t.Run("writes all of a publish or none of it", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB2"), "PUB2")
		ws := c.ontology.WorkspaceID
		n1, n2 := id26("FD-n1"), id26("FD-n2")
		c.published(t, c.publish("PUB2-a", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day)}, nil))
		load := c.loadReading(t, "L1", c.setID)
		c.loaded(t, "w1", n1, n2)
		before := c.index(t)

		// not_2 is still live: its new document can't go in beside it.
		p := c.publish("PUB2-b", []*model.FoundryDocument{c.note("n3", "not_3", "1|a", day), c.note("clash", "not_2", "1|b", day)},
			[]model.ConnectorDocumentUpdate{{ID: n2, Name: "n2.md", Version: "1|b", At: day}}, n1)
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, documentIDs(p.Add), c.sync.ID, syncWorker))
		_, err := c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, syncWorker)
		assert.ErrorIs(t, err, store.ErrConflict)
		assert.Equal(t, before, c.index(t))
		assert.Equal(t, []string{id26("FD-clash"), id26("FD-n3")}, purgeRows(t), "the reservations wait for the sweep")
		assert.Equal(t, []string{n1, n2}, members(t, load))
		assert.Nil(t, c.liveJob(t))
		assert.Nil(t, c.ontologyNow(t).RefreshDueAt)
	})

	// A sync's adds join the load that reads the set, and only they do:
	// what the transaction does is the size of the flush, not of the scope.
	t.Run("joins only what it adds to the load that reads the set", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB3"), "PUB3")
		held := c.connectorDocument(t, "held", c.setID, "not_0", day)
		load := c.loadReading(t, "L1", c.setID)
		c.loaded(t, "w1", held)
		other := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, c.ontology.WorkspaceID, "PUB3B")}
		other.loadReading(t, "LB", generalSetID(other.ontology.ID))
		other.loaded(t, "w1")
		// In the set, but put there by no publish: no fan-out joined it.
		late := c.connectorDocument(t, "late", c.setID, "not_9", day)

		reads := c.published(t, c.publish("PUB3", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day), c.note("n2", "not_2", "1|a", day)}, nil))
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: c.ontology.ID, Outcome: model.FoundryDocumentReadQueued}}, reads,
			"not the ontology whose load doesn't read the set")
		job := c.liveJob(t)
		require.NotNil(t, job)
		assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
		assert.Equal(t, load, job.LoadID)
		assert.Nil(t, job.RequestedByUserID, "work no person started")
		assert.Equal(t, []string{held, id26("FD-n1"), id26("FD-n2")}, members(t, load), "not %s", late)
		pending, err := c.db.ListPendingFoundryDocuments(ctx, load)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{id26("FD-n1"), id26("FD-n2")}, documentIDs(pending))
		assert.Nil(t, other.liveJob(t))
	})

	// A publish more than half the grace after its reservation might race
	// the sweep deleting its bytes, so it is refused whole; so is one whose
	// bytes were never reserved.
	t.Run("refuses a late reservation", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB4"), "PUB4")
		ws := c.ontology.WorkspaceID
		kept := id26("FD-kept")
		c.published(t, c.publish("PUB4-a", []*model.FoundryDocument{c.note("kept", "not_0", "1|a", day)}, nil))
		late, onTime := c.note("late", "not_1", "1|a", day), c.note("ontime", "not_2", "1|a", day)
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{late.ID, onTime.ID}, c.sync.ID, syncWorker))
		assert.ErrorIs(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{late.ID}, c.sync.ID, syncWorker), store.ErrConflict,
			"an id is reserved once")
		for id, age := range map[string]time.Duration{late.ID: 31 * time.Minute, onTime.ID: 29 * time.Minute} {
			_, err := testDB.Exec(`UPDATE foundry_document_purges SET created_at=NOW(6) - INTERVAL ? MICROSECOND WHERE document_id=?`,
				age.Microseconds(), id)
			require.NoError(t, err)
		}

		_, err := c.db.PublishConnectorDocuments(ctx, c.publish("PUB4-b", []*model.FoundryDocument{late, onTime}, nil, kept), c.sync.ID, syncWorker)
		assert.ErrorIs(t, err, store.ErrReservationExpired)
		_, err = c.db.GetFoundryDocument(ctx, ws, late.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, []string{"not_0=" + kept + "@1|a"}, c.index(t), "the removal goes with it")
		assert.Equal(t, []string{late.ID, onTime.ID}, purgeRows(t))

		never := c.note("never", "not_3", "1|a", day)
		_, err = c.db.PublishConnectorDocuments(ctx, c.publish("PUB4-c", []*model.FoundryDocument{onTime, never}, nil), c.sync.ID, syncWorker)
		assert.ErrorIs(t, err, store.ErrReservationExpired, "bytes no row owns")
		_, err = c.db.PublishConnectorDocuments(ctx, c.publish("PUB4-d", []*model.FoundryDocument{onTime}, nil), c.sync.ID, syncWorker)
		require.NoError(t, err)
		assert.Equal(t, []string{"not_0=" + kept + "@1|a", "not_2=" + onTime.ID + "@1|a"}, c.index(t))
		assert.Equal(t, []string{late.ID}, purgeRows(t), "the late one's bytes are swept")
	})

	// Only the lease holder writes, and only into a live connector's set.
	t.Run("refuses a lost lease and a deleted connector", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB5"), "PUB5")
		ws := c.ontology.WorkspaceID
		n1 := c.note("n1", "not_1", "1|a", day)
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{n1.ID}, c.sync.ID, syncWorker))
		assert.ErrorIs(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{id26("FD-n2")}, c.sync.ID, "sync-w2"), store.ErrNotFound)
		_, err := c.db.PublishConnectorDocuments(ctx, c.publish("PUB5-a", []*model.FoundryDocument{n1}, nil), c.sync.ID, "sync-w2")
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, c.db.DeleteConnector(ctx, ws, c.connector.ID))
		_, err = c.db.PublishConnectorDocuments(ctx, c.publish("PUB5-b", []*model.FoundryDocument{n1}, nil), c.sync.ID, syncWorker)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = c.db.GetFoundryDocument(ctx, ws, n1.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.Equal(t, []string{n1.ID}, purgeRows(t), "its bytes are swept")
	})

	t.Run("takes only connectors' documents of the set", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB6"), "PUB6")
		upload := c.note("upload", "not_1", "1|a", day)
		upload.ClientToken = new("doc-upload")
		elsewhere := c.note("elsewhere", "not_2", "1|a", day)
		elsewhere.SetID = generalSetID(c.ontology.ID)
		uploader := c.note("uploader", "not_3", "1|a", day)
		uploader.CreatedByUserID = new(id26("U-PUB6"))
		for _, d := range []*model.FoundryDocument{upload, elsewhere, uploader, foundryDocument(c.ontology, "plain")} {
			_, err := c.db.PublishConnectorDocuments(ctx, c.publish("PUB6", []*model.FoundryDocument{d}, nil), c.sync.ID, syncWorker)
			assert.ErrorContains(t, err, "is not a connector's document of set", d.ID)
		}
		p := c.publish("PUB6", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil)
		p.Grace = 0
		_, err := c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, syncWorker)
		assert.ErrorContains(t, err, "adds need the sweep's grace and a whole fan-out")
		reads, err := c.db.PublishConnectorDocuments(ctx, c.publish("PUB6", nil, nil), "no-sync", "no-worker")
		require.NoError(t, err, "an empty publish writes nothing")
		assert.Empty(t, reads)
	})

	// The graph worker deletes a document whose original is gone; the sync
	// that then updates or removes it leaves it so, and owes nothing.
	t.Run("leaves a document no longer uploaded as it is", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB7"), "PUB7")
		ws := c.ontology.WorkspaceID
		n1 := id26("FD-n1")
		c.published(t, c.publish("PUB7-a", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		c.loadReading(t, "L1", c.setID)
		c.loaded(t, "w1", n1)
		require.NoError(t, c.db.DeleteFoundryDocument(ctx, ws, n1))
		c.published(t, c.publish("PUB7-b", nil, []model.ConnectorDocumentUpdate{{ID: n1, Name: "x.md", Version: "1|b", At: day}}, n1))
		d, err := c.db.GetFoundryDocument(ctx, ws, n1)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDocumentDeleted, d.Status)
		assert.Equal(t, "1|a", d.Source.Version)
		assert.Empty(t, purgeRows(t))
		assert.Nil(t, c.ontologyNow(t).RefreshDueAt)
	})

	// What a sync writes stays its own: no person's change to the set or
	// its documents goes through.
	t.Run("leaves what it wrote to the connector's syncs", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "PUB8"), "PUB8")
		ws, general := c.ontology.WorkspaceID, generalSetID(c.ontology.ID)
		n1 := id26("FD-n1")
		c.published(t, c.publish("PUB8", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		upload := c.uploaded(t, "PUB8-upload")

		_, _, err := c.db.CreateFoundryDocument(ctx, c.documentIn("PUB8-into", c.setID), noCap)
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "an upload into the set")
		_, _, err = c.db.MoveFoundryDocument(ctx, ws, upload, c.setID, c.fan("PUB8-in"))
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "a move into the set")
		_, _, err = c.db.MoveFoundryDocument(ctx, ws, n1, general, c.fan("PUB8-out"))
		assert.ErrorIs(t, err, store.ErrConnectorOwned, "a move out of the set")
		assert.ErrorIs(t, c.db.RenameFoundryDocument(ctx, ws, n1, "Mine.md"), store.ErrConnectorOwned, "a rename of a document")
		assert.ErrorIs(t, c.db.UpdateFoundryDocumentSet(ctx, ws, c.setID, model.FoundryDocumentSetUpdate{Name: new("Mine")}),
			store.ErrConnectorOwned, "a change to the set")
		assert.ErrorIs(t, c.db.DeleteFoundryDocumentSet(ctx, ws, c.setID), store.ErrConnectorOwned, "a delete of the set")
		assert.Equal(t, []string{"not_1=" + n1 + "@1|a"}, c.index(t))
	})
}

func TestConnectorDocumentFanOut(t *testing.T) {
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	// With a job live, its load takes the documents when its version reads
	// the set, and its end owes the current load an extract.
	t.Run("leaves them to a live job", func(t *testing.T) {
		c := newConnectorSync(t, newFoundryFixture(t, "CFO1"), "CFO1")
		c.loadReading(t, "L1", c.setID)
		c.loaded(t, "w1")
		writing := c.loadReading(t, "L2", c.setID)
		reads := c.published(t, c.publish("CFO1", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: c.ontology.ID, Outcome: model.FoundryDocumentReadOwed}}, reads)
		assert.True(t, c.ontologyNow(t).ExtractOwed)
		assert.Equal(t, []string{id26("FD-n1")}, members(t, writing))
	})

	// A sync's extract is work no person started: it stops a job's cap
	// short of the month, which a person's upload may still spend.
	t.Run("queues within the month's share for work no person started", func(t *testing.T) {
		prod := model.FoundryGraphBudget{Since: time.Now().Add(-time.Hour), CapMicros: 50_000_000, ReserveMicros: 10_000_000}
		for i, tc := range []struct {
			spent int64
			want  model.FoundryDocumentReadOutcome
		}{
			{39_990_000, model.FoundryDocumentReadQueued},
			{40_000_000, model.FoundryDocumentReadNoBudget},
		} {
			t.Run(fmt.Sprintf("$%.2f spent", float64(tc.spent)/1e6), func(t *testing.T) {
				ctx := context.Background()
				tag := fmt.Sprintf("CFO2-%d", i)
				c := newConnectorSync(t, newFoundryFixture(t, tag), tag)
				load := c.loadReading(t, "L1", c.setID)
				c.loaded(t, "w1")
				_, err := testDB.ExecContext(ctx, `UPDATE foundry_load_jobs SET usd_micros=? WHERE id=?`, tc.spent, load)
				require.NoError(t, err)
				p := c.publish(tag, []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil)
				p.Fan.Budget = prod
				reads := c.published(t, p)
				assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: c.ontology.ID, Outcome: tc.want}}, reads)
				assert.Equal(t, []string{id26("FD-n1")}, members(t, load), "it joins the load either way")
			})
		}
	})
}

func TestFoundryDocumentPurges(t *testing.T) {
	// The sweep takes what is older than the grace, oldest first, and keeps
	// the original of a document a live job's load holds until the job ends.
	t.Run("lists what the grace has passed, but not what a live job holds", func(t *testing.T) {
		ctx := context.Background()
		day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		c := newConnectorSync(t, newFoundryFixture(t, "PRG"), "PRG")
		ws := c.ontology.WorkspaceID
		n1 := id26("FD-n1")
		c.published(t, c.publish("PRG-a", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil))
		load := c.loadReading(t, "L1", c.setID)
		job := c.claimed(t, "w1", load)
		c.published(t, c.publish("PRG-b", nil, nil, n1))
		old, older, fresh := id26("FD-old"), id26("FD-older"), id26("FD-fresh")
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{old, older, fresh}, c.sync.ID, syncWorker))
		for id, age := range map[string]time.Duration{n1: 4 * time.Hour, older: 3 * time.Hour, old: 2 * time.Hour} {
			_, err := testDB.Exec(`UPDATE foundry_document_purges SET created_at=NOW(6) - INTERVAL ? MICROSECOND WHERE document_id=?`,
				age.Microseconds(), id)
			require.NoError(t, err)
		}
		listed := func(limit int) []string {
			t.Helper()
			purges, err := c.db.ListFoundryDocumentPurges(ctx, time.Now().Add(-time.Hour), limit)
			require.NoError(t, err)
			var ids []string
			for _, p := range purges {
				assert.Equal(t, ws, p.WorkspaceID)
				ids = append(ids, p.DocumentID)
			}
			return ids
		}

		assert.Equal(t, []string{older, old}, listed(10), "the live load holds the removed one; the fresh one waits")
		assert.Equal(t, []string{older}, listed(1))
		c.ended(t, "w1", job.ID, model.FoundryGraphJobSucceeded, next("PRG"))
		removal := c.liveJob(t)
		require.NotNil(t, removal, "the load holds the removed document: an extract takes it out")
		assert.Equal(t, []string{older, old}, listed(10), "the extract's load holds it")
		c.claimed(t, "w1", removal.ID)
		require.NoError(t, c.db.RecordFoundryDocumentRemoval(ctx, removal.ID, "w1", n1))
		c.ended(t, "w1", removal.ID, model.FoundryGraphJobSucceeded, next("PRG2"))
		assert.Equal(t, []string{n1, older, old}, listed(10), "the jobs ended")
		require.NoError(t, c.db.DeleteFoundryDocumentPurge(ctx, older))
		assert.Equal(t, []string{n1, old}, listed(10))
	})
}

// A publish locks the workspace's live ontologies before the connector, its
// set and documents, as CreateFoundryLoad locks its ontology before reading
// documents: run together, neither waits for what the other holds, so no
// deadlock ends either.
func TestConnectorDocumentPublishLockOrder(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	c := newConnectorSync(t, newFoundryFixture(t, "CLK"), "CLK")
	ws := c.ontology.WorkspaceID
	b := foundryFixture{db: c.db, ontology: sampleOntology(t, c.db, ws, "CLKB")}
	for i, f := range []foundryFixture{c.foundryFixture, b} {
		f.loadReading(t, fmt.Sprintf("CLK-L%d", i), c.setID)
		f.loaded(t, "w1")
	}
	previous := ""
	for round := range 8 {
		// Leave no job live, so both loads can be created.
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET status='cancelled', active_key=NULL, finished_at=NOW(6) WHERE workspace_id=? AND active_key=1`, ws)
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_loads SET status='failed', finished_at=NOW(6) WHERE workspace_id=? AND status='writing'`, ws)
		require.NoError(t, err)
		require.NoError(t, c.db.RenewConnectorSyncLease(ctx, c.sync.ID, syncWorker, time.Minute))
		loads := make([]*model.FoundryGraphJob, 2)
		for i, f := range []foundryFixture{c.foundryFixture, b} {
			tag := fmt.Sprintf("CLK%d-%d", round, i)
			f.versionReading(t, tag, c.setID)
			loads[i] = loadJob(f.ontology, tag)
		}
		note := c.note(fmt.Sprintf("CLK-r%d", round), fmt.Sprintf("not_%d", round), "1|a", day)
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{note.ID}, c.sync.ID, syncWorker))
		var remove []string
		if previous != "" {
			remove = []string{previous}
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, errs[0] = c.db.PublishConnectorDocuments(ctx, c.publish(fmt.Sprintf("CLK%d", round), []*model.FoundryDocument{note}, nil, remove...),
				c.sync.ID, syncWorker)
		}()
		for i, f := range []foundryFixture{c.foundryFixture, b} {
			go func() {
				defer wg.Done()
				errs[i+1] = f.db.CreateFoundryLoad(ctx, loads[i])
			}()
		}
		wg.Wait()
		require.NoError(t, errs[0], "round %d: the publish", round)
		for _, err := range errs[1:] {
			var me *gomysql.MySQLError
			if errors.As(err, &me) {
				assert.NotEqual(t, uint16(1213), me.Number, "round %d: deadlock", round)
			}
			if err != nil && !errors.Is(err, store.ErrConflict) {
				assert.NoError(t, err, "round %d", round)
			}
		}
		previous = note.ID
	}
}

// DeleteConnector locks as a publish does, the workspace's live ontologies
// before the connector, its set and documents, and takes the connector's
// tables after the connector, as a sync's table write does. Run together
// with a publish into the connector's set, its sync's table write and
// CreateFoundryLoad of versions that read the set, none waits for what
// another holds, so no deadlock ends any; and in whatever order they go,
// nothing of the connector is left live, and every document of it waits for
// the sweep.
func TestConnectorDeleteLockOrder(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	f := newFoundryFixture(t, "DLK")
	ws := f.ontology.WorkspaceID
	b := foundryFixture{db: f.db, ontology: sampleOntology(t, f.db, ws, "DLKB")}
	for i, o := range []foundryFixture{f, b} {
		o.loadReading(t, fmt.Sprintf("DLK-L%d", i), generalSetID(f.ontology.ID))
		o.loaded(t, "w1")
	}
	for round := range 8 {
		// Leave no job live, so both loads can be created.
		_, err := testDB.Exec(`UPDATE foundry_load_jobs SET status='cancelled', active_key=NULL, finished_at=NOW(6) WHERE workspace_id=? AND active_key=1`, ws)
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE foundry_loads SET status='failed', finished_at=NOW(6) WHERE workspace_id=? AND status='writing'`, ws)
		require.NoError(t, err)
		tag := fmt.Sprintf("DLK%d", round)
		c := newConnectorSync(t, f, tag)
		held := c.note(tag+"-held", "not_0", "1|a", day)
		c.published(t, c.publish(tag+"-a", []*model.FoundryDocument{held}, nil))
		table := func(key string) *model.ConnectorTable {
			return &model.ConnectorTable{
				ID: id26("CT-" + key), ConnectorID: c.connector.ID, WorkspaceID: ws, TableName: "notes",
				Columns: json.RawMessage(`[]`), PrimaryKey: json.RawMessage(`["id"]`), ForeignKeys: json.RawMessage(`[]`),
				S3Key: key, RowCount: 1, SyncID: c.sync.ID, SyncedAt: time.Now(),
			}
		}
		require.NoError(t, c.db.UpsertConnectorTable(ctx, table(tag+"-1"), nil, syncWorker))
		loads := make([]*model.FoundryGraphJob, 2)
		for i, o := range []foundryFixture{f, b} {
			vtag := fmt.Sprintf("%s-%d", tag, i)
			o.versionReading(t, vtag, c.setID)
			loads[i] = loadJob(o.ontology, vtag)
		}
		note := c.note(tag+"-new", "not_1", "1|a", day)
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, ws, []string{note.ID}, c.sync.ID, syncWorker))

		var wg sync.WaitGroup
		errs := make([]error, 5)
		run := func(i int, fn func() error) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = fn()
			}()
		}
		run(0, func() error {
			_, err := c.db.PublishConnectorDocuments(ctx, c.publish(tag+"-b", []*model.FoundryDocument{note}, nil, held.ID), c.sync.ID, syncWorker)
			return err
		})
		run(1, func() error { return c.db.UpsertConnectorTable(ctx, table(tag+"-2"), nil, syncWorker) })
		run(2, func() error { return c.db.DeleteConnector(ctx, ws, c.connector.ID) })
		for i, o := range []foundryFixture{f, b} {
			run(3+i, func() error { return o.db.CreateFoundryLoad(ctx, loads[i]) })
		}
		wg.Wait()
		for i, err := range errs {
			var me *gomysql.MySQLError
			if errors.As(err, &me) {
				assert.NotEqual(t, uint16(1213), me.Number, "round %d, writer %d: deadlock", round, i)
			}
		}
		require.NoError(t, errs[2], "round %d: the delete", round)
		for _, err := range errs[:2] {
			if err != nil {
				assert.ErrorIs(t, err, store.ErrNotFound, "round %d: a sync's write after the delete", round)
			}
		}
		for _, err := range errs[3:] {
			if err != nil {
				assert.ErrorIs(t, err, store.ErrConflict, "round %d", round)
			}
		}

		var live, unpurged int
		require.NoError(t, testDB.QueryRow(`SELECT
  (SELECT COUNT(*) FROM foundry_documents WHERE set_id=? AND status='uploaded') +
  (SELECT COUNT(*) FROM foundry_document_sets WHERE id=? AND live_key=1) +
  (SELECT COUNT(*) FROM connector_tables WHERE connector_id=?)`, c.setID, c.setID, c.connector.ID).Scan(&live))
		assert.Zero(t, live, "round %d: a document, the set or a table of the deleted connector is live", round)
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_documents d
WHERE d.set_id=? AND NOT EXISTS (SELECT 1 FROM foundry_document_purges p WHERE p.document_id = d.id)`, c.setID).Scan(&unpurged))
		assert.Zero(t, unpurged, "round %d: a document of the deleted connector has no purge row", round)
		assert.Contains(t, purgeRows(t), note.ID, "round %d: published and deleted, or reserved and refused", round)
		// The next round's sync can be claimed once this one ends.
		require.NoError(t, c.db.FinishConnectorSync(ctx, c.sync.ID, syncWorker,
			model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed, Error: "the connector was deleted"}))
	}
}
