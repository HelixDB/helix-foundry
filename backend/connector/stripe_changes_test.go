package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventChanges is the ChangeWriter an events read fills: every call in
// order, and the upserted rows decoded as a snapshot holds them.
type eventChanges struct {
	noDocuments
	calls []string
	rows  map[string][]map[string]any
}

func (c *eventChanges) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.UpsertJSON(table, b)
}

func (c *eventChanges) UpsertJSON(table string, row []byte) error {
	var m map[string]any
	if err := json.Unmarshal(row, &m); err != nil {
		return err
	}
	if c.rows == nil {
		c.rows = map[string][]map[string]any{}
	}
	c.rows[table] = append(c.rows[table], m)
	c.calls = append(c.calls, "upsert "+table+" "+eventKey(table, m))
	return nil
}

func (c *eventChanges) Delete(table string, key map[string]any) error {
	c.calls = append(c.calls, "delete "+table+" "+eventKey(table, key))
	return nil
}

func (c *eventChanges) DeleteWhere(table, column string, value any) error {
	c.calls = append(c.calls, fmt.Sprintf("where %s %s=%v", table, column, value))
	return nil
}

func (c *eventChanges) Reset(table, _ string) { c.calls = append(c.calls, "reset "+table) }

// eventKey is a row's primary key values, as the calls record them.
func eventKey(table string, row map[string]any) string {
	if table == "directory_group_members" {
		return fmt.Sprintf("%v/%v", row["directory_group_id"], row["directory_user_id"])
	}
	for _, o := range append(slices.Clone(stripeObjects), workosObjects...) {
		if o.name == table {
			return fmt.Sprint(row[o.key])
		}
	}
	return "?"
}

func assertCalls(t *testing.T, c *eventChanges, want ...string) {
	t.Helper()
	if !slices.Equal(c.calls, want) {
		t.Fatalf("changes = %q, want %q", c.calls, want)
	}
}

