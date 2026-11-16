import type { EntityRef } from "../../../shared/foundry-read";
import type {
  AskResult,
  AskVisual,
  OntologyQuery,
  QueryExpr,
  QueryOutput,
  VisualKind,
} from "../../../shared/wfe";
import { formatNumber, plural } from "../../kit/util";
import {
  AGGREGATE_FN,
  exprText,
  queryNames,
  queryText,
} from "./ask-model";
import { isRecord } from "../../../shared/json";
import { NO_NAMING, type Naming } from "../explore/naming";

// How Ask draws a query's recorded output (contract 3.11.0): helix-foundry's
// Analyst approach, read from the output's shape rather than from anything
// the model wrote. A count or an aggregate is a figure; groups are a
// ranking or breakdown as bars, or a trend as a line when they are every
// date there is; rows are a table, or bars or a line when the visual asks
// and a label and a number (or a date and a number) are there. A kind the
// shape doesn't suit is a table. Every value is data: the page shows it
// only as text, and an entity a row names links to its page.

/** A value of an output as the page shows it: only ever as text. */
export type VisualCell =
  | { kind: "entity"; ref: EntityRef; name: string }
  | { kind: "number"; value: number }
  // `untrusted`: a document gave it (a row's value; a group's never says).
  | { kind: "text"; text: string; untrusted: boolean }
  | { kind: "none" };

/** An output in columns, named as the query names them. */
export type VisualTable = {
  columns: { label: string; numeric: boolean }[];
  rows: VisualCell[][];
};

/** A bar or a point of a line: its label, number and, for a row's entity, the entity. */
export type VisualPoint = {
  label: string;
  value: number;
  entity?: EntityRef;
  // What else the tooltip says: how many values an aggregate is over.
  detail?: string;
};

/**
 * A chart of one series: bars in the output's order, or a line in time
 * order, each point at its date's time (`at`, ms since the epoch), so dates
 * far apart are drawn far apart.
 */
export type VisualChart = {
  // What the labels and the numbers are, as the table's columns name them.
  label: string;
  measure: string;
  // Bars run sideways when their labels are long or many.
  horizontal: boolean;
} & (
  | { kind: "bar"; points: VisualPoint[] }
  | { kind: "line"; points: (VisualPoint & { at: number })[] }
);

/**
 * How a visual shows its output: one figure; a chart, its table a toggle
 * away; or the table. `note` says how much of the whole the output holds
 * and what the question's size bound cut.
 */
export type VisualPlan = { title: string; note: string } & (
  | { kind: "figure"; value: VisualCell; detail: string }
  | { kind: "chart"; chart: VisualChart; table: VisualTable }
  | { kind: "table"; table: VisualTable }
);

/**
 * A value as the page shows it. With `entity` (a column without a prop) an
 * object with a string type, id and name is that entity; `{"untrusted": …}`
 * is text a document gave; a JSON object or list is its JSON, as text.
 *
 * @example
 * cellOf({ type: "Customer", id: "cus_1", name: "Globex" }, true); // { kind: "entity", ref: { type: "Customer", id: "cus_1" }, name: "Globex" }
 * cellOf({ type: "Customer", name: "Globex" }, true); // { kind: "text", text: "Globex", untrusted: false }
 * cellOf({ untrusted: "<b>Net 30</b>" }); // { kind: "text", text: "<b>Net 30</b>", untrusted: true }
 * cellOf(1200); // { kind: "number", value: 1200 }
 * cellOf(null); // { kind: "none" }
 */
export function cellOf(v: unknown, entity = false): VisualCell {
  if (v === null || v === undefined) return { kind: "none" };
  if (typeof v === "number")
    return Number.isFinite(v)
      ? { kind: "number", value: v }
      : { kind: "text", text: String(v), untrusted: false };
  if (typeof v === "string") return { kind: "text", text: v, untrusted: false };
  if (typeof v === "boolean")
    return { kind: "text", text: String(v), untrusted: false };
  if (
    entity &&
    isRecord(v) &&
    typeof v.type === "string" &&
    typeof v.id === "string" &&
    typeof v.name === "string"
  )
    return { kind: "entity", ref: { type: v.type, id: v.id }, name: v.name };
  // A public snapshot's entity names no id (dashboard.Public): its name,
  // with nothing to open.
  if (
    entity &&
    isRecord(v) &&
    Object.keys(v).length === 2 &&
    typeof v.type === "string" &&
    typeof v.name === "string"
  )
    return { kind: "text", text: v.name, untrusted: false };
  if (isRecord(v) && Object.keys(v).length === 1 && "untrusted" in v)
    return {
      kind: "text",
      text:
        typeof v.untrusted === "string"
          ? v.untrusted
          : JSON.stringify(v.untrusted),
      untrusted: true,
    };
  return { kind: "text", text: JSON.stringify(v), untrusted: false };
}

