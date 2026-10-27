package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/migrations"
)

// testSchema is the database the store's tests run in, created afresh on the
// test MySQL server (HELIX_TEST_ROOT_DSN, a root DSN ending in "/").
const testSchema = "foundry_store_test"

var (
	testDB *sql.DB
	// testDSN opens testSchema, for a test that needs a connection of its own.
	testDSN string
)

func TestMain(m *testing.M) {
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
	}
	db, err := openTestSchema(rootDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration tests: %v. Run `make start-testdb` first.\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	os.Exit(code)
}

// openTestSchema creates testSchema with Foundry's migrations and the host's
// table of workspaces (hostWorkspaces), and opens it as the app does.
func openTestSchema(rootDSN string) (*sql.DB, error) {
	root, err := openWithRetry(rootDSN, 30, 500*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("cannot reach MySQL (%w)", err)
	}
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + testSchema, "CREATE DATABASE " + testSchema} {
		if _, err := root.Exec(stmt); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	_ = root.Close()
	testDSN = rootDSN + testSchema + "?parseTime=true&multiStatements=true"
	db, err := openWithRetry(testDSN, 1, 0)
	if err != nil {
		return nil, err
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("mysql"); err != nil {
		return nil, err
	}
	if err := goose.Up(db, "."); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.Exec(hostWorkspacesDDL); err != nil {
		return nil, fmt.Errorf("host workspaces: %w", err)
	}
	return db, nil
}

func openWithRetry(dsn string, attempts int, gap time.Duration) (*sql.DB, error) {
	mc, err := gomysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	mc.ClientFoundRows = true // match the app: matched-rows semantics for execOrNotFound
	db, err := sql.Open("mysql", mc.FormatDSN())
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if lastErr = db.Ping(); lastErr == nil {
			return db, nil
		}
		time.Sleep(gap)
	}
	_ = db.Close()
	return nil, lastErr
}

// hostWorkspacesDDL is the host's table of workspaces as the store reads it
// (store.Workspaces): an id, a status and when it was deleted.
const hostWorkspacesDDL = `CREATE TABLE workspaces (
  id         CHAR(26)    NOT NULL PRIMARY KEY,
  status     VARCHAR(16) NOT NULL DEFAULT 'active',
  deleted_at DATETIME(6) NULL
)`

// hostWorkspaces is the tests' host: its workspaces in hostWorkspacesDDL's
// table, locked and read as a host does.
type hostWorkspaces struct{}

func (hostWorkspaces) Table() string { return "workspaces" }

func (hostWorkspaces) Lock(ctx context.Context, q store.Executor, id string) error {
	var locked string
	err := q.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func (hostWorkspaces) Deleting(ctx context.Context, q store.Executor, id string) (bool, error) {
	var deleting bool
	err := q.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL OR status IN ('deleting', 'deleted') FROM workspaces WHERE id=?`, id).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	return deleting, err
}

func ptr[T any](v T) *T { return &v }

// id26 pads a short tag to a deterministic 26-char id matching the CHAR(26)
// ULID column width. Use it for fixture ids in tests.
func id26(tag string) string {
	const width = 26
	if len(tag) > width {
		return tag[:width]
	}
	return tag + strings.Repeat("0", width-len(tag))
}

// newTestStore returns a Store bound to the shared test connection. The
// caller's table state is wiped on Cleanup so tests stay independent and
// order-agnostic.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	truncateAll(t)
	t.Cleanup(func() { truncateAll(t) })
	return store.NewMySQL(testDB, hostWorkspaces{})
}

// truncateAll empties every table of the test schema but goose's.
func truncateAll(t *testing.T) {
	t.Helper()
	rows, err := testDB.Query(`SELECT table_name FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' AND table_name <> 'goose_db_version'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	// The schema has no foreign keys, so a plain TRUNCATE works.
	for _, table := range tables {
		if _, err := testDB.Exec("TRUNCATE TABLE " + table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
}

// createWorkspace adds a live workspace of the host's, as one would exist for
// every row the store writes.
func createWorkspace(t *testing.T, id string) {
	t.Helper()
	if _, err := testDB.Exec(`INSERT IGNORE INTO workspaces (id) VALUES (?)`, id); err != nil {
		t.Fatalf("create workspace %s: %v", id, err)
	}
}

// setWorkspaceStatus moves a workspace to status, as the host's deletion does
// ("deleting", then "deleted").
func setWorkspaceStatus(t *testing.T, id, status string) {
	t.Helper()
	if _, err := testDB.Exec(`UPDATE workspaces SET status=? WHERE id=?`, status, id); err != nil {
		t.Fatalf("set workspace %s %s: %v", id, status, err)
	}
}

// deleteWorkspace soft-deletes a workspace, as the host does once it is gone.
func deleteWorkspace(t *testing.T, id string) {
	t.Helper()
	if _, err := testDB.Exec(`UPDATE workspaces SET status='deleted', deleted_at=NOW(6) WHERE id=?`, id); err != nil {
		t.Fatalf("delete workspace %s: %v", id, err)
	}
}
