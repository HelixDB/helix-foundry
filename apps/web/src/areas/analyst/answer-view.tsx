// How an answer's rows are shown, by the shape the server checked them to
// have: a ranking or a breakdown as bars, a trend as a line, one row as a few
// values, and always the table. The numbers a summary states are emphasised
// only when the rows back them.
import "../workspace/home.css";
import { useMemo, useState, type ReactNode } from "react";
import { ChevronDown, ChevronUp } from "lucide-react";
import {
  answerNumber as toNumber,
  answerShape,
  isKeyColumn,
  isNumericColumn,
  isYearColumn,
  type AnswerShape,
  type HomeMetric,
} from "../../../../../packages/shared/src";
import {
  Button,
  DataGrid,
  Segmented,
  calendarDate,
  compareValues,
  formatNumber,
  formatValue,
  inferKind,
  plural,
  type ColumnKind,
  type GridColumn,
  type GridSort,
} from "../../kit";
import { BreakdownChart, TrendChart, type Point } from "../workspace/metrics";
import { columnLabel, columnWords } from "./chat-data";

type Row = Record<string, unknown>;

const present = (v: unknown) => v !== null && v !== undefined;
const isYearKey = (key: string) => /^(year|yr|fiscal_year)$/i.test(key);
const calendar = /^(\d{4})-(\d{2})(?:-(\d{2}))?$/;
// A date without a time is a calendar day (or month): read it in local time,
// not as UTC midnight, which would show the day before west of UTC.
function calendarText(v: unknown): string | null {
  const m = typeof v === "string" ? calendar.exec(v) : null;
  if (!m) return null;
  const d = calendarDate(+m[1], +m[2], m[3] ? +m[3] : 1);
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
    return isKeyColumn(key) || isYearKey(key)
      ? String(v)
      : formatNumber(v as number | bigint | string);
  return formatValue(v, inferKind([v]));
}

// Charts draw a Number; an integer beyond 2^53 also keeps its exact text for
// the tooltip and screen readers.
function point(label: string, v: unknown): Point {
  const value = toNumber(v)!,
    text = String(v).trim();
  return Number.isSafeInteger(value) || !/^-?\d+$/.test(text)
    ? { label, value }
    : { label, value, text: formatNumber(text) };
}

export type ChartPlan = {
  kind: "trend" | "breakdown";
  points: Point[];
  grain?: HomeMetric["grain"];
  label: string;
  value: string;
  // A ranking's order; its bars keep the rows' order.
  rank?: "desc" | "asc";
};
const DAY = 86400000;
// A chart for a ranking, a trend or a small breakdown: a label column (a date,
// a year or a category) and the measure, each label once. Keys are never the
// measure. The shape is the server's check, or read from the rows for answers
// from before it checked them.
export function chartPlan(rows: Row[], shape?: AnswerShape): ChartPlan | null {
  if (rows.length < 2) return null;
  const s = shape ?? answerShape(rows);
  if (!["ranking", "trend", "breakdown"].includes(s.kind)) return null;
  const label = s.label!,
    value = s.measure!;
  const measured = rows.filter(
    (r) => present(r[label]) && toNumber(r[value]) !== null,
  );
  if (measured.length < 2) return null;
  if (s.kind === "trend") {
    // Daily, weekly or monthly buckets (or years), oldest first.
    const days = measured
      .map((r) => point(String(r[label]).slice(0, 10), r[value]))
      .sort((a, b) => a.label.localeCompare(b.label));
    const monthly = days.every(
      (d) => d.label.length === 7 || d.label.endsWith("-01"),
    );
    const gaps = days
      .slice(1)
      .map((d, i) => (Date.parse(d.label) - Date.parse(days[i].label)) / DAY);
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
        ...d,
        label: monthly ? d.label.slice(0, 7) : d.label,
      })),
    };
  }
  return {
    kind: "breakdown",
    label,
    value,
    rank: s.kind === "ranking" ? s.order : undefined,
    points: measured.map((r) => point(String(r[label]), r[value])),
  };
}

export function AnswerChart({ plan }: { plan: ChartPlan }) {
  const title = `${columnLabel(plan.value)} by ${columnWords(plan.label)}`;
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
      <figcaption>
        {title}
        {plan.rank && <>, {plan.rank === "asc" ? "lowest" : "highest"} first</>}
      </figcaption>
      {plan.kind === "trend" ? (
        <TrendChart m={m} points={plan.points} />
      ) : (
        <BreakdownChart m={m} points={plan.points} rank={plan.rank} />
      )}
    </figure>
  );
}

