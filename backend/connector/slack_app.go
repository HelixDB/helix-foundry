package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var (
	_ Pushed   = (*slackAppKind)(nil)
	_ PassKind = (*slackAppKind)(nil)
)

// SlackFeedStore is what a Slack connector read through Helix's Slack app
// reads and writes of its feed; Foundry's store implements it.
type SlackFeedStore interface {
	// GetFoundrySlackFeed is the connector's live feed.
	GetFoundrySlackFeed(ctx context.Context, connectorID string) (*model.SlackFeed, error)
	// FoundrySlackFeedAfter are the feed's buffer rows after a seq, within budget.
	FoundrySlackFeedAfter(ctx context.Context, feedID string, after int64, budget model.SlackFeedBudget) ([]model.SlackFeedRow, error)
	// CommitFoundrySlackFold commits a fold while the feed's folded_seq is its From.
	CommitFoundrySlackFold(ctx context.Context, fold model.SlackFold) error
	// FoundrySlackDaysAfter are the mirror's index rows a fold wrote after seq.
	FoundrySlackDaysAfter(ctx context.Context, feedID string, seq int64) ([]model.SlackFeedDay, error)
	// MarkFoundrySlackDaysDocumented records that the days' documents were offered.
	MarkFoundrySlackDaysDocumented(ctx context.Context, feedID string, days []model.SlackDayKey) error
	// ListFoundrySlackFeedChannels are the channels the feed covers.
	ListFoundrySlackFeedChannels(ctx context.Context, feedID string) ([]model.SlackChannelJoin, error)
	// JoinFoundrySlackChannels covers channels newly readable, each with a joined backfill.
	JoinFoundrySlackChannels(ctx context.Context, feedID string, joins []model.SlackChannelJoin) (int, error)
	// PurgeFoundrySlackChannel forgets a channel the feed no longer reads.
	PurgeFoundrySlackChannel(ctx context.Context, feedID, channelID string) error
}

// SlackFeedOptions wire the Slack kind to Helix's Slack app
// (foundry.slack): Feed holds the feeds, their buffers and the mirror's
// index, Objects the mirror's days, and Budget bounds one fold
// (feed.read_budget). HelixAppID is Helix's own app (foundry.slack.app_id),
// whose bot's messages either credential keeps without their text.
// DirectoryEvery is how often a change read lists the team's users,
// channels and members again (feed.directory_every), RedocumentEvery how
// often a closed day that keeps changing offers its document again
// (feed.redocument_every), and RepliesDays how many of the newest days the
// backfill reads replies of (backfill.replies_days), which the stream's
// description says. Zero options make a kind that discovers but cannot
// sync, as the events kind's do.
type SlackFeedOptions struct {
	Feed            SlackFeedStore
	Objects         snapshot.Store
	Budget          model.SlackFeedBudget
	HelixAppID      string
	DirectoryEvery  time.Duration
	RedocumentEvery time.Duration
	RepliesDays     int
}

var (
	// errSlackAppUnwired is a sync asked of the app's kind of a catalog
	// built without SlackFeedOptions.
	errSlackAppUnwired = errors.New("connector: this catalog's Slack app kind discovers but cannot sync")
	// errSlackAppUnopened is a read through Helix's Slack app of a source
	// whose install the host didn't open.
	errSlackAppUnopened = errors.New("connector: a Slack connection through Helix's Slack app is read through its install (Source.SlackApp)")
)

// slackAppKind is a Slack connector read through its workspace's install of
// Helix's Slack app: the same tables and documents as a token's
// (slackKind), but its messages come from Events, and the backfill's pages,
// through the feed's buffer into the day mirror (Fold), not from
// conversations.history, which Slack allows such an app once a minute.
// Users, channels and members are still listed by the Web API, through the
// install. It is Pushed: it syncs when rows arrive.
type slackAppKind struct {
	poll *slackKind
	SlackFeedOptions
}

func (k *slackAppKind) Name() string        { return "slack" }
func (k *slackAppKind) DisplayName() string { return "Slack" }

// Prepare is the Slack kind's: settings decide the credential.
func (k *slackAppKind) Prepare(src Source) (json.RawMessage, error) { return k.poll.Prepare(src) }

// Hint is empty: the app's install is the credential, shown in Settings › Slack.
func (k *slackAppKind) Hint(string) string { return "" }

