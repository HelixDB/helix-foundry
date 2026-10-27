package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

var _ ChangeReader = (*workosKind)(nil)

// workosEventRetention is how far back WorkOS lists events.
const workosEventRetention = 90 * 24 * time.Hour

// workosEventPage is the most events WorkOS returns at once.
const workosEventPage = 100

// workosEffect is how one event type changes the followed tables: the row of
// table its object names is re-fetched, or deleted when deleted is set.
// cascade clears the rows a delete leaves in other tables, which WorkOS
// doesn't report one by one; reset reads a table in full instead.
type workosEffect struct {
	table   string
	deleted bool
	cascade []workosCascade
	reset   []string
}

type workosCascade struct{ table, column string }

// workosEffects are the events a change read applies. Invitations, SSO
// connections and directories have incomplete events, so every sync reads
// them in full.
var workosEffects = map[string]workosEffect{
	"organization.created": {table: "organizations"},
	"organization.updated": {table: "organizations"},
	"organization.deleted": {table: "organizations", deleted: true,
		cascade: []workosCascade{{"organization_memberships", "organization_id"}}},
	"user.created": {table: "users"},
	"user.updated": {table: "users"},
	"user.deleted": {table: "users", deleted: true,
		cascade: []workosCascade{{"organization_memberships", "user_id"}}},
	"organization_membership.created": {table: "organization_memberships"},
	"organization_membership.updated": {table: "organization_memberships"},
	"organization_membership.deleted": {table: "organization_memberships", deleted: true},
	"dsync.user.created":              {table: "directory_users"},
	"dsync.user.updated":              {table: "directory_users"},
	"dsync.user.deleted": {table: "directory_users", deleted: true,
		cascade: []workosCascade{{"directory_group_members", "directory_user_id"}}},
	"dsync.group.created": {table: "directory_groups"},
	"dsync.group.updated": {table: "directory_groups"},
	"dsync.group.deleted": {table: "directory_groups", deleted: true,
		cascade: []workosCascade{{"directory_group_members", "directory_group_id"}}},
	"dsync.group.user_added":   {table: "directory_group_members"},
	"dsync.group.user_removed": {table: "directory_group_members", deleted: true},
	// Member rows don't hold their directory, so a deleted directory's members are found by a full read.
	"dsync.deleted": {cascade: []workosCascade{{"directory_users", "directory_id"}, {"directory_groups", "directory_id"}},
		reset: []string{"directory_group_members"}},
}

// workosEventTypes are the event types that change any followed table, sorted.
func workosEventTypes(follow map[string]bool) []string {
	var types []string
	for name, e := range workosEffects {
		touches := follow[e.table]
		for _, c := range e.cascade {
			touches = touches || follow[c.table]
		}
		for _, t := range e.reset {
			touches = touches || follow[t]
		}
		if touches {
			types = append(types, name)
		}
	}
	slices.Sort(types)
	return types
}

// workosCursor is where the next change read begins.
type workosCursor struct {
	// Event is the newest event read. It is empty after a full read, or when
	// WorkOS listed none, and the next read then lists the events created since Created.
	Event string `json:"event,omitempty"`
	// Created is Event's time, or the read's own when there is no event.
	Created time.Time `json:"created"`
}

type workosEvent struct {
	ID        string    `json:"id"`
	Event     string    `json:"event"`
	CreatedAt time.Time `json:"created_at"`
	Data      struct {
		ID string `json:"id"`
		// User and Group are a group membership event's pair.
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		Group struct {
			ID string `json:"id"`
		} `json:"group"`
	} `json:"data"`
}

func (k *workosKind) Capture(src Source, table string) Capture {
	var settings workosSettings
	if json.Unmarshal(orEmpty(src.Settings), &settings) != nil || !settings.Events {
		return CaptureFull
	}
	for _, e := range workosEffects {
		if e.table == table {
			return CaptureEvents
		}
	}
	return CaptureFull
}

// Start begins at the current time, which the first change read reaches back
// from by ChangeOverlap. WorkOS answers order=desc on /events with its oldest
// event, so the newest can't be asked for.
func (k *workosKind) Start(context.Context, Source, []Table) (Start, error) {
	cursor, err := json.Marshal(workosCursor{Created: time.Now().UTC()})
	return Start{Cursor: cursor}, err
}

// Changes applies the events after the cursor in order, then re-fetches each
// changed object once: a payload is not the listed object (a directory user's
// carries previous_attributes, a membership's lacks organization_name).
func (k *workosKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	start := time.Now().UTC()
	var prev workosCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode workos change cursor: %w", ErrCursorExpired)
	}
	if start.Sub(prev.Created) >= workosEventRetention {
		return nil, ErrCursorExpired
	}
	header := workosHeader(src)
	c := &workosChanges{w: w, follow: map[string]bool{}, ids: map[string][]string{}, pending: map[string]map[string]bool{}}
	for _, t := range tables {
		c.follow[t.Name] = true
	}
	newest, err := k.events(ctx, header, prev, workosEventTypes(c.follow), c.apply)
	if err != nil {
		return nil, err
	}
	if err := k.refetch(ctx, header, c); err != nil {
		return nil, err
	}
	next := workosCursor{Event: prev.Event, Created: prev.Created}
	switch {
	case newest.ID != "":
		next.Event, next.Created = newest.ID, newest.CreatedAt
	case prev.Event == "":
		next.Created = start
	}
	return json.Marshal(next)
}

