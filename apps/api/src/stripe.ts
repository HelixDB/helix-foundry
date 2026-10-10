import { AppError, assert } from "./security.js";
import type { ConnectorConfig } from "./connectors.js";
import { retryingFetch } from "./retrying-fetch.js";

// Tests point this at a local fixture; the key is never sent anywhere else.
export const stripeApi = { base: "https://api.stripe.com" };
// Pinned so normalized columns do not change with the account's default version.
const VERSION = "2024-06-20";
const MAX_PAGES = 20_000;

type StripeObject = {
  name: string;
  path: string;
  key: string;
  query?: Record<string, string>;
  // Child lists embedded in each parent row, fetched in full when truncated.
  parent?: { object: string; field: string; path: (id: string) => string };
};
export const stripeObjects: StripeObject[] = [
  { name: "customers", path: "/v1/customers", key: "customer_id" },
  { name: "products", path: "/v1/products", key: "product_id" },
  { name: "prices", path: "/v1/prices", key: "price_id" },
  { name: "coupons", path: "/v1/coupons", key: "coupon_id" },
  {
    name: "promotion_codes",
    path: "/v1/promotion_codes",
    key: "promotion_code_id",
  },
  {
    name: "subscriptions",
    path: "/v1/subscriptions",
    key: "subscription_id",
    query: { status: "all" },
  },
  {
    name: "subscription_items",
    path: "/v1/subscriptions",
    key: "subscription_item_id",
    query: { status: "all" },
    parent: {
      object: "subscription",
      field: "items",
      path: (id) => "/v1/subscription_items?subscription=" + id,
    },
  },
  { name: "invoices", path: "/v1/invoices", key: "invoice_id" },
  {
    name: "invoice_line_items",
    path: "/v1/invoices",
    key: "line_item_id",
    parent: {
      object: "invoice",
      field: "lines",
      path: (id) => `/v1/invoices/${id}/lines`,
    },
  },
  { name: "charges", path: "/v1/charges", key: "charge_id" },
  {
    name: "payment_intents",
    path: "/v1/payment_intents",
    key: "payment_intent_id",
  },
  { name: "refunds", path: "/v1/refunds", key: "refund_id" },
  { name: "disputes", path: "/v1/disputes", key: "dispute_id" },
  { name: "payouts", path: "/v1/payouts", key: "payout_id" },
  {
    name: "balance_transactions",
    path: "/v1/balance_transactions",
    key: "balance_transaction_id",
  },
];

// Reference fields become <object>_id columns so relationships are explicit.
const references: Record<string, string> = {
  customer: "customer_id",
  product: "product_id",
  price: "price_id",
  default_price: "price_id",
  coupon: "coupon_id",
  promotion_code: "promotion_code_id",
  subscription: "subscription_id",
  subscription_item: "subscription_item_id",
  invoice: "invoice_id",
  latest_invoice: "invoice_id",
  charge: "charge_id",
  latest_charge: "charge_id",
  payment_intent: "payment_intent_id",
  refund: "refund_id",
  payout: "payout_id",
  balance_transaction: "balance_transaction_id",
  payment_method: "payment_method_id",
  default_payment_method: "payment_method_id",
};
const timestamps = new Set([
  "created",
  "arrival_date",
  "available_on",
  "billing_cycle_anchor",
  "cancel_at",
  "canceled_at",
  "current_period_end",
  "current_period_start",
  "due_date",
  "effectively_at",
  "ended_at",
  "expires_at",
  "period_end",
  "period_start",
  "redeem_by",
  "start_date",
  "trial_end",
  "trial_start",
  "webhooks_delivered_at",
]);

export function normalizeStripe(object: StripeObject, row: any) {
  const out: Record<string, unknown> = {};
  for (const [field, value] of Object.entries(row)) {
    if (field === "object") continue;
    if (field === "id") out[object.key] = value;
    else if (
      (Object.hasOwn(references, field) ||
        (field === "source" && object.name === "balance_transactions")) &&
      (value === null || (value && typeof value !== "boolean"))
    )
      out[references[field] || "source_id"] =
        typeof value === "object" ? ((value as any)?.id ?? null) : value;
    else if (timestamps.has(field) && typeof value === "number")
      out[field] = new Date(value * 1000).toISOString();
    else out[field] = value;
  }
  // Line items carry prices in `price` (pinned version) or pricing details.
  if (object.name === "invoice_line_items" && !out.price_id)
    out.price_id = row.pricing?.price_details?.price ?? null;
  return out;
}

const relationshipColumns: Record<string, string[]> = {
  products: ["price_id"],
  prices: ["product_id"],
  promotion_codes: ["coupon_id", "customer_id"],
  subscriptions: ["customer_id", "invoice_id"],
  subscription_items: ["subscription_id", "price_id"],
  invoices: [
    "customer_id",
    "subscription_id",
    "charge_id",
    "payment_intent_id",
  ],
  invoice_line_items: [
    "invoice_id",
    "price_id",
    "subscription_id",
    "subscription_item_id",
  ],
  charges: [
    "customer_id",
    "invoice_id",
    "payment_intent_id",
    "balance_transaction_id",
  ],
  payment_intents: ["customer_id", "invoice_id", "charge_id"],
  refunds: ["charge_id", "payment_intent_id", "balance_transaction_id"],
  disputes: ["charge_id", "payment_intent_id"],
  payouts: ["balance_transaction_id"],
};