// Streams are the conversations, as the app keeps them.
func (k *slackAppKind) Streams(Source) []Stream {
	s := slackConversations
	replies := "threads started on older days list no replies but those that arrived as they were posted"
	if k.RepliesDays > 0 {
		replies = fmt.Sprintf("threads started more than %d days ago list no replies but those that arrived as they were posted", k.RepliesDays)
	}
	s.Description = "Each finished UTC day of one Slack channel Helix Foundry is in, as Markdown: a heading with the channel and the UTC day, " +
		"the channel's name and id, then each message posted that day in time order as \"**Name** HH:MM: text\", in UTC, " +
		"with the replies of a thread it started listed beneath it, dated when sent on a later day; " + replies + ". " +
		"A day Slack may not have delivered whole ends by saying some messages may be missing. A day too long for one document keeps " +
		"its earliest messages. People are named by their Slack real name, else their handle, and mentions read @Name and #channel. " +
		"Helix Foundry's own messages show without their text."
	return []Stream{s}
}

// Discover proves the install with auth.test and probes what it lists, as
// a token's Discover does, but never reads history: whether the app may
// read each kind of channel's comes from its scopes.
func (k *slackAppKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	if src.SlackApp == nil {
		return Discovery{}, errSlackAppUnopened
	}
	return discoverSlack(ctx, k.poll.appClient(src.SlackApp), false, false)
}

// Capture follows every table: the feed says what changed, and the
// directory is listed again when it is due.
func (k *slackAppKind) Capture(_ Source, table string) Capture {
	if slices.ContainsFunc(slackObjects, func(o saasTable) bool { return o.name == table }) {
		return CaptureCursor
	}
	return CaptureFull
}

// slackAppCursor is where the app's next change read begins: the feed's
// folded_seq its tables hold, the days of history and window they were
// rendered with, when the directory was last listed, and the readable
// channels then, by name. Owed are channel-days whose documents wait
// (redocument_every, or a deadline), and Offered those the read put, which
// the next read, whose cursor says this sync committed, marks documented.
type slackAppCursor struct {
	Seq         *int64            `json:"seq"`
	HistoryDays int               `json:"history_days"`
	DirectoryAt time.Time         `json:"directory_at"`
	Window      string            `json:"window"`
	Channels    map[string]string `json:"channels"`
	Owed        []string          `json:"owed,omitempty"`
	Offered     []string          `json:"offered,omitempty"`
}

// pass is a read through the install: the Web API through its caller,
// messages from the mirror.
func (k *slackAppKind) pass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) (
	*slackPass, *slackMirror, error) {
	switch {
	case src.SlackApp == nil:
		return nil, nil, errSlackAppUnopened
	case k.Feed == nil || k.Objects == nil:
		return nil, nil, errSlackAppUnwired
	}
	mirror := &slackMirror{o: k.SlackFeedOptions, days: map[string][]model.SlackFeedDay{}}
	p, err := newSlackPass(ctx, src, k.poll.appClient(src.SlackApp), mirror, tables, rows, docs)
	if p != nil {
		p.states = map[string]model.SlackDayState{}
	}
	return p, mirror, err
}

// Sync and SyncDocuments read one table through SyncPass, as a token's do.
func (k *slackAppKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *slackAppKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	_, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return table, err
}

// SyncPass reads the tables in full, as a token's SyncPass does, but each
// channel's messages from every day the mirror holds of the window, as it
// is now: a day gets its rows whatever its state, and a document only once
// it is over and complete or lossy.
func (k *slackAppKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	p, mirror, err := k.pass(ctx, src, tables, rows, docs)
	if err != nil {
		return tables, err
	}
	if p.read["messages"] || p.read["channel_days"] {
		feed, err := k.Feed.GetFoundrySlackFeed(ctx, src.ConnectorID)
		if err != nil {
			return tables, fmt.Errorf("read the Slack feed of connector %s: %w", src.ConnectorID, err)
		}
		index, err := k.Feed.FoundrySlackDaysAfter(ctx, feed.ID, 0)
		if err != nil {
			return tables, err
		}
		for _, d := range index {
			if !d.Day.Before(p.window) {
				mirror.days[d.ChannelID] = append(mirror.days[d.ChannelID], d)
				p.states[d.ChannelID+":"+d.Day.Format(time.DateOnly)] = d.State
			}
		}
	}
	return tables, p.run(ctx)
}

