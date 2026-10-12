import { beforeAll, afterAll, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { ingestRows, seedDemo } from "../apps/api/src/datasets.js";
import { onboardingState } from "../apps/api/src/onboarding-state.js";
import { storedDiscovery } from "../apps/api/src/source-discovery.js";
import {
  confirmOntology,
  entityName,
  ontologyBundle,
  suggestOntology,
} from "../apps/api/src/discovery.js";
import { pipelineId, validateBundle } from "../apps/api/src/assistant.js";
import { config } from "../apps/api/src/config.js";
import {
  BundleSchema,
  SuggestedOntologySchema,
  type SuggestedOntology,
} from "../packages/shared/src/index.js";

const store = new MemoryStore(),
  scope = "discovery_" + Date.now(),
  other = scope + "_declared",
  named = scope + "_named",
  hops = scope + "_hops";
const scopes = [scope, other, named, hops];
let n = 0;
// A fresh workspace holding the given tables; a table with a source is a connector table.
async function suggestFor(
  tables: {
    name: string;
    rows: Record<string, unknown>[];
    kind?: string;
    connection?: string;
    discovery?: unknown;
  }[],
) {
  const s = `${scope}_${n++}`;
  scopes.push(s);
  await workspace(s);
  for (const t of tables) {
    const source = t.kind
      ? await transaction(store, s, async (tx) =>
          tx.put(
            resource(s, "source", t.name, {
              kind: t.kind,
              connectionId: t.connection || t.kind,
              discovery: t.discovery,
            }),
          ),
        )
      : undefined;
    await ingestRows(store, s, t.name, t.rows, source?.id);
  }
  const state = await onboardingState(store, s);
  const suggestion = await suggestOntology(store, s, {
    datasets: state.datasets,
    sources: state.sources,
    signature: state.signature,
  });
  return {
    suggestion,
    state,
    links: suggestion.links.map(
      (l) => `${l.from}.${l.fromColumn}->${l.to}.${l.toColumn}`,
    ),
  };
}
const range = <T>(count: number, row: (i: number) => T) =>
  Array.from({ length: count }, (_, i) => row(i));
let suggestion: SuggestedOntology, elapsed: number, input: any;
const workspace = (s: string) =>
  transaction(store, s, async (tx) =>
    tx.put(resource(s, "workspace", "Discovery", {}, "workspace")),
  );
beforeAll(async () => {
  await workspace(scope);
  await seedDemo(store, scope);
  const state = await onboardingState(store, scope);
  input = {
    datasets: state.datasets,
    sources: state.sources,
    signature: state.signature,
  };
  const start = performance.now();
  suggestion = await suggestOntology(store, scope, input);
  elapsed = performance.now() - start;
});
afterAll(async () => {
  for (const s of scopes)
    for (const d of ["incoming", "datasets"])
      await rm(resolve(config.dataDir, d, s), { recursive: true, force: true });
});
const entity = (name: string) =>
  suggestion.entities.find((e) => e.name === name)!;
const link = (from: string, column: string, to: string) =>
  suggestion.links.find(
    (l) =>
      l.from === entity(from).id &&
      l.fromColumn === column &&
      l.to === entity(to).id,
  );

it("consolidates the Loopwell sample into federated entities", () => {
  expect(SuggestedOntologySchema.parse(suggestion)).toEqual(suggestion);
  expect(suggestion.entities.map((e) => [e.name, e.kind])).toEqual([
    ["Account", "record"],
    ["User", "record"],
    ["Subscription", "record"],
    ["Invoice", "record"],
    ["Support ticket", "record"],
    ["Campaign", "record"],
    ["SSO connection", "record"],
    ["Event", "activity"],
  ]);
  const account = entity("Account");
  expect(account.members.map((m) => [m.datasetName, m.matched])).toEqual([
    ["PlanetScale · accounts", 60],
    ["WorkOS · organizations", 60],
    ["Stripe · customers", 46],
  ]);
  expect(account.key).toBe("account_id");
  expect(account.rows).toBe(60);
  expect(account.members[2].via).toMatchObject({
    column: "customer_id",
    targetColumn: "stripe_customer_id",
  });
  expect(account.properties).toContain("workos_name");
  expect(account.properties).not.toContain("stripe_metadata");
  expect(account.description).toBe(
    "60 accounts from PlanetScale, joined with WorkOS organizations and Stripe customers.",
  );
  expect(entity("User").members.map((m) => m.datasetName)).toEqual([
    "PlanetScale · users",
    "WorkOS · users",
  ]);
  expect(entity("Support ticket").key).toBe("ticket_id");
  expect(entity("SSO connection").key).toBe("connection_id");
  expect(suggestion.unplaced).toEqual([]);
  expect(suggestion.summary).toBe(
    "7 entities and 1 activity stream from 11 datasets across 5 sources. Accounts combine PlanetScale, WorkOS and Stripe; users combine PlanetScale and WorkOS.",
  );
});

it("links entities by values, names and case-insensitive keys", () => {
  expect(link("Event", "distinct_id", "User")).toMatchObject({
    name: "performed by",
    evidence: "values",
    matchedRows: 3767,
    totalRows: 3873,
  });
  // Links a two-hop path already gives are left to traversal: invoices reach
  // accounts through subscriptions, and tickets through their requester.
  expect(link("Invoice", "customer_id", "Account")).toBeUndefined();
  expect(link("Support ticket", "account_id", "Account")).toBeUndefined();
  expect(link("Support ticket", "requester_email", "User")).toMatchObject({
    name: "requested by",
    caseInsensitive: true,
    toColumn: "email",
    toDatasetId: entity("User").anchorDatasetId,
  });
  expect(link("Account", "signup_campaign_id", "Campaign")).toMatchObject({
    name: "came from",
    caseInsensitive: true,
    matchedRows: 47,
  });
  expect(link("Invoice", "subscription_id", "Subscription")?.name).toBe(
    "for subscription",
  );
  // Counted through the merge: one Stripe customer never joins an account.
  expect(link("Subscription", "customer_id", "Account")).toMatchObject({
    name: "billed to",
    matchedRows: 46,
    totalRows: 47,
  });
  expect(link("SSO connection", "organization_id", "Account")).toBeTruthy();
  expect(link("User", "account_id", "Account")?.caseInsensitive).toBe(false);
  // Stripe billing emails differ from login emails too often to be a link.
  expect(suggestion.links.some((l) => l.fromColumn === "email")).toBe(false);
  expect(suggestion.links).toHaveLength(7);
  const notes = suggestion.notes.map((n) => n.text);
  expect(notes).toContain(
    "Account combines PlanetScale accounts with WorkOS organizations (60 of 62 match, confirmed by external_id) and Stripe customers (46 of 48 match, confirmed by metadata.account_id).",
  );
  expect(notes).toContain(
    "User combines PlanetScale users with WorkOS users (320 of 327 match, confirmed by email).",
  );
  expect(notes).toContain("2 WorkOS organizations don't match any account.");
  expect(notes).toContain("2 campaign codes only match when ignoring case.");
  expect(notes).toContain("14 requester emails only match when ignoring case.");
  expect(notes).toContain(
    "1 Stripe subscription has a customer_id that doesn't match any account.",
  );
  expect(
    suggestion.notes.find((n) => n.text.startsWith("2 campaign codes")),
  ).toMatchObject({ links: ["account-signup_campaign_id-campaign"] });
});

it("is deterministic and fast", async () => {
  expect(elapsed).toBeLessThan(2000);
  const again = await suggestOntology(store, scope, {
    ...input,
    datasets: [...input.datasets].reverse(),
  });
  expect(JSON.stringify(again)).toBe(JSON.stringify(suggestion));
  expect(suggestion.hash).toMatch(/^[0-9a-f]{64}$/);
  expect(suggestion.signature).toBe(input.signature);
});

it("names entities from their table", () => {
  expect(entityName("support_tickets")).toBe("Support ticket");
  expect(entityName("sso_connections")).toBe("SSO connection");
  expect(entityName("marketing_campaigns")).toBe("Campaign");
  expect(entityName("categories")).toBe("Category");
  expect(entityName("addresses")).toBe("Address");
  expect(entityName("warehouses")).toBe("Warehouse");
  expect(entityName("movies")).toBe("Movie");
  expect(entityName("news")).toBe("News");
  expect(entityName("statuses")).toBe("Status");
});

it("builds valid pipelines, types and relationships from the suggestion", async () => {
  const datasets = new Map(input.datasets.map((d: any) => [d.id, d]));
  const edited = confirmOntology(suggestion, {
    hash: suggestion.hash,
    excluded: [],
    names: { [entity("Account").id]: "Customer" },
  });
  const plan = ontologyBundle(edited, datasets as any);
  expect(plan.pipelines.map((p) => p.name)).toEqual([
    "Customers (PlanetScale + WorkOS + Stripe)",
    "Users (PlanetScale + WorkOS)",
    "Support tickets (S3)",
    "Campaigns (S3)",
  ]);
  const customer = plan.ontology.find((t) => t.name === "Customer")!;
  expect(customer.datasetId).toBe(pipelineId(plan.pipelines[0].name));
  expect(plan.ontology.map((t) => t.name)).not.toContain("Event");
  const between = (from: string, to: string) =>
    plan.relationships.filter((r) => r.fromType === from && r.toType === to);
  expect(between("Subscription", "Customer")).toEqual([
    expect.objectContaining({
      fromColumn: "customer_id",
      toColumn: "stripe_customer_id",
    }),
  ]);
  expect(between("Support ticket", "User")).toEqual([
    expect.objectContaining({
      name: "requested by",
      fromColumn: "requester_email_key",
      toColumn: "email_key",
    }),
  ]);
  expect(between("Invoice", "Customer")).toEqual([]);
  expect(between("Support ticket", "Customer")).toEqual([]);
  const bundle = BundleSchema.parse({ ...plan, dashboards: [], actions: [] });
  const report = await validateBundle(store, scope, bundle);
  expect(
    report.checks.filter((c) => !c.passed),
    JSON.stringify(report.checks),
  ).toEqual([]);
  // The pipeline-backed type is not moved to a source dataset.
  expect(bundle.ontology.find((t) => t.name === "Customer")?.datasetId).toBe(
    customer.datasetId,
  );
  const rel = (from: string, to: string) =>
    (report.previews["relation:" + between(from, to)[0].name] as any).rows[0];
  expect(Number(rel("Support ticket", "User").matched)).toBe(114);
  expect(Number(rel("Customer", "Campaign").matched)).toBe(47);
  expect(Number(rel("Subscription", "Customer").matched)).toBe(46);
  expect(Number(rel("SSO connection", "Customer").matched)).toBe(6);
});

it("drops notes about excluded entities and recounts the summary", () => {
  const edited = confirmOntology(suggestion, {
    hash: suggestion.hash,
    excluded: [entity("Campaign").id, entity("Support ticket").id],
    names: {},
  });
  const notes = edited.notes.map((n) => n.text).join(" ");
  expect(notes).not.toMatch(/campaign codes|requester/);
  expect(notes).toMatch(/WorkOS organizations don't match/);
  expect(edited.summary).toBe(
    "5 entities and 1 activity stream from 9 datasets across 4 sources. Accounts combine PlanetScale, WorkOS and Stripe; users combine PlanetScale and WorkOS.",
  );
});

it("applies review edits and keeps at least one record type", () => {
  const records = suggestion.entities.filter((e) => e.kind === "record");
  const edited = confirmOntology(suggestion, {
    hash: suggestion.hash,
    excluded: [entity("Campaign").id],
    names: {},
  });
  expect(edited.entities.map((e) => e.name)).not.toContain("Campaign");
  expect(edited.links.some((l) => l.to === entity("Campaign").id)).toBe(false);
  expect(() =>
    confirmOntology(suggestion, {
      hash: suggestion.hash,
      excluded: records.map((e) => e.id),
      names: {},
    }),
  ).toThrow(/at least one/);
  expect(() =>
    confirmOntology(suggestion, {
      hash: suggestion.hash,
      excluded: [],
      names: { [entity("User").id]: "account" },
    }),
  ).toThrow(/unique/);
});

it("uses declared foreign keys as priors and pairs integer keys only by name", async () => {
  await workspace(other);
  const source = await transaction(store, other, async (tx) =>
    tx.put(
      resource(other, "source", "orders", {
        kind: "postgres",
        connectionId: "connection_shop",
        discovery: {
          foreignKeys: [
            {
              table_name: "orders",
              column_name: "buyer",
              referenced_table: "customers",
              referenced_column: "id",
            },
          ],
        },
      }),
    ),
  );
  const customers = await transaction(store, other, async (tx) =>
    tx.put(
      resource(other, "source", "customers", {
        kind: "postgres",
        connectionId: "connection_shop",
        discovery: source.data.discovery,
      }),
    ),
  );
  await ingestRows(
    store,
    other,
    "customers",
    [1, 2, 3, 4, 5].map((id) => ({ id, name: "C" + id })),
    customers.id,
  );
  await ingestRows(
    store,
    other,
    "orders",
    [1, 2, 3, 4, 5].map((id) => ({ order_id: id, buyer: id % 2 ? 1 : 9 })),
    source.id,
  );
  await ingestRows(
    store,
    other,
    "Tickets",
    [1, 2, 3, 4, 5].map((ticket_id) => ({
      ticket_id,
      customer_id: (ticket_id % 3) + 1,
    })),
  );
  const state = await onboardingState(store, other);
  const s = await suggestOntology(store, other, {
    datasets: state.datasets,
    sources: state.sources,
    signature: state.signature,
  });
  expect(s.entities.map((e) => e.name).sort()).toEqual([
    "Customer",
    "Order",
    "Ticket",
  ]);
  expect(
    s.links.map((l) => [l.from, l.fromColumn, l.to, l.evidence]).sort(),
  ).toEqual([
    ["order", "buyer", "customer", "declared"],
    ["ticket", "customer_id", "customer", "name"],
  ]);
  // Integer keys with unrelated names (order_id, ticket_id) are not linked.
  expect(s.links).toHaveLength(2);
});

it("links a column named for another table", async () => {
  await workspace(named);
  await ingestRows(store, named, "Customers", [
    { id: "A", name: "Acme" },
    { id: "B", name: "Beta" },
  ]);
  await ingestRows(
    store,
    named,
    "Orders",
    [1, 2, 3].map((id) => ({ id, customer: id < 3 ? "A" : "B" })),
  );
  const state = await onboardingState(store, named);
  const s = await suggestOntology(store, named, {
    datasets: state.datasets,
    sources: state.sources,
    signature: state.signature,
  });
  expect(
    s.links.map((l) => [l.from, l.fromColumn, l.to, l.toColumn, l.evidence]),
  ).toEqual([["order", "customer", "customer", "id", "name"]]);
});

it("keeps a direct link that its two-hop path contradicts", async () => {
  await workspace(hops);
  const accounts = ["A", "B", "C"].map((a) => ({ account_id: a }));
  const users = [1, 2, 3, 4, 5, 6].map((n) => ({
    user_id: "u" + n,
    account_id: accounts[n % 3].account_id,
  }));
  await ingestRows(store, hops, "Accounts", accounts);
  await ingestRows(store, hops, "Users", users);
  // Every ticket names the requester and, separately, the account. Invoices pay
  // a subscription; the subscription's customer is the invoice's customer.
  const ticket = (n: number, account: (u: (typeof users)[0]) => string) => ({
    ticket_id: "t" + n,
    user_id: users[n % 6].user_id,
    account_id: account(users[n % 6]),
  });
  await ingestRows(
    store,
    hops,
    "Tickets",
    [...Array(12).keys()].map((n) => ticket(n, (u) => u.account_id)),
  );
  // Escalations are filed against a different account than the requester's.
  await ingestRows(
    store,
    hops,
    "Escalations",
    [...Array(12).keys()].map((n) => {
      const u = users[n % 6];
      return {
        escalation_id: "e" + n,
        user_id: u.user_id,
        account_id: accounts.find((a) => a.account_id !== u.account_id)!
          .account_id,
      };
    }),
  );
  const state = await onboardingState(store, hops);
  const s = await suggestOntology(store, hops, {
    datasets: state.datasets,
    sources: state.sources,
    signature: state.signature,
  });
  const links = s.links.map((l) => `${l.from}.${l.fromColumn}->${l.to}`);
  expect(links).toContain("ticket.user_id->user");
  expect(links).not.toContain("ticket.account_id->account");
  expect(links).toContain("escalation.user_id->user");
  expect(links).toContain("escalation.account_id->account");
});

it("recognises spreadsheet headers and natural keys as keys", async () => {
  for (const key of ["Customer ID", "customer-id", "CustomerId"]) {
    const { suggestion, links } = await suggestFor([
      {
        name: "customers",
        rows: range(20, (i) => ({ [key]: "C" + i, Name: "N" + i })),
      },
      {
        name: "orders",
        rows: range(50, (i) => ({
          [key.replace(/customer/i, (w) => (w[0] === "C" ? "Order" : "order"))]:
            "O" + i,
          [key]: "C" + (i % 20),
        })),
      },
    ]);
    expect(suggestion.entities.map((e) => e.name).sort(), key).toEqual([
      "Customer",
      "Order",
    ]);
    expect(links, key).toEqual([`order.${key}->customer.${key}`]);
  }
  const { suggestion } = await suggestFor([
    {
      name: "products",
      rows: range(20, (i) => ({ sku: "SKU-" + i, title: "P" + i, price: i })),
    },
  ]);
  expect(suggestion.entities[0]).toMatchObject({ name: "Product", key: "sku" });
});

it("still suggests entities and confirms when no column is named like a key", async () => {
  const { suggestion } = await suggestFor([
    {
      name: "sales.csv",
      rows: range(10, (i) => ({ region: "R" + i, total: i * 3 })),
    },
  ]);
  expect(suggestion.entities.map((e) => [e.name, e.key])).toEqual([
    ["Sale", "region"],
  ]);
  const { suggestion: empty } = await suggestFor([
    { name: "dupes", rows: range(4, () => ({ region: "R", total: 1 })) },
  ]);
  expect(empty.entities).toEqual([]);
  // Nothing to build records from: the view is built from dashboards alone.
  const confirmed = confirmOntology(empty, {
    hash: empty.hash,
    excluded: [],
    names: {},
  });
  expect(confirmed.entities).toEqual([]);
});

it("keeps a foreign key between two uploads as a link", async () => {
  const { suggestion, links } = await suggestFor([
    {
      name: "customers",
      rows: range(40, (i) => ({ customer_id: "cus_" + i, name: "C" + i })),
    },
    {
      name: "subscriptions",
      rows: range(35, (i) => ({
        subscription_id: "sub_" + i,
        customer_id: "cus_" + i,
      })),
    },
  ]);
  expect(suggestion.entities.map((e) => e.members.length)).toEqual([1, 1]);
  expect(links).toEqual(["subscription.customer_id->customer.customer_id"]);
  expect(suggestion.summary).toMatch(/across 1 source\.$/);
});

it("keeps a direct link for rows its two-hop path cannot reach", async () => {
  const stripe = { kind: "stripe", connection: "stripe" };
  const { links } = await suggestFor([
    {
      ...stripe,
      name: "customers",
      rows: range(30, (i) => ({ customer_id: "cus_" + i })),
    },
    {
      ...stripe,
      name: "subscriptions",
      rows: range(20, (i) => ({
        subscription_id: "sub_" + i,
        customer_id: "cus_" + i,
      })),
    },
    {
      ...stripe,
      name: "invoices",
      // 40 one-off invoices have no subscription.
      rows: range(100, (i) => ({
        invoice_id: "in_" + i,
        customer_id: "cus_" + (i < 60 ? i % 20 : 20 + (i % 10)),
        subscription_id: i < 60 ? "sub_" + (i % 20) : null,
      })),
    },
  ]);
  expect(links).toContain("invoice.customer_id->customer.customer_id");
});

it("does not merge tables whose serial ids merely overlap", async () => {
  const { suggestion } = await suggestFor([
    {
      name: "users",
      kind: "postgres",
      connection: "app",
      rows: range(100, (i) => ({ id: i + 1, email: `app${i}@a.example` })),
    },
    {
      name: "users",
      kind: "mysql",
      connection: "forum",
      rows: range(80, (i) => ({ id: i + 1, email: `forum${i}@f.example` })),
    },
  ]);
  expect(suggestion.entities.map((e) => e.name)).toEqual([
    "User",
    "User (MySQL)",
  ]);
  expect(suggestion.notes.some((n) => n.kind === "merged")).toBe(false);
});

it("links 1:1 extension tables to the table they extend", async () => {
  const db = {
    kind: "postgres",
    connection: "db",
    discovery: {
      foreignKeys: [
        {
          table_name: "user_profiles",
          column_name: "user_id",
          referenced_table: "users",
          referenced_column: "user_id",
        },
      ],
    },
  };
  const { suggestion, links } = await suggestFor([
    { ...db, name: "users", rows: range(30, (i) => ({ user_id: "u" + i })) },
    {
      ...db,
      name: "user_profiles",
      rows: range(25, (i) => ({ user_id: "u" + i, bio: "b" + i })),
    },
    {
      ...db,
      name: "orders",
      rows: range(60, (i) => ({ order_id: "o" + i, user_id: "u" + (i % 30) })),
    },
  ]);
  expect(links.sort()).toEqual([
    "order.user_id->user.user_id",
    "user_profile.user_id->user.user_id",
  ]);
  expect(suggestion.links.find((l) => l.from === "user_profile")?.name).toBe(
    "extends",
  );
});

it("links integer keys across many tables without losing any", async () => {
  const nouns = ["user", "account", "order", "product", "invoice", "payment"];
  nouns.push("shipment", "review", "cart", "coupon", "vendor", "warehouse");
  const db = { kind: "postgres", connection: "shop" };
  const { links } = await suggestFor([
    ...nouns.map((noun, t) => ({
      ...db,
      name: noun + "s",
      rows: range(200, (i) => ({
        id: i + 1,
        [nouns[(t + 1) % nouns.length] + "_id"]: ((i * 7919 + t) % 200) + 1,
        [nouns[(t + 5) % nouns.length] + "_id"]: ((i * 104729 + t) % 200) + 1,
        country_id: (i % 20) + 1,
      })),
    })),
    {
      ...db,
      name: "countries",
      rows: range(20, (i) => ({ id: i + 1, name: "Country " + i })),
    },
  ]);
  expect(links.filter((l) => l.endsWith("->country.id"))).toHaveLength(12);
  expect(links.filter((l) => !l.endsWith("->country.id"))).toHaveLength(24);
});

it("names entities after files, not their folders", async () => {
  const { suggestion } = await suggestFor([
    {
      name: "exports/2024/orders.csv",
      rows: range(5, (i) => ({ order_id: "o" + i })),
    },
    {
      name: "customers.export",
      rows: range(5, (i) => ({ customer_id: "c" + i })),
    },
  ]);
  expect(suggestion.entities.map((e) => e.name).sort()).toEqual([
    "Customer",
    "Order",
  ]);
});

// A shop database's discovery, as a connection reads it: every table's
// columns, keys and foreign keys, whichever tables are selected.
const shop = {
  customers: range(5, (i) => ({ id: i + 1, name: "C" + i })),
  orders: range(5, (i) => ({ order_id: i + 1, buyer: (i % 5) + 1 })),
  shipments: range(5, (i) => ({ shipment_id: i + 1, parcel: (i % 5) + 1 })),
};
type ShopTable = keyof typeof shop;
const shopKey = (table_name: string, column_name: string, type: string) => ({
  table_schema: "public",
  table_name,
  column_name,
  constraint_type: type,
});
const shopForeignKey = (
  table_name: string,
  column_name: string,
  referenced_table: string,
  referenced_column: string,
) => ({
  table_schema: "public",
  table_name,
  column_name,
  referenced_schema: "public",
  referenced_table,
  referenced_column,
});
const shopDiscovery = {
  tables: Object.keys(shop).map((name) => ({ schema: "public", name })),
  columns: Object.entries(shop).flatMap(([table_name, rows]) =>
    Object.keys(rows[0]).map((name) => ({
      schema: "public",
      table_name,
      name,
      type: "integer",
      nullable: "NO",
    })),
  ),
  keys: [
    shopKey("customers", "id", "PRIMARY KEY"),
    shopKey("orders", "order_id", "PRIMARY KEY"),
    shopKey("orders", "buyer", "FOREIGN KEY"),
    shopKey("shipments", "shipment_id", "PRIMARY KEY"),
    shopKey("shipments", "parcel", "FOREIGN KEY"),
  ],
  foreignKeys: [
    shopForeignKey("orders", "buyer", "customers", "id"),
    shopForeignKey("shipments", "parcel", "orders", "order_id"),
  ],
  sample: shop.orders,
};
const ownKeys = (table: string) => (k: any) =>
  k.table_schema === "public" && k.table_name === table;
const linkNames = (s: SuggestedOntology) =>
  s.links
    .map((l) => [l.from, l.fromColumn, l.to, l.toColumn, l.evidence].join(" "))
    .sort();

it("suggests the same declared links from each source's own keys as from the whole discovery", async () => {
  const links = async (tables: ShopTable[], stored: boolean) =>
    linkNames(
      (
        await suggestFor(
          tables.map((name) => ({
            name,
            rows: shop[name],
            kind: "postgres",
            connection: "shop",
            discovery: stored
              ? storedDiscovery("postgres", shopDiscovery, {
                  schema: "public",
                  table: name,
                })
              : shopDiscovery,
          })),
        )
      ).suggestion,
    );
  // Without a schema, PostgreSQL's is public, as an import reads it.
  expect(
    storedDiscovery("postgres", shopDiscovery, { table: "orders" }),
  ).toEqual({
    keys: shopDiscovery.keys.filter(ownKeys("orders")),
    foreignKeys: [shopDiscovery.foreignKeys[0]],
  });
  const all: ShopTable[] = ["customers", "orders", "shipments"];
  expect(await links(all, false)).toEqual(
    expect.arrayContaining(["order buyer customer id declared"]),
  );
  // Including when only one side of a foreign key is selected.
  for (const tables of [
    all,
    ["orders", "shipments"],
    ["customers", "shipments"],
    ["customers", "orders"],
    ["orders"],
    ["shipments"],
  ] as ShopTable[][])
    expect(await links(tables, true), tables.join()).toEqual(
      await links(tables, false),
    );
});
