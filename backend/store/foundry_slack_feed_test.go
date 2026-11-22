package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// feedScopes are the scopes the tests' binds ask an install for.
var feedScopes = []string{"channels:history", "groups:history"}

// feedInstall installs team for workspaceID as install n, with the history
// scopes, a known bot and an admin installer.
func (f slackFixture) feedInstall(t *testing.T, n int, workspaceID, team, enterprise string) *model.SlackInstall {
	t.Helper()
	ctx := context.Background()
	in := f.install(n, workspaceID, team)
	in.Scopes, in.EnterpriseID = append([]string{"chat:write"}, feedScopes...), enterprise
	got, _, err := f.db.InstallFoundrySlack(ctx, in, sealAs("xoxb"))
	require.NoError(t, err)
	require.NoError(t, f.db.UpdateFoundrySlackInstallBot(ctx, got.ID, model.SlackInstallBotUpdate{BotID: ptr("B1"), InstallerAdmin: ptr(true)}))
	return got
}

func (f slackFixture) appConnector(workspaceID string, n int) *model.Connector {
	return &model.Connector{ID: id26(fmt.Sprintf("SAC-%s-%d-", f.tag, n)), WorkspaceID: workspaceID, Kind: model.ConnectorKindSlack,
		Name: fmt.Sprintf("Slack %d", n), Config: json.RawMessage(`{"credential":"helix_app"}`)}
}

func bindTo(install *model.SlackInstall, channels ...string) model.SlackFeedBind {
	return model.SlackFeedBind{InstallID: install.ID, Scopes: feedScopes, NoticeVersion: 1, BoundBy: agentUser, HistoryDays: 90, Channels: channels}
}

// bound creates app connector n of the fixture's workspace, reading
// install's channels.
func (f slackFixture) bound(t *testing.T, n int, install *model.SlackInstall, channels ...string) (*model.Connector, *model.SlackFeed) {
	t.Helper()
	c := f.appConnector(f.workspaceID, n)
	require.NoError(t, f.db.CreateSlackAppConnector(context.Background(), c, bindTo(install, channels...)))
	feed, err := f.db.GetFoundrySlackFeed(context.Background(), c.ID)
	require.NoError(t, err)
	return c, feed
}

func feedRow(channel, event string, day time.Time) model.SlackFeedRow {
	return model.SlackFeedRow{DedupeKey: model.SlackEventDedupe(event), ChannelID: channel, Day: model.SlackDay(day), Kind: model.SlackRowMessage,
		Source: model.SlackFeedFromEvent, Payload: json.RawMessage(`{"ts":"1.2","text":"hi"}`)}
}

// postedRow is a message row of channel's event posted at at, its payload's ts.
func postedRow(channel, event string, at time.Time) model.SlackFeedRow {
	r := feedRow(channel, event, at)
	r.Payload = json.RawMessage(fmt.Sprintf(`{"ts":"%d.000100","text":"hi"}`, at.Unix()))
	return r
}

var feedLimits = model.SlackFeedLimits{Spacing: 2 * time.Minute, MaxRows: 1000}

func dbCount(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(query, args...).Scan(&n))
	return n
}

func TestSlackAppConnectorBind(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "bind")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")

	before := time.Now().UTC()
	c, feed := f.bound(t, 1, install, "C2", "C1")
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.True(t, stored.Pushed, "read through the app: pushed")
	assert.Equal(t, install.ID, feed.InstallID)
	assert.Equal(t, "T1", feed.TeamID)
	assert.Equal(t, 1, feed.NoticeVersion)
	assert.Equal(t, agentUser, feed.BoundBy)
	assert.Nil(t, feed.DeletingAt)
	assert.WithinDuration(t, before, feed.BoundAt, 10*time.Second)

	channels, err := f.db.ListFoundrySlackFeedChannels(ctx, feed.ID)
	require.NoError(t, err)
	require.Len(t, channels, 2)
	for _, ch := range channels {
		assert.True(t, ch.CoveredFrom.Equal(feed.BoundAt), "events hold %s from the bind", ch.ChannelID)
	}
	progress, err := f.db.FoundrySlackBackfillProgress(ctx, feed.ID)
	require.NoError(t, err)
	require.Len(t, progress.Windows, 2)
	for i, want := range []string{"C2", "C1"} {
		w := progress.Windows[i]
		assert.Equal(t, want, w.ChannelID, "in the bind's order")
		assert.Equal(t, i, w.Place)
		assert.Equal(t, model.SlackBackfillBound, w.Reason)
		assert.Equal(t, model.SlackBackfillQueued, w.Status)
		assert.True(t, w.Latest.Equal(feed.BoundAt))
		assert.True(t, w.Oldest.Equal(feed.BoundAt.Add(-90*24*time.Hour)), "the window's history")
	}
	assert.False(t, progress.HistoryAt.IsZero(), "the install has its slots")

	t.Run("in use", func(t *testing.T) {
		err := f.db.CreateSlackAppConnector(ctx, f.appConnector(f.workspaceID, 2), bindTo(install, "C1"))
		assert.Equal(t, model.SlackFeedRefusal{Reason: model.SlackFeedInUse, ConnectorID: c.ID, WorkspaceID: f.workspaceID}, err)
		_, err = f.db.GetConnector(ctx, f.workspaceID, f.appConnector(f.workspaceID, 2).ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a refusal writes nothing")
	})
}

