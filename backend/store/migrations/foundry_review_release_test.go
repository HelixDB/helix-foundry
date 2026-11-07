package migrations

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReviewRelease adds release decisions, after foundryHealFindings.
const foundryReviewRelease = 20261115000000

// vitessChecks renames table's CHECK constraints as a Vitess host does when it
// copies a schema onto a branch: a suffix on the name, which a plain DROP CHECK
// of the name doesn't find. Plain MySQL has no rename, so each is dropped and
// added again from its stored clause.
func vitessChecks(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	rows, err := db.Query(`SELECT tc.constraint_name, cc.check_clause
FROM information_schema.table_constraints tc
JOIN information_schema.check_constraints cc
  ON cc.constraint_schema = tc.constraint_schema AND cc.constraint_name = tc.constraint_name
WHERE tc.constraint_schema = DATABASE() AND tc.table_name = ? AND tc.constraint_type = 'CHECK'`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var changes []string
	for rows.Next() {
		var name, clause string
		if err := rows.Scan(&name, &clause); err != nil {
			t.Fatal(err)
		}
		clause = strings.ReplaceAll(clause, `\'`, `'`)
		changes = append(changes, fmt.Sprintf("DROP CHECK `%s`, ADD CONSTRAINT `%s_5x1q9b0kd3` CHECK (%s)", name, name, clause))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(changes) == 0 {
		t.Fatalf("%s has no CHECK to rename", table)
	}
	if _, err := db.Exec("ALTER TABLE " + table + " " + strings.Join(changes, ", ")); err != nil {
		t.Fatal(err)
	}
}

// columnsOf counts the release columns of a table.
func columnsOf(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?
AND column_name IN ('claim_key', 'claim_section', 'claim_start', 'claim_end', 'statement_hash')`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The migration runs where Vitess renamed the CHECK it replaces, and its Down
// where Vitess renamed the one it added: the failure local MySQL hides, on the
// names a PlanetScale branch has.
func TestFoundryReviewReleaseMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryHealFindings); err != nil {
		t.Fatal(err)
	}
	vitessChecks(t, db, "foundry_review_decisions")
	if err := goose.UpTo(db, ".", foundryReviewRelease); err != nil {
		t.Fatalf("up on a renamed CHECK: %v", err)
	}
	for _, table := range []string{"foundry_review_decisions", "foundry_load_decisions"} {
		if n := columnsOf(t, db, table); n != 5 {
			t.Fatalf("%s has %d release columns", table, n)
		}
	}

	// The shape holds after the swap: a release with all of it, nothing else.
	key, hash := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	insert := func(id, kind, typ, doc string, norm, claimKey, section, start, end, statement any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, norm_name, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES (?, 'o', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'u')`, id, kind, typ, doc, norm, claimKey, section, start, end, statement)
		return err
	}
	if err := insert("r1", "release", "", "d", nil, key, 0, 1, 2, hash); err != nil {
		t.Fatalf("a release: %v", err)
	}
	if err := insert("r2", "release", "", "d2", nil, key, 0, 1, 2, nil); err == nil {
		t.Fatal("a release with no statement hash")
	}
	if err := insert("r3", "resolve", "T", "d3", "n", nil, nil, nil, nil, hash); err == nil {
		t.Fatal("a resolve with a statement hash")
	}

	// A release row stops the Down, which deletes nothing: a person does that
	// first, by hand, as the file says. It is safe to run again once they have.
	vitessChecks(t, db, "foundry_review_decisions")
	if err := goose.Down(db, "."); err == nil {
		t.Fatal("down with a release row left")
	}
	if n := columnsOf(t, db, "foundry_load_decisions"); n != 5 {
		t.Fatalf("a refused down left foundry_load_decisions with %d release columns", n)
	}
	if _, err := db.Exec(`DELETE FROM foundry_review_decisions WHERE kind = 'release'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down on a renamed CHECK: %v", err)
	}
	for _, table := range []string{"foundry_review_decisions", "foundry_load_decisions"} {
		if n := columnsOf(t, db, table); n != 0 {
			t.Fatalf("%s kept %d release columns", table, n)
		}
	}
	if err := goose.UpTo(db, ".", foundryReviewRelease); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// The file's DDL is all a deploy request carries: a DELETE in it would do
// nothing there while goose reports OK.
func TestFoundryReviewReleaseMigrationIsDDLOnly(t *testing.T) {
	raw, err := FS.ReadFile("20261115000000_foundry_review_release.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if up := strings.ToUpper(line); strings.Contains(up, "DELETE FROM") || strings.Contains(up, "UPDATE FOUNDRY") || strings.Contains(up, "INSERT INTO") {
			t.Fatalf("DML in the migration: %s", line)
		}
	}
}