// The columns of a result in the order they matter: what names each row, the
// measure, the other values, and keys last.
function ordered(keys: string[], shape?: AnswerShape) {
  const rank = (k: string) =>
    k === shape?.label ? 0 : k === shape?.measure ? 1 : isKeyColumn(k) ? 3 : 2;
  return keys
    .map((k, i) => ({ k, i }))
    .sort((a, b) => rank(a.k) - rank(b.k) || a.i - b.i)
    .map(({ k }) => k);
}

// One result row reads best as a few labelled values, the measure first and
// keys last and quieter.
export function AnswerStats({ row, shape }: { row: Row; shape?: AnswerShape }) {
  const s = shape ?? answerShape([row]);
  return (
    <dl className="an-stats">
      {ordered(Object.keys(row), s).map((key) => {
        const v = row[key],
          text = valueText(key, v),
          numeric = toNumber(v) !== null,
          isKey = isKeyColumn(key);
        return (
          <div
            key={key}
            className={
              isKey
                ? "an-stat is-key"
                : key === s.measure
                  ? "an-stat is-measure"
                  : "an-stat"
            }
          >
            <dt>{columnLabel(key)}</dt>
            <dd className={numeric && !isKey ? "hf-num" : undefined}>
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
// Table columns typed from the first rows, in the order they matter (the
// measure emphasised, keys last and quieter). Measures sit on the right.
export function answerColumns(rows: Row[], shape?: AnswerShape) {
  const sample = rows.slice(0, 50);
  const blank = <span className="hf-null">null</span>;
  const s = shape ?? answerShape(rows);
  return ordered(Object.keys(rows[0] || {}), s).map((key) => {
    const values = sample.map((r) => r[key]);
    const days =
      values.some(present) &&
      values.every((v) => !present(v) || calendarText(v) !== null);
    // Numbers sent as text still sort and align as numbers; years stay
    // as written ("2026", not "2,026").
    const year = isYearColumn(sample, key),
      numeric =
        !days &&
        !year &&
        !isKeyColumn(key) &&
        values.some((v) => typeof v === "string") &&
        isNumericColumn(sample, key);
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
      className:
        key === s.measure
          ? "is-measure"
          : isKeyColumn(key)
            ? "is-key"
            : undefined,
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
export function AnswerTable({
  rows,
  label,
  shape,
}: {
  rows: Row[];
  label: string;
  shape?: AnswerShape;
}) {
  const [all, setAll] = useState(false),
    [sort, setSort] = useState<GridSort>(null);
  const columns = useMemo(() => answerColumns(rows, shape), [rows, shape]);
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

// A ranking's chart starts with its top rows; the rest are one click away,
// and the table always has every row.
const TOP = 10,
  MOST = 30;
// The rows as a chart when their shape has one, or as the table, with a
// switch between the two. A heading names them when the answer's statement
// is not already their title.
export function AnswerResult({
  rows,
  total: all = rows.length,
  label,
  shape,
  heading,
}: {
  rows: Row[];
  // Rows in the full result; `rows` may be its first rows only.
  total?: number;
  label: string;
  shape: AnswerShape;
  heading?: string;
}) {
  // Bars start at zero, so a negative value would vanish: a signed
  // breakdown shows only as a table.
  const plan = useMemo(() => {
    const p = chartPlan(rows, shape);
    return p?.kind === "breakdown" && p.points.some((x) => x.value < 0)
      ? null
      : p;
  }, [rows, shape]);
  const [view, setView] = useState<"chart" | "table">("chart"),
    [more, setMore] = useState(false);
  // The chart has the rows given, which may be the result's first rows:
  // then it says so.
  const count = plan?.points.length || 0,
    partial = all > rows.length,
    whole = partial ? all : count,
    ranked = !!plan?.rank && count > TOP,
    top = more ? Math.min(MOST, count) : TOP;
  return (
    <div className="an-result">
      {(heading || plan) && (
        <div className="an-result-head">
          {heading && <h3>{heading}</h3>}
          {plan && (
            <Segmented
              label="Show the result as"
              value={view}
              onChange={(v) => setView(v as "chart" | "table")}
              options={[
                { value: "chart", label: "Chart" },
                { value: "table", label: "Table" },
              ]}
            />
          )}
        </div>
      )}
      {plan && view === "chart" ? (
        <>
          <AnswerChart
            plan={
              ranked ? { ...plan, points: plan.points.slice(0, top) } : plan
            }
          />
          {ranked ? (
            <div className="an-result-foot">
              <span className="hf-num">
                {top === whole
                  ? `All ${formatNumber(whole)}`
                  : `Top ${formatNumber(top)} of ${formatNumber(whole)}`}
              </span>
              {more && count > MOST ? (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setView("table")}
                >
                  {partial
                    ? `See the first ${formatNumber(rows.length)} rows in the table`
                    : "See every row in the table"}
                </Button>
              ) : (
                <Button
                  size="sm"
                  variant="ghost"
                  iconRight={more ? ChevronUp : ChevronDown}
                  aria-expanded={more}
                  onClick={() => setMore((v) => !v)}
                >
                  {more
                    ? `Show top ${TOP}`
                    : count > MOST || partial
                      ? `Show top ${formatNumber(Math.min(MOST, count))}`
                      : `Show all ${formatNumber(count)}`}
                </Button>
              )}
            </div>
          ) : (
            partial && (
              <div className="an-result-foot">
                <span className="hf-num">
                  First {formatNumber(rows.length)} of {plural(all, "row")}
                </span>
              </div>
            )
          )}
        </>
      ) : (
        <AnswerTable rows={rows} label={label} shape={shape} />
      )}
    </div>
  );
}

// The summary's first sentence answers the question, so it leads the answer
// as its statement when it is short enough to read as one. Initials, titles
// and abbreviations ("Lucas I. Kim", "Acme Inc.") don't end a sentence.
const notAnEnd =
  /(?:^|\s)(?:[A-Z]|Dr|Mr|Mrs|Ms|St|Inc|Ltd|Co|Corp|vs|etc|approx|No|Jan|Feb|Mar|Apr|Jun|Jul|Aug|Sep|Sept|Oct|Nov|Dec|e\.g|i\.e)\.$/;
export function leadSentence(
  summary: string,
): { statement: string; rest: string } | null {
  const text = summary.trim();
  for (const m of text.matchAll(/[.!?](?=\s+["“$€£A-Z0-9])/g)) {
    const end = m.index + 1,
      head = text.slice(0, end);
    if (notAnEnd.test(head)) continue;
    return head.length <= 180
      ? { statement: head, rest: text.slice(end).trim() }
      : null;
  }
  return text.length <= 180 ? { statement: text, rest: "" } : null;
}

// Numbers in the text, with a currency sign or a scale ("$1.2M", "12%",
// "3 million"). Not part of a word, a date or a decimal.
const quantity =
  /(?<![\w.,:/-])([$€£])?(\d{1,3}(?:,\d{3})+|\d+)(?:\.(\d+))?(%|\s?percent\b|\s?(?:thousand|million|billion)\b|[KkMB]\b|bn\b)?(?![\w/-]|[.,]\d)/g;
const scales: Record<string, number> = {
  k: 1e3,
  thousand: 1e3,
  m: 1e6,
  million: 1e6,
  b: 1e9,
  bn: 1e9,
  billion: 1e9,
};
const units = new Set(
  "more fewer less times percent points orders customers users people items units rows records days hours minutes weeks months years dollars sessions events visits products accounts invoices".split(
    " ",
  ),
);
const quiet = new Set(
  "a an and are at by for from in into is of on or per than that the their to was were with".split(
    " ",
  ),
);
const singular = (w: string) =>
  w.replace(/ies$/, "y").replace(/([^s])s$/, "$1");
// Unit words for this result: the usual ones and those its columns are named
// with ("order_count" → order, orders).
function unitWords(rows: Row[]) {
  const words = new Set([...units].map(singular));
  for (const key of Object.keys(rows[0] || {}))
    for (const w of columnWords(key).split(" ")) words.add(singular(w));
  return words;
}
// A column's numbers, the words of its name ("order", "count"), its
// integers' exact digits, and whether it holds cents (Stripe's amount_*
// columns, "price_cents"), which a summary may state in dollars.
type Column = {
  values: number[];
  words: Set<string>;
  exact: Set<string>;
  cents: boolean;
};
const digitsOf = (t: string) => t.replace(/^-/, "").replace(/^0+(?=\d)/, "");
const isCents = (key: string) =>
  /(^|_)(cents?|amount)($|_)/i.test(key) || /[a-z]Cents?$/.test(key);
// How the rows back a number: as one of their values (as written, rounded, in
// cents, or as a fraction for a percentage) or how many rows there are, or as
// the difference between two values of a column.
function backing(
  n: number,
  places: number,
  { percent, money }: { percent: boolean; money: boolean },
  values: Column[],
  counts: number[],
): "value" | "difference" | null {
  const tolerance = 0.5 * 10 ** -places + 1e-9,
    near = (v: number, cents = false) =>
      Math.abs(Math.abs(v) - n) <= tolerance ||
      (percent && Math.abs(Math.abs(v) * 100 - n) <= tolerance) ||
      (money && cents && Math.abs(Math.abs(v) / 100 - n) <= tolerance);
  if (
    counts.some((v) => near(v)) ||
    values.some((c) => c.values.some((v) => near(v, c.cents)))
  )
    return "value";
  for (const column of values) {
    const top = column.values.slice(0, 30);
    for (let i = 0; i < top.length; i++)
      for (let j = i + 1; j < top.length; j++)
        if (near(top[i] - top[j], column.cents)) return "difference";
  }
  return null;
}
// A difference is only stated as one: "8 more", "5 orders fewer than".
const comparison =
  /^\s(?:[a-z-]+\s)?(?:more|fewer|less|higher|lower|ahead|behind)\b/;
// The text with each quantity the rows back, and its unit, in bold: "42
// orders", "8 more", "$1,200". Years and numbers the rows don't back stay
// as they are.
export function Emphasized({
  text,
  rows,
  total,
}: {
  text: string;
  rows: Row[];
  total?: number;
}) {
  const parts = useMemo(() => {
    // Keys are left out: an ID that happens to match backs nothing. A Number
    // rounds integers beyond 2^53, so those are compared by their digits.
    const keys = Object.keys(rows[0] || {}).filter((k) => !isKeyColumn(k));
    const values: Column[] = keys
      .map((k) => ({
        values: rows
          .map((r) => toNumber(r[k]))
          .filter(
            (v): v is number =>
              v !== null && (!Number.isInteger(v) || Number.isSafeInteger(v)),
          ),
        words: new Set(columnWords(k).split(" ").map(singular)),
        exact: new Set(
          rows
            .map((r) => String(r[k] ?? "").trim())
            .filter((t) => /^-?\d+$/.test(t))
            .map(digitsOf),
        ),
        cents: isCents(k),
      }))
      .filter((c) => c.values.length || c.exact.size);
    const counts = [rows.length, ...(total === undefined ? [] : [total])];
    const words = unitWords(rows);
    const out: ReactNode[] = [];
    let at = 0;
    for (const m of text.matchAll(quantity)) {
      const [whole, sign, digits, decimals = "", suffix = ""] = m;
      const scale = scales[suffix.trim().toLowerCase()] ?? 1,
        percent = /%|percent/.test(suffix),
        n = Number(digits.replace(/,/g, "") + (decimals && "." + decimals));
      if (
        !sign &&
        !suffix &&
        !decimals &&
        !digits.includes(",") &&
        n >= 1900 &&
        n < 2200
      )
        continue;
      // Its unit: the next word, or the one after a describing word ("1,245
      // active customers").
      let end = m.index + whole.length,
        unit: string | undefined;
      const next = /^\s([a-z][a-z-]*)(?:\s([a-z][a-z-]*))?/.exec(
        text.slice(end),
      );
      if (next && !percent) {
        const [first, second] = [next[1], next[2]];
        if (words.has(singular(first))) {
          end += 1 + first.length;
          unit = singular(first);
        } else if (second && !quiet.has(first) && words.has(singular(second))) {
          end += 2 + first.length + second.length;
          unit = singular(second);
        }
      }
      // A unit that names a column ("42 orders", order_count) is backed only
      // by that column. A row count backs only a plain number.
      const named = unit ? values.filter((c) => c.words.has(unit)) : [],
        sources = named.length ? named : values;
      // A scaled number is compared in its own units ("1.2M" as 1.2 million).
      const rounded = sources.map((c) => ({
        ...c,
        values: c.values.map((v) => v / scale),
      }));
      const backed =
        !decimals && !suffix && n > Number.MAX_SAFE_INTEGER
          ? sources.some((c) => c.exact.has(digitsOf(digits.replace(/,/g, ""))))
            ? "value"
            : null
          : backing(
              n,
              decimals.length,
              { percent, money: !!sign },
              rounded,
              scale === 1 && !sign && !percent && !named.length ? counts : [],
            );
      if (
        !backed ||
        (backed === "difference" &&
          !comparison.test(text.slice(m.index + whole.length)))
      )
        continue;
      out.push(
        text.slice(at, m.index),
        <strong key={m.index}>{text.slice(m.index, end)}</strong>,
      );
      at = end;
    }
    out.push(text.slice(at));
    return out;
  }, [text, rows, total]);
  return <>{parts}</>;
}
