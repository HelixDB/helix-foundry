// Home's "Some things we noticed". GET returns the stored findings at once and,
// when the data, the accepted ontology or the settings changed since the last
// scan, queues one background scan (a "home_insights" job), so loading Home
// never runs the analysis. A scan selects at most three supported business
// drivers, runs their checks with one bounded query each, and keeps at most
// three findings across all of them. A failed scan keeps the earlier findings.
// Dismissals are per person and finding version.
import {
  insightStages,
  type HomeInsight,
  type HomeInsights,
  type InsightCoverage,
  type InsightDriver,
  type InsightPeriod,
  type InsightSeverity,
  type InsightStage,
  type InsightThresholds,
  type Resource,
} from "../../../packages/shared/src/index.js";
import { datasetTable } from "../../../packages/shared/src/index.js";
import { queryDatasets } from "./datasets.js";
import { q } from "./discovery.js";
import {
  chooseWindow,
  DAY,
  DEFAULT_THRESHOLDS,
  driverQuery,
  evaluate,
  period,
  periodLabel,
  type Candidate,
  type Coverage,
  type Note,
  type Window,
} from "./insights-checks.js";
import {
  acceptedOntology,
  businessContext,
  checkLabel,
  driverSummary,
  findActivities,
  proposeDrivers,
  publishedBundle,
  shareChecks,
  type Activity,
  type Driver,
} from "./insights-model.js";
import { metricsDatasets } from "./metrics.js";
import { assert, digest } from "./security.js";
import { now, resource, transaction, type Store } from "./store.js";

export const INSIGHTS_ID = "home_insights";
// Bumped when the checks change, so every workspace scans again.
export const INSIGHTS_VERSION = 3;
export const AUTO_SCAN_INTERVAL = 15 * 60_000;
const MAX_DRIVERS = 3,
  MAX_FINDINGS = 3,
  // Candidate activities whose coverage is probed before choosing drivers.
  MAX_PROBES = 6;

type StoredFinding = Omit<HomeInsight, "returned"> & {
  score: number;
  magnitude: number;
};
type Stored = {
  version?: number;
  // The inputs the shown findings were computed from.
  key?: string;
  // The inputs of the latest queued scan, and its job.
  requestedKey?: string;
  jobId?: string;
  stage?: InsightStage;
  stageDetail?: Partial<Record<InsightStage, string>>;
  checkedAt?: string;
  // Persisted request time bounds automatic scans across worker restarts.
  lastQueuedAt?: string;
  pendingManual?: boolean;
  pendingManualKey?: string;
  // Coverage of activities, by dataset version and time column.
  probes?: Record<string, Coverage | null>;
  candidates?: InsightDriver[];
  unavailable?: { id: string; label: string; reason: string }[];
  selected?: string[];
  ambiguous?: boolean;
  coverage?: InsightCoverage[];
  findings?: StoredFinding[];
  period?: { current: InsightPeriod; baseline: InsightPeriod };
  // False when the last scan found no activity with dates to check.
  supported?: boolean;
  settings?: Partial<InsightThresholds>;
  priorities?: { drivers: string[]; actor: string; at: string };
};

const stageLabels: Record<InsightStage, string> = {
  understand: "Understand the business",
  fundamentals: "Establish the fundamentals",
  shifts: "Look for meaningful shifts",
};
const stageDetails: Record<InsightStage, string> = {
  understand:
    "Connect activity, participants and outcomes in your ontology, and choose up to three priorities.",
  fundamentals: "Compare complete periods shared by the relevant sources.",
  shifts: "Check mix, value per activity and conversion beneath the totals.",
};

