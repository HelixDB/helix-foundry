package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The relay's fixtures: a standalone install linked to the world's
// workspace, and its Slack team.
const (
	localTeam   = "T0LOCAL"
	localDM     = "D0LOCAL"
	relayGone   = "C0GONE"
	relayLimitd = "C0LIMITED"
)

// relayed links a standalone install to the world's workspace, as its
// code's redeem does, and installs localTeam for it: the link and the
// relayed install.
func (w *world) relayed() (*model.RelayLink, *model.SlackInstall) {
	w.t.Helper()
	verifier := strings.Repeat("v", 43)
	code := hashOf("code-" + id.New())
	require.NoError(w.t, w.db.CreateFoundryRelayCode(w.ctx, &model.RelayCode{CodeHash: code, WorkspaceID: w.ws, UserID: adaID,
		CodeChallenge: model.RelayCodeChallenge(verifier), RedirectURI: "http://127.0.0.1:4000/settings/slack", Name: "Ada's laptop"}, time.Minute))
	link, err := w.db.RedeemFoundryRelayCode(w.ctx, code, verifier, id.New(), hashOf("hx_rl_"+id.New()))
	require.NoError(w.t, err)
	install, _, err := w.db.InstallFoundrySlack(w.ctx, &model.SlackInstall{ID: id.New(), WorkspaceID: w.ws, TeamID: localTeam, TeamName: "Local",
		AppID: "A0FOUNDRY", BotUserID: botUser, Scopes: slack.Scopes, InstalledByUserID: adaID, InstalledBySlackUserID: ada,
		Destination: model.SlackServesRelay{LinkID: link.ID, LocalWorkspaceID: id.New()}},
		func(installID string) (model.SlackSealedToken, error) {
			sealed, keyID, err := w.keys.Seal([]byte("xoxb-relay"), installID)
			return model.SlackSealedToken{Sealed: sealed, KeyID: keyID}, err
		})
	require.NoError(w.t, err)
	return link, install
}

// wrote is a message from user in channel's thread threadTS: a mention, or
// a direct message in a DM.
func wrote(user, channel, threadTS, msgTS string, shared bool) slack.Event {
	at := slack.Conversation{TeamID: localTeam, ChannelID: channel, ThreadTS: threadTS, ExtShared: shared}
	if inDM(channel) {
		return slack.DirectMessage{At: at, UserID: user, TS: msgTS, Text: "hello", EventTime: time.Now()}
	}
	return slack.Mention{At: at, UserID: user, TS: msgTS, Text: "<@U0BOT> hello", EventTime: time.Now()}
}

// deliver queues e (an event or a click) for install's local install, as
// the API does, live for ttl: the delivery's id.
func (w *world) deliver(install *model.SlackInstall, e any, ttl time.Duration) string {
	w.t.Helper()
	d := &model.RelayDelivery{ID: id.New(), InstallID: install.ID, Kind: model.SlackIntakeEvent}
	seen := model.RelaySeen{}
	switch e := e.(type) {
	case slack.Interaction:
		payload, err := slack.InteractionPayload(e)
		require.NoError(w.t, err)
		d.Kind, d.DedupeKey, d.Payload = model.SlackIntakeInteraction, slack.InteractionKey(e), payload
	case slack.Event:
		payload, err := slack.EventPayload(e)
		require.NoError(w.t, err)
		var at slack.Conversation
		switch e := e.(type) {
		case slack.Mention:
			at, d.SlackUserID = e.At, e.UserID
		case slack.DirectMessage:
			at, d.SlackUserID = e.At, e.UserID
		case slack.SessionStopped:
			at, d.SlackUserID = e.At, e.UserID
		}
		d.DedupeKey, d.Payload = model.SlackEventDedupe(fmt.Sprintf("Ev%04d", w.events.Add(1))), payload
		d.Conversation = model.SlackConversation(install.ID, at.ChannelID, at.ThreadTS, d.SlackUserID)
		seen = model.RelaySeen{ChannelID: at.ChannelID, UserIDs: []string{d.SlackUserID}}
	}
	require.NoError(w.t, w.db.QueueFoundryRelayDelivery(w.ctx, d, ttl, seen))
	return d.ID
}

