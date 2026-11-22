package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/local"
)

// The command refuses what it can't read before it opens the database.
func TestRebuildLoadsArguments(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"", "--current-created-before is required"},
		{"--current-created-before=yesterday", "--current-created-before: parsing time"},
		{"--current-created-before=2026-11-22", "--current-created-before: parsing time"},
		{"--current-created-before=2026-11-22T00:00:00Z extra", "unexpected arguments"},
		{"--nope", "flag provided but not defined"},
	} {
		var out bytes.Buffer
		err := runRebuildLoads(context.Background(), "unused:unused@tcp(127.0.0.1:1)/none", strings.Fields(tc.args), &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: %v, want %q", tc.args, err, tc.want)
		}
	}
}

// Over a migrated database, it says what it queued; with nothing owed,
// nothing.
func TestRebuildLoadsOverADatabase(t *testing.T) {
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		t.Skip("no test MySQL: set HELIX_TEST_ROOT_DSN")
	}
	const schema = "foundry_cmd_rebuild_test"
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + schema)
		_ = root.Close()
	})
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + schema, "CREATE DATABASE " + schema} {
		if _, err := root.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	db, err := openDB(ctx, rootDSN+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := local.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runRebuildLoads(ctx, rootDSN+schema, []string{"--current-created-before=2026-11-22T00:00:00.5+01:00"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "queued 0 reloads: \nalready reloading, 0: \nheld back by a live job, 0 (run again once it ends): \n" {
		t.Fatalf("output %q", got)
	}
}
