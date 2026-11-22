package syncer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
)

// audiencesText is a's items as "key=everyone" or "key=a,b" and its people
// as "email" or "email (guest)", each sorted.
func audiencesText(a connector.Audiences) (items, people string) {
	var is, ps []string
	for key, audience := range a.Items {
		if audience.Kind() == connector.AudienceEveryone {
			is = append(is, key+"=everyone")
			continue
		}
		var members []string
		for _, m := range audience.Members() {
			members = append(members, m.String())
		}
		is = append(is, key+"="+strings.Join(members, ","))
	}
	for email, p := range a.People {
		if p.Guest {
			ps = append(ps, email.String()+" (guest)")
		} else {
			ps = append(ps, email.String())
		}
	}
	slices.Sort(is)
	slices.Sort(ps)
	return strings.Join(is, " "), strings.Join(ps, " ")
}

// slackPeopleAccount is an account whose #general is public and #design
// private to Ada and Grace, a guest, both with their emails.
func slackPeopleAccount() *slackAccount {
	account := newSlackAccount(map[string]string{})
	account.private = map[string]bool{"C2": true}
	account.users = []string{`{"id":"U1","name":"ada","profile":{"email":"Ada@Example.com"}}`,
		`{"id":"U2","name":"grace","is_restricted":true,"profile":{"email":"grace@example.com"}}`}
	return account
}

// A sync of a kind that reports its source's permissions stores them once
// it has published, whatever tables it syncs; a failed read or write of them
// keeps what was stored, says why, and fails nothing; and a sync that fails
// first reads none.
func TestSyncerAudiences(t *testing.T) {
	t.Run("a Slack sync stores who each channel is shared with", func(t *testing.T) {
		f, _ := newSlackFixture(t, slackPeopleAccount(), `{"history_days":7,"tables":["channels"]}`, "free")
		before := time.Now()
		f.run(t, "sync-1")
		items, people := audiencesText(f.store.audiences["conn-1"])
		if items != "C1=everyone C2=ada@example.com,grace@example.com" || people != "ada@example.com grace@example.com (guest)" {
			t.Fatalf("audiences = %s; people = %s", items, people)
		}
		if at := f.store.audiencesAt["conn-1"]; at.Before(before) || at.After(time.Now()) || len(f.store.problems) != 0 {
			t.Fatalf("read at %s, problems %q", at, f.store.problems)
		}
	})
	t.Run("a read Slack refuses keeps what was stored and fails nothing", func(t *testing.T) {
		account := slackPeopleAccount()
		account.scopes = "channels:read,channels:history,groups:read,groups:history,users:read"
		f, _ := newSlackFixture(t, account, `{"history_days":7}`, "free")
		f.run(t, "sync-1")
		if len(f.store.audiences) != 0 || !slices.Equal(f.store.problems,
			[]string{"conn-1: add the users:read.email scope to the app and reinstall it, so Foundry can follow who each channel is shared with"}) {
			t.Fatalf("audiences = %v, problems = %q", f.store.audiences, f.store.problems)
		}
		if len(f.store.tables) != 5 {
			t.Fatalf("tables = %d", len(f.store.tables))
		}
	})
	t.Run("audiences the store can't keep are a problem, not a failed sync", func(t *testing.T) {
		f, _ := newSlackFixture(t, slackPeopleAccount(), `{"history_days":7,"tables":["users"]}`, "free")
		f.store.failAudiences = errors.New("connection reset")
		f.run(t, "sync-1")
		if len(f.store.audiences) != 0 ||
			!slices.Equal(f.store.problems, []string{"conn-1: Helix could not store who the source shares what with; it will try again at the next sync"}) {
			t.Fatalf("audiences = %v, problems = %q", f.store.audiences, f.store.problems)
		}
		// The next sync stores them.
		f.run(t, "sync-2")
		if items, _ := audiencesText(f.store.audiences["conn-1"]); items != "C1=everyone C2=ada@example.com,grace@example.com" {
			t.Fatalf("audiences = %s", items)
		}
	})
	t.Run("a sync that fails before it publishes reads none", func(t *testing.T) {
		f, _ := newSlackFixture(t, slackPeopleAccount(), `{"history_days":7,"tables":["channels"]}`, "free")
		f.store.leaseOwner = "worker-2"
		if _, err := f.syncer.Run(context.Background(), newSync("sync-1")); err == nil {
			t.Fatal("a sync whose lease was lost succeeded")
		}
		if len(f.store.audiences) != 0 || len(f.store.problems) != 0 {
			t.Fatalf("audiences = %v, problems = %q", f.store.audiences, f.store.problems)
		}
	})
	t.Run("a kind that reports no permissions reads none", func(t *testing.T) {
		f := newFixture(t, stripeServer(t, 200), "stripe", `{"tables":["customers"]}`, plan{code: "free"})
		f.run(t, "sync-1")
		if len(f.store.audiences) != 0 || len(f.store.problems) != 0 {
			t.Fatalf("audiences = %v, problems = %q", f.store.audiences, f.store.problems)
		}
	})
}
