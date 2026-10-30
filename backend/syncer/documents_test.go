package syncer

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/document"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// documentRows are the workspace's document sets, documents and purge rows,
// written as the provider writes them for a connector's sync.
type documentRows struct {
	sets map[string]*model.FoundryDocumentSet
	docs map[string]*model.FoundryDocument
	// purges are the purge rows, by document id, with when each was written.
	purges map[string]time.Time
	// clock is the database's time; a test moves it on to make a reservation late.
	clock time.Time
	// fail, when set, fails the steps it returns an error for: "reserve",
	// "publish" (one that adds or updates), "remove" (one that only
	// removes), "table" (UpsertConnectorTable), "spend"
	// (FoundryDocumentSetSpend), "graph" (SumFoundryGraphJobSpend), "mark"
	// and "clear" (of a set's skip).
	fail func(step string) error
	// ensures counts the set writes; publishes are the publishes written,
	// fanned the documents they fanned out, and owed how many owed refreshes.
	// lives is the publish's set's live count after each publish.
	ensures   int
	publishes []model.ConnectorDocumentsPublish
	fanned    []string
	owed      int
	lives     []int
	// spend is each set's month spent by work no person started, and
	// otherSpend what the kind's other sets of the workspace spent; graphSpend
	// is the workspace's graph jobs' month. skipped are the sets a paused
	// sync skipped; spendReads, marks and clears count the calls. spendGone
	// answers no set, as for a connector deleted since its sets were read.
	spendGone  bool
	spend      map[string]int64
	otherSpend int64
	graphSpend int64
	skipped    map[string]bool
	spendReads int
	marks      int
	clears     int
}

func newDocumentRows() documentRows {
	return documentRows{sets: map[string]*model.FoundryDocumentSet{}, docs: map[string]*model.FoundryDocument{},
		purges: map[string]time.Time{}, clock: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		spend: map[string]int64{}, skipped: map[string]bool{}}
}

func (r *documentRows) failStep(step string) error {
	if r.fail == nil {
		return nil
	}
	return r.fail(step)
}

// live is a set's uploaded documents, by external id.
func (r *documentRows) live(setID string) []*model.FoundryDocument {
	var out []*model.FoundryDocument
	for _, d := range r.docs {
		if d.SetID == setID && d.Status == model.FoundryDocumentUploaded {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b *model.FoundryDocument) int {
		return strings.Compare(a.Source.ExternalID, b.Source.ExternalID)
	})
	return out
}

// setOf is the connector's set of stream, or nil.
func (r *documentRows) setOf(stream string) *model.FoundryDocumentSet {
	for _, set := range r.sets {
		if set.Owner.Stream == stream {
			return set
		}
	}
	return nil
}

func (m *memoryStore) ListFoundryDocumentSets(context.Context, string) ([]*model.FoundryDocumentSet, error) {
	var out []*model.FoundryDocumentSet
	for _, set := range m.sets {
		copied := *set
		copied.Documents = len(m.live(set.ID))
		out = append(out, &copied)
	}
	slices.SortFunc(out, func(a, b *model.FoundryDocumentSet) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (m *memoryStore) EnsureConnectorDocumentSet(_ context.Context, set *model.FoundryDocumentSet, _, workerID string) (string, error) {
	if workerID != m.leaseOwner || m.connectors[set.Owner.ConnectorID] == nil {
		return "", foundrystore.ErrNotFound
	}
	m.ensures++
	if held := m.setOf(set.Owner.Stream); held != nil {
		held.Name, held.Description = set.Name, set.Description
		return held.ID, nil
	}
	copied, owner := *set, *set.Owner
	copied.Owner = &owner
	m.sets[set.ID] = &copied
	return set.ID, nil
}

func (m *memoryStore) ListConnectorDocuments(_ context.Context, _, setID, after string, limit int) ([]model.ConnectorDocument, error) {
	var out []model.ConnectorDocument
	for _, d := range m.live(setID) {
		if d.Source.ExternalID > after && len(out) < limit {
			out = append(out, model.ConnectorDocument{ID: d.ID, ExternalID: d.Source.ExternalID, Version: d.Source.Version, SHA256: d.SHA256,
				At: d.Source.At})
		}
	}
	return out, nil
}

func (m *memoryStore) ReserveConnectorDocuments(_ context.Context, _ string, ids []string, _, workerID string) error {
	if workerID != m.leaseOwner {
		return foundrystore.ErrNotFound
	}
	if err := m.failStep("reserve"); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := m.purges[id]; ok {
			return foundrystore.ErrConflict
		}
	}
	for _, id := range ids {
		m.purges[id] = m.clock
	}
	return nil
}

// PublishConnectorDocuments checks everything before it writes anything, so
// a refused publish writes nothing, as the provider's transaction does.
func (m *memoryStore) PublishConnectorDocuments(_ context.Context, p model.ConnectorDocumentsPublish, _, workerID string) (
	[]model.FoundryDocumentRead, error) {
	if workerID != m.leaseOwner {
		return nil, foundrystore.ErrNotFound
	}
	step := "publish"
	if len(p.Add)+len(p.Update) == 0 {
		step = "remove"
	}
	if err := m.failStep(step); err != nil {
		return nil, err
	}
	if set := m.sets[p.SetID]; set == nil || set.Owner.ConnectorID != p.ConnectorID || m.connectors[p.ConnectorID] == nil {
		return nil, foundrystore.ErrNotFound
	}
	removed := map[string]bool{}
	for _, id := range p.Remove {
		if d := m.docs[id]; d != nil && d.SetID == p.SetID && d.Status == model.FoundryDocumentUploaded {
			removed[id] = true
		}
	}
	taken := map[string]bool{}
	for _, d := range m.live(p.SetID) {
		taken[d.Source.ExternalID] = !removed[d.ID]
	}
	for _, d := range p.Add {
		if at, ok := m.purges[d.ID]; !ok || !at.After(m.clock.Add(-p.Grace/2)) {
			return nil, foundrystore.ErrReservationExpired
		}
		if taken[d.Source.ExternalID] {
			return nil, foundrystore.ErrConflict
		}
		taken[d.Source.ExternalID] = true
	}
	for id := range removed {
		m.docs[id].Status = model.FoundryDocumentDeleted
		m.purges[id] = m.clock
	}
	for _, d := range p.Add {
		delete(m.purges, d.ID)
		copied, source := *d, *d.Source
		copied.Source = &source
		m.docs[d.ID] = &copied
		m.fanned = append(m.fanned, d.ID)
	}
	for _, u := range p.Update {
		if d := m.docs[u.ID]; d != nil && d.Status == model.FoundryDocumentUploaded {
			d.Name, d.Source.Version, d.Source.At, d.Source.URL = u.Name, u.Version, u.At, u.URL
		}
	}
	if len(removed) > 0 {
		m.owed++
	}
	m.publishes = append(m.publishes, p)
	m.lives = append(m.lives, len(m.live(p.SetID)))
	return nil, nil
}

// FoundryDocumentSetSpend answers for the asked sets that a connector owns,
// as the provider does; the fake kind is the only kind.
func (m *memoryStore) FoundryDocumentSetSpend(_ context.Context, _ string, setIDs []string, _ time.Time) (map[string]model.FoundryDocumentSetSpend, error) {
	m.spendReads++
	if err := m.failStep("spend"); err != nil {
		return nil, err
	}
	workspace := m.otherSpend
	for _, usd := range m.spend {
		workspace += usd
	}
	out := map[string]model.FoundryDocumentSetSpend{}
	for _, setID := range setIDs {
		if set := m.sets[setID]; set != nil && m.connectors[set.Owner.ConnectorID] != nil && !m.spendGone {
			out[setID] = model.FoundryDocumentSetSpend{SetID: setID, ConnectorKind: "fake", USDMicros: m.spend[setID],
				WorkspaceUSDMicros: workspace, Skipped: m.skipped[setID]}
		}
	}
	return out, nil
}

func (m *memoryStore) MarkFoundryDocumentSetSkipped(_ context.Context, _, setID string, _ time.Time) error {
	if m.sets[setID] == nil {
		return foundrystore.ErrNotFound
	}
	if err := m.failStep("mark"); err != nil {
		return err
	}
	m.marks++
	m.skipped[setID] = true
	return nil
}

func (m *memoryStore) ClearFoundryDocumentSetSkipped(_ context.Context, _, setID, _, workerID string) error {
	if workerID != m.leaseOwner || m.sets[setID] == nil {
		return foundrystore.ErrNotFound
	}
	if err := m.failStep("clear"); err != nil {
		return err
	}
	m.clears++
	delete(m.skipped, setID)
	return nil
}

func (m *memoryStore) SumFoundryGraphJobSpend(context.Context, string, time.Time) (int64, error) {
	return m.graphSpend, m.failStep("graph")
}

// memoryObjects is the documents bucket: PutOnce writes a key once. fail,
// when set, runs before each put and fails those it returns an error for.
type memoryObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	fail    func(key string) error
}

func (o *memoryObjects) PutOnce(_ context.Context, key, _ string, body io.ReadSeeker) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		if err := o.fail(key); err != nil {
			return err
		}
	}
	o.puts++
	if _, ok := o.objects[key]; ok {
		return nil
	}
	b, err := io.ReadAll(body)
	o.objects[key] = b
	return err
}

