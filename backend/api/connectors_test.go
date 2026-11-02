package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// connectorStore keeps connectors in memory, scoped by workspace like the MySQL provider.
type connectorStore struct {
	foundrystore.Unimplemented
	rows    map[string]*foundrymodel.Connector
	syncs   []*foundrymodel.ConnectorSync
	tables  []*foundrymodel.ConnectorTable
	batches []*foundrymodel.ConnectorChangeBatch
}

func (s *connectorStore) ListConnectorChangeBatches(_ context.Context, workspaceID, syncID string) ([]*foundrymodel.ConnectorChangeBatch, error) {
	var out []*foundrymodel.ConnectorChangeBatch
	for _, b := range s.batches {
		if b.WorkspaceID == workspaceID && b.SyncID == syncID {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *connectorStore) EnqueueConnectorSync(_ context.Context, sync *foundrymodel.ConnectorSync) error {
	for _, existing := range s.syncs {
		if existing.ConnectorID == sync.ConnectorID && (existing.Status == foundrymodel.ConnectorSyncQueued || existing.Status == foundrymodel.ConnectorSyncRunning) {
			return foundrystore.ErrConflict
		}
	}
	copied := *sync
	copied.Status, copied.CreatedAt, copied.NextAttemptAt = foundrymodel.ConnectorSyncQueued, time.Now(), time.Now()
	s.syncs = append(s.syncs, &copied)
	return nil
}

func (s *connectorStore) GetConnectorSync(_ context.Context, workspaceID, id string) (*foundrymodel.ConnectorSync, error) {
	for _, sync := range s.syncs {
		if sync.ID == id && sync.WorkspaceID == workspaceID {
			return sync, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *connectorStore) GetActiveConnectorSync(_ context.Context, workspaceID, connectorID string) (*foundrymodel.ConnectorSync, error) {
	for _, sync := range s.syncs {
		if sync.ConnectorID == connectorID && sync.WorkspaceID == workspaceID &&
			(sync.Status == foundrymodel.ConnectorSyncQueued || sync.Status == foundrymodel.ConnectorSyncRunning) {
			return sync, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *connectorStore) ListConnectorSyncs(_ context.Context, workspaceID, connectorID string, limit int) ([]*foundrymodel.ConnectorSync, error) {
	var out []*foundrymodel.ConnectorSync
	for i := len(s.syncs) - 1; i >= 0 && len(out) < limit; i-- {
		if s.syncs[i].ConnectorID == connectorID && s.syncs[i].WorkspaceID == workspaceID {
			out = append(out, s.syncs[i])
		}
	}
	return out, nil
}

func (s *connectorStore) ListConnectorTables(_ context.Context, workspaceID, connectorID string) ([]*foundrymodel.ConnectorTable, error) {
	var out []*foundrymodel.ConnectorTable
	for _, t := range s.tables {
		if t.ConnectorID == connectorID && t.WorkspaceID == workspaceID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *connectorStore) DeleteConnector(_ context.Context, workspaceID, id string) error {
	c, ok := s.rows[id]
	if !ok || c.WorkspaceID != workspaceID {
		return foundrystore.ErrNotFound
	}
	delete(s.rows, id)
	return nil
}

func (s *connectorStore) CreateConnector(_ context.Context, c *foundrymodel.Connector) error {
	copied := *c
	copied.CreatedAt, copied.UpdatedAt = time.Now(), time.Now()
	s.rows[c.ID] = &copied
	return nil
}

func (s *connectorStore) GetConnector(_ context.Context, workspaceID, id string) (*foundrymodel.Connector, error) {
	c, ok := s.rows[id]
	if !ok || c.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	copied := *c
	return &copied, nil
}

func (s *connectorStore) UpdateConnector(_ context.Context, workspaceID, id string, u foundrymodel.ConnectorUpdate) error {
	c, ok := s.rows[id]
	if !ok || c.WorkspaceID != workspaceID {
		return foundrystore.ErrNotFound
	}
	if u.Name != nil {
		c.Name = *u.Name
	}
	if u.Config != nil {
		c.Config, c.SecretCiphertext, c.SecretKeyID, c.SecretHint = u.Config, u.SecretCiphertext, u.SecretKeyID, u.SecretHint
	}
	if u.Schedule != nil {
		c.SyncEverySeconds, c.NextSyncAt, c.SchedulePaused = u.Schedule.SyncEverySeconds, u.Schedule.NextSyncAt, nil
	}
	return nil
}

// stripeStub answers every Stripe probe with 200, or 401 when reject is set,
// and records the keys it was called with.
type stripeStub struct {
	mu     sync.Mutex
	reject bool
	keys   []string
}

func newStripeStub(t *testing.T) (*stripeStub, string) {
	t.Helper()
	stub := &stripeStub{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.keys = append(stub.keys, r.Header.Get("Authorization"))
		if stub.reject {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(server.Close)
	return stub, server.URL
}

func connectorService(t *testing.T, stripeURL string, plans ports.Plans) (*Service, *connectorStore, *secretbox.Keyring) {
	t.Helper()
	keyring, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	endpoints := connector.DefaultEndpoints()
	endpoints.Stripe = stripeURL
	var kinds []connector.Kind
	for _, k := range connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{}).Kinds() {
		if k.Name() == "mysql" {
			k = offlineKind{k}
		}
		kinds = append(kinds, k)
	}
	store := &connectorStore{rows: map[string]*foundrymodel.Connector{}}
	return &Service{deps: &runtimeDeps{
		DB: store, Plans: plans, ConnectorKeys: keyring, ConnectorCatalog: connector.NewCatalogOf(kinds...),
	}}, store, keyring
}

// offlineKind discovers one orders table without dialing, so the RPC tests need no database.
type offlineKind struct{ connector.Kind }

func (offlineKind) Discover(context.Context, connector.Source) (connector.Discovery, error) {
	return connector.Discovery{Tables: []connector.Table{{Name: "orders", PrimaryKey: []string{"id"}}}}, nil
}

func (k offlineKind) Target(settings json.RawMessage) (string, error) {
	return target(k.Kind, settings)
}

// target is the wrapped kind's Target, which embedding the Kind interface hides.
func target(k connector.Kind, settings json.RawMessage) (string, error) {
	if t, ok := k.(connector.Targeted); ok {
		return t.Target(settings)
	}
	return "", nil
}

func stripeConfig(key string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Stripe{Stripe: &foundryv1.StripeConnectorConfig{ApiKey: key}}}
}

func mysqlConfig(password string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Mysql{Mysql: &foundryv1.DatabaseConnectorConfig{
		Host: "127.0.0.1", Port: 3307, Database: "helix_provision_test", User: "helix", Password: password, DisableTls: true,
	}}}
}

var (
	freePlan = &stubFoundryPlans{}
	proPlan  = &stubFoundryPlans{status: held("pro")}
)

func TestFoundryConnectorRPCs(t *testing.T) {
	ctx := context.Background()

	// A deployment without a sealing key must not accept credentials it cannot protect.
	t.Run("reports connectors unavailable without a sealing key", func(t *testing.T) {
		s := &Service{deps: &runtimeDeps{}}
		_, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("CreateConnector error = %v, want Unavailable", err)
		}
	})

	// The key is sealed to this connector's row and never sent back.
	t.Run("stores a stripe connector with its key sealed", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, store, keyring := connectorService(t, stripeURL, freePlan)

		resp, err := s.CreateConnector(ports.WithActor(ctx, person), &foundryv1.CreateConnectorRequest{
			WorkspaceId: "ws-1", Name: " Billing ",
			Config: stripeConfig("sk_test_abcdef123456"),
		})
		if err != nil {
			t.Fatal(err)
		}
		c := resp.GetConnector()
		if c.GetName() != "Billing" || c.GetKind() != "stripe" || c.GetSecretHint() != "sk_test…3456" ||
			c.GetConfig().GetStripe() == nil || c.GetConfig().GetStripe().GetApiKey() != "" {
			t.Fatalf("connector = %+v", c)
		}
		if len(resp.GetDiscovery().GetTables()) != 15 {
			t.Fatalf("discovered %d tables, want 15", len(resp.GetDiscovery().GetTables()))
		}
		row := store.rows[c.GetId()]
		if bytes.Contains(row.SecretCiphertext, []byte("sk_test_abcdef123456")) || *row.CreatedByUserID != "user-1" {
			t.Fatalf("stored row = %+v", row)
		}
		key, err := keyring.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext(c.GetId()))
		if err != nil || string(key) != "sk_test_abcdef123456" {
			t.Fatalf("sealed key opens to (%q, %v)", key, err)
		}
	})

	// The plan is checked before anything is dialed with the customer's credential.
	t.Run("refuses a pro kind on a free workspace without dialing", func(t *testing.T) {
		s, store, _ := connectorService(t, "http://unused", freePlan)
		_, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if status.Code(err) != codes.FailedPrecondition || foundryReason(err) != "FOUNDRY_PRO_REQUIRED" {
			t.Fatalf("CreateConnector error = %v, want FOUNDRY_PRO_REQUIRED", err)
		}
		if len(store.rows) != 0 {
			t.Fatalf("stored %d connectors", len(store.rows))
		}
	})

	// The database password is write-only like an API key.
	t.Run("stores a database connector on pro without returning its password", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", proPlan)
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if err != nil {
			t.Fatal(err)
		}
		db := resp.GetConnector().GetConfig().GetMysql()
		if db.GetPassword() != "" || db.GetHost() != "127.0.0.1" || db.GetPort() != 3307 || resp.GetConnector().GetSecretHint() != "password set" {
			t.Fatalf("connector config = %+v", resp.GetConnector())
		}
	})
	// Max grants everything Pro does.
	t.Run("stores a database connector on max", func(t *testing.T) {
		maxPlan := &stubFoundryPlans{status: held("max")}
		s, _, _ := connectorService(t, "http://unused", maxPlan)
		if _, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")}); err != nil {
			t.Fatalf("CreateConnector on max = %v", err)
		}
	})
	t.Run("keeps a database password byte for byte", func(t *testing.T) {
		s, store, keyring := connectorService(t, "http://unused", proPlan)
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig(" pass word ")})
		if err != nil {
			t.Fatal(err)
		}
		row := store.rows[resp.GetConnector().GetId()]
		password, err := keyring.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext(row.ID))
		if err != nil || string(password) != " pass word " {
			t.Fatalf("sealed password opens to (%q, %v)", password, err)
		}
	})

	t.Run("reports a rejected key with the connector's message and stores nothing", func(t *testing.T) {
		stub, stripeURL := newStripeStub(t)
		stub.reject = true
		s, store, _ := connectorService(t, stripeURL, freePlan)
		_, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "Stripe rejected this key; check it and test again" {
			t.Fatalf("CreateConnector error = %v", err)
		}
		if len(store.rows) != 0 {
			t.Fatalf("stored %d connectors", len(store.rows))
		}
	})

	t.Run("reports a malformed key as an invalid argument", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", freePlan)
		_, err := s.TestConnector(ctx, &foundryv1.TestConnectorRequest{WorkspaceId: "ws-1", Config: stripeConfig("not-a-key")})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("TestConnector error = %v, want InvalidArgument", err)
		}
	})

	// Editing settings must not force the customer to paste the key again.
	t.Run("an update with a blank key keeps and re-tests the stored key", func(t *testing.T) {
		stub, stripeURL := newStripeStub(t)
		s, store, _ := connectorService(t, stripeURL, freePlan)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_original1234")})
		if err != nil {
			t.Fatal(err)
		}
		stub.keys = nil
		id := created.GetConnector().GetId()

		resp, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: stripeConfig("")})
		if err != nil {
			t.Fatal(err)
		}
		if len(stub.keys) == 0 || stub.keys[0] != "Bearer sk_test_original1234" || resp.GetConnector().GetSecretHint() != "sk_test…1234" {
			t.Fatalf("re-tested with %v, hint %q", stub.keys, resp.GetConnector().GetSecretHint())
		}
		if resp.GetDiscovery() == nil || store.rows[id].SecretKeyID != "k1" {
			t.Fatalf("update response = %+v", resp)
		}
		if len(store.syncs) != 0 {
			t.Fatalf("a credential-only update queued %d syncs", len(store.syncs))
		}
	})

	// The old source's tables stay readable until a sync replaces them, so one is queued at once.
	t.Run("an update that points elsewhere queues a sync", func(t *testing.T) {
		s, store, _ := connectorService(t, "http://unused", proPlan)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if err != nil {
			t.Fatal(err)
		}
		moved := mysqlConfig("")
		moved.GetMysql().Database = "orders_eu"
		_, err = s.UpdateConnector(ports.WithActor(ctx, person), &foundryv1.UpdateConnectorRequest{
			WorkspaceId: "ws-1", Id: created.GetConnector().GetId(), Config: moved,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(store.syncs) != 1 || store.syncs[0].ConnectorID != created.GetConnector().GetId() || *store.syncs[0].RequestedByUserID != "user-1" {
			t.Fatalf("syncs = %+v", store.syncs)
		}
	})
	t.Run("an update that points elsewhere waits for a running sync", func(t *testing.T) {
		s, store, _ := connectorService(t, "http://unused", proPlan)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if err != nil {
			t.Fatal(err)
		}
		id := created.GetConnector().GetId()
		if _, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id}); err != nil {
			t.Fatal(err)
		}
		moved := mysqlConfig("")
		moved.GetMysql().Database = "orders_eu"
		_, err = s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: moved})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("UpdateConnector error = %v, want FailedPrecondition", err)
		}
		if got := store.rows[id].Config; !strings.Contains(string(got), `"helix_provision_test"`) {
			t.Fatalf("stored settings = %s", got)
		}
	})

	t.Run("an update cannot change the kind", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, _, _ := connectorService(t, stripeURL, proPlan)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: created.GetConnector().GetId(), Config: mysqlConfig("helix")})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("UpdateConnector error = %v, want InvalidArgument", err)
		}
	})

	// Authorization checks the workspace in the request, so the store must hold every read to it.
	t.Run("does not find another workspace's connector", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, _, _ := connectorService(t, stripeURL, freePlan)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-2", Id: created.GetConnector().GetId()})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("GetConnector error = %v, want NotFound", err)
		}
	})

	t.Run("lists pro kinds as unavailable on a free workspace", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", freePlan)
		resp, err := s.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		available := map[string]bool{}
		for _, k := range resp.GetKinds() {
			available[k.GetKind()] = k.GetAvailable()
		}
		if !available["stripe"] || !available["workos"] || !available["posthog"] || available["postgres"] || available["mysql"] {
			t.Fatalf("availability = %v", available)
		}
	})
}