export const thresholdsOf = (s?: Stored): InsightThresholds => ({
  ...DEFAULT_THRESHOLDS,
  ...s?.settings,
});
const syncedAt = (d: Resource) => Date.parse(d.data.lastSync || d.updatedAt);
// What a scan depends on: the publication, the confirmed ontology, each
// dataset's version and the day of its last sync (a day only completes once a
// later snapshot exists), the settings and the confirmed priorities.
function inputsKey(
  workspace: Resource,
  datasets: Resource[],
  stored: Stored | undefined,
) {
  return digest(
    JSON.stringify([
      INSIGHTS_VERSION,
      workspace.data.activeGeneration,
      workspace.data.onboarding?.ontology?.hash || "",
      [...datasets]
        .sort((a, b) => a.id.localeCompare(b.id))
        .map((d) => [
          d.id,
          d.data.activeVersion,
          new Date(syncedAt(d)).toISOString().slice(0, 10),
        ]),
      stored?.settings || {},
      stored?.priorities?.drivers || [],
    ]),
  );
}

// Only the chosen drivers' actual dependencies invalidate cached findings.
// Before the first scan, the metadata-ranked shortlist stands in for the
// selection. Publication/ontology/settings changes remain part of the key.
function watchedDatasets(
  drivers: Driver[],
  datasets: Resource[],
  stored: Stored | undefined,
  activities: Activity[],
) {
  const desired = new Set(
    stored?.priorities?.drivers || stored?.selected || [],
  );
  let picked = drivers.filter((d) => desired.has(d.id));
  if (!picked.length)
    picked = [...drivers]
      .sort((a, b) => b.prior - a.prior || a.id.localeCompare(b.id))
      .slice(0, MAX_DRIVERS);
  const ids = new Set<string>();
  for (const d of picked) {
    ids.add(d.activity.datasetId);
    for (const p of [d.actor, d.contributor]) {
      if (p?.via) ids.add(p.via.datasetId);
      if (p) ids.add(p.datasetId);
    }
    if (d.funnel && d.funnel.link.side !== "flag")
      for (const id of [d.funnel.upstream, d.funnel.downstream]) {
        const a = activities.find((x) => x.id === id);
        if (a) ids.add(a.datasetId);
      }
  }
  return datasets.filter((d) => ids.has(d.id));
}
const probeKey = (a: Activity, d: Resource | undefined) =>
  `${a.datasetId}@${d?.data.activeVersion || ""}:${a.time.column}`;
const busy = (job?: Resource) =>
  !!job && ["queued", "running"].includes(job.data.status);

async function saveStored(
  store: Store,
  scope: string,
  change: (s: Stored) => Stored,
) {
  return transaction(store, scope, async (tx) => {
    const r = await tx.get(INSIGHTS_ID);
    const data = change({ ...(r?.data as Stored | undefined) });
    return (
      r
        ? tx.update(r, data)
        : tx.put(
            resource(scope, "homeInsights", "Home insights", data, INSIGHTS_ID),
          )
    ).data as Stored;
  });
}

// Queues a scan of the current inputs, reusing its job; `force` also restarts
// a failed one.
async function queueScan(
  store: Store,
  scope: string,
  key: string,
  force = false,
) {
  const id = "insights_" + digest(scope + ":" + key).slice(0, 24);
  return transaction(store, scope, async (tx) => {
    const previous = (await tx.get(INSIGHTS_ID))?.data as Stored | undefined;
    const active = previous?.jobId ? await tx.get(previous.jobId) : undefined;
    // Coalesce input changes during a run. Automatic scans use the latest
    // inputs on the next eligible request, never a second concurrent job.
    if (busy(active)) {
      if (force && previous?.requestedKey !== key) {
        const r = (await tx.get(INSIGHTS_ID))!;
        const updated = tx.update(r, {
          ...previous,
          pendingManual: true,
          pendingManualKey: key,
        });
        return { stored: updated.data as Stored, job: active! };
      }
      return { stored: previous!, job: active! };
    }
    const queuedAt = Date.parse(
      previous?.lastQueuedAt || active?.createdAt || previous?.checkedAt || "",
    );
    if (
      !force &&
      !previous?.pendingManual &&
      Number.isFinite(queuedAt) &&
      Date.now() - queuedAt < AUTO_SCAN_INTERVAL
    )
      return { stored: previous!, job: active };
    let job = await tx.get(id);
    if (!job)
      job = tx.put(
        resource(
          scope,
          "job",
          "Check for business shifts",
          {
            type: "home_insights",
            status: "queued",
            background: true,
            key,
          },
          id,
        ),
      );
    else if (!busy(job))
      job = tx.update(job, {
        ...job.data,
        status: "queued",
        attempts: 0,
        error: null,
        retryAt: null,
      });
    const r = await tx.get(INSIGHTS_ID),
      data: Stored = {
        ...(r?.data as Stored | undefined),
        requestedKey: key,
        jobId: id,
        lastQueuedAt: now(),
        pendingManual: false,
        pendingManualKey: undefined,
      };
    const stored = (
      r
        ? tx.update(r, data)
        : tx.put(
            resource(scope, "homeInsights", "Home insights", data, INSIGHTS_ID),
          )
    ).data as Stored;
    return { stored, job };
  });
}

