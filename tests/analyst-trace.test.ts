import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";
import { createElement as h } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { randomUUID } from "node:crypto";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";
import { seal } from "../apps/api/src/security.js";
import {
  answerShape,
  datasetTable,
  type Resource,
} from "../packages/shared/src/index.js";
import {
  compactSteps,
  estimateRange,
  resultText,
  runSteps,
  stepText,
} from "../apps/web/src/areas/analyst/chat-data.js";
import {
  AnswerChart,
  AnswerResult,
  Emphasized,
  answerColumns,
  chartPlan,
  leadSentence,
} from "../apps/web/src/areas/analyst/answer-view.js";

// Showing the Analyst's work: the result's checked shape, the major steps a
// run reports, the time comparable answers took, and the answer's statement
// with the numbers its rows back.

const customers = [
  { customer_id: 7, name: "Lucas I. Kim", email: "l@x.test", order_count: 42 },
  { customer_id: 3, name: "Noah T. Mehta", email: "n@x.test", order_count: 34 },
  { customer_id: 9, name: "Elena Parker", email: "e@x.test", order_count: 31 },
];

describe("answer shape", () => {
  it("reads a ranking from wide rows, never taking a key as the measure", () => {
    expect(answerShape(customers)).toEqual({
      kind: "ranking",
      label: "name",
      measure: "order_count",
      order: "desc",
      leaders: 1,
      keys: ["customer_id"],
    });
    // Keys stay keys even when they happen to run in order.
    const ids = customers.map((c, i) => ({ ...c, customer_id: i + 1 }));
    expect(answerShape(ids).measure).toBe("order_count");
    // A tie for first place is counted, so nothing names a single leader.
    const tied = customers.map((c, i) => ({
      ...c,
      order_count: i < 2 ? 42 : 31,
    }));
    expect(answerShape(tied)).toMatchObject({ kind: "ranking", leaders: 2 });
  });
  it("takes the measure the question names, and column types over values", () => {
    const rows = customers.map((c, i) => ({
      name: c.name,
      total_spent: 900 - i * 100,
      order_count: c.order_count,
    }));
    expect(answerShape(rows).measure).toBe("total_spent");
    expect(
      answerShape(rows, { hint: "Who has the most orders?" }).measure,
    ).toBe("order_count");
    // Digits stored as text are text.
    const coded = [
      { region: "North", reference: "0012", amount: 5 },
      { region: "South", reference: "0007", amount: 9 },
    ];
    expect(answerShape(coded).measure).toBe("reference");
    expect(
      answerShape(coded, { types: { reference: "VARCHAR", amount: "BIGINT" } })
        .measure,
    ).toBe("amount");
  });
  it("tells a trend from months ranked by a measure", () => {
    const months = [
      { month: "2026-01", revenue: 10 },
      { month: "2026-02", revenue: 30 },
      { month: "2026-03", revenue: 20 },
    ];
    expect(answerShape(months)).toMatchObject({
      kind: "trend",
      label: "month",
      measure: "revenue",
    });
    const ranked = [months[1], months[2], months[0]];
    expect(answerShape(ranked)).toMatchObject({
      kind: "ranking",
      label: "month",
      order: "desc",
    });
  });
  it("takes the measure the question names over one that merely runs in order", () => {
    const weeks = [
      { week: "2026-09-07", order_count: 12, cumulative_revenue: 100 },
      { week: "2026-09-14", order_count: 9, cumulative_revenue: 180 },
      { week: "2026-09-21", order_count: 15, cumulative_revenue: 290 },
    ];
    expect(
      answerShape(weeks, { hint: "How many orders each week?" }),
    ).toMatchObject({ kind: "trend", label: "week", measure: "order_count" });
    // With none named, the measure the rows run by.
    expect(answerShape(weeks).measure).toBe("cumulative_revenue");
    // The question outranks the query's title, which names both.
    expect(
      answerShape(weeks, {
        hint: [
          "How many orders each week?",
          "Orders and cumulative revenue by week",
        ],
      }).measure,
    ).toBe("order_count");
  });
  it("reads one row as a value and no rows as empty", () => {
    expect(answerShape([{ revenue: "65" }])).toEqual({
      kind: "value",
      measure: "revenue",
      keys: [],
    });
    expect(answerShape([])).toEqual({ kind: "empty", keys: [] });
  });
});

