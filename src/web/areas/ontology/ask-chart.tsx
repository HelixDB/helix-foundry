import {
  useLayoutEffect,
  useRef,
  useState,
  type MouseEvent,
  type ReactNode,
} from "react";
import { useNavigate } from "react-router-dom";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  LabelList,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type { EntityRef } from "../../../shared/foundry-read";
import { calendarDate, cx, formatCompact, formatNumber } from "../../kit";
import {
  CHART_AXIS as AXIS,
  chartHeight,
  type VisualChart,
} from "./ask-visuals";

// An Ask visual's chart, drawn with recharts (lazy-loaded by ask-visual.tsx,
// so recharts stays out of the main bundle): one series in --hf-chart-1,
// as the dataviz rules have it. Bars are at most 20px thick with a 4px
// rounded end, their value at the tip, upright for a few short labels and
// sideways otherwise; a line is 2px over a 10% wash on a time axis, so
// dates far apart are drawn far apart, its ticks at its points' dates, with
// a crosshair. Every mark has a tooltip with its exact value; a bar of
// an entity (and its label) opens the entity's page. Labels are data: they
// render only as text, cut with "…" when long (the tooltip and the table
// have them whole). Colours are ask.css's, from --hf-* tokens.

type Datum = {
  key: string;
  label: string;
  value: number;
  // A line's point: its date's time.
  at?: number;
  detail?: string;
  href?: string;
};

const BAR = 20,
  // Pixels per character of an 11px label, for cutting labels to fit.
  CHAR = 6.4,
  // The narrowest an upright bar's slot may be before bars turn sideways.
  SLOT = 64;