type Context = {
  workspace: Resource;
  datasets: Resource[];
  stored?: Stored;
  key: string;
  activities: Activity[];
  drivers: Driver[];
};
async function context(store: Store, scope: string): Promise<Context | null> {
  const workspace = await store.get(scope, "workspace");
  if (!workspace?.data.activeGeneration) return null;
  const datasets = await metricsDatasets(store, scope);
  if (!datasets.length) return null;
  const stored = (await store.get(scope, INSIGHTS_ID))?.data as
    | Stored
    | undefined;
  const bundle = await publishedBundle(
    (kind) => store.list(scope, kind),
    workspace.data.activeGeneration,
  );
  const activities = findActivities(
    acceptedOntology(workspace, bundle, datasets),
  );
  const drivers = proposeDrivers(
    activities,
    datasets,
    businessContext(workspace),
  );
  return {
    workspace,
    datasets,
    stored,
    key: inputsKey(
      workspace,
      watchedDatasets(drivers, datasets, stored, activities),
      stored,
    ),
    activities,
    drivers,
  };
}

const severityRank: Record<InsightSeverity, number> = {
  positive: 0,
  warning: 1,
  issue: 2,
};
const severityName: Record<InsightSeverity, string> = {
  positive: "a positive change",
  warning: "a warning",
  issue: "an issue",
};
const magnitudeText = (m: number, unit: HomeInsight["values"]["unit"]) =>
  `${m > 0 ? "+" : m < 0 ? "−" : ""}${(Math.abs(m) * 100).toFixed(1)}${unit === "share" ? " pts" : "%"}`;
// Why a finding this person dismissed is back, or undefined to keep it hidden:
// it became more severe, grew by half again, or reversed.
function returned(f: StoredFinding, dismissal: Resource) {
  const d = dismissal.data;
  if (severityRank[f.severity] > severityRank[d.severity as InsightSeverity])
    return `Back because it is now ${severityName[f.severity]}; it was ${severityName[d.severity as InsightSeverity]} when you dismissed it.`;
  if (Math.sign(f.magnitude) !== Math.sign(d.magnitude) && d.magnitude !== 0)
    return `Back because it reversed: ${magnitudeText(d.magnitude, f.values.unit)} when you dismissed it, ${magnitudeText(f.magnitude, f.values.unit)} now.`;
  if (
    Math.abs(f.magnitude) >= Math.abs(d.magnitude) * 1.5 &&
    Math.abs(f.magnitude - d.magnitude) >= 0.05
  )
    return `Back because it grew: ${magnitudeText(d.magnitude, f.values.unit)} when you dismissed it, ${magnitudeText(f.magnitude, f.values.unit)} now.`;
  return undefined;
}
const dismissalId = (userId: string, findingId: string) =>
  "insight_dismissal_" + digest(userId + ":" + findingId).slice(0, 32);

