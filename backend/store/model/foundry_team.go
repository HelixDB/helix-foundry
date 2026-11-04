package model

import "time"

// FoundryTeam is a team of a workspace's members, which holds grants for
// them.
type FoundryTeam struct {
	ID            string
	WorkspaceID   string
	Name          string
	MemberUserIDs []string
	// CreatedByUserID is nil for a team the host made.
	CreatedByUserID *string
	CreatedAt       time.Time
}