func TestSlackAppConnectorRefusals(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "refuse")
	refused := func(t *testing.T, n int, bind model.SlackFeedBind) model.SlackFeedRefusal {
		t.Helper()
		c := f.appConnector(f.workspaceID, n)
		err := f.db.CreateSlackAppConnector(ctx, c, bind)
		var r model.SlackFeedRefusal
		require.ErrorAs(t, err, &r)
		_, err = f.db.GetConnector(ctx, f.workspaceID, c.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "a refusal writes nothing")
		assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_feeds WHERE connector_id=?`, c.ID))
		return r
	}
	none := &model.SlackInstall{ID: f.installID(9)}
	assert.Equal(t, model.SlackFeedNotInstalled, refused(t, 1, bindTo(none)).Reason, "no install")

	install := f.installed(t)
	assert.Equal(t, model.SlackFeedNotInstalled, refused(t, 1, bindTo(none)).Reason, "another install than the channels were read through")
	assert.Equal(t, model.SlackFeedScopes, refused(t, 1, bindTo(install)).Reason)
	bind := bindTo(install)
	bind.Scopes = []string{"chat:write"}
	assert.Equal(t, model.SlackFeedAdmin, refused(t, 1, bind).Reason)
	require.NoError(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{InstallerAdmin: ptr(true)}))
	assert.Equal(t, model.SlackFeedBotUnknown, refused(t, 1, bind).Reason)
	require.NoError(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{BotID: ptr("B1")}))
	require.NoError(t, f.db.CreateSlackAppConnector(ctx, f.appConnector(f.workspaceID, 1), bind))

	t.Run("invalid", func(t *testing.T) {
		for _, bad := range []func(*model.SlackFeedBind){
			func(b *model.SlackFeedBind) { b.InstallID = "" },
			func(b *model.SlackFeedBind) { b.BoundBy = "" },
			func(b *model.SlackFeedBind) { b.NoticeVersion = 0 },
			func(b *model.SlackFeedBind) { b.HistoryDays = 0 },
			func(b *model.SlackFeedBind) { b.HistoryDays = 91 },
			func(b *model.SlackFeedBind) { b.Channels = []string{"C1", "C1"} },
			func(b *model.SlackFeedBind) { b.Channels = []string{"C 1"} },
		} {
			b := bindTo(install, "C1")
			bad(&b)
			assert.Error(t, f.db.CreateSlackAppConnector(ctx, f.appConnector(f.workspaceID, 3), b))
		}
		other := f.appConnector(f.workspaceID, 3)
		other.Kind = "stripe"
		assert.Error(t, f.db.CreateSlackAppConnector(ctx, other, bindTo(install, "C1")))
		secret := f.appConnector(f.workspaceID, 3)
		secret.SecretCiphertext = []byte("xoxb")
		assert.Error(t, f.db.CreateSlackAppConnector(ctx, secret, bindTo(install, "C1")), "a token connector isn't made here")
	})
}

// A second feed in one Enterprise Grid org is refused, in any workspace,
// even when both commit at once.
func TestSlackAppConnectorEnterprise(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "grid")
	other := id26("SWS-grid-other")
	createWorkspace(t, other)
	mine := f.feedInstall(t, 1, f.workspaceID, "T1", "E1")
	theirs := f.feedInstall(t, 2, other, "T2", "E1")
	c, _ := f.bound(t, 1, mine, "C1")
	err := f.db.CreateSlackAppConnector(ctx, f.appConnector(other, 2), bindTo(theirs, "C9"))
	assert.Equal(t, model.SlackFeedRefusal{Reason: model.SlackFeedEnterprise, ConnectorID: c.ID, WorkspaceID: f.workspaceID}, err)

	// Unbound, the org is free.
	require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, c.ID))
	require.NoError(t, f.db.CreateSlackAppConnector(ctx, f.appConnector(other, 2), bindTo(theirs, "C9")))

	// Two workspaces of another org at once: one wins, the other is refused.
	third := id26("SWS-grid-third")
	createWorkspace(t, third)
	a := f.feedInstall(t, 3, third, "T3", "E2")
	fourth := id26("SWS-grid-fourth")
	createWorkspace(t, fourth)
	b := f.feedInstall(t, 4, fourth, "T4", "E2")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, in := range []struct {
		ws      string
		install *model.SlackInstall
	}{{third, a}, {fourth, b}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.db.CreateSlackAppConnector(ctx, f.appConnector(in.ws, 10+i), bindTo(in.install, "C1"))
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		var r model.SlackFeedRefusal
		if assert.ErrorAs(t, err, &r) {
			assert.Equal(t, model.SlackFeedEnterprise, r.Reason)
		}
	}
	assert.Equal(t, 1, ok, "one feed in the org")
}

func TestSlackAppConnectorSwitch(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "switch")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	token := &model.Connector{ID: id26("SAC-switch-token"), WorkspaceID: f.workspaceID, Kind: model.ConnectorKindSlack, Name: "Slack token",
		Config: json.RawMessage(`{}`), SecretCiphertext: []byte("sealed"), SecretKeyID: "k1"}
	require.NoError(t, f.db.CreateConnector(ctx, token))
	dbExec(t, `UPDATE connectors SET change_cursor='{"at":1}' WHERE id=?`, token.ID)
	config := func(s string) model.ConnectorUpdate {
		return model.ConnectorUpdate{Config: json.RawMessage(s), SecretCiphertext: []byte("sealed"), SecretKeyID: "k1"}
	}
	read := func() (*model.Connector, *model.SlackFeed) {
		c, err := f.db.GetConnector(ctx, f.workspaceID, token.ID)
		require.NoError(t, err)
		feed, err := f.db.GetFoundrySlackFeed(ctx, token.ID)
		if err != nil {
			require.ErrorIs(t, err, store.ErrNotFound)
			return c, nil
		}
		return c, feed
	}

	// Staged: bound, still syncing by its token with its cursor.
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, config(`{"staged":true}`),
		model.SlackAppBindStaged{Bind: bindTo(install, "C1")}))
	c, staged := read()
	require.NotNil(t, staged)
	assert.False(t, c.Pushed, "staged: syncs by its token")
	assert.JSONEq(t, `{"at":1}`, string(c.ChangeCursor), "the token's cursor stays")
	assert.JSONEq(t, `{"staged":true}`, string(c.Config))

	// The flip: the same feed, pushed, read in full.
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, config(`{}`), model.SlackAppBindNow{Bind: bindTo(install, "C1")}))
	c, flipped := read()
	assert.Equal(t, staged.ID, flipped.ID, "the flip keeps the feed the backfill filled")
	assert.True(t, c.Pushed)
	assert.Nil(t, c.ChangeCursor)
	assert.Error(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{},
		model.SlackAppBindStaged{Bind: bindTo(install, "C1")}), "a connector reading through the app isn't staged")

	// Back to a token: the feed is unbound, its work deleted.
	dbExec(t, `UPDATE connectors SET change_cursor='{"seq":3}' WHERE id=?`, token.ID)
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, config(`{}`), model.SlackAppUnbind{}))
	c, none := read()
	assert.Nil(t, none)
	assert.False(t, c.Pushed)
	assert.Nil(t, c.ChangeCursor)
	assert.True(t, dbTrue(t, `SELECT deleting_at IS NOT NULL FROM foundry_slack_feeds WHERE id=?`, flipped.ID))
	for _, table := range []string{"foundry_slack_feed_channels", "foundry_slack_backfill"} {
		assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM `+table+` WHERE feed_id=?`, flipped.ID), table)
	}
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{}, model.SlackAppUnbind{}),
		"unbinding a token connector changes nothing")

	// Bound again at once, beside the feed still being deleted.
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{}, model.SlackAppBindNow{Bind: bindTo(install, "C1")}))
	_, again := read()
	require.NotNil(t, again)
	assert.NotEqual(t, flipped.ID, again.ID)

	// Another team: the old feed goes, a new one binds.
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, install.ID, true))
	t2 := f.feedInstall(t, 2, f.workspaceID, "T2", "")
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{}, model.SlackAppBindNow{Bind: bindTo(t2, "C7")}))
	_, other := read()
	assert.Equal(t, "T2", other.TeamID)
	assert.True(t, dbTrue(t, `SELECT deleting_at IS NOT NULL FROM foundry_slack_feeds WHERE id=?`, again.ID))

	t.Run("refused and bad", func(t *testing.T) {
		err := f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{}, model.SlackAppBindNow{Bind: bindTo(install, "C1")})
		assert.Equal(t, model.SlackFeedRefusal{Reason: model.SlackFeedNotInstalled}, err)
		_, kept := read()
		assert.Equal(t, other.ID, kept.ID, "a refusal leaves the feed it had")
		assert.ErrorIs(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, id26("SAC-nope"), model.ConnectorUpdate{}, model.SlackAppUnbind{}),
			store.ErrNotFound)
		db := scheduledConnector(t, f.db, f.workspaceID, "switch-db", nil)
		assert.Error(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, db.ID, model.ConnectorUpdate{}, model.SlackAppUnbind{}), "not Slack")
		assert.Error(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{}, nil))
		assert.Error(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{},
			model.SlackAppBindNow{Bind: model.SlackFeedBind{}}))
	})

	t.Run("delete marks the feed", func(t *testing.T) {
		require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, token.ID))
		assert.True(t, dbTrue(t, `SELECT deleting_at IS NOT NULL FROM foundry_slack_feeds WHERE id=?`, other.ID))
		_, err := f.db.GetFoundrySlackFeed(ctx, token.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestFoundrySlackFeedAppend(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "append")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

	got, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev0", day)}, At: day},
		feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackFeedAppended{Outcome: model.SlackAppendUnbound}, got, "no feed reads the install")

	c, feed := f.bound(t, 1, install, "C1", "C2")
	got, err = f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{
		Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day), feedRow("C2", "Ev2", day)}, At: day}, feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackFeedAppended{Outcome: model.SlackAppended, FeedID: feed.ID, Rows: 2}, got)
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.NextSyncAt, "a pushed connector with no sync due is due")
	assert.WithinDuration(t, time.Now().Add(2*time.Minute), *stored.NextSyncAt, 10*time.Second)
	dbExec(t, `UPDATE connectors SET next_sync_at=NOW(6) + INTERVAL 1 HOUR WHERE id=?`, c.ID)

	got, err = f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day)}, At: day},
		feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppendDuplicate, got.Outcome, "Slack's retry")
	got, err = f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{
		Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day), feedRow("C1", "Ev3", day)}, At: day}, feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackFeedAppended{Outcome: model.SlackAppended, FeedID: feed.ID, Rows: 1}, got)
	again, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(time.Hour), *again.NextSyncAt, 10*time.Second, "a sync due already isn't moved")

	rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20})
	require.NoError(t, err)
	var keys []string
	var seqs []int64
	for _, r := range rows {
		keys, seqs = append(keys, r.DedupeKey), append(seqs, r.Seq)
		assert.True(t, r.Day.Equal(day))
		assert.Equal(t, model.SlackRowMessage, r.Kind)
		assert.Equal(t, model.SlackFeedFromEvent, r.Source)
		assert.JSONEq(t, `{"ts":"1.2","text":"hi"}`, string(r.Payload))
	}
	assert.Equal(t, []string{"e:Ev1", "e:Ev2", "e:Ev3"}, keys)
	assert.Equal(t, []int64{1, 2, 5}, seqs, "a repeat leaves its number unused")

	t.Run("full", func(t *testing.T) {
		at := day.Add(90 * time.Minute)
		// C2's message was posted hours before it was appended (a relay held
		// it): its gap is of the minute it was posted. C1's second is of a
		// message older than any window, which needs none; C9 isn't covered.
		late := postedRow("C2", "Ev6", at.Add(-3*time.Hour+5*time.Second))
		got, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{
			Rows: []model.SlackFeedRow{postedRow("C1", "Ev4", at.Add(5*time.Second)), feedRow("C1", "Ev5", day), late, feedRow("C9", "Ev7", day)},
			At:   at.Add(17 * time.Second)}, model.SlackFeedLimits{Spacing: time.Minute, MaxRows: 5})
		require.NoError(t, err)
		assert.Equal(t, model.SlackFeedAppended{Outcome: model.SlackAppendFull, FeedID: feed.ID, Rows: 2}, got,
			"a gap of the minute for each covered channel, in the batch's place")
		rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 5, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20})
		require.NoError(t, err)
		require.Len(t, rows, 2)
		for i, w := range []struct {
			channel string
			minute  time.Time
		}{{"C1", at}, {"C2", at.Add(-3 * time.Hour)}} {
			assert.Equal(t, model.SlackRowGap, rows[i].Kind)
			assert.Equal(t, w.channel, rows[i].ChannelID)
			assert.JSONEq(t, fmt.Sprintf(`{"from":%q,"to":%q}`, w.minute.Format(time.RFC3339Nano), w.minute.Add(time.Minute).Format(time.RFC3339Nano)),
				string(rows[i].Payload))
		}
		var w struct{ oldest, latest time.Time }
		require.NoError(t, testDB.QueryRow(`SELECT oldest, latest FROM foundry_slack_backfill WHERE feed_id=? AND channel_id='C1' AND reason='gap'`,
			feed.ID).Scan(&w.oldest, &w.latest))
		assert.True(t, w.oldest.Equal(at) && w.latest.Equal(at.Add(time.Minute)), "a gap window reads the minute again")
	})

	t.Run("revoked", func(t *testing.T) {
		require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, install.ID, false))
		got, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev8", day)}, At: day},
			feedLimits)
		require.NoError(t, err)
		assert.Equal(t, model.SlackAppendUnbound, got.Outcome, "a removed install's events stop")
		live, err := f.db.GetFoundrySlackFeed(ctx, c.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(7), live.LastSeq, "the rows appended before stay")
	})

	t.Run("bad", func(t *testing.T) {
		good := model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev9", day)}, At: day}
		for _, bad := range []struct {
			batch  model.SlackFeedBatch
			limits model.SlackFeedLimits
		}{
			{model.SlackFeedBatch{At: day}, feedLimits},
			{model.SlackFeedBatch{Rows: good.Rows}, feedLimits},
			{good, model.SlackFeedLimits{MaxRows: 1}},
			{good, model.SlackFeedLimits{Spacing: time.Minute}},
			{model.SlackFeedBatch{Rows: []model.SlackFeedRow{{DedupeKey: "x"}}, At: day}, feedLimits},
		} {
			_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, bad.batch, bad.limits)
			assert.Error(t, err)
		}
	})
}

