// How an answer's rows are shown: a stat row for a single result, a small
// chart when the shape suits one, and the table.
import "../workspace/home.css";
import { useMemo, useState } from "react";
import { ChevronDown, ChevronUp } from "lucide-react";
import type { HomeMetric } from "../../../../../packages/shared/src";
import {
  Button,
  DataGrid,
  compareValues,
  formatNumber,
  formatValue,
  inferKind,
  plural,
  type ColumnKind,
  type GridColumn,
  type GridSort,
} from "../../kit";
import { fieldName } from "../../ui";
import { BreakdownChart, TrendChart } from "../workspace/metrics";

type Row = Record<string, unknown>;
type Point = { label: string; value: number };

const numberText = /^-?\d+(\.\d+)?(e[+-]?\d+)?$/i;
// A number, including numbers sent as text (big integers, decimals).
const toNumber = (v: unknown): number | null =>
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
const isNumeric = (rows: Row[], key: string) =>
  rows.some((r) => present(r[key])) &&
  rows.every((r) => !present(r[key]) || toNumber(r[key]) !== null);
// Keys, not measures: "id", "customer_id", "orderId".
const isIdLike = (key: string) => /(^|_)id$/i.test(key) || /[a-z]Id$/.test(key);
const isoTime = /^\d{4}-\d{2}(-\d{2}([T ][\d:.]+(Z|[+-]\d{2}:?\d{2})?)?)?$/;
const isTimeLike = (rows: Row[], key: string) =>
  rows.every(
    (r) =>
      !present(r[key]) ||
      (typeof r[key] === "string" && isoTime.test(r[key] as string)),
  ) && rows.some((r) => present(r[key]));
const isYearLike = (rows: Row[], key: string) =>
  /^(year|yr|fiscal_year)$/i.test(key) &&
  rows.every((r) => {
    const n = toNumber(r[key]);
    return n === null || (Number.isInteger(n) && n > 1900 && n < 2200);
  });

const acronyms = new Set(
  "id url sku mrr arr aov ltv cac arpu roi nps gmv usd eur gbp kpi utm".split(
    " ",
  ),
);
// Column names read as labels: "total_mrr" -> "Total MRR".
export const columnLabel = (key: string) =>
  fieldName(key)
    .split(" ")
    .map((w) => (acronyms.has(w.toLowerCase()) ? w.toUpperCase() : w))
    .join(" ");
const calendar = /^(\d{4})-(\d{2})(?:-(\d{2}))?$/;
// A date without a time is a calendar day (or month): read it in local time,
// not as UTC midnight, which would show the day before west of UTC.
function calendarText(v: unknown): string | null {
  const m = typeof v === "string" ? calendar.exec(v) : null;
  if (!m) return null;
  const d = new Date(+m[1], +m[2] - 1, m[3] ? +m[3] : 1);
  return d.toLocaleDateString(
    undefined,
    m[3]
      ? { year: "numeric", month: "short", day: "numeric" }
      : { year: "numeric", month: "short" },
  );
}
// Display text for one value outside the grid.
function valueText(key: string, v: unknown): string | null {
  if (!present(v)) return null;
  const day = calendarText(v);
  if (day) return day;
  // Numbers are formatted as sent, so an integer beyond 2^53 keeps every digit.
  if (toNumber(v) !== null)
    return isIdLike(key) || /^(year|yr|fiscal_year)$/i.test(key)
      ? String(v)
      : formatNumber(v as number | bigint | string);
  return formatValue(v, inferKind([v]));
}

export type ChartPlan = {
  kind: "trend" | "breakdown";
  points: Point[];
  grain?: HomeMetric["grain"];
  label: string;
  value: string;
};
const DAY = 86400000;
// A chart only for small, summarised results: a label column (a date, a year
// or a category) and a measure, each label once.
export function chartPlan(rows: Row[]): ChartPlan | null {
  if (rows.length < 2) return null;
  const keys = Object.keys(rows[0] || {});
  if (keys.length < 2 || keys.length > 4) return null;
  const time = keys.find((k) => isTimeLike(rows, k)),
    label =
      time ??
      keys.find((k) => isYearLike(rows, k)) ??
      keys.find((k) => !isNumeric(rows, k));
  if (!label) return null;
  const value = keys.find(
    (k) =>
      k !== label &&
      !isIdLike(k) &&
      !isYearLike(rows, k) &&
      !isTimeLike(rows, k) &&
      isNumeric(rows, k),
  );
  if (!value) return null;
  const labels = rows.map((r) => String(r[label] ?? ""));
  if (new Set(labels).size !== labels.length) return null;
  const measured = rows.filter(
    (r) => present(r[label]) && toNumber(r[value]) !== null,
  );
  if (measured.length < 2) return null;
  if (time) {
    // Daily, weekly or monthly buckets, oldest first.
    const days = measured
      .map((r) => ({
        day: String(r[label]).slice(0, 10),
        value: toNumber(r[value])!,
      }))
      .sort((a, b) => a.day.localeCompare(b.day));
    const monthly = days.every(
      (d) => d.day.length === 7 || d.day.endsWith("-01"),
    );
    const gaps = days
      .slice(1)
      .map((d, i) => (Date.parse(d.day) - Date.parse(days[i].day)) / DAY);
    const grain: HomeMetric["grain"] = monthly
      ? "month"
      : gaps.length && gaps.every((g) => g === 7)
        ? "week"
        : "day";
    return {
      kind: "trend",
      grain,
      label,
      value,
      points: days.map((d) => ({
        label: monthly ? d.day.slice(0, 7) : d.day,
        value: d.value,
      })),
    };
  }
  if (measured.length > 12) return null;
  return {
    kind: "breakdown",
    label,
    value,
    points: measured.map((r) => ({
      label: String(r[label]),
      value: toNumber(r[value])!,
    })),
  };
}

