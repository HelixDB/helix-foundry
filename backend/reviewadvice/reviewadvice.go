// Package reviewadvice is Founder's model's advice on a review card the
// rules leave undecided: a name between candidates the rules can't tell
// apart (tied_candidates), and a value documents state differently with no
// clear majority (values_split) or too few quotes to have one (few_quotes).
// The graph worker's advisor asks the workspace's verify model which choice
// the evidence supports and stores its pick beside the choices it was
// offered; the walk shows it, as unmeasured and never batched, only while
// the card still offers those choices.
//
// The site decides what shows (src/web/areas/reviews/advice.ts advise and
// MODEL_WHYS); Why is a port of the rules' three abstentions that decides
// only what is paid for, kept in step by testdata/abstain.json, which both
// run. Values turns a review's quotes into choices exactly as the site's
// model.ts statedValue and choicesOf do (testdata/values.json, run by both),
// so the choices stored match the card's.
//
// Every string from the graph reaches the model wrapped as untrusted data
// (Prompt); the model picks option and evidence ids, never values, and its
// rationale is plain text, sanitized and capped (Parse). Nothing here talks
// to a store, a model or the network.
package reviewadvice

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"strconv"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
)

// The rules' abstentions the model advises on, as the site's NoAdvice names
// them.
const (
	WhyTiedCandidates = "tied_candidates"
	WhyValuesSplit    = "values_split"
	WhyFewQuotes      = "few_quotes"
)

// MinQuotes is how many quotes a value's majority needs, as the site's
// MIN_QUOTES.
const MinQuotes = 3

// Choice is a FoundryReviewChoice as its REST JSON writes it: a name's
// resolve or a value's prop.
type Choice struct {
	Resolve *Resolve `json:"resolve,omitempty"`
	Prop    *Value   `json:"prop,omitempty"`
}

// Resolve is a name's choice: an entity, or not in the data.
type Resolve struct {
	EntityKey string `json:"entityKey,omitempty"`
	NotInData bool   `json:"notInData,omitempty"`
}

// Value is a FoundryPropValue a document states, one field set: text (a
// string, date or datetime), integer (its exact digits), number or boolean.
type Value struct {
	Text    *string  `json:"text,omitempty"`
	Integer *string  `json:"integer,omitempty"`
	Number  *float64 `json:"number,omitempty"`
	Boolean *bool    `json:"boolean,omitempty"`
}

// key is v's identity among a card's values, as the site's
// JSON.stringify(value) tells them apart: an integer is never a number, and
// -0 is 0.
func (v Value) key() string {
	switch {
	case v.Text != nil:
		return "t" + *v.Text
	case v.Integer != nil:
		return "i" + *v.Integer
	case v.Number != nil:
		return "n" + strconv.FormatFloat(*v.Number+0, 'g', -1, 64)
	case v.Boolean != nil:
		return "b" + strconv.FormatBool(*v.Boolean)
	}
	return ""
}

