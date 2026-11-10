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
	// Directory is set on a team the host keeps in step with a directory
	// group: its name and members are the group's.
	Directory *FoundryTeamDirectory
}

// FoundryTeamDirectory is the directory group a synced team follows.
type FoundryTeamDirectory struct {
	DirectoryID string
	GroupID     string
	// Provider names the directory as people know it ("Okta").
	Provider string
	// LeftOutUserIDs are the group's members the team leaves out: members
	// with raw access, whom its grants can't confine, while it holds grants.
	LeftOutUserIDs []string
}

// FoundryDirectoryTeam is a directory group as the host reads it, for
// SyncFoundryDirectoryTeams.
type FoundryDirectoryTeam struct {
	GroupID string
	Name    string
	Members []FoundryDirectoryTeamMember
}

// FoundryDirectoryTeamMember is a member of a directory group who is a
// member of the workspace.
type FoundryDirectoryTeamMember struct {
	UserID string
	// Unconfinable is a member (role member) with raw access: grants can't
	// confine them, so a team holding grants leaves them out. Owners and
	// admins are not unconfinable: they may sit on a team as they are.
	Unconfinable bool
}

// FoundryDirectorySync is what SyncFoundryDirectoryTeams did.
type FoundryDirectorySync struct {
	// TeamIDs names each group's team, by group id.
	TeamIDs map[string]string
	// LeftOut lists, by group id, the members its team left out.
	LeftOut map[string][]string
	// Deleted counts the teams of groups the directory no longer has.
	Deleted int
}