export function AnswerChart({ plan }: { plan: ChartPlan }) {
  const title = `${columnLabel(plan.value)} by ${columnLabel(plan.label).toLowerCase()}`;
  const m: HomeMetric = {
    id: "answer",
    title,
    why: "",
    kind: plan.kind,
    format: "number",
    dataset: "",
    grain: plan.grain,
    result: { points: plan.points },
  };
  return (
    <figure className="an-chart">
      <figcaption>{title}</figcaption>
      {plan.kind === "trend" ? (
        <TrendChart m={m} points={plan.points} />
      ) : (
        <BreakdownChart m={m} points={plan.points} />
      )}
    </figure>
  );
}

// One result row reads best as a few labelled values.
export function AnswerStats({ row }: { row: Row }) {
  return (
    <dl className="an-stats">
      {Object.entries(row).map(([key, v]) => {
        const text = valueText(key, v),
          numeric = toNumber(v) !== null;
        return (
          <div key={key} className="an-stat">
            <dt>{columnLabel(key)}</dt>
            <dd className={numeric ? "hf-num" : undefined}>
              {text ?? <span className="hf-null">—</span>}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}

const PREVIEW = 8,
  HEAD = 36,
  ROW = 34;
// Table columns typed from the first rows. Measures sit on the right.
export function answerColumns(rows: Row[]) {
  const sample = rows.slice(0, 50);
  const blank = <span className="hf-null">null</span>;
  return Object.keys(rows[0] || {}).map((key) => {
    const values = sample.map((r) => r[key]);
    const days =
      values.some(present) &&
      values.every((v) => !present(v) || calendarText(v) !== null);
    // Numbers sent as text still sort and align as numbers; years stay
    // as written ("2026", not "2,026").
    const year = isYearLike(sample, key),
      numeric =
        !days &&
        !year &&
        !isIdLike(key) &&
        values.some((v) => typeof v === "string") &&
        isNumeric(sample, key);
    const kind: ColumnKind = days
      ? "date"
      : numeric
        ? "number"
        : year
          ? "string"
          : inferKind(values);
    return {
      key,
      label: columnLabel(key),
      type: kind,
      kind,
      numeric,
      align: year ? ("right" as const) : undefined,
      render: days
        ? (r: Row) =>
            present(r[key]) ? (calendarText(r[key]) ?? String(r[key])) : blank
        : year
          ? (r: Row) => (present(r[key]) ? String(r[key]) : blank)
          : undefined,
    };
  });
}
// Every row in order, not just the ones shown; blanks stay last either way.
export function sortAnswer(
  rows: Row[],
  columns: ReturnType<typeof answerColumns>,
  sort: GridSort,
) {
  const col = sort && columns.find((c) => c.key === sort.key);
  if (!sort || !col) return rows;
  // Numbers compare as sent, so integers beyond 2^53 keep their order; text
  // that isn't a number sorts as a blank.
  const value = (r: Row) =>
    col.numeric && toNumber(r[col.key]) === null ? null : r[col.key];
  const dir = sort.dir === "desc" ? -1 : 1;
  return [...rows].sort((a, b) => {
    const va = value(a),
      vb = value(b);
    return !present(va) || !present(vb)
      ? compareValues(va, vb, col.kind)
      : compareValues(va, vb, col.kind) * dir;
  });
}
// The first rows, with the rest one click away.
export function AnswerTable({ rows, label }: { rows: Row[]; label: string }) {
  const [all, setAll] = useState(false),
    [sort, setSort] = useState<GridSort>(null);
  const columns = useMemo(() => answerColumns(rows), [rows]);
  const sorted = useMemo(
    () => sortAnswer(rows, columns, sort),
    [rows, sort, columns],
  );
  const more = rows.length > PREVIEW;
  const shown = all || !more ? sorted : sorted.slice(0, PREVIEW);
  return (
    <DataGrid
      className="an-table"
      label={label}
      rows={shown}
      columns={columns as GridColumn<Row>[]}
      sort={sort}
      onSortChange={setSort}
      maxHeight={all ? 440 : HEAD + PREVIEW * ROW + 2}
      rowHeight={ROW}
      footer={
        more ? (
          <Button
            size="sm"
            variant="ghost"
            iconRight={all ? ChevronUp : ChevronDown}
            aria-expanded={all}
            onClick={() => setAll((v) => !v)}
          >
            {all ? "Show fewer rows" : `Show all ${plural(rows.length, "row")}`}
          </Button>
        ) : undefined
      }
    />
  );
}
