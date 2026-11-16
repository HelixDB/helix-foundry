package suggest

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/sample"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// MaxSetup is the most of each of Setup's lists.
const MaxSetup = 5

// Setup is what an ontology's current version doesn't read or hold yet,
// from which Fill makes changes to the ontology without a model (source
// SourceSetup), each list in the order Fill offers it.
type Setup struct {
	// Links are relationships the ontology's documents state that no link
	// of the version covers: its open audit findings of kind "unmatched",
	// most claims first.
	Links []MissingLink
	// Connections are the names of the workspace's connections the
	// version's generation didn't read.
	Connections []string
	// Sets are the names of the workspace's document sets holding documents
	// that the version doesn't read, a connection's named with it ("Granola
	// / Notes").
	Sets []string
}

// MissingLink is a relationship documents state that no link covers.
type MissingLink struct {
	// Words are the relationship as people say it: "supplier of".
	Words string
	// Claims are the reviews behind it: how many times documents state it.
	Claims int64
}

// SetupOf is the Setup of an ontology's current version, at most MaxSetup
// of each list, from:
//   - findings, the ontology's audit findings: the open ones of kind
//     "unmatched", most support first, each read as its Finding's
//     signal.canonical with underscores as spaces (a finding without one is
//     skipped, as is a relationship already listed);
//   - connectors, the workspace's connections, of which those whose ids
//     read (the version's generation's input tables' connectors) doesn't
//     hold, in the order given;
//   - sets, the workspace's live document sets, of which those holding
//     documents that scope (the version's document sets) doesn't hold, in
//     the order given.
//
// API sources and their sets are left out (a generation reads one apart
// from its tables and scope), as are the sample company's connection and
// set (a PostgreSQL connection to its database as its user, and the set of
// its name that no connection owns): they are for the sample's own
// ontology, whose generation reads them.
func SetupOf(findings []*model.FoundryHealFinding, connectors []*model.Connector, read []string, sets []*model.FoundryDocumentSet, scope []string) Setup {
	out := Setup{Links: []MissingLink{}, Connections: []string{}, Sets: []string{}}
	open := slices.DeleteFunc(slices.Clone(findings), func(f *model.FoundryHealFinding) bool {
		return f.Status != model.FoundryHealOpen || f.Kind != "unmatched"
	})
	slices.SortStableFunc(open, func(a, b *model.FoundryHealFinding) int { return cmp.Compare(b.Support, a.Support) })
	for _, f := range open {
		var finding struct {
			Signal struct {
				Canonical string `json:"canonical"`
			} `json:"signal"`
		}
		_ = json.Unmarshal(f.Finding, &finding) // a finding that doesn't read names nothing
		words := collapse(strings.ReplaceAll(finding.Signal.Canonical, "_", " "))
		if len(out.Links) == MaxSetup || words == "" || slices.ContainsFunc(out.Links, func(l MissingLink) bool { return l.Words == words }) {
			continue
		}
		out.Links = append(out.Links, MissingLink{Words: words, Claims: f.Support})
	}
	for _, c := range connectors {
		if len(out.Connections) == MaxSetup || slices.Contains(read, c.ID) || c.Kind == model.ConnectorKindEvents || isSample(c) {
			continue
		}
		out.Connections = append(out.Connections, c.Name)
	}
	for _, set := range sets {
		switch {
		case len(out.Sets) == MaxSetup, set.Documents == 0, slices.Contains(scope, set.ID):
		case set.Owner != nil && set.Owner.ConnectorKind == model.ConnectorKindEvents:
		case set.Owner == nil && set.Name == sample.SetName:
		default:
			out.Sets = append(out.Sets, set.DisplayName())
		}
	}
	return out
}

// isSample reports whether c reads the sample company's database: a
// PostgreSQL connection to its database as its user, wherever it is served.
func isSample(c *model.Connector) bool {
	var db struct{ Database, User string }
	return c.Kind == "postgres" && json.Unmarshal(c.Config, &db) == nil && db.Database == sample.DatabaseName && db.User == sample.DatabaseUser
}

