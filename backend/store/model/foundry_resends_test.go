package model

import "testing"

// What a decision's status becomes once the engine has said what it did with
// it: every outcome ends a wait, and a release sent again changes nothing a
// job already said.
func TestFoundryReleaseStatusAfter(t *testing.T) {
	outcome := func(kind FoundryReleaseOutcomeKind, reason string) FoundryReleaseOutcome {
		return FoundryReleaseOutcome{DecisionID: "d", StatementHash: "h", Kind: kind, Reason: reason}
	}
	for name, tc := range map[string]struct {
		from    FoundryReleaseStatus
		outcome FoundryReleaseOutcome
		to      FoundryReleaseStatus
		reason  string
		ok      bool
	}{
		"written":                        {FoundryReleasePending, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"a copied decision written":      {"", outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"refused, with the reason":       {FoundryReleasePending, outcome(FoundryReleaseOutcomeRefused, "unresolved"), FoundryReleaseRefused, "unresolved", true},
		"refused, then written":          {FoundryReleaseRefused, outcome(FoundryReleaseOutcomeWritten, ""), FoundryReleaseWritten, "", true},
		"missed at its commit":           {"", outcome(FoundryReleaseOutcomeMissed, ""), FoundryReleaseMissed, "", true},
		"sent again, settled here":       {FoundryReleasePending, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseWritten, "", true},
		"sent again, carried":            {FoundryReleaseCarried, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseMissed, "", true},
		"sent again, written before":     {FoundryReleaseWritten, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseWritten, "", false},
		"sent again, refused before":     {FoundryReleaseRefused, outcome(FoundryReleaseOutcomeNotHeld, ""), FoundryReleaseRefused, "", false},
		"sent again, never reported":     {"", outcome(FoundryReleaseOutcomeNotHeld, ""), "", "", false},
		"an outcome of a later contract": {FoundryReleasePending, outcome("deferred", ""), FoundryReleaseRefused, "deferred", true},
	} {
		t.Run(name, func(t *testing.T) {
			to, reason, ok := tc.from.After(tc.outcome)
			if to != tc.to || reason != tc.reason || ok != tc.ok {
				t.Fatalf("%q after %+v = (%q, %q, %t), want (%q, %q, %t)", tc.from, tc.outcome, to, reason, ok, tc.to, tc.reason, tc.ok)
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
