package slack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/agent"
)

// cards.go draws an action Founder prepared as a card in the thread, in the
// site's words (src/web/areas/agent/model.ts cardView): a title per
// operation, the facts of its display, which Foundry resolved from the
// workspace (never the model's words), what running it does, and Run and
// Dismiss for its person, a delete's Run asking first. A card whose Run
// approves workflow steps the card can't show runs only in Foundry: it
// links to its thread there instead. Once clicked, the card says how it
// ended in place of its buttons.

// CardState is where a card stands: a closed set.
type CardState interface{ cardState() }

// CardOpen is a card waiting for its person: Run and Dismiss.
type CardOpen struct{}

// CardRan is a card that ran: clicked by By (a Slack user id; "" when not
// clicked in Slack), failed with Error when Failed.
type CardRan struct {
	By     string
	Failed bool
	Error  string
}

// CardExpired is a card whose confirmation ended before it ran: Prepare
// again.
type CardExpired struct{}

// CardDismissed is a card dismissed, by By ("" when not in Slack).
type CardDismissed struct{ By string }

func (CardOpen) cardState()      {}
func (CardRan) cardState()       {}
func (CardExpired) cardState()   {}
func (CardDismissed) cardState() {}

// cardView is a card's words.
type cardView struct {
	title string
	facts []fact
	// quote is the document's words a review was read from: untrusted.
	quote   string
	does    string
	cost    string
	caution string
	// run names the Run button: what it does.
	run string
	// inFoundry is a card that runs only in Foundry: its Run approves
	// workflow steps, which show only there.
	inFoundry bool
}

// fact is one thing a card names: a label and its value.
type fact struct{ label, value string }

// view reads an operation's display as D (numbers kept as written) into
// its card's words.
func view[D any](words func(D) cardView) func(json.RawMessage) (cardView, error) {
	return func(raw json.RawMessage) (cardView, error) {
		var d D
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&d); err != nil {
			return cardView{}, err
		}
		return words(d), nil
	}
}

func nameOf(r agent.Ref) string {
	if r.Name != "" {
		return r.Name
	}
	return r.ID
}

func quoted(r agent.Ref) string {
	if r.Name != "" {
		return "“" + r.Name + "”"
	}
	return r.ID
}

func vN(v agent.VersionRef) string {
	if v.Number > 0 {
		return "v" + strconv.Itoa(v.Number)
	}
	return "a version"
}

func listOf[R any](rs []R, name func(R) string) string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, name(r))
	}
	return strings.Join(names, ", ")
}

func connectorName(c agent.ConnectorRef) string { return nameOf(c.Ref) }

func loadText(l agent.LoadRef) string {
	parts := []string{"A load"}
	if l.Version.Number > 0 {
		parts[0] = "v" + strconv.Itoa(l.Version.Number) + " load"
	}
	if l.Status != "" {
		parts = append(parts, l.Status)
	}
	if l.CreatedAt != nil {
		parts = append(parts, "started "+l.CreatedAt.UTC().Format("2 Jan 2006"))
	}
	return strings.Join(parts, ", ")
}

func dashboardText(d agent.DashboardRef) string {
	parts := []string{nameOf(d.Ref)}
	if d.Pinned {
		parts = append(parts, "pinned for everyone")
	}
	if d.Published {
		parts = append(parts, "at a public link")
	}
	return strings.Join(parts, " · ")
}

func automationText(a agent.AutomationRef) string {
	parts := []string{nameOf(a.Ref)}
	switch a.Status {
	case "active":
		parts = append(parts, "on")
	case "paused":
		parts = append(parts, "paused")
	case "needs_approval":
		parts = append(parts, "waits for approval")
	}
	return strings.Join(parts, " · ")
}

func tilesFact(spec agent.SpecDisplay) fact {
	if len(spec.Tiles) == 0 {
		return fact{"Tiles", "None"}
	}
	return fact{"Tiles", listOf(spec.Tiles, func(t agent.TileDisplay) string {
		title := t.Title
		if title == "" {
			title = "Untitled"
		}
		return title + " (" + nameOf(t.Ontology) + ")"
	})}
}

// runtimes are where Founder runs, as people name them: the setting, where
// a thread runs on it, and its command-line tool.
var runtimes = map[string]struct{ title, on, cli string }{
	"api":         {"Foundry’s AI model", "the workspace’s AI model", ""},
	"claude_code": {"My Claude subscription", "your Claude subscription", "Claude Code"},
	"codex":       {"My ChatGPT subscription (Codex)", "your ChatGPT subscription", "Codex"},
}

