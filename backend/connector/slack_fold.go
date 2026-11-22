package connector

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The day mirror of a Slack connector read through Helix's Slack app: each
// channel-day's messages, replies included, in one immutable object of the
// object store, indexed by foundry_slack_feed_days. A fold applies the
// feed's buffer rows to it.
//
// A buffer row's payload is JSON, by its kind:
//   - message, changed: a Slack message object (slack.StoredMessage, or a
//     history or replies page's message): ts, thread_ts, user, bot_id,
//     app_id, username, bot_profile {name, app_id}, subtype, text,
//     edited {ts}, reply_count, reply_users_count, latest_reply. Other
//     fields are ignored.
//   - deleted: {"deleted_ts", "thread_ts"} (slack.MessageDeleted).
//   - replied: {"ts", "reply_count", "reply_users_count", "latest_reply"}
//     (slack.ParentReplied).
//   - span: SlackSpan.
//   - complete: SlackDayClosed, on the row's channel and day.
//   - gap: anything (model.SlackFeedGap.Row's), on the row's channel and day.
//   - joined: {"joined_at"} (slack.MemberJoined).
//   - left, channel: anything; the channel is the row's.
//
// A row whose payload can't be read is skipped and logged: it can never be
// applied, and holding it would hold every row after it.

// SlackSpan is a span row's payload: a history page (ThreadTS "") or a
// replies page of the thread ThreadTS listed every message it holds from
// From to To, Slack ts both, inclusive, as Listed. A message the mirror
// holds in that span that the page lacks was deleted unseen, during a gap:
// the fold drops it.
type SlackSpan struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	ThreadTS string   `json:"thread_ts,omitempty"`
	Listed   []string `json:"listed"`
}

// SlackDayClosed is a complete row's payload: the row's channel-day is
// covered whole (the backfill read it, or events held it from its start),
// or, Lossy, it never will be: a gap the budget couldn't read again. Its
// document then says messages may be missing.
type SlackDayClosed struct {
	Lossy bool `json:"lossy,omitempty"`
}

// slackTombstone is the subtype of a deleted thread root that keeps its
// replies, as conversations.history lists it.
const slackTombstone = "tombstone"

// slackFoldAttempts bounds how many times a fold that lost its
// compare-and-set to another fold runs again.
const slackFoldAttempts = 3

// slackEntry is one message of a mirror day.
type slackEntry struct {
	Message slackMessage `json:"m"`
	// Top is a message history lists on this day: a thread root, one in no
	// thread, or a thread broadcast. Reply is a reply, a broadcast too, of a
	// thread whose root is on this day.
	Top   bool `json:"top,omitempty"`
	Reply bool `json:"reply,omitempty"`
	// Gone is deleted: never shown, and never revived by a row that comes late.
	Gone bool `json:"gone,omitempty"`
	// Rank orders its text: a row replaces it only when greater.
	Rank slackRank `json:"rank"`
	// RepliesSeq is the row its reply fields came from; 0 while only a new
	// message's event set them, which a page or message_replied overrides.
	RepliesSeq int64 `json:"replies_seq,omitempty"`
}

// slackRank orders versions of a message: the later edit, then an event
// over the backfill's page, then the later row.
type slackRank struct {
	Edited string `json:"edited,omitempty"`
	Event  bool   `json:"event,omitempty"`
	Seq    int64  `json:"seq"`
}

// less reports a ranked before b.
func (a slackRank) less(b slackRank) bool {
	ae, _ := slackTime(a.Edited)
	be, _ := slackTime(b.Edited)
	switch {
	case !ae.Equal(be):
		return ae.Before(be)
	case a.Event != b.Event:
		return b.Event
	}
	return a.Seq < b.Seq
}

// slackMirrorDay is one channel-day of the mirror, as read and as a fold changes it.
type slackMirrorDay struct {
	entries map[string]*slackEntry // by ts
	state   model.SlackDayState
	changed bool
}