// held checks the documents' invariants, then returns the text of each live
// document of stream by external id. The invariants: one live document per
// row; each one's original is its bytes, and its reservation is claimed;
// every object in the bucket has a row that owns it, a live document or a
// purge row.
func (f fixture) held(t *testing.T, stream string) map[string]string {
	t.Helper()
	owned := map[string]bool{}
	rows := map[string]bool{}
	for _, d := range f.store.docs {
		if d.Status != model.FoundryDocumentUploaded {
			continue
		}
		row := d.SetID + "/" + d.Source.ExternalID
		body, stored := f.documents.objects[d.S3Key]
		sum := sha256.Sum256(body)
		_, reserved := f.store.purges[d.ID]
		switch {
		case rows[row]:
			t.Fatalf("two live documents of %s", row)
		case !stored || hex.EncodeToString(sum[:]) != d.SHA256 || int64(len(body)) != d.SizeBytes:
			t.Fatalf("document %s's original is %q", d.ID, body)
		case reserved:
			t.Fatalf("live document %s has a purge row", d.ID)
		}
		rows[row], owned[d.S3Key] = true, true
	}
	for key := range f.documents.objects {
		if _, purged := f.store.purges[path.Base(path.Dir(key))]; !owned[key] && !purged {
			t.Fatalf("object %s has no row that owns it", key)
		}
	}
	out := map[string]string{}
	if set := f.store.setOf(stream); set != nil {
		for _, d := range f.store.live(set.ID) {
			out[d.Source.ExternalID] = string(f.documents.objects[d.S3Key])
		}
	}
	return out
}

// liveID is the id of the live document of stream's row externalID.
func (f fixture) liveID(stream, externalID string) string {
	for _, d := range f.store.live(f.store.setOf(stream).ID) {
		if d.Source.ExternalID == externalID {
			return d.ID
		}
	}
	return ""
}

// note is one row of a noteKind table and the text of its documents.
type note struct {
	id, version, text string
	at                time.Time
	// drop takes the document back with Drop once Want says yes.
	drop bool
}

func (n note) row() map[string]any { return map[string]any{"id": n.id, "version": n.version} }

// day is a note's time at the source: day d of October 2026.
func day(d int) time.Time { return time.Date(2026, time.October, d, 9, 0, 0, 0, time.UTC) }

var (
	notesStream       = connector.Stream{Name: "notes", Table: "notes", Title: "Notes", Description: "Meeting notes.", Format: connector.Markdown}
	transcriptsStream = connector.Stream{Name: "transcripts", Table: "notes", Title: "Transcripts", Format: connector.PlainText}
)

// noteKind is a changeKind whose tables carry documents. A read writes each
// note's row, then Puts each of its table's streams' documents Want says
// yes to, "<stream>: <text>" (empty for an empty text), in the order of its
// notes, which tests keep newest first.
type noteKind struct {
	changeKind
	streams []connector.Stream
	notes   map[string][]note
	// puts are "<stream>/<id>" of every Put; held what Held said of each
	// note before its Want.
	puts []string
	held []string
	// skipWant Puts without asking; swallow ignores Put's error; extra runs
	// before each read offers anything.
	skipWant bool
	swallow  bool
	extra    func(docs connector.DocumentWriter)
}

func (k *noteKind) Streams(connector.Source) []connector.Stream { return k.streams }

func (k *noteKind) Sync(ctx context.Context, src connector.Source, table connector.Table, rows connector.RowWriter) (connector.Table, error) {
	notes, ok := k.notes[table.Name]
	if !ok {
		return k.changeKind.Sync(ctx, src, table, rows)
	}
	k.synced = append(k.synced, table.Name)
	return table, k.offer(ctx, table.Name, notes, rows.Write, nil)
}

func (k *noteKind) SyncDocuments(ctx context.Context, _ connector.Source, table connector.Table, rows connector.RowWriter,
	docs connector.DocumentWriter) (connector.Table, error) {
	k.synced = append(k.synced, table.Name)
	if k.syncErr != nil {
		return table, k.syncErr
	}
	return table, k.offer(ctx, table.Name, k.notes[table.Name], rows.Write, docs)
}

// offer writes notes' rows with write and Puts their documents to docs, if any.
func (k *noteKind) offer(ctx context.Context, table string, notes []note, write func(map[string]any) error, docs connector.DocumentWriter) error {
	if k.extra != nil && docs != nil {
		k.extra(docs)
	}
	for _, n := range notes {
		if err := write(n.row()); err != nil {
			return err
		}
		for _, s := range k.streams {
			if s.Table != table || docs == nil {
				continue
			}
			version, ok := docs.Held(s.Name, n.id)
			k.held = append(k.held, fmt.Sprintf("%s/%s=%s,%t", s.Name, n.id, version, ok))
			if !k.skipWant && !docs.Want(s.Name, n.id, n.version) {
				continue
			}
			if n.drop {
				if err := docs.Drop(s.Name, n.id); err != nil && !k.swallow {
					return err
				}
				continue
			}
			text := ""
			if n.text != "" {
				text = s.Name + ": " + n.text
			}
			k.puts = append(k.puts, s.Name+"/"+n.id)
			err := docs.Put(ctx, s.Name, connector.Document{ExternalID: n.id, Version: n.version, At: n.at, Title: "Note " + n.id,
				URL: "https://notes.example/" + n.id, Text: []byte(text)})
			if err != nil && !k.swallow {
				return err
			}
		}
	}
	return nil
}

// upsert is a change read's script that upserts notes into table, with their documents.
func (k *noteKind) upsert(table string, notes ...note) func(connector.ChangeWriter) error {
	return func(w connector.ChangeWriter) error {
		return k.offer(context.Background(), table, notes, func(row map[string]any) error { return w.Upsert(table, row) }, w)
	}
}

// newNoteKind reads notes, keyed by id and carrying the notes stream, and
// people, which carries none.
func newNoteKind(capture connector.Capture, notes ...note) *noteKind {
	return &noteKind{
		changeKind: changeKind{capture: capture, fakeKind: fakeKind{
			discovery: connector.Discovery{Tables: []connector.Table{{Name: "notes", PrimaryKey: []string{"id"}}, {Name: "people", PrimaryKey: []string{"id"}}}},
			rows:      map[string][]string{"people": {`{"id":"p1"}`}},
		}},
		streams: []connector.Stream{notesStream},
		notes:   map[string][]note{"notes": notes},
	}
}

func newNoteFixture(t *testing.T, kind connector.Kind) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "fake", `{}`, plan{code: "pro"})
	f.syncer.Catalog = connector.NewCatalogOf(kind)
	return f
}

// run runs a sync of id, which must succeed.
func (f fixture) run(t *testing.T, id string) Result {
	t.Helper()
	res, err := f.syncer.Run(context.Background(), newSync(id))
	if err != nil {
		t.Fatalf("sync %s: %v", id, err)
	}
	return res
}

func documentsCounted(outcome string) float64 {
	return testutil.ToFloat64(syncDocuments.WithLabelValues("fake", outcome))
}

var (
	n1 = note{id: "n1", version: "1|a", text: "first", at: day(1)}
	n2 = note{id: "n2", version: "1|a", text: "second", at: day(2)}
	n3 = note{id: "n3", version: "1|a", text: "third", at: day(3)}
	n4 = note{id: "n4", version: "1|a", text: "fourth", at: day(4)}
)

// resent is n sent again unchanged, arriving at.
func resent(n note, at time.Time) note {
	n.at = at
	return n
}

// newWindowKind is a noteKind whose notes stream keeps its newest n.
func newWindowKind(capture connector.Capture, n int, notes ...note) *noteKind {
	kind := newNoteKind(capture, notes...)
	kind.streams[0].Keep = connector.KeepNewest(n)
	return kind
}

// within fails the test if a publish left its set over n documents.
func (f fixture) within(t *testing.T, n int) {
	t.Helper()
	for i, live := range f.store.lives {
		if live > n {
			t.Fatalf("publish %d left %d documents, over %d", i, live, n)
		}
	}
}

// edited is n at a new version, with text when it is not empty.
func edited(n note, text string) note {
	n.version = "2|b"
	if text != "" {
		n.text = text
	}
	return n
}

