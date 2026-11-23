package reviewcards

import (
	"bytes"
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
)

// Review is a FoundryReview as the graph holds it, nothing of which is
// trusted, with the entities it concerns (FOUNDRY_CONCERNS).
type Review struct {
	Key           string   `json:"review_key"`
	Reason        string   `json:"reason"`
	Type          string   `json:"type"`
	DocID         string   `json:"doc_id"`
	NormName      string   `json:"norm_name"`
	Candidates    []string `json:"candidates"`
	EntityKey     string   `json:"entity_key"`
	Prop          string   `json:"prop"`
	Values        string   `json:"values"`
	SuggestedLink string   `json:"suggested_link"`
	Statement     string   `json:"statement"`
	ClaimKey      string   `json:"claim_key"`
	Claim         string   `json:"claim"`
	Section       *int64   `json:"section"`
	Start         *int64   `json:"start"`
	End           *int64   `json:"end"`
	Confidence    *float64 `json:"confidence"`
	Concerned     []Entity `json:"concerned,omitempty"`
}

// Entity is an entity a review concerns: its type, its stored identity (an
// entity_key, or a named type's norm_name) and its properties.
type Entity struct {
	Type  string                     `json:"type"`
	Key   string                     `json:"key"`
	Props map[string]json.RawMessage `json:"props,omitempty"`
}

// ClaimDecision is a saved release or reject of a held claim of a document,
// as foundry_review_decisions keeps it: which held claims it decides.
type ClaimDecision struct {
	Kind          string `json:"kind"`
	Section       uint32 `json:"section"`
	Start         uint32 `json:"start"`
	End           uint32 `json:"end"`
	ClaimKey      string `json:"claim_key"`
	StatementHash string `json:"statement_hash"`
}

// decides reports whether d decides the held claim c: a rejection of it at
// its place, under its key or of what it reads (a rebuild moves keys), or an
// approval of what it reads now. An approval of what it said before does
// not: the load refuses it, and the card asks to be read again.
func (d ClaimDecision) decides(c *Card) bool {
	if d.Section != *c.ClaimSection || d.Start != *c.ClaimStart || d.End != *c.ClaimEnd {
		return false
	}
	switch d.Kind {
	case "reject":
		return d.ClaimKey == *c.ClaimKey || d.StatementHash == *c.StatementHash
	case "release":
		return d.StatementHash == *c.StatementHash
	}
	return false
}

// Card is one row of foundry_review_cards.
type Card struct {
	ID     ID      `json:"-"`
	Kind   Kind    `json:"kind"`
	Lane   Lane    `json:"lane"`
	OnCard *ID     `json:"-"`
	DocID  string  `json:"doc_id"`
	Type   *string `json:"type,omitempty"`
	// SubjectType and ObjectType are a held claim's ends' types, which the
	// walk's type filter matches too.
	SubjectType   *string  `json:"subject_type,omitempty"`
	ObjectType    *string  `json:"object_type,omitempty"`
	NormName      *string  `json:"norm_name,omitempty"`
	EntityKey     *string  `json:"entity_key,omitempty"`
	Prop          *string  `json:"prop,omitempty"`
	ClaimKey      *string  `json:"claim_key,omitempty"`
	ClaimSection  *uint32  `json:"claim_section,omitempty"`
	ClaimStart    *uint32  `json:"claim_start,omitempty"`
	ClaimEnd      *uint32  `json:"claim_end,omitempty"`
	StatementHash *string  `json:"statement_hash,omitempty"`
	Link          *string  `json:"link,omitempty"`
	ReviewKey     string   `json:"review_key"`
	ReviewKeys    []string `json:"review_keys"`
	ReviewCount   int      `json:"review_count"`
	Confidence    *float64 `json:"confidence,omitempty"`
	// Band is a held claim's score band (Bands); nil for any other card.
	Band        *string `json:"band,omitempty"`
	Blocked     int     `json:"blocked"`
	Facts       int     `json:"facts"`
	PerDecision float64 `json:"per_decision"`
	Shuffle     uint32  `json:"shuffle"`
}

// Built is what Build makes of one document.
type Built struct {
	Cards []*Card
	// Invalid counts reviews dropped for a field no column holds.
	Invalid int
}

