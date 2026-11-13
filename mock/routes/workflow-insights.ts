import type {
  FoundryPrincipal,
  FoundryWorkflowInsightIssue,
  FoundryWorkflowInsightSegment,
  FoundryWorkflowInsightTotals,
  FoundryWorkflowInsights,
  FoundryWorkflowRun,
} from "../../src/shared/wfe";

// What runs of an ontology's workflows came to over a window of whole UTC
// days, as the backend's GetFoundryWorkflowInsights counts it (api/
// workflow_insights.go): pure, over runs as the API returns them. Runs count
// by the day they started; a done assessment's claims, gaps and suggestions
// count; a gap or suggestion recurs when runs name the same text, case,
// spacing and end punctuation aside.

/** A run with what insights read of it. */
export type InsightRun = FoundryWorkflowRun & { startedAt: string };

const HELPED = new Set(["essential", "helpful"]);
const VERDICTS = ["essential", "helpful", "marginal", "unused", "misleading"];
const GAP_KINDS = [
  "missing_type",
  "missing_link",
  "missing_property",
  "missing_data",
  "wrong_data",
  "other",
];
/** The most runs insights read of a window (the newest). */
export const INSIGHT_RUNS = 5000;
const ISSUES = 10,
  AGENTS = 10,
  DAY = 86_400_000;

type Assessed = {
  claims: string[];
  gaps: { kind: string; text: string }[];
  suggestions: string[];
};

/** A done assessment's claims' support, gaps and suggestions; none for any other. */
function assessed(r: FoundryWorkflowRun): Assessed {
  const a = r.assessment;
  const out: Assessed = { claims: [], gaps: [], suggestions: [] };
  if (a?.status !== "done" || !a.assessmentJson) return out;
  try {
    const body = JSON.parse(a.assessmentJson) as {
      claims?: { support?: unknown }[];
      gaps?: { kind?: unknown; what?: unknown }[];
      suggestions?: unknown[];
    };
    for (const c of body.claims || [])
      if (typeof c?.support === "string") out.claims.push(c.support);
    for (const g of body.gaps || [])
      if (typeof g?.what === "string" && g.what.trim())
        out.gaps.push({
          kind: typeof g.kind === "string" && g.kind ? g.kind : "other",
          text: g.what.trim(),
        });
    for (const s of body.suggestions || [])
      if (typeof s === "string" && s.trim()) out.suggestions.push(s.trim());
  } catch {
    // An assessment that isn't JSON counts its verdict alone.
  }
  return out;
}

/**
 * A gap's or suggestion's text as runs are matched by it.
 *
 *     normalize("  Add a  Health score.") // "add a health score"
 */
export const normalize = (text: string) =>
  text
    .toLowerCase()
    .replace(/\s+/g, " ")
    .trim()
    .replace(/[.!?;:,]+$/, "")
    .trim();

/** What `runs` come to. */
export function totalsOf(runs: readonly InsightRun[]) {
  const t = {
    runs: runs.length,
    open: 0,
    finished: 0,
    assessed: 0,
    helped: 0,
    used: 0,
    claimsSupported: 0,
    claimsUnsupported: 0,
    claimsContradicted: 0,
    reads: 0,
    questions: 0,
    sends: 0,
    uploads: 0,
  };
  const verdicts = new Map<string, number>(),
    kinds = new Map<string, number>();
  let usd = 0;
  for (const r of runs) {
    if (r.status === "open") t.open++;
    else t.finished++;
    const c = r.counts || {};
    if ((c.accesses || 0) > 0) t.used++;
    t.reads += c.reads || 0;
    t.questions += c.questions || 0;
    t.sends += c.sends || 0;
    t.uploads += c.uploads || 0;
    usd += Number(r.assessment?.usdMicros || 0);
    const a = r.assessment;
    if (a?.status !== "done" || !a.verdict) continue;
    t.assessed++;
    if (HELPED.has(a.verdict)) t.helped++;
    verdicts.set(a.verdict, (verdicts.get(a.verdict) || 0) + 1);
    const { claims, gaps } = assessed(r);
    for (const s of claims) {
      if (s === "supported") t.claimsSupported++;
      else if (s === "unsupported") t.claimsUnsupported++;
      else if (s === "contradicted") t.claimsContradicted++;
    }
    for (const kind of new Set(gaps.map((g) => g.kind)))
      kinds.set(kind, (kinds.get(kind) || 0) + 1);
  }
  return {
    ...t,
    verdicts: VERDICTS.filter((v) => verdicts.has(v)).map((verdict) => ({
      verdict,
      runs: String(verdicts.get(verdict)),
    })),
    assessmentUsdMicros: String(usd),
    gapKinds: GAP_KINDS.filter((k) => kinds.has(k)).map((kind) => ({
      kind,
      runs: kinds.get(kind)!,
    })),
  } satisfies FoundryWorkflowInsightTotals;
}

