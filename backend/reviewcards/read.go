package reviewcards

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// reviewFields are what a review is read with: as the site's document view
// reads it (explore/model.ts REVIEW_FIELDS), less what no card keeps.
var reviewFields = []string{"$id", "review_key", "reason", "type", "doc_id", "norm_name", "candidates", "entity_key", "prop", "values",
	"suggested_link", "statement", "claim_key", "claim", "section", "start", "end", "confidence"}

// Read is what one read of a load's documents (ReadQuery) found of each.
type Read struct {
	// Present are the documents the load's graph holds; one it doesn't was
	// taken out of it, and has no cards.
	Present map[string]bool
	// Reviews are each document's reviews, by its id.
	Reviews map[string][]*Review
	// Quotes are how many quotes of facts each document holds in each band.
	Quotes map[string]*[6]uint32
}

// ReadQuery is one read of load loadID's documents docIDs, by the document
// view's path: each document, its sections, and what is quoted from them,
// its reviews (with the entities they concern, every property each) and
// its quotes of facts (with their scores, for the spot checks' bands).
func ReadQuery(loadID string, docIDs []string) ([]byte, error) {
	if len(docIDs) == 0 {
		return nil, fmt.Errorf("reviewcards: a read of no documents")
	}
	either := make([]any, len(docIDs))
	for i, d := range docIDs {
		either[i] = eq("doc_id", d)
	}
	docs := map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": []any{
		eq("$label", "FoundryDocument"), eq("foundry_load", loadID), map[string]any{"or": map[string]any{"predicates": either}},
	}}}}}
	quoted := map[string]any{"in": map[string]any{"input": ref("secs"), "label": "FOUNDRY_FROM_SECTION"}}
	evidence := map[string]any{"where": map[string]any{"input": ref("quoted"), "predicate": map[string]any{"and": map[string]any{"predicates": []any{
		eq("$label", "FoundryEvidence"), map[string]any{"not": map[string]any{"predicate": map[string]any{"is_null": map[string]any{"property": "fact_key"}}}},
	}}}}}
	concerns := func(step string) map[string]any {
		return map[string]any{step: map[string]any{"input": ref("review_nodes"), "label": "FOUNDRY_CONCERNS"}}
	}
	entries := []any{
		entry("docs", docs),
		entry("documents", map[string]any{"values": map[string]any{"input": ref("docs"), "properties": []string{"doc_id"}}}),
		entry("secs", map[string]any{"in": map[string]any{"input": ref("docs"), "label": "FOUNDRY_IN_DOCUMENT"}}),
		entry("quoted", quoted),
		entry("evidence", map[string]any{"values": map[string]any{"input": evidence, "properties": []string{"$id", "doc_id", "confidence"}}}),
		entry("review_nodes", map[string]any{"where": map[string]any{"input": ref("quoted"), "predicate": eq("$label", "FoundryReview")}}),
		entry("reviews", map[string]any{"values": map[string]any{"input": ref("review_nodes"), "properties": reviewFields}}),
		entry("review_concerns", map[string]any{"edge_properties": map[string]any{"input": concerns("out_e")}}),
		entry("concerned", map[string]any{"value_map": map[string]any{"input": map[string]any{"dedup": map[string]any{"input": concerns("out")}}}}),
	}
	return json.Marshal(map[string]any{"request_type": "read", "query_name": "foundry_review_cards", "query": map[string]any{"read": map[string]any{
		"entries": entries, "returns": []string{"documents", "evidence", "reviews", "review_concerns", "concerned"}}}})
}

