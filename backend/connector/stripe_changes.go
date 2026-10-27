package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var _ ChangeReader = (*stripeKind)(nil)

// stripeEventRetention is how far back Stripe lists events.
const stripeEventRetention = 30 * 24 * time.Hour

// stripeEventTables maps an event's object type to the table holding that
// object. Balance transactions have no events.
var stripeEventTables = map[string]string{
	"customer": "customers", "product": "products", "price": "prices", "coupon": "coupons",
	"promotion_code": "promotion_codes", "subscription": "subscriptions", "invoice": "invoices",
	"charge": "charges", "payment_intent": "payment_intents", "refund": "refunds",
	"dispute": "disputes", "payout": "payouts",
}

// stripeCursor is where the next change read begins.
type stripeCursor struct {
	// Event is the newest event read. It is empty when Stripe listed none,
	// and the next read then lists the events created since Created.
	Event string `json:"event,omitempty"`
	// Created is Event's time, or the read's own when there is no event, in Unix seconds.
	Created int64 `json:"created"`
	// BalanceSince is the creation time the next read of balance transactions starts from.
	BalanceSince int64 `json:"balance_since"`
}

type stripeEvent struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Created int64  `json:"created"`
	Data    struct {
		Object struct {
			Object string `json:"object"`
			ID     string `json:"id"`
			// Invoice is an invoice item's invoice, whose lines the item's events change.
			Invoice any `json:"invoice"`
		} `json:"object"`
	} `json:"data"`
}

type stripeEventPage struct {
	Data    []stripeEvent `json:"data"`
	HasMore bool          `json:"has_more"`
}

func (k *stripeKind) Capture(src Source, _ string) Capture {
	var settings StripeSettings
	if json.Unmarshal(orEmpty(src.Settings), &settings) != nil || !settings.Events {
		return CaptureFull
	}
	return CaptureEvents
}

func (k *stripeKind) Start(ctx context.Context, src Source, _ []Table) (Start, error) {
	now := time.Now()
	newest, err := k.eventPage(ctx, stripeHeader(src), url.Values{"limit": {"1"}})
	if err != nil {
		return Start{}, err
	}
	c := stripeCursor{Created: now.Unix(), BalanceSince: now.Add(-ChangeOverlap).Unix()}
	if len(newest.Data) > 0 {
		c.Event, c.Created = newest.Data[0].ID, newest.Data[0].Created
	}
	cursor, err := json.Marshal(c)
	return Start{Cursor: cursor}, err
}

