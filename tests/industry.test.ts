import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { rm } from "node:fs/promises";
import { resolve } from "node:path";
import { execute as localExecute } from "../services/executor/src/engine.js";
const executor = vi.hoisted(() => ({
  queries: [] as any[],
  fail: false,
  onQuery: undefined as undefined | (() => Promise<void>),
}));
vi.mock("../apps/api/src/executor.js", () => ({
  execute: async (job: any) => {
    if (job.mode === "query") {
      executor.queries.push(job);
      const next = executor.onQuery;
      executor.onQuery = undefined;
      await next?.();
      if (executor.fail) throw new Error("Executor unavailable");
    }
    return localExecute(job);
  },
}));
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore, transaction } from "../apps/api/src/store.js";
import { ingestRows } from "../apps/api/src/datasets.js";
import { buildWorkspace } from "../apps/api/src/assistant.js";
import {
  industryPlaybook,
  INDUSTRY_ID,
  INDUSTRY_JOB,
  INDUSTRY_INTERVAL,
  runIndustryChecks,
  queueIndustryChecks,
} from "../apps/api/src/industry-checks.js";
import {
  ecommercePlaybook,
  retailReading,
  retailMetrics,
} from "../apps/api/src/industry-playbook.js";
import {
  metricsDatasets,
  schemaSignature,
  validateSpecs,
  mergePicks,
  homeMetrics,
} from "../apps/api/src/metrics.js";
import { config } from "../apps/api/src/config.js";
import { digest } from "../apps/api/src/security.js";
import { startWorker } from "../apps/api/src/jobs.js";
import type { IndustryPlaybook } from "../packages/shared/src/index.js";
import { retailData } from "./fixtures/retail.js";

let store: MemoryStore,
  app: Awaited<ReturnType<typeof createApp>>,
  scope: string;
const request = (method: any, path: string, payload?: any) =>
  app.inject({ method, url: `/api/v1/workspaces/${scope}${path}`, payload });
beforeEach(async () => {
  executor.queries = [];
  executor.fail = false;
  executor.onQuery = undefined;
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
    await rm(resolve(config.dataDir, d, scope), {
      recursive: true,
      force: true,
    });
});
async function publish(data: Record<string, Record<string, unknown>[]>) {
  for (const [name, rows] of Object.entries(data))
    await ingestRows(store, scope, name, rows);
  const state = (await request("POST", "/onboarding/ontology", {})).json();
  const confirmed = (
    await request("POST", "/onboarding/ontology/confirm", {
      hash: state.ontology.hash,
    })
  ).json();
  await buildWorkspace(store, scope, confirmed.buildRun.id);
  expect((await request("GET", "/onboarding")).json().complete).toBe(true);
}
const current = () => industryPlaybook(store, scope);
async function runQueued() {
  await transaction(store, scope, async (tx) => {
    const j = (await tx.get(INDUSTRY_JOB))!;
    tx.update(j, { ...j.data, status: "running" });
  });
  try {
    await runIndustryChecks(store, scope);
    await transaction(store, scope, async (tx) => {
      const j = (await tx.get(INDUSTRY_JOB))!;
      tx.update(j, { ...j.data, status: "succeeded" });
    });
  } catch (e) {
    await transaction(store, scope, async (tx) => {
      const j = (await tx.get(INDUSTRY_JOB))!;
      tx.update(j, { ...j.data, status: "failed", error: String(e) });
    });
  }
}
async function scan() {
  await request("POST", "/industry/check", {});
  await runQueued();
  return current();
}
const check = (r: IndustryPlaybook, id: string) =>
  r.checks.find((c) => c.id === id)!;