// repliesFrom gives m src's reply fields, and a root's thread_ts once it has replies.
func (m *slackMessage) repliesFrom(src slackMessage) {
	m.ReplyCount, m.ReplyUsersCount, m.LatestReply = src.ReplyCount, src.ReplyUsersCount, src.LatestReply
	if src.ReplyCount > 0 || src.ThreadTS == src.TS && src.TS == m.TS {
		m.ThreadTS = m.TS
	}
}

// sorted are d's entries by time, then ts.
func (d *slackMirrorDay) sorted() []*slackEntry {
	return slices.SortedFunc(maps.Values(d.entries), func(a, b *slackEntry) int {
		return cmp.Or(a.Message.at.Compare(b.Message.at), strings.Compare(a.Message.TS, b.Message.TS))
	})
}

// top are the messages history lists on d, newest first, each with its
// reply fields: Slack's, else, while none came, what d holds of its thread.
func (d *slackMirrorDay) top() []slackMessage {
	var top []slackMessage
	for _, e := range slices.Backward(d.sorted()) {
		if !e.Top || e.Gone {
			continue
		}
		m := e.Message
		if e.RepliesSeq == 0 && m.ReplyCount == 0 {
			users := map[string]bool{}
			for _, r := range d.thread(m.TS) {
				m.ReplyCount++
				users[cmp.Or(r.User, r.BotID)] = true
				m.LatestReply = r.TS
			}
			if m.ReplyCount > 0 {
				m.ReplyUsersCount, m.ThreadTS = len(users), m.TS
			}
		}
		top = append(top, m)
	}
	return top
}

// thread are the replies d holds of the thread root starts, in time order.
func (d *slackMirrorDay) thread(root string) []slackMessage {
	var replies []slackMessage
	for _, e := range d.sorted() {
		if e.Reply && !e.Gone && e.Message.ThreadTS == root && e.Message.TS != root {
			replies = append(replies, e.Message)
		}
	}
	return replies
}

// encode is d as its object holds it: one entry a line, by time, gzipped;
// with the SHA-256 of the lines, and how many messages it shows and their
// text's bytes.
func (d *slackMirrorDay) encode() (object []byte, digest string, messages, size int) {
	var lines bytes.Buffer
	for _, e := range d.sorted() {
		lines.Write(encodeJSON(e))
		lines.WriteByte('\n')
		if !e.Gone {
			messages++
			size += len(e.Message.Text)
		}
	}
	sum := sha256.Sum256(lines.Bytes())
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write(lines.Bytes()) // a bytes.Buffer takes every write
	_ = w.Close()
	return gz.Bytes(), hex.EncodeToString(sum[:]), messages, size
}

// readSlackDay reads a mirror day's object.
func (o SlackFeedOptions) readSlackDay(ctx context.Context, day model.SlackFeedDay) (*slackMirrorDay, error) {
	body, err := o.Objects.Open(ctx, day.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("read the Slack day %s: %w", day.ObjectKey, err)
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("read the Slack day %s: %w", day.ObjectKey, err)
	}
	d := &slackMirrorDay{entries: map[string]*slackEntry{}, state: day.State}
	lines := bufio.NewReader(gz)
	for {
		line, err := lines.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e slackEntry
			var ok bool
			if json.Unmarshal(line, &e) == nil {
				e.Message.at, ok = slackTime(e.Message.TS)
			}
			if !ok {
				return nil, fmt.Errorf("read the Slack day %s: an entry is not a message", day.ObjectKey)
			}
			d.entries[e.Message.TS] = &e
		}
		switch {
		case errors.Is(err, io.EOF):
			return d, nil
		case err != nil:
			return nil, fmt.Errorf("read the Slack day %s: %w", day.ObjectKey, err)
		}
	}
}

// slackWindow is the first instant a connector keeping historyDays days
// holds: midnight UTC, historyDays before the day it was ChangeOverlap
// ago, as a pass counts it.
func slackWindow(historyDays int) time.Time {
	return slackNow().UTC().Add(-ChangeOverlap).Truncate(24*time.Hour).AddDate(0, 0, -historyDays)
}

