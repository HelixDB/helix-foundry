package migrations

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"

	"github.com/helixdb/helix-foundry/backend/store"
)

// setup points goose at Foundry's migrations alone.
func setup(*sql.DB) error {
	goose.SetBaseFS(FS)
	return goose.SetDialect("mysql")
}

// isolatedTestDB is a fresh schema of its own on the test MySQL server
// (HELIX_TEST_ROOT_DSN, a root DSN ending in "/"), dropped after the test; a
// test skips without a server.
func isolatedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
	}
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	if err := root.Ping(); err != nil {
		root.Close()
		t.Skipf("no test DB (%v); run `make start-testdb`", err)
	}
	const schema = "foundry_migrations_test"
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + schema, "CREATE DATABASE " + schema} {
		if _, err := root.Exec(stmt); err != nil {
			root.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	root.Close()

	db, err := sql.Open("mysql", rootDSN+schema)
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		if r, err := sql.Open("mysql", rootDSN); err == nil {
			_, _ = r.Exec("DROP DATABASE IF EXISTS " + schema)
			r.Close()
		}
	})
	return db
}

// hostWorkspaces creates the host's table of workspaces, which a step locks
// rows of: the columns Foundry reads of it.
func hostWorkspaces(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE workspaces (
  id         CHAR(26)    NOT NULL PRIMARY KEY,
  status     VARCHAR(16) NOT NULL DEFAULT 'active',
  deleted_at DATETIME(6) NULL
)`); err != nil {
		t.Fatal(err)
	}
}

// workspacesTable is hostWorkspaces' table as a step reads it: it only
// names the table.
type workspacesTable struct{ store.Workspaces }

func (workspacesTable) Table() string { return "workspaces" }
