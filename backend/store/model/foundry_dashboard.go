package model

import (
	"encoding/json"
	"time"
)

// FoundryDashboard is a dashboard of a workspace (package dashboard): tiles
// of typed queries, which every member sees and reads as themselves.
type FoundryDashboard struct {
	ID          string
	WorkspaceID string
	Name        string
	Description string
	// Spec is the dashboard package's canonical spec; nil in a list.
	Spec json.RawMessage
	// Revision counts the edits of its name, description and spec, from 1.
	Revision        int64
	CreatedByUserID string
	UpdatedByUserID string
	ClientToken     *string
	// PinnedAt and PinnedByUserID are set together while it is pinned: in
	// every member's sidebar.
	PinnedAt       *time.Time
	PinnedByUserID *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Share is its public snapshot, without the snapshot itself; nil unless
	// published.
	Share *FoundryDashboardShare
}

// FoundryDashboardUpdate is an edit of a dashboard: each field set is
// changed, and at least one is.
type FoundryDashboardUpdate struct {
	Name        *string
	Description *string
	Spec        json.RawMessage
}

// FoundryDashboardEditor is who edits or deletes a dashboard: a manager (an
// owner or admin) any, anyone else only one they created that isn't pinned.
type FoundryDashboardEditor struct {
	UserID  string
	Manager bool
}

// FoundryDashboardShare is a dashboard's public snapshot.
type FoundryDashboardShare struct {
	DashboardID string
	WorkspaceID string
	// TokenSHA256 is the link's token's SHA-256, lowercase hex: what a
	// request is looked up by.
	TokenSHA256 string
	// TokenSealed is the token sealed with the connectors' keyring under
	// TokenKeyID, for its dashboard's id.
	TokenSealed []byte
	TokenKeyID  string
	// Snapshot is the public JSON (dashboard.Snapshot); nil when not read.
	Snapshot json.RawMessage
	// SpecRevision is the dashboard's revision the snapshot shows.
	SpecRevision int64
	// OntologyIDs are the ontologies its tiles read.
	OntologyIDs       []string
	PublishedByUserID string
	PublishedAt       time.Time
	FirstPublishedAt  time.Time
}

// FoundryDashboardRefusalReason is why the store refused a dashboard's write.
type FoundryDashboardRefusalReason string

const (
	// FoundryDashboardChanged: the dashboard's revision isn't the one the
	// write was made from.
	FoundryDashboardChanged FoundryDashboardRefusalReason = "changed"
	// FoundryDashboardNotEditable: the editor may not change it (it is
	// pinned, or another's, and they manage nothing).
	FoundryDashboardNotEditable FoundryDashboardRefusalReason = "not_editable"
	// FoundryDashboardLimit: the workspace has as many dashboards, or pinned
	// ones, as it may.
	FoundryDashboardLimit FoundryDashboardRefusalReason = "limit"
)

// FoundryDashboardRefusal is a dashboard's write the store refused, by reason.
type FoundryDashboardRefusal struct {
	Reason FoundryDashboardRefusalReason
	Detail string
}

func (r FoundryDashboardRefusal) Error() string { return r.Detail }
