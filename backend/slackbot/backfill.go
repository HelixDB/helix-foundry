package slackbot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// The Slack data connector's backfill (foundry.slack.feed, the connector
// spec's §8): the history of a feed's channels, and the replies of their
// threads, read from Slack a page at a time into the feed's buffer, which
// the fold applies to the day mirror. Slack allows an app outside its
// Marketplace one call a minute per method and Slack workspace
// (foundry.slack.backfill), so every call reserves its install's slot
// (foundry_slack_slots) first, shared by every replica, and a rate limit
// defers the slot, never sleeps. Windows read newest first: gaps, then
// channels joined, then those bound. A page, its progress and the threads
// it found are written in one transaction, under a compare-and-set on the
// window or thread as read, so a page two workers read is kept once.

const (
	// backfillBatch is the most feeds one pass reads a page for, per method.
	backfillBatch = 50
	// backfillCall bounds one history or replies call.
	backfillCall = 30 * time.Second
	// slackRetryAfter is the wait of a rate limit without a usable
	// Retry-After.
	slackRetryAfter = time.Minute
	// scopeWait is how long an install whose token lacks a history scope
	// leaves its slot before trying again: its reinstall adds it.
	scopeWait = time.Hour
	// sweepFeeds is how many feeds one listing of the sweep reads.
	sweepFeeds = 100
	// sweepOrphans is the most feeds of removed installs one sweep deletes.
	sweepOrphans = 20
	// sweepFeedRows is the most buffer rows one sweep prunes.
	sweepFeedRows = 5000
)

// Backfill reads one page for each feed whose install's slot is due, of
// history (conversations.history) then of replies (conversations.replies),
// as the connector spec's §8.2 says; then closes the days the pages finished
// and folds them. How each went is counted, never returned. With the feed off
// it does nothing.
func (b *Bot) Backfill(ctx context.Context) {
	if b.feed == nil {
		return
	}
	for _, method := range model.SlackSlotMethods {
		works, err := b.db.DueFoundrySlackBackfill(ctx, method, backfillBatch)
		if err != nil {
			backfills.WithLabelValues(string(method), "error").Inc()
			log.From(ctx).Error("list the Slack backfill due failed", "method", method, "err", err)
			continue
		}
		for _, w := range works {
			backfills.WithLabelValues(string(method), b.backfill(ctx, method, w)).Inc()
		}
	}
}

// backfill reads the page work names, once its install's slot is reserved,
// and names how it went: read; busy (another replica took the slot); lossy
// (a gap past the day's budget); deferred (a rate limit); scopes (a scope
// missing); refused (the token, which the uninstall path ends); cancelled
// or gone (the channel or thread can't be read); retried, failed (the
// window's last attempt); conflict (another worker wrote the page first);
// or error.
func (b *Bot) backfill(ctx context.Context, method model.SlackSlotMethod, work model.SlackBackfillWork) string {
	feed := work.Feed
	ctx = log.Into(ctx, log.From(ctx).With("feed", feed.ID, "connector_id", feed.ConnectorID, "install_id", feed.InstallID, "method", method))
	install, err := b.db.GetFoundrySlackInstallByID(ctx, feed.InstallID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && install.RevokedAt != nil:
		return "refused"
	case err != nil:
		log.From(ctx).Warn("read a Slack feed's install failed", "err", err)
		return "error"
	}
	per := b.cfg.Backfill.HistoryPerMinute
	if method == model.SlackSlotReplies {
		per = b.cfg.Backfill.RepliesPerMinute
	}
	switch reserved, err := b.db.ReserveFoundrySlackSlot(ctx, install.ID, method, time.Minute/time.Duration(per)); {
	case err != nil:
		log.From(ctx).Warn("reserve a Slack slot failed", "err", err)
		return "error"
	case !reserved:
		return "busy"
	}
	bot, err := b.token(ctx, install.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "refused"
	case err != nil:
		log.From(ctx).Warn("open a Slack install's token failed", "err", err)
		return "error"
	}
	app := b.appOf(install)
	app.Caller = syncer.SlackBotCaller{BaseURL: b.webAPI, Token: bot.Token}
	var outcome string
	switch item := work.Item.(type) {
	case *model.SlackBackfillWindow:
		outcome = b.history(ctx, feed, app, item)
	case *model.SlackBackfillThread:
		outcome = b.replies(ctx, feed, app, item)
	default:
		return "error"
	}
	if outcome == "read" || outcome == "lossy" || outcome == "failed" || outcome == "gone" {
		b.settle(ctx, feed, app)
	}
	return outcome
}

