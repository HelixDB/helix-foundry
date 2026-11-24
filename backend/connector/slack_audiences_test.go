package connector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/access"
)

// slackPeople are a workspace's users for audiences: Ada, with her email
// in capitals; Grace, a guest; Bea, a single-channel guest; Alan, deleted;
// a bot; Linus, without an email; and Ada's second account, no guest.
var slackPeople = []string{
	`{"id":"U1","name":"ada","profile":{"email":"  Ada@Example.COM "}}`,
	`{"id":"U2","name":"grace","is_restricted":true,"profile":{"email":"grace@example.com"}}`,
	`{"id":"U3","name":"bea","is_ultra_restricted":true,"profile":{"email":"bea@example.com"}}`,
	`{"id":"U4","name":"alan","deleted":true,"profile":{"email":"alan@example.com"}}`,
	`{"id":"UB","name":"deploy","is_bot":true,"profile":{"email":"deploy@example.com"}}`,
	`{"id":"U5","name":"linus","profile":{}}`,
	`{"id":"U6","name":"ada2","profile":{"email":"ada@example.com"}}`,
}

// slackAudienceRoutes serve slackPeople and channels: #general (public),
// #old (public, archived), #design (private: Ada, Grace, Alan, Linus and
// someone of another workspace) and #board (private: Bea).
func slackAudienceRoutes() map[string]func(*http.Request) (int, string) {
	w := &slackWorkspace{
		users: slackPeople,
		channels: []string{
			`{"id":"C1","name":"general","is_private":false,"is_member":true}`,
			`{"id":"C3","name":"old","is_archived":true}`,
			`{"id":"C2","name":"design","is_private":true,"is_member":true}`,
			`{"id":"C4","name":"board","is_private":true,"is_member":true}`,
		},
		members: map[string][]string{"C2": {`"U1"`, `"U2"`, `"U4"`, `"U5"`, `"W9"`}, "C4": {`"U3"`}},
	}
	routes := w.routes()
	for _, method := range []string{"users.conversations", "conversations.history", "conversations.replies"} {
		delete(routes, method)
	}
	return routes
}

func mustEmail(t *testing.T, raw string) Email {
	t.Helper()
	e, ok := ParseEmail(raw)
	if !ok {
		t.Fatalf("ParseEmail(%q) is no email", raw)
	}
	return e
}

