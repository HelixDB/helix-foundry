package suggest

import (
	"cmp"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/helixdb/helix-foundry/backend/ontology"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Type is an ontology type, as a write reads it: never its data.
type Type struct{ Name, Description string }

// Link is an ontology link between two types.
type Link struct{ Name, From, To, Description string }

// Version is what a version's definition gives a write: its types, links and
// competency questions.
type Version struct {
	Types     []Type
	Links     []Link
	Questions []string
}

// VersionOf reads a version's definition as its format stores it, each list
// in name order (questions in Questions' order, at most MaxQuestions):
//   - an agent bundle's ontology.toml, [types.<Type>] with its description and
//     [links.<name>] with from, to and description, and its draft's
//     competency questions;
//   - a Definition v1's entities and relationships, which has no questions.
//
// What doesn't read is left out: a write goes on with what is left.
func VersionOf(format model.OntologyFormat, definition json.RawMessage) Version {
	out := Version{Types: []Type{}, Links: []Link{}, Questions: []string{}}
	switch format {
	case model.OntologyFormatAgentBundle:
		var def map[string]any
		if json.Unmarshal(definition, &def) != nil {
			return out
		}
		out.Questions = Questions(def, MaxQuestions)
		source, _ := def["ontology_toml"].(string)
		var doc struct {
			Types map[string]struct {
				Description string `toml:"description"`
			} `toml:"types"`
			Links map[string]struct {
				From        string `toml:"from"`
				To          string `toml:"to"`
				Description string `toml:"description"`
			} `toml:"links"`
		}
		if _, err := toml.Decode(source, &doc); err != nil {
			return out
		}
		for name, t := range doc.Types {
			out.Types = append(out.Types, Type{Name: name, Description: t.Description})
		}
		for name, l := range doc.Links {
			out.Links = append(out.Links, Link{Name: name, From: l.From, To: l.To, Description: l.Description})
		}
	case model.OntologyFormatFoundryV1:
		def, err := ontology.Parse(definition)
		if err != nil {
			return out
		}
		for _, e := range def.Entities {
			out.Types = append(out.Types, Type{Name: e.Name, Description: e.Description})
		}
		for _, r := range def.Relationships {
			out.Links = append(out.Links, Link{Name: r.Name, From: r.From, To: r.To})
		}
	}
	slices.SortFunc(out.Types, func(a, b Type) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(out.Links, func(a, b Link) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Questions are an agent bundle's competency questions as the website's
// askSuggestions picks them: the draft's cq.<id> items in their ids' natural
// order (NaturalCompare), each trimmed question once, the first limit. They
// are review data in no file, so only the draft holds them, each as a TOML
// snippet ([cq.CQ1] question = "…"); one that doesn't read is skipped.
// definition is the version's definition as JSON objects decode: a draft
// lies at definition.draft.draft.items.
func Questions(definition map[string]any, limit int) []string {
	view, _ := definition["draft"].(map[string]any)
	draft, _ := view["draft"].(map[string]any)
	items, _ := draft["items"].(map[string]any)
	type competency struct{ id, question string }
	var found []competency
	for key, item := range items {
		id, ok := strings.CutPrefix(key, "cq.")
		fields, _ := item.(map[string]any)
		snippet, _ := fields["toml"].(string)
		var doc struct {
			CQ map[string]struct {
				Question string `toml:"question"`
			} `toml:"cq"`
		}
		if !ok || snippet == "" {
			continue
		}
		if _, err := toml.Decode(snippet, &doc); err != nil {
			continue
		}
		found = append(found, competency{id: id, question: strings.TrimSpace(doc.CQ[id].Question)})
	}
	slices.SortFunc(found, func(a, b competency) int { return NaturalCompare(a.id, b.id) })
	questions := []string{}
	for _, c := range found {
		if len(questions) == limit {
			break
		}
		if c.question == "" || slices.Contains(questions, c.question) {
			continue
		}
		questions = append(questions, c.question)
	}
	return questions
}

// idChunks splits an id into runs of digits and runs of anything else.
var idChunks = regexp.MustCompile(`\d+|\D+`)

// NaturalCompare orders ids as the website's localeCompare with numeric
// collation does the agent's: runs of digits by their value, so CQ2 comes
// before CQ10.
func NaturalCompare(a, b string) int {
	x, y := idChunks.FindAllString(a, -1), idChunks.FindAllString(b, -1)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		nx, errX := strconv.ParseUint(x[i], 10, 64)
		ny, errY := strconv.ParseUint(y[i], 10, 64)
		if errX == nil && errY == nil && nx != ny {
			return cmp.Compare(nx, ny)
		}
		return strings.Compare(x[i], y[i])
	}
	return cmp.Compare(len(x), len(y))
}