/**
 * A cell in words: an entity's name, a number in full, the text, or "—".
 *
 * @example
 * cellText({ kind: "number", value: 1200000 }); // "1,200,000"
 */
export const cellText = (c: VisualCell): string =>
  c.kind === "entity"
    ? c.name
    : c.kind === "number"
      ? formatNumber(c.value)
      : c.kind === "text"
        ? c.text
        : "—";

const DATE = /^\d{4}-\d{2}(?:-\d{2}(?:[T ][\d:.]+(?:Z|[+-]\d{2}:?\d{2})?)?)?$/;
/** A date's time in ms, NaN for text that isn't one ("2026-10-01 09:00" too). */
const timeOf = (text: string) =>
  DATE.test(text) ? Date.parse(text.replace(" ", "T")) : NaN;
/** A number as JSON writes it: how the engine writes a group's number. */
const NUMBER = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/;
/** Whether a column's cells are dates, at least two of them. */
const dates = (cells: VisualCell[]) =>
  cells.filter((c) => c.kind !== "none").length >= 2 &&
  cells.every(
    (c) =>
      c.kind === "none" ||
      (c.kind === "text" && Number.isFinite(timeOf(c.text))),
  );
const numeric = (cells: VisualCell[]) =>
  cells.some((c) => c.kind === "number") &&
  cells.every((c) => c.kind === "number" || c.kind === "none");

/** Most bars a chart shows before they turn sideways. */
const UPRIGHT_BARS = 8,
  UPRIGHT_LABEL = 12;

/**
 * A chart of `label` against `measure`: bars in the order given, or a line
 * oldest first, its points' labels dates. Points without a number are left
 * out; fewer than two is no chart (one bar says less than its figure).
 */
function chartOf(
  kind: "bar" | "line",
  points: VisualPoint[],
  label: string,
  measure: string,
): VisualChart | undefined {
  if (points.length < 2) return undefined;
  if (kind === "line")
    return {
      kind,
      // Dates in time order, whatever their precision or offset.
      points: points
        .map((p) => ({ ...p, at: timeOf(p.label) }))
        .sort((a, b) => a.at - b.at || a.label.localeCompare(b.label)),
      label,
      measure,
      horizontal: false,
    };
  return {
    kind,
    points,
    label,
    measure,
    horizontal:
      points.length > UPRIGHT_BARS ||
      points.some((p) => [...p.label].length > UPRIGHT_LABEL),
  };
}

/**
 * Why a line wasn't drawn: an output that leaves some of its dates out
 * (the most or greatest first, or rows not in their dates' order) would
 * join the dates it holds as if nothing were between them.
 */
const PARTIAL_LINE = "A line needs every date, and this leaves some out";

// A chart's plot and axis band, and each sideways bar's band, in pixels.
export const CHART_PLOT = 220,
  CHART_AXIS = 28,
  CHART_BAND = 30;

/**
 * How tall a chart is drawn: a sideways bar chart by its bars (at most
 * `bars` of them), anything else at the plot's height; with its axis.
 *
 * @example
 * chartHeight({ kind: "bar", points: [], label: "", measure: "", horizontal: true }, true, 15); // 28
 */
export const chartHeight = (
  chart: VisualChart,
  sideways: boolean,
  bars: number,
) =>
  (chart.kind === "bar" && sideways
    ? Math.min(chart.points.length, bars) * CHART_BAND
    : CHART_PLOT) + CHART_AXIS;

/** How much of the whole the output holds, and what the bound cut. */
function noteOf(shown: number, total: number, noun: string, cut?: string) {
  return [
    total > shown
      ? `${formatNumber(shown)} of ${plural(total, noun)}`
      : plural(shown, noun),
    cut,
  ]
    .filter(Boolean)
    .join(" · ");
}

