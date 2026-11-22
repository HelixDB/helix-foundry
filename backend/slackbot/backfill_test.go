package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// memObjects is the object store the feeds' day mirror lives in.
type memObjects struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (o *memObjects) Put(_ context.Context, key string, body io.ReadSeeker) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.m[key] = b
	return nil
}

func (o *memObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, ok := o.m[key]
	if !ok {
		return nil, errors.New("no such object")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (o *memObjects) Delete(_ context.Context, key string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, key)
	return nil
}

// feedWorld is a world whose install may sync channels (the history
// scopes, a known bot, an admin installer) and whose bot backfills.
type feedWorld struct {
	*world
	objects *memObjects
	conn    *model.Connector
	feed    *model.SlackFeed
}

// The feeds' tests' times: days before today, UTC.
func daysAgo(n int) time.Time { return model.SlackDay(time.Now()).AddDate(0, 0, -n) }

func newFeedWorld(t *testing.T, with ...func(*Options)) *feedWorld {
	t.Helper()
	objects := &memObjects{m: map[string][]byte{}}
	w := newWorld(t, append([]func(*Options){func(o *Options) {
		o.Objects = objects
		o.Cfg.Feed.Enabled, o.Cfg.AppID = true, "A0FOUNDRY"
		o.Cfg.Backfill.PageLimit, o.Cfg.Backfill.GapCallsPerDay, o.Cfg.Backfill.MaxAttempts = 2, 2, 2
		o.Cfg.Backfill.RetryBackoff = time.Second
	}}, with...)...)
	scopes := strings.Join(append(slices.Clone(slack.Scopes), slack.HistoryScopes...), ",")
	_, err := testDB.Exec(`UPDATE foundry_slack_installs SET scopes=?, bot_id='B0BOT', installer_admin=TRUE WHERE id=?`, scopes, w.install.ID)
	require.NoError(t, err)
	w.install, err = w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	return &feedWorld{world: w, objects: objects}
}

// bindTo is the binding of the world's install to channels, keeping days.
func (w *feedWorld) bindTo(days int, channels ...string) model.SlackFeedBind {
	return model.SlackFeedBind{InstallID: w.install.ID, Scopes: slack.HistoryScopes, NoticeVersion: 1, BoundBy: adaID, HistoryDays: days,
		Channels: channels}
}

// bind makes a Slack connection through the app reading channels, days of them.
func (w *feedWorld) bind(days int, channels ...string) {
	w.t.Helper()
	c := &model.Connector{ID: id.New(), WorkspaceID: w.ws, Kind: model.ConnectorKindSlack, Name: "Slack",
		Config: json.RawMessage(fmt.Sprintf(`{"history_days":%d,"credential":"helix_app","team_id":%q}`, days, team))}
	require.NoError(w.t, w.db.CreateSlackAppConnector(w.ctx, c, w.bindTo(days, channels...)))
	w.bound(c.ID)
}

// stage makes a Slack connection with a token, switching, staged, to the app.
func (w *feedWorld) stage(days int, channels ...string) {
	w.t.Helper()
	ctx := w.ctx
	c := &model.Connector{ID: id.New(), WorkspaceID: w.ws, Kind: model.ConnectorKindSlack, Name: "Slack",
		Config: json.RawMessage(fmt.Sprintf(`{"history_days":%d}`, days)), SecretHint: "xoxb…oken"}
	var err error
	c.SecretCiphertext, c.SecretKeyID, err = w.keys.Seal([]byte("xoxb-token"), connector.SecretContext(c.ID))
	require.NoError(w.t, err)
	require.NoError(w.t, w.db.CreateConnector(ctx, c))
	staged := json.RawMessage(fmt.Sprintf(`{"history_days":%d,"credential":"helix_app","team_id":%q,"staged":true}`, days, team))
	require.NoError(w.t, w.db.UpdateSlackAppConnector(ctx, w.ws, c.ID, model.ConnectorUpdate{Config: staged, SecretCiphertext: c.SecretCiphertext,
		SecretKeyID: c.SecretKeyID, SecretHint: c.SecretHint}, model.SlackAppBindStaged{Bind: w.bindTo(days, channels...)}))
	w.bound(c.ID)
}

func (w *feedWorld) bound(connectorID string) {
	w.t.Helper()
	var err error
	w.conn, err = w.db.GetConnector(w.ctx, w.ws, connectorID)
	require.NoError(w.t, err)
	w.feed, err = w.db.GetFoundrySlackFeed(w.ctx, connectorID)
	require.NoError(w.t, err)
}

// due makes the install's slots due now.
func (w *feedWorld) due() {
	w.t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_slack_slots SET next_at=NOW(6) - INTERVAL 1 SECOND WHERE install_id=?`, w.install.ID)
	require.NoError(w.t, err)
}

// drain backfills, the slots due each time, until nothing is left to read.
func (w *feedWorld) drain() {
	w.t.Helper()
	for range 50 {
		w.due()
		w.bot.Backfill(w.ctx)
		var left int
		require.NoError(w.t, testDB.QueryRow(`SELECT
  (SELECT COUNT(*) FROM foundry_slack_backfill WHERE feed_id=? AND status IN ('queued', 'running')) +
  (SELECT COUNT(*) FROM foundry_slack_backfill_threads WHERE feed_id=? AND status='queued')`, w.feed.ID, w.feed.ID).Scan(&left))
		if left == 0 {
			return
		}
	}
	w.t.Fatal("the backfill never finished")
}

// fold folds what the feed buffers.
func (w *feedWorld) fold() {
	w.t.Helper()
	_, err := w.bot.feed.Fold(w.ctx, w.conn.ID, 5, w.bot.appOf(w.install))
	require.NoError(w.t, err)
}

// state is the mirror's state of a channel-day, "" when it holds none.
func (w *feedWorld) state(channel string, day time.Time) model.SlackDayState {
	w.t.Helper()
	days, err := w.db.FoundrySlackDaysAfter(w.ctx, w.feed.ID, 0)
	require.NoError(w.t, err)
	for _, d := range days {
		if d.ChannelID == channel && d.Day.Equal(day) {
			return d.State
		}
	}
	return ""
}

// window is the feed's window of channel of reason.
func (w *feedWorld) window(channel string, reason model.SlackBackfillReason) model.SlackBackfillWindow {
	w.t.Helper()
	var got model.SlackBackfillWindow
	require.NoError(w.t, testDB.QueryRow(`SELECT status, attempts, pages, messages, threads, COALESCE(error, '') FROM foundry_slack_backfill
WHERE feed_id=? AND channel_id=? AND reason=? ORDER BY oldest DESC LIMIT 1`, w.feed.ID, channel, reason).
		Scan(&got.Status, &got.Attempts, &got.Pages, &got.Messages, &got.Threads, &got.Error))
	return got
}

// slotIn is how long until the install's slot for method is due.
func (w *feedWorld) slotIn(method model.SlackSlotMethod) time.Duration {
	w.t.Helper()
	var us int64
	require.NoError(w.t, testDB.QueryRow(`SELECT TIMESTAMPDIFF(MICROSECOND, NOW(6), next_at) FROM foundry_slack_slots WHERE install_id=? AND method=?`,
		w.install.ID, method).Scan(&us))
	return time.Duration(us) * time.Microsecond
}

// event appends what Slack sent of a message in channel at.
func (w *feedWorld) event(channel string, at time.Time, text string) {
	w.t.Helper()
	payload, _ := json.Marshal(map[string]any{"ts": slackTS(at), "user": ada, "text": text})
	_, err := w.db.AppendFoundrySlackFeed(w.ctx, w.install.ID, model.SlackFeedBatch{At: time.Now(), Rows: []model.SlackFeedRow{{
		DedupeKey: model.SlackEventDedupe("Ev" + id.New()), ChannelID: channel, Day: model.SlackDay(at), Kind: model.SlackRowMessage,
		Source: model.SlackFeedFromEvent, Payload: payload}}}, model.SlackFeedLimits{Spacing: time.Minute, MaxRows: 1000})
	require.NoError(w.t, err)
}

// msg is a message as Slack lists it.
func msg(at time.Time, user, text string, extra ...any) map[string]any {
	m := map[string]any{"type": "message", "ts": slackTS(at), "user": user, "text": text}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func TestBackfillReadsHistoryThenThreadsAndClosesTheirDays(t *testing.T) {
	w := newFeedWorld(t)
	d2, d3 := daysAgo(2), daysAgo(3)
	root, gone := d2.Add(10*time.Hour), d2.Add(13*time.Hour)
	w.slack.history[general] = []map[string]any{
		msg(d3.Add(9*time.Hour), ada, "three days ago"),
		msg(root, ada, "a thread", "thread_ts", slackTS(root), "reply_count", 2, "reply_users_count", 1, "latest_reply", slackTS(root.Add(2*time.Minute))),
		msg(d2.Add(11*time.Hour), "", "the bot's answer", "bot_id", "B0BOT"),
		msg(d2.Add(11*time.Hour+time.Minute), grace, "joined", "subtype", "channel_join"),
		msg(d2.Add(12*time.Hour), grace, "later"),
		// A thread Slack has no more of.
		msg(gone, grace, "deleted since", "thread_ts", slackTS(gone), "reply_count", 1, "latest_reply", slackTS(gone.Add(time.Minute))),
	}
	w.slack.replies[slackTS(root)] = []map[string]any{
		msg(root, ada, "a thread", "thread_ts", slackTS(root), "reply_count", 2),
		msg(root.Add(time.Minute), grace, "first reply", "thread_ts", slackTS(root)),
		msg(root.Add(2*time.Minute), ada, "second reply", "thread_ts", slackTS(root)),
	}
	w.bind(5, general)
	w.drain()

	history := w.slack.of("conversations.history")
	require.Len(t, history, 3, "6 messages listed 2 a page")
	assert.Equal(t, slackTS(w.feed.BoundAt), history[0].args["latest"], "the window ends at the bind")
	for i, c := range history {
		assert.Equal(t, general, c.args["channel"])
		assert.Equal(t, "false", c.args["inclusive"])
		assert.Equal(t, "2", c.args["limit"], "backfill.page_limit")
		assert.Equal(t, slackTS(w.feed.BoundAt.Add(-5*24*time.Hour)), c.args["oldest"])
		assert.Nil(t, c.args["cursor"], "no cursor outlives a page")
		if i > 0 {
			assert.Less(t, c.args["latest"], history[i-1].args["latest"], "newest first, each page ending where the last began")
		}
	}
	replies := w.slack.of("conversations.replies")
	require.Len(t, replies, 3, "two pages of the thread, one of the thread gone")
	assert.Equal(t, slackTS(gone), replies[0].args["ts"], "a thread is read once the page that found it is")
	assert.Equal(t, slackTS(root), replies[1].args["ts"])
	assert.Nil(t, replies[1].args["cursor"])
	assert.Equal(t, "2", replies[2].args["cursor"])

	bound := w.window(general, model.SlackBackfillBound)
	assert.Equal(t, model.SlackBackfillDone, bound.Status)
	assert.Equal(t, 3, bound.Pages)
	assert.Equal(t, 5, bound.Messages, "a channel join is no message")
	assert.Equal(t, 2, bound.Threads)
	var statuses []string
	rows, err := testDB.Query(`SELECT status FROM foundry_slack_backfill_threads WHERE feed_id=? ORDER BY thread_ts`, w.feed.ID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		statuses = append(statuses, s)
	}
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{"done", "gone"}, statuses)

	assert.Equal(t, model.SlackDayComplete, w.state(general, d2), "read whole once its threads were")
	assert.Equal(t, model.SlackDayComplete, w.state(general, d3))
	assert.Empty(t, w.state(general, daysAgo(1)), "a day with no message isn't held")

	var payloads []string
	rows, err = testDB.Query(`SELECT payload FROM foundry_slack_feed WHERE feed_id=? AND kind='message'`, w.feed.ID)
	require.NoError(t, err)
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		payloads = append(payloads, p)
	}
	require.NoError(t, rows.Close())
	all := strings.Join(payloads, "\n")
	assert.Contains(t, all, "second reply")
	assert.NotContains(t, all, "the bot's answer", "Helix's own bot's text isn't kept")
	assert.NotContains(t, all, "joined")
	var due bool
	require.NoError(t, testDB.QueryRow(`SELECT next_sync_at IS NOT NULL FROM connectors WHERE id=?`, w.conn.ID).Scan(&due))
	assert.True(t, due, "a pushed connector syncs what the backfill read")

	// The sweep prunes what the mirror holds.
	w.bot.Sweep(w.ctx)
	assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_feed f JOIN foundry_slack_feeds s ON s.id=f.feed_id WHERE f.feed_id=? AND f.seq <= s.folded_seq`,
		w.feed.ID))
}

