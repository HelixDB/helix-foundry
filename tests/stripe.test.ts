import { it, expect, beforeAll, afterAll } from "vitest";
import { createServer, type Server } from "node:http";
import { once } from "node:events";
import { readFile, rm } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";
import { discover, importSource } from "../apps/api/src/connectors.js";
import {
  normalizeStripe,
  stripeApi,
  stripeObjects,
} from "../apps/api/src/stripe.js";
import { sourceSyncDefaults } from "../apps/api/src/source-sync.js";
import { config } from "../apps/api/src/config.js";

const customers = Array.from({ length: 250 }, (_, i) => ({
  id: "cus_" + String(i).padStart(3, "0"),
  object: "customer",
  email: `c${i}@example.com`,
  created: 1_700_000_000 + i,
}));
const lines = (invoice: string, n: number) =>
  Array.from({ length: n }, (_, i) => ({
    id: `il_${invoice}_${i}`,
    object: "line_item",
    amount: 100 * (i + 1),
    // Line items can name their invoice, or leave it null.
    invoice: null,
    price: { id: "price_1", object: "price", product: "prod_1" },
  }));
const invoices = [
  {
    id: "in_1",
    object: "invoice",
    customer: "cus_001",
    subscription: "sub_1",
    created: 1_700_000_000,
    lines: { object: "list", data: lines("in_1", 2), has_more: false },
  },
  {
    id: "in_2",
    object: "invoice",
    customer: { id: "cus_002", object: "customer" },
    subscription: null,
    created: 1_700_000_100,
    lines: { object: "list", data: lines("in_2", 1), has_more: true },
  },
];
const seen: string[] = [];
let server: Server,
  throttled = false;

function page(rows: any[], u: URL) {
  const limit = Number(u.searchParams.get("limit") || 10),
    after = u.searchParams.get("starting_after"),
    start = after ? rows.findIndex((r) => r.id === after) + 1 : 0,
    data = rows.slice(start, start + limit);
  return { object: "list", data, has_more: start + limit < rows.length };
}

