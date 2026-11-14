package migrations

import (
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReviewQueue adds drawn to decisions and the labels table, after
// the workflow run assessments.
const foundryReviewQueue = 20261124000000

// The migration adds a column and a table and changes no row: decisions
// saved before it are drawn by nothing and keep their identity. The column
// takes ranked, random or nothing; the table keeps one label per person and
// claim, of a held claim or an accepted fact, in the shapes its CHECK lets
// through. The Down drops both where Vitess renamed the column's CHECK, and
// the Up runs again after it.
func TestFoundryReviewQueueMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", 20261121000000); err != nil {
		t.Fatal(err)
	}
	key, hash := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if _, err := db.Exec(`INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES ('r1', 'o', 'release', '', 'd', ?, 0, 1, 2, ?, 'u')`, key, hash); err != nil {
		t.Fatal(err)
	}
	var identity []byte
	if err := db.QueryRow(`SELECT identity_key FROM foundry_review_decisions WHERE id='r1'`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryReviewQueue); err != nil {
		t.Fatal(err)
	}
	var after []byte
	var drawn *string
	if err := db.QueryRow(`SELECT identity_key, drawn FROM foundry_review_decisions WHERE id='r1'`).Scan(&after, &drawn); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(identity) || drawn != nil {
		t.Fatalf("the migration changed a decision: drawn %v", drawn)
	}
	decide := func(id, kind string, d any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, decided_by_user_id, drawn)
 VALUES (?, 'o', ?, 'T', ?, 'n', 'u', ?)`, id, kind, id, d)
		return err
	}
	for id, d := range map[string]any{"x-ranked": "ranked", "x-random": "random", "x-none": nil} {
		if err := decide(id, "resolve", d); err != nil {
			t.Fatalf("drawn %v: %v", d, err)
		}
	}
	if err := decide("x-bad", "resolve", "lucky"); err == nil {
		t.Fatal("a draw the column doesn't know")
	}

	label := func(id, user, drawn, verdict string, claimKey, statement, fact, against, confidence any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_labels
 (id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end, claim_key, statement_hash, fact_key, confidence, verdict, against, decided_by_user_id)
 VALUES (?, 'o', 'l', ?, 'd', 0, 1, 2, ?, ?, ?, ?, ?, ?, ?)`, id, drawn, claimKey, statement, fact, confidence, verdict, against, user)
		return err
	}
	if err := label("l1", "ann", "random", "right", key, hash, nil, nil, 0.83); err != nil {
		t.Fatalf("a held claim's label: %v", err)
	}
	if err := label("l2", "ann", "random", "wrong", nil, nil, "fk", nil, nil); err != nil {
		t.Fatalf("an accepted fact's label: %v", err)
	}
	if err := label("l3", "bob", "recheck", "wrong", key, hash, nil, "right", nil); err != nil {
		t.Fatalf("a re-check: %v", err)
	}
	for name, err := range map[string]error{
		"a second label by one person": label("x1", "ann", "random", "wrong", key, hash, nil, nil, nil),
		"both keys":                    label("x2", "cat", "random", "right", key, hash, "fk", nil, nil),
		"no key":                       label("x3", "cat", "random", "right", nil, nil, nil, nil, nil),
		"a key without its statement":  label("x4", "cat", "random", "right", key, nil, nil, nil, nil),
		"a verdict it doesn't know":    label("x5", "cat", "random", "maybe", key, hash, nil, nil, nil),
		"a draw it doesn't know":       label("x6", "cat", "ranked", "right", key, hash, nil, nil, nil),
		"a re-check of nothing":        label("x7", "cat", "recheck", "right", key, hash, nil, nil, nil),
		"a random label that checks":   label("x8", "cat", "random", "right", key, hash, nil, "right", nil),
		"a score above one":            label("x9", "cat", "random", "right", key, hash, nil, nil, 1.5),
	} {
		if err == nil {
			t.Fatal(name)
		}
	}
	if _, err := db.Exec(`INSERT INTO foundry_review_labels (id, ontology_id, load_id, drawn, doc_id, claim_section, claim_start, claim_end, fact_key, verdict, decided_by_user_id)
 VALUES ('x10', 'o', 'l', 'random', 'd', 0, 2, 2, 'fk', 'right', 'cat')`); err == nil {
		t.Fatal("an empty span")
	}

	vitessChecks(t, db, "foundry_review_decisions")
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down on a renamed CHECK: %v", err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'foundry_review_labels'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("the labels table outlived the down")
	}
	if err := decide("x-after", "resolve", nil); err == nil {
		t.Fatal("drawn outlived the down")
	}
	if _, err := db.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, decided_by_user_id)
 VALUES ('x-after', 'o', 'resolve', 'T', 'x-after', 'n', 'u')`); err != nil {
		t.Fatalf("a decision after the down: %v", err)
	}
	if err := goose.UpTo(db, ".", foundryReviewQueue); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// The file's DDL is all a deploy request carries: a DML statement in it
// would do nothing there while goose reports OK.
func TestFoundryReviewQueueMigrationIsDDLOnly(t *testing.T) {
	raw, err := FS.ReadFile("20261124000000_foundry_review_queue.sql")
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