func dbCount(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(query, args...).Scan(&n))
	return n
}

func TestBackfillTakesOneCallAMinutePerInstall(t *testing.T) {
	w := newFeedWorld(t)
	for i := range 6 {
		w.slack.history[general] = append(w.slack.history[general], msg(daysAgo(2).Add(time.Duration(i)*time.Hour), ada, "hi"))
	}
	w.bind(5, general)
	other, err := New(Options{DB: w.db, Sessions: w.sessions, Slack: w.bot.slack, Keys: w.keys, Cfg: w.cfg, Objects: w.objects,
		WebAPI: w.slack.URL + "/api"})
	require.NoError(t, err)
	w.due()
	w.bot.Backfill(w.ctx)
	other.Backfill(w.ctx)
	w.bot.Backfill(w.ctx)
	assert.Len(t, w.slack.of("conversations.history"), 1, "one call a minute, across workers")
	assert.InDelta(t, time.Minute.Seconds(), w.slotIn(model.SlackSlotHistory).Seconds(), 5)

	// A rate limit defers the slot by its Retry-After, else by a minute.
	w.due()
	w.slack.queue("conversations.history", http.StatusTooManyRequests, "120", `{"ok":false,"error":"ratelimited"}`)
	w.bot.Backfill(w.ctx)
	assert.InDelta(t, 120, w.slotIn(model.SlackSlotHistory).Seconds(), 5)
	w.due()
	w.slack.queue("conversations.history", http.StatusOK, "", `{"ok":false,"error":"ratelimited"}`)
	w.bot.Backfill(w.ctx)
	assert.InDelta(t, 60, w.slotIn(model.SlackSlotHistory).Seconds(), 5)
	bound := w.window(general, model.SlackBackfillBound)
	assert.Equal(t, 1, bound.Pages, "a rate limit reads nothing")
	assert.Zero(t, bound.Attempts, "and counts no failure")
}

