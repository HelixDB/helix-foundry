// Package sample is Foundry's sample company, Acme Corp: the draft a
// generation stores in place of the AI's when it drafts from the sample
// (GenerateOntology's sample), so a new workspace sees a ready ontology at
// once. A local install serves Acme Corp's tables from its acme-db
// (local/compose.yaml), which the site connects as a PostgreSQL connector,
// and uploads its documents into a set. The draft names each table by its
// source id, which differs with every connector, so acme.json holds
// {{table:<name>}} and {{set}} in their place.
//
// acme.json is an AI draft of those tables and documents, from the
// sample's goal, written once and kept: draft it again on a stack whose
// engine serves the vendored contract, then put the placeholders back for
// the ids. Its documents' extraction is shipped too (Extraction), so a load
// of the sample replays it rather than asking the model again.
package sample

import (
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"
)

// Acme is the one sample.
const Acme = "acme"

// What the website's sample tour makes (src/web/areas/ontology/sample.ts, SAMPLE), by the same
// names, so the website knows an agent's sample as its own (the ontology by its name, the set by
// its name and no connector): sample_test.go checks that they match.
const (
	OntologyName   = "Acme Corp customers"
	ConnectorName  = "Acme Corp"
	SetName        = "Acme Corp sample"
	SetDescription = "Acme Corp, a made-up field-service scheduling software company, as of 27 October 2026: its customer accounts, contacts, subscriptions, invoices, support tickets and monthly product usage as tables, with three customer contracts, call notes, a support escalation, an incident report and its price list."
	Goal           = "Understand Acme Corp's customers: each account with its people, plan and subscription, contract terms, invoices, support tickets, usage and what was said on calls, so we can see which renewals are at risk, what we have promised, and what we owe."
)

// The sample database's login, a read-only user of its one database, the same wherever it is
// served: the stamp's sample RDS (helix-hyperscale infra/sample-db) or a local install's acme-db.
const (
	DatabasePort     = 5432
	DatabaseName     = "acme"
	DatabaseUser     = "acme_reader"
	DatabasePassword = "acme"
)

// Documents holds the sample company's documents under acme/, the files the website serves at
// /samples/acme/.
//
//go:embed acme
var Documents embed.FS

// RDSCA is the CA bundle a hosted stamp's sample database (Amazon RDS) is verified with, the
// website's /samples/rds-ca.pem.
//
//go:embed rds-ca.pem
var RDSCA string

// extractions holds, under acme-extractions/, each document's saved model work, the ontology
// agent's artifact (extraction::artifact::Artifact), gzipped: <document's file name>.json.gz.
//
//go:embed acme-extractions
var extractions embed.FS

// extractionsBySHA names each document's artifact by the SHA-256 of the document's bytes.
var extractionsBySHA = sync.OnceValue(func() map[string]string {
	files, err := fs.ReadDir(Documents, "acme")
	if err != nil {
		panic(err) // embedded: can't fail
	}
	by := map[string]string{}
	for _, f := range files {
		body, err := fs.ReadFile(Documents, "acme/"+f.Name())
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(body)
		by[hex.EncodeToString(sum[:])] = "acme-extractions/" + f.Name() + ".json.gz"
	}
	return by
})

// Extraction is the saved model work shipped for the sample document whose bytes hash to sum
// (SHA-256, lowercase hex, as model.FoundryDocument.SHA256 holds it), as the agent's artifact
// JSON; false for any other document.
//
// A load sends it with the document's own artifacts, so the agent answers each model call it
// holds from it at no cost and asks the model only the others (ExtractDocument.Artifacts). The
// agent keys a call by the model asked and the whole call (its prompts, schema, the text and
// the ontology's types), so it replays only where the model would be asked exactly what it was
// asked when the artifact was recorded: on Helix's default models, with the engine's prompts
// of that time, for the sample's draft. Anything else (another model, a changed prompt or
// draft, another text) is asked afresh, as without it.
//
// Each artifact is what a load of the sample saved for that document, the one object under its
// extractions/ prefix in the store (document.ArtifactPrefix), kept as stored: recorded on a
// fresh local stack (tests/stack/run.sh's, FOUNDRY_STACK_MODELS=1: Helix's default models)
// running the ontology agent hosted Helix runs, a236419 (contract 4.34.0). Record them again
// when the engine's prompts or the sample's draft or documents change, or the sample's
// documents go back to being read by the model on every load
// (helix_foundry_graph_sample_documents_total{replayed="false"} counts those).
//
//	json, ok := sample.Extraction(doc.SHA256)
func Extraction(sum string) ([]byte, bool) {
	name, ok := extractionsBySHA()[sum]
	if !ok {
		return nil, false
	}
	// Every document has one, embedded (TestExtractions): none of these can fail.
	f, err := extractions.Open(name)
	if err != nil {
		panic(fmt.Sprintf("sample: %s is not shipped: %v", name, err))
	}
	defer func() { _ = f.Close() }()
	z, err := gzip.NewReader(f)
	if err != nil {
		panic(fmt.Sprintf("sample: %s is not gzip: %v", name, err))
	}
	body, err := io.ReadAll(z)
	if err != nil {
		panic(fmt.Sprintf("sample: read %s: %v", name, err))
	}
	return body, true
}

// Title is the sample generation's title, as the AI titled its goal.
const Title = "Acme Corp customer accounts, contracts and renewal risk"

// Tables are the sample's tables, by name: a sample generation needs each
// of them synced, from one connector.
var Tables = []string{
	"accounts", "contacts", "subscriptions", "invoices", "support_tickets", "product_usage",
}

//go:embed acme.json
var acme string

var placeholder = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Draft returns the sample's agent_bundle Definition for one workspace:
// sources are its tables' source ids by name
// (ontologyagent.TableSourceID), set the id of the set that holds its
// documents. It fails for another sample, or when a table is missing.
//
//	def, err := sample.Draft("acme", map[string]string{"accounts": "src_…", …}, "01J…")
func Draft(name string, sources map[string]string, set string) (json.RawMessage, error) {
	if name != Acme {
		return nil, fmt.Errorf("no sample is called %q", name)
	}
	pairs := []string{"{{set}}", set}
	for _, t := range Tables {
		if sources[t] == "" {
			return nil, fmt.Errorf("the sample's table %s is missing", t)
		}
		pairs = append(pairs, "{{table:"+t+"}}", sources[t])
	}
	def := strings.NewReplacer(pairs...).Replace(acme)
	if left := placeholder.FindString(def); left != "" {
		return nil, fmt.Errorf("the sample's draft names %s, which nothing fills", left)
	}
	return json.RawMessage(def), nil
}
