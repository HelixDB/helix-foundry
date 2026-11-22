package slack

import (
	"net/url"
	"regexp"
	"strconv"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// citations.go links an Analyst answer's citations to Foundry's site, as the
// Foundry MCP's answer card does (src/mcp-apps/answer-model.ts citationLink,
// over src/web/areas/ontology/ask-model.ts citationHref): with no lookups,
// a quote opens its document's span, a rule break the load's rule breaks
// and a People enrichment row the load; everything else (an entity, whose
// page needs its version's keys, a fact, a derived fact, a table's row, a
// query's result, a recorded version) opens the question's thread, where
// the Analyst shows it.

// QuestionPage is where an Analyst question is on the site: its ontology of
// its workspace, the load it asked about ("" when unknown) and its thread
// ("" when unknown).
type QuestionPage struct {
	WorkspaceID string
	OntologyID  string
	LoadID      string
	ThreadID    string
}

// page is the site's page at path with query (each pair a key and its
// value, in order, empty values left out), then fragment ("" for none).
func (s Site) page(path string, query []string, fragment string) Link {
	sep := "?"
	for i := 0; i+1 < len(query); i += 2 {
		if query[i+1] == "" {
			continue
		}
		path += sep + query[i] + "=" + url.QueryEscape(query[i+1])
		sep = "&"
	}
	if fragment != "" {
		path += "#" + fragment
	}
	return Link{u: s.Origin() + path}
}

// Thread is the question's thread in the Analyst, or the ontology's Analyst
// without a thread.
func (s Site) Thread(p QuestionPage) Link {
	return s.page("/ask/"+url.PathEscape(p.OntologyID), []string{"workspace", p.WorkspaceID, "thread", p.ThreadID}, "")
}

// CitationLink is where a citation opens: its page on the site when it has
// one the question knows (a quote's document span; with the load known, a
// rule break's record and a People enrichment profile's load), else the
// question's thread.
func (s Site) CitationLink(p QuestionPage, t ontologyagent.CitationTarget) Link {
	ontology := "/ontology/" + url.PathEscape(p.OntologyID)
	switch {
	case t.Quote != nil:
		q := t.Quote
		return s.page(ontology+"/documents/"+url.PathEscape(q.DocID), []string{"load", p.LoadID, "workspace", p.WorkspaceID},
			"s"+strconv.FormatUint(uint64(q.Section), 10)+"-"+strconv.FormatUint(q.Start, 10)+"-"+strconv.FormatUint(q.End, 10))
	case t.Violation != nil && p.LoadID != "":
		return s.page(ontology+"/loads/"+url.PathEscape(p.LoadID)+"/rule-breaks",
			[]string{"constraint", t.Violation.Constraint, "key", t.Violation.Key, "workspace", p.WorkspaceID}, "")
	case t.Row != nil && t.Row.Source == ontologyagent.EnrichmentSourceID && p.LoadID != "":
		return s.page(ontology+"/loads/"+url.PathEscape(p.LoadID), []string{"workspace", p.WorkspaceID}, "")
	}
	return s.Thread(p)
}

// markerRE is a citation marker in an answer's text: [c1].
var markerRE = regexp.MustCompile(`\[(c\d+)\]`)

// Cited makes an answer's text safe mrkdwn with its citations as links,
// numbered by first use across the calls that share numbers: [c2] becomes
// <link|[1]> when c2 is the first marker cited. A marker no citation
// resolves is dropped, as the Analyst drops it.
func (s Site) Cited(p QuestionPage, text string, citations map[string]ontologyagent.CitationTarget, numbers map[string]int) Mrkdwn {
	var parts []Mrkdwn
	at := 0
	for _, m := range markerRE.FindAllStringSubmatchIndex(text, -1) {
		parts = append(parts, s.Text(text[at:m[0]]))
		at = m[1]
		marker := text[m[2]:m[3]]
		target, ok := citations[marker]
		if !ok {
			continue
		}
		n, seen := numbers[marker]
		if !seen {
			n = len(numbers) + 1
			numbers[marker] = n
		}
		parts = append(parts, s.Link(s.CitationLink(p, target), "["+strconv.Itoa(n)+"]"))
	}
	return Join(append(parts, s.Text(text[at:]))...)
}
