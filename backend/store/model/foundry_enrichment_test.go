package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func ExampleFoundryEnrichmentLedger_Headroom() {
	caps := FoundryEnrichmentCaps{Workspace: 50_000, Global: 300_000}
	ledger := FoundryEnrichmentLedger{WorkspaceCharged: 8_460, WorkspaceReserved: 12_240, GlobalCharged: 290_000}
	fmt.Println(ledger.Headroom(caps))
	// Output: 10000
}

func ExampleFoundryEnrichmentLedger_Unspent() {
	caps := FoundryEnrichmentCaps{Workspace: 50_000, Global: 300_000}
	ledger := FoundryEnrichmentLedger{WorkspaceCharged: 8_460, WorkspaceReserved: 41_540, GlobalCharged: 90_000, GlobalReserved: 200_000}
	fmt.Println(ledger.Headroom(caps), ledger.Unspent(caps))
	// Output: 0 41540
}

func TestFoundryEnrichmentEnd(t *testing.T) {
	// A partial end always says why, and no other end does, so the row's
	// stopped_by check can't be broken from Go.
	t.Run("pairs a reason with partial only", func(t *testing.T) {
		partial := PartialFoundryEnrichment(FoundryEnrichmentStopCap)
		if partial.Status() != FoundryEnrichmentPartial || *partial.StoppedBy() != FoundryEnrichmentStopCap {
			t.Fatalf("partial = %v %v", partial.Status(), partial.StoppedBy())
		}
		done := EndFoundryEnrichment(FoundryEnrichmentDone)
		if done.Status() != FoundryEnrichmentDone || done.StoppedBy() != nil {
			t.Fatalf("done = %v %v", done.Status(), done.StoppedBy())
		}
	})

	// Live statuses and partial are not ends EndFoundryEnrichment makes.
	for _, status := range []FoundryEnrichmentStatus{FoundryEnrichmentPreparing, FoundryEnrichmentQueued, FoundryEnrichmentRunning,
		FoundryEnrichmentPartial} {
		t.Run("refuses "+string(status), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s accepted as an end", status)
				}
			}()
			EndFoundryEnrichment(status)
		})
	}

	// Headroom and what is unspent never go negative, whichever cap is
	// reached.
	t.Run("headroom stops at zero", func(t *testing.T) {
		over := FoundryEnrichmentLedger{WorkspaceCharged: 60, GlobalCharged: 10}
		if headroom, unspent := over.Headroom(FoundryEnrichmentCaps{Workspace: 40, Global: 100}),
			over.Unspent(FoundryEnrichmentCaps{Workspace: 100, Global: 5}); headroom != 0 || unspent != 0 {
			t.Fatalf("headroom = %d, unspent = %d", headroom, unspent)
		}
	})

	// The zero-byte file's digest is SHA-256 of nothing, so staging it can be
	// checked like any other file.
	t.Run("the zero-byte file's digest is SHA-256 of nothing", func(t *testing.T) {
		sum := sha256.Sum256(nil)
		if FoundryEnrichmentNone.SHA256 != hex.EncodeToString(sum[:]) || FoundryEnrichmentNone.Key != "" {
			t.Fatalf("none = %+v", FoundryEnrichmentNone)
		}
	})

	// A halt ends at its time; no halt row is no halt.
	t.Run("a halt holds until its end", func(t *testing.T) {
		until := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		halt := &FoundryEnrichmentProvider{HaltedUntil: &until}
		if !halt.Halted(until.Add(-time.Second)) || halt.Halted(until) || (*FoundryEnrichmentProvider)(nil).Halted(until) {
			t.Fatal("halt window wrong")
		}
	})
}
