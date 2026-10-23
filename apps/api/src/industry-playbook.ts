// The built-in playbook is data, read from the same Markdown people review.
// Resolving it uses schema metadata only. Queries belong to industry-checks.ts.
import { readFileSync } from "node:fs";
import { z } from "zod";
import type {
  MetricSpec,
  Resource,
} from "../../../packages/shared/src/index.js";
import { digest } from "./security.js";

const roleSchema = z.object({
  title: z.string(),
  names: z.array(z.string()).min(1),
  of: z.string().optional(),
  type: z.enum(["number", "date", "text", "key"]).optional(),
});
const checkSchema = z.discriminatedUnion("kind", [
  z.object({
    id: z.string(),
    kind: z.literal("link"),
    title: z.string(),
    from: z.string(),
    column: z.string(),
    to: z.string(),
    key: z.string(),
    good: z.number().min(0).max(1),
    fix: z.string(),
  }),
  z.object({
    id: z.string(),
    kind: z.literal("unique"),
    title: z.string(),
    column: z.string(),
    good: z.number().min(0).max(1),
    fix: z.string(),
  }),
  z.object({
    id: z.string(),
    kind: z.literal("status"),
    title: z.string(),
    column: z.string(),
    values: z.array(z.string()),
  }),
]);
const playbookSchema = z.object({
  id: z.literal("ecommerce"),
  title: z.string(),
  version: z.number().int().positive(),
  roles: z.record(z.string(), roleSchema),
  signals: z.object({
    require: z.array(z.string()),
    boost: z.array(z.string()),
    min: z.number().int().positive(),
  }),
  metrics: z.array(
    z.object({
      title: z.string(),
      measure: z.enum(["count", "count_distinct", "sum", "avg"]),
      dataset: z.string(),
      column: z.string().optional(),
      time: z.string().optional(),
    }),
  ),
  checks: z.array(checkSchema),
  suggestions: z.array(
    z.object({
      id: z.string(),
      missing: z.string(),
      kind: z.enum(["connect", "change"]),
      text: z.string(),
    }),
  ),
});
export type Playbook = z.infer<typeof playbookSchema>;
type Column = {
  name: string;
  type: string;
  sensitive?: boolean;
  primaryKey?: boolean;
};
type Mapped = { datasetId: string; column?: string };
export type RetailReading = {
  book: Playbook;
  identity: string;
  mapping: Record<string, Mapped>;
  datasets: Resource[];
};
let book: Playbook | undefined;
export function ecommercePlaybook() {
  if (!book) {
    const text = readFileSync(
      new URL("../../../docs/industries/ecommerce.md", import.meta.url),
      "utf8",
    );
    const front = /```json\n([\s\S]*?)\n```/.exec(text);
    if (!front)
      throw new Error(
        "The e-commerce playbook needs a JSON configuration block",
      );
    const parsed = playbookSchema.parse(JSON.parse(front[1]));
    const refs = [
      ...parsed.signals.require,
      ...parsed.signals.boost,
      ...Object.values(parsed.roles).flatMap((r) => (r.of ? [r.of] : [])),
      ...parsed.metrics.flatMap((m) =>
        [m.dataset, m.column, m.time].filter((x): x is string => !!x),
      ),
      ...parsed.checks.flatMap((c) =>
        c.kind === "link" ? [c.from, c.column, c.to, c.key] : [c.column],
      ),
      ...parsed.suggestions.map((s) => s.missing),
    ];
    if (refs.some((r) => !parsed.roles[r]))
      throw new Error("An e-commerce rule names an unknown role");
    book = parsed;
  }
  return book;
}
const normal = (s: string) =>
  s
    .replace(/([a-z])([A-Z])/g, "$1_$2")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "_")
    .replace(/^_|_$/g, "");
const singular = (s: string) =>
  s.endsWith("ies")
    ? s.slice(0, -3) + "y"
    : s.endsWith("s") && !s.endsWith("ss")
      ? s.slice(0, -1)
      : s;
const datasetName = (d: Resource) =>
  singular(
    normal(
      d.name
        .split(" · ")
        .at(-1)!
        .replace(/\s*\(.*\)|\.(csv|json|parquet)$/gi, "")
        .trim(),
    ),
  );