// Fold applies the connector's waiting buffer rows, at most one Budget of
// them, to its feed's day mirror, and returns the feed as folded. Each
// channel-day it changes is written as a new object, then the store
// commits the fold under a compare-and-set on the feed's folded_seq, which
// fences a worker that lost its lease: a fold that lost to another runs
// again from the new folded_seq (its objects are the store's to record as
// replaced). Days before the window of historyDays leave the mirror. app
// names Helix's bot, whose messages are kept without their text.
//
// Rows apply in seq order (the payloads: see SlackSpan):
//   - message, changed: placed as Slack places them, a message in no thread
//     and a root on its day (Top), a reply on its root's day (Reply), a
//     broadcast both, none before the window. A message replaces the one held
//     only when its rank (slackRank) is greater, and never a tombstone with
//     anything but one; a tombstone replaces anything. Reply fields come
//     from the latest row that knows them: a page, a changed row or
//     message_replied, never a new message's event once one did.
//   - deleted: a root with replies becomes a tombstone that keeps them, as
//     history lists it; anything else is gone, and kept so, so a late row
//     never revives it.
//   - replied: the root's reply fields.
//   - span: what the page lacks of its span is gone.
//   - complete: the day is complete, or lossy; gap: partial again. Either is
//     ignored for a day the mirror doesn't hold.
//   - joined: the channel is covered from then, with a joined backfill of
//     the window before; left: the channel is purged.
//   - channel: nothing here; a change read lists the channels again.
func (o SlackFeedOptions) Fold(ctx context.Context, connectorID string, historyDays int, app SlackApp) (*model.SlackFeed, error) {
	if o.Feed == nil || o.Objects == nil {
		return nil, errSlackAppUnwired
	}
	for attempt := 1; ; attempt++ {
		feed, err := o.Feed.GetFoundrySlackFeed(ctx, connectorID)
		if err != nil {
			return nil, fmt.Errorf("read the Slack feed of connector %s: %w", connectorID, err)
		}
		rows, err := o.Feed.FoundrySlackFeedAfter(ctx, feed.ID, feed.FoldedSeq, o.Budget)
		if err != nil || len(rows) == 0 {
			return feed, err
		}
		fold, err := o.fold(ctx, feed, rows, historyDays, app)
		if err != nil {
			return nil, err
		}
		cerr := o.Feed.CommitFoundrySlackFold(ctx, fold)
		if cerr == nil {
			feed.FoldedSeq = fold.To
			return feed, nil
		}
		// Another fold may have won, or this one committed and its answer was lost.
		now, err := o.Feed.GetFoundrySlackFeed(ctx, connectorID)
		switch {
		case err != nil:
			return nil, errors.Join(cerr, err)
		case now.FoldedSeq == fold.To:
			return now, nil
		case now.FoldedSeq == fold.From || attempt == slackFoldAttempts:
			return nil, fmt.Errorf("commit the Slack fold of connector %s: %w", connectorID, cerr)
		}
	}
}

// slackFolding is one fold's state: the index it read, the days it read
// and changed, and the channels a left row purged.
type slackFolding struct {
	o      SlackFeedOptions
	feed   *model.SlackFeed
	index  map[model.SlackDayKey]model.SlackFeedDay
	days   map[model.SlackDayKey]*slackMirrorDay
	purged map[string]bool
	window time.Time
	ours   func(slackMessage) bool
}