// delivery is a delivery's status, and whether its conversation was told
// its install is offline.
func (w *world) delivery(deliveryID string) (string, bool) {
	w.t.Helper()
	var st string
	var noticed bool
	require.NoError(w.t, testDB.QueryRow(`SELECT status, noticed_at IS NOT NULL FROM foundry_relay_deliveries WHERE id=?`, deliveryID).
		Scan(&st, &noticed))
	return st, noticed
}

// lapse makes deliveries expired a while ago.
func (w *world) lapse(ids ...string) {
	w.t.Helper()
	for _, d := range ids {
		_, err := testDB.Exec(`UPDATE foundry_relay_deliveries SET expires_at=NOW(6) - INTERVAL 2 MINUTE WHERE id=?`, d)
		require.NoError(w.t, err)
	}
}

// postsTo is the messages and ephemerals sent to channel holding text.
func (w *world) postsTo(channel, text string) []call {
	var out []call
	for _, c := range append(w.slack.of("chat.postMessage"), w.slack.of("chat.postEphemeral")...) {
		if c.args["channel"] == channel && strings.Contains(c.text(), text) {
			out = append(out, c)
		}
	}
	return out
}

const offlineWords = "The Foundry this Slack workspace uses is offline"

func TestRelayedInstallsIntakeOnlyRefuses(t *testing.T) {
	w := newWorld(t)
	_, install := w.relayed()
	queue := func(e slack.Event, user string) string {
		payload, err := slack.EventPayload(e)
		require.NoError(t, err)
		in := &model.SlackIntake{ID: id.New(), InstallID: install.ID, DedupeKey: model.SlackEventDedupe(fmt.Sprintf("Ev%04d", w.events.Add(1))),
			Kind: model.SlackIntakeEvent, SlackUserID: user, Payload: payload}
		require.NoError(t, w.db.QueueFoundrySlackIntake(w.ctx, in))
		return in.ID
	}

	// A message from a channel shared with another organisation is refused
	// to its sender alone, as on any install; Founder never hears of it.
	shared := queue(wrote(ada, general, ts(1), ts(1), true), ada)
	require.Equal(t, 1, w.handle())
	assert.Equal(t, "done", w.row(shared).status)
	refusals := w.slack.of("chat.postEphemeral")
	require.Len(t, refusals, 1)
	assert.Equal(t, ada, refusals[0].args["user"])
	assert.Equal(t, general, refusals[0].args["channel"])
	assert.Contains(t, refusals[0].text(), sharedRefusal)

	// Anything else ends untouched: its install answers it, never Helix.
	plain := queue(wrote(ada, general, ts(2), ts(2), false), ada)
	dm := queue(wrote(ada, localDM, ts(3), ts(3), false), ada)
	stopped := queue(slack.SessionStopped{At: slack.Conversation{TeamID: localTeam, ChannelID: general, ThreadTS: ts(1), ExtShared: true}, UserID: ada}, ada)
	stop := slack.Stop{Clicked: w.clicked(ada, general, ts(1), ts(1)), TurnID: id.New()}
	payload, err := slack.InteractionPayload(stop)
	require.NoError(t, err)
	click := &model.SlackIntake{ID: id.New(), InstallID: install.ID, DedupeKey: slack.InteractionKey(stop), Kind: model.SlackIntakeInteraction,
		SlackUserID: ada, Payload: payload}
	require.NoError(t, w.db.QueueFoundrySlackIntake(w.ctx, click))
	require.Equal(t, 4, w.handle())
	for _, row := range []string{plain, dm, stopped, click.ID} {
		assert.Equal(t, "done", w.row(row).status)
	}
	assert.Len(t, w.slack.calls, 1, "only the refusal reached Slack")
	assert.Zero(t, w.founder.sends)
	assert.Empty(t, w.turns())

	// An event the bot can't read is given up; a rate limit defers the
	// refusal, which is told once it passes.
	bad := &model.SlackIntake{ID: id.New(), InstallID: install.ID, DedupeKey: model.SlackEventDedupe("EvBAD"), Kind: model.SlackIntakeEvent,
		SlackUserID: ada, Payload: json.RawMessage(`{"kind":"mention","data":{"at":"not a conversation"}}`)}
	require.NoError(t, w.db.QueueFoundrySlackIntake(w.ctx, bad))
	require.Equal(t, 1, w.handle())
	assert.Equal(t, "failed", w.row(bad.ID).status)

	w.slack.queue("chat.postEphemeral", 429, "30", `{"ok":false,"error":"ratelimited"}`)
	limited := queue(wrote(grace, relayLimitd, ts(4), ts(4), true), grace)
	require.Equal(t, 1, w.handle())
	r := w.row(limited)
	assert.Equal(t, "queued", r.status)
	assert.Zero(t, r.failures, "a rate limit is no failure")
	assert.True(t, r.next.After(time.Now().Add(20*time.Second)))
}

