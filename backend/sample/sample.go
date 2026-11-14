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
// the ids.
package sample

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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