func TestStripeChanges(t *testing.T) {
	ctx := context.Background()
	readsEvents := json.RawMessage(`{"mode":"test","events":true}`)
	reader := func(t *testing.T, url string) ChangeReader {
		t.Helper()
		return kindOf(t, catalogFor(url), "stripe").(ChangeReader)
	}
	source := Source{Secret: "sk_test_abc", Settings: readsEvents}
	cursorAt := func(event string, created time.Time) json.RawMessage {
		b, _ := json.Marshal(stripeCursor{Event: event, Created: created.Unix(), BalanceSince: created.Unix()})
		return b
	}
	tables := func(names ...string) []Table {
		var out []Table
		for _, name := range names {
			out = append(out, Table{Name: name})
		}
		return out
	}
	// event is one listed event naming object; extra adds fields to the object.
	event := func(id, typ, object, objectID string, created int64, extra string) string {
		return fmt.Sprintf(`{"id":%q,"object":"event","type":%q,"created":%d,"data":{"object":{"id":%q,"object":%q%s}}}`,
			id, typ, created, objectID, object, extra)
	}
	recent := time.Now().Add(-time.Hour)

	// A key without Events: Read still syncs, with every table read in full.
	t.Run("follows every table only when the key reads events", func(t *testing.T) {
		r := reader(t, "http://unused")
		for _, o := range stripeObjects {
			if got := r.Capture(source, o.name); got != CaptureEvents {
				t.Errorf("Capture(%s) = %s, want events", o.name, got)
			}
			if got := r.Capture(Source{Settings: json.RawMessage(`{"mode":"test"}`)}, o.name); got != CaptureFull {
				t.Errorf("Capture(%s) without events = %s, want full", o.name, got)
			}
		}
	})

	// Test and every sync probe events, so a key that loses Events: Read falls back to full reads.
	t.Run("records whether the key reads events", func(t *testing.T) {
		for status, want := range map[int]string{http.StatusOK: `{"mode":"test","events":true}`, http.StatusForbidden: `{"mode":"test"}`} {
			api := newAPIServer(t, map[string]int{"/v1/events": status}, nil)
			d, err := kindOf(t, catalogFor(api.url), "stripe").Discover(ctx, Source{Secret: "sk_test_abc", Settings: json.RawMessage(`{"mode":"test"}`)})
			if err != nil || string(d.Settings) != want || len(d.Tables) != len(stripeObjects) {
				t.Fatalf("events %d: Discover = (settings %s, %d tables, %v), want %s", status, d.Settings, len(d.Tables), err, want)
			}
			// The customer is told why change reads are off and what grants them.
			if warned := len(d.Warnings) == 1 && strings.Contains(d.Warnings[0], "Events: Read"); warned != (status == http.StatusForbidden) {
				t.Fatalf("events %d: warnings = %q", status, d.Warnings)
			}
		}
	})

	// Every event after the newest one at the start of a full read is the next change read's.
	t.Run("starts from the newest event", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("limit") != "1" || r.Header.Get("Stripe-Version") != stripeVersion {
					t.Errorf("newest event read as %s, version %q", r.URL.RawQuery, r.Header.Get("Stripe-Version"))
				}
				return http.StatusOK, `{"data":[` + event("evt_9", "customer.updated", "customer", "cus_1", 1700000000, "") + `],"has_more":true}`
			},
		})
		before := time.Now()
		start, err := reader(t, url).Start(ctx, source, tables("customers"))
		if err != nil {
			t.Fatal(err)
		}
		var c stripeCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil {
			t.Fatal(err)
		}
		if c.Event != "evt_9" || c.Created != 1700000000 {
			t.Fatalf("cursor = %+v, want evt_9", c)
		}
		if low, high := before.Add(-ChangeOverlap).Unix(), time.Now().Add(-ChangeOverlap).Unix(); c.BalanceSince < low || c.BalanceSince > high {
			t.Fatalf("balance transactions from %d, want the start less the overlap: %d to %d", c.BalanceSince, low, high)
		}
	})

	// An account with no events in Stripe's 30 days still gets a cursor; the next read lists by time.
	t.Run("starts from now when the account has no events", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) { return http.StatusOK, `{"data":[],"has_more":false}` },
		})
		before := time.Now().Unix()
		start, err := reader(t, url).Start(ctx, source, tables("customers"))
		if err != nil {
			t.Fatal(err)
		}
		var c stripeCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil {
			t.Fatal(err)
		}
		if c.Event != "" || c.Created < before || c.Created > time.Now().Unix() {
			t.Fatalf("cursor = %+v, want no event and the time of the start", c)
		}
	})

	// Paging with ending_before walks toward newer events, but each page is
	// newest first; an object named twice is fetched once, at our version,
	// since an event renders it at the account's.
	t.Run("pages events oldest first and re-fetches each object once", func(t *testing.T) {
		var mu sync.Mutex
		var pages []string
		fetched := map[string]int{}
		customer := func(r *http.Request) (int, string) {
			mu.Lock()
			defer mu.Unlock()
			if r.Header.Get("Stripe-Version") != stripeVersion {
				t.Errorf("%s fetched at version %q", r.URL.Path, r.Header.Get("Stripe-Version"))
			}
			fetched[r.URL.Path]++
			return http.StatusOK, fmt.Sprintf(`{"id":%q,"object":"customer","created":1700000000,"default_source":null}`, r.URL.Path[len("/v1/customers/"):])
		}
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				q := r.URL.Query()
				pages = append(pages, q.Get("limit")+" after "+q.Get("ending_before"))
				if q.Get("ending_before") == "evt_0" {
					return http.StatusOK, `{"data":[` + event("evt_2", "customer.created", "customer", "cus_2", 102, "") + `,` +
						event("evt_1", "customer.created", "customer", "cus_1", 101, "") + `],"has_more":true}`
				}
				return http.StatusOK, `{"data":[` + event("evt_3", "customer.updated", "customer", "cus_1", 103, "") + `],"has_more":false}`
			},
			"/v1/customers/cus_1": customer,
			"/v1/customers/cus_2": customer,
		})
		changes := &eventChanges{}
		next, err := reader(t, url).Changes(ctx, source, tables("customers"), cursorAt("evt_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(pages, []string{"100 after evt_0", "100 after evt_2"}) {
			t.Fatalf("event pages = %q", pages)
		}
		assertCalls(t, changes, "upsert customers cus_1", "upsert customers cus_2")
		if fetched["/v1/customers/cus_1"] != 1 {
			t.Fatalf("fetches = %v, want cus_1 once", fetched)
		}
		row := changes.rows["customers"][0]
		if _, kept := row["object"]; kept || row["created"] != "2023-11-14T22:13:20.000Z" {
			t.Fatalf("row = %v, want the full read's shape", row)
		}
		var c stripeCursor
		if err := json.Unmarshal(next, &c); err != nil || c.Event != "evt_3" || c.Created != 103 {
			t.Fatalf("next cursor = %s (%v), want evt_3", next, err)
		}
	})

	// A delete event needs no fetch; an object Stripe no longer has, or calls
	// deleted, is deleted too; a deleted draft invoice takes its lines.
	t.Run("deletes objects that are gone", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[` +
					event("evt_4", "invoice.deleted", "invoice", "in_1", 104, "") + `,` +
					event("evt_3", "coupon.updated", "coupon", "co_1", 103, "") + `,` +
					event("evt_2", "product.updated", "product", "prod_1", 102, "") + `,` +
					event("evt_1", "customer.deleted", "customer", "cus_1", 101, "") + `],"has_more":false}`
			},
			"/v1/products/prod_1": func(*http.Request) (int, string) {
				return http.StatusNotFound, `{"error":{"code":"resource_missing","param":"id"}}`
			},
			"/v1/coupons/co_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"id":"co_1","object":"coupon","deleted":true}`
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("customers", "products", "coupons", "invoices", "invoice_line_items"),
			cursorAt("evt_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "delete customers cus_1", "delete products prod_1", "delete coupons co_1",
			"delete invoices in_1", "where invoice_line_items invoice_id=in_1")
	})

	// customer.subscription.deleted is a subscription that ended: it stays, and
	// its items are listed again in place of the old ones.
	t.Run("keeps an ended subscription and replaces its items", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[` + event("evt_1", "customer.subscription.deleted", "subscription", "sub_1", 101, "") + `],"has_more":false}`
			},
			"/v1/subscriptions/sub_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"id":"sub_1","object":"subscription","status":"canceled","customer":"cus_1",
				  "items":{"object":"list","data":[{"id":"si_1","object":"subscription_item","subscription":"sub_1","price":{"id":"price_1","object":"price"}}],"has_more":false}}`
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("subscriptions", "subscription_items"), cursorAt("evt_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert subscriptions sub_1", "where subscription_items subscription_id=sub_1", "upsert subscription_items si_1")
		sub, item := changes.rows["subscriptions"][0], changes.rows["subscription_items"][0]
		if sub["status"] != "canceled" || sub["customer_id"] != "cus_1" || item["subscription_id"] != "sub_1" || item["price_id"] != "price_1" {
			t.Fatalf("subscription = %v, item = %v", sub, item)
		}
	})

	// Line items have no events: an invoice's change, or its invoice items',
	// lists its lines again, from their own endpoint when the embedded list is truncated.
	t.Run("replaces a changed invoice's lines", func(t *testing.T) {
		var mu sync.Mutex
		invoiceReads := 0
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[` + event("evt_2", "invoice.updated", "invoice", "in_1", 102, "") + `,` +
					event("evt_1", "invoiceitem.created", "invoiceitem", "ii_1", 101, `,"invoice":"in_1"`) + `],"has_more":false}`
			},
			"/v1/invoices/in_1": func(*http.Request) (int, string) {
				mu.Lock()
				invoiceReads++
				mu.Unlock()
				return http.StatusOK, `{"id":"in_1","object":"invoice","lines":{"object":"list","data":[],"has_more":true}}`
			},
			"/v1/invoices/in_1/lines": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[{"id":"il_1","object":"line_item","price":{"id":"price_1"}}],"has_more":false}`
			},
		})
		changes := &eventChanges{}
		_, err := reader(t, url).Changes(ctx, source, tables("invoices", "invoice_line_items"), cursorAt("evt_0", recent), changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert invoices in_1", "where invoice_line_items invoice_id=in_1", "upsert invoice_line_items il_1")
		if line := changes.rows["invoice_line_items"][0]; invoiceReads != 1 || line["invoice_id"] != "in_1" || line["price_id"] != "price_1" {
			t.Fatalf("invoice read %d times, line = %v", invoiceReads, line)
		}
	})

	// Balance transactions have no events: those created since the watermark
	// are read again, and the watermark moves to this read's start.
	t.Run("reads balance transactions created since the watermark", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) { return http.StatusOK, `{"data":[],"has_more":false}` },
			"/v1/balance_transactions": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("created[gte]") != "1700000000" {
					t.Errorf("balance transactions read with %s", r.URL.RawQuery)
				}
				return http.StatusOK, `{"data":[{"id":"txn_1","object":"balance_transaction","source":"ch_1"}],"has_more":false}`
			},
		})
		cursor, _ := json.Marshal(stripeCursor{Event: "evt_0", Created: recent.Unix(), BalanceSince: 1700000000})
		changes := &eventChanges{}
		before := time.Now()
		next, err := reader(t, url).Changes(ctx, source, tables("balance_transactions"), cursor, changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert balance_transactions txn_1")
		if changes.rows["balance_transactions"][0]["source_id"] != "ch_1" {
			t.Fatalf("row = %v", changes.rows["balance_transactions"][0])
		}
		var c stripeCursor
		if err := json.Unmarshal(next, &c); err != nil {
			t.Fatal(err)
		}
		if low, high := before.Add(-ChangeOverlap).Unix(), time.Now().Add(-ChangeOverlap).Unix(); c.Event != "evt_0" || c.BalanceSince < low || c.BalanceSince > high {
			t.Fatalf("next cursor = %+v, want evt_0 and the watermark at the read's start less the overlap", c)
		}
	})

	// Stripe lists 30 days of events; past that the read can't be complete, so it reads in full instead.
	t.Run("expires a cursor older than thirty days without asking", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){})
		_, err := reader(t, url).Changes(ctx, source, tables("customers"), cursorAt("evt_0", time.Now().Add(-31*24*time.Hour)), &eventChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})

	// Stripe refuses an ending_before event it no longer has with a 400 naming that parameter.
	t.Run("expires a cursor Stripe refuses", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) {
				return http.StatusBadRequest, `{"error":{"code":"resource_missing","param":"ending_before","type":"invalid_request_error"}}`
			},
		})
		_, err := reader(t, url).Changes(ctx, source, tables("customers"), cursorAt("evt_0", recent), &eventChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})

	// A table outside the read is read in full or not at all, so its events cost no fetch and write nothing.
	t.Run("leaves tables outside the read alone", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[` +
					event("evt_4", "charge.succeeded", "charge", "ch_1", 104, "") + `,` +
					event("evt_3", "invoice.updated", "invoice", "in_1", 103, "") + `,` +
					event("evt_2", "product.deleted", "product", "prod_1", 102, "") + `,` +
					event("evt_1", "customer.updated", "customer", "cus_1", 101, "") + `],"has_more":false}`
			},
			"/v1/customers/cus_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"id":"cus_1","object":"customer"}`
			},
		})
		changes := &eventChanges{}
		if _, err := reader(t, url).Changes(ctx, source, tables("customers"), cursorAt("evt_0", recent), changes); err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert customers cus_1")
	})

	// A cursor taken when Stripe listed no events has no event to page from,
	// so the next read lists every event since its time, less the overlap.
	t.Run("reads events since the cursor's time when it has no event", func(t *testing.T) {
		since := recent.Unix()
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/events": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("created[gte]") != fmt.Sprint(since-int64(ChangeOverlap/time.Second)) || q.Get("ending_before") != "" {
					t.Errorf("events read with %s", r.URL.RawQuery)
				}
				if q.Get("starting_after") == "" {
					return http.StatusOK, `{"data":[` + event("evt_2", "customer.created", "customer", "cus_2", since+2, "") + `],"has_more":true}`
				}
				return http.StatusOK, `{"data":[` + event("evt_1", "customer.created", "customer", "cus_1", since+1, "") + `],"has_more":false}`
			},
			"/v1/customers/cus_1": func(*http.Request) (int, string) { return http.StatusOK, `{"id":"cus_1","object":"customer"}` },
			"/v1/customers/cus_2": func(*http.Request) (int, string) { return http.StatusOK, `{"id":"cus_2","object":"customer"}` },
		})
		cursor, _ := json.Marshal(stripeCursor{Created: since, BalanceSince: since})
		changes := &eventChanges{}
		next, err := reader(t, url).Changes(ctx, source, tables("customers"), cursor, changes)
		if err != nil {
			t.Fatal(err)
		}
		assertCalls(t, changes, "upsert customers cus_1", "upsert customers cus_2")
		var c stripeCursor
		if err := json.Unmarshal(next, &c); err != nil || c.Event != "evt_2" || c.Created != since+2 {
			t.Fatalf("next cursor = %s (%v), want evt_2", next, err)
		}
	})
}
