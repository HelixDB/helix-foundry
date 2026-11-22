package model

import "time"

// FoundrySourceSharing is what one reader reads of one connector whose kind
// reports its source's permissions (connector.Audienced), as its last read
// of them says. A connector of such a kind with none is one whose audiences
// were never read or tried.
type FoundrySourceSharing struct {
	ConnectorID string
	// ReadAt is when the audiences were last read; nil while no read has
	// worked: every item is then unknown, and the reader sees none.
	ReadAt *time.Time
	// ProblemAt is when a read since ReadAt failed; nil for none.
	ProblemAt *time.Time
	// Visible are the keys of the items the source shares with the reader,
	// sorted.
	Visible []string
}

// FoundrySourceAudienceRead is when a connector's audiences were last read
// (nil while no read has worked) and when a read since failed (nil for
// none).
type FoundrySourceAudienceRead struct {
	ReadAt, ProblemAt *time.Time
}

// FoundrySourceAudience is one connector's audiences as owners and admins
// see them: when they were read, why the latest read failed, and how many of
// its items are shared with whom.
type FoundrySourceAudience struct {
	ConnectorID string
	// ReadAt is when the audiences were last read; nil while no read has worked.
	ReadAt *time.Time
	// Problem is why the latest read failed, when no read has worked since;
	// nil otherwise.
	Problem *FoundrySourceAudienceProblem
	// EveryoneItems are shared with everyone but the source's guests (public
	// channels), MembersItems with the people they list (private ones),
	// whoever of them Foundry matches.
	EveryoneItems, MembersItems int
}

// FoundrySourceAudienceProblem is why a read of a connector's audiences
// failed, worded for the customer, and when.
type FoundrySourceAudienceProblem struct {
	Message string
	At      time.Time
}

// FoundrySourcePerson is one of a source's people, as a reader's email
// matched them: its connector, and whether the source knows them as a guest.
type FoundrySourcePerson struct {
	ConnectorID string
	Guest       bool
}