// Changes re-fetches each object the events after the cursor name, once, at
// the pinned version: an event renders its object at the account's API version.
// Balance transactions have no events; those created since the cursor are read,
// and their status changes wait for reconcile.
func (k *stripeKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	start := time.Now()
	var prev stripeCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode stripe change cursor: %w", ErrCursorExpired)
	}
	if start.Sub(time.Unix(prev.Created, 0)) >= stripeEventRetention {
		return nil, ErrCursorExpired
	}
	header := stripeHeader(src)
	changed := stripeChanged{follow: map[string]bool{}, ids: map[string][]string{}, gone: map[string]map[string]bool{}}
	for _, t := range tables {
		changed.follow[t.Name] = true
	}
	newest, err := k.events(ctx, header, prev, changed.add)
	if err != nil {
		return nil, err
	}
	for _, o := range stripeObjects {
		for _, id := range changed.ids[o.name] {
			if changed.gone[o.name][id] {
				err = removeStripe(w, changed.follow, o.name, id)
			} else {
				err = k.refetch(ctx, header, o.name, id, changed.follow, w)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if changed.follow["balance_transactions"] {
		object, _ := stripeObject("balance_transactions")
		since := "created[gte]=" + strconv.FormatInt(prev.BalanceSince, 10)
		if err := k.list(ctx, header, "/v1/balance_transactions", since, func(row map[string]any) error {
			return w.Upsert(object.name, normalizeStripe(object, row))
		}); err != nil {
			return nil, err
		}
	}
	next := stripeCursor{Event: prev.Event, Created: prev.Created, BalanceSince: start.Add(-ChangeOverlap).Unix()}
	switch {
	case newest.ID != "":
		next.Event, next.Created = newest.ID, newest.Created
	case prev.Event == "":
		next.Created = start.Unix()
	}
	return json.Marshal(next)
}

// stripeChanged is what a read's events changed: per table, each object id in
// the order first named, and whether an event said it is gone.
type stripeChanged struct {
	follow map[string]bool
	ids    map[string][]string
	gone   map[string]map[string]bool
}

func (c *stripeChanged) add(e stripeEvent) {
	object := e.Data.Object
	table, id := stripeEventTables[object.Object], object.ID
	// customer.subscription.deleted is a subscription that ended; it can still be read.
	gone := strings.HasSuffix(e.Type, ".deleted") && table != "subscriptions"
	if object.Object == "invoiceitem" {
		table, gone = "invoices", false
		id, _ = object.Invoice.(string)
	}
	child, _, _ := stripeChildOf(table)
	if table == "" || id == "" || (!c.follow[table] && !c.follow[child]) {
		return
	}
	if c.gone[table] == nil {
		c.gone[table] = map[string]bool{}
	}
	if _, seen := c.gone[table][id]; !seen {
		c.ids[table] = append(c.ids[table], id)
	}
	c.gone[table][id] = c.gone[table][id] || gone
}

// events passes each event after the cursor to each, oldest first, and
// returns the newest. Paging with ending_before walks from the cursor toward
// newer events, but every page is still listed newest first.
func (k *stripeKind) events(ctx context.Context, header http.Header, c stripeCursor, each func(stripeEvent)) (stripeEvent, error) {
	if c.Event == "" {
		return k.eventsSince(ctx, header, c.Created-int64(ChangeOverlap/time.Second), each)
	}
	var newest stripeEvent
	before := c.Event
	for page := 0; ; page++ {
		if page == maxStripePages {
			return stripeEvent{}, fmt.Errorf("stripe events: more than %d pages", maxStripePages)
		}
		p, err := k.eventPage(ctx, header, url.Values{"limit": {"100"}, "ending_before": {before}})
		if err != nil {
			return stripeEvent{}, err
		}
		for i := len(p.Data) - 1; i >= 0; i-- {
			each(p.Data[i])
		}
		if len(p.Data) == 0 {
			return newest, nil
		}
		newest, before = p.Data[0], p.Data[0].ID
		if !p.HasMore {
			return newest, nil
		}
	}
}

// eventsSince is events for a cursor without an event: every event created
// since a time, listed newest first and so passed on only once all are read.
func (k *stripeKind) eventsSince(ctx context.Context, header http.Header, since int64, each func(stripeEvent)) (stripeEvent, error) {
	var all []stripeEvent
	query := url.Values{"limit": {"100"}, "created[gte]": {strconv.FormatInt(since, 10)}}
	for page := 0; ; page++ {
		if page == maxStripePages {
			return stripeEvent{}, fmt.Errorf("stripe events: more than %d pages", maxStripePages)
		}
		p, err := k.eventPage(ctx, header, query)
		if err != nil {
			return stripeEvent{}, err
		}
		all = append(all, p.Data...)
		if !p.HasMore || len(p.Data) == 0 {
			break
		}
		query.Set("starting_after", p.Data[len(p.Data)-1].ID)
	}
	for i := len(all) - 1; i >= 0; i-- {
		each(all[i])
	}
	if len(all) == 0 {
		return stripeEvent{}, nil
	}
	return all[0], nil
}

// eventPage lists one page of events. Stripe refuses an ending_before event
// it no longer lists with a 400 naming that parameter: the cursor expired.
func (k *stripeKind) eventPage(ctx context.Context, header http.Header, query url.Values) (stripeEventPage, error) {
	status, body, err := k.get(ctx, header, stripeEventsPath, query)
	if err == nil && status == http.StatusBadRequest && query.Has("ending_before") {
		var refusal struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &refusal) == nil && refusal.Error.Param == "ending_before" {
			return stripeEventPage{}, ErrCursorExpired
		}
	}
	if err := stripeRefusal(status, err, stripeEventsPath); err != nil {
		return stripeEventPage{}, err
	}
	var page stripeEventPage
	if err := json.Unmarshal(body, &page); err != nil {
		return stripeEventPage{}, fmt.Errorf("decode stripe events page: %w", err)
	}
	return page, nil
}

// refetch upserts one changed object as the full read writes it, deleting it
// when Stripe no longer has it. A parent's children are listed again in place of its old ones.
func (k *stripeKind) refetch(ctx context.Context, header http.Header, table, id string, follow map[string]bool, w ChangeWriter) error {
	status, body, err := k.get(ctx, header, "/v1/"+table+"/"+url.PathEscape(id), nil)
	if err == nil && status == http.StatusNotFound {
		return removeStripe(w, follow, table, id)
	}
	if err := stripeRefusal(status, err, "/v1/"+table); err != nil {
		return err
	}
	var row map[string]any
	if err := json.Unmarshal(body, &row); err != nil {
		return fmt.Errorf("decode stripe %s %s: %w", table, id, err)
	}
	if deleted, _ := row["deleted"].(bool); deleted {
		return removeStripe(w, follow, table, id)
	}
	if follow[table] {
		object, _ := stripeObject(table)
		if err := w.Upsert(table, normalizeStripe(object, row)); err != nil {
			return err
		}
	}
	child, column, ok := stripeChildOf(table)
	if !ok || !follow[child] {
		return nil
	}
	if err := w.DeleteWhere(child, column, id); err != nil {
		return err
	}
	object, _ := stripeObject(child)
	return k.children(ctx, header, child, row, func(r map[string]any) error {
		return w.Upsert(child, normalizeStripe(object, r))
	})
}

// removeStripe deletes a gone object's row and, for a parent, every child row it held.
func removeStripe(w ChangeWriter, follow map[string]bool, table, id string) error {
	if follow[table] {
		object, _ := stripeObject(table)
		if err := w.Delete(table, map[string]any{object.key: id}); err != nil {
			return err
		}
	}
	if child, column, ok := stripeChildOf(table); ok && follow[child] {
		return w.DeleteWhere(child, column, id)
	}
	return nil
}

// stripeChildOf is the table listed through table's rows, and the child
// column holding the parent's id.
func stripeChildOf(table string) (child, column string, ok bool) {
	for name, c := range stripeChildren {
		if c.parentPath == "/v1/"+table {
			return name, c.parentObject + "_id", true
		}
	}
	return "", "", false
}