func TestBackfillFailures(t *testing.T) {
	t.Run("a missing scope waits for a reinstall", func(t *testing.T) {
		w := newFeedWorld(t)
		w.bind(5, general)
		w.due()
		w.slack.queue("conversations.history", http.StatusOK, "", `{"ok":false,"error":"missing_scope"}`)
		w.bot.Backfill(w.ctx)
		assert.InDelta(t, time.Hour.Seconds(), w.slotIn(model.SlackSlotHistory).Seconds(), 5)
		assert.Zero(t, w.window(general, model.SlackBackfillBound).Attempts)
	})
	t.Run("a refused token waits for the uninstall", func(t *testing.T) {
		w := newFeedWorld(t)
		w.bind(5, general)
		w.due()
		w.slack.queue("conversations.history", http.StatusOK, "", `{"ok":false,"error":"invalid_auth"}`)
		w.bot.Backfill(w.ctx)
		bound := w.window(general, model.SlackBackfillBound)
		assert.Equal(t, model.SlackBackfillQueued, bound.Status)
		assert.Zero(t, bound.Attempts)
	})
	t.Run("a channel the bot left is cancelled", func(t *testing.T) {
		w := newFeedWorld(t)
		w.bind(5, general)
		w.due()
		w.slack.queue("conversations.history", http.StatusOK, "", `{"ok":false,"error":"not_in_channel"}`)
		w.bot.Backfill(w.ctx)
		bound := w.window(general, model.SlackBackfillBound)
		assert.Equal(t, model.SlackBackfillCancelled, bound.Status)
		assert.Contains(t, bound.Error, "not_in_channel")
	})
	t.Run("a window fails after its attempts, and its days close lossy", func(t *testing.T) {
		w := newFeedWorld(t)
		w.bind(5, general)
		w.event(general, daysAgo(2).Add(9*time.Hour), "seen live")
		w.fold()
		require.Equal(t, model.SlackDayPartial, w.state(general, daysAgo(2)))
		w.due()
		w.slack.queue("conversations.history", http.StatusInternalServerError, "", `oops`)
		w.bot.Backfill(w.ctx)
		bound := w.window(general, model.SlackBackfillBound)
		assert.Equal(t, model.SlackBackfillQueued, bound.Status)
		assert.Equal(t, 1, bound.Attempts)
		assert.InDelta(t, time.Minute.Seconds(), w.slotIn(model.SlackSlotHistory).Seconds(), 5, "a back-off under the slot's minute defers nothing more")
		w.due()
		w.slack.queue("conversations.history", http.StatusOK, "", `not JSON`)
		w.bot.Backfill(w.ctx)
		assert.Equal(t, model.SlackBackfillFailed, w.window(general, model.SlackBackfillBound).Status)
		assert.Equal(t, model.SlackDayLossy, w.state(general, daysAgo(2)))
	})
	t.Run("a thread Slack fails on is read again", func(t *testing.T) {
		w := newFeedWorld(t)
		root := daysAgo(2).Add(10 * time.Hour)
		w.slack.history[general] = []map[string]any{msg(root, ada, "a thread", "thread_ts", slackTS(root), "reply_count", 1)}
		w.slack.replies[slackTS(root)] = []map[string]any{msg(root, ada, "a thread"), msg(root.Add(time.Minute), ada, "reply", "thread_ts", slackTS(root))}
		w.bind(5, general)
		w.due()
		w.slack.queue("conversations.replies", http.StatusServiceUnavailable, "", ``)
		w.bot.Backfill(w.ctx)
		require.Equal(t, model.SlackBackfillDone, w.window(general, model.SlackBackfillBound).Status, "history, then the thread it found")
		require.Len(t, w.slack.of("conversations.replies"), 1)
		assert.Equal(t, 1, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_backfill_threads WHERE feed_id=? AND status='queued'`, w.feed.ID))
		assert.Equal(t, model.SlackDayPartial, w.state(general, daysAgo(2)), "its day waits for it")
		w.drain()
		assert.Equal(t, model.SlackDayComplete, w.state(general, daysAgo(2)))
	})
}

func TestGapsAreReadAgainWithinTheirBudget(t *testing.T) {
	w := newFeedWorld(t)
	d2 := daysAgo(2)
	root := d2.Add(10 * time.Hour)
	reply := root.Add(5 * time.Minute)
	w.slack.history[general] = []map[string]any{
		msg(root, ada, "a thread", "thread_ts", slackTS(root), "reply_count", 1, "latest_reply", slackTS(reply)),
	}
	w.slack.replies[slackTS(root)] = []map[string]any{msg(root, ada, "a thread"), msg(reply, grace, "reply", "thread_ts", slackTS(root))}
	w.bind(5, general)
	w.drain()
	require.Equal(t, model.SlackDayComplete, w.state(general, d2))
	before := len(w.slack.of("conversations.replies"))

	// A minute Slack never delivered, just after the thread's last reply.
	gap := model.SlackMinuteGap(general, reply.Add(time.Minute))
	_, err := w.db.AppendFoundrySlackFeedGaps(w.ctx, w.install.ID, []model.SlackFeedGap{gap}, time.Minute)
	require.NoError(t, err)
	w.fold()
	require.Equal(t, model.SlackDayPartial, w.state(general, d2))
	w.drain()
	history := w.slack.of("conversations.history")
	last := history[len(history)-1]
	assert.Equal(t, slackTS(gap.From), last.args["oldest"], "the gap's minute is read again")
	assert.Equal(t, slackTS(gap.To), last.args["latest"])
	assert.Len(t, w.slack.of("conversations.replies"), before+1, "and the thread whose replies it may have taken")
	assert.Equal(t, model.SlackDayComplete, w.state(general, d2))
	assert.Equal(t, model.SlackBackfillDone, w.window(general, model.SlackBackfillGap).Status)
	assert.Equal(t, 2, dbCount(t, `SELECT calls FROM foundry_slack_gap_budget WHERE feed_id=?`, w.feed.ID), "each call spent a repair")

	// The day's repairs are spent: the next gap isn't read, and its day is lossy.
	calls := len(history)
	_, err = w.db.AppendFoundrySlackFeedGaps(w.ctx, w.install.ID, []model.SlackFeedGap{model.SlackMinuteGap(general, d2.Add(15*time.Hour))}, time.Minute)
	require.NoError(t, err)
	w.drain()
	assert.Len(t, w.slack.of("conversations.history"), calls)
	assert.Equal(t, model.SlackBackfillCancelled, w.window(general, model.SlackBackfillGap).Status)
	assert.Equal(t, model.SlackDayLossy, w.state(general, d2))
}

func TestSweepClosesYesterdayOnceADay(t *testing.T) {
	w := newFeedWorld(t)
	w.bind(5, general)
	// A channel events hold from three days ago.
	const sales = "C0SALES"
	_, err := w.db.JoinFoundrySlackChannels(w.ctx, w.feed.ID, []model.SlackChannelJoin{{ChannelID: sales, CoveredFrom: daysAgo(3), Oldest: daysAgo(4)}})
	require.NoError(t, err)
	w.event(sales, daysAgo(2).Add(9*time.Hour), "seen live")
	w.event(general, daysAgo(2).Add(9*time.Hour), "seen live, before the backfill read it")
	w.fold()
	_, err = testDB.Exec(`UPDATE connectors SET next_sync_at=NULL WHERE id=?`, w.conn.ID)
	require.NoError(t, err)

	w.bot.Sweep(w.ctx)
	closes := `SELECT COUNT(*) FROM foundry_slack_feed WHERE feed_id=? AND kind='complete'`
	assert.Equal(t, 1, dbCount(t, closes, w.feed.ID), "only the channel events covered closes")
	assert.Equal(t, model.SlackDay(time.Now().Add(-w.cfg.Feed.CloseAt)), w.bot.closed)
	assert.Equal(t, 1, dbCount(t, `SELECT COUNT(*) FROM connectors WHERE id=? AND next_sync_at IS NOT NULL`, w.conn.ID), "and its connector syncs")
	w.bot.Sweep(w.ctx)
	assert.Equal(t, 1, dbCount(t, closes, w.feed.ID), "once a day")
	w.fold()
	assert.Equal(t, model.SlackDayComplete, w.state(sales, daysAgo(2)))
	assert.Equal(t, model.SlackDayPartial, w.state(general, daysAgo(2)))
}

func TestStagedSwitchFlipsOnceItsHistoryIsRead(t *testing.T) {
	w := newFeedWorld(t)
	w.slack.history[general] = []map[string]any{msg(daysAgo(2).Add(time.Hour), ada, "hi")}
	w.stage(5, general)
	staged := w.conn.Config

	w.bot.Sweep(w.ctx)
	c, err := w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(staged), string(c.Config), "it waits while its history is read")

	w.drain()
	// A sync of it live: the flip waits for it to end.
	require.NoError(t, w.db.EnqueueConnectorSync(w.ctx, &model.ConnectorSync{ID: id.New(), ConnectorID: w.conn.ID, WorkspaceID: w.ws}))
	w.bot.Sweep(w.ctx)
	c, err = w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
	require.NoError(t, err)
	assert.False(t, c.Pushed)
	_, err = testDB.Exec(`UPDATE connector_syncs SET status='succeeded', active_key=NULL WHERE connector_id=?`, w.conn.ID)
	require.NoError(t, err)

	w.bot.Sweep(w.ctx)
	c, err = w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
	require.NoError(t, err)
	assert.True(t, c.Pushed)
	var s connector.SlackSettings
	require.NoError(t, json.Unmarshal(c.Config, &s))
	assert.Equal(t, connector.SlackSettings{HistoryDays: 5, Credential: connector.SlackCredentialApp, TeamID: team}, s)
	secret, err := w.keys.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(c.ID))
	require.NoError(t, err)
	assert.Empty(t, secret, "the token is gone")
	assert.Empty(t, c.SecretHint)
	assert.Equal(t, 1, dbCount(t, `SELECT COUNT(*) FROM connector_syncs WHERE connector_id=? AND active_key=1 AND mode='full' AND origin='settings'`, c.ID),
		"a full read from the mirror is queued")
	assert.Equal(t, 1, dbCount(t, `SELECT COUNT(*) FROM connectors WHERE id=? AND change_cursor IS NULL`, c.ID))
	assert.Equal(t, model.SlackDayComplete, w.state(general, daysAgo(2)), "the mirror covers its window")

	// Flipped, a sweep leaves it.
	w.bot.Sweep(w.ctx)
	assert.Equal(t, 2, dbCount(t, `SELECT COUNT(*) FROM connector_syncs WHERE connector_id=?`, c.ID), "the sync that held it, and the full read")
}

// A staged switch waits for every window, not only the bound ones: a
// channel the bot joined meanwhile, one the settle's fold finds, and a gap
// all leave days partial, which the flip's full read would give no document.
func TestStagedSwitchWaitsForEveryWindow(t *testing.T) {
	w := newFeedWorld(t)
	const sales, support = "C0SALES", "C0SUPPORT"
	w.slack.history[general] = []map[string]any{msg(daysAgo(2).Add(time.Hour), ada, "hi")}
	w.stage(5, general)
	w.drain()
	staged := func(why string) {
		t.Helper()
		w.bot.Sweep(w.ctx)
		c, err := w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
		require.NoError(t, err)
		assert.False(t, c.Pushed, why)
	}

	_, err := w.db.JoinFoundrySlackChannels(w.ctx, w.feed.ID, []model.SlackChannelJoin{{ChannelID: sales, CoveredFrom: daysAgo(1), Oldest: daysAgo(5)}})
	require.NoError(t, err)
	staged("a joined window is still read")
	w.drain()

	_, err = w.db.AppendFoundrySlackFeedGaps(w.ctx, w.install.ID, []model.SlackFeedGap{model.SlackMinuteGap(general, daysAgo(2).Add(3*time.Hour))},
		time.Minute)
	require.NoError(t, err)
	staged("a gap window is still read")
	w.drain()

	joinedAt := daysAgo(1).Add(time.Hour)
	payload, _ := json.Marshal(map[string]any{"user_id": botUser, "joined_at": joinedAt.Format(time.RFC3339)})
	_, err = w.db.AppendFoundrySlackFeed(w.ctx, w.install.ID, model.SlackFeedBatch{At: time.Now(), Rows: []model.SlackFeedRow{{
		DedupeKey: model.SlackEventDedupe("Ev" + id.New()), ChannelID: support, Day: model.SlackDay(joinedAt), Kind: model.SlackRowJoined,
		Source: model.SlackFeedFromEvent, Payload: payload}}}, model.SlackFeedLimits{Spacing: time.Minute, MaxRows: 1000})
	require.NoError(t, err)
	staged("the settle's fold queued a joined window")
	assert.Equal(t, model.SlackBackfillQueued, w.window(support, model.SlackBackfillJoined).Status)
	w.drain()

	w.bot.Sweep(w.ctx)
	c, err := w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
	require.NoError(t, err)
	assert.True(t, c.Pushed, "every window read, it flips")
}

func TestOrphansAreDeletedPastTheirRetention(t *testing.T) {
	removed := func(w *feedWorld, ago time.Duration) {
		w.t.Helper()
		if err := w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, w.install.ID, false); !errors.Is(err, store.ErrNotFound) {
			require.NoError(w.t, err)
		}
		_, err := testDB.Exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6) - INTERVAL ? SECOND WHERE id=?`, int(ago.Seconds()), w.install.ID)
		require.NoError(w.t, err)
	}
	t.Run("an app connection is deleted", func(t *testing.T) {
		w := newFeedWorld(t)
		w.bind(5, general)
		removed(w, 6*24*time.Hour)
		w.bot.Sweep(w.ctx)
		_, err := w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
		require.NoError(t, err, "frozen within its retention")
		removed(w, 8*24*time.Hour)
		// A sync of it live: it waits.
		require.NoError(t, w.db.EnqueueConnectorSync(w.ctx, &model.ConnectorSync{ID: id.New(), ConnectorID: w.conn.ID, WorkspaceID: w.ws}))
		w.bot.Sweep(w.ctx)
		_, err = w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE connector_syncs SET status='succeeded', active_key=NULL WHERE connector_id=?`, w.conn.ID)
		require.NoError(t, err)
		w.bot.Sweep(w.ctx)
		_, err = w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = w.db.GetFoundrySlackFeed(w.ctx, w.conn.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "its feed goes with it")
	})
	t.Run("a staged switch reads by its token again", func(t *testing.T) {
		w := newFeedWorld(t)
		w.stage(5, general)
		removed(w, 8*24*time.Hour)
		w.bot.Sweep(w.ctx)
		c, err := w.db.GetConnector(w.ctx, w.ws, w.conn.ID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"history_days":5}`, string(c.Config))
		assert.False(t, c.Pushed)
		secret, err := w.keys.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(c.ID))
		require.NoError(t, err)
		assert.Equal(t, "xoxb-token", string(secret))
		_, err = w.db.GetFoundrySlackFeed(w.ctx, w.conn.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestReadPage(t *testing.T) {
	ctx := context.Background()
	answer := func(code int, header http.Header, body string, err error) connector.SlackCaller {
		return callerFunc(func(context.Context, string, url.Values) (int, http.Header, []byte, error) {
			return code, header, []byte(body), err
		})
	}
	retry := http.Header{"Retry-After": {"7"}}
	for name, c := range map[string]struct {
		caller connector.SlackCaller
		check  func(t *testing.T, page slackPage, err error)
	}{
		"a page": {answer(200, nil, `{"ok":true,"messages":[{"ts":"1.000001"}],"has_more":true,"response_metadata":{"next_cursor":"c2"}}`, nil),
			func(t *testing.T, page slackPage, err error) {
				require.NoError(t, err)
				assert.Len(t, page.Messages, 1)
				assert.True(t, page.HasMore)
				assert.Equal(t, "c2", page.Meta.NextCursor)
			}},
		"no answer": {answer(0, nil, "", errors.New("dial")), func(t *testing.T, _ slackPage, err error) { assert.ErrorContains(t, err, "dial") }},
		"a 429": {answer(429, retry, "", nil), func(t *testing.T, _ slackPage, err error) {
			wait, ok := rateLimited(err)
			assert.True(t, ok)
			assert.Equal(t, 7*time.Second, wait)
		}},
		"ratelimited without a Retry-After": {answer(200, nil, `{"ok":false,"error":"ratelimited"}`, nil), func(t *testing.T, _ slackPage, err error) {
			wait, _ := rateLimited(err)
			assert.Equal(t, time.Minute, wait)
		}},
		"an HTTP error": {answer(502, nil, "", nil), func(t *testing.T, _ slackPage, err error) {
			var api *slack.APIError
			require.ErrorAs(t, err, &api)
			assert.Equal(t, 502, api.Status)
			assert.False(t, slackRefused(err), "Slack's fault: tried again")
		}},
		"not JSON": {answer(200, nil, "<html>", nil), func(t *testing.T, _ slackPage, err error) { assert.ErrorContains(t, err, "decode") }},
		"a refusal": {answer(200, nil, `{"ok":false,"error":"channel_not_found"}`, nil), func(t *testing.T, _ slackPage, err error) {
			assert.True(t, slack.IsCode(err, "channel_not_found"))
		}},
		"a refusal without a code": {answer(200, nil, `{"ok":false}`, nil), func(t *testing.T, _ slackPage, err error) {
			assert.True(t, slack.IsCode(err, "unknown_error"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			page, err := readPage(ctx, c.caller, "conversations.history", nil)
			c.check(t, page, err)
		})
	}
}

type callerFunc func(context.Context, string, url.Values) (int, http.Header, []byte, error)

func (f callerFunc) Call(ctx context.Context, method string, query url.Values) (int, http.Header, []byte, error) {
	return f(ctx, method, query)
}

func TestSlackTimes(t *testing.T) {
	at := time.Date(2026, 11, 21, 9, 30, 15, 123456000, time.UTC)
	assert.Equal(t, "1795253415.123456", slackTS(at))
	assert.True(t, at.Equal(slackTime(slackTS(at))))
	assert.True(t, time.Unix(1795253415, 0).Equal(slackTime("1795253415")), "a ts without its fraction")
}

func TestUnmentionedRepliesAndTheBotsPosts(t *testing.T) {
	botAt := func(w *world, channel, thread string) *time.Time {
		w.t.Helper()
		var at *time.Time
		require.NoError(w.t, testDB.QueryRow(`SELECT bot_at FROM foundry_slack_threads WHERE install_id=? AND channel_id=? AND thread_ts=?`,
			w.install.ID, channel, thread).Scan(&at))
		return at
	}
	reply := func(w *world, user, msgTS, text string) string {
		at := slack.Conversation{TeamID: team, ChannelID: general, ThreadTS: ts(1)}
		return w.queueEvent(slack.ThreadReply{At: at, UserID: user, TS: msgTS, Text: text, EventTime: time.Now()}, user,
			model.SlackConversation(w.install.ID, general, ts(1), user))
	}
	following := func(o *Options) { o.Cfg.Follow.Enabled, o.Cfg.Follow.Window = true, 24*time.Hour }

	t.Run("a reply continues its person's thread, and the bot's posts are recorded", func(t *testing.T) {
		w := newWorld(t, following)
		w.message(ada, general, ts(1), ts(1), "<@U0BOT> who renewed?")
		w.settle()
		first := w.only()
		at := botAt(w, general, ts(1))
		require.NotNil(t, at)
		w.claimTurn(first.ID).finish(agent.StopAnswered, "")
		w.until("the first reply", w.replyDone(first.ID))

		reply(w, ada, ts(5), "and last month?")
		w.settle()
		second := w.last()
		assert.Equal(t, "and last month?", second.Input)
		assert.Equal(t, first.ThreadID, second.ThreadID)
		later := botAt(w, general, ts(1))
		assert.True(t, later.After(*at), "the second reply's post moved it on")

		// Someone unlinked, and nothing said: no word.
		posts := len(w.slack.of("chat.postMessage")) + len(w.slack.of("chat.postEphemeral"))
		reply(w, alan, ts(6), "me too")
		reply(w, ada, ts(7), "<@U0BOT>")
		w.settle()
		assert.Equal(t, posts, len(w.slack.of("chat.postMessage"))+len(w.slack.of("chat.postEphemeral")))
		assert.Len(t, w.turns(), 2)
	})
	t.Run("a direct message's posts aren't recorded", func(t *testing.T) {
		w := newWorld(t, following)
		w.message(ada, dmAda, ts(1), ts(1), "hi")
		w.settle()
		assert.Nil(t, botAt(w, dmAda, ts(1)))
	})
	t.Run("with follow-ups off nothing is recorded, and questions ask for a mention", func(t *testing.T) {
		w := newWorld(t)
		w.message(ada, general, ts(1), ts(1), "<@U0BOT> set up my ontology")
		w.settle()
		assert.Nil(t, botAt(w, general, ts(1)))
		turn := w.only()
		run := w.claimTurn(turn.ID)
		run.ask("Which region?")
		run.finish(agent.StopAsked, "")
		w.until("the reply", w.replyDone(turn.ID))
		asked := w.slack.posts("Which region?")
		require.Len(t, asked, 1)
		assert.Contains(t, asked[0].text(), "Without the mention")
	})
	t.Run("with follow-ups on, questions in a channel take a plain reply", func(t *testing.T) {
		w := newFeedWorld(t, following)
		w.message(ada, general, ts(1), ts(1), "<@U0BOT> set up my ontology")
		w.settle()
		turn := w.only()
		run := w.claimTurn(turn.ID)
		run.ask("Which region?")
		run.finish(agent.StopAsked, "")
		w.until("the reply", w.replyDone(turn.ID))
		asked := w.slack.posts("Which region?")
		require.Len(t, asked, 1)
		assert.Contains(t, asked[0].text(), "until someone else joins in")
	})
}

func TestBackfillIsOffWithoutTheFeed(t *testing.T) {
	w := newWorld(t)
	require.Nil(t, w.bot.feed)
	w.bot.Backfill(w.ctx)
	assert.Empty(t, w.slack.of("conversations.history"))

	bad := w.cfg
	bad.Feed.Enabled, bad.Backfill.PageLimit = true, 0
	_, err := New(Options{DB: w.db, Sessions: w.sessions, Slack: w.bot.slack, Keys: w.keys, Cfg: bad, Objects: &memObjects{}})
	assert.ErrorContains(t, err, "backfill")
}

// faultyStore fails the calls named in fail with their error, and passes
// the rest to the store.
type faultyStore struct {
	Store
	fail map[string]error
}

func (f *faultyStore) err(name string) error { return f.fail[name] }

func (f *faultyStore) DueFoundrySlackBackfill(ctx context.Context, method model.SlackSlotMethod, limit int) ([]model.SlackBackfillWork, error) {
	if err := f.err("DueFoundrySlackBackfill"); err != nil {
		return nil, err
	}
	return f.Store.DueFoundrySlackBackfill(ctx, method, limit)
}

func (f *faultyStore) GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error) {
	if err := f.err("GetFoundrySlackInstallByID"); err != nil {
		return nil, err
	}
	return f.Store.GetFoundrySlackInstallByID(ctx, id)
}

func (f *faultyStore) GetFoundrySlackInstallToken(ctx context.Context, installID string) (model.SlackSealedToken, error) {
	if err := f.err("GetFoundrySlackInstallToken"); err != nil {
		return model.SlackSealedToken{}, err
	}
	return f.Store.GetFoundrySlackInstallToken(ctx, installID)
}

func (f *faultyStore) ReserveFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, every time.Duration) (bool, error) {
	if err := f.err("ReserveFoundrySlackSlot"); err != nil {
		return false, err
	}
	return f.Store.ReserveFoundrySlackSlot(ctx, installID, method, every)
}

func (f *faultyStore) DeferFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, after time.Duration) error {
	if err := f.err("DeferFoundrySlackSlot"); err != nil {
		return err
	}
	return f.Store.DeferFoundrySlackSlot(ctx, installID, method, after)
}

func (f *faultyStore) AdvanceFoundrySlackBackfill(ctx context.Context, step model.SlackBackfillStep, spacing time.Duration) error {
	if err := f.err("AdvanceFoundrySlackBackfill"); err != nil {
		return err
	}
	return f.Store.AdvanceFoundrySlackBackfill(ctx, step, spacing)
}

func (f *faultyStore) FailFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, message string, maxAttempts int) (
	model.SlackBackfillStatus, error) {
	if err := f.err("FailFoundrySlackBackfill"); err != nil {
		return "", err
	}
	return f.Store.FailFoundrySlackBackfill(ctx, w, message, maxAttempts)
}

func (f *faultyStore) EndFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, status model.SlackBackfillStatus, message string) error {
	if err := f.err("EndFoundrySlackBackfill"); err != nil {
		return err
	}
	return f.Store.EndFoundrySlackBackfill(ctx, w, status, message)
}

