package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const slackTestToken = "xoxb-1234-5678-abcdefABCD"

// slackTestNow is the clock of every test read: with history_days 2, the
// window starts at 2026-10-27 00:00 UTC.
var slackTestNow = time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)

// slackTS is Slack's ts for an RFC 3339 time.
func slackTS(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// slackPage is a last page of a Slack list: items of field.
func slackPage(field string, items ...string) string {
	return fmt.Sprintf(`{"ok":true,%q:[%s],"response_metadata":{"next_cursor":""}}`, field, strings.Join(items, ","))
}

// slackMore is a page of items of field that cursor follows.
func slackMore(field, cursor string, items ...string) string {
	return fmt.Sprintf(`{"ok":true,%q:[%s],"has_more":true,"response_metadata":{"next_cursor":%q}}`, field, strings.Join(items, ","), cursor)
}

func slackError(code string) string { return fmt.Sprintf(`{"ok":false,"error":%q}`, code) }

func slackScope(needed string) string {
	return fmt.Sprintf(`{"ok":false,"error":"missing_scope","needed":%q,"provided":"users:read"}`, needed)
}

// slackCalls are a test Slack's calls, as "method?query", in order.
type slackCalls struct {
	mu  sync.Mutex
	log []string
}

func (c *slackCalls) add(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, call)
}

// of are the queries of method's calls.
func (c *slackCalls) of(method string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var queries []string
	for _, call := range c.log {
		if m, query, _ := strings.Cut(call, "?"); m == method {
			queries = append(queries, query)
		}
	}
	return queries
}

// slackAllScopes are the scopes the setup guide asks for.
const slackAllScopes = "channels:read,channels:history,groups:read,groups:history,users:read,users:read.email"

// slackAt is slackScoped with every scope the guide asks for.
func slackAt(t *testing.T, routes map[string]func(*http.Request) (int, string)) (*slackKind, *slackCalls) {
	t.Helper()
	return slackScoped(t, slackAllScopes, routes)
}