func TestRelayTellsConversationsTheirInstallIsOffline(t *testing.T) {
	w := newWorld(t)
	link, install := w.relayed()
	// Two messages of one thread, a direct message and a click wait for an
	// install that never claimed.
	first := w.deliver(install, wrote(ada, general, ts(1), ts(1), false), time.Hour)
	second := w.deliver(install, wrote(ada, general, ts(1), ts(2), false), time.Hour)
	dm := w.deliver(install, wrote(grace, localDM, ts(3), ts(3), false), time.Hour)
	click := w.deliver(install, slack.Approve{Clicked: w.clicked(grace, general, ts(1), ts(5)), ActionID: id.New(), PayloadHash: strings.Repeat("a", 64)},
		time.Hour)
	w.bot.Sweep(w.ctx)

	// The thread and the DM are told in a reply everyone there reads, once;
	// the clicker alone.
	thread := w.postsTo(general, offlineWords)
	require.Len(t, thread, 2)
	assert.Equal(t, "chat.postMessage", thread[0].method)
	assert.Equal(t, ts(1), thread[0].args["thread_ts"])
	assert.Contains(t, thread[0].text(), "your message waits up to 10 minutes.")
	assert.Equal(t, "chat.postEphemeral", thread[1].method)
	assert.Equal(t, grace, thread[1].args["user"])
	assert.Contains(t, thread[1].text(), "your click waits up to 10 minutes.")
	inDM := w.postsTo(localDM, offlineWords)
	require.Len(t, inDM, 1)
	assert.Equal(t, ts(3), inDM[0].args["thread_ts"])
	for _, d := range []string{first, second, dm, click} {
		st, noticed := w.delivery(d)
		assert.Equal(t, "queued", st, "a notice leaves the delivery waiting")
		assert.True(t, noticed)
	}

	// Each is told once; a conversation told lately isn't told again, and an
	// install that claims isn't offline.
	third := w.deliver(install, wrote(ada, general, ts(1), ts(6), false), time.Hour)
	w.bot.Sweep(w.ctx)
	assert.Len(t, w.postsTo(general, offlineWords), 2)
	_, noticed := w.delivery(third)
	assert.False(t, noticed)
	require.NoError(t, w.db.SeeFoundryRelayLink(w.ctx, link.ID, "203.0.113.7"))
	fresh := w.deliver(install, wrote(alan, general, ts(7), ts(7), false), time.Hour)
	w.bot.Sweep(w.ctx)
	_, noticed = w.delivery(fresh)
	assert.False(t, noticed)
	assert.Len(t, w.postsTo(general, offlineWords), 2)

	// The wait is said as relay.delivery_ttl has it.
	_, err := testDB.Exec(`UPDATE foundry_relay_links SET last_seen_at=NULL`)
	require.NoError(t, err)
	w.bot.cfg.Relay.DeliveryTTL = 40 * time.Second
	w.bot.Sweep(w.ctx)
	assert.Len(t, w.postsTo(general, "your message waits up to a minute."), 1, "the newer thread; the older was told lately")
}