// appOf is install as the fold names Helix's bot in it: its bot user, bot
// id and Helix's app. It calls nothing.
func (b *Bot) appOf(install *model.SlackInstall) connector.SlackApp {
	return connector.SlackApp{InstallID: install.ID, TeamID: install.TeamID, BotUserID: install.BotUserID, BotID: install.BotID, AppID: b.cfg.AppID}
}

// history reads the next page of window w, newest first: its messages, the
// span it listed (a message the mirror holds in it that the page lacks was
// deleted unseen), and the threads with replies on the days whose replies
// are read (backfill.replies_days). The next page ends at the oldest
// message listed, so no cursor outlives a call (a page that lists nothing
// but has more goes on by Slack's cursor). A gap window spends the day's
// gap budget first; past it the window is cancelled and its days close
// lossy. A gap window's first page also queues, within the budget, the
// threads whose replies the gap may have taken.
func (b *Bot) history(ctx context.Context, feed *model.SlackFeed, app connector.SlackApp, w *model.SlackBackfillWindow) string {
	bf := b.cfg.Backfill
	if w.Reason == model.SlackBackfillGap {
		switch spent, err := b.db.SpendFoundrySlackGap(ctx, feed.ID, bf.GapCallsPerDay); {
		case err != nil:
			log.From(ctx).Warn("spend a Slack gap repair failed", "err", err)
			return "error"
		case !spent:
			err := b.db.EndFoundrySlackBackfill(ctx, *w, model.SlackBackfillCancelled, "the day's gap repairs are spent: some messages may be missing")
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				log.From(ctx).Warn("end a Slack gap past its budget failed", "err", err)
				return "error"
			}
			return "lossy"
		}
	}
	query := url.Values{"channel": {w.ChannelID}, "oldest": {slackTS(w.Oldest)}, "latest": {slackTS(w.Latest)}, "inclusive": {"false"},
		"limit": {strconv.Itoa(bf.PageLimit)}}
	if w.Cursor != "" {
		query.Set("cursor", w.Cursor)
	}
	page, err := readPage(ctx, app.Caller, "conversations.history", query)
	if err != nil {
		if outcome, ok := b.slotFailed(ctx, feed, model.SlackSlotHistory, err); ok {
			return outcome
		}
		if slack.IsCode(err, "not_in_channel", "channel_not_found") {
			// The directory's next read purges the channel.
			if err := b.db.EndFoundrySlackBackfill(ctx, *w, model.SlackBackfillCancelled, err.Error()); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.From(ctx).Warn("cancel a Slack window failed", "err", err)
				return "error"
			}
			return "cancelled"
		}
		status, ferr := b.db.FailFoundrySlackBackfill(ctx, *w, err.Error(), bf.MaxAttempts)
		if ferr != nil {
			log.From(ctx).Warn("record a Slack window's failure failed", "err", ferr)
			return "error"
		}
		b.deferSlot(ctx, feed, model.SlackSlotHistory, bf.RetryBackoff<<min(w.Attempts, 10))
		if status == model.SlackBackfillFailed {
			log.From(ctx).Warn("a Slack window failed for good; its days close lossy", "channel", w.ChannelID, "err", err)
			return "failed"
		}
		log.From(ctx).Info("a Slack history page failed; it is read again", "channel", w.ChannelID, "err", err)
		return "retried"
	}
	repliesFrom := model.SlackDay(b.now()).AddDate(0, 0, -bf.RepliesDays)
	step := model.SlackHistoryStep{Window: *w, Latest: w.Latest}
	oldest := w.Latest
	var listed []string
	for _, raw := range page.Messages {
		var m slack.StoredMessage
		if json.Unmarshal(raw, &m) != nil || !model.ValidSlackTS(m.TS) {
			continue
		}
		at := slackTime(m.TS)
		listed = append(listed, m.TS)
		if at.Before(oldest) {
			oldest = at
		}
		if slack.SkippedSubtype(m.Subtype) {
			continue
		}
		step.Rows = append(step.Rows, b.pageRow(app, w.ChannelID, m))
		step.Messages++
		if m.ReplyCount > 0 && (m.ThreadTS == "" || m.ThreadTS == m.TS) && !model.SlackDay(at).Before(repliesFrom) {
			step.Threads = append(step.Threads, model.SlackBackfillThread{FeedID: feed.ID, ChannelID: w.ChannelID, ThreadTS: m.TS, Day: model.SlackDay(at)})
		}
	}
	switch {
	case page.HasMore && oldest.After(w.Oldest) && oldest.Before(w.Latest):
		step.Latest = oldest
	case page.HasMore && page.Meta.NextCursor != "":
		step.Cursor = page.Meta.NextCursor
	default:
		step.Latest, step.Done = w.Oldest, true
	}
	step.CoveredS = int64(w.Latest.Sub(step.Latest) / time.Second)
	if step.Cursor == "" {
		// Slack's bounds are exclusive: the page listed everything after its
		// oldest message (or the window's oldest), before the window's latest.
		from := step.Latest
		if step.Done {
			from = w.Oldest.Add(time.Microsecond)
		}
		to := w.Latest.Add(-time.Microsecond)
		if !to.Before(from) {
			span := connector.SlackSpan{From: slackTS(from), To: slackTS(to), Listed: listed}
			step.Rows = append(step.Rows, spanRow(w.ChannelID, model.SlackDay(to), span))
		}
	}
	if outcome := b.advance(ctx, step); outcome != "read" {
		return outcome
	}
	if w.Reason == model.SlackBackfillGap && w.Pages == 0 {
		b.lookback(ctx, feed, w)
	}
	return "read"
}

