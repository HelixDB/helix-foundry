package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// event buffers an event row of kind on channel, on the day of payload's ts.
func (a *slackAppTest) event(kind model.SlackFeedRowKind, channel, payload string) {
	a.feed.append(kind, model.SlackFeedFromEvent, channel, slackAtOf(payload), payload)
}

// close buffers a complete row of channel's day, YYYY-MM-DD.
func (a *slackAppTest) close(channel, day, payload string) {
	a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, channel, slackDayOf(day), payload)
}

// start is the cursor a full read starts from.
func (a *slackAppTest) start(t *testing.T) json.RawMessage {
	t.Helper()
	s, err := a.app.Start(context.Background(), a.src, slackTables)
	if err != nil {
		t.Fatal(err)
	}
	return s.Cursor
}

// changes reads the changes after cursor into a new writer whose documents
// go to docs, and returns the writer and the next cursor.
func (a *slackAppTest) changes(t *testing.T, cursor json.RawMessage, docs DocumentWriter) (*slackChanges, slackAppCursor) {
	t.Helper()
	w := newSlackChanges(docs)
	next, err := a.app.Changes(context.Background(), a.src, slackTables, cursor, w)
	if err != nil {
		t.Fatal(err)
	}
	var c slackAppCursor
	if err := json.Unmarshal(next, &c); err != nil {
		t.Fatal(err)
	}
	return w, c
}

// newSlackAppFeed is an app test of the parity workspace whose feed covers
// #general and #design: on the 28th #general holds a message, and on the
// 29th #design one, both days closed.
func newSlackAppFeed(t *testing.T) *slackAppTest {
	t.Helper()
	a := newSlackAppTest(t, newSlackParityWorkspace().routes())
	a.feed.cover("C1", "C2")
	a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Hello","ts":%q}`, slackTS("2026-10-28T09:00:00Z")))
	a.event(model.SlackRowMessage, "C2", parityQ)
	a.close("C1", "2026-10-28", `{}`)
	a.close("C2", "2026-10-29", `{}`)
	return a
}