// TestSyncerDocuments covers each case of a full read's documents.
func TestSyncerDocuments(t *testing.T) {
	t.Run("writes a new row's document into the stream's set and fans it out", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n2, n1)
		f := newNoteFixture(t, kind)
		added := documentsCounted("added")
		f.run(t, "sync-1")
		set := f.store.setOf("notes")
		if set == nil || set.Name != "Notes" || set.Description != "Meeting notes." || set.Owner.ConnectorID != "conn-1" || set.CreatedByUserID != nil {
			t.Fatalf("set = %+v", set)
		}
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "notes: first" || got["n2"] != "notes: second" {
			t.Fatalf("documents = %v", got)
		}
		d := f.store.docs[f.liveID("notes", "n2")]
		if d.Name != "Note n2" || d.MediaType != model.FoundryMediaMarkdown || d.Source.Version != "1|a" || !d.Source.At.Equal(day(2)) ||
			d.Source.URL == nil || *d.Source.URL != "https://notes.example/n2" || d.ClientToken != nil || d.S3Key != document.Key("ws-1", d.ID) {
			t.Fatalf("document = %+v, source = %+v", d, d.Source)
		}
		if len(f.store.fanned) != 2 || len(f.store.publishes) != 1 || f.store.owed != 0 || documentsCounted("added")-added != 2 {
			t.Fatalf("fanned = %v, publishes = %d, owed = %d", f.store.fanned, len(f.store.publishes), f.store.owed)
		}
		p := f.store.publishes[0]
		if p.Grace != time.Hour || p.Fan.RequestedByUserID != nil || !p.Fan.CanWrite("any") ||
			p.Fan.Budget.CapFor(nil) != 40e6 || p.Fan.SystemOnly || f.store.spendReads != 0 {
			t.Fatalf("publish = %+v", p)
		}
	})
	t.Run("keeps an unchanged document: no fetch, put, write or fan-out", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n2, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		puts, objects, kept := len(kind.puts), f.documents.puts, documentsCounted("kept")
		kind.extra = func(docs connector.DocumentWriter) {
			if version, ok := docs.Held("transcripts", "n1"); ok || version != "" {
				t.Errorf("a stream that is off holds %q", version)
			}
		}
		f.run(t, "sync-2")
		if len(kind.puts) != puts || f.documents.puts != objects || f.store.ensures != 1 || len(f.store.publishes) != 1 ||
			len(f.store.fanned) != 2 || len(f.store.purges) != 0 || documentsCounted("kept")-kept != 2 {
			t.Fatalf("puts = %v, objects = %d, ensures = %d, publishes = %d, fanned = %v",
				kind.puts, f.documents.puts, f.store.ensures, len(f.store.publishes), f.store.fanned)
		}
		if !slices.Contains(kind.held, "notes/n1=1|a,true") || !slices.Contains(kind.held, "notes/n1=,false") {
			t.Fatalf("held = %v", kind.held)
		}
	})
	t.Run("takes the first offer of an id offered twice", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1, edited(n1, "again"))
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		if got := f.held(t, "notes"); len(got) != 1 || got["n1"] != "notes: first" || !slices.Equal(kind.puts, []string{"notes/n1"}) {
			t.Fatalf("documents = %v, puts = %v", got, kind.puts)
		}
	})
	t.Run("changes a new version with the same bytes in place", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		id, objects := f.liveID("notes", "n1"), f.documents.puts
		kind.notes["notes"] = []note{edited(n1, "")}
		f.run(t, "sync-2")
		p := f.store.publishes[1]
		if f.liveID("notes", "n1") != id || f.store.docs[id].Source.Version != "2|b" || f.documents.puts != objects ||
			len(f.store.fanned) != 1 || len(p.Update) != 1 || len(p.Add)+len(p.Remove) != 0 || f.store.owed != 0 {
			t.Fatalf("document = %+v, publish = %+v, fanned = %v", f.store.docs[id], p, f.store.fanned)
		}
	})
	t.Run("replaces a new version with new bytes in one publish", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		old := f.liveID("notes", "n1")
		kind.notes["notes"] = []note{edited(n1, "changed")}
		f.run(t, "sync-2")
		p := f.store.publishes[1]
		if got := f.held(t, "notes"); got["n1"] != "notes: changed" || f.liveID("notes", "n1") == old {
			t.Fatalf("documents = %v", got)
		}
		if _, purged := f.store.purges[old]; !purged || len(p.Add) != 1 || !slices.Equal(p.Remove, []string{old}) ||
			len(f.store.fanned) != 2 || f.store.owed != 1 {
			t.Fatalf("publish = %+v, purges = %v, owed = %d", p, f.store.purges, f.store.owed)
		}
	})
	t.Run("keeps the newest the cap allows and removes what a full read leaves out", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n3, n2, n1)
		f := newNoteFixture(t, kind)
		f.syncer.MaxPerStream = 2
		overCap := documentsCounted("over_cap")
		f.run(t, "sync-1")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" || documentsCounted("over_cap")-overCap != 1 {
			t.Fatalf("documents = %v", got)
		}
		// A newer note pushes the oldest kept one past the cap.
		kind.notes["notes"] = []note{n4, n3, n2, n1}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n4"] == "" || got["n3"] == "" || f.store.owed != 1 {
			t.Fatalf("documents = %v, owed = %d", got, f.store.owed)
		}
		// A row gone from the source is not offered.
		kind.notes["notes"] = []note{n4, n2, n1}
		f.run(t, "sync-3")
		if got := f.held(t, "notes"); len(got) != 2 || got["n4"] == "" || got["n2"] == "" || f.store.owed != 2 {
			t.Fatalf("documents = %v, owed = %d", got, f.store.owed)
		}
	})
	t.Run("removes a held document the kind drops, and holds none it drops first", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n2, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		old, dropped := f.liveID("notes", "n1"), documentsCounted("dropped")
		gone := edited(n1, "")
		gone.drop = true
		fresh := n3
		fresh.drop = true
		kind.notes["notes"] = []note{n2, gone, fresh}
		f.run(t, "sync-2")
		p := f.store.publishes[len(f.store.publishes)-1]
		if got := f.held(t, "notes"); len(got) != 1 || got["n2"] != "notes: second" || !slices.Equal(p.Remove, []string{old}) ||
			len(p.Add)+len(p.Update) != 0 || documentsCounted("dropped")-dropped != 2 {
			t.Fatalf("documents = %v, publish = %+v", got, p)
		}
		kind.notes["notes"] = []note{n2, n1}
		f.run(t, "sync-3")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "notes: first" {
			t.Fatalf("documents after the source's document is back = %v", got)
		}
	})
	t.Run("refuses a drop Want did not say yes to", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		kept := n1
		kept.drop = true
		kind.notes["notes"] = []note{kept}
		kind.skipWant = true
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err == nil || !strings.Contains(err.Error(), "Want did not say yes to it") {
			t.Fatalf("sync = %v", err)
		}
		if got := f.held(t, "notes"); len(got) != 1 {
			t.Fatalf("documents = %v", got)
		}
	})
	t.Run("empties the set of a stream turned off or of a table not synced, and keeps the set", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n2, n1)
		kind.streams = []connector.Stream{notesStream, transcriptsStream}
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		transcripts := f.store.setOf("transcripts").ID
		if got := f.held(t, "transcripts"); len(got) != 2 || got["n1"] != "transcripts: first" {
			t.Fatalf("transcripts = %v", got)
		}
		kind.streams = []connector.Stream{notesStream}
		f.run(t, "sync-2")
		if got := f.held(t, "transcripts"); len(got) != 0 || len(f.held(t, "notes")) != 2 || len(f.store.sets) != 2 || f.store.owed != 1 {
			t.Fatalf("transcripts = %v, sets = %d", got, len(f.store.sets))
		}
		f.store.connectors["conn-1"].Config = []byte(`{"tables":["people"]}`)
		f.run(t, "sync-3")
		if got := f.held(t, "notes"); len(got) != 0 || len(f.store.sets) != 2 {
			t.Fatalf("notes = %v", got)
		}
		f.store.connectors["conn-1"].Config = []byte(`{}`)
		kind.streams = []connector.Stream{notesStream, transcriptsStream}
		f.run(t, "sync-4")
		if got := f.held(t, "transcripts"); len(got) != 2 || f.store.setOf("transcripts").ID != transcripts {
			t.Fatalf("transcripts = %v", got)
		}
	})
	t.Run("renames the set when the stream's title changes", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		renamed := notesStream
		renamed.Title, renamed.Description = "Meeting notes", "Every meeting's notes."
		kind.streams = []connector.Stream{renamed}
		f.run(t, "sync-2")
		if set := f.store.setOf("notes"); len(f.store.sets) != 1 || set.Name != "Meeting notes" || f.store.ensures != 2 {
			t.Fatalf("sets = %v", f.store.sets)
		}
	})
	t.Run("writes nothing once the lease is lost", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.store.leaseOwner = "worker-2"
		if _, err := f.syncer.Run(context.Background(), f.sync); err == nil || failure(t, err).Permanent {
			t.Fatalf("Run = %v", err)
		}
		if len(f.store.sets)+len(f.store.docs)+len(f.store.purges)+f.documents.puts != 0 {
			t.Fatalf("sets = %v, documents = %v, purges = %v, puts = %d", f.store.sets, f.store.docs, f.store.purges, f.documents.puts)
		}
		f.store.leaseOwner = "worker-1"
		f.run(t, "sync-1")
		kind.notes["notes"] = []note{n2, edited(n1, "changed")}
		f.store.leaseOwner = "worker-2"
		docs, puts := len(f.store.docs), f.documents.puts
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err == nil {
			t.Fatal("the sync succeeded")
		}
		if len(f.store.docs) != docs || len(f.store.purges) != 0 || f.documents.puts != puts {
			t.Fatalf("documents = %d, purges = %v, puts = %d", len(f.store.docs), f.store.purges, f.documents.puts)
		}
	})
	t.Run("cuts a text over the cap at a line, with a marker", func(t *testing.T) {
		long := note{id: "n1", version: "1", at: day(1), text: strings.Repeat("a line of text\n", 100)}
		kind := newNoteKind(connector.CaptureFull, long)
		f := newNoteFixture(t, kind)
		f.syncer.MaxDocumentBytes = 200
		truncated := documentsCounted("truncated")
		f.run(t, "sync-1")
		got := f.held(t, "notes")["n1"]
		if len(got) > 200 || !strings.HasSuffix(got, "a line of text\n[Truncated: the source holds 1507 bytes]\n") ||
			documentsCounted("truncated")-truncated != 1 {
			t.Fatalf("text = %q", got)
		}
	})
	t.Run("reads a live index of more than one page", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureFull, n1)
		f := newNoteFixture(t, kind)
		f.syncer.MaxRows, f.syncer.MaxPerStream = 2000, 2000
		f.run(t, "sync-1")
		set := f.store.setOf("notes").ID
		for i := range indexPage {
			n := note{id: fmt.Sprintf("m%04d", i), version: "1", text: "x", at: day(1)}
			kind.notes["notes"] = append(kind.notes["notes"], n)
			id := fmt.Sprintf("doc-%04d", i)
			f.store.docs[id] = &model.FoundryDocument{ID: id, SetID: set, Status: model.FoundryDocumentUploaded, S3Key: document.Key("ws-1", id),
				Source: &model.FoundryDocumentSource{ExternalID: n.id, Version: n.version}}
		}
		puts := len(kind.puts)
		f.run(t, "sync-2")
		if len(kind.puts) != puts || len(f.store.live(set)) != indexPage+1 || len(f.store.publishes) != 1 {
			t.Fatalf("puts = %d, live = %d, publishes = %d", len(kind.puts)-puts, len(f.store.live(set)), len(f.store.publishes))
		}
	})
	t.Run("publishes a batch every hundred documents", func(t *testing.T) {
		var notes []note
		for i := range 250 {
			notes = append(notes, note{id: fmt.Sprintf("n%03d", i), version: "1", text: "x", at: day(1)})
		}
		f := newNoteFixture(t, newNoteKind(connector.CaptureFull, notes...))
		f.syncer.MaxRows = 1000
		f.run(t, "sync-1")
		if got := f.held(t, "notes"); len(got) != 250 || len(f.store.publishes) != 3 || len(f.store.publishes[0].Add) != 100 {
			t.Fatalf("documents = %d, publishes = %d", len(got), len(f.store.publishes))
		}
	})
}

