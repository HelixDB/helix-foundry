package slack_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/slack"
)

// askPage is the question of the MCP answer card's tests
// (tests/unit/mcp-apps.test.ts: ws_1, ont_1, load_1, q_1).
var askPage = slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "ont_1", LoadID: "load_1", ThreadID: "q_1"}

// threadURL is that question's thread, as the MCP's ask_url names it.
const threadURL = "https://foundry.helix-db.com/ask/ont_1?workspace=ws_1&thread=q_1"

func TestCitationLink(t *testing.T) {
	s := site(t)
	acme := ontologyagent.EntityRef{Type: "Customer", ID: "acme"}
	globex := ontologyagent.EntityRef{Type: "Customer", ID: "globex"}
	for _, c := range []struct {
		name   string
		page   slack.QuestionPage
		target ontologyagent.CitationTarget
		want   string
	}{
		// The TS examples (mcp-apps.test.ts): a quote opens its span, an entity and a result the thread.
		{"quote", askPage, ontologyagent.CitationTarget{Quote: &ontologyagent.QuoteTarget{DocID: "doc_1", Start: 4, End: 9}},
			"https://foundry.helix-db.com/ontology/ont_1/documents/doc_1?load=load_1&workspace=ws_1#s0-4-9"},
		{"entity", askPage, ontologyagent.CitationTarget{Entity: &acme}, threadURL},
		{"result", askPage, ontologyagent.CitationTarget{Result: &ontologyagent.ResultTarget{EvidenceID: "e2"}}, threadURL},
		// The rest of citationHref, as the card knows it: no keys, tables or names.
		{"fact", askPage, ontologyagent.CitationTarget{Fact: &ontologyagent.FactTarget{Link: "partner_of", Subject: acme, Object: globex}}, threadURL},
		{"derived", askPage, ontologyagent.CitationTarget{Derived: &ontologyagent.DerivedTarget{Pred: "violation:x", Args: []ontologyagent.EntityRef{acme}}}, threadURL},
		{"row", askPage, ontologyagent.CitationTarget{Row: &ontologyagent.RowTarget{Source: "src_1", Mapping: "m", RowKey: "1"}}, threadURL},
		{"violation", askPage, ontologyagent.CitationTarget{Violation: &ontologyagent.ViolationTarget{Constraint: "one owner", Key: "k&1"}},
			"https://foundry.helix-db.com/ontology/ont_1/loads/load_1/rule-breaks?constraint=one+owner&key=k%261&workspace=ws_1"},
		{"enrichment", askPage, ontologyagent.CitationTarget{Row: &ontologyagent.RowTarget{Source: ontologyagent.EnrichmentSourceID, Mapping: "people"}},
			"https://foundry.helix-db.com/ontology/ont_1/loads/load_1?workspace=ws_1"},
		{"version of a prop", askPage, ontologyagent.CitationTarget{Version: &ontologyagent.VersionTarget{Tx: "t", At: time.Unix(1, 0),
			Of: ontologyagent.VersionOf{Prop: &ontologyagent.PropTarget{Entity: acme, Prop: "tier"}}}}, threadURL},
		{"version of a record", askPage, ontologyagent.CitationTarget{Version: &ontologyagent.VersionTarget{Tx: "t", At: time.Unix(1, 0),
			Of: ontologyagent.VersionOf{Record: &ontologyagent.ViolationTarget{Constraint: "c", Key: "k"}}}}, threadURL},
		// Without the load, what reads one opens the thread, and a quote its document.
		{"violation without a load", slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "ont_1"},
			ontologyagent.CitationTarget{Violation: &ontologyagent.ViolationTarget{Constraint: "c", Key: "k"}},
			"https://foundry.helix-db.com/ask/ont_1?workspace=ws_1"},
		{"enrichment without a load", slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "ont_1", ThreadID: "q_1"},
			ontologyagent.CitationTarget{Row: &ontologyagent.RowTarget{Source: ontologyagent.EnrichmentSourceID, Mapping: "people"}}, threadURL},
		{"quote without a load", slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "o/1"},
			ontologyagent.CitationTarget{Quote: &ontologyagent.QuoteTarget{DocID: "d 1", Section: 2, Start: 0, End: 3}},
			"https://foundry.helix-db.com/ontology/o%2F1/documents/d%201?workspace=ws_1#s2-0-3"},
	} {
		require.Equal(t, c.want, s.CitationLink(c.page, c.target).String(), c.name)
	}
}

func TestCited(t *testing.T) {
	s := site(t)
	acme := ontologyagent.EntityRef{Type: "Customer", ID: "acme"}
	citations := map[string]ontologyagent.CitationTarget{
		"c1": {Entity: &acme},
		"c2": {Quote: &ontologyagent.QuoteTarget{DocID: "doc_1", Start: 4, End: 9}},
	}
	numbers := map[string]int{}
	// Numbered by first use; an unknown marker is dropped; text around is escaped.
	got := s.Cited(askPage, "Globex [c2] signed <b> [c1][c9]. Again [c2].", citations, numbers)
	quote := "https://foundry.helix-db.com/ontology/ont_1/documents/doc_1?load=load_1&amp;workspace=ws_1#s0-4-9"
	thread := "https://foundry.helix-db.com/ask/ont_1?workspace=ws_1&amp;thread=q_1"
	require.Equal(t, "Globex <"+quote+"|[1]> signed &lt;b&gt; <"+thread+"|[2]>. Again <"+quote+"|[1]>.", got.String())
	require.Equal(t, map[string]int{"c2": 1, "c1": 2}, numbers)
	// Numbers carry across calls.
	require.Equal(t, "<"+thread+"|[2]>", s.Cited(askPage, "[c1]", citations, numbers).String())
	require.Equal(t, "No markers", s.Cited(askPage, "No markers", citations, numbers).String())
}

func ExampleSite_CitationLink() {
	s, _ := slack.NewSite("https://foundry.helix-db.com")
	page := slack.QuestionPage{WorkspaceID: "ws_1", OntologyID: "ont_1", LoadID: "load_1", ThreadID: "q_1"}
	fmt.Println(s.CitationLink(page, ontologyagent.CitationTarget{Quote: &ontologyagent.QuoteTarget{DocID: "doc_1", Start: 4, End: 9}}))
	fmt.Println(s.CitationLink(page, ontologyagent.CitationTarget{Entity: &ontologyagent.EntityRef{Type: "Customer", ID: "acme"}}))
	// Output:
	// https://foundry.helix-db.com/ontology/ont_1/documents/doc_1?load=load_1&workspace=ws_1#s0-4-9
	// https://foundry.helix-db.com/ask/ont_1?workspace=ws_1&thread=q_1
}
