package model

import (
	"encoding/json"
	"time"
)

// OntologySource says who wrote an ontology version.
type OntologySource string

const (
	OntologySourceManual    OntologySource = "manual"
	OntologySourceGenerated OntologySource = "generated"
)

// OntologyFormat says what a version's definition holds.
type OntologyFormat string

const (
	// OntologyFormatFoundryV1 is ontology.Definition: entities, relationships,
	// rules and constraints, validated against the synced tables.
	OntologyFormatFoundryV1 OntologyFormat = "foundry_v1"
	// OntologyFormatAgentBundle is the ontology agent's draft, stored and
	// returned as the agent generator wrote it (ontologyagent.Definition).
	OntologyFormatAgentBundle OntologyFormat = "agent_bundle"
)

// OntologyVersion is one immutable version of a Foundry ontology. Versions
// number 1, 2, 3 per ontology; the current one is a pointer on the ontology.
type OntologyVersion struct {
	ID           string
	WorkspaceID  string
	OntologyID   string
	Version      int
	Format       OntologyFormat
	Definition   json.RawMessage
	Source       OntologySource
	GenerationID *string
	// DocumentSetIDs is the version's scope: the document sets its loads
	// read, so the snapshot of a load is the uploaded documents of these
	// sets. Empty reads none. Nil only for a version written before scopes
	// (legacy), which reads its ontology's live sets until the
	// workspace-foundry-documents step stores them; every new version has one.
	DocumentSetIDs  *[]string
	CreatedByUserID *string
	CreatedAt       time.Time
}
