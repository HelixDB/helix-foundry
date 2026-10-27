package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const calcomTestKey = "cal_live_0123456789abcdef0123456789abcdef"

const (
	calcomMe = `{"status":"success","data":{"id":1,"uuid":"u-1","username":"ada","email":"ada@example.com","name":"Ada",
		"timeZone":"Europe/London","defaultScheduleId":5,"organizationId":null,"organization":{"isPlatform":false,"id":3}}}`
	calcomTeams = `{"status":"success","data":[{"id":7,"parentId":null,"name":"Acme","slug":"acme","isOrganization":false}]}`
	// calcomBooking has an attendee, a host and a reschedule; calcomOldBooking
	// predates eventType and names its event type by the deprecated id alone.
	calcomBooking = `{"id":11,"uid":"b1","title":"Intro","status":"accepted","start":"2026-10-20T09:00:00.000Z",
		"end":"2026-10-20T09:30:00.000Z","eventTypeId":3,"eventType":{"id":3,"slug":"intro"},"rescheduledFromUid":"b0",
		"absentHost":false,"icsUid":"ics-1","createdAt":"2026-10-01T00:00:00.000Z","updatedAt":null,
		"bookingFieldsResponses":{"notes":"hi"},
		"hosts":[{"id":1,"uuid":"u-1","name":"Ada","email":"ada@example.com","username":"ada","timeZone":"Europe/London"}],
		"attendees":[{"name":"Bob","email":"bob@example.com","displayEmail":"bob@example.com","timeZone":"UTC","absent":false},
			{"name":"Cy","email":"cy@example.com","timeZone":"UTC","absent":true,"phoneNumber":"+15550100"}]}`
	calcomOldBooking = `{"id":12,"uid":"b2","title":"Follow-up","status":"cancelled","eventTypeId":4,"hosts":[],"attendees":[]}`
)

// calcomNoPacing sends requests unpaced, so a test making many does not wait.
func calcomNoPacing(t *testing.T) {
	t.Helper()
	saved := calcomEvery
	calcomEvery = 0
	t.Cleanup(func() { calcomEvery = saved })
}

// calcomServer answers each path with its handler, unpaced, after checking the
// request carries the key and the cal-api-version its endpoint needs.
func calcomServer(t *testing.T, routes map[string]func(*http.Request) (int, string)) string {
	t.Helper()
	calcomNoPacing(t)
	checked := make(map[string]func(*http.Request) (int, string), len(routes))
	for path, handler := range routes {
		version := ""
		switch {
		case path == "/v2/bookings":
			version = "2026-05-01"
		case strings.HasSuffix(path, "/event-types"):
			version = "2026-06-12"
		case path == "/v2/schedules":
			version = "2024-06-11"
		}
		checked[path] = func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer "+calcomTestKey || r.Header.Get("cal-api-version") != version {
				t.Errorf("%s: Authorization = %q, cal-api-version = %q, want version %q",
					r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("cal-api-version"), version)
			}
			return handler(r)
		}
	}
	return routeServer(t, checked)
}

// calcomFullWriter takes room rows, then fails as a full snapshot does.
type calcomFullWriter struct{ room, rows int }

var errCalcomFull = errors.New("snapshot full")

func (w *calcomFullWriter) Write(map[string]any) error {
	if w.rows == w.room {
		return errCalcomFull
	}
	w.rows++
	return nil
}

func (w *calcomFullWriter) WriteJSON([]byte) error { return w.Write(nil) }

func calcomOK(body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return http.StatusOK, body }
}

func calcomAnswer(status int) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) { return status, `{"status":"error"}` }
}

func calcomSync(t *testing.T, url, table string) ([]map[string]any, error) {
	t.Helper()
	sink := &rowSink{}
	_, err := kindOf(t, catalogFor(url), "calcom").Sync(context.Background(), Source{Secret: calcomTestKey}, Table{Name: table}, sink)
	return sink.rows, err
}