/**
 * How a visual heads a query's columns, in words: a position by its type
 * (a type at more than one position gets its position: "Customer (2)"),
 * its entity or its names prop (`name`, or one the version's `names` lists)
 * by the type alone, and any other prop after the type, its underscores
 * spaces ("Invoice amount due"). `entityShown`: the entity's own column is
 * in the table too, so its names prop keeps the prop ("Customer name"),
 * never a second "Customer". Only headings change: columns keep their
 * place and their cells.
 *
 * @example
 * const heading = columnHeadings({
 *   start: { type: "Invoice" },
 *   steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
 *   return: { count: {} },
 * });
 * heading({ at: 1, prop: "name" }); // "Customer"
 * heading({ at: 1, prop: "name" }, true); // "Customer name"
 * heading({ at: 0, prop: "amount_due" }); // "Invoice amount due"
 * heading({ at: 0 }); // "Invoice"
 */
export function columnHeadings(q: OntologyQuery, naming: Naming = NO_NAMING) {
  const { at } = queryNames(q);
  const types = [q.start.type, ...(q.steps || []).map((s) => s.to)];
  const words = (s: string) => s.replace(/_/g, " ");
  const where = (i: number) => at(i).replace(/@(\d+)$/, " ($1)");
  return (
    c: { at: number; prop?: string | null; expr?: QueryExpr | null },
    entityShown = false,
  ): string => {
    if (c.expr) return `${where(c.at)}: ${words(exprText(c.expr))}`;
    if (!c.prop) return where(c.at);
    const t = types[c.at];
    const names =
      t === undefined
        ? false
        : c.prop === "name" || !!naming.types.get(t)?.names.includes(c.prop);
    return names && !entityShown
      ? where(c.at)
      : `${where(c.at)} ${words(c.prop)}`;
  };
}

/**
 * How to show a query's output, by its shape and the visual's `kind` hint
 * (left out, the shape decides; one the shape doesn't suit is a table),
 * titled by the visual or, without a title, by what the query returns, its
 * columns headed by columnHeadings with `naming`, the version's.
 *
 * - count, aggregate: a figure;
 * - groups (group_count, grouped_aggregate): bars, most first as the
 *   output orders them, or a line when every value is a date and the
 *   output holds every group;
 * - rows: a table; bars of the first entity (else text) column against the
 *   first number column when the hint is `bar`, a line of a date column
 *   against it when `line` and the output holds every row or orders them
 *   by that column.
 *
 * A group's value is text the engine wraps as a document's whatever gave
 * it, so it isn't shown as a document's, and a column of numbers in JSON's
 * writing is a number column, each shown as written.
 *
 * @example
 * const q = { start: { type: "Invoice" }, return: { group_count: { at: 0, prop: "status" } } } as const;
 * const plan = visualPlan(
 *   { kind: "group_count", groups: [{ value: { untrusted: "paid" }, count: 40 }, { value: null, count: 3 }], total_groups: 2 },
 *   q,
 * );
 * plan.kind; // "chart"
 * plan.kind === "chart" && plan.chart.points; // [{ label: "paid", value: 40 }, { label: "No status", value: 3 }]
 * plan.kind === "chart" && plan.chart.label; // "Invoice status"
 * plan.title; // "Count by Invoice.status"
 * visualPlan({ kind: "count", count: 12 }, { start: { type: "Invoice" }, return: { count: {} } }, { kind: "bar" }).kind; // "table"
 */