// slackScoped serves routes by Web API method, each answer saying the token
// has scopes, as Slack's X-OAuth-Scopes header does (none for ""), and checking each call
// is a GET with the token. It returns the slack kind calling them, unpaced
// and with the clock at slackTestNow, and its calls.
func slackScoped(t *testing.T, scopes string, routes map[string]func(*http.Request) (int, string)) (*slackKind, *slackCalls) {
	t.Helper()
	savedIntervals, savedNow := slackIntervals, slackNow
	slackIntervals = map[string]time.Duration{}
	for method := range savedIntervals {
		slackIntervals[method] = 0
	}
	slackNow = func() time.Time { return slackTestNow }
	t.Cleanup(func() { slackIntervals, slackNow = savedIntervals, savedNow })
	calls := &slackCalls{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		handler, ok := routes[method]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+slackTestToken {
			t.Errorf("%s %s with Authorization %q", r.Method, method, r.Header.Get("Authorization"))
		}
		calls.add(method + "?" + r.URL.RawQuery)
		status, body := handler(r)
		if scopes != "" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return kindOf(t, NewCatalog(Endpoints{Slack: server.URL + "/api"}, Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "slack").(*slackKind), calls
}

// slackWorkspace is the workspace a test serves: its users and channels,
// the channels the app is in, each channel's members and messages (newest
// first, as Slack lists them, whatever the window), and each thread's
// replies by channel:ts. A test may change it between reads.
type slackWorkspace struct {
	users, channels, joined []string
	members, history        map[string][]string
	replies                 map[string][]string
}

// The times of newSlackWorkspace's messages that tests look up.
var (
	slackParent = slackTS("2026-10-28T09:15:00.000100Z")
	slackLunch  = slackTS("2026-10-28T16:00:00.000300Z")
)

// newSlackWorkspace is Acme: Ada, Grace, Alan (deleted) and a bot; the app
// is in #general and #design but not #old. #general holds the 27th and
// 28th of October: a thread on the 28th with replies into the 29th, one
// of them broadcast to the channel, system messages, a bot's message, a
// broadcast from a thread before the window and a message before it.
// #design holds the 29th: mentions of every kind and authors without a user.
func newSlackWorkspace() *slackWorkspace {
	parent := fmt.Sprintf(`{"type":"message","user":"U1","ts":%q,"thread_ts":%q,"reply_count":3,"reply_users_count":2,"latest_reply":%q,`+
		`"edited":{"user":"U1","ts":%q},"text":"Morning <@U2>, see <#C2|design> and <https://example.com/a?b=1&amp;c=2|the doc> or <https://example.com>\n&lt;3 &amp; more"}`,
		slackParent, slackParent, slackTS("2026-10-29T08:00:00Z"), slackTS("2026-10-28T09:16:00Z"))
	broadcast := fmt.Sprintf(`{"type":"message","subtype":"thread_broadcast","user":"U2","text":"Shipping it","ts":%q,"thread_ts":%q}`,
		slackTS("2026-10-28T10:05:00Z"), slackParent)
	return &slackWorkspace{
		users: []string{
			`{"id":"U1","team_id":"T1","name":"ada","real_name":"Ada Lovelace","tz":"Europe/London","is_admin":true,"is_owner":true,"updated":1792000000,
				"profile":{"real_name":"Ada Lovelace","display_name":"ada","email":"ada@example.com","title":"Engineer"}}`,
			`{"id":"U2","team_id":"T1","name":"grace","real_name":"","profile":{"real_name":"Grace Hopper","display_name":"","title":""}}`,
			`{"id":"U3","team_id":"T1","name":"alan","deleted":true,"profile":{}}`,
			`{"id":"UB","team_id":"T1","name":"deploy","real_name":"Deploy Bot","is_bot":true,"profile":{"real_name":"Deploy Bot"}}`,
		},
		channels: []string{
			`{"id":"C1","name":"general","is_channel":true,"is_private":false,"is_archived":false,"is_member":true,"created":1700000000,"creator":"U1",
				"topic":{"value":"Company news","creator":"U1","last_set":1700000001},"purpose":{"value":"Everything","creator":"U1","last_set":0},"num_members":3}`,
			`{"id":"C2","name":"design","is_private":true,"is_member":true,"created":1700000100,"creator":"U2","topic":{"value":""},"purpose":{"value":""},"num_members":2}`,
			`{"id":"C3","name":"old","is_archived":true,"is_member":false,"created":1600000000}`,
		},
		joined:  []string{`{"id":"C1","name":"general","is_member":true}`, `{"id":"C2","name":"design","is_private":true,"is_member":true}`},
		members: map[string][]string{"C1": {`"U1"`, `"U2"`, `"U3"`}, "C2": {`"U1"`, `"U2"`}},
		history: map[string][]string{
			"C1": {
				fmt.Sprintf(`{"type":"message","user":"U3","text":"Lunch? <!here>","ts":%q}`, slackLunch),
				fmt.Sprintf(`{"type":"message","subtype":"channel_join","user":"U3","text":"<@U3> has joined the channel","ts":%q}`, slackTS("2026-10-28T15:30:00Z")),
				broadcast,
				parent,
				fmt.Sprintf(`{"type":"message","bot_id":"B9","bot_profile":{"name":"Deploy Bot"},"text":"Deployed v2","ts":%q}`, slackTS("2026-10-27T12:00:00Z")),
				fmt.Sprintf(`{"type":"message","subtype":"thread_broadcast","user":"U1","text":"Old thread news","ts":%q,"thread_ts":%q}`,
					slackTS("2026-10-27T11:00:00Z"), slackTS("2026-10-26T10:00:00Z")),
				fmt.Sprintf(`{"type":"message","user":"U1","text":"Before the window","ts":%q}`, slackTS("2026-10-26T23:59:59.999999Z")),
			},
			"C2": {
				fmt.Sprintf(`{"user":"U4","text":"<@U9> what about <#C7>, <#C1> and <#C2|>? <@U1|ada> <@U8|bob> <!channel> <!everyone> `+
					`<!subteam^S1|@eng> <!subteam^S2> <!date^1792000000^{date}|Oct 1> <mailto:a@b.co|a@b.co>","ts":%q}`, slackTS("2026-10-29T14:00:00Z")),
				fmt.Sprintf(`{"bot_id":"B8","username":"Zapier","text":"","ts":%q}`, slackTS("2026-10-29T13:00:00Z")),
				fmt.Sprintf(`{"bot_id":"B7","text":"beep","ts":%q}`, slackTS("2026-10-29T12:00:00Z")),
				fmt.Sprintf(`{"text":"hmm","ts":%q}`, slackTS("2026-10-29T11:00:00Z")),
			},
		},
		replies: map[string][]string{"C1:" + slackParent: {
			parent,
			fmt.Sprintf(`{"user":"U1","text":"Follow-up next day","ts":%q,"thread_ts":%q}`, slackTS("2026-10-29T08:00:00Z"), slackParent),
			fmt.Sprintf(`{"user":"U2","text":"Thanks!\nSecond line","ts":%q,"thread_ts":%q}`, slackTS("2026-10-28T09:20:00Z"), slackParent),
			broadcast,
			fmt.Sprintf(`{"subtype":"bot_add","user":"U1","text":"added an integration","ts":%q,"thread_ts":%q}`, slackTS("2026-10-28T10:30:00Z"), slackParent),
		}},
	}
}

// ofTypes are the channels of the types r asks for, as Slack lists them.
func ofTypes(r *http.Request, channels []string) []string {
	types := strings.Split(r.URL.Query().Get("types"), ",")
	return slices.DeleteFunc(slices.Clone(channels), func(raw string) bool {
		var ch slackChannel
		_ = json.Unmarshal([]byte(raw), &ch)
		return !slices.Contains(types, map[bool]string{false: "public_channel", true: "private_channel"}[ch.IsPrivate])
	})
}

func (w *slackWorkspace) routes() map[string]func(*http.Request) (int, string) {
	return map[string]func(*http.Request) (int, string){
		"auth.test": func(*http.Request) (int, string) {
			return http.StatusOK, `{"ok":true,"url":"https://acme.slack.com/","team":"Acme","user":"foundry","team_id":"T1","user_id":"UF","bot_id":"BF"}`
		},
		"users.list": func(*http.Request) (int, string) { return http.StatusOK, slackPage("members", w.users...) },
		"conversations.list": func(r *http.Request) (int, string) {
			return http.StatusOK, slackPage("channels", ofTypes(r, w.channels)...)
		},
		"users.conversations": func(r *http.Request) (int, string) {
			return http.StatusOK, slackPage("channels", ofTypes(r, w.joined)...)
		},
		"conversations.members": func(r *http.Request) (int, string) {
			return http.StatusOK, slackPage("members", w.members[r.URL.Query().Get("channel")]...)
		},
		"conversations.history": func(r *http.Request) (int, string) {
			return http.StatusOK, slackPage("messages", w.history[r.URL.Query().Get("channel")]...)
		},
		"conversations.replies": func(r *http.Request) (int, string) {
			return http.StatusOK, slackPage("messages", w.replies[r.URL.Query().Get("channel")+":"+r.URL.Query().Get("ts")]...)
		},
	}
}

// with is routes with answers in place of some.
func with(routes map[string]func(*http.Request) (int, string), answers map[string]string) map[string]func(*http.Request) (int, string) {
	for method, answer := range answers {
		routes[method] = func(*http.Request) (int, string) { return http.StatusOK, answer }
	}
	return routes
}

// slackSource is a prepared source whose window is two days and today.
var slackSource = Source{Secret: slackTestToken, Settings: json.RawMessage(`{"history_days":2}`)}

var slackTables = []Table{{Name: "users"}, {Name: "channels"}, {Name: "channel_members"}, {Name: "messages"}, {Name: "channel_days"}}

// slackRead reads tables in one SyncPass, into a sink per table.
func slackRead(t *testing.T, kind *slackKind, tables []Table, docs DocumentWriter) (map[string]*rowSink, error) {
	t.Helper()
	sinks, rows := map[string]*rowSink{}, map[string]RowWriter{}
	for _, table := range tables {
		sinks[table.Name] = &rowSink{}
		rows[table.Name] = sinks[table.Name]
	}
	written, err := kind.SyncPass(context.Background(), slackSource, tables, rows, docs)
	if fmt.Sprint(written) != fmt.Sprint(tables) {
		t.Errorf("SyncPass wrote %v, want %v", written, tables)
	}
	return sinks, err
}

func TestSlackConnector(t *testing.T) {
	ctx := context.Background()
	kind := kindOf(t, catalogFor("http://unused"), "slack")
	// A user token reads as its person, direct messages included.
	for _, secret := range []string{"", "xoxp-1234-5678-abcdef", "xoxb-", "xoxb-abc def", "Bearer xoxb-abc", "xoxa-2-abc", "xoxb_abc", "sk_live_abc"} {
		t.Run(fmt.Sprintf("rejects %q as a token", secret), func(t *testing.T) {
			_, err := kind.Prepare(Source{Secret: secret})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "enter a Slack bot token (xoxb-…)" {
				t.Fatalf("Prepare error = %v, want InvalidError", err)
			}
		})
	}
	t.Run("keeps 90 days of history unless told otherwise", func(t *testing.T) {
		for raw, want := range map[string]string{
			"": `{"history_days":90}`, `null`: `{"history_days":90}`, `{}`: `{"history_days":90}`, `{"history_days":0}`: `{"history_days":90}`,
			`{"history_days":1}`: `{"history_days":1}`, `{"history_days":365}`: `{"history_days":365}`,
		} {
			settings, err := kind.Prepare(Source{Secret: " " + slackTestToken + "\n", Settings: json.RawMessage(raw)})
			if err != nil || string(settings) != want {
				t.Errorf("Prepare(%s) = (%s, %v), want %s", raw, settings, err, want)
			}
		}
	})
	t.Run("refuses history outside 1 to 365 days", func(t *testing.T) {
		for _, raw := range []string{`{"history_days":-1}`, `{"history_days":366}`} {
			_, err := kind.Prepare(Source{Secret: slackTestToken, Settings: json.RawMessage(raw)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "keep 1 to 365 days of history" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
	})
	t.Run("takes only its token and its history days", func(t *testing.T) {
		for _, raw := range []string{`{"tables":["users"]}`, `{"history_days":"7"}`, `{"history_days":1.5}`, `[]`, `"90"`} {
			_, err := kind.Prepare(Source{Secret: slackTestToken, Settings: json.RawMessage(raw)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "this connector takes only its token and how many days of history to keep" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
	})
	t.Run("hints at a token by its type and last four characters", func(t *testing.T) {
		if hint := kind.Hint(" " + slackTestToken + " "); hint != "xoxb…ABCD" || kind.DisplayName() != "Slack" {
			t.Fatalf("Hint = %q, DisplayName = %q", hint, kind.DisplayName())
		}
		if hint := kind.Hint("xoxb-1"); hint != "…" {
			t.Fatalf("Hint of a short token = %q", hint)
		}
	})
	t.Run("keeps the conversations of its channel days", func(t *testing.T) {
		streams := kind.(DocumentKind).Streams(slackSource)
		if len(streams) != 1 || streams[0].Name != "conversations" || streams[0].Table != "channel_days" || streams[0].Title != "Conversations" ||
			streams[0].Format != Markdown || streams[0].Keep != (Keep{}) || streams[0].Problem() != "" {
			t.Fatalf("streams = %+v", streams)
		}
	})

	t.Run("lists every object the token reads, with keys, foreign keys and windows", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken, ForSync: true})
		if err != nil {
			t.Fatal(err)
		}
		want := []Table{
			{Name: "users", PrimaryKey: []string{"user_id"}},
			{Name: "channels", PrimaryKey: []string{"channel_id"}, ForeignKeys: []ForeignKey{foreignKey("creator_id", "users", "user_id")}},
			{Name: "channel_members", PrimaryKey: []string{"channel_id", "user_id"},
				ForeignKeys: []ForeignKey{foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id")}},
			{Name: "messages", PrimaryKey: []string{"message_id"}, Windowed: true,
				ForeignKeys: []ForeignKey{foreignKey("channel_id", "channels", "channel_id"), foreignKey("user_id", "users", "user_id")}},
			{Name: "channel_days", PrimaryKey: []string{"channel_day_id"}, Windowed: true, ForeignKeys: []ForeignKey{foreignKey("channel_id", "channels", "channel_id")}},
		}
		if fmt.Sprint(d.Tables) != fmt.Sprint(want) || len(d.Unavailable) != 0 || len(d.Warnings) != 0 {
			t.Fatalf("discovery = %+v", d)
		}
		types := "types=public_channel%2Cprivate_channel"
		for method, want := range map[string][]string{
			"auth.test": {""}, "users.list": {"limit=1"}, "conversations.list": {"limit=1&" + types},
			"users.conversations": {"limit=200&" + types}, "conversations.history": {"channel=C1&limit=1", "channel=C2&limit=1"},
		} {
			if got := calls.of(method); !slices.Equal(got, want) {
				t.Errorf("%s calls = %q, want %q", method, got, want)
			}
		}
	})
	// A scope the app lacks makes what needs it unavailable, saying which
	// scope to add; foreign keys into what is unavailable are dropped.
	for name, tc := range map[string]struct {
		answers     map[string]string
		tables      []string
		unavailable []Unavailable
	}{
		"users": {map[string]string{"users.list": slackScope("users:read")},
			[]string{"channels", "channel_members", "messages", "channel_days"},
			[]Unavailable{{"users", "add the users:read scope to the app and reinstall it"}}},
		"channels": {map[string]string{"conversations.list": slackScope("channels:read"), "users.conversations": slackScope("channels:read")},
			[]string{"users"}, []Unavailable{
				{"channels", "add the channels:read scope to the app and reinstall it"},
				{"channel_members", "add the channels:read scope to the app and reinstall it"},
				{"messages", "add the channels:read scope to the app and reinstall it"},
				{"channel_days", "add the channels:read scope to the app and reinstall it"},
			}},
		"the channels it is in": {map[string]string{"users.conversations": slackScope("groups:read")},
			[]string{"users", "channels"}, []Unavailable{
				{"channel_members", "add the groups:read scope to the app and reinstall it"},
				{"messages", "add the groups:read scope to the app and reinstall it"},
				{"channel_days", "add the groups:read scope to the app and reinstall it"},
			}},
		// Members, messages and days need channels too, and say why.
		"channels, though it reads the channels it is in": {map[string]string{"conversations.list": slackScope("groups:read")},
			[]string{"users"}, []Unavailable{
				{"channels", "add the groups:read scope to the app and reinstall it"},
				{"channel_members", "add the groups:read scope to the app and reinstall it"},
				{"messages", "add the groups:read scope to the app and reinstall it"},
				{"channel_days", "add the groups:read scope to the app and reinstall it"},
			}},
		// conversations.members needs only the scopes that list channels.
		"history": {map[string]string{"conversations.history": slackScope("channels:history")},
			[]string{"users", "channels", "channel_members"}, []Unavailable{
				{"messages", "add the channels:history scope to the app and reinstall it"},
				{"channel_days", "add the channels:history scope to the app and reinstall it"},
			}},
		"a scope Slack does not name": {map[string]string{"users.list": `{"ok":false,"error":"missing_scope"}`},
			[]string{"channels", "channel_members", "messages", "channel_days"},
			[]Unavailable{{"users", "add the missing scope to the app and reinstall it"}}},
	} {
		t.Run("reports what the app has no scope for: "+name, func(t *testing.T) {
			kind, _ := slackAt(t, with(newSlackWorkspace().routes(), tc.answers))
			d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(tableNames(d), tc.tables) || !slices.Equal(d.Unavailable, tc.unavailable) || len(d.Warnings) != 0 {
				t.Fatalf("tables = %v, unavailable = %q, warnings = %q", tableNames(d), d.Unavailable, d.Warnings)
			}
			if name != "users" {
				return
			}
			if channels := findTable(t, d, "channels"); len(channels.ForeignKeys) != 0 {
				t.Fatalf("channels = %+v", channels)
			}
			if members := findTable(t, d, "channel_members"); len(members.ForeignKeys) != 1 || members.ForeignKeys[0].ReferencedTable != "channels" {
				t.Fatalf("channel_members = %+v", members)
			}
		})
	}
	// Private channels need the groups: scopes, which the guide asks for only
	// for them, and each kind of channel's history its own scope: what the
	// token's scopes leave out is never asked for.
	without := func(scopes ...string) string {
		return strings.Join(slices.DeleteFunc(strings.Split(slackAllScopes, ","), func(s string) bool { return slices.Contains(scopes, s) }), ",")
	}
	for name, tc := range map[string]struct {
		scopes            string
		joined            []string // the channels the app is in, of newSlackWorkspace's; nil for all
		types             string
		history, warnings []string
		unavailable       []Unavailable
	}{
		"no private channel without groups:read": {scopes: without("groups:read", "groups:history"), types: "public_channel",
			history: []string{"channel=C1&limit=1"}},
		"private channels without groups:history": {scopes: without("groups:history"), types: "public_channel,private_channel",
			history: []string{"channel=C1&limit=1"}, warnings: []string{
				"The app cannot read the messages of the private channels it is in: add the groups:history scope to the app and reinstall it, then test again"}},
		"public channels without channels:history": {scopes: without("channels:history"), types: "public_channel,private_channel",
			history: []string{"channel=C2&limit=1"}, warnings: []string{
				"The app cannot read the messages of the public channels it is in: add the channels:history scope to the app and reinstall it, then test again"}},
		"only private channels, without groups:history": {scopes: without("groups:history"), joined: []string{"C2"}, types: "public_channel,private_channel",
			unavailable: []Unavailable{
				{"messages", "add the groups:history scope to the app and reinstall it"},
				{"channel_days", "add the groups:history scope to the app and reinstall it"},
			}},
		// Slack not listing the scopes leaves the calls to find out.
		"every channel when Slack does not list the scopes": {types: "public_channel,private_channel",
			history: []string{"channel=C1&limit=1", "channel=C2&limit=1"}},
	} {
		t.Run("reads the channels its scopes allow: "+name, func(t *testing.T) {
			w := newSlackWorkspace()
			if tc.joined != nil {
				w.joined = slices.DeleteFunc(w.joined, func(raw string) bool {
					return !slices.ContainsFunc(tc.joined, func(id string) bool { return strings.Contains(raw, `"id":"`+id+`"`) })
				})
			}
			kind, calls := slackScoped(t, tc.scopes, w.routes())
			d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
			if err != nil {
				t.Fatal(err)
			}
			wantTables := []string{"users", "channels", "channel_members", "messages", "channel_days"}
			if len(tc.unavailable) > 0 {
				wantTables = wantTables[:3]
			}
			if !slices.Equal(tableNames(d), wantTables) || !slices.Equal(d.Unavailable, tc.unavailable) || !slices.Equal(d.Warnings, tc.warnings) {
				t.Fatalf("tables = %v, unavailable = %q, warnings = %q", tableNames(d), d.Unavailable, d.Warnings)
			}
			types := "types=" + url.QueryEscape(tc.types)
			if got := calls.of("conversations.list"); !slices.Equal(got, []string{"limit=1&" + types}) {
				t.Errorf("conversations.list calls = %q", got)
			}
			if got := calls.of("users.conversations"); !slices.Equal(got, []string{"limit=200&" + types}) {
				t.Errorf("users.conversations calls = %q", got)
			}
			if got := calls.of("conversations.history"); !slices.Equal(got, tc.history) {
				t.Errorf("history calls = %q, want %q", got, tc.history)
			}
		})
	}
	t.Run("says nothing of a channel gone since it was listed", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"conversations.history": slackError("channel_not_found")}))
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		if err != nil || len(d.Tables) != 5 || len(d.Unavailable) != 0 || len(d.Warnings) != 0 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
	})
	t.Run("warns of a kind of channel whose history Slack refuses", func(t *testing.T) {
		routes := newSlackWorkspace().routes()
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("channel") == "C2" {
				return http.StatusOK, slackScope("groups:history")
			}
			return http.StatusOK, slackPage("messages")
		}
		kind, _ := slackAt(t, routes)
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		want := []string{"The app cannot read the messages of the private channels it is in: add the groups:history scope to the app and reinstall it, then test again"}
		if err != nil || len(d.Tables) != 5 || !slices.Equal(d.Warnings, want) {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
	})
	// Slack may answer a filtered page with nothing and a cursor though more follow.
	t.Run("pages the channels it is in for one of each kind to read", func(t *testing.T) {
		routes := newSlackWorkspace().routes()
		routes["users.conversations"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("channels", "j2")
			}
			return http.StatusOK, slackMore("channels", "j3", `{"id":"C2","name":"design","is_private":true}`, `{"id":"C1","name":"general"}`)
		}
		kind, calls := slackAt(t, routes)
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		if err != nil || len(d.Tables) != 5 || len(d.Warnings) != 0 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{"channel=C1&limit=1", "channel=C2&limit=1"}) ||
			len(calls.of("users.conversations")) != 2 {
			t.Fatalf("history calls = %q, users.conversations calls = %q", got, calls.of("users.conversations"))
		}
	})
	// Only an invite shows what the app may read, so its tables are listed.
	t.Run("lists the tables of an app in no channel, with how to invite it", func(t *testing.T) {
		kind, calls := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.conversations": slackPage("channels")}))
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		if err != nil || len(d.Tables) != 5 || len(d.Unavailable) != 0 || len(calls.of("conversations.history")) != 0 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
		want := []string{"The app is in no channels yet: invite it to each channel to sync with /invite @foundry, then test again"}
		if !slices.Equal(d.Warnings, want) {
			t.Fatalf("warnings = %q", d.Warnings)
		}
	})
	t.Run("says nothing of invites when it cannot read channels", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{
			"users.conversations": slackPage("channels"), "conversations.list": slackScope("channels:read"),
		}))
		d, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		if err != nil || !slices.Equal(tableNames(d), []string{"users"}) || len(d.Warnings) != 0 {
			t.Fatalf("Discover = (%+v, %v)", d, err)
		}
	})
	t.Run("refuses a token that reads nothing", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{
			"users.list": slackScope("users:read"), "conversations.list": slackScope("channels:read"), "users.conversations": slackScope("channels:read"),
		}))
		_, err := kind.Discover(ctx, Source{Secret: slackTestToken})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "this token cannot read Slack users or channels; add the app's scopes and test again" {
			t.Fatalf("Discover error = %v, want a refusal", err)
		}
	})
	for _, code := range []string{"invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired"} {
		t.Run("reports a rejected token: "+code, func(t *testing.T) {
			kind, calls := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"auth.test": slackError(code)}))
			_, err := kind.Discover(ctx, Source{Secret: slackTestToken})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != "Slack rejected this token; check it and enter it again" || len(calls.log) != 1 {
				t.Fatalf("Discover error = %v after %q, want a rejected token", err, calls.log)
			}
		})
	}
	// An outage says nothing about the object, so it must not make it unavailable.
	for answer, message := range map[string]string{
		slackError("fatal_error"): "Slack returned fatal_error for users.list",
		`{"ok":false}`:            "Slack returned an error for users.list",
	} {
		t.Run("refuses a probe Slack fails: "+answer, func(t *testing.T) {
			kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.list": answer}))
			_, err := kind.Discover(ctx, Source{Secret: slackTestToken})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != message {
				t.Fatalf("Discover error = %v, want %q", err, message)
			}
		})
	}
	t.Run("fails a channel listed without an id", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.conversations": slackPage("channels", `{"name":"general"}`)}))
		if _, err := kind.Discover(ctx, Source{Secret: slackTestToken}); err == nil || !strings.Contains(err.Error(), "a listed channel has no id") {
			t.Fatalf("Discover error = %v", err)
		}
	})
}