// A connector a permanent failure paused stays paused while events arrive:
// no sync is queued to fail again. A reinstall, or its owner's edit, lifts it.
func TestFoundrySlackFeedAppendPaused(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "paused")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, _ := f.bound(t, 1, install, "C1")
	dbExec(t, `UPDATE connectors SET next_sync_at=NULL, schedule_paused='Slack refused the scopes' WHERE id=?`, c.ID)
	day := model.SlackDay(time.Now())
	got, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day)}, At: day}, feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppended, got.Outcome)
	_, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{model.SlackMinuteGap("C1", time.Now())}, time.Minute)
	require.NoError(t, err)
	_, err = f.db.CloseFoundrySlackDays(ctx, mustFeed(t, f.db, c.ID).ID, day, time.Minute)
	require.NoError(t, err)
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.NextSyncAt, "a paused connector isn't due")
}

// An append makes its connector's sync due only after its feed's
// transaction, so a connector's delete or edit, which locks the connector
// and then the feed, never deadlocks with it.
func TestFoundrySlackFeedAppendLockOrder(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "lockorder")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, install, "C1")
	day := model.SlackDay(time.Now())

	tx, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var locked string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? FOR UPDATE`, c.ID).Scan(&locked))
	appended := make(chan error, 1)
	go func() {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day)}, At: day},
			feedLimits)
		appended <- err
	}()
	time.Sleep(500 * time.Millisecond) // the append reaches the connector's lock
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT id FROM foundry_slack_feeds WHERE id=? FOR UPDATE`, feed.ID).Scan(&locked),
		"the connector's holder takes the feed")
	require.NoError(t, tx.Commit())
	require.NoError(t, <-appended)
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.NotNil(t, stored.NextSyncAt, "and the append makes its sync due after")
}

// mustFeed is the connector's live feed.
func mustFeed(t *testing.T, db store.Store, connectorID string) *model.SlackFeed {
	t.Helper()
	feed, err := db.GetFoundrySlackFeed(context.Background(), connectorID)
	require.NoError(t, err)
	return feed
}

// Keeping more days of history reads the days added back: a bound window
// per covered channel, which a staged switch's flip waits for. Fewer adds
// none.
func TestSlackAppConnectorHistoryGrows(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "grows")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c := f.appConnector(f.workspaceID, 1)
	c.Config = json.RawMessage(`{"credential":"helix_app","history_days":30}`)
	bind := bindTo(install, "C1", "C2")
	bind.HistoryDays = 30
	require.NoError(t, f.db.CreateSlackAppConnector(ctx, c, bind))
	feed := mustFeed(t, f.db, c.ID)
	windows := func() []string {
		rows, err := testDB.Query(`SELECT channel_id, reason, ROUND(TIMESTAMPDIFF(SECOND, oldest, NOW(6)) / 86400), ROUND(TIMESTAMPDIFF(SECOND, latest, NOW(6)) / 86400)
FROM foundry_slack_backfill WHERE feed_id=? ORDER BY place, channel_id`, feed.ID)
		require.NoError(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var channel, reason string
			var oldest, latest int
			require.NoError(t, rows.Scan(&channel, &reason, &oldest, &latest))
			out = append(out, fmt.Sprintf("%s %s %d-%d", channel, reason, oldest, latest))
		}
		return out
	}
	require.Equal(t, []string{"C1 bound 30-0", "C2 bound 30-0"}, windows())

	update := func(days int) {
		t.Helper()
		b := bindTo(install)
		b.HistoryDays = days
		require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, c.ID,
			model.ConnectorUpdate{Config: json.RawMessage(fmt.Sprintf(`{"credential":"helix_app","history_days":%d}`, days)), SecretCiphertext: []byte{}},
			model.SlackAppBindNow{Bind: b}))
	}
	update(90)
	assert.Equal(t, []string{"C1 bound 30-0", "C2 bound 30-0", "C1 bound 90-30", "C2 bound 90-30"}, windows())
	assert.Equal(t, feed.ID, mustFeed(t, f.db, c.ID).ID, "the same feed")
	update(60)
	assert.Len(t, windows(), 4, "fewer days read nothing back")
}

// Rows are numbered in the order their appends commit, under the feed's
// lock: concurrent appends leave no number twice and none out.
func TestFoundrySlackFeedAppendConcurrent(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "race")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1")
	day := model.SlackDay(time.Now())
	var wg sync.WaitGroup
	const n = 24
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{
				Rows: []model.SlackFeedRow{feedRow("C1", fmt.Sprintf("Ev%d", i), day)}, At: day}, feedLimits)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, model.SlackFeedBudget{Rows: 100, Bytes: 1 << 20})
	require.NoError(t, err)
	require.Len(t, rows, n)
	for i, r := range rows {
		assert.Equal(t, int64(i+1), r.Seq)
	}
}