func (f *faultyStore) SpendFoundrySlackGap(ctx context.Context, feedID string, max int) (bool, error) {
	if err := f.err("SpendFoundrySlackGap"); err != nil {
		return false, err
	}
	return f.Store.SpendFoundrySlackGap(ctx, feedID, max)
}

func (f *faultyStore) QueueFoundrySlackThreads(ctx context.Context, threads []model.SlackBackfillThread) (int, error) {
	if err := f.err("QueueFoundrySlackThreads"); err != nil {
		return 0, err
	}
	return f.Store.QueueFoundrySlackThreads(ctx, threads)
}

func (f *faultyStore) FoundrySlackDaysAfter(ctx context.Context, feedID string, seq int64) ([]model.SlackFeedDay, error) {
	if err := f.err("FoundrySlackDaysAfter"); err != nil {
		return nil, err
	}
	return f.Store.FoundrySlackDaysAfter(ctx, feedID, seq)
}

func (f *faultyStore) GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error) {
	if err := f.err("GetConnector"); err != nil {
		return nil, err
	}
	return f.Store.GetConnector(ctx, workspaceID, id)
}

func (f *faultyStore) CloseFoundrySlackDays(ctx context.Context, feedID string, before time.Time, spacing time.Duration) (int, error) {
	if err := f.err("CloseFoundrySlackDays"); err != nil {
		return 0, err
	}
	return f.Store.CloseFoundrySlackDays(ctx, feedID, before, spacing)
}