const columns = (d: Resource): Column[] => d.data.profile?.columns || [];
const fits = (type: string | undefined, c: Column) =>
  !/STRUCT|MAP\(|\[|UNION\(|^LIST|^JSON/i.test(c.type) &&
  (type === "number"
    ? !c.sensitive &&
      /^(U?(TINYINT|SMALLINT|INTEGER|BIGINT|HUGEINT)|FLOAT|REAL|DOUBLE|DECIMAL|NUMERIC)/i.test(
        c.type,
      )
    : type === "date"
      ? /^(DATE|TIMESTAMP)/i.test(c.type)
      : type === "text"
        ? /^(VARCHAR|ENUM)/i.test(c.type)
        : true);

// A weak match keeps generic Home behaviour. Names identify candidates;
// typed columns and several independent retail entities must support them.
export function retailReading(datasets: Resource[]): RetailReading | undefined {
  const pb = ecommercePlaybook(),
    mapping: Record<string, Mapped> = {};
  const ordered = [...datasets].sort(
    (a, b) =>
      Number(!!b.data.generation) - Number(!!a.data.generation) ||
      (b.data.profile?.rows || 0) - (a.data.profile?.rows || 0) ||
      a.id.localeCompare(b.id),
  );
  for (const [id, r] of Object.entries(pb.roles)) {
    if (r.of) continue;
    const d = ordered.find((d) =>
      r.names.some((n) => singular(normal(n)) === datasetName(d)),
    );
    if (d) mapping[id] = { datasetId: d.id };
  }
  for (const [id, r] of Object.entries(pb.roles)) {
    const d = r.of && ordered.find((d) => d.id === mapping[r.of!]?.datasetId);
    if (!d) continue;
    const c = r.names
      .map((n) =>
        columns(d).find((c) => normal(c.name) === normal(n) && fits(r.type, c)),
      )
      .find(Boolean);
    if (c) mapping[id] = { datasetId: d.id, column: c.name };
  }
  if (
    !pb.signals.require.every((r) => mapping[r]) ||
    pb.signals.boost.filter((r) => mapping[r]).length < pb.signals.min
  )
    return;
  return {
    book: pb,
    identity: digest(JSON.stringify([pb, mapping])),
    mapping,
    datasets,
  };
}

// Only primitive specs the existing compiler can express. No ratio, join,
// status interpretation, industry benchmark or business definition is guessed.
export function retailMetrics(datasets: Resource[]): MetricSpec[] {
  const r = retailReading(datasets);
  if (!r) return [];
  return r.book.metrics.flatMap((m) => {
    const d = r.mapping[m.dataset],
      c = m.column && r.mapping[m.column],
      time = m.time && r.mapping[m.time];
    if (!d || (m.column && (!c || c.datasetId !== d.datasetId))) return [];
    const column =
      c &&
      datasets
        .find((x) => x.id === d.datasetId)
        ?.data.profile?.columns?.find((x: Column) => x.name === c.column);
    if (column?.sensitive) return [];
    const monetary = m.measure === "sum" || m.measure === "avg";
    return [
      {
        title: m.title,
        kind: "kpi" as const,
        datasetId: d.datasetId,
        measure: m.measure,
        ...(c ? { column: c.column } : {}),
        ...(time && time.datasetId === d.datasetId
          ? { timeColumn: time.column }
          : {}),
        format: "number" as const,
        scale:
          monetary && /_cents$/i.test(c ? c.column! : "")
            ? ("cents" as const)
            : ("none" as const),
        why: monetary
          ? "Recorded order amounts in source units; includes every recorded status. Confirm currency before comparing sources."
          : "Recorded orders and customers; includes every recorded order status.",
      },
    ];
  });
}

export function retailMetricPriority(spec: MetricSpec, datasets: Resource[]) {
  const specs = retailMetrics(datasets);
  const at = specs.findIndex(
    (s) =>
      s.kind === spec.kind &&
      s.datasetId === spec.datasetId &&
      s.measure === spec.measure &&
      s.column === spec.column &&
      s.timeColumn === spec.timeColumn &&
      !spec.filter,
  );
  return at < 0 ? specs.length : at;
}
