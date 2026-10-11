// Home insights, part 2: the four checks within one selected driver. One
// bounded aggregate query over complete, weekday-aligned UTC periods returns
// at most a few hundred rows; code turns them into candidate findings, each
// with its figures, evidence, definitions and limitations, or says why a
// check found nothing or could not run. These are measured shifts, not causes.
import {
  datasetTable,
  type HomeInsight,
  type InsightCheck,
  type InsightPeriod,
  type InsightSeverity,
  type InsightThresholds,
} from "../../../packages/shared/src/index.js";
import { lit, plural, q } from "./discovery.js";
import {
  capital,
  humanize,
  lowerName,
  type Activity,
  type Driver,
} from "./insights-model.js";

export const DEFAULT_THRESHOLDS: InsightThresholds = {
  minPeriodEvents: 30,
  minCohortSize: 10,
  cohortMinActivities: 2,
  minSegmentEvents: 20,
  relativeChange: 0.15,
  shareChange: 0.05,
  concentrationChange: 0.08,
  rateChange: 0.2,
  minImpact: 0.02,
  minZ: 2,
  issueRelative: 0.25,
  concentrationTop: 10,
  conversionLagDays: 1,
};

export const DAY = 86_400_000;
// From a probe: the activity's earliest and latest event and its row count.
export type Coverage = { lo: number; hi: number; rows: number };
export type Window = {
  days: number;
  // Exclusive, at UTC midnight.
  end: number;
  current: [number, number];
  baseline: [number, number];
  cohort: [number, number];
  // The data reaches back to the start of the cohort window.
  cohortReady: boolean;
};

const dayStart = (t: number) => Math.floor(t / DAY) * DAY;
const fmtDate = (t: number, opts: Intl.DateTimeFormatOptions) =>
  new Date(t).toLocaleDateString("en-US", { timeZone: "UTC", ...opts });
// "Oct 2–8, 2026", "Sep 25 – Oct 1, 2026"; end is exclusive.
export function periodLabel(start: number, end: number) {
  const last = end - DAY,
    a = new Date(start),
    b = new Date(last);
  const year = b.getUTCFullYear();
  if (start >= last) return fmtDate(start, { dateStyle: "medium" });
  if (a.getUTCFullYear() !== year)
    return `${fmtDate(start, { dateStyle: "medium" })} – ${fmtDate(last, { dateStyle: "medium" })}`;
  if (a.getUTCMonth() === b.getUTCMonth())
    return `${fmtDate(start, { month: "short" })} ${a.getUTCDate()}–${b.getUTCDate()}, ${year}`;
  return `${fmtDate(start, { month: "short", day: "numeric" })} – ${fmtDate(last, { month: "short", day: "numeric" })}, ${year}`;
}
export const period = ([start, end]: [number, number]): InsightPeriod => ({
  start: new Date(start).toISOString().slice(0, 10),
  end: new Date(end).toISOString().slice(0, 10),
  label: periodLabel(start, end),
});

// The latest two complete, equal periods: whole UTC days, weeks when there is
// enough activity each week, else four-week periods. The day of the latest
// record counts only when the snapshot was taken after that day ended, so a
// day still in progress never enters a comparison.
export function chooseWindow(
  cov: Coverage,
  syncedAt: number,
  t: InsightThresholds,
  many: string,
): Window | { reason: string } {
  const end = Math.min(dayStart(cov.hi) + DAY, dayStart(syncedAt));
  const complete = Math.max(0, Math.floor((end - cov.lo) / DAY));
  const perDay = cov.rows / Math.max(1, (end - cov.lo) / DAY);
  const fits = (days: number) => cov.lo <= end - 2 * days * DAY;
  const enough = (days: number) => perDay * days >= t.minPeriodEvents;
  const days = fits(7) && enough(7) ? 7 : fits(28) && enough(28) ? 28 : 0;
  if (!days)
    return {
      reason: !fits(7)
        ? `Needs two complete weeks of ${many}; the data covers ${complete} complete ${complete === 1 ? "day" : "days"}.`
        : !fits(28)
          ? `Too few ${many} each week (about ${Math.round(perDay * 7)}; needs ${t.minPeriodEvents}), and not enough history for four-week periods.`
          : `Too few ${many}: about ${Math.round(perDay * 28)} every four weeks; needs ${t.minPeriodEvents}.`,
    };
  const current: [number, number] = [end - days * DAY, end],
    baseline: [number, number] = [end - 2 * days * DAY, end - days * DAY],
    cohort: [number, number] = [baseline[0] - 2 * days * DAY, baseline[0]];
  return {
    days,
    end,
    current,
    baseline,
    cohort,
    cohortReady: cov.lo <= cohort[0],
  };
}

const ts = (a: Activity, alias: string) =>
  a.time.zoned
    ? `CAST(timezone('UTC', ${alias}.${q(a.time.column)}) AS TIMESTAMP)`
    : `CAST(${alias}.${q(a.time.column)} AS TIMESTAMP)`;
const at = (t: number) =>
  `TIMESTAMP ${lit(new Date(t).toISOString().slice(0, 19).replace("T", " "))}`;
const str = (expr: string) => `CAST(${expr} AS VARCHAR)`;
const COLUMNS = "part, w, g, n, v, s2, k, days, np";
// One output row: which part, which period, which group, then the figures.
const row = (
  part: string,
  w: string,
  g: string,
  f: Partial<Record<"n" | "v" | "s2" | "k" | "days" | "np", string>>,
) =>
  `SELECT ${lit(part)} AS part, ${w} AS w, ${g} AS g, ${[
    "n",
    "v",
    "s2",
    "k",
    "days",
    "np",
  ]
    .map((c) => `CAST(${f[c as keyof typeof f] ?? "NULL"} AS DOUBLE) AS ${c}`)
    .join(", ")}`;

// Top-K sizes the query reports; evaluation keeps the largest that is still a
// small minority of the participants.
const topSizes = (t: InsightThresholds) =>
  [...new Set([t.concentrationTop, 5, 3])].filter(
    (k) => k <= t.concentrationTop,
  );

