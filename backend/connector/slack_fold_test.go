package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// slackDayOf is midnight UTC of a YYYY-MM-DD.
func slackDayOf(day string) time.Time {
	at, err := time.Parse(time.DateOnly, day)
	if err != nil {
		panic(err)
	}
	return at
}

func TestSlackFold(t *testing.T) {
	ctx := context.Background()
	at := func(clock string) string { return slackTS("2026-10-28T" + clock + "Z") }
	msg := func(ts, text string, extra ...string) string {
		return fmt.Sprintf(`{"user":"U1","text":%q,"ts":%q%s}`, text, ts, strings.Join(extra, ""))
	}
	edited := func(ts string) string { return fmt.Sprintf(`,"edited":{"ts":%q}`, ts) }
	thread := func(ts string) string { return fmt.Sprintf(`,"thread_ts":%q`, ts) }
	row := func(a *slackAppTest, kind model.SlackFeedRowKind, source model.SlackFeedSource, payload string) {
		a.feed.append(kind, source, "C1", slackDayOf("2026-10-28"), payload)
	}
	event := func(a *slackAppTest, kind model.SlackFeedRowKind, payload string) {
		row(a, kind, model.SlackFeedFromEvent, payload)
	}
	mirror := func(t *testing.T, a *slackAppTest) []string {
		t.Helper()
		lines, _ := a.mirror(t, "C1", "2026-10-28")
		return lines
	}
	want := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if !slices.Equal(got, want) {
			t.Fatalf("mirror holds\n %s\nwant\n %s", strings.Join(got, "\n "), strings.Join(want, "\n "))
		}
	}

	t.Run("keeps the latest edit, whatever order its rows came in", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "first"))
		event(a, model.SlackRowChanged, msg(at("09:00:00"), "third", edited(at("09:10:00"))))
		event(a, model.SlackRowChanged, msg(at("09:00:00"), "second", edited(at("09:05:00"))))
		// A page read later, of the same edit, gives way to the event.
		row(a, model.SlackRowMessage, model.SlackFeedFromBackfill, msg(at("09:00:00"), "third as listed", edited(at("09:10:00"))))
		a.fold(t)
		want(t, mirror(t, a), at("09:00:00")+" top third")
	})

	t.Run("a change to a message it never had keeps it, in the window only", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowChanged, msg(at("09:00:00"), "edited", edited(at("09:10:00"))))
		event(a, model.SlackRowChanged, msg(slackTS("2026-10-20T09:00:00Z"), "too old"))
		a.fold(t)
		want(t, mirror(t, a), at("09:00:00")+" top edited")
		if d := a.feed.day("C1", "2026-10-20"); d.ObjectKey != "" {
			t.Fatalf("a day before the window was kept: %+v", d)
		}
	})

	t.Run("a span drops what its page lacks, of history and of a thread", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		root := at("09:00:00")
		event(a, model.SlackRowMessage, msg(root, "root"))
		event(a, model.SlackRowMessage, msg(at("09:01:00"), "kept reply", thread(root)))
		event(a, model.SlackRowMessage, msg(at("09:02:00"), "deleted reply", thread(root)))
		event(a, model.SlackRowMessage, msg(at("10:00:00"), "deleted"))
		event(a, model.SlackRowMessage, msg(at("12:00:00"), "after the span"))
		row(a, model.SlackRowSpan, model.SlackFeedFromBackfill, string(encodeJSON(SlackSpan{From: at("08:00:00"), To: at("11:00:00"), Listed: []string{root}})))
		row(a, model.SlackRowSpan, model.SlackFeedFromBackfill,
			string(encodeJSON(SlackSpan{From: root, To: at("09:59:00"), ThreadTS: root, Listed: []string{at("09:01:00")}})))
		a.fold(t)
		want(t, mirror(t, a), root+" top root", at("09:01:00")+" reply kept reply", "-"+at("09:02:00"), "-"+at("10:00:00"), at("12:00:00")+" top after the span")
	})

	t.Run("a deleted root with replies is a tombstone; anything else deleted stays gone", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		root, reply, deleted, lone := at("09:00:00"), at("09:01:00"), at("09:02:00"), at("10:00:00")
		event(a, model.SlackRowMessage, msg(root, "root"))
		event(a, model.SlackRowMessage, msg(reply, "reply", thread(root)))
		event(a, model.SlackRowMessage, msg(deleted, "deleted reply", thread(root)))
		event(a, model.SlackRowMessage, msg(lone, "lone"))
		event(a, model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q}`, root))
		event(a, model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q}`, lone))
		// Deleted before its message came: a late row never revives it.
		late := at("11:00:00")
		event(a, model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q}`, late))
		event(a, model.SlackRowMessage, msg(late, "late"))
		// A reply deleted, then its message replayed by a page.
		event(a, model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q,"thread_ts":%q}`, deleted, root))
		row(a, model.SlackRowMessage, model.SlackFeedFromBackfill, msg(deleted, "deleted reply", thread(root)))
		// A tombstone keeps its text over a later version of its root, and takes its reply fields.
		row(a, model.SlackRowMessage, model.SlackFeedFromBackfill, msg(root, "root again", edited(at("09:30:00")), thread(root),
			`,"reply_count":1,"reply_users_count":1`))
		a.fold(t)
		want(t, mirror(t, a), root+" top This message was deleted.", reply+" reply reply", "-"+deleted, "-"+lone, "-"+late)
		docs := &granolaDocs{want: map[string]bool{"conversations": true}}
		a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), `{}`)
		a.fold(t)
		sinks := a.read(t, slackFollowedTables, docs)
		if rows := slackJSON(sinks["messages"].rows); len(rows) != 1 || !strings.Contains(rows[0], `"subtype":"tombstone"`) ||
			!strings.Contains(rows[0], `"reply_count":1`) || !strings.Contains(rows[0], `"thread_ts":"`+root+`"`) {
			t.Fatalf("a deleted root's row is %v", rows)
		}
	})

	t.Run("a message deleted on a day it doesn't hold yet stays gone", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		ts := slackTS("2026-10-29T09:00:00Z")
		event(a, model.SlackRowDeleted, fmt.Sprintf(`{"deleted_ts":%q}`, ts))
		event(a, model.SlackRowMessage, msg(ts, "late"))
		a.fold(t)
		if lines, _ := a.mirror(t, "C1", "2026-10-29"); !slices.Equal(lines, []string{"-" + ts}) {
			t.Fatalf("mirror holds %v", lines)
		}
	})

	t.Run("reply fields come from the latest row that knows them, never from a new message's event once one did", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		root := at("09:00:00")
		event(a, model.SlackRowMessage, msg(root, "root"))
		event(a, model.SlackRowReplied, fmt.Sprintf(`{"ts":%q,"reply_count":2,"reply_users_count":2,"latest_reply":%q}`, root, at("09:05:00")))
		// Slack's retry of the root's event keeps the counts; a later replied row wins.
		event(a, model.SlackRowMessage, msg(root, "root"))
		event(a, model.SlackRowReplied, fmt.Sprintf(`{"ts":%q,"reply_count":3,"reply_users_count":2,"latest_reply":%q}`, root, at("09:06:00")))
		// One for a root it doesn't hold changes nothing.
		event(a, model.SlackRowReplied, fmt.Sprintf(`{"ts":%q,"reply_count":1,"reply_users_count":1,"latest_reply":%q}`, at("09:50:00"), at("09:51:00")))
		a.fold(t)
		day, err := a.app.readSlackDay(ctx, a.feed.day("C1", "2026-10-28"))
		if err != nil {
			t.Fatal(err)
		}
		top := day.top()
		if len(top) != 1 || top[0].ReplyCount != 3 || top[0].ReplyUsersCount != 2 || top[0].LatestReply != at("09:06:00") || top[0].ThreadTS != root {
			t.Fatalf("root is %+v", top)
		}
	})

	t.Run("counts a root's replies itself until Slack says how many", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		root := at("09:00:00")
		event(a, model.SlackRowMessage, msg(root, "root"))
		event(a, model.SlackRowMessage, msg(at("09:01:00"), "one", thread(root)))
		event(a, model.SlackRowMessage, fmt.Sprintf(`{"user":"U2","text":"two","ts":%q,"thread_ts":%q}`, at("09:02:00"), root))
		event(a, model.SlackRowMessage, fmt.Sprintf(`{"bot_id":"B9","text":"three","ts":%q,"thread_ts":%q}`, at("09:03:00"), root))
		a.fold(t)
		day, err := a.app.readSlackDay(ctx, a.feed.day("C1", "2026-10-28"))
		if err != nil {
			t.Fatal(err)
		}
		if top := day.top(); top[0].ReplyCount != 3 || top[0].ReplyUsersCount != 3 || top[0].LatestReply != at("09:03:00") || top[0].ThreadTS != root {
			t.Fatalf("root is %+v", top[0])
		}
	})

	t.Run("keeps Helix's bot's messages as stubs, and skips what says nothing", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, fmt.Sprintf(`{"bot_id":"BX","bot_profile":{"app_id":"AHELIX"},"text":"secret","ts":%q}`, at("09:00:00")))
		event(a, model.SlackRowMessage, fmt.Sprintf(`{"subtype":"channel_join","user":"U2","text":"joined","ts":%q}`, at("09:01:00")))
		a.fold(t)
		want(t, mirror(t, a), at("09:00:00")+" top ")
	})

	t.Run("a day is complete, lossy or partial as its last row says, once it holds anything", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		closed := func(payload string) {
			a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), payload)
		}
		gap := model.SlackMinuteGap("C1", slackDayOf("2026-10-28").Add(time.Hour)).Row("C1")
		closed(`{}`)
		a.fold(t)
		if d := a.feed.day("C1", "2026-10-28"); d.ObjectKey != "" {
			t.Fatalf("a complete row kept an empty day: %+v", d)
		}
		for _, step := range []struct {
			kind    model.SlackFeedRowKind
			payload string
			want    model.SlackDayState
		}{
			{model.SlackRowMessage, msg(at("09:00:00"), "hello"), model.SlackDayPartial},
			{model.SlackRowComplete, `{}`, model.SlackDayComplete},
			{model.SlackRowGap, string(gap.Payload), model.SlackDayPartial},
			{model.SlackRowComplete, `{"lossy":true}`, model.SlackDayLossy},
			{model.SlackRowComplete, `{}`, model.SlackDayComplete},
		} {
			a.feed.append(step.kind, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), step.payload)
			a.fold(t)
			if _, state := a.mirror(t, "C1", "2026-10-28"); state != step.want || a.feed.day("C1", "2026-10-28").State != step.want {
				t.Fatalf("after %s the day is %s, want %s", step.kind, state, step.want)
			}
		}
	})

	t.Run("covers a channel the bot joined, and purges one it left", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		joinedAt := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
		a.feed.append(model.SlackRowJoined, model.SlackFeedFromEvent, "C1", joinedAt, fmt.Sprintf(`{"user_id":"UF","joined_at":%q}`, joinedAt.Format(time.RFC3339)))
		// Joined before the window: what came of it before now was skipped, so
		// the whole window is read.
		long := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		a.feed.append(model.SlackRowJoined, model.SlackFeedFromEvent, "C2", long, fmt.Sprintf(`{"joined_at":%q}`, long.Format(time.RFC3339)))
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "before leaving"))
		a.feed.append(model.SlackRowLeft, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), `{"why":"left"}`)
		// Uncovered, until the bot is back: the channel's joined backfill reads it then.
		event(a, model.SlackRowMessage, msg(at("09:30:00"), "while away"))
		back := time.Date(2026, 10, 28, 9, 45, 0, 0, time.UTC)
		a.feed.append(model.SlackRowJoined, model.SlackFeedFromEvent, "C1", back, fmt.Sprintf(`{"user_id":"UF","joined_at":%q}`, back.Format(time.RFC3339)))
		event(a, model.SlackRowMessage, msg(at("10:00:00"), "after coming back"))
		a.feed.append(model.SlackRowChannel, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), `{"name":"renamed"}`)
		a.fold(t)
		// Each is covered from now, its window read up to now.
		if len(a.feed.joins) != 3 || !a.feed.joins[0].CoveredFrom.Equal(slackTestNow) || !a.feed.joins[0].Oldest.Equal(slackDayOf("2026-10-27")) ||
			!a.feed.joins[1].Oldest.Equal(slackDayOf("2026-10-27")) || !a.feed.joins[2].CoveredFrom.Equal(slackTestNow) ||
			!slices.Equal(a.feed.purges, []string{"C1"}) {
			t.Fatalf("joins %+v, purges %v", a.feed.joins, a.feed.purges)
		}
		want(t, mirror(t, a), at("10:00:00")+" top after coming back")
	})

	t.Run("skips a row it can't read, and reads on", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		for _, bad := range []struct {
			kind    model.SlackFeedRowKind
			payload string
		}{
			{model.SlackRowMessage, `{"ts":"yesterday","text":"x"}`},
			{model.SlackRowMessage, `[]`},
			{model.SlackRowDeleted, `[]`},
			{model.SlackRowDeleted, `{"deleted_ts":"x"}`},
			{model.SlackRowReplied, `{"ts":"x"}`},
			{model.SlackRowSpan, `[]`},
			{model.SlackRowSpan, `{"from":"x","to":"1.000001"}`},
			{model.SlackRowSpan, fmt.Sprintf(`{"from":%q,"to":%q,"thread_ts":"x"}`, at("09:00:00"), at("10:00:00"))},
			{model.SlackRowComplete, `[]`},
			{model.SlackRowJoined, `{}`},
			{model.SlackRowJoined, `[]`},
		} {
			event(a, bad.kind, bad.payload)
		}
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "fine"))
		a.fold(t)
		want(t, mirror(t, a), at("09:00:00")+" top fine")
		if a.feed.feed.FoldedSeq != a.feed.feed.LastSeq {
			t.Fatalf("folded to %d of %d", a.feed.feed.FoldedSeq, a.feed.feed.LastSeq)
		}
	})

	t.Run("writes a day again only when it changed, and drops the days that left the window", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "hello"))
		a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), `{}`)
		a.fold(t)
		first := a.feed.day("C1", "2026-10-28")
		// Closed again: the day is as it was.
		a.feed.append(model.SlackRowComplete, model.SlackFeedFromEvent, "C1", slackDayOf("2026-10-28"), `{}`)
		a.fold(t)
		if again := a.feed.day("C1", "2026-10-28"); again.ObjectKey != first.ObjectKey || len(a.feed.replaced) != 0 {
			t.Fatalf("an unchanged day was written again: %+v, replaced %v", again, a.feed.replaced)
		}
		// Two days later, the 28th has left a window of three days.
		slackNow = func() time.Time { return slackTestNow.AddDate(0, 0, 2) }
		event(a, model.SlackRowMessage, fmt.Sprintf(`{"user":"U1","text":"new","ts":%q}`, slackTS("2026-11-01T09:00:00Z")))
		a.fold(t)
		if d := a.feed.day("C1", "2026-10-28"); d.ObjectKey != "" || !slices.Equal(a.feed.replaced, []string{first.ObjectKey}) {
			t.Fatalf("the 28th is still %+v; replaced %v", d, a.feed.replaced)
		}
	})

	t.Run("runs again from the new folded seq when another fold won", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "one"))
		event(a, model.SlackRowMessage, msg(at("10:00:00"), "two"))
		// Another worker folds the first row while this fold runs.
		a.feed.before = func() {
			a.feed.mu.Lock()
			a.feed.feed.FoldedSeq = 1
			a.feed.mu.Unlock()
		}
		a.fold(t)
		lines := mirror(t, a)
		want(t, lines, at("10:00:00")+" top two")
		if a.feed.commits != 1 || len(a.feed.replaced) != 1 || a.feed.feed.FoldedSeq != 2 {
			t.Fatalf("commits %d, replaced %v, folded %d", a.feed.commits, a.feed.replaced, a.feed.feed.FoldedSeq)
		}
	})

	t.Run("takes another fold of the same rows as its own", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "one"))
		a.feed.before = func() { a.fold(t) }
		a.fold(t)
		day := a.feed.day("C1", "2026-10-28")
		if a.feed.commits != 1 || len(a.feed.replaced) != 1 || a.feed.replaced[0] == day.ObjectKey {
			t.Fatalf("commits %d, replaced %v, index %s", a.feed.commits, a.feed.replaced, day.ObjectKey)
		}
		if _, err := a.objects.Open(ctx, day.ObjectKey); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fails when its commit fails, or other folds keep winning", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "one"))
		a.feed.failCommit = errors.New("connection lost")
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); err == nil || !strings.Contains(err.Error(), "connection lost") {
			t.Fatalf("Fold error = %v", err)
		}
		a.feed.failCommit = nil
		var race func()
		race = func() {
			a.feed.mu.Lock()
			for range 2 {
				a.feed.feed.LastSeq++
				a.feed.rows = append(a.feed.rows, model.SlackFeedRow{Seq: a.feed.feed.LastSeq, Kind: model.SlackRowChannel, ChannelID: "C1",
					Payload: json.RawMessage(`{}`)})
			}
			a.feed.feed.FoldedSeq = a.feed.feed.LastSeq - 1
			a.feed.before = race
			a.feed.mu.Unlock()
		}
		a.feed.before = race
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); err == nil || !errors.Is(err, errSlackFakeConflict) {
			t.Fatalf("Fold error = %v", err)
		}
	})

	t.Run("fails on a store or bucket that fails, or a catalog not wired", func(t *testing.T) {
		a := newSlackAppTest(t, newSlackWorkspace().routes())
		event(a, model.SlackRowMessage, msg(at("09:00:00"), "one"))
		a.feed.failGet = errors.New("store down")
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); err == nil {
			t.Fatal("Fold of a store that fails succeeded")
		}
		a.feed.failGet = nil
		a.fold(t)
		day := a.feed.day("C1", "2026-10-28")
		event(a, model.SlackRowMessage, msg(at("10:00:00"), "two"))
		a.objects.fail[day.ObjectKey] = snapshot.ErrMissing
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); !errors.Is(err, snapshot.ErrMissing) {
			t.Fatalf("Fold of a day gone from the bucket: %v", err)
		}
		a.objects.objects[day.ObjectKey] = []byte("not gzip")
		delete(a.objects.fail, day.ObjectKey)
		if _, err := a.app.Fold(ctx, "conn1", 3, *a.src.SlackApp); err == nil {
			t.Fatal("Fold of a damaged day succeeded")
		}
		if _, err := (SlackFeedOptions{}).Fold(ctx, "conn1", 3, SlackApp{}); !errors.Is(err, errSlackAppUnwired) {
			t.Fatalf("Fold of an unwired catalog: %v", err)
		}
	})
}

