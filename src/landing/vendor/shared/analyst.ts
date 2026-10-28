// Area schemas for AIP Analyst: threads, messages, tool steps and saved
// analyses, and how an answer's result is read.
// Owned by the analyst area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index" would be circular.
// Type-only imports from "./index" are fine.
import { z } from "zod";
import type { DatasetProfile } from "./index";

export const GoalSchema = z.object({
  goal: z.string().min(5).max(4000),
  intent: z.enum(["build", "answer", "auto"]).default("build"),
  parentId: z.string().optional(),
  context: z
    .object({ page: z.string().optional(), resourceId: z.string().optional() })
    .optional(),
});
export type AssistantRun = {
  intent?: "auto" | "answer" | "build";
  resolvedIntent?: "answer" | "build";
  goal: string;
  status:
    | "queued"
    | "running"
    | "awaiting_review"
    | "succeeded"
    | "failed"
    | "canceled";
  stage: string;
  events: { at: string; stage: string; message: string }[];
  calls: number;
  tokens: number;
  proposalId?: string;
  error?: string;
  // The major steps planned for an answer (see AnswerStep), set once the run
  // is known to answer a question.
  outline?: AnswerStep[];
  // How long comparable answers took, in ms, when there is enough evidence.
  estimate?: { low: number; high: number; samples: number } | null;
};
export type AnswerStep = "understand" | "analyze" | "check" | "write";

// What an answer's rows are, read by code rather than by the model: the
// shape decides how they are shown, and the measure is the number that
// answers the question. DuckDB column types, when known, settle what a column
// holds (VARCHAR digits are text); otherwise its values do. The executor's
// profile of the full result, when given, settles the order of its values,
// ties and which labels are unique; otherwise the rows given do.
export type AnswerShape = {
  kind: "empty" | "value" | "ranking" | "trend" | "breakdown" | "table";
  // The column naming each row: a category, a date or a year.
  label?: string;
  // The number that answers the question.
  measure?: string;
  // A ranking's order: highest first, or lowest first.
  order?: "desc" | "asc";
  // How many rows share a ranking's first place.
  leaders?: number;
  // Keys and codes: never a measure, and shown after the values that matter.
  keys: string[];
};
// What the server checked in an answer's full result before describing it.
export type AnswerCheck = AnswerShape & {
  // Rows repeated exactly: a sign of a join that multiplied rows.
  duplicates: number;
  // Rows without the measure.
  missing: number;
};

type Row = Record<string, unknown>;
const numberText = /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i;
// A number, including numbers sent as text (big integers, decimals).
export const answerNumber = (v: unknown): number | null =>
  typeof v === "number"
    ? Number.isFinite(v)
      ? v
      : null
    : typeof v === "bigint"
      ? Number(v)
      : typeof v === "string" && numberText.test(v.trim())
        ? Number(v)
        : null;
const present = (v: unknown) => v !== null && v !== undefined;
// Keys, codes and positions, not measures: "id", "customer_id", "orderId",
// "zip_code", "order_number", "rank".
export const isKeyColumn = (key: string) =>
  /(^|_)(id|uuid|guid|key|code|sku|zip|zipcode|postcode|rank|position|rn|idx)$/i.test(
    key,
  ) ||
  /_(number|no)$/i.test(key) ||
  /[a-z](Id|ID|Uuid|Key|Code|Number)$/.test(key);
export const isNumericColumn = (rows: Row[], key: string) =>
  rows.some((r) => present(r[key])) &&
  rows.every((r) => !present(r[key]) || answerNumber(r[key]) !== null);
// "year", "order_year", "fiscalYear", with whole years as values.
export const isYearColumn = (rows: Row[], key: string) =>
  (/(^|_)(year|yr)$/i.test(key) || /[a-z]Year$/.test(key)) &&
  rows.every((r) => {
    const n = answerNumber(r[key]);
    return n === null || (Number.isInteger(n) && n > 1900 && n < 2200);
  });
const isoTime = /^\d{4}-\d{2}(-\d{2}([T ][\d:.]+(Z|[+-]\d{2}:?\d{2})?)?)?$/;
export const isTimeColumn = (rows: Row[], key: string) =>
  rows.every(
    (r) =>
      !present(r[key]) ||
      (typeof r[key] === "string" && isoTime.test(r[key] as string)),
  ) && rows.some((r) => present(r[key]));
const numericType =
  /^(TINYINT|SMALLINT|INTEGER|INT|BIGINT|HUGEINT|UTINYINT|USMALLINT|UINTEGER|UBIGINT|UHUGEINT|FLOAT|REAL|DOUBLE|DECIMAL|NUMERIC)\b/i;
const words = (text: string) =>
  text
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter((w) => w.length > 2)
    .map((w) => w.replace(/ies$/, "y").replace(/([^s])s$/, "$1"));
// Numbers beyond 2^53 are equal only when their text is.
const same = (a: unknown, b: unknown) => {
  const x = answerNumber(a),
    y = answerNumber(b);
  return (
    x !== null &&
    x === y &&
    (Math.abs(x) <= Number.MAX_SAFE_INTEGER ||
      String(a).trim() === String(b).trim())
  );
};