func TestFoundrySlackFeedRead(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "read")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1")
	day := model.SlackDay(time.Now())
	for i := 1; i <= 4; i++ {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", fmt.Sprintf("Ev%d", i), day)},
			At: day}, feedLimits)
		require.NoError(t, err)
	}
	size := int64(len(`{"ts":"1.2","text":"hi"}`))
	for _, c := range []struct {
		name   string
		after  int64
		budget model.SlackFeedBudget
		want   []int64
	}{
		{"rows", 0, model.SlackFeedBudget{Rows: 2, Bytes: 1 << 20}, []int64{1, 2}},
		{"bytes", 0, model.SlackFeedBudget{Rows: 10, Bytes: 3 * size}, []int64{1, 2, 3}},
		{"the first, whatever its size", 1, model.SlackFeedBudget{Rows: 10, Bytes: 1}, []int64{2}},
		{"after", 3, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20}, []int64{4}},
		{"none", 4, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20}, nil},
	} {
		rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, c.after, c.budget)
		require.NoError(t, err, c.name)
		var seqs []int64
		for _, r := range rows {
			seqs = append(seqs, r.Seq)
		}
		assert.Equal(t, c.want, seqs, c.name)
	}
	_, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, model.SlackFeedBudget{})
	assert.Error(t, err)
	_, err = f.db.FoundrySlackFeedAfter(ctx, id26("nofeed"), 0, model.SlackFeedBudget{Rows: 1, Bytes: 1})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func slackDay(channel string, day time.Time, key, state string) model.SlackFeedDay {
	return model.SlackFeedDay{SlackDayKey: model.SlackDayKey{ChannelID: channel, Day: day}, ObjectKey: key, Digest: sha(key), Messages: 3, Bytes: 120,
		State: model.SlackDayState(state)}
}

func TestFoundrySlackFold(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "fold")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, install, "C1", "C2")
	d1, d2 := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", fmt.Sprintf("Ev%d", i), d1)},
			At: d1}, feedLimits)
		require.NoError(t, err)
	}

	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 0, To: 2,
		Days: []model.SlackFeedDay{slackDay("C1", d1, "k/c1/d1/2", "partial"), slackDay("C2", d1, "k/c2/d1/2", "complete")}}))
	days, err := f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	require.Len(t, days, 2)
	assert.Equal(t, "C1", days[0].ChannelID)
	assert.Equal(t, int64(2), days[0].UpdatedSeq)
	assert.Equal(t, model.SlackDayComplete, days[1].State)
	assert.True(t, days[1].Day.Equal(d1))
	live, err := f.db.GetFoundrySlackFeed(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), live.FoldedSeq)

	// A fold that lost: nothing written, its objects recorded for the sweep.
	err = f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 0, To: 3,
		Days: []model.SlackFeedDay{slackDay("C1", d1, "k/lost", "partial")}})
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, []string{"k/lost"}, replacedKeys(t, f.db))
	assert.ErrorIs(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 2, To: 9}), store.ErrConflict,
		"past the last row")

	// The next fold replaces C1's object, writes d2 and drops C2's d1.
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 2, To: 3,
		Days:     []model.SlackFeedDay{slackDay("C1", d1, "k/c1/d1/3", "complete"), slackDay("C1", d2, "k/c1/d2/3", "lossy")},
		Replaced: []string{"k/c1/d1/2"}, Dropped: []model.SlackDayKey{{ChannelID: "C2", Day: d1}, {ChannelID: "C2", Day: d2}}}))
	days, err = f.db.FoundrySlackDaysAfter(ctx, feed.ID, 2)
	require.NoError(t, err)
	require.Len(t, days, 2)
	assert.Equal(t, "k/c1/d1/3", days[0].ObjectKey)
	assert.Equal(t, model.SlackDayLossy, days[1].State)
	all, err := f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	assert.Len(t, all, 2, "C2's day is dropped")
	assert.Equal(t, []string{"k/c1/d1/2", "k/c2/d1/2", "k/lost"}, replacedKeys(t, f.db))

	require.NoError(t, f.db.MarkFoundrySlackDaysDocumented(ctx, feed.ID, []model.SlackDayKey{{ChannelID: "C1", Day: d1}}))
	all, err = f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	assert.NotNil(t, all[0].DocumentedAt)
	assert.Nil(t, all[1].DocumentedAt)

	// A day of a channel the feed no longer covers is never written, so a
	// purge that committed while the fold ran stays: its object goes.
	require.NoError(t, f.db.PurgeFoundrySlackChannel(ctx, feed.ID, "C2"))
	_, err = f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev7", d2)}, At: d2},
		feedLimits)
	require.NoError(t, err)
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 3, To: 4,
		Days:     []model.SlackFeedDay{slackDay("C1", d2, "k/c1/d2/4", "complete"), slackDay("C2", d2, "k/c2/d2/4", "partial")},
		Replaced: []string{"k/c1/d2/3"}}))
	all, err = f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, []string{"C1", "C1"}, []string{all[0].ChannelID, all[1].ChannelID}, "no day of C2")
	assert.Subset(t, replacedKeys(t, f.db), []string{"k/c2/d2/4", "k/c1/d2/3"})

	// A fold of no rows drops days alone, under the same compare-and-set.
	assert.ErrorIs(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 3, To: 3,
		Dropped: []model.SlackDayKey{{ChannelID: "C1", Day: d1}}}), store.ErrConflict)
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 4, To: 4,
		Dropped: []model.SlackDayKey{{ChannelID: "C1", Day: d1}}}))
	all, err = f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.True(t, all[0].Day.Equal(d2))
	assert.Contains(t, replacedKeys(t, f.db), "k/c1/d1/3")
	assert.Equal(t, int64(4), mustFeed(t, f.db, c.ID).FoldedSeq)

	// Objects of a fold that never committed go too.
	require.NoError(t, f.db.ForgetFoundrySlackObjects(ctx, feed.ID, []string{"k/never"}))
	require.NoError(t, f.db.ForgetFoundrySlackObjects(ctx, feed.ID, nil))
	require.NoError(t, f.db.ForgetFoundrySlackObjects(ctx, id26("nofeed"), []string{"k/nobody"}))
	assert.Contains(t, replacedKeys(t, f.db), "k/never")
	assert.NotContains(t, replacedKeys(t, f.db), "k/nobody")

	t.Run("bad", func(t *testing.T) {
		for _, bad := range []model.SlackFold{
			{From: 0, To: 1},
			{FeedID: feed.ID, From: 4, To: 4, Days: []model.SlackFeedDay{slackDay("C1", d1, "a", "partial")}, Dropped: []model.SlackDayKey{{ChannelID: "C1", Day: d2}}},
			{FeedID: feed.ID, From: 3, To: 3},
			{FeedID: feed.ID, From: -1, To: 3},
			{FeedID: feed.ID, From: 3, To: 4, Days: []model.SlackFeedDay{slackDay("C1", d1, "a", "partial"), slackDay("C1", d1, "b", "partial")}},
			{FeedID: feed.ID, From: 3, To: 4, Days: []model.SlackFeedDay{slackDay("C1", d1, "a", "unknown")}},
			{FeedID: feed.ID, From: 3, To: 4, Days: []model.SlackFeedDay{slackDay("C1", d1, "a", "partial")},
				Dropped: []model.SlackDayKey{{ChannelID: "C1", Day: d1}}},
			{FeedID: feed.ID, From: 3, To: 4, Dropped: []model.SlackDayKey{{ChannelID: "", Day: d1}}},
		} {
			assert.Error(t, f.db.CommitFoundrySlackFold(ctx, bad))
		}
	})

	t.Run("a deleting feed folds no more", func(t *testing.T) {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev4", d1)}, At: d1},
			feedLimits)
		require.NoError(t, err)
		require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, c.ID))
		assert.ErrorIs(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 4, To: 5}), store.ErrConflict)
	})
}

