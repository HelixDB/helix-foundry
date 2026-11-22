package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// errUnsendable is a reply message past Block Kit's limits, which no retry
// sends: its follow ends.
var errUnsendable = errors.New("slackbot: the reply's message can't be sent")

// follower follows one claimed reply (the spec's §8.6): the turn's reply as
// its events so far make it, what they have the bot post apart and hasn't
// yet, and what each of the reply's messages shows in Slack.
type follower struct {
	b       *Bot
	r       *model.SlackReply
	owner   string
	install *model.SlackInstall
	bot     slack.Bot
	reply   slack.Reply
	// seq is the last event folded into reply.
	seq int
	// due is what the events have the bot post apart, not posted yet, in
	// their order.
	due []slack.Due
	// posted is the reply's record of what was posted (its Posted).
	posted map[string]string
	// written is what each of the reply's messages shows, as JSON, once
	// the follower wrote or knows it; synced is whether every message
	// shows what the reply draws now.
	written map[int]string
	synced  bool
	// nextUpdate is when the reply's messages may change next.
	nextUpdate time.Time
	// endedAt is when the follower saw the turn end; zero while it is live.
	endedAt time.Time
	// waiting is a card or questions the turn showed its person, which its
	// agent session then waits on.
	waiting bool
}

// NextFollow claims a due reply, and returns what follows its turn until it
// ends and its last message is written, or until the follow is released (a
// rate limit, a failed call, the worker stopping) or lost to another
// worker; nil when none was due. An error is the store's, claiming; how a
// follow ended is counted, and logged, never returned.
func (b *Bot) NextFollow(ctx context.Context) (func(context.Context), error) {
	r, err := b.db.ClaimFoundrySlackReply(ctx, b.cfg.Lease)
	if err != nil || r == nil {
		return nil, err
	}
	if r.LeaseOwner == nil {
		return nil, errors.New("slackbot: a claimed Slack reply has no lease owner")
	}
	return func(ctx context.Context) {
		logger := log.From(ctx).With("turn", r.TurnID, "install_id", r.InstallID)
		ctx = log.Into(ctx, logger)
		f := &follower{b: b, r: r, owner: *r.LeaseOwner, posted: map[string]string{}, written: map[int]string{}, nextUpdate: r.NextUpdateAt,
			reply: slack.Reply{TurnID: r.TurnID}}
		for k, v := range r.Posted {
			f.posted[k] = v
		}
		label := f.run(ctx)
		follows.WithLabelValues(label).Inc()
		if label != "finished" {
			logger.Info("a Slack follow stopped before its turn's reply was done", "outcome", label)
		}
	}, nil
}

// run follows until the reply is done, and names how its follow ended:
// finished, gone (its install or turn went), released (tried again after a
// while), deferred (a rate limit), lost or gave_up (Slack refused the
// reply's own message for good, or can't act there any more).
func (f *follower) run(ctx context.Context) string {
	b := f.b
	release := func(after time.Duration) {
		if err := b.db.ReleaseFoundrySlackReply(context.WithoutCancel(ctx), f.r.TurnID, f.owner, after); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.From(ctx).Warn("release a Slack follow failed; its lease lapses", "err", err)
		}
	}
	install, err := b.db.GetFoundrySlackInstallByID(ctx, f.r.InstallID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && install.RevokedAt != nil:
		return f.finish(ctx, "gone")
	case err != nil:
		release(b.cfg.RetryBackoff)
		return "released"
	}
	f.install = install
	if f.bot, err = b.token(ctx, install.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return f.finish(ctx, "gone")
		}
		release(b.cfg.RetryBackoff)
		return "released"
	}
	for {
		done, err := f.step(ctx)
		wait, limited := rateLimited(err)
		switch {
		case done:
			return "finished"
		case errors.Is(err, errLost):
			return "lost"
		case limited:
			release(wait)
			return "deferred"
		case slackGone(err) || slackRefused(err) || errors.Is(err, errUnsendable):
			log.From(ctx).Warn("Slack won't take the reply; its follow ends", "err", err)
			// Messages waiting on the turn needn't wait for the follow.
			if _, err := b.db.WakeFoundrySlackIntake(context.WithoutCancel(ctx), f.r.TurnID); err != nil {
				log.From(ctx).Warn("wake the messages waiting on a turn failed; their wait runs out", "err", err)
			}
			return f.finish(ctx, "gave_up")
		case err != nil:
			log.From(ctx).Warn("a Slack follow failed; it is tried again", "err", err)
			release(b.cfg.RetryBackoff)
			return "released"
		}
		select {
		case <-ctx.Done():
			release(0)
			return "released"
		case <-time.After(b.cfg.PollInterval):
		}
	}
}

