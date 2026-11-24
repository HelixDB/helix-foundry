package model

// FoundryHealFillState is where a suggestion's "Add this link" stands in a
// load (foundry_heal_fills.state): the move of the load to the version that
// adds the link, then the fill of the documents behind the suggestion.
type FoundryHealFillState string

const (
	// FoundryFillMoving is a move a resend job owes (a 'move' row).
	FoundryFillMoving FoundryHealFillState = "moving"
	// FoundryFillNeedsLoad is a version that adds more than links: nothing
	// moved, and Why says what needs a new load.
	FoundryFillNeedsLoad FoundryHealFillState = "needs_load"
	// FoundryFillMoved is a load that records the version now, with the
	// links it gained (LinksAdded).
	FoundryFillMoved FoundryHealFillState = "moved"
	// FoundryFillApplying, FoundryFillFilled and FoundryFillStopped are the
	// fill's, a later layer's: this binary reads them and writes none.
	FoundryFillApplying FoundryHealFillState = "applying"
	FoundryFillFilled   FoundryHealFillState = "filled"
	FoundryFillStopped  FoundryHealFillState = "stopped"
)

// FoundryFoldWhy is one thing a version changes that needs a new load, as
// the engine says it (contract 4.32.0's not_additive why): its kind and the
// item, as a dotted path of its table.
type FoundryFoldWhy struct {
	Kind string `json:"kind"`
	Item string `json:"item"`
}

// FoundryHealFill is one load's "Add this link" for one suggestion
// (fingerprint): the move from FromVersionID to ToVersionID and its state.
type FoundryHealFill struct {
	ID            string
	OntologyID    string
	LoadID        string
	Fingerprint   string
	FromVersionID string
	ToVersionID   string
	State         FoundryHealFillState
	// Why is set for FoundryFillNeedsLoad.
	Why []FoundryFoldWhy
	// LinksAdded is set once the load moved.
	LinksAdded []string
	// ToVersion is ToVersionID's number, read with the row.
	ToVersion int
}