func TestCalcomConnector(t *testing.T) {
	ctx := context.Background()
	kind := func(t *testing.T, url string) Kind {
		t.Helper()
		return kindOf(t, catalogFor(url), "calcom")
	}

	t.Run("rejects something that is not a cal.com key", func(t *testing.T) {
		for _, secret := range []string{"", "lin_api_abc", "cal_", "cal_live abc", "Bearer " + calcomTestKey} {
			_, err := kind(t, "http://unused").Prepare(Source{Secret: secret})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("Prepare(%q) error = %v, want InvalidError", secret, err)
			}
		}
	})
	// Cal.com has no table choice and no host: the key is the whole setting.
	t.Run("takes the key alone", func(t *testing.T) {
		settings, err := kind(t, "http://unused").Prepare(Source{Secret: " " + calcomTestKey + " "})
		if err != nil || string(settings) != `{}` {
			t.Fatalf("Prepare = (%s, %v)", settings, err)
		}
		_, err = kind(t, "http://unused").Prepare(Source{Secret: calcomTestKey, Settings: json.RawMessage(`{"tables":["bookings"]}`)})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare with tables error = %v, want InvalidError", err)
		}
	})
	t.Run("names itself and hints at a key by its type and last four", func(t *testing.T) {
		k := kind(t, "http://unused")
		if live, test := k.Hint(calcomTestKey), k.Hint(" cal_0123456789abcdef "); live != "cal_live…cdef" || test != "cal…cdef" {
			t.Fatalf("hints = %q, %q", live, test)
		}
		if k.DisplayName() != "Cal.com" {
			t.Fatalf("DisplayName = %q", k.DisplayName())
		}
	})
	t.Run("lists every object once the key works", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":    calcomOK(calcomMe),
			"/v2/teams": calcomOK(calcomTeams),
			"/v2/teams/7/memberships": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("take") != "1" {
					t.Errorf("admin probe query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, `{"status":"success","data":[{"id":70,"userId":1,"teamId":7}]}`
			},
		})
		d, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"users", "teams", "team_memberships", "event_types", "schedules", "bookings", "booking_attendees", "booking_hosts"}
		if !slices.Equal(tableNames(d), want) || len(d.Unavailable) != 0 || len(d.Warnings) != 0 {
			t.Fatalf("tables = %v, unavailable = %v, warnings = %v", tableNames(d), d.Unavailable, d.Warnings)
		}
		keys := map[string][]string{"bookings": {"booking_uid"}, "booking_attendees": {"booking_uid", "email"},
			"booking_hosts": {"booking_uid", "user_id"}, "team_memberships": {"membership_id"}}
		for table, key := range keys {
			if got := findTable(t, d, table).PrimaryKey; !slices.Equal(got, key) {
				t.Errorf("%s key = %v, want %v", table, got, key)
			}
		}
		foreignKeys := map[string]int{"users": 2, "teams": 1, "team_memberships": 2, "event_types": 4, "schedules": 1,
			"bookings": 3, "booking_attendees": 1, "booking_hosts": 2}
		for table, n := range foreignKeys {
			if got := findTable(t, d, table).ForeignKeys; len(got) != n {
				t.Errorf("%s foreign keys = %v, want %d", table, got, n)
			}
		}
		if fk := findTable(t, d, "bookings").ForeignKeys[1]; fk != foreignKey("rescheduled_from_uid", "bookings", "booking_uid") {
			t.Fatalf("bookings' second foreign key = %+v", fk)
		}
	})
	t.Run("reports a rejected key", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){"/v2/me": calcomAnswer(http.StatusUnauthorized)})
		_, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Cal.com rejected this key; check it and enter it again" {
			t.Fatalf("Discover error = %v, want a rejected key", err)
		}
	})
	t.Run("fails when /v2/me fails or answers without a user", func(t *testing.T) {
		for name, answer := range map[string]func(*http.Request) (int, string){
			"forbidden": calcomAnswer(http.StatusForbidden),
			"no user":   calcomOK(`{"status":"success"}`),
			"not json":  calcomOK(`<html>`),
		} {
			url := calcomServer(t, map[string]func(*http.Request) (int, string){"/v2/me": answer})
			if _, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey}); err == nil {
				t.Fatalf("%s: Discover succeeded", name)
			}
		}
	})
	// A key whose account Cal.com won't list teams for still reads its own bookings, event types and schedules.
	t.Run("reports teams Cal.com won't list as unavailable", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":    calcomOK(calcomMe),
			"/v2/teams": calcomAnswer(http.StatusForbidden),
		})
		d, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unavailableNames(d), []string{"teams", "team_memberships"}) || len(d.Tables) != 6 {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		for _, table := range []string{"users", "event_types"} {
			for _, fk := range findTable(t, d, table).ForeignKeys {
				if fk.ReferencedTable == "teams" {
					t.Errorf("%s keeps a foreign key into unavailable teams: %+v", table, fk)
				}
			}
		}
	})
	// An outage is not a missing permission: an unavailable table fails a sync for good.
	t.Run("fails when Cal.com fails to list teams", func(t *testing.T) {
		noBackoff(t)
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":    calcomOK(calcomMe),
			"/v2/teams": calcomAnswer(http.StatusBadGateway),
		})
		_, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "HTTP 502 for /v2/teams") {
			t.Fatalf("Discover error = %v, want a refusal", err)
		}
	})
	t.Run("warns a test when the user administers none of its teams", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":                  calcomOK(calcomMe),
			"/v2/teams":               calcomOK(`{"status":"success","data":[{"id":7},{"id":8}]}`),
			"/v2/teams/7/memberships": calcomAnswer(http.StatusForbidden),
			"/v2/teams/8/memberships": calcomAnswer(http.StatusForbidden),
		})
		d, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey})
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Tables) != 8 || len(d.Warnings) != 1 || !strings.Contains(d.Warnings[0], "not an admin") {
			t.Fatalf("tables = %v, warnings = %v", tableNames(d), d.Warnings)
		}
	})
	// The admin probe costs a request per team, and only a test shows warnings.
	t.Run("a sync's discovery skips the admin probe", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":    calcomOK(calcomMe),
			"/v2/teams": calcomOK(calcomTeams),
		})
		d, err := kind(t, url).Discover(ctx, Source{Secret: calcomTestKey, ForSync: true})
		if err != nil || len(d.Tables) != 8 || len(d.Warnings) != 0 {
			t.Fatalf("Discover = (%v, %v), warnings %v", tableNames(d), err, d.Warnings)
		}
	})
}