func runtimeOf(id string) struct{ title, on, cli string } {
	if r, ok := runtimes[id]; ok {
		return r
	}
	return struct{ title, on, cli string }{id, id, id}
}

// stepsInFoundry says why a workflow card whose Run approves its steps
// runs only in Foundry.
const stepsInFoundry = "Its steps show only in Foundry, so it runs there: review them in this thread in Foundry, then run it."

// sourceModes are how a generation reads an API source.
var sourceModes = map[string]string{"structured": "as rows", "extraction": "as documents"}

// cardViews are each operation's card: one per agent.Operations key.
var cardViews = map[string]func(json.RawMessage) (cardView, error){
	"create_ontology": view(func(d agent.CreateOntologyDisplay) cardView {
		out := cardView{title: "Create the ontology “" + d.Name + "”", facts: []fact{{"Name", d.Name}}, run: "Create",
			does: "Makes an empty ontology on this database, which no other ontology may then use. Nothing is drafted or loaded yet."}
		switch {
		case d.NewDatabase != nil:
			db := d.NewDatabase
			project := db.ProjectID
			if db.NewProject != "" {
				project = "New: " + db.NewProject
			}
			out.facts = append(out.facts, fact{"Database", "New: " + db.Name + " (" + db.Slug + ")"}, fact{"Project", project})
			out.does = "Makes a new Helix database on the shared cluster, then an empty ontology on it. Nothing is drafted or loaded yet."
		case d.Database != nil:
			out.facts = append(out.facts, fact{"Database", nameOf(*d.Database)})
		}
		return out
	}),
	"generate_ontology": view(func(d agent.GenerateDisplay) cardView {
		goal := d.Goal
		if goal == "" {
			goal = "None: the agent decides"
		}
		connectors := listOf(d.Connectors, connectorName)
		switch d.ConnectorsRead {
		case agent.ConnectorsNone:
			connectors = "None: documents only"
		case agent.ConnectorsEvery:
			if connectors == "" {
				connectors = "Every connection (none yet)"
			} else {
				connectors = "Every connection: " + connectors
			}
		}
		sets := listOf(d.DocumentSets, nameOf)
		if sets == "" {
			sets = "None"
		}
		out := cardView{title: "Draft a new version of " + quoted(d.Ontology), run: "Generate",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}, {"Goal", goal}, {"Connections", connectors}, {"Document sets", sets}},
			does:  "The ontology agent drafts a new version from these sources. Nothing changes until you make the draft current."}
		if len(d.EventSources) > 0 {
			out.facts = append(out.facts, fact{"API sources", listOf(d.EventSources, func(e agent.EventSourceDisplay) string {
				mode, ok := sourceModes[e.Mode]
				if !ok {
					mode = "(" + e.Mode + ")"
				}
				return nameOf(e.Connector.Ref) + " " + mode
			})})
		}
		var samples []agent.ConnectorRef
		for _, c := range d.Connectors {
			if c.Sample {
				samples = append(samples, c)
			}
		}
		switch {
		case d.Sample != "":
			out.title = "Draft the sample company’s version of " + quoted(d.Ontology)
			out.does = "Takes the sample company’s ready-made draft as a new version, in seconds, with no AI. Nothing changes until you make it current."
		case len(samples) > 0:
			out.caution = listOf(samples, connectorName) + " is the made-up sample company’s data: this draft would hold its records beside yours."
		}
		return out
	}),
	"set_current_version": view(func(d agent.SetCurrentVersionDisplay) cardView {
		current := "None yet"
		if d.Current != nil {
			current = vN(*d.Current)
		}
		return cardView{title: "Make " + vN(d.Version) + " current for " + quoted(d.Ontology), run: "Make current",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}, {"Version", vN(d.Version)}, {"Current now", current}},
			does:  "New loads write this version from then on. Loads already written stay as they are, and review decisions it doesn’t take are dropped."}
	}),
	"create_load": view(func(d agent.CreateLoadDisplay) cardView {
		return cardView{title: "Load " + vN(d.Version) + " of " + quoted(d.Ontology), run: "Load",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}, {"Version", vN(d.Version)}},
			does:  "Writes this version’s tables and documents into the ontology’s database, beside its other loads. The newest load that finishes ready becomes current.",
			cost:  "Reading its documents spends the workspace’s load budget."}
	}),
	"set_current_load": view(func(d agent.SetCurrentLoadDisplay) cardView {
		load, current := "a load", "None yet"
		if d.Load.Version.Number > 0 {
			load = "the v" + strconv.Itoa(d.Load.Version.Number) + " load"
		}
		if d.Current != nil {
			current = loadText(*d.Current)
		}
		return cardView{title: "Make " + load + " current for " + quoted(d.Ontology), run: "Make current",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}, {"Load", loadText(d.Load)}, {"Current now", current}},
			does:  "The Explorer, Records and Founder read this load from then on. Nothing is written or deleted."}
	}),
	"revise_version": view(func(d agent.ReviseDisplay) cardView {
		edits := listOf(d.Edits, func(e agent.EditDisplay) string {
			if e.Kind == "put" {
				return "Change " + e.ItemID
			}
			return "Remove " + e.ItemID
		})
		out := cardView{title: "Save " + plural(len(d.Edits), "edit") + " to " + vN(d.Version) + " of " + quoted(d.Ontology) + " as a new version",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}, {"Version", vN(d.Version)}, {"Edits", strings.ReplaceAll(edits, ", ", "; ")}},
			does:  "The ontology agent checks these edits and saves them as a new version. The current version stays until you make the new one current.",
			run:   "Save version"}
		if d.DryRun {
			out.title = "Preview " + plural(len(d.Edits), "edit") + " to " + vN(d.Version) + " of " + quoted(d.Ontology)
			out.does, out.run = "The ontology agent checks these edits against the version and its data, and saves nothing.", "Preview"
		}
		return out
	}),
	"sync_connector": view(func(d agent.SyncDisplay) cardView {
		out := cardView{title: "Sync " + quoted(d.Connector.Ref), run: "Sync", facts: []fact{{"Connection", nameOf(d.Connector.Ref)}},
			does: "Reads the connection’s tables from the source again. Loads that read them take the new rows at their next load."}
		if d.Connector.Kind != "" {
			out.facts = append(out.facts, fact{"Kind", d.Connector.Kind})
		}
		return out
	}),
	"add_sample_data": view(func(d agent.OntologyDisplay) cardView {
		return cardView{title: "Add the sample company’s data for " + quoted(d.Ontology), run: "Add sample data",
			facts: []fact{{"Ontology", nameOf(d.Ontology)}},
			does:  "Connects Acme Corp’s sample database and uploads its documents into the set “Acme Corp sample”, for drafting from. The data is made up; nothing of yours changes."}
	}),
	"create_document_set": view(func(d agent.CreateSetDisplay) cardView {
		description := d.Description
		if description == "" {
			description = "None"
		}
		return cardView{title: "Create the document set “" + d.Name + "”", run: "Create set",
			facts: []fact{{"Name", d.Name}, {"Description", description}, {"For", nameOf(d.Ontology)}},
			does:  "Makes an empty set of documents in the workspace. Upload documents into it from Data; the agent reads its description when it drafts."}
	}),
	"update_document_set": view(func(d agent.UpdateSetDisplay) cardView {
		out := cardView{title: "Rename and describe the set " + quoted(d.Set), run: "Save", facts: []fact{{"Set", nameOf(d.Set)}},
			does: "Its documents stay where they are, and ontologies that read the set go on reading it. The agent reads the description when it drafts."}
		switch {
		case d.Name != nil && d.Description == nil:
			out.title = "Rename the set " + quoted(d.Set) + " to “" + *d.Name + "”"
		case d.Name == nil:
			out.title = "Change the description of the set " + quoted(d.Set)
		}
		if d.Name != nil {
			out.facts = append(out.facts, fact{"New name", *d.Name})
		}
		if d.Description != nil {
			description := *d.Description
			if description == "" {
				description = "Cleared"
			}
			out.facts = append(out.facts, fact{"Description", description})
		}
		return out
	}),
	"move_document": view(func(d agent.MoveDisplay) cardView {
		from := "Unknown"
		if d.From.ID != "" {
			from = nameOf(d.From)
		}
		return cardView{title: "Move " + quoted(d.Document) + " to " + quoted(d.To), run: "Move",
			facts: []fact{{"Document", nameOf(d.Document)}, {"From", from}, {"To", nameOf(d.To)}},
			does:  "Ontologies read it by the sets their versions read: those that read the new set take it, and those that read the old one drop it at their next load."}
	}),
	"settle_review": view(settleView),
	"create_dashboard": view(func(d agent.CreateDashboardDisplay) cardView {
		return cardView{title: "Make the dashboard “" + d.Name + "”", run: "Create",
			facts: []fact{{"Name", "“" + d.Name + "” (as Founder wrote it)"}, tilesFact(d.Spec)},
			does:  "Makes a dashboard of these tiles. Everyone in the workspace sees it, and each reads its tiles as themselves."}
	}),
	"update_dashboard": view(func(d agent.UpdateDashboardDisplay) cardView {
		out := cardView{title: "Change the dashboard " + quoted(d.Dashboard.Ref), run: "Save",
			facts: []fact{{"Dashboard", dashboardText(d.Dashboard)}},
			does:  "Changes its name, description or tiles to what the full request holds. Everyone who opens it sees the change."}
		if d.Name != nil {
			out.facts = append(out.facts, fact{"New name", "“" + *d.Name + "”"})
		}
		if d.Description != nil {
			description := "None"
			if *d.Description != "" {
				description = "“" + *d.Description + "”"
			}
			out.facts = append(out.facts, fact{"New description", description})
		}
		if d.Spec != nil {
			out.facts = append(out.facts, tilesFact(*d.Spec))
		}
		return out
	}),
	"delete_dashboard": view(func(d agent.DashboardDisplay) cardView {
		return cardView{title: "Delete the dashboard " + quoted(d.Dashboard.Ref), run: "Delete",
			facts: []fact{{"Dashboard", dashboardText(d.Dashboard)}},
			does:  "Deletes the dashboard for everyone in the workspace, and its public link if it has one (only an owner or admin deletes a published one). The data its tiles read stays."}
	}),
	"pin_dashboard": view(func(d agent.PinDashboardDisplay) cardView {
		verb := "Unpin"
		if d.Pinned {
			verb = "Pin"
		}
		return cardView{title: verb + " the dashboard " + quoted(d.Dashboard.Ref) + " for everyone", run: verb,
			facts: []fact{{"Dashboard", dashboardText(d.Dashboard)}},
			does:  "Pinned dashboards lead every member’s list of dashboards, and are the ones people reading through shared sources see."}
	}),
	"publish_dashboard": view(func(d agent.PublishDashboardDisplay) cardView {
		title, ends := "Publish the dashboard "+quoted(d.Dashboard.Ref)+" at a public link", "Never"
		if d.Dashboard.Published {
			title = "Refresh the public snapshot of " + quoted(d.Dashboard.Ref)
		}
		switch {
		case d.ExpiresInDays == nil && d.Dashboard.Published:
			ends = "As it is set now"
		case d.ExpiresInDays != nil && *d.ExpiresInDays > 0:
			ends = plural(*d.ExpiresInDays, "day") + " after publishing"
		}
		return cardView{title: title, run: "Publish", facts: []fact{{"Dashboard", dashboardText(d.Dashboard)}, {"Link ends", ends}},
			does:    "Anyone with the link sees a snapshot of what its tiles read now, as you can read it, without signing in, until it is published again. Nothing else of the workspace shows. The link is on the dashboard’s page, never given to Founder.",
			caution: "Anyone who has the link sees this data."}
	}),
	"unpublish_dashboard": view(func(d agent.DashboardDisplay) cardView {
		return cardView{title: "Unpublish the dashboard " + quoted(d.Dashboard.Ref), run: "Unpublish",
			facts: []fact{{"Dashboard", dashboardText(d.Dashboard)}},
			does:  "Its public link stops showing anything at once, for everyone who has it."}
	}),
	"create_workflow": view(func(d agent.CreateAutomationDisplay) cardView {
		return cardView{title: "Set up the workflow “" + d.Name + "”", run: "Create and turn on",
			facts:   []fact{{"Name", "“" + d.Name + "” (as Founder wrote it)"}},
			does:    "Saves this workflow and turns it on, and the click that runs it approves its steps. You can pause or change it on its page.",
			cost:    "Its Founder steps spend the workspace’s workflows budget.",
			caution: stepsInFoundry, inFoundry: true}
	}),
	"update_workflow": view(func(d agent.UpdateAutomationDisplay) cardView {
		out := cardView{title: "Change the workflow " + quoted(d.Automation.Ref), run: "Save",
			facts: []fact{{"Workflow", automationText(d.Automation)}},
			does:  "Saves these changes to the workflow. Its steps don’t change, so this approves nothing."}
		if d.Name != nil {
			out.facts = append(out.facts, fact{"New name", "“" + *d.Name + "”"})
		}
		if d.Steps {
			out.does, out.caution, out.inFoundry = "Saves these changes to the workflow, and the click that runs it approves its steps.", stepsInFoundry, true
		}
		return out
	}),
	"pause_workflow": view(func(d agent.AutomationDisplay) cardView {
		return cardView{title: "Pause the workflow " + quoted(d.Automation.Ref), run: "Pause",
			facts: []fact{{"Workflow", automationText(d.Automation)}},
			does:  "It stops running when it would start. A run already going finishes."}
	}),
	"resume_workflow": view(func(d agent.AutomationDisplay) cardView {
		return cardView{title: "Turn the workflow " + quoted(d.Automation.Ref) + " back on", run: "Turn on",
			facts: []fact{{"Workflow", automationText(d.Automation)}},
			does:  "It runs again each time it starts, from now on."}
	}),
	"delete_workflow": view(func(d agent.AutomationDisplay) cardView {
		return cardView{title: "Delete the workflow " + quoted(d.Automation.Ref), run: "Delete",
			facts: []fact{{"Workflow", automationText(d.Automation)}},
			does:  "Deletes the workflow and its runs. Nothing it already did is undone."}
	}),
	"run_workflow": view(func(d agent.AutomationDisplay) cardView {
		return cardView{title: "Run the workflow " + quoted(d.Automation.Ref) + " now", run: "Run now",
			facts: []fact{{"Workflow", automationText(d.Automation)}},
			does:  "Runs its approved steps once, now, as you."}
	}),
	"set_assistant_runtime": view(func(d agent.AssistantRuntimeDisplay) cardView {
		to := runtimeOf(d.Runtime)
		out := cardView{title: "Run new Founder threads on " + to.on, run: "Switch",
			facts: []fact{{"Now", runtimeOf(d.Current).title}, {"From then on", to.title}},
			does:  "New threads run on the workspace’s AI model and count toward its Founder budget. Threads already started keep where they run."}
		if d.Runtime != "api" {
			helper := "Not running"
			if d.Online {
				helper = "Running"
			}
			out.facts = append(out.facts, fact{"Helper", helper})
			out.does = "New threads run on " + to.cli + " through the helper on your computer, billed to the account it’s signed in to instead of the workspace’s Founder budget. Threads already started keep where they run."
			if !d.Online {
				out.caution = "No helper running " + to.cli + " is online: start it on your computer before you send."
			}
		}
		return out
	}),
	"revoke_assistant_helper": view(func(d agent.AssistantHelperDisplay) cardView {
		runs := listOf(d.Runtimes, func(r string) string { return runtimeOf(r).cli })
		if runs == "" {
			runs = "Nothing yet"
		}
		status := "Not running"
		if d.Online {
			status = "Running"
		}
		return cardView{title: "Revoke the helper " + quoted(d.Host), run: "Revoke",
			facts: []fact{{"Helper", nameOf(d.Host)}, {"Runs", runs}, {"Status", status}},
			does:  "It stops running Founder’s turns at once, and a turn it is running ends. To run more, pair it again from Settings › AI models."}
	}),
}

