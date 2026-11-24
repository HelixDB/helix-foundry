package foundryaskworker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	"github.com/helixdb/helix-foundry/backend/ports"
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
	h.store.holdings = map[string]access.Holdings{"load-1": {
		Tables:    []access.TableRef{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-gone", Table: "deals"}},
		Documents: []access.DocumentRef{{ID: "doc-1", Set: access.SetRef{SetID: "set-1"}}},
	}}
	q := question(1)
	q.Grantee = &key
	return h, q
}

// partHarness is granteeHarness with a load that holds more than the key's
// grants do: PostgreSQL's people, and doc-2 in a set it doesn't hold.
func partHarness(t *testing.T) (*harness, *model.FoundryQuestion) {
	t.Helper()
	h, q := granteeHarness(t)
	h.store.grants = []model.FoundryAccessGrant{
		{WorkspaceID: "ws-1", Principal: *q.Grantee, Target: access.TableTarget("conn-1", "customers")},
		{WorkspaceID: "ws-1", Principal: *q.Grantee, Target: access.DocumentSetTarget("set-1")},
	}
	h.store.currentLoads = map[string]model.FoundryLoadInputs{"ont-1": {LoadID: "load-1", VersionID: "ver-3", Inputs: access.Inputs{
		Tables: []access.TableRef{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-gone", Table: "deals"}},
		Sets:   []access.SetRef{{SetID: "set-1"}, {SetID: "set-other"}},
	}}}
	h.store.holdings = map[string]access.Holdings{"load-1": {
		Tables:    []access.TableRef{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-gone", Table: "deals"}},
		Documents: []access.DocumentRef{{ID: "doc-1", Set: access.SetRef{SetID: "set-1"}}, {ID: "doc-2", Set: access.SetRef{SetID: "set-other"}}},
	}}
	return h, q
}

// askedWithin checks the agent's first request against ask_request.json and
// that its reads carry the caps the deployment configured, set here apart
// from the defaults.
func askedWithin(t *testing.T, h *harness) ontologyagent.AskRequest {
	t.Helper()
	raw := h.agent.calls()[0]
	if err := contracttest.Validate(t, contracttest.Schema(t, "ask_request.json"), []byte(raw)); err != nil {
		t.Fatalf("the request does not match ask_request.json: %v", err)
	}
	req, _ := decodeAsk(t, raw)
	ask := h.task.runner.cfg.Foundry.Ask
	if want := (ontologyagent.AskReadLimits{WallMS: uint64(ask.ReadWallMS), ScanEntities: uint64(ask.ReadScanEntities)}); req.Limits.Read != want {
		t.Fatalf("read caps = %+v, want %+v", req.Limits.Read, want)
	}
	return req
}

func TestGranteeQuestions(t *testing.T) {
	// It is asked without the goal the ontology was drafted from, and its
	// catalog names only the sets it holds; the tables and files of a load it
	// covers are all its.
	t.Run("asked as the grantee", func(t *testing.T) {
		h, q := granteeHarness(t)
		h.task.runner.cfg.Foundry.Ask.ReadWallMS, h.task.runner.cfg.Foundry.Ask.ReadScanEntities = 12_000, 34_000
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req := askedWithin(t, h)
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
		// It reads through what it holds, all of it, so nothing raw or historic.
		if req.Visibility == nil || !req.Visibility.Documents.All || len(req.Visibility.Sources) != 2 {
			t.Fatalf("visibility %+v", req.Visibility)
		}
	})
	// A question nobody asked through grants is asked as it was.
	t.Run("the host's question names no visibility", func(t *testing.T) {
		h := newHarness(t)
		if settled := h.run(t, question(1)); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req, _ := decodeAsk(t, h.agent.calls()[0])
		if req.Visibility != nil {
			t.Fatalf("visibility %+v", req.Visibility)
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
	// Of a load it holds only some of, it is asked with what it holds, and
	// told only of that, and what it read is kept with its answer.
	t.Run("asked of the part it holds", func(t *testing.T) {
		h, q := partHarness(t)
		h.task.runner.cfg.Foundry.Ask.ReadWallMS, h.task.runner.cfg.Foundry.Ask.ReadScanEntities = 12_000, 34_000
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req := askedWithin(t, h)
		catalog := []ontologyagent.CatalogEntry{
			{ID: ontologyagent.TableSourceID("conn-1", "customers"), Kind: ontologyagent.CatalogTable, Name: "Stripe / customers"},
			{ID: "set-1", Kind: ontologyagent.CatalogDocumentSet, Name: "Contracts"},
		}
		if fmt.Sprint(req.Catalog) != fmt.Sprint(catalog) {
			t.Fatalf("catalog %+v", req.Catalog)
		}
		want := ontologyagent.Visibility{
			Sources: []string{ontologyagent.TableSourceID("conn-1", "customers")}, Sets: []string{"set-1"},
			Documents: ontologyagent.VisibleDocuments{Only: []string{"doc-1"}}, Withheld: []ontologyagent.WithheldProp{},
		}
		if req.Visibility == nil || fmt.Sprintf("%+v", *req.Visibility) != fmt.Sprintf("%+v", want) {
			t.Fatalf("visibility %+v, want %+v", req.Visibility, want)
		}
		seen := access.Inputs{Tables: []access.TableRef{{ConnectorID: "conn-1", Table: "customers"}}, Sets: []access.SetRef{{SetID: "set-1"}}}
		if got := h.store.seen[q.ID]; got == nil || fmt.Sprint(*got) != fmt.Sprint(seen) {
			t.Fatalf("seen %+v", h.store.seen)
		}
	})
	// A later attempt keeps what an earlier attempt's events showed, and a run
	// over the whole load reads as one.
	t.Run("what earlier attempts read stays with the question", func(t *testing.T) {
		h, q := partHarness(t)
		earlier := access.Inputs{Sets: []access.SetRef{{SetID: "set-other"}}}
		q.Seen, q.Attempts = &earlier, 2
		h.run(t, q)
		got := h.store.seen[q.ID]
		if got == nil || !slices.Contains(got.Sets, access.SetRef{SetID: "set-other"}) || !slices.Contains(got.Sets, access.SetRef{SetID: "set-1"}) {
			t.Fatalf("seen %+v", got)
		}
		h, q = granteeHarness(t)
		q.Seen = &earlier
		h.run(t, q)
		if got, ok := h.store.seen[q.ID]; !ok || got != nil {
			t.Fatalf("a whole run read as %+v", got)
		}
		// An earlier attempt that ran whole read all of the load.
		h, q = partHarness(t)
		q.Attempts = 2
		h.run(t, q)
		got = h.store.seen[q.ID]
		if got == nil || !slices.Contains(got.Sets, access.SetRef{SetID: "set-other"}) {
			t.Fatalf("seen %+v", got)
		}
	})
	// Holding less of it by the time it's answered, its answer isn't kept.
	for name, shrink := range map[string]func(*harness){
		"a document moved out of what it holds": func(h *harness) {
			holdings := h.store.holdings["load-1"]
			holdings.Documents = []access.DocumentRef{{ID: "doc-1", Set: access.SetRef{SetID: "set-other"}}, {ID: "doc-2", Set: access.SetRef{SetID: "set-1"}}}
			h.store.holdings["load-1"] = holdings
		},
		"a value decided from what it doesn't hold": func(h *harness) {
			holdings := h.store.holdings["load-1"]
			holdings.Decisions = []access.PropDecision{{Type: "Customer", Prop: "plan", Sets: []access.SetRef{{SetID: "set-other"}}, Recorded: true}}
			h.store.holdings["load-1"] = holdings
		},
		"it no longer holds any of it": func(h *harness) { h.store.grants = nil },
	} {
		t.Run(name, func(t *testing.T) {
			h, q := partHarness(t)
			answerIt := h.agent.ask
			h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
				h.store.mu.Lock()
				shrink(h)
				h.store.mu.Unlock()
				return answerIt(req, stream)
			}
			settled := h.run(t, q)
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != noLongerGranted || settled[0].Result != nil {
				t.Fatalf("settled %+v", settled)
			}
		})
	}
	// A full grantee whose coverage narrowed while it ran read more than it now holds.
	t.Run("its grants come to cover less of it while it runs", func(t *testing.T) {
		h, q := granteeHarness(t)
		answerIt := h.agent.ask
		h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			h.store.mu.Lock()
			l := h.store.currentLoads["ont-1"]
			l.Inputs.Tables = append(slices.Clone(l.Inputs.Tables), access.TableRef{ConnectorID: "conn-other", Table: "x"})
			h.store.currentLoads["ont-1"] = l
			h.store.mu.Unlock()
			return answerIt(req, stream)
		}
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != noLongerGranted {
			t.Fatalf("settled %+v", settled)
		}
	})
	// Told of a turn that read part of a load while its grants hold that part.
	t.Run("told of a part it still holds", func(t *testing.T) {
		h, q := partHarness(t)
		holds := access.Inputs{Sets: []access.SetRef{{SetID: "set-1"}}}
		lost := access.Inputs{Sets: []access.SetRef{{SetID: "set-other"}}}
		h.store.history = []*model.FoundryQuestion{
			{Question: "kept", LoadID: "load-1", Seen: &holds, Result: json.RawMessage(answeredResult)},
			{Question: "lost", LoadID: "load-1", Seen: &lost, Result: json.RawMessage(answeredResult)},
		}
		h.run(t, q)
		req, _ := decodeAsk(t, h.agent.calls()[0])
		if len(req.History) != 1 || req.History[0].Question != "kept" {
			t.Fatalf("told %+v", req.History)
		}
	})
	// Its answer can repeat a turn it's told of, so it reads only while grants
	// hold what that turn read, also when this load holds none of it.
	t.Run("what its earlier turns read stays with the question", func(t *testing.T) {
		old := access.TableRef{ConnectorID: "conn-gone", Table: "old"}
		h, q := partHarness(t)
		h.store.grants = append(h.store.grants, model.FoundryAccessGrant{WorkspaceID: "ws-1", Principal: *q.Grantee, Target: access.TableTarget(old.ConnectorID, old.Table)})
		read := access.Inputs{Tables: []access.TableRef{old}}
		h.store.history = []*model.FoundryQuestion{{Question: "earlier", LoadID: "load-1", Seen: &read, Result: json.RawMessage(answeredResult)}}
		h.run(t, q)
		if got := h.store.seen[q.ID]; got == nil || !slices.Contains(got.Tables, old) {
			t.Fatalf("a part: seen %+v", got)
		}
		h, q = granteeHarness(t)
		h.store.olderLoads = map[string]model.FoundryLoadInputs{"load-0": {LoadID: "load-0", Inputs: read}}
		h.store.history = []*model.FoundryQuestion{{Question: "earlier", LoadID: "load-0", Result: json.RawMessage(answeredResult)}}
		h.run(t, q)
		got := h.store.seen[q.ID]
		if got == nil || !slices.Contains(got.Tables, old) || !slices.Contains(got.Tables, access.TableRef{ConnectorID: "conn-1", Table: "customers"}) {
			t.Fatalf("whole, after another load: seen %+v", got)
		}
		// A whole reader's turn of this same load holds nothing it doesn't.
		h, q = granteeHarness(t)
		h.store.history = []*model.FoundryQuestion{{Question: "earlier", LoadID: "load-1", Result: json.RawMessage(answeredResult)}}
		h.run(t, q)
		if got, ok := h.store.seen[q.ID]; ok {
			t.Fatalf("whole, on this load: seen %+v", got)
		}
	})
	// Grants taken away before it runs, or another load made current, and it
	// fails for good without asking.
	for name, change := range map[string]func(*harness){
		"its grants taken away": func(h *harness) { h.store.grants = nil },
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

// emails are the members' verified emails, by user id.
type emails map[string]string

func (emails) MayManageAI(context.Context, string, string) bool { return false }

func (emails) DisplayName(context.Context, string) (string, error) { return "", nil }

func (e emails) Member(_ context.Context, _, userID string) (ports.Member, error) {
	return ports.Member{Role: ports.RoleMember, Email: e[userID]}, nil
}

// slackHarness is the harness with a question Ada asked through a grant of
// conn-slack, a Slack connection whose load holds a day of #general (C1,
// public) and of #deals (C2, private, which lists Ada while inAll).
func slackHarness(t *testing.T, inAll bool) (*harness, *model.FoundryQuestion) {
	t.Helper()
	h := newHarness(t)
	ada := access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}
	h.task.runner.people = emails{"user-ada": "Ada@Example.com"}
	h.store.slack = true
	h.store.grants = []model.FoundryAccessGrant{{WorkspaceID: "ws-1", Principal: ada, Target: access.ConnectorTarget("conn-slack")}}
	conversations := access.SetRef{SetID: "set-slack", ConnectorID: "conn-slack"}
	h.store.currentLoads = map[string]model.FoundryLoadInputs{"ont-1": {LoadID: "load-1", VersionID: "ver-3", Inputs: access.Inputs{
		Sets: []access.SetRef{conversations},
	}}}
	h.store.holdings = map[string]access.Holdings{"load-1": {Documents: []access.DocumentRef{
		{ID: "doc-general", Set: conversations, Stream: "conversations", ExternalID: "C1:2026-11-01"},
		{ID: "doc-deals", Set: conversations, Stream: "conversations", ExternalID: "C2:2026-11-01"},
	}}}
	now := time.Now()
	visible := []string{"C1"}
	if inAll {
		visible = []string{"C1", "C2"}
	}
	h.store.sharing = map[string][]model.FoundrySourceSharing{
		"ada@example.com": {{ConnectorID: "conn-slack", ReadAt: &now, Visible: visible}},
		"agent":           {{ConnectorID: "conn-slack", ReadAt: &now, Visible: []string{"C1"}}},
	}
	q := question(1)
	q.Grantee = &ada
	return h, q
}

func TestGranteeQuestionsFollowSlack(t *testing.T) {
	// The channels Slack shares with Ada are what her question reads, and
	// what it read stays with it, as the API keeps it readable (Keeps).
	t.Run("only her channels", func(t *testing.T) {
		h, q := slackHarness(t, false)
		if settled := h.run(t, q); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req := askedWithin(t, h)
		if req.Visibility == nil || req.Visibility.Documents.All || fmt.Sprint(req.Visibility.Documents.Only) != "[doc-general]" {
			t.Fatalf("visibility %+v", req.Visibility)
		}
		seen := h.store.seen[q.ID]
		if seen == nil || fmt.Sprint(seen.Sets) != "[{set-slack conn-slack}]" || fmt.Sprint(h.store.seenItems[q.ID]) != "[{conn-slack C1}]" {
			t.Fatalf("seen %+v, items %+v", seen, h.store.seenItems[q.ID])
		}
	})
	// Slack's tables are read by their rows: the agent is told her channels'
	// rows and, Slack knowing her as no guest, its directory; the catalog
	// names the tables, and what her answer read keeps them and their channel.
	t.Run("Slack's tables by her channels' rows", func(t *testing.T) {
		h, q := slackHarness(t, false)
		now := time.Now()
		h.store.sharing["ada@example.com"] = []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &now, Visible: []string{"C1"}, Member: true}}
		tables := []access.TableRef{{ConnectorID: "conn-slack", Table: "users"}, {ConnectorID: "conn-slack", Table: "messages"},
			{ConnectorID: "conn-slack", Table: "reactions"}}
		load := h.store.currentLoads["ont-1"]
		load.Inputs.Tables = tables
		h.store.currentLoads["ont-1"] = load
		holdings := h.store.holdings["load-1"]
		holdings.Tables = tables
		h.store.holdings["load-1"] = holdings
		h.store.generation.Input.Tables = []model.OntologyGenerationTable{{ConnectorID: "conn-slack", Table: "users"},
			{ConnectorID: "conn-slack", Table: "messages"}, {ConnectorID: "conn-slack", Table: "reactions"}}
		if settled := h.run(t, q); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		req := askedWithin(t, h)
		messages := ontologyagent.TableSourceID("conn-slack", "messages")
		if req.Visibility == nil || fmt.Sprint(req.Visibility.Sources) != "["+ontologyagent.TableSourceID("conn-slack", "users")+"]" ||
			fmt.Sprint(req.Visibility.Rows) != fmt.Sprint([]ontologyagent.HeldRows{{Source: messages, Field: "message_id", Prefix: true, Items: []string{"C1:"}}}) {
			t.Fatalf("visibility %+v", req.Visibility)
		}
		var catalog []string
		for _, e := range req.Catalog {
			if e.Kind == ontologyagent.CatalogTable {
				catalog = append(catalog, e.ID)
			}
		}
		if !slices.Contains(catalog, messages) || slices.Contains(catalog, ontologyagent.TableSourceID("conn-slack", "reactions")) {
			t.Fatalf("catalog tables %v: the messages, never a table no rule tells apart", catalog)
		}
		seen := h.store.seen[q.ID]
		if seen == nil || fmt.Sprint(seen.Tables) != "[{conn-slack users} {conn-slack messages}]" || fmt.Sprint(h.store.seenItems[q.ID]) != "[{conn-slack *directory} {conn-slack C1}]" {
			t.Fatalf("seen %+v, items %+v", seen, h.store.seenItems[q.ID])
		}
	})
	// The load may hold days of a channel the last read didn't list, so no
	// reader of every listed channel reads it whole: each document is told.
	t.Run("every channel shared with her still reads the load document by document", func(t *testing.T) {
		h, q := slackHarness(t, true)
		holdings := h.store.holdings["load-1"]
		holdings.Documents = append(holdings.Documents, access.DocumentRef{ID: "doc-exec", Set: access.SetRef{SetID: "set-slack", ConnectorID: "conn-slack"},
			Stream: "conversations", ExternalID: "C9:2026-11-01"})
		h.store.holdings["load-1"] = holdings
		h.run(t, q)
		req := askedWithin(t, h)
		if req.Visibility == nil || req.Visibility.Documents.All || fmt.Sprint(req.Visibility.Documents.Only) != "[doc-general doc-deals]" {
			t.Fatalf("visibility %+v", req.Visibility)
		}
		if got := h.store.seenItems[q.ID]; fmt.Sprint(got) != "[{conn-slack C1} {conn-slack C2}]" {
			t.Fatalf("items %+v", got)
		}
	})
	t.Run("an email Slack has no account for reads none of its channels", func(t *testing.T) {
		h, q := slackHarness(t, true)
		h.task.runner.people = emails{"user-ada": "ada@elsewhere.example"}
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Error != noLongerGranted || len(h.agent.calls()) != 0 {
			t.Fatalf("settled %+v", settled)
		}
	})
	// She is told of an earlier answer only while every channel it read is
	// still hers: one that read #deals, which she has left, isn't told.
	t.Run("told only of answers whose channels are still hers", func(t *testing.T) {
		h, q := slackHarness(t, false)
		read := access.Inputs{Sets: []access.SetRef{{SetID: "set-slack", ConnectorID: "conn-slack"}}}
		h.store.history = []*model.FoundryQuestion{
			{Question: "general", LoadID: "load-1", Seen: &read, SeenItems: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}},
				Result: json.RawMessage(answeredResult)},
			{Question: "deals", LoadID: "load-1", Seen: &read, SeenItems: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}, {ConnectorID: "conn-slack", Item: "C2"}},
				Result: json.RawMessage(answeredResult)},
		}
		h.run(t, q)
		req, _ := decodeAsk(t, h.agent.calls()[0])
		if len(req.History) != 1 || req.History[0].Question != "general" {
			t.Fatalf("told %+v", req.History)
		}
	})
	t.Run("audiences never read: nothing of it", func(t *testing.T) {
		h, q := slackHarness(t, true)
		h.store.sharing = nil
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Error != noLongerGranted || len(h.agent.calls()) != 0 {
			t.Fatalf("settled %+v", settled)
		}
	})
	// Taken out of #general while it runs, she no longer reads what it read.
	t.Run("a channel she leaves while it runs", func(t *testing.T) {
		h, q := slackHarness(t, true)
		answerIt := h.agent.ask
		h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			h.store.mu.Lock()
			now := time.Now()
			h.store.sharing["ada@example.com"] = []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &now, Visible: []string{"C1"}}}
			h.store.mu.Unlock()
			return answerIt(req, stream)
		}
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != noLongerGranted {
			t.Fatalf("settled %+v", settled)
		}
	})
}
