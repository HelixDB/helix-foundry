// Retail checks run only in the durable worker. GET reads the last completed
// snapshot and may queue work, but never executes an aggregate or a model call.
import type {
  IndustryPlaybook,
  Resource,
} from "../../../packages/shared/src/index.js";
import { datasetTable } from "../../../packages/shared/src/index.js";
import { queryDatasets } from "./datasets.js";
import { q, lit } from "./discovery.js";
import { metricsDatasets } from "./metrics.js";
import { datasetPrivacyId } from "./dataset-privacy.js";
import { retailReading, type RetailReading } from "./industry-playbook.js";
import { assert, digest } from "./security.js";
import { now, resource, transaction, type Store } from "./store.js";

export const INDUSTRY_ID = "industry_playbook",
  INDUSTRY_JOB = "industry_check_job";
export const INDUSTRY_INTERVAL = 15 * 60_000;
type Snapshot = {
  key: string;
  generation: string;
  checkedAt: string;
  checks: IndustryPlaybook["checks"];
  suggestions: IndustryPlaybook["suggestions"];
};
type Stored = {
  snapshot?: Snapshot;
  requestedKey?: string;
  queuedAt?: string;
  generation?: string;
};
const busy = (job?: Resource) =>
  ["queued", "running"].includes(job?.data.status);
async function inputs(store: Store, scope: string) {
  const w = await store.get(scope, "workspace");
  if (!w?.data.activeGeneration) return;
  const r = retailReading(await metricsDatasets(store, scope));
  if (!r) return;
  const ids = new Set(Object.values(r.mapping).map((m) => m.datasetId));
  const datasets = r.datasets.filter((d) => ids.has(d.id));
  const generation = w.data.activeGeneration as string;
  const key = digest(
    JSON.stringify([
      r.identity,
      generation,
      w.data.onboarding?.ontology?.hash,
      datasets.map((d) => [d.id, d.data.activeVersion, d.data.privacy || null]),
    ]),
  );
  return { r, key, generation, datasets };
}

export async function queueIndustryChecks(
  store: Store,
  scope: string,
  force = false,
) {
  const ctx = await inputs(store, scope);
  if (!ctx) return;
  await transaction(store, scope, async (tx) => {
    const state = await tx.get(INDUSTRY_ID),
      s = state?.data as Stored | undefined;
    const job = await tx.get(INDUSTRY_JOB);
    if (busy(job)) {
      if (job?.data.status === "queued") {
        if (force || s?.generation !== ctx.generation)
          tx.update(job, { ...job.data, retryAt: null });
        if (state && s?.requestedKey !== ctx.key)
          tx.update(state, {
            ...s,
            requestedKey: ctx.key,
            generation: ctx.generation,
          });
      }
      return;
    }
    if (
      !force &&
      (s?.snapshot?.key === ctx.key ||
        (job?.data.status === "failed" && s?.requestedKey === ctx.key))
    )
      return;
    const due =
      !force && s?.generation === ctx.generation
        ? Math.max(Date.now(), Date.parse(s.queuedAt || "") + INDUSTRY_INTERVAL)
        : Date.now();
    const data = {
      type: "industry_checks",
      background: true,
      status: "queued",
      attempts: 0,
      error: null,
      retryAt: Number.isFinite(due) ? due : null,
    };
    job
      ? tx.update(job, data)
      : tx.put(resource(scope, "job", "Check retail data", data, INDUSTRY_JOB));
    const stored = {
      ...s,
      requestedKey: ctx.key,
      queuedAt: s?.queuedAt,
      generation: ctx.generation,
    };
    state
      ? tx.update(state, stored)
      : tx.put(
          resource(
            scope,
            "industryPlaybook",
            "Online retail",
            stored,
            INDUSTRY_ID,
          ),
        );
  });
  return ctx;
}

export async function industryPlaybook(
  store: Store,
  scope: string,
): Promise<IndustryPlaybook> {
  const ctx = await queueIndustryChecks(store, scope);
  if (!ctx)
    return { scan: "unsupported", stale: false, checks: [], suggestions: [] };
  const s = (await store.get(scope, INDUSTRY_ID))?.data as Stored | undefined;
  const job = await store.get(scope, INDUSTRY_JOB);
  const scan =
    job?.data.status === "failed"
      ? "failed"
      : busy(job)
        ? job!.data.status
        : "idle";
  return {
    playbook: { id: ctx.r.book.id, title: ctx.r.book.title },
    scan,
    stale: !!s?.snapshot && s.snapshot.key !== ctx.key,
    checkedAt: s?.snapshot?.checkedAt,
    checks: s?.snapshot?.checks || [],
    suggestions: s?.snapshot?.suggestions || [],
  };
}

