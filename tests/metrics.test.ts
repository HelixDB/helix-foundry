import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
// Counts the queries Home runs, so caching can be asserted.
const executor = vi.hoisted(() => ({ queries: 0 }));
vi.mock("../apps/api/src/executor.js", () => ({
  execute: async (job: any) => {
    if (job.mode === "query") executor.queries++;
    return localExecute(job);
  },
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import { queryDatasets } from "../apps/api/src/datasets.js";
import {
  compileMetric,
  describeForMetrics,
  metricsDatasets,
  METRICS_PROMPT,
} from "../apps/api/src/metrics.js";
import { sampleDatasets } from "../apps/api/src/sample-data.js";
import { Provider } from "../apps/api/src/providers.js";
import { config } from "../apps/api/src/config.js";
import type {
  HomeMetric,
  HomeMetrics,
  MetricSpec,
} from "../packages/shared/src/index.js";

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
  executor.queries = 0;
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const r = await app.inject({ method: "GET", url: "/api/v1/me" });
  expect(r.statusCode).toBe(200);
  scope = r.json().workspaces[0].id;
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
async function samples() {
  const r = await request("POST", "/demo", { advance: true });
  expect(r.statusCode, r.body).toBe(200);
}
async function home(): Promise<HomeMetrics> {
  const r = await request("GET", "/home/metrics");
  expect(r.statusCode, r.body).toBe(200);
  return r.json();
}
const metricsRuns = async () =>
  (await store.list(scope, "run")).filter(
    (r) => r.data.task === "home_metrics",
  );
const datasetNamed = async (name: string) =>
  (await store.list(scope, "dataset")).find((d) => d.name === name)!;
// The queries one call runs, from a zeroed counter.
async function counted<T>(fn: () => Promise<T>) {
  executor.queries = 0;
  const value = await fn();
  return { value, queries: executor.queries };
}
// A new version of a sample dataset with its rows changed.
async function reingest(
  name: string,
  change: (rows: Record<string, any>[]) => void,
) {
  const d = await datasetNamed(name);
  const rows = rowsOf(name).map((r) => ({ ...r }));
  change(rows);
  const r = await request("POST", "/ingest", { name, rows, datasetId: d.id });
  expect(r.statusCode, r.body).toBe(200);
  expect(r.json().data.activeVersion).not.toBe(d.data.activeVersion);
}

// The sample rows, generated once; they are deterministic.
let sampleRows: Record<string, Record<string, any>[]> | undefined;
const rowsOf = (name: string) =>
  (sampleRows ??= Object.fromEntries(
    sampleDatasets().map((d) => [d.name, d.rows as Record<string, any>[]]),
  ))[name];
const DAY = 86_400_000;
// The last 30 days of data against the 30 before, from the rows themselves.
function window30(rows: Record<string, any>[], time: string, value: string) {
  const at = (r: Record<string, any>) => Date.parse(r[time]);
  const end = Math.max(...rows.map(at));
  const sum = (from: number, to: number) =>
    rows
      .filter((r) => at(r) > from && at(r) <= to)
      .reduce((n, r) => n + Number(r[value]), 0);
  return {
    value: sum(end - 30 * DAY, end),
    previous: sum(end - 60 * DAY, end - 30 * DAY),
    asOf: new Date(end).toISOString().slice(0, 10),
  };
}
const byTitle = (m: HomeMetrics, title: string) =>
  m.metrics.find((x) => x.title === title);
const sortedDesc = (points: { value: number }[]) =>
  points.every((p, i) => i === 0 || points[i - 1].value >= p.value);

describe("code-picked metrics", () => {
  it("measures growth, revenue and a breakdown without AI and queues nothing", async () => {
    await samples();
    const m = await home();
    expect(m).toMatchObject({
      status: "ready",
      source: "computed",
      aiReady: false,
      generating: false,
    });
    expect(m.error).toBeUndefined();
    expect(m.generatedAt).toBeTruthy();
    expect(m.computedAt).toBeTruthy();
    expect(m.metrics.length).toBeGreaterThanOrEqual(4);
    expect(m.metrics.length).toBeLessThanOrEqual(8);
    expect(new Set(m.metrics.map((x) => x.id)).size).toBe(m.metrics.length);
    // Two sources of the same records don't show up as two identical cards.
    expect(new Set(m.metrics.map((x) => x.title)).size).toBe(m.metrics.length);
    // Every stand-in ran on the data.
    for (const x of m.metrics) {
      expect(x.result.error, x.title).toBeUndefined();
      expect(x.dataset, x.title).toBeTruthy();
    }

    const kpi = m.metrics.find((x) => x.kind === "kpi" && x.window === "30d")!;
    expect(kpi).toBeDefined();
    expect(kpi.result.value).toEqual(expect.any(Number));
    expect(kpi.result.previous).toEqual(expect.any(Number));
    expect(kpi.result.asOf).toMatch(/^\d{4}-\d{2}-\d{2}$/);

    const trend = m.metrics.find(
      (x) => x.kind === "trend" && x.grain === "month",
    )!;
    expect(trend).toBeDefined();
    expect(trend.window).toBeUndefined();
    const labels = trend.result.points!.map((p) => p.label);
    expect(labels.length).toBeGreaterThanOrEqual(3);
    expect(labels.every((l) => /^\d{4}-\d{2}$/.test(l))).toBe(true);
    expect(labels).toEqual([...labels].sort());
    expect(new Set(labels).size).toBe(labels.length);

    const breakdown = m.metrics.find((x) => x.kind === "breakdown")!;
    expect(breakdown).toBeDefined();
    expect(breakdown.window).toBeUndefined();
    expect(breakdown.result.points!.length).toBeGreaterThanOrEqual(2);
    expect(breakdown.result.points!.length).toBeLessThanOrEqual(8);
    expect(sortedDesc(breakdown.result.points!)).toBe(true);

    // Revenue: Stripe amounts are cents; Home shows dollars for the same
    // 30-day window the rows give.
    const invoices = rowsOf("Stripe · invoices");
    const expected = window30(invoices, "created", "amount_paid");
    const revenue = byTitle(m, "Revenue")!;
    expect(revenue).toMatchObject({
      kind: "kpi",
      format: "currency",
      dataset: "Invoices",
      window: "30d",
    });
    expect(expected.value).toBeGreaterThan(0);
    expect(revenue.result.value).toBeCloseTo(expected.value / 100, 2);
    expect(revenue.result.previous).toBeCloseTo(expected.previous / 100, 2);
    expect(revenue.result.asOf).toBe(expected.asOf);
    expect(revenue.result.value!).toBeLessThan(
      invoices.reduce((n, r) => n + r.amount_paid, 0) / 100,
    );

    expect(await metricsRuns()).toEqual([]);
  });

  it("serves cached results and re-runs them by code only when the data changes", async () => {
    await samples();
    const generate = vi
      .spyOn(Provider.prototype, "generate")
      .mockRejectedValue(new Error("Home must not call the model"));
    // Two requests at once compute once.
    const first = await counted(() => Promise.all([home(), home()]));
    const [m, twin] = first.value;
    expect(twin).toEqual(m);
    expect(first.queries).toBeGreaterThan(0);

    const cached = await counted(home);
    expect(cached.queries).toBe(0);
    expect(cached.value).toEqual(m);

    // A new version of the invoices: more paid in the latest invoice.
    await reingest("Stripe · invoices", (rows) => {
      const latest = rows.reduce((a, b) =>
        Date.parse(a.created) >= Date.parse(b.created) ? a : b,
      );
      latest.amount_paid += 12345;
    });
    const changed = await counted(home);
    const next = changed.value;
    // The stored queries run again, and nothing is picked again.
    expect(changed.queries).toBeGreaterThan(0);
    expect(changed.queries).toBeLessThanOrEqual(m.metrics.length);
    expect(next.source).toBe("computed");
    expect(next.metrics.map((x) => x.id)).toEqual(m.metrics.map((x) => x.id));
    expect(next.generatedAt).toBe(m.generatedAt);
    expect(next.computedAt! > m.computedAt!).toBe(true);
    expect(byTitle(next, "Revenue")!.result.value).toBeCloseTo(
      byTitle(m, "Revenue")!.result.value! + 123.45,
      2,
    );
    for (const x of next.metrics.filter((x) => x.dataset !== "Invoices"))
      expect(x.result).toEqual(m.metrics.find((y) => y.id === x.id)!.result);
    expect((await counted(home)).queries).toBe(0);

    // Regenerate without AI recomputes the stand-ins by code: the same
    // queries as the first load.
    const regenerated = await counted(() =>
      request("POST", "/home/metrics/regenerate", {}),
    );
    expect(regenerated.value.statusCode, regenerated.value.body).toBe(200);
    expect(regenerated.queries).toBe(first.queries);
    expect(regenerated.value.json()).toMatchObject({
      source: "computed",
      generating: false,
    });
    expect(
      regenerated.value.json().metrics.map((x: HomeMetric) => x.id),
    ).toEqual(m.metrics.map((x) => x.id));
    expect(generate).not.toHaveBeenCalled();
    expect(await metricsRuns()).toEqual([]);
  });

  it("returns empty for a workspace with no data", async () => {
    expect(await home()).toEqual({
      status: "empty",
      source: "computed",
      aiReady: false,
      generating: false,
      metrics: [],
    });
    await provider();
    const regenerated = await request("POST", "/home/metrics/regenerate", {});
    expect(regenerated.statusCode, regenerated.body).toBe(200);
    expect(regenerated.json()).toMatchObject({
      status: "empty",
      aiReady: true,
      generating: false,
      metrics: [],
    });
    expect(executor.queries).toBe(0);
    expect(await store.list(scope, "run")).toEqual([]);
  });
});

it("leaves out data excluded from setup through its source or import", async () => {
  const s = new MemoryStore(),
    w = "excluded";
  await transaction(s, w, async (tx) => {
    tx.put(
      resource(
        w,
        "workspace",
        w,
        { onboarding: { excludedIds: ["source", "job"] } },
        "workspace",
      ),
    );
    for (const [id, link] of [
      ["one", { sourceId: "source" }],
      ["two", { importJobId: "job" }],
      ["kept", {}],
    ] as const)
      tx.put(
        resource(
          w,
          "dataset",
          id,
          {
            activeVersion: "v",
            profile: { rows: 10, columns: [{ name: "id", type: "INTEGER" }] },
            ...link,
          },
          id,
        ),
      );
  });
  expect((await metricsDatasets(s, w)).map((d) => d.id)).toEqual(["kept"]);
});

describe("compileMetric", () => {
  const id = "11111111-2222-3333-4444-555555555555",
    table = '"d_11111111222233334444555555555555"';
  const dataset = resource(
    "unit",
    "dataset",
    "Stripe · invoices",
    {
      activeVersion: "v1",
      profile: {
        rows: 500,
        columns: [
          {
            name: "invoice_id",
            type: "VARCHAR",
            distinct: 500,
            primaryKey: true,
          },
          { name: "email", type: "VARCHAR", distinct: 40, sensitive: true },
          { name: "memo", type: "VARCHAR", distinct: 480 },
          { name: "status", type: "VARCHAR", distinct: 3 },
          { name: "amount_paid", type: "BIGINT", distinct: 40 },
          { name: "created", type: "TIMESTAMP", distinct: 500 },
          {
            name: "plan",
            type: "STRUCT(id VARCHAR, amount BIGINT)",
            distinct: 3,
          },
          { name: "tags", type: "VARCHAR[]", distinct: 4 },
          { name: "scores", type: "INTEGER[3]", distinct: 3 },
          { name: "periods", type: "TIMESTAMP[2]", distinct: 2 },
          { name: 'odd "tier"', type: "VARCHAR", distinct: 4 },
        ],
        sample: [
          { status: "paid", memo: "Thanks", email: "a@x.example" },
          { status: "void", memo: "Refund" },
          { status: "None" },
          { status: "paid" },
        ],
      },
    },
    id,
  );
  const spec = (s: Partial<MetricSpec>): MetricSpec => ({
    title: "Metric",
    why: "",
    kind: "kpi",
    datasetId: id,
    measure: "count",
    format: "number",
    scale: "none",
    ...s,
  });
  const error = (s: Partial<MetricSpec>) => {
    const c = compileMetric(spec(s), dataset);
    expect("error" in c, JSON.stringify(c)).toBe(true);
    return (c as { error: string }).error;
  };
  const sql = (
    s: Partial<MetricSpec> & { grain?: "day" | "week" | "month" },
  ) => {
    const c = compileMetric(spec(s), dataset);
    expect("sql" in c, JSON.stringify(c)).toBe(true);
    return (c as { sql: string }).sql;
  };

  it("rejects specs that don't fit the dataset", () => {
    expect("error" in compileMetric(spec({}), undefined)).toBe(true);
    // Unknown columns.
    expect(error({ measure: "sum", column: "nope" })).toMatch(/nope/);
    expect(error({ measure: "count_distinct", column: "nope" })).toMatch(
      /nope/,
    );
    expect(error({ timeColumn: "nope" })).toMatch(/date or time/);
    expect(error({ kind: "breakdown", groupBy: "nope" })).toMatch(/category/);
    expect(error({ filter: { column: "nope", value: "x" } })).toMatch(
      /filtered/,
    );
    // Sums need numbers, and never of sensitive columns.
    expect(error({ measure: "sum", column: "status" })).toMatch(/numeric/);
    expect(error({ measure: "avg", column: "created" })).toMatch(/numeric/);
    expect(error({ measure: "sum", column: "email" })).toMatch(/numeric/);
    expect(error({ measure: "sum" })).toBeTruthy();
    // Time columns must be dates or times; a trend needs one.
    expect(error({ timeColumn: "status" })).toMatch(/date or time/);
    expect(error({ timeColumn: "amount_paid", kind: "trend" })).toMatch(
      /date or time/,
    );
    expect(error({ kind: "trend" })).toMatch(/date or time/);
    // Breakdowns are by a category: not a key, free text, a sensitive column
    // or a number.
    for (const groupBy of ["invoice_id", "memo", "email", "amount_paid"])
      expect(error({ kind: "breakdown", groupBy }), groupBy).toMatch(
        /category/,
      );
    expect(error({ kind: "breakdown" })).toBeTruthy();
    // Nested columns can't be measured, filtered or grouped.
    expect(error({ measure: "count_distinct", column: "plan" })).toBeTruthy();
    expect(error({ measure: "count_distinct", column: "tags" })).toBeTruthy();
    expect(error({ measure: "sum", column: "plan" })).toBeTruthy();
    expect(error({ filter: { column: "plan", value: "x" } })).toBeTruthy();
    expect(error({ filter: { column: "tags", value: "x" } })).toBeTruthy();
    expect(error({ kind: "breakdown", groupBy: "plan" })).toBeTruthy();
    expect(error({ kind: "breakdown", groupBy: "tags" })).toBeTruthy();
    // So are fixed-size arrays of numbers or times.
    expect(error({ measure: "sum", column: "scores" })).toMatch(/numeric/);
    expect(error({ kind: "trend", timeColumn: "periods" })).toMatch(
      /date or time/,
    );
  });

  it("quotes identifiers, escapes values and scales cents", () => {
    const grouped = sql({ kind: "breakdown", groupBy: 'odd "tier"' });
    expect(grouped).toContain(`CAST("odd ""tier""" AS VARCHAR)`);
    expect(grouped).toContain(`FROM ${table}`);
    expect(grouped).toMatch(/ORDER BY 2 DESC/);
    expect(grouped).toMatch(/LIMIT 8$/);

    const filtered = sql({ filter: { column: "status", value: "it's'; --" } });
    expect(filtered).toContain(`'it''s''; --'`);
    expect(filtered).not.toContain(`'it's`);
    expect(filtered).toContain(`"status"`);

    const cents = sql({
      measure: "sum",
      column: "amount_paid",
      scale: "cents",
    });
    expect(cents).toContain(`sum(TRY_CAST("amount_paid" AS DOUBLE)) / 100.0`);
    expect(
      sql({ measure: "sum", column: "amount_paid", scale: "none" }),
    ).not.toContain("100.0");
    // Both windows of a kpi divide by 100.
    const windowed = sql({
      measure: "sum",
      column: "amount_paid",
      scale: "cents",
      timeColumn: "created",
    });
    expect(windowed.match(/\) FILTER \(WHERE [^)]*\) \/ 100\.0/g)).toHaveLength(
      2,
    );
    expect(windowed).toContain(`"asOf"`);
    expect(windowed).toContain(`"previous"`);

    expect(
      sql({
        measure: "count_distinct",
        column: "status",
        kind: "trend",
        timeColumn: "created",
      }),
    ).toContain(`count(DISTINCT "status")`);
    // Every period from first to last, zero-filled for counts and sums.
    const monthly = sql({ kind: "trend", timeColumn: "created" });
    expect(monthly).toContain(
      `date_trunc('month', CAST("created" AS TIMESTAMP))`,
    );
    expect(monthly).toContain("INTERVAL 1 MONTH");
    expect(monthly).toContain(`'%Y-%m'`);
    expect(monthly).toContain(`COALESCE("__v", 0)`);
    const weekly = sql({ kind: "trend", timeColumn: "created", grain: "week" });
    expect(weekly).toContain(
      `date_trunc('week', CAST("created" AS TIMESTAMP))`,
    );
    expect(weekly).toContain("INTERVAL 1 WEEK");
    expect(weekly).toContain(`'%Y-%m-%d'`);
    expect(
      sql({
        kind: "trend",
        timeColumn: "created",
        measure: "avg",
        column: "amount_paid",
      }),
    ).not.toContain("COALESCE");
  });

  it("describes columns and category values for the AI, never nested columns or rows", () => {
    const [d] = JSON.parse(JSON.stringify(describeForMetrics([dataset])));
    expect(d).not.toHaveProperty("sample");
    expect(d).toMatchObject({ id, name: "Stripe · invoices", rows: 500 });
    const columns: string[] = d.columns;
    expect(columns).toContain("invoice_id VARCHAR unique");
    expect(columns).toContain("status VARCHAR values: paid|void");
    expect(columns).toContain("amount_paid BIGINT");
    expect(columns).toContain("created TIMESTAMP");
    const names = columns.map((c) => c.split(" ")[0]);
    expect(names).not.toContain("plan");
    expect(names).not.toContain("tags");
    expect(names).not.toContain("scores");
    expect(names).not.toContain("periods");
    // Free text and sensitive values are never listed.
    const text = JSON.stringify(d);
    expect(text).not.toContain("Thanks");
    expect(text).not.toContain("a@x.example");
  });

  it("runs the compiled SQL on the data: quoted filter values and cents in dollars", async () => {
    await samples();
    const invoices = await datasetNamed("Stripe · invoices");
    const value = async (s: Partial<MetricSpec>) => {
      const c = compileMetric(spec({ ...s, datasetId: invoices.id }), invoices);
      expect("sql" in c, JSON.stringify(c)).toBe(true);
      const r = await queryDatasets(
        store,
        scope,
        [invoices.id],
        (c as { sql: string }).sql,
      );
      return r.rows[0] as Record<string, any>;
    };
    const rows = rowsOf("Stripe · invoices");
    expect(
      Number(
        (await value({ filter: { column: "status", value: "paid" } })).value,
      ),
    ).toBe(rows.filter((r) => r.status === "paid").length);
    expect(
      Number(
        (await value({ filter: { column: "status", value: "it's paid" } }))
          .value,
      ),
    ).toBe(0);
    const total = rows.reduce((n, r) => n + r.amount_paid, 0);
    expect(
      Number(
        (await value({ measure: "sum", column: "amount_paid", scale: "none" }))
          .value,
      ),
    ).toBe(total);
    expect(
      Number(
        (
          await value({
            measure: "sum",
            column: "amount_paid",
            scale: "cents",
          })
        ).value,
      ),
    ).toBeCloseTo(total / 100, 2);
    const w = window30(rows, "created", "amount_paid");
    const windowed = await value({
      measure: "sum",
      column: "amount_paid",
      scale: "cents",
      timeColumn: "created",
    });
    expect(Number(windowed.value)).toBeCloseTo(w.value / 100, 2);
    expect(Number(windowed.previous)).toBeCloseTo(w.previous / 100, 2);
    expect(windowed.asOf).toBe(w.asOf);
  });
});

describe("AI-picked metrics", () => {
  // The model's reply, in the shape its reply schema asks for (the dataset
  // under "datasetId" or "dataset").
  const reply =
    (specs: Record<string, any>[]) =>
    async (_system: string, _context: unknown, schema: unknown) => {
      const key = JSON.stringify(schema).includes('"datasetId"')
        ? "datasetId"
        : "dataset";
      return {
        value: {
          metrics: specs.map(({ datasetId, ...s }) => ({
            ...s,
            [key]: datasetId,
          })),
        },
        tokens: 42,
      };
    };
  // Five picks: three that work, one summing a text column in a table with
  // no money column, and the first again under another title.
  async function picks() {
    const invoices = await datasetNamed("Stripe · invoices"),
      subscriptions = await datasetNamed("Stripe · subscriptions"),
      accounts = await datasetNamed("PlanetScale · accounts");
    const revenue: MetricSpec = {
      title: "Revenue",
      why: "Money collected in the last 30 days.",
      kind: "kpi",
      datasetId: invoices.id,
      measure: "sum",
      column: "amount_paid",
      timeColumn: "created",
      format: "currency",
      scale: "cents",
    };
    return [
      revenue,
      {
        title: "Active subscriptions",
        why: "Customers paying today.",
        kind: "kpi",
        datasetId: subscriptions.id,
        measure: "count",
        filter: { column: "status", value: "active" },
        format: "number",
        scale: "none",
      },
      {
        title: "Accounts by plan",
        why: "Where customers sit across plans.",
        kind: "breakdown",
        datasetId: accounts.id,
        measure: "count",
        groupBy: "plan",
        format: "number",
        scale: "none",
      },
      {
        title: "Seats sold",
        why: "Seats across subscriptions.",
        kind: "kpi",
        datasetId: subscriptions.id,
        measure: "sum",
        column: "status",
        format: "number",
        scale: "none",
      },
      { ...revenue, title: "Paid revenue" },
    ] satisfies MetricSpec[];
  }
  const good = ["Revenue", "Active subscriptions", "Accounts by plan"];

  it("shows stand-ins at once, asks the AI once from the schema and keeps its working picks", async () => {
    await provider();
    await samples();
    const first = await home();
    expect(first).toMatchObject({
      status: "ready",
      source: "computed",
      aiReady: true,
      generating: true,
    });
    expect(first.error).toBeUndefined();
    expect(first.metrics.length).toBeGreaterThanOrEqual(4);
    expect(await metricsRuns()).toHaveLength(1);
    const [run] = await metricsRuns();
    expect(run.data).toMatchObject({
      task: "home_metrics",
      background: true,
      status: "queued",
    });
    expect(run.data.datasetIds).toHaveLength(sampleDatasets().length);
    // Asking again while it's queued doesn't queue another.
    const second = await counted(home);
    expect(second.value).toEqual(first);
    expect(second.queries).toBe(0);
    expect(await metricsRuns()).toHaveLength(1);

    const specs = await picks();
    const generate = vi
      .spyOn(Provider.prototype, "generate")
      .mockImplementation(reply(specs));
    await buildWorkspace(store, scope, run.id);
    expect(generate).toHaveBeenCalledTimes(1);
    const [system, context, schema] = generate.mock.calls[0] as [
      string,
      any,
      unknown,
    ];
    expect(system).toContain(METRICS_PROMPT);
    // The model sees one line per column, never rows.
    expect(context.datasets).toHaveLength(sampleDatasets().length);
    for (const d of context.datasets) {
      expect(d).not.toHaveProperty("sample");
      expect(d.columns.length).toBeGreaterThan(0);
      expect(d.columns.every((c: unknown) => typeof c === "string")).toBe(true);
      for (const v of Object.values(d))
        if (Array.isArray(v))
          expect(v.every((x) => typeof x !== "object")).toBe(true);
    }
    const sent = JSON.stringify(context);
    expect(sent).not.toContain(rowsOf("PlanetScale · users")[0].email);
    expect(sent).not.toContain(rowsOf("Stripe · invoices")[0].invoice_id);
    expect(sent).not.toContain(rowsOf("PlanetScale · accounts")[0].domain);
    const columns = (name: string) =>
      context.datasets.find((d: any) => d.name === name).columns as string[];
    expect(columns("Stripe · invoices")).toContain("invoice_id VARCHAR unique");
    expect(
      columns("Stripe · invoices").find((c) => c.startsWith("status ")),
    ).toMatch(/^status VARCHAR values: .*paid/);
    expect(
      columns("PlanetScale · accounts").find((c) => c.startsWith("plan ")),
    ).toMatch(/values: .*growth/);
    // Nested columns are left out.
    expect(
      columns("Stripe · subscriptions").map((c) => c.split(" ")[0]),
    ).not.toContain("plan");
    expect(context.ontology).toBeUndefined();
    // The reply schema only allows the workspace's datasets.
    expect(JSON.stringify(schema)).toContain(specs[0].datasetId);

    const done = (await store.get(scope, run.id))!;
    expect(done.data.status, done.data.error).toBe("succeeded");

    const picked = await home();
    expect(picked).toMatchObject({
      status: "ready",
      source: "ai",
      aiReady: true,
      generating: false,
    });
    expect(picked.error).toBeUndefined();
    // The AI's working picks come first, once each; code may top them up
    // with stand-ins that measure something else.
    const titles = picked.metrics.map((x) => x.title);
    expect(titles.slice(0, good.length)).toEqual(good);
    expect(titles).not.toContain("Paid revenue");
    expect(titles).not.toContain("Seats sold");
    expect(titles.filter((t) => t === "Revenue")).toHaveLength(1);
    expect(picked.metrics.length).toBeLessThanOrEqual(8);
    expect(new Set(picked.metrics.map((x) => x.id)).size).toBe(
      picked.metrics.length,
    );
    for (const x of picked.metrics)
      expect(x.result.error, x.title).toBeUndefined();

    const w = window30(rowsOf("Stripe · invoices"), "created", "amount_paid");
    expect(byTitle(picked, "Revenue")).toMatchObject({
      kind: "kpi",
      format: "currency",
      dataset: "Invoices",
      window: "30d",
      why: "Money collected in the last 30 days.",
    });
    expect(byTitle(picked, "Revenue")!.result.value).toBeCloseTo(
      w.value / 100,
      2,
    );
    const active = byTitle(picked, "Active subscriptions")!;
    expect(active.window).toBeUndefined();
    expect(active.result.value).toBe(
      rowsOf("Stripe · subscriptions").filter((r) => r.status === "active")
        .length,
    );
    const plans = byTitle(picked, "Accounts by plan")!;
    expect(plans.dataset).toBe("Accounts");
    expect(sortedDesc(plans.result.points!)).toBe(true);
    expect(plans.result.points!.reduce((n, p) => n + p.value, 0)).toBe(
      rowsOf("PlanetScale · accounts").length,
    );

    // Picked once: Home never asks again for the same schema.
    const again = await counted(home);
    expect(again.queries).toBe(0);
    expect(again.value).toEqual(picked);
    expect(generate).toHaveBeenCalledTimes(1);
    expect(await metricsRuns()).toHaveLength(1);

    // New data re-runs the AI's queries by code.
    await reingest("Stripe · invoices", (rows) =>
      rows.push({
        ...rows.at(-1)!,
        invoice_id: "in_extra",
        amount_paid: 50000,
      }),
    );
    const refreshed = await counted(home);
    expect(refreshed.queries).toBeGreaterThan(0);
    expect(refreshed.queries).toBeLessThanOrEqual(picked.metrics.length);
    expect(refreshed.value).toMatchObject({
      source: "ai",
      generating: false,
      generatedAt: picked.generatedAt,
    });
    expect(refreshed.value.metrics.map((x) => x.id)).toEqual(
      picked.metrics.map((x) => x.id),
    );
    expect(byTitle(refreshed.value, "Revenue")!.result.value).toBeCloseTo(
      byTitle(picked, "Revenue")!.result.value! + 500,
      2,
    );
    expect(generate).toHaveBeenCalledTimes(1);
    expect(await metricsRuns()).toHaveLength(1);
  });

  it("regenerates with a new AI run and keeps the metrics when it fails", async () => {
    await provider();
    await samples();
    await home();
    const [run] = await metricsRuns();
    const generate = vi
      .spyOn(Provider.prototype, "generate")
      .mockImplementation(reply(await picks()));
    await buildWorkspace(store, scope, run.id);
    const picked = await home();
    expect(picked.source).toBe("ai");

    const regenerated = await request("POST", "/home/metrics/regenerate", {});
    expect(regenerated.statusCode, regenerated.body).toBe(200);
    // The current metrics stay while the AI picks again.
    expect(regenerated.json()).toMatchObject({
      source: "ai",
      generating: true,
      metrics: picked.metrics,
    });
    const runs = await metricsRuns();
    expect(runs).toHaveLength(2);
    const next = runs.find((r) => r.id !== run.id)!;
    expect(next.data).toMatchObject({
      task: "home_metrics",
      background: true,
      status: "queued",
    });
    expect((await home()).generating).toBe(true);
    expect(await metricsRuns()).toHaveLength(2);

    generate.mockRejectedValue(new Error("The model is unavailable"));
    await buildWorkspace(store, scope, next.id);
    expect(generate).toHaveBeenCalledTimes(2);
    expect((await store.get(scope, next.id))?.data).toMatchObject({
      status: "failed",
      error: "The model is unavailable",
    });
    const failed = await home();
    expect(failed).toMatchObject({
      status: "ready",
      source: "ai",
      generating: false,
      error: "The model is unavailable",
    });
    expect(failed.metrics).toEqual(picked.metrics);
    expect(failed.generatedAt).toBe(picked.generatedAt);

    // Another try queues another run and clears the error while it runs; a
    // reply in the wrong shape fails in plain words and keeps the metrics.
    const retried = (
      await request("POST", "/home/metrics/regenerate", {})
    ).json();
    expect(retried).toMatchObject({ generating: true, source: "ai" });
    expect(retried.error).toBeUndefined();
    const third = (await metricsRuns()).find(
      (r) => r.id !== run.id && r.id !== next.id,
    )!;
    expect(third.data.status).toBe("queued");
    generate.mockResolvedValue({ value: { metrics: "none" }, tokens: 1 });
    await buildWorkspace(store, scope, third.id);
    const malformed = await home();
    expect(malformed.error).toMatch(/expected format/);
    expect(malformed.metrics).toEqual(picked.metrics);
    expect(await metricsRuns()).toHaveLength(3);
  });

  it("keeps the stand-ins when none of the AI's metrics run", async () => {
    await provider();
    await samples();
    const stand = await home();
    const [run] = await metricsRuns();
    const [, , accounts, seats] = await picks();
    vi.spyOn(Provider.prototype, "generate").mockImplementation(
      reply([seats, { ...accounts, groupBy: undefined }]),
    );
    await buildWorkspace(store, scope, run.id);
    // The reply was read; its metrics just didn't run.
    const failed = (await store.get(scope, run.id))!.data;
    expect(failed.status).toBe("failed");
    expect(failed.error).not.toMatch(/expected format/);
    const after = await home();
    expect(after).toMatchObject({
      source: "computed",
      generating: false,
      metrics: stand.metrics,
    });
    expect(after.error).toBeTruthy();
    expect(await metricsRuns()).toHaveLength(1);
  });

  it("picks again for a new schema and ignores a late pick for the old one", async () => {
    await provider();
    await samples();
    await home();
    const [stale] = await metricsRuns();
    // Leaving the events out changes what Home measures.
    const events = await datasetNamed("PostHog · events");
    const patched = await request("PATCH", "/onboarding", {
      excludedIds: [events.id],
    });
    expect(patched.statusCode, patched.body).toBe(200);
    const next = await home();
    expect(next).toMatchObject({ source: "computed", generating: true });
    expect(next.metrics.map((x) => x.dataset)).not.toContain("Events");
    const runs = await metricsRuns();
    expect(runs).toHaveLength(2);
    const fresh = runs.find((r) => r.id !== stale.id)!;
    expect(fresh.data.metricsSignature).not.toBe(stale.data.metricsSignature);
    expect(fresh.data.datasetIds).not.toContain(events.id);

    // The pick for the old schema finishes late and is not used.
    vi.spyOn(Provider.prototype, "generate").mockImplementation(
      reply(await picks()),
    );
    await buildWorkspace(store, scope, stale.id);
    const late = (await store.get(scope, stale.id))!.data;
    expect(late.status).toBe("failed");
    expect(late.error).not.toMatch(/expected format/);
    const still = await home();
    expect(still).toMatchObject({ source: "computed", generating: true });
    expect(still.metrics).toEqual(next.metrics);
    await buildWorkspace(store, scope, fresh.id);
    expect((await store.get(scope, fresh.id))?.data.status).toBe("succeeded");
    const picked = await home();
    expect(picked.source).toBe("ai");
    expect(picked.metrics.slice(0, good.length).map((x) => x.title)).toEqual(
      good,
    );
  });
});
