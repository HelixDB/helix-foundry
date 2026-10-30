package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// slackAccountNames are the channels a slackAccount holds: #general and #design.
var slackAccountNames = map[string]string{"C1": "general", "C2": "design"}

// slackAccount is a Slack workspace a test edits between syncs: Ada and
// Grace, and the channels of slackAccountNames the app is in (joined, at
// first #general alone), each with its members and its messages by ts, and
// each thread's replies by its parent's ts. Its token has every scope the
// guide asks for. It counts conversations.replies calls, keeps the channel
// and UTC day each read of history from a day on starts from, and waits
// wait before answering one, when set.
type slackAccount struct {
	mu       sync.Mutex
	joined   []string
	members  map[string][]string
	messages map[string]map[string]string // by channel, then ts: text
	replies  map[string][]string          // a thread parent's ts: its replies' ts, each said "Re: <parent's text>"
	threads  int
	since    []string // "<channel>@<day>"
	wait     func()
}

// newSlackAccount is an account whose #general holds messages.
func newSlackAccount(messages map[string]string) *slackAccount {
	return &slackAccount{joined: []string{"C1"}, members: map[string][]string{"C1": {"U1"}, "C2": {"U1", "U2"}},
		messages: map[string]map[string]string{"C1": messages, "C2": {}}, replies: map[string][]string{}}
}

func (a *slackAccount) serve(t *testing.T) string {
	t.Helper()
	page := func(field string, items []string) string {
		return fmt.Sprintf(`{"ok":true,%q:[%s],"response_metadata":{"next_cursor":""}}`, field, strings.Join(items, ","))
	}
	message := func(channel, ts string, replies []string) string {
		thread := ""
		if len(replies) > 0 {
			thread = fmt.Sprintf(`,"thread_ts":%q,"reply_count":%d,"latest_reply":%q`, ts, len(replies), replies[len(replies)-1])
		}
		return fmt.Sprintf(`{"type":"message","user":"U1","text":%q,"ts":%q%s}`, a.messages[channel][ts], ts, thread)
	}
	channel := func(id string) string {
		return fmt.Sprintf(`{"id":%q,"name":%q,"is_member":true}`, id, slackAccountNames[id])
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a.mu.Lock()
		wait := a.wait
		a.mu.Unlock()
		if q.Has("oldest") && !q.Has("latest") && wait != nil {
			wait()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("X-OAuth-Scopes", "channels:read,channels:history,groups:read,groups:history,users:read,users:read.email")
		var body string
		switch strings.TrimPrefix(r.URL.Path, "/api/") {
		case "auth.test":
			body = `{"ok":true,"user":"foundry","team_id":"T1"}`
		case "users.list":
			body = page("members", []string{`{"id":"U1","name":"ada","real_name":"Ada Lovelace"}`, `{"id":"U2","name":"grace","real_name":"Grace Hopper"}`})
		case "conversations.list":
			body = page("channels", []string{channel("C1"), channel("C2")})
		case "users.conversations":
			var listed []string
			for _, id := range a.joined {
				listed = append(listed, channel(id))
			}
			body = page("channels", listed)
		case "conversations.members":
			var listed []string
			for _, user := range a.members[q.Get("channel")] {
				listed = append(listed, strconv.Quote(user))
			}
			body = page("members", listed)
		case "conversations.history":
			id := q.Get("channel")
			if q.Has("oldest") && !q.Has("latest") {
				seconds, _, _ := strings.Cut(q.Get("oldest"), ".")
				unix, _ := strconv.ParseInt(seconds, 10, 64)
				a.since = append(a.since, id+"@"+time.Unix(unix, 0).UTC().Format(time.DateOnly))
			}
			var listed []string
			for _, ts := range slices.Backward(slices.Sorted(maps.Keys(a.messages[id]))) {
				listed = append(listed, message(id, ts, a.replies[ts]))
			}
			body = page("messages", listed)
		case "conversations.replies":
			a.threads++
			id, parent := q.Get("channel"), q.Get("ts")
			listed := []string{message(id, parent, a.replies[parent])}
			for _, ts := range a.replies[parent] {
				listed = append(listed, fmt.Sprintf(`{"user":"U1","text":%q,"ts":%q,"thread_ts":%q}`, "Re: "+a.messages[id][parent], ts, parent))
			}
			body = page("messages", listed)
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL + "/api"
}

// edit changes the account while no request is served.
func (a *slackAccount) edit(change func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change()
}

// took is how many threads were read since the last call.
func (a *slackAccount) took() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	threads := a.threads
	a.threads = 0
	return threads
}

// read are the channels and days history was read from since the last call, sorted.
func (a *slackAccount) read() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	since := a.since
	a.since = nil
	slices.Sort(since)
	return since
}

// countedSnapshots are a fixture's snapshots, counting the reads of one: a
// merge of a change read reads its table's.
type countedSnapshots struct {
	*memorySnapshots
	opens int
}

func (c *countedSnapshots) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	c.opens++
	return c.memorySnapshots.Open(ctx, key)
}

