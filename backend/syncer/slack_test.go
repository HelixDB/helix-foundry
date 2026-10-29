package syncer

import (
	"context"
	"encoding/json"
	"fmt"
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

// slackAccount is a Slack workspace a test edits between syncs: Ada, and
// #general, the one channel the app is in, with its messages and each
// thread's replies by their ts. Its token has every scope the guide asks
// for. It counts conversations.replies calls, keeps the UTC day each read
// of history from a day on starts from, and waits wait before answering
// one, when set.
type slackAccount struct {
	mu       sync.Mutex
	messages map[string]string   // ts: text
	replies  map[string][]string // a thread parent's ts: its replies' ts, each said "Re: <parent's text>"
	threads  int
	since    []string
	wait     func()
}

func (a *slackAccount) serve(t *testing.T) string {
	t.Helper()
	page := func(field string, items []string) string {
		return fmt.Sprintf(`{"ok":true,%q:[%s],"response_metadata":{"next_cursor":""}}`, field, strings.Join(items, ","))
	}
	message := func(ts, text string, replies []string) string {
		thread := ""
		if len(replies) > 0 {
			thread = fmt.Sprintf(`,"thread_ts":%q,"reply_count":%d,"latest_reply":%q`, ts, len(replies), replies[len(replies)-1])
		}
		return fmt.Sprintf(`{"type":"message","user":"U1","text":%q,"ts":%q%s}`, text, ts, thread)
	}
	general := `{"id":"C1","name":"general","is_member":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		wait := a.wait
		a.mu.Unlock()
		if q := r.URL.Query(); q.Has("oldest") && !q.Has("latest") && wait != nil {
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
			body = page("members", []string{`{"id":"U1","name":"ada","real_name":"Ada Lovelace"}`})
		case "conversations.list", "users.conversations":
			body = page("channels", []string{general})
		case "conversations.members":
			body = page("members", []string{`"U1"`})
		case "conversations.history":
			if q := r.URL.Query(); q.Has("oldest") && !q.Has("latest") {
				seconds, _, _ := strings.Cut(q.Get("oldest"), ".")
				unix, _ := strconv.ParseInt(seconds, 10, 64)
				a.since = append(a.since, time.Unix(unix, 0).UTC().Format(time.DateOnly))
			}
			var listed []string
			for _, ts := range slices.Backward(slices.Sorted(maps.Keys(a.messages))) {
				listed = append(listed, message(ts, a.messages[ts], a.replies[ts]))
			}
			body = page("messages", listed)
		case "conversations.replies":
			a.threads++
			parent := r.URL.Query().Get("ts")
			listed := []string{message(parent, a.messages[parent], a.replies[parent])}
			for _, ts := range a.replies[parent] {
				listed = append(listed, fmt.Sprintf(`{"user":"U1","text":%q,"ts":%q,"thread_ts":%q}`, "Re: "+a.messages[parent], ts, parent))
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

// took is how many threads were read since the last call.
func (a *slackAccount) took() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	threads := a.threads
	a.threads = 0
	return threads
}

// read are the days history was read from since the last call.
func (a *slackAccount) read() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	since := a.since
	a.since = nil
	return since
}

// TestSlackSyncDocuments runs Slack through the syncer on Free, which reads
// in full on every sync: the first sync fills the conversations set with a
// document per channel day, an unchanged sync reads no thread and writes
// nothing, a reply replaces only its day's document, and a day whose
// messages are gone loses its document with its row.
func TestSlackSyncDocuments(t *testing.T) {
	// The connector counts its window from the wall clock, ChangeOverlap ago.
	today := time.Now().UTC().Add(-connector.ChangeOverlap).Truncate(24 * time.Hour)
	at := func(days int, clock time.Duration) time.Time { return today.AddDate(0, 0, -days).Add(clock) }
	ts := func(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }
	kickoff, standup := at(2, 9*time.Hour), at(1, 9*time.Hour)
	account := &slackAccount{
		messages: map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"},
		replies:  map[string][]string{ts(kickoff): {ts(kickoff.Add(30 * time.Minute))}},
	}
	f := newFixture(t, "http://unused", "slack", `{"history_days":7}`, plan{code: "free"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Slack = account.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
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
	account.mu.Lock()
	account.replies[ts(standup)] = []string{ts(standup.Add(time.Hour))}
	account.mu.Unlock()
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
	account.mu.Lock()
	delete(account.messages, ts(kickoff))
	account.mu.Unlock()
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
	today := time.Now().UTC().Add(-connector.ChangeOverlap).Truncate(24 * time.Hour)
	at := func(days int, clock time.Duration) time.Time { return today.AddDate(0, 0, -days).Add(clock) }
	ts := func(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }
	kickoff, standup := at(2, 9*time.Hour), at(1, 9*time.Hour)
	account := &slackAccount{messages: map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"}, replies: map[string][]string{}}
	f := newFixture(t, "http://unused", "slack", `{"history_days":7}`, plan{code: "free"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Slack = account.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	f.run(t, "sync-1")
	before := f.held(t, "conversations")
	if len(before) != 2 {
		t.Fatalf("conversations = %q", before)
	}

	// A reply to the standup, and a message on a new day; the window's
	// history answers 400ms before the deadline, past when documents stop
	// being fetched (a quarter of the time left, 500ms).
	account.mu.Lock()
	account.replies[ts(standup)] = []string{ts(standup.Add(time.Hour))}
	account.messages[ts(at(0, 0))] = "Midnight"
	deadline := time.Now().Add(2 * time.Second)
	account.wait = func() { time.Sleep(time.Until(deadline.Add(-400 * time.Millisecond))) }
	account.mu.Unlock()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if _, err := f.syncer.Run(ctx, newSync("sync-2")); err != nil {
		t.Fatalf("sync-2: %v", err)
	}
	account.mu.Lock()
	account.wait = nil
	account.mu.Unlock()
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
// follow messages and channel days by reading history again from the
// cursor's day: an unchanged sync reads today alone and changes nothing, a
// message today changes its rows and writes no document, an edit on an
// older day waits for the reconcile, a cursor a sync before midnight left
// reads yesterday again, where a day emptied loses its row and its
// document, and the reconcile 24 hours after the full read reads in full.
func TestSlackSyncChanges(t *testing.T) {
	today := time.Now().UTC().Add(-connector.ChangeOverlap).Truncate(24 * time.Hour)
	at := func(days int, clock time.Duration) time.Time { return today.AddDate(0, 0, -days).Add(clock) }
	ts := func(t time.Time) string { return fmt.Sprintf("%d.000000", t.Unix()) }
	day := func(t time.Time) string { return t.Format(time.DateOnly) }
	kickoff, standup, midnight := at(2, 9*time.Hour), at(1, 9*time.Hour), at(0, 0)
	account := &slackAccount{messages: map[string]string{ts(kickoff): "Kickoff", ts(standup): "Standup"}, replies: map[string][]string{}}
	f := newFixture(t, "http://unused", "slack", `{"history_days":7}`, plan{code: "pro"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Slack = account.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	edit := func(change func()) {
		account.mu.Lock()
		defer account.mu.Unlock()
		change()
	}
	days := func(t *testing.T) []string {
		t.Helper()
		var ids []string
		for _, row := range f.snapshotRows(t, "channel_days") {
			var d struct {
				ID string `json:"channel_day_id"`
			}
			if err := json.Unmarshal([]byte(row), &d); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, d.ID)
		}
		return ids
	}

	if res, read := f.run(t, "sync-1"), account.read(); res.Mode != model.ConnectorSyncFull || !slices.Equal(read, []string{day(at(7, 0))}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	first := f.held(t, "conversations")
	if len(first) != 2 {
		t.Fatalf("conversations = %q", first)
	}

	// Unchanged: today's history alone is read, and nothing changes.
	publishes := len(f.store.publishes)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 || len(f.store.publishes) != publishes {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if read := account.read(); !slices.Equal(read, []string{day(today)}) {
		t.Fatalf("history read from %q", read)
	}

	// A message today: its rows change, and the day gets no document while it lasts.
	edit(func() { account.messages[ts(midnight)] = "Midnight" })
	res := f.run(t, "sync-3")
	if slices.Sort(res.Changed); res.Mode != model.ConnectorSyncChanges || !slices.Equal(res.Changed, []string{"channel_days", "messages"}) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if got := days(t); !slices.Equal(got, []string{"C1:" + day(kickoff), "C1:" + day(standup), "C1:" + day(today)}) {
		t.Fatalf("channel_days = %q", got)
	}
	if got := f.held(t, "conversations"); fmt.Sprint(got) != fmt.Sprint(first) {
		t.Fatalf("conversations = %q", got)
	}

	// An edit two days ago is a day no change read reads again.
	edit(func() { account.messages[ts(kickoff)] = "Kickoff, edited" })
	if res := f.run(t, "sync-4"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 || fmt.Sprint(f.held(t, "conversations")) != fmt.Sprint(first) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	account.read()

	// The cursor a sync before midnight left reads yesterday again: the
	// standup deleted, its day loses its row and its document.
	var cursor map[string]any
	if err := json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &cursor); err != nil || cursor["day"] != day(today) {
		t.Fatalf("cursor = %s", f.store.connectors["conn-1"].ChangeCursor)
	}
	cursor["day"] = day(standup)
	f.store.connectors["conn-1"].ChangeCursor, _ = json.Marshal(cursor)
	edit(func() { delete(account.messages, ts(standup)) })
	if res, read := f.run(t, "sync-5"), account.read(); res.Mode != model.ConnectorSyncChanges || !slices.Equal(read, []string{day(standup)}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	if got := days(t); !slices.Equal(got, []string{"C1:" + day(kickoff), "C1:" + day(today)}) {
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
	if res, read := f.run(t, "sync-6"), account.read(); res.Mode != model.ConnectorSyncFull || !slices.Equal(read, []string{day(at(7, 0))}) {
		t.Fatalf("mode = %s, history read from %q", res.Mode, read)
	}
	if got := f.held(t, "conversations")["C1:"+day(kickoff)]; !strings.Contains(got, "Kickoff, edited") {
		t.Fatalf("kickoff = %q", got)
	}
}