func TestFoundryConnectorSyncRPCs(t *testing.T) {
	ctx := context.Background()
	createStripe := func(t *testing.T, s *Service) string {
		t.Helper()
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if err != nil {
			t.Fatal(err)
		}
		return resp.GetConnector().GetId()
	}

	// Two syncs of one connector would race on the same snapshots.
	t.Run("queues one sync at a time", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, _, _ := connectorService(t, stripeURL, freePlan)
		id := createStripe(t, s)

		resp, err := s.SyncConnector(ports.WithActor(ctx, person), &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetSync().GetStatus() != foundryv1.ConnectorSyncStatus_CONNECTOR_SYNC_STATUS_QUEUED || resp.GetSync().GetConnectorId() != id {
			t.Fatalf("sync = %+v", resp.GetSync())
		}
		if _, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id}); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("second SyncConnector error = %v, want AlreadyExists", err)
		}
	})
	// A workspace that dropped to Free cannot queue database syncs.
	t.Run("refuses to queue a pro kind on a free workspace", func(t *testing.T) {
		plans := &stubFoundryPlans{status: held("pro")}
		s, _, _ := connectorService(t, "http://unused", plans)
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if err != nil {
			t.Fatal(err)
		}
		plans.status = held("free")
		_, err = s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: created.GetConnector().GetId()})
		if foundryReason(err) != "FOUNDRY_PRO_REQUIRED" {
			t.Fatalf("SyncConnector error = %v, want FOUNDRY_PRO_REQUIRED", err)
		}
	})
	t.Run("refuses to delete a connector while its sync is live", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, store, _ := connectorService(t, stripeURL, freePlan)
		id := createStripe(t, s)
		if _, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteConnector(ctx, &foundryv1.DeleteConnectorRequest{WorkspaceId: "ws-1", Id: id}); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("DeleteConnector error = %v, want FailedPrecondition", err)
		}
		store.syncs[0].Status = foundrymodel.ConnectorSyncSucceeded
		if _, err := s.DeleteConnector(ctx, &foundryv1.DeleteConnectorRequest{WorkspaceId: "ws-1", Id: id}); err != nil {
			t.Fatalf("DeleteConnector after the sync = %v", err)
		}
	})
	t.Run("shows synced tables and the newest sync on the connector", func(t *testing.T) {
		_, stripeURL := newStripeStub(t)
		s, store, _ := connectorService(t, stripeURL, freePlan)
		id := createStripe(t, s)
		store.tables = append(store.tables, &foundrymodel.ConnectorTable{
			ConnectorID: id, WorkspaceID: "ws-1", TableName: "customers", RowCount: 2, SyncID: "sync-1", SyncedAt: time.Now(),
			Columns: []byte(`[{"name":"customer_id"},{"name":"email"}]`), PrimaryKey: []byte(`["customer_id"]`),
			ForeignKeys: []byte(`[]`),
		})
		if _, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id}); err != nil {
			t.Fatal(err)
		}

		resp, err := s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		c := resp.GetConnector()
		if len(c.GetTables()) != 1 || c.GetTables()[0].GetRowCount() != 2 || len(c.GetTables()[0].GetColumns()) != 2 ||
			c.GetTables()[0].GetPrimaryKey()[0] != "customer_id" || c.GetLastSync() == nil {
			t.Fatalf("connector = %+v", c)
		}
	})
}

