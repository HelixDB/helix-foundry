// Area schemas for workspace essentials: home, activity, reviews, settings.
// Owned by the workspace area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index.js" would be circular.
// Type-only imports from "./index.js" are fine.
import { z } from "zod";

export const DashboardSchema = z.object({
  name: z.string(),
  description: z.string().default(""),
  widgets: z
    .array(
      z.object({
        title: z.string(),
        type: z.enum(["metric", "bar", "line", "table"]),
        sql: z.string(),
        inputs: z.array(z.string()).min(1),
        x: z.string().optional(),
        y: z.string().optional(),
      }),
    )
    .min(1)
    .max(12),
});

// Health of a connection or source as the UI shows it.
export const HealthStatusSchema = z.enum([
  "healthy",
  "syncing",
  "failed",
  "paused",
  "stale",
]);
export type HealthStatus = z.infer<typeof HealthStatusSchema>;

const at = z.string().nullable();

export const ActivityKindSchema = z.enum([
  "sync",
  "upload",
  "pipeline",
  "model",
  "job",
  "run",
  "setup",
  "edit",
  "publish",
]);
export type ActivityKind = z.infer<typeof ActivityKindSchema>;
// One entry of the activity feed: the tray, Home and the Activity page all show
// jobs, runs and audits through describeActivity so they read the same.
export const ActivityItemSchema = z.object({
  id: z.string(),
  kind: ActivityKindSchema,
  // What the entry is about ("Users (PlanetScale + WorkOS)"), not the command.
  title: z.string(),
  // Kind label shown beside the time: "Pipeline build", "Setup", "Analyst", ...
  label: z.string(),
  status: z.string(),
  at: z.string(),
  error: z.string().nullable(),
  // Older entries for the same pipeline or source folded into this one.
  repeats: z.number(),
  // Whatever the item links to; the web app turns this into a route.
  ref: z.object({
    sourceId: z.string().optional(),
    datasetId: z.string().optional(),
    pipelineId: z.string().optional(),
    runId: z.string().optional(),
    proposalId: z.string().optional(),
    logicalId: z.string().optional(),
  }),
});
export type ActivityItem = z.infer<typeof ActivityItemSchema>;
// GET /workspaces/:w/activity
export const ActivityPageSchema = z.object({
  items: z.array(ActivityItemSchema),
  total: z.number(),
});

type Describable = {
  id: string;
  kind: string;
  name: string;
  updatedAt: string;
  data: Record<string, any>;
};
const busy = (status: unknown) => status === "queued" || status === "running";
const setupTasks: Record<string, [string, string]> = {
  describe_ontology: ["Describing your ontology", "Described your ontology"],
  recommend_outcomes: ["Suggesting outcomes", "Suggested outcomes"],
  onboarding_build: ["Building your workspace", "Built your workspace"],
  home_metrics: ["Picking your key metrics", "Picked your key metrics"],
};
const providers: Record<string, string> = {
  local: "the local AI model",
  claude: "Claude",
  openai: "OpenAI",
  azure: "Azure OpenAI",
  bedrock: "Amazon Bedrock",
  custom: "your custom AI model",
};
// Describes a job, run or audit resource (or the slim copy the event stream sends).
export function describeActivity(r: Describable): ActivityItem {
  const d = r.data,
    active = busy(d.status);
  const base = {
    id: r.id,
    status: String(d.status || "succeeded"),
    at: r.updatedAt,
    error: typeof d.error === "string" && d.error ? d.error : null,
    repeats: 0,
  };
  if (r.kind === "run")
    return d.task
      ? {
          ...base,
          kind: "setup",
          label: "Setup",
          title: setupTasks[d.task]?.[active ? 0 : 1] || r.name,
          ref: { runId: r.id },
        }
      : {
          ...base,
          kind: "run",
          label: "Analyst",
          title: d.goal || r.name,
          ref: { runId: r.id },
        };
  if (r.kind === "audit") {
    const publish = !!d.proposalId && !d.objectId;
    const logicalId =
      d.evidence?.logicalId ||
      (typeof d.objectId === "string"
        ? d.objectId.slice(d.objectId.indexOf("_") + 1)
        : undefined);
    return {
      ...base,
      status: "succeeded",
      error: null,
      kind: publish ? "publish" : "edit",
      label: publish ? "Publication" : "Object edit",
      title: r.name,
      ref: publish
        ? { proposalId: d.proposalId }
        : logicalId
          ? { logicalId }
          : {},
    };
  }
  const ref = {
    ...(d.sourceId ? { sourceId: d.sourceId } : {}),
    ...(d.datasetId ? { datasetId: d.datasetId } : {}),
    ...(d.pipelineId ? { pipelineId: d.pipelineId } : {}),
  };
  switch (d.type) {
    case "pipeline":
      return {
        ...base,
        ref,
        kind: "pipeline",
        label: "Pipeline build",
        title: r.name.replace(/^Run /, ""),
      };
    case "sync":
      return {
        ...base,
        ref,
        kind: "sync",
        label: "Sync",
        title: r.name.replace(/^Sync /, ""),
      };
    case "upload":
      return { ...base, ref, kind: "upload", label: "Upload", title: r.name };
    case "provider_prepare": {
      const p = providers[d.settings?.provider] || "the AI model";
      return {
        ...base,
        ref,
        kind: "model",
        label: "AI model",
        title: active
          ? `Preparing ${p}`
          : d.status === "failed"
            ? `Couldn’t prepare ${p}`
            : `Prepared ${p}`,
      };
    }
    case "cleanup":
      return {
        ...base,
        ref,
        kind: "job",
        label: "Storage",
        title: active
          ? "Removing old workspace builds"
          : d.status === "failed"
            ? "Couldn’t remove old workspace builds"
            : "Removed old workspace builds",
      };
    // Home's background check for business shifts.
    case "home_insights":
      return {
        ...base,
        ref,
        kind: "job",
        label: "Home",
        title: active
          ? "Looking for business shifts"
          : d.status === "failed"
            ? "Couldn’t check for business shifts"
            : "Checked for business shifts",
      };
    default:
      return { ...base, ref, kind: "job", label: "Job", title: r.name };
  }
}
const repeatKey = (a: ActivityItem) =>
  a.kind === "pipeline"
    ? "pipeline:" + a.title
    : a.kind === "sync"
      ? "sync:" + (a.ref.sourceId || a.title)
      : a.kind === "job" && a.label === "Home"
        ? "home"
        : null;