// TestSyncerDocumentChanges covers each case of a change read's documents.
func TestSyncerDocumentChanges(t *testing.T) {
	// first syncs n2 and n1 in full, so the next sync reads changes.
	first := func(t *testing.T) (*noteKind, fixture) {
		kind := newNoteKind(connector.CaptureCursor, n2, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		return kind, f
	}
	t.Run("adds, keeps, changes in place and replaces, taking an id once", func(t *testing.T) {
		kind, f := first(t)
		old := f.liveID("notes", "n2")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3, edited(n2, "changed"), n1, edited(n3, "again"))}
		res := f.run(t, "sync-2")
		if res.Mode != model.ConnectorSyncChanges {
			t.Fatalf("mode = %s", res.Mode)
		}
		got := f.held(t, "notes")
		if len(got) != 3 || got["n3"] != "notes: third" || got["n2"] != "notes: changed" || f.liveID("notes", "n2") == old ||
			!slices.Equal(kind.puts[2:], []string{"notes/n3", "notes/n2"}) || f.store.owed != 1 || len(f.store.fanned) != 4 {
			t.Fatalf("documents = %v, puts = %v, owed = %d", got, kind.puts, f.store.owed)
		}
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", edited(n1, ""))}
		f.run(t, "sync-3")
		if p := f.store.publishes[len(f.store.publishes)-1]; len(p.Update) != 1 || len(f.store.fanned) != 4 {
			t.Fatalf("publish = %+v", p)
		}
	})
	t.Run("removes the document of a row a change read deletes", func(t *testing.T) {
		kind, f := first(t)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			return w.Delete("notes", map[string]any{"id": "n1"})
		}}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 1 || got["n2"] == "" || f.store.owed != 1 {
			t.Fatalf("documents = %v", got)
		}
	})
	t.Run("removes a document the kind drops in a change read, its row kept", func(t *testing.T) {
		kind, f := first(t)
		gone := edited(n1, "")
		gone.drop = true
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", gone)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 1 || got["n2"] == "" {
			t.Fatalf("documents = %v", got)
		}
	})
	t.Run("makes no write when nothing changed", func(t *testing.T) {
		_, f := first(t)
		publishes, puts := len(f.store.publishes), f.documents.puts
		if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(f.store.publishes) != publishes || f.documents.puts != puts {
			t.Fatalf("mode = %s, publishes = %d", res.Mode, len(f.store.publishes))
		}
	})
	// The zero Keep, Granola's: a new document is never refused.
	t.Run("goes over the cap, and the next full read trims it", func(t *testing.T) {
		kind, f := first(t)
		f.syncer.MaxPerStream = 2
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 3 {
			t.Fatalf("documents = %v", got)
		}
		kind.notes["notes"] = []note{n3, n2, n1}
		full := newSync("sync-3")
		full.Mode = model.ConnectorSyncFull
		if _, err := f.syncer.Run(context.Background(), full); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" {
			t.Fatalf("documents = %v", got)
		}
	})
	t.Run("reads a table it resets in full, putting each document once", func(t *testing.T) {
		kind, f := first(t)
		kind.notes["notes"] = []note{n3, n2, n1}
		upsert := kind.upsert("notes", n3)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			w.Reset("notes", "the cursor column was dropped")
			return upsert(w)
		}}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 3 || !slices.Equal(kind.puts[2:], []string{"notes/n3"}) {
			t.Fatalf("documents = %v, puts = %v", got, kind.puts)
		}
	})
	t.Run("empties the set of a stream turned off", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureCursor, n1)
		kind.streams = []connector.Stream{notesStream, transcriptsStream}
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		kind.streams = []connector.Stream{notesStream}
		if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(f.held(t, "transcripts")) != 0 || len(f.held(t, "notes")) != 1 {
			t.Fatalf("mode = %s, transcripts = %v", res.Mode, f.held(t, "transcripts"))
		}
	})
	t.Run("writes nothing once the lease is lost", func(t *testing.T) {
		kind, f := first(t)
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3)}
		f.store.leaseOwner = "worker-2"
		docs, puts := len(f.store.docs), f.documents.puts
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err == nil {
			t.Fatal("the sync succeeded")
		}
		if len(f.store.docs) != docs || len(f.store.purges) != 0 || f.documents.puts != puts {
			t.Fatalf("documents = %d, purges = %v, puts = %d", len(f.store.docs), f.store.purges, f.documents.puts)
		}
	})
}

// TestSyncerDocumentCrashes stops an attempt at each step, then retries it:
// the retry converges on what one clean attempt writes, with one live
// document per row, and every object a row owns.
func TestSyncerDocumentCrashes(t *testing.T) {
	for _, c := range []struct {
		name  string
		keep  connector.Keep
		steps []string
		// source is the source after the first sync of n3, n2 and n1, and
		// change its change read; want the documents a clean sync keeps.
		source []note
		change func(k *noteKind) func(connector.ChangeWriter) error
		want   map[string]string
	}{
		{
			// n4 is new, n3 replaced and n2 gone.
			steps:  []string{"reserve", "put", "publish", "late", "remove", "table", "prune"},
			source: []note{n4, edited(n3, "changed"), n1},
			change: func(k *noteKind) func(connector.ChangeWriter) error {
				upsert := k.upsert("notes", n4, edited(n3, "changed"))
				return func(w connector.ChangeWriter) error {
					if err := w.Delete("notes", map[string]any{"id": "n2"}); err != nil {
						return err
					}
					return upsert(w)
				}
			},
			want: map[string]string{"n4": "notes: fourth", "n3": "notes: changed", "n1": "notes: first"},
		},
		{
			// n4 is new and n3 replaced, which pushes n1 out of a window of
			// 3; no publish only removes.
			name: "a window, ", keep: connector.KeepNewest(3),
			steps:  []string{"reserve", "put", "publish", "late", "table", "prune"},
			source: []note{n4, edited(n3, "changed"), n2, n1},
			change: func(k *noteKind) func(connector.ChangeWriter) error {
				return k.upsert("notes", n4, edited(n3, "changed"))
			},
			want: map[string]string{"n4": "notes: fourth", "n3": "notes: changed", "n2": "notes: second"},
		},
	} {
		for _, mode := range []model.ConnectorSyncMode{model.ConnectorSyncFull, model.ConnectorSyncChanges} {
			for _, step := range c.steps {
				t.Run(fmt.Sprintf("%s%s read, %s", c.name, mode, step), func(t *testing.T) {
					kind := newNoteKind(connector.CaptureCursor, n3, n2, n1)
					kind.streams[0].Keep = c.keep
					f := newNoteFixture(t, kind)
					f.run(t, "sync-1")
					kind.notes["notes"] = c.source
					read := c.change(kind)
					kind.changes = []func(connector.ChangeWriter) error{read, read}
					failed := false
					once := func(want string) func(string) error {
						return func(got string) error {
							if got != want || failed {
								return nil
							}
							failed = true
							return errors.New("connection reset")
						}
					}
					switch step {
					case "put":
						fail := once("put")
						f.documents.fail = func(string) error { return fail("put") }
					case "late":
						f.documents.fail = func(string) error {
							if !failed {
								failed = true
								f.store.clock = f.store.clock.Add(31 * time.Minute)
							}
							return nil
						}
					case "prune":
						f.store.failPrune, failed = true, true
					default:
						f.store.fail = once(step)
					}
					sync := newSync("sync-2")
					sync.Mode = mode
					if _, err := f.syncer.Run(context.Background(), sync); err == nil || failure(t, err).Permanent || !failed {
						t.Fatalf("Run = %v, failed = %t", err, failed)
					}
					sync.Attempts = 2
					res, err := f.syncer.Run(context.Background(), sync)
					if err != nil || res.Mode != mode {
						t.Fatalf("retry = (%+v, %v)", res, err)
					}
					if got := f.held(t, "notes"); !maps.Equal(got, c.want) {
						t.Fatalf("documents = %v", got)
					}
					if n := c.keep.Newest(); n > 0 {
						f.within(t, n)
					}
				})
			}
		}
	}
}

