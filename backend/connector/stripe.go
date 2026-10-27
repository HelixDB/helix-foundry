package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// stripeVersion pins the API shape the sync reads.
const stripeVersion = "2024-06-20"

// stripeEventsPath is probed by Discover: a restricted key needs Events: Read
// for changes to be followed.
const stripeEventsPath = "/v1/events"

var stripeKeyRE = regexp.MustCompile(`^(sk|rk)_(test|live)_[A-Za-z0-9]+$`)

var stripeObjects = []saasTable{
	{"customers", "customer_id"},
	{"products", "product_id"},
	{"prices", "price_id"},
	{"coupons", "coupon_id"},
	{"promotion_codes", "promotion_code_id"},
	{"subscriptions", "subscription_id"},
	{"subscription_items", "subscription_item_id"},
	{"invoices", "invoice_id"},
	{"invoice_line_items", "line_item_id"},
	{"charges", "charge_id"},
	{"payment_intents", "payment_intent_id"},
	{"refunds", "refund_id"},
	{"disputes", "dispute_id"},
	{"payouts", "payout_id"},
	{"balance_transactions", "balance_transaction_id"},
}

// stripeListPaths is the list endpoint that proves an object readable. Items
// and line items are listed through their parents, so the parent's endpoint decides.
var stripeListPaths = map[string]string{
	"customers":            "/v1/customers",
	"products":             "/v1/products",
	"prices":               "/v1/prices",
	"coupons":              "/v1/coupons",
	"promotion_codes":      "/v1/promotion_codes",
	"subscriptions":        "/v1/subscriptions?status=all",
	"subscription_items":   "/v1/subscriptions?status=all",
	"invoices":             "/v1/invoices",
	"invoice_line_items":   "/v1/invoices",
	"charges":              "/v1/charges",
	"payment_intents":      "/v1/payment_intents",
	"refunds":              "/v1/refunds",
	"disputes":             "/v1/disputes",
	"payouts":              "/v1/payouts",
	"balance_transactions": "/v1/balance_transactions",
}

var stripeForeignKeys = referencedBy(stripeObjects, map[string][]string{
	"products":           {"price_id"},
	"prices":             {"product_id"},
	"promotion_codes":    {"coupon_id", "customer_id"},
	"subscriptions":      {"customer_id", "invoice_id"},
	"subscription_items": {"subscription_id", "price_id"},
	"invoices":           {"customer_id", "subscription_id", "charge_id", "payment_intent_id"},
	"invoice_line_items": {"invoice_id", "price_id", "subscription_id", "subscription_item_id"},
	"charges":            {"customer_id", "invoice_id", "payment_intent_id", "balance_transaction_id"},
	"payment_intents":    {"customer_id", "invoice_id", "charge_id"},
	"refunds":            {"charge_id", "payment_intent_id", "balance_transaction_id"},
	"disputes":           {"charge_id", "payment_intent_id"},
	"payouts":            {"balance_transaction_id"},
})

type stripeKind struct {
	base string
	http *http.Client
}

type StripeSettings struct {
	// Mode is live or test, read from the key.
	Mode string `json:"mode"`
	// Events is whether the key can read events, so a change sync can follow
	// them; Discover sets it.
	Events bool `json:"events,omitempty"`
}

func (k *stripeKind) Name() string        { return "stripe" }
func (k *stripeKind) DisplayName() string { return "Stripe" }

func (k *stripeKind) Prepare(src Source) (json.RawMessage, error) {
	key := strings.TrimSpace(src.Secret)
	if !stripeKeyRE.MatchString(key) {
		return nil, invalid("enter a Stripe secret or restricted key (sk_… or rk_…)")
	}
	var settings StripeSettings
	if err := json.Unmarshal(orEmpty(src.Settings), &settings); err != nil {
		return nil, invalid("Stripe settings are malformed")
	}
	settings.Mode = "test"
	if strings.Contains(key, "_live_") {
		settings.Mode = "live"
	}
	return json.Marshal(settings)
}

