package connector

import (
	"encoding/json"
	"testing"
	"time"
)

func TestIntervalFloor(t *testing.T) {
	floor, ok := kindOf(t, catalogFor("http://unused"), "posthog").(IntervalFloor)
	if !ok {
		t.Fatal("posthog has no interval floor")
	}
	// PostHog rate-limits or rejects recurring exports through /query, whatever the plan pays for.
	t.Run("keeps a query-api posthog connector to one sync a day", func(t *testing.T) {
		if got := floor.MinInterval(Source{Settings: json.RawMessage(`{"region":"us"}`)}); got != 24*time.Hour {
			t.Fatalf("MinInterval = %s", got)
		}
	})
	// Batch exports are PostHog's way out; the plan's interval applies to them.
	t.Run("leaves a posthog export connector to its plan", func(t *testing.T) {
		if got := floor.MinInterval(Source{Settings: json.RawMessage(`{"mode":"export"}`)}); got != 0 {
			t.Fatalf("MinInterval = %s", got)
		}
	})
}

// Attio reads every table in full on every sync, against rate limits the
// customer's other integrations share, so no plan syncs it more than daily.
func TestAttioIntervalFloor(t *testing.T) {
	floor, ok := kindOf(t, catalogFor("http://unused"), "attio").(IntervalFloor)
	if !ok {
		t.Fatal("attio has no interval floor")
	}
	if _, ok := floor.(ChangeReader); ok {
		t.Fatal("attio reads changes; its floor needs another look")
	}
	for _, settings := range []string{`{}`, `{"tables":["companies"]}`} {
		if got := floor.MinInterval(Source{Settings: json.RawMessage(settings)}); got != 24*time.Hour {
			t.Fatalf("MinInterval(%s) = %s", settings, got)
		}
	}
}