// Start folds what waits, as one fold's budget allows, so the full read
// renders from the mirror as folded and the prune never deletes a row it
// skipped, then covers the channels the install reads (cover) and returns
// the folded seq.
func (k *slackAppKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	p, _, err := k.pass(ctx, src, tables, nil, noDocuments{})
	if err != nil {
		return Start{}, err
	}
	feed, err := k.Fold(ctx, src.ConnectorID, p.historyDays, *src.SlackApp)
	if err != nil {
		return Start{}, err
	}
	joined, err := p.followed(ctx)
	if err != nil {
		return Start{}, err
	}
	names, _, err := k.cover(ctx, feed, p, joined)
	if err != nil {
		return Start{}, err
	}
	cursor := slackAppCursor{Seq: &feed.FoldedSeq, HistoryDays: p.historyDays, DirectoryAt: slackNow().UTC(),
		Window: p.window.Format(time.DateOnly), Channels: names}
	return Start{Cursor: encodeJSON(cursor)}, nil
}

// cover has the feed cover the channels the install reads now (joined, of
// a kind whose history its scopes allow) and forget the others: a channel
// newly readable is covered from now, with a joined backfill of the window
// before, and one no longer readable is purged. It returns the readable
// channels by name and the purged ones.
func (k *slackAppKind) cover(ctx context.Context, feed *model.SlackFeed, p *slackPass, joined []slackChannel) (map[string]string, []string, error) {
	names := map[string]string{}
	for _, ch := range joined {
		if p.scopes.has(slackHistoryScope(ch.IsPrivate)) {
			names[ch.ID] = ch.Name
		}
	}
	covered, err := k.Feed.ListFoundrySlackFeedChannels(ctx, feed.ID)
	if err != nil {
		return nil, nil, err
	}
	now, held := slackNow().UTC(), map[string]bool{}
	var purged []string
	for _, c := range covered {
		held[c.ChannelID] = true
		if _, ok := names[c.ChannelID]; !ok {
			if err := k.Feed.PurgeFoundrySlackChannel(ctx, feed.ID, c.ChannelID); err != nil {
				return nil, nil, err
			}
			purged = append(purged, c.ChannelID)
		}
	}
	var joins []model.SlackChannelJoin
	for _, id := range slices.Sorted(maps.Keys(names)) {
		if !held[id] {
			joins = append(joins, model.SlackChannelJoin{ChannelID: id, CoveredFrom: now, Oldest: p.window})
		}
	}
	if len(joins) > 0 {
		if _, err := k.Feed.JoinFoundrySlackChannels(ctx, feed.ID, joins); err != nil {
			return nil, nil, err
		}
	}
	return names, purged, nil
}

