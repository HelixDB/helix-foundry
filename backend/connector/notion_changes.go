package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"time"
)

var _ ChangeReader = (*notionKind)(nil)

// notionCursor is where a change read begins: the rows of Tables last
// edited from Since, and the data sources shared when it was taken, by id,
// each with the table it was read into and its schema. Every read moves
// each table it follows to its own start, a table it resets too, since the
// full read of it comes after, so one time holds for all of them.
type notionCursor struct {
	Since   time.Time                     `json:"since"`
	Tables  []string                      `json:"tables"`
	Sources map[string]notionSourceCursor `json:"sources"`
}

type notionSourceCursor struct {
	Table  string `json:"table"`
	Schema string `json:"schema"`
}

// notionSince is where the next change read begins when this one begins at
// start: timeCursor's, to the minute Notion keeps an edit's time to.
func notionSince(start time.Time) time.Time { return timeCursor(start).Truncate(time.Minute) }

func notionCursorOf(since time.Time, tables []Table, sources []notionSource) (json.RawMessage, error) {
	c := notionCursor{Since: since, Tables: []string{}, Sources: make(map[string]notionSourceCursor, len(sources))}
	for _, t := range tables {
		c.Tables = append(c.Tables, t.Name)
	}
	for _, s := range sources {
		c.Sources[s.id] = notionSourceCursor{Table: s.table.Name, Schema: s.schema}
	}
	return json.Marshal(c)
}

// Capture follows pages and every data source's table, which a change
// read filters and sorts by last edit; Notion lists users by neither, and
// there are few.
func (k *notionKind) Capture(_ Source, table string) Capture {
	if table == "users" {
		return CaptureFull
	}
	return CaptureCursor
}

func (k *notionKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	since := notionSince(time.Now())
	sources, err := k.client(src.Secret).sources(ctx)
	if err != nil {
		return Start{}, err
	}
	cursor, err := notionCursorOf(since, tables, sources)
	return Start{Cursor: cursor}, err
}

// Changes writes what changed in the followed tables since the cursor:
//  1. A table the cursor doesn't follow is reset, and so is a data
//     source's table that held another data source's rows (the name moves
//     to another when its holder is unshared) or whose data source's schema
//     changed: its stored rows aren't what a full read would write now.
//  2. The trash, newest edit first, back to the cursor: a page trashed
//     since loses its rows, and the syncer its document with them. It
//     comes first, so a page restored since is upserted after.
//  3. A data source the cursor has that Notion no longer shares loses its
//     rows of the pages table, as a full read would leave them out.
//  4. Each shared data source's rows edited since, from one query: its
//     table's whole rows, and the pages table's (of their titles alone
//     where its table isn't followed).
//  5. The other pages a search lists, newest edit first, back to the cursor.
//
// Each page the pages table upserts has its document offered as a full
// read offers it (document); one Notion no longer finds loses its pages
// row. What moves no page's last edit waits for the daily reconcile: a
// page shared, unshared or trashed without one, a row moved out of its data
// source (its old table keeps it), a formula or rollup whose inputs are on
// other pages, and a select option renamed.
func (k *notionKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := notionSince(time.Now())
	var prev notionCursor
	if err := json.Unmarshal(cursor, &prev); err != nil || prev.Since.IsZero() {
		return nil, fmt.Errorf("decode notion change cursor: %w", ErrCursorExpired)
	}
	c := k.client(src.Secret)
	sources, err := c.sources(ctx)
	if err != nil {
		return nil, err
	}
	pages := false
	followed := map[string]string{} // the data sources' tables this read follows, by data source id
	for _, t := range tables {
		i := slices.IndexFunc(sources, func(s notionSource) bool { return s.table.Name == t.Name })
		var was notionSourceCursor
		if i >= 0 {
			was = prev.Sources[sources[i].id]
		}
		switch {
		case !slices.Contains(prev.Tables, t.Name):
			w.Reset(t.Name, "the table has no change cursor yet")
		case t.Name == "pages":
			pages = true
		case i < 0:
			w.Reset(t.Name, "Notion no longer lists its data source")
		case was.Table != t.Name:
			w.Reset(t.Name, "its data source's rows were not read into it before")
		case was.Schema != sources[i].schema:
			w.Reset(t.Name, "its data source's properties changed")
		default:
			followed[sources[i].id] = t.Name
		}
	}
	if !pages && len(followed) == 0 {
		return notionCursorOf(next, tables, sources)
	}
	titles := url.Values{"filter_properties": {"title"}}
	newest := func(trash bool, each func(p notionPage) error) error {
		filter := map[string]any{"property": "object", "value": "page"}
		if trash {
			filter["in_trash"] = true
		}
		err := c.search(ctx, filter, "descending", titles, func(item map[string]any) error {
			p, err := notionPageOf("/v1/search", item)
			switch {
			case err != nil:
				return err
			case p.edited.Before(prev.Since):
				return errNotionEnough
			}
			return each(p)
		})
		if errors.Is(err, errNotionEnough) {
			return nil
		}
		return err
	}
	err = newest(true, func(p notionPage) error {
		key := map[string]any{"page_id": p.id}
		if table, ok := followed[p.dataSourceID]; ok {
			if err := w.Delete(table, key); err != nil {
				return err
			}
		}
		if pages {
			return w.Delete("pages", key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if pages {
		for _, id := range slices.Sorted(maps.Keys(prev.Sources)) {
			if !slices.ContainsFunc(sources, func(s notionSource) bool { return s.id == id }) {
				if err := w.DeleteWhere("pages", "data_source_id", id); err != nil {
					return nil, err
				}
			}
		}
	}
	page := func(p notionPage) error {
		gone, err := c.document(ctx, p, w)
		switch {
		case err != nil:
			return err
		case gone:
			return w.Delete("pages", map[string]any{"page_id": p.id})
		}
		return w.Upsert("pages", p.row())
	}
	queried := map[string]bool{}
	for _, s := range sources {
		table, whole := followed[s.id]
		if !whole && !pages {
			continue
		}
		queried[s.id] = true
		query := titles
		if whole {
			query = nil
		}
		err := c.rows(ctx, s, query, prev.Since, func(p notionPage, item map[string]any) error {
			if whole {
				row, err := c.tableRow(ctx, s, p, item)
				if err != nil {
					return err
				}
				if err := w.Upsert(table, row); err != nil {
					return err
				}
			}
			if !pages {
				return nil
			}
			return page(p)
		})
		if err != nil {
			return nil, err
		}
	}
	if !pages {
		return notionCursorOf(next, tables, sources)
	}
	listed := map[string]bool{}
	err = newest(false, func(p notionPage) error {
		if queried[p.dataSourceID] || listed[p.id] {
			return nil
		}
		listed[p.id] = true
		return page(p)
	})
	if err != nil {
		return nil, err
	}
	return notionCursorOf(next, tables, sources)
}