func TestFoundrySlackFeedPrune(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "prune")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, install, "C1")
	day := model.SlackDay(time.Now())
	for i := 1; i <= 5; i++ {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", fmt.Sprintf("Ev%d", i), day)},
			At: day}, feedLimits)
		require.NoError(t, err)
	}
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 0, To: 3,
		Days: []model.SlackFeedDay{slackDay("C1", day, "k/1", "partial"), slackDay("C1", day.AddDate(0, 0, -1), "k/0", "partial")}}))
	n, err := f.db.PruneFoundrySlackFeed(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "a batch at a time")
	n, err = f.db.PruneFoundrySlackFeed(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20})
	require.NoError(t, err)
	require.Len(t, rows, 2, "rows the mirror doesn't hold yet stay")
	n, err = f.db.PruneFoundrySlackFeed(ctx, 100)
	require.NoError(t, err)
	assert.Zero(t, n)

	// Deleted: its buffer, then its index (objects to the sweep), then itself.
	require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, c.ID))
	n, err = f.db.PruneFoundrySlackFeed(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "the buffer first")
	n, err = f.db.PruneFoundrySlackFeed(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Len(t, replacedKeys(t, f.db), 1, "one day's object, a batch at a time")
	n, err = f.db.PruneFoundrySlackFeed(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "the last day, then the feed")
	assert.Equal(t, []string{"k/0", "k/1"}, replacedKeys(t, f.db))
	assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_feeds WHERE id=?`, feed.ID))
	_, err = f.db.PruneFoundrySlackFeed(ctx, 0)
	assert.Error(t, err)
}

func TestFoundrySlackSlots(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "slots")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")

	// Two replicas at once: one call.
	var wg sync.WaitGroup
	got := make([]bool, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			got[i], err = f.db.ReserveFoundrySlackSlot(ctx, install.ID, model.SlackSlotHistory, time.Minute)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.ElementsMatch(t, []bool{true, false}, got)
	ok, err := f.db.ReserveFoundrySlackSlot(ctx, install.ID, model.SlackSlotReplies, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "each method has its own slot")

	require.NoError(t, f.db.DeferFoundrySlackSlot(ctx, install.ID, model.SlackSlotReplies, time.Hour))
	require.NoError(t, f.db.DeferFoundrySlackSlot(ctx, install.ID, model.SlackSlotReplies, time.Second), "never earlier")
	assert.True(t, dbTrue(t, `SELECT next_at > NOW(6) + INTERVAL 50 MINUTE FROM foundry_slack_slots WHERE install_id=? AND method='replies'`, install.ID))
	dbExec(t, `UPDATE foundry_slack_slots SET next_at=NOW(6) - INTERVAL 1 SECOND WHERE install_id=?`, install.ID)
	ok, err = f.db.ReserveFoundrySlackSlot(ctx, install.ID, model.SlackSlotHistory, time.Minute)
	require.NoError(t, err)
	assert.True(t, ok, "due again")

	// A reinstall keeps them.
	_, reinstalled, err := f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T1"), sealAs("xoxb-2"))
	require.NoError(t, err)
	require.True(t, reinstalled)
	assert.Equal(t, 2, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_slots WHERE install_id=?`, install.ID))

	_, err = f.db.ReserveFoundrySlackSlot(ctx, install.ID, "users", time.Minute)
	assert.Error(t, err)
	_, err = f.db.ReserveFoundrySlackSlot(ctx, install.ID, model.SlackSlotHistory, 0)
	assert.Error(t, err)
	assert.Error(t, f.db.DeferFoundrySlackSlot(ctx, install.ID, "users", time.Minute))
	assert.Error(t, f.db.DeferFoundrySlackSlot(ctx, install.ID, model.SlackSlotHistory, -time.Second))
	assert.ErrorIs(t, f.db.DeferFoundrySlackSlot(ctx, id26("noinstall"), model.SlackSlotHistory, time.Second), store.ErrNotFound)
}

func dueWork(t *testing.T, db store.Store, method model.SlackSlotMethod) []model.SlackBackfillWork {
	t.Helper()
	work, err := db.DueFoundrySlackBackfill(context.Background(), method, 10)
	require.NoError(t, err)
	return work
}

