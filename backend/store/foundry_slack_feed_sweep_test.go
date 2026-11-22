package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func TestListFoundrySlackFeeds(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "feeds")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	_, feed := f.bound(t, 1, install, "C1")

	got, err := f.db.ListFoundrySlackFeeds(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, feed.ID, got[0].ID)
	got, err = f.db.ListFoundrySlackFeeds(ctx, feed.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, got, "the page after it")
	_, err = f.db.ListFoundrySlackFeeds(ctx, "", 0)
	assert.Error(t, err)

	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, f.workspaceID, install.ID, false))
	got, err = f.db.ListFoundrySlackFeeds(ctx, "", 10)
	require.NoError(t, err)
	assert.Empty(t, got, "a removed install's feed is frozen")
}

func TestCloseFoundrySlackDays(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "close")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	c, feed := f.bound(t, 1, install, "C1", "C2", "C3", "C4", "C5", "C6", "C7")
	today := model.SlackDay(time.Now())
	day := today.AddDate(0, 0, -3)
	held := func(channel string, at time.Time, state model.SlackDayState, seq int) {
		dbExec(t, `INSERT INTO foundry_slack_feed_days (feed_id, channel_id, day, object_key, digest, messages, bytes, state, updated_seq)
VALUES (?, ?, ?, ?, ?, 1, 1, ?, ?)`, feed.ID, channel, at.Format(time.DateOnly), "k/"+channel+at.Format(time.DateOnly), strings.Repeat("a", 64),
			state, seq)
	}
	for _, ch := range []string{"C1", "C2", "C3", "C4", "C5", "C6", "C7", "C9"} {
		held(ch, day, model.SlackDayPartial, 1)
	}
	held("C1", today, model.SlackDayPartial, 1)                  // not over yet
	held("C1", day.AddDate(0, 0, -1), model.SlackDayComplete, 1) // closed already
	held("C2", day.AddDate(0, 0, -1), model.SlackDayLossy, 1)    // lossy already
	held("C3", day.AddDate(0, 0, 1), model.SlackDayPartial, 1)   // its window hasn't read it
	dbExec(t, `UPDATE foundry_slack_feed_channels SET covered_from=? WHERE feed_id=? AND channel_id IN ('C1', 'C4', 'C5', 'C7')`,
		day.AddDate(0, 0, -1), feed.ID) // events hold them from before the day
	dbExec(t, `UPDATE foundry_slack_backfill SET latest=? WHERE feed_id=? AND channel_id='C2'`, day, feed.ID) // read back past its start
	dbExec(t, `UPDATE foundry_slack_backfill SET status='failed' WHERE feed_id=? AND channel_id='C6'`, feed.ID)
	gap := func(channel, status string) {
		dbExec(t, `INSERT INTO foundry_slack_backfill (feed_id, channel_id, oldest, reason, latest, status) VALUES (?, ?, ?, 'gap', ?, ?)`,
			feed.ID, channel, day.Add(9*time.Hour), day.Add(9*time.Hour+time.Minute), status)
	}
	gap("C4", "queued")
	gap("C7", "cancelled")
	gap("C1", "done") // repaired
	dbExec(t, `INSERT INTO foundry_slack_backfill_threads (feed_id, channel_id, thread_ts, day) VALUES (?, 'C5', '1.000001', ?)`, feed.ID,
		day.Format(time.DateOnly))
	dbExec(t, `UPDATE connectors SET next_sync_at=NULL WHERE id=?`, c.ID)

	closed, err := f.db.CloseFoundrySlackDays(ctx, feed.ID, today, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 4, closed)
	rows, err := testDB.Query(`SELECT channel_id, day, source, payload FROM foundry_slack_feed WHERE feed_id=? AND kind='complete' ORDER BY channel_id`,
		feed.ID)
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var channel, source, payload string
		var at time.Time
		require.NoError(t, rows.Scan(&channel, &at, &source, &payload))
		assert.True(t, at.Equal(day))
		assert.Equal(t, "backfill", source)
		got = append(got, channel+" "+strings.ReplaceAll(payload, " ", ""))
	}
	require.NoError(t, rows.Close())
	assert.Equal(t, []string{`C1 {}`, `C2 {}`, `C6 {"lossy":true}`, `C7 {"lossy":true}`}, got,
		"read whole by events (C1) or a window (C2), or lossy (C6, C7); not C3 (unread), C4 (a gap live), C5 (a thread queued), C9 (not covered)")
	assert.True(t, dbTrue(t, `SELECT next_sync_at IS NOT NULL FROM connectors WHERE id=?`, c.ID), "its connector syncs them")

	closed, err = f.db.CloseFoundrySlackDays(ctx, feed.ID, today, time.Minute)
	require.NoError(t, err)
	assert.Zero(t, closed, "closed twice before a fold, kept once")
	dbExec(t, `UPDATE foundry_slack_feed_days SET updated_seq=2 WHERE feed_id=? AND channel_id='C1'`, feed.ID)
	closed, err = f.db.CloseFoundrySlackDays(ctx, feed.ID, today, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, closed, "a day a fold wrote again closes again")
	closed, err = f.db.CloseFoundrySlackDays(ctx, feed.ID, today.AddDate(0, 0, 1), time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 1, closed, "today, once it is over")

	_, err = f.db.CloseFoundrySlackDays(ctx, feed.ID, today.Add(time.Hour), time.Minute)
	assert.Error(t, err, "days close before a midnight")
	_, err = f.db.CloseFoundrySlackDays(ctx, feed.ID, today, 0)
	assert.Error(t, err)
	_, err = f.db.CloseFoundrySlackDays(ctx, "nope", today, time.Minute)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestFlipSlackAppConnector(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "flip")
	install := f.feedInstall(t, 1, f.workspaceID, "T1", "")
	token := &model.Connector{ID: id26("SAC-flip-token"), WorkspaceID: f.workspaceID, Kind: model.ConnectorKindSlack, Name: "Slack token",
		Config: json.RawMessage(`{}`), SecretCiphertext: []byte("sealed"), SecretKeyID: "k1", SecretHint: "xoxb…"}
	require.NoError(t, f.db.CreateConnector(ctx, token))
	staged, flipped := json.RawMessage(`{"credential":"helix_app","staged":true}`), json.RawMessage(`{"credential":"helix_app"}`)
	require.NoError(t, f.db.UpdateSlackAppConnector(ctx, f.workspaceID, token.ID, model.ConnectorUpdate{Config: staged, SecretCiphertext: []byte("sealed"),
		SecretKeyID: "k1", SecretHint: "xoxb…"}, model.SlackAppBindStaged{Bind: bindTo(install, "C1")}))
	dbExec(t, `UPDATE connectors SET change_cursor='{"at":1}' WHERE id=?`, token.ID)
	flip := func(before json.RawMessage) error {
		return f.db.FlipSlackAppConnector(ctx, f.workspaceID, token.ID, before, flipped, []byte("empty"), "k2")
	}

	assert.ErrorIs(t, flip(json.RawMessage(`{"credential":"helix_app"}`)), store.ErrConflict, "settings changed since they were read")
	require.NoError(t, f.db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SYNC-flip-1"), ConnectorID: token.ID, WorkspaceID: f.workspaceID}))
	assert.ErrorIs(t, flip(staged), store.ErrConflict, "a sync live")
	c, err := f.db.GetConnector(ctx, f.workspaceID, token.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(staged), string(c.Config), "a refused flip writes nothing")
	assert.False(t, c.Pushed)
	dbExec(t, `UPDATE connector_syncs SET status='succeeded', active_key=NULL WHERE connector_id=?`, token.ID)

	require.NoError(t, flip(json.RawMessage(`{"staged": true, "credential": "helix_app"}`)), "settings compare as JSON")
	c, err = f.db.GetConnector(ctx, f.workspaceID, token.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(flipped), string(c.Config))
	assert.True(t, c.Pushed)
	assert.Nil(t, c.ChangeCursor)
	assert.Equal(t, []byte("empty"), c.SecretCiphertext)
	assert.Equal(t, "k2", c.SecretKeyID)
	assert.Empty(t, c.SecretHint)
	sync, err := f.db.GetActiveConnectorSync(ctx, f.workspaceID, token.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ConnectorSyncFull, sync.Mode)
	assert.Equal(t, model.ConnectorSyncSettings, sync.Origin)
	dbExec(t, `UPDATE connector_syncs SET status='succeeded', active_key=NULL WHERE connector_id=?`, token.ID)
	assert.ErrorIs(t, flip(flipped), store.ErrConflict, "flipped already: pushed")

	t.Run("no live feed", func(t *testing.T) {
		dbExec(t, `UPDATE connectors SET pushed=FALSE, config=CAST(? AS JSON) WHERE id=?`, string(staged), token.ID)
		dbExec(t, `UPDATE foundry_slack_feeds SET deleting_at=NOW(6) WHERE connector_id=?`, token.ID)
		assert.ErrorIs(t, flip(staged), store.ErrConflict)
	})
	t.Run("refusals", func(t *testing.T) {
		assert.ErrorIs(t, f.db.FlipSlackAppConnector(ctx, f.workspaceID, id26("SAC-none"), staged, flipped, nil, "k1"), store.ErrNotFound)
		assert.Error(t, f.db.FlipSlackAppConnector(ctx, f.workspaceID, token.ID, json.RawMessage(`{`), flipped, nil, "k1"))
		events := &model.Connector{ID: id26("SAC-flip-events"), WorkspaceID: f.workspaceID, Kind: model.ConnectorKindEvents, Name: "Events",
			Config: json.RawMessage(`{}`), SecretCiphertext: []byte{}}
		require.NoError(t, f.db.CreateConnector(ctx, events))
		assert.ErrorContains(t, f.db.FlipSlackAppConnector(ctx, f.workspaceID, events.ID, staged, flipped, nil, "k1"), "kind")
	})
}