func TestFoundryConnectorCapture(t *testing.T) {
	ctx := context.Background()
	// show creates a Stripe connector whose key reads events, with one keyed and one keyless table synced.
	show := func(t *testing.T, plans ports.Plans) map[string]string {
		t.Helper()
		_, stripeURL := newStripeStub(t)
		s, store, _ := connectorService(t, stripeURL, plans)
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abc")})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetConnector().GetId()
		for name, key := range map[string]string{"customers": `["customer_id"]`, "balance_transactions": `[]`} {
			store.tables = append(store.tables, &foundrymodel.ConnectorTable{
				ConnectorID: id, WorkspaceID: "ws-1", TableName: name, SyncID: "sync-1", SyncedAt: time.Now(),
				Columns: []byte(`[{"name":"customer_id"}]`), PrimaryKey: []byte(key), ForeignKeys: []byte(`[]`),
			})
		}
		got, err := s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, table := range got.GetConnector().GetTables() {
			out[table.GetName()] = table.GetCapture()
		}
		return out
	}

	// Events are a Pro capture method; Free reads every table in full.
	t.Run("shows events on pro and full on free", func(t *testing.T) {
		if got := show(t, proPlan)["customers"]; got != "events" {
			t.Fatalf("pro capture = %q, want events", got)
		}
		if got := show(t, freePlan)["customers"]; got != "full" {
			t.Fatalf("free capture = %q, want full", got)
		}
	})
	// The syncer merges by key, so a keyless table is read in full whatever the kind offers.
	t.Run("shows a keyless table as full", func(t *testing.T) {
		if got := show(t, proPlan)["balance_transactions"]; got != "full" {
			t.Fatalf("keyless capture = %q, want full", got)
		}
	})
}