// fold applies rows to the mirror and writes the days it changed: the
// commit of the fold.
func (o SlackFeedOptions) fold(ctx context.Context, feed *model.SlackFeed, rows []model.SlackFeedRow, historyDays int,
	app SlackApp) (model.SlackFold, error) {
	index, err := o.Feed.FoundrySlackDaysAfter(ctx, feed.ID, 0)
	if err != nil {
		return model.SlackFold{}, err
	}
	f := &slackFolding{o: o, feed: feed, index: map[model.SlackDayKey]model.SlackFeedDay{}, days: map[model.SlackDayKey]*slackMirrorDay{},
		purged: map[string]bool{}, window: slackWindow(historyDays), ours: slackOurs(app, o.HelixAppID)}
	for _, d := range index {
		f.index[d.SlackDayKey] = d
	}
	for _, row := range rows {
		applied, err := f.apply(ctx, row)
		if err != nil {
			return model.SlackFold{}, err
		}
		if !applied {
			log.From(ctx).Warn("a Slack feed row can't be read; it is skipped", "feed", feed.ID, "seq", row.Seq, "kind", row.Kind)
		}
	}
	fold := model.SlackFold{FeedID: feed.ID, From: feed.FoldedSeq, To: rows[len(rows)-1].Seq}
	for _, key := range slices.SortedFunc(maps.Keys(f.days), func(a, b model.SlackDayKey) int {
		return cmp.Or(strings.Compare(a.ChannelID, b.ChannelID), a.Day.Compare(b.Day))
	}) {
		d := f.days[key]
		if !d.changed {
			continue
		}
		object, digest, messages, size := d.encode()
		held, had := f.index[key]
		had = had && !f.purged[key.ChannelID]
		if had && held.Digest == digest && held.State == d.state {
			continue
		}
		day := model.SlackFeedDay{SlackDayKey: key, Digest: digest, Messages: messages, Bytes: size, State: d.state,
			ObjectKey: fmt.Sprintf("ws/%s/connectors/%s/slack-days/%s/%s/%s/%d-%s.jsonl.gz", feed.WorkspaceID, feed.ConnectorID, feed.ID,
				key.ChannelID, key.Day.Format(time.DateOnly), fold.To, id.New())}
		if err := o.Objects.Put(ctx, day.ObjectKey, bytes.NewReader(object)); err != nil {
			return model.SlackFold{}, fmt.Errorf("write the Slack day %s: %w", day.ObjectKey, err)
		}
		fold.Days = append(fold.Days, day)
		if had {
			fold.Replaced = append(fold.Replaced, held.ObjectKey)
		}
	}
	for _, d := range index {
		if d.Day.Before(f.window) && !f.purged[d.ChannelID] {
			fold.Dropped = append(fold.Dropped, d.SlackDayKey)
		}
	}
	return fold, nil
}

// held reports a channel-day the mirror holds.
func (f *slackFolding) held(key model.SlackDayKey) bool {
	_, indexed := f.index[key]
	return f.days[key] != nil || indexed && !f.purged[key.ChannelID]
}

// day is a channel-day of the mirror, read once, or a new one, partial.
func (f *slackFolding) day(ctx context.Context, key model.SlackDayKey) (*slackMirrorDay, error) {
	if d := f.days[key]; d != nil {
		return d, nil
	}
	d := &slackMirrorDay{entries: map[string]*slackEntry{}, state: model.SlackDayPartial}
	if held, ok := f.index[key]; ok && !f.purged[key.ChannelID] {
		var err error
		if d, err = f.o.readSlackDay(ctx, held); err != nil {
			return nil, err
		}
	}
	f.days[key] = d
	return d, nil
}

