package foundrygraphworker

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// linked is a resolve decision of doc that linked name to entity, or "not in
// data" with no entity.
func linked(id, doc, name string, entity *string, at time.Time) *model.FoundryReviewDecision {
	return &model.FoundryReviewDecision{ID: id, OntologyID: "ont-1", Kind: model.FoundryReviewResolve, Type: "Customer", DocID: &doc,
		NormName: &name, EntityKey: entity, DecidedByUserID: "user", DecidedAt: at}
}

// aliases are the alias decisions a request carries.
func aliases(r ontologyagent.ExtractRequest) []ontologyagent.ExtractDecision {
	var out []ontologyagent.ExtractDecision
	for _, d := range r.Decisions {
		if d.Kind == ontologyagent.AliasDecision {
			out = append(out, d)
		}
	}
	return out
}

// Contract 4.22.0: each name a person linked to an entity in any document of
// the load is an alias every Extract of the load carries, so it links in
// every document where the names rung would ask; the engine checks each
// request against the schema (the fake agent's refused), so an alias it
// carries is one the engine takes. One alias a name, type and entity, under
// its first decision's id and time; a name linked to two entities goes as
// both, for the engine to link neither; "not in data" carries nothing.
func TestExtractCarriesAliases(t *testing.T) {
	acme, globex := "acme", "globex"
	decisions := func() []*model.FoundryReviewDecision {
		return []*model.FoundryReviewDecision{
			// A document no Extract here sends linked it first; doc-a later.
			linked("1", "doc-a", "acme corp", &acme, settledAt.Add(time.Hour)),
			linked("2", "doc-z", "acme corp", &acme, settledAt),
			linked("3", "doc-y", "helio", &acme, settledAt),
			linked("4", "doc-x", "helio", &globex, settledAt),
			linked("5", "doc-w", "initech", nil, settledAt),
		}
	}
	want := []ontologyagent.ExtractDecision{
		{Kind: ontologyagent.AliasDecision, Type: "Customer", NormName: "acme corp", EntityKey: &acme, DecisionID: "2", DecidedAt: settledAt},
		{Kind: ontologyagent.AliasDecision, Type: "Customer", NormName: "helio", EntityKey: &acme, DecisionID: "3", DecidedAt: settledAt},
		{Kind: ontologyagent.AliasDecision, Type: "Customer", NormName: "helio", EntityKey: &globex, DecisionID: "4", DecidedAt: settledAt},
	}

	t.Run("a load's Extract", func(t *testing.T) {
		h := newHarness(t)
		namedCustomers(t, h)
		h.store.decisions = decisions()
		if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 1 {
			t.Fatalf("%d extracts", len(calls))
		}
		if got := aliases(decodeExtract(t, calls[0])); !reflect.DeepEqual(got, want) {
			t.Fatalf("aliases %+v", got)
		}
		// Exactly the six fields the engine reads of one.
		const first = `{"kind":"alias","type":"Customer","norm_name":"acme corp","entity_key":"acme","decision_id":"2","decided_at":"2026-10-17T08:02:03Z"}`
		if !strings.Contains(calls[0], first) {
			t.Fatalf("the request lacks %s: %s", first, calls[0])
		}
	})

	t.Run("a resend, and one of no documents", func(t *testing.T) {
		h := newHarness(t)
		namedCustomers(t, h)
		owes(h, "doc-a")
		h.store.decisions = append(decisions(), approval("9", "doc-a", hashA))
		h.agent.extract = writesAll(t)
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 1 || !reflect.DeepEqual(aliases(decodeExtract(t, calls[0])), want) {
			t.Fatalf("%d extracts: %v", len(calls), calls)
		}

		h = newHarness(t)
		namedCustomers(t, h)
		h.store.owedDecisions = &model.FoundryLoadResend{LoadID: "load-0", Reason: model.FoundryResendDecisions, Requests: 1}
		h.store.decisions = decisions()
		h.agent.extract = writesAll(t)
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, calls = h.agent.calls()
		if len(calls) != 1 || len(decodeExtract(t, calls[0]).Documents) != 0 || !reflect.DeepEqual(aliases(decodeExtract(t, calls[0])), want) {
			t.Fatalf("%d extracts: %v", len(calls), calls)
		}
	})

	// A type the version dropped carries nothing: the engine would count it
	// stale, and it takes no room under the cap.
	t.Run("a type the version no longer has", func(t *testing.T) {
		h := newHarness(t)
		namedCustomers(t, h)
		gone := linked("6", "doc-v", "umbrella", &acme, settledAt)
		gone.Type = "Supplier"
		h.store.decisions = []*model.FoundryReviewDecision{gone}
		decisions, err := h.task.runner.decisions(t.Context(), extractJob(), "[types.Customer]\nprops = { name = \"string\" }", nil)
		if err != nil || len(decisions) != 0 {
			t.Fatalf("%+v %v", decisions, err)
		}
	})
}

