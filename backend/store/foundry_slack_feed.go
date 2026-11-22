package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The Slack data connector on Helix's Slack app
// (migrations/20261214000000_foundry_slack_feed.sql): a connector's feed of
// its Slack team, the buffer of what Slack sent and the backfill read, the
// day mirror's index, and the backfill's windows, threads, Tier-1 slots and
// gap budget.
//
// Locks are taken in one order: the workspace, the connector, the install,
// the feed, then the feed's other rows. An append takes the feed alone, and
// reads its install in a subquery (Vitess cannot parse FOR UPDATE OF).

const foundrySlackFeedColumns = `id, connector_id, workspace_id, team_id, enterprise_id, install_id, last_seq, folded_seq, notice_version, bound_by,
       bound_at, deleting_at`

const foundrySlackFeedRowColumns = `seq, dedupe_key, channel_id, day, kind, source, payload`

const foundrySlackDayColumns = `channel_id, day, object_key, digest, messages, bytes, state, updated_seq, documented_at`

const foundrySlackWindowColumns = `feed_id, channel_id, oldest, reason, place, status, latest, page_cursor, attempts, pages, messages, threads,
       covered_s, error, updated_at`

const foundrySlackThreadWorkColumns = `feed_id, channel_id, thread_ts, day, page_cursor, status`

// liveFoundrySlackBackfill is a window still to read.
const liveFoundrySlackBackfill = `status IN ('queued', 'running')`

// foundrySlackRowsPerInsert bounds one multi-row INSERT.
const foundrySlackRowsPerInsert = 500

// --- Binding ---

// GetFoundrySlackFeed returns the connector's live feed; ErrNotFound when it
// has none (a token connector, or one whose feed is being deleted).
func (s *mysqlDB) GetFoundrySlackFeed(ctx context.Context, connectorID string) (*model.SlackFeed, error) {
	return scanFoundrySlackFeed(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`
FROM foundry_slack_feeds WHERE live_connector=?`, connectorID))
}

// CreateSlackAppConnector stores c, a new Slack connector read through its
// workspace's install of Helix's Slack app (so with no secret or account),
// pushed, with its feed, in one
// transaction holding the workspace's lock (ErrNotFound when it is gone).
// The binding is refused (model.SlackFeedRefusal), and nothing is written,
// as bindFoundrySlackFeed says.
func (s *mysqlDB) CreateSlackAppConnector(ctx context.Context, c *model.Connector, bind model.SlackFeedBind) error {
	if c.Kind != model.ConnectorKindSlack {
		return fmt.Errorf("create Slack app connector: kind is %q", c.Kind)
	}
	if err := bind.Validate(); err != nil {
		return err
	}
	if len(c.SecretCiphertext) > 0 || c.SecretKeyID != "" || c.Account != nil {
		return fmt.Errorf("create Slack app connector: it reads through the install, with no secret or account of its own")
	}
	c.SecretCiphertext, c.Pushed = []byte{}, true
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, c.WorkspaceID); err != nil {
			return err
		}
		if err := tx.bindFoundrySlackFeed(ctx, c.WorkspaceID, c.ID, bind); err != nil {
			return err
		}
		return tx.CreateConnector(ctx, c)
	})
}