export function visualPlan(
  output: QueryOutput,
  query: OntologyQuery,
  hint: { kind?: VisualKind | null; title?: string | null } = {},
  naming: Naming = NO_NAMING,
): VisualPlan {
  const col = columnHeadings(query, naming);
  const text = queryText(query);
  const r = query.return;
  const cut = output.cut || undefined;
  const want = hint.kind ?? undefined;
  const fits = (k: VisualKind) => !want || want === k;
  switch (output.kind) {
    case "count":
    case "aggregate": {
      const title = hint.title || text.returns;
      const value =
        output.kind === "count" ? output.count : (output.value ?? null);
      const detail =
        output.kind === "aggregate"
          ? overValues(output.values, output.skipped)
          : "";
      const skipped = output.kind === "aggregate" && !!output.skipped;
      const cell = cellOf(value);
      if (fits("figure"))
        return { kind: "figure", title, note: cut ?? "", value: cell, detail };
      return {
        kind: "table",
        title,
        note: cut ?? "",
        table: {
          columns: [
            { label: text.returns, numeric: true },
            ...(output.kind === "aggregate"
              ? [{ label: "Values", numeric: true }]
              : []),
            ...(skipped ? [{ label: "Skipped", numeric: true }] : []),
          ],
          rows: [
            [
              cell,
              ...(output.kind === "aggregate" ? [cellOf(output.values)] : []),
              ...(output.kind === "aggregate" && skipped
                ? [cellOf(output.skipped)]
                : []),
            ],
          ],
        },
      };
    }
    case "group_count":
    case "grouped_aggregate": {
      // The column grouped by, and what each group measures.
      const by =
        "group_count" in r
          ? r.group_count
          : "aggregate" in r && r.aggregate.group_by
            ? r.aggregate.group_by
            : undefined;
      const label = by ? col(by) : "Value",
        none = by ? `No ${by.prop.replace(/_/g, " ")}` : "None";
      const measure =
        output.kind === "group_count"
          ? "Count"
          : "aggregate" in r
            ? `${AGGREGATE_FN[r.aggregate.fn]} of ${col(r.aggregate)}`
            : "Aggregate";
      // The engine wraps every group's value as `{"untrusted": text}`, a
      // number's too: a group is many entities', a document's as much as
      // any. The wrapper says nothing of where one came from.
      const values = output.groups.map((g): VisualCell => {
        const c = cellOf(g.value);
        return c.kind === "text" ? { ...c, untrusted: false } : c;
      });
      const measures = output.groups.map((g) =>
        cellOf("count" in g ? g.count : g.aggregate),
      );
      // A computed measure's groups say how many entities had none of it.
      const skipped = output.groups.some((g) => "skipped" in g && !!g.skipped);
      const table: VisualTable = {
        columns: [
          {
            label,
            numeric:
              values.some((c) => c.kind !== "none") &&
              values.every(
                (c) =>
                  c.kind === "none" ||
                  c.kind === "number" ||
                  (c.kind === "text" && NUMBER.test(c.text)),
              ),
          },
          { label: measure, numeric: true },
          ...(output.kind === "grouped_aggregate"
            ? [{ label: "Values", numeric: true }]
            : []),
          ...(skipped ? [{ label: "Skipped", numeric: true }] : []),
        ],
        rows: output.groups.map((g, i) => [
          values[i]!,
          measures[i]!,
          ...("values" in g ? [cellOf(g.values)] : []),
          ...(skipped ? [cellOf("skipped" in g ? (g.skipped ?? 0) : 0)] : []),
        ]),
      };
      // A line has a point per date: the entities without one are in its
      // table, not on it.
      const pointsOf = (dated: boolean) =>
        output.groups.flatMap((g, i): VisualPoint[] => {
          const m = measures[i]!,
            v = values[i]!;
          if (m.kind !== "number" || (dated && v.kind === "none")) return [];
          return [
            {
              label: v.kind === "none" ? none : cellText(v),
              value: m.value,
              ...("values" in g
                ? { detail: overValues(g.values, g.skipped) }
                : {}),
            },
          ];
        });
      // Dates are a trend only all together: the output holds the most
      // (or greatest) first, at most 100 of them.
      const dated = dates(values),
        partial = dated && output.groups.length < output.total_groups,
        line = dated && !partial;
      const chart =
        (line &&
          fits("line") &&
          chartOf("line", pointsOf(true), label, measure)) ||
        ((want === "bar" || (!want && !line)) &&
          chartOf("bar", pointsOf(false), label, measure)) ||
        undefined;
      const title = hint.title || text.returns;
      const note = [
        noteOf(output.groups.length, output.total_groups, "group", cut),
        partial && fits("line") ? PARTIAL_LINE : "",
      ]
        .filter(Boolean)
        .join(" · ");
      return chart && want !== "table"
        ? { kind: "chart", title, note, chart, table }
        : { kind: "table", title, note, table };
    }
    case "rows": {
      // A query that returns no rows names no columns: each is named by
      // its place.
      const columns = "rows" in r ? r.rows.columns : [];
      const width = Math.max(
        columns.length,
        ...output.rows.map((row) => row.cells.length),
      );
      // A column with neither a prop nor a computed number is its entity.
      const rows = output.rows.map((row) =>
        Array.from({ length: width }, (_, i) =>
          cellOf(row.cells[i], !columns[i]?.prop && !columns[i]?.expr),
        ),
      );
      const cells = (i: number) => rows.map((row) => row[i]!);
      // A position whose entity has a column of its own.
      const entities = new Set(
        columns.filter((c) => !c.prop && !c.expr).map((c) => c.at),
      );
      const table: VisualTable = {
        columns: Array.from({ length: width }, (_, i) => ({
          label: columns[i]
            ? col(columns[i], entities.has(columns[i].at))
            : `Column ${i + 1}`,
          numeric: numeric(cells(i)),
        })),
        rows,
      };
      const title = hint.title || text.path;
      const note = noteOf(output.rows.length, output.total, "row", cut);
      const measure = table.columns.findIndex((c) => c.numeric);
      const labelAt = (want: (c: VisualCell[]) => boolean) =>
        table.columns.findIndex((c, i) => i !== measure && want(cells(i)));
      const label =
        want === "line"
          ? labelAt(dates)
          : want === "bar"
            ? [
                labelAt((c) => c.some((x) => x.kind === "entity")),
                labelAt((c) => !numeric(c)),
              ].find((i) => i >= 0)
            : undefined;
      if (
        (want === "bar" || want === "line") &&
        measure >= 0 &&
        label !== undefined &&
        label >= 0
      ) {
        // Some of the rows are a trend only when they are those with the
        // first or last dates.
        const by = "rows" in r ? r.rows.order_by : undefined,
          dated = columns[label];
        if (
          want === "line" &&
          output.rows.length < output.total &&
          !(by && dated && by.at === dated.at && by.prop === dated.prop)
        )
          return {
            kind: "table",
            title,
            note: `${note} · ${PARTIAL_LINE}`,
            table,
          };
        const points = rows.flatMap((row): VisualPoint[] => {
          const l = row[label]!,
            m = row[measure]!;
          return m.kind === "number" && l.kind !== "none"
            ? [
                {
                  label: cellText(l),
                  value: m.value,
                  ...(l.kind === "entity" ? { entity: l.ref } : {}),
                },
              ]
            : [];
        });
        const chart = chartOf(
          want,
          points,
          table.columns[label]!.label,
          table.columns[measure]!.label,
        );
        if (chart) return { kind: "chart", title, note, chart, table };
      }
      return { kind: "table", title, note, table };
    }
  }
}