// Changes marks the days the last sync offered as documented, folds what
// waits (one budget of it), covers the channels the install reads, then
// renders what the mirror changed since the cursor:
//   - Every UTC day holding a channel-day a fold wrote since the cursor's
//     seq loses its messages and channel_days rows, one DeleteWhere on day
//     each, and has every readable channel's rows of it written again from
//     the mirror: a token's change read's cascade.
//   - A channel no longer readable loses its rows (DeleteWhere on
//     channel_id), as do the days that left the window (on day).
//   - Past slackCascades of those, or with other history_days, the read
//     resets messages and channel_days, which the syncer reads in full from
//     the mirror in the same sync.
//   - A channel-day the fold changed, one owed, or one of a renamed channel
//     offers its document once it is over and complete or lossy, at most
//     once each RedocumentEvery while the stream holds a version of it:
//     a closed day busy with replies is extracted again at most that often.
//     One that waits is owed by the next cursor.
//   - The directory (users, channels, channel_members) is reset, so read in
//     full in the same sync, once DirectoryEvery has passed since it was, or
//     when the readable channels or their names changed.
//
// The next cursor holds the seq folded: the index rows a failed sync
// rendered stay above the stored cursor, so the next read renders them
// again.
func (k *slackAppKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	var prev slackAppCursor
	if err := json.Unmarshal(cursor, &prev); err != nil || prev.Seq == nil {
		return nil, fmt.Errorf("decode Slack app change cursor: %w", ErrCursorExpired)
	}
	prevWindow, err := time.Parse(time.DateOnly, prev.Window)
	if err != nil {
		return nil, fmt.Errorf("decode Slack app change cursor's window %q: %w", prev.Window, ErrCursorExpired)
	}
	rewrite := map[string]bool{}
	p, mirror, err := k.pass(ctx, src, tables, map[string]RowWriter{
		"messages":     &slackDayRows{w: w, table: "messages", rewrite: rewrite},
		"channel_days": &slackDayRows{w: w, table: "channel_days", rewrite: rewrite},
	}, w)
	if err != nil {
		return nil, err
	}
	feed, err := k.Feed.GetFoundrySlackFeed(ctx, src.ConnectorID)
	if err != nil {
		return nil, fmt.Errorf("read the Slack feed of connector %s: %w", src.ConnectorID, err)
	}
	if offered := slackDayKeys(prev.Offered); len(offered) > 0 {
		if err := k.Feed.MarkFoundrySlackDaysDocumented(ctx, feed.ID, offered); err != nil {
			return nil, err
		}
	}
	if feed, err = k.Fold(ctx, src.ConnectorID, p.historyDays, *src.SlackApp); err != nil {
		return nil, err
	}
	joined, err := p.followed(ctx)
	if err != nil {
		return nil, err
	}
	names, purged, err := k.cover(ctx, feed, p, joined)
	if err != nil {
		return nil, err
	}
	now := slackNow().UTC()
	next := slackAppCursor{Seq: &feed.FoldedSeq, HistoryDays: p.historyDays, DirectoryAt: prev.DirectoryAt, Window: p.window.Format(time.DateOnly),
		Channels: names}
	if !now.Before(prev.DirectoryAt.Add(k.DirectoryEvery)) || !maps.Equal(prev.Channels, names) {
		for _, t := range []string{"users", "channels", "channel_members"} {
			if p.read[t] {
				w.Reset(t, "the directory is listed again")
			}
		}
		next.DirectoryAt = now
	}
	if !p.read["messages"] && !p.read["channel_days"] {
		return encodeJSON(next), nil
	}
	index, err := k.Feed.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	if err != nil {
		return nil, err
	}
	// The channels whose rows go: no longer readable, or purged.
	remove := map[string]bool{}
	for _, id := range slices.Concat(slices.Collect(maps.Keys(prev.Channels)), purged) {
		if _, ok := names[id]; !ok {
			remove[id] = true
		}
	}
	var exits []string
	for day := prevWindow; day.Before(p.window); day = day.AddDate(0, 0, 1) {
		exits = append(exits, day.Format(time.DateOnly))
	}
	owedBefore := map[string]bool{}
	for _, id := range prev.Owed {
		owedBefore[id] = true
	}
	held, candidates := map[string]model.SlackFeedDay{}, map[string]bool{}
	for _, d := range index {
		id := d.ChannelID + ":" + d.Day.Format(time.DateOnly)
		name, readable := names[d.ChannelID]
		if !readable || d.Day.Before(p.window) {
			continue
		}
		held[id] = d
		if d.UpdatedSeq > *prev.Seq {
			rewrite[d.Day.Format(time.DateOnly)] = true
			candidates[id] = true
		}
		if owedBefore[id] || prev.Channels[d.ChannelID] != "" && prev.Channels[d.ChannelID] != name {
			candidates[id] = true
		}
	}
	reset := ""
	switch {
	case prev.HistoryDays != p.historyDays:
		reset = "the days of history to keep changed"
	case len(remove)+len(exits)+len(rewrite) > slackCascades:
		reset = fmt.Sprintf("%d channels and days of Slack changed", len(remove)+len(exits)+len(rewrite))
	}
	if reset != "" {
		for _, t := range slackHistoryTables {
			if p.read[t] {
				w.Reset(t, reset)
			}
		}
		return encodeJSON(next), nil
	}
	cascade := func(column string, value string) error {
		for _, t := range slackHistoryTables {
			if !p.read[t] {
				continue
			}
			if err := w.DeleteWhere(t, column, value); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range slices.Sorted(maps.Keys(remove)) {
		if err := cascade("channel_id", id); err != nil {
			return nil, err
		}
	}
	for _, day := range slices.Concat(exits, slices.Sorted(maps.Keys(rewrite))) {
		if err := cascade("day", day); err != nil {
			return nil, err
		}
	}
	channels := map[string]slackChannel{}
	for _, ch := range joined {
		channels[ch.ID] = ch
	}
	for _, id := range slices.Sorted(maps.Keys(held)) {
		d := held[id]
		if !rewrite[d.Day.Format(time.DateOnly)] && !candidates[id] {
			continue
		}
		day, err := mirror.open(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, m := range day.top() {
			if err := p.message(channels[d.ChannelID], m); err != nil {
				return nil, err
			}
		}
		p.states[id] = d.State
	}
	owed := map[string]bool{}
	p.offer = func(d *slackDay) bool {
		id := d.id()
		if !candidates[id] {
			return false
		}
		documented := held[id].DocumentedAt
		if _, ok := p.docs.Held(slackConversations.Name, id); !ok || documented == nil || !now.Before(documented.Add(k.RedocumentEvery)) {
			return true
		}
		owed[id] = true
		return false
	}
	if err := p.channelDays(ctx); err != nil {
		return nil, err
	}
	offered := map[string]bool{}
	for _, id := range p.offered {
		offered[id] = true
	}
	for id := range candidates {
		channel, day, _ := strings.Cut(id, ":")
		if late, ok := p.late[channel]; ok && day >= late && day < p.today && !offered[id] && p.states[id] != model.SlackDayPartial {
			owed[id] = true
		}
	}
	next.Owed, next.Offered = slices.Sorted(maps.Keys(owed)), p.offered
	return encodeJSON(next), nil
}

// slackDayKeys reads channel-day ids ("C1:2026-10-28"), skipping any that isn't one.
func slackDayKeys(ids []string) []model.SlackDayKey {
	var keys []model.SlackDayKey
	for _, id := range ids {
		channel, day, _ := strings.Cut(id, ":")
		at, err := time.Parse(time.DateOnly, day)
		if err == nil && model.ValidSlackID(channel) {
			keys = append(keys, model.SlackDayKey{ChannelID: channel, Day: at})
		}
	}
	return keys
}

// Backlog is more when more rows wait than one fold takes, and some when
// any waits, or a fold since the cursor (the backfill's) wrote a day.
func (k *slackAppKind) Backlog(ctx context.Context, src Source, cursor json.RawMessage) (Backlog, error) {
	var c slackAppCursor
	if err := json.Unmarshal(cursor, &c); err != nil || c.Seq == nil {
		return BacklogNone, fmt.Errorf("read Slack app cursor %s: not {\"seq\": n}", cursor)
	}
	if k.Feed == nil {
		return BacklogNone, errSlackAppUnwired
	}
	feed, err := k.Feed.GetFoundrySlackFeed(ctx, src.ConnectorID)
	if err != nil {
		return BacklogNone, err
	}
	switch waiting := feed.LastSeq - feed.FoldedSeq; {
	case waiting > int64(k.Budget.Rows):
		return BacklogMore, nil
	case waiting > 0:
		return BacklogSome, nil
	}
	days, err := k.Feed.FoundrySlackDaysAfter(ctx, feed.ID, *c.Seq)
	if err != nil || len(days) == 0 {
		return BacklogNone, err
	}
	return BacklogSome, nil
}

// slackMirror reads a feed's mirror days for a pass: each channel's days of
// the window, and the day read last, which its documents' replies read.
type slackMirror struct {
	o SlackFeedOptions
	// days are each channel's days of the window, oldest first, as the
	// store lists them.
	days map[string][]model.SlackFeedDay
	last struct {
		key string
		day *slackMirrorDay
	}
}

// open reads a day, once in a row.
func (m *slackMirror) open(ctx context.Context, d model.SlackFeedDay) (*slackMirrorDay, error) {
	if m.last.key == d.ObjectKey && m.last.day != nil {
		return m.last.day, nil
	}
	day, err := m.o.readSlackDay(ctx, d)
	if err != nil {
		return nil, err
	}
	m.last.key, m.last.day = d.ObjectKey, day
	return day, nil
}

// history passes the messages history would list of channel from oldest
// to latest (zero for none), newest first.
func (m *slackMirror) history(ctx context.Context, channel string, oldest, latest time.Time, each func(slackMessage) error) error {
	for _, d := range slices.Backward(m.days[channel]) {
		if d.Day.Add(24*time.Hour).Before(oldest) || !latest.IsZero() && d.Day.After(latest) {
			continue
		}
		day, err := m.open(ctx, d)
		if err != nil {
			return err
		}
		for _, msg := range day.top() {
			if msg.at.Before(oldest) || !latest.IsZero() && msg.at.After(latest) {
				continue
			}
			if err := each(msg); err != nil {
				return err
			}
		}
	}
	return nil
}

// replies are the replies the mirror holds of the thread parent starts, on
// its root's day, in time order, stopping once they are over room bytes.
func (m *slackMirror) replies(ctx context.Context, channel, parent string, room int) ([]slackMessage, error) {
	at, _ := slackTime(parent)
	i := slices.IndexFunc(m.days[channel], func(d model.SlackFeedDay) bool { return d.Day.Equal(model.SlackDay(at)) })
	if i < 0 {
		return nil, nil
	}
	day, err := m.open(ctx, m.days[channel][i])
	if err != nil {
		return nil, err
	}
	var replies []slackMessage
	size := 0
	for _, r := range day.thread(parent) {
		replies = append(replies, r)
		if size += len(r.Text) + slackMessageOverhead; size > room {
			break
		}
	}
	return replies, nil
}