// UpdateSlackAppConnector applies update to a live Slack connector of the
// workspace (ErrNotFound otherwise) and switches its credential as sw says,
// in one transaction holding the workspace's lock, then the connector's:
//   - model.SlackAppBindNow: pushed, its change cursor nulled (the next sync
//     reads in full from the mirror). A feed it has on the same install
//     stays (a staged switch's flip); one on another is unbound and a new one
//     bound as bindFoundrySlackFeed says;
//   - model.SlackAppBindStaged: bound the same way, but not pushed and its
//     cursor kept: it syncs by its token until the flip. A connector read
//     through the app already is not staged (an error);
//   - model.SlackAppUnbind: its feed, if any, unbound; not pushed, its cursor
//     nulled.
//
// A refused binding writes nothing.
func (s *mysqlDB) UpdateSlackAppConnector(ctx context.Context, workspaceID, id string, update model.ConnectorUpdate, sw model.SlackAppSwitch) error {
	var bind *model.SlackFeedBind
	pushed, nullCursor := true, true
	switch sw := sw.(type) {
	case model.SlackAppBindNow:
		bind = &sw.Bind
	case model.SlackAppBindStaged:
		bind, pushed, nullCursor = &sw.Bind, false, false
	case model.SlackAppUnbind:
		pushed = false
	default:
		return fmt.Errorf("update Slack app connector: unknown switch %T", sw)
	}
	if bind != nil {
		if err := bind.Validate(); err != nil {
			return err
		}
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		var kind string
		var wasPushed bool
		err := tx.db.QueryRowContext(ctx, `SELECT kind, pushed FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
			id, workspaceID).Scan(&kind, &wasPushed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case kind != model.ConnectorKindSlack:
			return fmt.Errorf("update Slack app connector %s: kind is %q", id, kind)
		case !nullCursor && wasPushed:
			return fmt.Errorf("Slack connector %s reads through the app already: it isn't staged", id)
		}
		feed, err := scanFoundrySlackFeed(tx.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`
FROM foundry_slack_feeds WHERE live_connector=? FOR UPDATE`, id))
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if feed != nil && (bind == nil || feed.InstallID != bind.InstallID) {
			if err := tx.unbindFoundrySlackFeed(ctx, id); err != nil {
				return err
			}
			feed = nil
		}
		if bind != nil && feed == nil {
			if err := tx.bindFoundrySlackFeed(ctx, workspaceID, id, *bind); err != nil {
				return err
			}
		}
		return tx.updateConnector(ctx, workspaceID, id, update, nullCursor, &pushed)
	})
}

// bindFoundrySlackFeed binds connectorID to the workspace's live install
// bind.InstallID, under the workspace's lock the caller holds. It locks the
// install, and refuses (model.SlackFeedRefusal) when:
//   - the workspace's live install isn't bind.InstallID (not_installed);
//   - it lacks one of bind.Scopes (scopes);
//   - no Slack admin or owner installed it (admin);
//   - its bot_id isn't known (bot_unknown);
//   - another live feed reads it (in_use), or reads another team of its
//     Enterprise Grid org, in any workspace (enterprise).
//
// Else it writes the feed, bound now; each of bind.Channels covered from
// now; and a bound window per channel, [now - HistoryDays, now), in the
// order given.
func (s *mysqlDB) bindFoundrySlackFeed(ctx context.Context, workspaceID, connectorID string, bind model.SlackFeedBind) error {
	install, err := scanFoundrySlackInstall(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE live_workspace=? FOR UPDATE`, workspaceID))
	switch {
	case errors.Is(err, ErrNotFound):
		return model.SlackFeedRefusal{Reason: model.SlackFeedNotInstalled}
	case err != nil:
		return err
	case install.ID != bind.InstallID:
		return model.SlackFeedRefusal{Reason: model.SlackFeedNotInstalled}
	}
	for _, scope := range bind.Scopes {
		if !slices.Contains(install.Scopes, scope) {
			return model.SlackFeedRefusal{Reason: model.SlackFeedScopes}
		}
	}
	switch {
	case !install.InstallerAdmin:
		return model.SlackFeedRefusal{Reason: model.SlackFeedAdmin}
	case install.BotID == "":
		return model.SlackFeedRefusal{Reason: model.SlackFeedBotUnknown}
	}
	for _, in := range []struct {
		reason model.SlackFeedRefusalReason
		where  string
		arg    string
	}{
		{model.SlackFeedInUse, "live_install=?", install.ID},
		{model.SlackFeedEnterprise, "live_enterprise=?", install.EnterpriseID},
	} {
		if in.arg == "" {
			continue
		}
		var other, otherWorkspace string
		err := s.db.QueryRowContext(ctx, `SELECT connector_id, workspace_id FROM foundry_slack_feeds WHERE `+in.where, in.arg).
			Scan(&other, &otherWorkspace)
		switch {
		case err == nil:
			return model.SlackFeedRefusal{Reason: in.reason, ConnectorID: other, WorkspaceID: otherWorkspace}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT NOW(6)`).Scan(&now); err != nil {
		return err
	}
	feedID := id.New()
	_, err = s.db.ExecContext(ctx, `INSERT INTO foundry_slack_feeds
  (id, connector_id, workspace_id, team_id, enterprise_id, install_id, notice_version, bound_by, bound_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, feedID, connectorID, workspaceID, install.TeamID, install.EnterpriseID, install.ID, bind.NoticeVersion,
		bind.BoundBy, now)
	var me *mysql.MySQLError
	switch {
	case errors.As(err, &me) && me.Number == 1062 && strings.Contains(me.Message, "foundry_slack_feeds_enterprise_uq"):
		// Another workspace's feed of the org committed first.
		return model.SlackFeedRefusal{Reason: model.SlackFeedEnterprise}
	case err != nil:
		return err
	}
	oldest := now.Add(-time.Duration(bind.HistoryDays) * 24 * time.Hour)
	for start := 0; start < len(bind.Channels); start += foundrySlackRowsPerInsert {
		chunk := bind.Channels[start:min(start+foundrySlackRowsPerInsert, len(bind.Channels))]
		channels, windows := make([]any, 0, 3*len(chunk)), make([]any, 0, 6*len(chunk))
		for i, c := range chunk {
			channels = append(channels, feedID, c, now)
			windows = append(windows, feedID, c, oldest, start+i, now, model.SlackBackfillBound)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_feed_channels (feed_id, channel_id, covered_from) VALUES `+
			placeholderRows(len(chunk), 3), channels...); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_backfill (feed_id, channel_id, oldest, place, latest, reason) VALUES `+
			placeholderRows(len(chunk), 6), windows...); err != nil {
			return err
		}
	}
	return nil
}

// unbindFoundrySlackFeed marks the connector's live feed, if any, deleting,
// and deletes its channels, windows, threads and gap budget. Its buffer,
// index and objects go at PruneFoundrySlackFeed's next pass, a batch at a
// time, then the feed row. The caller holds the connector's lock.
func (s *mysqlDB) unbindFoundrySlackFeed(ctx context.Context, connectorID string) error {
	var feedID string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_slack_feeds WHERE live_connector=? FOR UPDATE`, connectorID).Scan(&feedID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("lock connector %s Slack feed: %w", connectorID, err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_feeds SET deleting_at=NOW(6) WHERE id=?`, feedID); err != nil {
		return fmt.Errorf("unbind Slack feed %s: %w", feedID, err)
	}
	return s.deleteFoundrySlackFeedWork(ctx, feedID)
}

// deleteFoundrySlackFeedWork deletes a feed's channels, windows, threads
// and gap budget.
func (s *mysqlDB) deleteFoundrySlackFeedWork(ctx context.Context, feedID string) error {
	for _, table := range []string{"foundry_slack_feed_channels", "foundry_slack_backfill", "foundry_slack_backfill_threads", "foundry_slack_gap_budget"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE feed_id=?`, feedID); err != nil {
			return fmt.Errorf("delete Slack feed %s %s: %w", feedID, table, err)
		}
	}
	return nil
}

// repointFoundrySlackFeeds has the workspace's live feeds of teamID, whose
// install was removed, read through installID, the team's new install, in
// InstallFoundrySlack's transaction. Each records a gap of every channel it
// covers from its old install's revocation to now, and its connector, if
// pushed, syncs now with its schedule's pause lifted. The connectors are
// locked before their feeds, as a connector's delete locks them.
func (s *mysqlDB) repointFoundrySlackFeeds(ctx context.Context, workspaceID, teamID, installID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT connector_id FROM foundry_slack_feeds
WHERE workspace_id=? AND team_id=? AND deleting_at IS NULL ORDER BY connector_id`, workspaceID, teamID)
	if err != nil {
		return err
	}
	connectors, err := scanStrings(rows)
	if err != nil || len(connectors) == 0 {
		return err
	}
	locked, err := s.db.QueryContext(ctx, `SELECT id FROM connectors WHERE id IN (`+idPlaceholders(len(connectors))+`) ORDER BY id FOR UPDATE`,
		toAnySlice(connectors)...)
	if err != nil {
		return err
	}
	if _, err := scanStrings(locked); err != nil {
		return err
	}
	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT NOW(6)`).Scan(&now); err != nil {
		return err
	}
	for _, connectorID := range connectors {
		feed, err := scanFoundrySlackFeed(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`
FROM foundry_slack_feeds WHERE live_connector=? AND team_id=? FOR UPDATE`, connectorID, teamID))
		switch {
		case errors.Is(err, ErrNotFound):
			continue
		case err != nil:
			return err
		}
		var revoked sql.NullTime
		if err := s.db.QueryRowContext(ctx, `SELECT revoked_at FROM foundry_slack_installs WHERE id=?`, feed.InstallID).Scan(&revoked); err != nil &&
			!errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_feeds SET install_id=? WHERE id=?`, installID, feed.ID); err != nil {
			return err
		}
		if revoked.Valid && revoked.Time.Before(now) {
			if _, err := s.recordFoundrySlackGaps(ctx, feed, []model.SlackFeedGap{{From: revoked.Time, To: now}}); err != nil {
				return err
			}
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE connectors SET next_sync_at=NOW(6), schedule_paused=NULL
WHERE id=? AND deleted_at IS NULL AND pushed`, connectorID); err != nil {
			return err
		}
	}
	return nil
}

// --- Appends ---

// lockFoundrySlackFeedByInstall locks the live feed reading installID, and
// says whether the install is live; ErrNotFound when no live feed reads it.
func (s *mysqlDB) lockFoundrySlackFeedByInstall(ctx context.Context, installID string) (*model.SlackFeed, bool, error) {
	var live bool
	feed, err := scanFoundrySlackFeed(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`,
  EXISTS (SELECT 1 FROM foundry_slack_installs i WHERE i.id=f.install_id AND i.revoked_at IS NULL)
FROM foundry_slack_feeds f WHERE f.live_install=? FOR UPDATE`, installID), &live)
	return feed, live, err
}

// AppendFoundrySlackFeed appends what one Slack request brought to the live
// feed reading installID, in one transaction holding the feed's lock, so
// rows are numbered in the order their appends commit:
//   - no live feed reads the install, or the install is revoked:
//     model.SlackAppendUnbound, and nothing is kept;
//   - the batch would leave more than limits.MaxRows unfolded: a gap of the
//     minute batch.At for each row's channel is recorded in its place
//     (model.SlackAppendFull), past the cap;
//   - else the rows are kept, but for those kept already
//     (model.SlackAppended, or model.SlackAppendDuplicate when none is new).
//
// When it keeps a row, a pushed connector with no sync due is due
// limits.Spacing from now.
func (s *mysqlDB) AppendFoundrySlackFeed(ctx context.Context, installID string, batch model.SlackFeedBatch, limits model.SlackFeedLimits) (
	model.SlackFeedAppended, error) {
	switch {
	case len(batch.Rows) == 0 || batch.At.IsZero():
		return model.SlackFeedAppended{}, fmt.Errorf("a Slack feed append has rows and a time")
	case limits.Spacing <= 0 || limits.MaxRows < 1:
		return model.SlackFeedAppended{}, fmt.Errorf("a Slack feed append has a spacing and a cap")
	}
	for _, r := range batch.Rows {
		if err := r.Validate(); err != nil {
			return model.SlackFeedAppended{}, err
		}
	}
	var out model.SlackFeedAppended
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		feed, live, err := tx.lockFoundrySlackFeedByInstall(ctx, installID)
		switch {
		case errors.Is(err, ErrNotFound) || err == nil && !live:
			out = model.SlackFeedAppended{Outcome: model.SlackAppendUnbound}
			return nil
		case err != nil:
			return err
		}
		out = model.SlackFeedAppended{FeedID: feed.ID, Outcome: model.SlackAppended}
		if feed.LastSeq+int64(len(batch.Rows))-feed.FoldedSeq > limits.MaxRows {
			var gaps []model.SlackFeedGap
			for _, r := range batch.Rows {
				if gap := model.SlackMinuteGap(r.ChannelID, batch.At); !slices.Contains(gaps, gap) {
					gaps = append(gaps, gap)
				}
			}
			out.Outcome = model.SlackAppendFull
			out.Rows, err = tx.recordFoundrySlackGaps(ctx, feed, gaps)
		} else {
			out.Rows, err = tx.appendFoundrySlackRows(ctx, feed, batch.Rows)
			if out.Rows == 0 {
				out.Outcome = model.SlackAppendDuplicate
			}
		}
		if err != nil || out.Rows == 0 {
			return err
		}
		return tx.dueFoundrySlackSync(ctx, feed.ConnectorID, limits.Spacing)
	})
	return out, err
}

// AppendFoundrySlackFeedGaps records gaps in the live feed reading
// installID (a gap naming no channel is one of every channel it covers): a
// gap row each, and a gap window each, merged with the channel's queued
// ones it meets. Unbound when no live feed reads a live install. A pushed
// connector with no sync due is due spacing from now.
func (s *mysqlDB) AppendFoundrySlackFeedGaps(ctx context.Context, installID string, gaps []model.SlackFeedGap, spacing time.Duration) (
	model.SlackFeedAppended, error) {
	if spacing <= 0 || len(gaps) == 0 {
		return model.SlackFeedAppended{}, fmt.Errorf("Slack feed gaps have a spacing and a gap")
	}
	for _, g := range gaps {
		if err := g.Validate(); err != nil {
			return model.SlackFeedAppended{}, err
		}
	}
	var out model.SlackFeedAppended
	err := s.WithTx(ctx, func(txDB Store) error {
		var err error
		out, err = txDB.(*mysqlDB).appendFoundrySlackGaps(ctx, installID, gaps, spacing)
		return err
	})
	return out, err
}

// RecordFoundrySlackEventsLimited records Slack's app_rate_limited for the
// live install installID (ErrNotFound when it isn't one): the install's
// events_limited_at moves up to minute, and its live feed, if any, records
// a gap of that minute in every channel it covers, as
// AppendFoundrySlackFeedGaps does.
func (s *mysqlDB) RecordFoundrySlackEventsLimited(ctx context.Context, installID string, minute time.Time, spacing time.Duration) (
	model.SlackFeedAppended, error) {
	if spacing <= 0 || minute.IsZero() {
		return model.SlackFeedAppended{}, fmt.Errorf("Slack's rate limit has a minute and a spacing")
	}
	gap := model.SlackMinuteGap("", minute)
	var out model.SlackFeedAppended
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_installs
SET events_limited_at=GREATEST(COALESCE(events_limited_at, ?), ?) WHERE id=? AND revoked_at IS NULL`, gap.From, gap.From, installID); err != nil {
			return err
		}
		var err error
		out, err = tx.appendFoundrySlackGaps(ctx, installID, []model.SlackFeedGap{gap}, spacing)
		return err
	})
	return out, err
}

