package migrations

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	// foundryHealFills is the migration before workspace grants.
	foundryHealFills = 20261221000000
	// foundryAccessWorkspaceGrants widens foundry_access_grants' target
	// check to the workspace kind.
	foundryAccessWorkspaceGrants = 20261223000000
)

// accessTargetChecks returns foundry_access_grants' target checks.
func accessTargetChecks(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT constraint_name FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND table_name = 'foundry_access_grants'
  AND constraint_type = 'CHECK' AND constraint_name LIKE 'foundry_access_grants_target_ch%'
ORDER BY constraint_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// The migration takes a workspace grant naming nothing, and still refuses
// one naming a connector, table or set and an unknown kind, on a host that
// names checks as MySQL does and on one that names them as Vitess does (the
// first 38 characters and a hash), leaving exactly one target check. Down
// deletes workspace grants, keeps the others and refuses workspace again.
func TestFoundryAccessWorkspaceGrantsMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryHealFills); err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) error {
				_, err := db.Exec(query, args...)
				return err
			}
			if vitess {
				if err := exec(`ALTER TABLE foundry_access_grants
  DROP CHECK foundry_access_grants_target_chk,
  ADD CONSTRAINT foundry_access_grants_target_ch_1uj2fris1di3lsi2unz9apmom CHECK (
    (target_kind = 'connector' AND connector_id <> '' AND table_name = '' AND document_set_id = '') OR
    (target_kind = 'table' AND connector_id <> '' AND table_name <> '' AND document_set_id = '') OR
    (target_kind = 'document_set' AND connector_id = '' AND table_name = '' AND document_set_id <> '') OR
    (target_kind = 'enrichment' AND connector_id = '' AND table_name = '' AND document_set_id = ''))`); err != nil {
					t.Fatal(err)
				}
			}
			grant := func(id, kind, connector, table, set string) error {
				return exec(`INSERT INTO foundry_access_grants
  (id, workspace_id, principal_kind, principal_id, target_kind, connector_id, table_name, document_set_id)
VALUES (?, 'w1', 'user', 'u1', ?, ?, ?, ?)`, id, kind, connector, table, set)
			}
			if err := grant("g0", "workspace", "", "", ""); err == nil {
				t.Fatalf("vitess=%v: a workspace grant was stored before the migration", vitess)
			}
			if err := grant("g1", "connector", "c1", "", ""); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryAccessWorkspaceGrants); err != nil {
				t.Fatal(err)
			}
			if got := accessTargetChecks(t, db); len(got) != 1 || got[0] != "foundry_access_grants_target_chk" {
				t.Fatalf("vitess=%v: target checks %v, want only the wide one", vitess, got)
			}
			if err := grant("g2", "workspace", "", "", ""); err != nil {
				t.Fatalf("vitess=%v: a workspace grant wasn't stored: %v", vitess, err)
			}
			for _, bad := range []struct{ id, kind, connector, table, set string }{
				{"g3", "workspace", "c1", "", ""},
				{"g4", "workspace", "", "t", ""},
				{"g5", "workspace", "", "", "s1"},
				{"g6", "everything", "", "", ""},
				{"g7", "connector", "", "", ""},
			} {
				if err := grant(bad.id, bad.kind, bad.connector, bad.table, bad.set); err == nil {
					t.Fatalf("vitess=%v: stored %+v", vitess, bad)
				}
			}
			for _, ok := range []struct{ id, kind, connector, table, set string }{
				{"g8", "table", "c1", "t", ""},
				{"g9", "document_set", "", "", "s1"},
				{"g10", "enrichment", "", "", ""},
			} {
				if err := grant(ok.id, ok.kind, ok.connector, ok.table, ok.set); err != nil {
					t.Fatalf("vitess=%v: %+v: %v", vitess, ok, err)
				}
			}
			if err := goose.DownTo(db, ".", foundryHealFills); err != nil {
				t.Fatal(err)
			}
			if got := accessTargetChecks(t, db); len(got) != 1 || got[0] != "foundry_access_grants_target_chk" {
				t.Fatalf("vitess=%v: down left target checks %v", vitess, got)
			}
			var kinds []string
			rows, err := db.Query(`SELECT target_kind FROM foundry_access_grants ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					t.Fatal(err)
				}
				kinds = append(kinds, k)
			}
			rows.Close()
			if want := []string{"connector", "enrichment", "table", "document_set"}; !slices.Equal(kinds, want) {
				t.Fatalf("vitess=%v: down kept %v, want %v", vitess, kinds, want)
			}
			if err := grant("g11", "workspace", "", "", ""); err == nil {
				t.Fatalf("vitess=%v: down still takes workspace grants", vitess)
			}
		})
	}
}
