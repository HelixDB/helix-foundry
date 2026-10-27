package syncer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// memoryStore holds one workspace; leaseOwner is the worker that still holds every sync's lease.
type memoryStore struct {
	documentRows
	workspace  *workspace
	connectors map[string]*model.Connector
	tables     map[string]*model.ConnectorTable
	leaseOwner string
	// batches are the change batches recorded, by table; replaced the
	// snapshot keys left for the sweep, in order.
	batches  map[string][]*model.ConnectorChangeBatch
	replaced []string
	// failPrune fails the next prune, as an attempt that dies after writing its tables does.
	failPrune bool
}

func (m *memoryStore) WorkspaceDeleting(_ context.Context, id string) (bool, error) {
	if m.workspace == nil || m.workspace.ID != id {
		return false, foundrystore.ErrNotFound
	}
	return m.workspace.Deleting, nil
}

// workspace is the host's workspace, as Foundry's store reads it.
type workspace struct {
	ID       string
	Deleting bool
}

func (m *memoryStore) GetConnector(_ context.Context, workspaceID, id string) (*model.Connector, error) {
	c, ok := m.connectors[id]
	if !ok || c.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	copied := *c
	return &copied, nil
}

func (m *memoryStore) ListConnectorTables(context.Context, string, string) ([]*model.ConnectorTable, error) {
	var out []*model.ConnectorTable
	for _, t := range m.tables {
		copied := *t
		out = append(out, &copied)
	}
	return out, nil
}

func (m *memoryStore) UpsertConnectorTable(_ context.Context, t *model.ConnectorTable, batch *model.ConnectorChangeBatch, workerID string) error {
	if workerID != m.leaseOwner {
		return foundrystore.ErrNotFound
	}
	if err := m.failStep("table"); err != nil {
		return err
	}
	if prev, ok := m.tables[t.TableName]; ok {
		t.ChangeSeq = prev.ChangeSeq
		if prev.S3Key != t.S3Key {
			m.replaced = append(m.replaced, prev.S3Key)
		}
		if t.ReconciledAt == nil {
			t.ReconciledAt = prev.ReconciledAt
		}
	}
	if batch != nil {
		t.ChangeSeq++
		batch.Seq = t.ChangeSeq
		m.batches[t.TableName] = append(m.batches[t.TableName], batch)
	}
	m.tables[t.TableName] = t
	return nil
}

func (m *memoryStore) ListConnectorChangeBatches(_ context.Context, _, syncID string) ([]*model.ConnectorChangeBatch, error) {
	var out []*model.ConnectorChangeBatch
	for _, batches := range m.batches {
		for _, b := range batches {
			if b.SyncID == syncID {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

func (m *memoryStore) TouchConnectorTable(_ context.Context, _, table, syncID string, reconciled bool, workerID string) error {
	if workerID != m.leaseOwner {
		return foundrystore.ErrNotFound
	}
	t := m.tables[table]
	t.SyncID = syncID
	if reconciled {
		now := time.Now()
		t.ReconciledAt = &now
	}
	return nil
}

func (m *memoryStore) PruneConnectorTables(_ context.Context, _, _ string, keep []string, _, workerID string) ([]string, error) {
	if workerID != m.leaseOwner {
		return nil, foundrystore.ErrNotFound
	}
	if m.failPrune {
		m.failPrune = false
		return nil, errors.New("connection reset")
	}
	var removed []string
	for name, t := range m.tables {
		if !slices.Contains(keep, name) {
			removed = append(removed, name)
			m.replaced = append(m.replaced, t.S3Key)
			delete(m.tables, name)
		}
	}
	return removed, nil
}

func (m *memoryStore) UpdateConnectorCursor(_ context.Context, connectorID string, cursor json.RawMessage, _, workerID string) error {
	if workerID != m.leaseOwner {
		return foundrystore.ErrNotFound
	}
	m.connectors[connectorID].ChangeCursor = cursor
	return nil
}

func (m *memoryStore) ClearConnectorCursor(_ context.Context, connectorID string, cursor json.RawMessage) error {
	if c := m.connectors[connectorID]; bytes.Equal(c.ChangeCursor, cursor) {
		c.ChangeCursor = nil
	}
	return nil
}

type memorySnapshots struct{ objects map[string][]byte }

func (m *memorySnapshots) Put(_ context.Context, key string, body io.ReadSeeker) error {
	b, err := io.ReadAll(body)
	m.objects[key] = b
	return err
}

func (m *memorySnapshots) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m.objects[key])), nil
}

func (m *memorySnapshots) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

// testPlans is the default catalog, Free, Pro and Max, with the test's fake kind on every plan.
func testPlans() []config.PlanConfig {
	plans := config.DefaultPlans()
	for i := range plans {
		plans[i].Kinds = append(plans[i].Kinds, "fake")
	}
	return plans
}

func catalog(t *testing.T) *foundryplan.Catalog {
	t.Helper()
	c, err := foundryplan.NewCatalog(testPlans(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// plan answers every workspace with one catalog plan, or err when Autumn is down.
type plan struct {
	code string
	err  error
}

func (p plan) Status(_ context.Context, _ string) (foundryplan.Status, error) {
	c, err := foundryplan.NewCatalog(testPlans(), nil)
	if err != nil {
		return foundryplan.Status{}, err
	}
	if p.err != nil {
		return foundryplan.Status{Plan: c.Free()}, p.err
	}
	return foundryplan.Status{Plan: c.Get(p.code)}, nil
}

// stripeServer serves customers and charges; every other object is unreadable.
func stripeServer(t *testing.T, status int) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case status != http.StatusOK:
			w.WriteHeader(status)
		case r.URL.Path == "/v1/customers":
			_, _ = w.Write([]byte(`{"data":[{"id":"cus_1","email":"a@b.c"},{"id":"cus_2","name":"B"}],"has_more":false}`))
		case r.URL.Path == "/v1/charges":
			_, _ = w.Write([]byte(`{"data":[{"id":"ch_1","customer":"cus_1"}],"has_more":false}`))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

type fixture struct {
	syncer    *Syncer
	store     *memoryStore
	snapshots *memorySnapshots
	documents *memoryObjects
	sync      *model.ConnectorSync
}

func newFixture(t *testing.T, stripeURL, kind string, settings string, p planReader) fixture {
	t.Helper()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, _ := keys.Seal([]byte("sk_test_abc"), connector.SecretContext("conn-1"))
	endpoints := connector.DefaultEndpoints()
	endpoints.Stripe = stripeURL
	store := &memoryStore{
		workspace:  &workspace{ID: "ws-1"},
		leaseOwner: "worker-1",
		connectors: map[string]*model.Connector{"conn-1": {
			ID: "conn-1", WorkspaceID: "ws-1", Kind: kind, Config: json.RawMessage(settings),
			SecretCiphertext: sealed, SecretKeyID: keyID,
		}},
		tables:       map[string]*model.ConnectorTable{},
		batches:      map[string][]*model.ConnectorChangeBatch{},
		documentRows: newDocumentRows(),
	}
	snapshots := &memorySnapshots{objects: map[string][]byte{}}
	documents := &memoryObjects{objects: map[string][]byte{}}
	return fixture{
		syncer: &Syncer{
			DB: store, Catalog: connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{}), Keys: keys,
			Plans: p, PlanCatalog: catalog(t), Snapshots: snapshots, MaxRows: 100, MaxBytes: 1 << 20, TempDir: t.TempDir(),
			Documents: documents, MaxDocumentBytes: 1 << 10, MaxPerStream: 1000, PurgeGrace: time.Hour,
			MonthBudget: func(time.Time) model.FoundryGraphBudget {
				return model.FoundryGraphBudget{CapMicros: 50e6, ReserveMicros: 10e6}
			},
		},
		store: store, snapshots: snapshots, documents: documents,
		sync: newSync("sync-1"),
	}
}

func newSync(id string) *model.ConnectorSync {
	owner := "worker-1"
	return &model.ConnectorSync{ID: id, ConnectorID: "conn-1", WorkspaceID: "ws-1", LeaseOwner: &owner}
}

// published is the snapshot keys connector_tables points at.
func (f fixture) published() []string {
	var keys []string
	for _, t := range f.store.tables {
		keys = append(keys, t.S3Key)
	}
	slices.Sort(keys)
	return keys
}

func (f fixture) stored() []string {
	keys := slices.Collect(maps.Keys(f.snapshots.objects))
	slices.Sort(keys)
	return keys
}

func lines(t *testing.T, gz []byte) []string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func failure(t *testing.T, err error) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("error = %v, want *Failure", err)
	}
	return f
}

func TestSyncerRun(t *testing.T) {
	// Every readable object becomes one snapshot and one connector_tables row.
	t.Run("snapshots each readable table", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil {
			t.Fatal(err)
		}
		if res.Rows != 3 || len(f.store.tables) != 2 {
			t.Fatalf("rows = %d, tables = %v", res.Rows, f.store.tables)
		}
		customers := f.store.tables["customers"]
		if customers.RowCount != 2 || !strings.HasPrefix(customers.S3Key, "ws/ws-1/connectors/conn-1/customers/") || customers.SyncID != "sync-1" {
			t.Fatalf("customers = %+v", customers)
		}
		if string(customers.Columns) != `[{"name":"customer_id"},{"name":"email"},{"name":"name"}]` ||
			string(customers.PrimaryKey) != `["customer_id"]` || string(customers.ForeignKeys) != `[]` {
			t.Fatalf("customers schema = %s %s %s", customers.Columns, customers.PrimaryKey, customers.ForeignKeys)
		}
		charges := f.store.tables["charges"]
		if !strings.Contains(string(charges.ForeignKeys), `"referenced_table":"customers"`) {
			t.Fatalf("charges foreign keys = %s", charges.ForeignKeys)
		}
		if got := lines(t, f.snapshots.objects[customers.S3Key]); len(got) != 2 || !strings.Contains(got[0], `"customer_id":"cus_1"`) {
			t.Fatalf("snapshot = %q", got)
		}
	})
	// An unchanged source keeps its snapshots: a load reading them, and the graph, are left alone.
	t.Run("keeps the snapshot of an unchanged table", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		first := f.published()
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.published(), first) || !slices.Equal(f.stored(), first) || len(res.Changed) != 0 || len(f.store.replaced) != 0 {
			t.Fatalf("published %v, first %v, changed %v", f.published(), first, res.Changed)
		}
		if f.store.tables["customers"].SyncID != "sync-2" || f.store.tables["customers"].ReconciledAt == nil {
			t.Fatalf("customers = %+v", f.store.tables["customers"])
		}
	})
	// A retry of the same sync keeps what an earlier attempt wrote instead of
	// reading it again, so it never rewrites a published snapshot either.
	t.Run("skips the tables an earlier attempt wrote", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		first := f.published()
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil {
			t.Fatal(err)
		}
		if res.Rows != 3 || !slices.Equal(f.published(), first) || !slices.Equal(f.stored(), first) {
			t.Fatalf("rows = %d, published %v, first %v", res.Rows, f.published(), first)
		}
	})
	// A worker that lost its lease leaves the new owner's tables and objects alone.
	t.Run("publishes nothing after losing the lease", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		f.store.leaseOwner = "worker-2"
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || !strings.Contains(fl.Error(), "lease lost") {
			t.Fatalf("failure = %+v", fl)
		}
		if len(f.store.tables) != 0 || len(f.snapshots.objects) != 0 {
			t.Fatalf("tables = %v, objects = %v", f.store.tables, f.stored())
		}
	})
	t.Run("drops tables no longer selected", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		f.store.connectors["conn-1"].Config = json.RawMessage(`{"mode":"test","tables":["charges"]}`)
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.store.tables["customers"]; ok || len(f.store.replaced) != 1 {
			t.Fatalf("tables = %v, replaced = %v", f.store.tables, f.store.replaced)
		}
	})
	t.Run("syncs only the chosen tables", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test","tables":["charges"]}`, nil)
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.store.tables["customers"]; ok || len(f.store.tables) != 1 {
			t.Fatalf("tables = %v", f.store.tables)
		}
	})
	// Nothing of a deleted workspace is read with its stored credential.
	t.Run("ends the sync of a deleted workspace", func(t *testing.T) {
		for _, ws := range []*workspace{{ID: "ws-1", Deleting: true}, nil} {
			f := newFixture(t, "http://unused", "stripe", `{}`, nil)
			f.store.workspace = ws
			_, err := f.syncer.Run(context.Background(), f.sync)
			if fl := failure(t, err); !fl.Permanent || fl.Message != "the workspace was deleted" {
				t.Fatalf("workspace %+v: failure = %+v", ws, fl)
			}
		}
	})
	t.Run("ends the sync of a deleted connector", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{}`, nil)
		delete(f.store.connectors, "conn-1")
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "the connector was deleted" {
			t.Fatalf("failure = %+v", fl)
		}
	})
	// A workspace that dropped to Free stops syncing its databases; buying Pro again is the fix.
	t.Run("stops a paid kind on a free workspace", func(t *testing.T) {
		f := newFixture(t, "http://unused", "mysql", `{"host":"127.0.0.1","port":1,"database":"x","user":"u","disable_tls":true}`, plan{code: "free"})
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || !fl.PlanRequired || fl.Message != "this connector needs Foundry Pro" {
			t.Fatalf("failure = %+v", fl)
		}
	})
	// An Autumn outage must neither stop a Free connector nor open a paid one.
	t.Run("runs a free kind on Free's rules while the plan can't be read", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, plan{err: errors.New("autumn down")})
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil || res.Plan.Code != "free" {
			t.Fatalf("Run = (%+v, %v)", res, err)
		}
		g := newFixture(t, "http://unused", "mysql", `{"host":"127.0.0.1","port":1,"database":"x","user":"u","disable_tls":true}`, plan{err: errors.New("autumn down")})
		_, err = g.syncer.Run(context.Background(), g.sync)
		if fl := failure(t, err); fl.Permanent {
			t.Fatalf("failure = %+v, want a retry", fl)
		}
	})
	// A rejected key may be a blip or a revocation, so it is retried until attempts run out.
	t.Run("retries a source refusal with its message", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusUnauthorized), "stripe", `{}`, nil)
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || fl.Message != "Stripe rejected this key; check it and test again" {
			t.Fatalf("failure = %+v", fl)
		}
	})
	t.Run("stops a table over the row limit", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{}`, nil)
		f.syncer.MaxRows = 1
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || !strings.Contains(fl.Message, "more than 1 rows") {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

// fakeKind discovers a fixed set of tables and writes each table's rows.
type fakeKind struct {
	discovery connector.Discovery
	rows      map[string][]string
	written   map[string]connector.Table
	syncErr   error
	synced    []string
	sources   []connector.Source
}

func (k *fakeKind) Name() string                                          { return "fake" }
func (k *fakeKind) DisplayName() string                                   { return "Fake" }
func (k *fakeKind) Prepare(src connector.Source) (json.RawMessage, error) { return src.Settings, nil }
func (k *fakeKind) Hint(string) string                                    { return "" }

func (k *fakeKind) Discover(_ context.Context, src connector.Source) (connector.Discovery, error) {
	k.sources = append(k.sources, src)
	return k.discovery, nil
}

func (k *fakeKind) Sync(_ context.Context, src connector.Source, table connector.Table, w connector.RowWriter) (connector.Table, error) {
	k.sources = append(k.sources, src)
	k.synced = append(k.synced, table.Name)
	if k.syncErr != nil {
		return connector.Table{}, k.syncErr
	}
	for _, row := range k.rows[table.Name] {
		if err := w.WriteJSON([]byte(row)); err != nil {
			return connector.Table{}, err
		}
	}
	if written, ok := k.written[table.Name]; ok {
		return written, nil
	}
	return table, nil
}

func newFakeFixture(t *testing.T, kind *fakeKind, settings string) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "fake", settings, nil)
	f.syncer.Catalog = connector.NewCatalogOf(kind)
	return f
}