func TestSlackSync(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, kind *slackKind, table string) ([]map[string]any, error) {
		t.Helper()
		sinks, err := slackRead(t, kind, []Table{{Name: table}}, noDocuments{})
		return sinks[table].rows, err
	}

	t.Run("pages users and writes their columns", func(t *testing.T) {
		w := newSlackWorkspace()
		routes := w.routes()
		routes["users.list"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("members", "u2", w.users[:2]...)
			}
			return http.StatusOK, slackPage("members", w.users[2:]...)
		}
		kind, calls := slackAt(t, routes)
		rows, err := read(t, kind, "users")
		if err != nil {
			t.Fatal(err)
		}
		if got := calls.of("users.list"); !slices.Equal(got, []string{"limit=200", "cursor=u2&limit=200"}) || len(calls.log) != 2 {
			t.Fatalf("calls = %q", calls.log)
		}
		want := []map[string]any{
			{"user_id": "U1", "team_id": "T1", "name": "ada", "real_name": "Ada Lovelace", "display_name": "ada", "email": "ada@example.com",
				"title": "Engineer", "is_bot": false, "is_admin": true, "is_owner": true, "deleted": false, "tz": "Europe/London",
				"updated_at": time.Unix(1792000000, 0).UTC().Format(time.RFC3339)},
			{"user_id": "U2", "team_id": "T1", "name": "grace", "real_name": "Grace Hopper", "display_name": nil, "email": nil, "title": nil,
				"is_bot": false, "is_admin": false, "is_owner": false, "deleted": false, "tz": nil, "updated_at": nil},
			{"user_id": "U3", "team_id": "T1", "name": "alan", "real_name": nil, "display_name": nil, "email": nil, "title": nil,
				"is_bot": false, "is_admin": false, "is_owner": false, "deleted": true, "tz": nil, "updated_at": nil},
		}
		if len(rows) != 4 || fmt.Sprint(rows[:3]) != fmt.Sprint(want) || rows[3]["is_bot"] != true {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("writes every channel, archived ones too", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		rows, err := read(t, kind, "channels")
		if err != nil {
			t.Fatal(err)
		}
		if got := calls.of("conversations.list"); !slices.Equal(got, []string{"limit=200&types=public_channel%2Cprivate_channel"}) || len(calls.log) != 2 {
			t.Fatalf("calls = %q", calls.log)
		}
		want := []map[string]any{
			{"channel_id": "C1", "name": "general", "is_private": false, "is_archived": false, "is_member": true, "topic": "Company news",
				"purpose": "Everything", "creator_id": "U1", "created_at": "2023-11-14T22:13:20Z", "num_members": 3},
			{"channel_id": "C2", "name": "design", "is_private": true, "is_archived": false, "is_member": true, "topic": nil,
				"purpose": nil, "creator_id": "U2", "created_at": "2023-11-14T22:15:00Z", "num_members": 2},
			{"channel_id": "C3", "name": "old", "is_private": false, "is_archived": true, "is_member": false, "topic": nil,
				"purpose": nil, "creator_id": nil, "created_at": "2020-09-13T12:26:40Z", "num_members": nil},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("pages the members of each channel the app is in", func(t *testing.T) {
		routes := newSlackWorkspace().routes()
		routes["conversations.members"] = func(r *http.Request) (int, string) {
			switch q := r.URL.Query(); {
			case q.Get("channel") == "C2":
				return http.StatusOK, slackPage("members", `"U2"`)
			case q.Get("cursor") == "":
				return http.StatusOK, slackMore("members", "m2", `"U1"`)
			}
			return http.StatusOK, slackPage("members", `"U3"`)
		}
		kind, calls := slackAt(t, routes)
		rows, err := read(t, kind, "channel_members")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{"channel_id": "C1", "user_id": "U1"}, {"channel_id": "C1", "user_id": "U3"}, {"channel_id": "C2", "user_id": "U2"}}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
		if got := calls.of("conversations.members"); !slices.Equal(got, []string{"channel=C1&limit=200", "channel=C1&cursor=m2&limit=200", "channel=C2&limit=200"}) ||
			len(calls.of("users.conversations")) != 1 || len(calls.log) != 5 {
			t.Fatalf("calls = %q", calls.log)
		}
	})
	// The window is today and the two days before it; Slack is asked for it,
	// and what it sends from before is left out, as are system messages.
	t.Run("writes the messages of the window", func(t *testing.T) {
		w := newSlackWorkspace()
		routes := w.routes()
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			switch q := r.URL.Query(); {
			case q.Get("channel") == "C2":
				return http.StatusOK, slackPage("messages", w.history["C2"]...)
			case q.Get("cursor") == "":
				return http.StatusOK, slackMore("messages", "h2", w.history["C1"][:3]...)
			}
			return http.StatusOK, slackPage("messages", w.history["C1"][3:]...)
		}
		kind, calls := slackAt(t, routes)
		rows, err := read(t, kind, "messages")
		if err != nil {
			t.Fatal(err)
		}
		since := "inclusive=true&limit=999&oldest=" + slackTS("2026-10-27T00:00:00Z")
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{"channel=C1&" + since, "channel=C1&cursor=h2&" + since, "channel=C2&" + since}) {
			t.Fatalf("history calls = %q", got)
		}
		var ids []any
		for _, row := range rows {
			ids = append(ids, row["message_id"])
		}
		wantIDs := []any{"C1:" + slackLunch, "C1:" + slackTS("2026-10-28T10:05:00Z"), "C1:" + slackParent, "C1:" + slackTS("2026-10-27T12:00:00Z"),
			"C1:" + slackTS("2026-10-27T11:00:00Z"), "C2:" + slackTS("2026-10-29T14:00:00Z"), "C2:" + slackTS("2026-10-29T13:00:00Z"),
			"C2:" + slackTS("2026-10-29T12:00:00Z"), "C2:" + slackTS("2026-10-29T11:00:00Z")}
		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("message ids = %q", ids)
		}
		want := map[string]any{
			"message_id": "C1:" + slackParent, "channel_id": "C1", "ts": slackParent, "thread_ts": slackParent, "user_id": "U1", "bot_id": nil,
			"subtype": nil, "text": "Morning <@U2>, see <#C2|design> and <https://example.com/a?b=1&amp;c=2|the doc> or <https://example.com>\n&lt;3 &amp; more",
			"sent_at": "2026-10-28T09:15:00Z", "edited_at": "2026-10-28T09:16:00Z", "reply_count": 3, "reply_users_count": 2,
			"latest_reply_at": "2026-10-29T08:00:00Z",
		}
		if fmt.Sprint(rows[2]) != fmt.Sprint(want) {
			t.Fatalf("thread parent = %v", rows[2])
		}
		broadcast := map[string]any{
			"message_id": "C1:" + slackTS("2026-10-28T10:05:00Z"), "channel_id": "C1", "ts": slackTS("2026-10-28T10:05:00Z"), "thread_ts": slackParent,
			"user_id": "U2", "bot_id": nil, "subtype": "thread_broadcast", "text": "Shipping it", "sent_at": "2026-10-28T10:05:00Z",
			"edited_at": nil, "reply_count": 0, "reply_users_count": 0, "latest_reply_at": nil,
		}
		if fmt.Sprint(rows[1]) != fmt.Sprint(broadcast) || rows[3]["bot_id"] != "B9" || rows[3]["user_id"] != nil {
			t.Fatalf("broadcast = %v, bot = %v", rows[1], rows[3])
		}
	})
	t.Run("writes a row per channel day with a message", func(t *testing.T) {
		kind, _ := slackAt(t, newSlackWorkspace().routes())
		rows, err := read(t, kind, "channel_days")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"channel_day_id": "C2:2026-10-29", "channel_id": "C2", "day": "2026-10-29", "message_count": 4,
				"first_message_at": "2026-10-29T11:00:00Z", "last_message_at": "2026-10-29T14:00:00Z"},
			{"channel_day_id": "C1:2026-10-28", "channel_id": "C1", "day": "2026-10-28", "message_count": 3,
				"first_message_at": "2026-10-28T09:15:00Z", "last_message_at": "2026-10-28T16:00:00Z"},
			{"channel_day_id": "C1:2026-10-27", "channel_id": "C1", "day": "2026-10-27", "message_count": 2,
				"first_message_at": "2026-10-27T11:00:00Z", "last_message_at": "2026-10-27T12:00:00Z"},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("leaves out a day of system messages only", func(t *testing.T) {
		w := newSlackWorkspace()
		w.history["C2"] = []string{
			fmt.Sprintf(`{"subtype":"channel_topic","user":"U1","text":"set the topic","ts":%q}`, slackTS("2026-10-29T10:00:00Z")),
			fmt.Sprintf(`{"subtype":"channel_archive","user":"U1","text":"archived the channel","ts":%q}`, slackTS("2026-10-29T10:01:00Z")),
		}
		kind, _ := slackAt(t, w.routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil || len(sinks["channel_days"].rows) != 2 || len(sinks["messages"].rows) != 5 || !slices.Equal(docs.putNames(),
			[]string{"conversations C1:2026-10-28", "conversations C1:2026-10-27"}) {
			t.Fatalf("SyncPass = %v, days = %v, puts = %v", err, sinks["channel_days"].rows, docs.putNames())
		}
	})
	t.Run("reads an app in no channel as empty tables", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.conversations": slackPage("channels")}))
		sinks, err := slackRead(t, kind, slackTables[2:], noDocuments{})
		if err != nil || len(sinks["channel_members"].rows)+len(sinks["messages"].rows)+len(sinks["channel_days"].rows) != 0 {
			t.Fatalf("SyncPass = %v", err)
		}
	})
	t.Run("reads ts as Slack writes it", func(t *testing.T) {
		if at, ok := slackTime("1792000000.000100"); !ok || !at.Equal(time.Unix(1792000000, 100_000).UTC()) {
			t.Fatalf("slackTime = %s, %v", at, ok)
		}
		for _, ts := range []string{"", "1792000000", "1792000000.1", "1792000000.0000001", "abc.000100", "1792000000.abcdef", "-1.000000", "1.-00001"} {
			if _, ok := slackTime(ts); ok {
				t.Errorf("slackTime(%q) is ok", ts)
			}
		}
		if slackStamp("") != nil || slackStamp(slackParent) != "2026-10-28T09:15:00Z" {
			t.Fatal("slackStamp")
		}
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, page := range map[string]func(r *http.Request) string{
		"is not JSON":                            func(*http.Request) string { return `<html>Bad gateway</html>` },
		"lacks its members":                      func(*http.Request) string { return `{"ok":true,"response_metadata":{"next_cursor":""}}` },
		"has null for its members":               func(*http.Request) string { return `{"ok":true,"members":null}` },
		"says more follow but gives no cursor":   func(*http.Request) string { return `{"ok":true,"members":[],"has_more":true}` },
		"gives back the cursor it was asked for": func(*http.Request) string { return slackMore("members", "c1") },
		"lists a user without an id":             func(*http.Request) string { return slackPage("members", `{"name":"ada"}`) },
		"lists a user that is not an object":     func(*http.Request) string { return slackPage("members", `"U1"`) },
	} {
		t.Run("fails a page that "+name, func(t *testing.T) {
			kind, calls := slackAt(t, map[string]func(*http.Request) (int, string){
				"users.list": func(r *http.Request) (int, string) { return http.StatusOK, page(r) },
			})
			rows, err := read(t, kind, "users")
			if err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v) after %q; want a failure before any row", rows, err, calls.log)
			}
		})
	}
	for name, answers := range map[string]map[string]string{
		"a channel without an id":               {"conversations.list": slackPage("channels", `{"name":"general"}`)},
		"a channel the app is in without an id": {"users.conversations": slackPage("channels", `{"name":"general"}`)},
		"a member without an id":                {"conversations.members": slackPage("members", `""`)},
		"a message without a ts":                {"conversations.history": slackPage("messages", `{"text":"hi"}`)},
		"a message with a ts that is not one":   {"conversations.history": slackPage("messages", `{"text":"hi","ts":"yesterday"}`)},
		"a message that is not an object":       {"conversations.history": slackPage("messages", `"hi"`)},
	} {
		t.Run("fails a read that lists "+name, func(t *testing.T) {
			kind, _ := slackAt(t, with(newSlackWorkspace().routes(), answers))
			if _, err := slackRead(t, kind, slackTables, noDocuments{}); err == nil {
				t.Fatal("SyncPass read it")
			}
		})
	}
	t.Run("reads no private channel without groups:read", func(t *testing.T) {
		kind, calls := slackScoped(t, "channels:read,channels:history,users:read", newSlackWorkspace().routes())
		sinks, err := slackRead(t, kind, slackTables, noDocuments{})
		if err != nil {
			t.Fatal(err)
		}
		var channels []any
		for _, row := range sinks["channels"].rows {
			channels = append(channels, row["channel_id"])
		}
		if !slices.Equal(channels, []any{"C1", "C3"}) || len(sinks["channel_members"].rows) != 3 || len(sinks["messages"].rows) != 5 {
			t.Fatalf("channels = %v, members = %v, messages = %d", channels, sinks["channel_members"].rows, len(sinks["messages"].rows))
		}
		for _, method := range []string{"conversations.list", "users.conversations"} {
			if got := calls.of(method); len(got) != 1 || !strings.Contains(got[0], "types=public_channel") || strings.Contains(got[0], "private") {
				t.Errorf("%s calls = %q", method, got)
			}
		}
	})
	// Discover warned of it; its members are still read.
	t.Run("reads no private channel's history without groups:history", func(t *testing.T) {
		kind, calls := slackScoped(t, "channels:read,channels:history,groups:read,users:read", newSlackWorkspace().routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil || len(sinks["channel_members"].rows) != 5 || len(sinks["messages"].rows) != 5 ||
			!slices.Equal(docs.putNames(), []string{"conversations C1:2026-10-28", "conversations C1:2026-10-27"}) {
			t.Fatalf("SyncPass = %v, rows = %v, puts = %v", err, sinks, docs.putNames())
		}
		if got := calls.of("conversations.history"); len(got) != 1 || !strings.HasPrefix(got[0], "channel=C1&") {
			t.Fatalf("history calls = %q", got)
		}
	})
	t.Run("reads a public channel's history only with channels:history", func(t *testing.T) {
		kind, calls := slackScoped(t, "channels:read,groups:read,groups:history,users:read", newSlackWorkspace().routes())
		sinks, err := slackRead(t, kind, slackTables[3:], noDocuments{})
		if err != nil || len(sinks["messages"].rows) != 4 || len(sinks["channel_days"].rows) != 1 {
			t.Fatalf("SyncPass = %v, rows = %v", err, sinks)
		}
		if got := calls.of("conversations.history"); len(got) != 1 || !strings.HasPrefix(got[0], "channel=C2&") {
			t.Fatalf("history calls = %q", got)
		}
	})
	t.Run("refuses messages and days once it may read no history", func(t *testing.T) {
		for _, table := range slackTables[3:] {
			kind, calls := slackScoped(t, "channels:read,groups:read,users:read", newSlackWorkspace().routes())
			_, err := slackRead(t, kind, []Table{table}, noDocuments{})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != "add the channels:history scope to the app and reinstall it" ||
				!errors.Is(err, errMissingScope) || len(calls.log) != 1 {
				t.Fatalf("%s: SyncPass error = %v after %q, want a refusal", table.Name, err, calls.log)
			}
		}
	})
	// Discover finds what the app may read; a scope gone since is refused with its fix.
	t.Run("refuses a table whose scope the app lost", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.list": slackScope("users:read")}))
		_, err := read(t, kind, "users")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "add the users:read scope to the app and reinstall it" || !errors.Is(err, errMissingScope) {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	for method, answer := range map[string]string{
		"auth.test": slackError("invalid_auth"), "users.list": slackError("fatal_error"), "conversations.list": slackError("fatal_error"),
		"users.conversations": slackError("fatal_error"), "conversations.members": slackError("fatal_error"),
		"conversations.history": slackError("fatal_error"), "conversations.replies": slackError("fatal_error"),
	} {
		t.Run("refuses a read "+method+" fails", func(t *testing.T) {
			kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{method: answer}))
			_, err := slackRead(t, kind, slackTables, &granolaDocs{want: map[string]bool{"conversations": true}})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) {
				t.Fatalf("SyncPass error = %v, want a refusal", err)
			}
		})
	}
	// The app removed from a channel, or the channel deleted, between
	// users.conversations and a read of it skips the channel, not the pass.
	for _, code := range []string{"not_in_channel", "channel_not_found"} {
		for _, method := range []string{"conversations.members", "conversations.history"} {
			t.Run("skips a channel gone since it was listed: "+method+" "+code, func(t *testing.T) {
				w := newSlackWorkspace()
				routes := w.routes()
				served := routes[method]
				routes[method] = func(r *http.Request) (int, string) {
					if r.URL.Query().Get("channel") == "C1" {
						return http.StatusOK, slackError(code)
					}
					return served(r)
				}
				kind, calls := slackAt(t, routes)
				docs := &granolaDocs{want: map[string]bool{"conversations": true}}
				sinks, err := slackRead(t, kind, slackTables, docs)
				if err != nil || !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29"}) || len(sinks["channel_days"].rows) != 1 ||
					len(sinks["messages"].rows) != 4 || len(calls.of("conversations.replies")) != 0 {
					t.Fatalf("SyncPass = %v, puts = %v, rows = %v", err, docs.putNames(), sinks)
				}
				if members := len(sinks["channel_members"].rows); method == "conversations.members" && members != 2 || method == "conversations.history" && members != 5 {
					t.Fatalf("members = %v", sinks["channel_members"].rows)
				}
				if got := calls.of("conversations.history"); method == "conversations.members" && len(got) != 1 {
					t.Fatalf("history calls = %q", got)
				}
			})
		}
	}
	// What it listed of the channel's messages before stays; its days, which would be partial, go.
	t.Run("leaves out the days of a channel gone while its history is read", func(t *testing.T) {
		w := newSlackWorkspace()
		routes := w.routes()
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			switch q := r.URL.Query(); {
			case q.Get("channel") == "C2":
				return http.StatusOK, slackPage("messages", w.history["C2"]...)
			case q.Get("cursor") == "":
				return http.StatusOK, slackMore("messages", "h2", w.history["C1"][:3]...)
			}
			return http.StatusOK, slackError("channel_not_found")
		}
		kind, _ := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil || !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29"}) || len(sinks["channel_days"].rows) != 1 ||
			len(sinks["messages"].rows) != 6 {
			t.Fatalf("SyncPass = %v, puts = %v, days = %v, messages = %v", err, docs.putNames(), sinks["channel_days"].rows, sinks["messages"].rows)
		}
	})
	// Documents name people by id without a scope for users, but an outage is no missing scope.
	t.Run("refuses users Slack fails to list for documents", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.list": slackError("fatal_error")}))
		_, err := slackRead(t, kind, slackTables[4:], &granolaDocs{want: map[string]bool{"conversations": true}})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Slack returned fatal_error for users.list" {
			t.Fatalf("SyncPass error = %v, want a refusal", err)
		}
	})
	t.Run("fails a list that never ends", func(t *testing.T) {
		var mu sync.Mutex
		pages := 0
		kind, _ := slackAt(t, map[string]func(*http.Request) (int, string){
			"users.list": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				pages++
				return http.StatusOK, slackMore("members", fmt.Sprintf("u%d", pages))
			},
		})
		if _, err := read(t, kind, "users"); err == nil || !strings.Contains(err.Error(), "more than 20000 pages") || pages != maxSlackPages {
			t.Fatalf("Sync error = %v after %d pages", err, pages)
		}
	})
	t.Run("retries a rate limit", func(t *testing.T) {
		noBackoff(t)
		var mu sync.Mutex
		calls := 0
		w := newSlackWorkspace()
		kind, _ := slackAt(t, map[string]func(*http.Request) (int, string){
			"users.list": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return http.StatusTooManyRequests, slackError("ratelimited")
				}
				return http.StatusOK, slackPage("members", w.users...)
			},
		})
		if rows, err := read(t, kind, "users"); err != nil || len(rows) != 4 || calls != 2 {
			t.Fatalf("Sync = (%v, %v) after %d calls", rows, err, calls)
		}
	})
	// A limit that outlasts the retries fails the attempt with a message the syncer retries on.
	t.Run("refuses a rate limit that outlasts the retries, saying when to try again", func(t *testing.T) {
		noBackoff(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(slackError("ratelimited")))
		}))
		t.Cleanup(server.Close)
		kind, _ := slackAt(t, nil)
		kind.base = server.URL
		before := time.Now()
		_, err := read(t, kind, "users")
		after := time.Now()
		var refusedErr *RefusedError
		const prefix = "this token has used up its Slack rate limit; try again after "
		if !errors.As(err, &refusedErr) || !strings.HasPrefix(refusedErr.Message, prefix) {
			t.Fatalf("Sync error = %v, want a rate limit with its time", err)
		}
		at := strings.TrimPrefix(refusedErr.Message, prefix)
		if early, late := before.Add(2*time.Minute).UTC().Format("15:04 UTC"), after.Add(2*time.Minute).UTC().Format("15:04 UTC"); at != early && at != late {
			t.Fatalf("retry at %q, want %s", at, early)
		}
	})
	t.Run("refuses Slack's ratelimited error", func(t *testing.T) {
		kind, _ := slackAt(t, map[string]func(*http.Request) (int, string){
			"users.list": func(*http.Request) (int, string) { return http.StatusOK, slackError("ratelimited") },
		})
		_, err := read(t, kind, "users")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "this token has used up its Slack rate limit; try again in a few minutes" {
			t.Fatalf("Sync error = %v, want a rate limit", err)
		}
	})
	t.Run("refuses an answer that is not HTTP 200 after retries", func(t *testing.T) {
		noBackoff(t)
		kind, _ := slackAt(t, map[string]func(*http.Request) (int, string){
			"users.list": func(*http.Request) (int, string) { return http.StatusBadGateway, `{}` },
		})
		_, err := read(t, kind, "users")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Slack returned HTTP 502 for users.list" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("refuses a read Slack could not be reached for", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		kind, _ := slackAt(t, nil)
		kind.base = server.URL
		_, err := read(t, kind, "users")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "could not reach Slack; try again" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("stops when the read is cancelled", func(t *testing.T) {
		kind, _ := slackAt(t, newSlackWorkspace().routes())
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := kind.Sync(cancelled, slackSource, Table{Name: "users"}, &rowSink{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync error = %v, want context.Canceled", err)
		}
	})
	// Slack's limits are per method, so each is paced under its own tier.
	t.Run("paces each method under its tier", func(t *testing.T) {
		tiers := map[string]time.Duration{
			"auth.test": 600 * time.Millisecond, "users.list": 3 * time.Second, "conversations.list": 3 * time.Second,
			"users.conversations": 1200 * time.Millisecond, "conversations.history": 1200 * time.Millisecond,
			"conversations.replies": 1200 * time.Millisecond, "conversations.members": 600 * time.Millisecond,
		}
		if !slices.Equal(slices.Sorted(maps.Keys(slackIntervals)), slices.Sorted(maps.Keys(tiers))) {
			t.Fatalf("paced methods = %v", slices.Sorted(maps.Keys(slackIntervals)))
		}
		for method, least := range tiers {
			if slackIntervals[method] < least {
				t.Errorf("%s is paced every %s, faster than its tier", method, slackIntervals[method])
			}
		}
		var mu sync.Mutex
		var at []time.Time
		kind, _ := slackAt(t, map[string]func(*http.Request) (int, string){
			"users.list": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				at = append(at, time.Now())
				if len(at) < 3 {
					return http.StatusOK, slackMore("members", fmt.Sprintf("u%d", len(at)))
				}
				return http.StatusOK, slackPage("members")
			},
		})
		slackIntervals["users.list"] = 50 * time.Millisecond
		if _, err := read(t, kind, "users"); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < 40*time.Millisecond {
				t.Fatalf("call %d came %s after the one before", i, gap)
			}
		}
	})
	// A snapshot over its row limit stops the read with the writer's error, which the syncer reports.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("row limit")
		kind, _ := slackAt(t, newSlackWorkspace().routes())
		for _, table := range slackTables {
			if _, err := kind.Sync(ctx, slackSource, table, refusingSink{full}); !errors.Is(err, full) {
				t.Fatalf("%s: Sync error = %v, want the writer's", table.Name, err)
			}
		}
	})
	t.Run("has no object it does not list", func(t *testing.T) {
		kind, _ := slackAt(t, nil)
		_, err := read(t, kind, "direct_messages")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Sync error = %v, want InvalidError", err)
		}
	})
}