func TestSlackAppChanges(t *testing.T) {
	wantDocs := func(t *testing.T, docs *granolaDocs, want ...string) {
		t.Helper()
		if got := docs.putNames(); !slices.Equal(got, want) {
			t.Fatalf("puts %v, want %v", got, want)
		}
	}

	t.Run("Start folds what waits first, so the full read skips nothing", func(t *testing.T) {
		a := newSlackAppFeed(t)
		var c slackAppCursor
		if err := json.Unmarshal(a.start(t), &c); err != nil {
			t.Fatal(err)
		}
		if *c.Seq != a.feed.feed.LastSeq || a.feed.feed.FoldedSeq != a.feed.feed.LastSeq || c.HistoryDays != 3 || c.Window != "2026-10-27" ||
			c.Channels["C1"] != "general" || c.Channels["C2"] != "design" || !c.DirectoryAt.Equal(slackTestNow) {
			t.Fatalf("cursor %+v, feed %+v", c, a.feed.feed)
		}
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		sinks := a.read(t, slackTables, docs)
		if len(sinks["messages"].rows) != 2 || len(sinks["users"].rows) != 5 || len(sinks["channel_members"].rows) != 5 {
			t.Fatalf("read %d messages, %d users and %d members", len(sinks["messages"].rows), len(sinks["users"].rows), len(sinks["channel_members"].rows))
		}
		wantDocs(t, docs, "conversations C2:2026-10-29", "conversations C1:2026-10-28")
	})

	t.Run("rewrites every channel's rows of each day that changed, and offers the changed days' documents", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		late := slackTS("2026-10-29T16:00:00Z")
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Late","ts":%q}`, late))
		a.close("C1", "2026-10-29", `{}`)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		w, next := a.changes(t, cursor, docs)
		if got := w.cascades(); !slices.Equal(got, []string{"cascade messages day=2026-10-29", "cascade channel_days day=2026-10-29"}) {
			t.Fatalf("cascades %v", got)
		}
		if got := w.upserted("messages"); !slices.Equal(got, []string{"C1:" + late, "C2:" + slackTS("2026-10-29T14:00:00Z")}) {
			t.Fatalf("messages upserted %v", got)
		}
		if got := w.upserted("channel_days"); !slices.Equal(got, []string{"C1:2026-10-29", "C2:2026-10-29"}) {
			t.Fatalf("channel_days upserted %v", got)
		}
		wantDocs(t, docs, "conversations C1:2026-10-29")
		if *next.Seq != a.feed.feed.LastSeq || !slices.Equal(next.Offered, []string{"C1:2026-10-29"}) || len(next.Owed) != 0 || len(w.resets) != 0 {
			t.Fatalf("next %+v, resets %v", next, w.resets)
		}

		// A sync that failed left its cursor: the next read renders the same days again.
		again, _ := a.changes(t, cursor, &granolaDocs{want: map[string]bool{"conversations": true}})
		if !slices.Equal(again.log, w.log) {
			t.Fatalf("a read again of the same cursor wrote\n%v\nnot\n%v", again.log, w.log)
		}

		// The next read, once that sync committed, marks its offers documented and writes nothing.
		w, _ = a.changes(t, encodeJSON(next), &granolaDocs{want: map[string]bool{"conversations": true}})
		if len(w.log) != 0 || len(a.feed.documented) != 1 || a.feed.day("C1", "2026-10-29").DocumentedAt == nil {
			t.Fatalf("a quiet read wrote %v; documented %v", w.log, a.feed.documented)
		}
	})

	t.Run("a day gets its document once it is over and closed, and a lossy one says so", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Open","ts":%q}`, slackTS("2026-10-29T16:00:00Z")))
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Today","ts":%q}`, slackTS("2026-10-30T09:00:00Z")))
		a.close("C1", "2026-10-30", `{}`)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		w, next := a.changes(t, cursor, docs)
		wantDocs(t, docs)
		if got := w.upserted("channel_days"); !slices.Equal(got, []string{"C1:2026-10-29", "C1:2026-10-30", "C2:2026-10-29"}) {
			t.Fatalf("channel_days upserted %v", got)
		}
		a.close("C1", "2026-10-29", `{"lossy":true}`)
		docs = &granolaDocs{want: map[string]bool{"conversations": true}}
		a.changes(t, encodeJSON(next), docs)
		wantDocs(t, docs, "conversations C1:2026-10-29")
		lossy := docs.puts[0].doc
		if !strings.HasSuffix(string(lossy.Text), "\n"+slackLossyLine+"\n") {
			t.Fatalf("a lossy day's document:\n%s", lossy.Text)
		}
		// Closed whole later, the same messages are another version, without the line.
		a.close("C1", "2026-10-29", `{}`)
		docs = &granolaDocs{want: map[string]bool{"conversations": true}}
		a.changes(t, encodeJSON(next), docs)
		if complete := docs.puts[0].doc; complete.Version == lossy.Version || strings.Contains(string(complete.Text), slackLossyLine) {
			t.Fatalf("a complete day's document is %s:\n%s", complete.Version, complete.Text)
		}
	})

	t.Run("offers a closed day that keeps changing at most once each redocument_every", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		// The 28th's document was offered an hour ago, and the stream holds it.
		hourAgo := slackTestNow.Add(-time.Hour)
		key := model.SlackDayKey{ChannelID: "C1", Day: slackDayOf("2026-10-28")}
		d := a.feed.days[key]
		d.DocumentedAt = &hourAgo
		a.feed.days[key] = d
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Reply","ts":%q,"thread_ts":%q}`, slackTS("2026-10-29T08:00:00Z"),
			slackTS("2026-10-28T09:00:00Z")))
		held := &slackHeld{granolaDocs: &granolaDocs{want: map[string]bool{"conversations": true}}, held: map[string]string{"C1:2026-10-28": "2|old|x"}}
		_, next := a.changes(t, cursor, held)
		if len(held.puts) != 0 || !slices.Equal(next.Owed, []string{"C1:2026-10-28"}) {
			t.Fatalf("puts %v, owed %v", held.putNames(), next.Owed)
		}
		// A day later the owed day is offered, though nothing changed since.
		slackNow = func() time.Time { return slackTestNow.Add(24 * time.Hour) }
		held.granolaDocs = &granolaDocs{want: map[string]bool{"conversations": true}}
		w, next := a.changes(t, encodeJSON(next), held)
		wantDocs(t, held.granolaDocs, "conversations C1:2026-10-28")
		if len(next.Owed) != 0 || !slices.Contains(w.cascades(), "cascade messages day=2026-10-27") {
			t.Fatalf("owed %v; cascades %v", next.Owed, w.cascades())
		}
	})

	t.Run("a deadline owes the documents it couldn't fetch", func(t *testing.T) {
		deadline := time.Now().Add(800 * time.Millisecond) // documents stop 200ms before it
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		routes, slow := newSlackParityWorkspace().routes(), false
		joined := routes["users.conversations"]
		routes["users.conversations"] = func(r *http.Request) (int, string) {
			if slow {
				time.Sleep(time.Until(deadline.Add(-100 * time.Millisecond)))
			}
			return joined(r)
		}
		a := newSlackAppTest(t, routes)
		a.feed.cover("C1", "C2")
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Hello","ts":%q}`, slackTS("2026-10-28T09:00:00Z")))
		cursor := a.start(t)
		a.close("C1", "2026-10-28", `{}`)
		slow = true
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		raw, err := a.app.Changes(ctx, a.src, slackTables, cursor, newSlackChanges(docs))
		if err != nil {
			t.Fatal(err)
		}
		var next slackAppCursor
		if err := json.Unmarshal(raw, &next); err != nil || !slices.Equal(next.Owed, []string{"C1:2026-10-28"}) || len(docs.puts) != 0 {
			t.Fatalf("owed %v, puts %v", next.Owed, docs.putNames())
		}
		// The next read, with time to spare, offers it though nothing changed since.
		slow, docs = false, &granolaDocs{want: map[string]bool{"conversations": true}}
		a.changes(t, raw, docs)
		wantDocs(t, docs, "conversations C1:2026-10-28")
	})

	t.Run("resets the history tables past slackCascades, or with other history_days", func(t *testing.T) {
		saved := slackCascades
		slackCascades = 1
		t.Cleanup(func() { slackCascades = saved })
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"One","ts":%q}`, slackTS("2026-10-27T10:00:00Z")))
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"Two","ts":%q}`, slackTS("2026-10-29T10:00:00Z")))
		w, _ := a.changes(t, cursor, &granolaDocs{})
		if !slices.Equal(w.resets, []string{"messages", "channel_days"}) || len(w.log) != 0 {
			t.Fatalf("resets %v, log %v", w.resets, w.log)
		}
		var c slackAppCursor
		_ = json.Unmarshal(cursor, &c)
		c.HistoryDays = 7
		w, _ = a.changes(t, encodeJSON(c), &granolaDocs{})
		if !slices.Equal(w.resets, []string{"messages", "channel_days"}) {
			t.Fatalf("resets %v", w.resets)
		}
	})

	t.Run("drops the days that left the window", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		slackNow = func() time.Time { return slackTestNow.AddDate(0, 0, 2) }
		w, next := a.changes(t, cursor, &granolaDocs{})
		if got := w.cascades(); !slices.Equal(got, []string{"cascade messages day=2026-10-27", "cascade channel_days day=2026-10-27",
			"cascade messages day=2026-10-28", "cascade channel_days day=2026-10-28"}) || next.Window != "2026-10-29" {
			t.Fatalf("cascades %v, window %s", got, next.Window)
		}
		// Their documents are expired, leaving every load; a token's are removed.
		expired := a.app.Streams(a.src)[0].Expired
		if !expired("C1:2026-10-28") || expired("C1:2026-10-29") || expired("C1:soon") || a.poll.Streams(a.src)[0].Expired != nil {
			t.Fatal("the app's stream expires other days than those before its window, or a token's expires any")
		}
	})

	t.Run("covers a channel newly readable and purges one no longer, then lists the directory again", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		var c slackAppCursor
		_ = json.Unmarshal(cursor, &c)
		c.Channels["C9"] = "gone"
		a.feed.cover("C9")
		delete(a.feed.channels, "C2")
		w, next := a.changes(t, encodeJSON(c), &granolaDocs{})
		if !slices.Equal(a.feed.purges, []string{"C9"}) || len(a.feed.joins) != 1 || a.feed.joins[0].ChannelID != "C2" ||
			!a.feed.joins[0].CoveredFrom.Equal(slackTestNow) || !a.feed.joins[0].Oldest.Equal(slackDayOf("2026-10-27")) {
			t.Fatalf("purges %v, joins %+v", a.feed.purges, a.feed.joins)
		}
		if got := w.cascades(); !slices.Equal(got, []string{"cascade messages channel_id=C9", "cascade channel_days channel_id=C9"}) {
			t.Fatalf("cascades %v", got)
		}
		if !slices.Equal(w.resets, []string{"users", "channels", "channel_members"}) || !next.DirectoryAt.Equal(slackTestNow) {
			t.Fatalf("resets %v, directory at %s", w.resets, next.DirectoryAt)
		}
	})

	t.Run("lists the directory again once directory_every passed", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		slackNow = func() time.Time { return slackTestNow.Add(time.Hour) }
		w, next := a.changes(t, cursor, &granolaDocs{})
		if !slices.Equal(w.resets, []string{"users", "channels", "channel_members"}) || !next.DirectoryAt.Equal(slackTestNow.Add(time.Hour)) {
			t.Fatalf("resets %v, directory at %s", w.resets, next.DirectoryAt)
		}
		// Only the directory read: nothing of history.
		w = newSlackChanges(&granolaDocs{})
		if _, err := a.app.Changes(context.Background(), a.src, []Table{{Name: "users"}}, cursor, w); err != nil || len(w.log) != 0 ||
			!slices.Equal(w.resets, []string{"users"}) {
			t.Fatalf("a directory read: %v, %v, %v", err, w.log, w.resets)
		}
	})

	t.Run("offers every day of a renamed channel again", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		var c slackAppCursor
		_ = json.Unmarshal(cursor, &c)
		c.Channels["C1"] = "old-general"
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		w, _ := a.changes(t, encodeJSON(c), docs)
		wantDocs(t, docs, "conversations C1:2026-10-28")
		if len(w.cascades()) != 0 {
			t.Fatalf("a rename rewrote rows: %v", w.cascades())
		}
	})

	t.Run("refuses a cursor it can't read, and a source it can't open", func(t *testing.T) {
		a := newSlackAppFeed(t)
		for _, raw := range []string{`{}`, `[]`, `{"seq":1,"window":"soon"}`, `{"day":"2026-10-28","channels":[]}`} {
			if _, err := a.app.Changes(context.Background(), a.src, slackTables, json.RawMessage(raw), newSlackChanges(&granolaDocs{})); !errors.Is(err, ErrCursorExpired) {
				t.Errorf("Changes(%s) error = %v", raw, err)
			}
		}
		cursor := a.start(t)
		a.feed.failGet = errors.New("store down")
		if _, err := a.app.Changes(context.Background(), a.src, slackTables, cursor, newSlackChanges(&granolaDocs{})); err == nil {
			t.Fatal("Changes with a store that fails succeeded")
		}
		if _, err := a.app.Start(context.Background(), a.src, slackTables); err == nil {
			t.Fatal("Start with a store that fails succeeded")
		}
		if _, err := a.app.SyncPass(context.Background(), a.src, slackTables, map[string]RowWriter{}, &granolaDocs{}); err == nil {
			t.Fatal("SyncPass with a store that fails succeeded")
		}
		a.feed.failGet = nil
		unopened := a.src
		unopened.SlackApp = nil
		if _, err := a.app.Start(context.Background(), unopened, slackTables); !errors.Is(err, errSlackAppUnopened) {
			t.Fatalf("Start without the install: %v", err)
		}
		if _, err := a.app.Discover(context.Background(), unopened); !errors.Is(err, errSlackAppUnopened) {
			t.Fatalf("Discover without the install: %v", err)
		}
		unwired := &slackAppKind{poll: a.poll}
		if _, err := unwired.Changes(context.Background(), a.src, slackTables, cursor, newSlackChanges(&granolaDocs{})); !errors.Is(err, errSlackAppUnwired) {
			t.Fatalf("Changes of an unwired catalog: %v", err)
		}
		if _, err := unwired.Backlog(context.Background(), a.src, cursor); !errors.Is(err, errSlackAppUnwired) {
			t.Fatalf("Backlog of an unwired catalog: %v", err)
		}
	})
}