describe("retail playbook and supported Home metrics", () => {
  it("loads the reviewed configuration and leaves weak matches generic", async () => {
    expect(ecommercePlaybook().checks).toHaveLength(6);
    await ingestRows(store, scope, "orders", retailData().orders);
    const ds = await metricsDatasets(store, scope);
    expect(retailReading(ds)).toBeUndefined();
    expect(retailMetrics(ds)).toEqual([]);
    expect(schemaSignature(ds)).toBe(
      digest(
        JSON.stringify(
          ds.map((d) => [
            d.id,
            d.data.profile.columns.map((c: any) => c.name + " " + c.type),
          ]),
        ),
      ),
    );
    executor.queries = [];
    expect(await current()).toMatchObject({ scan: "unsupported", checks: [] });
    expect(await store.get(scope, INDUSTRY_JOB)).toBeUndefined();
    expect(executor.queries).toHaveLength(0);
  });
  it("prioritizes four real calculations and cross-validates them against source rows", async () => {
    const data = retailData();
    await publish(data);
    const result = await homeMetrics(store, scope, "fixture");
    const metrics = result.metrics.filter((m) => m.kind === "kpi").slice(0, 4);
    expect(metrics.map((m) => m.title)).toEqual([
      "Recorded order value",
      "Orders",
      "Average order value",
      "Purchasing customers",
    ]);
    const end = Math.max(...data.orders.map((o) => Date.parse(o.ordered_at)));
    const rows = data.orders.filter(
      (o) => Date.parse(o.ordered_at) > end - 30 * 86400000,
    );
    const total = rows.reduce((sum, o) => sum + o.net_amount, 0);
    expect(metrics.map((m) => m.result.value)).toEqual([
      total,
      rows.length,
      total / rows.length,
      new Set(rows.map((o) => o.customer_id).filter(Boolean)).size,
    ]);
    expect(metrics.every((m) => m.format === "number")).toBe(true);
    expect(metrics[0].why).toContain("every recorded status");
    expect(
      result.metrics.some((m) =>
        /conversion|margin|repeat|roas|cac/i.test(m.title),
      ),
    ).toBe(false);
    expect(await store.list(scope, "run")).not.toContainEqual(
      expect.objectContaining({
        data: expect.objectContaining({ task: "industry" }),
      }),
    );
  });
  it("keeps retail calculations first after AI picks and deduplicates the remaining choices", async () => {
    await publish(retailData());
    const ds = await metricsDatasets(store, scope),
      specs = retailMetrics(ds);
    const fill = await validateSpecs(store, scope, specs, ds);
    const ai = await validateSpecs(
      store,
      scope,
      [...specs].reverse().map((s) => ({ ...s, title: "AI " + s.title })),
      ds,
    );
    const merged = mergePicks(ai, fill.kept, ds);
    expect(merged.kept.map((x) => x.metric.spec.title)).toEqual(
      specs.map((s) => s.title),
    );
    expect(new Set(merged.kept.map((x) => x.metric.sql)).size).toBe(
      merged.kept.length,
    );
  });
  it("invalidates the effective reading when table names change without a schema change", async () => {
    await publish(retailData());
    const ds = await metricsDatasets(store, scope);
    const before = schemaSignature(ds),
      order = ds.find((d) => d.name === "orders")!;
    const renamed = ds.map((d) =>
      d === order ? { ...d, name: "Support tickets" } : d,
    );
    expect(retailReading(renamed)).toBeUndefined();
    expect(schemaSignature(renamed)).not.toBe(before);
  });
  it("keeps monetary units explicit and respects additional protected columns", async () => {
    await publish(retailData());
    const ds = await metricsDatasets(store, scope);
    const protectedAmount = ds.map((d) => ({
      ...d,
      data: {
        ...d.data,
        profile: {
          ...d.data.profile,
          columns: d.data.profile.columns.map((c: any) => ({
            ...c,
            sensitive: c.sensitive || c.name === "net_amount",
          })),
        },
      },
    }));
    expect(retailReading(protectedAmount)).toBeUndefined();
    const cents = ds.map((d) => ({
      ...d,
      data: {
        ...d.data,
        profile: {
          ...d.data.profile,
          columns: d.data.profile.columns.map((c: any) =>
            c.name === "net_amount" ? { ...c, name: "net_amount_cents" } : c,
          ),
        },
      },
    }));
    expect(retailMetrics(cents)[0]).toMatchObject({
      format: "number",
      scale: "cents",
      column: "net_amount_cents",
    });
    expect(retailMetrics(ds)[0]).toMatchObject({
      format: "number",
      scale: "none",
    });
  });
});

