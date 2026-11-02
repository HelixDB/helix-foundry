package connector

import (
	"encoding/json"
	"testing"
)

// A target changes only with what decides where the credential is sent.
func TestTargets(t *testing.T) {
	catalog := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{})
	const db = `{"host":"db.example.com","port":5432,"database":"shop","user":"helix","tables":["orders"]}`
	const posthog = `{"region":"custom","host":"https://ph.example.com","project_id":"1","event_days":30}`
	cases := []struct {
		name, kind, a, b string
		same             bool
	}{
		{"database, user and tables", "postgres", db, `{"host":"db.example.com","port":5432,"database":"crm","user":"reader"}`, true},
		{"host", "postgres", db, `{"host":"db.attacker.example","port":5432,"database":"shop","user":"helix"}`, false},
		{"port", "postgres", db, `{"host":"db.example.com","port":6543,"database":"shop","user":"helix"}`, false},
		{"tls", "postgres", db, `{"host":"db.example.com","port":5432,"database":"shop","user":"helix","disable_tls":true}`, false},
		{"ca", "postgres", db, `{"host":"db.example.com","port":5432,"database":"shop","user":"helix","ca":"PEM"}`, false},
		{"mysql host", "mysql", db, `{"host":"db.attacker.example","port":5432,"database":"shop","user":"helix"}`, false},
		{"project and days", "posthog", posthog, `{"region":"custom","host":"https://ph.example.com","project_id":"2","event_days":7}`, true},
		{"custom host", "posthog", posthog, `{"region":"custom","host":"https://ph.attacker.example"}`, false},
		{"region", "posthog", `{"region":"us"}`, `{"region":"eu"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			targeted, ok := kindOf(t, catalog, c.kind).(Targeted)
			if !ok {
				t.Fatalf("%s is not Targeted", c.kind)
			}
			a, errA := targeted.Target(json.RawMessage(c.a))
			b, errB := targeted.Target(json.RawMessage(c.b))
			if errA != nil || errB != nil {
				t.Fatal(errA, errB)
			}
			if (a == b) != c.same {
				t.Fatalf("Target(a) = %q, Target(b) = %q, want same = %t", a, b, c.same)
			}
		})
	}
	if _, err := kindOf(t, catalog, "postgres").(Targeted).Target(json.RawMessage(`[]`)); err == nil {
		t.Fatal("malformed settings have a target")
	}
}
