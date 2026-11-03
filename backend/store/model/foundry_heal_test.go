package model

import (
	"encoding/json"
	"fmt"
	"testing"
)

func healFinding(spellings ...string) json.RawMessage {
	signal := map[string]any{"kind": "unmatched", "canonical": "supplier_of"}
	if len(spellings) > 0 {
		signal["spellings"] = spellings
	}
	raw, _ := json.Marshal(map[string]any{"signal": signal})
	return raw
}

func TestFoundryHealNext(t *testing.T) {
	seen := func(support, sources int64, spellings ...string) FoundryHealSeen {
		return FoundryHealSeen{Fingerprint: "fp", Kind: "unmatched", Finding: healFinding(spellings...), Support: support, Sources: sources}
	}
	stored := func(status FoundryHealStatus, dismissal *FoundryHealDismissal) *FoundryHealFinding {
		return &FoundryHealFinding{OntologyID: "o", Fingerprint: "fp", Kind: "unmatched", Status: status, LoadID: "l0", BaseVersionID: "v0",
			Finding: healFinding(), Support: 4, Sources: 2, Dismissal: dismissal}
	}
	dismissed := &FoundryHealDismissal{ByUserID: "u", Support: 4}
	for _, c := range []struct {
		name string
		row  *FoundryHealFinding
		seen FoundryHealSeen
		// want is the status written, "" for no row.
		want FoundryHealStatus
		// kept is whether the stored finding JSON is kept.
		kept bool
	}{
		{"new and shown", nil, seen(3, 2), FoundryHealOpen, false},
		{"new rows", nil, FoundryHealSeen{Fingerprint: "fp", Kind: "row_errors", Support: 3}, FoundryHealOpen, false},
		{"new and thin", nil, seen(3, 1), "", false},
		{"open", stored(FoundryHealOpen, nil), seen(9, 1), FoundryHealOpen, false},
		{"superseded found again", stored(FoundryHealSuperseded, nil), seen(2, 2), FoundryHealOpen, false},
		{"dismissed below twice", stored(FoundryHealDismissed, dismissed), seen(7, 5), FoundryHealDismissed, true},
		{"dismissed at twice", stored(FoundryHealDismissed, dismissed), seen(8, 5), FoundryHealOpen, false},
		{"dismissed, its own spelling", stored(FoundryHealDismissed, dismissed), seen(4, 2, "supplier_of"), FoundryHealDismissed, true},
		{"dismissed, a new spelling", stored(FoundryHealDismissed, dismissed), seen(4, 2, "supplier_of", "supplier_of_parts"), FoundryHealOpen, false},
		{"dismissed with no dismissal", stored(FoundryHealDismissed, nil), seen(4, 2), FoundryHealOpen, false},
		{"a fix's", stored("drafting", nil), seen(9, 3), "drafting", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			next := c.row.Next(c.seen, "l1", "v1")
			if c.want == "" {
				if next != nil {
					t.Fatalf("wrote %+v", next)
				}
				return
			}
			if next == nil || next.Status != c.want {
				t.Fatalf("next = %+v, want %s", next, c.want)
			}
			if next.LoadID != "l1" || next.BaseVersionID != "v1" || next.Support != c.seen.Support || next.Sources != c.seen.Sources {
				t.Fatalf("next = %+v doesn't count the new load", next)
			}
			if want := c.seen.Finding; c.kept {
				want = c.row.Finding
				if string(next.Finding) != string(want) {
					t.Fatalf("finding = %s, want the stored %s", next.Finding, want)
				}
			} else if string(next.Finding) != string(want) {
				t.Fatalf("finding = %s, want the seen %s", next.Finding, want)
			}
			if (next.Status == FoundryHealDismissed) != (next.Dismissal != nil) {
				t.Fatalf("dismissal %+v on a %s finding", next.Dismissal, next.Status)
			}
		})
	}
}

func TestFoundryHealSeenShown(t *testing.T) {
	for sources, want := range map[int64]bool{0: true, 1: false, 2: true, 9: true} {
		if got := (FoundryHealSeen{Sources: sources}).Shown(); got != want {
			t.Fatalf("sources %d: shown %v", sources, got)
		}
	}
}

func ExampleFoundryHealSpellings() {
	fmt.Println(FoundryHealSpellings(json.RawMessage(`{"signal":{"kind":"unmatched","canonical":"date_of_death","spellings":["date_of_death","date_of_death_no_link"]}}`)))
	fmt.Println(FoundryHealSpellings(json.RawMessage(`{"signal":{"kind":"unmatched","canonical":"supplier_of"}}`)))
	fmt.Println(FoundryHealSpellings(json.RawMessage(`{"signal":{"kind":"row_errors","mapping":"people"}}`)))
	fmt.Println(FoundryHealSpellings(json.RawMessage(`not json`)))
	// Output:
	// [date_of_death date_of_death_no_link]
	// [supplier_of]
	// []
	// []
}
