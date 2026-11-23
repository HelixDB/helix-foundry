package reviewcards_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
)

// testCase is one case of testdata/cards.json, which the site's mock runs
// too (mock/review-cards.ts): one document's reviews as the graph holds
// them, the saved decisions of its held claims and the links that hold one
// value, and the cards they make. A decision's statement_hash written
// "HASH:<link>:<statement>" is StatementHash of the two.
type testCase struct {
	Name      string                      `json:"name"`
	Load      string                      `json:"load"`
	Doc       string                      `json:"doc"`
	Single    []string                    `json:"single"`
	Decisions []reviewcards.ClaimDecision `json:"decisions"`
	Reviews   []*reviewcards.Review       `json:"reviews"`
	Invalid   int                         `json:"invalid"`
	Want      []wantCard                  `json:"want"`
}

type wantCard struct {
	Kind        string    `json:"kind"`
	Lane        string    `json:"lane"`
	ReviewKey   string    `json:"review_key"`
	ReviewKeys  []string  `json:"review_keys"`
	ReviewCount int       `json:"review_count"`
	Blocked     int       `json:"blocked"`
	Facts       int       `json:"facts"`
	PerDecision float64   `json:"per_decision"`
	Band        string    `json:"band,omitempty"`
	On          *[]string `json:"on,omitempty"`
}

func TestBuild(t *testing.T) {
	raw, err := os.ReadFile("testdata/cards.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []testCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			for i, d := range c.Decisions {
				if rest, ok := strings.CutPrefix(d.StatementHash, "HASH:"); ok {
					link, stored, _ := strings.Cut(rest, ":")
					c.Decisions[i].StatementHash = reviewcards.StatementHash(link, stored)
				}
			}
			built := reviewcards.Build(c.Load, c.Doc, c.Reviews, c.Decisions, func(link string) bool { return slices.Contains(c.Single, link) })
			if built.Invalid != c.Invalid {
				t.Errorf("invalid = %d, want %d", built.Invalid, c.Invalid)
			}
			got := []wantCard{}
			for _, card := range built.Cards {
				w := wantCard{Kind: string(card.Kind), Lane: string(card.Lane), ReviewKey: card.ReviewKey, ReviewKeys: card.ReviewKeys,
					ReviewCount: card.ReviewCount, Blocked: card.Blocked, Facts: card.Facts, PerDecision: card.PerDecision}
				if card.Band != nil {
					w.Band = *card.Band
				}
				if card.OnCard != nil {
					for _, n := range built.Cards {
						if n.ID == *card.OnCard {
							w.On = &[]string{*n.Type, *n.NormName}
						}
					}
				}
				if card.Shuffle != reviewcards.Shuffle(c.Load, card.ID) {
					t.Errorf("%s's shuffle isn't the load's order", card.ReviewKey)
				}
				got = append(got, w)
			}
			order := func(a, b wantCard) int {
				return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ReviewKey, b.ReviewKey))
			}
			slices.SortFunc(got, order)
			slices.SortFunc(c.Want, order)
			if !reflect.DeepEqual(got, c.Want) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(c.Want)
				t.Errorf("cards\n got %s\nwant %s", g, w)
			}
		})
	}
}

// A review keyed again (a resend) keeps its card: ids hash the decision's
// identity. A name's card names at most MaxReviewKeys reviews, and says how
// many it has.
func TestBuildIDsAndKeys(t *testing.T) {
	const doc = "D0000000000000000000000001"
	stmt := `{"subject":{"type":"Person","name":"Ada"},"object":{"type":"Company","name":"Acme"},"props":{}}`
	review := func(key string) []*reviewcards.Review {
		section, start, end, c := int64(0), int64(1), int64(9), 0.7
		return []*reviewcards.Review{
			{Key: key + "-claim", Reason: "low_confidence", DocID: doc, Statement: stmt, ClaimKey: strings.Repeat("ab", 32), Claim: `{"about":[1,2]}`,
				Section: &section, Start: &start, End: &end, SuggestedLink: "works_at", Confidence: &c},
			{Key: key + "-name", Reason: "unresolved", DocID: doc, Type: "Customer", NormName: "globex"},
			{Key: key + "-value", Reason: "prop_conflict", DocID: doc, Type: "Customer", EntityKey: "scus_1", Prop: "currency",
				Values:    `[{"value":"GBP","doc_id":"` + doc + `","quote":"q","span":{"section":0,"start":0,"end":1}}]`,
				Concerned: []reviewcards.Entity{{Type: "Customer", Key: "scus_1"}}},
		}
	}
	none := func(string) bool { return false }
	before := reviewcards.Build("L", doc, review("a"), nil, none)
	after := reviewcards.Build("L", doc, review("b"), nil, none)
	if len(before.Cards) != 3 || len(after.Cards) != 3 {
		t.Fatalf("cards = %d, %d", len(before.Cards), len(after.Cards))
	}
	for i := range before.Cards {
		if before.Cards[i].ID != after.Cards[i].ID || before.Cards[i].ReviewKey == after.Cards[i].ReviewKey {
			t.Errorf("card %d: id %x became %x as its review %s became %s", i, before.Cards[i].ID, after.Cards[i].ID,
				before.Cards[i].ReviewKey, after.Cards[i].ReviewKey)
		}
	}

	var many []*reviewcards.Review
	for i := range 205 {
		start := int64(i)
		many = append(many, &reviewcards.Review{Key: fmt.Sprintf("r%03d", i), Reason: "ambiguous", DocID: doc, Type: "Customer", NormName: "globex",
			Start: &start, Candidates: map[bool][]string{true: {"scus_1"}}[i == 150]})
	}
	built := reviewcards.Build("L", doc, many, nil, none)
	name := built.Cards[0]
	if name.ReviewCount != 205 || len(name.ReviewKeys) != reviewcards.MaxReviewKeys || name.ReviewKeys[0] != "r150" || name.ReviewKeys[1] != "r000" {
		t.Errorf("a name of 205 reviews names %d (first %s, then %s) of %d", len(name.ReviewKeys), name.ReviewKeys[0], name.ReviewKeys[1], name.ReviewCount)
	}
}

