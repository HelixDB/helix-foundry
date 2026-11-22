package connector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestSlackThreadsReplied(t *testing.T) {
	ctx := context.Background()
	at := func(stamp string) string { return slackTS(stamp + "Z") }
	msg := func(ts, extra string) string { return fmt.Sprintf(`{"user":"U1","text":"x","ts":%q%s}`, ts, extra) }
	a := newSlackAppTest(t, newSlackWorkspace().routes())
	day := slackDayOf("2026-10-28")
	event := func(channel, payload string) {
		a.feed.append(model.SlackRowMessage, model.SlackFeedFromEvent, channel, day, payload)
	}
	// Slack's reply fields: latest reply at 11:00, inside the span.
	listed := at("2026-10-28T09:00:00")
	event("C1", msg(listed, fmt.Sprintf(`,"thread_ts":%q,"reply_count":2,"reply_users_count":1,"latest_reply":%q`, listed, at("2026-10-28T11:00:00"))))
	// No reply fields: the replies the mirror holds, the latest at 10:30.
	held := at("2026-10-28T09:30:00")
	event("C1", msg(held, ""))
	event("C1", msg(at("2026-10-28T10:30:00"), fmt.Sprintf(`,"thread_ts":%q`, held)))
	// Replied before the span, a message in no thread, and another channel's thread.
	early := at("2026-10-28T08:00:00")
	event("C1", msg(early, fmt.Sprintf(`,"thread_ts":%q,"reply_count":1,"latest_reply":%q`, early, at("2026-10-28T08:05:00"))))
	event("C1", msg(at("2026-10-28T12:00:00"), ""))
	event("C2", msg(at("2026-10-28T09:00:00"), fmt.Sprintf(`,"thread_ts":%q,"reply_count":1,"latest_reply":%q`, listed, at("2026-10-28T11:00:00"))))
	a.fold(t)
	feed, err := a.feed.GetFoundrySlackFeed(ctx, "conn1")
	if err != nil {
		t.Fatal(err)
	}
	opts := a.app.SlackFeedOptions
	from, to := slackDayOf("2026-10-28").Add(10*time.Hour), slackDayOf("2026-10-28").Add(12*time.Hour)

	got, err := opts.SlackThreadsReplied(ctx, feed, "C1", day, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var roots []string
	for _, th := range got {
		if th.FeedID != feed.ID || th.ChannelID != "C1" || !th.Day.Equal(day) {
			t.Fatalf("a thread of the wrong feed, channel or day: %+v", th)
		}
		roots = append(roots, th.ThreadTS)
	}
	slices.Sort(roots)
	if want := []string{listed, held}; !slices.Equal(roots, want) {
		t.Fatalf("threads replied in the span are %v, want %v", roots, want)
	}

	if got, err := opts.SlackThreadsReplied(ctx, feed, "C1", day.AddDate(0, 0, 1), from, to); err != nil || len(got) != 0 {
		t.Fatalf("roots before since are left out: %v, %v", got, err)
	}
	if got, err := opts.SlackThreadsReplied(ctx, feed, "C1", day.AddDate(0, 0, -1), from, day.Add(-time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("days after the span are left out: %v, %v", got, err)
	}
	a.feed.fail = map[string]error{"FoundrySlackDaysAfter": errors.New("down")}
	if _, err := opts.SlackThreadsReplied(ctx, feed, "C1", day, from, to); err == nil {
		t.Fatal("the index's failure is returned")
	}
	a.feed.fail = nil
	for key := range a.objects.objects {
		a.objects.fail[key] = errors.New("gone")
	}
	if _, err := opts.SlackThreadsReplied(ctx, feed, "C1", day, from, to); err == nil {
		t.Fatal("a day's failure is returned")
	}
	if _, err := (SlackFeedOptions{}).SlackThreadsReplied(ctx, feed, "C1", day, from, to); !errors.Is(err, errSlackAppUnwired) {
		t.Fatalf("unwired: %v", err)
	}
}
