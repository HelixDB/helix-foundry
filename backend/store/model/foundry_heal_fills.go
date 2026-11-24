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

// FoundryFoldOutcome is what a move's Extract came to, as the backend
// records it (RecordFoundryLoadMove): State is FoundryFillMoved with
// LinksAdded, or FoundryFillNeedsLoad with Why.
type FoundryFoldOutcome struct {
	State      FoundryHealFillState
	LinksAdded []string
	Why        []FoundryFoldWhy
}

// FoundryFindingState is where a finding's "Add this link" stands in the
// ontology's current load (FoundryFindingStateOf): one of
// FoundryFindingOpen, FoundryFindingCovered, FoundryFindingMoving,
// FoundryFindingMoved and FoundryFindingNeedsLoad. Nothing stores it.
type FoundryFindingState interface{ foundryFindingState() }

// FoundryFindingOpen is a finding nothing was done for yet.
type FoundryFindingOpen struct{}

// FoundryVersionRef names a version by id and number.
type FoundryVersionRef struct {
	ID      string
	Version int
}

// FoundryFindingCovered is a version, not the load's, that declares and
// routes the finding's link: making it current moves the load.
type FoundryFindingCovered struct{ Version FoundryVersionRef }

// FoundryFindingMoving is the current load moving to Version.
type FoundryFindingMoving struct{ Version FoundryVersionRef }

// FoundryFindingMoved is the current load moved to Version, gaining
// LinksAdded.
type FoundryFindingMoved struct {
	Version    FoundryVersionRef
	LinksAdded []string
}

// FoundryFindingNeedsLoad is a move refused: a new load of Version is
// needed, for Why.
type FoundryFindingNeedsLoad struct {
	Version FoundryVersionRef
	Why     []FoundryFoldWhy
}

func (FoundryFindingOpen) foundryFindingState()      {}
func (FoundryFindingCovered) foundryFindingState()   {}
func (FoundryFindingMoving) foundryFindingState()    {}
func (FoundryFindingMoved) foundryFindingState()     {}
func (FoundryFindingNeedsLoad) foundryFindingState() {}

// FoundryFindingStateOf is a finding's state from the current load's fill
// row for it (nil for none) and a version that declares and routes its link
// but isn't the load's (nil for none). A fill row says more than a version
// does: once pressed, the row decides. A fill's later states (applying,
// filled, stopped) read as the move they followed, moved.
func FoundryFindingStateOf(fill *FoundryHealFill, covered *FoundryVersionRef) FoundryFindingState {
	if fill == nil {
		if covered != nil {
			return FoundryFindingCovered{Version: *covered}
		}
		return FoundryFindingOpen{}
	}
	to := FoundryVersionRef{ID: fill.ToVersionID, Version: fill.ToVersion}
	switch fill.State {
	case FoundryFillMoving:
		return FoundryFindingMoving{Version: to}
	case FoundryFillNeedsLoad:
		return FoundryFindingNeedsLoad{Version: to, Why: fill.Why}
	default:
		return FoundryFindingMoved{Version: to, LinksAdded: fill.LinksAdded}
	}
}
