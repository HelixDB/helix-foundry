package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// calcomKeyRE accepts any cal_ prefix: Cal.com documents live keys as
// cal_live_… and test keys as cal_…, and /v2/me proves the rest.
var calcomKeyRE = regexp.MustCompile(`^cal_[A-Za-z0-9_]+$`)

// Cal.com versions each endpoint by its cal-api-version header, and answers a
// request without it as an older version, so every call names its own. /v2/me,
// /v2/teams and team memberships have no versions.
const (
	calcomBookingsVersion   = "2026-05-01"
	calcomEventTypesVersion = "2026-06-12"
	calcomSchedulesVersion  = "2024-06-11"
)

const (
	// calcomBookingsPage and calcomMembershipsPage are the most each list returns.
	calcomBookingsPage    = 100
	calcomMembershipsPage = 250
	maxCalcomPages        = 20_000
	// maxCalcomAdminProbes bounds a test's search for a team the user administers.
	maxCalcomAdminProbes = 10
)

// calcomEvery and calcomBurst pace one key's requests. Cal.com allows an API
// key 120 requests a minute and blocks it for a minute after one more, so
// requests go one every 600ms after a burst of 10: no minute sees more than
// 110. Tests replace them.
var (
	calcomEvery = 600 * time.Millisecond
	calcomBurst = 10
)

var calcomObjects = []saasTable{
	{"users", "user_id"},
	{"teams", "team_id"},
	{"team_memberships", "membership_id"},
	{"event_types", "event_type_id"},
	{"schedules", "schedule_id"},
	// A booking is keyed by its uid, which reschedules point at; its numeric id is booking_id.
	{"bookings", "booking_uid"},
	// Attendees and hosts come within their booking; Discover keys them by
	// (booking_uid, email) and (booking_uid, user_id), as attendees have no id.
	{"booking_attendees", "booking_uid"},
	{"booking_hosts", "booking_uid"},
}

var calcomForeignKeys = map[string][]ForeignKey{
	// An organization is a team in Cal.com.
	"users":            {foreignKey("default_schedule_id", "schedules", "schedule_id"), foreignKey("organization_id", "teams", "team_id")},
	"teams":            {foreignKey("parent_id", "teams", "team_id")},
	"team_memberships": {foreignKey("team_id", "teams", "team_id"), foreignKey("user_id", "users", "user_id")},
	"event_types": {foreignKey("owner_id", "users", "user_id"), foreignKey("team_id", "teams", "team_id"),
		foreignKey("parent_event_type_id", "event_types", "event_type_id"), foreignKey("schedule_id", "schedules", "schedule_id")},
	"schedules": {foreignKey("owner_id", "users", "user_id")},
	"bookings": {foreignKey("event_type_id", "event_types", "event_type_id"),
		foreignKey("rescheduled_from_uid", "bookings", "booking_uid"), foreignKey("rescheduled_to_uid", "bookings", "booking_uid")},
	"booking_attendees": {foreignKey("booking_uid", "bookings", "booking_uid")},
	"booking_hosts":     {foreignKey("booking_uid", "bookings", "booking_uid"), foreignKey("user_id", "users", "user_id")},
}

type calcomKind struct {
	base string
	http *http.Client

	mu sync.Mutex
	// paces holds the limiter of each key in use, by the key's hash.
	paces map[[sha256.Size]byte]*rate.Limiter
}

func (k *calcomKind) Name() string        { return "calcom" }
func (k *calcomKind) DisplayName() string { return "Cal.com" }

func (k *calcomKind) Prepare(src Source) (json.RawMessage, error) {
	if !calcomKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Cal.com API key (cal_live_… or cal_…)")
	}
	if err := emptySettings(src.Settings); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// Discover proves the key on /v2/me. A key reads everything its user can, so