// TestSyncerDocumentErrors covers kind bugs, which fail the sync for good.
func TestSyncerDocumentErrors(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(k *noteKind)
		want  string
	}{
		{"an id with a space", func(k *noteKind) { k.notes["notes"] = []note{{id: "n 1", version: "1", text: "x", at: day(1)}} },
			`stream notes document "n 1": its id must be 1 to 255 printable ASCII characters, with no space`},
		{"a version too long", func(k *noteKind) {
			k.notes["notes"] = []note{{id: "n1", version: strings.Repeat("v", 129), text: "x", at: day(1)}}
		},
			`stream notes document "n1": its version must be 1 to 128 printable ASCII characters`},
		{"no time", func(k *noteKind) { k.notes["notes"] = []note{{id: "n1", version: "1", text: "x"}} },
			`stream notes document "n1": its time is missing or out of range`},
		{"no text", func(k *noteKind) { k.notes["notes"] = []note{{id: "n1", version: "1", at: day(1)}} },
			`stream notes document "n1": its text is empty or not UTF-8`},
		{"a Put without a yes", func(k *noteKind) { k.skipWant = true },
			`stream notes document "n1": Want did not say yes to it`},
		{"a Put without a yes the kind ignores", func(k *noteKind) { k.skipWant, k.swallow = true, true },
			`stream notes document "n1": Want did not say yes to it`},
		{"a stream that is off", func(k *noteKind) {
			k.extra = func(docs connector.DocumentWriter) { docs.Want("transcripts", "n1", "1") }
		}, "stream transcripts: the stream is off, or its table is not synced"},
		{"a stream whose table is not being read", func(k *noteKind) {
			k.streams = append(k.streams, connector.Stream{Name: "bios", Table: "people", Title: "Bios", Format: connector.PlainText})
			k.extra = func(docs connector.DocumentWriter) { docs.Want("bios", "p1", "1") }
		}, "stream bios: its table people is not being read"},
		{"a table keyed by two columns", func(k *noteKind) { k.discovery.Tables[0].PrimaryKey = []string{"id", "version"} },
			"stream notes: its table notes is keyed by 2 columns, not one"},
		{"a stream it can't keep", func(k *noteKind) {
			k.streams = []connector.Stream{{Name: "Notes", Table: "notes", Title: "Notes", Format: connector.Markdown}}
		},
			"stream Notes: its name must match ^[a-z][a-z0-9_]{0,31}$"},
		{"a window over the stream cap", func(k *noteKind) { k.streams[0].Keep = connector.KeepNewest(1001) },
			"stream notes: it keeps its newest 1001, more than foundry.documents.max_per_stream (1000)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			kind := newNoteKind(connector.CaptureFull, n1)
			c.setup(kind)
			f := newNoteFixture(t, kind)
			_, err := f.syncer.Run(context.Background(), f.sync)
			if fl := failure(t, err); !fl.Permanent || fl.Message != "a document this connection read can't be stored: "+c.want {
				t.Fatalf("failure = %+v", fl)
			}
			if len(f.store.docs) != 0 || f.store.tables["notes"] != nil {
				t.Fatalf("documents = %v, tables = %v", f.store.docs, f.store.tables)
			}
		})
	}
	t.Run("refuses to write documents without the syncer's limits", func(t *testing.T) {
		f := newNoteFixture(t, newNoteKind(connector.CaptureFull, n1))
		f.syncer.MaxPerStream = 0
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || len(f.store.sets) != 0 {
			t.Fatalf("failure = %+v, sets = %v", fl, f.store.sets)
		}
	})
	t.Run("in a change read", func(t *testing.T) {
		kind := newNoteKind(connector.CaptureCursor, n1)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			return w.Put(context.Background(), "notes", connector.Document{ExternalID: "n9"})
		}}
		_, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if fl := failure(t, err); !fl.Permanent || !strings.HasSuffix(fl.Message, `stream notes document "n9": Want did not say yes to it`) {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

// TestSyncerDocumentWindow covers KeepNewest: the set never holds more than
// its newest n by time, at any publish of any read.
func TestSyncerDocumentWindow(t *testing.T) {
	// first syncs notes in full into a window of n, so the next sync reads changes.
	first := func(t *testing.T, n int, notes ...note) (*noteKind, fixture) {
		t.Helper()
		kind := newWindowKind(connector.CaptureCursor, n, notes...)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		return kind, f
	}
	last := func(f fixture) model.ConnectorDocumentsPublish { return f.store.publishes[len(f.store.publishes)-1] }
	t.Run("trims the oldest in the publish that adds past n, and owes", func(t *testing.T) {
		kind, f := first(t, 3, n3, n2, n1)
		oldest := f.liveID("notes", "n1")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n4)}
		res := f.run(t, "sync-2")
		if got := f.held(t, "notes"); res.Mode != model.ConnectorSyncChanges || len(got) != 3 || got["n1"] != "" || got["n4"] != "notes: fourth" {
			t.Fatalf("mode = %s, documents = %v", res.Mode, got)
		}
		if p := last(f); len(f.store.publishes) != 2 || len(p.Add) != 1 || !slices.Equal(p.Remove, []string{oldest}) || f.store.owed != 1 {
			t.Fatalf("publishes = %d, last = %+v, owed = %d", len(f.store.publishes), p, f.store.owed)
		}
		f.within(t, 3)
	})
	t.Run("trims by time, then the higher id", func(t *testing.T) {
		a := note{id: "a", version: "1", text: "a", at: day(1)}
		b, c := note{id: "b", version: "1", text: "b", at: day(1)}, note{id: "c", version: "1", text: "c", at: day(2)}
		kind, f := first(t, 3, c, a, b)
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n4)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 3 || got["b"] != "" || got["a"] == "" || got["c"] == "" {
			t.Fatalf("documents = %v", got)
		}
		f.within(t, 3)
	})
	t.Run("never writes a document older than a full window's oldest", func(t *testing.T) {
		kind, f := first(t, 2, n3, n2)
		publishes, objects, outside := len(f.store.publishes), len(f.documents.objects), documentsCounted("outside_window")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n1)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" || !slices.Contains(kind.puts, "notes/n1") {
			t.Fatalf("documents = %v, puts = %v", got, kind.puts)
		}
		if len(f.store.publishes) != publishes || len(f.documents.objects) != objects || len(f.store.purges) != 0 ||
			documentsCounted("outside_window")-outside != 1 {
			t.Fatalf("publishes = %d, objects = %d, purges = %v", len(f.store.publishes), len(f.documents.objects), f.store.purges)
		}
	})
	t.Run("moves an identical resend to its new time in place, and keeps one at the same time", func(t *testing.T) {
		kind, f := first(t, 3, n2, n1)
		id, objects := f.liveID("notes", "n1"), f.documents.puts
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", resent(n1, day(5)))}
		f.run(t, "sync-2")
		if p := last(f); f.liveID("notes", "n1") != id || !f.store.docs[id].Source.At.Equal(day(5)) || len(p.Update) != 1 ||
			len(p.Add)+len(p.Remove) != 0 || len(f.store.fanned) != 2 || f.documents.puts != objects || f.store.owed != 0 {
			t.Fatalf("source = %+v, publish = %+v, fanned = %v", f.store.docs[id].Source, p, f.store.fanned)
		}
		publishes, kept := len(f.store.publishes), documentsCounted("kept")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", resent(n1, day(5)))}
		f.run(t, "sync-3")
		if len(f.store.publishes) != publishes || documentsCounted("kept")-kept != 1 {
			t.Fatalf("publishes = %d, kept = %v", len(f.store.publishes), documentsCounted("kept")-kept)
		}
		// n1 sorts newest now, so n2 is what two newer notes push out.
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n4, n3)}
		f.run(t, "sync-4")
		if got := f.held(t, "notes"); len(got) != 3 || got["n2"] != "" || f.liveID("notes", "n1") != id {
			t.Fatalf("documents = %v", got)
		}
		f.within(t, 3)
	})
	t.Run("holds a resend its batch trimmed again, in place", func(t *testing.T) {
		kind, f := first(t, 2, n2, n1)
		n1ID, n2ID := f.liveID("notes", "n1"), f.liveID("notes", "n2")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n4, resent(n1, day(3)))}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n2"] != "" || got["n4"] == "" || f.liveID("notes", "n1") != n1ID {
			t.Fatalf("documents = %v", got)
		}
		if p := last(f); len(p.Add) != 1 || len(p.Update) != 1 || p.Update[0].ID != n1ID || !slices.Equal(p.Remove, []string{n2ID}) ||
			len(f.store.fanned) != 3 {
			t.Fatalf("publish = %+v, fanned = %v", p, f.store.fanned)
		}
		f.within(t, 2)
	})
	t.Run("takes a pending new document a newer one pushes out of the batch, unwritten", func(t *testing.T) {
		kind, f := first(t, 2)
		added, outside := documentsCounted("added"), documentsCounted("outside_window")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n1, n2, n3)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" || len(f.documents.objects) != 2 {
			t.Fatalf("documents = %v, objects = %d", got, len(f.documents.objects))
		}
		if p := last(f); len(f.store.publishes) != 1 || len(p.Add) != 2 || len(p.Remove) != 0 || f.store.owed != 0 ||
			documentsCounted("added")-added != 2 || documentsCounted("outside_window")-outside != 1 {
			t.Fatalf("publishes = %+v, owed = %d", f.store.publishes, f.store.owed)
		}
		f.within(t, 2)
	})
	t.Run("takes a pending update a newer one pushes out of the batch, and removes it", func(t *testing.T) {
		kind, f := first(t, 2, n2, n1)
		n1ID, updated, removed := f.liveID("notes", "n1"), documentsCounted("updated"), documentsCounted("removed")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", resent(n1, day(1).Add(time.Hour)), n3)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" || got["n3"] == "" {
			t.Fatalf("documents = %v", got)
		}
		if p := last(f); len(p.Add) != 1 || len(p.Update) != 0 || !slices.Equal(p.Remove, []string{n1ID}) ||
			documentsCounted("updated") != updated || documentsCounted("removed")-removed != 1 {
			t.Fatalf("publish = %+v", p)
		}
		f.within(t, 2)
	})
	t.Run("takes a pending replacement a newer one pushes out of the batch, keeping the old one removed", func(t *testing.T) {
		kind, f := first(t, 2, n2, n1)
		n1ID, objects, replaced := f.liveID("notes", "n1"), len(f.documents.objects), documentsCounted("replaced")
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", edited(n1, "changed"), n3)}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" || got["n3"] == "" || len(f.documents.objects) != objects+1 {
			t.Fatalf("documents = %v, objects = %d", got, len(f.documents.objects))
		}
		if p := last(f); len(p.Add) != 1 || p.Add[0].Source.ExternalID != "n3" || !slices.Equal(p.Remove, []string{n1ID}) ||
			documentsCounted("replaced") != replaced {
			t.Fatalf("publish = %+v", p)
		}
		f.within(t, 2)
	})
	t.Run("a full read keeps the first n it offers and removes the held ones it does not", func(t *testing.T) {
		kind, f := first(t, 2, n2, n1)
		overCap := documentsCounted("over_cap")
		kind.notes["notes"] = []note{n4, n3, n2, n1}
		full := newSync("sync-2")
		full.Mode = model.ConnectorSyncFull
		if _, err := f.syncer.Run(context.Background(), full); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, "notes"); len(got) != 2 || got["n4"] == "" || got["n3"] == "" || documentsCounted("over_cap")-overCap != 2 ||
			f.store.owed != 1 || len(f.store.publishes) != 2 {
			t.Fatalf("documents = %v, owed = %d, publishes = %d", got, f.store.owed, len(f.store.publishes))
		}
		// An unchanged read writes nothing.
		kept := documentsCounted("kept")
		full.ID = "sync-3"
		if _, err := f.syncer.Run(context.Background(), full); err != nil {
			t.Fatal(err)
		}
		if len(f.store.publishes) != 2 || documentsCounted("kept")-kept != 2 {
			t.Fatalf("publishes = %d", len(f.store.publishes))
		}
		// A note gone from the source goes, though nothing replaces it.
		kind.notes["notes"] = []note{n4}
		full.ID = "sync-4"
		if _, err := f.syncer.Run(context.Background(), full); err != nil {
			t.Fatal(err)
		}
		if got := f.held(t, "notes"); len(got) != 1 || got["n4"] == "" {
			t.Fatalf("documents = %v", got)
		}
		f.within(t, 2)
	})
	t.Run("brings a set over a lowered n back to it, with or without a new document", func(t *testing.T) {
		kind, f := first(t, 3, n3, n2, n1)
		kind.streams[0].Keep = connector.KeepNewest(2)
		if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges {
			t.Fatalf("mode = %s", res.Mode)
		}
		if got := f.held(t, "notes"); len(got) != 2 || got["n1"] != "" {
			t.Fatalf("documents = %v", got)
		}
		kind.streams[0].Keep = connector.KeepNewest(1)
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n4)}
		f.run(t, "sync-3")
		if got := f.held(t, "notes"); len(got) != 1 || got["n4"] == "" || f.store.lives[len(f.store.lives)-1] != 1 || len(last(f).Remove) != 2 {
			t.Fatalf("documents = %v, publish = %+v", got, last(f))
		}
	})
	t.Run("keeps exactly the source's newest n through random reads", func(t *testing.T) {
		rng := rand.New(rand.NewPCG(4, 2))
		// newestFirst orders notes as a read offers them: newest first, ties to the lower id.
		newestFirst := func(notes []note) []note {
			slices.SortFunc(notes, func(a, b note) int { return cmp.Or(b.at.Compare(a.at), strings.Compare(a.id, b.id)) })
			return notes
		}
		for _, n := range []int{1, 7, 150} {
			kind, f := first(t, n)
			f.syncer.MaxRows = 1000
			source, clock := map[string]note{}, 0
			for i := range 40 {
				// Pieces arrive, some sent again as they were, some edited;
				// now and then two arrive at the same instant.
				touched := map[string]bool{}
				for range rng.IntN(250) {
					clock += rng.IntN(2)
					id := fmt.Sprintf("p%03d", rng.IntN(300))
					p, sent := source[id]
					switch {
					case !sent:
						p = note{id: id, version: "1", text: "piece " + id}
					case rng.IntN(3) == 0:
						p.version, p.text = p.version+"+", p.text+" again"
					}
					p.at = day(1).Add(time.Duration(clock) * time.Minute)
					source[id], touched[id] = p, true
				}
				var offered []note
				for id := range touched {
					offered = append(offered, source[id])
				}
				sync := newSync(fmt.Sprintf("sync-%d-%d", n, i))
				if i%5 == 4 {
					kind.notes["notes"] = newestFirst(slices.Collect(maps.Values(source)))
					sync.Mode = model.ConnectorSyncFull
				} else {
					kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", newestFirst(offered)...)}
				}
				if _, err := f.syncer.Run(context.Background(), sync); err != nil {
					t.Fatalf("n %d, sync %d: %v", n, i, err)
				}
				want := map[string]string{}
				for _, p := range newestFirst(slices.Collect(maps.Values(source)))[:min(n, len(source))] {
					want[p.id] = "notes: " + p.text
				}
				if got := f.held(t, "notes"); !maps.Equal(got, want) {
					t.Fatalf("n %d, sync %d: documents = %v, want %v", n, i, got, want)
				}
				for _, d := range f.store.live(f.store.setOf("notes").ID) {
					if p := source[d.Source.ExternalID]; !d.Source.At.Equal(p.at) || d.Source.Version != p.version {
						t.Fatalf("n %d, sync %d: %s is at %s, version %s; want %s, %s", n, i, p.id, d.Source.At, d.Source.Version, p.at, p.version)
					}
				}
				f.within(t, n)
			}
		}
	})
	t.Run("brings a set over a lowered n back to it after the documents a read removes", func(t *testing.T) {
		kind, f := first(t, 3, n3, n2, n1)
		kind.streams[0].Keep = connector.KeepNewest(1)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			return w.Delete("notes", map[string]any{"id": "n3"})
		}}
		f.run(t, "sync-2")
		if got := f.held(t, "notes"); len(got) != 1 || got["n2"] == "" {
			t.Fatalf("documents = %v", got)
		}
	})
}