func (s *mysqlDB) appendFoundrySlackGaps(ctx context.Context, installID string, gaps []model.SlackFeedGap, spacing time.Duration) (
	model.SlackFeedAppended, error) {
	feed, live, err := s.lockFoundrySlackFeedByInstall(ctx, installID)
	switch {
	case errors.Is(err, ErrNotFound) || err == nil && !live:
		return model.SlackFeedAppended{Outcome: model.SlackAppendUnbound}, nil
	case err != nil:
		return model.SlackFeedAppended{}, err
	}
	out := model.SlackFeedAppended{Outcome: model.SlackAppended, FeedID: feed.ID}
	if out.Rows, err = s.recordFoundrySlackGaps(ctx, feed, gaps); err != nil || out.Rows == 0 {
		if out.Rows == 0 {
			out.Outcome = model.SlackAppendDuplicate
		}
		return out, err
	}
	return out, s.dueFoundrySlackSync(ctx, feed.ConnectorID, spacing)
}

// recordFoundrySlackGaps records gaps in feed, whose lock the caller holds:
// for each channel a gap names that the feed covers (every one it covers,
// for a gap naming none), a gap row and a gap window merged with the
// channel's queued, unread gap windows it meets or touches. It returns how
// many gap rows it kept.
func (s *mysqlDB) recordFoundrySlackGaps(ctx context.Context, feed *model.SlackFeed, gaps []model.SlackFeedGap) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id FROM foundry_slack_feed_channels WHERE feed_id=? ORDER BY channel_id`, feed.ID)
	if err != nil {
		return 0, err
	}
	covered, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	var gapRows []model.SlackFeedRow
	for _, g := range gaps {
		channels := []string{g.ChannelID}
		if g.ChannelID == "" {
			channels = covered
		}
		for _, c := range channels {
			if !slices.Contains(covered, c) {
				continue
			}
			gapRows = append(gapRows, g.Row(c))
			if err := s.queueFoundrySlackGap(ctx, feed.ID, c, g.From.UTC(), g.To.UTC()); err != nil {
				return 0, err
			}
		}
	}
	return s.appendFoundrySlackRows(ctx, feed, gapRows)
}

// queueFoundrySlackGap queues a gap window of [from, to) in a channel of a
// feed whose lock the caller holds, merged with the channel's queued gap
// windows no page has read that it meets or touches. A window with the same
// oldest already is read again from its newest end.
func (s *mysqlDB) queueFoundrySlackGap(ctx context.Context, feedID, channelID string, from, to time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT oldest, latest FROM foundry_slack_backfill
WHERE feed_id=? AND channel_id=? AND reason='gap' AND status='queued' AND pages=0 AND oldest <= ? AND latest >= ? FOR UPDATE`,
		feedID, channelID, to, from)
	if err != nil {
		return err
	}
	var met []time.Time
	for rows.Next() {
		var oldest, latest time.Time
		if err := rows.Scan(&oldest, &latest); err != nil {
			_ = rows.Close()
			return err
		}
		met = append(met, oldest)
		if oldest.Before(from) {
			from = oldest
		}
		if latest.After(to) {
			to = latest
		}
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, oldest := range met {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_slack_backfill WHERE feed_id=? AND channel_id=? AND oldest=?`,
			feedID, channelID, oldest); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO foundry_slack_backfill (feed_id, channel_id, oldest, reason, latest)
VALUES (?, ?, ?, 'gap', ?)
ON DUPLICATE KEY UPDATE page_cursor=IF(latest < VALUES(latest), NULL, page_cursor), latest=GREATEST(latest, VALUES(latest)),
  status=IF(status='running', status, 'queued'), updated_at=NOW(6)`, feedID, channelID, from, to)
	return err
}

