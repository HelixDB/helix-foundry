// Package sample is Foundry's sample company, Kestrel: the draft a
// generation stores in place of the AI's when it drafts from the sample
// (GenerateOntology's sample), so a new workspace sees a ready ontology at
// once. The site uploads the sample's files (public/samples/kestrel) into a
// set; the draft names its table files by their document ids, which differ
// in every workspace, so kestrel.json holds {{file:<name>}} and {{set}} in
// their place.
//
// kestrel.json is an AI draft of those files, from the sample's goal,
// written once and kept: draft it again on a stack whose engine serves the
// vendored contract, then put the placeholders back for the ids.
package sample

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Kestrel is the one sample.
const Kestrel = "kestrel"

// Title is the sample generation's title, as the AI titled its goal.
const Title = "Kestrel customer accounts, contracts and renewal risk"

// Tables are the sample's table files, by name: a sample generation needs
// each of them read, in its set.
var Tables = []string{
	"accounts.csv", "contacts.csv", "subscriptions.csv", "invoices.csv", "support_tickets.csv", "product_usage.csv",
}

//go:embed kestrel.json
var kestrel string

var placeholder = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// Draft returns the sample's agent_bundle Definition for one workspace:
// files are its table files' document ids by name, set the id of the set
// that holds them. It fails for another sample, or when a table file is
// missing.
//
//	def, err := sample.Draft("kestrel", map[string]string{"accounts.csv": "01J…", …}, "01J…")
func Draft(name string, files map[string]string, set string) (json.RawMessage, error) {
	if name != Kestrel {
		return nil, fmt.Errorf("no sample is called %q", name)
	}
	pairs := []string{"{{set}}", set}
	for _, t := range Tables {
		if files[t] == "" {
			return nil, fmt.Errorf("the sample's %s is missing", t)
		}
		pairs = append(pairs, "{{file:"+t+"}}", files[t])
	}
	def := strings.NewReplacer(pairs...).Replace(kestrel)
	if left := placeholder.FindString(def); left != "" {
		return nil, fmt.Errorf("the sample's draft names %s, which nothing fills", left)
	}
	return json.RawMessage(def), nil
}
