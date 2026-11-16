package integration

import (
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// What helix_foundry_prepare_operation's fields say of each operation, one line each. An API
// source's parts are said only where the host has them (Service.Events): an agent reading them
// elsewhere would be sent to an operation that is refused. Those are Helix's hosts, the only ones
// with a shared cluster and its subscription, so a new database's words for them go with them.
type foundryOperationDoc struct {
	name string
	// eventsOnly is an operation only a host with API sources offers; events replaces doc on
	// such a host when set.
	eventsOnly bool
	// hostsOnly is an operation only a host with the Assistant's helpers offers (Service.AgentHosts).
	hostsOnly bool
	// publicOnly is an operation only a host with dashboards' public links offers
	// (Service.PublicDashboards).
	publicOnly bool
	// automationsOnly is a workflow operation, which a host with workflows off doesn't offer
	// (Service.AutomationsOff).
	automationsOnly bool
	doc, events     string
}

// memberOperations are the operations of a user who manages nothing of the workspace (no
// connectors or ontologies): making dashboards and workflows and changing and deleting their
// own, as the site lets every member. Foundry refuses what isn't theirs when it runs.
var memberOperations = map[string]bool{"create_dashboard": true, "update_dashboard": true, "delete_dashboard": true,
	"create_workflow": true, "update_workflow": true, "pause_workflow": true, "resume_workflow": true, "delete_workflow": true,
	"run_workflow": true}

// memberOperation allows memberOperations alone: a user's who manages nothing.
func memberOperation(op string) bool { return memberOperations[op] }

var foundryOperationDocs = []foundryOperationDoc{
	{name: "create_ontology",
		doc: "{name} on tenant:<id>. On workspace:<id>, {name, database: {name, slug, project_id or project_name}} first makes a database in that project, or in a new project of that name. " +
			"Names are 1 to 120 characters; a slug is lowercase letters, digits and single dashes, at most 63, given a short random suffix if another database has it. The database's key is never returned.",
		events: "{name} on tenant:<id>. On workspace:<id>, {name, database: {name, slug, project_id or project_name}} first makes a database on the shared cluster in that project, or in a new project of that name, and needs a Helix subscription. " +
			"Names are 1 to 120 characters; a slug is lowercase letters, digits and single dashes, at most 63, given a short random suffix if another database has it. The database's key is never returned."},
	{name: "create_event_source", eventsOnly: true,
		doc: "{name, description, text_field}: an API source (connector kind events) that people and agents send JSON pieces to with " + toolFoundrySendPieces + ". " +
			"name is 1 to 120 characters, unique among the workspace's API sources; description, at most 1000 characters, says what each piece is; " +
			"text_field (optional, 1 to 64 bytes, not id, received_at or occurred_at) is the top-level field holding each piece's text, locked as a string, for entity extraction to read. " +
			"Returns the source, its token_url (where a person makes a send token: no tool returns one) and the send tool."},
	{name: "add_sample_data",
		doc: "{}: adds the made-up Acme Corp sample company to the workspace, as Foundry's own sample does: a read-only PostgreSQL connector \"Acme Corp\" to its six tables, whose sync it queues (its login never passes through you), " +
			"and its 9 documents in the document set \"Acme Corp sample\"; what the workspace already has of it is kept. Name the ontology \"Acme Corp customers\" for Foundry to show it as the sample, then generate it with sample \"acme\". " +
			"The draft calls no model, but loads read the documents with the workspace's model, so adding them is refused (FOUNDRY_AI_NOT_CONFIGURED) on a local install with no AI provider."},
	{name: "sync_connector", doc: "{}.", events: "{}. An API source syncs when pieces arrive, so it needs none."},
	{name: "generate_ontology",
		doc: "{goal, connector_ids, document_set_ids, no_connectors, sample}, each optional. " + generateReads,
		events: "{goal, connector_ids, document_set_ids, no_connectors, event_sources, sample}, each optional. " + generateReads +
			" event_sources are the API sources to read, each {connector_id, mode}: connector_ids never names one, and no connector_ids reads none. " +
			"Mode structured reads its table through mappings, and its pieces reach the graph when the ontology refreshes; it needs pieces synced into its table first. " +
			"Mode extraction reads the source's newest 500 pieces as Markdown documents of its own set (one of the 20 sets; document_set_ids never names it), extracted as they arrive, about 1 cent a piece for each ontology that reads it; " +
			"work no one started spends at most $5 a month on a source, within a share for all the workspace's API sources, and past that new pieces wait for next month or a load a person starts. " +
			"It needs the source's text_field and a piece synced as a document (list_connectors' extraction.documents above 0)."},
	{name: "answer_generation_questions",
		doc: "{generation_id, round, answers, use_defaults}: answers the round of questions a generation that needs_input waits on (helix_foundry_get_generation lists them, " +
			"with its question_round, which round names) and carries it on from where the agent asked. " +
			"answers are {question_id, options, text}, each question once: options are the labels of the options the user chose, as the question lists them, and text their own words (1 to 2000 characters). " +
			"A question without options takes text alone; one of one option (multiple false) one option or text, not both; one with multiple one or more options, with text or without, or text alone. " +
			"Ask the user, never answer for them: show each question with its options, the recommended one marked, and let them answer in their own words too. " +
			"Every blocking question needs an answer unless use_defaults, which answers each question left out with its recommended answer (or leaves it to the agent); " +
			"questions no one answers take those at the generation's answers_due_at."},
	{name: "set_current_version", doc: "{version_id, expected_current_version_id}: the current version as you read it, empty when there is none."},
	{name: "create_load", doc: "{version_id}."},
	{name: "set_current_load", doc: "{load_id}."},
	{name: "revise_version", doc: "{version_id, edits, dry_run}: 1 to 100 edits, each {put: {item_id, toml}} with the item's whole TOML (such as item rule.overdue) or {remove: {item_id}}; dry_run previews without storing a version."},
	{name: "upload_document",
		doc: "{set_id, name, size_bytes, sha256}: the file's name, ending .md, .markdown, .mdx, .txt, .json, .csv, .tsv, .jsonl, .ndjson, .parquet or .pdf; its exact size " +
			"(at most 1 MiB for Markdown, MDX and text and 3 MiB for the rest, unless the stamp says otherwise); and its lowercase hex SHA-256. " +
			"Then PUT the bytes to the upload URL it returns and call " + toolFoundryCompleteUpload + ". " +
			"Ontologies read Markdown, MDX and text as text; a PDF as its text once Helix has read it (at most 150 pages; an encrypted or damaged PDF, or a scan with no text layer, stays stored unread); " +
			"and CSV, TSV, JSON, JSONL and Parquet each as a table once read. The document's read says how and when."},
	{name: "create_document_set", doc: "{name, description}."},
	{name: "update_document_set", doc: "{set_id, name, description}, name and description each optional."},
	{name: "move_document", doc: "{document_id, set_id}."},
	{name: "set_assistant_runtime", hostsOnly: true,
		doc: "{runtime}: where the workspace's new Founder threads run: api (Foundry's own worker, on the workspace's model) " +
			"claude_code (a helper on the owner's computer, on the Claude subscription its Claude Code is signed in to) " +
			"or codex (a helper's Codex, on the owner's ChatGPT plan, with their own Codex setup: only once the owner confirmed it in Settings › AI models, never here); " +
			toolFoundryGetAssistantRuntime + " says which helpers run. A thread keeps the runtime it started with."},
	{name: "pair_assistant_helper", hostsOnly: true,
		doc: "{}: a one-time code, valid for ten minutes, that pairs a helper on the owner's computer with this install, and the command that pairs it. Its result holds the code: say it only to the person."},
	{name: "revoke_assistant_helper", hostsOnly: true,
		doc: "{host_id}: unpairs a helper (" + toolFoundryGetAssistantRuntime + "'s hosts): it and the turns it runs stop at once."},
	{name: "settle_review",
		doc: "{review_id, decision}: the review_key of a review the current load holds (" + toolFoundryListReviews + "), and the decision its settle names: " +
			"resolve {resolve: {entity_key}} with one of its candidates or {resolve: {not_in_data: true}}; " +
			"prop {prop: {text|integer|number|boolean: value}} (an integer as a string of digits) or {prop: {null: true}} to remove the property; " +
			"release {release: {statement_hash}} to approve the statement it holds back or {reject: {statement_hash}} to say it is wrong, with its statement_hash. " +
			"A resolve, a release and a prop apply to the current load at once (" + toolFoundryListReviewDecisions + "'s applied says what became of each): " +
			"a resolve that chooses an entity also links that name to it in every document of that load where the load would otherwise ask, " +
			"and not_in_data applies to the review's document only; a resolve of a name the load already links another way applies from the next load. A reject is sent to no load."},
	{name: "create_dashboard",
		doc: "{name, description, spec}: a dashboard of the workspace, unpinned, which every member sees and reads as themselves; " +
			"a name of 1 to 120 characters, a description of at most 1000, and a spec (none for no tiles yet). Preview each tile with " + toolFoundryPreviewDashboardTile + " first. " +
			dashboardSpecDoc},
	{name: "update_dashboard",
		doc: "{expected_revision, name, description, spec}: the revision " + toolFoundryGetDashboard + " gave and at least one change, " +
			"spec the whole new spec. Its creator changes a dashboard that isn't pinned; owners and admins change any."},
	{name: "delete_dashboard",
		doc: "{}: deletes the dashboard and its public link. Its creator deletes a dashboard that is neither pinned nor published; owners and admins delete any."},
	{name: "pin_dashboard",
		doc: "{expected_revision, pinned}: the revision " + toolFoundryGetDashboard + " gave, and true to pin the dashboard in every member's sidebar, false to unpin it. Owners and admins only."},
	{name: "publish_dashboard", publicOnly: true,
		doc: "{expected_revision, expires_in_days}: optional, for people outside the workspace only: to share a dashboard inside it, give its url, which every member opens and reads as themselves, live. " +
			"Publishing reads every tile as you and keeps their outputs as a snapshot at a public link, which anyone who has it sees with no sign-in, unchanged until it is published again, " +
			"until it is unpublished or expires; publishing again refreshes the snapshot at the same link, or makes a new link once the old one expired. " +
			"expires_in_days 7, 30 or 90 ends the link that many days from now, 0 never; left out, a live link keeps its own and a new one never ends. " +
			"Owners and admins only (else FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY), while the workspace has public links on (else FOUNDRY_DASHBOARD_PUBLIC_OFF): " +
			"say plainly that anyone with the link sees the data as you see it. No tool returns the link: its page in Foundry shows owners and admins."},
	{name: "unpublish_dashboard", doc: "{}: deletes the public snapshot, so its link shows nothing, whether or not public links are on. Owners and admins only (else FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY)."},
	{name: "create_workflow", automationsOnly: true,
		doc: "{name, description, spec}: a workflow of yours, run as you, which waits for you to approve it in Foundry (its url) before it ever runs: " +
			"say so, and that its page shows each step and what it may do unasked. A name of 1 to 120 characters, a description of at most 1000. " + automationSpecDoc},
	{name: "update_workflow", automationsOnly: true,
		doc: "{expected_revision, name, description, spec}: the revision " + toolFoundryGetWorkflow + " gave and at least one change, spec the whole new spec. " +
			"Only its owner changes one; a changed spec stops it until they approve it again in Foundry."},
	{name: "pause_workflow", automationsOnly: true, doc: "{}: stops its runs until it is resumed; a run going on finishes. Its owner, or an owner or admin."},
	{name: "resume_workflow", automationsOnly: true, doc: "{}: runs it again when its trigger fires, from now. Its owner only, once approved."},
	{name: "delete_workflow", automationsOnly: true, doc: "{}: deletes it and its runs, cancelling a live one. Its owner, or an owner or admin."},
	{name: "run_workflow", automationsOnly: true, doc: "{}: runs its approved spec now, as its owner: its owner only, while no run of it is live."},
}

// generateReads is what a generation reads, on every host.
const generateReads = "No connector_ids reads every connector unless no_connectors is true; no document_set_ids reads no document set: list up to 20. " +
	"A set's CSV, TSV, JSON, JSONL and Parquet files that have been read are tables, counted with the synced tables toward the 50 tables a generation covers (choose fewer past it); " +
	"its Markdown, MDX, text and read PDFs are text the agent reads and loads extract (a PDF still being read is left out; a scan has no text). " +
	"A generation needs at least one table, table file or set holding text. " +
	"sample \"acme\" drafts the Acme Corp sample company from its connector, with its six tables synced, and its document set, as its one connector_ids and document_set_ids: a ready-made draft, in seconds and with no model."

// foundryOperations is each operation the host offers and allows lets the caller prepare, in
// order, and what its payload is: those of API sources only with events, of the Assistant's
// helpers only with them, of dashboards' public links only with public links, of workflows only
// where they are on.
func foundryOperations(s *Service, allows func(string) bool) (names []any, payloads string) {
	lines := []string{"Each operation's payload:"}
	for _, op := range foundryOperationDocs {
		if op.eventsOnly && !s.Events || op.hostsOnly && !s.AgentHosts || op.publicOnly && !s.PublicDashboards || op.automationsOnly && s.AutomationsOff ||
			!allows(op.name) {
			continue
		}
		doc := op.doc
		if s.Events && op.events != "" {
			doc = op.events
		}
		names = append(names, op.name)
		lines = append(lines, "- "+op.name+": "+doc)
	}
	return names, strings.Join(lines, "\n")
}

// foundryOperationSchema is the input schema of helix_foundry_prepare_operation, naming the
// operations allows lets the caller prepare: a caller allowed no create_ontology manages
// nothing of the workspace, and is told only of dashboards' targets.
func foundryOperationSchema(s *Service, allows func(string) bool) *jsonschema.Schema {
	schema := schemaFor[FoundryOperationInput]()
	names, payloads := foundryOperations(s, allows)
	// What dashboards' and workflows' operations target; a host with workflows off offers none of theirs.
	shared := "create_dashboard and create_workflow: workspace:<workspace_id>; the other dashboard operations: dashboard:<workspace_id>/<dashboard_id>; " +
		"the other workflow operations: workflow:<workspace_id>/<workflow_id>."
	if s.AutomationsOff {
		shared = "create_dashboard: workspace:<workspace_id>; the other dashboard operations: dashboard:<workspace_id>/<dashboard_id>."
	}
	target := shared
	if allows("create_ontology") {
		target = "create_ontology: tenant:<database id> for an existing database, or workspace:<workspace_id> for a new one."
		if s.Events && allows("create_event_source") {
			target += " create_event_source: workspace:<workspace_id>."
		}
		if s.AgentHosts {
			var ops []string
			for _, op := range []string{"set_assistant_runtime", "pair_assistant_helper", "revoke_assistant_helper"} {
				if allows(op) {
					ops = append(ops, op)
				}
			}
			if len(ops) > 0 {
				target += " " + strings.Join(ops, ", ") + ": workspace:<workspace_id>."
			}
		}
		target += " sync_connector: connector:<workspace_id>/<connector_id>. " + shared + " Every other operation: ontology:<workspace_id>/<ontology_id>."
	}
	schema.Properties["operation"].Description = "The action to prepare; payload says what each takes."
	schema.Properties["operation"].Enum = names
	schema.Properties["target"].Description = target
	schema.Properties["payload"].Description = payloads
	return schema
}

// everyOperation allows every operation the host offers: an MCP client's.
func everyOperation(string) bool { return true }

// executeOperationSchema is the input schema of helix_foundry_execute_operation.
func executeOperationSchema(s *Service, allows func(string) bool) *jsonschema.Schema {
	schema := schemaFor[ExecuteFoundryInput]()
	schema.Properties["operation"].Enum, _ = foundryOperations(s, allows)
	return schema
}

// withoutEventSources takes the API source fields out of an output schema, on
// a host without them. Such a host never fills them: a field left out of the
// schema and then filled would fail the SDK's check of the tool's output.
func withoutEventSources(schema *jsonschema.Schema) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	delete(schema.Properties, "event_source")
	delete(schema.Properties, "event_sources")
	for _, property := range schema.Properties {
		withoutEventSources(property)
	}
	withoutEventSources(schema.Items)
	return schema
}

// notNullable makes an input schema's required arrays and objects plain: a nil slice or pointer
// infers ["null", "array"], but a required field is never null. An optional one keeps null, which
// some clients send for a field they leave unset.
func notNullable(schema *jsonschema.Schema) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	for _, name := range schema.Required {
		if property := schema.Properties[name]; property != nil && len(property.Types) == 2 && property.Types[0] == "null" {
			property.Type, property.Types = property.Types[1], nil
		}
	}
	for _, property := range schema.Properties {
		notNullable(property)
	}
	notNullable(schema.Items)
	return schema
}

// withItems bounds how many items schema's array field takes.
func withItems(schema *jsonschema.Schema, field string, minItems, maxItems int) *jsonschema.Schema {
	schema.Properties[field].MinItems, schema.Properties[field].MaxItems = jsonschema.Ptr(minItems), jsonschema.Ptr(maxItems)
	return schema
}

// schemaFor is the schema the SDK would infer for T, which AddTool panics
// without too.
func schemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("integration: schema of %T: %v", *new(T), err))
	}
	return schema
}
