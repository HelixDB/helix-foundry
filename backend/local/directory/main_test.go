package directory_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/local/directory/migrations"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// testSchema is the database the directory's tests run in, created afresh
// on the test MySQL server.
const testSchema = "foundry_directory_test"

var testDB *sql.DB

// TestMain migrates testSchema on HELIX_TEST_ROOT_DSN, a root DSN ending in
// "/". Without it the tests that need MySQL skip; there is no default
// server.
func TestMain(m *testing.M) {
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		os.Exit(m.Run())
	}
	db, err := openSchema(rootDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "directory tests: %v\n", err)
		os.Exit(1)
	}
	testDB = db
	code := m.Run()
	_ = db.Close()
	if root, err := sql.Open("mysql", rootDSN); err == nil {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + testSchema)
		_ = root.Close()
	}
	os.Exit(code)
}

func openSchema(rootDSN string) (*sql.DB, error) {
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + testSchema, "CREATE DATABASE " + testSchema} {
		if _, err := root.Exec(stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	db, err := sql.Open("mysql", rootDSN+testSchema+"?parseTime=true")
	if err != nil {
		return nil, err
	}
	p, err := migrations.Provider(db)
	if err != nil {
		return nil, err
	}
	if _, err := p.Up(context.Background()); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// needDB is testDB, emptied, with the owner seeded; the test skips without
// a server.
func needDB(t *testing.T) *directory.Store {
	t.Helper()
	if testDB == nil {
		t.Skip("no test MySQL: set HELIX_TEST_ROOT_DSN")
	}
	for _, table := range []string{"tenants", "projects", "workspaces", "users"} {
		if _, err := testDB.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	st := directory.NewStore(testDB)
	if err := st.SeedOwner(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

// asOwner is a context of a call the install's front end let through.
func asOwner(t *testing.T, st *directory.Store) context.Context {
	t.Helper()
	owner, err := st.Owner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: owner.ID})
}
