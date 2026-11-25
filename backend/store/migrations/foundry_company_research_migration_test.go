package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryCompanyResearch adds company research's calls.
const foundryCompanyResearch = 20261224000000

// The company research migration goes up and down cleanly, also on a host
// that names its CHECKs as Vitess does (the first 38 characters and a
// hash), and its CHECKs hold.
func TestFoundryCompanyResearchMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryCompanyResearch); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain) VALUES ('r1', 'w', 'u', 'audit', 'acme.com')`,
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain, result) VALUES ('r2', 'w', 'u', 'goal', 'acme.com', '{}')`,
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain, usd_micros) VALUES ('r3', 'w', 'u', 'goal', 'acme.com', -1)`,
			} {
				if _, err := db.Exec(bad); err == nil {
					t.Fatalf("a CHECK let through %s", bad)
				}
			}
			for _, good := range []string{
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain, result, usd_micros)
VALUES ('r4', 'w', 'u', 'research', 'acme.com', '{"company_name":"Acme"}', 1200)`,
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain) VALUES ('r5', 'w', 'u', 'research', 'acme.com')`,
				`INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain, usd_micros) VALUES ('r6', 'w', 'u', 'goal', 'acme.com', 30)`,
			} {
				if _, err := db.Exec(good); err != nil {
					t.Fatal(err)
				}
			}
			if vitess {
				if _, err := db.Exec(`ALTER TABLE foundry_company_research
  DROP CHECK foundry_company_research_kind_chk,
  DROP CHECK foundry_company_research_result_chk,
  DROP CHECK foundry_company_research_spend_chk,
  ADD CONSTRAINT foundry_company_research_kind_chk_5z0d1kqv0b8r2m3n4p5q6r7s8 CHECK (kind IN ('research', 'goal')),
  ADD CONSTRAINT foundry_company_research_result_c_9a8b7c6d5e4f3g2h1j0k9l8m7 CHECK (kind = 'research' OR result IS NULL),
  ADD CONSTRAINT foundry_company_research_spend_ch_1q2w3e4r5t6y7u8i9o0p1a2s3 CHECK (usd_micros >= 0)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := goose.DownTo(db, ".", foundryCompanyResearch-1); err != nil {
				t.Fatalf("down: %v", err)
			}
			var left int
			if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = 'foundry_company_research'`).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Fatal("down left the table")
			}
			if err := goose.UpTo(db, ".", foundryCompanyResearch); err != nil {
				t.Fatalf("up again: %v", err)
			}
		})
	}
}