// What the engine says of each name decision of a document is kept on its
// row (4.22.0), unknown refusals as they came, with the entities an alias
// linked; a resolve's outcome is not written to the load's list. A resend's
// failed event still says what its settling wrote, which joins the row.
func TestExtractRecordsNames(t *testing.T) {
	const names = `"names":[{"decision":"resolve","type":"Customer","norm_name":"acme corp","decision_id":"1","outcome":{"kind":"written"}},` +
		`{"decision":"alias","type":"Customer","norm_name":"helio","decision_id":"3","outcome":{"kind":"refused","reason":"two_rows"}},` +
		`{"decision":"alias","type":"Customer","norm_name":"initech","outcome":{"kind":"refused","reason":"a_reason_of_a_later_engine"}}],`
	id1, id3 := "1", "3"
	want := []model.FoundryDocumentName{
		{Decision: model.FoundryNameResolve, Type: "Customer", NormName: "acme corp", DecisionID: &id1, Outcome: model.FoundryNameWritten},
		{Decision: model.FoundryNameAlias, Type: "Customer", NormName: "helio", DecisionID: &id3, Outcome: model.FoundryNameRefused, Reason: model.FoundryNameTwoRows},
		{Decision: model.FoundryNameAlias, Type: "Customer", NormName: "initech", Outcome: model.FoundryNameRefused, Reason: "a_reason_of_a_later_engine"},
	}

	t.Run("a commit", func(t *testing.T) {
		h := newHarness(t)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			a := strings.Replace(documentEvent("doc-a", `{"kind":"ingested"}`, noUsage), `"names":[],`, names, 1)
			events := []string{extracted[0], a}
			for _, doc := range []string{"doc-b", "doc-c", "doc-d"} {
				events = append(events, documentEvent(doc, `{"kind":"out_of_scope"}`, noUsage))
			}
			return extractEvents(stream, append(events, `{"event":"extract_finished"}`), `{"status":"done","totals":`+totals+`}`)
		}
		if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		got := h.store.extractions["doc-a"]
		if got == nil || !reflect.DeepEqual(got.Names, want) || got.Counts.ResolvedCarried != 4 || len(got.Releases) != 0 {
			t.Fatalf("recorded %+v", got)
		}
		if b := h.store.extractions["doc-b"]; b == nil || b.Names != nil {
			t.Fatalf("doc-b recorded %+v", b)
		}
	})

	t.Run("a resend the engine failed", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a")
		h.store.decisions = []*model.FoundryReviewDecision{approval("9", "doc-a", hashA)}
		failed := strings.Replace(documentEvent("doc-a", `{"kind":"failed","code":"store","retryable":false}`, noUsage), `"names":[],`, names, 1)
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{extracted[0], failed, `{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
		}
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if !reflect.DeepEqual(h.store.namesSaid["doc-a"], want) || h.store.extractions["doc-a"] != nil {
			t.Fatalf("names %+v, row %+v", h.store.namesSaid, h.store.extractions["doc-a"])
		}
	})

	// A name decision of a kind or outcome this contract doesn't write, or a
	// refusal without its reason, fails the job rather than guess.
	for name, entry := range map[string]string{
		"unknown decision":    `{"decision":"rename","type":"Customer","norm_name":"a","outcome":{"kind":"written"}}`,
		"unknown outcome":     `{"decision":"resolve","type":"Customer","norm_name":"a","outcome":{"kind":"maybe"}}`,
		"refused, no reason":  `{"decision":"alias","type":"Customer","norm_name":"a","outcome":{"kind":"refused"}}`,
		"written, a reason":   `{"decision":"alias","type":"Customer","norm_name":"a","outcome":{"kind":"written","reason":"bound"}}`,
		"no name":             `{"decision":"alias","type":"Customer","norm_name":"","outcome":{"kind":"written"}}`,
		"no type":             `{"decision":"alias","type":"","norm_name":"a","outcome":{"kind":"written"}}`,
		"not_named, a reason": `{"decision":"resolve","type":"Customer","norm_name":"a","outcome":{"kind":"not_named","reason":"absent"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
				a := strings.Replace(documentEvent("doc-a", `{"kind":"ingested"}`, noUsage), `"names":[],`, `"names":[`+entry+`],`, 1)
				return extractEvents(stream, []string{extracted[0], a, `{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
			}
			if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobFailed || !strings.Contains(outcome.Error, "can't read") {
				t.Fatalf("%+v", outcome)
			}
		})
	}
}
