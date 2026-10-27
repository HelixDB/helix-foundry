import { Suspense, lazy, useId, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ChevronDown, ChevronUp } from "lucide-react";
import type { EntityRef } from "../../../shared/foundry-read";
import {
  Button,
  DataGrid,
  Segmented,
  Skeleton,
  formatNumber,
  plural,
  type GridColumn,
} from "../../kit";
import type {
  OntologyQuery,
  QueryOutput,
  VisualKind,
} from "../../../shared/wfe";
import {
  cellText,
  chartHeight,
  visualPlan,
  type VisualCell,
  type VisualTable,
} from "./ask-visuals";

// A query result as Ask shows it (visualPlan): its title, then one figure,
// a chart with its table a toggle away (helix-foundry's Analyst result), or
// the table, then how much of the whole it holds and, in an answer, the way
// to the step that ran its query. Tables are the kit's DataGrid, sortable,
// their first rows shown and the rest a click away; an entity a row names
// links to its page, reading the question's load. Every value renders as
// text, a document's in a muted style, never as markup or a link of its own.

const AskChart = lazy(() =>
  import("./ask-chart").then((m) => ({ default: m.AskChart })),
);

// A chart's bars beyond these are left to the table.
export const MAX_BARS = 15;
const PREVIEW = 8,
  HEAD = 36,
  ROW = 34;

type Href = (r: EntityRef) => string | undefined;
type Row = Record<string, VisualCell>;

/** A cell as text: an entity as a link to its page when it has one. */
function Cell({ cell, href }: { cell: VisualCell; href: Href }) {
  switch (cell.kind) {
    case "entity": {
      const to = href(cell.ref);
      return to ? (
        <Link to={to} className="ak-visual-entity" title={cell.name}>
          {cell.name}
        </Link>
      ) : (
        <span title={cell.name}>{cell.name}</span>
      );
    }
    case "number":
      return <>{formatNumber(cell.value)}</>;
    case "text":
      return (
        <span
          className={cell.untrusted ? "ak-untrusted" : undefined}
          title={cell.untrusted ? `From a document: ${cell.text}` : cell.text}
        >
          {cell.text}
        </span>
      );
    case "none":
      return <span className="hf-null">—</span>;
  }
}

/** What a cell sorts by: a name, a number or text; nothing sorts last. */
const sortValue = (c: VisualCell | undefined) =>
  !c || c.kind === "none" ? null : c.kind === "number" ? c.value : cellText(c);

/** An output's table: sortable, its first rows shown, the rest a click away. */
function ResultTable({
  table,
  label,
  href,
}: {
  table: VisualTable;
  label: string;
  href: Href;
}) {
  const [all, setAll] = useState(false);
  const rows = useMemo(
    () =>
      table.rows.map(
        (cells): Row =>
          Object.fromEntries(cells.map((c, i) => [`c${i}`, c] as const)),
      ),
    [table],
  );
  const columns = useMemo(
    () =>
      table.columns.map(
        (c, i): GridColumn<Row> => ({
          key: `c${i}`,
          label: c.label,
          type: c.numeric ? "number" : "string",
          sortValue: (r) => sortValue(r[`c${i}`]),
          render: (r) => <Cell cell={r[`c${i}`]!} href={href} />,
        }),
      ),
    [table, href],
  );
  const more = rows.length > PREVIEW;
  return (
    <DataGrid
      className="ak-visual-table"
      label={label}
      rows={rows}
      columns={columns}
      rowHeight={ROW}
      maxHeight={more && !all ? HEAD + PREVIEW * ROW + 2 : 440}
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

/**
 * A query's output as a figure, chart or table (visualPlan, by the visual's
 * hint in an answer), titled. `onShowQuery`, in an answer, lights the step
 * that ran the query.
 */
export function AskVisual({
  output,
  query,
  hint,
  href,
  onShowQuery,
  untitled,
}: {
  output: QueryOutput;
  query: OntologyQuery;
  // The visual's kind and title, in an answer.
  hint?: { kind?: VisualKind | null; title?: string | null };
  href: Href;
  onShowQuery?: () => void;
  // Where what it shows is said already (a step's Result): the title names
  // it to assistive technology only.
  untitled?: boolean;
}) {
  const plan = useMemo(
    () => visualPlan(output, query, hint),
    [output, query, hint?.kind, hint?.title],
  );
  const [view, setView] = useState<"chart" | "table">("chart");
  const titleId = useId();
  const chart = plan.kind === "chart" && view === "chart" ? plan.chart : null;
  const cut =
    chart?.kind === "bar" && chart.points.length > MAX_BARS
      ? `The chart shows the first ${MAX_BARS} of ${formatNumber(chart.points.length)}; the table has every one`
      : "";
  const note = [plan.note, cut].filter(Boolean).join(" · ");
  return (
    <figure className="ak-visual" aria-labelledby={titleId}>
      <div className="ak-visual-head">
        <div
          id={titleId}
          className={untitled ? "hf-sr-only" : "ak-visual-title"}
        >
          {plan.title}
        </div>
        {plan.kind === "chart" && (
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
      {plan.kind === "figure" ? (
        <p className="ak-figure">
          <span className="ak-figure-value">{cellText(plan.value)}</span>
          {plan.detail && (
            <span className="ak-figure-detail">{plan.detail}</span>
          )}
        </p>
      ) : chart ? (
        <Suspense
          fallback={
            <Skeleton
              width="100%"
              height={chartHeight(chart, chart.horizontal, MAX_BARS)}
            />
          }
        >
          <AskChart
            chart={
              chart.kind === "bar"
                ? { ...chart, points: chart.points.slice(0, MAX_BARS) }
                : chart
            }
            href={href}
          />
        </Suspense>
      ) : (
        <ResultTable table={plan.table} label={plan.title} href={href} />
      )}
      {(note || onShowQuery) && (
        <div className="ak-visual-foot">
          {note && <span>{note}</span>}
          {onShowQuery && (
            <button
              type="button"
              className="ak-visual-query"
              onClick={onShowQuery}
            >
              Show the query
            </button>
          )}
        </div>
      )}
    </figure>
  );
}