// Discover lists the readable objects and records in the settings whether the
// key reads events. A key that can't still syncs every table in full.
func (k *stripeKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var settings StripeSettings
	if err := json.Unmarshal(orEmpty(src.Settings), &settings); err != nil {
		return Discovery{}, invalid("Stripe settings are malformed")
	}
	header := stripeHeader(src)
	paths := append(uniqueValues(stripeListPaths), stripeEventsPath)
	readable, err := probeAll(ctx, paths, func(ctx context.Context, path string) (bool, error) {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		status, _, err := get(ctx, k.http, k.base+path+sep+"limit=1", header)
		switch {
		case err != nil:
			return false, refused(err, "could not reach Stripe; try again")
		case status == http.StatusUnauthorized:
			return false, refused(nil, "Stripe rejected this key; check it and test again")
		case status == http.StatusForbidden:
			return false, nil
		case status >= 300:
			return false, refused(nil, "Stripe returned HTTP %d; try again", status)
		}
		return true, nil
	})
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(stripeObjects, func(name string) bool { return readable[stripeListPaths[name]] }, stripeForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this key cannot read any supported Stripe objects; grant read access and test again")
	}
	settings.Events = readable[stripeEventsPath]
	if !settings.Events {
		d.Warnings = append(d.Warnings, "this key can't read events, so on Pro and Max every sync reads every object in full; "+
			"grant Events: Read to read only what changed")
	}
	if d.Settings, err = json.Marshal(settings); err != nil {
		return Discovery{}, fmt.Errorf("encode stripe settings: %w", err)
	}
	return d, nil
}

func (k *stripeKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 2) }

func stripeHeader(src Source) http.Header {
	return http.Header{
		"Authorization":  {"Bearer " + strings.TrimSpace(src.Secret)},
		"Stripe-Version": {stripeVersion},
	}
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

func uniqueValues(m map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range m {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// maxStripePages stops a runaway list.
const maxStripePages = 20_000

// stripeChildren are objects listed through a list embedded in each parent.
var stripeChildren = map[string]struct {
	parentPath, parentQuery, parentObject, field string
	path                                         func(parentID string) string
}{
	"subscription_items": {"/v1/subscriptions", "status=all", "subscription", "items",
		func(id string) string { return "/v1/subscription_items?subscription=" + url.QueryEscape(id) }},
	"invoice_line_items": {"/v1/invoices", "", "invoice", "lines",
		func(id string) string { return "/v1/invoices/" + url.PathEscape(id) + "/lines" }},
}

// stripeReferences renames expandable references to the referenced table's key.
var stripeReferences = map[string]string{
	"customer": "customer_id", "product": "product_id", "price": "price_id", "default_price": "price_id",
	"coupon": "coupon_id", "promotion_code": "promotion_code_id", "subscription": "subscription_id",
	"subscription_item": "subscription_item_id", "invoice": "invoice_id", "latest_invoice": "invoice_id",
	"charge": "charge_id", "latest_charge": "charge_id", "payment_intent": "payment_intent_id",
	"refund": "refund_id", "payout": "payout_id", "balance_transaction": "balance_transaction_id",
	"payment_method": "payment_method_id", "default_payment_method": "payment_method_id",
}

// stripeTimestamps are Unix-second fields written as ISO 8601.
var stripeTimestamps = map[string]bool{
	"created": true, "arrival_date": true, "available_on": true, "billing_cycle_anchor": true, "cancel_at": true,
	"canceled_at": true, "current_period_end": true, "current_period_start": true, "due_date": true,
	"effectively_at": true, "ended_at": true, "expires_at": true, "period_end": true, "period_start": true,
	"redeem_by": true, "start_date": true, "trial_end": true, "trial_start": true, "webhooks_delivered_at": true,
}

func (k *stripeKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return table, k.copyRows(ctx, src, table.Name, w)
}

// stripeObject is the object a table holds; ok is false for a table Stripe doesn't have.
func stripeObject(table string) (saasTable, bool) {
	for _, o := range stripeObjects {
		if o.name == table {
			return o, true
		}
	}
	return saasTable{}, false
}

func (k *stripeKind) copyRows(ctx context.Context, src Source, table string, w RowWriter) error {
	object, ok := stripeObject(table)
	if !ok {
		return invalid("Stripe has no object %q", table)
	}
	header := stripeHeader(src)
	write := func(row map[string]any) error { return w.Write(normalizeStripe(object, row)) }
	child, ok := stripeChildren[table]
	if !ok {
		path, query, _ := strings.Cut(stripeListPaths[table], "?")
		return k.list(ctx, header, path, query, write)
	}
	return k.list(ctx, header, child.parentPath, child.parentQuery, func(parent map[string]any) error {
		return k.children(ctx, header, table, parent, write)
	})
}

// children passes each of one parent's child rows to write, with the parent's
// id set: from the list embedded in the parent, or from the child's own
// endpoint when that list is truncated.
func (k *stripeKind) children(ctx context.Context, header http.Header, table string, parent map[string]any,
	write func(map[string]any) error) error {
	child := stripeChildren[table]
	parentID, _ := parent["id"].(string)
	withParent := func(row map[string]any) error {
		if _, set := row[child.parentObject+"_id"]; !set {
			row[child.parentObject+"_id"] = parentID
		}
		return write(row)
	}
	embedded, _ := parent[child.field].(map[string]any)
	if more, _ := embedded["has_more"].(bool); more {
		path, query, _ := strings.Cut(child.path(parentID), "?")
		return k.list(ctx, header, path, query, withParent)
	}
	items, _ := embedded["data"].([]any)
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			if err := withParent(row); err != nil {
				return err
			}
		}
	}
	return nil
}

// get sends one GET of path with query, retrying rate limits.
func (k *stripeKind) get(ctx context.Context, header http.Header, path string, query url.Values) (int, []byte, error) {
	target := k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return fetch(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = header.Clone()
		}
		return req, err
	})
}