func (f *faultyStore) PruneFoundrySlackFeed(ctx context.Context, limit int) (int64, error) {
	if err := f.err("PruneFoundrySlackFeed"); err != nil {
		return 0, err
	}
	return f.Store.PruneFoundrySlackFeed(ctx, limit)
}

func (f *faultyStore) ListFoundrySlackOrphans(ctx context.Context, retention time.Duration, limit int) ([]*model.SlackFeed, error) {
	if err := f.err("ListFoundrySlackOrphans"); err != nil {
		return nil, err
	}
	return f.Store.ListFoundrySlackOrphans(ctx, retention, limit)
}

func (f *faultyStore) GetActiveConnectorSync(ctx context.Context, workspaceID, connectorID string) (*model.ConnectorSync, error) {
	if err := f.err("GetActiveConnectorSync"); err != nil {
		return nil, err
	}
	return f.Store.GetActiveConnectorSync(ctx, workspaceID, connectorID)
}

func (f *faultyStore) DeleteConnector(ctx context.Context, workspaceID, id string) error {
	if err := f.err("DeleteConnector"); err != nil {
		return err
	}
	return f.Store.DeleteConnector(ctx, workspaceID, id)
}

func (f *faultyStore) ListFoundrySlackFeeds(ctx context.Context, after string, limit int) ([]*model.SlackFeed, error) {
	if err := f.err("ListFoundrySlackFeeds"); err != nil {
		return nil, err
	}
	return f.Store.ListFoundrySlackFeeds(ctx, after, limit)
}

