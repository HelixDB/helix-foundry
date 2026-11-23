package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// What the foundry-slack-worker's backfill and sweep do to the Slack data
// connector's feeds (migrations/20261214000000_foundry_slack_feed.sql):
// list them, close the channel-days read whole, and flip a staged switch.

// ListFoundrySlackFeeds returns up to limit live feeds of live installs
// whose id is after after ("" for the first), by id.
func (s *mysqlDB) ListFoundrySlackFeeds(ctx context.Context, after string, limit int) ([]*model.SlackFeed, error) {
	if limit < 1 {
		return nil, fmt.Errorf("a Slack feed list has a limit")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+prefixColumns("f.", foundrySlackFeedColumns)+`
FROM foundry_slack_feeds f
WHERE f.deleting_at IS NULL AND f.id > ?
  AND EXISTS (SELECT 1 FROM foundry_slack_installs i WHERE i.id=f.install_id AND i.revoked_at IS NULL)
ORDER BY f.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (*model.SlackFeed, error) { return scanFoundrySlackFeed(r) })
}

// CloseFoundrySlackDays closes the channel-days of a live feed (ErrNotFound
// otherwise) before before that the mirror holds partial and that are read
// whole, in one transaction holding the feed's lock: a complete row each,
// for the next fold. A channel-day is read whole when events hold the
// channel from its start (covered_from), or a bound or joined window read
// it back past its start, or it is lossy: a failed window, or a gap window
// the budget cancelled, left some of it unread, and its row says so. None
// closes while a live gap window overlaps it or a thread of it is queued.
// A row is keyed by the day's index row (updated_seq), so a day closed
// again after a gap is a new row, and one closed twice before a fold is
// kept once. Rows are appended past the buffer's cap. Once it commits, a
// pushed connector with no sync due is due spacing from now, whether a day
// closed or not: yesterday's documents are offered once it closes, and days
// leave the window. It returns how many rows it appended.
func (s *mysqlDB) CloseFoundrySlackDays(ctx context.Context, feedID string, before time.Time, spacing time.Duration) (int, error) {
	if spacing <= 0 || !before.Equal(model.SlackDay(before)) {
		return 0, fmt.Errorf("a Slack feed's days close before a UTC midnight, with a spacing")
	}
	kept, connectorID := 0, ""
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		kept = 0
		feed, err := tx.lockLiveFoundrySlackFeed(ctx, feedID)
		if err != nil {
			return err
		}
		connectorID = feed.ConnectorID
		overlaps := `b.feed_id=d.feed_id AND b.channel_id=d.channel_id AND b.oldest < d.day + INTERVAL 1 DAY AND b.latest > d.day`
		rows, err := tx.db.QueryContext(ctx, `SELECT d.channel_id, d.day, d.updated_seq,
  c.covered_from <= d.day OR EXISTS (SELECT 1 FROM foundry_slack_backfill b WHERE b.feed_id=d.feed_id AND b.channel_id=d.channel_id
    AND b.reason IN ('bound', 'joined') AND b.status IN ('queued', 'running', 'done', 'failed') AND b.oldest <= d.day AND b.latest <= d.day),
  EXISTS (SELECT 1 FROM foundry_slack_backfill b WHERE `+overlaps+` AND (b.status='failed' OR b.reason='gap' AND b.status='cancelled'))
FROM foundry_slack_feed_days d JOIN foundry_slack_feed_channels c ON c.feed_id=d.feed_id AND c.channel_id=d.channel_id
WHERE d.feed_id=? AND d.state='partial' AND d.day < ?
  AND NOT EXISTS (SELECT 1 FROM foundry_slack_backfill b WHERE `+overlaps+` AND b.reason='gap' AND b.`+liveFoundrySlackBackfill+`)
  AND NOT EXISTS (SELECT 1 FROM foundry_slack_backfill_threads t WHERE t.feed_id=d.feed_id AND t.channel_id=d.channel_id AND t.day=d.day
    AND t.status='queued')
ORDER BY d.channel_id, d.day`, feedID, before.Format(time.DateOnly))
		if err != nil {
			return err
		}
		type candidate struct {
			channel     string
			day         time.Time
			seq         int64
			whole, lost bool
		}
		days, err := scanAll(rows, func(r rowScanner) (candidate, error) {
			var c candidate
			return c, r.Scan(&c.channel, &c.day, &c.seq, &c.whole, &c.lost)
		})
		if err != nil {
			return err
		}
		var closing []model.SlackFeedRow
		for _, c := range days {
			if !c.whole && !c.lost {
				continue
			}
			day := model.SlackDay(c.day)
			payload := json.RawMessage(`{}`)
			if c.lost {
				payload = json.RawMessage(`{"lossy":true}`)
			}
			closing = append(closing, model.SlackFeedRow{ChannelID: c.channel, Day: day, Kind: model.SlackRowComplete, Source: model.SlackFeedFromBackfill,
				DedupeKey: model.SlackBackfillDedupe(c.channel, day.Format(time.DateOnly), strconv.FormatInt(c.seq, 10), model.SlackRowComplete),
				Payload:   payload})
		}
		kept, err = tx.appendFoundrySlackRows(ctx, feed, closing)
		return err
	})
	if err != nil {
		return kept, err
	}
	return kept, s.dueFoundrySlackSync(ctx, connectorID, spacing)
}

// FlipSlackAppConnector ends a staged switch of a live Slack connector of
// the workspace (ErrNotFound otherwise) to Helix's Slack app, in one
// transaction holding the workspace's lock, then the connector's: its
// settings become flipped, its secret sealed (the empty token) and its
// hint cleared, it is pushed, its change cursor nulled, and a full read
// queued from those settings (a settings sync). ErrConflict, and nothing
// written, when its settings aren't staged any more (someone changed them),
// it is pushed, it has no live feed, or a sync of it is live: the flip
// waits for it to end.
func (s *mysqlDB) FlipSlackAppConnector(ctx context.Context, workspaceID, connectorID string, staged, flipped json.RawMessage, sealed []byte,
	keyID string) error {
	if !json.Valid(staged) || !json.Valid(flipped) {
		return fmt.Errorf("a Slack connector's flip names its settings before and after, as JSON")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		var kind string
		var pushed, same, fed bool
		err := tx.db.QueryRowContext(ctx, `SELECT kind, pushed, config = CAST(? AS JSON),
  EXISTS (SELECT 1 FROM foundry_slack_feeds f WHERE f.live_connector=c.id)
FROM connectors c WHERE c.id=? AND c.workspace_id=? AND c.deleted_at IS NULL FOR UPDATE`, string(staged), connectorID, workspaceID).
			Scan(&kind, &pushed, &same, &fed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case kind != model.ConnectorKindSlack:
			return fmt.Errorf("flip Slack connector %s: kind is %q", connectorID, kind)
		case pushed || !same || !fed:
			return ErrConflict
		}
		update := model.ConnectorUpdate{Config: flipped, SecretCiphertext: sealed, SecretKeyID: keyID}
		yes := true
		if err := tx.updateConnector(ctx, workspaceID, connectorID, update, true, &yes); err != nil {
			return err
		}
		return tx.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id.New(), ConnectorID: connectorID, WorkspaceID: workspaceID,
			Origin: model.ConnectorSyncSettings, Mode: model.ConnectorSyncFull})
	})
}