func storedTable(name, key, syncID string) *model.ConnectorTable {
	return &model.ConnectorTable{TableName: name, S3Key: key, SyncID: syncID, RowCount: 4}
}

func TestSyncerUnavailableTables(t *testing.T) {
	discovery := connector.Discovery{
		Tables:      []connector.Table{{Name: "orders"}},
		Unavailable: []connector.Unavailable{{Name: "big", Reason: "is 3.1 GB, over the 2 GB limit"}, {Name: "charges"}},
	}
	rows := map[string][]string{"orders": {`{"id":1}`}}
	// The default choice syncs every table, so one synced before must not
	// silently disappear because it cannot be read now.
	t.Run("fails a stored table that turned unavailable", func(t *testing.T) {
		for name, want := range map[string]string{
			"big":     "table big can't be synced: is 3.1 GB, over the 2 GB limit",
			"charges": "table charges can't be synced: the connection cannot read it",
		} {
			f := newFakeFixture(t, &fakeKind{discovery: discovery, rows: rows}, `{}`)
			f.store.tables[name] = storedTable(name, "old-key", "sync-0")
			f.snapshots.objects["old-key"] = []byte("x")
			_, err := f.syncer.Run(context.Background(), f.sync)
			if fl := failure(t, err); !fl.Permanent || fl.Message != want {
				t.Fatalf("failure = %+v", fl)
			}
			if _, ok := f.store.tables[name]; !ok || f.snapshots.objects["old-key"] == nil {
				t.Fatalf("pruned %s: tables %v", name, f.store.tables)
			}
		}
	})
	t.Run("fails an unavailable table chosen by name", func(t *testing.T) {
		f := newFakeFixture(t, &fakeKind{discovery: discovery, rows: rows}, `{"tables":["orders","big"]}`)
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || !strings.HasPrefix(fl.Message, "table big can't be synced") {
			t.Fatalf("failure = %+v", fl)
		}
	})
	t.Run("ignores an unavailable table out of scope", func(t *testing.T) {
		for _, settings := range []string{`{}`, `{"tables":["orders"]}`} {
			f := newFakeFixture(t, &fakeKind{discovery: discovery, rows: rows}, settings)
			if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
				t.Fatalf("%s: %v", settings, err)
			}
		}
	})
	// A table the source no longer has at all is dropped; its snapshot waits for the sweep.
	t.Run("prunes a stored table that is gone", func(t *testing.T) {
		f := newFakeFixture(t, &fakeKind{discovery: discovery, rows: rows}, `{}`)
		f.store.tables["gone"] = storedTable("gone", "gone-key", "sync-0")
		f.snapshots.objects["gone-key"] = []byte("x")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.store.tables["gone"]; ok || !slices.Equal(f.store.replaced, []string{"gone-key"}) {
			t.Fatalf("tables = %v, replaced = %v", f.store.tables, f.store.replaced)
		}
	})
	// A probe that was rate limited is retried; only a real refusal fails for good.
	t.Run("retries a SaaS probe that was rate limited", func(t *testing.T) {
		posthog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "FROM groups") {
				w.WriteHeader(http.StatusTooManyRequests)
			}
			_, _ = w.Write([]byte(`{"results":[]}`))
		}))
		t.Cleanup(posthog.Close)
		f := newFixture(t, "", "posthog", `{"region":"us","project_id":"42","event_days":30}`, nil)
		endpoints := connector.DefaultEndpoints()
		endpoints.PostHogUS = posthog.URL
		f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
		f.store.tables["groups"] = storedTable("groups", "groups-key", "sync-0")
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || fl.Message != "PostHog returned HTTP 429; try again" {
			t.Fatalf("failure = %+v", fl)
		}
		if _, ok := f.store.tables["groups"]; !ok {
			t.Fatal("pruned groups")
		}
	})
	t.Run("covers a SaaS kind", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, http.StatusOK), "stripe", `{"mode":"test"}`, nil)
		f.store.tables["subscriptions"] = storedTable("subscriptions", "sub-key", "sync-0")
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "table subscriptions can't be synced: the connection cannot read it" {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

func TestSyncerWrittenTables(t *testing.T) {
	discovery := connector.Discovery{
		Tables:   []connector.Table{{Name: "orders", Columns: []connector.Column{{Name: "guess"}}}, {Name: "items"}},
		Settings: json.RawMessage(`{"region":"eu-west-2"}`),
	}
	rows := map[string][]string{"orders": {`{"id":1,"total":2}`, `{"id":2,"total":3}`}, "items": {`{"sku":"a"}`}}
	// A file source learns its exact columns and key while reading; they win over the test's guess.
	t.Run("stores the columns and key the kind wrote", func(t *testing.T) {
		kind := &fakeKind{discovery: discovery, rows: rows, written: map[string]connector.Table{"orders": {
			Name: "orders", Columns: []connector.Column{{Name: "id", Type: "integer"}, {Name: "total", Type: "float", Nullable: true}},
			PrimaryKey: []string{"id"},
		}}}
		f := newFakeFixture(t, kind, `{"region":""}`)
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil {
			t.Fatal(err)
		}
		total := res.Rows
		orders, items := f.store.tables["orders"], f.store.tables["items"]
		if total != 3 || string(orders.Columns) != `[{"name":"id","type":"integer"},{"name":"total","type":"float","nullable":true}]` ||
			string(orders.PrimaryKey) != `["id"]` || string(items.Columns) != `[]` {
			t.Fatalf("total = %d, orders = %s %s, items = %s", total, orders.Columns, orders.PrimaryKey, items.Columns)
		}
		// Discovery's corrected settings reach Sync but are not stored.
		for _, src := range kind.sources[1:] {
			if string(src.Settings) != `{"region":"eu-west-2"}` || !src.ForSync || src.WorkspaceID != "ws-1" {
				t.Fatalf("sync source = %+v", src)
			}
		}
		if !kind.sources[0].ForSync || string(f.store.connectors["conn-1"].Config) != `{"region":""}` {
			t.Fatalf("discover source = %+v, stored config = %s", kind.sources[0], f.store.connectors["conn-1"].Config)
		}
	})
	t.Run("skips only tables this sync wrote", func(t *testing.T) {
		kind := &fakeKind{discovery: discovery, rows: rows}
		f := newFakeFixture(t, kind, `{}`)
		f.store.tables["orders"] = storedTable("orders", "orders-key", "sync-1")
		f.store.tables["items"] = storedTable("items", "items-key", "sync-0")
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil {
			t.Fatal(err)
		}
		if total := res.Rows; !slices.Equal(kind.synced, []string{"items"}) || total != 5 || f.store.tables["orders"].S3Key != "orders-key" {
			t.Fatalf("synced = %v, total = %d, orders = %+v", kind.synced, res.Rows, f.store.tables["orders"])
		}
	})
	t.Run("stops a table over the byte limit", func(t *testing.T) {
		f := newFakeFixture(t, &fakeKind{discovery: discovery, rows: rows}, `{}`)
		f.syncer.MaxBytes = 10
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "table orders holds more than 10 bytes, the most one table may hold" {
			t.Fatalf("failure = %+v", fl)
		}
	})
	t.Run("retries a failed read", func(t *testing.T) {
		f := newFakeFixture(t, &fakeKind{discovery: discovery, syncErr: errors.New("boom")}, `{}`)
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || fl.Message != "the sync failed; Helix will retry" {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

// s3Bucket serves a path-style bucket of objects: listings and reads.
func s3Bucket(t *testing.T, objects map[string]string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/acme-exports/")
		if r.URL.Query().Get("list-type") == "2" {
			var keys []string
			for k := range objects {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<ListBucketResult><Name>acme-exports</Name>`))
			for _, k := range keys {
				fmt.Fprintf(w, `<Contents><Key>%s</Key><Size>%d</Size><ETag>"%d"</ETag></Contents>`, k, len(objects[k]), len(objects[k]))
			}
			_, _ = w.Write([]byte(`</ListBucketResult>`))
			return
		}
		body, ok := objects[key]
		if !ok || r.Header.Get("If-Match") != fmt.Sprintf(`"%d"`, len(body)) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// TestSyncerS3 runs the s3 kind end to end against a fake bucket: tables
// from keys, a choice, exact columns, and a malformed file.
func TestSyncerS3(t *testing.T) {
	s3Fixture := func(t *testing.T, objects map[string]string, settings string) fixture {
		f := newFixture(t, "http://unused", "s3", settings, plan{code: "pro"})
		endpoints := connector.DefaultEndpoints()
		endpoints.S3 = s3Bucket(t, objects)
		f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{
			MaxSourceBytes: 1 << 20, MaxConnectorBytes: 1 << 20, MaxTableBytes: 1 << 20, TempDir: t.TempDir(),
		}, connector.EventsOptions{})
		return f
	}
	objects := map[string]string{
		"e2e/customers.csv":           "id,name\n1,Ada\n2,Grace\n",
		"e2e/events/2026-10-01.jsonl": "{\"id\":1,\"kind\":\"view\"}\n",
		"e2e/events/2026-10-02.jsonl": "{\"id\":2,\"page\":\"/p\"}\n",
	}
	settings := `{"bucket":"acme-exports","prefix":"e2e/","region":"eu-west-2","keys":{"access_key_id":"AKIAEXAMPLE"}%s}`
	t.Run("syncs the chosen tables with exact columns and drops the rest", func(t *testing.T) {
		f := s3Fixture(t, objects, fmt.Sprintf(settings, ""))
		f.store.tables["old"] = &model.ConnectorTable{TableName: "old", S3Key: "old-key", SyncID: "sync-0"}
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil {
			t.Fatal(err)
		}
		rows := res.Rows
		events := f.store.tables["events"]
		if rows != 4 || len(f.store.tables) != 2 ||
			string(events.Columns) != `[{"name":"id","type":"integer"},{"name":"kind","type":"string","nullable":true},{"name":"page","type":"string","nullable":true}]` ||
			string(events.PrimaryKey) != `["id"]` {
			t.Fatalf("rows = %d, tables = %v, events = %s %s", rows, f.store.tables, events.Columns, events.PrimaryKey)
		}
		if got := lines(t, f.snapshots.objects[events.S3Key]); len(got) != 2 || got[1] != `{"id":2,"kind":null,"page":"/p"}` {
			t.Fatalf("snapshot = %q", got)
		}
		f.store.connectors["conn-1"].Config = json.RawMessage(fmt.Sprintf(settings, `,"tables":["customers"]`))
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.store.tables["events"]; ok || len(f.store.tables) != 1 {
			t.Fatalf("tables = %v", f.store.tables)
		}
	})
	// A stored table now over the source limit fails the sync rather than vanishing.
	t.Run("fails a stored table that turned unavailable", func(t *testing.T) {
		f := s3Fixture(t, objects, fmt.Sprintf(settings, ""))
		f.syncer.Catalog = connector.NewCatalog(connector.Endpoints{S3: s3Bucket(t, objects)}, connector.Guard{AllowPrivate: true}, connector.S3Options{
			MaxSourceBytes: 30, MaxConnectorBytes: 1 << 20, MaxTableBytes: 1 << 20, TempDir: t.TempDir(),
		}, connector.EventsOptions{})
		f.store.tables["events"] = &model.ConnectorTable{TableName: "events", S3Key: "events-key", SyncID: "sync-0"}
		f.snapshots.objects["events-key"] = []byte("x")
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "table events can't be synced: is 44 bytes, over the 30 bytes limit" {
			t.Fatalf("failure = %+v", fl)
		}
		if f.store.tables["events"].S3Key != "events-key" || f.snapshots.objects["events-key"] == nil {
			t.Fatalf("pruned: %v", f.store.tables)
		}
	})
	t.Run("fails for good on a malformed file", func(t *testing.T) {
		bad := maps.Clone(objects)
		bad["e2e/events/2026-10-02.jsonl"] = "{\"id\":2,\n"
		f := s3Fixture(t, bad, fmt.Sprintf(settings, ""))
		_, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != "e2e/events/2026-10-02.jsonl record 1: the file ends inside a record" {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

// changeKind is a fakeKind that can follow its tables: Start hands out
// cursor "c0", or start when set, and each Changes call applies the next scripted change.
type changeKind struct {
	fakeKind
	capture  connector.Capture
	start    json.RawMessage
	starts   int
	changes  []func(w connector.ChangeWriter) error
	cursors  []string
	released []string
	allowed  []connector.Capture
	// releasedWith is the credential the last Release ran with; releaseErr fails it.
	releasedWith string
	releaseErr   error
}

func (k *changeKind) Capture(src connector.Source, _ string) connector.Capture {
	k.allowed = src.Captures
	return k.capture
}

func (k *changeKind) Start(context.Context, connector.Source, []connector.Table) (connector.Start, error) {
	k.starts++
	if k.start != nil {
		return connector.Start{Cursor: k.start}, nil
	}
	return connector.Start{Cursor: json.RawMessage(`"c0"`)}, nil
}

func (k *changeKind) Changes(_ context.Context, _ connector.Source, _ []connector.Table, cursor json.RawMessage, w connector.ChangeWriter) (json.RawMessage, error) {
	k.cursors = append(k.cursors, string(cursor))
	if len(k.changes) == 0 {
		return cursor, nil
	}
	next := k.changes[0]
	k.changes = k.changes[1:]
	if err := next(w); err != nil {
		return nil, err
	}
	return json.RawMessage(fmt.Sprintf(`"c%d"`, len(k.cursors))), nil
}

func (k *changeKind) Release(_ context.Context, src connector.Source, cursor json.RawMessage) error {
	if k.releaseErr != nil {
		return k.releaseErr
	}
	k.released, k.releasedWith = append(k.released, string(cursor)), src.Secret
	return nil
}

func (k *changeKind) Name() string { return "fake" }

// refreshes records the graph refreshes a sync owes, and the spacing each
// asked for; fail refuses them.
type refreshes struct {
	tables [][]string
	every  []time.Duration
	fail   error
}

func (r *refreshes) OweFoundryRefreshes(_ context.Context, _, _ string, tables []string, refreshEvery time.Duration) error {
	if r.fail != nil {
		return r.fail
	}
	r.tables, r.every = append(r.tables, tables), append(r.every, refreshEvery)
	return nil
}

func ordersKind(rows ...string) *changeKind {
	return &changeKind{capture: connector.CaptureCursor, fakeKind: fakeKind{
		discovery: connector.Discovery{Tables: []connector.Table{{Name: "orders", PrimaryKey: []string{"id"}}}},
		rows:      map[string][]string{"orders": rows},
	}}
}

func newChangeFixture(t *testing.T, kind connector.Kind, code string) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "fake", `{}`, plan{code: code})
	f.syncer.Catalog = connector.NewCatalogOf(kind)
	return f
}

// snapshotRows reads the rows connector_tables points at for table, sorted.
func (f fixture) snapshotRows(t *testing.T, table string) []string {
	t.Helper()
	rows := lines(t, f.snapshots.objects[f.store.tables[table].S3Key])
	slices.Sort(rows)
	return rows
}

func TestSyncerChanges(t *testing.T) {
	// A full read compares with the previous snapshot; the batch is what an engine applying changes reads.
	t.Run("records a full read's changes as a batch", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`)
		f := newChangeFixture(t, kind, "free")
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil || !slices.Equal(res.Changed, []string{"orders"}) {
			t.Fatalf("Run = (%+v, %v)", res, err)
		}
		if b := f.store.batches["orders"]; len(b) != 1 || b[0].Kind != model.ConnectorReplaceBatch || b[0].Inserts != 2 {
			t.Fatalf("first batches = %+v", b)
		}
		kind.rows["orders"] = []string{`{"id":2,"n":"B"}`, `{"id":3,"n":"c"}`}
		res, err = f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || !slices.Equal(res.Changed, []string{"orders"}) {
			t.Fatalf("Run = (%+v, %v)", res, err)
		}
		b := f.store.batches["orders"][1]
		if b.Kind != model.ConnectorChangesBatch || b.Inserts != 1 || b.Updates != 1 || b.Deletes != 1 || b.Seq != 2 || b.S3Key == nil {
			t.Fatalf("second batch = %+v", b)
		}
		got := lines(t, f.snapshots.objects[*b.S3Key])
		slices.Sort(got)
		want := []string{`{"op":"delete","key":{"id":1}}`, `{"op":"upsert","key":{"id":2},"row":{"id":2,"n":"B"}}`, `{"op":"upsert","key":{"id":3},"row":{"id":3,"n":"c"}}`}
		if !slices.Equal(got, want) {
			t.Fatalf("batch file = %v", got)
		}
		if len(f.store.replaced) != 1 {
			t.Fatalf("replaced = %v", f.store.replaced)
		}
	})
	// Free reads in full every time and keeps no cursor, even from a kind that could follow changes.
	t.Run("never follows changes on a plan without the capture method", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "free")
		for _, id := range []string{"sync-1", "sync-2"} {
			if _, err := f.syncer.Run(context.Background(), newSync(id)); err != nil {
				t.Fatal(err)
			}
		}
		if kind.starts != 0 || len(kind.cursors) != 0 || f.store.connectors["conn-1"].ChangeCursor != nil {
			t.Fatalf("starts = %d, cursors = %v", kind.starts, kind.cursors)
		}
	})
	// A database kind falls back from its log to a cursor column only if it knows the plan dropped the log.
	t.Run("tells the kind the capture methods the plan allows", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		want := []connector.Capture{connector.CaptureFull, connector.CaptureCursor, connector.CaptureEvents}
		if !slices.Equal(kind.allowed, want) {
			t.Fatalf("Captures = %v, want %v", kind.allowed, want)
		}
	})
	// The cursor is taken before the full read, so a change made during it is read again next time.
	// A first read's Start can take a slot; failing for good afterwards stores
	// no cursor the release tick could find it by.
	t.Run("gives back what a first read took when it fails for good", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.syncErr = &connector.InvalidError{Message: "table orders is over the limit"}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err == nil {
			t.Fatal("the read succeeded")
		}
		if kind.starts != 1 || !slices.Equal(kind.released, []string{""}) {
			t.Fatalf("starts = %d, released = %q", kind.starts, kind.released)
		}
	})
	t.Run("keeps what a first read took while it may be retried", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.syncErr = &connector.RefusedError{Message: "the database is restarting"}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err == nil {
			t.Fatal("the read succeeded")
		}
		if len(kind.released) != 0 {
			t.Fatalf("released = %q", kind.released)
		}
	})
	// A merge matches rows by the stored key; under a new key its deletes
	// would name the wrong fields, so the sync reads the table in full.
	t.Run("reads in full when a followed table's key changed", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		kind.discovery.Tables[0].PrimaryKey = []string{"id", "n"}
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || res.Mode != model.ConnectorSyncFull || len(kind.cursors) != 0 {
			t.Fatalf("Run = (%+v, %v), change reads = %v", res, err, kind.cursors)
		}
		if got := prevKey(f.store.tables["orders"]); !slices.Equal(got, []string{"id", "n"}) {
			t.Fatalf("stored key = %v", got)
		}
	})
	// Widening a column leaves every row's bytes as they were; generation
	// must still see the new type.
	t.Run("records a new schema over unchanged rows", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`)
		kind.written = map[string]connector.Table{"orders": {Name: "orders", PrimaryKey: []string{"id"},
			Columns: []connector.Column{{Name: "id", Type: "integer"}, {Name: "n", Type: "text"}}}}
		f := newChangeFixture(t, kind, "free")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		key := f.store.tables["orders"].S3Key
		kind.written["orders"] = connector.Table{Name: "orders", PrimaryKey: []string{"id"},
			Columns: []connector.Column{{Name: "id", Type: "bigint"}, {Name: "n", Type: "text"}}}
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || len(res.Changed) != 0 {
			t.Fatalf("Run = (%+v, %v)", res, err)
		}
		stored := f.store.tables["orders"]
		if stored.S3Key != key || !strings.Contains(string(stored.Columns), `"bigint"`) || len(f.store.batches["orders"]) != 1 {
			t.Fatalf("table = %+v, batches = %d", stored, len(f.store.batches["orders"]))
		}
	})
	// The kind names a version column; the merge must use it, or a replayed
	// export would undo newer rows.
	t.Run("keeps stored rows newer than a change read's", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"new","v":5}`, `{"id":2,"n":"b","v":1}`)
		kind.discovery.Tables[0].Version = "v"
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			if err := w.UpsertJSON("orders", []byte(`{"id":1,"n":"stale","v":4}`)); err != nil {
				return err
			}
			return w.(connector.VersionedWriter).DeleteVersion("orders", map[string]any{"id": 2}, 2)
		}}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil {
			t.Fatal(err)
		}
		if got := f.snapshotRows(t, "orders"); !slices.Equal(got, []string{`{"id":1,"n":"new","v":5}`}) {
			t.Fatalf("rows = %v", got)
		}
	})
	t.Run("reads only changes once a cursor exists", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			if err := w.UpsertJSON("orders", []byte(`{"id":2,"n":"B"}`)); err != nil {
				return err
			}
			return w.Delete("orders", map[string]any{"id": 1})
		}}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if kind.starts != 1 || string(f.store.connectors["conn-1"].ChangeCursor) != `"c0"` {
			t.Fatalf("starts = %d, cursor = %s", kind.starts, f.store.connectors["conn-1"].ChangeCursor)
		}
		kind.synced = nil
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil {
			t.Fatal(err)
		}
		if res.Mode != model.ConnectorSyncChanges || len(kind.synced) != 0 || !slices.Equal(kind.cursors, []string{`"c0"`}) {
			t.Fatalf("mode = %s, synced = %v, cursors = %v", res.Mode, kind.synced, kind.cursors)
		}
		if got := f.snapshotRows(t, "orders"); !slices.Equal(got, []string{`{"id":2,"n":"B"}`}) {
			t.Fatalf("rows = %v", got)
		}
		b := f.store.batches["orders"][1]
		if b.Updates != 1 || b.Deletes != 1 || string(f.store.connectors["conn-1"].ChangeCursor) != `"c1"` {
			t.Fatalf("batch = %+v, cursor = %s", b, f.store.connectors["conn-1"].ChangeCursor)
		}
	})
	// An overlap re-read returns rows that did not change; it must not refresh a graph.
	t.Run("writes nothing when the changes change nothing", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			return w.UpsertJSON("orders", []byte(`{"id":1,"n":"a"}`))
		}}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		key := f.store.tables["orders"].S3Key
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || len(res.Changed) != 0 || f.store.tables["orders"].S3Key != key || len(f.store.batches["orders"]) != 1 {
			t.Fatalf("Run = (%+v, %v), batches = %v", res, err, f.store.batches["orders"])
		}
	})
	// The daily full read is what catches deletes a change read can't see.
	t.Run("reads in full when the reconcile is due", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-25 * time.Hour)
		f.store.tables["orders"].ReconciledAt = &old
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 || len(kind.cursors) != 0 {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	t.Run("reads in full when asked to", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		full := newSync("sync-2")
		full.Mode = model.ConnectorSyncFull
		if res, err := f.syncer.Run(context.Background(), full); err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 {
			t.Fatalf("Run = (%+v, %v)", res, err)
		}
	})
	// A source that no longer holds the changes after the cursor starts over from a full read.
	t.Run("starts over when the cursor expired", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.changes = []func(connector.ChangeWriter) error{func(connector.ChangeWriter) error { return connector.ErrCursorExpired }}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 || string(f.store.connectors["conn-1"].ChangeCursor) != `"c0"` {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	// A purged binlog or a dropped slot is why every table was copied again,
	// so the connector's CDC status must say so after the full read.
	t.Run("shows a lost position as the last reset", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.start = json.RawMessage(`{"cdc":{"position":"p0"}}`)
		kind.changes = []func(connector.ChangeWriter) error{func(connector.ChangeWriter) error {
			return &connector.PositionLostError{Reason: "the binlog was purged"}
		}}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil {
			t.Fatal(err)
		}
		var cursor struct {
			CDC struct {
				Position    string     `json:"position"`
				LastResetAt *time.Time `json:"last_reset_at"`
				LastReset   string     `json:"last_reset"`
			} `json:"cdc"`
		}
		stored := f.store.connectors["conn-1"].ChangeCursor
		if err := json.Unmarshal(stored, &cursor); err != nil || cursor.CDC.Position != "p0" ||
			cursor.CDC.LastReset != "the binlog was purged" || cursor.CDC.LastResetAt == nil {
			t.Fatalf("cursor = %s", stored)
		}
	})
	// A truncate or a schema change can't be followed row by row; that table is read whole.
	t.Run("reads a table in full when the reader resets it", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			w.Reset("orders", "truncated")
			return nil
		}}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		kind.synced, kind.rows["orders"] = nil, []string{`{"id":9}`}
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || !slices.Equal(kind.synced, []string{"orders"}) || !slices.Equal(f.snapshotRows(t, "orders"), []string{`{"id":9}`}) {
			t.Fatalf("Run = (%+v, %v), synced = %v", res, err, kind.synced)
		}
		if res.Mode != model.ConnectorSyncChanges || string(f.store.connectors["conn-1"].ChangeCursor) != `"c1"` {
			t.Fatalf("mode = %s, cursor = %s", res.Mode, f.store.connectors["conn-1"].ChangeCursor)
		}
	})
	// A downgrade stops change reads; a replication slot left behind would fill the customer's disk.
	t.Run("gives back what the reader held when the plan stops allowing it", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		f.syncer.Plans = plan{code: "free"}
		if _, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.released, []string{`"c0"`}) || f.store.connectors["conn-1"].ChangeCursor != nil {
			t.Fatalf("released = %v, cursor = %s", kind.released, f.store.connectors["conn-1"].ChangeCursor)
		}
	})
	// A deleted or paused connector's replication slot keeps WAL on the
	// customer's database until something drops it.
	t.Run("releases a stranded cursor with the stored credential and forgets it", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if err := f.syncer.Release(context.Background(), f.store.connectors["conn-1"]); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.released, []string{`"c0"`}) || kind.releasedWith != "sk_test_abc" || f.store.connectors["conn-1"].ChangeCursor != nil {
			t.Fatalf("released = %v with %q, cursor = %s", kind.released, kind.releasedWith, f.store.connectors["conn-1"].ChangeCursor)
		}
	})
	t.Run("keeps the cursor when the source refuses the release", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		kind.releaseErr = errors.New("connection refused")
		if err := f.syncer.Release(context.Background(), f.store.connectors["conn-1"]); err == nil {
			t.Fatal("Release succeeded against a refusing source")
		}
		if f.store.connectors["conn-1"].ChangeCursor == nil {
			t.Fatal("the cursor was forgotten though its slot may still exist")
		}
	})
	t.Run("lists only kinds that hold something on their source", func(t *testing.T) {
		f := newChangeFixture(t, ordersKind(), "pro")
		if got := f.syncer.ReleaserKinds(); !slices.Equal(got, []string{"fake"}) {
			t.Fatalf("ReleaserKinds = %v", got)
		}
		f.syncer.Catalog = connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
		if got := f.syncer.ReleaserKinds(); !slices.Equal(got, []string{"postgres"}) {
			t.Fatalf("default ReleaserKinds = %v, want [postgres]", got)
		}
	})
	// A retry reads past the first attempt's cursor, so a table that attempt
	// merged is merged again; skipping it would lose the changes in between.
	t.Run("merges a followed table again on a retry", func(t *testing.T) {
		kind := ordersKind(`{"id":1,"n":"a"}`)
		kind.changes = []func(connector.ChangeWriter) error{
			func(w connector.ChangeWriter) error { return w.UpsertJSON("orders", []byte(`{"id":2,"n":"b"}`)) },
			func(w connector.ChangeWriter) error {
				if err := w.UpsertJSON("orders", []byte(`{"id":2,"n":"b"}`)); err != nil {
					return err
				}
				return w.UpsertJSON("orders", []byte(`{"id":3,"n":"c"}`))
			},
		}
		f := newChangeFixture(t, kind, "pro")
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		retried := newSync("sync-2")
		f.store.failPrune = true
		if _, err := f.syncer.Run(context.Background(), retried); err == nil {
			t.Fatal("the first attempt succeeded, want it to fail after writing orders")
		}
		if _, err := f.syncer.Run(context.Background(), retried); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(kind.cursors, []string{`"c0"`, `"c0"`}) {
			t.Fatalf("cursors = %v, want the uncommitted cursor read twice", kind.cursors)
		}
		if got := f.snapshotRows(t, "orders"); !slices.Equal(got, []string{`{"id":1,"n":"a"}`, `{"id":2,"n":"b"}`, `{"id":3,"n":"c"}`}) {
			t.Fatalf("rows = %v", got)
		}
		if b := f.store.batches["orders"]; len(b) != 3 || b[2].Inserts != 1 || b[2].Updates != 0 || b[2].Seq != 3 {
			t.Fatalf("batches = %+v", b)
		}
	})
	// A table without a key can't take changes by key; it is read whole even from a kind that follows changes.
	t.Run("reads a keyless table in full every sync", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.discovery.Tables[0].PrimaryKey = nil
		f := newChangeFixture(t, kind, "pro")
		for _, id := range []string{"sync-1", "sync-2"} {
			if _, err := f.syncer.Run(context.Background(), newSync(id)); err != nil {
				t.Fatal(err)
			}
		}
		if kind.starts != 0 || len(kind.cursors) != 0 || len(kind.synced) != 2 {
			t.Fatalf("starts = %d, cursors = %v, synced = %v", kind.starts, kind.cursors, kind.synced)
		}
	})
	// Only a table whose content moved makes the graphs that read it stale.
	t.Run("owes refreshes for the tables it changed", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "free")
		owed := &refreshes{}
		f.syncer.Refresh = owed
		for _, id := range []string{"sync-1", "sync-2"} {
			if _, err := f.syncer.Run(context.Background(), newSync(id)); err != nil {
				t.Fatal(err)
			}
		}
		if len(owed.tables) != 1 || !slices.Equal(owed.tables[0], []string{"orders"}) {
			t.Fatalf("owed = %v", owed.tables)
		}
	})
	// The retry keeps the table the first attempt wrote, so only that
	// attempt's batch says the graph is stale.
	t.Run("owes refreshes on a retry for the tables an earlier attempt changed", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		f := newChangeFixture(t, kind, "free")
		owed := &refreshes{fail: errors.New("database is down")}
		f.syncer.Refresh = owed
		if _, err := f.syncer.Run(context.Background(), f.sync); err == nil {
			t.Fatal("the first attempt succeeded without marking its refreshes")
		}
		owed.fail = nil
		f.sync.Attempts = 2
		if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
			t.Fatal(err)
		}
		if len(owed.tables) != 1 || !slices.Equal(owed.tables[0], []string{"orders"}) {
			t.Fatalf("owed = %v", owed.tables)
		}
	})
}

