import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve as resolvePath } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";
import {
  fromDraft,
  knowledgeStatement,
  parseKnowledge,
  playbooks,
  referenceFor,
  resolve,
} from "../apps/api/src/industry.js";
import type { BusinessInsights } from "../packages/shared/src/index.js";

// Industry playbooks: an online store is recognised from its data, its
// coverage and links are checked, and suggestions follow what's missing and
// what people ask. Nothing asks the owner to choose.
let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
beforeEach(async () => {
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  scope = (await app.inject({ method: "GET", url: "/api/v1/me" })).json()
    .workspaces[0].id;
});
afterEach(async () => {
  vi.restoreAllMocks();
  await app.close();
  for (const d of ["incoming", "datasets"])
    await rm(resolvePath(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});
const ingest = async (name: string, rows: unknown[]) => {
  const r = await request("POST", "/ingest", { name, rows });
  expect(r.statusCode, r.body).toBeLessThan(300);
  return r.json().id as string;
};
// A small store: four orders, one without a customer; visits with a traffic
// source but no campaign; products with cost; no spend, checkout or support.
async function shop() {
  await ingest("customers", [
    { customer_id: 1, email: "a@x.test", signup_date: "2026-01-01" },
    { customer_id: 2, email: "b@x.test", signup_date: "2026-01-02" },
    { customer_id: 3, email: "B@x.test ", signup_date: "2026-01-03" },
  ]);
  await ingest("orders", [
    {
      order_id: 1,
      customer_id: 1,
      order_date: "2026-02-01",
      status: "completed",
      net_amount: 40,
    },
    {
      order_id: 2,
      customer_id: 1,
      order_date: "2026-02-03",
      status: "completed",
      net_amount: 60,
    },
    {
      order_id: 3,
      customer_id: 2,
      order_date: "2026-02-04",
      status: "returned",
      net_amount: 25,
    },
    {
      order_id: 4,
      customer_id: null,
      order_date: "2026-02-05",
      status: "cancelled",
      net_amount: 10,
    },
  ]);
  await ingest("order_items", [
    {
      order_item_id: 1,
      order_id: 1,
      product_id: 1,
      quantity: 1,
      unit_price: 40,
      discount_amount: 0,
    },
    {
      order_item_id: 2,
      order_id: 2,
      product_id: 2,
      quantity: 2,
      unit_price: 30,
      discount_amount: 0,
    },
    {
      order_item_id: 3,
      order_id: 3,
      product_id: 1,
      quantity: 1,
      unit_price: 25,
      discount_amount: 5,
    },
  ]);
  await ingest("products", [
    { product_id: 1, product_name: "Mug", list_price: 40, unit_cost: 15 },
    { product_id: 2, product_name: "Bowl", list_price: 30, unit_cost: 9 },
  ]);
  const sessions = await ingest("sessions", [
    {
      session_id: 1,
      customer_id: 1,
      session_date: "2026-02-01",
      traffic_source: "email",
      order_id: 1,
    },
    {
      session_id: 2,
      customer_id: 1,
      session_date: "2026-02-03",
      traffic_source: "organic",
      order_id: 2,
    },
    {
      session_id: 3,
      customer_id: 2,
      session_date: "2026-02-04",
      traffic_source: "social",
      order_id: null,
    },
  ]);
  await ingest("shipments", [
    {
      shipment_id: 1,
      order_id: 1,
      shipped_date: "2026-02-02",
      delivered_date: "2026-02-05",
      carrier: "Standard",
    },
  ]);
  return sessions;
}
const business = async (): Promise<BusinessInsights> => {
  const r = await request("GET", "/business");
  expect(r.statusCode, r.body).toBe(200);
  return r.json();
};
const provider = () =>
  transaction(store, scope, async (tx) =>
    tx.put(
      resource(
        scope,
        "provider",
        "AI",
        { settings: { provider: "local", model: "test-model" } },
        "provider",
      ),
    ),
  );

describe("playbooks", () => {
  it("parse from their markdown and refer only to roles they define", () => {
    for (const pb of playbooks()) {
      const roles = new Set(Object.keys(pb.roles));
      for (const m of Object.values(pb.metrics))
        for (const n of m.needs) expect(roles, `${pb.id}: ${n}`).toContain(n);
      for (const a of pb.advice) {
        if (a.missing)
          expect(roles, `${pb.id}: ${a.missing}`).toContain(a.missing);
        if (a.low) expect(pb.checks.map((c) => c.id)).toContain(a.low);
        expect(pb.areas.map((x) => x.id)).toContain(a.area);
      }
      for (const d of pb.drivers || [])
        for (const n of d.needs) expect(roles, `${pb.id}: ${n}`).toContain(n);
      for (const area of pb.areas)
        for (const m of area.metrics) expect(pb.metrics[m], m).toBeDefined();
      for (const id of Object.keys(pb.normal))
        expect(pb.metrics[id], id).toBeDefined();
    }
    const shop = playbooks().find((b) => b.id === "ecommerce")!;
    expect(shop.title).toBe("Online retail");
    expect(shop.drivers!.length).toBeGreaterThanOrEqual(10);
    expect(shop.intelligence!.length).toBeGreaterThanOrEqual(10);
  });
});

describe("a store's workspace", () => {
  it("is read as online retail, with coverage, link checks and suggestions", async () => {
    await shop();
    const b = await business();
    expect(b.industry).toEqual({
      id: "ecommerce",
      title: "Online retail",
      source: "inferred",
    });
    const area = (id: string) => b.areas.find((a) => a.id === id)!;
    expect(area("revenue").status).toBe("ready");
    // Product cost and line discounts are both there.
    expect(area("cost").status).toBe("ready");
    expect(area("marketing").status).toBe("partial");
    const ids = b.suggestions.map((s) => s.id);
    // Missing data becomes suggestions; what exists (product cost) doesn't.
    expect(ids).toEqual(
      expect.arrayContaining([
        "connect_spend",
        "connect_support",
        "utm_tags",
        "checkout_events",
        "refund_reasons",
      ]),
    );
    expect(ids).not.toContain("product_cost");
    expect(ids).not.toContain("connect_analytics");
    // One of four orders has no customer; emails repeat once ignoring case.
    const check = (id: string) => b.checks.find((c) => c.id === id)!;
    expect(check("orders_link_customers")).toMatchObject({
      rate: 0.75,
      ok: false,
    });
    expect(check("orders_link_customers").text).toContain("75%");
    expect(check("lines_link_orders")).toMatchObject({ rate: 1, ok: true });
    expect(check("sessions_link_orders")).toMatchObject({ rate: 1, ok: true });
    expect(check("duplicate_customers").ok).toBe(false);
    expect(ids).toContain("one_customer_id");
    expect(check("status_meaning").text).toMatch(
      /returned.*counted apart from sales/i,
    );
    expect(
      b.knowledge.filter((k) => k.source === "typical").length,
    ).toBeGreaterThan(0);
  });

  it("is generic when the data isn't a store", async () => {
    await ingest("invoices", [{ invoice_id: 1, account_id: 1, amount: 100 }]);
    await ingest("subscriptions", [{ subscription_id: 1, plan: "pro" }]);
    const b = await business();
    expect(b.industry).toBeNull();
    expect(b.suggestions).toEqual([]);
  });
});

describe("knowledge", () => {
  it("reads targets and statements, and a target wins over a typical range", async () => {
    await shop();
    const pb = playbooks()[0];
    expect(parseKnowledge("Our target return rate is 8%", pb)).toEqual({
      kind: "target",
      metric: "return_rate",
      value: 0.08,
    });
    expect(
      parseKnowledge("Sales double in the Black Friday week", pb).kind,
    ).toBe("seasonality");
    expect(parseKnowledge("Returns aren't sales", pb).kind).toBe("definition");
    expect(knowledgeStatement("Remember that we pause ads in August")).toBe(
      "we pause ads in August",
    );
    expect(
      knowledgeStatement("What is our target return rate?"),
    ).toBeUndefined();

    expect(referenceFor(await resolve(store, scope), "Conversion")).toBe(
      "typical 1–3%",
    );
    let b = (
      await request("POST", "/business/knowledge", {
        text: "Our target return rate is 8%",
      })
    ).json() as BusinessInsights;
    expect(b.knowledge[0]).toMatchObject({
      source: "you",
      kind: "target",
      metric: "return_rate",
    });
    expect(referenceFor(await resolve(store, scope), "Return rate")).toBe(
      "your target 8%",
    );
    // Removing a typical range hides it; removing your own deletes it.
    b = (
      await request("DELETE", "/business/knowledge/typical:conversion_rate")
    ).json();
    expect(b.knowledge.some((k) => k.id === "typical:conversion_rate")).toBe(
      false,
    );
    expect(
      referenceFor(await resolve(store, scope), "Conversion"),
    ).toBeUndefined();
    b = (
      await request("DELETE", `/business/knowledge/${b.knowledge[0].id}`)
    ).json();
    expect(b.knowledge.some((k) => k.source === "you")).toBe(false);
  });
});

describe("the owner's own words", () => {
  it("picks a built-in playbook for a retailer, and drafts one for anything else", async () => {
    await shop();
    let b = (
      await request("PATCH", "/business", { description: "Furniture retailer" })
    ).json() as BusinessInsights;
    expect(b.industry?.source).toBe("described");
    expect(b.industry?.id).toBe("ecommerce");

    // No AI: the description is kept, and the reading stays generic.
    b = (
      await request("PATCH", "/business", {
        description: "Specialty coffee roaster, wholesale and online",
      })
    ).json();
    expect(b.description).toBe(
      "Specialty coffee roaster, wholesale and online",
    );
    expect(b.industry).toBeNull();
    expect((await store.list(scope, "run")).length).toBe(0);

    // With AI, a playbook is drafted for this workspace and validated.
    await provider();
    b = (
      await request("PATCH", "/business", {
        description: "Specialty coffee roaster",
      })
    ).json();
    expect(b.drafting).toBe(true);
    const run = (await store.list(scope, "run")).find(
      (r) => r.data.task === "draft_playbook",
    )!;
    vi.spyOn(Provider.prototype, "generate").mockResolvedValue({
      tokens: 1,
      value: {
        title: "Coffee roasting",
        roles: [
          {
            id: "order",
            title: "Orders",
            entity: true,
            of: "",
            type: "any",
            names: ["order"],
          },
          {
            id: "wholesale_account",
            title: "Cafés",
            entity: true,
            of: "",
            type: "any",
            names: ["wholesale_account", "cafe"],
          },
          {
            id: "amount",
            title: "Amount",
            entity: false,
            of: "order",
            type: "number",
            names: ["net_amount"],
          },
          {
            id: "orphan",
            title: "Orphan",
            entity: false,
            of: "nothing",
            type: "number",
            names: ["x"],
          },
        ],
        areas: [
          {
            id: "revenue",
            title: "Revenue",
            keywords: ["revenue"],
            metrics: [
              {
                id: "revenue",
                title: "Revenue",
                needs: ["order", "amount"],
                hint: "",
              },
            ],
          },
          {
            id: "wholesale",
            title: "Wholesale",
            keywords: ["cafe", "wholesale"],
            metrics: [
              {
                id: "active_cafes",
                title: "Active cafés",
                needs: ["wholesale_account"],
                hint: "",
              },
              { id: "broken", title: "Broken", needs: ["orphan"], hint: "" },
            ],
          },
        ],
        advice: [
          {
            id: "connect_wholesale",
            kind: "connect",
            area: "wholesale",
            missing: "wholesale_account",
            text: "Connect your wholesale accounts.",
            keywords: ["cafe"],
          },
          {
            id: "bad",
            kind: "connect",
            area: "nowhere",
            missing: "order",
            text: "x",
            keywords: [],
          },
        ],
        normal: [
          {
            metric: "revenue",
            low: 2,
            high: 5,
            unit: "percent",
            text: "Revenue grows 2–5% a month",
          },
        ],
      },
    });
    await buildWorkspace(store, scope, run.id);
    expect((await store.get(scope, run.id))!.data.status).toBe("succeeded");
    b = await business();
    expect(b.industry).toEqual({
      id: "drafted",
      title: "Coffee roasting",
      source: "drafted",
    });
    expect(b.areas.map((a) => a.id)).toEqual(["revenue", "wholesale"]);
    expect(b.areas[1].metrics.map((m) => m.id)).toEqual(["active_cafes"]);
    expect(b.suggestions.map((s) => s.id)).toEqual(["connect_wholesale"]);
    expect(b.knowledge.find((k) => k.source === "typical")?.text).toBe(
      "Revenue grows 2–5% a month",
    );
  });

  it("keeps only the parts of a draft that hold together", () => {
    const pb = fromDraft({
      title: "X",
      roles: [
        {
          id: "a",
          title: "A",
          entity: true,
          of: "",
          type: "any",
          names: ["a"],
        },
        {
          id: "f",
          title: "F",
          entity: false,
          of: "missing",
          type: "number",
          names: ["f"],
        },
      ],
      areas: [
        {
          id: "one",
          title: "One",
          keywords: [],
          metrics: [{ id: "m", title: "M", needs: ["f"], hint: "" }],
        },
        {
          id: "two",
          title: "Two",
          keywords: [],
          metrics: [{ id: "n", title: "N", needs: ["a"], hint: "" }],
        },
      ],
      advice: [],
      normal: [
        { metric: "n", low: 5, high: 1, unit: "number", text: "backwards" },
      ],
    });
    expect(Object.keys(pb.roles)).toEqual(["a"]);
    expect(pb.areas.map((a) => a.id)).toEqual(["two"]);
    expect(pb.normal).toEqual({});
  });
});

describe("the Analyst", () => {
  let sessions = "";
  const answering = (onAnalysis?: (context: any) => void) =>
    vi
      .spyOn(Provider.prototype, "generate")
      .mockImplementation(async (system: string, context: any) => {
        if (system.includes("Choose answer"))
          return { value: { intent: "answer" }, tokens: 1 };
        if (system.includes("Answer the question")) onAnalysis?.(context);
        return {
          value: system.includes("Summarise")
            ? { summary: "Email leads.", highlights: [] }
            : {
                title: "Repeat buyers by source",
                sql: 'SELECT "traffic_source", count(*) AS n FROM sessions GROUP BY 1',
                inputs: [sessions],
                assumptions: [],
              },
          tokens: 1,
        };
      });
  const ask = async (goal: string) => {
    const run = (
      await request("POST", "/assistant/runs", { goal, intent: "auto" })
    ).json();
    await buildWorkspace(store, scope, run.id);
    return (await store.get(scope, run.id))!;
  };

  it("reads with the playbook, notes what missing data limited, and learns from the question", async () => {
    sessions = await shop();
    await provider();
    let seen: any;
    answering((c) => (seen = c.business));
    const done = await ask("Which campaign brings the most repeat customers?");
    expect(done.data.status, done.data.error).toBe("succeeded");
    expect(seen.industry).toBe("Online retail");
    expect(seen.drivers.map((d: any) => d.level)).toContain("Net revenue");
    expect(
      seen.drivers.find((d: any) => d.level === "Conversion funnel")
        .notInThisData,
    ).toBe(true);
    expect(seen.industryRulesOfThumb.length).toBeGreaterThan(5);
    expect(done.data.answer.note).toMatch(/UTM/);
    // The question counts as evidence, and moves its suggestion up.
    const b = await business();
    expect(b.suggestions[0]).toMatchObject({ id: "utm_tags", asked: 1 });
    const profile = await store.get(scope, "business_profile");
    expect(profile!.data.gaps.utm_tags.examples[0].q).toContain("campaign");
  });

  it("remembers what it's told about the business instead of querying", async () => {
    await shop();
    await provider();
    const generate = answering();
    const done = await ask("Our target return rate is 8%");
    expect(done.data.status).toBe("succeeded");
    expect(done.data.answer.saved).toBe(true);
    expect(done.data.answer.summary).toContain("target return rate is 8%");
    expect(generate).not.toHaveBeenCalled();
    const b = await business();
    expect(b.knowledge[0]).toMatchObject({ source: "you", kind: "target" });
  });
});