// appendFoundrySlackRows numbers rows after feed's last_seq and keeps those
// not kept already, under the feed's lock the caller holds, and returns how
// many it kept. A row kept already leaves its number unused.
func (s *mysqlDB) appendFoundrySlackRows(ctx context.Context, feed *model.SlackFeed, rows []model.SlackFeedRow) (int, error) {
	kept := 0
	for chunk := range slices.Chunk(rows, foundrySlackRowsPerInsert) {
		args := make([]any, 0, 8*len(chunk))
		for _, r := range chunk {
			feed.LastSeq++
			args = append(args, feed.ID, feed.LastSeq, r.DedupeKey, r.ChannelID, r.Day.Format(time.DateOnly), string(r.Kind), string(r.Source),
				string(r.Payload), len(r.Payload))
		}
		res, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_slack_feed
  (feed_id, seq, dedupe_key, channel_id, day, kind, source, payload, bytes) VALUES `+placeholderRows(len(chunk), 9), args...)
		if err != nil {
			return 0, fmt.Errorf("append Slack feed %s rows: %w", feed.ID, err)
		}
		n, _ := res.RowsAffected()
		kept += int(n)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_feeds SET last_seq=? WHERE id=?`, feed.LastSeq, feed.ID); err != nil {
		return 0, fmt.Errorf("number Slack feed %s rows: %w", feed.ID, err)
	}
	return kept, nil
}

// dueFoundrySlackSync has a live pushed connector with no sync due sync
// spacing from now, as a send to an event source does.
func (s *mysqlDB) dueFoundrySlackSync(ctx context.Context, connectorID string, spacing time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connectors SET next_sync_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE id=? AND next_sync_at IS NULL AND pushed AND deleted_at IS NULL`, spacing.Microseconds(), connectorID)
	return err
}

// --- Folds ---

// FoundrySlackFeedAfter returns a feed's buffer rows numbered after after,
// up to its last_seq, oldest first: at most budget.Rows, within
// budget.Bytes of payload but for the first, which comes whatever its size.
func (s *mysqlDB) FoundrySlackFeedAfter(ctx context.Context, feedID string, after int64, budget model.SlackFeedBudget) ([]model.SlackFeedRow, error) {
	if budget.Rows < 1 || budget.Bytes < 1 {
		return nil, fmt.Errorf("a Slack feed read has a budget")
	}
	var last int64
	if err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM foundry_slack_feeds WHERE id=?`, feedID).Scan(&last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	sizes, err := s.db.QueryContext(ctx, `SELECT seq, bytes FROM foundry_slack_feed WHERE feed_id=? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`,
		feedID, after, last, budget.Rows)
	if err != nil {
		return nil, err
	}
	cut, total := after, int64(0)
	for sizes.Next() {
		var seq, bytes int64
		if err := sizes.Scan(&seq, &bytes); err != nil {
			_ = sizes.Close()
			return nil, err
		}
		if total += bytes; total > budget.Bytes && cut > after {
			break
		}
		cut = seq
	}
	_ = sizes.Close()
	if err := sizes.Err(); err != nil || cut == after {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackFeedRowColumns+`
FROM foundry_slack_feed WHERE feed_id=? AND seq > ? AND seq <= ? ORDER BY seq`, feedID, after, cut)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackFeedRow)
}

// CommitFoundrySlackFold commits a fold of the buffer rows (From, To] of a
// live feed, only while its folded_seq is still From (ErrConflict
// otherwise: another fold won, or the feed is deleting): folded_seq moves
// to To, the fold's days are written at updated_seq To, its dropped days'
// index rows deleted, and their objects and the ones it replaced recorded
// for the sweep (connector_replaced_snapshots). A fold that loses has its
// own days' objects recorded instead, so they go too.
func (s *mysqlDB) CommitFoundrySlackFold(ctx context.Context, fold model.SlackFold) error {
	if fold.FeedID == "" || fold.From < 0 || fold.To <= fold.From {
		return fmt.Errorf("a Slack fold names its feed and a span of rows")
	}
	seen := map[model.SlackDayKey]bool{}
	for _, d := range fold.Days {
		if err := d.Validate(); err != nil {
			return err
		}
		if seen[d.SlackDayKey] {
			return fmt.Errorf("a Slack fold writes channel %s's day %s twice", d.ChannelID, d.Day.Format(time.DateOnly))
		}
		seen[d.SlackDayKey] = true
	}
	for _, d := range fold.Dropped {
		if !model.ValidSlackID(d.ChannelID) || seen[d] {
			return fmt.Errorf("a Slack fold drops channel %q's day %s, which it doesn't write", d.ChannelID, d.Day.Format(time.DateOnly))
		}
	}
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		switch err := execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_feeds SET folded_seq=?
WHERE id=? AND folded_seq=? AND deleting_at IS NULL AND last_seq >= ?`, fold.To, fold.FeedID, fold.From, fold.To); {
		case errors.Is(err, ErrNotFound):
			return ErrConflict
		case err != nil:
			return err
		}
		var workspaceID, connectorID string
		if err := tx.db.QueryRowContext(ctx, `SELECT workspace_id, connector_id FROM foundry_slack_feeds WHERE id=?`, fold.FeedID).
			Scan(&workspaceID, &connectorID); err != nil {
			return err
		}
		for chunk := range slices.Chunk(fold.Days, foundrySlackRowsPerInsert) {
			args := make([]any, 0, 9*len(chunk))
			for _, d := range chunk {
				args = append(args, fold.FeedID, d.ChannelID, d.Day.Format(time.DateOnly), d.ObjectKey, d.Digest, d.Messages, d.Bytes, string(d.State),
					fold.To)
			}
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_feed_days
  (feed_id, channel_id, day, object_key, digest, messages, bytes, state, updated_seq) VALUES `+placeholderRows(len(chunk), 9)+`
ON DUPLICATE KEY UPDATE object_key=VALUES(object_key), digest=VALUES(digest), messages=VALUES(messages), bytes=VALUES(bytes),
  state=VALUES(state), updated_seq=VALUES(updated_seq)`, args...); err != nil {
				return err
			}
		}
		replaced := fold.Replaced
		for _, d := range fold.Dropped {
			var key string
			err := tx.db.QueryRowContext(ctx, `SELECT object_key FROM foundry_slack_feed_days WHERE feed_id=? AND channel_id=? AND day=? FOR UPDATE`,
				fold.FeedID, d.ChannelID, d.Day.Format(time.DateOnly)).Scan(&key)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_slack_feed_days WHERE feed_id=? AND channel_id=? AND day=?`,
				fold.FeedID, d.ChannelID, d.Day.Format(time.DateOnly)); err != nil {
				return err
			}
			replaced = append(replaced, key)
		}
		for _, key := range replaced {
			if err := recordReplacedSnapshot(ctx, tx.db, workspaceID, connectorID, key); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, ErrConflict) || len(fold.Days) == 0 {
		return err
	}
	// The fold lost: its objects are no one's. Only a feed the prune
	// finished deleting while the fold ran is gone, and then they stay.
	var workspaceID, connectorID string
	switch err := s.db.QueryRowContext(ctx, `SELECT workspace_id, connector_id FROM foundry_slack_feeds WHERE id=?`, fold.FeedID).
		Scan(&workspaceID, &connectorID); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrConflict
	case err != nil:
		return err
	}
	for _, d := range fold.Days {
		if err := recordReplacedSnapshot(ctx, s.db, workspaceID, connectorID, d.ObjectKey); err != nil {
			return err
		}
	}
	return ErrConflict
}

// FoundrySlackDaysAfter returns a feed's index rows a fold wrote after seq,
// by day and channel: what the change read renders.
func (s *mysqlDB) FoundrySlackDaysAfter(ctx context.Context, feedID string, seq int64) ([]model.SlackFeedDay, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackDayColumns+`
FROM foundry_slack_feed_days WHERE feed_id=? AND updated_seq > ? ORDER BY day, channel_id`, feedID, seq)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackDay)
}

// MarkFoundrySlackDaysDocumented records that the days' documents were
// offered now, by a sync that succeeded.
func (s *mysqlDB) MarkFoundrySlackDaysDocumented(ctx context.Context, feedID string, days []model.SlackDayKey) error {
	for _, d := range days {
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_feed_days SET documented_at=NOW(6) WHERE feed_id=? AND channel_id=? AND day=?`,
			feedID, d.ChannelID, d.Day.Format(time.DateOnly)); err != nil {
			return err
		}
	}
	return nil
}

