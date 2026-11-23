package migrations

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReviewCards adds the review cards index, after source audiences.
const foundryReviewCards = 20261218000000

// A card and the decision of its identity join on identity_key whatever the
// values hold (quotes, backslashes, emoji, Turkish İ, NULLs) and whatever
// display columns of the other kind a card fills, and no two identities
// join; a held claim has none. name_hash reads a resolve's decisions by its
// type and name. The shapes the CHECKs refuse are refused, and the Down and
// the Up run again.
func TestFoundryReviewCardsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryReviewCards); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(7, 11))
	pieces := []string{"a", "Globex", `"`, `\`, `\"`, "'", "🙂", "İ", "i̇", " ", "ß", "\t", "Ünï", "{}", "[1]", "null", "NULL"}
	text := func() string {
		var b strings.Builder
		for range 1 + r.IntN(4) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		return b.String()
	}
	docID := func(n int) string { return fmt.Sprintf("D%025d", n) }
	type pair struct {
		kind, ty, doc, name, entity, prop string
	}
	var pairs []pair
	for i := range 300 {
		p := pair{kind: []string{"resolve", "prop"}[i%2], ty: text(), doc: docID(r.IntN(5)), name: text(), entity: text(), prop: text()}
		pairs = append(pairs, p)
		var err error
		switch p.kind {
		case "resolve":
			// A resolve's chosen entity is no part of its identity.
			_, err = db.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, entity_key, decided_by_user_id)
 VALUES (?, 'o', 'resolve', ?, ?, ?, ?, 'u')`, fmt.Sprintf("d%d", i), p.ty, p.doc, p.name, p.entity)
		case "prop":
			_, err = db.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, entity_key, prop, value, decided_by_user_id)
 VALUES (?, 'o', 'prop', ?, ?, ?, '1', 'u')`, fmt.Sprintf("d%d", i), p.ty, p.entity, p.prop)
		}
		if err != nil {
			// Two random identities may meet: the unique key keeps one.
			if strings.Contains(err.Error(), "Duplicate") {
				pairs = pairs[:len(pairs)-1]
				continue
			}
			t.Fatalf("decision %d: %v", i, err)
		}
		// The card fills every display column, the other kind's included.
		if _, err := db.Exec(`INSERT INTO foundry_review_cards (load_id, card_id, ontology_id, kind, lane, kind_order, doc_id, type, norm_name,
 entity_key, prop, review_key, review_keys, review_count, blocked, facts, per_decision, shuffle)
 VALUES ('l', UNHEX(SHA2(?, 256)), 'o', ?, 'decide', ?, ?, ?, ?, ?, ?, 'k', '[]', 1, 0, 0, 1, 0)`,
			fmt.Sprint(i), p.kind, map[string]int{"resolve": 1, "prop": 2}[p.kind], p.doc, p.ty, p.name, p.entity, p.prop); err != nil {
			t.Fatalf("card %d: %v", i, err)
		}
	}
	var joined, wrong int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(NOT (d.kind=c.kind AND d.type=c.type)), 0) FROM foundry_review_cards c