// finish ends the follow, done, under its lease, and names how: label, or
// lost.
func (f *follower) finish(ctx context.Context, label string) string {
	switch err := f.b.db.FinishFoundrySlackReply(context.WithoutCancel(ctx), f.r.TurnID, f.owner); {
	case errors.Is(err, store.ErrNotFound):
		return "lost"
	case err != nil:
		log.From(ctx).Warn("finish a Slack follow failed; its lease lapses and it is followed again", "err", err)
		return "unrecorded"
	}
	return label
}

// update writes u under the follow's lease, renewing it: errLost once
// another worker holds it.
func (f *follower) update(ctx context.Context, u model.SlackReplyUpdate) error {
	u.Lease = f.b.cfg.Lease
	switch err := f.b.db.UpdateFoundrySlackReply(ctx, f.r.TurnID, f.owner, u); {
	case errors.Is(err, store.ErrNotFound):
		return errLost
	case err != nil:
		return fmt.Errorf("record the follow: %w", err)
	}
	return nil
}

// step reads the turn and its new events, posts what they have the bot
// post, writes the reply's messages when they may change, and, once the
// turn ended and everything shows, ends the follow: done true.
func (f *follower) step(ctx context.Context) (bool, error) {
	b := f.b
	ws := f.install.WorkspaceID
	turn, err := b.db.GetFoundryAgentTurn(ctx, ws, f.r.TurnID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Its thread was purged: there is nothing more to show.
		if f.reply.End == nil {
			f.reply.End = slack.Stopped{}
		}
		turn = nil
	case err != nil:
		return false, fmt.Errorf("read the turn: %w", err)
	}
	if turn != nil {
		for {
			events, err := b.db.ListFoundryAgentEvents(ctx, f.r.TurnID, f.seq, eventsPage)
			if err != nil {
				return false, fmt.Errorf("read the turn's events: %w", err)
			}
			for _, ev := range events {
				f.seq = ev.Seq
				e, err := agent.DecodeEvent(ev.Event)
				if err != nil {
					log.From(ctx).Warn("a turn's event can't be read; it is skipped", "seq", ev.Seq, "err", err)
					continue
				}
				due := f.reply.Apply(e)
				switch due.(type) {
				case slack.CardDue, slack.QuestionsDue:
					f.waiting = true
				}
				// Each due once: Founder polls a question with get_question,
				// whose every answer names it again.
				if due != nil && !model.SlackPostDone(f.posted[due.Key()]) &&
					!slices.ContainsFunc(f.due, func(d slack.Due) bool { return d.Key() == due.Key() }) {
					f.due = append(f.due, due)
				}
			}
			if len(events) < eventsPage {
				break
			}
		}
		if !turn.Status.Live() && f.seq < turn.LastSeq {
			// It ended, and its events up to its last are gone: its thread
			// was purged or deleted. There is nothing more to show.
			log.From(ctx).Info("a followed turn's events are gone; its reply ends", "seq", f.seq, "last_seq", turn.LastSeq)
			if f.reply.End == nil {
				f.reply.End = slack.Stopped{}
			}
			f.due = nil
		}
		if !turn.Status.Live() && f.seq >= turn.LastSeq && f.reply.End == nil {
			switch turn.Status {
			case model.FoundryAgentTurnCancelled:
				f.reply.End = slack.Stopped{By: f.r.SlackUserID}
			case model.FoundryAgentTurnFailed:
				why := ""
				if turn.Error != nil {
					why = *turn.Error
				}
				f.reply.End = slack.Finished{Finished: agent.Finished{Status: string(turn.Status), Reason: agent.StopFailed, Error: why}}
			default:
				f.reply.End = slack.Finished{Finished: agent.Finished{Status: string(turn.Status), Reason: agent.StopAnswered}}
			}
		}
	}
	live := f.reply.End == nil
	if !live && f.endedAt.IsZero() {
		f.endedAt = b.now()
	}
	if f.r.MessageTS == nil {
		// Its intake worker died between holding it and posting, or gave up.
		first := b.site.Reply(f.reply)[0]
		if err := first.Check(); err != nil {
			return false, fmt.Errorf("%w: %w", errUnsendable, err)
		}
		ts, err := b.slack.PostMessage(ctx, f.bot, slack.Post{Channel: f.r.ChannelID, ThreadTS: f.r.ThreadTS, Message: first})
		if err != nil {
			return false, err
		}
		if err := f.update(ctx, model.SlackReplyUpdate{MessageTS: &ts}); err != nil {
			return false, err
		}
		f.r.MessageTS = &ts
	}
	for len(f.due) > 0 {
		if model.SlackPostDone(f.posted[f.due[0].Key()]) {
			f.due = f.due[1:]
			continue
		}
		posted, err := f.post(ctx, f.due[0], live)
		if err != nil {
			return false, err
		}
		if !posted {
			break
		}
		f.due = f.due[1:]
	}
	if err := f.write(ctx); err != nil {
		return false, err
	}
	if live || len(f.due) > 0 || !f.synced {
		return false, f.update(ctx, model.SlackReplyUpdate{AfterSeq: &f.seq})
	}
	status := slack.AgentActive
	if e, ok := f.reply.End.(slack.Finished); f.waiting || ok && e.Reason == agent.StopAsked {
		status = slack.AgentSuspended
	}
	b.setAgentStatus(ctx, f.bot, f.r.ChannelID, f.r.ThreadTS, status)
	if _, err := b.db.WakeFoundrySlackIntake(ctx, f.r.TurnID); err != nil {
		return false, fmt.Errorf("wake the messages waiting on the turn: %w", err)
	}
	if label := f.finish(ctx, "finished"); label != "finished" {
		return false, errLost
	}
	return true, nil
}