func TestShuffleIsTheLoadsHashUnit(t *testing.T) {
	id := reviewcards.NameID("Customer", "D", "globex")
	if got, want := float64(reviewcards.Shuffle("L", id))/(1<<32), reviewcards.HashUnit("L", "order", fmt.Sprintf("%x", id[:])); got != want {
		t.Fatalf("shuffle %v, hashUnit %v", got, want)
	}
	// The site's hashUnit("load-1", "x"), computed in the browser.
	if got := reviewcards.HashUnit("load-1", "x"); got < 0 || got >= 1 {
		t.Fatalf("hashUnit = %v", got)
	}
}

func TestBandOf(t *testing.T) {
	for _, c := range []struct {
		score *float64
		want  string
	}{{nil, "none"}, {ptr(0), "none"}, {ptr(0.000001), "low"}, {ptr(0.599), "low"}, {ptr(0.6), "0.6"}, {ptr(0.85), "0.8"}, {ptr(0.9), "0.9"}, {ptr(1), "0.9"}} {
		if got := reviewcards.Bands[reviewcards.BandOf(c.score)]; got != c.want {
			t.Errorf("BandOf(%v) = %s, want %s", c.score, got, c.want)
		}
	}
}

// testdata/names.json is what the site's normalizeName (explore/naming.ts)
// makes of each name, and the engine's norm_name: they must agree, as a
// held claim's ends are matched to names by it.
func TestNames(t *testing.T) {
	raw, err := os.ReadFile("testdata/names.json")
	if err != nil {
		t.Fatal(err)
	}
	var names [][2]string
	if err := json.Unmarshal(raw, &names); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if got := namewords.NormName(n[0]); got != n[1] {
			t.Errorf("NormName(%q) = %q, want %q", n[0], got, n[1])
		}
	}
}

func TestNext(t *testing.T) {
	// Only quotes in 0.8: every draw is one.
	pools := [6]reviewcards.Pool{1: {Quotes: 3}}
	for n := range 12 {
		d, ok := reviewcards.Next("L", n, pools)
		if !ok || d.Band != 1 || !d.Quote {
			t.Fatalf("draw %d = %+v, %t", n, d, ok)
		}
	}
	if _, ok := reviewcards.Next("L", 0, [6]reviewcards.Pool{}); ok {
		t.Fatal("a draw from nothing")
	}
	// The bands in turn, passing over an empty one.
	pools = [6]reviewcards.Pool{0: {Held: 5}, 2: {Held: 5}}
	var bands []int
	for n := range 6 {
		d, _ := reviewcards.Next("L", n, pools)
		bands = append(bands, d.Band)
	}
	if !slices.Equal(bands, []int{0, 2, 2, 0, 0, 0}) {
		t.Fatalf("bands = %v", bands)
	}
	// Held claims and quotes in proportion.
	quotes := 0
	for n := range 6000 {
		if d, _ := reviewcards.Next("L", n*6, [6]reviewcards.Pool{0: {Held: 1, Quotes: 3}}); d.Quote {
			quotes++
		}
	}
	if quotes < 4200 || quotes > 4800 {
		t.Fatalf("%d of 6000 draws were quotes, want about 4500", quotes)
	}
	if got := reviewcards.QuoteTarget("L", 0, 0, 10); got < 0 || got > 9 {
		t.Fatalf("target %d", got)
	}
	q := []reviewcards.Quote{{Start: 1, End: 2, Link: "a"}, {Start: 3, End: 4, Link: "a"}, {Start: 5, End: 6, Link: "a"}}
	if a, b := reviewcards.QuoteOrder("L", "D", q), reviewcards.QuoteOrder("L", "D", []reviewcards.Quote{q[2], q[0], q[1]}); !reflect.DeepEqual(a, b) {
		t.Fatal("a document's quotes are drawn in an order that depends on how they were read")
	}
}

func TestRechecked(t *testing.T) {
	n := 0
	for i := range 20000 {
		id := fmt.Sprintf("%026d", i)
		if reviewcards.Rechecked(id) != (reviewcards.HashUnit("recheck", id) < 0.05) {
			t.Fatalf("%s: Rechecked disagrees with HashUnit", id)
		}
		if reviewcards.Rechecked(id) {
			n++
		}
	}
	if n < 800 || n > 1200 {
		t.Fatalf("%d of 20000 rechecked", n)
	}
}

func ptr(f float64) *float64 { return &f }
