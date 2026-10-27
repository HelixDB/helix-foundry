package local

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// freshDB is a new, empty schema on the test MySQL server
// (HELIX_TEST_ROOT_DSN, a root DSN ending in "/"), dropped after the test.
// Without it the test skips; there is no default server.
func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		t.Skip("no test MySQL: set HELIX_TEST_ROOT_DSN")
	}
	const schema = "foundry_local_test"
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + schema, "CREATE DATABASE " + schema} {
		if _, err := root.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("mysql", rootDSN+schema+"?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + schema)
		_ = root.Close()
	})
	return db
}

// A fresh database takes both sets, each in its own version table, and a
// second run applies nothing.
func TestMigrate(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("again: %v", err)
	}
	for table, atLeast := range map[string]int{"foundry_directory_goose_db_version": 1, "goose_db_version": 25} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE version_id > 0 AND is_applied`).Scan(&n); err != nil || n < atLeast {
			t.Fatalf("%s: %d versions, %v", table, n, err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id = 1`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the directory's version is in Foundry's table: %d, %v", n, err)
	}
}

// Foundry's store reads and locks the directory's workspaces.
func TestWorkspacesUnderTheStore(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	dir := directory.NewStore(db)
	if err := dir.SeedOwner(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := dir.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actorCtx := ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: owner.ID})
	svc := directory.NewService(dir, "http://helix", http.DefaultClient)
	w, err := svc.CreateWorkspace(actorCtx, &directoryv1.CreateWorkspaceRequest{Slug: "acme", Region: directory.Region})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMySQL(db, Workspaces{})

	if deleting, err := st.WorkspaceDeleting(ctx, w.GetId()); err != nil || deleting {
		t.Fatalf("deleting = %v, %v", deleting, err)
	}
	if _, err := st.WorkspaceDeleting(ctx, "01J00000000000000000000000"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	err = st.WithTx(ctx, func(tx store.Store) error {
		return Workspaces{}.Lock(ctx, db, w.GetId())
	})
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	// A connector of the workspace is the store's, bound to it.
	if err := st.CreateConnector(ctx, &model.Connector{ID: "01J00000000000000000000001", WorkspaceID: w.GetId(), Kind: "stripe", Name: "Stripe",
		Config: []byte(`{}`), SecretCiphertext: []byte("x"), SecretKeyID: "local", SecretHint: "h"}); err != nil {
		t.Fatalf("create connector: %v", err)
	}

	if _, err := db.Exec(`UPDATE workspaces SET status = 'deleting' WHERE id = ?`, w.GetId()); err != nil {
		t.Fatal(err)
	}
	if deleting, err := st.WorkspaceDeleting(ctx, w.GetId()); err != nil || !deleting {
		t.Fatalf("deleting = %v, %v", deleting, err)
	}
	if _, err := db.Exec(`UPDATE workspaces SET deleted_at = NOW(6) WHERE id = ?`, w.GetId()); err != nil {
		t.Fatal(err)
	}
	if err := (Workspaces{}).Lock(ctx, db, w.GetId()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lock a deleted workspace: %v", err)
	}
}

// The API's read queries go through the directory's, with its checks.
func TestGraphReader(t *testing.T) {
	db := freshDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	helix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Helix-Tenant-ID") == "" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer helix.Close()
	dir := directory.NewStore(db)
	if err := dir.SeedOwner(ctx); err != nil {
		t.Fatal(err)
	}
	owner, _ := dir.Owner(ctx)
	actorCtx := ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: owner.ID})
	svc := directory.NewService(dir, helix.URL, http.DefaultClient)
	w, err := svc.CreateWorkspace(actorCtx, &directoryv1.CreateWorkspaceRequest{Slug: "acme", Region: directory.Region})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateProject(actorCtx, &directoryv1.CreateProjectRequest{WorkspaceId: w.GetId(), Slug: "crm"})
	if err != nil {
		t.Fatal(err)
	}
	tn, err := svc.CreateTenant(actorCtx, &directoryv1.CreateTenantRequest{ProjectId: p.GetId(), Name: "Shop", Slug: "shop"})
	if err != nil {
		t.Fatal(err)
	}

	code, body, err := GraphReader{Directory: svc}.ReadQuery(actorCtx, tn.GetTenant().GetId(), []byte(`{"request_type":"read","query":{}}`))
	if err != nil || code != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("read = %d %s, %v", code, body, err)
	}
	if _, _, err := (GraphReader{Directory: svc}).ReadQuery(actorCtx, tn.GetTenant().GetId(), []byte(`{"request_type":"write","query":{}}`)); err == nil {
		t.Fatal("a write passed as a read")
	}

	// The ports read the same rows.
	dbs := Databases{Directory: dir, HelixURL: helix.URL}
	if got, err := dbs.Live(ctx, tn.GetTenant().GetId()); err != nil || got.WorkspaceID != w.GetId() || got.ProjectID != p.GetId() {
		t.Fatalf("live = %+v, %v", got, err)
	}
	if !(People{Directory: dir}).MayManageAI(ctx, w.GetId(), owner.ID) {
		t.Fatal("the owner may not manage AI")
	}
}