// Fill is the suggestions for an ontology's current version: each kind's
// Count, kinds in Kinds' order, workflows only when workflows is true (the
// deployment has them). Each kind is filled in this order, from:
//   - questions: the version's competency questions (SourceQuestions, each
//     titled by Title), then written's, then fixed examples;
//   - dashboards: written's, then a dashboard that answers each competency
//     question, from the third on and then the first two (so they differ
//     from the questions'), then fixed examples;
//   - changes to the ontology: setup's (SourceSetup) missing links, then
//     connections, then document sets, each as a draft of a new version
//     that holds or reads it, then written's, then fixed examples;
//   - workflows: written's, then Fallback's, then fixed examples (4).
//
// written are the model's suggestions (SourceModel), of any kind. Every
// suggestion passes Suggestion.Clean, or is skipped, as is one whose prompt
// another of its kind has, case aside. The ontology's name is said as
// written, at most 100 characters, or "this ontology" where it can't be.
func Fill(ontologyName string, questions []string, written []Suggestion, setup Setup, workflows bool) []Suggestion {
	name := named(ontologyName)
	asked, answered := []Suggestion{}, []Suggestion{}
	for _, q := range questions {
		q = collapse(q)
		asked = append(asked, Suggestion{Kind: Question, Title: Title(q), Prompt: q, Topic: Question.topic(), Source: SourceQuestions})
	}
	for _, q := range slices.Concat(questions[min(2, len(questions)):], questions[:min(2, len(questions))]) {
		q = collapse(q)
		answered = append(answered, Suggestion{Kind: Dashboard, Title: Title(q), Prompt: "Build me a dashboard that answers: " + q,
			Topic: Dashboard.topic(), Source: SourceQuestions})
	}
	changes := []Suggestion{}
	for _, l := range setup.Links {
		changes = append(changes, Suggestion{Kind: Ontology, Title: titled("Add the “"+l.Words+"” link", "Add a missing link"),
			Prompt: "Draft a new version of " + name + " with a link for “" + l.Words + "”, which " + claims(l.Claims) + ".",
			Topic:  "Missing link", Source: SourceSetup})
	}
	for _, c := range setup.Connections {
		changes = append(changes, Suggestion{Kind: Ontology, Title: titled("Add "+collapse(c), "Add a connection"),
			Prompt: "Draft a new version of " + name + " that also reads " + collapse(c) + ".", Topic: "New source", Source: SourceSetup})
	}
	for _, set := range setup.Sets {
		changes = append(changes, Suggestion{Kind: Ontology, Title: titled("Add "+collapse(set), "Add a document set"),
			Prompt: "Draft a new version of " + name + " that also reads the documents in " + collapse(set) + ".", Topic: "New documents",
			Source: SourceSetup})
	}
	byKind := map[Kind][]Suggestion{}
	for _, w := range written {
		w.Source = SourceModel
		byKind[w.Kind] = append(byKind[w.Kind], w)
	}
	return pick(map[Kind][]Suggestion{
		Question:  slices.Concat(asked, byKind[Question], fixed(Question, name)),
		Dashboard: slices.Concat(byKind[Dashboard], answered, fixed(Dashboard, name)),
		Ontology:  slices.Concat(changes, byKind[Ontology], fixed(Ontology, name)),
		Workflow:  slices.Concat(byKind[Workflow], Fallback(ontologyName, questions), fixed(Workflow, name)),
	}, workflows)
}

// Starter is the suggestions for an ontology without a current version:
// each kind's fixed examples (SourceFixed), the changes to the ontology
// helping to connect data or upload documents and draft its first
// version. Workflows only when workflows is true.
func Starter(ontologyName string, workflows bool) []Suggestion {
	name := named(ontologyName)
	first := []Suggestion{
		{Kind: Ontology, Title: "Connect your data", Prompt: "Help me connect my data and draft the first version of " + name + "."},
		{Kind: Ontology, Title: "Start from documents", Prompt: "Help me upload documents and draft the first version of " + name + " from them."},
		{Kind: Ontology, Title: "Draft from connected data", Prompt: "Draft the first version of " + name + " from the data already connected."},
	}
	return pick(map[Kind][]Suggestion{
		Question:  fixed(Question, name),
		Dashboard: fixed(Dashboard, name),
		Ontology:  fixedOf(first),
		Workflow:  fixed(Workflow, name),
	}, workflows)
}