func TestFoundrySlackBackfill(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "backfill")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, install, "C2", "C1")

	// Gaps first, then joined ones newest first, then bound ones in order.
	joinedAt := feed.BoundAt.Add(time.Hour)
	n, err := f.db.JoinFoundrySlackChannels(ctx, feed.ID, []model.SlackChannelJoin{
		{ChannelID: "C3", CoveredFrom: joinedAt, Oldest: joinedAt.Add(-24 * time.Hour)},
		{ChannelID: "C4", CoveredFrom: joinedAt.Add(time.Hour), Oldest: joinedAt.Add(-24 * time.Hour)},
		{ChannelID: "C1", CoveredFrom: joinedAt, Oldest: joinedAt.Add(-24 * time.Hour)},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n, "a channel covered already is left as it is")
	next := func() *model.SlackBackfillWindow {
		t.Helper()
		work := dueWork(t, f.db, model.SlackSlotHistory)
		require.Len(t, work, 1)
		assert.Equal(t, feed.ID, work[0].Feed.ID)
		w, ok := work[0].Item.(*model.SlackBackfillWindow)
		require.True(t, ok)
		return w
	}
	assert.Equal(t, "C4", next().ChannelID, "joined, newest first")
	_, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{model.SlackMinuteGap("C2", joinedAt)}, time.Minute)
	require.NoError(t, err)
	gap := next()
	assert.Equal(t, model.SlackBackfillGap, gap.Reason)
	assert.Equal(t, "C2", gap.ChannelID)
	require.NoError(t, f.db.EndFoundrySlackBackfill(ctx, *gap, model.SlackBackfillCancelled, "past the gap budget"))
	assert.Equal(t, "C4", next().ChannelID)
	for _, ch := range []string{"C4", "C3"} {
		w := next()
		require.Equal(t, ch, w.ChannelID)
		require.NoError(t, f.db.EndFoundrySlackBackfill(ctx, *w, model.SlackBackfillCancelled, "not_in_channel"))
	}
	w := next()
	assert.Equal(t, "C2", w.ChannelID, "bound, in the bind's order")
	assert.Equal(t, model.SlackBackfillBound, w.Reason)

	// A page: its rows, progress and threads in one go.
	pageEnd := w.Latest.Add(-time.Hour)
	day := model.SlackDay(pageEnd)
	row := model.SlackFeedRow{DedupeKey: model.SlackBackfillDedupe("C2", "1.1", "", model.SlackRowMessage), ChannelID: "C2", Day: day,
		Kind: model.SlackRowMessage, Source: model.SlackFeedFromBackfill, Payload: json.RawMessage(`{"ts":"1.1"}`)}
	thread := model.SlackBackfillThread{FeedID: feed.ID, ChannelID: "C2", ThreadTS: "1.1", Day: day}
	step := model.SlackHistoryStep{Window: *w, Latest: pageEnd, Cursor: "dXNlcjpVMDYx", Rows: []model.SlackFeedRow{row},
		Threads: []model.SlackBackfillThread{thread, thread}, Messages: 1, CoveredS: 3600}
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, step, time.Minute))
	assert.ErrorIs(t, f.db.AdvanceFoundrySlackBackfill(ctx, step, time.Minute), store.ErrConflict, "a page written once")
	after := next()
	assert.Equal(t, model.SlackBackfillRunning, after.Status)
	assert.True(t, after.Latest.Equal(pageEnd))
	assert.Equal(t, "dXNlcjpVMDYx", after.Cursor)
	assert.Equal(t, 1, after.Pages)
	assert.Equal(t, 1, after.Messages)
	assert.Equal(t, 1, after.Threads, "a thread is counted once")
	assert.Equal(t, int64(3600), after.CoveredS)
	rows, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, model.SlackFeedFromBackfill, rows[len(rows)-1].Source)
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.NotNil(t, stored.NextSyncAt)

	done := model.SlackHistoryStep{Window: *after, Latest: after.Oldest, Done: true}
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, done, time.Minute))
	assert.Equal(t, "C1", next().ChannelID)

	// Replies: the thread found, newest day first.
	older := model.SlackBackfillThread{FeedID: feed.ID, ChannelID: "C1", ThreadTS: "0.9", Day: day.AddDate(0, 0, -3)}
	queued, err := f.db.QueueFoundrySlackThreads(ctx, []model.SlackBackfillThread{older})
	require.NoError(t, err)
	assert.Equal(t, 1, queued)
	work := dueWork(t, f.db, model.SlackSlotReplies)
	require.Len(t, work, 1)
	th, ok := work[0].Item.(*model.SlackBackfillThread)
	require.True(t, ok)
	assert.Equal(t, "1.1", th.ThreadTS)
	reply := model.SlackRepliesStep{Thread: *th, Cursor: "next", Status: model.SlackThreadQueued}
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, reply, time.Minute))
	assert.ErrorIs(t, f.db.AdvanceFoundrySlackBackfill(ctx, reply, time.Minute), store.ErrConflict)
	th = dueWork(t, f.db, model.SlackSlotReplies)[0].Item.(*model.SlackBackfillThread)
	assert.Equal(t, "next", th.Cursor)
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, model.SlackRepliesStep{Thread: *th, Status: model.SlackThreadDone}, time.Minute))
	th = dueWork(t, f.db, model.SlackSlotReplies)[0].Item.(*model.SlackBackfillThread)
	assert.Equal(t, "0.9", th.ThreadTS)
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, model.SlackRepliesStep{Thread: *th, Status: model.SlackThreadGone}, time.Minute))
	assert.Empty(t, dueWork(t, f.db, model.SlackSlotReplies))
	// A gap's look back reads a done thread again.
	queued, err = f.db.QueueFoundrySlackThreads(ctx, []model.SlackBackfillThread{older})
	require.NoError(t, err)
	assert.Equal(t, 1, queued)
	assert.Len(t, dueWork(t, f.db, model.SlackSlotReplies), 1)

	progress, err := f.db.FoundrySlackBackfillProgress(ctx, feed.ID)
	require.NoError(t, err)
	assert.Len(t, progress.Windows, 4, "bound and joined windows")
	assert.Equal(t, 1, progress.QueuedThreads)

	t.Run("failures", func(t *testing.T) {
		w := next()
		status, err := f.db.FailFoundrySlackBackfill(ctx, *w, "internal_error", 2)
		require.NoError(t, err)
		assert.Equal(t, model.SlackBackfillQueued, status)
		status, err = f.db.FailFoundrySlackBackfill(ctx, *w, strings.Repeat("é", 300), 2)
		require.NoError(t, err)
		assert.Equal(t, model.SlackBackfillFailed, status, "after its attempts")
		_, err = f.db.FailFoundrySlackBackfill(ctx, *w, "again", 2)
		assert.ErrorIs(t, err, store.ErrNotFound, "a failed window is done with")
		_, err = f.db.FailFoundrySlackBackfill(ctx, *w, "x", 0)
		assert.Error(t, err)
		assert.ErrorIs(t, f.db.EndFoundrySlackBackfill(ctx, *w, model.SlackBackfillCancelled, ""), store.ErrNotFound)
		assert.Error(t, f.db.EndFoundrySlackBackfill(ctx, *w, model.SlackBackfillDone, ""))
		assert.Empty(t, dueWork(t, f.db, model.SlackSlotHistory), "nothing left to read")
	})

	t.Run("bad steps", func(t *testing.T) {
		w := model.SlackBackfillWindow{FeedID: feed.ID, ChannelID: "C1", Oldest: day, Latest: day.Add(time.Hour)}
		for _, bad := range []model.SlackBackfillStep{
			nil,
			model.SlackHistoryStep{Window: w, Latest: day.Add(2 * time.Hour)},
			model.SlackHistoryStep{Window: w, Latest: day.Add(-time.Hour)},
			model.SlackHistoryStep{Window: w, Latest: day, Done: true, Cursor: "c"},
			model.SlackHistoryStep{Window: w, Latest: day, Messages: -1},
			model.SlackHistoryStep{Window: w, Latest: day, Threads: []model.SlackBackfillThread{{FeedID: "other", ChannelID: "C1", ThreadTS: "1.1", Day: day}}},
			model.SlackHistoryStep{Window: w, Latest: day, Rows: []model.SlackFeedRow{{}}},
			model.SlackRepliesStep{Thread: model.SlackBackfillThread{FeedID: feed.ID}, Status: "lost"},
			model.SlackRepliesStep{Thread: model.SlackBackfillThread{FeedID: feed.ID}, Status: model.SlackThreadDone, Cursor: "c"},
		} {
			assert.Error(t, f.db.AdvanceFoundrySlackBackfill(ctx, bad, time.Minute))
		}
		assert.Error(t, f.db.AdvanceFoundrySlackBackfill(ctx, model.SlackHistoryStep{Window: w, Latest: day}, 0))
		_, err := f.db.QueueFoundrySlackThreads(ctx, []model.SlackBackfillThread{{FeedID: feed.ID, ChannelID: "C1", ThreadTS: "x", Day: day}})
		assert.Error(t, err)
		_, err = f.db.DueFoundrySlackBackfill(ctx, "users", 1)
		assert.Error(t, err)
	})

	t.Run("a deleted feed's work stops", func(t *testing.T) {
		th := dueWork(t, f.db, model.SlackSlotReplies)[0].Item.(*model.SlackBackfillThread)
		require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, c.ID))
		assert.ErrorIs(t, f.db.AdvanceFoundrySlackBackfill(ctx, model.SlackRepliesStep{Thread: *th, Status: model.SlackThreadDone}, time.Minute),
			store.ErrNotFound)
		_, err := f.db.FoundrySlackBackfillProgress(ctx, feed.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.JoinFoundrySlackChannels(ctx, feed.ID, nil)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

// The due-work query skips installs whose slot isn't due, removed installs
// and feeds with nothing to read.
func TestFoundrySlackBackfillDue(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "due")
	other := id26("SWS-due-other")
	createWorkspace(t, other)
	idle := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	busy := f.feedInstall(t, 2, other, "T2", "")
	_, idleFeed := f.bound(t, 1, idle, "C1")
	busyConnector := f.appConnector(other, 2)
	require.NoError(t, f.db.CreateSlackAppConnector(ctx, busyConnector, bindTo(busy, "C1")))
	busyFeed, err := f.db.GetFoundrySlackFeed(ctx, busyConnector.ID)
	require.NoError(t, err)

	dbExec(t, `UPDATE foundry_slack_slots SET next_at=NOW(6) - INTERVAL 1 MINUTE WHERE install_id=?`, busy.ID)
	dbExec(t, `UPDATE foundry_slack_slots SET next_at=NOW(6) - INTERVAL 2 MINUTE WHERE install_id=?`, idle.ID)
	work := dueWork(t, f.db, model.SlackSlotHistory)
	require.Len(t, work, 2)
	assert.Equal(t, idleFeed.ID, work[0].Feed.ID, "the longest due first")

	ok, err := f.db.ReserveFoundrySlackSlot(ctx, idle.ID, model.SlackSlotHistory, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	work = dueWork(t, f.db, model.SlackSlotHistory)
	require.Len(t, work, 1, "an install whose slot isn't due")
	assert.Equal(t, busyFeed.ID, work[0].Feed.ID)
	assert.Empty(t, dueWork(t, f.db, model.SlackSlotReplies), "no thread to read")

	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, other, busy.ID, true))
	assert.Empty(t, dueWork(t, f.db, model.SlackSlotHistory), "a removed install")
}

