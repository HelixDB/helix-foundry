package slackbot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix's Slack relay, the hosted worker's side (the spec's §10.3 and the
// connector spec's §11): a relayed install serves a standalone Foundry
// linked to its workspace (model.SlackServesRelay), whose own worker runs
// the bot. The API queues Slack's requests for it as deliveries, which its
// install claims through the door; Helix runs no Founder turn for it. What
// is left here: the refusal of a message from a channel shared with another
// organisation, which never reaches the install (relayedIntake); telling a
// conversation its install is offline, and that a delivery expired; and
// turning the channel messages it never took into gaps (Sweep, relay).

const (
	// relayOffline is how long a link may go without a claim or a call
	// before what waits for it is told its install is offline: a claim
	// waits at most 20 s, so an install online claims well within it.
	relayOffline = 90 * time.Second
	// relayNoticeEvery is the least time between two offline notices in a
	// conversation.
	relayNoticeEvery = 10 * time.Minute
	// relayAckGrace is how long past its expiry a delivery its install
	// claimed waits for its ack: a claim's lease.
	relayAckGrace = time.Minute
	// sweepRelay is the most deliveries one sweep tells of, offline and
	// expired each.
	sweepRelay = 100
	// sweepRelayFeed is the most relay feed rows one sweep turns into gaps.
	sweepRelayFeed = 5000
	// relayWaitBudget is the longest one sweep waits for Slack's pace of a
	// message a second in a channel (the Limiter's) to tell conversations
	// of one channel: a notice is given once, so it waits its turn rather
	// than be dropped.
	relayWaitBudget = 20 * time.Second
	// sharedRefusal is what a message from a channel shared with another
	// organisation is told, whoever's install it reached.
	sharedRefusal = "Helix Foundry doesn't answer in channels shared with other organisations. Message me directly instead."
	// notDelivered is what a delivery that expired unclaimed is told.
	notDelivered = "Not delivered: the Foundry install was offline."
)

// relayedIntake ends a relayed install's intake row. The API queues a
// relayed install's requests as deliveries, and here only what its install
// must never get: a mention or a direct message from a channel shared with
// another organisation, refused in Slack as every install refuses it
// (sharedRefusal). Anything else (a stop there, or a row the API should
// have relayed) ends untouched: Helix never answers for a relayed install.
func (b *Bot) relayedIntake(ctx context.Context, c claim, p slack.Payload) (model.SlackIntakeOutcome, error) {
	var at slack.Conversation
	user := ""
	if c.in.Kind == model.SlackIntakeEvent {
		e, err := slack.DecodeEvent(p.Kind, p.Data)
		if err != nil {
			return model.SlackIntakeGaveUp{Error: err.Error()}, nil
		}
		switch e := e.(type) {
		case slack.Mention:
			at, user = e.At, e.UserID
		case slack.DirectMessage:
			at, user = e.At, e.UserID
		}
	}
	if !at.ExtShared {
		log.From(ctx).Info("ended a relayed install's intake row: its install answers it, not Helix")
		return model.SlackIntakeEnded{}, nil
	}
	if err := b.tell(ctx, c.bot, at.ChannelID, at.ThreadTS, user, b.say(sharedRefusal)); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}

// relay tells each conversation whose messages wait for a linked install
// offline for relayOffline that they wait, at most once per
// relayNoticeEvery; tells each delivery that expired unclaimed (or claimed,
// and unacked for relayAckGrace past its expiry) that it wasn't delivered;
// and, with foundry.slack.relay.feed_ttl set, turns the channel messages
// held for a relayed install past it into gaps, which its install reads
// again from Slack. Deliveries of a revoked link expired with it, untold.
// The store marks each notice before it is posted, so a notice is given at
// most once: one Slack refuses or rate-limits is counted, not tried again,
// but one the Limiter holds back for a channel's pace waits its turn,
// within relayWaitBudget for the sweep.
func (b *Bot) relay(ctx context.Context) {
	budget := relayWaitBudget
	minutes := max(int(b.cfg.Relay.DeliveryTTL.Round(time.Minute)/time.Minute), 1)
	waits := "a minute"
	if minutes > 1 {
		waits = fmt.Sprintf("%d minutes", minutes)
	}
	offline := func(what string) string {
		return "The Foundry this Slack workspace uses is offline; your " + what + " waits up to " + waits + "."
	}
	switch told, err := b.db.NoticeFoundryRelayOffline(ctx, relayOffline, relayNoticeEvery, sweepRelay); {
	case err != nil:
		sweeps.WithLabelValues("relay_offline", "error").Inc()
		log.From(ctx).Error("take the relay's offline notices failed", "err", err)
	default:
		for _, d := range told {
			sweeps.WithLabelValues("relay_offline", b.relayNotice(ctx, d, offline, &budget)).Inc()
		}
	}
	switch expired, err := b.db.ExpireFoundryRelayDeliveries(ctx, relayAckGrace, sweepRelay); {
	case err != nil:
		sweeps.WithLabelValues("relay_expire", "error").Inc()
		log.From(ctx).Error("expire the relay's deliveries failed", "err", err)
	default:
		for _, d := range expired {
			sweeps.WithLabelValues("relay_expire", b.relayNotice(ctx, d, func(string) string { return notDelivered }, &budget)).Inc()
		}
	}
	if b.cfg.Relay.FeedTTL <= 0 {
		return
	}
	switch n, err := b.db.ExpireFoundryRelayFeed(ctx, b.cfg.Relay.FeedTTL, sweepRelayFeed); {
	case err != nil:
		sweeps.WithLabelValues("relay_feed", "error").Inc()
		log.From(ctx).Error("expire the relay feed's rows failed", "err", err)
	default:
		sweeps.WithLabelValues("relay_feed", "expired").Add(float64(n))
	}
}

// relayNotice tells delivery d's conversation what say says of it
// ("message" or "click"): a reply in a message's thread, where everyone in it
// reads that the bot is offline; to a click's (a shortcut's, or an agent
// session stop's) clicker alone, as tell does. A post the Limiter holds back for its
// channel's pace waits for it once, taking the wait from budget while it
// lasts. It names how that went: told; gone (its install revoked, or Slack
// saying the bot can't post there); limited (a rate limit); unreadable (a
// payload the API never queues); or error.
func (b *Bot) relayNotice(ctx context.Context, d *model.RelayDelivery, say func(what string) string, budget *time.Duration) string {
	logger := log.From(ctx).With("delivery", d.ID, "install_id", d.InstallID)
	var (
		channel, threadTS, user string
		message                 bool
	)
	p, err := slack.ReadPayload(d.Payload)
	switch {
	case err != nil:
	case d.Kind == model.SlackIntakeEvent:
		var e slack.Event
		if e, err = slack.DecodeEvent(p.Kind, p.Data); err != nil {
			break
		}
		switch e := e.(type) {
		case slack.Mention:
			channel, threadTS, user, message = e.At.ChannelID, e.At.ThreadTS, e.UserID, true
		case slack.DirectMessage:
			channel, threadTS, user, message = e.At.ChannelID, e.At.ThreadTS, e.UserID, true
		case slack.SessionStopped:
			channel, threadTS, user = e.At.ChannelID, e.At.ThreadTS, e.UserID
		}
	case d.Kind == model.SlackIntakeInteraction || d.Kind == model.SlackIntakeShortcut:
		var in slack.Interaction
		if in, err = slack.DecodeInteraction(p.Kind, p.Data); err != nil {
			break
		}
		var c slack.Clicked
		switch in := in.(type) {
		case slack.Approve:
			c = in.Clicked
		case slack.Dismiss:
			c = in.Clicked
		case slack.Refresh:
			c = in.Clicked
		case slack.Review:
			c = in.Clicked
		case slack.Run:
			c = in.Clicked
		case slack.Stop:
			c = in.Clicked
		case slack.Unlink:
			c = in.Clicked
		case slack.AskAbout:
			c = in.Clicked
		}
		channel, threadTS, user = c.ChannelID, c.ThreadTS, c.UserID
	}
	if channel == "" || user == "" {
		logger.Warn("a relay delivery's conversation can't be read; it isn't told", "kind", d.Kind, "err", err)
		return "unreadable"
	}
	text := say("click")
	if message {
		text = say("message")
	}
	install, err := b.db.GetFoundrySlackInstallByID(ctx, d.InstallID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && install.RevokedAt != nil:
		return "gone"
	case err != nil:
		logger.Warn("read a relay delivery's install failed; it isn't told", "err", err)
		return "error"
	}
	bot, err := b.token(ctx, install.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "gone"
	case err != nil:
		logger.Warn("open a relayed install's token failed; a delivery isn't told", "err", err)
		return "error"
	}
	post := func() error {
		if message {
			_, err := b.slack.PostMessage(ctx, bot, slack.Post{Channel: channel, ThreadTS: threadTS, Message: b.say(text)})
			return err
		}
		return b.tell(ctx, bot, channel, threadTS, user, b.say(text))
	}
	err = post()
	var held *slack.RateLimitedError
	if errors.As(err, &held) && held.Deferred && held.RetryAfter <= *budget {
		*budget -= held.RetryAfter
		select {
		case <-ctx.Done():
			return "error"
		case <-time.After(held.RetryAfter):
		}
		err = post()
	}
	if _, limited := rateLimited(err); limited {
		return "limited"
	}
	switch {
	case slackGone(err):
		return "gone"
	case err != nil:
		logger.Warn("tell a relay delivery's conversation failed", "err", err)
		return "error"
	}
	return "told"
}