func TestRelayTellsExpiredDeliveriesOnce(t *testing.T) {
	w := newWorld(t)
	link, install := w.relayed()
	require.NoError(t, w.db.SeeFoundryRelayLink(w.ctx, link.ID, "203.0.113.7"))
	a := w.deliver(install, wrote(ada, general, ts(1), ts(1), false), time.Hour)
	b := w.deliver(install, wrote(ada, general, ts(1), ts(2), false), time.Hour)
	stop := w.deliver(install, slack.SessionStopped{At: slack.Conversation{TeamID: localTeam, ChannelID: general, ThreadTS: ts(8)}, UserID: grace}, time.Hour)
	live := w.deliver(install, wrote(alan, general, ts(9), ts(9), false), time.Hour)
	w.lapse(a, b, stop)
	w.bot.Sweep(w.ctx)

	told := w.postsTo(general, notDelivered)
	require.Len(t, told, 2, "the thread once, the stop's clicker")
	assert.Equal(t, "chat.postMessage", told[0].method)
	assert.Equal(t, ts(1), told[0].args["thread_ts"])
	assert.Equal(t, "chat.postEphemeral", told[1].method)
	assert.Equal(t, grace, told[1].args["user"])
	for _, d := range []string{a, b, stop} {
		st, _ := w.delivery(d)
		assert.Equal(t, "expired", st)
	}
	st, _ := w.delivery(live)
	assert.Equal(t, "queued", st)
	assert.Empty(t, w.postsTo(general, offlineWords), "an install that claims isn't offline")

	// A delivery its install claimed has its lease's time to ack past its
	// expiry; then it expires and is told too.
	claim, err := w.db.LeaseFoundryRelay(w.ctx, link.ID, time.Minute, model.RelayBudget{Deliveries: 10})
	require.NoError(t, err)
	require.Len(t, claim.Deliveries, 1)
	_, err = testDB.Exec(`UPDATE foundry_relay_deliveries SET expires_at=NOW(6) - INTERVAL 30 SECOND WHERE id=?`, live)
	require.NoError(t, err)
	w.bot.Sweep(w.ctx)
	st, _ = w.delivery(live)
	assert.Equal(t, "leased", st)
	w.lapse(live)
	w.bot.Sweep(w.ctx)
	st, _ = w.delivery(live)
	assert.Equal(t, "expired", st)
	assert.Len(t, w.postsTo(general, notDelivered), 3)
	w.bot.Sweep(w.ctx)
	assert.Len(t, w.postsTo(general, notDelivered), 3, "told once")
}

