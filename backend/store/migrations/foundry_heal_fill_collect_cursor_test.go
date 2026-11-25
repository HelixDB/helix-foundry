package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryHealFillCollectCursor adds a moved fill's saved collect cursor to
// foundry_heal_fills, after foundryCompanyResearch.
const foundryHealFillCollectCursor = 20261225000000

// The cursor goes onto a table that already holds fills, where Vitess
// renamed its CHECKs: every row keeps what it held, with no cursor; the
// CHECK refuses half a cursor and counts without one; and the Down goes
// back cleanly where Vitess renamed the cursor's CHECK too, and up again.
func TestFoundryHealFillCollectCursorMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryCompanyResearch); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	fill := func(id, state, active string, owed int) error {
		var key any
		if active != "" {
			key = active
		}
		return exec(`INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, active_key,
  links_added, docs_owed, docs_filled, reviews_taken, paused_until)
VALUES (?, 'o', ?, REPEAT(?, 64), 'v1', 'v2', ?, ?, '["mentors"]', ?, ?, ?, NOW(6))`, id, "l"+id, id[:1], state, key, owed, owed, owed)
	}
	for _, f := range []struct{ id, state, active string }{{"a", "moved", ""}, {"b", "applying", "lb"}, {"c", "filled", ""}} {
		if err := fill(f.id, f.state, f.active, 3); err != nil {
			t.Fatalf("fill %s: %v", f.id, err)
		}
	}
	vitessChecks(t, db, "foundry_heal_fills")
	if err := goose.UpTo(db, ".", foundryHealFillCollectCursor); err != nil {
		t.Fatalf("up on renamed CHECKs: %v", err)
	}
	var kept, uncursored int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(collect_after IS NULL AND collect_version_id IS NULL AND collect_owed=0 AND collect_left=0)
FROM foundry_heal_fills WHERE docs_owed=3 AND docs_filled=3 AND reviews_taken=3 AND paused_until IS NOT NULL`).Scan(&kept, &uncursored); err != nil {
		t.Fatal(err)
	}
	if kept != 3 || uncursored != 3 {
		t.Fatalf("%d fills kept, %d with no cursor; want 3, 3", kept, uncursored)
	}
	cursor := func(after, version any, owed, left int) error {
		return exec(`UPDATE foundry_heal_fills SET collect_after=?, collect_version_id=?, collect_owed=?, collect_left=? WHERE id='a'`,
			after, version, owed, left)
	}
	for _, refused := range []struct {
		name           string
		after, version any
		owed, left     int
	}{
		{"a cursor with no version", "01KDOCUMENT000000000000000", nil, 1, 0},
		{"a version with no cursor", nil, "v2", 0, 0},
		{"owed with no cursor", nil, nil, 1, 0},
		{"left with no cursor", nil, nil, 0, 1},
		{"owed less than nothing", "01KDOCUMENT000000000000000", "v2", -1, 0},
		{"left less than nothing", "01KDOCUMENT000000000000000", "v2", 0, -1},
	} {
		if err := cursor(refused.after, refused.version, refused.owed, refused.left); err == nil {
			t.Fatalf("%s was stored", refused.name)
		}
	}
	for _, ok := range []struct {
		after, version any
		owed, left     int
	}{{"01KDOCUMENT000000000000000", "v2", 0, 0}, {"01KDOCUMENT000000000000001", "v2", 50000, 7}, {nil, nil, 0, 0}} {
		if err := cursor(ok.after, ok.version, ok.owed, ok.left); err != nil {
			t.Fatalf("%v: %v", ok, err)
		}
	}
	// An older binary's writes leave the cursor as it is, whatever state they set.
	if err := cursor("01KDOCUMENT000000000000000", "v2", 2, 0); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE foundry_heal_fills SET state='applying', active_key=load_id, docs_owed=2 WHERE id='a'`); err != nil {
		t.Fatalf("an older binary's start: %v", err)
	}

	vitessChecks(t, db, "foundry_heal_fills")
	if err := goose.DownTo(db, ".", foundryCompanyResearch); err != nil {
		t.Fatalf("down on a renamed CHECK: %v", err)
	}
	var columns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()
AND table_name = 'foundry_heal_fills' AND column_name LIKE 'collect%'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("%d cursor columns left after the Down", columns)
	}
	if err := goose.UpTo(db, ".", foundryHealFillCollectCursor); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