func (f *faultyStore) FoundrySlackBackfillProgress(ctx context.Context, feedID string) (model.SlackBackfillProgress, error) {
	if err := f.err("FoundrySlackBackfillProgress"); err != nil {
		return model.SlackBackfillProgress{}, err
	}
	return f.Store.FoundrySlackBackfillProgress(ctx, feedID)
}

func (f *faultyStore) FlipSlackAppConnector(ctx context.Context, workspaceID, connectorID string, staged, flipped json.RawMessage, sealed []byte,
	keyID string) error {
	if err := f.err("FlipSlackAppConnector"); err != nil {
		return err
	}
	return f.Store.FlipSlackAppConnector(ctx, workspaceID, connectorID, staged, flipped, sealed, keyID)
}

func (f *faultyStore) MarkFoundrySlackThreadBot(ctx context.Context, installID, channelID, threadTS string, at time.Time) error {
	if err := f.err("MarkFoundrySlackThreadBot"); err != nil {
		return err
	}
	return f.Store.MarkFoundrySlackThreadBot(ctx, installID, channelID, threadTS, at)
}

// faulty has the world's bot use a store failing the calls named.
func (w *feedWorld) faulty(fail map[string]error) {
	w.bot.db = &faultyStore{Store: w.db, fail: fail}
	w.bot.feed.Feed = w.bot.db
	direct := w.bot.transport.(Direct)
	direct.DB = w.bot.db
	w.bot.transport = direct
}

