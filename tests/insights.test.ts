import { beforeEach, afterEach, describe, it, expect, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
// Records the queries a scan runs, and can make them fail.
const executor = vi.hoisted(() => ({
  queries: [] as any[],
  fail: undefined as undefined | ((job: any) => boolean),
}));
vi.mock("../apps/api/src/executor.js", () => ({
  execute: async (job: any) => {
    if (job.mode === "query") {
      executor.queries.push(job);
      if (executor.fail?.(job)) throw new Error("Executor unavailable");
    }
    return localExecute(job);
  },
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import {
  buildWorkspace,
  publishProposal,
  saveProposal,
} from "../apps/api/src/assistant.js";
import { ingestRows, queryDatasets } from "../apps/api/src/datasets.js";
import { rankCandidates, runInsightsScan } from "../apps/api/src/insights.js";
import {
  chooseWindow,
  DEFAULT_THRESHOLDS,
  driverQuery,
} from "../apps/api/src/insights-checks.js";
import {
  acceptedOntology,
  findActivities,
  publishedBundle,
  proposeDrivers,
  type Activity,
  type Driver,
} from "../apps/api/src/insights-model.js";
import { config } from "../apps/api/src/config.js";
import {
  BundleSchema,
  collapseActivity,
  describeActivity,
  type HomeInsight,
  type HomeInsights,
} from "../packages/shared/src/index.js";
import { clinic, commerce, weekStart } from "./fixtures/business.js";

let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string,
  actor: string;
const request = (method: any, path: string, payload?: unknown) =>
  app.inject({
    method,
    url: `/api/v1/workspaces/${scope}` + path,
    payload: payload as any,
  });
beforeEach(async () => {
  executor.queries = [];
  executor.fail = undefined;
  store = new MemoryStore();
  app = await createApp(store);
  await app.ready();
  const me = (await app.inject({ method: "GET", url: "/api/v1/me" })).json();
  scope = me.workspaces[0].id;
  actor = me.user.id;
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

const DAY = 86_400_000;
// Ingests the tables, confirms the suggested ontology as it is and publishes.
async function publish(data: Record<string, Record<string, unknown>[]>) {
  for (const [name, rows] of Object.entries(data))
    await ingestRows(store, scope, name, rows);
  const s = (await request("POST", "/onboarding/ontology", {})).json();
  const c = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: s.ontology.hash,
    })
  ).json();
  await buildWorkspace(store, scope, c.buildRun.id);
  const o = (await request("GET", "/onboarding")).json();
  expect(o.complete, JSON.stringify(o.buildRun?.data.error)).toBe(true);
}
async function insights(): Promise<HomeInsights> {
  const r = await request("GET", "/home/insights");
  expect(r.statusCode, r.body).toBe(200);
  return r.json();
}
const stored = async () => (await store.get(scope, "home_insights"))?.data;
// Runs the queued scan as the worker would: claim, run, then succeed or fail.
async function runQueued() {
  const job = (await store.get(scope, (await stored())!.jobId))!;
  const set = (data: Record<string, unknown>) =>
    transaction(store, scope, async (tx) => {
      const j = (await tx.get(job.id))!;
      tx.update(j, { ...j.data, ...data });
    });
  await set({ status: "running" });
  try {
    await runInsightsScan(store, scope, job);
    await set({ status: "succeeded", error: null });
  } catch (e) {
    await set({ status: "failed", error: (e as Error).message });
    return e as Error;
  }
}
async function scan() {
  await insights();
  executor.queries = [];
  const error = await runQueued();
  expect(error).toBeUndefined();
  return insights();
}
const words = (s: string) => s.split(/\s+/).filter(Boolean).length;
const finding = (r: HomeInsights, check: string, re: RegExp) =>
  r.findings.find((f) => f.check === check && re.test(f.headline));

describe("review regressions", () => {
  it.each(["downstream", "upstream"] as const)(
    "bounds %s conversion links to the upstream period and never counts preceding events",
    async (side) => {
      const b = weekStart(1),
        c = weekStart(0),
        at = (t: number) => new Date(t).toISOString();
      const upRows = [
        { id: "b1", target: "d1", time: at(b + DAY) },
        { id: "b2", target: "d2", time: at(b + 2 * DAY) },
        { id: "c1", target: "d3", time: at(c + DAY) },
        { id: "c2", target: "d4", time: at(c + 2 * DAY) },
      ];
      const downstream = [
        { id: "d1", source: "b1", time: at(c + DAY) }, // after baseline ends
        { id: "d2", source: "b2", time: at(b + DAY) }, // before upstream event
        { id: "d3", source: "c1", time: at(c + DAY + 1000) },
        { id: "d3", source: "c1", time: at(c + DAY + 2000) }, // still one conversion
        { id: "d4", source: "c2", time: at(c + 8 * DAY) }, // after current ends
      ];
      const u = await ingestRows(store, scope, "Upstream", upRows),
        d = await ingestRows(store, scope, "Downstream", downstream);
      const activity = (id: string, datasetId: string): Activity => ({
        id,
        name: id,
        one: id,
        many: id,
        datasetId,
        datasetName: id,
        key: "id",
        time: { column: "time", zoned: false },
        rows: 4,
        direction: "up",
        participants: [],
        measures: [],
        dimensions: [],
        funnels: [],
      });
      const up = activity("up", u.id),
        down = activity("down", d.id);
      const driver: Driver = {
        id: "test",
        kind: "activity",
        label: "Test",
        activity: up,
        dimensions: [],
        prior: 1,
        reasons: [],
        checks: {
          cohort: "unsupported",
          mix: "unsupported",
          unit: "unsupported",
          conversion: null,
        },
        funnel: {
          upstream: "up",
          downstream: "down",
          link:
            side === "downstream"
              ? { side, column: "source", key: "id" }
              : { side, column: "target", key: "id" },
        },
      };
      const query = driverQuery(
        driver,
        {
          days: 7,
          end: c + 7 * DAY,
          current: [c, c + 7 * DAY],
          baseline: [b, c],
          cohort: [b - 14 * DAY, b],
          cohortReady: false,
        },
        DEFAULT_THRESHOLDS,
        new Map([
          ["up", up],
          ["down", down],
        ]),
      );
      const result = await queryDatasets(
        store,
        scope,
        query.inputs,
        query.sql,
        { limit: 1000 },
      );
      expect(
        result.rows
          .filter((r: any) => r.part === "conv_total")
          .map((r: any) => [r.w, r.n, r.v]),
      ).toEqual([
        ["base", 2, 0],
        ["cur", 2, 1],
      ]);
    },
  );

  it("sets aside missing baseline days rather than calling recovery a business shift", async () => {
    const data = commerce(),
      b = weekStart(1),
      c = weekStart(0);
    data.orders = data.orders.filter((o) => {
      const t = Date.parse(String(o.ordered_at));
      return t < b || t >= c || [0, 1, 6].includes(Math.floor((t - b) / DAY));
    });
    await publish(data);
    const r = await scan();
    expect(r.findings.some((f) => f.driver.label === "Revenue")).toBe(false);
    expect(
      r.coverage
        .find((c) => c.label === "Revenue")
        ?.checks.some(
          (c) => c.state === "set_aside" && c.note?.includes("baseline period"),
        ),
    ).toBe(true);
  });

  it("marks old event periods stale even when the snapshot was synchronized near them", async () => {
    const shift = 35 * DAY,
      data = commerce();
    for (const rows of Object.values(data))
      for (const row of rows)
        for (const [k, v] of Object.entries(row))
          if (typeof v === "string" && /^202\d-\d\d-\d\dT/.test(v))
            (row as Record<string, unknown>)[k] = new Date(
              Date.parse(v) - shift,
            ).toISOString();
    await publish(data);
    await transaction(store, scope, async (tx) => {
      for (const d of await tx.list("dataset"))
        tx.update(d, {
          ...d.data,
          lastSync: new Date(Date.now() - shift).toISOString(),
        });
    });
    const r = await scan();
    expect(r.findings.length).toBeGreaterThan(0);
    expect(
      r.findings.every((f) =>
        f.details.limitations.some((l) => l.startsWith("The data ends on")),
      ),
    ).toBe(true);
  });

  it.each([
    ["EUR", "€"],
    ["GBP", "£"],
    ["USD", "$"],
  ] as const)(
    "preserves %s currency in evidence and Analyst follow-ups",
    async (currency, symbol) => {
      const data = commerce({ hiddenShifts: true });
      for (const o of data.orders) {
        o["revenue_" + currency.toLowerCase()] = o.net_amount;
        delete o.net_amount;
      }
      await publish(data);
      const r = await scan(),
        f = r.findings.find((f) => f.check === "unit")!;
      expect(f).toBeDefined();
      expect(f.details.table.rows.flat().join(" ")).toContain(symbol);
      expect(f.ask).toContain(symbol);
      if (currency !== "USD") expect(f.ask).not.toContain("$");
    },
  );

  it("classifies named spend before generic currency suffixes and never proposes it as Revenue", async () => {
    const data = commerce();
    for (const o of data.orders) {
      o.spend_usd = o.net_amount;
      delete o.net_amount;
    }
    await publish(data);
    const ws = (await store.get(scope, "workspace"))!,
      datasets = await store.list(scope, "dataset"),
      bundle = await publishedBundle(
        (k) => store.list(scope, k),
        ws.data.activeGeneration,
      );
    const activities = findActivities(acceptedOntology(ws, bundle, datasets)),
      money = activities
        .flatMap((a) => a.measures)
        .find((m) => m.column === "spend_usd")!;
    expect(money).toMatchObject({ direction: "down", currency: "USD" });
    expect(
      proposeDrivers(activities, datasets, "").some(
        (d) => d.kind === "revenue",
      ),
    ).toBe(false);
  });

  it("rejects a dismissal from before a finding escalated", async () => {
    await publish(commerce());
    const r = await scan(),
      f = r.findings[0];
    await transaction(store, scope, async (tx) => {
      const s = (await tx.get("home_insights"))!;
      tx.update(s, {
        ...s.data,
        findings: s.data.findings.map((x: HomeInsight) =>
          x.id === f.id ? { ...x, version: "escalated", severity: "issue" } : x,
        ),
      });
    });
    const rejected = await request("POST", `/home/insights/${f.id}/dismiss`, {
      version: f.version,
    });
    expect(rejected.statusCode).toBe(409);
    expect(await store.list(scope, "insightDismissal")).toHaveLength(0);
    expect(
      (
        await request("POST", `/home/insights/${f.id}/dismiss`, {
          version: "escalated",
        })
      ).statusCode,
    ).toBe(200);
  });

  it("clears confirmation when the chosen driver no longer has sufficient history", async () => {
    await publish(commerce());
    const first = await scan(),
      revenue = first.priorities.candidates.find((d) => d.label === "Revenue")!;
    await request("PUT", "/home/insights/priorities", {
      drivers: [revenue.id],
    });
    await runQueued();
    const d = (await store.list(scope, "dataset")).find(
      (d) => d.name === "orders",
    )!;
    await ingestRows(
      store,
      scope,
      d.name,
      [{ ...commerce().orders[0], ordered_at: new Date().toISOString() }],
      undefined,
      d.id,
    );
    const refreshed = (await store.get(scope, d.id))!;
    if (refreshed.data.pendingVersion)
      expect(
        (await request("POST", `/datasets/${d.id}/accept-version`, {}))
          .statusCode,
      ).toBe(200);
    expect((await store.get(scope, d.id))!.data.activeVersion).not.toBe(
      d.data.activeVersion,
    );
    const after = await scan();
    expect(after.priorities.confirmed).toBe(false);
    expect((await stored())?.priorities).toBeUndefined();
    expect(after.scan.steps[0].detail).not.toContain("confirmed by an owner");
  });
});
// What every finding promises, whatever the data.
function wellFormed(r: HomeInsights) {
  expect(r.findings.length).toBeLessThanOrEqual(3);
  const selected = new Set(r.priorities.selected.map((d) => d.id));
  expect(r.priorities.selected.length).toBeLessThanOrEqual(3);
  for (const f of r.findings) {
    expect(selected.has(f.driver.id), f.headline).toBe(true);
    expect(
      words(f.support.text) + words(f.support.figure),
      JSON.stringify(f.support),
    ).toBeLessThanOrEqual(4);
    expect(["positive", "warning", "issue"]).toContain(f.severity);
    expect(f.details.table.rows.length).toBeGreaterThan(0);
    expect(f.details.query).toMatch(/^WITH /);
    expect(f.details.definition).not.toBe("");
    expect(f.details.limitations).toContain(
      "Two periods cannot establish long-term seasonality.",
    );
    expect(f.periods.current.end > f.periods.current.start).toBe(true);
    expect(f.periods.baseline.end).toBe(f.periods.current.start);
  }
  expect(new Set(r.findings.map((f) => f.id)).size).toBe(r.findings.length);
}

describe("a shop whose totals hold steady", () => {
  it("queues the scan off the request path and runs no query to show Home", async () => {
    await publish(commerce());
    executor.queries = [];
    const first = await insights();
    expect(executor.queries).toHaveLength(0);
    expect(first).toMatchObject({
      status: "ready",
      findings: [],
      scan: { state: "queued" },
    });
    expect(first.scan.steps.map((s) => s.label)).toEqual([
      "Understand the business",
      "Establish the fundamentals",
      "Look for meaningful shifts",
    ]);
    const job = await store.get(scope, (await stored())!.jobId);
    expect(job?.data).toMatchObject({
      type: "home_insights",
      status: "queued",
      background: true,
    });
    // Asking again reuses the queued scan.
    await insights();
    expect(
      (await store.list(scope, "job")).filter(
        (j) => j.data.type === "home_insights",
      ),
    ).toHaveLength(1);
    expect(executor.queries).toHaveLength(0);
  });

  it("analyses only the three selected drivers and keeps three findings across them", async () => {
    await publish(commerce());
    const r = await scan();
    wellFormed(r);
    expect(r.priorities.selected.map((d) => d.label)).toEqual([
      "Revenue",
      "Traffic",
      "Customer engagement",
    ]);
    // One bounded query per selected driver after the coverage probes; each
    // reads only that driver's tables.
    const checks = executor.queries.filter((q) => /^WITH a AS/.test(q.sql));
    const probes = executor.queries.filter((q) =>
      /'probe' AS part/.test(q.sql),
    );
    expect(checks).toHaveLength(3);
    expect(probes.length).toBeLessThanOrEqual(6);
    expect(executor.queries).toHaveLength(checks.length + probes.length);
    for (const q of checks) expect(q.inputs.length).toBe(1);
    expect(checks.every((q) => q.limit === 1000)).toBe(true);
    expect(r.findings).toHaveLength(3);
    expect(r.findings.map((f) => f.driver.label).sort()).toEqual(
      ["Customer engagement", "Revenue", "Traffic"].sort(),
    );
    expect(r.checkedAt).toBeDefined();
    expect(r.stale).toBe(false);
    expect(r.scan.state).toBe("idle");
    expect(r.scan.steps.every((s) => s.state === "done")).toBe(true);
    expect(r.scan.steps[0].detail).toBe(
      "Priorities: Revenue, Traffic and Customer engagement.",
    );
    // Showing it again neither queues nor queries.
    executor.queries = [];
    await insights();
    expect(executor.queries).toHaveLength(0);
    expect(
      (await store.list(scope, "job")).filter(
        (j) => j.data.type === "home_insights",
      ),
    ).toHaveLength(1);
  });

  it("reveals a source-mix shift, a fixed-cohort decline and buyer concentration with their figures", async () => {
    await publish(commerce());
    const r = await scan();
    const referral = finding(r, "mix", /^Referral sessions grew 150%/)!;
    expect(referral).toBeDefined();
    expect(referral.severity).toBe("positive");
    expect(referral.support).toEqual({
      figure: "500",
      text: "referral sessions",
      figureFirst: true,
    });
    expect(referral.values.baseline).toBeCloseTo(0.1, 6);
    expect(referral.values.current).toBeCloseTo(0.25, 6);
    expect(referral.values.absolute).toBeCloseTo(0.15, 6);
    expect(referral.values.relative).toBeCloseTo(1.5, 6);
    expect(referral.details.table.rows).toContainEqual([
      "referral",
      "200",
      "500",
      "+150%",
      "10% → 25%",
    ]);
    expect(referral.details.table.rows.at(-1)).toEqual([
      "Total",
      "2,000",
      "2,000",
      "0%",
      "100%",
    ]);
    expect(referral.details.contribution).toBe(
      "Referral added 300 sessions, going from 10% to 25% of the total. Sessions overall were unchanged.",
    );

    const cohort = finding(r, "cohort", /^Regular customers have fewer/)!;
    expect(cohort.severity).toBe("issue");
    expect(cohort.support).toEqual({
      text: "Regular sessions",
      figure: "↓30.0%",
    });
    expect(cohort.values).toMatchObject({
      baseline: 600,
      current: 420,
      absolute: -180,
      unit: "number",
    });
    expect(cohort.values.relative).toBeCloseTo(-0.3, 6);
    expect(cohort.details.table.rows).toEqual([
      ["Regular customers (120, fixed)", "600", "420", "−30.0%"],
      ["Everyone else", "1,400", "1,580", "+12.9%"],
      ["Total", "2,000", "2,000", "0%"],
    ]);
    expect(cohort.periods.cohort).toBeDefined();
    expect(cohort.details.definition).toMatch(
      /Membership stays fixed across both periods\. “Everyone else” covers other customers and sessions with no customer; it does not mean new customers\./,
    );

    const top = finding(r, "mix", /depends more on a few customers/)!;
    expect(top.severity).toBe("warning");
    expect(top.support).toEqual({ text: "Top customers", figure: "45% share" });
    expect(top.values.baseline).toBeCloseTo(0.22, 4);
    expect(top.values.current).toBeCloseTo(0.45, 4);
    expect(top.details.table.rows).toEqual([
      ["Top 10 customers", "6,600", "13,500"],
      ["Everyone else", "23,400", "16,500"],
      ["Total", "30,000", "30,000"],
    ]);
    expect(top.details.definition).toMatch(
      /ranked independently in each period/,
    );
    // Steady checks and unsupported ones say so, without inventing a finding.
    const revenue = r.coverage.find((c) => c.label === "Revenue")!;
    expect(revenue.checks.find((c) => c.check === "unit")).toMatchObject({
      state: "steady",
    });
    expect(revenue.checks.find((c) => c.check === "conversion")).toEqual({
      check: "conversion",
      label: "Progression and conversion",
      state: "skipped",
      note: "No recorded link between orders and a later outcome.",
    });
    // A check two lenses share runs once.
    expect(
      r.coverage
        .find((c) => c.label === "Traffic")!
        .checks.find((c) => c.check === "cohort")!.note,
    ).toBe("Checked under Customer engagement.");
  });

  it("dismisses a finding for this person, keeps it hidden while unchanged, and undoes", async () => {
    await publish(commerce());
    const r = await scan();
    const referral = finding(r, "mix", /^Referral/)!;
    const dismissed = (
      await request("POST", `/home/insights/${referral.id}/dismiss`, {
        version: referral.version,
      })
    ).json();
    expect(dismissed.findings.map((f: HomeInsight) => f.id)).not.toContain(
      referral.id,
    );
    expect(dismissed.dismissed).toBe(1);
    // An unchanged finding stays dismissed after another scan.
    await request("POST", "/home/insights/scan", {});
    await runQueued();
    const again = await insights();
    expect(again.findings.map((f) => f.id)).not.toContain(referral.id);
    expect(again.dismissed).toBe(1);
    const undone = (
      await request("DELETE", `/home/insights/${referral.id}/dismiss`)
    ).json();
    expect(undone.findings.map((f: HomeInsight) => f.id)).toContain(
      referral.id,
    );
    expect(undone.dismissed).toBe(0);
    expect(
      (await request("POST", "/home/insights/nope/dismiss", { version: "x" }))
        .statusCode,
    ).toBe(404);
  });

  it("brings a dismissed finding back when it becomes more severe or grows, saying why", async () => {
    await publish(commerce());
    const r = await scan();
    const cohort = finding(r, "cohort", /^Regular/)!;
    await request("POST", `/home/insights/${cohort.id}/dismiss`, {
      version: cohort.version,
    });
    const change = (patch: Record<string, unknown>) =>
      transaction(store, scope, async (tx) => {
        const s = (await tx.get("home_insights"))!;
        tx.update(s, {
          ...s.data,
          findings: s.data.findings.map((f: any) =>
            f.id === cohort.id ? { ...f, ...patch } : f,
          ),
        });
      });
    // Dismissed as an issue of −30%: a −35% issue stays hidden.
    await change({ magnitude: -0.35 });
    expect((await insights()).findings.map((f) => f.id)).not.toContain(
      cohort.id,
    );
    await change({ magnitude: -0.5 });
    const grown = (await insights()).findings.find((f) => f.id === cohort.id)!;
    expect(grown.returned).toBe(
      "Back because it grew: −30.0% when you dismissed it, −50.0% now.",
    );
    // Dismissing again records the new size.
    await request("POST", `/home/insights/${cohort.id}/dismiss`, {
      version: grown.version,
    });
    expect((await insights()).findings.map((f) => f.id)).not.toContain(
      cohort.id,
    );
    const top = finding(r, "mix", /depends more/)!;
    await request("POST", `/home/insights/${top.id}/dismiss`, {
      version: top.version,
    });
    await transaction(store, scope, async (tx) => {
      const s = (await tx.get("home_insights"))!;
      tx.update(s, {
        ...s.data,
        findings: s.data.findings.map((f: any) =>
          f.id === top.id ? { ...f, severity: "issue" } : f,
        ),
      });
    });
    expect(
      (await insights()).findings.find((f) => f.id === top.id)?.returned,
    ).toBe(
      "Back because it is now an issue; it was a warning when you dismissed it.",
    );
  });

  it("keeps earlier findings with their check time when a refresh fails, and retries on request", async () => {
    await publish(commerce());
    const before = await scan();
    // New data: the inputs change, so Home queues another scan.
    const sessions = (await store.list(scope, "dataset")).find(
      (d) => d.name === "sessions",
    )!;
    await ingestRows(
      store,
      scope,
      "sessions",
      commerce().sessions.slice(0, -5),
      undefined,
      sessions.id,
    );
    const queued = await insights();
    expect(queued.stale).toBe(true);
    expect(queued.scan.state).toBe("queued");
    expect(queued.findings.map((f) => f.id)).toEqual(
      before.findings.map((f) => f.id),
    );
    executor.fail = (job) => /^WITH a AS/.test(job.sql);
    const error = await runQueued();
    expect(error?.message).toBe("Executor unavailable");
    const failed = await insights();
    expect(failed.scan).toMatchObject({
      state: "failed",
      error: "Executor unavailable",
    });
    expect(failed.findings.map((f) => f.id)).toEqual(
      before.findings.map((f) => f.id),
    );
    expect(failed.checkedAt).toBe(before.checkedAt);
    // A failed scan of the same inputs is not retried on every load.
    await insights();
    expect((await store.get(scope, (await stored())!.jobId))?.data.status).toBe(
      "failed",
    );
    executor.fail = undefined;
    const retry = (await request("POST", "/home/insights/scan", {})).json();
    expect(retry.scan.state).toBe("queued");
    expect(await runQueued()).toBeUndefined();
    const after = await insights();
    expect(after.scan.state).toBe("idle");
    expect(after.stale).toBe(false);
    expect(after.checkedAt! > before.checkedAt!).toBe(true);
  });

  it("checks again when a new publication changes the accepted ontology", async () => {
    await publish(commerce());
    const before = await scan();
    const ws = (await store.get(scope, "workspace"))!;
    const current = (await store.list(scope, "proposal")).find(
      (p) => p.data.publishedGeneration === ws.data.activeGeneration,
    )!;
    const draft = await saveProposal(
      store,
      scope,
      BundleSchema.parse(current.data.bundle),
    );
    await publishProposal(store, scope, draft.id, draft.data.hash, actor);
    const queued = await insights();
    expect(queued).toMatchObject({ stale: true, scan: { state: "queued" } });
    expect(queued.findings).toHaveLength(before.findings.length);
    expect(await runQueued()).toBeUndefined();
    const after = await insights();
    expect(after.stale).toBe(false);
    expect(after.findings.map((f) => f.id).sort()).toEqual(
      before.findings.map((f) => f.id).sort(),
    );
  });

  it("lets an owner confirm the priorities, and only those are analysed", async () => {
    await publish(commerce());
    const r = await scan();
    expect(r.priorities.confirmed).toBe(false);
    const ids = r.priorities.candidates.map((c) => c.id);
    expect(
      (
        await request("PUT", "/home/insights/priorities", {
          drivers: ["revenue:nope"],
        })
      ).statusCode,
    ).toBe(400);
    const chosen = ids.filter((id) => !id.startsWith("revenue"));
    const confirmed = (
      await request("PUT", "/home/insights/priorities", { drivers: chosen })
    ).json();
    expect(confirmed.priorities.confirmed).toBe(true);
    expect(confirmed.scan.state).toBe("queued");
    executor.queries = [];
    await runQueued();
    const after = await insights();
    expect(after.priorities.selected.map((d) => d.id)).toEqual(chosen);
    expect(after.findings.every((f) => chosen.includes(f.driver.id))).toBe(
      true,
    );
    expect(
      executor.queries.filter((q) => /^WITH a AS/.test(q.sql)),
    ).toHaveLength(chosen.length);
    // None goes back to the automatic choice.
    const auto = (
      await request("PUT", "/home/insights/priorities", { drivers: [] })
    ).json();
    expect(auto.priorities.confirmed).toBe(false);
  });

  it("applies configurable thresholds", async () => {
    await publish(commerce());
    await scan();
    expect(
      (
        await request("PUT", "/home/insights/settings", {
          relativeChange: 0.001,
          unknown: 1,
        })
      ).statusCode,
    ).toBe(400);
    const r = (
      await request("PUT", "/home/insights/settings", { relativeChange: 0.4 })
    ).json();
    expect(r.scan.state).toBe("queued");
    await runQueued();
    const after = await insights();
    expect(finding(after, "cohort", /^Regular/)).toBeUndefined();
    expect(
      after.coverage
        .find((c) => c.label === "Customer engagement")!
        .checks.find((c) => c.check === "cohort")!.note,
    ).toMatch(/moved −30\.0%, within ±40\.0%/);
  });
});

describe("constant totals that hide offsetting changes", () => {
  it("finds fewer-but-larger orders and a conversion drop the mix hides", async () => {
    await publish(commerce({ hiddenShifts: true, sessionLinks: true }));
    const r = await scan();
    wellFormed(r);
    const unit = finding(r, "unit", /^Fewer orders were offset by larger/)!;
    expect(unit).toBeDefined();
    expect(unit.support).toEqual({ text: "Value per order", figure: "↑25.0%" });
    expect(unit.details.table.rows).toEqual([
      ["Orders", "800", "640", "−20.0%"],
      ["Value per order", "50", "62.50", "+25.0%"],
      ["Revenue", "40,000", "40,000", "0%"],
    ]);
    const conversion = finding(r, "conversion", /^Paid sessions convert less/)!;
    expect(conversion).toBeDefined();
    expect(conversion.severity).toBe("warning");
    expect(conversion.values.relative!).toBeLessThan(-0.2);
    expect(conversion.details.contribution).toMatch(
      /overall conversion stayed at 15%, so the overall rate hides it\.$/,
    );
    expect(conversion.details.definition).toMatch(
      /^A session counts as converted when an order links to it .*Only sessions from the first 6 days of each period count, so each had at least 1 day to convert\.$/,
    );
    // The funnel is checked once, under Traffic.
    expect(
      r.coverage
        .find((c) => c.label === "Revenue")!
        .checks.find((c) => c.check === "conversion")!.note,
    ).toBe("Checked under Traffic.");
  });
});

describe("a clinic, nothing like a shop", () => {
  it("adapts the checks, disables only the unsupported ones and shows fewer findings", async () => {
    await publish(clinic());
    const r = await scan();
    wellFormed(r);
    expect(r.priorities.selected.map((d) => d.label)).toEqual([
      "Acquisition",
      "Patient engagement",
    ]);
    expect(r.priorities.candidates.some((c) => c.kind === "revenue")).toBe(
      false,
    );
    for (const c of r.coverage) {
      expect(c.checks.find((x) => x.check === "unit")!.state).toBe("skipped");
      expect(c.checks.find((x) => x.check === "conversion")).toMatchObject({
        state: "skipped",
        note: "No recorded link between appointments and a later outcome.",
      });
    }
    expect(r.findings).toHaveLength(2);
    const cohort = finding(r, "cohort", /^Regular patients have fewer/)!;
    expect(cohort.details.table.rows[0]).toEqual([
      "Regular patients (80, fixed)",
      "240",
      "168",
      "−30.0%",
    ]);
    expect(finding(r, "mix", /^Online appointments grew 100%/)).toBeDefined();
  });
});

describe("a B2B software company (the sample data)", () => {
  it("maps its own ontology and explains what the data cannot support", async () => {
    await request("POST", "/demo", { advance: true });
    const s = (await request("POST", "/onboarding/ontology", {})).json();
    const c = (
      await request("POST", "/onboarding/ontology/confirm", {
        hash: s.ontology.hash,
      })
    ).json();
    await buildWorkspace(store, scope, c.buildRun.id);
    const r = await scan();
    wellFormed(r);
    expect(r.priorities.selected[0].label).toBe("User engagement");
    // Invoices are revenue, but too few each period to compare.
    expect(
      r.priorities.unavailable.find((u) => u.id === "revenue:type:Invoice"),
    ).toMatchObject({
      label: "Revenue (invoices)",
      reason: expect.stringMatching(/^Too few invoices/),
    });
    // Events naming their user are not a funnel.
    for (const cov of r.coverage)
      expect(cov.checks.find((x) => x.check === "conversion")!.state).toBe(
        "skipped",
      );
    for (const cov of r.coverage)
      for (const check of cov.checks) expect(check.note).not.toBe("");
  });
});

describe("data that cannot support a finding", () => {
  it("finds nothing in samples too small to compare", async () => {
    const small = commerce();
    await publish({
      customers: small.customers,
      sessions: small.sessions.filter((_, i) => i % 400 === 0),
      orders: small.orders.filter((_, i) => i % 100 === 0),
    });
    const r = await scan();
    expect(r.findings).toEqual([]);
    expect(r.priorities.selected).toEqual([]);
    expect(r.priorities.unavailable.length).toBeGreaterThan(0);
    expect(r.status).toBe("ready");
  });

  it("sets aside a week with a gap in the data, and a tracking change", async () => {
    const data = commerce({ trackingBreak: true });
    // Orders are missing for four days in the middle of the latest week; most
    // sessions there carry no customer.
    const day = (o: Record<string, unknown>) =>
      Math.floor((Date.parse(o.ordered_at as string) - weekStart(0)) / DAY);
    await publish({
      ...data,
      orders: data.orders.filter(
        (o) => day(o) < 0 || [0, 1, 6].includes(day(o)),
      ),
    });
    const r = await scan();
    const engagement = r.coverage.find(
      (c) => c.label === "Customer engagement",
    )!;
    expect(engagement.checks.find((c) => c.check === "cohort")).toMatchObject({
      state: "set_aside",
      note: expect.stringMatching(/looks like a tracking change/),
    });
    expect(
      finding(r, "cohort", /Regular customers have fewer sessions/),
    ).toBeUndefined();
    const revenue = r.coverage.find((c) => c.label === "Revenue")!;
    for (const check of revenue.checks.filter((c) => c.check !== "conversion"))
      expect(check).toMatchObject({
        state: "set_aside",
        note: "The latest period has orders on only 3 of 7 days, which looks like a gap in the data rather than a business change.",
      });
    expect(r.findings.every((f) => f.driver.label !== "Revenue")).toBe(true);
  });

  it("is unsupported before anything is published, and queues nothing", async () => {
    await ingestRows(store, scope, "orders", commerce().orders.slice(0, 50));
    const r = await insights();
    expect(r.status).toBe("unsupported");
    expect(await store.list(scope, "job")).toHaveLength(0);
    expect((await request("POST", "/home/insights/scan", {})).statusCode).toBe(
      409,
    );
  });
});

describe("complete periods", () => {
  const t = DEFAULT_THRESHOLDS;
  const today = Math.floor(Date.now() / DAY) * DAY;
  it("leaves out a day still in progress", () => {
    // Data through 09:00 today, synced at 10:00: today is incomplete.
    const w = chooseWindow(
      { lo: today - 60 * DAY, hi: today + 9 * 3_600_000, rows: 6000 },
      today + 10 * 3_600_000,
      t,
      "orders",
    );
    expect("reason" in w).toBe(false);
    if ("reason" in w) return;
    expect(w.end).toBe(today);
    expect(w.days).toBe(7);
    expect(w.current).toEqual([today - 7 * DAY, today]);
    expect(w.baseline).toEqual([today - 14 * DAY, today - 7 * DAY]);
    expect(w.cohort).toEqual([today - 28 * DAY, today - 14 * DAY]);
    expect(w.cohortReady).toBe(true);
    // Data through yesterday, synced today: yesterday is complete.
    const y = chooseWindow(
      { lo: today - 60 * DAY, hi: today - 3_600_000, rows: 6000 },
      today + 3_600_000,
      t,
      "orders",
    );
    expect("reason" in y || y.end).toBe(today);
  });
  it("uses four-week periods when weeks are too thin, and says why it cannot compare", () => {
    const four = chooseWindow(
      { lo: today - 200 * DAY, hi: today - DAY, rows: 300 },
      today,
      t,
      "invoices",
    );
    expect("reason" in four ? four.reason : four.days).toBe(28);
    expect(
      chooseWindow(
        { lo: today - 10 * DAY, hi: today - DAY, rows: 5000 },
        today,
        t,
        "orders",
      ),
    ).toEqual({
      reason:
        "Needs two complete weeks of orders; the data covers 10 complete days.",
    });
    expect(
      chooseWindow(
        { lo: today - 30 * DAY, hi: today - DAY, rows: 40 },
        today,
        t,
        "orders",
      ),
    ).toEqual({
      reason:
        "Too few orders each week (about 9; needs 30), and not enough history for four-week periods.",
    });
  });
});

describe("ranking", () => {
  const candidate = (subject: string, impact: number, extra = {}) =>
    ({
      check: "mix",
      subject,
      severity: "warning",
      impact,
      z: 5,
      hidden: false,
      ...extra,
    }) as any;
  const driver = (id: string) => ({ id }) as any;
  it("keeps one finding per shift, at most three, favouring other drivers for a second", () => {
    const ranked = rankCandidates(
      [
        {
          c: candidate("a", 0.3),
          d: driver("d1"),
          w: {} as any,
          sql: "",
          sources: [],
        },
        {
          c: candidate("a", 0.2),
          d: driver("d2"),
          w: {} as any,
          sql: "",
          sources: [],
        },
        {
          c: candidate("b", 0.2),
          d: driver("d1"),
          w: {} as any,
          sql: "",
          sources: [],
        },
        {
          c: candidate("c", 0.22),
          d: driver("d2"),
          w: {} as any,
          sql: "",
          sources: [],
        },
        {
          c: candidate("d", 0.21),
          d: driver("d3"),
          w: {} as any,
          sql: "",
          sources: [],
        },
      ],
      ["d1", "d2", "d3"],
    );
    expect(ranked.map((r) => r.c.subject)).toEqual(["a", "c", "d"]);
    expect(ranked[0].d.id).toBe("d1");
    // A second finding from one driver still wins when it is clearly stronger.
    expect(
      rankCandidates(
        [
          {
            c: candidate("a", 0.3),
            d: driver("d1"),
            w: {} as any,
            sql: "",
            sources: [],
          },
          {
            c: candidate("b", 0.29),
            d: driver("d1"),
            w: {} as any,
            sql: "",
            sources: [],
          },
          {
            c: candidate("c", 0.1),
            d: driver("d2"),
            w: {} as any,
            sql: "",
            sources: [],
          },
        ],
        ["d1", "d2"],
      ).map((r) => r.c.subject),
    ).toEqual(["a", "b", "c"]);
  });
});

describe("activity", () => {
  it("names and folds Home's checks in the activity feed", () => {
    const job = (status: string, at: string) => ({
      id: "j" + at,
      kind: "job",
      name: "Check for business shifts",
      updatedAt: at,
      data: { type: "home_insights", status },
    });
    const items = [
      describeActivity(job("running", "3")),
      describeActivity(job("succeeded", "2")),
      describeActivity(job("failed", "1")),
    ];
    expect(items.map((i) => [i.label, i.title])).toEqual([
      ["Home", "Looking for business shifts"],
      ["Home", "Checked for business shifts"],
      ["Home", "Couldn’t check for business shifts"],
    ]);
    const folded = collapseActivity(items);
    expect(folded).toHaveLength(1);
    expect(folded[0].repeats).toBe(2);
  });
});