function view(
  stored: Stored | undefined,
  job: Resource | undefined,
  key: string | null,
  dismissals: Resource[],
): HomeInsights {
  const state: HomeInsights["scan"]["state"] =
    job?.data.status === "queued"
      ? "queued"
      : job?.data.status === "running"
        ? "running"
        : job?.data.status === "failed"
          ? "failed"
          : "idle";
  const at = stored?.stage ? insightStages.indexOf(stored.stage) : -1;
  const steps = insightStages.map((id, i) => ({
    id,
    label: stageLabels[id],
    detail: stored?.stageDetail?.[id] || stageDetails[id],
    state:
      state === "running"
        ? i < at
          ? ("done" as const)
          : i === at
            ? ("active" as const)
            : ("waiting" as const)
        : state === "queued" || (!stored?.checkedAt && state !== "failed")
          ? ("waiting" as const)
          : ("done" as const),
  }));
  const byFinding = new Map(dismissals.map((d) => [d.data.findingId, d]));
  const findings: HomeInsight[] = [];
  let dismissed = 0;
  for (const f of stored?.findings || []) {
    const d = byFinding.get(f.id);
    const back = d && returned(f, d);
    if (d && !back) {
      dismissed++;
      continue;
    }
    const { score: _s, magnitude: _m, ...shown } = f;
    findings.push(back ? { ...shown, returned: back } : shown);
  }
  const candidates = stored?.candidates || [],
    selected = new Set(stored?.selected || []);
  return {
    status:
      key === null ||
      (stored?.supported === false && stored.key === key && !busy(job))
        ? "unsupported"
        : "ready",
    scan: {
      state,
      stage: state === "running" ? stored?.stage : undefined,
      steps,
      error:
        state === "failed"
          ? String(job?.data.error || "The check stopped.").slice(0, 300)
          : undefined,
    },
    checkedAt: stored?.checkedAt,
    stale: !!stored?.checkedAt && key !== null && stored.key !== key,
    period: stored?.period,
    priorities: {
      selected: candidates.filter((c) => selected.has(c.id)),
      candidates,
      ambiguous: !!stored?.ambiguous && !stored.priorities,
      confirmed: !!stored?.priorities,
      unavailable: stored?.unavailable || [],
    },
    coverage: stored?.coverage || [],
    findings,
    dismissed,
  };
}

async function dismissalsOf(store: Store, scope: string, userId: string) {
  return (await store.list(scope, "insightDismissal")).filter(
    (d) => d.data.userId === userId,
  );
}

// What Home shows. Never runs a query: it reads the stored findings and, when
// their inputs changed, queues a scan.
export async function homeInsights(
  store: Store,
  scope: string,
  userId: string,
): Promise<HomeInsights> {
  const ctx = await context(store, scope);
  if (!ctx) {
    const stored = (await store.get(scope, INSIGHTS_ID))?.data as
      | Stored
      | undefined;
    return view(stored, undefined, null, []);
  }
  let { stored } = ctx;
  let job = stored?.jobId ? await store.get(scope, stored.jobId) : undefined;
  // A failed scan of these same inputs waits for Retry instead of looping.
  const settled =
    stored?.key === ctx.key ||
    (stored?.requestedKey === ctx.key &&
      (busy(job) || job?.data.status === "failed"));
  if (!settled) ({ stored, job } = await queueScan(store, scope, ctx.key));
  return view(stored, job, ctx.key, await dismissalsOf(store, scope, userId));
}

// Retry, or check again now.
export async function rescanInsights(
  store: Store,
  scope: string,
  userId: string,
) {
  const ctx = await context(store, scope);
  assert(ctx, "Publish a workspace first", 409);
  await queueScan(store, scope, ctx.key, true);
  return homeInsights(store, scope, userId);
}