// releasingKind is a kind that holds something on its source, recording the
// settings, credential and cursor each Release ran with; err fails it.
type releasingKind struct {
	connector.Kind
	err      error
	released []string
}

func (k *releasingKind) Release(_ context.Context, src connector.Source, cursor json.RawMessage) error {
	if k.err != nil {
		return k.err
	}
	var s connector.DatabaseSettings
	_ = json.Unmarshal(src.Settings, &s) // written by Prepare
	k.released = append(k.released, s.Database+"|"+src.Secret+"|"+string(cursor))
	return nil
}

func (k *releasingKind) Target(settings json.RawMessage) (string, error) {
	return target(k.Kind, settings)
}

func TestFoundryConnectorReleaseOnSettingsChange(t *testing.T) {
	ctx := context.Background()
	maxPlan := &stubFoundryPlans{status: held("max")}
	// setup is a MySQL connector, made releasing, whose last sync left a cursor.
	setup := func(t *testing.T, releaseErr error) (*Service, *connectorStore, *releasingKind, string) {
		t.Helper()
		s, store, _ := connectorService(t, "http://unused", maxPlan)
		var kinds []connector.Kind
		rk := &releasingKind{err: releaseErr}
		for _, k := range s.deps.ConnectorCatalog.Kinds() {
			if k.Name() == "mysql" {
				rk.Kind, k = k, rk
			}
			kinds = append(kinds, k)
		}
		s.deps.ConnectorCatalog = connector.NewCatalogOf(kinds...)
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")})
		if err != nil {
			t.Fatal(err)
		}
		id := resp.GetConnector().GetId()
		store.rows[id].ChangeCursor = json.RawMessage(`{"slot":"helix_foundry_x"}`)
		return s, store, rk, id
	}
	otherDatabase := func() *foundryv1.ConnectorConfig {
		cfg := mysqlConfig("")
		cfg.GetMysql().Database = "other"
		return cfg
	}
	update := func(s *Service, id string, cfg *foundryv1.ConnectorConfig) error {
		_, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: cfg})
		return err
	}
	database := func(store *connectorStore, id string) string {
		var d connector.DatabaseSettings
		_ = json.Unmarshal(store.rows[id].Config, &d)
		return d.Database
	}

	// Saving forgets the cursor, and a slot can only be dropped from the database
	// that made it, so the old settings' hold is given back before they go.
	t.Run("releases under the old settings before new ones replace them", func(t *testing.T) {
		s, store, rk, id := setup(t, nil)
		if err := update(s, id, otherDatabase()); err != nil {
			t.Fatal(err)
		}
		if want := []string{`helix_provision_test|helix|{"slot":"helix_foundry_x"}`}; !slices.Equal(rk.released, want) {
			t.Fatalf("released = %v, want %v", rk.released, want)
		}
		if got := database(store, id); got != "other" {
			t.Fatalf("stored database = %q", got)
		}
	})
	// A kept password goes only to the server it was entered for, so another
	// workspace admin can't point the connector at a server of their own.
	t.Run("refuses to send the stored password to another server", func(t *testing.T) {
		for name, edit := range map[string]func(*foundryv1.DatabaseConnectorConfig){
			"host": func(c *foundryv1.DatabaseConnectorConfig) { c.Host = "db.attacker.example" },
			"port": func(c *foundryv1.DatabaseConnectorConfig) { c.Port = 3308 },
			"tls":  func(c *foundryv1.DatabaseConnectorConfig) { c.DisableTls = false },
		} {
			t.Run(name, func(t *testing.T) {
				s, store, rk, id := setup(t, nil)
				cfg := mysqlConfig("")
				edit(cfg.GetMysql())
				err := update(s, id, cfg)
				if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "enter the credential again") {
					t.Fatalf("UpdateConnector = %v, want InvalidArgument", err)
				}
				if len(rk.released) != 0 {
					t.Fatalf("released = %v", rk.released)
				}
				if got := database(store, id); got != "helix_provision_test" {
					t.Fatalf("stored database = %q, want the old one", got)
				}
			})
		}
	})
	t.Run("never sends a new server's password to the old one", func(t *testing.T) {
		s, _, rk, id := setup(t, nil)
		cfg := mysqlConfig("new-server-password")
		cfg.GetMysql().Host = "db.elsewhere.example"
		if err := update(s, id, cfg); err != nil {
			t.Fatal(err)
		}
		if want := []string{`helix_provision_test|helix|{"slot":"helix_foundry_x"}`}; !slices.Equal(rk.released, want) {
			t.Fatalf("released = %v, want %v", rk.released, want)
		}
	})
	t.Run("releases nothing when the settings stay the same", func(t *testing.T) {
		s, _, rk, id := setup(t, nil)
		if err := update(s, id, mysqlConfig("rotated")); err != nil {
			t.Fatal(err)
		}
		if len(rk.released) != 0 {
			t.Fatalf("released = %v", rk.released)
		}
	})
	// A host that is gone can't hold anything Helix could drop; refusing would
	// leave the customer unable to move off it.
	t.Run("saves new settings when the old source can't be reached", func(t *testing.T) {
		s, store, _, id := setup(t, &connector.RefusedError{Message: "could not connect"})
		if err := update(s, id, otherDatabase()); err != nil {
			t.Fatalf("UpdateConnector = %v", err)
		}
		if got := database(store, id); got != "other" {
			t.Fatalf("stored database = %q", got)
		}
	})
	t.Run("keeps the old settings while their source refuses the drop", func(t *testing.T) {
		s, store, _, id := setup(t, errors.New("replication slot is active for PID 42"))
		err := update(s, id, otherDatabase())
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "drop it on that database") {
			t.Fatalf("UpdateConnector = %v, want FailedPrecondition", err)
		}
		if got := database(store, id); got != "helix_provision_test" {
			t.Fatalf("stored database = %q, want the old one", got)
		}
	})
}