// lookback queues the threads of window w's channel whose latest reply,
// as the mirror holds it, is within feed.gap_thread_lookback before the gap
// or in it, on the days whose replies are read: their replies may have come
// in it. Each spends a gap repair; once they are spent the rest are left.
func (b *Bot) lookback(ctx context.Context, feed *model.SlackFeed, w *model.SlackBackfillWindow) {
	since := model.SlackDay(b.now()).AddDate(0, 0, -b.cfg.Backfill.RepliesDays)
	threads, err := b.feed.SlackThreadsReplied(ctx, feed, w.ChannelID, since, w.Oldest.Add(-b.cfg.Feed.GapThreadLookback), w.Latest)
	if err != nil {
		log.From(ctx).Warn("read the threads a Slack gap may have taken replies of failed", "err", err)
		return
	}
	var queue []model.SlackBackfillThread
	for _, t := range threads {
		spent, err := b.db.SpendFoundrySlackGap(ctx, feed.ID, b.cfg.Backfill.GapCallsPerDay)
		if err != nil || !spent {
			break
		}
		queue = append(queue, t)
	}
	if len(queue) == 0 {
		return
	}
	if _, err := b.db.QueueFoundrySlackThreads(ctx, queue); err != nil {
		log.From(ctx).Warn("queue the threads a Slack gap may have taken replies of failed", "err", err)
	}
}

// replies reads the next page of thread t's replies, oldest first, root
// first: its messages and the span it listed of the thread. The thread is
// done once Slack has no next page, gone once Slack has no such thread (or
// the channel can't be read, or refuses the call for good).
func (b *Bot) replies(ctx context.Context, feed *model.SlackFeed, app connector.SlackApp, t *model.SlackBackfillThread) string {
	query := url.Values{"channel": {t.ChannelID}, "ts": {t.ThreadTS}, "limit": {strconv.Itoa(b.cfg.Backfill.PageLimit)}}
	if t.Cursor != "" {
		query.Set("cursor", t.Cursor)
	}
	page, err := readPage(ctx, app.Caller, "conversations.replies", query)
	if err != nil {
		if outcome, ok := b.slotFailed(ctx, feed, model.SlackSlotReplies, err); ok {
			return outcome
		}
		if slack.IsCode(err, "thread_not_found", "not_in_channel", "channel_not_found") || slackRefused(err) {
			log.From(ctx).Info("a Slack thread can't be read; it is left", "channel", t.ChannelID, "err", err)
			if outcome := b.advance(ctx, model.SlackRepliesStep{Thread: *t, Status: model.SlackThreadGone}); outcome != "read" {
				return outcome
			}
			return "gone"
		}
		log.From(ctx).Info("a Slack replies page failed; it is read again", "channel", t.ChannelID, "err", err)
		b.deferSlot(ctx, feed, model.SlackSlotReplies, b.cfg.Backfill.RetryBackoff)
		return "retried"
	}
	step := model.SlackRepliesStep{Thread: *t, Status: model.SlackThreadDone}
	if page.HasMore && page.Meta.NextCursor != "" {
		step.Status, step.Cursor = model.SlackThreadQueued, page.Meta.NextCursor
	}
	var listed []string
	var first, last time.Time
	for _, raw := range page.Messages {
		var m slack.StoredMessage
		if json.Unmarshal(raw, &m) != nil || !model.ValidSlackTS(m.TS) {
			continue
		}
		at := slackTime(m.TS)
		if len(listed) == 0 || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
		listed = append(listed, m.TS)
		if !slack.SkippedSubtype(m.Subtype) {
			step.Rows = append(step.Rows, b.pageRow(app, t.ChannelID, m))
		}
	}
	if len(listed) > 0 {
		step.Rows = append(step.Rows, spanRow(t.ChannelID, t.Day, connector.SlackSpan{From: slackTS(first), To: slackTS(last), ThreadTS: t.ThreadTS,
			Listed: listed}))
	}
	return b.advance(ctx, step)
}