// newSlackFixture syncs account with settings on plan.
func newSlackFixture(t *testing.T, account *slackAccount, settings, code string) (fixture, *countedSnapshots) {
	t.Helper()
	f := newFixture(t, "http://unused", "slack", settings, plan{code: code})
	endpoints := connector.DefaultEndpoints()
	endpoints.Slack = account.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	counted := &countedSnapshots{memorySnapshots: f.snapshots}
	f.syncer.Snapshots = counted
	return f, counted
}

// slackClock is the day the connector counts its window from, ChangeOverlap
// before the wall clock, and functions of it: at is clock past midnight
// days before it, ts a time as Slack's ts, and day a time's UTC day.
func slackClock() (today time.Time, at func(days int, clock time.Duration) time.Time, ts, day func(time.Time) string) {
	today = time.Now().UTC().Add(-connector.ChangeOverlap).Truncate(24 * time.Hour)
	return today, func(days int, clock time.Duration) time.Time { return today.AddDate(0, 0, -days).Add(clock) },
		func(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }, func(t time.Time) string { return t.Format(time.DateOnly) }
}

// slackRowIDs are the keys of table's snapshot rows, by column, sorted.
func slackRowIDs(t *testing.T, f fixture, table, column string) []string {
	t.Helper()
	var ids []string
	for _, row := range f.snapshotRows(t, table) {
		var fields map[string]any
		if err := json.Unmarshal([]byte(row), &fields); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, fmt.Sprint(fields[column]))
	}
	slices.Sort(ids)
	return ids
}

// TestSlackSyncDocuments runs Slack through the syncer on Free, which reads
// in full on every sync: the first sync fills the conversations set with a
// document per channel day, an unchanged sync reads no thread and writes
// nothing, a reply replaces only its day's document, and a day whose
// messages are gone loses its document with its row.
func TestSlackSyncDocuments(t *testing.T) {
	_, at, ts, _ := slackClock()
	kickoff, standup := at(2, 9*time.Hour), at(1, 9*time.Hour)
	account := newSlackAccount(map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"})
	account.replies[ts(kickoff)] = []string{ts(kickoff.Add(30 * time.Minute))}
	f, _ := newSlackFixture(t, account, `{"history_days":7}`, "free")
	day := func(t time.Time) string { return "C1:" + t.Format(time.DateOnly) }
	document := func(t time.Time, lines string) string {
		return fmt.Sprintf("# #general, %s\n\nChannel: #general (C1)\n\n%s", t.Format(time.DateOnly), lines)
	}

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || len(f.store.tables) != 5 {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	if threads := account.took(); threads != 1 {
		t.Fatalf("threads read = %d; want the kickoff's", threads)
	}
	conversations := f.held(t, "conversations")
	want := map[string]string{
		day(kickoff): document(kickoff, "**Ada Lovelace** 09:00: Kickoff\n  - **Ada Lovelace** 09:30: Re: Kickoff\n"),
		day(standup): document(standup, "**Ada Lovelace** 09:00: Standup\n"),
	}
	if fmt.Sprint(conversations) != fmt.Sprint(want) {
		t.Fatalf("conversations = %q", conversations)
	}
	if set := f.store.setOf("conversations"); set.Name != "Conversations" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("conversations set = %+v", set)
	}

	// Nothing changed: every day is held at its version, so no thread is read and nothing is written.
	publishes := len(f.store.publishes)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s", res.Mode)
	}
	if threads := account.took(); threads != 0 || len(f.store.publishes) != publishes {
		t.Fatalf("threads read = %d, publishes = %d", threads, len(f.store.publishes)-publishes)
	}

	// A reply to the standup replaces its day's document and reads only its thread.
	kept := f.liveID("conversations", day(kickoff))
	account.edit(func() { account.replies[ts(standup)] = []string{ts(standup.Add(time.Hour))} })
	if res := f.run(t, "sync-3"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s", res.Mode)
	}
	if threads := account.took(); threads != 1 {
		t.Fatalf("threads read = %d; want the standup's", threads)
	}
	if got := f.held(t, "conversations")[day(standup)]; got != document(standup, "**Ada Lovelace** 09:00: Standup\n  - **Ada Lovelace** 10:00: Re: Standup\n") {
		t.Fatalf("standup = %q", got)
	}
	if f.liveID("conversations", day(kickoff)) != kept {
		t.Fatalf("the kickoff's document was replaced")
	}

	// The kickoff deleted, its day has no row, and its document goes.
	account.edit(func() { delete(account.messages["C1"], ts(kickoff)) })
	f.run(t, "sync-4")
	if conversations := f.held(t, "conversations"); len(conversations) != 1 || conversations[day(standup)] == "" {
		t.Fatalf("conversations = %q", conversations)
	}
	if threads := account.took(); threads != 0 {
		t.Fatalf("threads read = %d", threads)
	}
}