func TestFoundryConnectorCDCPlan(t *testing.T) {
	ctx := context.Background()
	withCDC := func() *foundryv1.ConnectorConfig {
		cfg := mysqlConfig("helix")
		cfg.GetMysql().Cdc = &foundryv1.DatabaseCDCConfig{Enabled: true}
		return cfg
	}
	// CDC is a Max capture method; Pro must hear which plan has it rather than save a setting it ignores.
	t.Run("refuses change data capture on pro, naming the plan with it", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", proPlan)
		_, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: withCDC()})
		if foundryReason(err) != "FOUNDRY_PRO_REQUIRED" || !strings.Contains(status.Convert(err).Message(), "Foundry Max") {
			t.Fatalf("CreateConnector = %v", err)
		}
	})
	t.Run("accepts it on max", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", &stubFoundryPlans{status: held("max")})
		if _, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: withCDC()}); err != nil {
			t.Fatalf("CreateConnector = %v", err)
		}
	})
	t.Run("leaves pro settings without it alone", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", proPlan)
		if _, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Orders", Config: mysqlConfig("helix")}); err != nil {
			t.Fatalf("CreateConnector = %v", err)
		}
	})
}

func foundryReason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}

// Key-only kinds store no settings, and the config rebuilt from them names the
// kind without the key.
func TestKeyOnlyConnectorConfigs(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	for _, tc := range []struct {
		kind, key, hint string
		config          func(key string) *foundryv1.ConnectorConfig
	}{
		{"calcom", "cal_live_0123456789abcdef01234567894f2a", "cal_live…4f2a", func(key string) *foundryv1.ConnectorConfig {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Calcom{Calcom: &foundryv1.CalcomConnectorConfig{ApiKey: key}}}
		}},
		{"calcom", "cal_0123456789abcdef01234567894f2a", "cal…4f2a", func(key string) *foundryv1.ConnectorConfig {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Calcom{Calcom: &foundryv1.CalcomConnectorConfig{ApiKey: key}}}
		}},
		{"granola", "grn_0123456789abcdef4f2a", "grn…4f2a", func(key string) *foundryv1.ConnectorConfig {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Granola{Granola: &foundryv1.GranolaConnectorConfig{ApiKey: key}}}
		}},
		{"notion", "ntn_0123456789abcdef4f2a", "ntn…4f2a", func(key string) *foundryv1.ConnectorConfig {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Notion{Notion: &foundryv1.NotionConnectorConfig{Token: key}}}
		}},
		{"notion", "secret_0123456789abcdef4f2a", "secret…4f2a", func(key string) *foundryv1.ConnectorConfig {
			return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Notion{Notion: &foundryv1.NotionConnectorConfig{Token: key}}}
		}},
	} {
		t.Run(tc.hint, func(t *testing.T) {
			kind, src, secretless, err := sourceFromProto(catalog, tc.config("  "+tc.key+"\n"))
			if err != nil || kind.Name() != tc.kind || secretless || src.Secret != tc.key || string(src.Settings) != "{}" {
				t.Fatalf("source = (%v, %+v, %v, %v)", kind, src, secretless, err)
			}
			settings, err := kind.Prepare(src)
			if err != nil || string(settings) != "{}" || kind.Hint(src.Secret) != tc.hint {
				t.Fatalf("prepare = (%s, %v), hint = %q", settings, err, kind.Hint(src.Secret))
			}
			got, err := configToProto(tc.kind, settings)
			if err != nil || !proto.Equal(got, tc.config("")) {
				t.Fatalf("config = (%v, %v)", got, err)
			}
		})
	}
}