// fixed is a kind's fixed examples, named for the ontology (name, as
// named says it), more than its Count, so a repeated prompt still leaves
// enough.
func fixed(kind Kind, name string) []Suggestion {
	switch kind {
	case Question:
		return fixedOf([]Suggestion{
			{Kind: Question, Title: "What it holds", Prompt: "What does " + name + " hold? List its types and how many of each it has."},
			{Kind: Question, Title: "What changed recently", Prompt: "What was added to " + name + " most recently, and where did it come from?"},
			{Kind: Question, Title: "Missing details", Prompt: "Which records in " + name + " are missing important details?"},
		})
	case Dashboard:
		return fixedOf([]Suggestion{
			{Kind: Dashboard, Title: "Overview", Prompt: "Build me a dashboard of " + name + " at a glance: how many of each type it holds and what was added most recently."},
			{Kind: Dashboard, Title: "Data quality", Prompt: "Build me a dashboard of the data quality of " + name + ": the records missing key details and the open reviews."},
			{Kind: Dashboard, Title: "Activity by month", Prompt: "Build me a dashboard of the activity in " + name + " by month."},
		})
	case Ontology:
		return fixedOf([]Suggestion{
			{Kind: Ontology, Title: "Add missing details", Prompt: "Draft a new version of " + name + " that adds the properties people ask about most but it doesn't hold yet."},
			{Kind: Ontology, Title: "Link related records", Prompt: "Draft a new version of " + name + " with links between the types that are related but not linked yet."},
			{Kind: Ontology, Title: "Flag what breaks a standard", Prompt: "Draft a new version of " + name + " with rules that flag records breaking the business's standards."},
		})
	}
	return fixedOf([]Suggestion{
		{Kind: Workflow, Title: "Monday brief", Icon: Briefing, Prompt: "Every Monday at 9am, summarise what changed in " + name + " last week and email me."},
		{Kind: Workflow, Title: "Fresh data daily", Icon: Refresh, Prompt: "Every day at 6am, sync my connections, then load " + name + "."},
		{Kind: Workflow, Title: "Load check", Icon: Check, Prompt: "When a load of " + name + " is ready, check for records missing key details and email me any it finds."},
		{Kind: Workflow, Title: "New documents digest", Icon: Report, Prompt: "When documents arrive in a document set " + name + " reads, summarise what they add and email me."},
		{Kind: Workflow, Title: "Rule breaks alert", Icon: Alert, Prompt: "When a load of " + name + " is ready, email me if it holds more open rule breaks than the last one."},
	})
}

// fixedOf is examples as fixed ones: SourceFixed, their kind's own topic.
func fixedOf(examples []Suggestion) []Suggestion {
	for i := range examples {
		examples[i].Topic, examples[i].Source = examples[i].Kind.topic(), SourceFixed
	}
	return examples
}

// pick takes each kind's first Count candidates that pass
// Suggestion.Clean, with a prompt no other of the kind has, kinds in Kinds'
// order, leaving workflows out unless workflows is true.
func pick(candidates map[Kind][]Suggestion, workflows bool) []Suggestion {
	out := []Suggestion{}
	for _, kind := range Kinds {
		if kind == Workflow && !workflows {
			continue
		}
		seen := map[string]bool{}
		for _, c := range candidates[kind] {
			if len(seen) == kind.Count() {
				break
			}
			s, err := c.Clean()
			if err != nil || s.Kind != kind || seen[strings.ToLower(s.Prompt)] {
				continue
			}
			seen[strings.ToLower(s.Prompt)] = true
			out = append(out, s)
		}
	}
	return out
}

// claims is "N claims in your documents state", in the singular for one.
func claims(n int64) string {
	if n == 1 {
		return "1 claim in your documents states"
	}
	return fmt.Sprintf("%d claims in your documents state", n)
}

// titled is title when it fits MaxTitle, else short.
func titled(title, short string) string {
	if utf8.RuneCountInString(title) <= MaxTitle {
		return title
	}
	return short
}
