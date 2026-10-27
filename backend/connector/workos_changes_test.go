package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkOSChanges(t *testing.T) {
	ctx := context.Background()
	reader := func(t *testing.T, url string) ChangeReader {
		t.Helper()
		return kindOf(t, catalogFor(url), "workos").(ChangeReader)
	}
	source := Source{Secret: "sk_test_abc", Settings: json.RawMessage(`{"events":true}`)}
	cursorAt := func(event string, created time.Time) json.RawMessage {
		b, _ := json.Marshal(workosCursor{Event: event, Created: created})
		return b
	}
	tables := func(names ...string) []Table {
		var out []Table
		for _, name := range names {
			out = append(out, Table{Name: name})
		}
		return out
	}
	page := func(events ...string) string {
		return `{"object":"list","data":[` + strings.Join(events, ",") + `],"list_metadata":{}}`
	}
	event := func(id, typ, data string) string {
		return fmt.Sprintf(`{"object":"event","id":%q,"event":%q,"data":%s,"created_at":"2026-10-01T12:00:00.000Z"}`, id, typ, data)
	}
	member := func(id, typ, group, user string) string {
		return event(id, typ, fmt.Sprintf(`{"directory_id":"dir_1","user":{"id":%q},"group":{"id":%q}}`, user, group))
	}
	recent := time.Now().Add(-time.Hour)

	// Invitations, SSO connections and directories have incomplete events, so
	// they are read in full; without events every table is.
	t.Run("follows the objects whose events are complete", func(t *testing.T) {
		r := reader(t, "http://unused")
		followed := []string{"organizations", "users", "organization_memberships", "directory_users", "directory_groups", "directory_group_members"}
		for _, o := range workosObjects {
			want := CaptureFull
			if slices.Contains(followed, o.name) {
				want = CaptureEvents
			}
			if got := r.Capture(source, o.name); got != want {
				t.Errorf("Capture(%s) = %s, want %s", o.name, got, want)
			}
			if got := r.Capture(Source{Settings: json.RawMessage(`{}`)}, o.name); got != CaptureFull {
				t.Errorf("Capture(%s) without events = %s, want full", o.name, got)
			}
		}
	})

	// Test and every sync probe events, so a key that can't read them falls back to full reads.
	t.Run("records whether the key reads events", func(t *testing.T) {
		for status, want := range map[int]string{http.StatusOK: `{"events":true}`, http.StatusForbidden: `{}`} {
			api := newAPIServer(t, map[string]int{"/events": status}, nil)
			d, err := kindOf(t, catalogFor(api.url), "workos").Discover(ctx, Source{Secret: "sk_test_abc"})
			if err != nil || string(d.Settings) != want || len(d.Tables) != len(workosObjects) {
				t.Fatalf("events %d: Discover = (settings %s, %d tables, %v), want %s", status, d.Settings, len(d.Tables), err, want)
			}
			if warned := len(d.Warnings) == 1 && strings.Contains(d.Warnings[0], "can't read events"); warned != (status == http.StatusForbidden) {
				t.Fatalf("events %d: warnings = %q", status, d.Warnings)
			}
			for _, r := range api.requests {
				if r.URL.Path == "/events" && (r.URL.Query().Get("events") == "" || r.URL.Query().Get("limit") != "1") {
					t.Fatalf("events probed as %s; WorkOS requires a type filter", r.URL.RawQuery)
				}
			}
		}
	})

	// WorkOS answers order=desc with its oldest event, so a cursor taken from
	// it would replay every retained event; the cursor is the current time instead.
	t.Run("starts at the current time without asking WorkOS", func(t *testing.T) {
		before := time.Now().UTC()
		start, err := reader(t, routeServer(t, map[string]func(*http.Request) (int, string){})).Start(ctx, source, tables("directory_users"))
		if err != nil {
			t.Fatal(err)
		}
		var c workosCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil || c.Event != "" || c.Created.Before(before) || c.Created.After(time.Now()) {
			t.Fatalf("cursor = %s (%v), want the current time and no event", start.Cursor, err)
		}
	})

	// Events are read in order, 100 at a time; directory users and groups are
	// re-fetched once each, since their events omit groups and add
	// previous_attributes; membership events write the (group, user) row.
	t.Run("applies events in order and re-fetches what they name", func(t *testing.T) {
		var mu sync.Mutex
		var afters []string
		fetched := map[string]int{}
		var first []string
		for i := 1; i <= 100; i++ {
			first = append(first, event(fmt.Sprintf("event_%03d", i), "dsync.user.updated", `{"id":"du_1","previous_attributes":{}}`))
		}
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				q := r.URL.Query()
				if q.Get("order") != "asc" || q.Get("limit") != "100" {
					t.Errorf("events read as %s", r.URL.RawQuery)
				}
				afters = append(afters, q.Get("after"))
				if q.Get("after") == "event_000" {
					return http.StatusOK, page(first...)
				}
				return http.StatusOK, page(
					member("event_101", "dsync.group.user_added", "grp_1", "du_1"),
					event("event_102", "dsync.group.created", `{"id":"grp_1"}`),
					member("event_103", "dsync.group.user_removed", "grp_1", "du_2"))
			},
			"/directory_users/du_1": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				fetched[r.URL.Path]++
				return http.StatusOK, `{"object":"directory_user","id":"du_1","directory_id":"dir_1","email":"a@b.c","groups":[{"id":"grp_1"}]}`
			},
			"/directory_groups/grp_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"object":"directory_group","id":"grp_1","directory_id":"dir_1","name":"Eng"}`
			},
		})
		changes := &eventChanges{}
		next, err := reader(t, url).Changes(ctx, source, tables("directory_users", "directory_groups", "directory_group_members"),
			cursorAt("event_000", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(afters, []string{"event_000", "event_100"}) || fetched["/directory_users/du_1"] != 1 {
			t.Fatalf("pages after %q, fetches %v", afters, fetched)
		}
		assertCalls(t, changes, "upsert directory_group_members grp_1/du_1", "delete directory_group_members grp_1/du_2",
			"upsert directory_users du_1", "upsert directory_groups grp_1")
		user := changes.rows["directory_users"][0]
		if _, kept := user["groups"]; kept || user["directory_id"] != "dir_1" || user["email"] != "a@b.c" {
			t.Fatalf("directory user = %v, want the full read's shape", user)
		}
		var c workosCursor
		if err := json.Unmarshal(next, &c); err != nil || c.Event != "event_103" {
			t.Fatalf("next cursor = %s (%v), want event_103", next, err)
		}
	})

	// A membership is re-fetched for the full read's shape, which its event
	// lacks; one deleted before the read reaches it is deleted.
	t.Run("deletes an object gone by the time it is re-fetched", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(*http.Request) (int, string) {
				return http.StatusOK, page(
					event("event_1", "organization_membership.created", `{"id":"om_1","organization_id":"org_1"}`),
					event("event_2", "user.updated", `{"id":"user_1"}`))
			},
			"/user_management/users/user_1": func(*http.Request) (int, string) {
				return http.StatusNotFound, `{"message":"User not found"}`
			},
			"/user_management/organization_memberships/om_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"object":"organization_membership","id":"om_1","organization_id":"org_1","organization_name":"Acme","user_id":"user_2"}`
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("users", "organization_memberships"), cursorAt("event_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "delete users user_1", "upsert organization_memberships om_1")
		if m := changes.rows["organization_memberships"][0]; m["organization_name"] != "Acme" || m["organization_id"] != "org_1" {
			t.Fatalf("membership = %v", m)
		}
	})

	// WorkOS sends no user_removed for a deleted group's or user's
	// memberships. The cascade clears earlier rows, and member rows this same
	// read added are deleted one by one, since a cascade doesn't reach them.
	t.Run("a deleted group or directory user takes its member rows", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(*http.Request) (int, string) {
				return http.StatusOK, page(
					member("event_1", "dsync.group.user_added", "grp_1", "du_1"),
					member("event_2", "dsync.group.user_added", "grp_2", "du_2"),
					event("event_3", "dsync.group.deleted", `{"id":"grp_1","directory_id":"dir_1"}`),
					event("event_4", "dsync.user.deleted", `{"id":"du_2","directory_id":"dir_1"}`))
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("directory_users", "directory_groups", "directory_group_members"),
			cursorAt("event_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes,
			"upsert directory_group_members grp_1/du_1", "upsert directory_group_members grp_2/du_2",
			"delete directory_groups grp_1", "delete directory_group_members grp_1/du_1", "where directory_group_members directory_group_id=grp_1",
			"delete directory_users du_2", "delete directory_group_members grp_2/du_2", "where directory_group_members directory_user_id=du_2")
	})

	// A deleted directory sends no user or group deletes, and member rows
	// don't hold the directory, so they are read in full; a deleted user or
	// organization takes its memberships.
	t.Run("a deleted directory, user or organization clears what it held", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(*http.Request) (int, string) {
				return http.StatusOK, page(
					event("event_1", "dsync.deleted", `{"id":"dir_1","object":"directory"}`),
					event("event_2", "user.deleted", `{"id":"user_1"}`),
					event("event_3", "organization.deleted", `{"id":"org_1"}`))
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("organizations", "users", "organization_memberships",
			"directory_users", "directory_groups", "directory_group_members"), cursorAt("event_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes,
			"where directory_users directory_id=dir_1", "where directory_groups directory_id=dir_1", "reset directory_group_members",
			"delete users user_1", "where organization_memberships user_id=user_1",
			"delete organizations org_1", "where organization_memberships organization_id=org_1")
	})

	// A table outside the read is read in full or not at all: its event types
	// aren't requested, and an event for it costs no fetch.
	t.Run("leaves tables outside the read alone", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(r *http.Request) (int, string) {
				if got := r.URL.Query()["events"]; !slices.Equal(got, []string{"user.created", "user.deleted", "user.updated"}) {
					t.Errorf("event types = %q", got)
				}
				return http.StatusOK, page(
					event("event_1", "organization.updated", `{"id":"org_1"}`),
					member("event_2", "dsync.group.user_added", "grp_1", "du_1"),
					event("event_3", "user.updated", `{"id":"user_1"}`))
			},
			"/user_management/users/user_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"object":"user","id":"user_1","email":"a@b.c"}`
			},
		})
		changes := &eventChanges{}
		if _, err := reader(t, url).Changes(ctx, source, tables("users"), cursorAt("event_0", recent), changes); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert users user_1")
	})

	// A cursor taken when WorkOS listed no events starts at its time, less
	// the overlap; WorkOS takes range_start or after, never both.
	t.Run("reads events since the cursor's time when it has no event", func(t *testing.T) {
		since := recent.UTC().Truncate(time.Second)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("range_start") != since.Add(-2*time.Minute).Format("2006-01-02T15:04:05Z") || q.Has("after") {
					t.Errorf("events read as %s", r.URL.RawQuery)
				}
				return http.StatusOK, page(event("event_1", "user.deleted", `{"id":"user_1"}`))
			},
		})
		changes := &eventChanges{}
		next, err := reader(t, url).Changes(ctx, source, tables("users"), cursorAt("", since), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "delete users user_1")
		var c workosCursor
		if err := json.Unmarshal(next, &c); err != nil || c.Event != "event_1" {
			t.Fatalf("next cursor = %s (%v), want event_1", next, err)
		}
	})

	// WorkOS lists 90 days of events; past that the read can't be complete, so it reads in full instead.
	t.Run("expires a cursor older than ninety days without asking", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){})
		_, err := reader(t, url).Changes(ctx, source, tables("users"), cursorAt("event_0", time.Now().Add(-91*24*time.Hour)), &eventChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})

	// WorkOS's refusal of a cursor is undocumented, so a refused first page
	// counts as an expired cursor only when the same list without it succeeds.
	t.Run("expires a cursor WorkOS refuses", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/events": func(r *http.Request) (int, string) {
				if r.URL.Query().Has("after") {
					return http.StatusUnprocessableEntity, `{"message":"Validation failed"}`
				}
				return http.StatusOK, page()
			},
		})
		_, err := reader(t, url).Changes(ctx, source, tables("users"), cursorAt("event_0", recent), &eventChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
}