describe("answer view", () => {
  it("leads with the summary's first sentence, past initials", () => {
    expect(
      leadSentence(
        "Lucas I. Kim has the most orders. He placed 42 orders, 8 more than Noah.",
      ),
    ).toEqual({
      statement: "Lucas I. Kim has the most orders.",
      rest: "He placed 42 orders, 8 more than Noah.",
    });
    expect(leadSentence("Revenue was 65.")).toEqual({
      statement: "Revenue was 65.",
      rest: "",
    });
    expect(leadSentence("x".repeat(200) + ". More.")).toBeNull();
  });
  it("bolds only the quantities the rows back, with their units", () => {
    const html = renderToStaticMarkup(
      h(Emphasized, {
        text: "He placed 42 orders in 2026, 8 more than Noah T. Mehta, and 99 returns; customer 7 is first of the top 11.",
        rows: customers,
      }),
    );
    expect(html).toContain("<strong>42 orders</strong>");
    expect(html).toContain("<strong>8 more</strong>");
    // A year, a number the rows don't hold and a key stay as written.
    expect(html).toContain(" in 2026,");
    expect(html).toContain("and 99 returns");
    expect(html).toContain("customer 7 is");
    // 11 is a difference (42 − 31), but nothing here compares by it.
    expect(html).toContain("the top 11.");
    const money = renderToStaticMarkup(
      h(Emphasized, {
        text: "Acme paid $1,200, 12% of the total, about $1.2K in all; 7 orders.",
        rows: [{ name: "Acme", amount_cents: 120000, share: 0.12 }],
      }),
    );
    expect(money).toContain("<strong>$1,200</strong>");
    expect(money).toContain("<strong>12%</strong>");
    expect(money).toContain("<strong>$1.2K</strong>");
    expect(money).toContain("; 7 orders.");
    // Only cents are stated in dollars: a hundredth of dollars backs nothing.
    expect(
      renderToStaticMarkup(
        h(Emphasized, {
          text: "Acme earned $1,200.",
          rows: [{ name: "Acme", revenue_dollars: 120000 }],
        }),
      ),
    ).not.toContain("<strong>");
  });
  it("puts the measure first and keys last, and ranks the chart", () => {
    const shape = answerShape(customers);
    const columns = answerColumns(customers, shape);
    expect(columns.map((c) => c.key)).toEqual([
      "name",
      "order_count",
      "email",
      "customer_id",
    ]);
    expect(columns[1].className).toBe("is-measure");
    expect(columns[3].className).toBe("is-key");
    const plan = chartPlan(customers, shape)!;
    expect(plan).toMatchObject({
      kind: "breakdown",
      label: "name",
      value: "order_count",
      rank: "desc",
    });
    const chart = renderToStaticMarkup(h(AnswerChart, { plan }));
    expect(chart).toContain("Order count by name, highest first");
    expect(chart.match(/wk-bar-rank/g)).toHaveLength(3);
    expect(chart.match(/is-first/g)).toHaveLength(1);
  });
  it("says when a chart has only the result's first rows", () => {
    const ranked = Array.from({ length: 100 }, (_, i) => ({
      name: `Customer ${i}`,
      order_count: 500 - i,
    }));
    const top = renderToStaticMarkup(
      h(AnswerResult, {
        rows: ranked,
        total: 1000,
        label: "Customers",
        shape: answerShape(ranked),
      }),
    );
    expect(top).toContain("Top 10 of 1,000");
    expect(top).toContain("Show top 30");
    const days = Array.from({ length: 100 }, (_, i) => ({
      day: new Date(Date.UTC(2026, 0, 1 + i)).toISOString().slice(0, 10),
      orders: (i * 7) % 13,
    }));
    const trend = renderToStaticMarkup(
      h(AnswerResult, {
        rows: days,
        total: 1000,
        label: "Orders by day",
        shape: answerShape(days),
      }),
    );
    expect(answerShape(days).kind).toBe("trend");
    expect(trend).toContain("First 100 of 1,000 rows");
  });
  it("shows signed results as a table, and ranks and bolds big integers exactly", () => {
    const signed = [
      { region: "North", profit: 120 },
      { region: "South", profit: -40 },
      { region: "East", profit: 15 },
    ];
    const table = renderToStaticMarkup(
      h(AnswerResult, {
        rows: signed,
        label: "Profit by region",
        shape: answerShape(signed),
      }),
    );
    expect(answerShape(signed).kind).toBe("breakdown");
    expect(table).not.toContain("Show the result as");
    expect(table).not.toContain("wk-bars");
    // Both round to the same Number; only one is first.
    const big = [
      { name: "A", points: "9007199254740993" },
      { name: "B", points: "9007199254740992" },
      { name: "C", points: "5" },
    ];
    const shape = answerShape(big);
    expect(shape).toMatchObject({ kind: "ranking", leaders: 1 });
    const chart = renderToStaticMarkup(
      h(AnswerChart, { plan: chartPlan(big, shape)! }),
    );
    expect(chart.match(/is-first/g)).toHaveLength(1);
    const text = (t: string) =>
      renderToStaticMarkup(h(Emphasized, { text: t, rows: [big[0], big[2]] }));
    expect(text("A has 9,007,199,254,740,993 points.")).toContain(
      "<strong>9,007,199,254,740,993 points</strong>",
    );
    expect(text("A has 9,007,199,254,740,992 points.")).not.toContain(
      "<strong>",
    );
  });
});