// ParseRead reads ReadQuery's answer, nothing in which is trusted: each of
// docIDs the graph holds, with its reviews and quote counts. A review or a
// quote of a document not asked about is left out.
func ParseRead(body []byte, docIDs []string) (Read, error) {
	var response struct {
		Documents []struct {
			DocID string `json:"doc_id"`
		} `json:"documents"`
		Evidence []struct {
			ID         int64    `json:"$id"`
			DocID      string   `json:"doc_id"`
			Confidence *float64 `json:"confidence"`
		} `json:"evidence"`
		Reviews   []map[string]json.RawMessage `json:"reviews"`
		Concerns  []edge                       `json:"review_concerns"`
		Concerned []map[string]json.RawMessage `json:"concerned"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Read{}, fmt.Errorf("reviewcards: read the documents' reviews: %w", err)
	}
	asked := map[string]bool{}
	for _, d := range docIDs {
		asked[d] = true
	}
	read := Read{Present: map[string]bool{}, Reviews: map[string][]*Review{}, Quotes: map[string]*[6]uint32{}}
	for _, d := range response.Documents {
		if asked[d.DocID] {
			read.Present[d.DocID] = true
			read.Quotes[d.DocID] = &[6]uint32{}
		}
	}
	seen := map[int64]bool{}
	for _, e := range response.Evidence {
		if q := read.Quotes[e.DocID]; q != nil && !seen[e.ID] {
			seen[e.ID] = true
			q[BandOf(score(e.Confidence))]++
		}
	}
	for _, r := range parseReviews(response.Reviews, response.Concerns, response.Concerned) {
		if read.Present[r.DocID] {
			read.Reviews[r.DocID] = append(read.Reviews[r.DocID], r)
		}
	}
	return read, nil
}

// edge is an edge of a read: its ends' ids.
type edge struct {
	From int64 `json:"$from"`
	To   int64 `json:"$to"`
}

// parseReviews reads a read's reviews, each once, with the entities each
// concerns (an ontology's, by the identity a review names it by), nothing
// of which is trusted.
func parseReviews(reviews []map[string]json.RawMessage, concerns []edge, concernedRows []map[string]json.RawMessage) []*Review {
	entities := map[int64]Entity{}
	for _, e := range concernedRows {
		var id int64
		label, key, norm := text(e["$label"]), text(e["entity_key"]), text(e["norm_name"])
		if json.Unmarshal(e["$id"], &id) != nil || label == "" || strings.HasPrefix(label, "Foundry") || (key != "") == (norm != "") {
			continue
		}
		props := map[string]json.RawMessage{}
		for k, v := range e {
			if !strings.HasPrefix(k, "$") {
				props[k] = v
			}
		}
		entities[id] = Entity{Type: label, Key: key + norm, Props: props}
	}
	concerned := map[int64][]Entity{}
	for _, c := range concerns {
		if e, ok := entities[c.To]; ok {
			concerned[c.From] = append(concerned[c.From], e)
		}
	}
	var out []*Review
	seen := map[int64]bool{}
	for _, item := range reviews {
		var id int64
		if json.Unmarshal(item["$id"], &id) != nil || seen[id] {
			continue
		}
		seen[id] = true
		r := &Review{
			Key: text(item["review_key"]), Reason: text(item["reason"]), Type: text(item["type"]), DocID: text(item["doc_id"]),
			NormName: text(item["norm_name"]), EntityKey: text(item["entity_key"]), Prop: text(item["prop"]), Values: text(item["values"]),
			SuggestedLink: text(item["suggested_link"]), Statement: text(item["statement"]), ClaimKey: text(item["claim_key"]),
			Claim: text(item["claim"]), Section: offset(item["section"]), Start: offset(item["start"]), End: offset(item["end"]),
			Confidence: confidence(item["confidence"]), Quote: text(item["quote"]), Concerned: concerned[id],
		}
		var candidates []any
		_ = json.Unmarshal(item["candidates"], &candidates) // best-effort: candidates that don't read are none
		for _, c := range candidates {
			if key, ok := c.(string); ok && key != "" && !slices.Contains(r.Candidates, key) {
				r.Candidates = append(r.Candidates, key)
			}
		}
		out = append(out, r)
	}
	return out
}

// MaxReviewsByKey is how many review keys one ReviewsByKeyQuery names, as
// the site's reviewsByKeyRequest reads them.
const MaxReviewsByKey = 200

// ReviewsByKeyQuery reads load loadID's reviews of keys (at most
// MaxReviewsByKey) with the entities they concern, every property each, as
// the site's reviewsByKeyRequest does, and the quote each is of.
func ReviewsByKeyQuery(loadID string, keys []string) ([]byte, error) {
	if len(keys) == 0 || len(keys) > MaxReviewsByKey {
		return nil, fmt.Errorf("reviewcards: a read of %d review keys", len(keys))
	}
	nodes := map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": []any{
		eq("$label", "FoundryReview"), eq("foundry_load", loadID),
		map[string]any{"is_in": map[string]any{"value": map[string]string{"property": "review_key"}, "values": map[string]string{"param": "keys"}}},
	}}}}}
	concerns := func(step string) map[string]any {
		return map[string]any{step: map[string]any{"input": ref("review_nodes"), "label": "FOUNDRY_CONCERNS"}}
	}
	entries := []any{
		entry("review_nodes", nodes),
		entry("reviews", map[string]any{"values": map[string]any{"input": ref("review_nodes"), "properties": append(slices.Clone(reviewFields), "quote")}}),
		entry("review_concerns", map[string]any{"edge_properties": map[string]any{"input": concerns("out_e")}}),
		entry("concerned", map[string]any{"value_map": map[string]any{"input": map[string]any{"dedup": map[string]any{"input": concerns("out")}}}}),
	}
	return json.Marshal(map[string]any{"request_type": "read", "query_name": "foundry_reviews_by_key", "query": map[string]any{"read": map[string]any{
		"entries": entries, "returns": []string{"reviews", "review_concerns", "concerned"}}},
		"parameters": map[string]any{"keys": keys}, "parameter_types": map[string]any{"keys": map[string]string{"array": "string"}}})
}

// ParseReviewsByKey reads ReviewsByKeyQuery's answer, nothing in which is
// trusted: each review of keys, by its key (the first read of a key).
func ParseReviewsByKey(body []byte, keys []string) (map[string]*Review, error) {
	var response struct {
		Reviews   []map[string]json.RawMessage `json:"reviews"`
		Concerns  []edge                       `json:"review_concerns"`
		Concerned []map[string]json.RawMessage `json:"concerned"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("reviewcards: read reviews by key: %w", err)
	}
	out := map[string]*Review{}
	for _, r := range parseReviews(response.Reviews, response.Concerns, response.Concerned) {
		if _, ok := out[r.Key]; !ok && slices.Contains(keys, r.Key) {
			out[r.Key] = r
		}
	}
	return out, nil
}

