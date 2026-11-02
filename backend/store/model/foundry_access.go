package model

import (
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
)

// FoundryAccessGrant is one source an owner or admin granted a principal.
type FoundryAccessGrant struct {
	ID              string
	WorkspaceID     string
	Principal       access.Principal
	Target          access.Target
	CreatedByUserID *string
	CreatedAt       time.Time
}

// FoundryLoadInputs is what an ontology's load holds, as far as grants go.
type FoundryLoadInputs struct {
	OntologyID string
	LoadID     string
	VersionID  string
	Inputs     access.Inputs
	// Partial says the grantee it was read for holds only some of it, which
	// handlers narrow what they answer by.
	Partial bool
}