// A run as the Analyst stores it, with events at seconds after `t0`.
const t0 = Date.parse("2026-10-10T10:00:00.000Z");
const at = (s: number) => new Date(t0 + s * 1000).toISOString();
const run = (
  data: Record<string, any>,
  events: [number, string][],
  updated = events.at(-1)?.[0] ?? 0,
): Resource => ({
  id: "r1",
  workspaceId: "w1",
  kind: "run",
  name: "Who has the most orders?",
  description: "",
  revision: 1,
  createdAt: at(0),
  updatedAt: at(updated),
  data: {
    goal: "Who has the most orders?",
    intent: "auto",
    resolvedIntent: "answer",
    outline: ["understand", "analyze", "check", "write"],
    ...data,
    events: events.map(([s, stage]) => ({
      at: at(s),
      stage,
      message: "a prompt the trace never shows",
    })),
  },
});
const answered = {
  status: "succeeded",
  answer: {
    title: "Customers by order count",
    rows: customers,
    total: 3,
    citations: [
      { datasetId: "a", name: "Orders" },
      { datasetId: "b", name: "Customers" },
    ],
    summary: "Lucas I. Kim has the most orders.",
    check: { ...answerShape(customers), duplicates: 0, missing: 0 },
  },
};
const states = (r: Resource) => runSteps(r).map((s) => [s.id, s.state]);