export function answerShape(
  rows: Row[],
  {
    types,
    hint = "",
    profile,
  }: {
    types?: Record<string, string>;
    // What was asked, most telling first: the question, then the query's
    // title. A later hint only decides between measures earlier ones name
    // equally.
    hint?: string | string[];
    profile?: Pick<DatasetProfile, "rows" | "columns">;
  } = {},
): AnswerShape {
  const columns = Object.keys(rows[0] || {}),
    keys = columns.filter(isKeyColumn);
  if (!rows.length) return { kind: "empty", keys };
  const full = new Map(profile?.columns.map((c) => [c.name, c]));
  types ??=
    profile && Object.fromEntries(profile.columns.map((c) => [c.name, c.type]));
  const total = profile?.rows ?? rows.length;
  // A column with a value somewhere: in the full result when it is profiled.
  const filled = (k: string) => {
    const c = full.get(k);
    return c ? c.nulls < total : rows.some((r) => present(r[k]));
  };
  const numeric = (k: string) =>
    types?.[k]
      ? numericType.test(types[k]) && filled(k)
      : isNumericColumn(rows, k);
  const time = (k: string) =>
    /^(DATE|TIMESTAMP)/i.test(types?.[k] || "") || isTimeColumn(rows, k);
  const year = (k: string) => isYearColumn(rows, k);
  const unique = (k: string) => {
    const c = full.get(k);
    return c
      ? c.distinct + Math.min(c.nulls, 1) === total
      : new Set(rows.map((r) => String(r[k] ?? ""))).size === rows.length;
  };
  const measures = columns.filter(
    (k) => !isKeyColumn(k) && !year(k) && !time(k) && numeric(k),
  );
  // Measures whose rows run in order, highest or lowest first.
  const ordered = (k: string) => {
    const o = full.get(k)?.order;
    if (o)
      return total - full.get(k)!.nulls < 3 || (!o.up && !o.down)
        ? null
        : !o.up
          ? "desc"
          : !o.down
            ? "asc"
            : null;
    const values = rows
      .map((r) => answerNumber(r[k]))
      .filter((v): v is number => v !== null);
    if (values.length < 3 || values[0] === values.at(-1)) return null;
    const desc = values[0] > values.at(-1)!;
    return values.every(
      (v, i) => !i || (desc ? v <= values[i - 1] : v >= values[i - 1]),
    )
      ? desc
        ? "desc"
        : "asc"
      : null;
  };
  // The measure the question names ("orders" → order_count), else one the
  // rows are ranked by, else the first. Order only decides between measures
  // the hints name equally.
  const asked = [hint].flat().map((h) => new Set(words(h)));
  const measure = measures
    .map((k, i) => ({
      k,
      i,
      scores: asked.map((a) => words(k).filter((w) => a.has(w)).length),
      sorted: ordered(k) ? 1 : 0,
    }))
    .sort(
      (a, b) =>
        a.scores.reduce((d, s, j) => d || b.scores[j] - s, 0) ||
        b.sorted - a.sorted ||
        a.i - b.i,
    )[0]?.k;
  if (rows.length === 1) return { kind: "value", measure, keys };
  const when =
    columns.find((k) => time(k) && unique(k)) ??
    columns.find((k) => year(k) && unique(k));
  const texts = columns.filter(
    (k) => !isKeyColumn(k) && !numeric(k) && !time(k) && unique(k),
  );
  const label =
    texts.find((k) => /(^|_)(name|title|label)$|name$/i.test(k)) ??
    texts.find((k) => !/mail|url|phone|address/i.test(k)) ??
    texts[0] ??
    keys.find(unique);
  if (!measure) return { kind: "table", keys };
  const order = ordered(measure);
  // A date column makes a trend, unless the rows are ranked by the measure
  // instead of running in time order ("the months with the most orders").
  if (when) {
    const o = full.get(when)?.order,
      times = rows.map((r) => String(r[when] ?? ""));
    const inTime = o
      ? !o.up || !o.down
      : times.every((t, i) => !i || t >= times[i - 1]) ||
        times.every((t, i) => !i || t <= times[i - 1]);
    if (inTime || !order) return { kind: "trend", label: when, measure, keys };
  }
  const named = when && order ? when : label;
  if (!named) return { kind: "table", measure, keys };
  if (order) {
    const top = rows.map((r) => r[measure]).find(present);
    return {
      kind: "ranking",
      label: named,
      measure,
      order,
      leaders:
        full.get(measure)?.order?.first ??
        rows.filter((r) => same(r[measure], top)).length,
      keys,
    };
  }
  // Every row with the same value: when the question asks for the most or
  // the least, a ranking in which they all share first place.
  const extreme =
    /\b(top|most|highest|largest|biggest|best|leading|least|lowest|fewest|smallest|worst)\b/i.exec(
      [hint].flat()[0] || "",
    );
  const o = full.get(measure)?.order,
    values = rows.map((r) => r[measure]).filter(present),
    tied = o
      ? !o.up && !o.down
        ? o.first
        : 0
      : values.every((v) => same(v, values[0]))
        ? values.length
        : 0;
  if (extreme && tied >= 2)
    return {
      kind: "ranking",
      label: named,
      measure,
      order: /least|lowest|fewest|smallest|worst/i.test(extreme[1])
        ? "asc"
        : "desc",
      leaders: tied,
      keys,
    };
  return total <= 12
    ? { kind: "breakdown", label: named, measure, keys }
    : { kind: "table", label: named, measure, keys };
}

// A possible next investigation, supported by a published ontology property.
export type ChatFollowup = {
  id: string;
  label: string;
  question: string;
  ontology: { type: string; datasetId: string; property: string };
  version: string;
};
export type ChatFollowups = {
  runId: string | null;
  generation: string | null;
  items: ChatFollowup[];
};