func TestSlackAppBacklog(t *testing.T) {
	ctx := context.Background()
	a := newSlackAppFeed(t)
	backlog := func(cursor json.RawMessage) Backlog {
		t.Helper()
		b, err := a.app.Backlog(ctx, a.src, cursor)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cursor := a.start(t)
	if b := backlog(cursor); b != BacklogNone {
		t.Fatalf("a folded feed's backlog is %v", b)
	}
	a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"More","ts":%q}`, slackTS("2026-10-28T10:00:00Z")))
	if b := backlog(cursor); b != BacklogSome {
		t.Fatalf("a row waiting is %v", b)
	}
	a.app.Budget.Rows = 0
	if b := backlog(cursor); b != BacklogMore {
		t.Fatalf("more rows than a fold takes is %v", b)
	}
	a.app.Budget.Rows = 1000
	// The backfill folded it: the mirror changed beyond the cursor.
	a.fold(t)
	if b := backlog(cursor); b != BacklogSome {
		t.Fatalf("a day folded beyond the cursor is %v", b)
	}
	if _, err := a.app.Backlog(ctx, a.src, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Backlog of a cursor without a seq succeeded")
	}
	a.feed.failGet = errors.New("store down")
	if _, err := a.app.Backlog(ctx, a.src, cursor); err == nil {
		t.Fatal("Backlog of a store that fails succeeded")
	}
}

func TestSlackAppKind(t *testing.T) {
	ctx := context.Background()
	a := newSlackAppTest(t, newSlackParityWorkspace().routes())
	t.Run("is a pushed Slack kind with no credential to hint at", func(t *testing.T) {
		if a.app.Name() != "slack" || a.app.DisplayName() != "Slack" || a.app.Hint("x") != "" {
			t.Fatalf("%q %q %q", a.app.Name(), a.app.DisplayName(), a.app.Hint("x"))
		}
		for _, table := range slackTables {
			if a.app.Capture(a.src, table.Name) != CaptureCursor {
				t.Errorf("%s isn't followed", table.Name)
			}
		}
		if a.app.Capture(a.src, "events") != CaptureFull {
			t.Error("an unknown table is followed")
		}
		streams := a.app.Streams(a.src)
		if len(streams) != 1 || streams[0].Name != "conversations" || streams[0].Problem() != "" ||
			!strings.Contains(streams[0].Description, "more than 2 days ago list no replies") {
			t.Fatalf("streams %+v", streams)
		}
		unset := &slackAppKind{poll: a.poll}
		if d := unset.Streams(a.src)[0].Description; !strings.Contains(d, "older days list no replies") || unset.Streams(a.src)[0].Problem() != "" {
			t.Fatalf("description %q", d)
		}
	})

	t.Run("discovers through the install without reading history", func(t *testing.T) {
		d, err := a.app.Discover(ctx, a.src)
		if err != nil || len(d.Tables) != 5 || len(d.Unavailable) != 0 || len(a.calls.of("conversations.history")) != 0 {
			t.Fatalf("Discover = %+v, %v; history calls %v", d, err, a.calls.of("conversations.history"))
		}
		noHistory := newSlackAppTest(t, newSlackParityWorkspace().routes())
		noHistory.src.SlackApp.Caller = slackRoutesCaller{routes: newSlackParityWorkspace().routes(), scopes: "channels:read,groups:read,users:read",
			calls: &slackCalls{}}
		d, err = noHistory.app.Discover(ctx, noHistory.src)
		if err != nil || !slices.ContainsFunc(d.Unavailable, func(u Unavailable) bool {
			return u.Name == "messages" && strings.Contains(u.Reason, "channels:history")
		}) {
			t.Fatalf("Discover without history scopes = %+v, %v", d, err)
		}
	})

	t.Run("reads one table through its pass", func(t *testing.T) {
		a := newSlackAppFeed(t)
		a.fold(t)
		sink := &rowSink{}
		if _, err := a.app.Sync(ctx, a.src, Table{Name: "channel_days"}, sink); err != nil || len(sink.rows) != 2 {
			t.Fatalf("Sync = %v, %v", sink.rows, err)
		}
		if _, err := a.app.SyncPass(ctx, a.src, []Table{{Name: "reactions"}}, nil, &granolaDocs{}); err == nil {
			t.Fatal("SyncPass of an unknown table succeeded")
		}
	})

	t.Run("prepares settings as the Slack kind does", func(t *testing.T) {
		got, err := a.app.Prepare(Source{Settings: json.RawMessage(`{"credential":"helix_app","team_id":"T1"}`)})
		if err != nil || string(got) != `{"history_days":90,"credential":"helix_app","team_id":"T1"}` {
			t.Fatalf("Prepare = %s, %v", got, err)
		}
	})

	t.Run("retries a rate limit or an outage of the install's caller, and fails on one that gets no answer", func(t *testing.T) {
		saved := retryBackoff
		retryBackoff = func(int, string) time.Duration { return 0 }
		t.Cleanup(func() { retryBackoff = saved })
		answers := []int{http.StatusTooManyRequests, http.StatusBadGateway}
		routes := newSlackParityWorkspace().routes()
		auth := routes["auth.test"]
		routes["auth.test"] = func(r *http.Request) (int, string) {
			if len(answers) > 0 {
				status := answers[0]
				answers = answers[1:]
				return status, ""
			}
			return auth(r)
		}
		app := &SlackApp{Caller: slackRoutesCaller{routes: routes, scopes: slackAllScopes, calls: &slackCalls{}}}
		if got, err := a.poll.appClient(app).auth(ctx); err != nil || got.team != "T1" {
			t.Fatalf("auth = %+v, %v", got, err)
		}
		app.Caller = failingCaller{}
		if _, err := a.poll.appClient(app).auth(ctx); err == nil {
			t.Fatal("a call with no answer succeeded")
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		app.Caller = slackRoutesCaller{routes: map[string]func(*http.Request) (int, string){"auth.test": func(*http.Request) (int, string) {
			return http.StatusTooManyRequests, ""
		}}, calls: &slackCalls{}}
		if _, err := a.poll.appClient(app).auth(cancelled); err == nil {
			t.Fatal("a cancelled call succeeded")
		}
	})
}

// failingCaller gets no answer.
type failingCaller struct{}

func (failingCaller) Call(context.Context, string, url.Values) (int, http.Header, []byte, error) {
	return 0, nil, nil, errors.New("connection refused")
}

func TestSlackKindFor(t *testing.T) {
	catalog := NewCatalog(Endpoints{}, Guard{}, S3Options{}, EventsOptions{})
	for _, c := range []struct {
		settings string
		app      bool
		fails    bool
	}{
		{``, false, false},
		{`{"history_days":30}`, false, false},
		{`{"credential":"helix_app"}`, true, false},
		{`{"credential":"helix_app","staged":true}`, false, false},
		{`{"credential":"pipes"}`, false, true},
		{`[]`, false, true},
	} {
		k, err := catalog.KindFor("slack", json.RawMessage(c.settings))
		_, isApp := k.(*slackAppKind)
		if (err != nil) != c.fails || err == nil && isApp != c.app {
			t.Errorf("KindFor(%s) = %T, %v", c.settings, k, err)
		}
	}
	if k, err := catalog.KindFor("stripe", nil); err != nil || k.Name() != "stripe" {
		t.Fatalf("KindFor(stripe) = %v, %v", k, err)
	}
	if _, err := catalog.KindFor("fax", nil); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("KindFor(fax) error = %v", err)
	}
	app, _ := catalog.KindFor("slack", json.RawMessage(`{"credential":"helix_app"}`))
	if _, ok := app.(Pushed); !ok {
		t.Fatal("the app's kind isn't pushed")
	}
	if _, err := app.(*slackAppKind).Start(context.Background(), Source{Settings: slackAppSettings, SlackApp: &SlackApp{}}, nil); !errors.Is(err, errSlackAppUnwired) {
		t.Fatalf("Start of an unwired catalog: %v", err)
	}
}

func TestSlackPrepareCredentials(t *testing.T) {
	kind := kindOf(t, catalogFor("http://unused"), "slack")
	for _, c := range []struct {
		secret, settings, want, fails string
	}{
		{"", `{"credential":"helix_app"}`, `{"history_days":90,"credential":"helix_app"}`, ""},
		{"", `{"credential":"helix_app","history_days":90}`, `{"history_days":90,"credential":"helix_app"}`, ""},
		{slackTestToken, `{"credential":"helix_app","team_id":"T1","staged":true,"history_days":30}`,
			`{"history_days":30,"credential":"helix_app","team_id":"T1","staged":true}`, ""},
		{"", `{"credential":"helix_app","history_days":91}`, "", "keep 1 to 90 days of history"},
		{slackTestToken, `{"credential":"helix_app"}`, "", "a connection through Helix's Slack app takes no token"},
		{"", `{"credential":"helix_app","staged":true}`, "", "enter a Slack bot token (xoxb-…)"},
		{"", `{"credential":"helix_app","team_id":"not a team"}`, "", `Slack team "not a team" is not a Slack id`},
		{slackTestToken, `{"team_id":"T1"}`, "", "a connection with a bot token reads no Slack team through Helix's Slack app"},
		{slackTestToken, `{"staged":true}`, "", "a connection with a bot token reads no Slack team through Helix's Slack app"},
		{slackTestToken, `{"credential":"pipes"}`, "", "a Slack connection reads with a bot token or through Helix's Slack app"},
	} {
		got, err := kind.Prepare(Source{Secret: c.secret, Settings: json.RawMessage(c.settings)})
		var invalidErr *InvalidError
		switch {
		case c.fails == "" && (err != nil || string(got) != c.want):
			t.Errorf("Prepare(%s) = %s, %v; want %s", c.settings, got, err, c.want)
		case c.fails != "" && (!errors.As(err, &invalidErr) || invalidErr.Message != c.fails):
			t.Errorf("Prepare(%s) error = %v, want %q", c.settings, err, c.fails)
		}
	}
}

func TestSlackSharedChannels(t *testing.T) {
	ws := newSlackWorkspace()
	ws.joined = append([]string{`{"id":"C5","name":"partners","is_member":true,"is_ext_shared":true}`,
		`{"id":"C6","name":"soon-partners","is_member":true,"is_private":true,"is_pending_ext_shared":true}`}, ws.joined...)
	ws.members["C5"], ws.members["C6"] = []string{`"U1"`}, []string{`"U1"`}
	kind, calls := slackAt(t, ws.routes())
	sinks, err := slackRead(t, kind, slackTables, &granolaDocs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"conversations.history", "conversations.members"} {
		for _, query := range calls.of(method) {
			if strings.Contains(query, "C5") || strings.Contains(query, "C6") {
				t.Fatalf("%s read a shared channel: %s", method, query)
			}
		}
	}
	for _, row := range sinks["channel_members"].rows {
		if row["channel_id"] == "C5" || row["channel_id"] == "C6" {
			t.Fatalf("a shared channel's members were kept: %v", row)
		}
	}
	if _, err := kind.Discover(context.Background(), slackSource); err != nil {
		t.Fatal(err)
	}
}

func TestSlackDayKeys(t *testing.T) {
	got := slackDayKeys([]string{"C1:2026-10-28", "C1", "bad id:2026-10-28", "C2:tomorrow", "C2:2026-10-29"})
	want := []model.SlackDayKey{{ChannelID: "C1", Day: slackDayOf("2026-10-28")}, {ChannelID: "C2", Day: slackDayOf("2026-10-29")}}
	if !slices.Equal(got, want) {
		t.Fatalf("slackDayKeys = %v", got)
	}
}

// putFailing is a bucket that refuses every write.
type putFailing struct{ *eventObjects }

func (putFailing) Put(context.Context, string, io.ReadSeeker) error { return errors.New("bucket down") }

func TestSlackAppFailures(t *testing.T) {
	ctx := context.Background()
	down := errors.New("store down")
	// Each store method failing fails the read that needs it: Changes from
	// a cursor that offered a day and holds a channel gone, after a row
	// that joins and leaves channels.
	for _, method := range []string{"FoundrySlackFeedAfter", "FoundrySlackDaysAfter", "MarkFoundrySlackDaysDocumented", "ListFoundrySlackFeedChannels",
		"JoinFoundrySlackChannels", "PurgeFoundrySlackChannel"} {
		t.Run(method, func(t *testing.T) {
			a := newSlackAppFeed(t)
			cursor := a.start(t)
			var c slackAppCursor
			_ = json.Unmarshal(cursor, &c)
			c.Offered = []string{"C1:2026-10-28"}
			a.feed.cover("C9")
			delete(a.feed.channels, "C2")
			a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"More","ts":%q}`, slackTS("2026-10-28T10:00:00Z")))
			a.feed.fail[method] = down
			if _, err := a.app.Changes(ctx, a.src, slackTables, encodeJSON(c), newSlackChanges(&granolaDocs{})); !errors.Is(err, down) {
				t.Fatalf("Changes error = %v", err)
			}
			if _, err := a.app.Start(ctx, a.src, slackTables); method != "MarkFoundrySlackDaysDocumented" && !errors.Is(err, down) {
				t.Fatalf("Start error = %v", err)
			}
		})
	}
	t.Run("a fold's row that fails to join or purge", func(t *testing.T) {
		for _, c := range []struct{ method, kind, payload string }{
			{"JoinFoundrySlackChannels", "joined", `{"joined_at":"2026-10-28T08:00:00Z"}`},
			{"PurgeFoundrySlackChannel", "left", `{}`},
		} {
			a := newSlackAppTest(t, newSlackParityWorkspace().routes())
			a.feed.append(model.SlackFeedRowKind(c.kind), model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), c.payload)
			a.feed.fail[c.method] = down
			if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); !errors.Is(err, down) {
				t.Fatalf("%s: Fold error = %v", c.method, err)
			}
		}
	})
	t.Run("a bucket that can't be read or written", func(t *testing.T) {
		a := newSlackAppFeed(t)
		a.app.Objects = putFailing{a.objects}
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); err == nil || !strings.Contains(err.Error(), "bucket down") {
			t.Fatalf("Fold error = %v", err)
		}
		a.app.Objects = a.objects
		cursor := a.start(t)
		day := a.feed.day("C1", "2026-10-28")
		a.objects.fail[day.ObjectKey] = errors.New("bucket down")
		if _, err := a.app.SyncPass(ctx, a.src, slackTables, map[string]RowWriter{"users": &rowSink{}, "channels": &rowSink{},
			"channel_members": &rowSink{}, "messages": &rowSink{}, "channel_days": &rowSink{}}, &granolaDocs{}); err == nil {
			t.Fatal("SyncPass of a day it can't read succeeded")
		}
		a.event(model.SlackRowMessage, "C2", fmt.Sprintf(`{"user":"U2","text":"More","ts":%q}`, slackTS("2026-10-28T10:00:00Z")))
		a.fold(t)
		if _, err := a.app.Changes(ctx, a.src, slackTables, cursor, newSlackChanges(&granolaDocs{})); err == nil {
			t.Fatal("Changes of a day it can't read succeeded")
		}
		// An object that isn't a day's.
		a.objects.objects[day.ObjectKey] = gzipped(t, []byte("{\"m\":{\"ts\":\"never\"}}\n"))
		delete(a.objects.fail, day.ObjectKey)
		if _, err := a.app.readSlackDay(ctx, day); err == nil {
			t.Fatal("a day of entries that aren't messages was read")
		}
	})
	t.Run("a writer that fails", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"More","ts":%q}`, slackTS("2026-10-28T10:00:00Z")))
		a.close("C1", "2026-10-28", `{}`)
		for _, w := range []ChangeWriter{&slackFailingChanges{slackChanges: newSlackChanges(&granolaDocs{}), on: "cascade"},
			&slackFailingChanges{slackChanges: newSlackChanges(&granolaDocs{}), on: "upsert"},
			newSlackChanges(&granolaDocs{want: map[string]bool{"conversations": true}, fail: errors.New("put failed")})} {
			if _, err := a.app.Changes(ctx, a.src, slackTables, cursor, w); err == nil {
				t.Fatalf("Changes into %T succeeded", w)
			}
		}
		a.feed.cover("C9")
		var c slackAppCursor
		_ = json.Unmarshal(cursor, &c)
		c.Channels["C9"] = "gone"
		if _, err := a.app.Changes(ctx, a.src, slackTables, encodeJSON(c), &slackFailingChanges{slackChanges: newSlackChanges(&granolaDocs{}), on: "cascade"}); err == nil {
			t.Fatal("Changes whose channel cascade fails succeeded")
		}
	})
	t.Run("a backoff the read's context ends", func(t *testing.T) {
		saved := retryBackoff
		retryBackoff = func(int, string) time.Duration { return time.Hour }
		t.Cleanup(func() { retryBackoff = saved })
		short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		app := &SlackApp{Caller: slackRoutesCaller{routes: map[string]func(*http.Request) (int, string){"auth.test": func(*http.Request) (int, string) {
			return http.StatusTooManyRequests, ""
		}}, calls: &slackCalls{}}}
		if _, err := (&slackKind{}).appClient(app).auth(short); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("auth = %v", err)
		}
	})
}

// slackFailingChanges is a slackChanges that fails its first cascade or upsert.
type slackFailingChanges struct {
	*slackChanges
	on string
}

func (c *slackFailingChanges) DeleteWhere(table, column string, value any) error {
	if c.on == "cascade" {
		return errors.New("cascade failed")
	}
	return c.slackChanges.DeleteWhere(table, column, value)
}

func (c *slackFailingChanges) UpsertJSON(table string, raw []byte) error {
	if c.on == "upsert" {
		return errors.New("upsert failed")
	}
	return c.slackChanges.UpsertJSON(table, raw)
}

func (c *slackFailingChanges) Upsert(table string, row map[string]any) error {
	b, _ := json.Marshal(row)
	return c.UpsertJSON(table, b)
}

func TestSlackMirrorReads(t *testing.T) {
	ctx := context.Background()
	t.Run("reads a dropped day again, and cuts a thread's replies at the day's bytes", func(t *testing.T) {
		savedBudget, savedBytes := slackTextBudget, slackDayBytes
		slackTextBudget, slackDayBytes = 1, 600
		t.Cleanup(func() { slackTextBudget, slackDayBytes = savedBudget, savedBytes })
		a := newSlackAppTest(t, newSlackParityWorkspace().routes())
		a.feed.cover("C1")
		root := slackTS("2026-10-28T09:00:00Z")
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Root","ts":%q}`, root))
		for i := range 5 {
			a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":%q,"ts":%q,"thread_ts":%q}`, strings.Repeat("r", 100),
				slackTS(fmt.Sprintf("2026-10-28T09:0%d:00Z", i+1)), root))
		}
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U1","text":"Next day","ts":%q}`, slackTS("2026-10-29T09:00:00Z")))
		a.close("C1", "2026-10-28", `{}`)
		a.close("C1", "2026-10-29", `{}`)
		a.fold(t)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		a.read(t, slackFollowedTables, docs)
		wantDocs := []string{"conversations C1:2026-10-29", "conversations C1:2026-10-28"}
		if got := docs.putNames(); !slices.Equal(got, wantDocs) {
			t.Fatalf("puts %v", got)
		}
		if text := string(docs.puts[1].doc.Text); strings.Count(text, strings.Repeat("r", 100)) != 2 {
			t.Fatalf("the 28th's document:\n%s", text)
		}
	})
	t.Run("has no replies of a thread whose day it doesn't hold, and stops where each says", func(t *testing.T) {
		m := &slackMirror{days: map[string][]model.SlackFeedDay{}}
		replies, err := m.replies(ctx, "C1", slackTS("2026-10-28T09:00:00Z"), 100)
		if err != nil || replies != nil {
			t.Fatalf("replies = %v, %v", replies, err)
		}
		a := newSlackAppFeed(t)
		a.fold(t)
		mirror := &slackMirror{o: a.app.SlackFeedOptions, days: map[string][]model.SlackFeedDay{"C1": {a.feed.day("C1", "2026-10-28")}}}
		stop := errors.New("stop")
		if err := mirror.history(ctx, "C1", slackDayOf("2026-10-27"), time.Time{}, func(slackMessage) error { return stop }); !errors.Is(err, stop) {
			t.Fatalf("history = %v", err)
		}
		if err := mirror.history(ctx, "C1", slackDayOf("2026-10-28").Add(10*time.Hour), time.Time{}, func(m slackMessage) error {
			return fmt.Errorf("listed %s, before oldest", m.TS)
		}); err != nil {
			t.Fatal(err)
		}
		a.objects.fail[a.feed.day("C1", "2026-10-28").ObjectKey] = errors.New("bucket down")
		mirror.last.day = nil // read again, not the day held
		if _, err := mirror.replies(ctx, "C1", slackTS("2026-10-28T09:00:00Z"), 100); err == nil {
			t.Fatal("replies of a day it can't read succeeded")
		}
	})
	t.Run("leaves a no-op row's day as it is, and a deleted message before the window alone", func(t *testing.T) {
		a := newSlackAppFeed(t)
		a.fold(t)
		day := a.feed.day("C1", "2026-10-28")
		a.event(model.SlackRowReplied, "C1", fmt.Sprintf(`{"ts":%q,"reply_count":1}`, slackTS("2026-10-28T23:00:00Z")))
		a.event(model.SlackRowReplied, "C1", fmt.Sprintf(`{"ts":%q,"reply_count":1}`, slackTS("2026-10-20T23:00:00Z")))
		a.event(model.SlackRowDeleted, "C1", fmt.Sprintf(`{"deleted_ts":%q}`, slackTS("2026-10-20T23:00:00Z")))
		a.fold(t)
		if again := a.feed.day("C1", "2026-10-28"); again.ObjectKey != day.ObjectKey || len(a.feed.days) != 2 {
			t.Fatalf("a no-op fold changed the index: %+v", a.feed.days)
		}
	})
	t.Run("reads only the history tables chosen", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		a.event(model.SlackRowMessage, "C1", fmt.Sprintf(`{"user":"U2","text":"More","ts":%q}`, slackTS("2026-10-28T10:00:00Z")))
		w := newSlackChanges(&granolaDocs{})
		if _, err := a.app.Changes(ctx, a.src, []Table{{Name: "channel_days"}}, cursor, w); err != nil {
			t.Fatal(err)
		}
		if got := w.cascades(); !slices.Equal(got, []string{"cascade channel_days day=2026-10-28"}) || len(w.upserted("messages")) != 0 {
			t.Fatalf("cascades %v, messages %v", got, w.upserted("messages"))
		}
	})
}