describe("analysis steps", () => {
  it("follows an answer's stages as four steps, with when each ran", () => {
    const done = run(answered, [
      [1, "routing"],
      [2, "routing"],
      [3, "analysis"],
      [6, "query"],
      [9, "check"],
      [9, "summary"],
      [14, "complete"],
    ]);
    const steps = runSteps(done);
    expect(
      steps.map((s) => [s.id, s.state, s.endedAt! - s.startedAt!]),
    ).toEqual([
      ["understand", "done", 5000],
      ["analyze", "done", 3000],
      ["check", "done", 0],
      ["write", "done", 5000],
    ]);
    // Folded, the last two steps that ran.
    expect(compactSteps(steps).map((s) => s.id)).toEqual(["check", "write"]);
    expect(steps.map((s) => stepText(s, done))).toEqual([
      "Planned “Customers by order count”",
      "Ran a read-only query on Orders and Customers.",
      "3 rows, ranked by order count, highest first.",
      "Summarized the result in plain words.",
    ]);
  });
  it("shows at most three steps while working, and never plans as done", () => {
    const working = run({ status: "running" }, [
      [1, "routing"],
      [3, "analysis"],
      [6, "query"],
    ]);
    expect(states(working)).toEqual([
      ["understand", "done"],
      ["analyze", "running"],
      ["check", "pending"],
      ["write", "pending"],
    ]);
    expect(compactSteps(runSteps(working)).map((s) => s.id)).toEqual([
      "understand",
      "analyze",
      "check",
    ]);
    // Before routing decides, nothing beyond the question is outlined.
    const routing = run({ status: "running", resolvedIntent: undefined }, [
      [1, "routing"],
    ]);
    expect(states(routing)).toEqual([["understand", "running"]]);
    // Queued, the first step waits.
    expect(
      states(run({ status: "queued", resolvedIntent: undefined }, [])),
    ).toEqual([["understand", "pending"]]);
  });
  it("keeps finished work when a run fails or is stopped", () => {
    const failed = run({ status: "failed", error: "Binder Error" }, [
      [1, "routing"],
      [3, "analysis"],
      [6, "query"],
      [7, "analysis-repair"],
      [9, "query"],
      [10, "failed"],
    ]);
    expect(states(failed)).toEqual([
      ["understand", "done"],
      ["analyze", "failed"],
      ["check", "pending"],
      ["write", "pending"],
    ]);
    const stopped = run({ status: "canceled" }, [[1, "routing"]], 4);
    expect(states(stopped)).toEqual([
      ["understand", "stopped"],
      ["analyze", "pending"],
      ["check", "pending"],
      ["write", "pending"],
    ]);
    // A corrected query says so.
    const repaired = run({ status: "running" }, [
      [3, "analysis"],
      [6, "query"],
      [7, "analysis-repair"],
    ]);
    expect(stepText(runSteps(repaired)[1], repaired)).toBe(
      "The first query didn’t run, so it is being corrected.",
    );
  });
  it("starts again on retry, with reused work done, and marks a missing summary", () => {
    const retried = run(
      { ...answered, answer: { ...answered.answer, summary: undefined } },
      [
        [1, "routing"],
        [3, "analysis"],
        [6, "failed"],
        [20, "queued"],
        [21, "routing"],
        [21, "analysis"],
        [22, "query"],
        [24, "check"],
        [24, "summary"],
        [26, "complete"],
      ],
    );
    expect(states(retried)).toEqual([
      ["understand", "done"],
      ["analyze", "done"],
      ["check", "done"],
      ["write", "skipped"],
    ]);
    expect(stepText(runSteps(retried)[3], retried)).toBe(
      "No summary this time; the result is below.",
    );
  });
  it("names a build's steps as they happen", () => {
    const build = run(
      {
        status: "awaiting_review",
        intent: "build",
        resolvedIntent: undefined,
        outline: undefined,
      },
      [
        [1, "catalog"],
        [4, "pipelines"],
        [8, "ontology"],
        [9, "dashboards"],
        [12, "validation"],
        [13, "dashboards_repair_0"],
        [15, "review"],
      ],
    );
    expect(states(build)).toEqual([
      ["data", "done"],
      ["prepare", "done"],
      ["model", "done"],
      ["view", "done"],
      ["test", "done"],
    ]);
  });
  it("leaves choosing to build out of a build's steps", () => {
    const routed = {
      status: "running",
      intent: "auto",
      resolvedIntent: "build",
      outline: undefined,
    };
    expect(states(run(routed, [[1, "routing"]]))).toEqual([
      ["data", "pending"],
    ]);
    expect(
      states(
        run(routed, [
          [1, "routing"],
          [3, "catalog"],
        ]),
      ),
    ).toEqual([["data", "running"]]);
  });
  it("words the check and the estimate", () => {
    expect(
      resultText(
        { ...answerShape(customers), leaders: 2, duplicates: 3, missing: 1 },
        3,
        120,
      ),
    ).toBe(
      "First 3 of 120 rows, ranked by order count, highest first. 2 share first place. 3 rows repeated exactly. 1 row without order count.",
    );
    expect(estimateRange({ low: 8200, high: 14900 })).toMatchObject({
      text: "8–15s",
      high: 15000,
    });
    expect(estimateRange({ low: 45000, high: 75000 })?.text).toBe("45s–1m 15s");
    expect(estimateRange(null)).toBeNull();
  });
});