// apply applies one row; false when its payload can't be read.
func (f *slackFolding) apply(ctx context.Context, row model.SlackFeedRow) (bool, error) {
	key := model.SlackDayKey{ChannelID: row.ChannelID, Day: row.Day}
	switch row.Kind {
	case model.SlackRowMessage, model.SlackRowChanged:
		var m slackMessage
		var ok bool
		if json.Unmarshal(row.Payload, &m) == nil {
			m.at, ok = slackTime(m.TS)
		}
		if !ok {
			return false, nil
		}
		if slackSkipped[m.Subtype] {
			return true, nil
		}
		if f.ours(m) {
			m.Text = ""
		}
		authoritative := row.Kind == model.SlackRowChanged || row.Source == model.SlackFeedFromBackfill
		return true, f.upsert(ctx, row.ChannelID, m, slackRank{Edited: m.Edited.TS, Event: row.Source == model.SlackFeedFromEvent, Seq: row.Seq},
			authoritative)
	case model.SlackRowDeleted:
		var del struct {
			DeletedTS string `json:"deleted_ts"`
			ThreadTS  string `json:"thread_ts"`
		}
		if json.Unmarshal(row.Payload, &del) != nil {
			return false, nil
		}
		return f.delete(ctx, row.ChannelID, del.DeletedTS, del.ThreadTS)
	case model.SlackRowReplied:
		var replied slackMessage
		var ok bool
		if json.Unmarshal(row.Payload, &replied) == nil {
			replied.at, ok = slackTime(replied.TS)
		}
		if !ok {
			return false, nil
		}
		return true, f.replied(ctx, row.ChannelID, replied, row.Seq)
	case model.SlackRowSpan:
		var span SlackSpan
		if json.Unmarshal(row.Payload, &span) != nil {
			return false, nil
		}
		return f.span(ctx, row.ChannelID, span)
	case model.SlackRowComplete, model.SlackRowGap:
		var closed SlackDayClosed
		if row.Kind == model.SlackRowComplete && json.Unmarshal(row.Payload, &closed) != nil {
			return false, nil
		}
		if !f.held(key) || key.Day.Before(f.window) {
			return true, nil
		}
		d, err := f.day(ctx, key)
		if err != nil {
			return true, err
		}
		switch {
		case row.Kind == model.SlackRowGap:
			d.state = model.SlackDayPartial
		case closed.Lossy:
			d.state = model.SlackDayLossy
		default:
			d.state = model.SlackDayComplete
		}
		d.changed = true
		return true, nil
	case model.SlackRowJoined:
		var joined struct {
			JoinedAt time.Time `json:"joined_at"`
		}
		if json.Unmarshal(row.Payload, &joined) != nil || joined.JoinedAt.IsZero() {
			return false, nil
		}
		oldest := f.window
		if !oldest.Before(joined.JoinedAt) {
			oldest = joined.JoinedAt.Add(-time.Microsecond)
		}
		_, err := f.o.Feed.JoinFoundrySlackChannels(ctx, f.feed.ID, []model.SlackChannelJoin{{ChannelID: row.ChannelID,
			CoveredFrom: joined.JoinedAt.UTC(), Oldest: oldest}})
		return true, err
	case model.SlackRowLeft:
		if err := f.o.Feed.PurgeFoundrySlackChannel(ctx, f.feed.ID, row.ChannelID); err != nil {
			return true, err
		}
		f.purged[row.ChannelID] = true
		for k := range f.days {
			if k.ChannelID == row.ChannelID {
				delete(f.days, k)
			}
		}
		return true, nil
	}
	return true, nil // channel: the change read lists the channels again
}

// slackPlaces are the days a message lands on, at midnight UTC, zero for
// none: its own day when history lists it (top), and its thread root's day
// when it is a reply (reply). A broadcast is both.
func slackPlaces(m slackMessage) (top, reply time.Time) {
	root, ok := slackTime(m.ThreadTS)
	switch {
	case !ok || m.ThreadTS == m.TS:
		return model.SlackDay(m.at), time.Time{}
	case m.Subtype == "thread_broadcast":
		return model.SlackDay(m.at), model.SlackDay(root)
	}
	return time.Time{}, model.SlackDay(root)
}

// upsert keeps m, ranked rank, in channel's days, as Fold says.
func (f *slackFolding) upsert(ctx context.Context, channel string, m slackMessage, rank slackRank, authoritative bool) error {
	top, reply := slackPlaces(m)
	for _, place := range []struct {
		day time.Time
		top bool
	}{{top, true}, {reply, false}} {
		if place.day.IsZero() || place.day.Before(f.window) {
			continue
		}
		d, err := f.day(ctx, model.SlackDayKey{ChannelID: channel, Day: place.day})
		if err != nil {
			return err
		}
		e := d.entries[m.TS]
		switch {
		case e == nil:
			e = &slackEntry{Message: m, Rank: rank}
			if authoritative {
				e.RepliesSeq = rank.Seq
			}
			d.entries[m.TS] = e
		case e.Gone:
			continue
		default:
			tomb, held := m.Subtype == slackTombstone, e.Message.Subtype == slackTombstone
			if tomb && !held || tomb == held && e.Rank.less(rank) {
				old := e.Message
				e.Message, e.Rank = m, rank
				e.Message.repliesFrom(old)
			}
			if authoritative && rank.Seq > e.RepliesSeq {
				e.Message.repliesFrom(m)
				e.RepliesSeq = rank.Seq
			}
		}
		e.Top, e.Reply = e.Top || place.top, e.Reply || !place.top
		d.changed = true
	}
	return nil
}

