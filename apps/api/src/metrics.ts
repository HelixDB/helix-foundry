// Home metrics. The AI picks growth and analytics metrics once from the
// datasets' schemas (columns, types and category values, never rows) as
// specs; code compiles each spec to DuckDB SQL, keeps the ones that run, and
// stores them. Home re-runs the stored queries, without the AI, whenever an
// input's data version changes. Until the AI has picked (or without AI),
// metrics chosen by code from the schema stand in.
import {
  datasetTable,
  type HomeMetric,
  type HomeMetrics,
  type MetricSpec,
  type Resource,
} from "../../../packages/shared/src/index.js";
import { z } from "zod";
import { queryDatasets } from "./datasets.js";
import { lit, plural, q } from "./discovery.js";
import { digest } from "./security.js";
import { resource, transaction, type Store } from "./store.js";

export const METRICS_ID = "home_metrics";
// Bumped when compiling or normalising changes: stored specs are then
// recompiled and re-run by code, without asking the AI again.
export const METRICS_VERSION = 3;
type Column = {
  name: string;
  type: string;
  nulls?: number;
  distinct?: number;
  sensitive?: boolean;
  primaryKey?: boolean;
};
export type Compiled = {
  id: string;
  spec: MetricSpec & { grain?: "day" | "week" | "month" };
  sql: string;
};
type Stored = {
  signature: string;
  source: "ai" | "computed";
  generatedAt: string;
  metrics: Compiled[];
  results: {
    versions: Record<string, string>;
    computedAt: string;
    values: Record<string, HomeMetric["result"]>;
  };
  // The AI is asked once per schema; a new schema or Regenerate asks again.
  aiSignature?: string;
  runId?: string;
  version?: number;
};

const temporal = (t: string) => /^(DATE|TIMESTAMP)/i.test(t);
const numericType = (t: string) =>
  /^(TINYINT|SMALLINT|INTEGER|BIGINT|HUGEINT|UTINYINT|USMALLINT|UINTEGER|UBIGINT|FLOAT|REAL|DOUBLE|DECIMAL|NUMERIC)/i.test(
    t,
  );
