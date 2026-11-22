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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// slackFeedFake is one connector's feed as Foundry's store keeps it, in
// memory: its buffer, the mirror's index, the channels it covers, and what
// a fold recorded as replaced. before, when set, runs once before the next
// commit's compare-and-set, as another fold would; failCommit fails it.
type slackFeedFake struct {
	mu         sync.Mutex
	feed       model.SlackFeed
	rows       []model.SlackFeedRow
	days       map[model.SlackDayKey]model.SlackFeedDay
	channels   map[string]model.SlackChannelJoin
	joins      []model.SlackChannelJoin
	purges     []string
	replaced   []string
	documented []model.SlackDayKey
	commits    int
	before     func()
	failCommit error
	failGet    error
}

var errSlackFakeConflict = errors.New("store: conflict")

func newSlackFeedFake() *slackFeedFake {
	return &slackFeedFake{feed: model.SlackFeed{ID: "feed1", ConnectorID: "conn1", WorkspaceID: "ws1", TeamID: "T1", InstallID: "inst1"},
		days: map[model.SlackDayKey]model.SlackFeedDay{}, channels: map[string]model.SlackChannelJoin{}}
}

// append buffers a row of kind on channel's day from source, its payload raw JSON.
func (f *slackFeedFake) append(kind model.SlackFeedRowKind, source model.SlackFeedSource, channel string, day time.Time, payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feed.LastSeq++
	f.rows = append(f.rows, model.SlackFeedRow{Seq: f.feed.LastSeq, DedupeKey: fmt.Sprintf("e:Ev%d", f.feed.LastSeq), ChannelID: channel,
		Day: model.SlackDay(day), Kind: kind, Source: source, Payload: json.RawMessage(payload)})
}

func (f *slackFeedFake) GetFoundrySlackFeed(_ context.Context, connectorID string) (*model.SlackFeed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet != nil {
		return nil, f.failGet
	}
	if connectorID != f.feed.ConnectorID {
		return nil, errors.New("store: not found")
	}
	feed := f.feed
	return &feed, nil
}