// TestSyncerDocumentPause covers a kind with an allowance: its streams take
// and remove no documents while the month is spent, then catch up in full,
// once.
func TestSyncerDocumentPause(t *testing.T) {
	// first syncs n2 and n1 in full, into a window of 3 of a kind with an
	// allowance of 5 a set and 20 a workspace; the month's graph budget is
	// 50, of which work no person started may spend 40.
	first := func(t *testing.T) (*noteKind, fixture, string) {
		t.Helper()
		kind := newWindowKind(connector.CaptureCursor, 3, n2, n1)
		f := newNoteFixture(t, kind)
		f.syncer.Allowances = map[string]model.FoundrySetAllowance{"fake": {SetMicros: 5e6, WorkspaceMicros: 20e6}}
		f.run(t, "sync-1")
		return kind, f, f.store.setOf("notes").ID
	}
	// gone deletes n1 at the source, then sends n3.
	gone := func(kind *noteKind) func(connector.ChangeWriter) error {
		upsert := kind.upsert("notes", n3)
		return func(w connector.ChangeWriter) error {
			if err := w.Delete("notes", map[string]any{"id": "n1"}); err != nil {
				return err
			}
			return upsert(w)
		}
	}
	for _, c := range []struct {
		name   string
		spend  func(rows *documentRows, set string)
		paused bool
	}{
		{"the set's allowance spent", func(r *documentRows, set string) { r.spend[set] = 5e6 }, true},
		{"the set's allowance not quite spent", func(r *documentRows, set string) { r.spend[set] = 5e6 - 1 }, false},
		{"the workspace's allowance spent", func(r *documentRows, set string) { r.spend[set], r.otherSpend = 1e6, 19e6 }, true},
		{"the workspace's allowance not quite spent", func(r *documentRows, set string) { r.spend[set], r.otherSpend = 1e6, 19e6-1 }, false},
		{"the month's share for work no person started spent", func(r *documentRows, _ string) { r.graphSpend = 40e6 }, true},
		{"the month's share not quite spent", func(r *documentRows, _ string) { r.graphSpend = 40e6 - 1 }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			kind, f, set := first(t)
			c.spend(&f.store.documentRows, set)
			var pausedSaid []bool
			kind.extra = func(docs connector.DocumentWriter) {
				pausedSaid = append(pausedSaid, docs.Paused("notes"), docs.Paused("transcripts"))
			}
			before, publishes, puts, counted := f.held(t, "notes"), len(f.store.publishes), len(kind.puts), documentsCounted("paused")
			kind.notes["notes"] = []note{n3, n2}
			kind.changes = []func(connector.ChangeWriter) error{gone(kind)}
			res := f.run(t, "sync-2")
			got := f.held(t, "notes")
			if res.Mode != model.ConnectorSyncChanges || f.store.tables["notes"].SyncID != "sync-2" || f.store.spendReads != 2 {
				t.Fatalf("mode = %s, table = %+v, spend reads = %d", res.Mode, f.store.tables["notes"], f.store.spendReads)
			}
			if !c.paused {
				if len(got) != 2 || got["n3"] == "" || got["n1"] != "" || !slices.Equal(pausedSaid, []bool{false, false}) || f.store.marks != 0 {
					t.Fatalf("documents = %v, paused = %v, marks = %d", got, pausedSaid, f.store.marks)
				}
				return
			}
			// The deleted note's document stays, and the new one waits.
			if !maps.Equal(got, before) || len(f.store.publishes) != publishes || len(kind.puts) != puts ||
				!slices.Equal(pausedSaid, []bool{true, false}) || documentsCounted("paused")-counted != 1 {
				t.Fatalf("documents = %v, publishes = %d, puts = %v, paused = %v", got, len(f.store.publishes), kind.puts, pausedSaid)
			}
			if f.store.marks != 1 || !f.store.skipped[set] || len(f.store.sets) != 1 {
				t.Fatalf("marks = %d, skipped = %v", f.store.marks, f.store.skipped)
			}
			// A full read removes nothing either; each sync marks the set once.
			kind.notes["notes"] = []note{n4}
			full := newSync("sync-3")
			full.Mode = model.ConnectorSyncFull
			if _, err := f.syncer.Run(context.Background(), full); err != nil {
				t.Fatal(err)
			}
			if got := f.held(t, "notes"); !maps.Equal(got, before) || len(f.store.publishes) != publishes || f.store.marks != 2 {
				t.Fatalf("documents = %v, publishes = %d, marks = %d", got, len(f.store.publishes), f.store.marks)
			}
		})
	}
	// The pause keeps what the graph last extracted: a set that holds no
	// documents has nothing to keep, so its first sync writes them, for a
	// person's load to read, and only the next one pauses.
	for _, c := range []struct {
		name  string
		spend func(r *documentRows)
	}{
		{"the workspace's allowance", func(r *documentRows) { r.otherSpend = 20e6 }},
		{"the month's share for work no person started", func(r *documentRows) { r.graphSpend = 40e6 }},
	} {
		t.Run("writes a set's first documents with "+c.name+" spent, then pauses", func(t *testing.T) {
			kind := newWindowKind(connector.CaptureCursor, 3, n2, n1)
			f := newNoteFixture(t, kind)
			f.syncer.Allowances = map[string]model.FoundrySetAllowance{"fake": {SetMicros: 5e6, WorkspaceMicros: 20e6}}
			c.spend(&f.store.documentRows)
			f.run(t, "sync-1")
			set := f.store.setOf("notes").ID
			if got := f.held(t, "notes"); len(got) != 2 || f.store.marks != 0 || f.store.skipped[set] {
				t.Fatalf("documents = %v, marks = %d", got, f.store.marks)
			}
			before, publishes := f.held(t, "notes"), len(f.store.publishes)
			kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3)}
			f.run(t, "sync-2")
			if !maps.Equal(f.held(t, "notes"), before) || len(f.store.publishes) != publishes || f.store.marks != 1 || !f.store.skipped[set] {
				t.Fatalf("documents = %v, publishes = %d, marks = %d", f.held(t, "notes"), len(f.store.publishes)-publishes, f.store.marks)
			}
		})
	}
	// A sync a person requested, such as the full read a text field's
	// change queues, writes its documents whatever is spent, and catches up
	// on a skip; the next sync no one requested pauses again.
	t.Run("writes the documents of a sync a person requested, spent or not", func(t *testing.T) {
		kind, f, set := first(t)
		f.store.spend[set] = 5e6
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3)}
		f.run(t, "sync-2")
		if !f.store.skipped[set] || len(f.held(t, "notes")) != 2 {
			t.Fatalf("skipped = %v, documents = %v", f.store.skipped, f.held(t, "notes"))
		}
		kind.notes["notes"] = []note{n4, n3, n2}
		kind.changes = nil
		asked, user := newSync("sync-3"), "user-1"
		asked.RequestedByUserID = &user
		res, err := f.syncer.Run(context.Background(), asked)
		if err != nil {
			t.Fatal(err)
		}
		got := f.held(t, "notes")
		if res.Mode != model.ConnectorSyncFull || len(got) != 3 || got["n4"] == "" || got["n1"] != "" || f.store.skipped[set] ||
			f.store.clears != 1 || f.store.marks != 1 {
			t.Fatalf("mode = %s, documents = %v, skipped = %v, clears = %d, marks = %d", res.Mode, got, f.store.skipped, f.store.clears,
				f.store.marks)
		}
		f.within(t, 3)
		kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", note{id: "n5", version: "1|a", text: "fifth", at: day(5)})}
		f.run(t, "sync-4")
		if !maps.Equal(f.held(t, "notes"), got) || f.store.marks != 2 || !f.store.skipped[set] {
			t.Fatalf("documents = %v, marks = %d", f.held(t, "notes"), f.store.marks)
		}
	})
	t.Run("catches up in full once there is room, then forgets the skip", func(t *testing.T) {
		kind, f, set := first(t)
		f.store.spend[set] = 5e6
		kind.notes["notes"] = []note{n3, n2}
		kind.changes = []func(connector.ChangeWriter) error{gone(kind)}
		f.run(t, "sync-2")
		if !f.store.skipped[set] || len(f.held(t, "notes")) != 2 {
			t.Fatalf("skipped = %v, documents = %v", f.store.skipped, f.held(t, "notes"))
		}
		delete(f.store.spend, set) // a new month
		res := f.run(t, "sync-3")
		if got := f.held(t, "notes"); res.Mode != model.ConnectorSyncFull || len(got) != 2 || got["n3"] != "notes: third" || got["n1"] != "" ||
			f.store.skipped[set] || f.store.clears != 1 {
			t.Fatalf("mode = %s, documents = %v, skipped = %v, clears = %d", res.Mode, got, f.store.skipped, f.store.clears)
		}
		if p := f.store.publishes[len(f.store.publishes)-2]; !p.Fan.SystemOnly {
			t.Fatalf("publish = %+v", p)
		}
		if res := f.run(t, "sync-4"); res.Mode != model.ConnectorSyncChanges || f.store.clears != 1 {
			t.Fatalf("mode = %s, clears = %d", res.Mode, f.store.clears)
		}
		f.within(t, 3)
	})
	t.Run("keeps the skip when the full read fails, and catches up on the next", func(t *testing.T) {
		kind, f, set := first(t)
		f.store.spend[set] = 5e6
		f.run(t, "sync-2")
		delete(f.store.spend, set)
		kind.syncErr = errors.New("connection reset")
		if _, err := f.syncer.Run(context.Background(), newSync("sync-3")); err == nil || failure(t, err).Permanent {
			t.Fatalf("Run = %v", err)
		}
		if !f.store.skipped[set] || f.store.clears != 0 {
			t.Fatalf("skipped = %v, clears = %d", f.store.skipped, f.store.clears)
		}
		kind.syncErr = nil
		if res := f.run(t, "sync-4"); res.Mode != model.ConnectorSyncFull || f.store.skipped[set] || f.store.clears != 1 {
			t.Fatalf("mode = %s, skipped = %v, clears = %d", res.Mode, f.store.skipped, f.store.clears)
		}
	})
	t.Run("keeps the skip when clearing it fails, and the retry converges", func(t *testing.T) {
		kind, f, set := first(t)
		f.store.spend[set] = 5e6
		f.run(t, "sync-2")
		delete(f.store.spend, set)
		kind.notes["notes"] = []note{n3, n2, n1}
		failed := false
		f.store.fail = func(step string) error {
			if step != "clear" || failed {
				return nil
			}
			failed = true
			return errors.New("connection reset")
		}
		sync := newSync("sync-3")
		if _, err := f.syncer.Run(context.Background(), sync); err == nil || failure(t, err).Permanent || !failed {
			t.Fatalf("Run = %v", err)
		}
		if !f.store.skipped[set] || f.store.tables["notes"].SyncID != "sync-1" {
			t.Fatalf("skipped = %v, table = %+v", f.store.skipped, f.store.tables["notes"])
		}
		sync.Attempts = 2
		res, err := f.syncer.Run(context.Background(), sync)
		if err != nil || res.Mode != model.ConnectorSyncFull || f.store.skipped[set] || f.store.clears != 1 || len(f.held(t, "notes")) != 3 {
			t.Fatalf("retry = (%+v, %v), skipped = %v, documents = %v", res, err, f.store.skipped, f.held(t, "notes"))
		}
		f.within(t, 3)
	})
	for _, c := range []struct {
		name  string
		setup func(f fixture, set string)
	}{
		{"the sets' spend can't be read", func(f fixture, _ string) {
			f.store.fail = func(step string) error {
				if step == "spend" {
					return errors.New("connection reset")
				}
				return nil
			}
		}},
		{"the month's graph spend can't be read", func(f fixture, _ string) {
			f.store.fail = func(step string) error {
				if step == "graph" {
					return errors.New("connection reset")
				}
				return nil
			}
		}},
		{"the set is gone from the spend read", func(f fixture, _ string) { f.store.spendGone = true }},
		{"the set's skip can't be marked", func(f fixture, set string) {
			f.store.spend[set] = 5e6
			f.store.fail = func(step string) error {
				if step == "mark" {
					return errors.New("connection reset")
				}
				return nil
			}
		}},
	} {
		t.Run("retries, writing nothing, when "+c.name, func(t *testing.T) {
			kind, f, set := first(t)
			c.setup(f, set)
			kind.changes = []func(connector.ChangeWriter) error{kind.upsert("notes", n3)}
			publishes := len(f.store.publishes)
			if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err == nil || failure(t, err).Permanent {
				t.Fatalf("Run = %v", err)
			}
			if len(f.store.publishes) != publishes || f.store.tables["notes"].SyncID != "sync-1" || len(f.held(t, "notes")) != 2 {
				t.Fatalf("publishes = %d, table = %+v", len(f.store.publishes), f.store.tables["notes"])
			}
		})
	}
	t.Run("fans its documents out as work no person started", func(t *testing.T) {
		_, f, _ := first(t)
		if p := f.store.publishes[0]; !p.Fan.SystemOnly || p.Fan.RequestedByUserID != nil || f.store.spendReads != 1 || f.store.marks != 0 {
			t.Fatalf("publish = %+v, spend reads = %d", p, f.store.spendReads)
		}
	})
	t.Run("leaves a kind without an allowance alone", func(t *testing.T) {
		f := newNoteFixture(t, newWindowKind(connector.CaptureCursor, 3, n2, n1))
		f.syncer.Allowances = map[string]model.FoundrySetAllowance{"events": {SetMicros: 1, WorkspaceMicros: 1}}
		f.store.graphSpend = 50e6
		f.run(t, "sync-1")
		if len(f.held(t, "notes")) != 2 || f.store.spendReads != 0 || f.store.publishes[0].Fan.SystemOnly {
			t.Fatalf("documents = %v, spend reads = %d", f.held(t, "notes"), f.store.spendReads)
		}
	})
}