// delete drops the message ts of thread (none when it is a root, or in no
// thread) from the days it lands on. A root with replies becomes a
// tombstone, as history lists one. A message the mirror doesn't hold is
// kept gone on the day it would land on, so a late row never revives it.
func (f *slackFolding) delete(ctx context.Context, channel, ts, thread string) (bool, error) {
	at, ok := slackTime(ts)
	if !ok {
		return false, nil
	}
	gone := slackMessage{TS: ts, ThreadTS: thread, at: at}
	top, reply := slackPlaces(gone)
	if thread != "" && thread != ts {
		// A deleted broadcast is on its own day too; a reply is held nowhere else.
		top = model.SlackDay(at)
	}
	found := false
	for _, day := range []time.Time{top, reply} {
		key := model.SlackDayKey{ChannelID: channel, Day: day}
		if day.IsZero() || day.Before(f.window) || !f.held(key) {
			continue
		}
		d, err := f.day(ctx, key)
		if err != nil {
			return true, err
		}
		e := d.entries[ts]
		if e == nil {
			continue
		}
		found = true
		switch {
		case e.Gone:
		case e.Top && (e.Message.ReplyCount > 0 || len(d.thread(ts)) > 0):
			// As conversations.history lists a deleted root with replies.
			tomb := slackMessage{TS: ts, ThreadTS: ts, User: "USLACKBOT", Subtype: slackTombstone, Text: "This message was deleted.", at: at}
			tomb.repliesFrom(e.Message)
			e.Message = tomb
		default:
			e.Gone, e.Message = true, gone
		}
		d.changed = true
	}
	if found {
		return true, nil
	}
	day := cmp.Or(reply, top)
	if day.Before(f.window) {
		return true, nil
	}
	d, err := f.day(ctx, model.SlackDayKey{ChannelID: channel, Day: day})
	if err != nil {
		return true, err
	}
	d.entries[ts], d.changed = &slackEntry{Message: gone, Gone: true}, true
	return true, nil
}

// replied gives a root the reply fields of a message_replied row seq.
func (f *slackFolding) replied(ctx context.Context, channel string, replied slackMessage, seq int64) error {
	key := model.SlackDayKey{ChannelID: channel, Day: model.SlackDay(replied.at)}
	if !f.held(key) || key.Day.Before(f.window) {
		return nil
	}
	d, err := f.day(ctx, key)
	if err != nil {
		return err
	}
	if e := d.entries[replied.TS]; e != nil && !e.Gone && seq > e.RepliesSeq {
		e.Message.repliesFrom(replied)
		e.RepliesSeq, d.changed = seq, true
	}
	return nil
}

// span drops what a page lacks of the span it listed: a history page's
// messages of each day it spans, or a replies page's replies of its thread.
func (f *slackFolding) span(ctx context.Context, channel string, span SlackSpan) (bool, error) {
	from, okFrom := slackTime(span.From)
	to, okTo := slackTime(span.To)
	root, okRoot := slackTime(span.ThreadTS)
	if !okFrom || !okTo || span.ThreadTS != "" && !okRoot {
		return false, nil
	}
	first, last := model.SlackDay(from), model.SlackDay(to)
	if span.ThreadTS != "" {
		first, last = model.SlackDay(root), model.SlackDay(root)
	}
	listed := map[string]bool{}
	for _, ts := range span.Listed {
		listed[ts] = true
	}
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		key := model.SlackDayKey{ChannelID: channel, Day: day}
		if day.Before(f.window) || !f.held(key) {
			continue
		}
		d, err := f.day(ctx, key)
		if err != nil {
			return true, err
		}
		for ts, e := range d.entries {
			at := e.Message.at
			inThread := span.ThreadTS == "" && e.Top || span.ThreadTS != "" && e.Reply && e.Message.ThreadTS == span.ThreadTS && ts != span.ThreadTS
			if e.Gone || !inThread || at.Before(from) || at.After(to) || listed[ts] {
				continue
			}
			e.Gone, e.Message = true, slackMessage{TS: ts, ThreadTS: e.Message.ThreadTS, at: at}
			d.changed = true
		}
	}
	return true, nil
}