// An API source's description and text field reach the kind only when the
// request carries them, so an update that leaves one out stores nothing over
// it; "" is carried, and clears it.
func TestEventsConnectorSettings(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	for _, tc := range []struct {
		description, textField *string
		settings, prepared     string
	}{
		{nil, nil, `{}`, `{}`},
		{proto.String(" Replies "), nil, `{"description":" Replies "}`, `{"description":"Replies"}`},
		{nil, proto.String("reply"), `{"text_field":"reply"}`, `{"text_field":"reply"}`},
		{proto.String(""), proto.String(""), `{"description":"","text_field":""}`, `{"description":"","text_field":""}`},
	} {
		t.Run(tc.settings, func(t *testing.T) {
			config := &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{
				Description: tc.description, TextField: tc.textField,
			}}}
			kind, src, secretless, err := sourceFromProto(catalog, config)
			if err != nil || kind.Name() != foundrymodel.ConnectorKindEvents || !secretless || src.Secret != "" || string(src.Settings) != tc.settings {
				t.Fatalf("source = (%v, %s, %v, %v)", kind, src.Settings, secretless, err)
			}
			if prepared, err := kind.Prepare(src); err != nil || string(prepared) != tc.prepared {
				t.Fatalf("prepare = (%s, %v)", prepared, err)
			}
		})
	}
}

// Granola's one setting, transcripts, is stored only when on, so a
// connector made before it existed reads as off.
func TestGranolaConnectorConfig(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	for _, tc := range []struct {
		transcripts bool
		stored      string
	}{{false, `{}`}, {true, `{"transcripts":true}`}} {
		t.Run(tc.stored, func(t *testing.T) {
			config := func(key string) *foundryv1.ConnectorConfig {
				return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Granola{Granola: &foundryv1.GranolaConnectorConfig{
					ApiKey: key, Transcripts: tc.transcripts,
				}}}
			}
			kind, src, _, err := sourceFromProto(catalog, config(" grn_0123456789abcdef4f2a "))
			if err != nil || kind.Name() != "granola" || src.Secret != "grn_0123456789abcdef4f2a" {
				t.Fatalf("source = (%v, %+v, %v)", kind, src, err)
			}
			settings, err := kind.Prepare(src)
			if err != nil || string(settings) != tc.stored {
				t.Fatalf("prepare = (%s, %v)", settings, err)
			}
			got, err := configToProto("granola", settings)
			if err != nil || !proto.Equal(got, config("")) {
				t.Fatalf("config = (%v, %v)", got, err)
			}
		})
	}
	if _, err := configToProto("granola", json.RawMessage(`{"transcripts":"yes"}`)); err == nil {
		t.Fatal("configToProto read settings Prepare never stores")
	}
}

// Slack's one setting, history_days, is stored with its default applied,
// so a connector made without one reads back as keeping 90 days.
func TestSlackConnectorConfig(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	config := func(token string, days int32) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{
			BotToken: token, HistoryDays: days,
		}}}
	}
	for _, tc := range []struct {
		days, stored int32
	}{{0, 90}, {1, 1}, {30, 30}, {365, 365}} {
		t.Run(fmt.Sprint(tc.days), func(t *testing.T) {
			kind, src, secretless, err := sourceFromProto(catalog, config(" xoxb-1234-5678-abcdef4f2a\n", tc.days))
			if err != nil || kind.Name() != "slack" || secretless || src.Secret != "xoxb-1234-5678-abcdef4f2a" ||
				string(src.Settings) != fmt.Sprintf(`{"history_days":%d}`, tc.days) {
				t.Fatalf("source = (%v, %+v, %v, %v)", kind, src, secretless, err)
			}
			settings, err := kind.Prepare(src)
			if err != nil || string(settings) != fmt.Sprintf(`{"history_days":%d}`, tc.stored) || kind.Hint(src.Secret) != "xoxb…4f2a" {
				t.Fatalf("prepare = (%s, %v), hint = %q", settings, err, kind.Hint(src.Secret))
			}
			got, err := configToProto("slack", settings)
			if err != nil || !proto.Equal(got, config("", tc.stored)) {
				t.Fatalf("config = (%v, %v)", got, err)
			}
		})
	}
	for token, message := range map[string]string{
		"xoxb-1234-5678-abcdef4f2a": "keep 1 to 365 days of history",
		"xoxp-1234-5678-abcdef4f2a": "enter a Slack bot token (xoxb-…)",
	} {
		kind, src, _, err := sourceFromProto(catalog, config(token, 366))
		if err != nil {
			t.Fatal(err)
		}
		var invalidErr *connector.InvalidError
		if _, err := kind.Prepare(src); !errors.As(err, &invalidErr) || invalidErr.Message != message {
			t.Errorf("Prepare(%s, 366 days) error = %v, want %q", token[:4], err, message)
		}
	}
	if _, err := configToProto("slack", json.RawMessage(`{"history_days":"90"}`)); err == nil {
		t.Fatal("configToProto read settings Prepare never stores")
	}
}

