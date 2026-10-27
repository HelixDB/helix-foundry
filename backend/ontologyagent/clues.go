package ontologyagent

import (
	"context"
	"encoding/json"
	"fmt"
)

// clues.go is People enrichment's Clues (schema/clues_request.json and
// schema/clues_event.json): before a load's Ingest, the graph worker asks the
// agent whom the load could look up. The agent runs only the create passes
// of the mappings that make the [enrichments] item's people, over the tables
// Ingest will read, with no graph, and streams each person's clues and name
// as the load's identifier index will hold them.

// CluesRequest asks whom a load of a version could look up.
type CluesRequest struct {
	// RequestID is the idempotency key; Clues writes nothing, so a retry
	// simply runs again.
	RequestID string `json:"request_id"`
	// Load is the load Ingest will create: a row's size counts its id, as
	// the load's chunks do, so a row too large to load names no one.
	Load         string `json:"load"`
	OntologyTOML string `json:"ontology_toml"`
	MappingsTOML string `json:"mappings_toml"`
	// Sources are the tables, as the load's Ingest is sent them.
	Sources []Source `json:"sources"`
	// MaxRequestBytes is the largest write the load's proxy takes, as
	// Ingest's helix.max_request_bytes.
	MaxRequestBytes uint64 `json:"max_request_bytes"`
}

// CluedEntity is one person a load could look up, as the engine sends them:
// the entity (Person[c_17]), the name a profile found for them must give as
// clue_name to bind them, and the clues the load's identifier index holds,
// normalized as a profile's clue_email and clue_linkedin_url must repeat
// them. A field the person lacks (no name, a clue shared, conflicting or
// rejected) is empty.
type CluedEntity struct {
	Entity      string `json:"entity"`
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	LinkedInURL string `json:"linkedin_url,omitempty"`
}

// CluesEnd is the event that ends a Clues stream: CluesDone, CluesNoItem,
// CluesInvalidMappings or CluesSourceUnavailable.
type CluesEnd interface{ cluesEnd() }

// CluesDone is a stream that sent everyone: Entities people of the item's
// type, every one on a page, with or without a clue. NoClue have no clue
// left, NoName no name, Conflicting a clue left out for holding two values,
// and Shared a value left out because another entity holds it too.
type CluesDone struct {
	Entities    int64 `json:"entities"`
	NoClue      int64 `json:"no_clue"`
	NoName      int64 `json:"no_name"`
	Conflicting int64 `json:"conflicting"`
	Shared      int64 `json:"shared"`
}

// CluesNoItem is mappings with no [enrichments] item: nobody is looked up.
type CluesNoItem struct{}

// CluesInvalidMappings is mappings that don't fit the ontology or the tables,
// as Ingest's invalid_mappings: a retry fails the same way.
type CluesInvalidMappings struct{ Errors []string }

// CluesSourceUnavailable is a table that couldn't be read; a retry may read it.
type CluesSourceUnavailable struct{ Source string }

func (CluesDone) cluesEnd()              {}
func (CluesNoItem) cluesEnd()            {}
func (CluesInvalidMappings) cluesEnd()   {}
func (CluesSourceUnavailable) cluesEnd() {}

// CluesCall runs one Clues call (Client.Clues): each event goes to onEvent as
// it arrives, and the event that ends the stream is returned.
type CluesCall func(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error)

// ReadClues runs req through call and hands add every person each clues
// page sends, in entity order, as they arrive: nothing is held, so a load of
// any size reads in the same memory. It returns the event that ended the
// stream. An error from add ends the call with that error. A stream that
// breaks the contract (an unknown event, a page after the end, a done whose
// count is not the people paged) is an error.
func ReadClues(ctx context.Context, call CluesCall, req CluesRequest, add func(CluedEntity) error) (CluesEnd, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode clues request: %w", err)
	}
	var people int64
	started := false
	last, err := call(ctx, body, func(e json.RawMessage) error {
		var event struct {
			Event    string        `json:"event"`
			Entities []CluedEntity `json:"entities"`
		}
		if err := json.Unmarshal(e, &event); err != nil {
			return fmt.Errorf("read clues event: %w", err)
		}
		switch {
		case event.Event == "clues_started" && !started:
			started = true
		case event.Event == "clues" && started:
			for _, person := range event.Entities {
				if err := add(person); err != nil {
					return err
				}
				people++
			}
		default:
			return fmt.Errorf("clues event %q out of place", event.Event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !started {
		return nil, fmt.Errorf("the clues stream ended without clues_started")
	}
	end, err := decodeCluesEnd(last)
	if err != nil {
		return nil, err
	}
	if done, ok := end.(CluesDone); ok && done.Entities != people {
		return nil, fmt.Errorf("clues done counts %d people, but %d were sent", done.Entities, people)
	}
	return end, nil
}

// cluesEnding names the events that end a Clues stream.
var cluesEnding = map[string]bool{"done": true, "no_enrichment": true, "invalid_mappings": true, "source_unavailable": true}

// decodeCluesEnd reads the event that ended a stream.
func decodeCluesEnd(data []byte) (CluesEnd, error) {
	var raw struct {
		Event  string   `json:"event"`
		Errors []string `json:"errors"`
		Source string   `json:"source"`
		CluesDone
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("read clues end: %w", err)
	}
	switch raw.Event {
	case "done":
		return raw.CluesDone, nil
	case "no_enrichment":
		return CluesNoItem{}, nil
	case "invalid_mappings":
		return CluesInvalidMappings{Errors: raw.Errors}, nil
	case "source_unavailable":
		return CluesSourceUnavailable{Source: raw.Source}, nil
	}
	return nil, fmt.Errorf("clues stream ended with %q", raw.Event)
}