// Folds repeated builds of one pipeline (by name: ids change on publish),
// syncs of one source or Home's checks into the newest entry. Items must be
// newest first.
// "adjacent" only folds back-to-back repeats, keeping a timeline's history.
export function collapseActivity(
  items: ActivityItem[],
  mode: "all" | "adjacent" = "all",
) {
  const out: ActivityItem[] = [],
    seen = new Map<string, ActivityItem>();
  for (const item of items) {
    const key = repeatKey(item),
      last = out.at(-1);
    const into = !key
      ? undefined
      : mode === "all"
        ? seen.get(key)
        : last && repeatKey(last) === key
          ? last
          : undefined;
    if (into) {
      into.repeats += 1 + item.repeats;
      continue;
    }
    const copy = { ...item };
    out.push(copy);
    if (key) seen.set(key, copy);
  }
  return out;
}

// GET /workspaces/:w/home: one small summary instead of full resource lists.
export const HomeSummarySchema = z.object({
  workspace: z.object({
    id: z.string(),
    name: z.string(),
    activeGeneration: z.string().nullable(),
    publishedAt: at,
  }),
  counts: z.object({
    datasets: z.number(),
    sources: z.number(),
    pipelines: z.number(),
    objectTypes: z.number(),
    objects: z.number(),
    dashboards: z.number(),
  }),
  // Sources grouped per logical connection; sample data and files count as one each.
  connections: z.array(
    z.object({
      id: z.string(),
      name: z.string(),
      // Connector key for SourceLogo: stripe, postgres, neon, upload, ...
      connector: z.string(),
      status: HealthStatusSchema,
      sources: z.number(),
      datasets: z.number(),
      lastSyncAt: at,
      error: z.string().nullable(),
      // A source to open, when the connection has one.
      sourceId: z.string().nullable(),
      sample: z.boolean(),
    }),
  ),
  pipelines: z.object({
    total: z.number(),
    running: z.number(),
    // Published pipelines whose latest build failed.
    failed: z.number(),
    failing: z.array(
      z.object({
        pipelineId: z.string(),
        name: z.string(),
        jobId: z.string(),
        at: z.string(),
        error: z.string().nullable(),
      }),
    ),
    lastRun: z
      .object({
        jobId: z.string(),
        pipelineId: z.string().nullable(),
        name: z.string(),
        status: z.string(),
        at: z.string(),
        error: z.string().nullable(),
      })
      .nullable(),
  }),
  findings: z.object({
    open: z.number(),
    items: z.array(
      z.object({
        id: z.string(),
        name: z.string(),
        kind: z.string(),
        at: z.string(),
        datasetId: z.string().nullable(),
        runId: z.string().nullable(),
      }),
    ),
  }),
  proposals: z.object({
    pending: z.number(),
    items: z.array(
      z.object({ id: z.string(), name: z.string(), at: z.string() }),
    ),
  }),
  ontology: z.object({
    types: z.array(
      z.object({
        id: z.string(),
        typeId: z.string(),
        name: z.string(),
        objects: z.number(),
        properties: z.number(),
        datasetId: z.string(),
      }),
    ),
    links: z.array(
      z.object({ name: z.string(), from: z.string(), to: z.string() }),
    ),
  }),
  // Newest first, with repeated builds or syncs folded (see collapseActivity).
  activity: z.array(ActivityItemSchema),
  dashboards: z.array(
    z.object({
      id: z.string(),
      name: z.string(),
      description: z.string(),
      widgets: z.number(),
    }),
  ),
});
export type HomeSummary = z.infer<typeof HomeSummarySchema>;

