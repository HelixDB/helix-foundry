package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// slackKeys are the followed tables' keys.
var slackKeys = map[string]string{"messages": "message_id", "channel_days": "channel_day_id"}

// slackChanges is a ChangeWriter that logs a change read's changes in order
// and keeps them to merge as the syncer does; its documents go to the
// DocumentWriter it holds. A change read deletes by cascade only.
type slackChanges struct {
	DocumentWriter
	log []string
	// rows are each table's last upsert by key; wheres its cascades, as column and value.
	rows   map[string]map[string]map[string]any
	wheres map[string][][2]string
	resets []string
}

func newSlackChanges(docs DocumentWriter) *slackChanges {
	return &slackChanges{DocumentWriter: docs, rows: map[string]map[string]map[string]any{}, wheres: map[string][][2]string{}}
}

func (c *slackChanges) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.UpsertJSON(table, b)
}

func (c *slackChanges) UpsertJSON(table string, raw []byte) error {
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	key := fmt.Sprint(row[slackKeys[table]])
	c.log = append(c.log, "upsert "+table+" "+key)
	if c.rows[table] == nil {
		c.rows[table] = map[string]map[string]any{}
	}
	c.rows[table][key] = row
	return nil
}

func (c *slackChanges) Delete(table string, key map[string]any) error {
	return fmt.Errorf("unexpected delete from %s of %v", table, key)
}

func (c *slackChanges) DeleteWhere(table, column string, value any) error {
	c.log = append(c.log, fmt.Sprintf("cascade %s %s=%v", table, column, value))
	c.wheres[table] = append(c.wheres[table], [2]string{column, fmt.Sprint(value)})
	return nil
}

func (c *slackChanges) Reset(table, _ string) { c.resets = append(c.resets, table) }

// cascades are the log's cascades.
func (c *slackChanges) cascades() []string {
	return slices.DeleteFunc(slices.Clone(c.log), func(entry string) bool { return !strings.HasPrefix(entry, "cascade ") })
}

// upserted are the keys of table's upserts, sorted.
func (c *slackChanges) upserted(table string) []string {
	return slices.Sorted(maps.Keys(c.rows[table]))
}

// merged is table's rows once the changes apply to prev as the syncer's
// merge applies them: a cascade removes the stored rows it matches that no
// upsert names, and the last upsert of a key wins.
func (c *slackChanges) merged(table string, prev []string) []string {
	out := map[string]map[string]any{}
	for _, line := range prev {
		var row map[string]any
		_ = json.Unmarshal([]byte(line), &row)
		key := fmt.Sprint(row[slackKeys[table]])
		if c.rows[table][key] != nil || slices.ContainsFunc(c.wheres[table], func(w [2]string) bool { return fmt.Sprint(row[w[0]]) == w[1] }) {
			continue
		}
		out[key] = row
	}
	maps.Copy(out, c.rows[table])
	return slackJSON(slices.Collect(maps.Values(out)))
}

// slackJSON is rows as JSON, sorted.
func slackJSON(rows []map[string]any) []string {
	var out []string
	for _, row := range rows {
		b, _ := json.Marshal(row)
		out = append(out, string(b))
	}
	slices.Sort(out)
	return out
}

// slackFollowedTables are the tables a change read follows.
var slackFollowedTables = []Table{{Name: "messages"}, {Name: "channel_days"}}

// slackCursorAt is a cursor of the test window's history_days, 3.
func slackCursorAt(day string, channels ...string) json.RawMessage {
	b, _ := json.Marshal(slackCursor{Day: day, Channels: append([]string{}, channels...), HistoryDays: 3})
	return b
}

