package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

var _ ChangeReader = (*slackKind)(nil)

// slackFollowed are the tables a change read follows: what
// conversations.history and conversations.members list of each channel the
// app is in. Slack lists users and channels with no "changed since"
// filter, so every sync reads them in full.
var slackFollowed = map[string]bool{"messages": true, "channel_days": true, "channel_members": true}

// slackHistoryTables are the followed tables conversations.history fills.
var slackHistoryTables = []string{"messages", "channel_days"}

// slackCascades is the most channels whose rows a change read removes one
// at a time from a table, since the merge checks every stored row against
// each such DeleteWhere; past it, the read resets the table, which the
// syncer reads in full in the same sync. Tests lower it.
var slackCascades = 32

// slackHeldBytes bounds the message rows a change read holds until it
// knows which days changed; past it, the read upserts each row as it reads
// it and rewrites every day it read. Tests lower it.
var slackHeldBytes = 64 << 20

// slackCursor is where the next change read begins, as the read before it
// left Slack. Days and Members are null after a full read, which keeps no
// digests.
type slackCursor struct {
	// Day is the UTC day each channel's history is read again from, and
	// From an earlier day for a channel whose document of that day waited
	// near a deadline.
	Day  string            `json:"day"` // YYYY-MM-DD
	From map[string]string `json:"from,omitempty"`
	// Channels are those whose history was read, a window of HistoryDays.
	Channels    []string `json:"channels"`
	HistoryDays int      `json:"history_days"`
	// Days are a digest of each day's messages from Day on, over every
	// channel; a day without messages has none.
	Days map[string]string `json:"days"`
	// Members are a digest of each channel's members, of every channel the app is in.
	Members map[string]string `json:"members"`
}