// Home metrics: the growth and analytics numbers on Home. A spec names a
// dataset and its columns; code compiles it to SQL, so the AI picks metrics
// once and they re-run without it.
export const MetricSpecSchema = z.object({
  title: z.string().min(1).max(60),
  why: z.string().max(200).default(""),
  // kpi: one number (with timeColumn, the last 30 days against the 30 before);
  // trend: a value over time (needs timeColumn); breakdown: by category (groupBy).
  kind: z.enum(["kpi", "trend", "breakdown"]),
  datasetId: z.string(),
  measure: z.enum(["count", "count_distinct", "sum", "avg"]),
  column: z.string().optional(),
  timeColumn: z.string().optional(),
  groupBy: z.string().optional(),
  filter: z.object({ column: z.string(), value: z.string() }).optional(),
  format: z.enum(["number", "currency", "percent"]).default("number"),
  // "cents" for amounts stored in minor units (e.g. Stripe).
  scale: z.enum(["none", "cents"]).default("none"),
});
export type MetricSpec = z.infer<typeof MetricSpecSchema>;
export const MetricPickSchema = z.object({
  metrics: z.array(MetricSpecSchema).min(1).max(8),
});
export type HomeMetric = {
  id: string;
  title: string;
  why: string;
  kind: MetricSpec["kind"];
  format: MetricSpec["format"];
  dataset: string;
  // A kpi over a time column compares the last 30 days of data with the 30 before.
  window?: "30d";
  grain?: "day" | "week" | "month";
  result: {
    value?: number | null;
    previous?: number | null;
    asOf?: string;
    points?: { label: string; value: number }[];
    error?: string;
  };
};
export type HomeMetrics = {
  status: "ready" | "empty";
  // Who picked the metrics: the AI from the schema, or code as a stand-in.
  source: "ai" | "computed";
  aiReady: boolean;
  generating: boolean;
  error?: string;
  generatedAt?: string;
  computedAt?: string;
  metrics: HomeMetric[];
};

// Home's "Some things we noticed": business shifts beneath headline totals.
// Code picks at most three business drivers from the accepted ontology, runs
// up to four checks within them off the request path, and keeps at most three
// findings across all drivers. Everything shown is computed; nothing is
// written by a model.
export const insightChecks = ["cohort", "mix", "unit", "conversion"] as const;
export type InsightCheck = (typeof insightChecks)[number];
export type InsightSeverity = "positive" | "warning" | "issue";
// The scan's major steps, in order.
export const insightStages = ["understand", "fundamentals", "shifts"] as const;
export type InsightStage = (typeof insightStages)[number];
export type InsightDriverKind =
  | "revenue"
  | "customers"
  | "acquisition"
  | "experience"
  | "activity";