// The one query a driver needs, over its activity's complete periods (and the
// cohort window before them), plus its funnel's upstream rows when it has one.
export function driverQuery(
  d: Driver,
  w: Window,
  t: InsightThresholds,
  activities: Map<string, Activity>,
) {
  const a = d.activity,
    inputs = new Set([a.datasetId]);
  const measure = d.measure
    ? `TRY_CAST(a.${q(d.measure.column)} AS DOUBLE)${d.measure.scale === "cents" ? " / 100.0" : ""}`
    : "1.0";
  let join = "",
    actor = "CAST(NULL AS VARCHAR)",
    contributor = "CAST(NULL AS VARCHAR)";
  if (d.actor?.via) {
    inputs.add(d.actor.via.datasetId);
    join = ` LEFT JOIN ${q(datasetTable(d.actor.via.datasetId))} i ON ${str(`a.${q(d.actor.column)}`)} = ${str(`i.${q(d.actor.via.key)}`)}`;
    actor = str(`i.${q(d.actor.via.column)}`);
  } else if (d.actor) actor = str(`a.${q(d.actor.column)}`);
  if (d.contributor)
    contributor =
      d.contributor === d.actor ? actor : str(`a.${q(d.contributor.column)}`);
  const dims = d.dimensions.slice(0, 3);
  const cohort = !!d.actor && w.cohortReady;
  const from = cohort ? w.cohort[0] : w.baseline[0];
  const time = ts(a, "a");
  const ctes = [
    `a AS (SELECT ${time} AS __t, ${measure} AS __m, ${actor} AS __p, ${contributor} AS __c${dims
      .map((x, i) => `, ${str(`a.${q(x.column)}`)} AS __d${i}`)
      .join(
        "",
      )} FROM ${q(datasetTable(a.datasetId))} a${join} WHERE a.${q(a.time.column)} IS NOT NULL AND ${time} >= ${at(from)} AND ${time} < ${at(w.end)}${d.measure ? ` AND ${measure} IS NOT NULL` : ""})`,
    `p AS (SELECT *, CASE WHEN __t >= ${at(w.current[0])} THEN 'cur' WHEN __t >= ${at(w.baseline[0])} THEN 'base' ELSE 'cohort' END AS __w FROM a)`,
  ];
  const parts = [
    `${row("total", "__w", "''", { n: "count(*)", v: "sum(__m)", s2: "sum(__m * __m)", k: "count(DISTINCT __p)", days: "count(DISTINCT CAST(__t AS DATE))", np: "count(*) - count(__p)" })} FROM p WHERE __w <> 'cohort' GROUP BY __w`,
  ];
  if (cohort) {
    ctes.push(
      `r AS (SELECT __p FROM p WHERE __w = 'cohort' AND __p IS NOT NULL GROUP BY __p HAVING count(*) >= ${t.cohortMinActivities})`,
    );
    parts.push(
      `${row("cohort", "p.__w", "CASE WHEN r.__p IS NULL THEN 'other' ELSE 'regular' END", { n: "count(*)", v: "sum(__m)", s2: "sum(__m * __m)", k: "count(DISTINCT p.__p)" })} FROM p LEFT JOIN r ON p.__p = r.__p WHERE p.__w <> 'cohort' GROUP BY 2, 3`,
      `${row("cohort_size", "'cohort'", "''", { n: "count(*)" })} FROM r`,
    );
  }
  dims.forEach((_, i) =>
    parts.push(
      `${row("d" + i, "__w", `COALESCE(NULLIF(TRIM(__d${i}), ''), '(none)')`, { n: "count(*)", v: "sum(__m)", s2: "sum(__m * __m)", k: "count(DISTINCT __p)" })} FROM p WHERE __w <> 'cohort' GROUP BY 2, 3`,
    ),
  );
  if (d.contributor) {
    ctes.push(
      `c AS (SELECT __w, __c, count(*) AS n, sum(__m) AS v, sum(__m * __m) AS s2, row_number() OVER (PARTITION BY __w ORDER BY sum(__m) DESC NULLS LAST, __c) AS rn FROM p WHERE __w <> 'cohort' AND __c IS NOT NULL GROUP BY __w, __c)`,
    );
    parts.push(
      `${row("contributors", "__w", "''", { n: "count(*)" })} FROM c GROUP BY __w`,
      ...topSizes(t).map(
        (k) =>
          `${row("top", "__w", lit(String(k)), { n: "sum(n)", v: "sum(v)", s2: "sum(s2)", k: "count(*)" })} FROM c WHERE rn <= ${k} GROUP BY __w`,
      ),
    );
  }
  const f = d.funnel,
    up = f && activities.get(f.upstream),
    down = f && activities.get(f.downstream);
  if (f && up && down) {
    inputs.add(up.datasetId);
    inputs.add(down.datasetId);
    const lag = t.conversionLagDays * DAY,
      seg = up.dimensions.find((x) => x.role === "source") || up.dimensions[0],
      ut = ts(up, "u");
    let x = "",
      converted = "";
    if (f.link.side === "flag")
      converted = `CASE WHEN TRY_CAST(u.${q(f.link.column)} AS BOOLEAN) THEN 1.0 ELSE 0.0 END`;
    else {
      const [col, key] =
        f.link.side === "downstream"
          ? [f.link.column, up.key!]
          : [f.link.key, f.link.column];
      // downstream: a downstream row names the upstream key; upstream: the
      // upstream row names a downstream key.
      x =
        f.link.side === "downstream"
          ? ` LEFT JOIN (SELECT DISTINCT ${str(q(col))} AS __k FROM ${q(datasetTable(down.datasetId))} WHERE ${q(col)} IS NOT NULL) x ON ${str(`u.${q(key)}`)} = x.__k`
          : ` LEFT JOIN (SELECT DISTINCT ${str(q(col))} AS __k FROM ${q(datasetTable(down.datasetId))}) x ON ${str(`u.${q(key)}`)} = x.__k`;
      converted = "CASE WHEN x.__k IS NULL THEN 0.0 ELSE 1.0 END";
    }
    ctes.push(
      `u AS (SELECT CASE WHEN ${ut} >= ${at(w.current[0])} THEN 'cur' ELSE 'base' END AS __w, ${seg ? str(`u.${q(seg.column)}`) : "CAST(NULL AS VARCHAR)"} AS __s, ${converted} AS __x FROM ${q(datasetTable(up.datasetId))} u${x} WHERE u.${q(up.time.column)} IS NOT NULL AND ((${ut} >= ${at(w.baseline[0])} AND ${ut} < ${at(w.baseline[1] - lag)}) OR (${ut} >= ${at(w.current[0])} AND ${ut} < ${at(w.end - lag)})))`,
    );
    parts.push(
      `${row("conv_total", "__w", "''", { n: "count(*)", v: "sum(__x)" })} FROM u GROUP BY __w`,
    );
    if (seg)
      parts.push(
        `${row("conv", "__w", "COALESCE(NULLIF(TRIM(__s), ''), '(none)')", { n: "count(*)", v: "sum(__x)" })} FROM u GROUP BY 2, 3`,
      );
  }
  return {
    sql: `WITH ${ctes.join(",\n")}\nSELECT ${COLUMNS} FROM (\n${parts.join("\nUNION ALL\n")}\n) ORDER BY part, w, g`,
    inputs: [...inputs],
  };
}

