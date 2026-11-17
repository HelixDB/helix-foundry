package api

import (
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

func TestFoundryLoadUnreadToProto(t *testing.T) {
	ended := time.Date(2026, 11, 14, 12, 26, 0, 0, time.UTC)
	ready := &foundrymodel.FoundryLoad{Status: foundrymodel.FoundryLoadReady}
	partial := &foundrymodel.FoundryLoad{Status: foundrymodel.FoundryLoadPartial}
	failed := &foundrymodel.FoundryLoad{Status: foundrymodel.FoundryLoadFailed}
	quota := foundrymodel.FoundryLoadUnread{Read: 3, Failed: map[string]int{"quota": 2}, Unrecorded: 1, LastEnd: &ended}

	t.Run("says nothing when the model failed none", func(t *testing.T) {
		if got := foundryLoadUnreadToProto(ready, true, foundrymodel.FoundryLoadUnread{Read: 9, Unrecorded: 2}); got != nil {
			t.Fatalf("unread = %v", got)
		}
	})
	t.Run("counts failed and unrecorded documents, with the reason and the re-read", func(t *testing.T) {
		got := foundryLoadUnreadToProto(partial, true, quota)
		if got.GetDocuments() != 3 || got.GetModelFailure() != "quota" || got.GetReason() != "provider quota" ||
			!got.GetRereadAt().AsTime().Equal(ended.Add(15*time.Minute)) {
			t.Fatalf("unread = %v", got)
		}
	})
	t.Run("doubles the wait with each re-read in a row", func(t *testing.T) {
		u := quota
		u.Rereads = 2
		if got := foundryLoadUnreadToProto(ready, true, u); !got.GetRereadAt().AsTime().Equal(ended.Add(time.Hour)) {
			t.Fatalf("reread at = %v", got.GetRereadAt().AsTime())
		}
		u.Rereads = 7
		if got := foundryLoadUnreadToProto(ready, true, u); !got.GetRereadAt().AsTime().Equal(ended.Add(6 * time.Hour)) {
			t.Fatalf("reread at = %v", got.GetRereadAt().AsTime())
		}
		u.Rereads = foundrymodel.FoundryRereadLimit
		if got := foundryLoadUnreadToProto(ready, true, u); got.GetRereadAt() != nil || got.GetDocuments() != 3 {
			t.Fatalf("past the limit: %v", got)
		}
	})
	t.Run("re-reads only the current servable load, and only failures that pass", func(t *testing.T) {
		for name, tc := range map[string]struct {
			load    *foundrymodel.FoundryLoad
			current bool
			u       foundrymodel.FoundryLoadUnread
		}{
			"not current": {ready, false, quota},
			"failed":      {failed, true, quota},
			"a job live":  {ready, true, foundrymodel.FoundryLoadUnread{Failed: map[string]int{"quota": 1}, Live: true, LastEnd: &ended}},
			"the model rejected the request": {ready, true,
				foundrymodel.FoundryLoadUnread{Failed: map[string]int{"unsupported": 2}, LastEnd: &ended}},
		} {
			if got := foundryLoadUnreadToProto(tc.load, tc.current, tc.u); got.GetRereadAt() != nil || got.GetDocuments() == 0 {
				t.Fatalf("%s: unread = %v", name, got)
			}
		}
	})
	t.Run("leaves out unrecorded documents while a job writes the load", func(t *testing.T) {
		u := foundrymodel.FoundryLoadUnread{Failed: map[string]int{"rate_limited": 1}, Unrecorded: 5, Live: true}
		if got := foundryLoadUnreadToProto(ready, true, u); got.GetDocuments() != 1 || got.GetReason() != "provider busy" {
			t.Fatalf("unread = %v", got)
		}
	})
	t.Run("words a failure recorded before Foundry kept its class", func(t *testing.T) {
		u := foundrymodel.FoundryLoadUnread{Failed: map[string]int{"": 9}, LastEnd: &ended}
		got := foundryLoadUnreadToProto(ready, true, u)
		if got.GetDocuments() != 9 || got.GetModelFailure() != "" || got.GetReason() != "model failed" || got.GetRereadAt() == nil {
			t.Fatalf("unread = %v", got)
		}
	})
}