func TestSlackRank(t *testing.T) {
	early, late := slackTS("2026-10-28T09:00:00Z"), slackTS("2026-10-28T10:00:00Z")
	for _, c := range []struct {
		a, b slackRank
		less bool
	}{
		{slackRank{Edited: early, Seq: 9}, slackRank{Edited: late, Seq: 1}, true},
		{slackRank{Edited: late}, slackRank{Edited: early, Event: true}, false},
		{slackRank{Seq: 9}, slackRank{Event: true, Seq: 1}, true},
		{slackRank{Event: true, Seq: 1}, slackRank{Seq: 9}, false},
		{slackRank{Event: true, Seq: 1}, slackRank{Event: true, Seq: 2}, true},
		{slackRank{Seq: 2}, slackRank{Seq: 2}, false},
	} {
		if got := c.a.less(c.b); got != c.less {
			t.Errorf("%+v.less(%+v) = %v", c.a, c.b, got)
		}
	}
}

func TestSlackPlaces(t *testing.T) {
	root := slackTS("2026-10-27T23:00:00Z")
	day := func(s string) time.Time {
		if s == "" {
			return time.Time{}
		}
		return slackDayOf(s)
	}
	for _, c := range []struct {
		m          string
		top, reply string
	}{
		{fmt.Sprintf(`{"ts":%q}`, slackTS("2026-10-28T01:00:00Z")), "2026-10-28", ""},
		{fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, root, root), "2026-10-27", ""},
		{fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, slackTS("2026-10-28T01:00:00Z"), root), "", "2026-10-27"},
		{fmt.Sprintf(`{"ts":%q,"thread_ts":%q,"subtype":"thread_broadcast"}`, slackTS("2026-10-28T01:00:00Z"), root), "2026-10-28", "2026-10-27"},
		{fmt.Sprintf(`{"ts":%q,"thread_ts":"bad"}`, slackTS("2026-10-28T01:00:00Z")), "2026-10-28", ""},
	} {
		var m slackMessage
		_ = json.Unmarshal([]byte(c.m), &m)
		m.at, _ = slackTime(m.TS)
		if top, reply := slackPlaces(m); !top.Equal(day(c.top)) || !reply.Equal(day(c.reply)) {
			t.Errorf("slackPlaces(%s) = %s, %s; want %s, %s", c.m, top, reply, c.top, c.reply)
		}
	}
}
