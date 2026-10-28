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
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Acme is the one sample.
const Acme = "acme"

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