// passKind is a noteKind that reads every table in one pass.
type passKind struct {
	*noteKind
	passes [][]string
}

func (k *passKind) SyncPass(ctx context.Context, _ connector.Source, tables []connector.Table, rows map[string]connector.RowWriter,
	docs connector.DocumentWriter) ([]connector.Table, error) {
	var names []string
	for _, t := range tables {
		names = append(names, t.Name)
	}
	k.passes = append(k.passes, names)
	for _, t := range tables {
		if notes, ok := k.notes[t.Name]; ok {
			if err := k.offer(ctx, t.Name, notes, rows[t.Name].Write, docs); err != nil {
				return nil, err
			}
			continue
		}
		for _, row := range k.rows[t.Name] {
			if err := rows[t.Name].WriteJSON([]byte(row)); err != nil {
				return nil, err
			}
		}
	}
	return tables, nil
}

// newPassKind reads notes and calls, each carrying a stream, and people.
func newPassKind(capture connector.Capture) *passKind {
	kind := newNoteKind(capture, n2, n1)
	kind.discovery.Tables = append(kind.discovery.Tables, connector.Table{Name: "calls", PrimaryKey: []string{"id"}})
	kind.streams = append(kind.streams, connector.Stream{Name: "calls", Table: "calls", Title: "Calls", Format: connector.PlainText})
	kind.notes["calls"] = []note{{id: "c2", version: "1", text: "call two", at: day(2)}, {id: "c1", version: "1", text: "call one", at: day(1)}}
	return &passKind{noteKind: kind}
}

