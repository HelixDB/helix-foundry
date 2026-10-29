package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

var _ ChangeReader = (*slackKind)(nil)

// slackFollowed are the tables read from conversations.history, which a
// change read reads again from a day on. Slack lists users, channels and
// members with no "changed since" filter, so every sync reads them in full.
var slackFollowed = map[string]bool{"messages": true, "channel_days": true}

// slackCursor is where the next change read begins: the UTC day it reads
// history again from, the channels whose history the read before it read,
// and the history_days that read kept.
type slackCursor struct {
	Day         string   `json:"day"` // YYYY-MM-DD
	Channels    []string `json:"channels"`
	HistoryDays int      `json:"history_days"`
}

func (k *slackKind) Capture(_ Source, table string) Capture {
	if slackFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

// Start is today's UTC day and the channels the full read reads the
// history of: an auth.test, and users.conversations a page per 200
// channels the app is in.
func (k *slackKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	p, err := k.pass(ctx, src, tables, nil, noDocuments{})
	if err != nil {
		return Start{}, err
	}
	channels, err := p.followed(ctx)
	if err != nil {
		return Start{}, err
	}
	next := slackCursor{Day: p.today, Channels: []string{}, HistoryDays: p.historyDays}
	for _, ch := range channels {
		next.Channels = append(next.Channels, ch.ID)
	}
	cursor, err := json.Marshal(next)
	return Start{Cursor: cursor}, err
}

// Changes reads each channel's history again from the start of the
// cursor's day: that whole day and every day since, so a message posted,
// edited or deleted on them, or a reply to a thread started on them, is
// read. Each such day's messages and row are deleted, one DeleteWhere on
// day per day whatever the channel, so the merge's cost doesn't grow with
// the channels, then written again from what Slack lists now: a deleted
// message loses its row, and a day left with none its row and document. A
// quiet channel costs one conversations.history call. The next cursor is
// today, so the first change read after midnight reads yesterday once more
// and writes its document, now that the day is over. A channel the cursor
// doesn't list (newly joined, or newly readable with a scope) is read from
// the window's start in place of any rows it had; one the app is no longer
// in, whose history it may no longer read, or gone since it was listed,
// loses its rows, as a full read leaves them out. Documents follow the
// full read's rules (channelDays), and a day whose document was wanted too
// near the deadline to fetch keeps the next cursor at it. A cursor kept
// with other history_days resets both tables, which the syncer then reads
// in full.
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
	from, err := time.Parse(time.DateOnly, prev.Day)
	if err != nil {
		return nil, fmt.Errorf("decode slack change cursor's day %q: %w", prev.Day, ErrCursorExpired)
	}
	rows := map[string]RowWriter{}
	for _, t := range tables {
		rows[t.Name] = slackUpserts{w: w, table: t.Name}
	}
	p, err := k.pass(ctx, src, tables, rows, w)
	if err != nil {
		return nil, err
	}
	channels, err := p.followed(ctx)
	if err != nil {
		return nil, err
	}
	next := slackCursor{Day: p.today, Channels: []string{}, HistoryDays: p.historyDays}
	if prev.HistoryDays != p.historyDays {
		for _, t := range tables {
			w.Reset(t.Name, "the days of history to keep changed")
		}
		for _, ch := range channels {
			next.Channels = append(next.Channels, ch.ID)
		}
		return json.Marshal(next)
	}
	// A cursor from before the window, or after today by another clock, reads the window or today.
	today := p.window.AddDate(0, 0, p.historyDays)
	if from.After(today) {
		from = today
	}
	if from.Before(p.window) {
		from = p.window
	}
	cascade := func(column string, value any) error {
		for _, t := range tables {
			if err := w.DeleteWhere(t.Name, column, value); err != nil {
				return err
			}
		}
		return nil
	}
	for day := from; !day.After(today); day = day.AddDate(0, 0, 1) {
		if err := cascade("day", day.Format(time.DateOnly)); err != nil {
			return nil, err
		}
	}
	listed, known := map[string]bool{}, map[string]bool{}
	for _, ch := range channels {
		listed[ch.ID] = true
	}
	for _, id := range prev.Channels {
		known[id] = true
		if !listed[id] {
			if err := cascade("channel_id", id); err != nil {
				return nil, err
			}
		}
	}
	for _, ch := range channels {
		since := from
		if !known[ch.ID] {
			if err := cascade("channel_id", ch.ID); err != nil {
				return nil, err
			}
			since = p.window
		}
		// A channel gone keeps the rows this read already upserted, as a full
		// read does, and leaves the cursor, so it is read from the window's
		// start if it is listed again.
		err := p.history(ctx, ch, since)
		if errors.Is(err, errChannelGone) {
			if err := cascade("channel_id", ch.ID); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		next.Channels = append(next.Channels, ch.ID)
	}
	if err := p.channelDays(ctx); err != nil {
		return nil, err
	}
	if p.late != "" && p.late < next.Day {
		next.Day = p.late
	}
	return json.Marshal(next)
}

// followed proves the token with auth.test, then lists the channels whose
// history a read reads: those the app is in, of each kind whose history the
// token's scopes allow.
func (p *slackPass) followed(ctx context.Context) ([]slackChannel, error) {
	if err := p.auth(ctx); err != nil {
		return nil, err
	}
	joined, err := p.joined(ctx)
	return slices.DeleteFunc(joined, func(ch slackChannel) bool { return !p.scopes.has(slackHistoryScope(ch.IsPrivate)) }), err
}

// slackUpserts writes a table's rows as a change read's upserts.
type slackUpserts struct {
	w     ChangeWriter
	table string
}

func (u slackUpserts) Write(row map[string]any) error { return u.w.Upsert(u.table, row) }
func (u slackUpserts) WriteJSON(row []byte) error     { return u.w.UpsertJSON(u.table, row) }