// A candidate business priority: one activity of the ontology seen through
// one lens (revenue, customer engagement, acquisition, experience, volume).
export type InsightDriver = {
  id: string;
  kind: InsightDriverKind;
  label: string;
  // The ontology type whose records are the activity ("Order").
  activity: string;
  // What is measured: "Revenue (net_amount)", "Sessions".
  measure: string;
  // Why it was proposed, in plain words.
  reasons: string[];
};
// Whole UTC days; end is exclusive.
export type InsightPeriod = { start: string; end: string; label: string };
export type HomeInsight = {
  // Stable across scans for the same shift; version changes when its
  // severity or size materially changes.
  id: string;
  version: string;
  severity: InsightSeverity;
  check: InsightCheck;
  driver: { id: string; label: string };
  headline: string;
  // Three or four words, the figure set apart so it can be bold; it leads
  // ("12,500 referral sessions") or follows ("Regular sessions ↓25.3%").
  support: { text: string; figure: string; figureFirst?: boolean };
  values: {
    current: number;
    baseline: number;
    absolute: number;
    relative: number | null;
    unit: "number" | "currency" | "share" | "rate";
  };
  periods: {
    current: InsightPeriod;
    baseline: InsightPeriod;
    cohort?: InsightPeriod;
  };
  details: {
    why: string;
    contribution: string;
    comparison: string;
    definition: string;
    evidence: string;
    limitations: string[];
    // Previous and latest share, when the finding is about a share.
    bars?: { label: string; share: number; text: string; previous: boolean }[];
    table: { caption: string; columns: string[]; rows: string[][] };
    // The bounded aggregate the figures came from.
    query: string;
    sources: { datasetId: string; name: string }[];
  };
  // A follow-up question for the Analyst, built from the computed figures.
  ask: string;
  observedAt: string;
  // Set when a finding this person dismissed is back, saying why.
  returned?: string;
  impact: number;
  strength: number;
};
export type InsightCoverage = {
  driver: string;
  label: string;
  checks: {
    check: InsightCheck;
    label: string;
    // found: a shift is among the findings or was outranked; steady: checked
    // and nothing material; skipped: the data cannot support it; set_aside:
    // the change looks like a data-quality change, not a business one.
    state: "found" | "steady" | "skipped" | "set_aside";
    note: string;
  }[];
};
export type HomeInsights = {
  // unsupported: no accepted ontology, or no activity with dates to check.
  status: "unsupported" | "ready";
  scan: {
    state: "idle" | "queued" | "running" | "failed";
    stage?: InsightStage;
    steps: {
      id: InsightStage;
      label: string;
      detail: string;
      state: "done" | "active" | "waiting";
    }[];
    error?: string;
  };
  // When the shown findings were last computed successfully.
  checkedAt?: string;
  // The data, ontology or settings changed since checkedAt.
  stale: boolean;
  period?: { current: InsightPeriod; baseline: InsightPeriod };
  priorities: {
    selected: InsightDriver[];
    candidates: InsightDriver[];
    // Several drivers are about as well supported; an owner may confirm.
    ambiguous: boolean;
    confirmed: boolean;
    // Drivers the ontology suggests but the data cannot support yet, and why.
    unavailable: { id: string; label: string; reason: string }[];
  };
  coverage: InsightCoverage[];
  // At most three, minus the ones this person dismissed.
  findings: HomeInsight[];
  dismissed: number;
};
// PUT /home/insights/priorities: up to three candidate driver ids, or none to
// go back to the automatic choice.
export const InsightPrioritiesSchema = z.object({
  drivers: z.array(z.string().min(1).max(200)).max(3),
});
// POST /home/insights/:id/dismiss
export const InsightDismissSchema = z.object({
  version: z.string().min(1).max(100),
});
// PUT /home/insights/settings: thresholds; anything left out keeps its default.
export const InsightThresholdsSchema = z
  .object({
    // Activities each compared period needs.
    minPeriodEvents: z.number().int().min(5).max(1_000_000),
    // Participants a fixed cohort needs, and the activities in the cohort
    // window that make a participant part of it.
    minCohortSize: z.number().int().min(3).max(1_000_000),
    cohortMinActivities: z.number().int().min(1).max(1000),
    // Activities a segment needs, across both periods, to be compared.
    minSegmentEvents: z.number().int().min(5).max(1_000_000),
    // Smallest relative change, share change (fraction), concentration
    // change (fraction) and conversion change (relative) that count.
    relativeChange: z.number().min(0.01).max(5),
    shareChange: z.number().min(0.005).max(1),
    concentrationChange: z.number().min(0.005).max(1),
    rateChange: z.number().min(0.01).max(5),
    // Smallest effect on the driver's total, as a fraction of it.
    minImpact: z.number().min(0).max(1),
    // Smallest test statistic (|z|) that counts as evidence.
    minZ: z.number().min(0).max(10),
    // A decline at least this large (relative) is an Issue, not a Warning.
    issueRelative: z.number().min(0.01).max(5),
    concentrationTop: z.number().int().min(1).max(100),
    // Days at the end of each period left out of conversion, so every
    // counted activity had time to convert.
    conversionLagDays: z.number().int().min(0).max(6),
  })
  .partial()
  .strict();
export type InsightThresholds = Required<
  z.infer<typeof InsightThresholdsSchema>
>;
// GET /workspaces/:w/tokens: one workspace API token, without its secret.
export type ApiTokenMetadata = {
  id: string;
  name: string;
  readOnly: boolean;
  createdAt: string;
};
