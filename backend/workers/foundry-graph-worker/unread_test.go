package foundrygraphworker

import (
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// extractModelFailing ends each of docs failed by the model with failure,
// and every other pending document ingested, then the call done.
func extractModelFailing(h *harness, failure string, docs ...string) func(string, agentv1.Agent_ExtractServer) error {
	return func(_ string, stream agentv1.Agent_ExtractServer) error {
		var events []string
		for _, d := range h.store.pending {
			outcome := `{"kind":"ingested"}`
			if strings.Contains(" "+strings.Join(docs, " ")+" ", " "+d.ID+" ") {
				outcome = `{"kind":"failed","code":"model","model_failure":"` + failure + `","retryable":true}`
			}
			events = append(events, documentEvent(d.ID, outcome, noUsage))
		}
		return extractEvents(stream, events, `{"status":"done","totals":`+totals+`}`)
	}
}

func TestUnreadDocuments(t *testing.T) {
	// The 2026-11-14 outage: every model call on Helix's key failed for
	// quota, and the load still read as ready. Now it fails, saying none
	// was read and why, and each row keeps the failure's class.
	t.Run("fails a load whose every document the model failed", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = extractModelFailing(h, "quota", "doc-a", "doc-b", "doc-c", "doc-d")
		outcome := h.run(t, loadJob())
		const want = "none of the load's documents could be read (4 documents): the model provider's quota or credit ran out; start a new load to read them"
		if outcome.Status != model.FoundryGraphJobFailed || outcome.Error != want || !outcome.IngestDone {
			t.Fatalf("outcome = %+v", outcome)
		}
		for id, e := range h.store.extractions {
			if e.Status != model.FoundryExtractionFailed || e.Error.Code != model.FoundryExtractionModel || !e.Error.Retryable ||
				e.Error.ModelFailure != "quota" {
				t.Fatalf("%s = %+v %+v", id, e, e.Error)
			}
		}
	})

	// Some read: the load is partial, which a current load holding more is
	// never replaced by, and says how many went unread and that Helix reads
	// them again.
	t.Run("leaves a load partial when the model failed some documents", func(t *testing.T) {
		for failure, want := range map[string]string{
			"quota":       "2 documents weren't read: the model provider's quota or credit ran out; Helix reads them again on its own while the load is current",
			"unavailable": "2 documents weren't read: the model provider was busy; Helix reads them again on its own while the load is current",
			"unsupported": "2 documents weren't read: the model rejected the request; the next load reads them",
		} {
			h := newHarness(t)
			h.agent.extract = extractModelFailing(h, failure, "doc-b", "doc-d")
			outcome := h.run(t, loadJob())
			if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != want || len(outcome.HeldSets) != 0 {
				t.Fatalf("%s: outcome = %+v", failure, outcome)
			}
			if e := h.store.extractions["doc-b"]; e.Error.ModelFailure != failure {
				t.Fatalf("%s: doc-b = %+v", failure, e.Error)
			}
		}
	})

	t.Run("says one document in the singular", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = extractModelFailing(h, "rate_limited", "doc-c")
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobBudgetExhausted ||
			outcome.Error != "1 document wasn't read: the model provider was busy; Helix reads them again on its own while the load is current" {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	// A workspace's own key refused stops the load at once (DocumentFailure):
	// the owner's words stay, and with nothing read the load fails.
	t.Run("fails a load the owner's key read nothing of", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.agent.extract = extractModelFailing(h, "quota", "doc-a")
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobFailed ||
			outcome.Error != "None of the load's documents could be read. The Claude account is out of credit or quota." {
			t.Fatalf("outcome = %+v", outcome)
		}
		if len(h.store.extractions) != 1 {
			t.Fatalf("the load went on past a refused key: %d rows", len(h.store.extractions))
		}
	})

	t.Run("keeps the owner's words when its key stopped a load with some read", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.agent.extract = extractModelFailing(h, "quota", "doc-c")
		outcome := h.run(t, loadJob())
		if outcome.Status != model.FoundryGraphJobBudgetExhausted || outcome.Error != "The Claude account is out of credit or quota." {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	// A document's other failures (too large, unsupported) are not the
	// model's: the load is ready, as before.
	t.Run("leaves a load ready when the model failed nothing", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				documentEvent("doc-a", `{"kind":"ingested"}`, usage),
				documentEvent("doc-b", `{"kind":"failed","code":"unsupported","retryable":false}`, noUsage),
				documentEvent("doc-c", `{"kind":"ingested"}`, usage),
				documentEvent("doc-d", `{"kind":"ingested"}`, usage),
			}, `{"status":"done","totals":`+totals+`}`)
		}
		if outcome := h.run(t, loadJob()); outcome.Status != model.FoundryGraphJobSucceeded || outcome.Error != "" {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	// An extract into a load already written leaves its status to the store
	// (a partial one becomes ready once nothing waits): the job succeeds and
	// the rows say what went unread.
	t.Run("lets an extract with model failures succeed", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = extractModelFailing(h, "quota", "doc-a", "doc-b", "doc-c", "doc-d")
		if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
	})

	// A re-read sends what a passing failure left unread and what no job
	// recorded, never a document the model failed for good.
	t.Run("re-reads only what a passing failure left unread", func(t *testing.T) {
		h := newHarness(t)
		failed := func(f string) *model.FoundryDocumentExtraction {
			return &model.FoundryDocumentExtraction{Status: model.FoundryExtractionFailed,
				Error: &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true, ModelFailure: f}}
		}
		h.store.prior = map[string]map[string]*model.FoundryDocumentExtraction{"load-0": {
			"doc-a": failed("quota"), "doc-b": failed("context_too_long"), "doc-c": failed(""),
			"doc-d": {Status: model.FoundryExtractionFailed, Error: &model.FoundryExtractionError{Code: model.FoundryExtractionStore, Retryable: true}},
		}}
		h.agent.extract = extractModelFailing(h, "quota")
		job := extractJob()
		token := model.FoundryRereadToken + job.ID
		job.ClientToken = &token
		if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		requests := h.extractRequests(t)
		if len(requests) != 1 || !slices.Equal(docIDs(requests[0]), []string{"doc-a", "doc-c"}) {
			t.Fatalf("sent %v", requests)
		}

		// Any other job sends every document the load waits for.
		h = newHarness(t)
		h.store.prior = map[string]map[string]*model.FoundryDocumentExtraction{"load-0": {"doc-b": failed("context_too_long")}}
		h.agent.extract = extractModelFailing(h, "quota")
		h.run(t, extractJob())
		if requests := h.extractRequests(t); len(requests) != 1 || len(docIDs(requests[0])) != 4 {
			t.Fatalf("sent %v", requests)
		}
	})
}