// settleView is settle_review's card: the decision in words, the review's
// document, the entity it is about, and its quote.
func settleView(d agent.SettleDisplay) cardView {
	r := d.Review
	name := r.NormName
	if name == "" {
		name = r.Quote
	}
	if name == "" {
		name = "this name"
	}
	// named is the entity a stored identity names, by the name the load gave it.
	named := func(key string) string {
		for _, e := range r.Entities {
			if e.Type == r.Type && (e.EntityKey == key || (e.EntityKey == "" && e.NormName == key)) && e.Name != "" {
				return e.Name
			}
		}
		return key
	}
	what := "the value"
	if r.Type != "" && r.Prop != "" {
		owner := r.Type
		if r.EntityKey != "" {
			owner = named(r.EntityKey)
		}
		what = owner + "’s " + r.Prop
	}
	out := cardView{facts: []fact{{"Ontology", nameOf(d.Ontology)}}, quote: r.Quote}
	if d.Document != nil {
		out.facts = append(out.facts, fact{"Document", nameOf(*d.Document)})
	}
	if statement := statementText(r.Statement, r.Link); statement != "" {
		out.facts = append(out.facts, fact{"Statement", statement})
	}
	if r.EntityKey != "" {
		out.facts = append(out.facts, fact{"Entity", named(r.EntityKey)})
	}
	switch d.Decision.Kind {
	case agent.DecisionLink:
		linked := named(d.Decision.EntityKey)
		out.title, out.run = "Link “"+name+"” to "+linked, "Link"
		out.does = "The name is this entity in every load from the next one."
		out.facts = append(out.facts, fact{"Links to", linked})
		if !d.Decision.Candidate {
			out.caution = "This entity isn’t one the review suggested."
		}
	case agent.DecisionNotInData:
		out.title, out.run = "Mark “"+name+"” as not in the data", "Mark not in data"
		out.does = "The name is no one in the data: later loads leave it unlinked."
	case agent.DecisionSetValue:
		out.title, out.run = "Set "+what+" to "+valueText(d.Decision.Value), "Set value"
		out.does = "Replaces the property’s value from the next load on, whatever documents or tables say."
		out.caution = "The decision holds in every later load, even after the tables change."
	case agent.DecisionRemoveValue:
		out.title, out.run = "Remove "+what, "Remove value"
		out.does = "Leaves the property without a value from the next load on."
	case agent.DecisionApprove:
		out.title, out.run = "Approve the held statement", "Approve"
		out.does = "Writes the statement into the current load now."
	default:
		out.title, out.run = "Reject the held statement", "Reject"
		out.does = "Writes nothing, and the review no longer shows."
	}
	if len(r.Candidates) > 0 {
		out.facts = append(out.facts, fact{"Suggested", listOf(r.Candidates, named)})
	}
	return out
}