describe("persisted background checks", () => {
  it("skips unmapped fields instead of claiming they pass", async () => {
    const data = retailData();
    await publish({
      ...data,
      orders: data.orders.map(
        ({ customer_id, session_id, status, ...row }) => row,
      ),
    });
    const result = await scan();
    for (const id of ["orders_customers", "orders_sessions", "order_statuses"])
      expect(check(result, id).state).toBe("skipped");
    expect(check(result, "lines_orders").total).toBe(30);
    expect(check(result, "lines_orders").matched).toBe(24);
  });
  it("never queries on GET, coalesces requests, and reuses persisted results after an API restart", async () => {
    await publish(retailData());
    executor.queries = [];
    const responses = await Promise.all([current(), current(), current()]);
    expect(responses.every((r) => r.scan === "queued")).toBe(true);
    expect(executor.queries).toHaveLength(0);
    expect(
      (await store.list(scope, "job")).filter(
        (j) => j.data.type === "industry_checks",
      ),
    ).toHaveLength(1);
    await runQueued();
    expect(executor.queries).toHaveLength(6);
    expect(executor.queries.every((q) => q.limit === 1)).toBe(true);
    const result = await current();
    executor.queries = [];
    await app.close();
    app = await createApp(store);
    await app.ready();
    expect((await request("GET", "/industry")).json()).toEqual(result);
    expect(executor.queries).toHaveLength(0);
  });
  it("cross-validates link and email counts, including null links and duplicate reference keys", async () => {
    const data = retailData();
    data.sessions.push({ ...data.sessions[0] });
    await publish(data);
    const r = await scan();
    const customers = new Set(data.customers.map((c) => c.customer_id));
    expect(check(r, "orders_customers")).toMatchObject({
      total: data.orders.length,
      matched: data.orders.filter(
        (o) => o.customer_id && customers.has(o.customer_id),
      ).length,
    });
    expect(check(r, "orders_sessions")).toMatchObject({
      total: 20,
      matched: 2,
      state: "attention",
    });
    expect(check(r, "orders_sessions").text).toContain("10.0%");
    expect(check(r, "customer_emails")).toMatchObject({
      total: 5,
      matched: 4,
      state: "attention",
    });
    expect(check(r, "order_statuses")).toMatchObject({
      total: 20,
      matched: 2,
      state: "attention",
    });
    expect(check(r, "order_statuses").text).toContain("include these rows");
    expect(JSON.stringify(r)).not.toContain("same@example.test");
  });
  it("keeps previous checks on failure and only retries the same failed inputs explicitly", async () => {
    await publish(retailData());
    const before = await scan();
    executor.fail = true;
    await request("POST", "/industry/check", {});
    await runQueued();
    const failed = await current();
    expect(failed).toMatchObject({
      scan: "failed",
      checks: before.checks,
      checkedAt: before.checkedAt,
    });
    executor.queries = [];
    await current();
    await queueIndustryChecks(store, scope);
    expect(executor.queries).toHaveLength(0);
    expect((await store.get(scope, INDUSTRY_JOB))!.data.status).toBe("failed");
    executor.fail = false;
    expect((await scan()).scan).toBe("idle");
  });
  it("schedules changed inputs after the persistent cooldown and lets an editor run them now", async () => {
    const data = retailData();
    await publish(data);
    await scan();
    const d = (await metricsDatasets(store, scope)).find(
      (d) => d.name === "orders",
    )!;
    await ingestRows(
      store,
      scope,
      d.name,
      data.orders.slice(0, -1),
      undefined,
      d.id,
    );
    executor.queries = [];
    expect(await current()).toMatchObject({ scan: "queued", stale: true });
    const job = (await store.get(scope, INDUSTRY_JOB))!;
    expect(job.data.retryAt).toBeGreaterThan(Date.now());
    const at = Date.parse((await store.get(scope, INDUSTRY_ID))!.data.queuedAt);
    expect(job.data.retryAt).toBe(at + INDUSTRY_INTERVAL);
    expect(executor.queries).toHaveLength(0);
    await request("POST", "/industry/check", {});
    expect((await store.get(scope, INDUSTRY_JOB))!.data.retryAt).toBeNull();
    await runQueued();
    expect((await current()).stale).toBe(false);
  });
  it("ignores unrelated dataset versions", async () => {
    await publish(retailData());
    const before = await scan();
    await ingestRows(store, scope, "inventory_counts", [{ stock: 3 }]);
    executor.queries = [];
    expect(await current()).toEqual(before);
    expect(executor.queries).toHaveLength(0);
  });
  it("runs a new publication immediately even when an earlier refresh is delayed", async () => {
    const data = retailData();
    await publish(data);
    await scan();
    const d = (await metricsDatasets(store, scope)).find(
      (d) => d.name === "orders",
    )!;
    await ingestRows(
      store,
      scope,
      d.name,
      data.orders.slice(0, -1),
      undefined,
      d.id,
    );
    await current();
    expect(
      (await store.get(scope, INDUSTRY_JOB))!.data.retryAt,
    ).toBeGreaterThan(Date.now());
    await transaction(store, scope, async (tx) => {
      const w = (await tx.get("workspace"))!;
      tx.update(w, { ...w.data, activeGeneration: "new-publication" });
    });
    await queueIndustryChecks(store, scope);
    expect((await store.get(scope, INDUSTRY_JOB))!.data.retryAt).toBeNull();
    expect((await store.get(scope, INDUSTRY_ID))!.data.generation).toBe(
      "new-publication",
    );
  });
  it("records the inputs actually scanned, without automatically repeating their failure", async () => {
    const data = retailData();
    await publish(data);
    await current();
    const d = (await metricsDatasets(store, scope)).find(
      (d) => d.name === "orders",
    )!;
    await ingestRows(
      store,
      scope,
      d.name,
      data.orders.slice(0, -1),
      undefined,
      d.id,
    );
    executor.fail = true;
    await runQueued();
    expect((await current()).scan).toBe("failed");
    expect((await store.get(scope, INDUSTRY_JOB))!.data.status).toBe("failed");
  });
  it("runs through the durable worker and keeps a failed check out of automatic AI repair", async () => {
    await publish(retailData());
    await current();
    executor.fail = true;
    await transaction(store, scope, async (tx) => {
      const job = (await tx.get(INDUSTRY_JOB))!;
      tx.update(job, { ...job.data, attempts: 2 });
    });
    const before = (await store.list(scope, "run")).length;
    let stop = startWorker(store);
    try {
      await vi.waitFor(
        async () => {
          expect((await store.get(scope, INDUSTRY_JOB))!.data.status).toBe(
            "failed",
          );
        },
        { timeout: 10000, interval: 100 },
      );
      expect(await store.get(scope, "failure_" + INDUSTRY_JOB)).toBeUndefined();
      expect(await store.list(scope, "run")).toHaveLength(before);
    } finally {
      await stop();
    }
    executor.fail = false;
    await request("POST", "/industry/check", {});
    stop = startWorker(store);
    try {
      await vi.waitFor(
        async () => {
          expect((await current()).scan).toBe("idle");
        },
        { timeout: 10000, interval: 100 },
      );
      expect((await current()).checks).toHaveLength(6);
    } finally {
      await stop();
    }
  });
  it("discards a result when its publication changes during a scan", async () => {
    await publish(retailData());
    const before = await scan();
    executor.onQuery = async () =>
      transaction(store, scope, async (tx) => {
        const w = (await tx.get("workspace"))!;
        tx.update(w, { ...w.data, activeGeneration: "replacement" });
      });
    await request("POST", "/industry/check", {});
    await runQueued();
    expect((await store.get(scope, INDUSTRY_ID))!.data.snapshot.checkedAt).toBe(
      before.checkedAt,
    );
    expect(await current()).toMatchObject({ stale: true, scan: "queued" });
  });
  it("allows reader access but protects manual refresh", async () => {
    await publish(retailData());
    await scan();
    const token = (
      await request("POST", "/tokens", { name: "Reader", readOnly: true })
    ).json().token;
    const headers = { authorization: `Bearer ${token}` };
    expect(
      (
        await app.inject({
          method: "GET",
          url: `/api/v1/workspaces/${scope}/industry`,
          headers,
        })
      ).statusCode,
    ).toBe(200);
    expect(
      (
        await app.inject({
          method: "POST",
          url: `/api/v1/workspaces/${scope}/industry/check`,
          headers,
          payload: {},
        })
      ).statusCode,
    ).toBe(403);
  });
  it("removes fulfilled suggestions after a successful refresh", async () => {
    await publish(retailData());
    const before = await scan();
    expect(before.suggestions.map((s) => s.id)).toContain("spend");
    await ingestRows(store, scope, "ad_spend", [
      { channel: "paid", spend: 20 },
    ]);
    const after = await scan();
    expect(after.suggestions.map((s) => s.id)).not.toContain("spend");
    expect(after.suggestions.length).toBeLessThanOrEqual(3);
  });
});