func TestRelayNoticeOutcomes(t *testing.T) {
	w := newWorld(t)
	_, install := w.relayed()
	say := func(what string) string { return "offline: your " + what }
	of := func(deliveryID string) *model.RelayDelivery {
		var payload []byte
		var kind string
		require.NoError(t, testDB.QueryRow(`SELECT kind, payload FROM foundry_relay_deliveries WHERE id=?`, deliveryID).Scan(&kind, &payload))
		return &model.RelayDelivery{ID: deliveryID, InstallID: install.ID, Kind: model.SlackIntakeKind(kind), Payload: payload}
	}

	notice := func(d *model.RelayDelivery) string {
		budget := time.Minute
		return w.bot.relayNotice(w.ctx, d, say, &budget)
	}
	assert.Equal(t, "told", notice(of(w.deliver(install, wrote(ada, general, ts(1), ts(1), false), time.Hour))))
	w.slack.queue("chat.postMessage", 200, "", `{"ok":false,"error":"channel_not_found"}`)
	assert.Equal(t, "gone", notice(of(w.deliver(install, wrote(ada, relayGone, ts(3), ts(3), false), time.Hour))))
	w.slack.queue("chat.postEphemeral", 200, "", `{"ok":false,"error":"internal_error"}`)
	assert.Equal(t, "error", notice(of(w.deliver(install, slack.Unlink{Clicked: w.clicked(ada, relayGone, "", ts(4))}, time.Hour))))

	// Every click is told by who clicked where, one a second in a channel:
	// each waits its turn.
	for _, in := range []slack.Interaction{
		slack.Dismiss{Clicked: w.clicked(ada, general, "", ts(5)), ActionID: id.New()},
		slack.Refresh{Clicked: w.clicked(ada, general, "", ts(5)), ActionID: id.New()},
		slack.Review{Clicked: w.clicked(ada, general, "", ts(5)), ActionID: id.New(), PayloadHash: strings.Repeat("b", 64)},
		slack.Run{Clicked: w.clicked(ada, general, "", ts(5)), ActionID: id.New(), PayloadHash: strings.Repeat("c", 64)},
		slack.Stop{Clicked: w.clicked(ada, general, "", ts(5)), TurnID: id.New()},
	} {
		assert.Equal(t, "told", notice(of(w.deliver(install, in, time.Hour))), "%T", in)
	}
	assert.Len(t, w.postsTo(general, "offline: your click"), 5)

	// Past the sweep's budget a channel's pace drops the notice; a sweep
	// stopped while it waits ends it.
	assert.Equal(t, "told", notice(of(w.deliver(install, wrote(grace, general, ts(10), ts(10), false), time.Hour))))
	none := time.Duration(0)
	assert.Equal(t, "limited", w.bot.relayNotice(w.ctx, of(w.deliver(install, wrote(grace, general, ts(11), ts(11), false), time.Hour)), say, &none))
	stopping, cancel := context.WithTimeout(w.ctx, 300*time.Millisecond)
	defer cancel()
	budget := time.Minute
	assert.Equal(t, "error", w.bot.relayNotice(stopping, of(w.deliver(install, wrote(grace, general, ts(12), ts(12), false), time.Hour)), say, &budget))
	assert.Less(t, budget, time.Minute, "the wait came out of the budget")

	// A payload the API never queues, or of no conversation, isn't told.
	for _, d := range []*model.RelayDelivery{
		{ID: "d1", InstallID: install.ID, Kind: model.SlackIntakeEvent, Payload: json.RawMessage(`{"kind":"nope","data":{}}`)},
		{ID: "d2", InstallID: install.ID, Kind: model.SlackIntakeEvent, Payload: json.RawMessage(`{"kind":"mention","data":{"at":1}}`)},
		{ID: "d3", InstallID: install.ID, Kind: model.SlackIntakeInteraction, Payload: json.RawMessage(`{"kind":"stop","data":{"user_id":[]}}`)},
		{ID: "d4", InstallID: install.ID, Kind: model.SlackIntakeEvent, Payload: json.RawMessage(`{"kind":"uninstalled","data":{"team_id":"T0LOCAL"}}`)},
		{ID: "d5", InstallID: install.ID, Kind: model.SlackIntakeEvent, Payload: json.RawMessage(`{"kind":"mention","data":{"user_id":"U0ADA"}}`)},
	} {
		assert.Equal(t, "unreadable", notice(d), d.ID)
	}

	// Slack's own rate limit drops it, and holds the method back.
	w.slack.queue("chat.postMessage", 429, "30", `{"ok":false,"error":"ratelimited"}`)
	assert.Equal(t, "limited", notice(of(w.deliver(install, wrote(ada, relayLimitd, ts(2), ts(2), false), time.Hour))))
	short := 5 * time.Second
	assert.Equal(t, "limited", w.bot.relayNotice(w.ctx, of(w.deliver(install, wrote(ada, relayGone, ts(13), ts(13), false), time.Hour)), say, &short),
		"held for Slack's Retry-After, past the budget")

	// An install revoked, or unknown, isn't told.
	told := of(w.deliver(install, wrote(ada, general, ts(6), ts(6), false), time.Hour))
	unknown := *told
	unknown.InstallID = id.New()
	assert.Equal(t, "gone", notice(&unknown))
	require.NoError(t, w.db.RevokeFoundrySlackInstall(w.ctx, w.ws, install.ID, false))
	assert.Equal(t, "gone", notice(told))
	require.NoError(t, w.db.MarkFoundrySlackUninstalled(w.ctx, install.ID))
	_, err := testDB.Exec(`UPDATE foundry_slack_installs SET revoked_at=NULL WHERE id=?`, install.ID)
	require.NoError(t, err)
	assert.Equal(t, "gone", notice(told), "its token gone with Slack's uninstall")
}

func TestRelayRevokeUninstallsItsInstallsUntold(t *testing.T) {
	w := newWorld(t)
	link, install := w.relayed()
	waiting := w.deliver(install, wrote(ada, general, ts(1), ts(1), false), time.Hour)
	require.NoError(t, w.db.RevokeFoundryRelayLink(w.ctx, w.ws, link.ID))
	w.bot.Sweep(w.ctx)

	uninstalls := w.slack.of("apps.uninstall")
	require.Len(t, uninstalls, 1, "the link's install, not the workspace's own")
	got, err := w.db.GetFoundrySlackInstallByID(w.ctx, install.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.UninstalledAt)
	st, _ := w.delivery(waiting)
	assert.Equal(t, "expired", st)
	assert.Empty(t, w.postsTo(general, offlineWords))
	assert.Empty(t, w.postsTo(general, notDelivered), "a revoke's deliveries expire untold")
	mine, err := w.db.GetFoundrySlackInstallByID(w.ctx, w.install.ID)
	require.NoError(t, err)
	assert.Nil(t, mine.RevokedAt)

	w.bot.Sweep(w.ctx)
	assert.Len(t, w.slack.of("apps.uninstall"), 1, "told once")
}