// counted is how many times outcome was counted for method's backfill.
func counted(method model.SlackSlotMethod, outcome string) float64 {
	return testutil.ToFloat64(backfills.WithLabelValues(string(method), outcome))
}

func TestBackfillStoreFailuresAreCountedNotFatal(t *testing.T) {
	history, replies := model.SlackSlotHistory, model.SlackSlotReplies
	for name, c := range map[string]struct {
		fail    map[string]error
		method  model.SlackSlotMethod
		outcome string
		// thread: the window is read and its thread queued first; gap: a
		// gap is queued first; refusal: Slack's answer to the call.
		thread, gap bool
		refusal     string
	}{
		"due":           {fail: map[string]error{"DueFoundrySlackBackfill": errBroken}, method: history, outcome: "error"},
		"install":       {fail: map[string]error{"GetFoundrySlackInstallByID": errBroken}, method: history, outcome: "error"},
		"install gone":  {fail: map[string]error{"GetFoundrySlackInstallByID": store.ErrNotFound}, method: history, outcome: "refused"},
		"slot":          {fail: map[string]error{"ReserveFoundrySlackSlot": errBroken}, method: history, outcome: "error"},
		"token":         {fail: map[string]error{"GetFoundrySlackInstallToken": errBroken}, method: history, outcome: "error"},
		"token gone":    {fail: map[string]error{"GetFoundrySlackInstallToken": store.ErrNotFound}, method: history, outcome: "refused"},
		"page":          {fail: map[string]error{"AdvanceFoundrySlackBackfill": errBroken}, method: history, outcome: "error"},
		"page raced":    {fail: map[string]error{"AdvanceFoundrySlackBackfill": store.ErrConflict}, method: history, outcome: "conflict"},
		"feed unbound":  {fail: map[string]error{"AdvanceFoundrySlackBackfill": store.ErrNotFound}, method: history, outcome: "unbound"},
		"failure":       {fail: map[string]error{"FailFoundrySlackBackfill": errBroken}, method: history, outcome: "error", refusal: "fatal_error"},
		"cancel":        {fail: map[string]error{"EndFoundrySlackBackfill": errBroken}, method: history, outcome: "error", refusal: "not_in_channel"},
		"defer":         {fail: map[string]error{"DeferFoundrySlackSlot": errBroken}, method: history, outcome: "scopes", refusal: "missing_scope"},
		"settle":        {fail: map[string]error{"GetConnector": errBroken}, method: history, outcome: "read"},
		"settle gone":   {fail: map[string]error{"GetConnector": store.ErrNotFound}, method: history, outcome: "read"},
		"close":         {fail: map[string]error{"CloseFoundrySlackDays": errBroken}, method: history, outcome: "read"},
		"close unbound": {fail: map[string]error{"CloseFoundrySlackDays": store.ErrNotFound}, method: history, outcome: "read"},
		"fold":          {fail: map[string]error{"FoundrySlackDaysAfter": errBroken}, method: history, outcome: "read"},
		"thread page":   {fail: map[string]error{"AdvanceFoundrySlackBackfill": errBroken}, method: replies, outcome: "error", thread: true},
		"thread gone":   {fail: map[string]error{"AdvanceFoundrySlackBackfill": errBroken}, method: replies, outcome: "error", thread: true, refusal: "thread_not_found"},
		"gap budget":    {fail: map[string]error{"SpendFoundrySlackGap": errBroken}, method: history, outcome: "error", gap: true},
		"gap over":      {fail: map[string]error{"EndFoundrySlackBackfill": errBroken}, method: history, outcome: "error", gap: true},
		"lookback":      {fail: map[string]error{"QueueFoundrySlackThreads": errBroken}, method: history, outcome: "read", gap: true},
	} {
		t.Run(name, func(t *testing.T) {
			w := newFeedWorld(t)
			root := daysAgo(2).Add(10 * time.Hour)
			w.slack.history[general] = []map[string]any{msg(root, ada, "a thread", "thread_ts", slackTS(root), "reply_count", 1,
				"latest_reply", slackTS(root.Add(time.Minute)))}
			w.bind(5, general)
			if c.thread || c.gap {
				w.slack.replies[slackTS(root)] = []map[string]any{msg(root, ada, "a thread")}
				w.drain()
				w.slack.replies[slackTS(root)] = nil
			}
			if c.thread {
				_, err := w.db.QueueFoundrySlackThreads(w.ctx, []model.SlackBackfillThread{{FeedID: w.feed.ID, ChannelID: general, ThreadTS: slackTS(root),
					Day: daysAgo(2)}})
				require.NoError(t, err)
				w.slack.replies[slackTS(root)] = []map[string]any{msg(root, ada, "a thread")}
			}
			if c.gap {
				_, err := w.db.AppendFoundrySlackFeedGaps(w.ctx, w.install.ID, []model.SlackFeedGap{model.SlackMinuteGap(general, root.Add(2*time.Minute))},
					time.Minute)
				require.NoError(t, err)
				if c.fail["EndFoundrySlackBackfill"] != nil {
					_, err := testDB.Exec(`INSERT INTO foundry_slack_gap_budget (feed_id, day, calls) VALUES (?, DATE(NOW(6)), 2)`, w.feed.ID)
					require.NoError(t, err)
				}
			}
			if c.refusal != "" {
				w.slack.queue("conversations."+string(c.method), http.StatusOK, "", fmt.Sprintf(`{"ok":false,"error":%q}`, c.refusal))
			}
			w.due()
			w.faulty(c.fail)
			before := counted(c.method, c.outcome)
			w.bot.Backfill(w.ctx)
			assert.Equal(t, before+1, counted(c.method, c.outcome))
		})
	}
}