// The reasons each decision settles; a prop_conflict takes a value, and any
// other reason no decision settles (a link the ontology lacks).
var (
	resolveReasons = map[string]bool{"unresolved": true, "ambiguous": true, "collision": true, "identifier_conflict": true,
		"adjudicated": true, "legal_form": true, "part_name": true, "own_name": true}
	releaseReasons = map[string]bool{"low_confidence": true, "unverified": true, "conflict": true}
	// blocking are the reasons whose review of a name blocks an accepted
	// claim with its link: the load still asks about the name.
	blocking  = map[string]bool{"unresolved": true, "ambiguous": true, "collision": true, "identifier_conflict": true}
	weakLinks = map[string]bool{"legal_form": true, "part_name": true, "own_name": true}
)

// Column limits: a review whose field passes one is dropped as invalid.
const (
	maxTypeChars = 120
	maxNameChars = 512
	maxLinkChars = 255
	maxKeyChars  = 256
)

// statement is what approving a held claim states, as its card shows it.
type statement struct {
	Subject struct{ Type, Name string } `json:"subject"`
	Object  struct{ Type, Name string } `json:"object"`
}

// held is a held claim being built: its card, and what waitsOn reads.
type held struct {
	card     *Card
	review   *Review
	stated   statement
	named    bool
	conflict bool
	unsettle []unsettled
}

// unsettled is an end of a held claim whose name the load linked on less
// than a name in its document.
type unsettled struct {
	norm string
	by   string
}

// name is a name's reviews being built into a card.
type name struct {
	ty, norm         string
	reviews          []*Review
	weak, isBlocking bool
}

