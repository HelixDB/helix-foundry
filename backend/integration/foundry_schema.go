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
	eventsOnly  bool
	doc, events string
}

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
			"and its 9 documents in the document set \"Acme Corp sample\"; what the workspace already has of it is kept. Name the ontology \"Acme Corp customers\" for Foundry to show it as the sample, then generate it with sample \"acme\"."},
	{name: "sync_connector", doc: "{}.", events: "{}. An API source syncs when pieces arrive, so it needs none."},
	{name: "generate_ontology",
		doc: "{goal, connector_ids, document_set_ids, no_connectors, sample}, each optional. " + generateReads,
		events: "{goal, connector_ids, document_set_ids, no_connectors, event_sources, sample}, each optional. " + generateReads +
			" event_sources are the API sources to read, each {connector_id, mode}: connector_ids never names one, and no connector_ids reads none. " +
			"Mode structured reads its table through mappings, and its pieces reach the graph when the ontology refreshes; it needs pieces synced into its table first. " +
			"Mode extraction reads the source's newest 500 pieces as Markdown documents of its own set (one of the 20 sets; document_set_ids never names it), extracted as they arrive, about 1 cent a piece for each ontology that reads it; " +
			"work no one started spends at most $5 a month on a source, within a share for all the workspace's API sources, and past that new pieces wait for next month or a load a person starts. " +
			"It needs the source's text_field and a piece synced as a document (list_connectors' extraction.documents above 0)."},
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
	{name: "settle_review",
		doc: "{review_id, decision}: the review_key of a FoundryReview the current load holds, and a decision of {resolve: {entity_key}}, {resolve: {not_in_data: true}}, " +
			"{prop: {text|integer|number|boolean: value}} (an integer as a string of digits), or {prop: {null: true}} to remove the property."},
}

// generateReads is what a generation reads, on every host.
const generateReads = "No connector_ids reads every connector unless no_connectors is true; no document_set_ids reads no document set: list up to 20. " +
	"A set's CSV, TSV, JSON, JSONL and Parquet files that have been read are tables, counted with the synced tables toward the 50 tables a generation covers (choose fewer past it); " +
	"its Markdown, MDX, text and read PDFs are text the agent reads and loads extract (a PDF still being read is left out; a scan has no text). " +
	"A generation needs at least one table, table file or set holding text. " +
	"sample \"acme\" drafts the Acme Corp sample company from its connector, with its six tables synced, and its document set, as its one connector_ids and document_set_ids: a ready-made draft, in seconds and with no model."

// foundryOperations is each operation the host offers, in order, and what its payload is.
func foundryOperations(events bool) (names []any, payloads string) {
	lines := []string{"Each operation's payload:"}
	for _, op := range foundryOperationDocs {
		if op.eventsOnly && !events {
			continue
		}
		doc := op.doc
		if events && op.events != "" {
			doc = op.events
		}
		names = append(names, op.name)
		lines = append(lines, "- "+op.name+": "+doc)
	}
	return names, strings.Join(lines, "\n")
}

// foundryOperationSchema is the input schema of helix_foundry_prepare_operation.
func foundryOperationSchema(events bool) *jsonschema.Schema {
	schema := schemaFor[FoundryOperationInput]()
	names, payloads := foundryOperations(events)
	target := "create_ontology: tenant:<database id> for an existing database, or workspace:<workspace_id> for a new one."
	if events {
		target += " create_event_source: workspace:<workspace_id>."
	}
	target += " sync_connector: connector:<workspace_id>/<connector_id>. Every other operation: ontology:<workspace_id>/<ontology_id>."
	schema.Properties["operation"].Description = "The action to prepare; payload says what each takes."
	schema.Properties["operation"].Enum = names
	schema.Properties["target"].Description = target
	schema.Properties["payload"].Description = payloads
	return schema
}

// executeOperationSchema is the input schema of helix_foundry_execute_operation.
func executeOperationSchema(events bool) *jsonschema.Schema {
	schema := schemaFor[ExecuteFoundryInput]()
	schema.Properties["operation"].Enum, _ = foundryOperations(events)
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
