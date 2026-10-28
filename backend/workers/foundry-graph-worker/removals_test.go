package foundrygraphworker

import (
	"context"
	"slices"
	"testing"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func (s *memStore) ListRemovedFoundryDocuments(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.removed), nil
}

func (s *memStore) RecordFoundryDocumentRemoval(_ context.Context, _, _, documentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgotten = append(s.forgotten, documentID)
	s.removed = slices.DeleteFunc(s.removed, func(id string) bool { return id == documentID })
	return nil
}

// removing ends an Extract that took out doc-x, naming it, and doc-y, which
// the load didn't hold, without.
func removing(req string, stream agentv1.Agent_ExtractServer) error {
	extract, err := readExtract(req)
	if err != nil || len(extract.Remove) == 0 {
		return extractAll(req, stream)
	}
	return extractEvents(stream, []string{
		`{"event":"extract_started","contract_version":"4.2.0","extractor":"haiku","verifier":"sol","prompt_version":"7"}`,
		`{"event":"document_removed","doc_id":"doc-x","facts_removed":3,"entities_removed":1}`,
		`{"event":"extract_finished"}`,
	}, `{"status":"done","totals":`+totals+`}`)
}

func TestDocumentRemovals(t *testing.T) {
	// What a deleted document put in the graph leaves the load in place:
	// before any extraction, in an Extract of its own that spends nothing.
	t.Run("takes deleted documents out before extracting", func(t *testing.T) {
		h := newHarness(t)
		h.store.removed = []string{"doc-x", "doc-y"}
		h.agent.extract = removing
		outcome := h.run(t, extractJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		_, extracts := h.agent.calls()
		if len(extracts) != 2 {
			t.Fatalf("%d extracts, want the removal then the documents", len(extracts))
		}
		removal := decodeExtract(t, extracts[0])
		if !slices.Equal(removal.Remove, []string{"doc-x", "doc-y"}) || len(removal.Documents) != 0 || removal.Limits.MaxUSD != 0 ||
			removal.RequestID != "job-2:remove" {
			t.Fatalf("removal request = %s", extracts[0])
		}
		if second := decodeExtract(t, extracts[1]); len(second.Remove) != 0 || len(second.Documents) != 4 {
			t.Fatalf("extract request = %s", extracts[1])
		}
		// doc-y, which the load didn't hold, is forgotten once Extract is done.
		if !slices.Equal(h.store.forgotten, []string{"doc-x", "doc-y"}) {
			t.Fatalf("forgotten = %v", h.store.forgotten)
		}
	})

	// An extract owed only for removals runs with no document to extract.
	t.Run("runs for removals alone", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending, h.store.removed = nil, []string{"doc-x"}
		h.agent.extract = removing
		outcome := h.run(t, extractJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if _, extracts := h.agent.calls(); len(extracts) != 1 || !slices.Equal(h.store.forgotten, []string{"doc-x"}) {
			t.Fatalf("%d extracts, forgotten = %v", len(extracts), h.store.forgotten)
		}
	})
}