// Quote is a quote of a fact in a document, at its place, as a spot check
// draws one.
type Quote struct {
	Section    uint32
	Start      uint32
	End        uint32
	Link       string
	FactKey    string
	Confidence *float64
}

// QuotesQuery reads the quotes of facts of one document of a load, each
// with its section's ordinal.
func QuotesQuery(loadID, docID string) ([]byte, error) {
	doc := map[string]any{"limit": map[string]any{"count": map[string]int{"literal": 1}, "input": map[string]any{"nodes_where": map[string]any{
		"predicate": map[string]any{"and": map[string]any{"predicates": []any{
			eq("$label", "FoundryDocument"), eq("foundry_load", loadID), eq("doc_id", docID),
		}}}}}}}
	entries := []any{
		entry("doc", doc),
		entry("secs", map[string]any{"in": map[string]any{"input": ref("doc"), "label": "FOUNDRY_IN_DOCUMENT"}}),
		entry("sections", map[string]any{"values": map[string]any{"input": ref("secs"), "properties": []string{"$id", "ordinal"}}}),
		entry("quoted_in", map[string]any{"edge_properties": map[string]any{"input": map[string]any{
			"in_e": map[string]any{"input": ref("secs"), "label": "FOUNDRY_FROM_SECTION"}}}}),
		entry("quotes", map[string]any{"values": map[string]any{"input": map[string]any{"where": map[string]any{
			"input": map[string]any{"in": map[string]any{"input": ref("secs"), "label": "FOUNDRY_FROM_SECTION"}},
			"predicate": map[string]any{"and": map[string]any{"predicates": []any{
				eq("$label", "FoundryEvidence"), map[string]any{"not": map[string]any{"predicate": map[string]any{"is_null": map[string]any{"property": "fact_key"}}}},
			}}}}}, "properties": []string{"$id", "fact_key", "link", "start", "end", "confidence"}}}),
	}
	return json.Marshal(map[string]any{"request_type": "read", "query_name": "foundry_review_quotes", "query": map[string]any{"read": map[string]any{
		"entries": entries, "returns": []string{"sections", "quoted_in", "quotes"}}}})
}

// ParseQuotes reads QuotesQuery's answer: each quote of a fact at its place,
// nothing of which is trusted; one with no place, or none that a column
// holds, is left out.
func ParseQuotes(body []byte) ([]Quote, error) {
	var response struct {
		Sections []struct {
			ID      int64  `json:"$id"`
			Ordinal *int64 `json:"ordinal"`
		} `json:"sections"`
		In []struct {
			From int64 `json:"$from"`
			To   int64 `json:"$to"`
		} `json:"quoted_in"`
		Quotes []map[string]json.RawMessage `json:"quotes"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("reviewcards: read a document's quotes: %w", err)
	}
	ordinal := map[int64]int64{}
	for _, s := range response.Sections {
		if s.Ordinal != nil && *s.Ordinal >= 0 {
			ordinal[s.ID] = *s.Ordinal
		}
	}
	in := map[int64]int64{}
	for _, e := range response.In {
		in[e.From] = e.To
	}
	var quotes []Quote
	seen := map[int64]bool{}
	for _, q := range response.Quotes {
		var id int64
		if json.Unmarshal(q["$id"], &id) != nil || seen[id] {
			continue
		}
		seen[id] = true
		section, ok := ordinal[in[id]]
		start, end, link, fact := offset(q["start"]), offset(q["end"]), text(q["link"]), text(q["fact_key"])
		if !ok || start == nil || end == nil || *start >= *end || section > math.MaxUint32 || *end > math.MaxUint32 ||
			link == "" || !fits(link, maxLinkChars) || fact == "" || len(fact) > 128 || !ascii(fact) {
			continue
		}
		quotes = append(quotes, Quote{Section: uint32(section), Start: uint32(*start), End: uint32(*end), Link: link, FactKey: fact,
			Confidence: confidence(q["confidence"])})
	}
	return quotes, nil
}

func entry(name string, root any) map[string]any {
	return map[string]any{"query": map[string]any{"name": name, "root": root}}
}

func ref(name string) map[string]any {
	return map[string]any{"nodes": map[string]any{"reference": map[string]string{"var": name}}}
}

func eq(property, value string) map[string]any {
	return map[string]any{"eq": map[string]any{"left": map[string]string{"property": property},
		"right": map[string]any{"constant": map[string]string{"string": value}}}}
}

func text(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s) // best-effort: anything but a string is none
	return s
}

func offset(raw json.RawMessage) *int64 {
	var n *int64
	if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 {
		return nil
	}
	return n
}

func confidence(raw json.RawMessage) *float64 {
	var c *float64
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return score(c)
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