func TestSlackChanges(t *testing.T) {
	ctx := context.Background()
	// full is a full read of the followed tables, as JSON rows by table, and the versions it offers its documents at.
	full := func(t *testing.T, w *slackWorkspace, scopes string) (map[string][]string, map[string]string) {
		t.Helper()
		kind, _ := slackScoped(t, scopes, w.routes())
		docs := &granolaDocs{}
		sinks, err := slackRead(t, kind, slackTables, docs)
		if err != nil {
			t.Fatal(err)
		}
		versions := map[string]string{}
		for _, offer := range docs.offers {
			fields := strings.Fields(offer)
			versions[fields[1]] = fields[2]
		}
		return map[string][]string{"messages": slackJSON(sinks["messages"].rows), "channel_days": slackJSON(sinks["channel_days"].rows)}, versions
	}
	// changes is a change read of w from cursor, its next cursor and its calls.
	changes := func(t *testing.T, w *slackWorkspace, scopes string, cursor json.RawMessage, docs DocumentWriter) (*slackChanges, slackCursor, *slackCalls) {
		t.Helper()
		kind, calls := slackScoped(t, scopes, w.routes())
		c := newSlackChanges(docs)
		next, err := kind.Changes(ctx, slackSource, slackFollowedTables, cursor, c)
		if err != nil {
			t.Fatal(err)
		}
		var decoded slackCursor
		if err := json.Unmarshal(next, &decoded); err != nil {
			t.Fatalf("next cursor %s: %v", next, err)
		}
		return c, decoded, calls
	}
	// since is a history query from midnight UTC of day.
	since := func(channel, day string) string {
		return "channel=" + channel + "&inclusive=true&limit=999&oldest=" + slackTS(day+"T00:00:00Z")
	}
	// assertMerged checks the changes leave each table as a full read of w writes it.
	assertMerged := func(t *testing.T, c *slackChanges, prev, want map[string][]string) {
		t.Helper()
		for table, rows := range want {
			if got := c.merged(table, prev[table]); !slices.Equal(got, rows) {
				t.Errorf("%s merged =\n%s\nwant\n%s", table, strings.Join(got, "\n"), strings.Join(rows, "\n"))
			}
		}
	}
	today := "2026-10-30"

	t.Run("follows messages and channel days only", func(t *testing.T) {
		kind, _ := slackAt(t, newSlackWorkspace().routes())
		for table, capture := range map[string]Capture{
			"users": CaptureFull, "channels": CaptureFull, "channel_members": CaptureFull, "messages": CaptureCursor, "channel_days": CaptureCursor,
		} {
			if got := kind.Capture(slackSource, table); got != capture {
				t.Errorf("Capture(%s) = %s, want %s", table, got, capture)
			}
		}
	})
	t.Run("starts from today with the channels whose history it reads", func(t *testing.T) {
		for scopes, channels := range map[string][]string{
			slackAllScopes: {"C1", "C2"},
			strings.ReplaceAll(slackAllScopes, ",groups:history", ""): {"C1"},
		} {
			kind, calls := slackScoped(t, scopes, newSlackWorkspace().routes())
			start, err := kind.Start(ctx, slackSource, slackFollowedTables)
			if err != nil {
				t.Fatal(err)
			}
			if want := slackCursorAt(today, channels...); string(start.Cursor) != string(want) || start.Source != nil || start.Close != nil {
				t.Fatalf("Start = %s, want %s", start.Cursor, want)
			}
			if len(calls.log) != 2 || len(calls.of("auth.test")) != 1 || len(calls.of("users.conversations")) != 1 {
				t.Fatalf("calls = %q", calls.log)
			}
		}
	})
	// The hourly sync of a quiet workspace: one history call a channel.
	t.Run("reads history again only from the cursor's day", func(t *testing.T) {
		w := newSlackWorkspace()
		c, next, calls := changes(t, w, slackAllScopes, slackCursorAt(today, "C1", "C2"), &granolaDocs{})
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{since("C1", today), since("C2", today)}) || len(calls.log) != 4 {
			t.Fatalf("calls = %q", calls.log)
		}
		if want := []string{"cascade messages day=" + today, "cascade channel_days day=" + today}; !slices.Equal(c.log, want) || len(c.resets) != 0 {
			t.Fatalf("changes = %q, resets = %v", c.log, c.resets)
		}
		if fmt.Sprint(next) != fmt.Sprint(slackCursor{Day: today, Channels: []string{"C1", "C2"}, HistoryDays: 3}) {
			t.Fatalf("next cursor = %+v", next)
		}
		if docs := c.DocumentWriter.(*granolaDocs); len(docs.offers) != 0 {
			t.Fatalf("offers = %q", docs.offers)
		}
	})
	t.Run("writes today's messages and row, and no document until the day is over", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, _ := full(t, w, slackAllScopes)
		w.history["C1"] = append([]string{fmt.Sprintf(`{"user":"U1","text":"Stand-up at 10","ts":%q}`, slackTS("2026-10-30T09:00:00Z"))}, w.history["C1"]...)
		// A clock ahead of the read's lists a message of tomorrow: a day not over either.
		w.history["C2"] = append([]string{fmt.Sprintf(`{"user":"U2","text":"Early","ts":%q}`, slackTS("2026-10-31T00:30:00Z"))}, w.history["C2"]...)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		c, _, calls := changes(t, w, slackAllScopes, slackCursorAt(today, "C1", "C2"), docs)
		if got := c.upserted("messages"); !slices.Equal(got, []string{"C1:" + slackTS("2026-10-30T09:00:00Z"), "C2:" + slackTS("2026-10-31T00:30:00Z")}) {
			t.Fatalf("messages upserted = %q", got)
		}
		row := c.rows["channel_days"]["C1:"+today]
		if !slices.Equal(c.upserted("channel_days"), []string{"C1:" + today, "C2:2026-10-31"}) || row["message_count"] != 1.0 || row["first_message_at"] != "2026-10-30T09:00:00Z" {
			t.Fatalf("channel days = %v", c.rows["channel_days"])
		}
		if message := c.rows["messages"]["C1:"+slackTS("2026-10-30T09:00:00Z")]; message["day"] != today || message["channel_day_id"] != "C1:"+today {
			t.Fatalf("message = %v", message)
		}
		if len(docs.offers) != 0 || len(calls.of("users.list"))+len(calls.of("conversations.list"))+len(calls.of("conversations.replies")) != 0 {
			t.Fatalf("offers = %q, calls = %q", docs.offers, calls.log)
		}
		want, _ := full(t, w, slackAllScopes)
		assertMerged(t, c, prev, want)
	})
	// The cursor a read before midnight left: yesterday is read once more, now over.
	t.Run("writes yesterday's document after midnight, as a full read writes it", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, versions := full(t, w, slackAllScopes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		c, next, calls := changes(t, w, slackAllScopes, slackCursorAt("2026-10-29", "C1", "C2"), docs)
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{since("C1", "2026-10-29"), since("C2", "2026-10-29")}) {
			t.Fatalf("history calls = %q", got)
		}
		if !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29"}) || docs.puts[0].doc.Version != versions["C2:2026-10-29"] {
			t.Fatalf("puts = %v, offers = %q, want at %s", docs.putNames(), docs.offers, versions["C2:2026-10-29"])
		}
		design29 := "# #design, 2026-10-29\n\nChannel: #design (C2)\n\n" +
			"**Slack** 11:00: hmm\n" +
			"\n**B7** 12:00: beep\n" +
			"\n**Zapier** 13:00:\n" +
			"\n**U4** 14:00: @U9 what about #C7, #general and #design? @Ada Lovelace @bob @channel @everyone @eng @subteam Oct 1 [a@b.co](mailto:a@b.co)\n"
		if p := docs.puts[0].doc; string(p.Text) != design29 || p.Title != "#design, 2026-10-29" || !p.At.Equal(time.Date(2026, 10, 29, 14, 0, 0, 0, time.UTC)) ||
			p.URL != "https://app.slack.com/client/T1/C2" {
			t.Fatalf("put = %+v", p)
		}
		// #C7, which the app is not in, sends it to conversations.list once for names, as a full read has them.
		if len(calls.of("conversations.list")) != 1 || len(calls.of("users.list")) != 1 || len(calls.of("conversations.replies")) != 0 {
			t.Fatalf("calls = %q", calls.log)
		}
		if next.Day != today {
			t.Fatalf("next cursor = %+v", next)
		}
		assertMerged(t, c, prev, prev)
	})
	// #design's 29th mentions #old, a channel the app isn't in, by id alone:
	// whichever tables are chosen, both reads list the channels to name it.
	t.Run("names a channel the app isn't in as a full read of the same tables does", func(t *testing.T) {
		for _, tables := range [][]Table{slackTables, slackTables[3:], slackTables[4:]} {
			w := newSlackWorkspace()
			w.history["C2"][0] = strings.Replace(w.history["C2"][0], "<#C7>", "<#C3>", 1)
			kind, _ := slackAt(t, w.routes())
			held := &granolaDocs{want: map[string]bool{"conversations": true}}
			if _, err := slackRead(t, kind, tables, held); err != nil {
				t.Fatal(err)
			}
			var followed []Table
			for _, table := range tables {
				if kind.Capture(slackSource, table.Name) != CaptureFull {
					followed = append(followed, table)
				}
			}
			docs := &granolaDocs{want: map[string]bool{"conversations": true}}
			kind, _ = slackAt(t, w.routes())
			if _, err := kind.Changes(ctx, slackSource, followed, slackCursorAt("2026-10-29", "C1", "C2"), newSlackChanges(docs)); err != nil {
				t.Fatal(err)
			}
			var full *granolaPut
			for i, p := range held.puts {
				if p.doc.ExternalID == "C2:2026-10-29" {
					full = &held.puts[i]
				}
			}
			if full == nil || len(docs.puts) != 1 || !strings.Contains(string(full.doc.Text), "#old, #general") ||
				string(docs.puts[0].doc.Text) != string(full.doc.Text) || docs.puts[0].doc.Version != full.doc.Version {
				t.Fatalf("%d tables: change read put %v, full read %v", len(tables), docs.putNames(), held.putNames())
			}
		}
	})
	// A worker's clock a little ahead of Slack's: at 00:01 the 29th may still
	// get a message, so it isn't over, and the cursor stays on it.
	t.Run("takes its day from ChangeOverlap ago, so a message just before midnight is read", func(t *testing.T) {
		saved := slackTestNow
		t.Cleanup(func() { slackTestNow = saved })
		slackTestNow = time.Date(2026, 10, 30, 0, 1, 0, 0, time.UTC)
		w := newSlackWorkspace()
		kind, _ := slackAt(t, w.routes())
		if start, err := kind.Start(ctx, slackSource, slackFollowedTables); err != nil || string(start.Cursor) != string(slackCursorAt("2026-10-29", "C1", "C2")) {
			t.Fatalf("Start = %s, %v", start.Cursor, err)
		}
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		_, next, _ := changes(t, w, slackAllScopes, slackCursorAt("2026-10-29", "C1", "C2"), docs)
		if len(docs.offers) != 0 || next.Day != "2026-10-29" {
			t.Fatalf("offers = %q, next cursor = %+v", docs.offers, next)
		}
		// Posted at 23:59:30 by Slack's clock, after that read's history.
		w.history["C1"] = append([]string{fmt.Sprintf(`{"user":"U1","text":"Night","ts":%q}`, slackTS("2026-10-29T23:59:30Z"))}, w.history["C1"]...)
		slackTestNow = time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC)
		docs = &granolaDocs{want: map[string]bool{"conversations": true}}
		_, next, _ = changes(t, w, slackAllScopes, slackCursorAt(next.Day, next.Channels...), docs)
		if !slices.Equal(docs.putNames(), []string{"conversations C1:2026-10-29", "conversations C2:2026-10-29"}) ||
			!strings.HasSuffix(string(docs.puts[0].doc.Text), "**Ada Lovelace** 23:59: Night\n") || next.Day != today {
			t.Fatalf("puts = %v, next cursor = %+v", docs.putNames(), next)
		}
	})
	t.Run("names people by id without users:read, as a full read does, without asking users.list", func(t *testing.T) {
		scopes := strings.ReplaceAll(slackAllScopes, ",users:read,", ",")
		w := newSlackWorkspace()
		kind, _ := slackScoped(t, scopes, w.routes())
		held := &granolaDocs{}
		if _, err := slackRead(t, kind, slackTables[1:], held); err != nil {
			t.Fatal(err)
		}
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		_, _, calls := changes(t, w, scopes, slackCursorAt("2026-10-29", "C1", "C2"), docs)
		if len(docs.puts) != 1 || !strings.HasSuffix(docs.puts[0].doc.Version, "|ids") || !slices.Contains(held.offers, docs.offers[0]) ||
			len(calls.of("users.list")) != 0 {
			t.Fatalf("puts = %v, offers = %q, full read's = %q, calls = %q", docs.putNames(), docs.offers, held.offers, calls.log)
		}
	})
	t.Run("drops a message deleted on a day it reads again, and the day's document changes", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, versions := full(t, w, slackAllScopes)
		w.history["C2"] = slices.DeleteFunc(w.history["C2"], func(m string) bool { return strings.Contains(m, `"beep"`) })
		docs := &slackHeld{granolaDocs: &granolaDocs{want: map[string]bool{"conversations": true}}, held: versions}
		c, _, _ := changes(t, w, slackAllScopes, slackCursorAt("2026-10-29", "C1", "C2"), docs)
		if !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29"}) || docs.puts[0].doc.Version == versions["C2:2026-10-29"] ||
			strings.Contains(string(docs.puts[0].doc.Text), "beep") {
			t.Fatalf("puts = %v", docs.puts)
		}
		want, _ := full(t, w, slackAllScopes)
		if len(want["messages"]) != len(prev["messages"])-1 {
			t.Fatalf("messages = %q", want["messages"])
		}
		assertMerged(t, c, prev, want)
	})
	t.Run("drops the row of a day left with no messages, so its document goes with it", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, versions := full(t, w, slackAllScopes)
		w.history["C2"] = nil
		docs := &slackHeld{granolaDocs: &granolaDocs{want: map[string]bool{"conversations": true}}, held: versions}
		c, _, _ := changes(t, w, slackAllScopes, slackCursorAt("2026-10-29", "C1", "C2"), docs)
		if len(docs.offers) != 0 || len(c.rows) != 0 {
			t.Fatalf("offers = %q, upserts = %v", docs.offers, c.rows)
		}
		want, _ := full(t, w, slackAllScopes)
		if slices.ContainsFunc(want["channel_days"], func(row string) bool { return strings.Contains(row, "C2:2026-10-29") }) {
			t.Fatalf("channel_days = %q", want["channel_days"])
		}
		assertMerged(t, c, prev, want)
	})
	t.Run("reads a channel the cursor doesn't list from the window's start, in place of its rows", func(t *testing.T) {
		w := newSlackWorkspace()
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		c, next, calls := changes(t, w, slackAllScopes, slackCursorAt(today, "C1"), docs)
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{since("C1", today), since("C2", "2026-10-27")}) {
			t.Fatalf("history calls = %q", got)
		}
		if got := c.cascades(); !slices.Equal(got, []string{"cascade messages day=" + today, "cascade channel_days day=" + today,
			"cascade messages channel_id=C2", "cascade channel_days channel_id=C2"}) {
			t.Fatalf("cascades = %q", got)
		}
		if !slices.Equal(docs.putNames(), []string{"conversations C2:2026-10-29"}) || !slices.Equal(next.Channels, []string{"C1", "C2"}) {
			t.Fatalf("puts = %v, next cursor = %+v", docs.putNames(), next)
		}
		// Joined since the full read: what that read stored of it is replaced.
		prev, _ := full(t, w, slackAllScopes)
		assertMerged(t, c, prev, prev)
	})
	t.Run("drops the rows of a channel it no longer reads", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, _ := full(t, w, slackAllScopes)
		// The app left #design, so it isn't listed; C9 was deleted.
		w.joined = w.joined[:1]
		c, next, _ := changes(t, w, slackAllScopes, slackCursorAt(today, "C1", "C2", "C9"), &granolaDocs{})
		if got := c.cascades(); !slices.Equal(got, []string{"cascade messages day=" + today, "cascade channel_days day=" + today,
			"cascade messages channel_id=C2", "cascade channel_days channel_id=C2", "cascade messages channel_id=C9", "cascade channel_days channel_id=C9"}) {
			t.Fatalf("cascades = %q", got)
		}
		if !slices.Equal(next.Channels, []string{"C1"}) {
			t.Fatalf("next cursor = %+v", next)
		}
		want, _ := full(t, w, slackAllScopes)
		assertMerged(t, c, prev, want)
	})
	t.Run("drops the rows of a channel whose history it may no longer read", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, _ := full(t, w, slackAllScopes)
		scopes := strings.ReplaceAll(slackAllScopes, ",groups:history", "")
		c, next, calls := changes(t, w, scopes, slackCursorAt(today, "C1", "C2"), &granolaDocs{})
		if !slices.Equal(next.Channels, []string{"C1"}) || !slices.Equal(calls.of("conversations.history"), []string{since("C1", today)}) {
			t.Fatalf("next cursor = %+v, calls = %q", next, calls.log)
		}
		want, _ := full(t, w, scopes)
		assertMerged(t, c, prev, want)
	})
	t.Run("drops a channel gone while its history is read, and leaves it out of the cursor", func(t *testing.T) {
		w := newSlackWorkspace()
		prev, _ := full(t, w, slackAllScopes)
		routes := w.routes()
		history := routes["conversations.history"]
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("channel") == "C2" {
				return http.StatusOK, slackError("channel_not_found")
			}
			return history(r)
		}
		kind, _ := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		c := newSlackChanges(docs)
		raw, err := kind.Changes(ctx, slackSource, slackFollowedTables, slackCursorAt("2026-10-29", "C1", "C2"), c)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(slackCursorAt(today, "C1")) || len(docs.offers) != 0 {
			t.Fatalf("next cursor = %s, offers = %q", raw, docs.offers)
		}
		w.joined = w.joined[:1]
		want, _ := full(t, w, slackAllScopes)
		assertMerged(t, c, prev, want)
	})
	t.Run("reads a cursor from before the window from the window's start", func(t *testing.T) {
		c, next, calls := changes(t, newSlackWorkspace(), slackAllScopes, slackCursorAt("2026-09-01", "C1", "C2"), &granolaDocs{})
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{since("C1", "2026-10-27"), since("C2", "2026-10-27")}) {
			t.Fatalf("history calls = %q", got)
		}
		var days []string
		for _, entry := range c.cascades() {
			if day, ok := strings.CutPrefix(entry, "cascade messages day="); ok {
				days = append(days, day)
			}
		}
		if !slices.Equal(days, []string{"2026-10-27", "2026-10-28", "2026-10-29", today}) || next.Day != today {
			t.Fatalf("days = %q, next cursor = %+v", days, next)
		}
	})
	t.Run("reads a cursor ahead of its clock from today", func(t *testing.T) {
		_, _, calls := changes(t, newSlackWorkspace(), slackAllScopes, slackCursorAt("2026-11-02", "C1", "C2"), &granolaDocs{})
		if got := calls.of("conversations.history"); !slices.Equal(got, []string{since("C1", today), since("C2", today)}) {
			t.Fatalf("history calls = %q", got)
		}
	})
	// A setting saved while a change sync was queued: the window moved, so both tables are read in full.
	t.Run("resets both tables when the days of history to keep changed", func(t *testing.T) {
		cursor, _ := json.Marshal(slackCursor{Day: today, Channels: []string{"C1"}, HistoryDays: 7})
		c, next, calls := changes(t, newSlackWorkspace(), slackAllScopes, cursor, &granolaDocs{})
		if !slices.Equal(c.resets, []string{"messages", "channel_days"}) || len(c.log) != 0 || len(calls.of("conversations.history")) != 0 {
			t.Fatalf("resets = %v, changes = %q, calls = %q", c.resets, c.log, calls.log)
		}
		if fmt.Sprint(next) != fmt.Sprint(slackCursor{Day: today, Channels: []string{"C1", "C2"}, HistoryDays: 3}) {
			t.Fatalf("next cursor = %+v", next)
		}
	})
	t.Run("asks for a full read for a cursor it can't read", func(t *testing.T) {
		for _, cursor := range []string{`not json`, `[]`, `{}`, `{"day":"yesterday"}`, `{"day":"2026-10-29T00:00:00Z","channels":["C1"]}`} {
			kind, calls := slackAt(t, newSlackWorkspace().routes())
			_, err := kind.Changes(ctx, slackSource, slackFollowedTables, json.RawMessage(cursor), newSlackChanges(&granolaDocs{}))
			if !errors.Is(err, ErrCursorExpired) || len(calls.log) != 0 {
				t.Errorf("Changes(%s) = %v after %q, want ErrCursorExpired", cursor, err, calls.log)
			}
		}
	})
	// Its next sync reads the day again, so the document isn't left for the reconcile.
	t.Run("keeps the cursor at a day whose document it wanted too near the deadline", func(t *testing.T) {
		deadline := time.Now().Add(800 * time.Millisecond) // documents stop 200ms before it
		ctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		routes := newSlackWorkspace().routes()
		history := routes["conversations.history"]
		routes["conversations.history"] = func(r *http.Request) (int, string) {
			time.Sleep(time.Until(deadline.Add(-100 * time.Millisecond)))
			return history(r)
		}
		kind, calls := slackAt(t, routes)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		raw, err := kind.Changes(ctx, slackSource, slackFollowedTables, slackCursorAt("2026-10-29", "C1", "C2"), newSlackChanges(docs))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != string(slackCursorAt("2026-10-29", "C1", "C2")) || len(docs.offers) != 1 || len(docs.puts) != 0 || len(calls.of("users.list")) != 0 {
			t.Fatalf("next cursor = %s, offers = %q, puts = %v, calls = %q", raw, docs.offers, docs.putNames(), calls.log)
		}
	})
	t.Run("refuses changes once it may read no history", func(t *testing.T) {
		kind, _ := slackScoped(t, "channels:read,groups:read,users:read", newSlackWorkspace().routes())
		_, err := kind.Changes(ctx, slackSource, slackFollowedTables, slackCursorAt(today, "C1"), newSlackChanges(&granolaDocs{}))
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !errors.Is(err, errMissingScope) || errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v", err)
		}
		if _, err := kind.Start(ctx, slackSource, slackFollowedTables); !errors.Is(err, errMissingScope) {
			t.Fatalf("Start error = %v", err)
		}
	})
	t.Run("fails a history Slack fails to list", func(t *testing.T) {
		kind, _ := slackAt(t, with(newSlackWorkspace().routes(), map[string]string{"conversations.history": slackError("internal_error")}))
		_, err := kind.Changes(ctx, slackSource, slackFollowedTables, slackCursorAt(today, "C1"), newSlackChanges(&granolaDocs{}))
		if err == nil || errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v", err)
		}
	})
}
