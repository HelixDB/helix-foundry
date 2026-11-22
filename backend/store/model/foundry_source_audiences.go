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
	// Guest is a reader the source knows as a guest: they see only the
	// items that list them, none shared with everyone.
	Guest bool
	// Visible are the keys of the items the source shares with the reader,
	// sorted.
	Visible []string
	// Items counts the items the source shares with anyone at that read.
	Items int
}

// SeesAll reports whether the reader sees every item the connector's source
// holds, as of a read that worked.
//
//	at := time.Now()
//	model.FoundrySourceSharing{ReadAt: &at, Visible: []string{"C1"}, Items: 1}.SeesAll() // true
func (s FoundrySourceSharing) SeesAll() bool { return s.ReadAt != nil && len(s.Visible) == s.Items }

// FoundrySourceAudience is one connector's audiences as owners and admins
// see them: when they were read, why the latest read failed, what its items
// are shared with, and the source's people.
type FoundrySourceAudience struct {
	ConnectorID string
	// ReadAt is when the audiences were last read; nil while no read has worked.
	ReadAt *time.Time
	// Problem is why the latest read failed, when no read has worked since;
	// nil otherwise.
	Problem *FoundrySourceAudienceProblem
	// EveryoneItems are shared with everyone but the source's guests (public
	// channels), MembersItems with the people they list (private ones).
	EveryoneItems, MembersItems int
	// People are the source's people, by email, sorted.
	People []FoundrySourcePerson
}

// FoundrySourceAudienceProblem is why a read of a connector's audiences
// failed, worded for the customer, and when.
type FoundrySourceAudienceProblem struct {
	Message string
	At      time.Time
}

// FoundrySourcePerson is one of a source's people: their email, as
// connector.ParseEmail writes it, and whether the source knows them as a guest.
type FoundrySourcePerson struct {
	Email string
	Guest bool
}