func TestFoundrySlackGaps(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "gaps")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1", "C2")
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	gapWindows := func(channel string) [][2]time.Time {
		t.Helper()
		rows, err := testDB.Query(`SELECT oldest, latest FROM foundry_slack_backfill WHERE feed_id=? AND channel_id=? AND reason='gap' ORDER BY oldest`,
			feed.ID, channel)
		require.NoError(t, err)
		defer rows.Close()
		var out [][2]time.Time
		for rows.Next() {
			var w [2]time.Time
			require.NoError(t, rows.Scan(&w[0], &w[1]))
			out = append(out, [2]time.Time{w[0].UTC(), w[1].UTC()})
		}
		return out
	}

	got, err := f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{
		model.SlackMinuteGap("C1", at), model.SlackMinuteGap("C1", at.Add(2*time.Minute)), model.SlackMinuteGap("C9", at)}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, model.SlackFeedAppended{Outcome: model.SlackAppended, FeedID: feed.ID, Rows: 2}, got, "a channel it doesn't cover has none")
	assert.Equal(t, [][2]time.Time{{at, at.Add(time.Minute)}, {at.Add(2 * time.Minute), at.Add(3 * time.Minute)}}, gapWindows("C1"))

	// A gap that meets both merges them.
	got, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{model.SlackMinuteGap("C1", at.Add(time.Minute))}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Rows)
	assert.Equal(t, [][2]time.Time{{at, at.Add(3 * time.Minute)}}, gapWindows("C1"))
	got, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{model.SlackMinuteGap("C1", at.Add(time.Minute))}, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppendDuplicate, got.Outcome, "a gap recorded twice")

	// A window a page has read isn't merged into; one with the same oldest
	// is read again from its newest end.
	w := dueWork(t, f.db, model.SlackSlotHistory)[0].Item.(*model.SlackBackfillWindow)
	require.Equal(t, model.SlackBackfillGap, w.Reason)
	require.NoError(t, f.db.AdvanceFoundrySlackBackfill(ctx, model.SlackHistoryStep{Window: *w, Latest: at.Add(2 * time.Minute), Cursor: "c"},
		time.Minute))
	_, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{{ChannelID: "C1", From: at, To: at.Add(5 * time.Minute)}},
		time.Minute)
	require.NoError(t, err)
	assert.Equal(t, [][2]time.Time{{at, at.Add(5 * time.Minute)}}, gapWindows("C1"))
	w = dueWork(t, f.db, model.SlackSlotHistory)[0].Item.(*model.SlackBackfillWindow)
	assert.Empty(t, w.Cursor, "read again from the top")
	assert.Equal(t, model.SlackBackfillRunning, w.Status)

	// Every covered channel.
	limited := at.Add(time.Hour + 30*time.Second)
	got, err = f.db.RecordFoundrySlackEventsLimited(ctx, install.ID, limited, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, got.Rows)
	assert.Len(t, gapWindows("C2"), 1)
	stored, err := f.db.GetFoundrySlackInstallByID(ctx, install.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.EventsLimitedAt)
	assert.True(t, stored.EventsLimitedAt.Equal(limited.Truncate(time.Minute)))
	_, err = f.db.RecordFoundrySlackEventsLimited(ctx, install.ID, at, time.Minute)
	require.NoError(t, err)
	stored, err = f.db.GetFoundrySlackInstallByID(ctx, install.ID)
	require.NoError(t, err)
	assert.True(t, stored.EventsLimitedAt.Equal(limited.Truncate(time.Minute)), "never moves back")

	t.Run("bad and unbound", func(t *testing.T) {
		_, err := f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, nil, time.Minute)
		assert.Error(t, err)
		_, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{{ChannelID: "C1", From: at, To: at}}, time.Minute)
		assert.Error(t, err)
		_, err = f.db.AppendFoundrySlackFeedGaps(ctx, install.ID, []model.SlackFeedGap{model.SlackMinuteGap("C 1", at)}, time.Minute)
		assert.Error(t, err)
		_, err = f.db.RecordFoundrySlackEventsLimited(ctx, install.ID, time.Time{}, time.Minute)
		assert.Error(t, err)
		_, err = f.db.RecordFoundrySlackEventsLimited(ctx, id26("noinstall"), at, time.Minute)
		assert.ErrorIs(t, err, store.ErrNotFound)
		got, err := f.db.AppendFoundrySlackFeedGaps(ctx, id26("noinstall"), []model.SlackFeedGap{model.SlackMinuteGap("C1", at)}, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, model.SlackAppendUnbound, got.Outcome)
	})
}

func TestFoundrySlackGapBudget(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "budget")
	feedID := id26("FEED-budget")
	var wg sync.WaitGroup
	spent := make([]bool, 5)
	for i := range spent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			spent[i], err = f.db.SpendFoundrySlackGap(ctx, feedID, 3)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	n := 0
	for _, s := range spent {
		if s {
			n++
		}
	}
	assert.Equal(t, 3, n, "at most the day's budget")
	ok, err := f.db.SpendFoundrySlackGap(ctx, feedID, 3)
	require.NoError(t, err)
	assert.False(t, ok)
	ok, err = f.db.SpendFoundrySlackGap(ctx, feedID, 0)
	require.NoError(t, err)
	assert.False(t, ok, "no budget")
	dbExec(t, `UPDATE foundry_slack_gap_budget SET day=DATE(NOW(6)) - INTERVAL 1 DAY WHERE feed_id=?`, feedID)
	ok, err = f.db.SpendFoundrySlackGap(ctx, feedID, 3)
	require.NoError(t, err)
	assert.True(t, ok, "a new day")
}