type Figures = { n: number; v: number; s2: number; k: number };
type Parsed = {
  total: Record<string, Figures & { days: number; np: number }>;
  cohort: Record<string, Record<string, Figures>>;
  cohortSize?: number;
  dims: Record<string, Record<string, Figures>>[];
  contributors: Record<string, number>;
  top: Record<string, Record<string, Figures>>;
  conv: Record<string, Record<string, Figures>>;
  convTotal: Record<string, Figures>;
};
const num = (v: unknown) =>
  v == null || v === "" || !Number.isFinite(Number(v)) ? 0 : Number(v);
export function parseRows(rows: Record<string, unknown>[]): Parsed {
  const out: Parsed = {
    total: {},
    cohort: {},
    dims: [{}, {}, {}],
    contributors: {},
    top: {},
    conv: {},
    convTotal: {},
  };
  for (const r of rows) {
    const part = String(r.part),
      w = String(r.w),
      g = String(r.g ?? "");
    const f = { n: num(r.n), v: num(r.v), s2: num(r.s2), k: num(r.k) };
    const into = (m: Record<string, Record<string, Figures>>, key: string) =>
      ((m[key] ||= {})[w] = f);
    if (part === "total")
      out.total[w] = { ...f, days: num(r.days), np: num(r.np) };
    else if (part === "cohort") into(out.cohort, g);
    else if (part === "cohort_size") out.cohortSize = f.n;
    else if (/^d\d$/.test(part)) into(out.dims[Number(part[1])], g);
    else if (part === "contributors") out.contributors[w] = f.n;
    else if (part === "top") into(out.top, g);
    else if (part === "conv") into(out.conv, g);
    else if (part === "conv_total") out.convTotal[w] = f;
  }
  return out;
}

// Number formatting for the text a finding carries (en-US; the web app shows
// it as given).
const int = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 }),
  dec1 = new Intl.NumberFormat("en-US", {
    minimumFractionDigits: 1,
    maximumFractionDigits: 1,
  }),
  dec2 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 }),
  usd = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: 0,
  }),
  usdCents = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }),
  usdCompact = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    notation: "compact",
    maximumFractionDigits: 1,
  });
const money = (v: number) =>
  Math.abs(v) >= 1000 || Number.isInteger(v)
    ? usd.format(v)
    : usdCents.format(v);
const pctOf = (r: number) => {
  const p = Math.abs(r * 100);
  return (p >= 100 ? int.format(p) : dec1.format(p)) + "%";
};
const arrow = (r: number) => (r > 0 ? "↑" : r < 0 ? "↓" : "");
const signed = (r: number | null) =>
  r === null
    ? "new"
    : Math.abs(r) < 0.0005
      ? "0%"
      : (r > 0 ? "+" : "−") + pctOf(r);
const share = (s: number) =>
  (Math.abs(s) < 0.1 ? dec1 : int).format(s * 100) + "%";
const pts = (d: number) => dec1.format(Math.abs(d * 100)) + " pts";
const signedPts = (d: number) =>
  Math.abs(d) < 0.0005 ? "0 pts" : (d > 0 ? "+" : "−") + pts(d);
const wordCount = (s: string) => s.split(/\s+/).filter(Boolean).length;
// "a session", "an order", "a user".
const an = (noun: string) =>
  (/^(a|e|i|o|un[^i]|hour|honest)/i.test(noun) ? "an " : "a ") + noun;
// A category value as a label: plain words are capitalised ("Referral"),
// identifiers and URLs are quoted as they are.
const valueLabel = (v: string) =>
  /^[a-z][a-z -]*$/i.test(v) ? capital(v) : `“${v}”`;
const valueLower = (v: string) =>
  /^[a-z][a-z -]*$/i.test(v) ? v.toLowerCase() : `“${v}”`;
// The first of the options that fits in four words.
function brief(
  options: { text: string; figure: string; figureFirst?: boolean }[],
) {
  return (
    options.find((o) => wordCount(o.text) + wordCount(o.figure) <= 4) ||
    options.at(-1)!
  );
}
// Relative change; null when something appears from nothing.
const rel = (a: number, b: number) => (a > 0 ? (b - a) / a : b > 0 ? null : 0);
const delta = (r: number | null, threshold: number) =>
  r !== null && Math.abs(r) < threshold / 2;
// |z| for a change in a sum, treating each sum as compound Poisson (its
// variance is the sum of squares).
const sumZ = (a: Figures, b: Figures) =>
  (b.v - a.v) / Math.sqrt(Math.max(1e-12, a.s2 + b.s2));
// |z| for a change in one part's share of a total, by the delta method.
function shareZ(g0: Figures, t0: Figures, g1: Figures, t1: Figures) {
  const variance = (g: Figures, t: Figures) => {
    const rest = { v: t.v - g.v, s2: Math.max(0, t.s2 - g.s2) };
    return t.v > 0
      ? (rest.v ** 2 * g.s2 + g.v ** 2 * rest.s2) / t.v ** 4
      : Infinity;
  };
  const s0 = t0.v > 0 ? g0.v / t0.v : 0,
    s1 = t1.v > 0 ? g1.v / t1.v : 0;
  return (
    (s1 - s0) / Math.sqrt(Math.max(1e-12, variance(g0, t0) + variance(g1, t1)))
  );
}
// Two-proportion z.
function rateZ(x0: number, n0: number, x1: number, n1: number) {
  const p = (x0 + x1) / (n0 + n1),
    se = Math.sqrt(p * (1 - p) * (1 / n0 + 1 / n1));
  return se > 0 ? (x1 / n1 - x0 / n0) / se : 0;
}
const variance = (f: Figures) =>
  f.n > 1 ? Math.max(0, (f.s2 - f.v ** 2 / f.n) / (f.n - 1)) : 0;