const nested = (t: string) => /STRUCT|MAP\(|\[\]|UNION\(|^LIST|^JSON/i.test(t);
const categorical = (c: Column) =>
  !c.sensitive &&
  !c.primaryKey &&
  !nested(c.type) &&
  /^(VARCHAR|BOOLEAN|ENUM)/i.test(c.type) &&
  (c.distinct ?? 0) >= 1 &&
  (c.distinct ?? 99) <= 50;
const blank = (v: unknown) =>
  v == null || ["", "None", "null", "NaN"].includes(String(v).trim());

// Datasets Home measures: the current ones, including pipeline outputs, minus
// replaced samples and data left out of setup.
export async function metricsDatasets(store: Store, scope: string) {
  const w = await store.get(scope, "workspace"),
    generation = w?.data.activeGeneration,
    excluded = new Set<string>(w?.data.onboarding?.excludedIds || []);
  return (await store.list(scope, "dataset"))
    .filter(
      (d) =>
        (!d.data.generation || d.data.generation === generation) &&
        !d.data.retiredSample &&
        !excluded.has(d.id) &&
        !excluded.has(d.data.sourceId) &&
        !excluded.has(d.data.importJobId) &&
        d.data.activeVersion &&
        d.data.profile?.columns?.length,
    )
    .sort((a, b) => a.id.localeCompare(b.id));
}
const columnsOf = (d: Resource): Column[] => d.data.profile?.columns || [];
// Changes when a dataset or a column's name or type does, not with new rows.
export const schemaSignature = (datasets: Resource[]) =>
  digest(
    JSON.stringify(
      datasets.map((d) => [
        d.id,
        columnsOf(d).map((c) => c.name + " " + c.type),
      ]),
    ),
  );
// A dataset's everyday name: "Users (PlanetScale + WorkOS)" -> "Users",
// "Stripe · invoices" -> "Invoices".
export function noun(d: Resource) {
  const n = d.name
    .split(" · ")
    .at(-1)!
    .replace(/\s*\(.*\)\s*$/, "")
    .replace(/\.(csv|parquet|json)$/i, "")
    .replace(/[_-]+/g, " ")
    .trim();
  return n.charAt(0).toUpperCase() + n.slice(1);
}

// What the AI sees: one line per column with its type, and the values of
// low-cardinality text columns (from the stored profile sample, so no query
// runs), so filters and breakdowns use real values.
export function describeForMetrics(datasets: Resource[]) {
  return datasets.map((d) => {
    const sample: Record<string, unknown>[] = d.data.profile?.sample || [];
    return {
      id: d.id,
      name: d.name,
      rows: d.data.profile?.rows,
      merged: !!d.data.generation || undefined,
      columns: columnsOf(d)
        .filter((c) => !nested(c.type))
        .map((c) => {
          const values =
            categorical(c) && (c.distinct ?? 99) <= 12
              ? [
                  ...new Set(
                    sample
                      .map((r) => r[c.name])
                      .filter((v) => !blank(v))
                      .map(String),
                  ),
                ].slice(0, 12)
              : [];
          return [
            c.name,
            c.type,
            c.primaryKey ? "unique" : "",
            c.nulls ? `${c.nulls} null` : "",
            values.length ? "values: " + values.join("|") : "",
          ]
            .filter(Boolean)
            .join(" ");
        }),
    };
  });
}

// The AI's reply format, one option per dataset listing only that dataset's
// columns: numeric ones to sum, dates to trend over, categories to break down
// by, and each category's real values to filter on. Constrained decoding then
// can't pair a dataset with another's columns or invent a value.
export function metricPickSchema(datasets: Resource[]) {
  const names = (cols: Column[]) =>
    cols.map((c) => c.name) as [string, ...string[]];
  const variants = datasets.map((d) => {
    const cols = columnsOf(d).filter((c) => !nested(c.type) && !c.sensitive),
      numeric = cols.filter((c) => numericType(c.type) && !c.primaryKey),
      times = cols.filter((c) => temporal(c.type)),
      groups = cols.filter((c) => categorical(c) && (c.distinct ?? 99) <= 12),
      filters = groups
        .map((c) => ({ c, values: observed(d, c.name) }))
        .filter((f) => f.values.length);
    const shape: Record<string, z.ZodType> = {
      title: z.string().max(60),
      why: z.string().max(200),
      kind: z.enum(
        times.length
          ? groups.length
            ? ["kpi", "trend", "breakdown"]
            : ["kpi", "trend"]
          : groups.length
            ? ["kpi", "breakdown"]
            : ["kpi"],
      ),
      dataset: z.literal(d.id),
      measure: z.enum(
        numeric.length
          ? ["count", "count_distinct", "sum", "avg"]
          : ["count", "count_distinct"],
      ),
      format: z.enum(["number", "currency", "percent"]),
      scale: z.enum(["none", "cents"]),
    };
    if (cols.length) shape.column = z.enum(names(cols)).optional();
    if (times.length) shape.timeColumn = z.enum(names(times)).optional();
    if (groups.length) shape.groupBy = z.enum(names(groups)).optional();
    if (filters.length)
      shape.filter = (
        filters.length === 1
          ? z.object({
              column: z.literal(filters[0].c.name),
              value: z.enum(filters[0].values as [string, ...string[]]),
            })
          : z.union(
              filters.map((f) =>
                z.object({
                  column: z.literal(f.c.name),
                  value: z.enum(f.values as [string, ...string[]]),
                }),
              ) as unknown as [z.ZodObject, z.ZodObject, ...z.ZodObject[]],
            )
      ).optional();
    return z.object(shape);
  });
  return z.object({
    metrics: z
      .array(
        variants.length === 1
          ? variants[0]
          : z.union(
              variants as unknown as [
                z.ZodObject,
                z.ZodObject,
                ...z.ZodObject[],
              ],
            ),
      )
      .min(1)
      .max(8),
  });
}
// How a reply is read: the same fields with columns as plain text, so with a
// provider that doesn't enforce the format, one bad metric is dropped on its
// own rather than failing the whole reply.
export const MetricReplySchema = z.object({
  metrics: z
    .array(
      z.object({
        title: z.string().max(60),
        why: z.string().max(200).default(""),
        kind: z.enum(["kpi", "trend", "breakdown"]),
        dataset: z.string(),
        measure: z.enum(["count", "count_distinct", "sum", "avg"]),
        column: z.string().optional(),
        timeColumn: z.string().optional(),
        groupBy: z.string().optional(),
        filter: z.object({ column: z.string(), value: z.string() }).optional(),
        format: z.enum(["number", "currency", "percent"]).default("number"),
        scale: z.enum(["none", "cents"]).default("none"),
      }),
    )
    .min(1)
    .max(8),
});
// The reply back as specs.
export const specsFromPick = (reply: { metrics: any[] }): MetricSpec[] =>
  reply.metrics.map(({ dataset, ...m }) => ({ ...m, datasetId: dataset }));

// Compiles a spec to one SELECT, or says why it can't be run.
export function compileMetric(
  spec: MetricSpec & { grain?: "day" | "week" | "month" },
  dataset: Resource | undefined,
): { sql: string } | { error: string } {
  if (!dataset) return { error: "unknown dataset" };
  const cols = new Map(columnsOf(dataset).map((c) => [c.name, c]));
  const col = (name?: string) => (name ? cols.get(name) : undefined);
  const t = q(datasetTable(dataset.id));
  const measured = col(spec.column);
  if (spec.measure === "sum" || spec.measure === "avg") {
    if (!measured || !numericType(measured.type) || measured.sensitive)
      return { error: `${spec.column} is not a numeric column` };
  } else if (spec.measure === "count_distinct") {
    if (!measured || nested(measured.type))
      return { error: `${spec.column} is not a column` };
  }
  const scale = spec.scale === "cents" ? " / 100.0" : "";
  const measure =
    spec.measure === "count"
      ? "count(*)"
      : spec.measure === "count_distinct"
        ? `count(DISTINCT ${q(measured!.name)})`
        : `${spec.measure}(TRY_CAST(${q(measured!.name)} AS DOUBLE))${scale}`;
  const filterCol = spec.filter && col(spec.filter.column);
  if (spec.filter && (!filterCol || nested(filterCol.type)))
    return { error: `${spec.filter.column} can't be filtered` };
  const where = filterCol
    ? `lower(CAST(${q(filterCol.name)} AS VARCHAR)) = lower(${lit(spec.filter!.value)})`
    : "";
  const time = col(spec.timeColumn);
  if (spec.timeColumn && (!time || !temporal(time.type)))
    return { error: `${spec.timeColumn} is not a date or time column` };
  if (spec.kind === "trend") {
    if (!time) return { error: "a trend needs a date or time column" };
    // Every period between the first and last, so a quiet month shows as 0
    // for a count or sum instead of the line skipping it (an average stays
    // empty: there is nothing to average).
    const grain = spec.grain || "month",
      bucket = `date_trunc('${grain}', CAST(${q(time.name)} AS TIMESTAMP))`,
      label = grain === "month" ? "%Y-%m" : "%Y-%m-%d",
      value = spec.measure === "avg" ? `"__v"` : `COALESCE("__v", 0)`;
    return {
      sql: `WITH d AS (SELECT ${bucket} AS "__b", ${measure} AS "__v" FROM ${t} WHERE ${q(time.name)} IS NOT NULL${where ? " AND " + where : ""} GROUP BY 1), s AS (SELECT unnest(generate_series((SELECT min("__b") FROM d), (SELECT max("__b") FROM d), INTERVAL 1 ${grain.toUpperCase()})) AS "__b") SELECT * FROM (SELECT strftime(s."__b", '${label}') AS "label", CAST(${value} AS DOUBLE) AS "value" FROM s LEFT JOIN d ON s."__b" = d."__b" ORDER BY 1 DESC LIMIT 24) ORDER BY "label"`,
    };
  }
  if (spec.kind === "breakdown") {
    const g = col(spec.groupBy);
    if (!g || !categorical(g))
      return { error: `${spec.groupBy} is not a category column` };
    return {
      sql: `SELECT COALESCE(CAST(${q(g.name)} AS VARCHAR), '(none)') AS "label", CAST(${measure} AS DOUBLE) AS "value" FROM ${t}${where ? " WHERE " + where : ""} GROUP BY 1 ORDER BY 2 DESC NULLS LAST, 1 LIMIT 8`,
    };
  }
  if (!time)
    return {
      sql: `SELECT CAST(${measure} AS DOUBLE) AS "value" FROM ${t}${where ? " WHERE " + where : ""}`,
    };
  // The last 30 days of data against the 30 before, anchored on the table's
  // own latest date (so a snapshot that stopped updating still compares like
  // for like, and every metric on a table uses the same window).
  const filtered = (window: string) =>
    measure.replace(/\)(\s*\/ 100\.0)?$/, ` ) FILTER (WHERE ${window})$1`);
  return {
    sql: `WITH e AS (SELECT max(CAST(${q(time.name)} AS TIMESTAMP)) AS "__end" FROM ${t}), b AS (SELECT *, CAST(${q(time.name)} AS TIMESTAMP) AS "__ts" FROM ${t} WHERE ${q(time.name)} IS NOT NULL${where ? " AND " + where : ""}) SELECT CAST(${filtered(`"__ts" > "__end" - INTERVAL 30 DAY`)} AS DOUBLE) AS "value", CAST(${filtered(`"__ts" > "__end" - INTERVAL 60 DAY AND "__ts" <= "__end" - INTERVAL 30 DAY`)} AS DOUBLE) AS "previous", strftime("__end", '%Y-%m-%d') AS "asOf" FROM e LEFT JOIN b ON true GROUP BY "__end"`,
  };
}

const num = (v: unknown) =>
  v == null || v === "" ? null : Number.isFinite(Number(v)) ? Number(v) : null;
async function run(
  store: Store,
  scope: string,
  m: Compiled,
): Promise<HomeMetric["result"]> {
  try {
    const r = await queryDatasets(store, scope, [m.spec.datasetId], m.sql, {
      limit: 100,
    });
    if (m.spec.kind === "kpi")
      return {
        value: num(r.rows[0]?.value),
        previous: m.spec.timeColumn ? num(r.rows[0]?.previous) : undefined,
        asOf: r.rows[0]?.asOf ?? undefined,
      };
    return {
      points: r.rows
        .map((x: any) => ({ label: String(x.label), value: num(x.value) }))
        .filter((p: any) => p.value !== null) as {
        label: string;
        value: number;
      }[],
    };
  } catch (e) {
    return { error: (e as Error).message.slice(0, 300) };
  }
}
const usable = (m: Compiled, r: HomeMetric["result"]) =>
  !r.error &&
  (m.spec.kind === "kpi"
    ? r.value != null
    : (r.points?.length || 0) >= (m.spec.kind === "trend" ? 3 : 2));

const byName = (cols: Column[], name?: string) =>
  name
    ? cols.find((c) => c.name === name) ||
      cols.find((c) => c.name.toLowerCase() === name.toLowerCase())
    : undefined;
const createdColumn = (cols: Column[]) =>
  cols.find(
    (c) =>
      temporal(c.type) &&
      /creat|sign.?up|joined|start|timestamp/i.test(c.name) &&
      !/update|end|cancel|expire/i.test(c.name),
  );
const moneyPatterns = [
  /^(amount_paid|revenue|mrr|arr)$/i,
  /^(amount|total|price)$/i,
  /^(spend|spend_usd|cost)$/i,
];
const moneyColumn = (cols: Column[]) =>
  moneyPatterns
    .map((p) =>
      cols.find((c) => numericType(c.type) && !c.sensitive && p.test(c.name)),
    )
    .find(Boolean);
const observed = (d: Resource, column: string) => [
  ...new Set(
    ((d.data.profile?.sample || []) as Record<string, unknown>[])
      .map((r) => r[column])
      .filter((v) => !blank(v))
      .map(String),
  ),
];

// Small models fill in every field. Keep what the metric's kind needs, repair
// what code can (a column's case, a count with no column, a sum given no
// column in a table with one money column), and give up on a metric whose
// filter doesn't exist in the data: the filter is what it means.
export function normalizeSpec(
  spec: MetricSpec,
  d: Resource | undefined,
): MetricSpec | undefined {
  if (!d) return undefined;
  const cols = columnsOf(d),
    out: MetricSpec = { ...spec };
  if (out.measure === "count") delete out.column;
  else if (out.measure === "count_distinct") {
    const c = byName(cols, out.column);
    if (c && !nested(c.type)) out.column = c.name;
    else {
      out.measure = "count";
      delete out.column;
    }
  } else {
    const named = byName(cols, out.column);
    const c =
      named && numericType(named.type) && !named.sensitive
        ? named
        : moneyColumn(cols);
    if (!c) return undefined;
    out.column = c.name;
  }
  if (out.measure === "count" || out.measure === "count_distinct") {
    out.format = "number";
    out.scale = "none";
  }
  const time = byName(cols, out.timeColumn);
  // A standing total ("Active subscriptions") is not a count of things added
  // in the last 30 days, whatever time column came with it.
  if (
    out.kind === "kpi" &&
    /^(active|total|current|all|open)\b/i.test(out.title)
  )
    delete out.timeColumn;
  else if (time && temporal(time.type)) out.timeColumn = time.name;
  else if (out.kind === "trend") {
    const c = createdColumn(cols);
    if (!c) return undefined;
    out.timeColumn = c.name;
  } else delete out.timeColumn;
  if (out.kind === "breakdown") {
    const g = byName(cols, out.groupBy);
    if (!g || !categorical(g)) return undefined;
    out.groupBy = g.name;
  } else delete out.groupBy;
  if (out.filter) {
    const f = byName(cols, out.filter.column);
    if (!f || nested(f.type) || f.sensitive) return undefined;
    const values = observed(d, f.name),
      match = values.find(
        (v) => v.toLowerCase() === out.filter!.value.toLowerCase(),
      );
    if (values.length && !match && (f.distinct ?? 99) <= 12) return undefined;
    out.filter = { column: f.name, value: match ?? out.filter.value };
  }
  return out;
}

// Compiles and runs specs, keeping those that give a usable answer; nothing
// goes back to the AI. A trend with too few months is tried weekly, then daily.
export async function validateSpecs(
  store: Store,
  scope: string,
  specs: MetricSpec[],
  datasets: Resource[],
) {
  const byId = new Map(datasets.map((d) => [d.id, d]));
  const seen = new Set<string>(),
    kept: { metric: Compiled; result: HomeMetric["result"] }[] = [];
  let dropped = 0;
  const tried = await Promise.all(
    specs.map(async (spec, i) => {
      const fixed = normalizeSpec(spec, byId.get(spec.datasetId));
      if (!fixed) return undefined;
      const grains =
        fixed.kind === "trend"
          ? (["month", "week", "day"] as const)
          : [undefined];
      for (const grain of grains) {
        const s = grain ? { ...fixed, grain } : fixed;
        const c = compileMetric(s, byId.get(spec.datasetId));
        if ("error" in c) return undefined;
        const metric = {
          id: `m${i}_${digest(c.sql).slice(0, 8)}`,
          spec: s,
          sql: c.sql,
        };
        const result = await run(store, scope, metric);
        if (usable(metric, result)) return { metric, result };
        if (result.error || fixed.kind !== "trend") return undefined;
      }
      return undefined;
    }),
  );
  for (const t of tried) {
    if (!t || seen.has(t.metric.sql)) {
      dropped++;
      continue;
    }
    seen.add(t.metric.sql);
    kept.push(t);
  }
  return { kept, dropped };
}

// Stand-in metrics chosen by code from the schema: growth of the largest
// record types, money, and a breakdown or two by status-like columns.
export function computedSpecs(datasets: Resource[]): MetricSpec[] {
  const specs: MetricSpec[] = [];
  const created = (d: Resource) =>
    columnsOf(d).find(
      (c) =>
        temporal(c.type) &&
        /creat|sign.?up|joined|start|timestamp/i.test(c.name) &&
        !/update|end|cancel|expire/i.test(c.name),
    );
  const recordName = (d: Resource) => plural(noun(d).replace(/s$/, ""));
  // Merged pipeline outputs first: they are the records, not their parts. One
  // dataset per kind of record, so the same users from two sources don't make
  // two "New users" cards.
  const records = [...datasets]
    .filter((d) => columnsOf(d).some((c) => c.primaryKey) && created(d))
    .sort(
      (a, b) =>
        Number(!!b.data.generation) - Number(!!a.data.generation) ||
        (b.data.profile?.rows || 0) - (a.data.profile?.rows || 0),
    )
    .filter(
      (d, i, all) =>
        all.findIndex(
          (x) => recordName(x).toLowerCase() === recordName(d).toLowerCase(),
        ) === i,
    );
  for (const d of records.slice(0, 3)) {
    const name = recordName(d),
      time = created(d)!.name;
    specs.push({
      title: `New ${name.toLowerCase()}`,
      why: `${name} added in the last 30 days of data, against the 30 before.`,
      kind: "kpi",
      datasetId: d.id,
      measure: "count",
      timeColumn: time,
      format: "number",
      scale: "none",
    });
    if (specs.filter((s) => s.kind === "trend").length < 2)
      specs.push({
        title: `New ${name.toLowerCase()} over time`,
        why: `How quickly ${name.toLowerCase()} are being added.`,
        kind: "trend",
        datasetId: d.id,
        measure: "count",
        timeColumn: time,
        format: "number",
        scale: "none",
      });
  }
  // Revenue before spend: the first money column by preference, anywhere.
  const moneyNames = [
    /^(amount_paid|revenue|mrr|arr)$/i,
    /^(amount|total|price)$/i,
    /^(spend|spend_usd|cost)$/i,
  ];
  const money = moneyNames
    .flatMap((pattern) =>
      datasets.flatMap((d) =>
        columnsOf(d)
          .filter(
            (c) => numericType(c.type) && !c.sensitive && pattern.test(c.name),
          )
          .map((c) => ({ d, c })),
      ),
    )
    .at(0);
  if (money)
    specs.push({
      title: /spend|cost/i.test(money.c.name) ? "Spend" : "Revenue",
      why: `Total ${money.c.name.replace(/_/g, " ")} in ${noun(money.d).toLowerCase()}.`,
      kind: "kpi",
      datasetId: money.d.id,
      measure: "sum",
      column: money.c.name,
      timeColumn: created(money.d)?.name,
      format: "currency",
      scale: /^amount/i.test(money.c.name) ? "cents" : "none",
    });
  let breakdowns = 0;
  for (const d of records.length ? records : datasets)
    for (const c of columnsOf(d)) {
      if (breakdowns >= 2) break;
      if (
        categorical(c) &&
        (c.distinct ?? 0) >= 2 &&
        (c.distinct ?? 99) <= 12 &&
        /^(status|plan|tier|channel|category|type|role|priority|state)$/i.test(
          c.name,
        )
      ) {
        breakdowns++;
        specs.push({
          title: `${noun(d)} by ${c.name.replace(/_/g, " ")}`,
          why: `How ${noun(d).toLowerCase()} split by ${c.name.replace(/_/g, " ")}.`,
          kind: "breakdown",
          datasetId: d.id,
          measure: "count",
          groupBy: c.name,
          format: "number",
          scale: "none",
        });
      }
    }
  return specs.slice(0, 8);
}

// The AI's metrics first, then code-picked ones that measure something else,
// up to eight: a weak AI answer never leaves Home with less than before.
export function mergePicks(
  ai: Awaited<ReturnType<typeof validateSpecs>>,
  fill: Awaited<ReturnType<typeof validateSpecs>>["kept"],
) {
  const shape = (m: Compiled) =>
    [
      m.spec.datasetId,
      m.spec.kind,
      m.spec.measure,
      m.spec.column,
      m.spec.groupBy,
    ]
      .map(String)
      .join("|");
  const sqls = new Set(ai.kept.map((k) => k.metric.sql)),
    shapes = new Set(ai.kept.map((k) => shape(k.metric)));
  const extra = fill.filter(
    (k) => !sqls.has(k.metric.sql) && !shapes.has(shape(k.metric)),
  );
  return { kept: [...ai.kept, ...extra].slice(0, 8), dropped: ai.dropped };
}

const versionsOf = (metrics: Compiled[], datasets: Resource[]) => {
  const byId = new Map(datasets.map((d) => [d.id, d]));
  return Object.fromEntries(
    [...new Set(metrics.map((m) => m.spec.datasetId))].map((id) => [
      id,
      String(byId.get(id)?.data.activeVersion || ""),
    ]),
  );
};
// Stores picked metrics with the results that validated them.
export async function saveMetrics(
  store: Store,
  scope: string,
  signature: string,
  source: Stored["source"],
  picked: Awaited<ReturnType<typeof validateSpecs>>,
  datasets: Resource[],
  extra: Partial<Stored> = {},
) {
  const metrics = picked.kept.map((k) => k.metric);
  return transaction(store, scope, async (tx) => {
    const previous = await tx.get(METRICS_ID);
    const data: Stored = {
      ...(previous?.data as Stored | undefined),
      ...extra,
      version: METRICS_VERSION,
      signature,
      source,
      generatedAt: new Date().toISOString(),
      metrics,
      results: {
        versions: versionsOf(metrics, datasets),
        computedAt: new Date().toISOString(),
        values: Object.fromEntries(
          picked.kept.map((k) => [k.metric.id, k.result]),
        ),
      },
    };
    return previous
      ? tx.update(previous, data)
      : tx.put(
          resource(scope, "homeMetrics", "Home metrics", data, METRICS_ID),
        );
  });
}

async function aiReady(store: Store, scope: string) {
  const provider = await store.get(scope, "provider"),
    jobId = (await store.get(scope, "workspace"))?.data.onboarding
      ?.providerJobId,
    job = jobId && (await store.get(scope, jobId));
  return !!provider && (!job || job.data.status === "succeeded");
}
// Queues the one AI call that picks metrics for this schema.
export async function queueAiMetrics(
  store: Store,
  scope: string,
  signature: string,
  datasets: Resource[],
  actor: string,
  attempt = 0,
) {
  const id = "metrics_" + digest(JSON.stringify([signature, attempt]));
  return transaction(store, scope, async (tx) => {
    const prior = await tx.get(id);
    if (prior) return prior;
    return tx.put(
      resource(
        scope,
        "run",
        "Pick key metrics",
        {
          goal: "Pick the growth and analytics metrics for Home",
          intent: "build",
          task: "home_metrics",
          background: true,
          authority: { userId: actor, readOnly: false },
          datasetIds: datasets.map((d) => d.id),
          metricsSignature: signature,
          status: "queued",
          stage: "queued",
          events: [],
          calls: 0,
          tokens: 0,
        },
        id,
      ),
    );
  });
}

const inflight = new Map<string, Promise<HomeMetrics>>();
// What Home shows. Cached results come back as they are; results whose input
// data changed are recomputed by code; a new schema gets stand-in metrics at
// once and, with AI set up, a single AI call to pick better ones.
export function homeMetrics(store: Store, scope: string, actor: string) {
  const key = scope;
  if (!inflight.has(key))
    inflight.set(
      key,
      loadMetrics(store, scope, actor).finally(() => inflight.delete(key)),
    );
  return inflight.get(key)!;
}
async function loadMetrics(
  store: Store,
  scope: string,
  actor: string,
): Promise<HomeMetrics> {
  const datasets = await metricsDatasets(store, scope),
    ready = await aiReady(store, scope);
  if (!datasets.length)
    return {
      status: "empty",
      source: "computed",
      aiReady: ready,
      generating: false,
      metrics: [],
    };
  const signature = schemaSignature(datasets);
  let stored = await store.get(scope, METRICS_ID);
  if (stored?.data.signature !== signature) {
    const picked = await validateSpecs(
      store,
      scope,
      computedSpecs(datasets),
      datasets,
    );
    let runId: string | undefined;
    if (ready && stored?.data.aiSignature !== signature)
      runId = (await queueAiMetrics(store, scope, signature, datasets, actor))
        .id;
    stored = await saveMetrics(
      store,
      scope,
      signature,
      "computed",
      picked,
      datasets,
      runId ? { aiSignature: signature, runId } : {},
    );
  } else if ((stored.data as Stored).version !== METRICS_VERSION) {
    const s = stored.data as Stored;
    const picked = await validateSpecs(
      store,
      scope,
      s.metrics.map((m) => m.spec),
      datasets,
    );
    stored = await saveMetrics(
      store,
      scope,
      signature,
      picked.kept.length ? s.source : "computed",
      picked.kept.length
        ? picked
        : await validateSpecs(store, scope, computedSpecs(datasets), datasets),
      datasets,
    );
  } else {
    const s = stored.data as Stored;
    const current = versionsOf(s.metrics, datasets);
    if (JSON.stringify(current) !== JSON.stringify(s.results.versions)) {
      const values = Object.fromEntries(
        await Promise.all(
          s.metrics.map(
            async (m) => [m.id, await run(store, scope, m)] as const,
          ),
        ),
      );
      stored = await transaction(store, scope, async (tx) => {
        const r = (await tx.get(METRICS_ID))!;
        return tx.update(r, {
          ...r.data,
          results: {
            versions: current,
            computedAt: new Date().toISOString(),
            values,
          },
        });
      });
    }
  }
  const s = stored!.data as Stored,
    byId = new Map(datasets.map((d) => [d.id, d])),
    runRes = s.runId ? await store.get(scope, s.runId) : undefined,
    status = runRes?.data.status;
  return {
    status: "ready",
    source: s.source,
    aiReady: ready,
    generating: status === "queued" || status === "running",
    error:
      status === "failed"
        ? runRes!.data.error || "The AI couldn’t pick metrics."
        : undefined,
    generatedAt: s.generatedAt,
    computedAt: s.results.computedAt,
    metrics: s.metrics.map((m) => ({
      id: m.id,
      title: m.spec.title,
      why: m.spec.why,
      kind: m.spec.kind,
      format: m.spec.format,
      dataset: byId.get(m.spec.datasetId)
        ? noun(byId.get(m.spec.datasetId)!)
        : "",
      window: m.spec.kind === "kpi" && m.spec.timeColumn ? "30d" : undefined,
      grain: m.spec.grain,
      result: s.results.values[m.id] || { error: "Not computed yet" },
    })),
  };
}

// Regenerate: with AI, a fresh AI pick for the current schema; without it,
// the stand-in metrics again.
export async function regenerateMetrics(
  store: Store,
  scope: string,
  actor: string,
) {
  const datasets = await metricsDatasets(store, scope);
  if (!datasets.length) return;
  const signature = schemaSignature(datasets);
  if (await aiReady(store, scope)) {
    const stored = await store.get(scope, METRICS_ID);
    const attempt = (stored?.data.attempt || 0) + 1;
    const r = await queueAiMetrics(
      store,
      scope,
      signature,
      datasets,
      actor,
      attempt,
    );
    if (!stored) {
      const picked = await validateSpecs(
        store,
        scope,
        computedSpecs(datasets),
        datasets,
      );
      await saveMetrics(store, scope, signature, "computed", picked, datasets);
    }
    await transaction(store, scope, async (tx) => {
      const s = (await tx.get(METRICS_ID))!;
      tx.update(s, {
        ...s.data,
        aiSignature: signature,
        runId: r.id,
        attempt,
      });
    });
    return;
  }
  const picked = await validateSpecs(
    store,
    scope,
    computedSpecs(datasets),
    datasets,
  );
  await saveMetrics(store, scope, signature, "computed", picked, datasets);
}

export const METRICS_PROMPT =
  "Pick the metrics a founder or operator would check first on their home page: growth and business health from these datasets. Return 4 to 8 metrics. Good choices: new customers, accounts, users or signups over time (count by a creation date); revenue (sum of a paid amount; format currency, scale cents when amounts are in cents such as Stripe amount_* columns); active or canceled subscriptions (count with a filter on a status value); activity volume (events); and one or two breakdowns by a category with few values (status, plan, channel). kind kpi is one headline number: add timeColumn only when it counts or sums things added over time (new customers, signups, revenue), to compare the last 30 days with the 30 before; leave it out for standing totals such as active subscriptions. kind trend is a value over time and needs timeColumn. kind breakdown is a value by category and needs groupBy. measure count counts rows; count_distinct counts unique values of column; sum and avg need a numeric column. A filter compares a column to one of its listed values exactly. Use only the listed datasets and columns. Prefer datasets marked merged, which combine several sources, over their raw parts. Titles are short plain English such as 'New customers', 'Revenue' or 'Active subscriptions'. why is one short sentence on what the metric tells the reader. Leave out every field a metric doesn't need: no column for count, no groupBy unless kind is breakdown, no filter unless the metric is about one listed value, format number and scale none for counts. Use each dataset's own columns: a filter or groupBy column must be listed under the same dataset.";