export async function dismissInsight(
  store: Store,
  scope: string,
  userId: string,
  findingId: string,
  version: string,
) {
  await transaction(store, scope, async (tx) => {
    const f = (
      (await tx.get(INSIGHTS_ID))?.data as Stored | undefined
    )?.findings?.find((x) => x.id === findingId);
    assert(f, "That finding is no longer shown", 404);
    assert(
      f.version === version,
      "That finding changed. Review it before dismissing it.",
      409,
    );
    tx.put(
      resource(
        scope,
        "insightDismissal",
        f.headline,
        {
          userId,
          findingId,
          version,
          severity: f.severity,
          magnitude: f.magnitude,
          at: now(),
        },
        dismissalId(userId, findingId),
      ),
    );
  });
  return homeInsights(store, scope, userId);
}
export async function restoreInsight(
  store: Store,
  scope: string,
  userId: string,
  findingId: string,
) {
  await transaction(store, scope, async (tx) => {
    if (await tx.get(dismissalId(userId, findingId)))
      tx.delete(dismissalId(userId, findingId));
  });
  return homeInsights(store, scope, userId);
}

// An owner's choice of up to three priorities from the candidates; none goes
// back to the automatic choice.
export async function confirmPriorities(
  store: Store,
  scope: string,
  userId: string,
  drivers: string[],
) {
  const unique = [...new Set(drivers)];
  await saveStored(store, scope, (s) => {
    const known = new Set((s.candidates || []).map((c) => c.id));
    assert(
      unique.every((id) => known.has(id)),
      "Choose priorities from the candidates",
    );
    return {
      ...s,
      priorities: unique.length
        ? { drivers: unique, actor: userId, at: now() }
        : undefined,
    };
  });
  return rescanInsights(store, scope, userId);
}
export async function updateInsightSettings(
  store: Store,
  scope: string,
  userId: string,
  settings: Partial<InsightThresholds>,
) {
  await saveStored(store, scope, (s) => ({ ...s, settings }));
  return rescanInsights(store, scope, userId);
}

// Coverage of an activity: its earliest and latest event and row count, from
// one aggregate over its time column. Cached per dataset version.
async function probe(
  store: Store,
  scope: string,
  a: Activity,
): Promise<Coverage | null> {
  const time = a.time.zoned
    ? `CAST(timezone('UTC', a.${q(a.time.column)}) AS TIMESTAMP)`
    : `CAST(a.${q(a.time.column)} AS TIMESTAMP)`;
  const r = await queryDatasets(
    store,
    scope,
    [a.datasetId],
    `SELECT 'probe' AS part, CAST(min(${time}) AS VARCHAR) AS lo, CAST(max(${time}) AS VARCHAR) AS hi, CAST(count(*) AS DOUBLE) AS n FROM ${q(datasetTable(a.datasetId))} a WHERE a.${q(a.time.column)} IS NOT NULL`,
    { limit: 1 },
  );
  const row = r.rows[0] as Record<string, unknown> | undefined;
  const parse = (v: unknown) =>
    typeof v === "string" ? Date.parse(v.replace(" ", "T") + "Z") : NaN;
  const lo = parse(row?.lo),
    hi = parse(row?.hi),
    rows = Number(row?.n || 0);
  return Number.isFinite(lo) && Number.isFinite(hi) && rows > 0
    ? { lo, hi, rows }
    : null;
}