// Google's settings store the user in lower case and its days of past
// events, 90 unless chosen; the config rebuilt from them carries no key.
func TestGoogleConnectorConfig(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "foundry@acme.iam.gserviceaccount.com", "private_key_id": "5c1e0d9f1a2b",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	config := func(key, subject string, days int32) *foundryv1.ConnectorConfig {
		return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Google{Google: &foundryv1.GoogleWorkspaceConnectorConfig{
			ServiceAccountKey: key, Subject: subject, EventDays: days,
		}}}
	}
	for _, tc := range []struct {
		subject    string
		days, kept int32
		stored     string
	}{
		{" Ada@Acme.com ", 0, 90, `{"subject":"ada@acme.com","event_days":90}`},
		{"ada@acme.com", 30, 30, `{"subject":"ada@acme.com","event_days":30}`},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			kind, src, secretless, err := sourceFromProto(catalog, config("  "+string(key)+"\n", tc.subject, tc.days))
			if err != nil || kind.Name() != "google" || secretless || src.Secret != string(key) {
				t.Fatalf("source = (%v, %s, %v, %v)", kind, src.Settings, secretless, err)
			}
			settings, err := kind.Prepare(src)
			if err != nil || string(settings) != tc.stored || kind.Hint(src.Secret) != "foundry@acme.iam.gserviceaccount.com…1a2b" {
				t.Fatalf("prepare = (%s, %v), hint = %q", settings, err, kind.Hint(src.Secret))
			}
			got, err := configToProto("google", settings)
			if err != nil || !proto.Equal(got, config("", "ada@acme.com", tc.kept)) {
				t.Fatalf("config = (%v, %v)", got, err)
			}
		})
	}
	if _, err := configToProto("google", json.RawMessage(`{"subject":1}`)); err == nil {
		t.Fatal("configToProto read settings Prepare never stores")
	}
}

// A source's reason follows the object's name; SaaS objects have none.
func TestDiscoveryUnavailable(t *testing.T) {
	out := discoveryToProto(connector.Discovery{Unavailable: []connector.Unavailable{
		{Name: "balance_transactions"}, {Name: "notes.xlsx", Reason: "unsupported file type"},
	}})
	if got := out.GetUnavailable(); len(got) != 2 || got[0] != "balance_transactions" || got[1] != "notes.xlsx: unsupported file type" {
		t.Fatalf("unavailable = %q", got)
	}
}

func TestDiscoveryWarnings(t *testing.T) {
	warning := "no index starts with cursor column events.updated_at"
	if got := discoveryToProto(connector.Discovery{Warnings: []string{warning}}).GetWarnings(); len(got) != 1 || got[0] != warning {
		t.Fatalf("warnings = %q", got)
	}
}

