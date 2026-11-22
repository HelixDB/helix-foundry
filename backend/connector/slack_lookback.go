package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// SlackThreadsReplied are the threads of channel in feed's day mirror whose
// roots are on days from since's and whose latest reply, as the mirror
// holds it (Slack's reply fields, else the replies it holds), falls in
// [from, to]: the threads a gap from from to to may have taken replies of,
// which the backfill reads again. The mirror's days are read as a fold
// left them.
func (o SlackFeedOptions) SlackThreadsReplied(ctx context.Context, feed *model.SlackFeed, channel string, since, from, to time.Time) (
	[]model.SlackBackfillThread, error) {
	if o.Feed == nil || o.Objects == nil {
		return nil, errSlackAppUnwired
	}
	index, err := o.Feed.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("read the Slack feed %s days: %w", feed.ID, err)
	}
	var threads []model.SlackBackfillThread
	for _, d := range index {
		if d.ChannelID != channel || d.Day.Before(model.SlackDay(since)) || d.Day.After(to) {
			continue
		}
		day, err := o.readSlackDay(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, m := range day.top() {
			latest, ok := slackTime(m.LatestReply)
			if m.ReplyCount == 0 || !ok || latest.Before(from) || latest.After(to) {
				continue
			}
			threads = append(threads, model.SlackBackfillThread{FeedID: feed.ID, ChannelID: channel, ThreadTS: m.TS, Day: d.Day})
		}
	}
	return threads, nil
}