type Selection = {
  drivers: Driver[];
  windows: Map<string, Window>;
  candidates: InsightDriver[];
  unavailable: { id: string; label: string; reason: string }[];
  ambiguous: boolean;
  ownerConfirmed: boolean;
  probes: Record<string, Coverage | null>;
};
// Drivers with enough history and activity, then the owner's choice or the
// best three. Only the candidates' activities are probed; no check runs here.
async function selectDrivers(
  store: Store,
  scope: string,
  drivers: Driver[],
  datasets: Resource[],
  stored: Stored | undefined,
  t: InsightThresholds,
  activities: Activity[],
): Promise<Selection> {
  const byId = new Map(datasets.map((d) => [d.id, d]));
  const chosen = new Set(stored?.priorities?.drivers || []);
  const eligible = chosen.size
    ? drivers.filter((d) => chosen.has(d.id))
    : drivers;
  const byActivity = new Map(activities.map((a) => [a.id, a]));
  const targets: Activity[] = [];
  for (const d of eligible) {
    const needed = [d.activity];
    if (d.funnel && d.checks.conversion === null)
      for (const id of [d.funnel.upstream, d.funnel.downstream]) {
        const a = byActivity.get(id);
        if (a && !needed.some((n) => n.id === a.id)) needed.push(a);
      }
    const missing = needed.filter((a) => !targets.some((x) => x.id === a.id));
    if (targets.length + missing.length <= MAX_PROBES) targets.push(...missing);
  }
  const probes: Record<string, Coverage | null> = {};
  for (const a of targets) {
    const k = probeKey(a, byId.get(a.datasetId));
    probes[k] =
      stored?.probes && k in stored.probes
        ? stored.probes[k]
        : await probe(store, scope, a);
  }
  const windows = new Map<string, Window>(),
    scored: { d: Driver; score: number }[] = [],
    unavailable: Selection["unavailable"] = [];
  for (const d of eligible) {
    const k = probeKey(d.activity, byId.get(d.activity.datasetId));
    if (!(k in probes)) continue;
    const cov = probes[k];
    let coverage = cov,
      sync = syncedAt(byId.get(d.activity.datasetId)!);
    if (cov && d.funnel && d.checks.conversion === null) {
      const endpoints = [d.funnel.upstream, d.funnel.downstream].map((id) =>
        byActivity.get(id),
      );
      const linked = endpoints.map(
        (a) => a && probes[probeKey(a, byId.get(a.datasetId))],
      );
      if (linked.every((c) => !!c)) {
        coverage = {
          lo: Math.max(cov.lo, ...linked.map((c) => c!.lo)),
          hi: Math.min(cov.hi, ...linked.map((c) => c!.hi)),
          rows: cov.rows,
        };
        sync = Math.min(
          sync,
          ...endpoints.map((a) => syncedAt(byId.get(a!.datasetId)!)),
        );
      } else
        d.checks.conversion =
          "Both linked activities need dated coverage before conversion can be compared.";
    }
    const w = coverage
      ? chooseWindow(coverage, sync, t, d.activity.many)
      : { reason: `No ${d.activity.many} have a date.` };
    if ("reason" in w) {
      unavailable.push({
        id: d.id,
        label:
          d.kind === "activity" ? d.label : `${d.label} (${d.activity.many})`,
        reason: w.reason,
      });
      continue;
    }
    windows.set(d.id, w);
    scored.push({
      d,
      score: d.prior * (w.days === 7 ? 1 : 0.85),
    });
  }
  scored.sort((a, b) => b.score - a.score || a.d.id.localeCompare(b.d.id));
  const confirmed = scored.filter((s) => chosen.has(s.d.id));
  const picked = (confirmed.length ? confirmed : scored).slice(0, MAX_DRIVERS);
  const ambiguous =
    !confirmed.length &&
    scored.length > MAX_DRIVERS &&
    (scored[MAX_DRIVERS - 1].score - scored[MAX_DRIVERS].score) /
      scored[MAX_DRIVERS - 1].score <
      0.1;
  return {
    drivers: picked.map((s) => s.d),
    windows,
    candidates: chosen.size
      ? drivers.map(driverSummary)
      : scored.map((s) => driverSummary(s.d)),
    unavailable: unavailable.slice(0, 6),
    ambiguous,
    ownerConfirmed: confirmed.length > 0,
    probes,
  };
}

type Ranked = {
  c: Candidate;
  d: Driver;
  w: Window;
  sql: string;
  sources: { datasetId: string; name: string }[];
  score: number;
};
const severityWeight: Record<InsightSeverity, number> = {
  issue: 1.15,
  warning: 1,
  positive: 0.9,
};
// The strongest findings across drivers: measured impact times evidence
// strength, favouring shifts the totals hide; one per shift, and a second
// from the same driver only when it clearly beats the rest.
export function rankCandidates(all: Omit<Ranked, "score">[], order: string[]) {
  const best = new Map<string, Ranked>();
  for (const x of all) {
    const score =
      x.c.impact *
      (0.5 + 0.5 * Math.min(1, Math.abs(x.c.z) / 4)) *
      ([1, 0.92, 0.85][order.indexOf(x.d.id)] ?? 0.8) *
      (x.c.hidden ? 1.25 : 1) *
      severityWeight[x.c.severity];
    const prior = best.get(x.c.subject);
    if (!prior || score > prior.score) best.set(x.c.subject, { ...x, score });
  }
  const pool = [...best.values()],
    out: Ranked[] = [];
  while (out.length < MAX_FINDINGS && pool.length) {
    const adjusted = (x: Ranked) =>
      x.score * 0.85 ** out.filter((o) => o.d.id === x.d.id).length;
    pool.sort(
      (a, b) =>
        adjusted(b) - adjusted(a) || a.c.subject.localeCompare(b.c.subject),
    );
    out.push(pool.shift()!);
  }
  return out;
}