func TestSyncerPass(t *testing.T) {
	t.Run("reads every table in one pass", func(t *testing.T) {
		kind := newPassKind(connector.CaptureFull)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		if len(kind.passes) != 1 || !slices.Equal(kind.passes[0], []string{"notes", "people", "calls"}) || len(kind.synced) != 0 {
			t.Fatalf("passes = %v, synced = %v", kind.passes, kind.synced)
		}
		if len(f.store.tables) != 3 || len(f.held(t, "notes")) != 2 || f.held(t, "calls")["c1"] != "calls: call one" {
			t.Fatalf("tables = %v, calls = %v", f.store.tables, f.held(t, "calls"))
		}
	})
	t.Run("reads the tables an earlier attempt published into writers that discard them", func(t *testing.T) {
		kind := newPassKind(connector.CaptureFull)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		f.store.tables["calls"].SyncID = "sync-0"
		notes, people := f.store.tables["notes"].S3Key, f.store.tables["people"].S3Key
		kind.notes["notes"] = []note{n3, n2, n1}
		kind.notes["calls"] = append([]note{{id: "c3", version: "1", text: "call three", at: day(3)}}, kind.notes["calls"]...)
		retry := newSync("sync-1")
		retry.Attempts = 2
		if _, err := f.syncer.Run(context.Background(), retry); err != nil {
			t.Fatal(err)
		}
		if len(kind.passes) != 2 || !slices.Equal(kind.passes[1], []string{"notes", "people", "calls"}) {
			t.Fatalf("passes = %v", kind.passes)
		}
		if f.store.tables["notes"].S3Key != notes || f.store.tables["people"].S3Key != people || len(f.held(t, "notes")) != 2 ||
			len(f.held(t, "calls")) != 3 || slices.Contains(kind.puts, "notes/n3") {
			t.Fatalf("tables = %v, notes = %v, puts = %v", f.store.tables, f.held(t, "notes"), kind.puts)
		}
	})
	t.Run("reads a table a change read resets alone", func(t *testing.T) {
		kind := newPassKind(connector.CaptureCursor)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		upsert := kind.upsert("calls", note{id: "c3", version: "1", text: "call three", at: day(3)})
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			w.Reset("notes", "the cursor column was dropped")
			return upsert(w)
		}}
		if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(kind.passes) != 2 || !slices.Equal(kind.passes[1], []string{"notes"}) {
			t.Fatalf("mode = %s, passes = %v", res.Mode, kind.passes)
		}
		if len(f.held(t, "calls")) != 3 || len(f.held(t, "notes")) != 2 {
			t.Fatalf("calls = %v, notes = %v", f.held(t, "calls"), f.held(t, "notes"))
		}
	})
	t.Run("names the table that went over a limit", func(t *testing.T) {
		kind := newPassKind(connector.CaptureFull)
		kind.notes["calls"] = append(kind.notes["calls"], note{id: "c0", version: "1", text: "call zero", at: day(1)})
		f := newNoteFixture(t, kind)
		f.syncer.MaxRows = 2
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "table calls has more than 2 rows; choose fewer tables" {
			t.Fatalf("failure = %+v", fl)
		}
	})
	t.Run("finishes each stream table's documents", func(t *testing.T) {
		kind := newPassKind(connector.CaptureFull)
		f := newNoteFixture(t, kind)
		f.run(t, "sync-1")
		kind.notes["notes"] = kind.notes["notes"][:1]
		kind.notes["calls"] = kind.notes["calls"][1:]
		f.run(t, "sync-2")
		if notes, calls := f.held(t, "notes"), f.held(t, "calls"); len(notes) != 1 || notes["n2"] == "" || len(calls) != 1 || calls["c1"] == "" ||
			f.store.owed != 2 {
			t.Fatalf("notes = %v, calls = %v, owed = %d", notes, calls, f.store.owed)
		}
	})
}

func TestDocumentText(t *testing.T) {
	t.Run("truncates at a line, or a character, never over the cap", func(t *testing.T) {
		for _, c := range []struct {
			text string
			max  int64
			want string
		}{
			{"short\n", 100, "short\n"},
			{strings.Repeat("line\n", 20), 60, "line\nline\nline\n[Truncated: the source holds 100 bytes]\n"},
			// No line keeps half of what fits: the cut is at a character, never inside one.
			{"# T\n" + strings.Repeat("é", 50), 60, "# T\nééééééé\n[Truncated: the source holds 104 bytes]\n"},
			{strings.Repeat("é", 50), 59, "ééééééééé\n[Truncated: the source holds 100 bytes]\n"},
			// A cap below the marker's length cuts without it.
			{strings.Repeat("é", 50), 9, "éééé"},
		} {
			got, truncated := truncate([]byte(c.text), c.max)
			if string(got) != c.want || truncated != (c.text != c.want) || int64(len(got)) > c.max || !utf8.Valid(got) {
				t.Errorf("truncate(%q, %d) = %q, %t", c.text, c.max, got, truncated)
			}
		}
	})
	t.Run("cleans a title to a document name", func(t *testing.T) {
		for title, want := range map[string]string{
			"Standup / planning":            "Standup - planning",
			" a\tb\nc \\ d ":                "a b c - d",
			"":                              "n-1",
			"\x00\n":                        "n-1",
			"bad \xff byte":                 "bad � byte",
			strings.Repeat("é", 300):        strings.Repeat("é", 255),
			strings.Repeat("x", 254) + " y": strings.Repeat("x", 254),
		} {
			if got := documentName(title, "n/1"); got != want || !document.ValidName(got) {
				t.Errorf("documentName(%q) = %q, want %q", title, got, want)
			}
		}
	})
	t.Run("keeps only an absolute https link", func(t *testing.T) {
		for raw, kept := range map[string]bool{
			"https://notes.granola.ai/d/1": true, "http://notes.granola.ai/d/1": false, "/d/1": false, "https://": false,
			"javascript:alert(1)": false, "https://x.example/" + strings.Repeat("a", 2048): false, "": false,
		} {
			if got := sourceURL(raw); (got != nil) != kept || got != nil && *got != raw {
				t.Errorf("sourceURL(%q) = %v", raw, got)
			}
		}
	})
}
