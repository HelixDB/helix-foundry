package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryHealFindings creates foundry_heal_findings and foundry_heal_runs.
const foundryHealFindings = 20261114000000

// The heal tables go up and down cleanly, and their checks refuse what the
// worker never writes: a dismissal without its person or support, a
// status no layer writes, a run that is live without its lease, and two
// live runs of one ontology.
func TestFoundryHealFindingsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryHealFindings); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	finding := func(status, by string, at any) error {
		var dismissedBy any
		if by != "" {
			dismissedBy = by
		}
		return exec(`INSERT INTO foundry_heal_findings (ontology_id, fingerprint, kind, status, base_version_id, load_id, finding, support, sources,
  support_at_dismissal, dismissed_by) VALUES ('o1', REPEAT('a', 64), 'unmatched', ?, 'v1', 'l1', '{}', 3, 2, ?, ?)`, status, at, dismissedBy)
	}
	for _, refused := range []struct {
		name   string
		status string
		by     string
		at     any
	}{
		{"dismissed by no one", "dismissed", "", nil},
		{"dismissed at no support", "dismissed", "u1", nil},
		{"a support with no one", "open", "", 3},
		{"an unknown status", "hidden", "", nil},
	} {
		if err := finding(refused.status, refused.by, refused.at); err == nil {
			t.Fatalf("%s was stored", refused.name)
		}
	}
	for _, status := range []string{"open", "dismissed", "superseded", "drafting", "checking", "approved", "failed"} {
		by, at := "", any(nil)
		if status == "dismissed" {
			by, at = "u1", 3
		}
		if err := finding(status, by, at); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if err := exec(`DELETE FROM foundry_heal_findings`); err != nil {
			t.Fatal(err)
		}
	}
	if err := finding("open", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := finding("open", "", nil); err == nil {
		t.Fatal("an ontology held two rows of one fingerprint")
	}

	run := func(id, status string, active, lease any, finished bool) error {
		finishedAt := "NULL"
		if finished {
			finishedAt = "NOW(6)"
		}
		return exec(`INSERT INTO foundry_heal_runs (id, ontology_id, load_id, status, active_key, lease_until, finished_at)
VALUES (?, 'o1', 'l1', ?, ?, ?, `+finishedAt+`)`, id, status, active, lease)
	}
	if err := run("r1", "running", nil, "2030-01-01", false); err == nil {
		t.Fatal("a running run with no active key was stored")
	}
	if err := run("r1", "running", 1, nil, false); err == nil {
		t.Fatal("a running run with no lease was stored")
	}
	if err := run("r1", "complete", nil, nil, false); err == nil {
		t.Fatal("a complete run with no end was stored")
	}
	if err := run("r1", "running", 1, "2030-01-01", false); err != nil {
		t.Fatal(err)
	}
	if err := run("r2", "running", 1, "2030-01-01", false); err == nil {
		t.Fatal("an ontology held two running runs")
	}
	if err := run("r2", "incomplete", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE foundry_heal_runs SET pages = 1, cuts = 2 WHERE id = 'r2'`); err == nil {
		t.Fatal("a run cut more pages than it read")
	}
	// A run measured no rows until a complete one says it did.
	var measured bool
	if err := db.QueryRow(`SELECT rows_measured FROM foundry_heal_runs WHERE id = 'r2'`).Scan(&measured); err != nil || measured {
		t.Fatalf("a new run's rows_measured = %v, %v", measured, err)
	}

	if err := goose.DownTo(db, ".", foundryHealFindings-1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"foundry_heal_findings", "foundry_heal_runs"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s outlived its migration's down", table)
		}
	}
	if err := goose.UpTo(db, ".", foundryHealFindings); err != nil {
		t.Fatal(err)
	}
}