/**
 * What an aggregate is over: its values, and the entities a computed
 * measure had none of, when any.
 *
 * @example
 * overValues(3); // "Over 3 values"
 * overValues(2, 1); // "Over 2 values · 1 without one, skipped"
 */
export function overValues(values: number, skipped?: number): string {
  return (
    `Over ${plural(values, "value")}` +
    (skipped ? ` · ${skipped} without one, skipped` : "")
  );
}

/** A visual the page can draw: its query, and the output it shows. */
export type Drawable = {
  visual: AskVisual;
  query: OntologyQuery;
  output: QueryOutput;
};

/**
 * An answer's visuals that can be drawn, in order, each with its query and
 * output. A visual whose result has no output is left out: lists of
 * questions leave outputs out (GET of one question has them), and a result
 * the question's size bound dropped has none.
 *
 * @example
 * drawable({
 *   status: "answered",
 *   answer: { blocks: [{ para: "Most are paid [c1]." }], visuals: [{ evidence_id: "e1" }, { evidence_id: "e2" }] },
 *   citations: [],
 *   queries: [
 *     { evidence_id: "e1", query: { start: { type: "Invoice" }, return: { count: {} } }, output: { kind: "count", count: 3 } },
 *     { evidence_id: "e2", query: { start: { type: "Invoice" }, return: { count: {} } } },
 *   ],
 *   usage: { input: 0, cache_read: 0, cache_write: 0, output: 0, usd: 0 },
 * }).map((d) => d.visual.evidence_id); // ["e1"]
 */
export function drawable(result: Extract<AskResult, { status: "answered" }>) {
  const byId = new Map(result.queries.map((q) => [q.evidence_id, q]));
  return (result.answer.visuals ?? []).flatMap((visual): Drawable[] => {
    const q = byId.get(visual.evidence_id);
    return q?.output ? [{ visual, query: q.query, output: q.output }] : [];
  });
}

/**
 * Visuals by the block they follow: `placed[i]` follow block i, in order. A
 * visual without a block, or past the last, follows the last; with no
 * blocks, all of them follow "block 0".
 *
 * @example
 * placeVisuals([{ visual: { evidence_id: "e1", after: 0 } }, { visual: { evidence_id: "e2" } }], 2)
 *   .map((p) => p.map((d) => d.visual.evidence_id)); // [["e1"], ["e2"]]
 */
export function placeVisuals<D extends { visual: AskVisual }>(
  visuals: readonly D[],
  blocks: number,
): D[][] {
  const last = Math.max(0, blocks - 1);
  return Array.from({ length: last + 1 }, (_, i) =>
    visuals.filter((d) => Math.min(d.visual.after ?? last, last) === i),
  );
}