// The scan, run by the worker as a "home_insights" job.
export async function runInsightsScan(
  store: Store,
  scope: string,
  _job?: Resource,
) {
  const stage = (s: InsightStage | undefined, detail?: string) =>
    saveStored(store, scope, (x) => ({
      ...x,
      stage: s,
      stageDetail:
        detail && s ? { ...x.stageDetail, [s]: detail } : x.stageDetail,
    }));
  try {
    await stage("understand");
    const ctx = await context(store, scope);
    if (!ctx) {
      await saveStored(store, scope, (x) => ({
        ...x,
        stage: undefined,
        supported: false,
        checkedAt: now(),
        findings: [],
      }));
      return;
    }
    const { workspace, datasets, stored } = ctx;
    const t = thresholdsOf(stored);
    const activities = ctx.activities;
    const byActivity = new Map(activities.map((a) => [a.id, a]));
    const proposed = ctx.drivers;
    const sel = await selectDrivers(
      store,
      scope,
      proposed,
      datasets,
      stored,
      t,
      activities,
    );
    const names = sel.drivers.map((d) => d.label);
    const list = (xs: string[]) =>
      xs.length < 2
        ? xs.join("")
        : xs.slice(0, -1).join(", ") + " and " + xs.at(-1);
    await stage(
      "understand",
      names.length
        ? `Priorities: ${list(names)}${sel.ownerConfirmed ? " (confirmed by an owner)" : ""}.`
        : "No activity with enough dated history to check yet.",
    );
    const first = sel.drivers[0] && sel.windows.get(sel.drivers[0].id);
    await stage(
      "fundamentals",
      first
        ? `Complete ${first.days === 7 ? "weeks" : "four-week periods"}: ${periodLabel(...first.current)} against ${periodLabel(...first.baseline)}, in UTC.`
        : undefined,
    );
    await stage("shifts");
    const byId = new Map(datasets.map((d) => [d.id, d]));
    const found: Omit<Ranked, "score">[] = [],
      coverage: InsightCoverage[] = [];
    let checks = 0,
      setAside = 0;
    for (const d of shareChecks(sel.drivers)) {
      const w = sel.windows.get(d.id)!;
      const { sql, inputs } = driverQuery(d, w, t, byActivity);
      const r = await queryDatasets(store, scope, inputs, sql, { limit: 1000 });
      const { candidates, notes } = evaluate(
        d,
        w,
        t,
        r.rows as Record<string, unknown>[],
        byActivity,
      );
      const stale = Date.now() - w.end > 14 * DAY;
      const sources = inputs.map((id) => ({
        datasetId: id,
        name: byId.get(id)?.name || id,
      }));
      for (const c of candidates) {
        c.details.limitations.push(
          ...(d.measure && !d.measure.currency
            ? [
                "Currency is not specified; amounts are shown without a currency symbol.",
              ]
            : []),
          ...(stale
            ? [
                `The data ends on ${new Date(w.end - DAY).toLocaleDateString("en-US", { timeZone: "UTC", dateStyle: "medium" })}; nothing newer was imported.`,
              ]
            : []),
          "Two periods cannot establish long-term seasonality.",
        );
        found.push({ c, d, w, sql, sources });
      }
      const order: Note["state"][] = [
        "found",
        "set_aside",
        "steady",
        "skipped",
      ];
      coverage.push({
        driver: d.id,
        label: d.label,
        checks: (["cohort", "mix", "unit", "conversion"] as const).map(
          (check) => {
            const n = notes
              .filter((x) => x.check === check)
              .sort((a, b) => order.indexOf(a.state) - order.indexOf(b.state));
            if (n[0] && n[0].state !== "skipped") checks++;
            if (n[0]?.state === "set_aside") setAside++;
            return {
              check,
              label: checkLabel(check),
              state: n[0]?.state || "skipped",
              note: n.map((x) => x.note).join(" ") || "Not checked.",
            };
          },
        ),
      });
    }
    const ranked = rankCandidates(
      found,
      sel.drivers.map((d) => d.id),
    );
    const observedAt = now();
    const findings: StoredFinding[] = ranked.map(
      ({ c, d, w, sql, sources, score }) => {
        const id = digest(c.subject).slice(0, 16);
        return {
          id,
          version: digest(
            [id, c.severity, Math.round(c.magnitude * 100)].join(":"),
          ).slice(0, 12),
          severity: c.severity,
          check: c.check,
          driver: { id: d.id, label: d.label },
          headline: c.headline,
          support: c.support,
          values: c.values,
          periods: {
            current: period(w.current),
            baseline: period(w.baseline),
            ...(c.cohort ? { cohort: period(w.cohort) } : {}),
          },
          details: {
            ...c.details,
            comparison: `${periodLabel(...w.current)} against ${periodLabel(...w.baseline)}: complete ${w.days === 7 ? "weeks" : "four-week periods"} with the same weekdays, in UTC.`,
            query: sql,
            sources,
          },
          ask: c.ask,
          observedAt,
          impact: Math.round(c.impact * 1e4) / 1e4,
          strength: Math.round(Math.min(1, Math.abs(c.z) / 4) * 100) / 100,
          score,
          magnitude: c.magnitude,
        };
      },
    );
    await saveStored(store, scope, (x) => ({
      ...x,
      version: INSIGHTS_VERSION,
      pendingManual: !!x.pendingManual && x.pendingManualKey !== ctx.key,
      pendingManualKey:
        x.pendingManualKey !== ctx.key ? x.pendingManualKey : undefined,
      key:
        stored?.priorities && !sel.ownerConfirmed
          ? ctx.key
          : inputsKey(
              workspace,
              watchedDatasets(
                proposed,
                datasets,
                {
                  ...stored,
                  selected: sel.drivers.map((d) => d.id),
                  priorities: sel.ownerConfirmed
                    ? stored?.priorities
                    : undefined,
                },
                activities,
              ),
              {
                ...stored,
                priorities: sel.ownerConfirmed ? stored?.priorities : undefined,
              },
            ),
      stage: undefined,
      stageDetail: {
        ...x.stageDetail,
        shifts: sel.drivers.length
          ? `${checks} ${checks === 1 ? "check" : "checks"} across ${sel.drivers.length} ${sel.drivers.length === 1 ? "priority" : "priorities"}: ${findings.length ? `${findings.length} ${findings.length === 1 ? "shift" : "shifts"} worth a look` : "nothing material"}${setAside ? `; ${setAside} set aside as possible data-quality changes` : ""}.`
          : undefined,
      },
      checkedAt: observedAt,
      probes: sel.probes,
      candidates: sel.candidates,
      unavailable: sel.unavailable,
      selected: sel.drivers.map((d) => d.id),
      priorities:
        !sel.ownerConfirmed &&
        JSON.stringify(x.priorities) === JSON.stringify(stored?.priorities)
          ? undefined
          : x.priorities,
      ambiguous: sel.ambiguous,
      coverage,
      findings,
      period: first
        ? { current: period(first.current), baseline: period(first.baseline) }
        : x.period,
      supported: activities.length > 0 && proposed.length > 0,
    }));
  } catch (e) {
    await stage(undefined).catch(() => {});
    throw e;
  }
}