// TestSlackSyncNearDeadline runs a full read of Slack that reaches its
// documents too near its attempt's deadline: it fetches none, and the
// conversations set keeps what it holds, though their days changed, until
// the next full read writes them, all but today's.
func TestSlackSyncNearDeadline(t *testing.T) {
	today, at, ts, _ := slackClock()
	kickoff, standup := at(2, 9*time.Hour), at(1, 9*time.Hour)
	account := newSlackAccount(map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"})
	f, _ := newSlackFixture(t, account, `{"history_days":7}`, "free")
	f.run(t, "sync-1")
	before := f.held(t, "conversations")
	if len(before) != 2 {
		t.Fatalf("conversations = %q", before)
	}

	// A reply to the standup, and a message on a new day; the window's
	// history answers 400ms before the deadline, past when documents stop
	// being fetched (a quarter of the time left, 500ms).
	deadline := time.Now().Add(2 * time.Second)
	account.edit(func() {
		account.replies[ts(standup)] = []string{ts(standup.Add(time.Hour))}
		account.messages["C1"][ts(at(0, 0))] = "Midnight"
		account.wait = func() { time.Sleep(time.Until(deadline.Add(-400 * time.Millisecond))) }
	})
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if _, err := f.syncer.Run(ctx, newSync("sync-2")); err != nil {
		t.Fatalf("sync-2: %v", err)
	}
	account.edit(func() { account.wait = nil })
	if after := f.held(t, "conversations"); fmt.Sprint(after) != fmt.Sprint(before) || account.took() != 0 {
		t.Fatalf("conversations = %q, want them as held", after)
	}
	if rows := f.snapshotRows(t, "channel_days"); len(rows) != 3 {
		t.Fatalf("channel_days = %q", rows)
	}

	// The next sync writes the standup's new reply; today's day, still
	// going, has its row and no document until it is over.
	f.run(t, "sync-3")
	if after := f.held(t, "conversations"); len(after) != 2 || after["C1:"+standup.Format(time.DateOnly)] == before["C1:"+standup.Format(time.DateOnly)] ||
		after["C1:"+today.Format(time.DateOnly)] != "" {
		t.Fatalf("conversations = %q", after)
	}
	if rows := f.snapshotRows(t, "channel_days"); len(rows) != 3 {
		t.Fatalf("channel_days = %q", rows)
	}
}

// TestSlackSyncChanges runs Slack on Pro, whose syncs after the first
// follow messages, channel days and members by reading each channel's
// history again from the cursor's day, and its members beside it: the
// sync after the full read writes today's days and reads members in full,
// for want of digests; an unchanged sync then writes nothing and reads no
// snapshot; a message today changes its rows and writes no document; an
// edit on an older day waits for the reconcile; a cursor a sync before
// midnight left reads yesterday again, where a day emptied loses its row
// and its document; and the reconcile 24 hours after the full read reads
// in full.
func TestSlackSyncChanges(t *testing.T) {
	today, at, ts, day := slackClock()
	kickoff, standup, midnight := at(2, 9*time.Hour), at(1, 9*time.Hour), at(0, 0)
	account := newSlackAccount(map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"})
	f, snapshots := newSlackFixture(t, account, `{"history_days":7}`, "pro")

	if res, read := f.run(t, "sync-1"), account.read(); res.Mode != model.ConnectorSyncFull || !slices.Equal(read, []string{"C1@" + day(at(7, 0))}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	first := f.held(t, "conversations")
	if len(first) != 2 {
		t.Fatalf("conversations = %q", first)
	}

	// Today's history alone is read, and nothing changes.
	publishes := len(f.store.publishes)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 || len(f.store.publishes) != publishes {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if read := account.read(); !slices.Equal(read, []string{"C1@" + day(today)}) {
		t.Fatalf("history read from %q", read)
	}

	// The quiet hour: the cursor's digests are the day's and the members', so no table is merged.
	opens, batches := snapshots.opens, fmt.Sprint(f.store.batches)
	if res := f.run(t, "sync-3"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 || snapshots.opens != opens ||
		fmt.Sprint(f.store.batches) != batches || len(f.store.publishes) != publishes {
		t.Fatalf("mode = %s, changed = %v, snapshots read = %d", res.Mode, res.Changed, snapshots.opens-opens)
	}
	account.read()

	// A message today: its rows change, and the day gets no document while it lasts.
	account.edit(func() { account.messages["C1"][ts(midnight)] = "Midnight" })
	res := f.run(t, "sync-4")
	if slices.Sort(res.Changed); res.Mode != model.ConnectorSyncChanges || !slices.Equal(res.Changed, []string{"channel_days", "messages"}) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if got := slackRowIDs(t, f, "channel_days", "channel_day_id"); !slices.Equal(got, []string{"C1:" + day(kickoff), "C1:" + day(standup), "C1:" + day(today)}) {
		t.Fatalf("channel_days = %q", got)
	}
	if got := f.held(t, "conversations"); fmt.Sprint(got) != fmt.Sprint(first) {
		t.Fatalf("conversations = %q", got)
	}

	// An edit two days ago is a day no change read reads again.
	account.edit(func() { account.messages["C1"][ts(kickoff)] = "Kickoff, edited" })
	if res := f.run(t, "sync-5"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 || fmt.Sprint(f.held(t, "conversations")) != fmt.Sprint(first) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	account.read()

	// The cursor a sync before midnight left reads yesterday again (here
	// with no digest of it, so it is rewritten): the standup deleted, its
	// day loses its row and its document.
	var cursor map[string]any
	if err := json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &cursor); err != nil || cursor["day"] != day(today) || cursor["members"] == nil {
		t.Fatalf("cursor = %s", f.store.connectors["conn-1"].ChangeCursor)
	}
	cursor["day"], cursor["days"] = day(standup), nil
	f.store.connectors["conn-1"].ChangeCursor, _ = json.Marshal(cursor)
	account.edit(func() { delete(account.messages["C1"], ts(standup)) })
	if res, read := f.run(t, "sync-6"), account.read(); res.Mode != model.ConnectorSyncChanges || !slices.Equal(read, []string{"C1@" + day(standup)}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	if got := slackRowIDs(t, f, "channel_days", "channel_day_id"); !slices.Equal(got, []string{"C1:" + day(kickoff), "C1:" + day(today)}) {
		t.Fatalf("channel_days = %q", got)
	}
	if got := f.held(t, "conversations"); len(got) != 1 || got["C1:"+day(kickoff)] != first["C1:"+day(kickoff)] {
		t.Fatalf("conversations = %q", got)
	}
	if rows := f.snapshotRows(t, "messages"); len(rows) != 2 {
		t.Fatalf("messages = %q", rows)
	}

	// A day after the full read, the reconcile reads in full and finds the edit.
	f.syncer.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if res, read := f.run(t, "sync-7"), account.read(); res.Mode != model.ConnectorSyncFull || !slices.Equal(read, []string{"C1@" + day(at(7, 0))}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	if got := f.held(t, "conversations")["C1:"+day(kickoff)]; !strings.Contains(got, "Kickoff, edited") {
		t.Fatalf("kickoff = %q", got)
	}
}

// TestSlackSyncChannels runs Slack on Pro while the app joins and leaves
// channels: a channel joined resets the history tables, which the same
// sync reads in full, so its days get their rows and documents at once;
// messages deleted from two channels leave both through the merge; and a
// channel left loses its messages, days, members and documents.
func TestSlackSyncChannels(t *testing.T) {
	today, at, ts, day := slackClock()
	kickoff, review := at(2, 9*time.Hour), at(1, 11*time.Hour)
	account := newSlackAccount(map[string]string{ts(kickoff): "Kickoff", ts(at(0, time.Minute)): "Stand-up"})
	account.messages["C2"] = map[string]string{ts(review): "Design review", ts(at(0, 2*time.Minute)): "Mock-ups"}
	f, _ := newSlackFixture(t, account, `{"history_days":7}`, "pro")
	f.run(t, "sync-1")
	f.run(t, "sync-2")
	account.read()

	// #design joined: the history tables are read in full, every channel's.
	account.edit(func() { account.joined = []string{"C1", "C2"} })
	res := f.run(t, "sync-3")
	if slices.Sort(res.Changed); res.Mode != model.ConnectorSyncChanges || !slices.Equal(res.Changed, []string{"channel_days", "channel_members", "messages"}) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if read := account.read(); !slices.Equal(read, []string{"C1@" + day(at(7, 0)), "C2@" + day(at(7, 0))}) {
		t.Fatalf("history read from %q", read)
	}
	if got := slices.Sorted(maps.Keys(f.held(t, "conversations"))); !slices.Equal(got, []string{"C1:" + day(kickoff), "C2:" + day(review)}) {
		t.Fatalf("conversations = %q", got)
	}
	if got := f.snapshotRows(t, "channel_members"); len(got) != 3 {
		t.Fatalf("channel_members = %q", got)
	}

	// Today's message deleted in each channel: the day is written again without either.
	account.edit(func() {
		delete(account.messages["C1"], ts(at(0, time.Minute)))
		delete(account.messages["C2"], ts(at(0, 2*time.Minute)))
	})
	f.run(t, "sync-4")
	if got := slackRowIDs(t, f, "messages", "message_id"); !slices.Equal(got, []string{"C1:" + ts(kickoff), "C2:" + ts(review)}) {
		t.Fatalf("messages = %q", got)
	}
	if got := slackRowIDs(t, f, "channel_days", "channel_day_id"); slices.Contains(got, "C1:"+day(today)) || slices.Contains(got, "C2:"+day(today)) {
		t.Fatalf("channel_days = %q", got)
	}

	// #general left: its rows and its document go, and #design's stay.
	account.edit(func() { account.joined = []string{"C2"} })
	res = f.run(t, "sync-5")
	if slices.Sort(res.Changed); res.Mode != model.ConnectorSyncChanges || !slices.Equal(res.Changed, []string{"channel_days", "channel_members", "messages"}) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if got := slackRowIDs(t, f, "messages", "message_id"); !slices.Equal(got, []string{"C2:" + ts(review)}) {
		t.Fatalf("messages = %q", got)
	}
	if got := slackRowIDs(t, f, "channel_members", "channel_id"); !slices.Equal(got, []string{"C2", "C2"}) {
		t.Fatalf("channel_members = %q", got)
	}
	if got := slices.Sorted(maps.Keys(f.held(t, "conversations"))); !slices.Equal(got, []string{"C2:" + day(review)}) {
		t.Fatalf("conversations = %q", got)
	}
}

// TestSlackSyncHistoryDays runs Slack on Pro when its history_days changes
// between syncs: the change read resets the history tables, which the same
// sync reads in full over the new window, and a day that left it goes.
func TestSlackSyncHistoryDays(t *testing.T) {
	_, at, ts, day := slackClock()
	kickoff, standup := at(5, 9*time.Hour), at(1, 9*time.Hour)
	account := newSlackAccount(map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"})
	f, _ := newSlackFixture(t, account, `{"history_days":7}`, "pro")
	f.run(t, "sync-1")
	f.run(t, "sync-2")
	account.read()

	f.store.connectors["conn-1"].Config = json.RawMessage(`{"history_days":3}`)
	if res, read := f.run(t, "sync-3"), account.read(); res.Mode != model.ConnectorSyncChanges || !slices.Equal(read, []string{"C1@" + day(at(3, 0))}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	if got := slackRowIDs(t, f, "messages", "message_id"); !slices.Equal(got, []string{"C1:" + ts(standup)}) {
		t.Fatalf("messages = %q", got)
	}
	var cursor struct {
		HistoryDays int `json:"history_days"`
	}
	if err := json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &cursor); err != nil || cursor.HistoryDays != 3 {
		t.Fatalf("cursor = %s", f.store.connectors["conn-1"].ChangeCursor)
	}
}

// TestSlackSyncChangesNearDeadline runs a change read of Slack that wants a
// finished day's document too near its attempt's deadline: the cursor
// keeps that day for that channel alone, so the next sync reads the
// channel from it and writes the document, while the other channel is read
// from today.
func TestSlackSyncChangesNearDeadline(t *testing.T) {
	today, at, ts, day := slackClock()
	standup := at(1, 9*time.Hour)
	account := newSlackAccount(map[string]string{ts(standup): "Standup"})
	account.joined = []string{"C1", "C2"}
	account.messages["C2"] = map[string]string{ts(at(0, time.Minute)): "Mock-ups"}
	f, _ := newSlackFixture(t, account, `{"history_days":7}`, "pro")
	f.run(t, "sync-1")
	f.run(t, "sync-2")
	before := f.held(t, "conversations")

	// A reply to the standup, read by a cursor a sync before midnight left;
	// history answers past when documents stop being fetched.
	var cursor map[string]any
	_ = json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &cursor)
	cursor["day"], cursor["days"] = day(standup), nil
	f.store.connectors["conn-1"].ChangeCursor, _ = json.Marshal(cursor)
	deadline := time.Now().Add(2 * time.Second)
	account.edit(func() {
		account.replies[ts(standup)] = []string{ts(standup.Add(time.Hour))}
		account.wait = func() { time.Sleep(time.Until(deadline.Add(-400 * time.Millisecond))) }
	})
	account.read()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if res, err := f.syncer.Run(ctx, newSync("sync-3")); err != nil || res.Mode != model.ConnectorSyncChanges {
		t.Fatalf("sync-3: %v, mode = %s", err, res.Mode)
	}
	account.edit(func() { account.wait = nil })
	if after := f.held(t, "conversations"); fmt.Sprint(after) != fmt.Sprint(before) || account.took() != 0 {
		t.Fatalf("conversations = %q, want them as held", after)
	}
	var next struct {
		Day  string            `json:"day"`
		From map[string]string `json:"from"`
	}
	if err := json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &next); err != nil || next.Day != day(today) ||
		fmt.Sprint(next.From) != fmt.Sprintf("map[C1:%s]", day(standup)) {
		t.Fatalf("cursor = %s", f.store.connectors["conn-1"].ChangeCursor)
	}
	account.read()

	f.run(t, "sync-4")
	if read := account.read(); !slices.Equal(read, []string{"C1@" + day(standup), "C2@" + day(today)}) {
		t.Fatalf("history read from %q", read)
	}
	if got := f.held(t, "conversations")["C1:"+day(standup)]; !strings.Contains(got, "Re: Standup") || account.took() != 1 {
		t.Fatalf("standup = %q", got)
	}
	if cursor := f.store.connectors["conn-1"].ChangeCursor; strings.Contains(string(cursor), `"from"`) {
		t.Fatalf("cursor = %s", cursor)
	}
}
