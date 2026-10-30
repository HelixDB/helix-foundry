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

// FoundryLoadInputs is what an ontology's current load holds, as far as
// grants go.
type FoundryLoadInputs struct {
	LoadID    string
	VersionID string
	Inputs    access.Inputs
}