func TestRelayFeedRowsHeldPastTheirTimeBecomeGaps(t *testing.T) {
	w := newWorld(t)
	link, install := w.relayed()
	local := install.Destination.(model.SlackServesRelay).LocalWorkspaceID
	require.NoError(t, w.db.SetFoundryRelayFeeds(w.ctx, link.ID, []model.RelayFeedOn{{LocalWorkspaceID: local, On: true}}))
	row := model.SlackFeedRow{DedupeKey: model.SlackEventDedupe("EvFEED1"), ChannelID: general, Day: model.SlackDay(time.Now()), Kind: model.SlackRowMessage,
		Source: model.SlackFeedFromEvent, Payload: json.RawMessage(`{"text":"hello"}`)}
	_, err := w.db.AppendFoundryRelayFeed(w.ctx, install.ID, model.RelayFeedAppend{Rows: []model.SlackFeedRow{row}, At: time.Now()},
		model.RelayFeedLimits{TTL: time.Hour, MaxRows: 100})
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_relay_feed SET expires_at=NOW(6) - INTERVAL 1 MINUTE`)
	require.NoError(t, err)
	kinds := func() []string {
		rows, err := testDB.Query(`SELECT kind FROM foundry_relay_feed WHERE install_id=? ORDER BY kind`, install.ID)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var k string
			require.NoError(t, rows.Scan(&k))
			out = append(out, k)
		}
		return out
	}

	// Without a feed_ttl the sweep leaves the relay feed alone.
	ttl := w.bot.cfg.Relay.FeedTTL
	w.bot.cfg.Relay.FeedTTL = 0
	w.bot.Sweep(w.ctx)
	assert.Equal(t, []string{"message"}, kinds())

	w.bot.cfg.Relay.FeedTTL = ttl
	w.bot.Sweep(w.ctx)
	assert.Equal(t, []string{"gap"}, kinds(), "the message its install never took is a gap it reads again")
}

// failingStore fails the relay's sweeps, and reads of installs, with err.
type failingStore struct {
	Store
	err error
}

func (f failingStore) NoticeFoundryRelayOffline(context.Context, time.Duration, time.Duration, int) ([]*model.RelayDelivery, error) {
	return nil, f.err
}

func (f failingStore) ExpireFoundryRelayDeliveries(context.Context, time.Duration, int) ([]*model.RelayDelivery, error) {
	return nil, f.err
}

func (f failingStore) ExpireFoundryRelayFeed(context.Context, time.Duration, int) (int, error) {
	return 0, f.err
}

func (f failingStore) GetFoundrySlackInstallByID(context.Context, string) (*model.SlackInstall, error) {
	return nil, f.err
}

func TestRelaySweepGoesOnPastFailures(t *testing.T) {
	w := newWorld(t, func(o *Options) { o.DB = failingStore{Store: o.DB, err: errors.New("database down")} })
	_, install := w.relayed()
	steps := []string{"relay_offline", "relay_expire", "relay_feed"}
	before := make([]float64, len(steps))
	for i, step := range steps {
		before[i] = testutil.ToFloat64(sweeps.WithLabelValues(step, "error"))
	}
	w.bot.Sweep(w.ctx)
	for i, step := range steps {
		assert.Equal(t, before[i]+1, testutil.ToFloat64(sweeps.WithLabelValues(step, "error")), step)
	}

	// A delivery whose install can't be read, or whose token can't be
	// opened, isn't told.
	d := &model.RelayDelivery{ID: id.New(), InstallID: install.ID, Kind: model.SlackIntakeEvent}
	var err error
	d.Payload, err = slack.EventPayload(wrote(ada, general, ts(1), ts(1), false))
	require.NoError(t, err)
	say := func(string) string { return notDelivered }
	budget := time.Minute
	assert.Equal(t, "error", w.bot.relayNotice(w.ctx, d, say, &budget))
	other, err := secretbox.NewKeyring("k2", map[string][]byte{"k2": bytes.Repeat([]byte{9}, 32)})
	require.NoError(t, err)
	w.bot.keys = other
	w.bot.db = w.db
	assert.Equal(t, "error", w.bot.relayNotice(w.ctx, d, say, &budget))
	assert.Empty(t, w.slack.calls)
}