func TestCalcomSync(t *testing.T) {
	ctx := context.Background()

	// Leaving status out walks every status; the cursor is opaque and null on the last page.
	t.Run("pages bookings by cursor through every status", func(t *testing.T) {
		var cursors []string
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/bookings": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("limit") != "100" || q.Has("status") {
					t.Errorf("bookings query = %s", r.URL.RawQuery)
				}
				cursors = append(cursors, q.Get("cursor"))
				if q.Get("cursor") == "" {
					return http.StatusOK, `{"status":"success","data":[` + calcomBooking + `],"pagination":{"nextCursor":"c1","hasMore":true}}`
				}
				return http.StatusOK, `{"status":"success","data":[` + calcomOldBooking + `],"pagination":{"nextCursor":null,"hasMore":false}}`
			},
		})
		rows, err := calcomSync(t, url, "bookings")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cursors, []string{"", "c1"}) || len(rows) != 2 {
			t.Fatalf("cursors = %q, rows = %v", cursors, rows)
		}
		want := map[string]any{"booking_id": 11, "booking_uid": "b1", "title": "Intro", "status": "accepted",
			"start": "2026-10-20T09:00:00.000Z", "end": "2026-10-20T09:30:00.000Z", "event_type_id": 3, "rescheduled_from_uid": "b0",
			"absent_host": false, "ics_uid": "ics-1", "created_at": "2026-10-01T00:00:00.000Z", "updated_at": nil,
			"booking_fields_responses": map[string]any{"notes": "hi"}}
		if fmt.Sprint(rows[0]) != fmt.Sprint(want) {
			t.Fatalf("row = %v", rows[0])
		}
		old := map[string]any{"booking_id": 12, "booking_uid": "b2", "title": "Follow-up", "status": "cancelled", "event_type_id": 4}
		if fmt.Sprint(rows[1]) != fmt.Sprint(old) {
			t.Fatalf("row = %v", rows[1])
		}
	})
	t.Run("writes each booking's attendees and hosts tied to it", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/bookings": calcomOK(`{"status":"success","data":[` + calcomBooking + `,` + calcomOldBooking + `],"pagination":{"hasMore":false}}`),
		})
		attendees, err := calcomSync(t, url, "booking_attendees")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"booking_uid": "b1", "name": "Bob", "email": "bob@example.com", "display_email": "bob@example.com", "time_zone": "UTC", "absent": false},
			{"booking_uid": "b1", "name": "Cy", "email": "cy@example.com", "time_zone": "UTC", "absent": true, "phone_number": "+15550100"},
		}
		if fmt.Sprint(attendees) != fmt.Sprint(want) {
			t.Fatalf("attendees = %v", attendees)
		}
		hosts, err := calcomSync(t, url, "booking_hosts")
		if err != nil {
			t.Fatal(err)
		}
		host := map[string]any{"booking_uid": "b1", "user_id": 1, "uuid": "u-1", "name": "Ada", "email": "ada@example.com",
			"username": "ada", "time_zone": "Europe/London"}
		if len(hosts) != 1 || fmt.Sprint(hosts[0]) != fmt.Sprint(host) {
			t.Fatalf("hosts = %v", hosts)
		}
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, response := range map[string]string{
		"lacks its rows":                       `{"status":"success","pagination":{"hasMore":false}}`,
		"lacks its pagination":                 `{"status":"success","data":[` + calcomBooking + `]}`,
		"says more follow but gives no cursor": `{"status":"success","data":[` + calcomBooking + `],"pagination":{"nextCursor":null,"hasMore":true}}`,
		"holds an object, not a list":          `{"status":"success","data":` + calcomBooking + `,"pagination":{"hasMore":false}}`,
	} {
		t.Run("fails a bookings page that "+name, func(t *testing.T) {
			url := calcomServer(t, map[string]func(*http.Request) (int, string){"/v2/bookings": calcomOK(response)})
			rows, err := calcomSync(t, url, "bookings")
			if err == nil || len(rows) != 0 {
				t.Fatalf("Sync error = %v, rows = %v; want a failure before any row", err, rows)
			}
		})
	}
	t.Run("writes the key's user, then each other member of the teams it administers once", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":    calcomOK(calcomMe),
			"/v2/teams": calcomOK(`{"status":"success","data":[{"id":7},{"id":8}]}`),
			"/v2/teams/7/memberships": calcomOK(`{"status":"success","data":[
				{"id":70,"userId":1,"teamId":7,"accepted":true,"role":"OWNER","user":{"uuid":"u-1","email":"ada@example.com"}},
				{"id":71,"userId":2,"teamId":7,"accepted":false,"role":"MEMBER","user":{"uuid":"u-2","email":"grace@example.com",
					"name":"Grace","avatarUrl":null}}]}`),
			// Only a team's admins list its members; the user is a plain member of team 8.
			"/v2/teams/8/memberships": calcomAnswer(http.StatusForbidden),
		})
		rows, err := calcomSync(t, url, "users")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"user_id": 1, "uuid": "u-1", "username": "ada", "email": "ada@example.com", "name": "Ada", "time_zone": "Europe/London",
				"default_schedule_id": 5, "organization_id": nil},
			{"user_id": 2, "uuid": "u-2", "email": "grace@example.com", "name": "Grace", "avatar_url": nil},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("users = %v", rows)
		}
		rows, err = calcomSync(t, url, "team_memberships")
		if err != nil {
			t.Fatal(err)
		}
		membership := map[string]any{"membership_id": 70, "user_id": 1, "team_id": 7, "accepted": true, "role": "OWNER"}
		if len(rows) != 2 || fmt.Sprint(rows[0]) != fmt.Sprint(membership) {
			t.Fatalf("memberships = %v", rows)
		}
	})
	t.Run("writes teams", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){"/v2/teams": calcomOK(calcomTeams)})
		rows, err := calcomSync(t, url, "teams")
		want := map[string]any{"team_id": 7, "parent_id": nil, "name": "Acme", "slug": "acme", "is_organization": false}
		if err != nil || len(rows) != 1 || fmt.Sprint(rows[0]) != fmt.Sprint(want) {
			t.Fatalf("teams = (%v, %v)", rows, err)
		}
	})
	t.Run("pages a team's memberships by take and skip", func(t *testing.T) {
		var skips []string
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/teams": calcomOK(calcomTeams),
			"/v2/teams/7/memberships": func(r *http.Request) (int, string) {
				skip := r.URL.Query().Get("skip")
				skips = append(skips, skip)
				if r.URL.Query().Get("take") != "250" {
					t.Errorf("memberships query = %s", r.URL.RawQuery)
				}
				n := 1
				if skip == "0" {
					n = 250
				}
				rows := make([]string, n)
				for i := range rows {
					rows[i] = fmt.Sprintf(`{"id":%d,"userId":%d,"teamId":7}`, len(skips)*1000+i, i)
				}
				return http.StatusOK, `{"status":"success","data":[` + strings.Join(rows, ",") + `]}`
			},
		})
		rows, err := calcomSync(t, url, "team_memberships")
		if err != nil || !slices.Equal(skips, []string{"0", "250"}) || len(rows) != 251 {
			t.Fatalf("Sync = (%d rows, %v), skips = %v", len(rows), err, skips)
		}
	})
	// Cal.com refusing a later page means the user lost the team part way; its first page alone is not the team.
	t.Run("fails a team's memberships lost part way or without rows", func(t *testing.T) {
		full := make([]string, calcomMembershipsPage)
		for i := range full {
			full[i] = fmt.Sprintf(`{"id":%d}`, i)
		}
		for name, page2 := range map[string]func(*http.Request) (int, string){
			"refused": calcomAnswer(http.StatusForbidden),
			"no rows": calcomOK(`{"status":"success"}`),
		} {
			url := calcomServer(t, map[string]func(*http.Request) (int, string){
				"/v2/teams": calcomOK(calcomTeams),
				"/v2/teams/7/memberships": func(r *http.Request) (int, string) {
					if r.URL.Query().Get("skip") == "0" {
						return http.StatusOK, `{"status":"success","data":[` + strings.Join(full, ",") + `]}`
					}
					return page2(r)
				},
			})
			if _, err := calcomSync(t, url, "team_memberships"); err == nil {
				t.Fatalf("%s second page: Sync succeeded", name)
			}
		}
	})
	t.Run("writes the user's event types, then each team's", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/teams": calcomOK(`{"status":"success","data":[{"id":7},{"id":8}]}`),
			"/v2/event-types": calcomOK(`{"status":"success","data":[{"id":3,"title":"Intro","slug":"intro","lengthInMinutes":30,
				"ownerId":1,"scheduleId":5,"users":[{"id":1,"name":"Ada"}],"locations":[{"type":"integration"}]}]}`),
			"/v2/teams/7/event-types": calcomOK(`{"status":"success","data":[{"id":4,"title":"Team sync","teamId":7,"ownerId":null,
				"parentEventTypeId":null,"schedulingType":"roundRobin","isRRWeightsEnabled":true,
				"hosts":[{"userId":1,"name":"Ada"},{"userId":2,"name":"Grace"}],"team":{"id":7,"slug":"acme"}}]}`),
			// A team deleted since it was listed has no event types.
			"/v2/teams/8/event-types": calcomAnswer(http.StatusNotFound),
		})
		rows, err := calcomSync(t, url, "event_types")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"event_type_id": 3, "title": "Intro", "slug": "intro", "length_in_minutes": 30, "owner_id": 1, "schedule_id": 5,
				"user_ids": []any{1}, "locations": []any{map[string]any{"type": "integration"}}},
			{"event_type_id": 4, "title": "Team sync", "team_id": 7, "owner_id": nil, "parent_event_type_id": nil,
				"scheduling_type": "roundRobin", "is_rr_weights_enabled": true, "host_user_ids": []any{1, 2}},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("event types = %v", rows)
		}
	})
	t.Run("writes the user's schedules", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/schedules": calcomOK(`{"status":"success","data":[{"id":5,"ownerId":1,"name":"Working hours","timeZone":"Europe/London",
				"availability":[{"days":["Monday"],"startTime":"09:00","endTime":"17:00"}],"isDefault":true,"overrides":[]}]}`),
		})
		rows, err := calcomSync(t, url, "schedules")
		want := map[string]any{"schedule_id": 5, "owner_id": 1, "name": "Working hours", "time_zone": "Europe/London", "is_default": true,
			"overrides": []any{}, "availability": []any{map[string]any{"days": []any{"Monday"}, "startTime": "09:00", "endTime": "17:00"}}}
		if err != nil || len(rows) != 1 || fmt.Sprint(rows[0]) != fmt.Sprint(want) {
			t.Fatalf("schedules = (%v, %v)", rows, err)
		}
	})
	// Discover lists only what the key reads, so a refusal mid-sync is the key losing access: the sync retries.
	for table, path := range map[string]string{
		"teams": "/v2/teams", "team_memberships": "/v2/teams", "event_types": "/v2/event-types", "schedules": "/v2/schedules", "bookings": "/v2/bookings",
	} {
		t.Run("refuses "+table+" the key no longer reads", func(t *testing.T) {
			url := calcomServer(t, map[string]func(*http.Request) (int, string){path: calcomAnswer(http.StatusForbidden)})
			_, err := calcomSync(t, url, table)
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != "this key cannot read "+path+" on Cal.com" {
				t.Fatalf("Sync error = %v, want a refusal", err)
			}
		})
	}
	// A full snapshot fails the table; the read stops there.
	t.Run("stops at the writer's error", func(t *testing.T) {
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/me":                  calcomOK(calcomMe),
			"/v2/teams":               calcomOK(calcomTeams),
			"/v2/teams/7/memberships": calcomOK(`{"status":"success","data":[{"id":70,"userId":2,"teamId":7,"user":{"uuid":"u-2"}}]}`),
			"/v2/event-types":         calcomOK(`{"status":"success","data":[{"id":3}]}`),
			"/v2/teams/7/event-types": calcomOK(`{"status":"success","data":[{"id":4}]}`),
			"/v2/schedules":           calcomOK(`{"status":"success","data":[{"id":5}]}`),
			"/v2/bookings":            calcomOK(`{"status":"success","data":[` + calcomBooking + `],"pagination":{"hasMore":false}}`),
		})
		k := kindOf(t, catalogFor(url), "calcom")
		// Each table fails at every row it would write: a team's member after the user, a team's event type after the user's.
		rows := map[string]int{"users": 2, "teams": 1, "team_memberships": 1, "event_types": 2, "schedules": 1,
			"bookings": 1, "booking_attendees": 2, "booking_hosts": 1}
		for table, n := range rows {
			for limit := range n {
				w := &calcomFullWriter{room: limit}
				_, err := k.Sync(ctx, Source{Secret: calcomTestKey}, Table{Name: table}, w)
				if !errors.Is(err, errCalcomFull) || w.rows != limit {
					t.Fatalf("%s with room for %d rows: Sync error = %v after %d rows", table, limit, err, w.rows)
				}
			}
		}
	})
	t.Run("refuses a table it has not", func(t *testing.T) {
		_, err := calcomSync(t, "http://unused", "payments")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Sync error = %v, want InvalidError", err)
		}
	})
	t.Run("retries a rate limit", func(t *testing.T) {
		noBackoff(t)
		calls := 0
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/schedules": func(*http.Request) (int, string) {
				calls++
				if calls == 1 {
					return http.StatusTooManyRequests, `{"status":"error"}`
				}
				return http.StatusOK, `{"status":"success","data":[{"id":5}]}`
			},
		})
		rows, err := calcomSync(t, url, "schedules")
		if err != nil || len(rows) != 1 || calls != 2 {
			t.Fatalf("Sync = (%v, %v) after %d calls", rows, err, calls)
		}
	})
	// Cal.com blocks a key for a minute and sends no Retry-After; the refusal says when the block ends.
	t.Run("refuses a spent rate limit with when it resets", func(t *testing.T) {
		noBackoff(t)
		calcomNoPacing(t)
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("X-RateLimit-Remaining-Default", "0")
			w.Header().Set("X-RateLimit-Reset-Default", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(server.Close)
		before := time.Now()
		_, err := calcomSync(t, server.URL, "schedules")
		after := time.Now()
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || calls != maxRetries+1 {
			t.Fatalf("Sync error = %v after %d calls, want a refusal after %d", err, calls, maxRetries+1)
		}
		resets := false
		for at := before.Add(time.Minute).Truncate(time.Second); !at.After(after.Add(time.Minute)); at = at.Add(time.Second) {
			resets = resets || refusedErr.Message == "this key has used up its Cal.com rate limit; it resets at "+at.UTC().Format("15:04:05 UTC")
		}
		if !resets {
			t.Fatalf("refusal = %q, want the reset a minute after %s", refusedErr.Message, before.UTC())
		}
	})
	// Every request of a key waits its turn, a retry and another read's included.
	t.Run("paces one key's requests", func(t *testing.T) {
		noBackoff(t)
		var (
			mu    sync.Mutex
			times []time.Time
		)
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/schedules": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				times = append(times, time.Now())
				if len(times) == 1 {
					return http.StatusTooManyRequests, `{"status":"error"}`
				}
				return http.StatusOK, `{"status":"success","data":[]}`
			},
		})
		savedEvery, savedBurst := calcomEvery, calcomBurst
		calcomEvery, calcomBurst = 40*time.Millisecond, 1
		t.Cleanup(func() { calcomEvery, calcomBurst = savedEvery, savedBurst })
		k := kindOf(t, catalogFor(url), "calcom")
		for range 2 {
			if _, err := k.Sync(ctx, Source{Secret: calcomTestKey}, Table{Name: "schedules"}, &rowSink{}); err != nil {
				t.Fatal(err)
			}
		}
		if len(times) != 3 {
			t.Fatalf("requests = %d, want 3", len(times))
		}
		for i := 1; i < len(times); i++ {
			if gap := times[i].Sub(times[i-1]); gap < 35*time.Millisecond {
				t.Fatalf("request %d came %s after the one before, want at least 40ms", i, gap)
			}
		}
	})
	// A limiter back at its full burst remembers nothing, so it goes when another key starts; one still spending stays.
	t.Run("keeps only the limiters of keys in use", func(t *testing.T) {
		savedEvery, savedBurst := calcomEvery, calcomBurst
		calcomEvery, calcomBurst = time.Hour, 1
		t.Cleanup(func() { calcomEvery, calcomBurst = savedEvery, savedBurst })
		k := &calcomKind{}
		busy := k.pace("cal_busy")
		if !busy.Allow() {
			t.Fatal("a new limiter refused its burst")
		}
		k.pace("cal_idle")
		if k.pace("cal_busy") != busy {
			t.Fatal("a key's reads do not share its limiter")
		}
		k.pace("cal_new")
		_, idle := k.paces[sha256.Sum256([]byte("cal_idle"))]
		if len(k.paces) != 2 || idle || k.paces[sha256.Sum256([]byte("cal_busy"))] != busy {
			t.Fatalf("limiters = %d, idle kept = %v", len(k.paces), idle)
		}
	})
}