// advance writes a page's step: read, or conflict (another worker wrote
// that page first), gone (the feed was unbound) or error.
func (b *Bot) advance(ctx context.Context, step model.SlackBackfillStep) string {
	switch err := b.db.AdvanceFoundrySlackBackfill(ctx, step, b.cfg.Feed.Spacing); {
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	case errors.Is(err, store.ErrNotFound):
		return "unbound"
	case err != nil:
		log.From(ctx).Warn("write a Slack backfill page failed; it is read again", "err", err)
		return "error"
	}
	return "read"
}

// slotFailed handles what fails every call of an install's method alike,
// and names it: a rate limit defers the slot by its Retry-After; a missing
// scope by scopeWait; a token Slack refuses waits for the uninstall that
// follows. false for anything else.
func (b *Bot) slotFailed(ctx context.Context, feed *model.SlackFeed, method model.SlackSlotMethod, err error) (string, bool) {
	if wait, limited := rateLimited(err); limited {
		b.deferSlot(ctx, feed, method, wait)
		return "deferred", true
	}
	switch {
	case slack.IsCode(err, "missing_scope"):
		log.From(ctx).Warn("a Slack install lacks a history scope; its backfill waits for a reinstall", "err", err)
		b.deferSlot(ctx, feed, method, scopeWait)
		return "scopes", true
	case slack.IsCode(err, "invalid_auth", "account_inactive", "token_revoked", "token_expired", "not_authed"):
		return "refused", true
	}
	return "", false
}

// deferSlot has the feed's install's slot for method wait after.
func (b *Bot) deferSlot(ctx context.Context, feed *model.SlackFeed, method model.SlackSlotMethod, after time.Duration) {
	if err := b.db.DeferFoundrySlackSlot(ctx, feed.InstallID, method, after); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.From(ctx).Warn("defer a Slack slot failed", "err", err)
	}
}

// settle closes the feed's days read whole by now (those before
// feed.close_at past midnight UTC) and folds what waits, before and after,
// so the days a page made are closed and the closes are folded.
func (b *Bot) settle(ctx context.Context, feed *model.SlackFeed, app connector.SlackApp) {
	c, err := b.db.GetConnector(ctx, feed.WorkspaceID, feed.ConnectorID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.From(ctx).Warn("read a Slack feed's connector failed", "err", err)
		}
		return
	}
	var s connector.SlackSettings
	if err := json.Unmarshal(c.Config, &s); err != nil {
		log.From(ctx).Warn("a Slack connector's settings can't be read", "err", err)
		return
	}
	days := cmp.Or(s.HistoryDays, model.SlackAppHistoryDaysMax)
	if _, err := b.feed.Fold(ctx, feed.ConnectorID, days, app); err != nil {
		log.From(ctx).Warn("fold a Slack feed failed; its sync folds it", "err", err)
		return
	}
	closed, err := b.db.CloseFoundrySlackDays(ctx, feed.ID, model.SlackDay(b.now().Add(-b.cfg.Feed.CloseAt)), b.cfg.Feed.Spacing)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return
	case err != nil:
		log.From(ctx).Warn("close a Slack feed's days failed", "err", err)
		return
	case closed == 0:
		return
	}
	if _, err := b.feed.Fold(ctx, feed.ConnectorID, days, app); err != nil {
		log.From(ctx).Warn("fold a Slack feed failed; its sync folds it", "err", err)
	}
}