// PruneFoundrySlackFeed deletes up to limit rows the feeds no longer need,
// and returns how many: buffer rows the mirror holds (seq <= folded_seq);
// and, of a feed being deleted, its buffer, then its index rows, whose
// objects it records for the sweep, then the feed itself.
func (s *mysqlDB) PruneFoundrySlackFeed(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, fmt.Errorf("a Slack feed prune has a limit")
	}
	type due struct {
		id       string
		folded   int64
		deleting bool
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.folded_seq, f.deleting_at IS NOT NULL FROM foundry_slack_feeds f
WHERE f.deleting_at IS NOT NULL
   OR EXISTS (SELECT 1 FROM foundry_slack_feed b WHERE b.feed_id=f.id AND b.seq <= f.folded_seq)
ORDER BY f.id`)
	if err != nil {
		return 0, err
	}
	feeds, err := scanAll(rows, func(r rowScanner) (due, error) {
		var d due
		return d, r.Scan(&d.id, &d.folded, &d.deleting)
	})
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, f := range feeds {
		if deleted >= int64(limit) {
			break
		}
		q, args := `DELETE FROM foundry_slack_feed WHERE feed_id=? AND seq <= ? ORDER BY seq LIMIT ?`, []any{f.id, f.folded, int64(limit) - deleted}
		if f.deleting {
			q, args = `DELETE FROM foundry_slack_feed WHERE feed_id=? ORDER BY seq LIMIT ?`, []any{f.id, int64(limit) - deleted}
		}
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		if deleted += n; !f.deleting || deleted >= int64(limit) {
			continue
		}
		err = s.WithTx(ctx, func(txDB Store) error {
			tx := txDB.(*mysqlDB)
			n, err := tx.dropDeletingFoundrySlackFeed(ctx, f.id, int64(limit)-deleted)
			deleted += n
			return err
		})
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// dropDeletingFoundrySlackFeed deletes up to limit index rows of a feed
// being deleted whose buffer is gone, recording their objects for the
// sweep, then, once none is left, the feed. It returns how many rows it
// deleted.
func (s *mysqlDB) dropDeletingFoundrySlackFeed(ctx context.Context, feedID string, limit int64) (int64, error) {
	var workspaceID, connectorID string
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id, connector_id FROM foundry_slack_feeds WHERE id=? AND deleting_at IS NOT NULL FOR UPDATE`,
		feedID).Scan(&workspaceID, &connectorID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var buffered bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_slack_feed WHERE feed_id=?)`, feedID).Scan(&buffered); err != nil || buffered {
		return 0, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, day, object_key FROM foundry_slack_feed_days WHERE feed_id=?
ORDER BY channel_id, day LIMIT ? FOR UPDATE`, feedID, limit+1)
	if err != nil {
		return 0, err
	}
	type day struct {
		channel string
		day     time.Time
		key     string
	}
	days, err := scanAll(rows, func(r rowScanner) (day, error) {
		var d day
		return d, r.Scan(&d.channel, &d.day, &d.key)
	})
	if err != nil {
		return 0, err
	}
	more := int64(len(days)) > limit
	if more {
		days = days[:limit]
	}
	for _, d := range days {
		if err := recordReplacedSnapshot(ctx, s.db, workspaceID, connectorID, d.key); err != nil {
			return 0, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_slack_feed_days WHERE feed_id=? AND channel_id=? AND day=?`,
			feedID, d.channel, d.day.Format(time.DateOnly)); err != nil {
			return 0, err
		}
	}
	if more {
		return int64(len(days)), nil
	}
	if err := s.deleteFoundrySlackFeedWork(ctx, feedID); err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_slack_feeds WHERE id=?`, feedID); err != nil {
		return 0, err
	}
	return int64(len(days)) + 1, nil
}

// --- Channels ---

// ListFoundrySlackFeedChannels returns the channels a feed covers, and from
// when, by channel.
func (s *mysqlDB) ListFoundrySlackFeedChannels(ctx context.Context, feedID string) ([]model.SlackChannelJoin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, covered_from FROM foundry_slack_feed_channels WHERE feed_id=? ORDER BY channel_id`, feedID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (model.SlackChannelJoin, error) {
		var c model.SlackChannelJoin
		return c, r.Scan(&c.ChannelID, &c.CoveredFrom)
	})
}

// JoinFoundrySlackChannels has a live feed (ErrNotFound otherwise) cover
// channels newly readable, each with a joined window of [Oldest,
// CoveredFrom); a channel it covers already is left as it is. It returns how
// many it joined.
func (s *mysqlDB) JoinFoundrySlackChannels(ctx context.Context, feedID string, joins []model.SlackChannelJoin) (int, error) {
	for _, j := range joins {
		if !model.ValidSlackID(j.ChannelID) || !j.Oldest.Before(j.CoveredFrom) {
			return 0, fmt.Errorf("a Slack channel join names its channel, and is covered after its window's oldest")
		}
	}
	joined := 0
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		joined = 0
		if _, err := tx.lockLiveFoundrySlackFeed(ctx, feedID); err != nil {
			return err
		}
		for _, j := range joins {
			res, err := tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_slack_feed_channels (feed_id, channel_id, covered_from) VALUES (?, ?, ?)`,
				feedID, j.ChannelID, j.CoveredFrom)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_backfill (feed_id, channel_id, oldest, reason, latest) VALUES (?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE reason=VALUES(reason), latest=VALUES(latest), status='queued', page_cursor=NULL`,
				feedID, j.ChannelID, j.Oldest, model.SlackBackfillJoined, j.CoveredFrom); err != nil {
				return err
			}
			joined++
		}
		return nil
	})
	return joined, err
}

// PurgeFoundrySlackChannel forgets a channel a live feed (ErrNotFound
// otherwise) no longer reads: its index rows, whose objects it records for
// the sweep, its coverage, windows and threads. The connector's rows and
// documents of it are the syncer's to delete.
func (s *mysqlDB) PurgeFoundrySlackChannel(ctx context.Context, feedID, channelID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		feed, err := tx.lockLiveFoundrySlackFeed(ctx, feedID)
		if err != nil {
			return err
		}
		rows, err := tx.db.QueryContext(ctx, `SELECT object_key FROM foundry_slack_feed_days WHERE feed_id=? AND channel_id=? FOR UPDATE`,
			feedID, channelID)
		if err != nil {
			return err
		}
		keys, err := scanStrings(rows)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := recordReplacedSnapshot(ctx, tx.db, feed.WorkspaceID, feed.ConnectorID, key); err != nil {
				return err
			}
		}
		for _, table := range []string{"foundry_slack_feed_days", "foundry_slack_feed_channels", "foundry_slack_backfill", "foundry_slack_backfill_threads"} {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE feed_id=? AND channel_id=?`, feedID, channelID); err != nil {
				return err
			}
		}
		return nil
	})
}