// events passes each event after the cursor to each, in order, and returns
// the newest. A cursor without an event starts at its time instead; WorkOS
// takes a starting event or a starting time, not both.
func (k *workosKind) events(ctx context.Context, header http.Header, c workosCursor, types []string,
	each func(workosEvent) error) (workosEvent, error) {
	query := url.Values{"events": types, "order": {"asc"}, "limit": {fmt.Sprint(workosEventPage)}}
	if c.Event != "" {
		query.Set("after", c.Event)
	} else {
		query.Set("range_start", c.Created.Add(-ChangeOverlap).UTC().Format("2006-01-02T15:04:05Z"))
	}
	var newest workosEvent
	for page := 0; ; page++ {
		if page == maxWorkOSPages {
			return workosEvent{}, fmt.Errorf("workos events: more than %d pages", maxWorkOSPages)
		}
		events, status, err := k.eventPage(ctx, header, query)
		refusedStart := status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity
		if err != nil && page == 0 && refusedStart {
			// WorkOS refused where the cursor starts; if it still lists events, the cursor is what it refused.
			if _, _, listErr := k.eventPage(ctx, header, url.Values{"events": types, "limit": {"1"}}); listErr == nil {
				return workosEvent{}, ErrCursorExpired
			}
		}
		if err != nil {
			return workosEvent{}, err
		}
		for _, e := range events {
			if err := each(e); err != nil {
				return workosEvent{}, err
			}
		}
		if len(events) == 0 {
			return newest, nil
		}
		newest = events[len(events)-1]
		if len(events) < workosEventPage {
			return newest, nil
		}
		query.Del("range_start")
		query.Set("after", newest.ID)
	}
}

// eventPage lists one page of events, with the status WorkOS answered.
func (k *workosKind) eventPage(ctx context.Context, header http.Header, query url.Values) ([]workosEvent, int, error) {
	status, body, err := k.get(ctx, header, "/events", query)
	if err := workosRefusal(status, err, "/events"); err != nil {
		return nil, status, err
	}
	var page struct {
		Data []workosEvent `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, status, fmt.Errorf("decode workos events page: %w", err)
	}
	return page.Data, status, nil
}

// workosChanges applies one read's events to the followed tables.
type workosChanges struct {
	w      ChangeWriter
	follow map[string]bool
	// ids are the objects to re-fetch per table, in the order first named;
	// pending turns false for one deleted since.
	ids     map[string][]string
	pending map[string]map[string]bool
	// added are the member rows this read upserted. A cascade's DeleteWhere
	// reaches only rows from before the read, so these are deleted one by one.
	added []map[string]any
}

func (c *workosChanges) apply(e workosEvent) error {
	effect, ok := workosEffects[e.Event]
	if !ok {
		return nil
	}
	id := e.Data.ID
	var err error
	switch {
	case effect.table == "directory_group_members":
		err = c.member(e.Data.Group.ID, e.Data.User.ID, effect.deleted)
	case effect.deleted:
		err = c.remove(effect.table, id)
	case effect.table != "":
		c.refetchLater(effect.table, id)
	}
	if err != nil {
		return err
	}
	for _, cascade := range effect.cascade {
		if err := c.cascade(cascade.table, cascade.column, id); err != nil {
			return err
		}
	}
	for _, table := range effect.reset {
		if c.follow[table] {
			c.w.Reset(table, "a directory was deleted, and its group members don't name it")
		}
	}
	return nil
}

func (c *workosChanges) refetchLater(table, id string) {
	if !c.follow[table] || id == "" {
		return
	}
	if c.pending[table] == nil {
		c.pending[table] = map[string]bool{}
	}
	if _, seen := c.pending[table][id]; !seen {
		c.ids[table] = append(c.ids[table], id)
	}
	c.pending[table][id] = true
}

func (c *workosChanges) remove(table, id string) error {
	if _, seen := c.pending[table][id]; seen {
		c.pending[table][id] = false
	}
	if !c.follow[table] {
		return nil
	}
	object, _ := workosObject(table)
	return c.w.Delete(table, map[string]any{object.key: id})
}

func (c *workosChanges) member(group, user string, removed bool) error {
	const table = "directory_group_members"
	if !c.follow[table] {
		return nil
	}
	object, _ := workosObject(table)
	row := normalizeWorkOS(object, map[string]any{"id": user}, workosParents[table], group)
	if removed {
		return c.w.Delete(table, row)
	}
	c.added = append(c.added, row)
	return c.w.Upsert(table, row)
}

func (c *workosChanges) cascade(table, column, value string) error {
	if !c.follow[table] {
		return nil
	}
	if table == "directory_group_members" {
		kept := c.added[:0]
		for _, row := range c.added {
			if row[column] != value {
				kept = append(kept, row)
				continue
			}
			if err := c.w.Delete(table, row); err != nil {
				return err
			}
		}
		c.added = kept
	}
	return c.w.DeleteWhere(table, column, value)
}

// refetch reads each changed object once, in the order first named, and
// upserts it as the full read writes it; one WorkOS no longer has is deleted.
func (k *workosKind) refetch(ctx context.Context, header http.Header, c *workosChanges) error {
	for _, o := range workosObjects {
		for _, id := range c.ids[o.name] {
			if !c.pending[o.name][id] {
				continue
			}
			status, body, err := k.get(ctx, header, workosObjectPaths[o.name]+"/"+url.PathEscape(id), nil)
			if err == nil && status == http.StatusNotFound {
				if err := c.w.Delete(o.name, map[string]any{o.key: id}); err != nil {
					return err
				}
				continue
			}
			if err := workosRefusal(status, err, workosObjectPaths[o.name]); err != nil {
				return err
			}
			var row map[string]any
			if err := json.Unmarshal(body, &row); err != nil {
				return fmt.Errorf("decode workos %s %s: %w", o.name, id, err)
			}
			parent := workosParents[o.name]
			parentID, _ := row[parent.column].(string)
			if err := c.w.Upsert(o.name, normalizeWorkOS(o, row, parent, parentID)); err != nil {
				return err
			}
		}
	}
	return nil
}