// valueText is a decided value as a card says it: text quoted, numbers and
// yes/no as they are.
func valueText(v map[string]any) string {
	if text, ok := v["text"].(string); ok {
		return "“" + text + "”"
	}
	for _, key := range []string{"integer", "number", "boolean"} {
		if x, ok := v[key]; ok {
			return fmt.Sprint(x)
		}
	}
	return "a value"
}

// statementText is a held statement as words, "Contract “MSA” signed_with
// Customer “Globex”", from its JSON and its link; "" when it names neither
// end.
func statementText(raw, link string) string {
	var s struct {
		Subject, Object *struct{ Type, Name string }
	}
	if json.Unmarshal([]byte(raw), &s) != nil {
		return ""
	}
	end := func(e *struct{ Type, Name string }) string {
		switch {
		case e == nil || e.Type == "":
			return ""
		case e.Name == "":
			return e.Type
		}
		return e.Type + " “" + e.Name + "”"
	}
	subject, object := end(s.Subject), end(s.Object)
	if subject == "" && object == "" {
		return ""
	}
	var parts []string
	for _, p := range []string{subject, link, object} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// cutPlain is plain text cut to max characters, ending "…" when cut.
func cutPlain(text string, max int) Plain {
	if utf8.RuneCountInString(text) <= max {
		return Plain(text)
	}
	return Plain(string([]rune(text)[:max-1]) + "…")
}

// Chat is a Founder thread of a workspace, which the site shows on its Ask
// page (/ask?workspace=…&chat=…).
type Chat struct{ WorkspaceID, ThreadID string }

// Chat is the page of a Founder thread.
func (s Site) Chat(c Chat) Link {
	return s.page("/ask", []string{"workspace", c.WorkspaceID, "chat", c.ThreadID}, "")
}

// cardViewOf is c's card's words; it refuses an operation with no card and
// a display that isn't its operation's.
func cardViewOf(c agent.ActionCard) (cardView, error) {
	words, ok := cardViews[c.Operation]
	if !ok {
		return cardView{}, fmt.Errorf("slack: no card for %q", c.Operation)
	}
	v, err := words(c.Display)
	if err != nil {
		return cardView{}, fmt.Errorf("slack: read the %s card: %w", c.Operation, err)
	}
	return v, nil
}

// RunsInFoundry reports whether c runs only in Foundry, so Slack never runs
// it: a workflow card whose Run approves steps the Slack card can't show
// (create_workflow, update_workflow with steps), or a card Card can't draw.
func RunsInFoundry(c agent.ActionCard) bool {
	v, err := cardViewOf(c)
	return err != nil || v.inFoundry
}

// Card draws an action's card for its person (a Slack user id) as it
// stands: its title, its facts, the document's words a review was read
// from, what running it does, its cost and caution; then Run and Dismiss
// and who may run it while open (a delete's Run red, asking first, its
// confirmation cut to Block Kit's limits), Prepare again once expired, or
// how it ended. A card that runs only in Foundry (RunsInFoundry) offers
// Review in Foundry, its thread's page (chat), in place of Run. It refuses
// an operation with no card and a display that isn't its operation's.
func (s Site) Card(c agent.ActionCard, chat Chat, person string, state CardState) (Message, error) {
	v, err := cardViewOf(c)
	if err != nil {
		return Message{}, err
	}
	blocks := []Block{Header{Text: cutPlain(v.title, MaxHeaderText)}}
	// No card has more facts than a section has fields (MaxSectionFields).
	fields := make([]Mrkdwn, 0, len(v.facts))
	for _, f := range v.facts {
		fields = append(fields, Cut(Join(Bold(s.Text(f.label)), Mrkdwn{s: "\n"}, s.Text(f.value)), MaxFieldText))
	}
	blocks = append(blocks, Section{Fields: fields})
	if quote := strings.TrimSpace(v.quote); quote != "" {
		blocks = append(blocks, Section{Text: Cut(Join(Bold(s.Text("Quote")), Mrkdwn{s: "\n"}, Italic(s.Text(quote))), MaxSectionText)})
	}
	blocks = append(blocks, Section{Text: s.Text(v.does)})
	var notes []Mrkdwn
	for _, note := range []string{v.cost, v.caution} {
		if note != "" {
			notes = append(notes, s.Text(note))
		}
	}
	if len(notes) > 0 {
		blocks = append(blocks, Context{Elements: notes})
	}
	danger := strings.HasPrefix(c.Operation, "delete_")
	switch state := state.(type) {
	case CardRan:
		ran := Join(s.Text("Ran"), by(s, state.By), s.Text(": succeeded."))
		if state.Failed {
			why := strings.TrimSpace(state.Error)
			if why == "" {
				why = "it failed"
			}
			ran = Join(s.Text("Ran"), by(s, state.By), s.Text(": failed: "+why))
		}
		blocks = append(blocks, Context{Elements: []Mrkdwn{Cut(ran, MaxSectionText)}})
	case CardDismissed:
		blocks = append(blocks, Context{Elements: []Mrkdwn{Join(s.Text("Dismissed"), by(s, state.By), s.Text("."))}})
	case CardExpired:
		blocks = append(blocks,
			Context{Elements: []Mrkdwn{s.Text("Expired: it wasn’t run in time. Prepare it again to run it.")}},
			Actions{Elements: []Button{{ActionID: ActionRefresh, Text: "Prepare again", Value: ButtonValue{Action: c.ActionID}}}})
	case CardOpen:
		run := Button{ActionID: ActionApprove, Text: cutPlain(v.run, MaxButtonText), Value: ButtonValue{Action: c.ActionID, PayloadHash: c.PayloadHash},
			Style: StylePrimary}
		who := Join(s.Text("Only "), UserMention(person), s.Text(" can run this."))
		switch {
		case v.inFoundry:
			page := s.Chat(chat)
			run = Button{ActionID: ActionOpen, Text: "Review in Foundry", URL: &page, Style: StylePrimary}
			who = Join(s.Text("Only "), UserMention(person), s.Text(" can run this, in Foundry."))
		case danger:
			run.Style = StyleDanger
			run.Confirm = &Confirm{Title: "Run this?", Text: Cut(s.Text(v.title+"? "+v.does), MaxConfirmText), Confirm: cutPlain(v.run, MaxConfirmButton),
				Deny: "Cancel", Style: StyleDanger}
		}
		blocks = append(blocks, Actions{Elements: []Button{run, {ActionID: ActionDismiss, Text: "Dismiss", Value: ButtonValue{Action: c.ActionID}}}},
			Context{Elements: []Mrkdwn{who}})
	default:
		return Message{}, fmt.Errorf("slack: a card in state %T", state)
	}
	return Message{Text: Cut(s.Text(v.title), MaxMessageText), Blocks: blocks}, nil
}

// by is " by" and a Slack user's mention, or nothing for "".
func by(s Site, userID string) Mrkdwn {
	if userID == "" {
		return Mrkdwn{}
	}
	return Join(s.Text(" by "), UserMention(userID))
}