// only teams can be out of reach: when Cal.com won't list the key's teams,
// teams and their memberships are unavailable. Only a team's admins list its
// members, so a test also warns when the user administers none of its teams;
// a sync skips that probe, one request per team.
func (k *calcomKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	if _, err := k.me(ctx, src.Secret); err != nil {
		return Discovery{}, err
	}
	teams, teamsReadable, err := k.teams(ctx, src.Secret)
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(calcomObjects, func(name string) bool {
		return teamsReadable || name != "teams" && name != "team_memberships"
	}, calcomForeignKeys)
	for i := range d.Tables {
		switch d.Tables[i].Name {
		case "booking_attendees":
			d.Tables[i].PrimaryKey = []string{"booking_uid", "email"}
		case "booking_hosts":
			d.Tables[i].PrimaryKey = []string{"booking_uid", "user_id"}
		}
	}
	if src.ForSync || len(teams) == 0 {
		return d, nil
	}
	// The first teams only: past them the probes would hold up the test for the pace.
	for _, team := range teams[:min(len(teams), maxCalcomAdminProbes)] {
		path := fmt.Sprintf("/v2/teams/%d/memberships", int64Of(team["id"]))
		_, admin, err := k.list(ctx, src.Secret, path, "", url.Values{"take": {"1"}})
		if err != nil {
			return Discovery{}, err
		}
		if admin {
			return d, nil
		}
	}
	d.Warnings = append(d.Warnings, "this key's user is not an admin of its Cal.com teams, so team_memberships and users "+
		"leave out the other members; use a team admin's key to read them")
	return d, nil
}

func (k *calcomKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 2) }

