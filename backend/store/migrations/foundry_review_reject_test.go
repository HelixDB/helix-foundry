package migrations

import (
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReviewReject adds reject decisions, after foundryLoadResends.
const foundryReviewReject = 20261118000000

// The migration swaps one CHECK, where Vitess renamed the one it replaces,
// and its Down where Vitess renamed the one it added. It changes no column,
// so the rows and the identity key stay as they were.
func TestFoundryReviewRejectMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryLoadResends); err != nil {
		t.Fatal(err)
	}
	key, hash := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	insert := func(id, kind, typ, doc string, norm, claimKey, section, start, end, statement any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, norm_name, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES (?, 'o', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'u')`, id, kind, typ, doc, norm, claimKey, section, start, end, statement)
		return err
	}
	if err := insert("x0", "reject", "", "d", nil, key, 0, 1, 2, hash); err == nil {
		t.Fatal("a reject before the migration")
	}
	if err := insert("r1", "release", "", "d", nil, key, 0, 1, 2, hash); err != nil {
		t.Fatalf("a release before the migration: %v", err)
	}
	var identity []byte
	if err := db.QueryRow(`SELECT identity_key FROM foundry_review_decisions WHERE id='r1'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}

	vitessChecks(t, db, "foundry_review_decisions")
	if err := goose.UpTo(db, ".", foundryReviewReject); err != nil {
		t.Fatalf("up on a renamed CHECK: %v", err)
	}
	// The release it held is as it was, under the same identity.
	var after []byte
	if err := db.QueryRow(`SELECT identity_key FROM foundry_review_decisions WHERE id='r1'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(identity) {
		t.Fatal("the migration changed a row's identity")
	}
	var checks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE()
AND table_name = 'foundry_review_decisions' AND constraint_type = 'CHECK' AND constraint_name LIKE 'foundry_review_decisions_shape_chk%'`).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("%d shape CHECKs after the swap", checks)
	}

	// The shape holds after the swap: a reject has a release's columns, and
	// is another decision than the release of its claim.
	if err := insert("x1", "reject", "", "d", nil, key, 0, 1, 2, hash); err != nil {
		t.Fatalf("a reject: %v", err)
	}
	if err := insert("x2", "reject", "", "d", nil, key, 0, 1, 2, hash); err == nil {
		t.Fatal("a second reject of one claim")
	}
	if err := insert("x3", "reject", "", "d2", nil, key, 0, 1, 2, nil); err == nil {
		t.Fatal("a reject with no statement hash")
	}
	if err := insert("x4", "reject", "T", "d3", nil, key, 0, 1, 2, hash); err == nil {
		t.Fatal("a reject with a type")
	}
	if err := insert("x5", "reject", "", "d4", nil, key, 0, 2, 2, hash); err == nil {
		t.Fatal("a reject of an empty span")
	}
	if err := insert("x6", "dismiss", "", "d5", nil, key, 0, 1, 2, hash); err == nil {
		t.Fatal("a kind the shape doesn't know")
	}
	if err := insert("r2", "resolve", "T", "d6", "n", nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("a resolve after the swap: %v", err)
	}

	// A reject row stops the Down, which deletes nothing: a person does that
	// first, by hand, as the file says. It is safe to run again once they have.
	vitessChecks(t, db, "foundry_review_decisions")
	if err := goose.Down(db, "."); err == nil {
		t.Fatal("down with a reject row left")
	}
	if err := insert("x7", "reject", "", "d7", nil, key, 0, 1, 2, hash); err != nil {
		t.Fatalf("a refused down left the old CHECK: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM foundry_review_decisions WHERE kind = 'reject'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down on a renamed CHECK: %v", err)
	}
	if err := insert("x8", "reject", "", "d8", nil, key, 0, 1, 2, hash); err == nil {
		t.Fatal("a reject after the down")
	}
	if err := insert("r3", "release", "", "d8", nil, key, 0, 1, 2, hash); err != nil {
		t.Fatalf("a release after the down: %v", err)
	}
	if err := goose.UpTo(db, ".", foundryReviewReject); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// The file's DDL is all a deploy request carries: a DELETE in it would do
// nothing there while goose reports OK.
func TestFoundryReviewRejectMigrationIsDDLOnly(t *testing.T) {
	raw, err := FS.ReadFile("20261118000000_foundry_review_reject.sql")
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
