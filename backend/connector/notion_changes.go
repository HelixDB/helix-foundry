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

// notionSearchLag is how far a change read's searches reach back before
// its cursor, past the queries' overlap: Notion's search is eventually
// consistent, its trash most of all, so an edit may join it, in its place
// by last edit, well after it was made. A page listed again costs little:
// its unchanged row merges to nothing, and a held version reads no Markdown.
const notionSearchLag = 30 * time.Minute

// notionCursor is where a change read begins: the rows of Tables last
// edited from Since, and the data sources shared when it was taken, by id,
// each with the table it was read into and its schema. Every read moves
// each table it follows to its own start, a table it resets too, since the
// full read of it comes after, so one time holds for all of them.
//
// Full is where the last full read of the pages table began, kept through
// change reads: a page made since was never ranked against its stream's
// cap. Trashed are the pages made before Full whose document the stream
// held when a change read since took them out for the trash, so one
// restored has its document back: at most as many as that full read kept.
type notionCursor struct {
	Since   time.Time                     `json:"since"`
	Full    time.Time                     `json:"full"`
	Tables  []string                      `json:"tables"`
	Sources map[string]notionSourceCursor `json:"sources"`
	Trashed []string                      `json:"trashed,omitempty"`
}

type notionSourceCursor struct {
	Table  string `json:"table"`
	Schema string `json:"schema"`
}

// notionSince is where the next change read begins when this one begins at
// start: timeCursor's, to the minute Notion keeps an edit's time to.
func notionSince(start time.Time) time.Time { return timeCursor(start).Truncate(time.Minute) }

func notionCursorOf(since, full time.Time, trashed []string, tables []Table, sources []notionSource) (json.RawMessage, error) {
	c := notionCursor{Since: since, Full: full, Tables: []string{}, Sources: make(map[string]notionSourceCursor, len(sources)), Trashed: trashed}
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
	listed, err := k.listed(ctx, k.client(src.Secret), src)
	if err != nil {
		return Start{}, err
	}
	cursor, err := notionCursorOf(since, since, nil, tables, notionSources(listed))
	return Start{Cursor: cursor}, err
}

// Changes writes what changed in the followed tables since the cursor:
//  1. A table the cursor doesn't follow is reset, and so is a data
//     source's table that held another data source's rows (the name moves
//     to another when its holder is unshared) or whose data source's schema
//     changed: its stored rows aren't what a full read would write now. A
//     data source the cursor has that the search of them leaves out is read
//     by id, since the search is eventually consistent: one Notion still
//     shares is read as if listed.
//  2. The trash, newest edit first, back to notionSearchLag before the
//     cursor: a page trashed since loses its rows, and the syncer its
//     document with them. It comes first, so a page restored since is
//     upserted after.
//  3. A data source the cursor has that Notion no longer shares (it
//     answers 404 or 403, or the data source is in the trash) loses its
//     rows of the pages table, as a full read would leave them out.
//  4. Each shared data source's rows edited since, from one query: its
//     table's whole rows, and the pages table's (of their titles alone
//     where its table isn't followed).
//  5. The other pages a search lists, newest edit first, back to
//     notionSearchLag before the cursor.
//
// Each page the pages table upserts has its document offered as a full
// read offers it (document) when the stream holds one, when it was made
// since the last full read of the pages table began, or when a change read
// since took it out for the trash with its document; one Notion no longer
// finds loses its pages row. What moves no page's last edit waits for the
// daily reconcile: a page shared, unshared or trashed without one, a row
// moved out of its data source (its old table keeps it), a formula or
// rollup whose inputs are on other pages, and a select option renamed.
func (k *notionKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := notionSince(time.Now())
	var prev notionCursor
	if err := json.Unmarshal(cursor, &prev); err != nil || prev.Since.IsZero() || prev.Full.IsZero() {
		return nil, fmt.Errorf("decode notion change cursor: %w", ErrCursorExpired)
	}
	c := k.client(src.Secret)
	listed, err := k.listed(ctx, c, src)
	if err != nil {
		return nil, err
	}
	var unshared []string // the cursor's data sources Notion no longer shares
	for _, id := range slices.Sorted(maps.Keys(prev.Sources)) {
		if slices.ContainsFunc(listed, func(l notionListed) bool { return l.id == id }) {
			continue
		}
		switch l, shared, err := c.dataSource(ctx, id); {
		case err != nil:
			return nil, err
		case shared:
			listed = append(listed, l)
		default:
			unshared = append(unshared, id)
		}
	}
	sources := notionSources(listed)
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
	// The pages table read in full after this read, if at all, ranks its
	// documents against the cap again.
	full, trashed := next, map[string]bool{}
	if pages {
		full = prev.Full
		for _, id := range prev.Trashed {
			trashed[id] = true
		}
	}
	done := func() (json.RawMessage, error) {
		return notionCursorOf(next, full, slices.Sorted(maps.Keys(trashed)), tables, sources)
	}
	if !pages && len(followed) == 0 {
		return done()
	}
	held := func(id string) bool {
		_, ok := w.Held(notionPagesStream.Name, id)
		return ok
	}
	titles := url.Values{"filter_properties": {"title"}}
	back := prev.Since.Add(-notionSearchLag)
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
			case p.edited.Before(back):
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
		if !pages {
			return nil
		}
		if p.created.Before(full) && held(p.id) {
			trashed[p.id] = true
		}
		return w.Delete("pages", key)
	})
	if err != nil {
		return nil, err
	}
	if pages {
		for _, id := range unshared {
			if err := w.DeleteWhere("pages", "data_source_id", id); err != nil {
				return nil, err
			}
		}
	}
	page := func(p notionPage) error {
		// A change read has no cap. A page made before the last full read
		// whose document the stream doesn't hold may be one that read left
		// past the cap (a workspace of more pages than the stream keeps), and
		// a document for it now would go at the next reconcile, extracted for
		// nothing: so only a page made since, or one taken out for the trash
		// with its document since, is offered one. An older page that has
		// just gained a body waits for the reconcile.
		if !held(p.id) && p.created.Before(full) && !trashed[p.id] {
			return w.Upsert("pages", p.row())
		}
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
		return done()
	}
	listedPages := map[string]bool{}
	err = newest(false, func(p notionPage) error {
		if queried[p.dataSourceID] || listedPages[p.id] {
			return nil
		}
		listedPages[p.id] = true
		return page(p)
	})
	if err != nil {
		return nil, err
	}
	return done()
}