// audienceText is a's items as "key=everyone" or "key=a,b", sorted, and
// its people as "email" or "email (guest)", sorted.
func audienceText(a Audiences) (items, people string) {
	var is, ps []string
	for key, audience := range a.Items {
		switch audience.Kind() {
		case AudienceEveryone:
			is = append(is, key+"=everyone")
		case AudienceMembers:
			var members []string
			for _, m := range audience.Members() {
				members = append(members, m.String())
			}
			is = append(is, key+"="+strings.Join(members, ","))
		}
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

func TestSlackAudiences(t *testing.T) {
	ctx := context.Background()
	t.Run("shares public channels with everyone and private ones with their members who have an email", func(t *testing.T) {
		kind, calls := slackAt(t, slackAudienceRoutes())
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		items, people := audienceText(a)
		// Alan is deleted, Linus has no email and W9 is of another workspace:
		// none of them is anyone #design can be shared with.
		if items != "C1=everyone C2=ada@example.com,grace@example.com C3=everyone C4=bea@example.com" {
			t.Errorf("items = %s", items)
		}
		// Both of Ada's accounts are one person; the bot and the deleted account are none.
		if people != "ada@example.com bea@example.com (guest) grace@example.com (guest)" {
			t.Errorf("people = %s", people)
		}
		if got := calls.of("conversations.members"); !slices.Equal(got, []string{"channel=C2&limit=200", "channel=C4&limit=200"}) {
			t.Errorf("members read = %q; want the private channels' alone", got)
		}
		if got := calls.of("conversations.list"); !slices.Equal(got, []string{"limit=200&types=public_channel%2Cprivate_channel"}) {
			t.Errorf("channels listed = %q", got)
		}
	})
	t.Run("one guest account of an email makes its person a guest", func(t *testing.T) {
		routes := with(slackAudienceRoutes(), map[string]string{"users.list": slackPage("members",
			`{"id":"U1","profile":{"email":"ada@example.com"}}`, `{"id":"U7","is_restricted":true,"profile":{"email":"ADA@example.com"}}`)})
		kind, _ := slackAt(t, routes)
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if _, people := audienceText(a); people != "ada@example.com (guest)" {
			t.Fatalf("people = %s", people)
		}
	})
	// Ada's second account is a guest's and is the one in #design: #design
	// lists her email, and she reads no public channel.
	t.Run("a private channel lists an email by whichever of its accounts is in it", func(t *testing.T) {
		routes := with(slackAudienceRoutes(), map[string]string{"users.list": slackPage("members",
			`{"id":"U1","profile":{"email":"ada@example.com"}}`, `{"id":"U7","is_restricted":true,"profile":{"email":"ADA@example.com"}}`)})
		routes["conversations.members"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("channel") == "C2" {
				return http.StatusOK, slackPage("members", `"U7"`)
			}
			return http.StatusOK, slackPage("members")
		}
		kind, _ := slackAt(t, routes)
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if items, people := audienceText(a); items != "C1=everyone C2=ada@example.com C3=everyone C4=" || people != "ada@example.com (guest)" {
			t.Fatalf("items = %s; people = %s", items, people)
		}
	})
	t.Run("follows every page of users, channels and members", func(t *testing.T) {
		routes := slackAudienceRoutes()
		routes["users.list"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("members", "u2", slackPeople[:2]...)
			}
			return http.StatusOK, slackPage("members", slackPeople[2:]...)
		}
		routes["conversations.list"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("channels", "c2", `{"id":"C1","name":"general"}`)
			}
			return http.StatusOK, slackPage("channels", `{"id":"C2","name":"design","is_private":true}`)
		}
		routes["conversations.members"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("members", "m2", `"U1"`)
			}
			return http.StatusOK, slackPage("members", `"U3"`)
		}
		kind, calls := slackAt(t, routes)
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if items, _ := audienceText(a); items != "C1=everyone C2=ada@example.com,bea@example.com" {
			t.Errorf("items = %s", items)
		}
		for method, want := range map[string]int{"users.list": 2, "conversations.list": 2, "conversations.members": 2} {
			if got := len(calls.of(method)); got != want {
				t.Errorf("%s called %d times, want %d", method, got, want)
			}
		}
	})
	t.Run("without groups:read lists public channels alone, leaving private ones unknown", func(t *testing.T) {
		kind, calls := slackScoped(t, "channels:read,channels:history,users:read,users:read.email", slackAudienceRoutes())
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if items, _ := audienceText(a); items != "C1=everyone C3=everyone" {
			t.Errorf("items = %s", items)
		}
		if len(calls.of("conversations.members")) != 0 {
			t.Errorf("members read = %q", calls.of("conversations.members"))
		}
	})
	t.Run("refuses without a scope it matches people by, reading nothing more", func(t *testing.T) {
		for missing, scopes := range map[string]string{
			"users:read.email": "channels:read,groups:read,users:read",
			"users:read":       "channels:read,groups:read",
			"channels:read":    "groups:read,users:read,users:read.email",
		} {
			kind, calls := slackScoped(t, scopes, slackAudienceRoutes())
			_, err := kind.Audiences(ctx, slackSource)
			var refusedErr *RefusedError
			if !errors.Is(err, errMissingScope) || !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "add the "+missing+" scope") {
				t.Errorf("without %s: error = %v", missing, err)
			}
			if len(calls.log) != 1 {
				t.Errorf("without %s: calls = %q; want auth.test alone", missing, calls.log)
			}
		}
	})
	t.Run("leaves a private channel gone since it was listed unknown", func(t *testing.T) {
		routes := slackAudienceRoutes()
		routes["conversations.members"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("channel") == "C2" {
				return http.StatusOK, slackError("channel_not_found")
			}
			return http.StatusOK, slackPage("members", `"U3"`)
		}
		kind, _ := slackAt(t, routes)
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if items, _ := audienceText(a); items != "C1=everyone C3=everyone C4=bea@example.com" {
			t.Errorf("items = %s", items)
		}
	})
	t.Run("a private channel whose members have no email lists no one", func(t *testing.T) {
		routes := with(slackAudienceRoutes(), map[string]string{"conversations.members": slackPage("members", `"U5"`)})
		kind, _ := slackAt(t, routes)
		a, err := kind.Audiences(ctx, slackSource)
		if err != nil {
			t.Fatal(err)
		}
		if a.Items["C2"].Kind() != AudienceMembers || len(a.Items["C2"].Members()) != 0 {
			t.Fatalf("C2 = %+v", a.Items["C2"])
		}
	})
	t.Run("fails on any other error, reading nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			method, answer string
			want           func(error) bool
		}{
			"a rejected token": {"auth.test", slackError("invalid_auth"), func(err error) bool {
				var auth *AuthError
				return errors.As(err, &auth)
			}},
			"a rate limit on users": {"users.list", slackError("ratelimited"), func(err error) bool {
				var r *RefusedError
				return errors.As(err, &r) && strings.Contains(r.Message, "rate limit")
			}},
			"a user without an id": {"users.list", slackPage("members", `{"name":"x"}`), func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "a listed user has no id")
			}},
			"an incomplete page of channels": {"conversations.list", `{"ok":true}`, func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "no complete page of channels")
			}},
			"a channel without an id": {"conversations.list", slackPage("channels", `{"name":"x"}`), func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "a listed channel has no id")
			}},
			"a member without an id": {"conversations.members", slackPage("members", `""`), func(err error) bool {
				return err != nil && strings.Contains(err.Error(), "lists a member without an id")
			}},
			"a members error": {"conversations.members", slackError("fatal_error"), func(err error) bool {
				var r *RefusedError
				return errors.As(err, &r) && r.Message == "Slack returned fatal_error for conversations.members"
			}},
		} {
			kind, _ := slackAt(t, with(slackAudienceRoutes(), map[string]string{tc.method: tc.answer}))
			a, err := kind.Audiences(ctx, slackSource)
			if !tc.want(err) || a.Items != nil || a.People != nil {
				t.Errorf("%s: (%+v, %v)", name, a, err)
			}
		}
	})
	t.Run("a channel day's document is its channel's", func(t *testing.T) {
		kind, _ := slackAt(t, slackAudienceRoutes())
		for _, tc := range []struct {
			stream, id, item string
			ok               bool
		}{
			{"conversations", "C1:2026-10-28", "C1", true},
			{"conversations", "C1:", "", false},
			{"conversations", ":2026-10-28", "", false},
			{"conversations", "C1", "", false},
			{"pages", "C1:2026-10-28", "", false},
		} {
			if item, ok := kind.ItemOfDocument(tc.stream, tc.id); item != tc.item || ok != tc.ok {
				t.Errorf("ItemOfDocument(%s, %s) = %q, %v", tc.stream, tc.id, item, ok)
			}
		}
	})
}