// slackHeader opens newSlackWorkspace's #general documents.
const slackHeader = "Channel: #general (C1)\n"

func TestSlackDocuments(t *testing.T) {
	ctx := context.Background()
	general28 := "# #general, 2026-10-28\n\n" + slackHeader + "\n" +
		"**Ada Lovelace** 09:15: Morning @Grace Hopper, see #design and [the doc](https://example.com/a?b=1&c=2) or https://example.com\n<3 & more\n" +
		"  - **Grace Hopper** 09:20: Thanks!\n    Second line\n" +
		"  - **Grace Hopper** 10:05: Shipping it\n" +
		"  - **Ada Lovelace** 2026-10-29 08:00: Follow-up next day\n" +
		"\n**alan** 16:00: Lunch? @here\n"
	general27 := "# #general, 2026-10-27\n\n" + slackHeader + "\n" +
		"**Ada Lovelace** 11:00: Old thread news\n" +
		"\n**Deploy Bot** 12:00: Deployed v2\n"
	design29 := "# #design, 2026-10-29\n\nChannel: #design (C2)\n\n" +
		"**Slack** 11:00: hmm\n" +
		"\n**B7** 12:00: beep\n" +
		"\n**Zapier** 13:00:\n" +
		"\n**U4** 14:00: @U9 what about #C7, #general and #design? @Ada Lovelace @bob @channel @everyone @eng @subteam Oct 1 [a@b.co](mailto:a@b.co)\n"
	texts := func(docs *granolaDocs) map[string]string {
		out := map[string]string{}
		for _, p := range docs.puts {
			out[p.doc.ExternalID] = string(p.doc.Text)
		}
		return out
	}
	versions := func(docs *granolaDocs) map[string]string {
		out := map[string]string{}
		for _, offer := range docs.offers {
			fields := strings.Fields(offer)
			out[fields[1]] = fields[2]
		}
		return out
	}

	t.Run("writes each channel day's conversation, newest first", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29", "conversations C1:2026-10-28", "conversations C1:2026-10-27"}) {
			t.Fatalf("puts = %v", docs.putNames())
		}
		want := map[string]string{"C1:2026-10-28": general28, "C1:2026-10-27": general27, "C2:2026-10-29": design29}
		for id, text := range texts(docs) {
			if text != want[id] {
				t.Errorf("%s =\n%s\nwant\n%s", id, text, want[id])
			}
		}
		version := regexp.MustCompile(`^1\|[0-9a-f]{40}$`)
		// #general's 28th is as recent as its thread's latest reply.
		at := []time.Time{time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC), time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC),
			time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
		for i, p := range docs.puts {
			channel, day, _ := strings.Cut(p.doc.ExternalID, ":")
			name := map[string]string{"C1": "general", "C2": "design"}[channel]
			if !version.MatchString(p.doc.Version) || !p.doc.At.Equal(at[i]) || p.doc.Title != "#"+name+", "+day ||
				p.doc.URL != "https://app.slack.com/client/T1/"+channel || docs.offers[i] != "conversations "+p.doc.ExternalID+" "+p.doc.Version {
				t.Errorf("put %d = %+v, offered %q", i, p.doc, docs.offers[i])
			}
		}
		// Only the thread a wanted day holds is read, once.
		if got := calls.of("conversations.replies"); !slices.Equal(got, []string{"channel=C1&limit=999&ts=" + slackParent}) ||
			len(calls.of("auth.test")) != 1 || len(calls.of("users.list")) != 1 || len(calls.of("conversations.history")) != 2 {
			t.Fatalf("calls = %q", calls.log)
		}
		if len(sinks["users"].rows) != 4 || len(sinks["channels"].rows) != 3 || len(sinks["channel_members"].rows) != 5 ||
			len(sinks["messages"].rows) != 9 || len(sinks["channel_days"].rows) != 3 {
			t.Fatalf("rows = %v", sinks)
		}
	})
	offered := func(docs *granolaDocs) []string {
		var ids []string
		for _, offer := range docs.offers {
			ids = append(ids, strings.Fields(offer)[1])
		}
		return ids
	}
	// The syncer keeps the first documents offered up to its cap.
	t.Run("offers the most recently active days first, the lower id first at the same time", func(t *testing.T) {
		w := newSlackWorkspace()
		w.history["C2"] = []string{fmt.Sprintf(`{"user":"U1","text":"Same time","ts":%q}`, slackTS("2026-10-29T08:00:00Z"))}
		kind, _ := slackAt(t, w.routes())
		docs := &granolaDocs{}
		if _, err := slackRead(t, kind, slackTables, docs); err != nil {
			t.Fatal(err)
		}
		if ids := offered(docs); !slices.Equal(ids, []string{"C1:2026-10-28", "C2:2026-10-29", "C1:2026-10-27"}) {
			t.Fatalf("offers = %q", ids)
		}
	})
	// An old thread answered today must not fall past the cap with its day.
	t.Run("dates a day by its threads' latest replies", func(t *testing.T) {
		w := newSlackWorkspace()
		w.history["C1"][4] = strings.Replace(w.history["C1"][4], `"text":"Deployed v2"`,
			fmt.Sprintf(`"text":"Deployed v2","thread_ts":%q,"reply_count":1,"latest_reply":%q`, slackTS("2026-10-27T12:00:00Z"), slackTS("2026-10-29T14:30:00Z")), 1)
		kind, _ := slackAt(t, w.routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil {
			t.Fatal(err)
		}
		if ids := offered(docs); !slices.Equal(ids, []string{"C1:2026-10-27", "C2:2026-10-29", "C1:2026-10-28"}) {
			t.Fatalf("offers = %q", ids)
		}
		if at := docs.puts[0].doc.At; !at.Equal(time.Date(2026, 10, 29, 14, 30, 0, 0, time.UTC)) {
			t.Fatalf("C1:2026-10-27 at %s", at)
		}
		// Its row says when its messages were posted.
		if row := sinks["channel_days"].rows[0]; row["channel_day_id"] != "C1:2026-10-27" || row["last_message_at"] != "2026-10-27T12:00:00Z" {
			t.Fatalf("row = %v", row)
		}
	})
	// A day the stream holds at its version costs no conversations.replies call.
	t.Run("reads replies only for a day that is wanted", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		docs := &granolaDocs{}
		if _, err := slackRead(t, kind, slackTables, docs); err != nil {
			t.Fatal(err)
		}
		if len(docs.offers) != 3 || len(docs.puts) != 0 || len(calls.of("conversations.replies")) != 0 {
			t.Fatalf("offers = %q, puts = %v, calls = %q", docs.offers, docs.putNames(), calls.log)
		}
	})
	t.Run("versions a day by what Slack says of its messages", func(t *testing.T) {
		offered := func(t *testing.T, w *slackWorkspace) map[string]string {
			t.Helper()
			kind, _ := slackAt(t, w.routes())
			docs := &granolaDocs{}
			if _, err := slackRead(t, kind, slackTables[3:], docs); err != nil {
				t.Fatal(err)
			}
			return versions(docs)
		}
		base := offered(t, newSlackWorkspace())
		if len(base) != 3 {
			t.Fatalf("versions = %v", base)
		}
		lunch := func(w *slackWorkspace, change func(string) string) { w.history["C1"][0] = change(w.history["C1"][0]) }
		for name, tc := range map[string]struct {
			change  func(w *slackWorkspace)
			changed []string
		}{
			"nothing changes": {func(*slackWorkspace) {}, nil},
			"Slack lists the messages in another order": {func(w *slackWorkspace) { slices.Reverse(w.history["C1"]) }, nil},
			"a system message is posted": {func(w *slackWorkspace) {
				w.history["C1"] = append([]string{fmt.Sprintf(`{"subtype":"channel_leave","user":"U3","ts":%q}`, slackTS("2026-10-28T17:00:00Z"))}, w.history["C1"]...)
			}, nil},
			"a message is edited": {func(w *slackWorkspace) {
				lunch(w, func(m string) string {
					return strings.Replace(m, `"Lunch? <!here>"`, `"Lunch at 1? <!here>","edited":{"ts":"`+slackTS("2026-10-28T16:05:00Z")+`"}`, 1)
				})
			}, []string{"C1:2026-10-28"}},
			"a message's edit is all that changes": {func(w *slackWorkspace) {
				lunch(w, func(m string) string {
					return strings.Replace(m, `"text"`, `"edited":{"ts":"`+slackTS("2026-10-28T16:05:00Z")+`"},"text"`, 1)
				})
			}, []string{"C1:2026-10-28"}},
			"a thread gets a reply": {func(w *slackWorkspace) {
				w.history["C1"][3] = strings.Replace(strings.Replace(w.history["C1"][3], `"reply_count":3`, `"reply_count":4`, 1),
					slackTS("2026-10-29T08:00:00Z"), slackTS("2026-10-29T09:00:00Z"), 1)
			}, []string{"C1:2026-10-28"}},
			"a message is posted": {func(w *slackWorkspace) {
				w.history["C1"] = append([]string{fmt.Sprintf(`{"user":"U1","text":"Late","ts":%q}`, slackTS("2026-10-28T23:00:00Z"))}, w.history["C1"]...)
			}, []string{"C1:2026-10-28"}},
			"a message is deleted": {func(w *slackWorkspace) { w.history["C1"] = slices.Delete(w.history["C1"], 4, 5) }, []string{"C1:2026-10-27"}},
			"the channel is renamed": {func(w *slackWorkspace) {
				w.joined[0] = `{"id":"C1","name":"announcements","is_member":true}`
			}, []string{"C1:2026-10-27", "C1:2026-10-28"}},
		} {
			w := newSlackWorkspace()
			tc.change(w)
			got := offered(t, w)
			var changed []string
			for id, version := range got {
				if base[id] != version {
					changed = append(changed, id)
				}
			}
			slices.Sort(changed)
			if len(got) != 3 || !slices.Equal(changed, tc.changed) {
				t.Errorf("%s: changed %v, want %v", name, changed, tc.changed)
			}
		}
	})
	// The messages a read keeps are bounded; a day dropped is read again, by itself, if wanted.
	t.Run("reads a day whose messages were dropped again", func(t *testing.T) {
		for budget, reread := range map[int][]string{0: {"C1:2026-10-28", "C1:2026-10-27", "C2:2026-10-29"}, 1500: {"C1:2026-10-28", "C1:2026-10-27"}} {
			saved := slackTextBudget
			slackTextBudget = budget
			kind, calls := slackAt(t, newSlackWorkspace().routes())
			docs := &granolaDocs{want: map[string]bool{"conversations": true}}
			_, err := slackRead(t, kind, slackTables, docs)
			slackTextBudget = saved
			if err != nil {
				t.Fatal(err)
			}
			if got := texts(docs); fmt.Sprint(got) != fmt.Sprint(map[string]string{"C1:2026-10-28": general28, "C1:2026-10-27": general27, "C2:2026-10-29": design29}) {
				t.Fatalf("budget %d: documents = %q", budget, got)
			}
			var days []string
			for _, query := range calls.of("conversations.history") {
				q, _ := url.ParseQuery(query)
				if q.Get("latest") == "" {
					continue
				}
				day, _ := slackTime(q.Get("oldest"))
				if q.Get("oldest") != slackTS(day.Format(time.RFC3339)) || q.Get("latest") != slackTS(day.Add(24*time.Hour-time.Microsecond).Format(time.RFC3339Nano)) ||
					q.Get("inclusive") != "true" || q.Get("limit") != "999" {
					t.Errorf("budget %d: day read %s", budget, query)
				}
				days = append(days, q.Get("channel")+":"+day.Format(time.DateOnly))
			}
			slices.Sort(days)
			slices.Sort(reread)
			if !slices.Equal(days, reread) {
				t.Fatalf("budget %d: read again %v, want %v", budget, days, reread)
			}
		}
	})
	t.Run("puts nothing for a day whose messages went before it was read again", func(t *testing.T) {
		saved := slackTextBudget
		slackTextBudget = 0
		t.Cleanup(func() { slackTextBudget = saved })
		w := newSlackWorkspace()
		routes := w.routes()
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			if q := r.URL.Query(); q.Get("latest") == "" {
				return http.StatusOK, slackPage("messages", w.history[q.Get("channel")]...)
			}
			return http.StatusOK, slackPage("messages")
		}
		kind, _ := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil || len(docs.offers) != 3 || len(docs.puts) != 0 || len(sinks["channel_days"].rows) != 3 {
			t.Fatalf("SyncPass = %v, offers = %q, puts = %v", err, docs.offers, docs.putNames())
		}
	})
	t.Run("fails a day it could not read again", func(t *testing.T) {
		saved := slackTextBudget
		slackTextBudget = 0
		t.Cleanup(func() { slackTextBudget = saved })
		w := newSlackWorkspace()
		routes := w.routes()
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			if q := r.URL.Query(); q.Get("latest") == "" {
				return http.StatusOK, slackPage("messages", w.history[q.Get("channel")]...)
			}
			return http.StatusOK, slackError("internal_error")
		}
		kind, _ := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := slackRead(t, kind, slackTables, docs); err == nil || len(docs.puts) != 0 {
			t.Fatalf("SyncPass = %v, puts = %v", err, docs.putNames())
		}
	})
	t.Run("names people by id when it may not read users", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"users.list": slackScope("users:read")}))
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := slackRead(t, kind, slackTables[1:], docs); err != nil {
			t.Fatal(err)
		}
		if got := texts(docs)["C1:2026-10-27"]; got != strings.Replace(general27, "**Ada Lovelace**", "**U1**", 1) {
			t.Fatalf("C1:2026-10-27 = %q", got)
		}
	})
	t.Run("reads users for documents though their table is not read", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables[4:], docs)
		if err != nil || len(sinks) != 1 || texts(docs)["C1:2026-10-28"] != general28 || len(calls.of("users.list")) != 1 ||
			len(calls.of("conversations.list")) != 0 || len(calls.of("conversations.members")) != 0 {
			t.Fatalf("SyncPass = %v, calls = %q", err, calls.log)
		}
	})
	// A paused stream takes no document, so the read keeps no messages and names no one.
	t.Run("keeps no messages while the stream is paused", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		docs := &pausedDocs{granolaDocs: &granolaDocs{}}
		if _, err := slackRead(t, kind, slackTables[3:], docs); err != nil {
			t.Fatal(err)
		}
		if len(docs.offers) != 3 || len(docs.puts) != 0 || len(calls.of("users.list")) != 0 {
			t.Fatalf("offers = %q, puts = %v, calls = %q", docs.offers, docs.putNames(), calls.log)
		}
		// Were a day wanted anyway, its messages would be read again.
		docs.want = map[string]bool{"conversations": true}
		docs.offers = nil
		kind, calls = slackAt(t, newSlackWorkspace().routes())
		if _, err := slackRead(t, kind, slackTables[3:], docs); err != nil || len(docs.puts) != 3 || len(calls.of("conversations.history")) != 5 {
			t.Fatalf("SyncPass = %v, puts = %v, calls = %q", err, docs.putNames(), calls.log)
		}
	})
	t.Run("pages a thread's replies", func(t *testing.T) {
		w := newSlackWorkspace()
		routes := w.routes()
		replies := w.replies["C1:"+slackParent]
		routes["conversations.replies"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("messages", "r2", replies[:2]...)
			}
			return http.StatusOK, slackPage("messages", replies[2:]...)
		}
		kind, calls := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := slackRead(t, kind, slackTables, docs); err != nil || texts(docs)["C1:2026-10-28"] != general28 {
			t.Fatalf("SyncPass = %v, documents = %q", err, texts(docs))
		}
		if got := calls.of("conversations.replies"); len(got) != 2 || !strings.Contains(got[1], "cursor=r2") {
			t.Fatalf("replies calls = %q", got)
		}
	})
	for name, reply := range map[string]string{
		"without a ts": `{"user":"U2","text":"hi"}`, "that is not an object": `"hi"`,
	} {
		t.Run("fails a thread that lists a reply "+name, func(t *testing.T) {
			w := newSlackWorkspace()
			w.replies["C1:"+slackParent] = append(w.replies["C1:"+slackParent], reply)
			kind, _ := slackAt(t, w.routes())
			docs := &granolaDocs{want: map[string]bool{"conversations": true}}
			if _, err := slackRead(t, kind, slackTables, docs); err == nil || !strings.Contains(err.Error(), "lists a reply without a valid ts") {
				t.Fatalf("SyncPass error = %v", err)
			}
		})
	}
	t.Run("writes a thread gone since its parent was listed without replies", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"conversations.replies": slackError("thread_not_found")}))
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := slackRead(t, kind, slackTables, docs); err != nil || len(docs.puts) != 3 {
			t.Fatalf("SyncPass = %v, puts = %v", err, docs.putNames())
		}
		want := "# #general, 2026-10-28\n\n" + slackHeader + "\n" +
			"**Ada Lovelace** 09:15: Morning @Grace Hopper, see #design and [the doc](https://example.com/a?b=1&c=2) or https://example.com\n<3 & more\n" +
			"\n**alan** 16:00: Lunch? @here\n"
		if got := texts(docs)["C1:2026-10-28"]; got != want {
			t.Fatalf("C1:2026-10-28 =\n%s\nwant\n%s", got, want)
		}
	})
	// The held document stays until a sync finds the day gone with its channel.
	for name, budget := range map[string]int{"while its threads are read": 64 << 20, "while its day is read again": 0} {
		t.Run("puts no document of a channel gone "+name, func(t *testing.T) {
			saved := slackTextBudget
			slackTextBudget = budget
			t.Cleanup(func() { slackTextBudget = saved })
			w := newSlackWorkspace()
			routes := w.routes()
			routes["conversations.replies"] = func(*http.Request) (int, string) { return http.StatusOK, slackError("channel_not_found") }
			routes["conversations.history"] = func(r *http.Request) (int, string) {
				if q := r.URL.Query(); q.Get("latest") == "" || q.Get("channel") == "C2" {
					return http.StatusOK, slackPage("messages", w.history[q.Get("channel")]...)
				}
				return http.StatusOK, slackError("not_in_channel")
			}
			kind, _ := slackAt(t, routes)
			docs := &granolaDocs{want: map[string]bool{"conversations": true}}
			want := []string{"conversations C2:2026-10-29", "conversations C1:2026-10-27"}
			if budget == 0 {
				want = want[:1]
			}
			if _, err := slackRead(t, kind, slackTables, docs); err != nil || len(docs.offers) != 3 || !slices.Equal(docs.putNames(), want) {
				t.Fatalf("SyncPass = %v, offers = %q, puts = %v", err, docs.offers, docs.putNames())
			}
		})
	}
	// One day of a busy channel may hold far more than a document does; it
	// keeps its earliest messages, as the syncer's cut of a document would.
	busy := func(t *testing.T, replies []string) *slackWorkspace {
		t.Helper()
		saved := slackDayBytes
		slackDayBytes = 2000
		t.Cleanup(func() { slackDayBytes = saved })
		w := newSlackWorkspace()
		w.joined, w.history["C1"] = w.joined[:1], nil
		at := func(i int) string { return slackTS(fmt.Sprintf("2026-10-29T10:%02d:00Z", i)) }
		for i := 9; i >= 0; i-- {
			thread := ""
			if i < 2 && len(replies) > 0 {
				thread = fmt.Sprintf(`,"thread_ts":%q,"reply_count":%d,"latest_reply":%q`, at(i), len(replies), at(i))
			}
			w.history["C1"] = append(w.history["C1"], fmt.Sprintf(`{"user":"U1","text":"m%d","ts":%q%s}`, i, at(i), thread))
		}
		for i := range 2 {
			w.replies["C1:"+at(i)] = []string{fmt.Sprintf(`{"user":"U1","text":"m%d","ts":%q,"thread_ts":%q}`, i, at(i), at(i))}
			for j, text := range replies {
				w.replies["C1:"+at(i)] = append(w.replies["C1:"+at(i)],
					fmt.Sprintf(`{"user":"U2","text":%q,"ts":%q,"thread_ts":%q}`, text, slackTS(fmt.Sprintf("2026-10-29T11:%02d:00Z", j)), at(i)))
			}
		}
		return w
	}
	t.Run("keeps a large day's earliest messages, saying how many it left out", func(t *testing.T) {
		for _, budget := range []int{64 << 20, 0} {
			saved := slackTextBudget
			slackTextBudget = budget
			kind, _ := slackAt(t, busy(t, nil).routes())
			docs := &granolaDocs{want: map[string]bool{"conversations": true}}
			sinks, err := slackRead(t, kind, slackTables[3:], docs)
			slackTextBudget = saved
			if err != nil {
				t.Fatal(err)
			}
			want := "# #general, 2026-10-29\n\n" + slackHeader
			for i := range 8 {
				want += fmt.Sprintf("\n**Ada Lovelace** 10:%02d: m%d\n", i, i)
			}
			want += "\n[Left out: 2 later messages of this day]\n"
			if got := texts(docs)["C1:2026-10-29"]; got != want {
				t.Fatalf("budget %d: C1:2026-10-29 =\n%s\nwant\n%s", budget, got, want)
			}
			// What Slack says of every message makes its row and version.
			if len(sinks["messages"].rows) != 10 || fmt.Sprint(sinks["channel_days"].rows[0]["message_count"]) != "10" {
				t.Fatalf("budget %d: rows = %v", budget, sinks["channel_days"].rows)
			}
		}
	})
	t.Run("stops reading a large day's threads once it holds more than a document", func(t *testing.T) {
		long := strings.Repeat("a", 1000)
		w := busy(t, []string{long + "1", long + "2", long + "3", long + "4"})
		routes := w.routes()
		replies := routes["conversations.replies"]
		routes["conversations.replies"] = func(r *http.Request) (int, string) {
			_, page := replies(r)
			var listed struct{ Messages []json.RawMessage }
			_ = json.Unmarshal([]byte(page), &listed)
			items := make([]string, len(listed.Messages))
			for i, m := range listed.Messages {
				items[i] = string(m)
			}
			if r.URL.Query().Get("cursor") == "" {
				return http.StatusOK, slackMore("messages", "r2", items[:3]...)
			}
			return http.StatusOK, slackPage("messages", items[3:]...)
		}
		kind, calls := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := slackRead(t, kind, slackTables[3:], docs); err != nil {
			t.Fatal(err)
		}
		want := "# #general, 2026-10-29\n\n" + slackHeader + "\n**Ada Lovelace** 10:00: m0\n" +
			"  - **Grace Hopper** 11:00: " + long + "1\n  - **Grace Hopper** 11:01: " + long + "2\n"
		if got := texts(docs)["C1:2026-10-29"]; got != want {
			t.Fatalf("C1:2026-10-29 =\n%s\nwant\n%s", got, want)
		}
		if got := calls.of("conversations.replies"); len(got) != 1 || !strings.Contains(got[0], "ts="+slackTS("2026-10-29T10:00:00Z")) {
			t.Fatalf("replies calls = %q", got)
		}
	})
	t.Run("stops at Put's error", func(t *testing.T) {
		kind, _ := slackAt(t, newSlackWorkspace().routes())
		failed := errors.New("store down")
		docs := &granolaDocs{want: map[string]bool{"conversations": true}, fail: failed}
		if _, err := slackRead(t, kind, slackTables, docs); !errors.Is(err, failed) || len(docs.puts) != 1 {
			t.Fatalf("SyncPass error = %v after %d puts, want Put's at once", err, len(docs.puts))
		}
	})
	// Documents belong to the channel_days table; a read of the others offers none.
	t.Run("offers no document unless channel days are read", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks, err := slackRead(t, kind, slackTables[:4], docs)
		if err != nil || len(sinks["messages"].rows) != 9 || len(docs.offers) != 0 || len(calls.of("conversations.replies")) != 0 {
			t.Fatalf("SyncPass = %v, offers = %q, calls = %q", err, docs.offers, calls.log)
		}
	})
	// Sync has nowhere to Put documents, so it wants none and reads no thread.
	t.Run("reads one table through Sync without documents", func(t *testing.T) {
		kind, calls := slackAt(t, newSlackWorkspace().routes())
		sink := &rowSink{}
		if _, err := kind.Sync(ctx, slackSource, Table{Name: "channel_days"}, sink); err != nil || len(sink.rows) != 3 ||
			len(calls.of("conversations.replies")) != 0 {
			t.Fatalf("Sync = %v, rows = %v, calls = %q", err, sink.rows, calls.log)
		}
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		if _, err := kind.SyncDocuments(ctx, slackSource, Table{Name: "channel_days"}, &rowSink{}, docs); err != nil || len(docs.puts) != 3 {
			t.Fatalf("SyncDocuments = %v, puts = %v", err, docs.putNames())
		}
	})
}

// pausedDocs is a granolaDocs whose streams are paused.
type pausedDocs struct{ *granolaDocs }

func (pausedDocs) Paused(string) bool { return true }