func TestFoundryConnectorSchedules(t *testing.T) {
	ctx := context.Background()
	created := func(t *testing.T, plans ports.Plans) (*Service, *connectorStore, string) {
		t.Helper()
		_, stripeURL := newStripeStub(t)
		s, store, _ := connectorService(t, stripeURL, plans)
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Billing", Config: stripeConfig("sk_test_abcdef123456")})
		if err != nil {
			t.Fatal(err)
		}
		return s, store, resp.GetConnector().GetId()
	}
	every := func(n int64) *int64 { return &n }
	// A new connector syncs at the scheduler's next tick, then on its plan's interval.
	t.Run("schedules a new connector's first sync at once", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		row := store.rows[id]
		if row.NextSyncAt == nil || time.Since(*row.NextSyncAt) > time.Minute {
			t.Fatalf("next sync = %v", row.NextSyncAt)
		}
		resp, err := s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if sched := resp.GetConnector().GetSchedule(); sched.GetEverySeconds() != 0 || sched.GetEffectiveEverySeconds() != 3600 {
			t.Fatalf("schedule = %+v", sched)
		}
	})
	// Rotating a key changes no settings, so no sync is queued to reschedule the connector.
	t.Run("keeps the schedule when only the credential changes", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		at := time.Now().Add(42 * time.Minute).UTC()
		store.rows[id].NextSyncAt = &at
		if _, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: stripeConfig("sk_test_rotated987654")}); err != nil {
			t.Fatal(err)
		}
		if row := store.rows[id]; row.NextSyncAt == nil || !row.NextSyncAt.Equal(at) {
			t.Fatalf("next sync = %v, want %v", row.NextSyncAt, at)
		}
		if _, err := store.GetActiveConnectorSync(ctx, "ws-1", id); !errors.Is(err, foundrystore.ErrNotFound) {
			t.Fatalf("a credential change queued a sync: %v", err)
		}
	})
	// A connector paused by a rejected key resumes once the key is replaced.
	t.Run("resumes a paused connector when its credential is replaced", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		reason := "Stripe rejected this key"
		store.rows[id].NextSyncAt, store.rows[id].SchedulePaused = nil, &reason
		if _, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: stripeConfig("sk_test_rotated987654")}); err != nil {
			t.Fatal(err)
		}
		if row := store.rows[id]; row.NextSyncAt == nil || time.Since(*row.NextSyncAt) > time.Minute || row.SchedulePaused != nil {
			t.Fatalf("next sync = %v, paused = %v", row.NextSyncAt, row.SchedulePaused)
		}
	})
	// The worker never schedules a kind below its own floor, so neither may the shown interval.
	t.Run("shows a kind's floor over the plan's interval", func(t *testing.T) {
		pro := held("pro").Plan
		if got := scheduleToProto(&foundrymodel.Connector{}, pro, 24*time.Hour).GetEffectiveEverySeconds(); got != 86400 {
			t.Fatalf("effective = %d, want 86400", got)
		}
		hourly := 3600
		if got := scheduleToProto(&foundrymodel.Connector{SyncEverySeconds: &hourly}, pro, 24*time.Hour).GetEffectiveEverySeconds(); got != 86400 {
			t.Fatalf("effective with an hourly choice = %d, want 86400", got)
		}
	})
	// The plan sets the shortest interval a customer may pick.
	t.Run("refuses an interval below the plan's shortest", func(t *testing.T) {
		s, _, id := created(t, proPlan)
		_, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, SyncEverySeconds: every(900)})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Foundry Pro syncs at most every 1h") {
			t.Fatalf("UpdateConnector error = %v", err)
		}
	})
	t.Run("stores a longer interval and schedules by it", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		resp, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, SyncEverySeconds: every(7200)})
		if err != nil {
			t.Fatal(err)
		}
		row := store.rows[id]
		if row.SyncEverySeconds == nil || *row.SyncEverySeconds != 7200 || row.NextSyncAt == nil || time.Until(*row.NextSyncAt) < 119*time.Minute {
			t.Fatalf("row = %+v", row)
		}
		if sched := resp.GetConnector().GetSchedule(); sched.GetEverySeconds() != 7200 || sched.GetEffectiveEverySeconds() != 7200 {
			t.Fatalf("schedule = %+v", sched)
		}
	})
	// -1 turns automatic syncs off; the API keeps 0 for "the plan's interval".
	t.Run("turns automatic syncs off and back to the plan's", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		resp, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, SyncEverySeconds: every(-1)})
		if err != nil {
			t.Fatal(err)
		}
		if row := store.rows[id]; row.SyncEverySeconds == nil || *row.SyncEverySeconds != 0 || row.NextSyncAt != nil {
			t.Fatalf("row = %+v", row)
		}
		if sched := resp.GetConnector().GetSchedule(); sched.GetEverySeconds() != -1 || sched.GetEffectiveEverySeconds() != 0 {
			t.Fatalf("schedule = %+v", sched)
		}
		if _, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, SyncEverySeconds: every(0)}); err != nil {
			t.Fatal(err)
		}
		if row := store.rows[id]; row.SyncEverySeconds != nil || row.NextSyncAt == nil {
			t.Fatalf("row = %+v", row)
		}
	})
	// A paused schedule shows why, so the owner knows what to fix.
	t.Run("shows why automatic syncs paused", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		reason := "Stripe rejected this key"
		store.rows[id].SchedulePaused, store.rows[id].NextSyncAt = &reason, nil
		resp, err := s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil || resp.GetConnector().GetSchedule().GetPausedReason() != reason {
			t.Fatalf("GetConnector = (%+v, %v)", resp, err)
		}
	})
	// The customer can ask for a full read, which catches what change reads miss at once.
	t.Run("queues a full sync when asked", func(t *testing.T) {
		s, _, id := created(t, proPlan)
		resp, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id, Full: true})
		if err != nil || resp.GetSync().GetMode() != "full" || resp.GetSync().GetOrigin() != "manual" {
			t.Fatalf("SyncConnector = (%+v, %v)", resp, err)
		}
	})
	// The sync history says what each sync changed, table by table.
	t.Run("lists a sync's changes per table", func(t *testing.T) {
		s, store, id := created(t, proPlan)
		synced, err := s.SyncConnector(ctx, &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: id})
		if err != nil {
			t.Fatal(err)
		}
		store.batches = append(store.batches, &foundrymodel.ConnectorChangeBatch{WorkspaceID: "ws-1", SyncID: synced.GetSync().GetId(),
			TableName: "customers", Kind: foundrymodel.ConnectorChangesBatch, Inserts: 1, Updates: 2, Deletes: 3})
		resp, err := s.GetConnectorSync(ctx, &foundryv1.GetConnectorSyncRequest{WorkspaceId: "ws-1", Id: synced.GetSync().GetId()})
		if err != nil {
			t.Fatal(err)
		}
		if tables := resp.GetSync().GetTables(); len(tables) != 1 || tables[0].GetTable() != "customers" || tables[0].GetDeletes() != 3 {
			t.Fatalf("tables = %+v", tables)
		}
	})
	// Each kind names the cheapest plan that unlocks it, so the website can offer it.
	t.Run("lists each kind with its lowest plan", func(t *testing.T) {
		s, _, _ := connectorService(t, "http://unused", freePlan)
		resp, err := s.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range resp.GetKinds() {
			wantPlan, wantAvailable := "free", true
			if slices.Contains([]string{"postgres", "mysql", "s3"}, k.GetKind()) {
				wantPlan, wantAvailable = "pro", false
			}
			if k.GetPlanCode() != wantPlan || k.GetAvailable() != wantAvailable || k.GetRequiresPro() != !wantAvailable {
				t.Fatalf("kind = %+v", k)
			}
		}
	})
}