export function stripeRelationships() {
  const primary = new Map(stripeObjects.map((o) => [o.key, o.name]));
  return Object.entries(relationshipColumns).flatMap(([table, columns]) =>
    columns.map((column) => ({
      tableName: table,
      columnName: column,
      referencedTable: primary.get(column)!,
      referencedColumn: column,
    })),
  );
}

function stripeKey(c: ConnectorConfig) {
  const key = String(c.apiKey || "").trim();
  assert(
    /^(sk|rk)_(test|live)_[A-Za-z0-9]+$/.test(key),
    "Enter a Stripe secret or restricted key (sk_… or rk_…)",
  );
  return key;
}

async function request(key: string, path: string, params = {}) {
  const url = new URL(path, stripeApi.base);
  for (const [k, v] of Object.entries(params))
    url.searchParams.set(k, String(v));
  const response = await retryingFetch(url, {
    Authorization: "Bearer " + key,
    "Stripe-Version": VERSION,
  });
  const body: any = await response.json().catch(() => ({}));
  if (response.status === 401)
    throw new AppError(
      400,
      "Stripe rejected this key. Check it and test again.",
    );
  if (response.status === 403)
    throw new AppError(
      403,
      "This key cannot read that object. Grant read access in Stripe.",
    );
  if (!response.ok)
    throw new Error(
      `Stripe returned HTTP ${response.status}: ${body.error?.message || ""}`,
    );
  return body;
}

async function* list(
  key: string,
  path: string,
  query: Record<string, string> = {},
) {
  let after: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const body = await request(key, path, {
      ...query,
      limit: 100,
      ...(after ? { starting_after: after } : {}),
    });
    assert(Array.isArray(body.data), "Unexpected Stripe list response");
    for (const row of body.data) yield row;
    if (!body.has_more || !body.data.length) return;
    after = body.data.at(-1).id;
  }
  throw new Error(`Stripe pagination exceeded ${MAX_PAGES} pages`);
}

async function* rows(key: string, object: StripeObject) {
  if (!object.parent) {
    yield* list(key, object.path, object.query);
    return;
  }
  const { field, path } = object.parent;
  const parentKey = object.parent.object + "_id";
  for await (const parent of list(key, object.path, object.query)) {
    const embedded = parent[field];
    const children = embedded?.has_more
      ? list(key, path(parent.id))
      : embedded?.data || [];
    for await (const child of children)
      yield { [parentKey]: parent.id, ...child };
  }
}

function lookup(name: unknown) {
  const object = stripeObjects.find((o) => o.name === name);
  assert(object, "Choose a Stripe object");
  return object;
}

export async function discoverStripe(c: ConnectorConfig) {
  const key = stripeKey(c);
  const mode = key.includes("_live_") ? "live" : "test";
  if (c.table) {
    const object = lookup(c.table),
      sample: Record<string, unknown>[] = [];
    for await (const row of rows(key, object)) {
      sample.push(normalizeStripe(object, row));
      if (sample.length === 10) break;
    }
    const names = [...new Set(sample.flatMap((r) => Object.keys(r)))];
    return { mode, sample, columns: names.map((name) => ({ name })) };
  }
  // Probe each object once; restricted keys may only read some of them.
  const probes = await Promise.all(
    stripeObjects.map(async (o) => {
      try {
        await request(key, o.path, { ...o.query, limit: 1 });
        return true;
      } catch (e) {
        if (e instanceof AppError && e.statusCode === 403) return false;
        throw e;
      }
    }),
  );
  const tables = stripeObjects
    .filter((_, i) => probes[i])
    .map((o) => ({ name: o.name }));
  assert(
    tables.length,
    "This key cannot read any supported Stripe objects. Grant read access and test again.",
  );
  const readable = new Set(tables.map((t) => t.name));
  return {
    mode,
    tables,
    unavailable: stripeObjects
      .filter((o) => !readable.has(o.name))
      .map((o) => o.name),
    foreignKeys: stripeRelationships().filter(
      (k) => readable.has(k.tableName) && readable.has(k.referencedTable),
    ),
  };
}

export async function importStripe(
  c: ConnectorConfig,
  write: (row: unknown) => Promise<void>,
) {
  const key = stripeKey(c),
    object = lookup(c.table);
  for await (const row of rows(key, object))
    await write(normalizeStripe(object, row));
  return [
    { name: object.key, type: "varchar" },
    ...(object.parent
      ? [{ name: object.parent.object + "_id", type: "varchar" }]
      : [{ name: "created", type: "timestamp" }]),
  ];
}