// Cite is a place in a document the model's pick stands on.
type Cite struct {
	DocID   string `json:"docId"`
	Section int64  `json:"section"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
}

// Quote is one PropQuote of a value review: the value a document states,
// and where.
type Quote struct {
	Value   Value
	DocID   string
	Quote   string
	Section int64
	Start   int64
	End     int64
}

// Quotes reads a value review's `values` (JSON text of PropQuote records,
// none of it trusted) as the site's reviewValues does: a quote whose value,
// document, text or place doesn't read is left out.
//
//	Quotes(`[{"value": 9007199254740993, "doc_id": "d", "quote": "q", "span": {"section": 0, "start": 0, "end": 1}}]`)[0].Value
//	// {Integer: "9007199254740993"}
func Quotes(raw string) []Quote {
	var items []json.RawMessage
	if json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	var out []Quote
	for _, each := range items {
		var item, span map[string]json.RawMessage
		if json.Unmarshal(each, &item) != nil || json.Unmarshal(item["span"], &span) != nil || span == nil {
			continue
		}
		value, ok := statedValue(item["value"])
		section, s1 := safeOffset(span["section"])
		start, s2 := safeOffset(span["start"])
		end, s3 := safeOffset(span["end"])
		var doc, quote *string
		_ = json.Unmarshal(item["doc_id"], &doc) // best-effort: anything but a string is none
		_ = json.Unmarshal(item["quote"], &quote)
		if !ok || !s1 || !s2 || !s3 || end <= start || doc == nil || *doc == "" || quote == nil {
			continue
		}
		out = append(out, Quote{Value: value, DocID: *doc, Quote: *quote, Section: section, Start: start, End: end})
	}
	return out
}

var integerLiteral = regexp.MustCompile(`^-?\d+$`)

// statedValue is a quote's JSON value as a choice takes it: a string as
// text, a boolean as itself, an integer literal as its digits, any other
// finite number as a number.
func statedValue(raw json.RawMessage) (Value, bool) {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0:
		return Value{}, false
	case raw[0] == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return Value{}, false
		}
		return Value{Text: &s}, true
	case string(raw) == "true" || string(raw) == "false":
		b := string(raw) == "true"
		return Value{Boolean: &b}, true
	case integerLiteral.Match(raw):
		// As BigInt(literal).toString(): every digit, and -0 as 0.
		n, ok := new(big.Int).SetString(string(raw), 10)
		if !ok {
			return Value{}, false
		}
		digits := n.String()
		return Value{Integer: &digits}, true
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || !json.Valid(raw) {
		return Value{}, false
	}
	return Value{Number: &f}, true
}

// safeOffset reads a JSON number that is a whole, non-negative, safe
// integer (Number.isSafeInteger and ≥ 0), as the site checks a span.
func safeOffset(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || f < 0 || f > 1<<53-1 || f != math.Trunc(f) {
		return 0, false
	}
	return int64(f), true
}

// Values are the choices quotes offer, each value once, as the site's
// choicesOf lists them: in the order each first appears, holding its last
// quote's value (a Map's insertion).
//
//	Values(Quotes(`[{"value": "12 months", …}, {"value": 12, …}, {"value": "12 months", …}]`)) // [{Text: "12 months"}, {Integer: "12"}]
func Values(quotes []Quote) []Value {
	var out []Value
	at := map[string]int{}
	for _, q := range quotes {
		k := q.Value.key()
		if i, ok := at[k]; ok {
			out[i] = q.Value
			continue
		}
		at[k] = len(out)
		out = append(out, q.Value)
	}
	return out
}

// Names are the decisions of a card's name (type and normalized name)
// across the ontology's documents, as the index sends them with a page:
// how many, how many choices, and the newest.
type Names struct {
	Total   int            `json:"total"`
	Choices int            `json:"choices"`
	Newest  []NameDecision `json:"newest"`
}

// NameDecision is one resolve decision of a name: its document, and the
// entity it chose (nil: not in the data).
type NameDecision struct {
	DocID     string  `json:"docId"`
	EntityKey *string `json:"entityKey,omitempty"`
}

// Input is what the rules read of a card: its kind and lane, its settling
// review as the load holds it now, and its name's decisions (nil: none).
type Input struct {
	Kind   reviewcards.Kind
	Lane   reviewcards.Lane
	Review *reviewcards.Review
	Names  *Names
}

// Why is the rules' abstention on in, which the model advises on, or "" when
// a rule suggests a choice, the card is for information, or its review
// settles nothing the walk shows (stale). It ports the site's adviseName
// and adviseValue only as far as they end in WhyTiedCandidates,
// WhyValuesSplit or WhyFewQuotes; the site decides what shows, so a
// difference only costs a call.
//
//	Why(Input{Kind: reviewcards.KindName, Lane: reviewcards.LaneDecide, Review: twoCandidates}) // "tied_candidates"
func Why(in Input) string {
	if in.Lane != reviewcards.LaneDecide || in.Review == nil {
		return ""
	}
	r := in.Review
	switch in.Kind {
	case reviewcards.KindName:
		if !settlesName(r) {
			return ""
		}
		var earlier []NameDecision
		if in.Names != nil {
			for _, d := range in.Names.Newest {
				if d.DocID != r.DocID {
					earlier = append(earlier, d)
				}
			}
		}
		// One choice across the name's decisions in other documents is a
		// precedent: Not in data, or an entity the read found.
		if in.Names != nil && in.Names.Choices == 1 && len(earlier) > 0 &&
			(earlier[0].EntityKey == nil || found(r, *earlier[0].EntityKey)) {
			return ""
		}
		if len(r.Candidates) > 1 {
			return WhyTiedCandidates
		}
	case reviewcards.KindValue:
		quotes, ok := settlesValue(r)
		if !ok {
			return ""
		}
		if len(quotes) < MinQuotes {
			return WhyFewQuotes
		}
		counts := map[string]int{}
		top := 0
		for _, q := range quotes {
			counts[q.Value.key()]++
			top = max(top, counts[q.Value.key()])
		}
		// At least two thirds is a majority.
		if top*3 < len(quotes)*2 {
			return WhyValuesSplit
		}
	}
	return ""
}

// Settles reports whether in's review settles by the decision its card
// takes, as the walk shows the card: one that doesn't (gone from the load,
// stale, or a held claim's) is never advised.
func Settles(in Input) bool {
	if in.Review == nil {
		return false
	}
	switch in.Kind {
	case reviewcards.KindName:
		return settlesName(in.Review)
	case reviewcards.KindValue:
		_, ok := settlesValue(in.Review)
		return ok
	}
	return false
}

// Abstains reports whether the rules leave in's card to the model.
func Abstains(in Input) bool { return Why(in) != "" }

// resolveReasons are the reasons a resolve decision settles, as the site's
// RESOLVE_REASONS.
var resolveReasons = map[string]bool{"unresolved": true, "ambiguous": true, "collision": true, "identifier_conflict": true,
	"adjudicated": true, "legal_form": true, "part_name": true, "own_name": true}

// settlesName reports whether r settles by a resolve the walk offers: its
// reason is one, and it names its type, document and name.
func settlesName(r *reviewcards.Review) bool {
	return resolveReasons[r.Reason] && r.Key != "" && r.Type != "" && r.DocID != "" && r.NormName != ""
}

// settlesValue is r's quotes when it settles by a value the walk offers: a
// prop_conflict naming its type, entity and property, of an entity the read
// found, with a quote that reads.
func settlesValue(r *reviewcards.Review) ([]Quote, bool) {
	if r.Reason != "prop_conflict" || r.Key == "" || r.Type == "" || r.EntityKey == "" || r.Prop == "" || !found(r, r.EntityKey) {
		return nil, false
	}
	quotes := Quotes(r.Values)
	return quotes, len(quotes) > 0
}

// found reports whether the read found r's entity of key, of its type.
func found(r *reviewcards.Review, key string) bool {
	return entity(r, key) != nil
}

func entity(r *reviewcards.Review, key string) *reviewcards.Entity {
	for i, e := range r.Concerned {
		if e.Type == r.Type && e.Key == key {
			return &r.Concerned[i]
		}
	}
	return nil
}