func TestSweepStoreFailuresAreCountedNotFatal(t *testing.T) {
	sweep := func(step, outcome string) float64 { return testutil.ToFloat64(sweeps.WithLabelValues(step, outcome)) }
	for name, c := range map[string]struct {
		fail          map[string]error
		staged        bool
		removed       bool
		step, outcome string
	}{
		"prune":          {fail: map[string]error{"PruneFoundrySlackFeed": errBroken}, step: "prune_feed", outcome: "error"},
		"orphans":        {fail: map[string]error{"ListFoundrySlackOrphans": errBroken}, step: "orphan", outcome: "error"},
		"orphan":         {fail: map[string]error{"GetConnector": errBroken}, removed: true, step: "orphan", outcome: "error"},
		"orphan gone":    {fail: map[string]error{"GetConnector": store.ErrNotFound}, removed: true, step: "orphan", outcome: "gone"},
		"orphan's sync":  {fail: map[string]error{"GetActiveConnectorSync": errBroken}, removed: true, step: "orphan", outcome: "error"},
		"orphan delete":  {fail: map[string]error{"DeleteConnector": errBroken}, removed: true, step: "orphan", outcome: "error"},
		"orphan deleted": {fail: map[string]error{"DeleteConnector": store.ErrNotFound}, removed: true, step: "orphan", outcome: "gone"},
		"feeds":          {fail: map[string]error{"ListFoundrySlackFeeds": errBroken}, step: "feeds", outcome: "error"},
		"close":          {fail: map[string]error{"CloseFoundrySlackDays": errBroken}, step: "close", outcome: "error"},
		"flip":           {fail: map[string]error{"GetConnector": errBroken}, step: "flip", outcome: "error"},
		"progress":       {fail: map[string]error{"FoundrySlackBackfillProgress": errBroken}, staged: true, step: "flip", outcome: "error"},
		"flip install":   {fail: map[string]error{"GetFoundrySlackInstallByID": errBroken}, staged: true, step: "flip", outcome: "error"},
		"flip write":     {fail: map[string]error{"FlipSlackAppConnector": errBroken}, staged: true, step: "flip", outcome: "error"},
		"flip gone":      {fail: map[string]error{"FlipSlackAppConnector": store.ErrNotFound}, staged: true, step: "flip", outcome: "waiting"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newFeedWorld(t)
			if c.staged {
				w.stage(5, general)
				w.drain()
			} else {
				w.bind(5, general)
			}
			if c.removed {
				require.NoError(t, w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, w.install.ID, false))
				_, err := testDB.Exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6) - INTERVAL 8 DAY WHERE id=?`, w.install.ID)
				require.NoError(t, err)
			}
			w.faulty(c.fail)
			before := sweep(c.step, c.outcome)
			w.bot.Sweep(w.ctx)
			assert.Equal(t, before+1, sweep(c.step, c.outcome))
			if c.step == "close" {
				assert.True(t, w.bot.closed.IsZero(), "a close that failed is tried at the next sweep")
			}
		})
	}
}

func TestBotPostsAreRecordedBestEffort(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.Cfg.Follow.Enabled, o.Cfg.Follow.Window = true, time.Hour })
	w.bot.db = &faultyStore{Store: w.db, fail: map[string]error{"MarkFoundrySlackThreadBot": errBroken}}
	w.message(ada, general, ts(1), ts(1), "<@U0BOT> hi")
	w.settle()
	assert.Len(t, w.turns(), 1, "the message is answered all the same")
	w.bot.posted(w.ctx, w.install.ID, general, ts(1), "not a ts")
}