/** An element's width, kept up to date. */
function useWidth() {
  const ref = useRef<HTMLDivElement>(null),
    [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(Math.floor(entry!.contentRect.width)),
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

/** `text` in at most `max` characters, the last an ellipsis if cut. */
const fit = (text: string, max: number) => {
  const chars = [...text];
  return chars.length > max
    ? chars.slice(0, Math.max(1, max - 1)).join("") + "…"
    : text;
};

const DAY = /^(\d{4})-(\d{2})(?:-(\d{2}))?/;
/** A date label as "Oct 1" (or "Oct 1, 2026" when `long`), a month as "Oct 2026". */
function dateText(label: string, long = false) {
  const m = DAY.exec(label);
  if (!m) return label;
  const d = calendarDate(+m[1]!, +m[2]!, m[3] ? +m[3] : 1);
  return d.toLocaleDateString(
    undefined,
    !m[3]
      ? { month: "short", year: "numeric" }
      : long
        ? { month: "short", day: "numeric", year: "numeric" }
        : { month: "short", day: "numeric" },
  );
}

/** The tooltip: the exact value first, then what it is. */
function Tip({
  active,
  payload,
  line,
}: {
  active?: boolean;
  payload?: readonly { payload?: unknown }[];
  line: boolean;
}) {
  const d = active ? (payload?.[0]?.payload as Datum | undefined) : undefined;
  if (!d) return null;
  return (
    <div className="ak-viz-tip">
      <span className="ak-viz-tip-value">{formatNumber(d.value)}</span>
      <span>{line ? dateText(d.label, true) : d.label}</span>
      {d.detail && <span>{d.detail}</span>}
      {d.href && <span className="ak-viz-tip-hint">Click to open</span>}
    </div>
  );
}

/**
 * A category label: text cut to `max` characters, or, for an entity, a link
 * to its page (an SVG link, so the router opens it in place).
 */
function CategoryTick({
  x,
  y,
  payload,
  data,
  sideways,
  max,
  open,
}: {
  x?: number | string;
  y?: number | string;
  payload?: { value?: unknown };
  data: Datum[];
  sideways: boolean;
  max: number;
  open: (d: Datum, e: MouseEvent) => void;
}): ReactNode {
  const d = data[Number(payload?.value)];
  if (!d) return null;
  const text = fit(d.label, max);
  const label = (
    <text
      x={Number(x)}
      y={Number(y)}
      dy={sideways ? "0.32em" : "1.1em"}
      textAnchor={sideways ? "end" : "middle"}
      className="ak-viz-tick"
    >
      {text !== d.label && <title>{d.label}</title>}
      {text}
    </text>
  );
  return d.href ? (
    <a
      href={d.href}
      className="ak-viz-link"
      aria-label={d.label}
      onClick={(e) => open(d, e)}
    >
      {label}
    </a>
  ) : (
    label
  );
}

/** The longest an axis's compact numbers are, in pixels. */
const tickWidth = (values: number[]) =>
  Math.ceil(
    Math.max(...[0, ...values].map((v) => formatCompact(v).length)) * CHAR,
  ) + 12;

export function AskChart({
  chart,
  href,
}: {
  chart: VisualChart;
  href: (r: EntityRef) => string | undefined;
}) {
  const navigate = useNavigate();
  const [box, width] = useWidth();
  const data: Datum[] = chart.points.map((p, i) => ({
    key: String(i),
    label: p.label,
    value: p.value,
    ...("at" in p ? { at: p.at } : {}),
    ...(p.detail ? { detail: p.detail } : {}),
    ...(p.entity && href(p.entity) ? { href: href(p.entity) } : {}),
  }));
  // A plain click opens an entity in place. A modified click on its label
  // is the browser's; on its bar, which is no link, it opens a new tab.
  const open = (d: Datum | undefined, e: MouseEvent, link: boolean) => {
    if (!d?.href) return;
    const modified = e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0;
    if (modified && link) return;
    e.preventDefault();
    if (modified) window.open(d.href, "_blank", "noopener");
    else navigate(d.href);
  };
  const linked = data.some((d) => d.href);
  const values = data.map((d) => d.value);
  // Upright bars turn sideways when their slots are too narrow for labels.
  const across =
    chart.kind === "bar" &&
    (chart.horizontal || (width > 0 && width / data.length < SLOT));
  const tip = (
    <Tooltip
      isAnimationActive={false}
      content={(p) => (
        <Tip
          active={p.active}
          payload={p.payload}
          line={chart.kind === "line"}
        />
      )}
    />
  );
  const grid = <CartesianGrid vertical={across} horizontal={!across} />;
  let body: ReactNode = null;
  if (width > 0 && chart.kind === "line") {
    const labels = new Map(data.map((d) => [d.at, d.label]));
    body = (
      <AreaChart
        width={width}
        height={chartHeight(chart, false, data.length)}
        data={data}
        margin={{ top: 12, right: 16, bottom: 0, left: 0 }}
      >
        {grid}
        <XAxis
          dataKey="at"
          type="number"
          scale="time"
          domain={["dataMin", "dataMax"]}
          ticks={chart.points.map((p) => p.at)}
          tickFormatter={(t) => dateText(labels.get(Number(t)) ?? "")}
          interval="preserveStartEnd"
          minTickGap={24}
          tickLine={false}
          height={AXIS}
        />
        <YAxis
          tickFormatter={(v) => formatCompact(Number(v))}
          width={tickWidth(values)}
          axisLine={false}
          tickLine={false}
        />
        {tip}
        <Area
          type="linear"
          dataKey="value"
          className="ak-viz-line"
          strokeWidth={2}
          dot={false}
          activeDot={{ r: 4, strokeWidth: 2, className: "ak-viz-dot" }}
          isAnimationActive={false}
        />
      </AreaChart>
    );
  } else if (width > 0) {
    // Sideways, the labels take at most 40% of the width.
    const labels = Math.min(
      Math.max(...data.map((d) => [...d.label].length)) * CHAR + 16,
      width * 0.4,
    );
    const tick = (props: object) => (
      <CategoryTick
        {...props}
        data={data}
        sideways={across}
        max={
          across
            ? Math.floor((labels - 16) / CHAR)
            : Math.floor(width / data.length / CHAR)
        }
        open={(d, e) => open(d, e, true)}
      />
    );
    body = (
      <BarChart
        width={width}
        height={chartHeight(chart, across, data.length)}
        data={data}
        layout={across ? "vertical" : "horizontal"}
        margin={{ top: 16, right: across ? 56 : 8, bottom: 0, left: 0 }}
        className={cx(linked && "is-linked")}
      >
        {grid}
        {across ? (
          <>
            <XAxis
              type="number"
              tickFormatter={(v) => formatCompact(Number(v))}
              height={AXIS}
              tickLine={false}
              axisLine={false}
            />
            <YAxis
              type="category"
              dataKey="key"
              width={labels}
              tick={tick}
              interval={0}
              tickLine={false}
            />
          </>
        ) : (
          <>
            <XAxis
              dataKey="key"
              tick={tick}
              interval={0}
              height={AXIS}
              tickLine={false}
            />
            <YAxis
              tickFormatter={(v) => formatCompact(Number(v))}
              width={tickWidth(values)}
              axisLine={false}
              tickLine={false}
            />
          </>
        )}
        {tip}
        <Bar
          dataKey="value"
          className="ak-viz-bar"
          maxBarSize={BAR}
          isAnimationActive={false}
          onClick={(item, _, e) =>
            open(item.payload as Datum | undefined, e, false)
          }
        >
          <LabelList
            dataKey="value"
            position={across ? "right" : "top"}
            className="ak-viz-value"
            formatter={(v) => formatCompact(Number(v))}
          />
        </Bar>
      </BarChart>
    );
  }
  // The height the plan expects is held while the width is measured, and
  // after: bars that turn sideways in a narrow box only ever grow it.
  return (
    <div
      className="ak-viz"
      ref={box}
      style={{
        minHeight: Math.max(
          chartHeight(chart, chart.horizontal, data.length),
          chartHeight(chart, across, data.length),
        ),
      }}
    >
      {body}
    </div>
  );
}
