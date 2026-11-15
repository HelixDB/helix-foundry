package ontologyagent

import (
	"encoding/json"
	"fmt"
)

// The documents of a load whose reviews ask about a name: those an alias of
// that name (a resolve that chose an entity, contract 4.22.0) settles in
// place when they are sent again. The API reads them as a person settles a
// resolve against the current load (SettleFoundryReview), and the graph
// worker as a resend job sends a resolve carried to its load from another
// time; both send the same read of the load's reviews and keep the same
// documents, every one of them. From contract 4.24.0 the engine indexes
// reviews by the name they ask about (its IndexLevel::NAMES), so the read is
// one lookup of the name intersected with the load: 1-3 ms for a name of a
// few reviews, about 0.1 s for one of 4,500, where it was a pass over the
// load's reviews (1.2 s at 100,000, 27-51 s at 1M). A graph whose index is
// still building (the first Extract after the engine rolls out builds it)
// reads the same reviews by that pass.

// AskingReviewsMax is the most reviews one name's read returns, a bound on
// the answer's size rather than on what the read costs: about 75 bytes a
// review, so 23 MB at most, under graphproxy.MaxResponseBytes (32 MiB). A name with
// more reviews than that in one load keeps the documents of those read, and
// the rest wait for the next load, which carries the alias to every document
// that writes its name.
const AskingReviewsMax = 300000

// AsksAboutName is a review reason that asks about a name the load links to
// nothing, or to nothing a person could stand by: one the engine settles in
// place when the name's decision is sent with the document (contract
// 4.22.0). A name already linked another way (adjudicated, legal_form,
// part_name, own_name) is the engine's to call bound.
func AsksAboutName(reason string) bool {
	switch reason {
	case "unresolved", "ambiguous", "collision", "identifier_conflict":
		return true
	}
	return false
}

// AskingQuery is the read request (request_type "read", one entry named
// items) of load's reviews of type ty and name, with each review's doc_id,
// reason and entity_key, at most AskingReviewsMax of them.
func AskingQuery(load, ty, name string) ([]byte, error) {
	predicates := []any{}
	for _, p := range [][2]string{{"foundry_load", load}, {"$label", "FoundryReview"}, {"type", ty}, {"norm_name", name}} {
		predicates = append(predicates, map[string]any{"eq": map[string]any{"left": map[string]string{"property": p[0]},
			"right": map[string]any{"constant": map[string]string{"string": p[1]}}}})
	}
	root := map[string]any{"value_map": map[string]any{"input": map[string]any{"limit": map[string]any{
		"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": predicates}}}},
		"count": map[string]int{"literal": AskingReviewsMax}}}, "properties": []string{"doc_id", "reason", "entity_key"}}}
	return json.Marshal(map[string]any{"request_type": "read", "query": map[string]any{"read": map[string]any{
		"entries": []any{map[string]any{"query": map[string]any{"name": "items", "root": root}}}, "returns": []string{"items"}}}})
}

// AskingDocuments reads AskingQuery's answer: each document once, in the
// order read, of a review that still asks about the name (AsksAboutName)
// with no entity linked. past says the read filled AskingReviewsMax, so
// some documents that ask may have been left out.
func AskingDocuments(body []byte) (docs []string, past bool, err error) {
	var response struct {
		Items []struct {
			DocID     string          `json:"doc_id"`
			Reason    string          `json:"reason"`
			EntityKey json.RawMessage `json:"entity_key"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, false, fmt.Errorf("read the reviews of a name: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range response.Items {
		linked := len(r.EntityKey) > 0 && string(r.EntityKey) != "null"
		if !linked && r.DocID != "" && AsksAboutName(r.Reason) && !seen[r.DocID] {
			seen[r.DocID] = true
			docs = append(docs, r.DocID)
		}
	}
	return docs, len(response.Items) >= AskingReviewsMax, nil
}
