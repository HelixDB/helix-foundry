package model

import "time"

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
	// FoundryFillApplying is a fill of the documents behind the suggestion's
	// reviews under way: the heal worker owed them as 'link' resend rows,
	// which resend jobs send with the links to fill (contract 4.33.0). A load
	// fills one suggestion at a time (active_key).
	FoundryFillApplying FoundryHealFillState = "applying"
	// FoundryFillFilled is a fill whose every owed document ended.
	FoundryFillFilled FoundryHealFillState = "filled"
	// FoundryFillStopped is a fill that ended before its documents did, for
	// its StopReason; its owed rows ended with it.
	FoundryFillStopped FoundryHealFillState = "stopped"
)

// The reasons a fill stops (foundry_heal_fills.stop_reason).
const (
	// FoundryFillCapReached: the fill spent its cap (FoundryFillCapUSD).
	FoundryFillCapReached = "cap_reached"
	// FoundryFillModelUnavailable: its resend jobs failed
	// FoundryFillFailuresMax times in a row, backing off between.
	FoundryFillModelUnavailable = "model_unavailable"
	// FoundryFillDismissed: a person dismissed the suggestion.
	FoundryFillDismissed = "dismissed"
	// FoundryFillLoadReplaced: another load became current.
	FoundryFillLoadReplaced = "load_replaced"
)

// The fill's bounds. A fill spends at most FoundryFillCapUSD, an ontology's
// fills FoundryFillDailyCapUSD a day (its resend jobs' spend, as only fills
// spend there); both sit inside the graph worker's own job and month caps.
// One collect owes at most FoundryFillMaxDocuments documents, the rest
// counted as left (a load in full asks them); a resend job sends at most
// FoundryFillDocsPerJob of them, FoundryFillDocsPerExtract an Extract, for
// at most FoundryFillJobWall, then ends, so syncs, approvals and loads take
// their turn between. A job that fails backs the fill off by
// FoundryFillBackoff, then stops it.
const (
	FoundryFillCapUSD         = 5.0
	FoundryFillDailyCapUSD    = 10.0
	FoundryFillMaxDocuments   = 50000
	FoundryFillDocsPerJob     = 2000
	FoundryFillDocsPerExtract = 200
	FoundryFillJobWall        = 10 * time.Minute
)

// FoundryFillBackoff is how long a fill pauses after each failed resend job
// in a row; one more failure stops it (FoundryFillModelUnavailable).
var FoundryFillBackoff = []time.Duration{time.Hour, 4 * time.Hour, 24 * time.Hour}

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
	// The fill's: why it stopped (FoundryFillStopped), and its counts.
	StopReason string
	Counts     FoundryFillCounts
	// SpentUSD and CapUSD are what it spent and may spend.
	SpentUSD, CapUSD float64
	// PausedUntil is when a paused fill resumes; nil when it isn't paused.
	PausedUntil *time.Time
	Failures    int
}

// FoundryFillCounts are a fill's documents: owed by its collect, left past
// FoundryFillMaxDocuments, and how each owed one ended; and what its
// documents' fills took and added.
type FoundryFillCounts struct {
	Owed, Left, Filled, Nothing, Refused, Native int64
	ReviewsTaken, FactsAdded                     int64
}

// Done is the owed documents that ended.
func (c FoundryFillCounts) Done() int64 { return c.Filled + c.Nothing + c.Refused + c.Native }

// FoundryFillDocument is what a resend job's Extract said of one document it
// sent to be filled (contract 4.33.0's filled event), with what it spent.
// Requests is the row's, as the job read it: only that request ends.
type FoundryFillDocument struct {
	DocumentID string
	Requests   int
	// Outcome is written, nothing, native, or refused (Reason).
	Outcome, Reason           string
	Facts, Held, ReviewsTaken int64
	// What its calls spent, as its usage says.
	USDMicros, InputTokens, OutputTokens int64
}

// FoundryFillDue is a fill the heal worker has to act on: a moved one whose
// documents to collect (Finding, the suggestion as the engine wrote it),
// or an applying one to resume (Resume).
type FoundryFillDue struct {
	Fill    *FoundryHealFill
	Due     FoundryHealDue
	Finding []byte
	Resume  bool
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

// FoundryFindingApplying is the current load moved to Version and its
// documents behind the finding being filled: Fill says how far.
type FoundryFindingApplying struct {
	Version FoundryVersionRef
	Fill    FoundryHealFill
}

// FoundryFindingFixed is the fill done: Fill's counts say what it took and
// added.
type FoundryFindingFixed struct {
	Version FoundryVersionRef
	Fill    FoundryHealFill
}

// FoundryFindingStopped is the fill stopped, for Fill.StopReason.
type FoundryFindingStopped struct {
	Version FoundryVersionRef
	Fill    FoundryHealFill
}

func (FoundryFindingOpen) foundryFindingState()      {}
func (FoundryFindingCovered) foundryFindingState()   {}
func (FoundryFindingMoving) foundryFindingState()    {}
func (FoundryFindingMoved) foundryFindingState()     {}
func (FoundryFindingNeedsLoad) foundryFindingState() {}
func (FoundryFindingApplying) foundryFindingState()  {}
func (FoundryFindingFixed) foundryFindingState()     {}
func (FoundryFindingStopped) foundryFindingState()   {}

// FoundryFindingStateOf is a finding's state from the current load's fill
// row for it (nil for none) and a version that declares and routes its link
// but isn't the load's (nil for none). A fill row says more than a version
// does: once pressed, the row decides. A fill's later states read as
// applying, fixed and stopped, with the fill.
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
	case FoundryFillApplying:
		return FoundryFindingApplying{Version: to, Fill: *fill}
	case FoundryFillFilled:
		return FoundryFindingFixed{Version: to, Fill: *fill}
	case FoundryFillStopped:
		return FoundryFindingStopped{Version: to, Fill: *fill}
	default:
		return FoundryFindingMoved{Version: to, LinksAdded: fill.LinksAdded}
	}
}