func (f *slackFeedFake) FoundrySlackFeedAfter(_ context.Context, feedID string, after int64, budget model.SlackFeedBudget) ([]model.SlackFeedRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if budget.Rows < 1 {
		return nil, errors.New("a Slack feed read has a budget")
	}
	var rows []model.SlackFeedRow
	for _, r := range f.rows {
		if r.Seq > after && len(rows) < budget.Rows {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

func (f *slackFeedFake) CommitFoundrySlackFold(_ context.Context, fold model.SlackFold) error {
	f.mu.Lock()
	before := f.before
	f.before = nil
	f.mu.Unlock()
	if before != nil {
		before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCommit != nil {
		return f.failCommit
	}
	if f.feed.FoldedSeq != fold.From {
		for _, d := range fold.Days {
			f.replaced = append(f.replaced, d.ObjectKey)
		}
		return errSlackFakeConflict
	}
	f.commits++
	f.feed.FoldedSeq = fold.To
	for _, d := range fold.Days {
		d.UpdatedSeq = fold.To
		if held, ok := f.days[d.SlackDayKey]; ok {
			d.DocumentedAt = held.DocumentedAt
		}
		f.days[d.SlackDayKey] = d
	}
	f.replaced = append(f.replaced, fold.Replaced...)
	for _, k := range fold.Dropped {
		if d, ok := f.days[k]; ok {
			f.replaced = append(f.replaced, d.ObjectKey)
			delete(f.days, k)
		}
	}
	return nil
}

func (f *slackFeedFake) FoundrySlackDaysAfter(_ context.Context, _ string, seq int64) ([]model.SlackFeedDay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var days []model.SlackFeedDay
	for _, d := range f.days {
		if d.UpdatedSeq > seq {
			days = append(days, d)
		}
	}
	slices.SortFunc(days, func(a, b model.SlackFeedDay) int {
		if c := a.Day.Compare(b.Day); c != 0 {
			return c
		}
		return strings.Compare(a.ChannelID, b.ChannelID)
	})
	return days, nil
}

func (f *slackFeedFake) MarkFoundrySlackDaysDocumented(_ context.Context, _ string, days []model.SlackDayKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := slackNow().UTC()
	for _, k := range days {
		f.documented = append(f.documented, k)
		if d, ok := f.days[k]; ok {
			d.DocumentedAt = &now
			f.days[k] = d
		}
	}
	return nil
}

func (f *slackFeedFake) ListFoundrySlackFeedChannels(context.Context, string) ([]model.SlackChannelJoin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.SortedFunc(maps.Values(f.channels), func(a, b model.SlackChannelJoin) int { return strings.Compare(a.ChannelID, b.ChannelID) }), nil
}

func (f *slackFeedFake) JoinFoundrySlackChannels(_ context.Context, _ string, joins []model.SlackChannelJoin) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, j := range joins {
		if !j.Oldest.Before(j.CoveredFrom) {
			return 0, errors.New("a Slack channel join is covered after its window's oldest")
		}
		f.joins = append(f.joins, j)
		if _, ok := f.channels[j.ChannelID]; !ok {
			f.channels[j.ChannelID] = j
			n++
		}
	}
	return n, nil
}

func (f *slackFeedFake) PurgeFoundrySlackChannel(_ context.Context, _, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, channelID)
	delete(f.channels, channelID)
	for k, d := range f.days {
		if k.ChannelID == channelID {
			f.replaced = append(f.replaced, d.ObjectKey)
			delete(f.days, k)
		}
	}
	return nil
}

// cover has the feed cover channels from long ago, as a bind seeds them.
func (f *slackFeedFake) cover(channels ...string) {
	for _, id := range channels {
		f.channels[id] = model.SlackChannelJoin{ChannelID: id, CoveredFrom: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	}
}

// day is the index row of channel's day, YYYY-MM-DD.
func (f *slackFeedFake) day(channel, day string) model.SlackFeedDay {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, _ := time.Parse(time.DateOnly, day)
	return f.days[model.SlackDayKey{ChannelID: channel, Day: at}]
}

// slackRoutesCaller is an install's caller answering from routes, each
// answer saying the install has scopes, as Slack's header does.
type slackRoutesCaller struct {
	routes map[string]func(*http.Request) (int, string)
	scopes string
	calls  *slackCalls
}

func (c slackRoutesCaller) Call(_ context.Context, method string, query url.Values) (int, http.Header, []byte, error) {
	handler, ok := c.routes[method]
	if !ok {
		return http.StatusNotFound, nil, nil, nil
	}
	c.calls.add(method + "?" + query.Encode())
	status, body := handler(httptest.NewRequest(http.MethodGet, "/api/"+method+"?"+query.Encode(), nil))
	header := http.Header{}
	if c.scopes != "" {
		header.Set("X-OAuth-Scopes", c.scopes)
	}
	return status, header, []byte(body), nil
}

// slackHelixApp is Helix's Slack app in the test team: its bot user UF and
// bot BF, as the test workspace's auth.test says, and its app AHELIX.
const slackHelixApp = "AHELIX"

// slackAppSettings are an app connector's settings, keeping three days.
var slackAppSettings = json.RawMessage(`{"history_days":3,"credential":"helix_app","team_id":"T1"}`)

// slackAppTest is a catalog wired to a fake feed and bucket, with its
// token kind and its app kind, the app's source through an install whose
// calls routes answer, and the calls it made.
type slackAppTest struct {
	poll    *slackKind
	app     *slackAppKind
	feed    *slackFeedFake
	objects *eventObjects
	src     Source
	calls   *slackCalls
}

func newSlackAppTest(t *testing.T, routes map[string]func(*http.Request) (int, string)) *slackAppTest {
	t.Helper()
	poll, _ := slackAt(t, routes)
	feed, objects := newSlackFeedFake(), &eventObjects{objects: map[string][]byte{}, fail: map[string]error{}, opens: map[string]int{}}
	catalog := NewCatalogOf(poll).WithSlackFeed(SlackFeedOptions{Feed: feed, Objects: objects, Budget: model.SlackFeedBudget{Rows: 1000, Bytes: 1 << 20},
		HelixAppID: slackHelixApp, DirectoryEvery: time.Hour, RedocumentEvery: 24 * time.Hour, RepliesDays: 2})
	tokenKind, err := catalog.KindFor("slack", json.RawMessage(`{"history_days":3}`))
	if err != nil {
		t.Fatal(err)
	}
	appKind, err := catalog.KindFor("slack", slackAppSettings)
	if err != nil {
		t.Fatal(err)
	}
	calls := &slackCalls{}
	app := &SlackApp{InstallID: "inst1", TeamID: "T1", BotUserID: "UF", BotID: "BF", Caller: slackRoutesCaller{routes: routes, scopes: slackAllScopes, calls: calls}}
	return &slackAppTest{poll: tokenKind.(*slackKind), app: appKind.(*slackAppKind), feed: feed, objects: objects, calls: calls,
		src: Source{Settings: slackAppSettings, ConnectorID: "conn1", WorkspaceID: "ws1", SlackApp: app}}
}

// fold folds what the feed buffers.
func (a *slackAppTest) fold(t *testing.T) {
	t.Helper()
	if _, err := a.app.Fold(context.Background(), "conn1", 3, *a.src.SlackApp); err != nil {
		t.Fatal(err)
	}
}

// read is a full read of tables into a sink per table, its documents to docs.
func (a *slackAppTest) read(t *testing.T, tables []Table, docs DocumentWriter) map[string]*rowSink {
	t.Helper()
	sinks, rows := map[string]*rowSink{}, map[string]RowWriter{}
	for _, table := range tables {
		sinks[table.Name] = &rowSink{}
		rows[table.Name] = sinks[table.Name]
	}
	if _, err := a.app.SyncPass(context.Background(), a.src, tables, rows, docs); err != nil {
		t.Fatal(err)
	}
	return sinks
}

// mirror is what the mirror holds of channel's day: "ts text" per message
// shown, "-ts" per message gone, by time, and its state.
func (a *slackAppTest) mirror(t *testing.T, channel, day string) (lines []string, state model.SlackDayState) {
	t.Helper()
	d := a.feed.day(channel, day)
	if d.ObjectKey == "" {
		return nil, ""
	}
	m, err := a.app.readSlackDay(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.sorted() {
		switch {
		case e.Gone:
			lines = append(lines, "-"+e.Message.TS)
		default:
			role := map[[2]bool]string{{true, false}: "top", {false, true}: "reply", {true, true}: "both"}[[2]bool{e.Top, e.Reply}]
			lines = append(lines, e.Message.TS+" "+role+" "+e.Message.Text)
		}
	}
	return lines, m.state
}

// The parity fixture: Acme's #general (C1) and #design (C2) as Slack lists
// them now, with the test window of three days and today, 2026-10-27 to 30.
var (
	parityM1 = fmt.Sprintf(`{"type":"message","bot_id":"B9","bot_profile":{"name":"Deploy Bot"},"text":"Deployed v2","ts":%q}`,
		slackTS("2026-10-27T12:00:00Z"))
	parityH1 = fmt.Sprintf(`{"type":"message","user":"UF","bot_id":"BF","app_id":"AHELIX","bot_profile":{"name":"Helix Foundry","app_id":"AHELIX"},`+
		`"text":"Here is the answer","ts":%q}`, slackTS("2026-10-27T13:00:00Z"))
	parityP   = slackTS("2026-10-28T09:15:00.000100Z")
	parityR1  = fmt.Sprintf(`{"user":"U2","text":"Thanks!\nSecond line","ts":%q,"thread_ts":%q}`, slackTS("2026-10-28T09:20:00Z"), parityP)
	parityR2  = fmt.Sprintf(`{"user":"UF","bot_id":"BF","app_id":"AHELIX","text":"A bot reply","ts":%q,"thread_ts":%q}`, slackTS("2026-10-28T09:30:00Z"), parityP)
	parityR3  = fmt.Sprintf(`{"user":"U1","text":"Follow-up next day","ts":%q,"thread_ts":%q}`, slackTS("2026-10-29T08:00:00Z"), parityP)
	parityBTS = slackTS("2026-10-29T10:05:00Z")
	parityB   = fmt.Sprintf(`{"type":"message","subtype":"thread_broadcast","user":"U2","text":"Shipping it","ts":%q,"thread_ts":%q}`, parityBTS, parityP)
	parityPm  = fmt.Sprintf(`{"type":"message","user":"U1","ts":%q,"thread_ts":%q,"reply_count":4,"reply_users_count":3,"latest_reply":%q,`+
		`"edited":{"user":"U1","ts":%q},"text":"Morning <@U2>, see <#C2|design>"}`, parityP, parityP, parityBTS, slackTS("2026-10-28T09:16:00Z"))
	parityL = fmt.Sprintf(`{"type":"message","user":"U1","text":"Lunch? <!here>","ts":%q,"edited":{"ts":%q}}`,
		slackTS("2026-10-28T16:00:00Z"), slackTS("2026-10-28T16:05:00Z"))
	parityD    = slackTS("2026-10-29T11:00:00Z")
	parityRDTS = slackTS("2026-10-29T11:30:00Z")
	parityRD   = fmt.Sprintf(`{"user":"U1","text":"Still here","ts":%q,"thread_ts":%q}`, parityRDTS, parityD)
	parityTomb = fmt.Sprintf(`{"type":"message","subtype":"tombstone","text":"This message was deleted.","user":"USLACKBOT","hidden":true,`+
		`"ts":%q,"thread_ts":%q,"reply_count":1,"reply_users_count":1,"latest_reply":%q}`, parityD, parityD, parityRDTS)
	parityT  = slackTS("2026-10-27T09:00:00Z")
	parityTR = fmt.Sprintf(`{"user":"U1","text":"Old reply","ts":%q,"thread_ts":%q}`, slackTS("2026-10-27T09:10:00Z"), parityT)
	parityTm = fmt.Sprintf(`{"user":"U2","text":"Old thread","ts":%q,"thread_ts":%q,"reply_count":1,"reply_users_count":1,"latest_reply":%q}`,
		parityT, parityT, slackTS("2026-10-27T09:10:00Z"))
	parityQ = fmt.Sprintf(`{"user":"U1","text":"Question <@U2>","ts":%q}`, slackTS("2026-10-29T14:00:00Z"))
)

// newSlackParityWorkspace is Acme as Slack lists it once the fixture's
// history happened: a thread on the 28th whose replies run into the 29th,
// one broadcast to the channel and one Helix's bot's; a message edited; a
// root deleted with a reply (a tombstone); a message deleted; a bot's and
// Helix's bot's posts; and in #design a thread of the 27th, past the
// backfill's replies_days.
func newSlackParityWorkspace() *slackWorkspace {
	ws := newSlackWorkspace()
	ws.users = append(ws.users, `{"id":"UF","team_id":"T1","name":"foundry","real_name":"Helix Foundry","is_bot":true,"profile":{"real_name":"Helix Foundry"}}`)
	ws.history = map[string][]string{
		"C1": {parityTomb, parityB, parityL, parityPm,
			fmt.Sprintf(`{"type":"message","subtype":"channel_join","user":"U3","text":"<@U3> has joined the channel","ts":%q}`, slackTS("2026-10-27T15:30:00Z")),
			parityH1, parityM1},
		"C2": {parityQ, parityTm},
	}
	ws.replies = map[string][]string{
		"C1:" + parityP: {parityPm, parityR1, parityR2, parityR3, parityB},
		"C1:" + parityD: {parityTomb, parityRD},
		"C2:" + parityT: {parityTm, parityTR},
	}
	return ws
}

// slackAt is the time of a message's JSON.
func slackAtOf(raw string) time.Time {
	var m slackMessage
	_ = json.Unmarshal([]byte(raw), &m)
	at, _ := slackTime(m.TS)
	return at
}

// slackDocs are a docs' puts as "id version title url at\ntext", sorted.
func slackDocs(docs *granolaDocs) []string {
	var out []string
	for _, p := range docs.puts {
		out = append(out, strings.Join([]string{p.doc.ExternalID, p.doc.Version, p.doc.Title, p.doc.URL, p.doc.At.Format(time.RFC3339Nano)}, " ")+
			"\n"+string(p.doc.Text))
	}
	slices.Sort(out)
	return out
}

func TestSlackAppParity(t *testing.T) {
	// polling is the token's full read of the fixture.
	ws := newSlackParityWorkspace()
	a := newSlackAppTest(t, ws.routes())
	pollDocs := &granolaDocs{want: map[string]bool{"conversations": true}}
	pollRows := map[string]*rowSink{}
	rows := map[string]RowWriter{}
	for _, table := range slackTables {
		pollRows[table.Name] = &rowSink{}
		rows[table.Name] = pollRows[table.Name]
	}
	if _, err := a.poll.SyncPass(context.Background(), slackSource, slackTables, rows, pollDocs); err != nil {
		t.Fatal(err)
	}
	if got := pollDocs.putNames(); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"conversations C1:2026-10-27", "conversations C1:2026-10-28",
		"conversations C1:2026-10-29", "conversations C2:2026-10-27", "conversations C2:2026-10-29"}) || len(pollRows["messages"].rows) != 8 {
		t.Fatalf("polling documents %v and %d messages", got, len(pollRows["messages"].rows))
	}
	if text := strings.Join(slackDocs(pollDocs), "\n"); strings.Contains(text, "Here is the answer") || strings.Contains(text, "A bot reply") ||
		!strings.Contains(text, "**Helix Foundry** 09:30:\n") {
		t.Fatalf("a token's documents show Helix's bot's text, or no stub of it:\n%s", text)
	}
	for _, row := range pollRows["messages"].rows {
		if row["bot_id"] == "BF" && row["text"] != "" {
			t.Fatalf("a token keeps the text of Helix's bot: %v", row)
		}
	}

	compare := func(t *testing.T, a *slackAppTest, sameDocs func(id string) bool) {
		t.Helper()
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		got := a.read(t, slackTables, docs)
		for _, table := range slackTables {
			if g, w := slackJSON(got[table.Name].rows), slackJSON(pollRows[table.Name].rows); !slices.Equal(g, w) {
				t.Errorf("%s rows differ:\n got %s\nwant %s", table.Name, strings.Join(g, "\n     "), strings.Join(w, "\n     "))
			}
		}
		gotDocs, wantDocs := map[string]Document{}, map[string]Document{}
		for _, p := range docs.puts {
			gotDocs[p.doc.ExternalID] = p.doc
		}
		for _, p := range pollDocs.puts {
			wantDocs[p.doc.ExternalID] = p.doc
		}
		if !slices.Equal(slices.Sorted(maps.Keys(gotDocs)), slices.Sorted(maps.Keys(wantDocs))) {
			t.Fatalf("documents of %v, want %v", slices.Sorted(maps.Keys(gotDocs)), slices.Sorted(maps.Keys(wantDocs)))
		}
		for id, want := range wantDocs {
			got := gotDocs[id]
			if got.Version != want.Version || got.Title != want.Title || got.URL != want.URL || !got.At.Equal(want.At) {
				t.Errorf("document %s is %s %q %s %s, want %s %q %s %s", id, got.Version, got.Title, got.URL, got.At, want.Version, want.Title, want.URL, want.At)
			}
			if same := string(got.Text) == string(want.Text); same != sameDocs(id) {
				t.Errorf("document %s's text (same: %v):\n%s\nwant:\n%s", id, same, got.Text, want.Text)
			}
		}
	}

	t.Run("events write the same rows, documents and versions as polling", func(t *testing.T) {
		a := newSlackAppTest(t, ws.routes())
		event := func(kind model.SlackFeedRowKind, channel, payload string) {
			a.feed.append(kind, model.SlackFeedFromEvent, channel, slackAtOf(payload), payload)
		}
		replied := func(channel, root string, count, users int, latest string) {
			event(model.SlackRowReplied, channel, fmt.Sprintf(`{"ts":%q,"reply_count":%d,"reply_users_count":%d,"latest_reply":%q}`, root, count, users, latest))
		}
		ts := func(raw string) string {
			var m slackMessage
			_ = json.Unmarshal([]byte(raw), &m)
			return m.TS
		}
		event(model.SlackRowMessage, "C2", fmt.Sprintf(`{"user":"U2","text":"Old thread","ts":%q}`, parityT))
		event(model.SlackRowMessage, "C2", parityTR)
		replied("C2", parityT, 1, 1, ts(parityTR))
		event(model.SlackRowMessage, "C1", parityM1)
		event(model.SlackRowMessage, "C1", parityH1)
		event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Morning","ts":%q}`, parityP))
		// The edit, before any reply: its reply fields are none.
		event(model.SlackRowChanged, "C1", fmt.Sprintf(`{"user":"U1","ts":%q,"edited":{"user":"U1","ts":%q},"text":"Morning <@U2>, see <#C2|design>"}`,
			parityP, slackTS("2026-10-28T09:16:00Z")))
		event(model.SlackRowMessage, "C1", parityR1)
		replied("C1", parityP, 1, 1, ts(parityR1))
		event(model.SlackRowMessage, "C1", parityR2)
		replied("C1", parityP, 2, 2, ts(parityR2))
		event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Lunch?","ts":%q}`, slackTS("2026-10-28T16:00:00Z")))
		event(model.SlackRowChanged, "C1", parityL)
		event(model.SlackRowMessage, "C1", parityR3)
		replied("C1", parityP, 3, 3, ts(parityR3))
		event(model.SlackRowMessage, "C1", parityB)
		replied("C1", parityP, 4, 3, parityBTS)
		event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Oops","ts":%q}`, parityD))
		event(model.SlackRowMessage, "C1", parityRD)
		replied("C1", parityD, 1, 1, parityRDTS)
		event(model.SlackRowChanged, "C1", parityTomb)
		typo := slackTS("2026-10-29T12:00:00Z")
		event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Typo","ts":%q}`, typo))
		event(model.SlackRowDeleted, "C1", fmt.Sprintf(`{"deleted_ts":%q,"ts":%q}`, typo, typo))
		event(model.SlackRowMessage, "C2", parityQ)
		for _, cd := range [][2]string{{"C1", "2026-10-27"}, {"C1", "2026-10-28"}, {"C1", "2026-10-29"}, {"C2", "2026-10-27"}, {"C2", "2026-10-28"}, {"C2", "2026-10-29"}} {
			day, _ := time.Parse(time.DateOnly, cd[1])
			a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, cd[0], day, `{}`)
		}
		a.fold(t)
		compare(t, a, func(string) bool { return true })
		if lines, _ := a.mirror(t, "C1", "2026-10-28"); slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "A bot reply") }) {
			t.Fatalf("the mirror keeps Helix's bot's text: %v", lines)
		}
	})

	t.Run("backfill pages write the same rows and versions, and replies within replies_days", func(t *testing.T) {
		a := newSlackAppTest(t, ws.routes())
		page := func(channel string, thread string, messages []string) {
			var listed []string
			for _, raw := range messages {
				var m slackMessage
				_ = json.Unmarshal([]byte(raw), &m)
				if m.TS == thread {
					continue
				}
				listed = append(listed, m.TS)
				a.feed.append(model.SlackRowMessage, model.SlackFeedFromBackfill, channel, slackAtOf(raw), raw)
			}
			from, to := slackTS("2026-10-27T00:00:00Z"), slackTS("2026-10-30T15:00:00Z")
			a.feed.append(model.SlackRowSpan, model.SlackFeedFromBackfill, channel, slackTestNow,
				string(encodeJSON(SlackSpan{From: from, To: to, ThreadTS: thread, Listed: listed})))
		}
		for _, channel := range []string{"C1", "C2"} {
			page(channel, "", ws.history[channel])
		}
		// replies_days 2: the threads of the 28th and 29th, not #design's of the 27th.
		page("C1", parityP, ws.replies["C1:"+parityP])
		page("C1", parityD, ws.replies["C1:"+parityD])
		for _, cd := range [][2]string{{"C1", "2026-10-27"}, {"C1", "2026-10-28"}, {"C1", "2026-10-29"}, {"C2", "2026-10-27"}, {"C2", "2026-10-29"}} {
			day, _ := time.Parse(time.DateOnly, cd[1])
			a.feed.append(model.SlackRowComplete, model.SlackFeedFromBackfill, cd[0], day, `{}`)
		}
		a.fold(t)
		compare(t, a, func(id string) bool { return id != "C2:2026-10-27" })
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		a.read(t, slackFollowedTables, docs)
		for _, p := range docs.puts {
			if p.doc.ExternalID == "C2:2026-10-27" && strings.Contains(string(p.doc.Text), "Old reply") {
				t.Fatalf("a day past replies_days lists replies the backfill never read:\n%s", p.doc.Text)
			}
		}
	})
}