export type Candidate = {
  check: InsightCheck;
  subject: string;
  severity: InsightSeverity;
  headline: string;
  support: HomeInsight["support"];
  values: HomeInsight["values"];
  details: Pick<
    HomeInsight["details"],
    "why" | "contribution" | "definition" | "evidence" | "bars" | "table"
  > & { limitations: string[] };
  ask: string;
  cohort?: boolean;
  impact: number;
  z: number;
  // A total that barely moved hides the change: what this feature is for.
  hidden: boolean;
  magnitude: number;
};
export type Note = {
  check: InsightCheck;
  state: "found" | "steady" | "skipped" | "set_aside";
  note: string;
};

// The driver's checks, from its query's rows.
export function evaluate(
  d: Driver,
  w: Window,
  t: InsightThresholds,
  rows: Record<string, unknown>[],
  activities: Map<string, Activity>,
): { candidates: Candidate[]; notes: Note[] } {
  const R = parseRows(rows),
    a = d.activity,
    candidates: Candidate[] = [],
    notes: Note[] = [];
  const note = (check: InsightCheck, state: Note["state"], text: string) =>
    notes.push({ check, state, note: text });
  const T0 = R.total.base,
    T1 = R.total.cur;
  const base = periodLabel(...w.baseline),
    cur = periodLabel(...w.current),
    // Keeps findings of different measures on one activity apart.
    measured = d.measure?.column ?? "count";
  const isMoney = !!d.measure,
    // What is measured, in words: "revenue", "sessions".
    noun = isMoney ? d.label.toLowerCase() : a.many,
    fmt = (v: number) => (isMoney ? money(v) : int.format(v)),
    direction = d.measure ? d.measure.direction : a.direction;
  for (const [c, reason] of Object.entries(d.checks))
    if (reason) note(c as InsightCheck, "skipped", reason);
  const all = (state: Note["state"], text: string) => {
    for (const c of ["cohort", "mix", "unit", "conversion"] as const)
      if (d.checks[c] === null) note(c, state, text);
  };
  if (!T0 || !T1 || T0.n < t.minPeriodEvents || T1.n < t.minPeriodEvents) {
    all(
      "skipped",
      `Too few ${a.many} in each period (${int.format(T0?.n || 0)} and ${int.format(T1?.n || 0)}; needs ${t.minPeriodEvents}).`,
    );
    return { candidates, notes };
  }
  // A period missing most of its days of data is a gap, not a business change.
  if (T0.days >= 0.85 * w.days && T1.days < 0.6 * w.days) {
    all(
      "set_aside",
      `The latest period has ${a.many} on only ${T1.days} of ${w.days} days, which looks like a gap in the data rather than a business change.`,
    );
    return { candidates, notes };
  }
  const totalRel = rel(T0.v, T1.v) ?? 0;
  const totalPhrase =
    Math.abs(totalRel) < 0.005
      ? `${capital(noun)} overall ${isMoney ? "was" : "were"} unchanged`
      : `${capital(noun)} overall ${totalRel > 0 ? "rose" : "fell"} ${pctOf(totalRel)}`;
  const actorOne = d.actor ? lowerName(d.actor.entity) : "",
    actorMany = d.actor ? plural(actorOne) : "";
  const evidence = (extra = "") =>
    `${int.format(T0.n)} and ${int.format(T1.n)} ${a.many}${isMoney ? ` with ${an(d.measure!.column)}` : ""} in the two periods, from ${a.datasetName}${extra}.`;
  const severity = (change: number, offset = false): InsightSeverity =>
    direction === "unknown"
      ? "warning"
      : (direction === "up") === change > 0
        ? offset
          ? "warning"
          : "positive"
        : Math.abs(change) >= t.issueRelative
          ? "issue"
          : "warning";
  const material = (r: number | null, z: number, impact: number) =>
    r !== null &&
    Math.abs(r) >= t.relativeChange &&
    Math.abs(z) >= t.minZ &&
    impact >= t.minImpact;
  const steadyText = (what: string, r: number | null, z: number) =>
    r === null
      ? `${what}: nothing to compare.`
      : Math.abs(r) < t.relativeChange
        ? `${what} moved ${signed(r)}, within ±${pctOf(t.relativeChange)}.`
        : `${what} moved ${signed(r)}, but the evidence is weak (|z| ${dec1.format(Math.abs(z))}).`;

  // 1. Activity of a fixed core cohort.
  if (d.checks.cohort === null) {
    const size = R.cohortSize ?? 0,
      reg0 = R.cohort.regular?.base,
      reg1 = R.cohort.regular?.cur;
    const cohortLabel = periodLabel(...w.cohort);
    if (!w.cohortReady)
      note(
        "cohort",
        "skipped",
        `Needs ${a.many} from ${cohortLabel} to fix a cohort; the data starts later.`,
      );
    else if (size < t.minCohortSize || !reg0 || !reg1)
      note(
        "cohort",
        "skipped",
        `Only ${int.format(size)} ${size === 1 ? actorOne : actorMany} had at least ${t.cohortMinActivities} ${a.many} during ${cohortLabel}; a fixed cohort needs ${t.minCohortSize}.`,
      );
    else {
      const link0 = 1 - T0.np / T0.n,
        link1 = 1 - T1.np / T1.n;
      const o0 = R.cohort.other?.base || { n: 0, v: 0, s2: 0, k: 0 },
        o1 = R.cohort.other?.cur || { n: 0, v: 0, s2: 0, k: 0 };
      const r = rel(reg0.v, reg1.v),
        z = sumZ(reg0, reg1),
        impact = Math.abs(reg1.v - reg0.v) / T0.v;
      if (Math.abs(link1 - link0) >= 0.15)
        note(
          "cohort",
          "set_aside",
          `The share of ${a.many} linked to ${an(actorOne)} moved from ${share(link0)} to ${share(link1)}, which looks like a tracking change rather than a business one.`,
        );
      // A cohort that is nearly everyone only restates the total.
      else if (reg0.v >= 0.9 * T0.v)
        note(
          "cohort",
          "steady",
          `Regular ${actorMany} account for ${share(reg0.v / T0.v)} of ${noun}, so a fixed cohort adds nothing to the total.`,
        );
      else if (!material(r, z, impact))
        note(
          "cohort",
          "steady",
          steadyText(`Regular ${actorMany}' ${noun}`, r, z),
        );
      else {
        const change = r!,
          hidden = delta(totalRel, t.relativeChange);
        const lessMore = change < 0 ? "less" : "more";
        candidates.push({
          check: "cohort",
          subject: `cohort:${a.id}:${d.actor!.entity}:${measured}`,
          severity: severity(change),
          headline: isMoney
            ? `Regular ${actorMany} are spending ${lessMore}.`
            : `Regular ${actorMany} have ${change < 0 ? "fewer" : "more"} ${a.many}.`,
          support: brief([
            {
              text: isMoney
                ? `Regular-${actorOne} ${noun}`
                : `Regular ${a.many}`,
              figure: arrow(change) + pctOf(change),
            },
            {
              text: `Regular ${actorMany}`,
              figure: arrow(change) + pctOf(change),
            },
            { text: "Regulars", figure: arrow(change) + pctOf(change) },
          ]),
          values: {
            current: reg1.v,
            baseline: reg0.v,
            absolute: reg1.v - reg0.v,
            relative: change,
            unit: isMoney ? "currency" : "number",
          },
          details: {
            why:
              change < 0
                ? isMoney
                  ? `Steady totals can hide that established ${actorMany} are spending less. The reasons are not established by these figures.`
                  : `Replacement activity can keep totals steady while the ${actorMany} you rely on engage less. Its effect on other outcomes still needs checking.`
                : `Established ${actorMany} are more active than before. These figures do not show why, or whether it lasts.`,
            contribution: `Regular ${actorMany} ${reg1.v >= reg0.v ? "added" : "lost"} ${fmt(Math.abs(reg1.v - reg0.v))}${isMoney ? "" : ` ${a.many}`}; everyone else ${o1.v >= o0.v ? "added" : "lost"} ${fmt(Math.abs(o1.v - o0.v))}. ${totalPhrase}.`,
            definition: `${capital(actorMany)} with at least ${t.cohortMinActivities} ${a.many} during ${cohortLabel}: ${int.format(size)} ${actorMany}. Membership stays fixed across both periods. “Everyone else” covers other ${actorMany}${T0.np + T1.np > 0 ? ` and ${a.many} with no ${actorOne}` : ""}; it does not mean new ${actorMany}.`,
            evidence: evidence(
              `, linked to ${actorMany} by ${d.actor!.column}${d.actor!.via ? ` through ${lowerName(d.actor!.via.entity)}` : ""}`,
            ),
            limitations: [],
            bars: [
              {
                label: "Previous",
                share: reg0.v / T0.v,
                text: share(reg0.v / T0.v),
                previous: true,
              },
              {
                label: "Latest",
                share: reg1.v / T1.v,
                text: share(reg1.v / T1.v),
                previous: false,
              },
            ],
            table: {
              caption: `${capital(noun)} by a fixed cohort of ${actorMany}`,
              columns: ["Group", "Previous", "Latest", "Change"],
              rows: [
                [
                  `Regular ${actorMany} (${int.format(size)}, fixed)`,
                  fmt(reg0.v),
                  fmt(reg1.v),
                  signed(r),
                ],
                [
                  "Everyone else",
                  fmt(o0.v),
                  fmt(o1.v),
                  signed(rel(o0.v, o1.v)),
                ],
                ["Total", fmt(T0.v), fmt(T1.v), signed(totalRel)],
              ],
            },
          },
          ask: `Regular ${actorMany} (at least ${t.cohortMinActivities} ${a.many} during ${cohortLabel}) went from ${fmt(reg0.v)} to ${fmt(reg1.v)}${isMoney ? ` of ${noun}` : ` ${a.many}`} between ${base} and ${cur}, while the total went from ${fmt(T0.v)} to ${fmt(T1.v)}. Break this down by week and show which of these ${actorMany} changed most.`,
          cohort: true,
          impact,
          z,
          hidden,
          magnitude: change,
        });
        note(
          "cohort",
          "found",
          `Regular ${actorMany}' ${noun} ${signed(r)} (${fmt(reg0.v)} → ${fmt(reg1.v)}).`,
        );
      }
    }
  }

  // 2. Mix and dependency: shares by category, and concentration.
  if (d.checks.mix === null) {
    const before = candidates.length;
    const steady: string[] = [];
    d.dimensions.slice(0, 3).forEach((dim, i) => {
      const groups = R.dims[i] || {};
      const fig = (g: string, w: "base" | "cur") =>
        groups[g]?.[w] || { n: 0, v: 0, s2: 0, k: 0 };
      const none0 = fig("(none)", "base").v / T0.v,
        none1 = fig("(none)", "cur").v / T1.v;
      if (Math.abs(none1 - none0) >= t.shareChange) {
        note(
          "mix",
          "set_aside",
          `${capital(a.many)} with no ${humanize(dim.column)} went from ${share(none0)} to ${share(none1)}, which looks like a tagging change rather than a business one.`,
        );
        return;
      }
      let best: { g: string; ds: number; z: number } | undefined;
      for (const g of Object.keys(groups)) {
        if (g === "(none)") continue;
        const g0 = fig(g, "base"),
          g1 = fig(g, "cur");
        if (g0.n + g1.n < t.minSegmentEvents) continue;
        const ds = g1.v / T1.v - g0.v / T0.v,
          z = shareZ(g0, T0, g1, T1);
        if (!best || Math.abs(ds) > Math.abs(best.ds)) best = { g, ds, z };
      }
      if (!best) return;
      const g0 = fig(best.g, "base"),
        g1 = fig(best.g, "cur");
      const s0 = g0.v / T0.v,
        s1 = g1.v / T1.v,
        segRel = rel(g0.v, g1.v),
        impact = Math.abs(g1.v - g0.v) / T0.v;
      if (
        Math.abs(best.ds) < t.shareChange ||
        Math.abs(best.z) < t.minZ ||
        impact < t.minImpact
      ) {
        steady.push(
          `${valueLabel(best.g)}'s share by ${humanize(dim.column)} moved ${signedPts(best.ds)}.`,
        );
        return;
      }
      const G = valueLabel(best.g),
        g = valueLower(best.g),
        grew = g1.v > g0.v;
      const sev: InsightSeverity =
        direction === "unknown"
          ? "warning"
          : direction === "up"
            ? grew
              ? totalRel >= -t.relativeChange / 2
                ? "positive"
                : "warning"
              : totalRel <= -t.relativeChange &&
                  Math.abs(segRel ?? 0) >= t.issueRelative
                ? "issue"
                : "warning"
            : grew
              ? Math.abs(segRel ?? 1) >= t.issueRelative
                ? "issue"
                : "warning"
              : "positive";
      const ordered = Object.keys(groups)
        .sort(
          (x, y) =>
            Math.max(fig(y, "base").v, fig(y, "cur").v) -
              Math.max(fig(x, "base").v, fig(x, "cur").v) || x.localeCompare(y),
        )
        .slice(0, 8);
      candidates.push({
        check: "mix",
        subject: `mix:${a.id}:${dim.column}:${best.g}:${measured}`,
        severity: sev,
        headline:
          segRel === null
            ? `${G} ${noun} appeared.`
            : `${G} ${noun} ${grew ? "grew" : "fell"} ${pctOf(segRel)}.`,
        support: brief([
          {
            figure: fmt(g1.v),
            text: `${g} ${noun}`,
            figureFirst: true,
          },
          { text: `${G} share`, figure: share(s1) },
          { text: "Share", figure: share(s1) },
        ]),
        values: {
          current: s1,
          baseline: s0,
          absolute: s1 - s0,
          relative: segRel,
          unit: "share",
        },
        details: {
          why: grew
            ? `Your ${humanize(dim.column)} mix has changed. Whether the added ${g} ${a.many} convert or last, and whether a campaign or tagging change explains it, is not established by these figures.`
            : `Less of your ${noun} comes from ${g}. A channel, campaign or tagging change could explain it; these figures do not show which.`,
          contribution: `${G} ${grew ? "added" : "lost"} ${fmt(Math.abs(g1.v - g0.v))}${isMoney ? "" : ` ${a.many}`}, going from ${share(s0)} to ${share(s1)} of the total. ${totalPhrase}.`,
          definition: `Share of ${noun} by ${humanize(dim.column)} (${a.datasetName}, ${dim.column}) in each period. Counts are shown beside shares, so a change in the total does not mislead.`,
          evidence: evidence(),
          limitations:
            g0.v === 0
              ? [
                  `${G} appears for the first time in the latest period; check whether it is new or a relabelled ${humanize(dim.column)}.`,
                ]
              : [],
          bars: [
            { label: "Previous", share: s0, text: share(s0), previous: true },
            { label: "Latest", share: s1, text: share(s1), previous: false },
          ],
          table: {
            caption: `${capital(noun)} by ${humanize(dim.column)}`,
            columns: [dim.label, "Previous", "Latest", "Change", "Share"],
            rows: [
              ...ordered.map((g) => {
                const x0 = fig(g, "base"),
                  x1 = fig(g, "cur");
                return [
                  g,
                  fmt(x0.v),
                  fmt(x1.v),
                  signed(rel(x0.v, x1.v)),
                  `${share(x0.v / T0.v)} → ${share(x1.v / T1.v)}`,
                ];
              }),
              ["Total", fmt(T0.v), fmt(T1.v), signed(totalRel), "100%"],
            ],
          },
        },
        ask: `${G} ${noun} went from ${fmt(g0.v)} (${share(s0)}) to ${fmt(g1.v)} (${share(s1)}) between ${base} and ${cur}. Compare ${humanize(dim.column)} by week and show what else changed.`,
        impact: Math.abs(best.ds),
        z: best.z,
        hidden: delta(totalRel, t.relativeChange),
        magnitude: best.ds,
      });
    });
    // Concentration: the share of the top few, ranked in each period.
    const contributor = d.contributor;
    if (contributor) {
      const one = lowerName(contributor.entity),
        many = plural(one);
      const c0 = R.contributors.base || 0,
        c1 = R.contributors.cur || 0;
      const k = topSizes(t)
        .filter((k) => k <= 0.2 * Math.min(c0, c1))
        .sort((x, y) => y - x)[0];
      if (!k)
        steady.push(
          `Only ${int.format(Math.min(c0, c1))} ${many} were active in a period, too few to measure concentration.`,
        );
      else {
        const top0 = R.top[String(k)]?.base,
          top1 = R.top[String(k)]?.cur;
        if (top0 && top1) {
          const s0 = top0.v / T0.v,
            s1 = top1.v / T1.v,
            ds = s1 - s0,
            z = shareZ(top0, T0, top1, T1);
          if (Math.abs(ds) < t.concentrationChange || Math.abs(z) < t.minZ)
            steady.push(`The top ${k} ${many}' share moved ${signedPts(ds)}.`);
          else {
            const rising = ds > 0;
            candidates.push({
              check: "mix",
              subject: `top:${a.id}:${contributor.entity}:${measured}`,
              severity: rising ? "warning" : "positive",
              headline: rising
                ? isMoney
                  ? `${capital(noun)} depends more on a few ${many}.`
                  : `${capital(a.many)} depend more on a few ${many}.`
                : `${capital(noun)} ${isMoney ? "is" : "are"} spread across more ${many}.`,
              support: brief([
                { text: `Top ${many}`, figure: `${share(s1)} share` },
                { text: `Top ${k}`, figure: `${share(s1)} share` },
              ]),
              values: {
                current: s1,
                baseline: s0,
                absolute: ds,
                relative: s0 > 0 ? ds / s0 : null,
                unit: "share",
              },
              details: {
                why: rising
                  ? `Steady totals can conceal greater exposure to a small number of ${many}. Check whether the rest ${isMoney ? "bought" : "did"} less; a few large ${a.many} may explain it, and the cause is not established.`
                  : `${capital(noun)} relies less on its largest ${many} than before. These figures do not show why.`,
                contribution: `The top ${k} ${many} went from ${fmt(top0.v)} to ${fmt(top1.v)}; everyone else from ${fmt(T0.v - top0.v)} to ${fmt(T1.v - top1.v)}. ${totalPhrase}.`,
                definition: `Top ${k} ${many} ranked independently in each period by ${noun}. This measures concentration; it does not mean the same ${k} ${many} ${isMoney ? "spent" : "did"} more. ${int.format(c0)} and ${int.format(c1)} ${many} were active in the two periods.`,
                evidence: evidence(
                  `, by ${contributor.column}${contributor.via ? ` through ${lowerName(contributor.via.entity)}` : ""}`,
                ),
                limitations: [],
                bars: [
                  {
                    label: "Previous",
                    share: s0,
                    text: share(s0),
                    previous: true,
                  },
                  {
                    label: "Latest",
                    share: s1,
                    text: share(s1),
                    previous: false,
                  },
                ],
                table: {
                  caption: `${capital(noun)} concentration`,
                  columns: ["Group", "Previous", "Latest"],
                  rows: [
                    [`Top ${k} ${many}`, fmt(top0.v), fmt(top1.v)],
                    ["Everyone else", fmt(T0.v - top0.v), fmt(T1.v - top1.v)],
                    ["Total", fmt(T0.v), fmt(T1.v)],
                  ],
                },
              },
              ask: `The top ${k} ${many} accounted for ${share(s0)} of ${noun} in ${base} and ${share(s1)} in ${cur}. Who are they, and did the remaining ${many} ${isMoney ? "spend" : "do"} less?`,
              impact: Math.abs(ds),
              z,
              hidden: delta(totalRel, t.relativeChange),
              magnitude: ds,
            });
          }
        }
      }
    }
    if (candidates.length > before)
      note(
        "mix",
        "found",
        candidates
          .slice(before)
          .map((c) => c.headline)
          .join(" "),
      );
    else if (!notes.some((n) => n.check === "mix" && n.state === "set_aside"))
      note(
        "mix",
        "steady",
        steady.join(" ") || "No category or participant moved materially.",
      );
  }

  // 3. Value or output per activity.
  if (d.checks.unit === null && d.measure) {
    {
      const A0 = T0.v / T0.n,
        A1 = T1.v / T1.n,
        rA = rel(A0, A1),
        rN = rel(T0.n, T1.n) ?? 0;
      const z =
        (A1 - A0) /
        Math.sqrt(Math.max(1e-12, variance(T0) / T0.n + variance(T1) / T1.n));
      const value = `Value per ${a.one}`;
      if (!material(rA, z, Math.abs(rA ?? 0)))
        note("unit", "steady", steadyText(value, rA, z));
      else {
        const change = rA!,
          offset =
            Math.sign(rN) !== Math.sign(change) &&
            Math.abs(rN) >= t.relativeChange / 2;
        const sev: InsightSeverity =
          d.measure.direction === "up"
            ? change < 0
              ? totalRel <= -t.relativeChange / 2 &&
                Math.abs(change) >= t.issueRelative
                ? "issue"
                : "warning"
              : offset
                ? "warning"
                : "positive"
            : change > 0
              ? "warning"
              : "positive";
        candidates.push({
          check: "unit",
          subject: `unit:${a.id}:value`,
          severity: sev,
          headline:
            change < 0
              ? rN > -t.relativeChange / 2
                ? `${capital(a.many)} held up, but value per ${a.one} fell ${pctOf(change)}.`
                : `Value per ${a.one} fell ${pctOf(change)}.`
              : rN <= -t.relativeChange / 2
                ? `Fewer ${a.many} were offset by larger ${a.many}.`
                : `Value per ${a.one} rose ${pctOf(change)}.`,
          support: brief([
            { text: value, figure: arrow(change) + pctOf(change) },
            { text: `Per ${a.one}`, figure: arrow(change) + pctOf(change) },
            { text: "Average value", figure: arrow(change) + pctOf(change) },
          ]),
          values: {
            current: A1,
            baseline: A0,
            absolute: A1 - A0,
            relative: change,
            unit: "currency",
          },
          details: {
            why:
              change < 0
                ? `Each ${a.one} brings in less than before. Discounts, a change in what is bought or a few unusual ${a.many} could explain it; these figures do not show which.`
                : offset
                  ? `Larger ${a.many} made up for fewer of them. Steady totals can hide fewer purchases; check whether a few large ${a.many} explain it.`
                  : `Each ${a.one} brings in more than before. Higher value per ${a.one} is not assumed to be better on its own.`,
            contribution: `${capital(a.many)} ${signed(rN)} and value per ${a.one} ${signed(change)}: ${noun} ${signed(totalRel)} overall.`,
            definition: `${capital(noun)} = number of ${a.many} × average ${humanize(d.measure.column)} per ${a.one}${d.measure.scale === "cents" ? " (stored in cents)" : ""}.`,
            evidence: evidence(),
            limitations: [],
            table: {
              caption: `${capital(noun)} as ${a.many} × value per ${a.one}`,
              columns: ["Measure", "Previous", "Latest", "Change"],
              rows: [
                [
                  capital(a.many),
                  int.format(T0.n),
                  int.format(T1.n),
                  signed(rN),
                ],
                [value, money(A0), money(A1), signed(change)],
                [capital(noun), money(T0.v), money(T1.v), signed(totalRel)],
              ],
            },
          },
          ask: `${capital(a.many)} went from ${int.format(T0.n)} to ${int.format(T1.n)} and value per ${a.one} from ${money(A0)} to ${money(A1)} between ${base} and ${cur}. Show how ${a.one} values are distributed in both periods.`,
          impact: Math.abs(change),
          z,
          hidden: delta(totalRel, t.relativeChange),
          magnitude: change,
        });
        note("unit", "found", `${value} ${signed(change)}.`);
      }
    }
  }

  // 4. Progression and conversion along a recorded link.
  const f = d.funnel,
    up = f && activities.get(f.upstream),
    down = f && activities.get(f.downstream);
  if (d.checks.conversion === null && f && up && down) {
    const C0 = R.convTotal.base,
      C1 = R.convTotal.cur;
    const lag = t.conversionLagDays,
      outcome = f.link.side === "flag" ? humanize(f.link.column) : down.one;
    if (!C0 || !C1 || C0.n < t.minPeriodEvents || C1.n < t.minPeriodEvents)
      note(
        "conversion",
        "skipped",
        `Too few ${up.many} with a complete observation window (${int.format(C0?.n || 0)} and ${int.format(C1?.n || 0)}; needs ${t.minPeriodEvents}).`,
      );
    else {
      const r0 = C0.v / C0.n,
        r1 = C1.v / C1.n,
        overall = rel(r0, r1);
      if (r0 > 0.01 && r1 < 0.2 * r0)
        note(
          "conversion",
          "set_aside",
          `Almost no ${up.many} link to ${an(outcome)} in the latest period (${share(r1)} against ${share(r0)}); the link may have stopped being recorded.`,
        );
      else {
        const seg =
          up.dimensions.find((x) => x.role === "source") || up.dimensions[0];
        type Pick = {
          g: string | null;
          rel: number;
          z: number;
          weight: number;
        };
        const options: Pick[] = [];
        const consider = (
          g: string | null,
          x0: Figures,
          x1: Figures,
          weight: number,
        ) => {
          const a0 = x0.v / x0.n,
            a1 = x1.v / x1.n,
            rr = rel(a0, a1);
          if (rr === null) return;
          const z = rateZ(x0.v, x0.n, x1.v, x1.n);
          if (
            Math.abs(rr) >= t.rateChange &&
            Math.abs(a1 - a0) >= 0.005 &&
            Math.abs(z) >= t.minZ
          )
            options.push({ g, rel: rr, z, weight });
        };
        consider(null, C0, C1, 1);
        for (const [g, byW] of Object.entries(R.conv)) {
          if (g === "(none)" || !byW.base || !byW.cur) continue;
          if (
            byW.base.n < t.minSegmentEvents / 2 ||
            byW.cur.n < t.minSegmentEvents / 2
          )
            continue;
          consider(
            g,
            byW.base,
            byW.cur,
            (byW.base.n + byW.cur.n) / (C0.n + C1.n),
          );
        }
        const pick = options.sort(
          (x, y) =>
            Math.abs(y.rel) * Math.max(0.2, y.weight) -
            Math.abs(x.rel) * Math.max(0.2, x.weight),
        )[0];
        if (!pick)
          note(
            "conversion",
            "steady",
            steadyText(
              `Conversion from ${up.many} to ${plural(outcome)}`,
              overall,
              rateZ(C0.v, C0.n, C1.v, C1.n),
            ),
          );
        else {
          const x0 = pick.g ? R.conv[pick.g].base : C0,
            x1 = pick.g ? R.conv[pick.g].cur : C1;
          const a0 = x0.v / x0.n,
            a1 = x1.v / x1.n,
            G = pick.g ? valueLabel(pick.g) : "";
          const hidden = !!pick.g && delta(overall, t.rateChange);
          const ordered = Object.keys(R.conv)
            .filter((g) => R.conv[g].base && R.conv[g].cur)
            .sort(
              (x, y) => R.conv[y].cur.n - R.conv[x].cur.n || x.localeCompare(y),
            )
            .slice(0, 8);
          const linkText =
            f.link.side === "flag"
              ? `${capital(an(up.one))} counts as converted when ${f.link.column} is true.`
              : f.link.side === "downstream"
                ? `${capital(an(up.one))} counts as converted when ${an(down.one)} links to it (${down.datasetName}, ${f.link.column}).`
                : `${capital(an(up.one))} counts as converted when its ${f.link.column} links to ${an(down.one)}.`;
          candidates.push({
            check: "conversion",
            subject: `conv:${up.id}>${down.id}:${pick.g ?? "overall"}`,
            severity:
              pick.rel < 0
                ? Math.abs(pick.rel) >= t.issueRelative
                  ? "issue"
                  : "warning"
                : "positive",
            headline: `${pick.g ? `${G} ${up.many}` : capital(up.many)} convert ${pick.rel < 0 ? "less" : "more"} often.`,
            support: brief([
              {
                text: pick.g ? `${G} conversion` : "Conversion",
                figure: `${arrow(a1 - a0)}${pts(a1 - a0)}`,
              },
              {
                text: "Conversion",
                figure: `${arrow(a1 - a0)}${pts(a1 - a0)}`,
              },
            ]),
            values: {
              current: a1,
              baseline: a0,
              absolute: a1 - a0,
              relative: pick.rel,
              unit: "rate",
            },
            details: {
              why:
                pick.rel < 0
                  ? `Fewer ${pick.g ? valueLower(pick.g) + " " : ""}${up.many} lead to ${an(outcome)}.${hidden ? " Gains elsewhere keep the overall rate steady." : ""} The cause is not established by these figures.`
                  : `${pick.g ? `${G} ${up.many}` : capital(up.many)} lead to ${an(outcome)} more often. Whether it lasts is not established.`,
              contribution: `${pick.g ? `${G} conversion` : "Conversion"} went from ${share(a0)} to ${share(a1)}${pick.g ? `; overall conversion ${share(r0) === share(r1) ? `stayed at ${share(r1)}` : `went from ${share(r0)} to ${share(r1)}`}${hidden ? ", so the overall rate hides it" : ""}` : ""}.`,
              definition: `${linkText} Only ${up.many} from the first ${w.days - lag} days of each period count, so each had at least ${lag} ${lag === 1 ? "day" : "days"} to convert.`,
              evidence: `${capital(up.many)} from ${up.datasetName}${f.link.side === "flag" ? "" : ` and ${down.many} from ${down.datasetName}`}. ${int.format(C0.n)} and ${int.format(C1.n)} ${up.many} in the two periods.`,
              limitations: [
                `Conversion follows recorded links; ${up.many} without one count as not converted.`,
              ],
              table: {
                caption: `Share of ${up.many} that led to ${an(outcome)}${seg ? `, by ${humanize(seg.column)}` : ""}`,
                columns: [
                  seg ? seg.label : "Group",
                  "Previous",
                  "Latest",
                  `Latest ${up.many}`,
                ],
                rows: [
                  ...ordered.map((g) => [
                    g,
                    share(R.conv[g].base.v / R.conv[g].base.n),
                    share(R.conv[g].cur.v / R.conv[g].cur.n),
                    int.format(R.conv[g].cur.n),
                  ]),
                  ["Overall", share(r0), share(r1), int.format(C1.n)],
                ],
              },
            },
            ask: `${pick.g ? `${G} ${up.many}` : capital(up.many)} converted to ${plural(outcome)} at ${share(a0)} in ${base} and ${share(a1)} in ${cur}${pick.g ? `, while overall conversion went from ${share(r0)} to ${share(r1)}` : ""}. What changed for these ${up.many}?`,
            // Outcomes gained or lost at the new rate, as a share of the
            // baseline's outcomes.
            impact: (Math.abs(a1 - a0) * x1.n) / Math.max(1, C0.v),
            z: pick.z,
            hidden,
            magnitude: pick.rel,
          });
          note(
            "conversion",
            "found",
            `${pick.g ? `${G} conversion` : "Conversion"} ${share(a0)} → ${share(a1)}.`,
          );
        }
      }
    }
  }
  return { candidates, notes };
}