// A reinstall after a removal (a new install id) takes the team's feeds
// over: a gap from the removal to now, and the connector syncs again.
func TestFoundrySlackReinstallRepointsFeed(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "repoint")
	old := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, old, "C1", "C2")
	dbExec(t, `UPDATE connectors SET schedule_paused='Helix Foundry was removed', next_sync_at=NULL WHERE id=?`, c.ID)
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, old.ID, true))
	dbExec(t, `UPDATE foundry_slack_installs SET revoked_at=NOW(6) - INTERVAL 2 HOUR WHERE id=?`, old.ID)

	// A Slack team installed for the workspace meanwhile reads nothing of it.
	other, _, err := f.db.InstallFoundrySlack(ctx, f.install(2, f.workspaceID, "T9"), sealAs("x"))
	require.NoError(t, err)
	live, err := f.db.GetFoundrySlackFeed(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, old.ID, live.InstallID)
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, other.ID, true))

	fresh := f.feedInstall(t, 3, f.workspaceID, "T1", "")
	require.NotEqual(t, old.ID, fresh.ID)
	live, err = f.db.GetFoundrySlackFeed(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, fresh.ID, live.InstallID)
	assert.Equal(t, feed.ID, live.ID)
	assert.Equal(t, int64(2), live.LastSeq, "a gap row per covered channel")
	assert.Equal(t, 2, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_backfill WHERE feed_id=? AND reason='gap'
  AND oldest < NOW(6) - INTERVAL 119 MINUTE AND latest > NOW(6) - INTERVAL 1 MINUTE`, feed.ID), "from the removal to now")
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.SchedulePaused)
	require.NotNil(t, stored.NextSyncAt)
	got, err := f.db.AppendFoundrySlackFeed(ctx, fresh.ID, model.SlackFeedBatch{
		Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", time.Now())}, At: time.Now()}, feedLimits)
	require.NoError(t, err)
	assert.Equal(t, model.SlackAppended, got.Outcome, "events reach it again")

	orphans, err := f.db.ListFoundrySlackOrphans(ctx, time.Hour, 10)
	require.NoError(t, err)
	assert.Empty(t, orphans)
}

func TestFoundrySlackOrphans(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "orphan")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, _ := f.bound(t, 1, install, "C1")
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, install.ID, true))
	orphans, err := f.db.ListFoundrySlackOrphans(ctx, time.Hour, 10)
	require.NoError(t, err)
	assert.Empty(t, orphans, "within the retention")
	dbExec(t, `UPDATE foundry_slack_installs SET revoked_at=NOW(6) - INTERVAL 2 HOUR WHERE id=?`, install.ID)
	orphans, err = f.db.ListFoundrySlackOrphans(ctx, time.Hour, 10)
	require.NoError(t, err)
	require.Len(t, orphans, 1)
	assert.Equal(t, c.ID, orphans[0].ConnectorID)
	require.NoError(t, f.db.DeleteConnector(ctx, f.workspaceID, c.ID))
	orphans, err = f.db.ListFoundrySlackOrphans(ctx, time.Hour, 10)
	require.NoError(t, err)
	assert.Empty(t, orphans, "deleting")
	_, err = f.db.ListFoundrySlackOrphans(ctx, 0, 10)
	assert.Error(t, err)
}

func TestFoundrySlackChannels(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "channels")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1", "C2")
	day := model.SlackDay(time.Now())
	_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", "Ev1", day)}, At: day},
		feedLimits)
	require.NoError(t, err)
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 0, To: 1,
		Days: []model.SlackFeedDay{slackDay("C1", day, "k/c1", "partial"), slackDay("C2", day, "k/c2", "partial")}}))
	_, err = f.db.QueueFoundrySlackThreads(ctx, []model.SlackBackfillThread{{FeedID: feed.ID, ChannelID: "C1", ThreadTS: "1.1", Day: day}})
	require.NoError(t, err)

	require.NoError(t, f.db.PurgeFoundrySlackChannel(ctx, feed.ID, "C1"))
	channels, err := f.db.ListFoundrySlackFeedChannels(ctx, feed.ID)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	assert.Equal(t, "C2", channels[0].ChannelID)
	days, err := f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	require.NoError(t, err)
	require.Len(t, days, 1)
	assert.Equal(t, []string{"k/c1"}, replacedKeys(t, f.db))
	for _, table := range []string{"foundry_slack_backfill", "foundry_slack_backfill_threads"} {
		assert.Zero(t, dbCount(t, `SELECT COUNT(*) FROM `+table+` WHERE feed_id=? AND channel_id='C1'`, feed.ID), table)
	}

	// Joined again later: covered from then, with a window of its own.
	now := time.Now().UTC().Truncate(time.Microsecond)
	n, err := f.db.JoinFoundrySlackChannels(ctx, feed.ID, []model.SlackChannelJoin{{ChannelID: "C1", CoveredFrom: now, Oldest: now.Add(-time.Hour)}})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, dbCount(t, `SELECT COUNT(*) FROM foundry_slack_backfill WHERE feed_id=? AND channel_id='C1' AND reason='joined'`, feed.ID))
	_, err = f.db.JoinFoundrySlackChannels(ctx, feed.ID, []model.SlackChannelJoin{{ChannelID: "C5", CoveredFrom: now, Oldest: now}})
	assert.Error(t, err)

	assert.ErrorIs(t, f.db.PurgeFoundrySlackChannel(ctx, id26("nofeed"), "C1"), store.ErrNotFound)
}

func TestFoundrySlackInstallFeedFields(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "fields")
	install := f.installed(t)
	assert.Empty(t, install.BotID, "unknown until Finish reads it")
	assert.False(t, install.InstallerAdmin)
	assert.Nil(t, install.EventsLimitedAt)
	require.NoError(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{BotID: ptr("B7"), InstallerAdmin: ptr(true)}))
	got, err := f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, "B7", got.BotID)
	assert.True(t, got.InstallerAdmin)
	require.NoError(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{InstallerAdmin: ptr(false)}))
	got, err = f.db.GetFoundrySlackInstall(ctx, f.workspaceID)
	require.NoError(t, err)
	assert.Equal(t, "B7", got.BotID, "nil keeps")
	assert.False(t, got.InstallerAdmin)

	assert.Error(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{}))
	assert.Error(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{BotID: ptr("B 7")}))
	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, install.ID, true))
	assert.ErrorIs(t, f.db.UpdateFoundrySlackInstallBot(ctx, install.ID, model.SlackInstallBotUpdate{BotID: ptr("B8")}), store.ErrNotFound)
}

func TestFoundrySlackThreadPosts(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "posts")
	install := f.installed(t)
	for n, user := range []string{"UADA", "UGRACE"} {
		require.NoError(t, f.send(n+1, 0))
		m := f.mapping(n+1, install.ID, f.threadID(n+1))
		m.SlackUserID = user
		require.NoError(t, f.db.CreateFoundrySlackThread(ctx, m))
	}
	mappings, err := f.db.ListFoundrySlackThreadMappings(ctx, install.ID, "C1", "1712345678.000100")
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	assert.Nil(t, mappings[0].BotAt)
	assert.Nil(t, mappings[0].OthersAt)

	at := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, f.db.MarkFoundrySlackThreadBot(ctx, install.ID, "C1", "1712345678.000100", at))
	require.NoError(t, f.db.MarkFoundrySlackThreadBot(ctx, install.ID, "C1", "1712345678.000100", at.Add(-time.Hour)))
	require.NoError(t, f.db.MarkFoundrySlackThreadOthers(ctx, install.ID, "C1", "1712345678.000100", at.Add(-time.Minute)))
	mappings, err = f.db.ListFoundrySlackThreadMappings(ctx, install.ID, "C1", "1712345678.000100")
	require.NoError(t, err)
	for _, m := range mappings {
		require.NotNil(t, m.BotAt)
		assert.True(t, m.BotAt.Equal(at), "never moves back")
		require.NotNil(t, m.OthersAt)
		assert.True(t, m.OthersAt.Equal(at.Add(-time.Minute)))
	}
	thread, err := f.db.GetFoundrySlackThread(ctx, install.ID, "C1", "1712345678.000100", "UADA")
	require.NoError(t, err)
	assert.NotNil(t, thread.BotAt)
	none, err := f.db.ListFoundrySlackThreadMappings(ctx, install.ID, "C1", "9.9")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// The scheduler re-dues a pushed Slack connector with a live sync, and
// queues its sync with origin pushed.
func TestSlackAppConnectorSchedules(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "sched")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, _ := f.bound(t, 1, install, "C1")
	dbExec(t, `UPDATE connectors SET next_sync_at=NOW(6) - INTERVAL 1 MINUTE WHERE id=?`, c.ID)
	n, err := f.db.QueueDueConnectorSyncs(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	active, err := f.db.GetActiveConnectorSync(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ConnectorSyncPushed, active.Origin)

	dbExec(t, `UPDATE connectors SET next_sync_at=NOW(6) - INTERVAL 1 MINUTE WHERE id=?`, c.ID)
	n, err = f.db.QueueDueConnectorSyncs(ctx, 10, 5*time.Minute)
	require.NoError(t, err)
	assert.Zero(t, n, "its live sync stays")
	stored, err := f.db.GetConnector(ctx, f.workspaceID, c.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.NextSyncAt, "due again after the live sync")
	assert.WithinDuration(t, time.Now().Add(5*time.Minute), *stored.NextSyncAt, 10*time.Second)

	// A token connector isn't pushed: its live sync leaves it unscheduled.
	token := &model.Connector{ID: id.New(), WorkspaceID: f.workspaceID, Kind: model.ConnectorKindSlack, Name: "Token", Config: json.RawMessage(`{}`),
		SecretCiphertext: []byte("sealed"), SecretKeyID: "k1"}
	require.NoError(t, f.db.CreateConnector(ctx, token))
	stored, err = f.db.GetConnector(ctx, f.workspaceID, token.ID)
	require.NoError(t, err)
	assert.False(t, stored.Pushed)
	source := eventSource(t, f.db, f.workspaceID, "sched")
	stored, err = f.db.GetConnector(ctx, f.workspaceID, source.ID)
	require.NoError(t, err)
	assert.True(t, stored.Pushed, "an event source always is")
}

// A stored value outside its closed set is refused as it is read, never
// passed on.
func TestFoundrySlackFeedBadRows(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "badrows")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1")
	day := model.SlackDay(time.Now())
	for i := 1; i <= 2; i++ {
		_, err := f.db.AppendFoundrySlackFeed(ctx, install.ID, model.SlackFeedBatch{Rows: []model.SlackFeedRow{feedRow("C1", fmt.Sprintf("Ev%d", i), day)},
			At: day}, feedLimits)
		require.NoError(t, err)
	}
	require.NoError(t, f.db.CommitFoundrySlackFold(ctx, model.SlackFold{FeedID: feed.ID, From: 0, To: 1,
		Days: []model.SlackFeedDay{slackDay("C1", day, "k", "partial")}}))
	_, err := f.db.QueueFoundrySlackThreads(ctx, []model.SlackBackfillThread{{FeedID: feed.ID, ChannelID: "C1", ThreadTS: "1.1", Day: day}})
	require.NoError(t, err)
	budget := model.SlackFeedBudget{Rows: 10, Bytes: 1 << 20}

	for _, c := range []struct {
		name, corrupt string
		read          func() error
	}{
		{"row kind", `UPDATE foundry_slack_feed SET kind='edit' WHERE feed_id=? AND seq=1`, func() error {
			_, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 0, budget)
			return err
		}},
		{"row source", `UPDATE foundry_slack_feed SET source='relay' WHERE feed_id=? AND seq=2`, func() error {
			_, err := f.db.FoundrySlackFeedAfter(ctx, feed.ID, 1, budget)
			return err
		}},
		{"day state", `UPDATE foundry_slack_feed_days SET state='done' WHERE feed_id=?`, func() error {
			_, err := f.db.FoundrySlackDaysAfter(ctx, feed.ID, 0)
			return err
		}},
		{"window reason", `UPDATE foundry_slack_backfill SET reason='lost' WHERE feed_id=?`, func() error {
			_, err := f.db.DueFoundrySlackBackfill(ctx, model.SlackSlotHistory, 10)
			return err
		}},
		{"window status", `UPDATE foundry_slack_backfill SET reason='bound', status='paused' WHERE feed_id=?`, func() error {
			_, err := f.db.FoundrySlackBackfillProgress(ctx, feed.ID)
			return err
		}},
		{"thread status", `UPDATE foundry_slack_backfill_threads SET status='lost' WHERE feed_id=?`, func() error {
			// Only queued threads are read: a lost one reads as none.
			work, err := f.db.DueFoundrySlackBackfill(ctx, model.SlackSlotReplies, 10)
			if err == nil && len(work) == 0 {
				return fmt.Errorf("refused as none")
			}
			return err
		}},
	} {
		dbExec(t, c.corrupt, feed.ID)
		assert.Error(t, c.read(), c.name)
	}
}