// Build makes the cards of one document's reviews of load loadID, given the
// saved decisions of its held claims (which a name's rank leaves out) and
// which links hold one value. Only reviews of docID count. A review that
// settles nothing, or that misses what the backend reads of it (stale), is
// no card; one with a field no column holds is no card either, and counted.
//
// Example: a document with one unresolved name with a candidate and one held
// claim of it gives a name card (per decision 1, as it blocks one claim and
// nothing waits on it) and a claim card.
func Build(loadID, docID string, reviews []*Review, decisions []ClaimDecision, single func(link string) bool) Built {
	var b Built
	// Reviews by key, so a duplicate card keeps the first.
	sorted := slices.Clone(reviews)
	slices.SortStableFunc(sorted, func(x, y *Review) int { return cmp.Compare(x.Key, y.Key) })

	// What is stated at each claim's place: two statements there differ.
	stated := map[string]map[string]bool{}
	for _, r := range sorted {
		if r.DocID != docID || r.ClaimKey == "" || !offsets(r) {
			continue
		}
		place := placeKey(r)
		if stated[place] == nil {
			stated[place] = map[string]bool{}
		}
		stated[place][r.Statement] = true
	}
	// Names linked on less than a name, by document type and name.
	linked := map[[2]string]string{}
	names := map[[2]string]*name{}
	var nameOrder [][2]string
	var helds []*held
	var values []*Card
	for _, r := range sorted {
		if r.DocID != docID {
			continue
		}
		switch {
		case resolveReasons[r.Reason]:
			if r.Key == "" || r.Type == "" || r.NormName == "" {
				continue // stale
			}
			if !fits(r.Type, maxTypeChars) || !fits(r.NormName, maxNameChars) || !fits(r.Key, maxKeyChars) {
				b.Invalid++
				continue
			}
			key := [2]string{r.Type, r.NormName}
			n := names[key]
			if n == nil {
				n = &name{ty: r.Type, norm: r.NormName}
				names[key] = n
				nameOrder = append(nameOrder, key)
			}
			n.reviews = append(n.reviews, r)
			n.weak = n.weak || r.Reason != "adjudicated" && !blocking[r.Reason]
			n.isBlocking = n.isBlocking || blocking[r.Reason]
			if weakLinks[r.Reason] {
				linked[key] = r.Reason
			}
		case releaseReasons[r.Reason]:
			// A conflict a load wrote before claims kept statements was never
			// one a person settles.
			if r.Reason == "conflict" && r.Statement == "" {
				continue
			}
			var s statement
			if r.SuggestedLink == "" || r.Statement == "" || json.Unmarshal([]byte(r.Statement), &s) != nil ||
				s.Subject.Type == "" || s.Subject.Name == "" || s.Object.Type == "" || s.Object.Name == "" ||
				r.Key == "" || r.ClaimKey == "" || !offsets(r) {
				continue // stale
			}
			if !fits(r.Key, maxKeyChars) || !fits(r.SuggestedLink, maxLinkChars) || !hex64(r.ClaimKey) ||
				!fits(s.Subject.Type, maxTypeChars) || !fits(s.Object.Type, maxTypeChars) ||
				*r.Section > math.MaxUint32 || *r.End > math.MaxUint32 || *r.Start >= *r.End {
				b.Invalid++
				continue
			}
			section, start, end := uint32(*r.Section), uint32(*r.Start), uint32(*r.End)
			hash := StatementHash(r.SuggestedLink, r.Statement)
			band := Bands[BandOf(r.Confidence)]
			c := &Card{
				ID: ClaimID(docID, section, start, end, r.ClaimKey), Kind: KindClaim, Lane: LaneDecide, DocID: docID,
				SubjectType: &s.Subject.Type, ObjectType: &s.Object.Type, ClaimKey: &r.ClaimKey, ClaimSection: &section,
				ClaimStart: &start, ClaimEnd: &end, StatementHash: &hash, Link: &r.SuggestedLink, ReviewKey: r.Key,
				ReviewKeys: []string{r.Key}, ReviewCount: 1, Confidence: score(r.Confidence), Band: &band, Facts: 1, PerDecision: 1,
			}
			helds = append(helds, &held{card: c, review: r, stated: s, named: ends(r.Claim) >= 2, conflict: r.Reason == "conflict"})
		case r.Reason == "prop_conflict":
			entity := concerned(r, r.Type, r.EntityKey)
			if r.Key == "" || r.Type == "" || r.EntityKey == "" || r.Prop == "" || entity == nil || !hasValues(r.Values) {
				continue // stale
			}
			if !fits(r.Key, maxKeyChars) || !fits(r.Type, maxTypeChars) || !fits(r.EntityKey, maxNameChars) || !fits(r.Prop, maxTypeChars) {
				b.Invalid++
				continue
			}
			// Only a table row's value survives a conflict: the table's value
			// stands, and the card is for information.
			lane := LaneDecide
			if scalar(entity.Props[r.Prop]) {
				lane = LaneFYI
			}
			values = append(values, &Card{
				ID: ValueID(docID, r.Type, r.EntityKey, r.Prop), Kind: KindValue, Lane: lane, DocID: docID, Type: &r.Type,
				EntityKey: &r.EntityKey, Prop: &r.Prop, ReviewKey: r.Key, ReviewKeys: []string{r.Key}, ReviewCount: 1, PerDecision: 1,
			})
		}
	}

	// Each held claim's lane and the name it waits on.
	nameIDs := map[[2]string]ID{}
	for _, key := range nameOrder {
		nameIDs[key] = NameID(key[0], docID, key[1])
	}
	waiting := map[ID]int{}
	for _, h := range helds {
		s := h.stated
		for _, end := range []struct{ Type, Name string }{s.Subject, s.Object} {
			norm := namewords.NormName(end.Name)
			if by, ok := linked[[2]string{end.Type, norm}]; ok && !slices.ContainsFunc(h.unsettle, func(u unsettled) bool { return u.norm == norm }) {
				h.unsettle = append(h.unsettle, unsettled{norm: norm, by: by})
			}
		}
		differs := len(stated[placeKey(h.review)]) > 1
		if differs || h.conflict && !single(*h.card.Link) {
			h.card.Lane = LanePool
			continue
		}
		if on, ok := waitsOn(h, names, single(*h.card.Link)); ok {
			id := nameIDs[on]
			h.card.OnCard = &id
			if !slices.ContainsFunc(decisions, func(d ClaimDecision) bool { return d.decides(h.card) }) {
				waiting[id]++
			}
		}
	}

	seen := map[ID]bool{}
	add := func(c *Card) {
		if seen[c.ID] {
			return
		}
		seen[c.ID] = true
		c.Shuffle = Shuffle(loadID, c.ID)
		b.Cards = append(b.Cards, c)
	}
	for _, key := range nameOrder {
		n := names[key]
		rs := slices.Clone(n.reviews)
		slices.SortStableFunc(rs, func(x, y *Review) int {
			return cmp.Or(cmp.Compare(deref(x.Section), deref(y.Section)), cmp.Compare(deref(x.Start), deref(y.Start)))
		})
		settling := rs[0]
		for _, pick := range []func(r *Review) bool{
			func(r *Review) bool { return len(r.Candidates) > 0 && blocking[r.Reason] },
			func(r *Review) bool { return len(r.Candidates) > 0 },
			func(r *Review) bool { return blocking[r.Reason] },
		} {
			if i := slices.IndexFunc(rs, pick); i >= 0 {
				settling = rs[i]
				break
			}
		}
		keys := []string{settling.Key}
		blocked, fyi := 0, true
		for _, r := range rs {
			if r != settling && len(keys) < MaxReviewKeys {
				keys = append(keys, r.Key)
			}
			if blocking[r.Reason] && r.SuggestedLink != "" {
				blocked++
			}
			fyi = fyi && r.Reason == "adjudicated"
		}
		lane := LaneDecide
		if fyi {
			lane = LaneFYI
		}
		id := nameIDs[key]
		h := waiting[id]
		ty, norm := n.ty, n.norm
		add(&Card{
			ID: id, Kind: KindName, Lane: lane, DocID: docID, Type: &ty, NormName: &norm, ReviewKey: settling.Key, ReviewKeys: keys,
			ReviewCount: len(rs), Blocked: blocked, Facts: blocked + h, PerDecision: float64(blocked+h) / float64(1+h),
		})
	}
	for _, h := range helds {
		add(h.card)
	}
	for _, c := range values {
		add(c)
	}
	return b
}

