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
// documents. The engine keeps no index of reviews by name, so the read
// filters the load's FoundryReview nodes of that type and name; adding one
// is an engine store change.

// AskingDocumentsMax is the most documents one name's read keeps. Past it
// the rest wait for the next load, which carries the alias to every
// document that writes its name.
const AskingDocumentsMax = 10000

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
// reason and entity_key. It reads at most 4 × AskingDocumentsMax reviews,
// as one document may hold several reviews of one name.
func AskingQuery(load, ty, name string) ([]byte, error) {
	predicates := []any{}
	for _, p := range [][2]string{{"foundry_load", load}, {"$label", "FoundryReview"}, {"type", ty}, {"norm_name", name}} {
		predicates = append(predicates, map[string]any{"eq": map[string]any{"left": map[string]string{"property": p[0]},
			"right": map[string]any{"constant": map[string]string{"string": p[1]}}}})
	}
	root := map[string]any{"value_map": map[string]any{"input": map[string]any{"limit": map[string]any{
		"input": map[string]any{"nodes_where": map[string]any{"predicate": map[string]any{"and": map[string]any{"predicates": predicates}}}},
		"count": map[string]int{"literal": 4 * AskingDocumentsMax}}}, "properties": []string{"doc_id", "reason", "entity_key"}}}
	return json.Marshal(map[string]any{"request_type": "read", "query": map[string]any{"read": map[string]any{
		"entries": []any{map[string]any{"query": map[string]any{"name": "items", "root": root}}}, "returns": []string{"items"}}}})
}

// AskingDocuments reads AskingQuery's answer: each document once, in the
// order read, of a review that still asks about the name (AsksAboutName)
// with no entity linked, at most AskingDocumentsMax. past says the read or
// the documents ran past what is kept, so some documents that ask were left
// out.
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
	past = len(response.Items) >= 4*AskingDocumentsMax || len(docs) > AskingDocumentsMax
	return docs[:min(len(docs), AskingDocumentsMax)], past, nil
}