describe("answer runs", () => {
  let store: MemoryStore,
    app: Awaited<ReturnType<typeof createApp>>,
    scope: string,
    sql: string,
    inputs: string[];
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
    const ds = await request("POST", "/ingest", {
      name: "Orders",
      rows: [
        { id: 1, customer: "Kim", amount: 25 },
        { id: 2, customer: "Kim", amount: 40 },
        { id: 3, customer: "Kim", amount: 10 },
        { id: 4, customer: "Mehta", amount: 15 },
        { id: 5, customer: "Mehta", amount: 5 },
        { id: 6, customer: "Parker", amount: 30 },
      ],
    });
    expect(ds.statusCode, ds.body).toBe(200);
    inputs = [ds.json().id];
    sql = `SELECT customer, count(*) AS order_count FROM ${datasetTable(inputs[0])} GROUP BY customer ORDER BY order_count DESC, customer`;
  });
  afterEach(async () => {
    vi.restoreAllMocks();
    await app.close();
    for (const d of ["incoming", "datasets"])
      await rm(resolve(config.dataDir, d, scope), {
        recursive: true,
        force: true,
      });
  });
  const model = (contexts: Record<string, any> = {}) =>
    vi
      .spyOn(Provider.prototype, "generate")
      .mockImplementation(async (system: string, context: any) => {
        if (system.includes("Choose answer"))
          return { value: { intent: "answer" }, tokens: 1 };
        if (system.includes("Summarise this query result")) {
          contexts.summary = context;
          return {
            value: { summary: "Kim has the most orders.", highlights: [] },
            tokens: 1,
          };
        }
        return {
          value: { title: "Orders by customer", sql, inputs, assumptions: [] },
          tokens: 1,
        };
      });
  const ask = async (goal: string, intent = "auto") => {
    const r = await request("POST", "/assistant/runs", { goal, intent });
    expect(r.statusCode, r.body).toBe(200);
    return r.json() as Resource;
  };
  const answer = async (id: string) => {
    await buildWorkspace(store, scope, id);
    const r = (await store.get(scope, id))!;
    expect(r.data.status, r.data.error).toBe("succeeded");
    return r;
  };
  const useHosted = (model: string, allowRawData: boolean) =>
    transaction(store, scope, async (tx) =>
      tx.put(
        resource(
          scope,
          "provider",
          "AI provider",
          {
            settings: { provider: "openai", model, allowRawData },
            secret: seal("sk-test-key"),
          },
          "provider",
        ),
      ),
    );

  it("checks order and ties over the full result, not only the rows shown", async () => {
    const query = (sql: string) =>
      localExecute({
        mode: "query",
        scope,
        sql,
        version: randomUUID(),
        limit: 100,
        order: true,
      });
    // 150 customers share first place; 100 rows are shown, all tied.
    const tied = await query(
      `SELECT 'c' || i::VARCHAR AS name, CASE WHEN i < 150 THEN 5 ELSE 3 END AS order_count FROM range(200) t(i) ORDER BY order_count DESC, i`,
    );
    expect(tied.rows).toHaveLength(100);
    expect(answerShape(tied.rows).kind).toBe("table");
    expect(answerShape(tied.rows, { profile: tied.profile })).toMatchObject({
      kind: "ranking",
      order: "desc",
      leaders: 150,
    });
    // Ranked in the rows shown, but not after them.
    const unranked = await query(
      `SELECT 'c' || i::VARCHAR AS name, CASE WHEN i < 100 THEN 500 - i ELSE 900 END AS order_count FROM range(150) t(i) ORDER BY i`,
    );
    expect(answerShape(unranked.rows).kind).toBe("ranking");
    expect(answerShape(unranked.rows, { profile: unranked.profile }).kind).toBe(
      "table",
    );
    expect(unranked.profile.columns[1].order).toEqual({
      up: 1,
      down: 99,
      first: 1,
    });
    // A measure empty in the rows shown is still the one asked about.
    const late = await query(
      `SELECT 'c' || i::VARCHAR AS name, 1000 - i AS visits, CASE WHEN i >= 100 THEN 200 - i END AS order_count FROM range(150) t(i) ORDER BY i`,
    );
    expect(
      answerShape(late.rows, {
        profile: late.profile,
        hint: "Which customers have the most orders?",
      }),
    ).toMatchObject({ kind: "ranking", measure: "order_count", leaders: 1 });
    // A column named rowid keeps its name, and every column its order.
    const ids = await query(
      `SELECT i AS rowid, 300 - i AS order_count FROM range(150) t(i) ORDER BY i`,
    );
    expect(Object.keys(ids.rows[0])).toEqual(["rowid", "order_count"]);
    expect(ids.profile.columns.map((c) => [c.name, c.order])).toEqual([
      ["rowid", { up: 149, down: 0, first: 1 }],
      ["order_count", { up: 0, down: 149, first: 1 }],
    ]);
  });
  it("checks the result in code before the summary, and outlines the steps", async () => {
    const contexts: Record<string, any> = {};
    model(contexts);
    const done = await answer((await ask("Who has the most orders?")).id);
    expect(done.data.outline).toEqual([
      "understand",
      "analyze",
      "check",
      "write",
    ]);
    expect(done.data.events.map((e: any) => e.stage)).toContain("check");
    expect(done.data.answer.check).toEqual({
      kind: "ranking",
      label: "customer",
      measure: "order_count",
      order: "desc",
      leaders: 1,
      keys: [],
      duplicates: 0,
      missing: 0,
    });
    // The summary is told what code verified, so it never invents a leader.
    expect(contexts.summary.result.check).toMatchObject({
      shape: "ranking",
      measure: "order_count",
      leaders: 1,
    });
    expect(runSteps(done).map((s) => s.state)).toEqual([
      "done",
      "done",
      "done",
      "done",
    ]);
    // An explicit question has no routing step but the same outline; where
    // rows can't be sent to the model, there is no summary step.
    await useHosted("gpt-5.5", false);
    const plain = await answer(
      (await ask("Orders per customer?", "answer")).id,
    );
    expect(plain.data.outline).toEqual(["understand", "analyze", "check"]);
    expect(plain.data.events[0].stage).toBe("analysis");
    expect(plain.data.answer.summary).toBeUndefined();
  });

  it("estimates from comparable answers only, once, and not for a retry", async () => {
    model();
    const first = await ask("Who has the most orders?");
    expect(first.data.estimate).toBeNull();
    for (const goal of [
      "Who has the most orders?",
      "Which customer orders most?",
      "Top customers by orders?",
    ]) {
      const r = await answer((await ask(goal)).id);
      expect(r.data.timing.key).toMatch(/^local:qwen3:4b:summary:\d+$/);
      expect(r.data.timing.took).toBeGreaterThanOrEqual(0);
    }
    const next = await ask("And the fewest orders?");
    expect(next.data.estimate).toMatchObject({ samples: 3 });
    expect(next.data.estimate.low).toBeLessThanOrEqual(next.data.estimate.high);
    // Proposals take their own time and get no estimate.
    expect(
      (await ask("Build a dashboard of orders", "build")).data.estimate,
    ).toBeUndefined();
    // Another model is not comparable.
    await useHosted("gpt-5.5", true);
    expect(
      (await ask("Who has the most orders now?")).data.estimate,
    ).toBeNull();
  });

  it("times only a first attempt, and a retry shows reused work as done", async () => {
    model();
    // Stopped before it was worked on, then retried.
    const asked = await ask("Who has the most orders?");
    await request("POST", `/assistant/runs/${asked.id}/cancel`, {});
    const resumed = await request(
      "POST",
      `/assistant/runs/${asked.id}/resume`,
      {},
    );
    expect(resumed.json().data.estimate).toBeNull();
    const done = await answer(asked.id);
    expect(done.data.timing).toBeUndefined();
    expect(runSteps(done).map((s) => [s.id, s.state])).toEqual([
      ["understand", "done"],
      ["analyze", "done"],
      ["check", "done"],
      ["write", "done"],
    ]);
    // A second retry reuses the saved plan, and says so in its events.
    await transaction(store, scope, async (tx) => {
      const r = (await tx.get(asked.id))!;
      tx.update(r, { ...r.data, status: "failed", error: "Lost" });
    });
    await request("POST", `/assistant/runs/${asked.id}/resume`, {});
    const again = await answer(asked.id);
    const attempt = again.data.events.slice(
      again.data.events.findLastIndex((e: any) => e.stage === "queued") + 1,
    );
    expect(attempt).toContainEqual(
      expect.objectContaining({
        stage: "analysis",
        message: "Reused the saved result",
      }),
    );
    expect(runSteps(again)[0]).toMatchObject({
      id: "understand",
      state: "done",
    });
  });
});
