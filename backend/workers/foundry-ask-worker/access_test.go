package foundryaskworker

import (
	"encoding/json"
	"fmt"
	"testing"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// granteeHarness is the harness with a question an agent's key asked
// through its grants: Stripe's customers, every table of conn-gone and the
// Contracts set, which cover load-1, the current load. It doesn't hold the
// Notes set Stripe's connection owns, only its customers table.
func granteeHarness(t *testing.T) (*harness, *model.FoundryQuestion) {
	t.Helper()
	h := newHarness(t)
	key := access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred-1"}
	for _, target := range []access.Target{access.TableTarget("conn-1", "customers"), access.ConnectorTarget("conn-gone"), access.DocumentSetTarget("set-1")} {
		h.store.grants = append(h.store.grants, model.FoundryAccessGrant{WorkspaceID: "ws-1", Principal: key, Target: target})
	}
	h.store.currentLoads = map[string]model.FoundryLoadInputs{"ont-1": {LoadID: "load-1", VersionID: "ver-3", Inputs: access.Inputs{
		Tables: []access.TableRef{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-gone", Table: "deals"}},
		Sets:   []access.SetRef{{SetID: "set-1"}},
	}}}
	q := question(1)
	q.Grantee = &key
	return h, q
}

func TestGranteeQuestions(t *testing.T) {
	// It is asked without the goal the ontology was drafted from, and its
	// catalog names only the sets it holds; the tables and files of a load it
	// covers are all its.
	t.Run("asked as the grantee", func(t *testing.T) {
		h, q := granteeHarness(t)
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req, _ := decodeAsk(t, h.agent.calls()[0])
		catalog := []ontologyagent.CatalogEntry{
			{ID: ontologyagent.TableSourceID("conn-1", "customers"), Kind: ontologyagent.CatalogTable, Name: "Stripe / customers"},
			{ID: ontologyagent.TableSourceID("conn-gone", "deals"), Kind: ontologyagent.CatalogTable, Name: "conn-gone / deals"},
			{ID: "doc-csv", Kind: ontologyagent.CatalogTable, Name: "Contracts / orders.csv"},
			{ID: "doc-gone", Kind: ontologyagent.CatalogTable, Name: "doc-gone"},
			{ID: "set-1", Kind: ontologyagent.CatalogDocumentSet, Name: "Contracts"},
		}
		if req.Goal != "" || fmt.Sprint(req.Catalog) != fmt.Sprint(catalog) {
			t.Fatalf("goal %q, catalog %+v", req.Goal, req.Catalog)
		}
	})
	// It is told only what it said about loads its grants cover now.
	t.Run("told only of loads it still reads", func(t *testing.T) {
		h, q := granteeHarness(t)
		turn := func(text, loadID string) *model.FoundryQuestion {
			return &model.FoundryQuestion{Question: text, LoadID: loadID, Result: json.RawMessage(answeredResult)}
		}
		h.store.history = []*model.FoundryQuestion{turn("current", "load-1"), turn("covered", "load-0"), turn("uncovered", "load-other"), turn("gone", "load-gone")}
		h.store.olderLoads = map[string]model.FoundryLoadInputs{
			"load-0":     {LoadID: "load-0", Inputs: access.Inputs{Sets: []access.SetRef{{SetID: "set-1"}}}},
			"load-other": {LoadID: "load-other", Inputs: access.Inputs{Sets: []access.SetRef{{SetID: "set-no-one-holds"}}}},
		}
		if settled := h.run(t, q); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req, _ := decodeAsk(t, h.agent.calls()[0])
		var told []string
		for _, turn := range req.History {
			told = append(told, turn.Question)
		}
		if fmt.Sprint(told) != "[current covered]" {
			t.Fatalf("told %v", told)
		}
	})
	// Grants taken away before it runs, or another load made current, and it
	// fails for good without asking.
	for name, change := range map[string]func(*harness){
		"its grants taken away": func(h *harness) { h.store.grants = h.store.grants[1:] },
		"another load current": func(h *harness) {
			l := h.store.currentLoads["ont-1"]
			l.LoadID = "load-2"
			h.store.currentLoads["ont-1"] = l
		},
		"no load current": func(h *harness) { h.store.currentLoads = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, q := granteeHarness(t)
			change(h)
			settled := h.run(t, q)
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != noLongerGranted ||
				settled[0].Result != nil || len(h.agent.calls()) != 0 {
				t.Fatalf("settled %+v, asked %d times", settled, len(h.agent.calls()))
			}
		})
	}
	// Taken away while it runs, and its answer isn't kept.
	t.Run("its grants taken away while it runs", func(t *testing.T) {
		h, q := granteeHarness(t)
		answerIt := h.agent.ask
		h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			h.store.mu.Lock()
			h.store.grants = nil
			h.store.mu.Unlock()
			return answerIt(req, stream)
		}
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != noLongerGranted ||
			settled[0].Result != nil || len(h.agent.calls()) != 1 {
			t.Fatalf("settled %+v", settled)
		}
	})
}