// slackPage is one page of conversations.history or conversations.replies:
// its messages as Slack sent them, and whether more follow.
type slackPage struct {
	Messages []json.RawMessage `json:"messages"`
	HasMore  bool              `json:"has_more"`
	Meta     struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// readPage calls method through caller, within backfillCall, and reads
// its page: a rate limit (429, or ratelimited) is a *slack.RateLimitedError
// of its Retry-After (slackRetryAfter when it has none), any other answer
// but a 200 with ok an *slack.APIError, as the bot's own client says them.
func readPage(ctx context.Context, caller connector.SlackCaller, method string, query url.Values) (slackPage, error) {
	ctx, cancel := context.WithTimeout(ctx, backfillCall)
	defer cancel()
	code, header, body, err := caller.Call(ctx, method, query)
	if err != nil {
		return slackPage{}, fmt.Errorf("slack: %s: %w", method, err)
	}
	var answer struct {
		slackPage
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeErr := json.Unmarshal(body, &answer)
	if code == http.StatusTooManyRequests || decodeErr == nil && answer.Error == "ratelimited" {
		wait := slackRetryAfter
		if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
			wait = time.Duration(seconds) * time.Second
		}
		return slackPage{}, &slack.RateLimitedError{Method: method, RetryAfter: wait}
	}
	switch {
	case code != http.StatusOK:
		return slackPage{}, &slack.APIError{Method: method, Status: code}
	case decodeErr != nil:
		return slackPage{}, fmt.Errorf("slack: %s: decode: %w", method, decodeErr)
	case !answer.OK:
		return slackPage{}, &slack.APIError{Method: method, Code: cmp.Or(answer.Error, "unknown_error"), Status: code}
	}
	return answer.slackPage, nil
}

// pageRow is a message a page listed as a buffer row: on its own day, or,
// a reply but a broadcast, its root's; keyed by its channel, ts and edit,
// so a page read twice is kept once. Helix's own bot's text isn't kept,
// and anyone's is cut at config.MaxDocumentBytes.
func (b *Bot) pageRow(app connector.SlackApp, channel string, m slack.StoredMessage) model.SlackFeedRow {
	ours := func(id, of string) bool { return id != "" && id == of }
	if ours(m.User, app.BotUserID) || ours(m.BotID, app.BotID) || ours(m.AppID, app.AppID) || ours(m.BotProfile.AppID, app.AppID) {
		m.Text = ""
	}
	if len(m.Text) > config.MaxDocumentBytes {
		cut := config.MaxDocumentBytes
		for cut > 0 && !utf8.RuneStart(m.Text[cut]) {
			cut--
		}
		m.Text = m.Text[:cut]
	}
	day := m.TS
	if m.ThreadTS != "" && m.ThreadTS != m.TS && m.Subtype != "thread_broadcast" {
		day = m.ThreadTS
	}
	payload, _ := json.Marshal(m) // plain fields
	return model.SlackFeedRow{DedupeKey: model.SlackBackfillDedupe(channel, m.TS, m.Edited.TS, model.SlackRowMessage), ChannelID: channel,
		Day: model.SlackDay(slackTime(day)), Kind: model.SlackRowMessage, Source: model.SlackFeedFromBackfill, Payload: payload}
}

// spanRow is a page's span as a buffer row on day, keyed by its channel,
// bounds and thread.
func spanRow(channel string, day time.Time, span connector.SlackSpan) model.SlackFeedRow {
	payload, _ := json.Marshal(span) // plain fields
	return model.SlackFeedRow{DedupeKey: model.SlackBackfillDedupe(channel, span.From+"-"+span.To, span.ThreadTS, model.SlackRowSpan), ChannelID: channel,
		Day: day, Kind: model.SlackRowSpan, Source: model.SlackFeedFromBackfill, Payload: payload}
}

// slackTS is t as a Slack ts: Unix seconds and microseconds.
func slackTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// slackTime is a Slack ts as a time, in UTC; the Unix epoch for one that
// isn't one (callers check it with model.ValidSlackTS).
func slackTime(ts string) time.Time {
	secs, frac, _ := strings.Cut(ts, ".")
	s, _ := strconv.ParseInt(secs, 10, 64)
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, us*1000).UTC()
}