// pushedKind is a changeKind whose rows Helix holds: it follows its tables
// by events whatever the plan allows, and reports backlog after each sync.
type pushedKind struct {
	changeKind
	backlog    connector.Backlog
	backlogErr error
	// asked are the cursors Backlog was asked about, and stored the cursor
	// the connector held at each ask, read through storedCursor.
	asked, stored []string
	storedCursor  func() json.RawMessage
}

func (k *pushedKind) Backlog(_ context.Context, _ connector.Source, cursor json.RawMessage) (connector.Backlog, error) {
	k.asked = append(k.asked, string(cursor))
	if k.storedCursor != nil {
		k.stored = append(k.stored, string(k.storedCursor()))
	}
	return k.backlog, k.backlogErr
}

// newPushedKind follows one table, events, whose first read holds rows; its
// change reads upsert a piece each, as many times as scripted.
func newPushedKind(changes int, rows ...string) *pushedKind {
	k := &pushedKind{changeKind: changeKind{capture: connector.CaptureEvents, fakeKind: fakeKind{
		discovery: connector.Discovery{Tables: []connector.Table{{Name: "events", PrimaryKey: []string{"id"}}}},
		rows:      map[string][]string{"events": rows},
	}}}
	for i := range changes {
		k.changes = append(k.changes, func(w connector.ChangeWriter) error {
			return w.UpsertJSON("events", []byte(fmt.Sprintf(`{"id":"p%d"}`, i)))
		})
	}
	return k
}