func TestEmailsAndAudiences(t *testing.T) {
	for raw, want := range map[string]string{
		"ada@example.com": "ada@example.com", " Ada@Example.COM\n": "ada@example.com", "a@b": "a@b",
		"": "", "  ": "", "ada": "", "@example.com": "", "ada@": "", "a da@example.com": "", "ada@exa\tmple.com": "", strings.Repeat("a", 309) + "@example.com": "",
		strings.Repeat("a", 308) + "@example.com": strings.Repeat("a", 308) + "@example.com",
	} {
		e, ok := ParseEmail(raw)
		if e.String() != want || ok != (want != "") {
			t.Errorf("ParseEmail(%q) = %q, %v", raw, e, ok)
		}
	}
	// Members are sorted, without repeats or the zero Email; everyone lists no one.
	ada, grace := mustEmail(t, "ada@example.com"), mustEmail(t, "Grace@example.com")
	members := MembersAudience(grace, Email{}, ada, grace)
	if members.Kind() != AudienceMembers || fmt.Sprint(members.Members()) != "[ada@example.com grace@example.com]" {
		t.Errorf("members = %v %v", members.Kind(), members.Members())
	}
	// Members can't be changed through what Members returns.
	members.Members()[0] = grace
	if members.Members()[0] != ada {
		t.Error("Members returned the audience's own slice")
	}
	if everyone := EveryoneAudience(); everyone.Kind() != AudienceEveryone || everyone.Members() != nil {
		t.Errorf("everyone = %v %v", everyone.Kind(), everyone.Members())
	}
	// The zero Audience shares with no one: only full readers read its item.
	var zero Audience
	if zero.Kind() != AudienceMembers || len(zero.Members()) != 0 || len(MembersAudience().Members()) != 0 {
		t.Error("the zero audience shares with someone")
	}
	// Audiences' maps key people by their one form.
	a := Audiences{People: map[Email]Person{ada: {}}}
	if _, ok := a.People[mustEmail(t, " ADA@example.com")]; !ok || len(slices.Collect(maps.Keys(a.People))) != 1 {
		t.Error("an email in another case is another person")
	}
}