beforeAll(async () => {
  server = createServer((req, res) => {
    const u = new URL(req.url!, "http://localhost");
    seen.push(req.url!);
    res.setHeader("content-type", "application/json");
    const send = (status: number, body: unknown) => {
      res.statusCode = status;
      res.end(JSON.stringify(body));
    };
    if (req.headers.authorization === "Bearer rk_test_bad")
      return send(401, { error: { message: "Invalid API Key" } });
    expect(req.headers["stripe-version"]).toBe("2024-06-20");
    if (u.pathname === "/v1/customers") {
      // Simulate one rate-limited response mid-pagination.
      if (u.searchParams.get("starting_after") === "cus_099" && !throttled) {
        throttled = true;
        res.setHeader("retry-after", "0");
        return send(429, { error: { message: "Rate limited" } });
      }
      return send(200, page(customers, u));
    }
    if (u.pathname === "/v1/invoices") return send(200, page(invoices, u));
    if (u.pathname === "/v1/invoices/in_2/lines")
      return send(200, page(lines("in_2", 3), u));
    if (u.pathname === "/v1/payouts")
      return send(403, { error: { type: "permission_error" } });
    return send(200, { object: "list", data: [], has_more: false });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  stripeApi.base = "http://127.0.0.1:" + (server.address() as any).port;
});
afterAll(async () => {
  server.closeAllConnections();
  await new Promise<void>((r) => server.close(() => r()));
});

async function imported(table: string) {
  const scope = "stripe-" + randomUUID();
  try {
    const result = await importSource(scope, "stripe", {
      apiKey: "rk_test_abc",
      table,
    });
    const text = await readFile(
      resolve(config.dataDir, "incoming", scope, result.file),
      "utf8",
    );
    return {
      result,
      rows: text.trim()
        ? text
            .trim()
            .split("\n")
            .map((l) => JSON.parse(l))
        : [],
    };
  } finally {
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
}

it("lists the objects a restricted key can read, with relationships", async () => {
  const found: any = await discover("stripe", { apiKey: "rk_test_abc" });
  expect(found.mode).toBe("test");
  expect(found.tables.map((t: any) => t.name)).toContain("customers");
  expect(found.unavailable).toEqual(["payouts"]);
  expect(found.foreignKeys).toContainEqual({
    tableName: "invoices",
    columnName: "customer_id",
    referencedTable: "customers",
    referencedColumn: "customer_id",
  });
  expect(
    found.foreignKeys.some((k: any) => k.referencedTable === "payouts"),
  ).toBe(false);
  const preview: any = await discover("stripe", {
    apiKey: "rk_test_abc",
    table: "customers",
  });
  expect(preview.sample).toHaveLength(10);
  expect(preview.sample[0]).toMatchObject({
    customer_id: "cus_000",
    created: "2023-11-14T22:13:20.000Z",
  });
});

it("rejects malformed and invalid keys", async () => {
  await expect(discover("stripe", { apiKey: "pk_test_abc" })).rejects.toThrow(
    "restricted key",
  );
  await expect(discover("stripe", { apiKey: "rk_test_bad" })).rejects.toThrow(
    "rejected this key",
  );
});

it("imports every page, retrying rate limits", async () => {
  const { rows, result } = await imported("customers");
  expect(result.records).toBe(250);
  expect(new Set(rows.map((r) => r.customer_id)).size).toBe(250);
  expect(rows[0]).not.toHaveProperty("object");
  expect(throttled).toBe(true);
  expect(seen).toContain("/v1/customers?limit=100&starting_after=cus_199");
});

it("flattens references and fetches truncated child lists", async () => {
  const invoiceRows = (await imported("invoices")).rows;
  expect(invoiceRows[1]).toMatchObject({
    invoice_id: "in_2",
    customer_id: "cus_002",
  });
  const { rows } = await imported("invoice_line_items");
  expect(rows.map((r) => r.line_item_id)).toEqual([
    "il_in_1_0",
    "il_in_1_1",
    "il_in_2_0",
    "il_in_2_1",
    "il_in_2_2",
  ]);
  expect(rows[4]).toMatchObject({ invoice_id: "in_2", price_id: "price_1" });
  expect(rows.map((r) => r.invoice_id)).toEqual([
    "in_1",
    "in_1",
    "in_2",
    "in_2",
    "in_2",
  ]);
});

it("keeps relationship columns canonical when references are absent", () => {
  const object = (name: string) => stripeObjects.find((o) => o.name === name)!;
  const invoices = object("invoices");
  expect(
    normalizeStripe(invoices, {
      id: "in_1",
      subscription: null,
      customer: null,
    }),
  ).toEqual({ invoice_id: "in_1", subscription_id: null, customer_id: null });
  expect(normalizeStripe(invoices, { subscription: "sub_1" })).toEqual({
    subscription_id: "sub_1",
  });
  // An absent reference never clears an id that is already known, such as
  // the parent invoice added to each line item.
  expect(
    normalizeStripe(object("invoice_line_items"), {
      invoice_id: "in_1",
      id: "il_1",
      invoice: null,
      subscription: { object: "subscription" },
      price: "price_1",
    }),
  ).toEqual({
    invoice_id: "in_1",
    line_item_id: "il_1",
    subscription_id: null,
    price_id: "price_1",
  });
});

it("returns a schema for empty objects and refreshes hourly", async () => {
  const { result } = await imported("refunds");
  expect(result.records).toBe(0);
  expect(result.schema?.[0]).toEqual({ name: "refund_id", type: "varchar" });
  const defaults = sourceSyncDefaults("stripe");
  expect(defaults.sync.mode).toBe("snapshot");
  expect(new Date(defaults.nextRun!).getUTCMinutes()).toBe(0);
});