func (k *calcomKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	key := src.Secret
	write := func(row map[string]any) error { return w.Write(normalizeCalcom(table.Name, row)) }
	switch table.Name {
	case "users":
		return table, k.users(ctx, key, write)
	case "teams", "team_memberships":
		teams, ok, err := k.teams(ctx, key)
		switch {
		case err != nil:
			return table, err
		case !ok:
			return table, refused(nil, "this key cannot read /v2/teams on Cal.com")
		case table.Name == "teams":
			return table, calcomWriteAll(teams, write)
		}
		return table, k.memberships(ctx, key, teams, write)
	case "event_types":
		return table, k.eventTypes(ctx, key, write)
	case "schedules":
		page, ok, err := k.list(ctx, key, "/v2/schedules", calcomSchedulesVersion, nil)
		switch {
		case err != nil:
			return table, err
		case !ok:
			return table, refused(nil, "this key cannot read /v2/schedules on Cal.com")
		}
		return table, calcomWriteAll(*page.Data, write)
	case "bookings", "booking_attendees", "booking_hosts":
		return table, k.bookings(ctx, key, nil, func(booking map[string]any) error {
			for _, row := range calcomBookingRows(table.Name, booking) {
				if err := w.Write(row); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return table, invalid("Cal.com has no object %q", table.Name)
}

// calcomWriteAll writes each row in order.
func calcomWriteAll(rows []map[string]any, write func(map[string]any) error) error {
	for _, row := range rows {
		if err := write(row); err != nil {
			return err
		}
	}
	return nil
}

// users writes the key's own user, then each other member of the teams it
// administers, once: only a team's admins list its members.
func (k *calcomKind) users(ctx context.Context, key string, write func(map[string]any) error) error {
	me, err := k.me(ctx, key)
	if err != nil {
		return err
	}
	if err := write(me); err != nil {
		return err
	}
	teams, _, err := k.teams(ctx, key)
	if err != nil {
		return err
	}
	seen := map[any]bool{me["id"]: true}
	return k.memberships(ctx, key, teams, func(membership map[string]any) error {
		user, _ := membership["user"].(map[string]any)
		if user == nil || seen[membership["userId"]] {
			return nil
		}
		seen[membership["userId"]] = true
		row := maps.Clone(user)
		row["id"] = membership["userId"]
		return write(row)
	})
}

// memberships lists each team's memberships by take and skip. A team whose
// members the key's user may not list, as it does not administer it, is skipped.
func (k *calcomKind) memberships(ctx context.Context, key string, teams []map[string]any, each func(map[string]any) error) error {
	for _, team := range teams {
		path := fmt.Sprintf("/v2/teams/%d/memberships", int64Of(team["id"]))
		for skip, pages := 0, 0; ; pages++ {
			if pages == maxCalcomPages {
				return fmt.Errorf("cal.com %s: more than %d pages", path, maxCalcomPages)
			}
			query := url.Values{"take": {strconv.Itoa(calcomMembershipsPage)}, "skip": {strconv.Itoa(skip)}}
			page, ok, err := k.list(ctx, key, path, "", query)
			if err != nil {
				return err
			}
			// Losing access part way would leave the team half listed.
			if !ok && skip > 0 {
				return refused(nil, "Cal.com stopped listing %s part way; try again", path)
			}
			if !ok {
				break
			}
			if err := calcomWriteAll(*page.Data, each); err != nil {
				return err
			}
			if len(*page.Data) < calcomMembershipsPage {
				break
			}
			skip += len(*page.Data)
		}
	}
	return nil
}

// eventTypes lists the user's own event types, then each team's: hidden ones
// only where the user is a team admin.
func (k *calcomKind) eventTypes(ctx context.Context, key string, write func(map[string]any) error) error {
	page, ok, err := k.list(ctx, key, "/v2/event-types", calcomEventTypesVersion, nil)
	switch {
	case err != nil:
		return err
	case !ok:
		return refused(nil, "this key cannot read /v2/event-types on Cal.com")
	}
	if err := calcomWriteAll(*page.Data, write); err != nil {
		return err
	}
	teams, _, err := k.teams(ctx, key)
	if err != nil {
		return err
	}
	for _, team := range teams {
		page, ok, err := k.list(ctx, key, fmt.Sprintf("/v2/teams/%d/event-types", int64Of(team["id"])), calcomEventTypesVersion, nil)
		if err != nil {
			return err
		}
		// A team gone since it was listed has none.
		if !ok {
			continue
		}
		if err := calcomWriteAll(*page.Data, write); err != nil {
			return err
		}
	}
	return nil
}

// bookings pages /v2/bookings by cursor through every status, with query's
// filters.
func (k *calcomKind) bookings(ctx context.Context, key string, query url.Values, each func(map[string]any) error) error {
	q := url.Values{"limit": {strconv.Itoa(calcomBookingsPage)}}
	for name, values := range query {
		q[name] = values
	}
	for pages := 0; ; pages++ {
		if pages == maxCalcomPages {
			return fmt.Errorf("cal.com /v2/bookings: more than %d pages", maxCalcomPages)
		}
		page, ok, err := k.list(ctx, key, "/v2/bookings", calcomBookingsVersion, q)
		// An incomplete page fails the attempt; ending early would publish a partial table.
		switch {
		case err != nil:
			return err
		case !ok:
			return refused(nil, "this key cannot read /v2/bookings on Cal.com")
		case page.Pagination == nil:
			return fmt.Errorf("cal.com /v2/bookings: the response has no pagination")
		case page.Pagination.HasMore && page.Pagination.NextCursor == "":
			return fmt.Errorf("cal.com /v2/bookings: a page says more follow but gives no cursor")
		}
		if err := calcomWriteAll(*page.Data, each); err != nil {
			return err
		}
		if !page.Pagination.HasMore {
			return nil
		}
		q.Set("cursor", page.Pagination.NextCursor)
	}
}

// me reads the key's own user.
func (k *calcomKind) me(ctx context.Context, key string) (map[string]any, error) {
	status, body, err := k.get(ctx, key, "/v2/me", "", nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, refused(nil, "Cal.com returned HTTP %d for /v2/me; try again", status)
	}
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode cal.com /v2/me: %w", err)
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("cal.com /v2/me: the response has no user")
	}
	return resp.Data, nil
}

// teams lists the key's teams; ok is false when Cal.com won't list them for it.
func (k *calcomKind) teams(ctx context.Context, key string) ([]map[string]any, bool, error) {
	page, ok, err := k.list(ctx, key, "/v2/teams", "", nil)
	if err != nil || !ok {
		return nil, ok, err
	}
	return *page.Data, true, nil
}

// calcomPage is one list answer; only a cursor-paged list has pagination.
type calcomPage struct {
	Data       *[]map[string]any `json:"data"`
	Pagination *struct {
		NextCursor string `json:"nextCursor"`
		HasMore    bool   `json:"hasMore"`
	} `json:"pagination"`
}

// list reads one page of path. A 403 or 404 is a list the key may not read
// and returns ok false. A page without its rows fails the read rather than
// ending it: a short read would publish part of the table.
func (k *calcomKind) list(ctx context.Context, key, path, version string, query url.Values) (calcomPage, bool, error) {
	var page calcomPage
	status, body, err := k.get(ctx, key, path, version, query)
	switch {
	case err != nil:
		return page, false, err
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return page, false, nil
	case status >= 300:
		return page, false, refused(nil, "Cal.com returned HTTP %d for %s; try again", status, path)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return page, false, fmt.Errorf("decode cal.com %s: %w", path, err)
	}
	if page.Data == nil {
		return page, false, fmt.Errorf("cal.com %s: the response has no data", path)
	}
	return page, true, nil
}

// get sends one paced request with the version header its endpoint needs. A
// rejected key and a spent rate limit are refused here, the limit with when
// Cal.com lifts it, so the sync retries rather than failing for good.
func (k *calcomKind) get(ctx context.Context, key, path, version string, query url.Values) (int, []byte, error) {
	key = strings.TrimSpace(key)
	target := k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	pace := k.pace(key)
	status, header, body, err := fetchWithHeader(ctx, k.http, func() (*http.Request, error) {
		// Retries wait their turn too: Cal.com counts every request against the key.
		if err := pace.Wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = http.Header{"Authorization": {"Bearer " + key}}
			if version != "" {
				req.Header.Set("cal-api-version", version)
			}
		}
		return req, err
	})
	if err != nil {
		return 0, nil, refused(err, "could not reach Cal.com; try again")
	}
	switch status {
	case http.StatusUnauthorized:
		return 0, nil, refused(nil, "Cal.com rejected this key; check it and enter it again")
	case http.StatusTooManyRequests:
		// Cal.com sends no Retry-After; the reset header is the seconds until the key's block ends.
		resets := "later"
		if seconds, err := strconv.Atoi(header.Get("X-RateLimit-Reset-Default")); err == nil && seconds > 0 {
			resets = "at " + time.Now().Add(time.Duration(seconds)*time.Second).UTC().Format("15:04:05 UTC")
		}
		return 0, nil, refused(nil, "this key has used up its Cal.com rate limit; it resets %s", resets)
	}
	return status, body, nil
}

// pace is key's limiter, shared by every read of the key in this process; a
// key used elsewhere at the same time can still spend the limit, and a 429
// then refuses the read. A limiter back at its full burst remembers nothing,
// so the next new key drops it and only keys in use stay.
func (k *calcomKind) pace(key string) *rate.Limiter {
	id := sha256.Sum256([]byte(key))
	k.mu.Lock()
	defer k.mu.Unlock()
	if l, ok := k.paces[id]; ok {
		return l
	}
	for other, l := range k.paces {
		if l.Tokens() >= float64(l.Burst()) {
			delete(k.paces, other)
		}
	}
	if k.paces == nil {
		k.paces = map[[sha256.Size]byte]*rate.Limiter{}
	}
	l := rate.NewLimiter(rate.Every(calcomEvery), calcomBurst)
	k.paces[id] = l
	return l
}

// calcomBookingRows is what a booking holds in table: itself, or its
// attendees or hosts, each tied to it by booking_uid.
func calcomBookingRows(table string, booking map[string]any) []map[string]any {
	if table == "bookings" {
		return []map[string]any{normalizeCalcom(table, booking)}
	}
	field := "attendees"
	if table == "booking_hosts" {
		field = "hosts"
	}
	items, _ := booking[field].([]any)
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			row := normalizeCalcom(table, m)
			row["booking_uid"] = booking["uid"]
			rows = append(rows, row)
		}
	}
	return rows
}