// lockLiveFoundrySlackFeed locks a live feed; ErrNotFound when it is
// deleting or gone.
func (s *mysqlDB) lockLiveFoundrySlackFeed(ctx context.Context, feedID string) (*model.SlackFeed, error) {
	return scanFoundrySlackFeed(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`
FROM foundry_slack_feeds WHERE id=? AND deleting_at IS NULL FOR UPDATE`, feedID))
}

// --- Slots ---

// ReserveFoundrySlackSlot reserves the install's slot for method: true, and
// the slot's next one is every from now, when it was due; false when it
// isn't due yet (another replica called). One statement, no lock held.
func (s *mysqlDB) ReserveFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, every time.Duration) (bool, error) {
	if _, err := model.ParseSlackSlotMethod(string(method)); err != nil {
		return false, err
	}
	if every <= 0 {
		return false, fmt.Errorf("a Slack slot comes every so often")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_slots SET next_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE install_id=? AND method=? AND next_at <= NOW(6)`, every.Microseconds(), installID, method)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeferFoundrySlackSlot has the install's slot for method wait at least
// after from now (Slack's Retry-After); ErrNotFound when it has none.
func (s *mysqlDB) DeferFoundrySlackSlot(ctx context.Context, installID string, method model.SlackSlotMethod, after time.Duration) error {
	if _, err := model.ParseSlackSlotMethod(string(method)); err != nil {
		return err
	}
	if after < 0 {
		return fmt.Errorf("a Slack slot defers to later")
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_slots SET next_at=GREATEST(next_at, NOW(6) + INTERVAL ? MICROSECOND)
WHERE install_id=? AND method=?`, after.Microseconds(), installID, method)
}

// --- Backfill ---

// DueFoundrySlackBackfill returns up to limit live feeds' next backfill
// work for method, of live installs whose slot for it is due, the
// longest-due first. No lock is taken: ReserveFoundrySlackSlot decides who
// calls, and AdvanceFoundrySlackBackfill's compare-and-set who writes.
//   - history: a window, gaps first, then joined ones newest first, then
//     bound ones in the bind's order;
//   - replies: a queued thread, the newest day first.
func (s *mysqlDB) DueFoundrySlackBackfill(ctx context.Context, method model.SlackSlotMethod, limit int) ([]model.SlackBackfillWork, error) {
	work := `EXISTS (SELECT 1 FROM foundry_slack_backfill b WHERE b.feed_id=f.id AND b.` + liveFoundrySlackBackfill + `)`
	if method == model.SlackSlotReplies {
		work = `EXISTS (SELECT 1 FROM foundry_slack_backfill_threads t WHERE t.feed_id=f.id AND t.status='queued')`
	} else if method != model.SlackSlotHistory {
		return nil, fmt.Errorf("unknown Slack slot method %q", method)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+prefixColumns("f.", foundrySlackFeedColumns)+`
FROM foundry_slack_feeds f JOIN foundry_slack_slots sl ON sl.install_id=f.install_id AND sl.method=? AND sl.next_at <= NOW(6)
WHERE f.deleting_at IS NULL
  AND EXISTS (SELECT 1 FROM foundry_slack_installs i WHERE i.id=f.install_id AND i.revoked_at IS NULL)
  AND `+work+`
ORDER BY sl.next_at, f.id LIMIT ?`, method, limit)
	if err != nil {
		return nil, err
	}
	feeds, err := scanAll(rows, func(r rowScanner) (*model.SlackFeed, error) { return scanFoundrySlackFeed(r) })
	if err != nil {
		return nil, err
	}
	out := make([]model.SlackBackfillWork, 0, len(feeds))
	for _, f := range feeds {
		var item model.SlackBackfillItem
		if method == model.SlackSlotHistory {
			item, err = scanFoundrySlackWindow(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackWindowColumns+`
FROM foundry_slack_backfill WHERE feed_id=? AND `+liveFoundrySlackBackfill+`
ORDER BY FIELD(reason, 'gap', 'joined', 'bound'), IF(reason='bound', place, 0), latest DESC, channel_id LIMIT 1`, f.ID))
		} else {
			item, err = scanFoundrySlackThreadWork(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackThreadWorkColumns+`
FROM foundry_slack_backfill_threads WHERE feed_id=? AND status='queued' ORDER BY day DESC, channel_id, thread_ts LIMIT 1`, f.ID))
		}
		switch {
		case errors.Is(err, ErrNotFound):
			continue // done since the first read
		case err != nil:
			return nil, err
		}
		out = append(out, model.SlackBackfillWork{Feed: f, Item: item})
	}
	return out, nil
}

