package model_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

func ExampleFoundryLoadUnread_Failures() {
	u := model.FoundryLoadUnread{Read: 3, Failed: map[string]int{"quota": 1, "timeout": 2}, Unrecorded: 8}
	fmt.Println(u.Failures())
	fmt.Println(model.FoundryLoadUnread{Read: 9, Unrecorded: 2}.Failures())
	// Output:
	// 3
	// 0
}

func ExampleFoundryLoadUnread_RereadAt() {
	ended := time.Date(2026, 11, 14, 12, 26, 0, 0, time.UTC)
	for _, rereads := range []int{0, 1, 4, 5, 27, 28} {
		at, ok := model.FoundryLoadUnread{Failed: map[string]int{"quota": 9}, LastEnd: &ended, Rereads: rereads}.RereadAt()
		if !ok {
			fmt.Println(rereads, "no more re-reads")
			continue
		}
		fmt.Println(rereads, at.Sub(ended))
	}
	// Output:
	// 0 15m0s
	// 1 30m0s
	// 4 4h0m0s
	// 5 6h0m0s
	// 27 6h0m0s
	// 28 no more re-reads
}

func TestFoundryLoadUnreadRereadAt(t *testing.T) {
	ended := time.Date(2026, 11, 14, 12, 26, 0, 0, time.UTC)
	for name, u := range map[string]model.FoundryLoadUnread{
		"a job live":     {Failed: map[string]int{"quota": 1}, LastEnd: &ended, Live: true},
		"no job ended":   {Failed: map[string]int{"quota": 1}},
		"past the limit": {Failed: map[string]int{"quota": 1}, LastEnd: &ended, Rereads: model.FoundryRereadLimit},
	} {
		if at, ok := u.RereadAt(); ok || !at.IsZero() {
			t.Fatalf("%s: reread at %v, %t", name, at, ok)
		}
	}
	// Each wait is at most the cap and never shrinks, whatever the count.
	last := time.Duration(0)
	for n := range model.FoundryRereadLimit {
		at, ok := model.FoundryLoadUnread{LastEnd: &ended, Rereads: n}.RereadAt()
		wait := at.Sub(ended)
		if !ok || wait < last || wait > model.FoundryRereadMost || wait < model.FoundryRereadFirst {
			t.Fatalf("re-read %d waits %v after %v", n, wait, last)
		}
		last = wait
	}
}