// calcomIDColumn is the column each table's id lands in; a host's id is its user's.
var calcomIDColumn = map[string]string{
	"users": "user_id", "teams": "team_id", "team_memberships": "membership_id", "event_types": "event_type_id",
	"schedules": "schedule_id", "bookings": "booking_id", "booking_attendees": "attendee_id", "booking_hosts": "user_id",
}

// normalizeCalcom renames id to the table's id column, keeps the ids of an
// event type's users and hosts and of a booking's event type, drops objects
// another table holds and writes field names in snake_case.
func normalizeCalcom(table string, row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for field, value := range row {
		switch {
		case field == "id":
			out[calcomIDColumn[table]] = value
		case table == "bookings" && field == "uid":
			out["booking_uid"] = value
		// A booking's attendees and hosts are child tables; a membership's user,
		// an event type's team and the user's organization are rows of users and teams.
		case table == "bookings" && (field == "attendees" || field == "hosts"),
			field == "user", field == "team", field == "organization":
		// eventTypeId is deprecated for eventType's id, read below.
		case table == "bookings" && (field == "eventType" || field == "eventTypeId"):
		case table == "event_types" && field == "users":
			out["user_ids"] = githubField(value, "id")
		case table == "event_types" && field == "hosts":
			out["host_user_ids"] = githubField(value, "userId")
		default:
			// Round robin's RR stays one word: isRRWeightsEnabled is is_rr_weights_enabled.
			out[snakeCase(strings.ReplaceAll(field, "RR", "Rr"))] = value
		}
	}
	if table == "bookings" {
		out["event_type_id"] = row["eventTypeId"]
		if eventType, ok := row["eventType"].(map[string]any); ok {
			out["event_type_id"] = eventType["id"]
		}
	}
	return out
}