// AdvanceFoundrySlackBackfill writes one call's answer, in one transaction
// holding the live feed's lock (ErrNotFound when it is deleting or gone),
// under a compare-and-set on the work as the step names it (ErrConflict
// when it moved on, and nothing is written):
//   - model.SlackHistoryStep: the window's next page ends at Latest, from
//     Cursor; it is done, or running; its progress grows by a page, the
//     messages, the new threads and the seconds covered; the threads are
//     queued;
//   - model.SlackRepliesStep: the thread's next page is Cursor, and its
//     status Status.
//
// Either way the page's rows are appended past the buffer's cap (a fold
// follows at once), and a pushed connector with no sync due is due spacing
// from now when one is new.
func (s *mysqlDB) AdvanceFoundrySlackBackfill(ctx context.Context, step model.SlackBackfillStep, spacing time.Duration) error {
	if spacing <= 0 {
		return fmt.Errorf("a Slack backfill step has a spacing")
	}
	var feedID string
	var rows []model.SlackFeedRow
	var write func(tx *mysqlDB) error
	switch st := step.(type) {
	case model.SlackHistoryStep:
		w := st.Window
		switch {
		case len(st.Cursor) > 255 || st.Latest.Before(w.Oldest) || st.Latest.After(w.Latest) || st.Messages < 0 || st.CoveredS < 0:
			return fmt.Errorf("a Slack history step's page ends within its window, with counts and a cursor of at most 255 bytes")
		case st.Done && st.Cursor != "":
			return fmt.Errorf("a done Slack history step has no next page")
		}
		for _, t := range st.Threads {
			if t.FeedID != w.FeedID || !model.ValidSlackID(t.ChannelID) || !model.ValidSlackTS(t.ThreadTS) || !t.Day.Equal(model.SlackDay(t.Day)) {
				return fmt.Errorf("a Slack history step's thread is of its feed, with a channel, a ts and a day")
			}
		}
		feedID, rows = w.FeedID, st.Rows
		write = func(tx *mysqlDB) error {
			status := model.SlackBackfillRunning
			if st.Done {
				status = model.SlackBackfillDone
			}
			if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_backfill
SET latest=?, page_cursor=?, status=?, pages=pages+1, messages=messages+?, covered_s=covered_s+?, attempts=0, error=NULL, updated_at=NOW(6)
WHERE feed_id=? AND channel_id=? AND oldest=? AND latest=? AND page_cursor <=> ? AND `+liveFoundrySlackBackfill,
				st.Latest, nullString(st.Cursor), status, st.Messages, st.CoveredS, w.FeedID, w.ChannelID, w.Oldest, w.Latest, nullString(w.Cursor)); err != nil {
				return err
			}
			queued, err := tx.queueFoundrySlackThreads(ctx, st.Threads, false)
			if err != nil || queued == 0 {
				return err
			}
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_slack_backfill SET threads=threads+? WHERE feed_id=? AND channel_id=? AND oldest=?`,
				queued, w.FeedID, w.ChannelID, w.Oldest)
			return err
		}
	case model.SlackRepliesStep:
		t := st.Thread
		if _, err := model.ParseSlackThreadStatus(string(st.Status)); err != nil {
			return err
		}
		if len(st.Cursor) > 255 || st.Status != model.SlackThreadQueued && st.Cursor != "" {
			return fmt.Errorf("a Slack replies step's cursor is at most 255 bytes, and only a queued thread has one")
		}
		feedID, rows = t.FeedID, st.Rows
		write = func(tx *mysqlDB) error {
			return execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_backfill_threads SET page_cursor=?, status=?
WHERE feed_id=? AND channel_id=? AND thread_ts=? AND status='queued' AND page_cursor <=> ?`,
				nullString(st.Cursor), st.Status, t.FeedID, t.ChannelID, t.ThreadTS, nullString(t.Cursor))
		}
	default:
		return fmt.Errorf("unknown Slack backfill step %T", step)
	}
	for _, r := range rows {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		feed, err := tx.lockLiveFoundrySlackFeed(ctx, feedID)
		if err != nil {
			return err
		}
		if err := write(tx); errors.Is(err, ErrNotFound) {
			return ErrConflict
		} else if err != nil {
			return err
		}
		kept, err := tx.appendFoundrySlackRows(ctx, feed, rows)
		if err != nil || kept == 0 {
			return err
		}
		return tx.dueFoundrySlackSync(ctx, feed.ConnectorID, spacing)
	})
}

// QueueFoundrySlackThreads queues threads for the backfill to read the
// replies of (a gap's look back); one read already is read again from its
// first page. It returns how many it queued or queued again.
func (s *mysqlDB) QueueFoundrySlackThreads(ctx context.Context, threads []model.SlackBackfillThread) (int, error) {
	for _, t := range threads {
		if t.FeedID == "" || !model.ValidSlackID(t.ChannelID) || !model.ValidSlackTS(t.ThreadTS) || !t.Day.Equal(model.SlackDay(t.Day)) {
			return 0, fmt.Errorf("a Slack backfill thread names its feed, channel, ts and day")
		}
	}
	return s.queueFoundrySlackThreads(ctx, threads, true)
}

// queueFoundrySlackThreads inserts threads, queued; again, one read
// already is queued again from its first page. It returns how many rows it
// wrote.
func (s *mysqlDB) queueFoundrySlackThreads(ctx context.Context, threads []model.SlackBackfillThread, again bool) (int, error) {
	insert, onDup := `INSERT IGNORE`, ``
	if again {
		insert, onDup = `INSERT`, ` ON DUPLICATE KEY UPDATE page_cursor=IF(status='queued', page_cursor, NULL), status='queued'`
	}
	queued := 0
	for chunk := range slices.Chunk(threads, foundrySlackRowsPerInsert) {
		args := make([]any, 0, 4*len(chunk))
		for _, t := range chunk {
			args = append(args, t.FeedID, t.ChannelID, t.ThreadTS, t.Day.Format(time.DateOnly))
		}
		res, err := s.db.ExecContext(ctx, insert+` INTO foundry_slack_backfill_threads (feed_id, channel_id, thread_ts, day) VALUES `+
			placeholderRows(len(chunk), 4)+onDup, args...)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		if again {
			// An insert counts 1, a row changed 2, one left as it is 1
			// (CLIENT_FOUND_ROWS): every thread named was queued.
			n = int64(len(chunk))
		}
		queued += int(n)
	}
	return queued, nil
}

// FailFoundrySlackBackfill records a failed call of a live window
// (ErrNotFound otherwise): its attempts grow, and it fails once they reach
// maxAttempts. It returns the window's status after.
func (s *mysqlDB) FailFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, message string, maxAttempts int) (
	model.SlackBackfillStatus, error) {
	if maxAttempts < 1 {
		return "", fmt.Errorf("a Slack backfill window has attempts")
	}
	if err := execOrNotFound(ctx, s.db, `UPDATE foundry_slack_backfill
SET status=IF(attempts + 1 >= ?, 'failed', status), attempts=attempts + 1, error=?, updated_at=NOW(6)
WHERE feed_id=? AND channel_id=? AND oldest=? AND `+liveFoundrySlackBackfill, maxAttempts, truncateRunes(message, 255), w.FeedID, w.ChannelID,
		w.Oldest); err != nil {
		return "", err
	}
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM foundry_slack_backfill WHERE feed_id=? AND channel_id=? AND oldest=?`,
		w.FeedID, w.ChannelID, w.Oldest).Scan(&status); err != nil {
		return "", err
	}
	return model.ParseSlackBackfillStatus(status)
}

// EndFoundrySlackBackfill ends a live window (ErrNotFound otherwise) as
// cancelled (its channel can't be read, or a gap past the budget) or failed,
// saying why.
func (s *mysqlDB) EndFoundrySlackBackfill(ctx context.Context, w model.SlackBackfillWindow, status model.SlackBackfillStatus, message string) error {
	if status != model.SlackBackfillCancelled && status != model.SlackBackfillFailed {
		return fmt.Errorf("a Slack backfill window ends cancelled or failed, not %q", status)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_backfill SET status=?, error=?, updated_at=NOW(6)
WHERE feed_id=? AND channel_id=? AND oldest=? AND `+liveFoundrySlackBackfill, status, nullString(truncateRunes(message, 255)), w.FeedID, w.ChannelID,
		w.Oldest)
}

// truncateRunes cuts s to at most n characters.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// SpendFoundrySlackGap spends one of a feed's gap-repair calls for today
// (UTC): false when it has spent max already.
func (s *mysqlDB) SpendFoundrySlackGap(ctx context.Context, feedID string, max int) (bool, error) {
	if max < 1 {
		return false, nil
	}
	for try := 0; try < 2; try++ {
		res, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_gap_budget SET calls=calls + 1 WHERE feed_id=? AND day=DATE(NOW(6)) AND calls < ?`,
			feedID, max)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil
		}
		res, err = s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_slack_gap_budget (feed_id, day, calls) VALUES (?, DATE(NOW(6)), 1)`, feedID)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return true, nil
		}
		// Today's row exists: at the cap, or another spender inserted it
		// since the update, which the next update sees.
	}
	return false, nil
}

// FoundrySlackBackfillProgress returns what a live feed's backfill has left
// (ErrNotFound when it is deleting or gone).
func (s *mysqlDB) FoundrySlackBackfillProgress(ctx context.Context, feedID string) (model.SlackBackfillProgress, error) {
	var p model.SlackBackfillProgress
	feed, err := scanFoundrySlackFeed(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackFeedColumns+`
FROM foundry_slack_feeds WHERE id=? AND deleting_at IS NULL`, feedID))
	if err != nil {
		return p, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackWindowColumns+`
FROM foundry_slack_backfill WHERE feed_id=? AND reason IN ('bound', 'joined') ORDER BY reason, place, channel_id, oldest`, feedID)
	if err != nil {
		return p, err
	}
	if p.Windows, err = scanAll(rows, func(r rowScanner) (model.SlackBackfillWindow, error) {
		w, err := scanFoundrySlackWindow(r)
		if err != nil {
			return model.SlackBackfillWindow{}, err
		}
		return *w, nil
	}); err != nil {
		return p, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM foundry_slack_backfill_threads WHERE feed_id=? AND status='queued'),
  (SELECT COUNT(*) FROM foundry_slack_feed_days WHERE feed_id=? AND state='lossy')`, feedID, feedID).Scan(&p.QueuedThreads, &p.LossyDays); err != nil {
		return p, err
	}
	slots, err := s.db.QueryContext(ctx, `SELECT method, next_at FROM foundry_slack_slots WHERE install_id=?`, feed.InstallID)
	if err != nil {
		return p, err
	}
	defer slots.Close()
	for slots.Next() {
		var method string
		var at time.Time
		if err := slots.Scan(&method, &at); err != nil {
			return p, err
		}
		if method == string(model.SlackSlotHistory) {
			p.HistoryAt = at
		} else {
			p.RepliesAt = at
		}
	}
	return p, slots.Err()
}