// Every Slack table but its directory tells each row's channel by the field
// of its key (the table's first key column, which a mapping's row_key
// names): a row a grantee holds by a channel is that channel's, as the
// engine reads it from the stored row key. users is the directory; a table
// Slack doesn't sync holds no row.
func TestSlackTableRows(t *testing.T) {
	kind, _ := slackAt(t, newSlackWorkspace().routes())
	var tables []Table
	for _, o := range slackObjects {
		tables = append(tables, Table{Name: o.name})
	}
	sinks, err := slackRead(t, kind, tables, noDocuments{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range slackObjects {
		rule := kind.TableRows(o.name)
		if o.name == "users" {
			if rule.Match() != access.RowsDirectory {
				t.Errorf("users = %v; want the directory", rule.Match())
			}
			continue
		}
		rows := sinks[o.name].rows
		if len(rows) == 0 {
			t.Fatalf("%s wrote no rows to check", o.name)
		}
		for _, row := range rows {
			channel := fmt.Sprint(row["channel_id"])
			field, prefix, held := access.TableRows{Rule: rule, Items: []string{channel}}.Held()
			if field != o.key {
				t.Errorf("%s holds rows by %s; its key is %s", o.name, field, o.key)
			}
			key := fmt.Sprint(row[field])
			if prefix && !strings.HasPrefix(key, held[0]) || !prefix && key != held[0] {
				t.Errorf("%s row %s isn't held by its channel %s (%v)", o.name, key, channel, held)
			}
			for _, other := range []string{"C", channel[:len(channel)-1], channel + "0"} {
				_, _, by := access.TableRows{Rule: rule, Items: []string{other}}.Held()
				if prefix && strings.HasPrefix(key, by[0]) || !prefix && key == by[0] {
					t.Errorf("%s row %s is held by channel %s too", o.name, key, other)
				}
			}
		}
	}
	if kind.TableRows("reactions").Match() != access.RowsUnheld {
		t.Error("a table Slack doesn't sync is told apart")
	}
	app := newSlackAppTest(t, slackAudienceRoutes()).app
	for _, o := range slackObjects {
		if app.TableRows(o.name) != kind.TableRows(o.name) {
			t.Errorf("the install tells %s apart as a token doesn't", o.name)
		}
	}
}