JOIN foundry_review_decisions d ON d.ontology_id=c.ontology_id AND d.identity_key=c.identity_key`).Scan(&joined, &wrong); err != nil {
		t.Fatal(err)
	}
	if joined != len(pairs) || wrong != 0 {
		t.Fatalf("%d of %d cards joined their decisions, %d of another identity", joined, len(pairs), wrong)
	}
	var unjoined int
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_review_cards c WHERE NOT EXISTS (
 SELECT 1 FROM foundry_review_decisions d WHERE d.ontology_id=c.ontology_id AND d.identity_key=c.identity_key)`).Scan(&unjoined); err != nil {
		t.Fatal(err)
	}
	if unjoined != 0 {
		t.Fatalf("%d cards joined no decision", unjoined)
	}

	// A held claim has no identity_key: it joins its decisions by place.
	key, hash := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	card := func(id, kind, lane string, order int, band, claimKey, onCard any, start, end int) error {
		_, err := db.Exec(`INSERT INTO foundry_review_cards (load_id, card_id, ontology_id, kind, lane, kind_order, on_card, doc_id, type, norm_name,
 entity_key, prop, claim_key, claim_section, claim_start, claim_end, statement_hash, review_key, review_keys, review_count, band, blocked, facts,
 per_decision, shuffle)
 VALUES ('l', UNHEX(SHA2(?, 256)), 'o', ?, ?, ?, ?, 'D', 'T', IF(?='resolve', 'n', NULL), IF(?='prop', 'e', NULL), IF(?='prop', 'p', NULL),
  ?, 0, ?, ?, ?, 'k', '[]', 1, ?, 0, 1, 1, 0)`,
			id, kind, lane, order, onCard, kind, kind, kind, claimKey, start, end, map[bool]any{true: hash, false: nil}[claimKey != nil], band)
		return err
	}
	if err := card("held", "release", "decide", 0, "0.9", key, []byte(strings.Repeat("x", 32)), 1, 2); err != nil {
		t.Fatalf("a held claim on a name's card: %v", err)
	}
	if err := card("pool", "release", "pool", 0, "low", key, nil, 3, 4); err != nil {
		t.Fatalf("a held claim of the pool: %v", err)
	}
	var identity []byte
	if err := db.QueryRow(`SELECT identity_key FROM foundry_review_cards WHERE card_id=UNHEX(SHA2('held', 256))`).Scan(&identity); err != nil || identity != nil {
		t.Fatalf("a held claim's identity = %x, %v", identity, err)
	}
	for name, err := range map[string]error{
		"a name in the pool":            card("x1", "resolve", "pool", 1, nil, nil, nil, 1, 2),
		"a held claim without a band":   card("x2", "release", "decide", 0, nil, key, nil, 1, 2),
		"a name with a band":            card("x3", "resolve", "decide", 1, "0.9", nil, nil, 1, 2),
		"a value out of its order":      card("x4", "prop", "decide", 0, nil, nil, nil, 1, 2),
		"an empty span":                 card("x5", "release", "decide", 0, "0.9", key, nil, 2, 2),
		"a held claim in fyi":           card("x6", "release", "fyi", 0, "0.9", key, nil, 1, 2),
		"a pool claim on a name's card": card("x7", "release", "pool", 0, "0.9", key, []byte(strings.Repeat("x", 32)), 1, 2),
		"a name on a name's card":       card("x8", "resolve", "decide", 1, nil, nil, []byte(strings.Repeat("x", 32)), 1, 2),
	} {
		if err == nil {
			t.Fatal(name)
		}
	}

	// name_hash: a resolve's decisions by type and name, through its index.
	var named int
	p := pairs[0]
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_review_decisions FORCE INDEX (foundry_review_decisions_names_idx)
WHERE ontology_id='o' AND kind='resolve' AND name_hash=UNHEX(SHA2(CAST(JSON_ARRAY(?, ?) AS CHAR), 256)) AND type=? AND norm_name=?`,
		p.ty, p.name, p.ty, p.name).Scan(&named); err != nil || named != 1 {
		t.Fatalf("the decisions of a name = %d, %v", named, err)
	}

	draw := func(seq int, kind, band string, claimKey, fact any) error {
		_, err := db.Exec(`INSERT INTO foundry_review_draws (load_id, seq, kind, band, doc_id, claim_section, claim_start, claim_end, claim_key,
 statement_hash, fact_key) VALUES ('l', ?, ?, ?, 'D', 0, 1, 2, ?, IF(? IS NULL, NULL, ?), ?)`, seq, kind, band, claimKey, claimKey, hash, fact)
		return err
	}
	if err := draw(0, "held", "0.9", key, nil); err != nil {
		t.Fatalf("a held draw: %v", err)
	}
	if err := draw(1, "quote", "none", nil, "fk"); err != nil {
		t.Fatalf("a quote draw: %v", err)
	}
	for name, err := range map[string]error{
		"a seq drawn twice":      draw(1, "quote", "none", nil, "fk"),
		"a held draw with a key": draw(2, "held", "0.9", key, "fk"),
		"a quote without a key":  draw(3, "quote", "0.9", nil, nil),
		"a band it doesn't know": draw(4, "quote", "0.5", nil, "fk"),
	} {
		if err == nil {
			t.Fatal(name)
		}
	}
	if _, err := db.Exec(`INSERT INTO foundry_review_loads (load_id, ontology_id, full_started_at, full_cursor) VALUES ('l2', 'o', NOW(6), '')`); err == nil {
		t.Fatal("a pass running without a request")
	}

	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, table := range []string{"foundry_review_loads", "foundry_review_owed", "foundry_review_docs", "foundry_review_cards", "foundry_review_draws", "foundry_review_reads"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s outlived the down (%v)", table, err)
		}
	}
	if err := db.QueryRow(`SELECT name_hash FROM foundry_review_decisions LIMIT 1`).Scan(new([]byte)); err == nil {
		t.Fatal("name_hash outlived the down")
	}
	if err := goose.UpTo(db, ".", foundryReviewCards); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
