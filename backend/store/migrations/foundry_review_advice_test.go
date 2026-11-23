package migrations

import (
	"fmt"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReviewAdvice adds review advice, after the review cards index.
const foundryReviewAdvice = 20261219000000

// The CHECKs refuse every shape no outcome has, and a stop or month they
// don't know; the Down drops the three tables and the Up runs again.
func TestFoundryReviewAdviceMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryReviewAdvice); err != nil {
		t.Fatal(err)
	}
	n := 0
	advice := func(outcome string, offered, choice, rationale, cites, model, prompt any) error {
		n++
		_, err := db.Exec(`INSERT INTO foundry_review_advice (load_id, card_id, outcome, offered, choice, rationale, cites, model, prompt_version)
 VALUES ('l', UNHEX(SHA2(?, 256)), ?, ?, ?, ?, ?, ?, ?)`, n, outcome, offered, choice, rationale, cites, model, prompt)
		return err
	}
	const offered, choice, cites = `[{"resolve":{"notInData":true}}]`, `{"resolve":{"notInData":true}}`, `[{"docId":"d"}]`
	for name, err := range map[string]error{
		"advised": advice("advised", offered, choice, "why", cites, "m", "review-advice-1"),
		"none":    advice("none", offered, nil, nil, nil, "m", "review-advice-1"),
		"rules":   advice("rules", nil, nil, nil, nil, nil, nil),
		"skipped": advice("skipped", nil, nil, nil, nil, nil, nil),
	} {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"an outcome it doesn't know":   advice("maybe", nil, nil, nil, nil, nil, nil),
		"advised without a choice":     advice("advised", offered, nil, nil, nil, "m", "p"),
		"a choice without a rationale": advice("advised", offered, choice, nil, cites, "m", "p"),
		"a choice without cites":       advice("advised", offered, choice, "why", nil, "m", "p"),
		"none with a choice":           advice("none", offered, choice, "why", cites, "m", "p"),
		"none without a model":         advice("none", offered, nil, nil, nil, nil, nil),
		"rules with a model":           advice("rules", offered, nil, nil, nil, "m", "p"),
		"a model without its prompt":   advice("none", offered, nil, nil, nil, "m", nil),
		"a model without offered":      advice("none", nil, nil, nil, nil, "m", "p"),
		"skipped with offered":         advice("skipped", offered, nil, nil, nil, nil, nil),
	} {
		if err == nil {
			t.Fatal(name)
		}
	}
	load := func(id string, stopped any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_advice_loads (load_id, workspace_id, ontology_id, stopped) VALUES (?, 'w', 'o', ?)`, id, stopped)
		return err
	}
	for i, stopped := range []any{nil, "done", "limit", "budget", "plan", "no_model", "failed"} {
		if err := load(fmt.Sprint("l", i), stopped); err != nil {
			t.Fatalf("stopped %v: %v", stopped, err)
		}
	}
	if err := load("x", "tired"); err == nil {
		t.Fatal("a stop it doesn't know")
	}
	spend := func(month string, micros int64) error {
		_, err := db.Exec(`INSERT INTO foundry_review_advice_spend (workspace_id, month, usd_micros) VALUES ('w', ?, ?)`, month, micros)
		return err
	}
	if err := spend("2026-11-01", 5); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{"a month not on its 1st": spend("2026-12-02", 1), "a negative spend": spend("2027-01-01", -1)} {
		if err == nil {
			t.Fatal(name)
		}
	}

	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, table := range []string{"foundry_review_advice_loads", "foundry_review_advice", "foundry_review_advice_spend"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s outlived the down (%v)", table, err)
		}
	}
	if err := goose.UpTo(db, ".", foundryReviewAdvice); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
