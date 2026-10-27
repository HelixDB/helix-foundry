package ontologyagent

import (
	"fmt"
	"maps"
	"slices"

	"github.com/BurntSushi/toml"
)

// AllDocuments is what a route's documents say to read every set's documents.
const AllDocuments = "all"

// DocumentRoutes are the document sets a version's mappings route to
// extraction. Each [extractions.<name>] table of mappings.toml names, in
// documents, the ids of the sets whose documents it reads, or "all"; a
// document whose set no route names is stored and read by none (it ends
// out_of_scope). The agent validates the routes; this reads only which sets
// they name, for the website's warning on an upload no route reads.
type DocumentRoutes struct {
	all  bool
	sets map[string]bool
}

// ParseDocumentRoutes reads the routes of a mappings.toml. Mappings with no
// [extractions] route nothing. A documents value that is neither "all" nor
// a list of set ids is an error.
func ParseDocumentRoutes(mappingsTOML string) (DocumentRoutes, error) {
	var mappings map[string]any
	if _, err := toml.Decode(mappingsTOML, &mappings); err != nil {
		return DocumentRoutes{}, fmt.Errorf("read mappings: %w", err)
	}
	routes := DocumentRoutes{sets: map[string]bool{}}
	extractions, ok := mappings["extractions"].(map[string]any)
	if !ok && mappings["extractions"] != nil {
		return DocumentRoutes{}, fmt.Errorf("extractions must be a table of routes")
	}
	for name, value := range extractions {
		route, ok := value.(map[string]any)
		if !ok {
			return DocumentRoutes{}, fmt.Errorf("extractions.%s must be a table", name)
		}
		switch documents := route["documents"].(type) {
		case string:
			if documents != AllDocuments {
				return DocumentRoutes{}, fmt.Errorf("extractions.%s.documents is %q, not %q or a list of set ids", name, documents, AllDocuments)
			}
			routes.all = true
		case []any:
			for _, set := range documents {
				id, ok := set.(string)
				if !ok {
					return DocumentRoutes{}, fmt.Errorf("extractions.%s.documents lists %v, not a set id", name, set)
				}
				routes.sets[id] = true
			}
		default:
			return DocumentRoutes{}, fmt.Errorf("extractions.%s.documents must be %q or a list of set ids", name, AllDocuments)
		}
	}
	return routes, nil
}

// Routes reports whether a route reads the documents of set setID.
func (r DocumentRoutes) Routes(setID string) bool {
	return r.all || r.sets[setID]
}

// Sets returns the ids of the sets the routes name, sorted; "all" names none.
func (r DocumentRoutes) Sets() []string {
	return slices.Sorted(maps.Keys(r.sets))
}

// MappedSources returns the ids of the sources a mappings.toml's
// [mappings.<name>] tables read, sorted and once each. A nested mapping
// (parent) reads its parent's source, and names none of its own. Mappings
// with no [mappings] read none; a source that is not a string is an error.
func MappedSources(mappingsTOML string) ([]string, error) {
	var mappings map[string]any
	if _, err := toml.Decode(mappingsTOML, &mappings); err != nil {
		return nil, fmt.Errorf("read mappings: %w", err)
	}
	tables, ok := mappings["mappings"].(map[string]any)
	if !ok && mappings["mappings"] != nil {
		return nil, fmt.Errorf("mappings must be a table of mappings")
	}
	sources := map[string]bool{}
	for name, value := range tables {
		mapping, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mappings.%s must be a table", name)
		}
		switch source := mapping["source"].(type) {
		case nil:
		case string:
			sources[source] = true
		default:
			return nil, fmt.Errorf("mappings.%s.source must be a source id", name)
		}
	}
	return slices.Sorted(maps.Keys(sources)), nil
}

// Enrichment is a version's People enrichment: the [enrichments.<name>] item
// of its mappings.toml, at most one per file. The agent validates the item
// against the ontology and the built-in catalog; this reads what the backend
// needs to look people up and to slim what it publishes.
type Enrichment struct {
	// Name is the item's name; Type the keyed type that holds the people.
	Name string
	Type string
	// Clues names the props of Type holding each clue; at least one is set.
	Clues EnrichmentClues
	// Props maps each target prop of Type to the profile field filling it.
	Props map[string]string
	// WorkHistory, when set, links each person to the employers in their
	// profile's experience.
	WorkHistory *EnrichmentWorkHistory
}

// EnrichmentClues are the props a person is looked up by.
type EnrichmentClues struct {
	Email       string `toml:"email"`
	LinkedInURL string `toml:"linkedin_url"`
}

// EnrichmentWorkHistory links a person to each employer, matched by Match,
// with Props filled from the experience's fields.
type EnrichmentWorkHistory struct {
	Link  string            `toml:"link"`
	Match string            `toml:"match"`
	Props map[string]string `toml:"props"`
}

// Fields are the profile fields the item maps, sorted: what a published
// profile holds beside its identity.
func (e *Enrichment) Fields() []string {
	fields := slices.Collect(maps.Values(e.Props))
	slices.Sort(fields)
	return slices.Compact(fields)
}

// ParseEnrichments reads the [enrichments] item of a mappings.toml; nil when
// it has none. More than one item, or one without a type or a clue, is an
// error.
func ParseEnrichments(mappingsTOML string) (*Enrichment, error) {
	var mappings struct {
		Enrichments map[string]struct {
			Type        string                 `toml:"type"`
			Clues       EnrichmentClues        `toml:"clues"`
			Props       map[string]string      `toml:"props"`
			WorkHistory *EnrichmentWorkHistory `toml:"work_history"`
		} `toml:"enrichments"`
	}
	if _, err := toml.Decode(mappingsTOML, &mappings); err != nil {
		return nil, fmt.Errorf("read mappings: %w", err)
	}
	switch len(mappings.Enrichments) {
	case 0:
		return nil, nil
	case 1:
	default:
		return nil, fmt.Errorf("mappings hold %d enrichments; at most one is allowed", len(mappings.Enrichments))
	}
	for name, item := range mappings.Enrichments {
		switch {
		case item.Type == "":
			return nil, fmt.Errorf("enrichments.%s has no type", name)
		case item.Clues.Email == "" && item.Clues.LinkedInURL == "":
			return nil, fmt.Errorf("enrichments.%s has no clue", name)
		}
		return &Enrichment{Name: name, Type: item.Type, Clues: item.Clues, Props: item.Props, WorkHistory: item.WorkHistory}, nil
	}
	panic("unreachable")
}