/** The issues runs named most: their gaps, or their suggestions. */
function recurring(
  runs: readonly InsightRun[],
  of: (a: Assessed) => { kind?: string; text: string }[],
): FoundryWorkflowInsightIssue[] {
  type Group = {
    kind?: string;
    text: string;
    runs: Set<string>;
    last: InsightRun;
    workflows: Set<string>;
  };
  const groups = new Map<string, Group>();
  // Newest first, so each group's text and kind are the newest run's.
  const newest = [...runs].sort(
    (a, b) =>
      b.startedAt.localeCompare(a.startedAt) || b.id.localeCompare(a.id),
  );
  for (const r of newest)
    for (const issue of of(assessed(r))) {
      const key = normalize(issue.text);
      if (!key) continue;
      const g = groups.get(key) || {
        kind: issue.kind,
        text: issue.text,
        runs: new Set(),
        last: r,
        workflows: new Set(),
      };
      g.runs.add(r.id);
      g.workflows.add(r.workflowId || "");
      groups.set(key, g);
    }
  return [...groups.values()]
    .sort(
      (a, b) =>
        b.runs.size - a.runs.size ||
        b.last.startedAt.localeCompare(a.last.startedAt) ||
        a.text.localeCompare(b.text),
    )
    .slice(0, ISSUES)
    .map((g) => ({
      ...(g.kind && { kind: g.kind }),
      text: g.text,
      runs: g.runs.size,
      lastRunId: g.last.id,
      lastWorkflowId: g.last.workflowId || "",
      lastAt: g.last.startedAt,
      workflowIds: [...g.workflows].sort(),
    }));
}

const gapsOf = (a: Assessed) => a.gaps;
const suggestionsOf = (a: Assessed) => a.suggestions.map((text) => ({ text }));

/** The principal who started a run. */
export const starterOf = (r: FoundryWorkflowRun): FoundryPrincipal =>
  r.startedByServiceCredentialId
    ? { kind: "KIND_SERVICE_CREDENTIAL", id: r.startedByServiceCredentialId }
    : { kind: "KIND_USER", id: r.startedByUserId || "" };

/** The UTC day of `at`, YYYY-MM-DD. */
const dayOf = (at: string | number | Date) =>
  new Date(at).toISOString().slice(0, 10);

/**
 * What `runs` of one ontology (or of its workflow `workflowId`) came to over
 * the `days` whole UTC days ending on `now`'s, and the window before.
 */
export function insights(
  runs: readonly InsightRun[],
  opts: { days: number; now: Date; workflowId?: string },
): FoundryWorkflowInsights {
  const days = opts.days;
  const end = Date.UTC(
    opts.now.getUTCFullYear(),
    opts.now.getUTCMonth(),
    opts.now.getUTCDate() + 1,
  );
  const start = end - days * DAY,
    before = start - days * DAY;
  const at = (r: InsightRun) => Date.parse(r.startedAt);
  const inWindow = runs
    .filter((r) => at(r) >= start && at(r) < end)
    .sort((a, b) => b.startedAt.localeCompare(a.startedAt))
    .slice(0, INSIGHT_RUNS);
  const previous = runs.filter((r) => at(r) >= before && at(r) < start);
  const dates = Array.from({ length: days }, (_, i) => dayOf(start + i * DAY));
  const byDay = (rs: readonly InsightRun[]) =>
    dates.map((d) => rs.filter((r) => dayOf(r.startedAt) === d));
  const segment = (
    rs: InsightRun[],
    who: Partial<FoundryWorkflowInsightSegment>,
  ): FoundryWorkflowInsightSegment => ({
    ...who,
    totals: totalsOf(rs),
    dailyRuns: byDay(rs).map((d) => d.length),
    ...(rs[0] && { lastRunAt: rs[0].startedAt }),
    ...(recurring(rs, gapsOf)[0] && {
      topGap: recurring(rs, gapsOf)[0]!.text,
    }),
  });
  const groups = <K>(key: (r: InsightRun) => K) => {
    const out = new Map<K, InsightRun[]>();
    for (const r of inWindow) out.set(key(r), [...(out.get(key(r)) || []), r]);
    return [...out];
  };
  const t = totalsOf(inWindow);
  const finished = inWindow.filter((r) => r.status === "finished"),
    done = finished.filter((r) => r.assessment?.status === "done"),
    used = done.filter((r) => (r.counts?.accesses || 0) > 0);
  return {
    from: dates[0],
    days,
    totals: t,
    previous: totalsOf(previous),
    series: byDay(inWindow).map((rs, i) => ({
      date: dates[i],
      totals: totalsOf(rs),
    })),
    funnel: {
      started: inWindow.length,
      finished: finished.length,
      assessed: done.length,
      used: used.length,
      helped: used.filter((r) => HELPED.has(r.assessment?.verdict || ""))
        .length,
    },
    gaps: recurring(inWindow, gapsOf),
    suggestions: recurring(inWindow, suggestionsOf),
    agents: groups((r) => JSON.stringify(starterOf(r)))
      .map(([k, rs]) => segment(rs, { principal: JSON.parse(k) }))
      .sort((a, b) => (b.totals?.runs || 0) - (a.totals?.runs || 0))
      .slice(0, AGENTS),
    workflows: opts.workflowId
      ? []
      : groups((r) => r.workflowId || "")
          .map(([workflowId, rs]) => segment(rs, { workflowId }))
          .sort((a, b) => (b.totals?.runs || 0) - (a.totals?.runs || 0)),
    truncated:
      runs.filter((r) => at(r) >= start && at(r) < end).length > INSIGHT_RUNS,
  };
}