// messageTS is the ts of the reply's i-th message: its first is the reply's
// own, each later one is posted under "m:<i>"; "" for one not posted.
func (f *follower) messageTS(i int) string {
	if i == 0 {
		return *f.r.MessageTS
	}
	if ts := f.posted["m:"+strconv.Itoa(i)]; model.ValidSlackTS(ts) {
		return ts
	}
	return ""
}

// write brings the reply's messages up to what the reply draws now, while
// they may change: each that changed is updated, each new one posted (once
// its earlier one filled, which never changes again). After a write the
// next waits updateGap times the install's live follows, so an install's
// replies share chat.update's limit. A follower that claimed the reply
// again takes its full messages as written.
func (f *follower) write(ctx context.Context) error {
	b := f.b
	msgs := b.site.Reply(f.reply)
	shown := make([]string, len(msgs))
	for i, m := range msgs {
		if err := m.Check(); err != nil {
			return fmt.Errorf("%w: %w", errUnsendable, err)
		}
		raw, err := json.Marshal(m.Blocks)
		if err != nil {
			return err
		}
		shown[i] = m.Text.String() + "\x00" + string(raw)
	}
	if len(f.written) == 0 {
		for i := range len(msgs) - 1 {
			if f.messageTS(i+1) != "" {
				f.written[i] = shown[i]
			}
		}
	}
	f.synced = true
	for i := range msgs {
		if f.written[i] != shown[i] {
			f.synced = false
		}
	}
	if f.synced || b.now().Before(f.nextUpdate) {
		return nil
	}
	for i, m := range msgs {
		if f.written[i] == shown[i] {
			continue
		}
		if ts := f.messageTS(i); ts != "" {
			if err := b.slack.UpdateMessage(ctx, f.bot, slack.Update{Channel: f.r.ChannelID, TS: ts, Message: m}); err != nil {
				return err
			}
		} else {
			key := "m:" + strconv.Itoa(i)
			if err := f.posting(ctx, key, []slack.Message{m}); err != nil {
				return err
			}
		}
		f.written[i] = shown[i]
	}
	f.synced = true
	live, err := b.db.CountFoundrySlackFollows(ctx, f.install.ID)
	if err != nil {
		return fmt.Errorf("count the install's follows: %w", err)
	}
	gap := updateGap * time.Duration(max(live, 1))
	f.nextUpdate = b.now().Add(gap)
	return f.update(ctx, model.SlackReplyUpdate{UpdateAfter: &gap})
}