func TestSlackFoldFailures(t *testing.T) {
	ctx := context.Background()
	down := errors.New("bucket down")
	at := slackTS("2026-10-28T09:00:00Z")
	for _, row := range []struct {
		kind    model.SlackFeedRowKind
		payload string
	}{
		{model.SlackRowComplete, `{}`},
		{model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q}`, at)},
		{model.SlackRowReplied, fmt.Sprintf(`{"ts":%q,"reply_count":1}`, at)},
		{model.SlackRowSpan, string(encodeJSON(SlackSpan{From: at, To: at}))},
	} {
		t.Run(string(row.kind)+" of a day it can't read", func(t *testing.T) {
			a := newSlackAppFeed(t)
			a.fold(t)
			a.objects.fail[a.feed.day("C1", "2026-10-28").ObjectKey] = down
			a.feed.append(row.kind, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), row.payload)
			if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); !errors.Is(err, down) {
				t.Fatalf("Fold error = %v", err)
			}
		})
	}
	t.Run("a day cut short", func(t *testing.T) {
		a := newSlackAppFeed(t)
		a.fold(t)
		day := a.feed.day("C1", "2026-10-28")
		whole := a.objects.objects[day.ObjectKey]
		a.objects.objects[day.ObjectKey] = whole[:len(whole)-6]
		if _, err := a.app.readSlackDay(ctx, day); err == nil {
			t.Fatal("a day cut short was read")
		}
	})
	t.Run("a commit whose answer is lost, from a store that then fails", func(t *testing.T) {
		a := newSlackAppFeed(t)
		storeDown := errors.New("store down")
		a.feed.before = func() {
			a.feed.mu.Lock()
			a.feed.failCommit, a.feed.failGet = errors.New("connection lost"), storeDown
			a.feed.mu.Unlock()
		}
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); !errors.Is(err, storeDown) {
			t.Fatalf("Fold error = %v", err)
		}
	})
	t.Run("an install that fails, or an index that can't be read", func(t *testing.T) {
		a := newSlackAppFeed(t)
		cursor := a.start(t)
		a.feed.fail["FoundrySlackDaysAfter"] = down
		if _, err := a.app.Changes(ctx, a.src, slackTables, cursor, newSlackChanges(&granolaDocs{})); !errors.Is(err, down) {
			t.Fatalf("Changes error = %v", err)
		}
		if _, err := a.app.SyncPass(ctx, a.src, slackFollowedTables, map[string]RowWriter{"messages": &rowSink{}, "channel_days": &rowSink{}},
			&granolaDocs{}); !errors.Is(err, down) {
			t.Fatalf("SyncPass error = %v", err)
		}
		delete(a.feed.fail, "FoundrySlackDaysAfter")
		a.src.SlackApp.Caller = failingCaller{}
		if _, err := a.app.Start(ctx, a.src, slackTables); err == nil {
			t.Fatal("Start through an install that fails succeeded")
		}
		if _, err := a.app.Changes(ctx, a.src, slackTables, cursor, newSlackChanges(&granolaDocs{})); err == nil {
			t.Fatal("Changes through an install that fails succeeded")
		}
	})
}

// A Slack connector's settings decide its reader: a token's polls Slack,
// and one through Helix's Slack app is pushed from its feed, unless its
// switch from a token is staged.
func ExampleCatalog_KindFor() {
	catalog := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{})
	for _, settings := range []string{`{"history_days":30}`, `{"credential":"helix_app"}`, `{"credential":"helix_app","staged":true}`} {
		kind, _ := catalog.KindFor("slack", json.RawMessage(settings))
		_, pushed := kind.(Pushed)
		fmt.Println(kind.Name(), pushed)
	}
	// Output:
	// slack false
	// slack true
	// slack false
}