func TestSyncerPushed(t *testing.T) {
	// afterFirstSync is a pushed kind on plan code that has had its first,
	// full, sync and holds cursor "c0".
	afterFirstSync := func(t *testing.T, code string) (fixture, *pushedKind) {
		t.Helper()
		kind := newPushedKind(1, `{"id":"a"}`)
		f := newChangeFixture(t, kind, code)
		if res, err := f.syncer.Run(context.Background(), f.sync); err != nil || res.Mode != model.ConnectorSyncFull {
			t.Fatalf("first sync = (%+v, %v)", res, err)
		}
		return f, kind
	}
	// Free's only capture is full, and it reconciles never; rows that reach
	// Helix only through a change read must still be read on Free.
	t.Run("follows a pushed kind on Free", func(t *testing.T) {
		f, kind := afterFirstSync(t, "free")
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || res.Mode != model.ConnectorSyncChanges || kind.starts != 1 || !slices.Equal(kind.cursors, []string{`"c0"`}) {
			t.Fatalf("Run = (%+v, %v), starts = %d, cursors = %v", res, err, kind.starts, kind.cursors)
		}
		if !slices.Equal(kind.allowed, []connector.Capture{connector.CaptureFull}) || !slices.Equal(f.snapshotRows(t, "events"), []string{`{"id":"a"}`, `{"id":"p0"}`}) {
			t.Fatalf("allowed = %v, rows = %v", kind.allowed, f.snapshotRows(t, "events"))
		}
	})
	// Nothing changes at a source behind Helix's back, so a full read would
	// only rewrite the table; the reconcile a plan schedules never runs.
	t.Run("never reconciles a pushed kind", func(t *testing.T) {
		f, kind := afterFirstSync(t, "max")
		kind.changes = append(kind.changes, kind.changes[0])
		old := time.Now().Add(-25 * time.Hour)
		f.store.tables["events"].ReconciledAt = &old
		if res, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil || res.Mode != model.ConnectorSyncChanges {
			t.Fatalf("Run with a reconcile due = (%+v, %v)", res, err)
		}
		f.store.tables["events"].ReconciledAt = nil
		if res, err := f.syncer.Run(context.Background(), newSync("sync-3")); err != nil || res.Mode != model.ConnectorSyncChanges || kind.starts != 1 {
			t.Fatalf("Run never reconciled = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	// The reads that need the whole table still happen: a full sync asked
	// for, a lost cursor, a table never synced, or one stored before digests.
	t.Run("reads a pushed kind in full when asked to", func(t *testing.T) {
		f, kind := afterFirstSync(t, "pro")
		full := newSync("sync-2")
		full.Mode = model.ConnectorSyncFull
		if res, err := f.syncer.Run(context.Background(), full); err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	t.Run("reads a pushed kind in full without a cursor", func(t *testing.T) {
		f, kind := afterFirstSync(t, "pro")
		f.store.connectors["conn-1"].ChangeCursor = nil
		if res, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	t.Run("reads a pushed kind in full when a table is new", func(t *testing.T) {
		f, kind := afterFirstSync(t, "pro")
		kind.discovery.Tables = append(kind.discovery.Tables, connector.Table{Name: "more", PrimaryKey: []string{"id"}})
		if res, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	t.Run("reads a pushed kind in full when its table has no digest", func(t *testing.T) {
		f, kind := afterFirstSync(t, "pro")
		f.store.tables["events"].ContentDigest = nil
		if res, err := f.syncer.Run(context.Background(), newSync("sync-2")); err != nil || res.Mode != model.ConnectorSyncFull || kind.starts != 2 {
			t.Fatalf("Run = (%+v, %v), starts = %d", res, err, kind.starts)
		}
	})
	// The worker schedules the next sync by what waits beyond the cursor this
	// one stored; asked before it is stored, the batches just merged would
	// count as waiting.
	t.Run("reports the backlog beyond the stored cursor", func(t *testing.T) {
		kind := newPushedKind(1, `{"id":"a"}`)
		kind.backlog = connector.BacklogMore
		f := newChangeFixture(t, kind, "pro")
		kind.storedCursor = func() json.RawMessage { return f.store.connectors["conn-1"].ChangeCursor }
		res, err := f.syncer.Run(context.Background(), f.sync)
		if err != nil || res.Pushed == nil || *res.Pushed != (PushedResult{Backlog: connector.BacklogMore, Rows: 1}) {
			t.Fatalf("first sync = (%+v, %v)", res, err)
		}
		kind.backlog = connector.BacklogNone
		res, err = f.syncer.Run(context.Background(), newSync("sync-2"))
		if err != nil || res.Pushed == nil || *res.Pushed != (PushedResult{Backlog: connector.BacklogNone, Rows: 2}) {
			t.Fatalf("second sync = (%+v, %v)", res, err)
		}
		if want := []string{`"c0"`, `"c1"`}; !slices.Equal(kind.asked, want) || !slices.Equal(kind.stored, want) {
			t.Fatalf("asked = %v, stored when asked = %v", kind.asked, kind.stored)
		}
	})
	t.Run("reports no backlog for a kind Helix does not hold", func(t *testing.T) {
		f := newChangeFixture(t, ordersKind(`{"id":1}`), "pro")
		for _, id := range []string{"sync-1", "sync-2"} {
			if res, err := f.syncer.Run(context.Background(), newSync(id)); err != nil || res.Pushed != nil {
				t.Fatalf("Run = (%+v, %v)", res, err)
			}
		}
	})
	// A failed backlog read is retried, but what the sync merged is in: the
	// cursor is stored and the refreshes owed, so a retry merges nothing twice
	// and no graph misses a refresh if the retries run out.
	t.Run("retries a failed backlog read once the sync is stored", func(t *testing.T) {
		kind := newPushedKind(0, `{"id":"a"}`)
		kind.backlogErr = errors.New("connection reset")
		f := newChangeFixture(t, kind, "pro")
		owed := &refreshes{}
		f.syncer.Refresh = owed
		res, err := f.syncer.Run(context.Background(), f.sync)
		if fl := failure(t, err); fl.Permanent || res.Pushed != nil {
			t.Fatalf("failure = %+v, pushed = %+v", fl, res.Pushed)
		}
		if string(f.store.connectors["conn-1"].ChangeCursor) != `"c0"` || len(owed.tables) != 1 {
			t.Fatalf("cursor = %s, owed = %v", f.store.connectors["conn-1"].ChangeCursor, owed.tables)
		}
	})
	// A sync that fails before it publishes its table counts no rows, though
	// its merge read them all: the worker spaces the source's next sync by
	// the rows its table held, so a merge that keeps failing runs no more
	// often than one that succeeds.
	t.Run("reports the rows its table held when it fails before publishing", func(t *testing.T) {
		f, _ := afterFirstSync(t, "pro")
		delete(f.snapshots.objects, f.store.tables["events"].S3Key)
		res, err := f.syncer.Run(context.Background(), newSync("sync-2"))
		if fl := failure(t, err); fl.Permanent || res.Rows != 0 || res.StoredRows != 1 {
			t.Fatalf("failure = %+v, rows = %d, stored rows = %d", fl, res.Rows, res.StoredRows)
		}
	})
	// A refresh applies only what changed, so a pushed source's table, however
	// large, is refreshed on its plan's spacing like any other kind's.
	t.Run("spaces every kind's refreshes by the plan alone", func(t *testing.T) {
		pushed := newChangeFixture(t, newPushedKind(0, `{"id":"a"}`, `{"id":"b"}`, `{"id":"c"}`, `{"id":"d"}`), "pro")
		other := newChangeFixture(t, ordersKind(`{"id":1}`, `{"id":2}`, `{"id":3}`, `{"id":4}`), "pro")
		for _, f := range []fixture{pushed, other} {
			f.syncer.Refresh = &refreshes{}
			if _, err := f.syncer.Run(context.Background(), f.sync); err != nil {
				t.Fatal(err)
			}
			if got := f.syncer.Refresh.(*refreshes).every; !slices.Equal(got, []time.Duration{time.Hour}) {
				t.Fatalf("spacing = %v", got)
			}
		}
	})
}

func TestSyncerDenseTables(t *testing.T) {
	var (
		id = connector.Column{Name: "id", Type: "string"}
		x  = connector.Column{Name: "x", Type: "number"}
	)
	// eventsKind follows one Dense table, events, whose first read holds
	// piece a with the typed columns id and x.
	eventsKind := func(changes ...func(connector.ChangeWriter) error) *changeKind {
		table := connector.Table{Name: "events", PrimaryKey: []string{"id"}, Dense: true}
		written := table
		written.Columns = []connector.Column{id, x}
		return &changeKind{capture: connector.CaptureEvents, changes: changes, fakeKind: fakeKind{
			discovery: connector.Discovery{Tables: []connector.Table{table}},
			rows:      map[string][]string{"events": {`{"id":"a","x":1}`}},
			written:   map[string]connector.Table{"events": written},
		}}
	}
	// declare is a change read that declares columns, then upserts rows.
	declare := func(columns []connector.Column, rows ...string) func(connector.ChangeWriter) error {
		return func(w connector.ChangeWriter) error {
			w.(connector.ColumnWriter).Columns("events", columns)
			for _, r := range rows {
				if err := w.UpsertJSON("events", []byte(r)); err != nil {
					return err
				}
			}
			return nil
		}
	}
	syncs := func(t *testing.T, f fixture, ids ...string) {
		t.Helper()
		for _, id := range ids {
			if _, err := f.syncer.Run(context.Background(), newSync(id)); err != nil {
				t.Fatalf("%s: %v", id, err)
			}
		}
	}
	// The engine catalogs a table from its first rows, which are its oldest:
	// a field first sent after them must reach them as null, and generation
	// reads its type from the stored columns.
	t.Run("pads old rows with a field added between change reads", func(t *testing.T) {
		y, z := connector.Column{Name: "y", Type: "string"}, connector.Column{Name: "z", Type: "boolean"}
		kind := eventsKind(
			declare([]connector.Column{id, x, y}, `{"id":"b","x":2,"y":"s"}`),
			declare([]connector.Column{id, x, y, z}, `{"id":"c","x":3,"y":null,"z":true}`),
		)
		f := newChangeFixture(t, kind, "pro")
		syncs(t, f, "sync-1", "sync-2")
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, []string{`{"id":"a","x":1,"y":null}`, `{"id":"b","x":2,"y":"s"}`}) {
			t.Fatalf("rows after one change read = %v", got)
		}
		syncs(t, f, "sync-3")
		want := []string{`{"id":"a","x":1,"y":null,"z":null}`, `{"id":"b","x":2,"y":"s","z":null}`, `{"id":"c","x":3,"y":null,"z":true}`}
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, want) {
			t.Fatalf("rows after two change reads = %v", got)
		}
		events := f.store.tables["events"]
		if string(events.Columns) != `[{"name":"id","type":"string"},{"name":"x","type":"number"},{"name":"y","type":"string"},{"name":"z","type":"boolean"}]` {
			t.Fatalf("columns = %s", events.Columns)
		}
		// Padding is not a change: the batch holds the one new piece.
		if b := f.store.batches["events"]; len(b) != 3 || b[2].Inserts != 1 || b[2].Updates != 0 || b[2].Deletes != 0 {
			t.Fatalf("batches = %+v", b)
		}
	})
	// A field sent only as null has no type yet; once a value locks it, the
	// stored column must say so, or generation maps it as untyped.
	t.Run("gives a column stored as null its locked type", func(t *testing.T) {
		kind := eventsKind(
			declare([]connector.Column{id, x, {Name: "y", Type: "null"}}, `{"id":"b","x":2,"y":null}`),
			declare([]connector.Column{id, x, {Name: "y", Type: "string"}}, `{"id":"c","x":3,"y":"s"}`),
		)
		f := newChangeFixture(t, kind, "pro")
		syncs(t, f, "sync-1", "sync-2")
		if got := string(f.store.tables["events"].Columns); !strings.Contains(got, `{"name":"y","type":"null"}`) {
			t.Fatalf("columns after the null = %s", got)
		}
		syncs(t, f, "sync-3")
		if got := string(f.store.tables["events"].Columns); got != `[{"name":"id","type":"string"},{"name":"x","type":"number"},{"name":"y","type":"string"}]` {
			t.Fatalf("columns after the value = %s", got)
		}
	})
	// Declared columns are a Dense table's contract; a table without it keeps
	// its rows as the source sent them and its columns as merged before.
	t.Run("leaves a table that is not Dense as merged before", func(t *testing.T) {
		kind := ordersKind(`{"id":1}`)
		kind.changes = []func(connector.ChangeWriter) error{func(w connector.ChangeWriter) error {
			w.(connector.ColumnWriter).Columns("orders", []connector.Column{{Name: "id", Type: "integer"}, {Name: "n", Type: "text"}, {Name: "m"}})
			return w.Upsert("orders", map[string]any{"id": 2, "n": "b"})
		}}
		f := newChangeFixture(t, kind, "pro")
		syncs(t, f, "sync-1", "sync-2")
		if got := f.snapshotRows(t, "orders"); !slices.Equal(got, []string{`{"id":1}`, `{"id":2,"n":"b"}`}) {
			t.Fatalf("rows = %v", got)
		}
		if got := string(f.store.tables["orders"].Columns); got != `[{"name":"id"},{"name":"n"}]` {
			t.Fatalf("columns = %s", got)
		}
	})
	// A read that declares nothing must not wipe the stored columns.
	t.Run("keeps a Dense table's columns when its read declares none", func(t *testing.T) {
		kind := eventsKind(func(w connector.ChangeWriter) error { return w.UpsertJSON("events", []byte(`{"id":"b","x":2}`)) })
		f := newChangeFixture(t, kind, "pro")
		syncs(t, f, "sync-1", "sync-2")
		if got := string(f.store.tables["events"].Columns); got != `[{"name":"id","type":"string"},{"name":"x","type":"number"}]` {
			t.Fatalf("columns = %s", got)
		}
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, []string{`{"id":"a","x":1}`, `{"id":"b","x":2}`}) {
			t.Fatalf("rows = %v", got)
		}
	})
}

// eventLog is an event source's batches as the provider holds them, beside
// the syncer's store: the cursor and the snapshot key it reports are the
// store's, as one consistent read of connectors and connector_tables gives.
type eventLog struct {
	store   *memoryStore
	objects *memorySnapshots
	schema  events.Schema
	batches []events.Batch
}

func (l *eventLog) EventTable(_ context.Context, connectorID string) (events.Schema, int64, string, error) {
	var cursor struct{ Seq int64 }
	if c := l.store.connectors[connectorID].ChangeCursor; c != nil {
		if err := json.Unmarshal(c, &cursor); err != nil {
			return events.Schema{}, 0, "", err
		}
	}
	key := ""
	if t := l.store.tables["events"]; t != nil {
		key = t.S3Key
	}
	return l.schema, cursor.Seq, key, nil
}

func (l *eventLog) waiting(after int64, limit int) []events.Batch {
	var out []events.Batch
	for _, b := range l.batches {
		if b.Seq > after && len(out) < limit {
			out = append(out, b)
		}
	}
	return out
}

func (l *eventLog) EventBatchesAfter(_ context.Context, _ string, after int64, budget events.ReadBudget) (events.Schema, []events.Batch, error) {
	return l.schema, budget.Cut(l.schema, l.waiting(after, budget.Batches)), nil
}

func (l *eventLog) EventBacklog(_ context.Context, _ string, after int64, budget events.ReadBudget) (connector.Backlog, error) {
	return budget.Backlog(l.schema, l.waiting(after, budget.Batches+1)), nil
}

// send stores pieces, each "<id> <data>", as the next batch, received at
// 12:00:0<seq>, its fields merged into the schema as a send does.
func (l *eventLog) send(t *testing.T, pieces ...string) {
	t.Helper()
	var in []events.Piece
	for _, p := range pieces {
		id, data, _ := strings.Cut(p, " ")
		in = append(in, events.Piece{ID: id, Data: []byte(data)})
	}
	send, err := events.Validate(in, events.Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := l.schema.Merge(send, 200)
	if err != nil {
		t.Fatal(err)
	}
	seq := int64(len(l.batches) + 1)
	obj, err := events.EncodeLines(send, time.Date(2026, 10, 24, 12, 0, int(seq), 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	key := fmt.Sprintf("ws/ws-1/events/conn-1/batch-%d.jsonl.gz", seq)
	l.objects.objects[key] = obj.Body
	l.batches = append(l.batches, events.Batch{ID: fmt.Sprintf("batch-%d", seq), Seq: seq, Key: key, Pieces: send.Pieces(), Bytes: send.Bytes()})
	l.schema = merged.Schema
}

func TestSyncerEventSource(t *testing.T) {
	ctx := context.Background()
	wide := events.ReadBudget{Batches: 1000, Pieces: 50000, Bytes: 128 << 20}
	// source is an event source on Free, whose only capture is full, synced
	// by the real events kind.
	source := func(t *testing.T, budget events.ReadBudget) (fixture, *eventLog) {
		t.Helper()
		f := newFixture(t, "http://unused", "events", `{"description":"orders"}`, plan{code: "free"})
		log := &eventLog{store: f.store, objects: f.snapshots}
		f.syncer.Catalog = connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{},
			connector.EventsOptions{Batches: log, Objects: f.snapshots, Budget: budget})
		return f, log
	}
	run := func(t *testing.T, f fixture, sync *model.ConnectorSync) Result {
		t.Helper()
		res, err := f.syncer.Run(ctx, sync)
		if err != nil {
			t.Fatalf("%s: %v", sync.ID, err)
		}
		return res
	}
	// The first sync replays the batches into the table; the next merges a
	// later piece by id, pads the row it did not touch with the field the
	// piece added, and stores each column's type for generation. The table
	// a full read then builds is the same, row for row.
	t.Run("merges pieces by id and pads earlier rows with later fields", func(t *testing.T) {
		f, log := source(t, wide)
		log.send(t, `a {"x":1}`, `b {"x":2}`)
		res := run(t, f, newSync("sync-1"))
		if res.Mode != model.ConnectorSyncFull || res.Pushed == nil || *res.Pushed != (PushedResult{Backlog: connector.BacklogNone, Rows: 2}) {
			t.Fatalf("first sync = %+v", res)
		}
		log.send(t, `a {"x":3,"note":"late"}`)
		res = run(t, f, newSync("sync-2"))
		if res.Mode != model.ConnectorSyncChanges || *res.Pushed != (PushedResult{Backlog: connector.BacklogNone, Rows: 2}) ||
			string(f.store.connectors["conn-1"].ChangeCursor) != `{"seq":2}` {
			t.Fatalf("second sync = %+v, cursor %s", res, f.store.connectors["conn-1"].ChangeCursor)
		}
		want := []string{
			`{"id":"a","received_at":"2026-10-24T12:00:02.000000Z","occurred_at":null,"note":"late","x":3}`,
			`{"id":"b","received_at":"2026-10-24T12:00:01.000000Z","occurred_at":null,"x":2,"note":null}`,
		}
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, want) {
			t.Fatalf("rows = %q", got)
		}
		columns := `[{"name":"id","type":"string"},{"name":"received_at","type":"string"},{"name":"occurred_at","type":"string","nullable":true},` +
			`{"name":"note","type":"string","nullable":true},{"name":"x","type":"number","nullable":true}]`
		if got := string(f.store.tables["events"].Columns); got != columns {
			t.Fatalf("columns = %s", got)
		}
		if b := f.store.batches["events"]; len(b) != 2 || b[1].Inserts != 0 || b[1].Updates != 1 || b[1].Deletes != 0 {
			t.Fatalf("batches = %+v", b)
		}
		full := newSync("sync-3")
		full.Mode = model.ConnectorSyncFull
		if res := run(t, f, full); res.Mode != model.ConnectorSyncFull || len(res.Changed) != 0 {
			t.Fatalf("full sync = %+v", res)
		}
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, want) {
			t.Fatalf("rows after a full read = %q", got)
		}
	})
	// A backlog longer than one read is merged over several syncs, each
	// saying whether more waits, and no piece is lost on the way.
	t.Run("takes a backlog over several syncs", func(t *testing.T) {
		f, log := source(t, events.ReadBudget{Batches: 1, Pieces: wide.Pieces, Bytes: wide.Bytes})
		log.send(t, `a {"x":1}`)
		log.send(t, `b {"x":2}`)
		log.send(t, `c {"x":3}`)
		for i, want := range []connector.Backlog{connector.BacklogMore, connector.BacklogSome, connector.BacklogNone} {
			if res := run(t, f, newSync(fmt.Sprintf("sync-%d", i+1))); res.Pushed == nil || res.Pushed.Backlog != want {
				t.Fatalf("sync %d = %+v, want backlog %v", i+1, res, want)
			}
		}
		want := []string{
			`{"id":"a","received_at":"2026-10-24T12:00:01.000000Z","occurred_at":null,"x":1}`,
			`{"id":"b","received_at":"2026-10-24T12:00:02.000000Z","occurred_at":null,"x":2}`,
			`{"id":"c","received_at":"2026-10-24T12:00:03.000000Z","occurred_at":null,"x":3}`,
		}
		if got := f.snapshotRows(t, "events"); !slices.Equal(got, want) || string(f.store.connectors["conn-1"].ChangeCursor) != `{"seq":3}` {
			t.Fatalf("rows = %q, cursor %s", got, f.store.connectors["conn-1"].ChangeCursor)
		}
	})
}

// TestSyncerEventSourceDocuments runs an API source with a text field
// through the syncer and the real events kind, keeping its newest 2 pieces:
// they become Markdown documents of its Pieces set, each sync keeps the set
// to its window in the publish that adds past it, an identical resend moves
// in place, a spent allowance pauses the set and the next sync with room
// catches up in full, and clearing the text field empties the set.
func TestSyncerEventSourceDocuments(t *testing.T) {
	f := newFixture(t, "http://unused", "events", `{"description":"Replies","text_field":"reply"}`, plan{code: "free"})
	log := &eventLog{store: f.store, objects: f.snapshots}
	f.syncer.Catalog = connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{
		Batches: log, Objects: f.snapshots, Budget: events.ReadBudget{Batches: 1000, Pieces: 50000, Bytes: 128 << 20}, Extracted: 2})
	f.syncer.Allowances = map[string]model.FoundrySetAllowance{"events": {SetMicros: 5e6, WorkspaceMicros: 20e6}}
	body := func(id, reply string) string { return fmt.Sprintf("# %s\n\n- Piece: %s\n\n%s", reply, id, reply) }
	at := func(seq int) time.Time { return time.Date(2026, 10, 24, 12, 0, seq, 0, time.UTC) }
	syncAs := func(id string, mode model.ConnectorSyncMode) {
		t.Helper()
		if res := f.run(t, id); res.Mode != mode {
			t.Fatalf("%s: mode = %s, want %s", id, res.Mode, mode)
		}
		f.within(t, 2)
	}

	// The first sync reads in full: each piece is a document at its
	// arrival, named by its text, versioned by its body's SHA-256, and fanned
	// out as work no person started.
	log.send(t, `a {"reply":"first"}`, `b {"reply":"second"}`)
	syncAs("sync-1", model.ConnectorSyncFull)
	set := f.store.setOf("pieces")
	if set == nil || set.Name != "Pieces" || set.Description != "Replies" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("set = %+v", set)
	}
	if got := f.held(t, "pieces"); !maps.Equal(got, map[string]string{"a": body("a", "first"), "b": body("b", "second")}) {
		t.Fatalf("documents = %q", got)
	}
	for _, d := range f.store.live(set.ID) {
		sum := sha256.Sum256(f.documents.objects[d.S3Key])
		if d.Source.Version != hex.EncodeToString(sum[:]) || !d.Source.At.Equal(at(1)) || d.MediaType != "text/markdown" ||
			d.Name != map[string]string{"a": "first", "b": "second"}[d.Source.ExternalID] {
			t.Fatalf("document = %+v, source %+v", d, d.Source)
		}
	}
	if p := f.store.publishes[0]; !p.Fan.SystemOnly {
		t.Fatalf("publish = %+v", p)
	}

	// A third piece goes past the window: the change read adds it and
	// removes the oldest, b (a and b arrived together, and the lower id is
	// the newer), in the same publish, which owes a refresh.
	owed := f.store.owed
	log.send(t, `c {"reply":"third"}`)
	syncAs("sync-2", model.ConnectorSyncChanges)
	if got := slices.Sorted(maps.Keys(f.held(t, "pieces"))); !slices.Equal(got, []string{"a", "c"}) || f.store.owed != owed+1 {
		t.Fatalf("documents = %v, owed = %d", got, f.store.owed-owed)
	}

	// An identical resend of a moves its document to its new arrival in
	// place: no new document, nothing fanned out.
	a, fanned := f.liveID("pieces", "a"), len(f.store.fanned)
	log.send(t, `a {"reply":"first"}`)
	syncAs("sync-3", model.ConnectorSyncChanges)
	if f.liveID("pieces", "a") != a || !f.store.docs[a].Source.At.Equal(at(3)) || len(f.store.fanned) != fanned {
		t.Fatalf("a = %s (was %s), at %s, fanned %d", f.liveID("pieces", "a"), a, f.store.docs[a].Source.At, len(f.store.fanned)-fanned)
	}

	// The same id twice in one sync leaves its document with the later body.
	log.send(t, `c {"reply":"third, edited"}`)
	log.send(t, `c {"reply":"third, edited again"}`)
	syncAs("sync-4", model.ConnectorSyncChanges)
	if got := f.held(t, "pieces"); got["c"] != body("c", "third, edited again") || len(got) != 2 {
		t.Fatalf("documents = %q", got)
	}

	// Once the set's allowance is spent, a sync writes its rows but no
	// document: the set keeps what its graph last extracted, and is marked
	// skipped.
	f.store.spend[set.ID] = 5e6
	held, publishes := f.held(t, "pieces"), len(f.store.publishes)
	log.send(t, `d {"reply":"fourth"}`)
	syncAs("sync-5", model.ConnectorSyncChanges)
	if !maps.Equal(f.held(t, "pieces"), held) || len(f.store.publishes) != publishes || !f.store.skipped[set.ID] {
		t.Fatalf("documents = %q, publishes = %d, skipped = %v", f.held(t, "pieces"), len(f.store.publishes)-publishes, f.store.skipped)
	}

	// With room again, the next sync reads in full and keeps the newest 2,
	// d (sent while paused) among them, then clears the skip.
	f.store.spend[set.ID] = 0
	syncAs("sync-6", model.ConnectorSyncFull)
	if got := f.held(t, "pieces"); !maps.Equal(got, map[string]string{"c": body("c", "third, edited again"), "d": body("d", "fourth")}) ||
		f.store.skipped[set.ID] || f.store.clears != 1 {
		t.Fatalf("documents = %q, skipped = %v, clears = %d", got, f.store.skipped, f.store.clears)
	}

	// Clearing the text field empties the set at the next sync, which owes
	// the refresh; the set stays, and so does every piece's row.
	f.store.connectors["conn-1"].Config = json.RawMessage(`{"description":"Replies","text_field":""}`)
	owed = f.store.owed
	f.run(t, "sync-7")
	if got := f.held(t, "pieces"); len(got) != 0 || f.store.setOf("pieces") == nil || f.store.owed != owed+1 {
		t.Fatalf("documents = %q, owed = %d", got, f.store.owed-owed)
	}
	if rows := f.snapshotRows(t, "events"); len(rows) != 4 {
		t.Fatalf("rows = %q", rows)
	}
}

// TestSyncerEventSourceResend resends an API source's oldest held piece,
// identically, and then sends more new pieces than one flush publishes
// (flushDocuments): the change read moves the resent piece in place first,
// so no trim of an earlier flush removes it and its later Put never makes
// it a new document, extracted again. The window then trims the oldest of
// the rest.
func TestSyncerEventSourceResend(t *testing.T) {
	const window = 150
	f := newFixture(t, "http://unused", "events", `{"description":"Replies","text_field":"reply"}`, plan{code: "free"})
	log := &eventLog{store: f.store, objects: f.snapshots}
	f.syncer.Catalog = connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{
		Batches: log, Objects: f.snapshots, Budget: events.ReadBudget{Batches: 1000, Pieces: 50000, Bytes: 128 << 20}, Extracted: window})
	f.syncer.Allowances = map[string]model.FoundrySetAllowance{"events": {SetMicros: 5e6, WorkspaceMicros: 20e6}}
	f.syncer.MaxRows = 2000
	pieces := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf(`%s%03d {"reply":"%s %d"}`, prefix, i, prefix, i)
		}
		return out
	}
	// p000 to p149 arrive together, so p149, the highest id, is the oldest.
	log.send(t, pieces("p", window)...)
	if res := f.run(t, "sync-1"); res.Mode != model.ConnectorSyncFull || len(f.held(t, "pieces")) != window {
		t.Fatalf("first sync = %+v, documents = %d", res, len(f.held(t, "pieces")))
	}
	resent, fanned := f.liveID("pieces", "p149"), len(f.store.fanned)
	log.send(t, `p149 {"reply":"p 149"}`)
	log.send(t, pieces("n", flushDocuments+1)...)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges {
		t.Fatalf("second sync = %+v", res)
	}
	f.within(t, window)
	if id := f.liveID("pieces", "p149"); id != resent || !f.store.docs[id].Source.At.Equal(time.Date(2026, 10, 24, 12, 0, 2, 0, time.UTC)) {
		t.Fatalf("p149 = %s (was %s)", id, resent)
	}
	if got := len(f.store.fanned) - fanned; got != flushDocuments+1 {
		t.Fatalf("fanned %d documents, want only the %d new pieces", got, flushDocuments+1)
	}
	var want []string
	for i := range window - flushDocuments - 2 {
		want = append(want, fmt.Sprintf("p%03d", i))
	}
	want = append(append(want, "p149"), pieces("n", flushDocuments+1)...)
	for i, p := range want {
		want[i], _, _ = strings.Cut(p, " ")
	}
	if got := slices.Sorted(maps.Keys(f.held(t, "pieces"))); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("documents = %v", got)
	}
}