// posting posts msgs in the reply's thread under key, at least once: each
// message's key is recorded pending, then its ts once posted, so a post
// cut short (a rate limit between two messages) goes on from the first
// message not posted; then key, the first message's ts. One message's key
// is key itself; the i-th of several is key and ".<i>".
func (f *follower) posting(ctx context.Context, key string, msgs []slack.Message) error {
	first := ""
	for i, m := range msgs {
		part := key
		if len(msgs) > 1 {
			part = key + "." + strconv.Itoa(i)
		}
		ts := f.posted[part]
		if !model.ValidSlackTS(ts) {
			if err := f.update(ctx, model.SlackReplyUpdate{Posted: map[string]string{part: model.SlackPostPending}}); err != nil {
				return err
			}
			var err error
			if ts, err = f.b.slack.PostMessage(ctx, f.bot, slack.Post{Channel: f.r.ChannelID, ThreadTS: f.r.ThreadTS, Message: m}); err != nil {
				return err
			}
			if err := f.update(ctx, model.SlackReplyUpdate{Posted: map[string]string{part: ts}}); err != nil {
				return err
			}
			f.posted[part] = ts
		}
		if first == "" {
			first = ts
		}
	}
	if len(msgs) > 1 {
		if err := f.update(ctx, model.SlackReplyUpdate{Posted: map[string]string{key: first}}); err != nil {
			return err
		}
		f.posted[key] = first
	}
	return nil
}

// refused records key refused for good, never to be posted or tried again.
func (f *follower) refused(ctx context.Context, key string, why error) error {
	log.From(ctx).Warn("Slack refused a post for good; it is dropped", "key", key, "err", why)
	if err := f.update(ctx, model.SlackReplyUpdate{Posted: map[string]string{key: model.SlackPostRefused}}); err != nil {
		return err
	}
	f.posted[key] = model.SlackPostRefused
	return nil
}

// post posts what an event has the bot post apart from the reply, once:
// an Analyst question's answer once it has ended (false, nothing posted,
// while it is live, until questionWait after the turn ended), the card of
// an action as the store has it now, or the questions the turn asks. One
// whose question, action or drawing is gone is dropped; one past Block
// Kit's limits, or that Slack refuses for good while the reply's thread
// still takes posts, is recorded refused (SlackPostRefused) and dropped.
func (f *follower) post(ctx context.Context, due slack.Due, turnLive bool) (bool, error) {
	b := f.b
	ws := f.install.WorkspaceID
	var msgs []slack.Message
	switch d := due.(type) {
	case slack.AnswerDue:
		q, err := b.db.GetFoundryQuestion(ctx, d.Question.OntologyID, d.Question.ID)
		switch {
		case errors.Is(err, store.ErrNotFound) || err == nil && q.WorkspaceID != ws:
			return true, nil
		case err != nil:
			return false, fmt.Errorf("read the Analyst question: %w", err)
		case q.Status.Live() && (turnLive || b.now().Sub(f.endedAt) < questionWait):
			return false, nil
		}
		name := ""
		switch o, err := b.db.GetFoundryOntology(ctx, ws, q.OntologyID); {
		case err == nil:
			name = o.Name
		case !errors.Is(err, store.ErrNotFound):
			return false, fmt.Errorf("read the question's ontology: %w", err)
		}
		why := ""
		if q.Error != nil {
			why = *q.Error
		}
		msgs = b.site.Answer(slack.Question{
			QuestionPage: slack.QuestionPage{WorkspaceID: ws, OntologyID: q.OntologyID, LoadID: q.LoadID, ThreadID: q.ThreadID},
			OntologyName: name, Status: q.Status, Error: why, Result: q.Result,
		})
	case slack.CardDue:
		a, err := b.db.GetFoundryAgentAction(ctx, ws, d.Card.ActionID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("read the action: %w", err)
		}
		state := cardState(a, "", b.now())
		if state == nil {
			state = slack.CardOpen{} // a click runs it now: Run again reads how it stands
		}
		m, err := b.site.Card(cardOf(a), slack.Chat{WorkspaceID: ws, ThreadID: a.ThreadID}, f.r.SlackUserID, state)
		if err != nil {
			log.From(ctx).Warn("an action's card can't be drawn; it is left to the site", "action", a.ID, "err", err)
			return true, nil
		}
		msgs = []slack.Message{m}
	case slack.QuestionsDue:
		msgs = b.site.Questions(d.Asked, ws, slack.RepliesIn(f.r.ChannelID, f.install.BotUserID, f.install.Scopes, false))
	default:
		return true, nil
	}
	if len(msgs) == 0 || strings.TrimSpace(msgs[0].Text.String()) == "" {
		return true, nil
	}
	for _, m := range msgs {
		if err := m.Check(); err != nil {
			return true, f.refused(ctx, due.Key(), err)
		}
	}
	switch err := f.posting(ctx, due.Key(), msgs); {
	case err != nil && slackRefused(err) && !slackGone(err):
		return true, f.refused(ctx, due.Key(), err)
	case err != nil:
		return false, err
	}
	return true, nil
}