// waitsOn is the name a held claim waits on, if any: the first of its ends
// (subject, then object) whose name has a review in its document that holds
// it. One the load linked on less than a name holds it where the engine
// holds the claim for it (part of a name and an own-name identifier always,
// a legal form on a link that holds one value); one the load matched to no
// row, or several, holds a claim an end of which isn't in the graph. A name
// the model linked (adjudicated) holds nothing: it is in the graph.
func waitsOn(h *held, names map[[2]string]*name, single bool) ([2]string, bool) {
	holding := map[string]bool{}
	for _, u := range h.unsettle {
		if u.by != "legal_form" || single {
			holding[u.norm] = true
		}
	}
	for _, end := range []struct{ Type, Name string }{h.stated.Subject, h.stated.Object} {
		norm := namewords.NormName(end.Name)
		key := [2]string{end.Type, norm}
		n := names[key]
		if n == nil {
			continue
		}
		if n.weak && holding[norm] || !n.weak && n.isBlocking && !h.named {
			return key, true
		}
	}
	return [2]string{}, false
}

func offsets(r *Review) bool {
	return r.Section != nil && r.Start != nil && r.End != nil && *r.Section >= 0 && *r.Start >= 0 && *r.End >= 0
}

func placeKey(r *Review) string {
	b, _ := json.Marshal([]any{r.ClaimKey, *r.Section, *r.Start, *r.End})
	return string(b)
}

func fits(s string, chars int) bool { return utf8.ValidString(s) && utf8.RuneCountInString(s) <= chars }

func hex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range []byte(s) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// ends is how many entities a held claim's stored JSON names.
func ends(claim string) int {
	var c struct {
		About []json.RawMessage `json:"about"`
	}
	if json.Unmarshal([]byte(claim), &c) != nil {
		return 0
	}
	return len(c.About)
}

// concerned is the entity of type ty and stored identity key a review concerns.
func concerned(r *Review, ty, key string) *Entity {
	for i := range r.Concerned {
		if e := &r.Concerned[i]; e.Type == ty && e.Key == key {
			return e
		}
	}
	return nil
}

// hasValues reports whether a prop_conflict's values (the engine's
// PropQuote records, as JSON text) hold a value a person can choose: a
// string, a number or a boolean, from a document, at a span.
func hasValues(raw string) bool {
	var quotes []propQuote
	if json.Unmarshal([]byte(raw), &quotes) != nil {
		return false
	}
	return slices.ContainsFunc(quotes, func(q propQuote) bool {
		s := q.Span
		return scalar(q.Value) && q.DocID != "" && q.Quote != nil && s.Section != nil && s.Start != nil && s.End != nil &&
			*s.Section >= 0 && *s.Start >= 0 && *s.End > *s.Start
	})
}

// propQuote is one of the engine's PropQuote records: a value a document
// states, and where.
type propQuote struct {
	Value json.RawMessage `json:"value"`
	DocID string          `json:"doc_id"`
	Quote *string         `json:"quote"`
	Span  struct {
		Section *int64 `json:"section"`
		Start   *int64 `json:"start"`
		End     *int64 `json:"end"`
	} `json:"span"`
}

// scalar reports whether raw is a JSON string, finite number or boolean.
func scalar(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '"':
		var s string
		return json.Unmarshal(raw, &s) == nil
	case 't', 'f':
		var v bool
		return json.Unmarshal(raw, &v) == nil
	case 'n', '[', '{':
		return false
	}
	var n json.Number
	return json.Unmarshal(raw, &n) == nil
}

// score is a verifier's score kept only within [0, 1].
func score(c *float64) *float64 {
	if c == nil || math.IsNaN(*c) || *c < 0 || *c > 1 {
		return nil
	}
	v := *c
	return &v
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