func (k *slackKind) Capture(_ Source, table string) Capture {
	if slackFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

// Start is the UTC day it was ChangeOverlap ago and the channels whose
// history the full read reads, with no digests: an auth.test, and
// users.conversations a page per 200 channels the app is in.
func (k *slackKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	p, err := k.pass(ctx, src, tables, nil, noDocuments{})
	if err != nil {
		return Start{}, err
	}
	joined, err := p.followed(ctx)
	if err != nil {
		return Start{}, err
	}
	cursor, err := json.Marshal(slackCursor{Day: p.today, Channels: p.readable(joined), HistoryDays: p.historyDays})
	return Start{Cursor: cursor}, err
}

// Changes reads each channel the app is in again, its members beside its
// history from the cursor's day (readChannels): that whole day and every
// day since, so a message posted, edited or deleted on them, or a reply to
// a thread started on them, is read. A quiet channel costs one
// conversations.history and one conversations.members call, the two at
// once, and a quiet sync writes nothing:
//   - A day whose digest of every channel's messages is the cursor's keeps
//     its rows. Another loses them, one DeleteWhere on day whatever the
//     channels, and has them written again from what Slack lists now, so a
//     deleted message loses its row, and a day left with none its row and
//     document; the read holds its message rows until it knows
//     (slackHeldRows).
//   - A channel whose members' digest is the cursor's keeps them; another's
//     are written again, one DeleteWhere on channel_id each.
//
// The next cursor is today, so the first change read after midnight reads
// yesterday once more and offers its document, now that the day is over;
// documents follow the full read's rules (channelDays). A channel whose
// document of a day was wanted too near the deadline to fetch is read
// again from that day, for its documents alone, until it is fetched; the
// others are not held back. A channel the app is no longer in, whose
// history it may no longer read, or gone since it was listed, loses its
// rows, as a full read leaves them out.
//
// The read resets a table, which the syncer then reads in full in the same
// sync, where following it would cost more than that read or keep what a
// full read would not:
//   - messages and channel_days, for a channel whose history the cursor
//     doesn't list (newly joined, or newly readable with a scope): a full
//     read offers its days' documents under the stream's cap, newest first
//     among every channel's, where a change read has no cap. So do a cursor
//     kept with other history_days, and more than slackCascades channels
//     to remove;
//   - channel_members, after a full read, whose cursor has no digests to
//     compare, and past slackCascades channels to rewrite or remove. The
//     read still reads every channel's members, for the next cursor.
//
// Invisible here, until the daily reconcile reads in full: an edit or a
// deletion on an older day, a new reply to a thread started on one (its
// parent's reply count and its day's document), and the days that leave the
// window, whose rows stay until then.
func (k *slackKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	var prev slackCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode slack change cursor: %w", ErrCursorExpired)
	}
	since, err := time.Parse(time.DateOnly, prev.Day)
	if err != nil {
		return nil, fmt.Errorf("decode slack change cursor's day %q: %w", prev.Day, ErrCursorExpired)
	}
	lag := map[string]time.Time{}
	for id, day := range prev.From {
		if lag[id], err = time.Parse(time.DateOnly, day); err != nil {
			return nil, fmt.Errorf("decode slack change cursor's day %q of channel %s: %w", day, id, ErrCursorExpired)
		}
	}
	held := &slackHeldRows{w: w, rows: map[string]map[string][]map[string]any{}}
	dayRows := &slackDayRows{w: w}
	p, err := k.pass(ctx, src, tables, map[string]RowWriter{"messages": held, "channel_days": dayRows}, w)
	if err != nil {
		return nil, err
	}
	joined, err := p.followed(ctx)
	if err != nil {
		return nil, err
	}
	readable := p.readable(joined)
	next := slackCursor{Day: p.today, Channels: readable, HistoryDays: p.historyDays}
	history := slices.ContainsFunc(slackHistoryTables, func(t string) bool { return p.read[t] })
	known, reads := map[string]bool{}, map[string]bool{}
	for _, id := range prev.Channels {
		known[id] = true
	}
	for _, id := range readable {
		reads[id] = true
	}
	left := slices.DeleteFunc(slices.Clone(prev.Channels), func(id string) bool { return reads[id] })
	reset := ""
	switch {
	case !history:
	case prev.HistoryDays != p.historyDays:
		reset = "the days of history to keep changed"
	case slices.ContainsFunc(readable, func(id string) bool { return !known[id] }):
		reset = "the app reads the history of a channel it did not"
	case len(left) > slackCascades:
		reset = fmt.Sprintf("the app no longer reads the history of %d channels", len(left))
	}
	if reset != "" {
		// The full read reads them; this one reads the members alone.
		for _, t := range slackHistoryTables {
			p.read[t] = false
		}
	}
	// A cursor from before the window, or after today by another clock, reads the window or today.
	today := p.window.AddDate(0, 0, p.historyDays)
	if since.After(today) {
		since = today
	}
	if since.Before(p.window) {
		since = p.window
	}
	held.from = since.Format(time.DateOnly)
	members := map[string][]string{} // written by the members goroutine, read once it is done
	gone, err := p.readChannels(ctx, joined, func(ch slackChannel) time.Time {
		day, ok := lag[ch.ID]
		switch {
		case !ok || !day.Before(since):
			return since
		case day.Before(p.window):
			return p.window
		}
		return day
	}, func(ch slackChannel, ids []string) error {
		members[ch.ID] = ids
		return nil
	})
	if err != nil {
		return nil, err
	}
	next.Channels = slices.DeleteFunc(next.Channels, func(id string) bool { return gone[id] })
	if p.read["channel_members"] {
		if next.Members, err = slackMemberChanges(w, prev.Members, joined, members, gone); err != nil {
			return nil, err
		}
	}
	cascade := func(column string, value any) error {
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
	// Those whose history rows go: left, or gone since they were listed.
	remove := slices.DeleteFunc(slices.Clone(prev.Channels), func(id string) bool { return reads[id] && !gone[id] })
	switch {
	case !history:
		return json.Marshal(next)
	case reset == "" && len(remove) > slackCascades:
		reset = fmt.Sprintf("the app no longer reads the history of %d channels", len(remove))
	}
	if reset != "" {
		for _, t := range tables {
			if slices.Contains(slackHistoryTables, t.Name) {
				w.Reset(t.Name, reset)
			}
		}
		return json.Marshal(next)
	}
	for _, id := range remove {
		if err := cascade("channel_id", id); err != nil {
			return nil, err
		}
	}
	for id := range gone {
		p.forget(id)
	}
	held.forget(gone)
	digests := p.dayDigests(held.from)
	// A day the cursor has no digest of (any, after a full read) is
	// rewritten while it may hold rows: up to today, and a later one, of a
	// clock ahead of the read's, while Slack lists messages on it.
	unknown := func(day string) bool { return prev.Days == nil || day < prev.Day }
	candidates := slices.Collect(maps.Keys(digests))
	for day := range prev.Days {
		candidates = append(candidates, day)
	}
	for day := since; !day.After(today); day = day.AddDate(0, 0, 1) {
		candidates = append(candidates, day.Format(time.DateOnly))
	}
	dayRows.rewrite = map[string]bool{}
	for _, day := range candidates {
		switch {
		case day < held.from:
		case held.through, unknown(day) && (day <= p.today || digests[day] != ""), !unknown(day) && prev.Days[day] != digests[day]:
			dayRows.rewrite[day] = true
		}
	}
	for _, day := range slices.Sorted(maps.Keys(dayRows.rewrite)) {
		if err := cascade("day", day); err != nil {
			return nil, err
		}
	}
	if err := held.flush(dayRows.rewrite); err != nil {
		return nil, err
	}
	if err := p.channelDays(ctx); err != nil {
		return nil, err
	}
	if len(p.late) > 0 {
		next.From = p.late
	}
	next.Days = map[string]string{}
	for day, digest := range digests {
		if day >= next.Day {
			next.Days[day] = digest
		}
	}
	return json.Marshal(next)
}

// followed proves the token with auth.test, then lists the channels the
// app is in, of the types the token may list.
func (p *slackPass) followed(ctx context.Context) ([]slackChannel, error) {
	if err := p.auth(ctx); err != nil {
		return nil, err
	}
	return p.joined(ctx)
}

// readable are the ids of the channels of joined whose history the token's scopes allow.
func (p *slackPass) readable(joined []slackChannel) []string {
	ids := []string{}
	for _, ch := range joined {
		if p.scopes.has(slackHistoryScope(ch.IsPrivate)) {
			ids = append(ids, ch.ID)
		}
	}
	return ids
}

// dayDigests are a digest of each day's messages from from on, over every
// channel the pass read: the XOR of a SHA-256 of each channel day's id,
// count and digest (slackDay), so neither the order channels are read in
// nor Slack lists messages in counts.
func (p *slackPass) dayDigests(from string) map[string]string {
	sums := map[string][sha256.Size]byte{}
	for _, d := range p.days {
		if d.day < from {
			continue
		}
		h, sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%x", d.id(), d.count, d.sum)), sums[d.day]
		for i := range sum {
			sum[i] ^= h[i]
		}
		sums[d.day] = sum
	}
	digests := map[string]string{}
	for day, sum := range sums {
		digests[day] = hex.EncodeToString(sum[:8])
	}
	return digests
}

// slackMemberChanges writes what changed in the members of the channels
// the app is in (joined) since prev's digests, and returns theirs: a new
// channel's members are upserted; a channel's whose digest changed are
// removed, one DeleteWhere, and upserted again; and a channel the app
// left, or gone since it was listed, loses them. With no digests (prev is
// nil after a full read), or past slackCascades channels to remove from,
// it resets the table.
func slackMemberChanges(w ChangeWriter, prev map[string]string, joined []slackChannel, members map[string][]string,
	gone map[string]bool) (map[string]string, error) {
	next, rewrite := map[string]string{}, map[string]bool{}
	for _, ch := range joined {
		if gone[ch.ID] {
			continue
		}
		sum := sha256.Sum256([]byte(strings.Join(slices.Sorted(slices.Values(members[ch.ID])), "\x00")))
		next[ch.ID] = hex.EncodeToString(sum[:8])
		rewrite[ch.ID] = prev[ch.ID] != next[ch.ID]
	}
	var remove []string
	for _, id := range slices.Sorted(maps.Keys(prev)) {
		if _, kept := next[id]; !kept || rewrite[id] {
			remove = append(remove, id)
		}
	}
	switch {
	case prev == nil:
		w.Reset("channel_members", "a full read keeps no digest of each channel's members")
		return next, nil
	case len(remove) > slackCascades:
		w.Reset("channel_members", fmt.Sprintf("the members of %d channels changed", len(remove)))
		return next, nil
	}
	for _, id := range remove {
		if err := w.DeleteWhere("channel_members", "channel_id", id); err != nil {
			return nil, err
		}
	}
	for _, ch := range joined {
		if !rewrite[ch.ID] {
			continue
		}
		for _, user := range members[ch.ID] {
			if err := w.Upsert("channel_members", map[string]any{"channel_id": ch.ID, "user_id": user}); err != nil {
				return nil, err
			}
		}
	}
	return next, nil
}

// slackHeldRows holds a change read's message rows, by channel and day,
// until it knows the days that changed (flush). Rows of a day before from
// are of a day read again only for its documents, whose rows the read that
// wrote them left as they are, and are dropped. Past slackHeldBytes it
// upserts every row as it comes (through), and the read rewrites every day
// it read: the rows of a channel gone since it was listed already written
// then stay, as a full read's do.
type slackHeldRows struct {
	w       ChangeWriter
	from    string
	rows    map[string]map[string][]map[string]any
	bytes   int
	through bool
}

func (h *slackHeldRows) Write(row map[string]any) error {
	channel, _ := row["channel_id"].(string)
	day, _ := row["day"].(string)
	text, _ := row["text"].(string)
	switch {
	case day < h.from:
		return nil
	case h.through:
		return h.w.Upsert("messages", row)
	}
	if h.rows[channel] == nil {
		h.rows[channel] = map[string][]map[string]any{}
	}
	h.rows[channel][day] = append(h.rows[channel][day], row)
	if h.bytes += len(text) + slackMessageOverhead; h.bytes <= slackHeldBytes {
		return nil
	}
	h.through = true
	return h.flush(nil)
}

// WriteJSON holds a row the source encoded; the pass writes maps.
func (h *slackHeldRows) WriteJSON(raw []byte) error {
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	return h.Write(row)
}

// flush upserts the rows held of days, or every row held for nil, and holds none.
func (h *slackHeldRows) flush(days map[string]bool) error {
	for _, channel := range slices.Sorted(maps.Keys(h.rows)) {
		for _, day := range slices.Sorted(maps.Keys(h.rows[channel])) {
			if days != nil && !days[day] {
				continue
			}
			for _, row := range h.rows[channel][day] {
				if err := h.w.Upsert("messages", row); err != nil {
					return err
				}
			}
		}
	}
	h.rows, h.bytes = map[string]map[string][]map[string]any{}, 0
	return nil
}

// forget drops the rows held of channels gone since they were listed.
func (h *slackHeldRows) forget(gone map[string]bool) {
	for id := range gone {
		delete(h.rows, id)
	}
}

// slackDayRows upserts the channel_days rows of the days a change read rewrites.
type slackDayRows struct {
	w       ChangeWriter
	rewrite map[string]bool
}

func (d *slackDayRows) Write(row map[string]any) error {
	if day, _ := row["day"].(string); !d.rewrite[day] {
		return nil
	}
	return d.w.Upsert("channel_days", row)
}

// WriteJSON writes a row the source encoded; the pass writes maps.
func (d *slackDayRows) WriteJSON(raw []byte) error {
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	return d.Write(row)
}
