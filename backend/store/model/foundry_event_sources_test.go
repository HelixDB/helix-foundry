package model

import (
	"fmt"
	"testing"
	"time"
)

func ExampleEventSyncSpacing_For() {
	spacing := EventSyncSpacing{After: time.Minute, PerMillionRows: 10 * time.Minute}
	fmt.Println(spacing.For(0), spacing.For(500_000), spacing.For(2_000_000))
	// Output:
	// 1m0s 6m0s 21m0s
}

func TestFoundryEventSourceStateHolding(t *testing.T) {
	// The caps count what a source's state says it holds, with the
	// snapshot's size read apart (ISIZE): every count must reach the
	// projection, or a send could take the table past a cap.
	t.Run("carries every count with the snapshot's size", func(t *testing.T) {
		s := FoundryEventSourceState{
			TableKey: "k", TableRows: 7, TableFields: []string{"id", "amount"}, PendingPieces: 3, PendingBytes: 300,
		}
		h := s.Holding(4096)
		if h.TableRows != 7 || h.TableBytes != 4096 || len(h.TableFields) != 2 || h.PendingPieces != 3 || h.PendingBytes != 300 {
			t.Fatalf("Holding = %+v", h)
		}
	})
}