export async function rescanIndustry(store: Store, scope: string) {
  assert(
    await inputs(store, scope),
    "Publish a supported retail workspace first",
    409,
  );
  await queueIndustryChecks(store, scope, true);
  return industryPlaybook(store, scope);
}
const pct = (rate: number) => (rate * 100).toFixed(1) + "%";
async function checkRetailData(store: Store, scope: string, r: RetailReading) {
  const checks: IndustryPlaybook["checks"] = [];
  for (const c of r.book.checks) {
    const needed =
      c.kind === "link" ? [c.from, c.column, c.to, c.key] : [c.column];
    if (needed.some((id) => !r.mapping[id])) {
      checks.push({
        id: c.id,
        state: "skipped",
        text: `${c.title}: the required fields are not mapped yet.`,
      });
      continue;
    }
    const m = r.mapping[c.column],
      col = q(m.column!),
      table = q(datasetTable(m.datasetId));
    let sql: string,
      ids = [m.datasetId];
    if (c.kind === "link") {
      const target = r.mapping[c.key];
      ids.push(target.datasetId);
      sql = `SELECT count(*) AS total, count(*) FILTER (WHERE EXISTS (SELECT 1 FROM ${q(datasetTable(target.datasetId))} b WHERE CAST(b.${q(target.column!)} AS VARCHAR) = CAST(a.${col} AS VARCHAR))) AS matched FROM ${table} a`;
    } else if (c.kind === "unique")
      sql = `SELECT count(*) AS total, count(DISTINCT lower(trim(CAST(${col} AS VARCHAR)))) AS matched FROM ${table} WHERE ${col} IS NOT NULL AND trim(CAST(${col} AS VARCHAR)) <> ''`;
    else
      sql = `SELECT count(*) AS total, count(*) FILTER (WHERE lower(trim(CAST(${col} AS VARCHAR))) IN (${c.values.map(lit).join(", ")})) AS matched FROM ${table}`;
    const result = await queryDatasets(store, scope, [...new Set(ids)], sql, {
      limit: 1,
    });
    const row = result.rows[0],
      total = Number(row?.total || 0),
      matched = Number(row?.matched || 0);
    if (!total) {
      checks.push({
        id: c.id,
        state: "skipped",
        text: `${c.title}: no rows to check.`,
      });
      continue;
    }
    const rate = matched / total;
    checks.push({
      id: c.id,
      total,
      matched,
      state:
        c.kind === "status"
          ? matched
            ? "attention"
            : "passed"
          : rate >= c.good
            ? "passed"
            : "attention",
      text:
        c.kind === "status"
          ? `${matched} of ${total} orders have a cancelled, returned, refunded, voided or failed status. Home's recorded totals include these rows; review statuses before treating the totals as sales.`
          : `${c.title}: ${pct(rate)} (${matched} of ${total}${c.kind === "unique" ? " nonblank email rows" : " rows, including missing links"}).${rate < c.good ? " " + c.fix : ""}`,
    });
  }
  return checks;
}

export async function runIndustryChecks(store: Store, scope: string) {
  const ctx = await inputs(store, scope);
  if (!ctx) return;
  await transaction(store, scope, async (tx) => {
    const state = await tx.get(INDUSTRY_ID);
    if (state)
      tx.update(state, {
        ...state.data,
        requestedKey: ctx.key,
        generation: ctx.generation,
        queuedAt: now(),
      });
  });
  const checks = await checkRetailData(store, scope, ctx.r);
  const suggestions = ctx.r.book.suggestions
    .filter((s) => !ctx.r.mapping[s.missing])
    .slice(0, 3)
    .map(({ id, kind, text }) => ({ id, kind, text }));
  const fresh = await inputs(store, scope);
  if (fresh?.key !== ctx.key) return;
  await transaction(store, scope, async (tx) => {
    if ((await tx.get("workspace"))?.data.activeGeneration !== ctx.generation)
      return;
    for (const d of ctx.datasets) {
      if ((await tx.get(d.id))?.revision !== d.revision) return;
      const policy = await tx.get(datasetPrivacyId(d.id));
      if (
        JSON.stringify(policy?.data.additionalColumns || []) !==
        JSON.stringify(d.data.privacy?.additionalColumns || [])
      )
        return;
    }
    const state = await tx.get(INDUSTRY_ID);
    const snapshot: Snapshot = {
      key: ctx.key,
      generation: ctx.generation,
      checkedAt: now(),
      checks,
      suggestions,
    };
    const data = { ...state?.data, snapshot };
    state
      ? tx.update(state, data)
      : tx.put(
          resource(
            scope,
            "industryPlaybook",
            "Online retail",
            data,
            INDUSTRY_ID,
          ),
        );
  });
}
