package model

import "testing"

// What a decision's status becomes once the engine has said what it did with
// it: every outcome ends a wait, and a release sent again changes nothing a
// job already said.
func TestFoundryDecisionAppliedAfter(t *testing.T) {
	outcome := func(kind FoundryReleaseOutcomeKind, reason string) FoundryReleaseOutcome {
		return FoundryReleaseOutcome{DecisionID: "d", StatementHash: "h", Kind: kind, Reason: reason}
	}
	failed := FoundryReleaseJobFailed
	for name, tc := range map[string]struct {
		from    FoundryDecisionApplied
		outcome FoundryReleaseOutcome
		to      FoundryReleaseStatus
		reason  string
		ok      bool
	}{
		"written":                        {FoundryDecisionApplied{Status: FoundryReleasePending}, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"a copied decision written":      {FoundryDecisionApplied{}, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"refused, with the reason":       {FoundryDecisionApplied{Status: FoundryReleasePending}, outcome(FoundryReleaseOutcomeRefused, "unresolved"), FoundryReleaseRefused, "unresolved", true},
		"refused, then written":          {FoundryDecisionApplied{Status: FoundryReleaseRefused, Reason: "unresolved"}, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"missed at its commit":           {FoundryDecisionApplied{}, outcome(FoundryReleaseOutcomeMissed, ""), FoundryReleaseMissed, "", true},
		"sent again, settled here":       {FoundryDecisionApplied{Status: FoundryReleasePending}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseWritten, "", true},
		"sent again, carried":            {FoundryDecisionApplied{Status: FoundryReleaseCarried}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseMissed, "", true},
		"sent again, written before":     {FoundryDecisionApplied{Status: FoundryReleaseWritten}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseWritten, "", false},
		"sent again, refused before":     {FoundryDecisionApplied{Status: FoundryReleaseRefused, Reason: "conflict"}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseRefused, "conflict", false},
		"sent again, never reported":     {FoundryDecisionApplied{}, outcome(FoundryReleaseOutcomeNotHeld, ""), "", "", false},
		"an outcome of a later contract": {FoundryDecisionApplied{Status: FoundryReleasePending}, outcome("deferred", ""), FoundryReleaseRefused, "deferred", true},
		// A job that failed may have written the claim first. Settled here,
		// no review left means it did; carried, it says nothing: the load
		// may never have held the claim.
		"its job failed, settled here, sent again":   {FoundryDecisionApplied{Status: FoundryReleasePending, Reason: failed}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseWritten, "", true},
		"its job failed, carried, sent again":        {FoundryDecisionApplied{Status: FoundryReleaseCarried, Reason: failed}, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseCarried, failed, false},
		"its job failed, carried, written this time": {FoundryDecisionApplied{Status: FoundryReleaseCarried, Reason: failed}, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"its job failed, carried, refused this time": {FoundryDecisionApplied{Status: FoundryReleaseCarried, Reason: failed}, outcome(FoundryReleaseOutcomeRefused, "conflict"), FoundryReleaseRefused, "conflict", true},
		"its job failed, carried, missed this time":  {FoundryDecisionApplied{Status: FoundryReleaseCarried, Reason: failed}, outcome(FoundryReleaseOutcomeMissed, ""), FoundryReleaseMissed, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			to, reason, ok := tc.from.After(tc.outcome)
			if to != tc.to || reason != tc.reason || ok != tc.ok {
				t.Fatalf("%+v after %+v = (%q, %q, %t), want (%q, %q, %t)", tc.from, tc.outcome, to, reason, ok, tc.to, tc.reason, tc.ok)
			}
			if ok && to.Waits() {
				t.Fatalf("%q still waits", to)
			}
		})
	}
	if !FoundryReleasePending.Waits() || !FoundryReleaseCarried.Waits() || FoundryReleaseWritten.Waits() || FoundryReleaseStatus("").Waits() {
		t.Fatal("only a pending or carried decision waits")
	}
}

// A release written by a run whose word was lost moves its document's counts
// forward when a later run hears not_held of it; nothing else does here, as
// a written word is counted by the worker.
func TestFoundryDecisionAppliedMovesForward(t *testing.T) {
	notHeld := FoundryReleaseOutcome{DecisionID: "d", StatementHash: "h", Kind: FoundryReleaseOutcomeNotHeld}
	written := FoundryReleaseOutcome{DecisionID: "d", StatementHash: "h", Kind: FoundryReleaseOutcomeWritten}
	for name, tc := range map[string]struct {
		from    FoundryDecisionApplied
		outcome FoundryReleaseOutcome
		forward bool
	}{
		"not_held, settled here":                 {FoundryDecisionApplied{Status: FoundryReleasePending}, notHeld, true},
		"not_held, settled here, its job failed": {FoundryDecisionApplied{Status: FoundryReleasePending, Reason: FoundryReleaseJobFailed}, notHeld, true},
		"not_held, carried":                      {FoundryDecisionApplied{Status: FoundryReleaseCarried}, notHeld, false},
		"not_held, written before":               {FoundryDecisionApplied{Status: FoundryReleaseWritten}, notHeld, false},
		"written, counted by the worker":         {FoundryDecisionApplied{Status: FoundryReleasePending}, written, false},
		"a withdraw's not_held":                  {FoundryDecisionApplied{Status: FoundryReleasePending}, FoundryReleaseOutcome{Withdraw: true, Kind: FoundryReleaseOutcomeNotHeld}, false},
	} {
		t.Run(name, func(t *testing.T) {
			to, _, _ := tc.from.After(tc.outcome)
			if got := tc.from.MovesForward(tc.outcome, to); got != tc.forward {
				t.Fatalf("%+v after %+v (%q) moves forward %t, want %t", tc.from, tc.outcome, to, got, tc.forward)
			}
		})
	}
}

// A withdraw ends as the engine says, and a not_written of an approval its
// load had recorded written is the take-back done already, by a run that
// stopped or whose document needed a model call before its event said so:
// withdrawn. Only a withdraw that ends withdrawn from waiting, of what the
// load recorded written, moves its document's counts back.
func TestFoundryWithdrawAfter(t *testing.T) {
	outcome := func(kind FoundryReleaseOutcomeKind, reason string) FoundryReleaseOutcome {
		return FoundryReleaseOutcome{DecisionID: "d", StatementHash: "h", Withdraw: true, Kind: kind, Reason: reason}
	}
	waits := FoundryDecisionApplied{Status: FoundryReleasePending}
	written := FoundryDecisionApplied{Status: FoundryReleasePending, WasWritten: true}
	for name, tc := range map[string]struct {
		from    FoundryDecisionApplied
		outcome FoundryReleaseOutcome
		to      FoundryReleaseStatus
		reason  string
		back    bool
	}{
		"withdrawn":                       {written, outcome(FoundryReleaseOutcomeWithdrawn, ""), FoundryWithdrawn, "", true},
		"withdrawn, of one it may have":   {waits, outcome(FoundryReleaseOutcomeWithdrawn, ""), FoundryWithdrawn, "", false},
		"not written":                     {waits, outcome(FoundryReleaseOutcomeNotWritten, ""), FoundryNotWritten, "", false},
		"not written, as taken back":      {written, outcome(FoundryReleaseOutcomeNotWritten, ""), FoundryWithdrawn, "", true},
		"refused":                         {written, outcome(FoundryReleaseOutcomeRefused, "differs"), FoundryReleaseRefused, "differs", false},
		"a release's word":                {written, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseRefused, "written", false},
		"its job failed, then taken back": {FoundryDecisionApplied{Status: FoundryReleasePending, Reason: FoundryReleaseJobFailed, WasWritten: true}, outcome(FoundryReleaseOutcomeNotWritten, ""), FoundryWithdrawn, "", true},
		"answered already":                {FoundryDecisionApplied{Status: FoundryWithdrawn, WasWritten: true}, outcome(FoundryReleaseOutcomeNotWritten, ""), FoundryWithdrawn, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			to, reason, ok := tc.from.After(tc.outcome)
			if !ok || to != tc.to || reason != tc.reason || tc.from.MovesBack(to) != tc.back {
				t.Fatalf("%+v after %+v = (%q, %q, %t), moves back %t", tc.from, tc.outcome, to, reason, ok, tc.from.MovesBack(to))
			}
		})
	}
}

// A decision is unconfirmed while it still waits and its job failed: one a
// job reported on is what that job said, whatever reason it carries.
func TestFoundryDecisionAppliedUnconfirmed(t *testing.T) {
	for name, tc := range map[string]struct {
		applied FoundryDecisionApplied
		want    bool
	}{
		"settled here, its job failed": {FoundryDecisionApplied{Status: FoundryReleasePending, Reason: FoundryReleaseJobFailed}, true},
		"carried, its job failed":      {FoundryDecisionApplied{Status: FoundryReleaseCarried, Reason: FoundryReleaseJobFailed}, true},
		"waiting for its job":          {FoundryDecisionApplied{Status: FoundryReleasePending}, false},
		"refused by the engine":        {FoundryDecisionApplied{Status: FoundryReleaseRefused, Reason: "unresolved"}, false},
		"refused, the reason reused":   {FoundryDecisionApplied{Status: FoundryReleaseRefused, Reason: FoundryReleaseJobFailed}, false},
		"written":                      {FoundryDecisionApplied{Status: FoundryReleaseWritten}, false},
		"never reported":               {FoundryDecisionApplied{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.applied.Unconfirmed(); got != tc.want {
				t.Fatalf("Unconfirmed() = %t, want %t", got, tc.want)
			}
		})
	}
}
