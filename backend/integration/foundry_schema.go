package integration

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// What helix_prepare_foundry_operation's fields say of each operation. An
// API source's parts are said only where the host has them (Service.Events):
// an agent reading them elsewhere would be sent to an operation that is
// refused. Those are Helix's hosts, the only ones with a shared cluster and its
// subscription, so a new database's words for them go with them.
const (
	operationsRest          = "sync_connector, generate_ontology, set_current_version, create_load, set_current_load, revise_version, upload_document, create_document_set, update_document_set, move_document or settle_review."
	targetOntology          = "create_ontology: tenant:<database id> for an existing database, or workspace:<workspace_id> for a new one."
	targetEventSource       = " create_event_source: workspace:<workspace_id>."
	targetRest              = " sync_connector: connector:<workspace_id>/<connector_id>. The rest: ontology:<workspace_id>/<ontology_id>."
	payloadOntologyStart    = "create_ontology: {name} on tenant:<id>; on workspace:<id>, {name, database: {name, slug, project_id or project_name}}, which makes a database "
	payloadOntologyEnd      = "in that project or in a new project of that name first (names of 1 to 120 characters, a slug of lowercase letters, digits and single dashes, at most 63, with a short random suffix if another database has it) and never returns the database's key"
	payloadOntology         = payloadOntologyStart + payloadOntologyEnd + "."
	payloadOntologyHosted   = payloadOntologyStart + "on the shared cluster " + payloadOntologyEnd + "; it needs a Helix subscription."
	payloadEventSource      = " create_event_source: {name, description, text_field}: an API source (connector kind events) that people and agents send pieces of JSON data to with helix_foundry_send_pieces, a name of 1 to 120 characters no other API source of the workspace has, a description of at most 1000 characters of what each piece is, and optionally text_field, the top-level field holding each piece's text (1 to 64 bytes, not id, received_at or occurred_at), which it locks as a string so every piece must send it as one, for entity extraction to read; it returns the source, its token_url (where a person makes a send token: no tool returns one) and the send tool."
	payloadSync             = " sync_connector: {}; an API source syncs when pieces arrive, so it needs none."
	payloadSyncNoEvents     = " sync_connector: {}."
	payloadGenerate         = " generate_ontology: {goal, connector_ids, document_set_ids, no_connectors, event_sources}, each optional; no connector_ids means every connector but API sources unless no_connectors is true, no document_set_ids means no document set: list the ones to read, at most 20. event_sources are the API sources to read, each {connector_id, mode}, and connector_ids never names one. Mode structured reads its table through mappings, and its pieces reach the graph when the ontology refreshes; it needs pieces synced into its table first. Mode extraction reads the source's newest 500 pieces as Markdown documents of its own set (one of the 20 sets; document_set_ids never names it), extracted as they arrive, about 1 cent a piece for each ontology that reads it; work no one started spends at most $5 a month on a source, within a share for all the workspace's API sources, and past that new pieces wait for next month or a load a person starts. It needs the source's text_field and a piece synced as a document (list_connectors' extraction.documents above 0)."
	payloadGenerateNoEvents = " generate_ontology: {goal, connector_ids, document_set_ids, no_connectors}, each optional; no connector_ids means every connector unless no_connectors is true, no document_set_ids means no document set: list the ones to read, at most 20."
	payloadRest             = " A set's CSV, TSV, JSON, JSONL and Parquet files that have been read are tables beside the connectors', and count with the synced tables toward the 50 tables one generation covers (choose fewer connectors or sets past it); its Markdown, MDX, text and PDFs whose text has been read are text the agent reads and loads extract (a PDF still being read is left out, and a scanned PDF has no text). A generation needs at least one table, table file or set holding text. set_current_version: {version_id, expected_current_version_id}, the current version as read, empty when there is none. create_load: {version_id}. set_current_load: {load_id}. revise_version: {version_id, edits, dry_run}: 1 to 100 edits, each {put: {item_id, toml}} or {remove: {item_id}}, such as item rule.overdue with its whole TOML; dry_run previews without storing a version. upload_document: {set_id, name, size_bytes, sha256}: the file's name, ending .md, .markdown, .mdx, .txt, .json, .csv, .tsv, .jsonl, .ndjson, .parquet or .pdf, its exact size (at most 1 MiB for Markdown, MDX and text and 3 MiB for the other types, unless the stamp says otherwise) and lowercase hex SHA-256; ontologies read the Markdown, MDX and text ones as text; each PDF as its text, once Helix has read it (a PDF of at most 150 pages; an encrypted or damaged one, or a scan with no text layer, can't be read and stays stored); and each CSV, TSV, JSON, JSONL and Parquet one as a table of its own once Helix has read it. The document's read says how and when. create_document_set: {name, description}. update_document_set: {set_id, name, description}, either optional. move_document: {document_id, set_id}. settle_review: {review_id, decision}, the review_key of a FoundryReview the current load holds and a decision of {resolve: {entity_key}}, {resolve: {not_in_data: true}} or {prop: {text|integer|number|boolean: value}} ({integer} as a string of digits), or {prop: {null: true}} to remove the property."
)

// connectorLinkKindNoEvents is helix_foundry_connector_link's kind on a host
// without API sources; its struct tag is the one that has them.
const connectorLinkKindNoEvents = "The connector's kind: stripe, workos, posthog, github, linear, calcom, granola, slack, notion, google, postgres, mysql or s3."

// foundryOperationSchema is the input schema of helix_prepare_foundry_operation.
func foundryOperationSchema(events bool) *jsonschema.Schema {
	schema := schemaFor[FoundryOperationInput]()
	operation, target, payload := "One of 12 operations: create_ontology, "+operationsRest,
		targetOntology+targetRest,
		payloadOntology+payloadSyncNoEvents+payloadGenerateNoEvents+payloadRest
	if events {
		operation, target, payload = "One of 13 operations: create_ontology, create_event_source, "+operationsRest,
			targetOntology+targetEventSource+targetRest,
			payloadOntologyHosted+payloadEventSource+payloadSync+payloadGenerate+payloadRest
	}
	schema.Properties["operation"].Description = operation
	schema.Properties["target"].Description = target
	schema.Properties["payload"].Description = payload
	return schema
}

// connectorLinkSchema is the input schema of helix_foundry_connector_link on a
// host without API sources.
func connectorLinkSchema() *jsonschema.Schema {
	schema := schemaFor[FoundryConnectorLinkInput]()
	schema.Properties["kind"].Description = connectorLinkKindNoEvents
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

// schemaFor is the schema the SDK would infer for T, which AddTool panics
// without too.
func schemaFor[T any]() *jsonschema.Schema {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("integration: schema of %T: %v", *new(T), err))
	}
	return schema
}