// stripeRefusal is the error for a read of path that failed; nil when it succeeded.
func stripeRefusal(status int, err error, path string) error {
	switch {
	case err != nil:
		return refused(err, "could not reach Stripe; the sync will retry")
	case status == http.StatusUnauthorized:
		return refused(nil, "Stripe rejected this key; update the connector")
	case status == http.StatusForbidden:
		return refused(nil, "this key can no longer read %s; grant read access in Stripe", path)
	case status >= 300:
		return refused(nil, "Stripe returned HTTP %d for %s", status, path)
	}
	return nil
}

// list pages through a Stripe list endpoint, 100 objects at a time.
func (k *stripeKind) list(ctx context.Context, header http.Header, path, rawQuery string, each func(map[string]any) error) error {
	after := ""
	for page := 0; ; page++ {
		if page == maxStripePages {
			return fmt.Errorf("stripe %s: more than %d pages", path, maxStripePages)
		}
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return err
		}
		query.Set("limit", "100")
		if after != "" {
			query.Set("starting_after", after)
		}
		status, body, err := k.get(ctx, header, path, query)
		if err := stripeRefusal(status, err, path); err != nil {
			return err
		}
		var listPage struct {
			Data    []map[string]any `json:"data"`
			HasMore bool             `json:"has_more"`
		}
		if err := json.Unmarshal(body, &listPage); err != nil {
			return fmt.Errorf("decode stripe %s page: %w", path, err)
		}
		for _, row := range listPage.Data {
			if err := each(row); err != nil {
				return err
			}
		}
		if !listPage.HasMore || len(listPage.Data) == 0 {
			return nil
		}
		after, _ = listPage.Data[len(listPage.Data)-1]["id"].(string)
	}
}

// stripeSignedLinks are links Stripe signs afresh on every read: kept, every
// read of an invoice or charge would look like a change to it.
var stripeSignedLinks = map[string]bool{"hosted_invoice_url": true, "invoice_pdf": true, "receipt_url": true}

// normalizeStripe renames id to the object's key, collapses expandable
// references to their ids and writes timestamps as ISO 8601.
func normalizeStripe(object saasTable, row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for field, value := range row {
		column, isReference := stripeReferences[field]
		if field == "source" && object.name == "balance_transactions" {
			column, isReference = "source_id", true
		}
		switch {
		case field == "object", stripeSignedLinks[field]:
		case field == "id":
			out[object.key] = value
		case isReference && (value == nil || truthy(value)):
			id := value
			if expanded, ok := value.(map[string]any); ok {
				id = expanded["id"]
			}
			if id != nil || out[column] == nil {
				out[column] = id
			}
		case stripeTimestamps[field] && isNumber(value):
			out[field] = time.Unix(int64(value.(float64)), 0).UTC().Format("2006-01-02T15:04:05.000Z")
		default:
			out[field] = value
		}
	}
	if object.name == "invoice_line_items" && !truthy(out["price_id"]) {
		out["price_id"] = nil
		if pricing, ok := row["pricing"].(map[string]any); ok {
			if details, ok := pricing["price_details"].(map[string]any); ok && details["price"] != nil {
				out["price_id"] = details["price"]
			}
		}
	}
	return out
}

// truthy is JavaScript truthiness for decoded JSON, minus booleans, which a
// reference field never holds.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil, bool:
		return false
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}

func isNumber(v any) bool {
	_, ok := v.(float64)
	return ok
}