// ListFoundrySlackOrphans returns up to limit live feeds whose install was
// removed more than retention ago, longest first: their connectors are
// deleted (DeleteConnector), with what they hold.
func (s *mysqlDB) ListFoundrySlackOrphans(ctx context.Context, retention time.Duration, limit int) ([]*model.SlackFeed, error) {
	if retention <= 0 {
		return nil, fmt.Errorf("a Slack feed is an orphan after a while")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+prefixColumns("f.", foundrySlackFeedColumns)+`
FROM foundry_slack_installs i JOIN foundry_slack_feeds f ON f.live_install=i.id
WHERE i.revoked_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY i.revoked_at, f.id LIMIT ?`, retention.Microseconds(), limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (*model.SlackFeed, error) { return scanFoundrySlackFeed(r) })
}

// --- Installs and threads ---

// UpdateFoundrySlackInstallBot stores what Finish learned of a live
// install's bot and installer (ErrNotFound when it isn't live).
func (s *mysqlDB) UpdateFoundrySlackInstallBot(ctx context.Context, installID string, u model.SlackInstallBotUpdate) error {
	var assignments []string
	var args []any
	if u.BotID != nil {
		if *u.BotID != "" && !model.ValidSlackID(*u.BotID) {
			return fmt.Errorf("Slack bot id %q is not a Slack id", *u.BotID)
		}
		assignments, args = append(assignments, "bot_id=?"), append(args, *u.BotID)
	}
	if u.InstallerAdmin != nil {
		assignments, args = append(assignments, "installer_admin=?"), append(args, *u.InstallerAdmin)
	}
	if len(assignments) == 0 {
		return fmt.Errorf("a Slack install's bot update is empty")
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_installs SET `+joinComma(assignments)+` WHERE id=? AND revoked_at IS NULL`,
		append(args, installID)...)
}

// ListFoundrySlackThreadMappings returns the mappings of a Slack thread
// whose Founder threads live, with when the bot and others last posted.
func (s *mysqlDB) ListFoundrySlackThreadMappings(ctx context.Context, installID, channelID, threadTS string) ([]*model.SlackThread, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackThreadColumns+`
FROM foundry_slack_threads s JOIN foundry_agent_threads t ON t.id=s.agent_thread_id AND t.deleted_at IS NULL
WHERE s.install_id=? AND s.channel_id=? AND s.thread_ts=? ORDER BY s.slack_user_id`, installID, channelID, threadTS)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackThread)
}

// MarkFoundrySlackThreadBot records that the bot posted in a Slack thread
// at, on each of its mappings (none moves back).
func (s *mysqlDB) MarkFoundrySlackThreadBot(ctx context.Context, installID, channelID, threadTS string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_threads SET bot_at=GREATEST(COALESCE(bot_at, ?), ?)
WHERE install_id=? AND channel_id=? AND thread_ts=?`, at.UTC(), at.UTC(), installID, channelID, threadTS)
	return err
}

// MarkFoundrySlackThreadOthers records that someone neither the bot nor a
// mapping's person posted in a Slack thread at, on each of its mappings
// (none moves back).
func (s *mysqlDB) MarkFoundrySlackThreadOthers(ctx context.Context, installID, channelID, threadTS string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_threads SET others_at=GREATEST(COALESCE(others_at, ?), ?)
WHERE install_id=? AND channel_id=? AND thread_ts=?`, at.UTC(), at.UTC(), installID, channelID, threadTS)
	return err
}

// --- SQL and scans ---

// placeholderRows is n rows of width placeholders: "(?, ?), (?, ?)".
func placeholderRows(n, width int) string {
	row := "(" + strings.TrimSuffix(strings.Repeat("?, ", width), ", ") + ")"
	return strings.TrimSuffix(strings.Repeat(row+", ", n), ", ")
}

// prefixColumns qualifies each of a column list's names with prefix.
func prefixColumns(prefix, columns string) string {
	names := strings.Split(columns, ",")
	for i, n := range names {
		names[i] = prefix + strings.TrimSpace(n)
	}
	return strings.Join(names, ", ")
}

// scanFoundrySlackFeed scans a feed's columns, then extra.
func scanFoundrySlackFeed(scanner rowScanner, extra ...any) (*model.SlackFeed, error) {
	var f model.SlackFeed
	var deleting sql.NullTime
	if err := scanner.Scan(append([]any{&f.ID, &f.ConnectorID, &f.WorkspaceID, &f.TeamID, &f.EnterpriseID, &f.InstallID, &f.LastSeq, &f.FoldedSeq,
		&f.NoticeVersion, &f.BoundBy, &f.BoundAt, &deleting}, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	f.DeletingAt = timePtr(deleting)
	return &f, nil
}

func scanFoundrySlackFeedRow(scanner rowScanner) (model.SlackFeedRow, error) {
	var r model.SlackFeedRow
	var kind, source string
	var payload []byte
	if err := scanner.Scan(&r.Seq, &r.DedupeKey, &r.ChannelID, &r.Day, &kind, &source, &payload); err != nil {
		return r, err
	}
	var err error
	if r.Kind, err = model.ParseSlackFeedRowKind(kind); err != nil {
		return r, err
	}
	if r.Source, err = model.ParseSlackFeedSource(source); err != nil {
		return r, err
	}
	r.Day, r.Payload = r.Day.UTC(), payload
	return r, nil
}

func scanFoundrySlackDay(scanner rowScanner) (model.SlackFeedDay, error) {
	var d model.SlackFeedDay
	var state string
	var documented sql.NullTime
	if err := scanner.Scan(&d.ChannelID, &d.Day, &d.ObjectKey, &d.Digest, &d.Messages, &d.Bytes, &state, &d.UpdatedSeq, &documented); err != nil {
		return d, err
	}
	var err error
	d.State, err = model.ParseSlackDayState(state)
	d.Day, d.DocumentedAt = d.Day.UTC(), timePtr(documented)
	return d, err
}

func scanFoundrySlackWindow(scanner rowScanner) (*model.SlackBackfillWindow, error) {
	var w model.SlackBackfillWindow
	var reason, status string
	var cursor, message sql.NullString
	if err := scanner.Scan(&w.FeedID, &w.ChannelID, &w.Oldest, &reason, &w.Place, &status, &w.Latest, &cursor, &w.Attempts, &w.Pages, &w.Messages,
		&w.Threads, &w.CoveredS, &message, &w.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if w.Reason, err = model.ParseSlackBackfillReason(reason); err != nil {
		return nil, err
	}
	if w.Status, err = model.ParseSlackBackfillStatus(status); err != nil {
		return nil, err
	}
	w.Cursor, w.Error = cursor.String, message.String
	return &w, nil
}

func scanFoundrySlackThreadWork(scanner rowScanner) (*model.SlackBackfillThread, error) {
	var t model.SlackBackfillThread
	var status string
	var cursor sql.NullString
	if err := scanner.Scan(&t.FeedID, &t.ChannelID, &t.ThreadTS, &t.Day, &cursor, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	t.Status, err = model.ParseSlackThreadStatus(status)
	t.Day, t.Cursor = t.Day.UTC(), cursor.String
	return &t, err
}
