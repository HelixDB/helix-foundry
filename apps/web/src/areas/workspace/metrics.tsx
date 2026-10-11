import {
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
} from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight, CircleAlert, RefreshCw, Sparkles } from "lucide-react";
import type {
  BusinessInsights,
  HomeMetric,
  HomeMetrics,
} from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useWorkspace } from "../../ui";
import { Button, Panel, Skeleton, StatCard, calendarDate, cx } from "../../kit";
import { paths } from "../../paths";

// `text` is the exact value, in the metric's format, when `value` cannot hold
// it (an integer beyond 2^53 from an answer).
type Point = { label: string; value: number; text?: string };
type Format = HomeMetric["format"];

const usdWhole = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    maximumFractionDigits: 0,
  }),
  usdCents = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }),
  usdCompact = new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    notation: "compact",
    maximumFractionDigits: 1,
  }),
  compact = new Intl.NumberFormat("en-US", {
    notation: "compact",
    maximumFractionDigits: 1,
  }),
  oneDecimal = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 }),
  twoDecimals = new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 });

// A metric value for display: currency is already in major units. Big
// numbers are compact ("$12.4K") unless `exact`, as tooltips show them.
function formatMetric(
  v: number | null | undefined,
  format: Format,
  exact = false,
) {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  const plain = Math.abs(v) < 10 ? twoDecimals : oneDecimal,
    big = !exact && Math.abs(v) >= 10000;
  if (format === "currency")
    return big
      ? usdCompact.format(v)
      : Number.isInteger(v) || Math.abs(v) >= 10000
        ? usdWhole.format(v)
        : usdCents.format(v);
  if (format === "percent") return plain.format(v) + "%";
  return big ? compact.format(v) : plain.format(v);
}
// A point's value in full, as tooltips and screen readers give it.
const exactText = (p: Point, format: Format) =>
  p.text ?? formatMetric(p.value, format, true);
// Shorter still, for axis ticks.
const formatTick = (v: number, format: Format) =>
  format === "currency"
    ? usdCompact.format(v)
    : compact.format(v) + (format === "percent" ? "%" : "");

// "YYYY-MM" or "YYYY-MM-DD" as a local date (no UTC shift).
function parseLabel(label: string) {
  const m = /^(\d{4})-(\d{2})(?:-(\d{2}))?$/.exec(label);
  return m
    ? { date: calendarDate(+m[1], +m[2], m[3] ? +m[3] : 1), day: !!m[3] }
    : null;
}
// Axis text ("Jan 26", "Mar 3") or tooltip text ("January 2026", "Week of Mar 3, 2026").
function pointLabel(label: string, grain: HomeMetric["grain"], long = false) {
  const p = parseLabel(label);
  if (!p) return label;
  if (!p.day)
    return p.date.toLocaleDateString(
      undefined,
      long
        ? { month: "long", year: "numeric" }
        : { month: "short", year: "2-digit" },
    );
  if (!long)
    return p.date.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
    });
  const text = p.date.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  });
  return grain === "week" ? `Week of ${text}` : text;
}
// Where a label sits in time (months, or days), so a bucket with no rows
// leaves a gap on the axis instead of squeezing its neighbours together.
function timeOf(label: string) {
  const p = parseLabel(label);
  return !p
    ? null
    : p.day
      ? Math.round(p.date.getTime() / 86400000)
      : p.date.getFullYear() * 12 + p.date.getMonth();
}
function shortDate(iso: string) {
  const p = parseLabel(iso);
  if (!p) return iso;
  return p.date.toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    ...(p.date.getFullYear() !== new Date().getFullYear()
      ? { year: "numeric" }
      : {}),
  });
}

// Three round gridlines (two intervals) that always include zero.
function niceTicks(min: number, max: number) {
  const lo = Math.min(0, min);
  let hi = Math.max(0, max);
  if (hi === lo) hi = lo + 1;
  const raw = (hi - lo) / 2,
    mag = 10 ** Math.floor(Math.log10(raw));
  for (const k of [1, 2, 2.5, 5, 10, 20]) {
    const step = k * mag,
      start = Math.floor(lo / step) * step;
    if (start + 2 * step >= hi - step * 1e-9)
      return [start, start + step, start + 2 * step];
  }
  return [lo, (lo + hi) / 2, hi];
}

// Tracks an element's content width.
function useWidth<T extends HTMLElement>() {
  const ref = useRef<T>(null),
    [width, setWidth] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth);
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(entry.contentRect.width),
    );
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, width] as const;
}

// Arrow keys move the highlighted point; Home/End jump to the ends.
function onStepKey(
  e: KeyboardEvent,
  count: number,
  keys: [string, string],
  set: (update: (active: number | null) => number) => void,
) {
  const step: ((at: number) => number) | null =
    e.key === keys[0]
      ? (at) => Math.max(0, at - 1)
      : e.key === keys[1]
        ? (at) => Math.min(count - 1, at + 1)
        : e.key === "Home"
          ? () => 0
          : e.key === "End"
            ? () => count - 1
            : null;
  if (!step) return;
  e.preventDefault();
  set((active) => step(active ?? count - 1));
}

const H = 184,
  PAD_T = 10,
  PAD_B = 24,
  PAD_R = 10;

function TrendChart({ m, points }: { m: HomeMetric; points: Point[] }) {
  const [box, width] = useWidth<HTMLDivElement>();
  const [active, setActive] = useState<number | null>(null),
    [focused, setFocused] = useState(false);
  const n = points.length,
    values = points.map((p) => p.value);
  const ticks = niceTicks(Math.min(...values), Math.max(...values)),
    tickText = ticks.map((t) => formatTick(t, m.format));
  const lo = ticks[0],
    hi = ticks[2];
  const left = Math.ceil(Math.max(...tickText.map((t) => t.length)) * 6.6) + 8,
    w = Math.max(1, width - left - PAD_R),
    h = H - PAD_T - PAD_B;
  // Each point's share of the way along the x axis.
  const times = points.map((p) => timeOf(p.label)),
    t0 = times[0],
    tN = times[n - 1];
  const along =
    t0 !== null && tN !== null && tN > t0 && times.every((t) => t !== null)
      ? times.map((t) => (t! - t0) / (tN - t0))
      : points.map((_, i) => (n > 1 ? i / (n - 1) : 0.5));
  const nearest = (f: number) =>
    along.reduce(
      (best, p, i) => (Math.abs(p - f) < Math.abs(along[best] - f) ? i : best),
      0,
    );
  const x = (i: number) => left + along[i] * w,
    y = (v: number) => PAD_T + h - ((v - lo) / (hi - lo)) * h;
  // Buckets with no rows are missing, not zero (an average has no value
  // there): the line breaks and a faint dashed bridge spans the gap.
  const gapBefore = (i: number) => {
    const a = times[i - 1],
      b = times[i];
    return a !== null && b !== null && b - a > (m.grain === "week" ? 7 : 1);
  };
  const xy = (i: number) =>
    `${x(i).toFixed(1)},${y(points[i].value).toFixed(1)}`;
  const trace = points.map((_, i) => (i ? "L" : "M") + xy(i)).join(""),
    line = points
      .map((_, i) => (i && !gapBefore(i) ? "L" : "M") + xy(i))
      .join(""),
    bridges = points
      .map((_, i) => (i && gapBefore(i) ? `M${xy(i - 1)}L${xy(i)}` : ""))
      .join(""),
    base = y(0).toFixed(1),
    area = `${trace}L${x(n - 1).toFixed(1)},${base}L${x(0).toFixed(1)},${base}Z`;
  const xTicks = [...new Set([0, nearest(0.5), n - 1])];
  const peak = values.indexOf(Math.max(...values)),
    first = points[0],
    last = points[n - 1];
  const summary =
    `${m.title} by ${m.grain || "period"}: from ${formatMetric(first.value, m.format)} in ${pointLabel(first.label, m.grain, true)}` +
    ` to ${formatMetric(last.value, m.format)} in ${pointLabel(last.label, m.grain, true)}` +
    (peak !== 0 && peak !== n - 1
      ? `, peaking at ${formatMetric(points[peak].value, m.format)} in ${pointLabel(points[peak].label, m.grain, true)}`
      : "");
  const pick = (clientX: number, el: Element) => {
    const r = el.getBoundingClientRect();
    setActive(nearest((clientX - r.left - left) / w));
  };
  const a = active === null ? null : points[active];
  const ax = active === null ? 0 : x(active);
  return (
    <div className="wk-trend" ref={box}>
      <div
        className="wk-chart-focus"
        role="img"
        aria-label={summary}
        tabIndex={0}
        onFocus={() => {
          setFocused(true);
          setActive((i) => i ?? n - 1);
        }}
        onBlur={() => {
          setFocused(false);
          setActive(null);
        }}
        onKeyDown={(e) => {
          onStepKey(e, n, ["ArrowLeft", "ArrowRight"], setActive);
        }}
      >
        {width > 0 && (
          <svg
            width={width}
            height={H}
            aria-hidden
            onPointerMove={(e) => pick(e.clientX, e.currentTarget)}
            onPointerDown={(e) => pick(e.clientX, e.currentTarget)}
            onPointerLeave={() => setActive(null)}
          >
            {ticks.map((t, i) => (
              <g key={i}>
                <line
                  className={cx("wk-grid-line", t === 0 && "is-base")}
                  x1={left}
                  x2={left + w}
                  y1={Math.round(y(t)) + 0.5}
                  y2={Math.round(y(t)) + 0.5}
                />
                <text
                  className="wk-axis"
                  x={left - 8}
                  y={y(t)}
                  dy="0.32em"
                  textAnchor="end"
                >
                  {tickText[i]}
                </text>
              </g>
            ))}
            {xTicks.map((i, k) => (
              <text
                key={i}
                className="wk-axis"
                x={x(i)}
                y={H - 6}
                textAnchor={
                  n === 1 || (k > 0 && k < xTicks.length - 1)
                    ? "middle"
                    : k === 0
                      ? "start"
                      : "end"
                }
              >
                {pointLabel(points[i].label, m.grain)}
              </text>
            ))}
            <path className="wk-trend-area" d={area} />
            <path className="wk-trend-line" d={line} />
            {bridges && <path className="wk-trend-bridge" d={bridges} />}
            {active !== null && (
              <line
                className="wk-crosshair"
                x1={Math.round(ax) + 0.5}
                x2={Math.round(ax) + 0.5}
                y1={PAD_T}
                y2={PAD_T + h}
              />
            )}
            <circle
              className="wk-trend-dot"
              r={4}
              cx={active === null ? x(n - 1) : ax}
              cy={y((a || last).value)}
            />
            {/* The full plot is the hit target, so the pointer finds the nearest X. */}
            <rect
              className="wk-trend-hit"
              x={left}
              y={0}
              width={w}
              height={H}
            />
          </svg>
        )}
      </div>
      {a && (
        <div
          className="wk-chart-tip"
          aria-hidden
          style={{
            left: ax,
            top: PAD_T,
            transform:
              ax > width / 2
                ? "translateX(calc(-100% - 10px))"
                : "translateX(10px)",
          }}
        >
          <strong>{exactText(a, m.format)}</strong>
          <span>{pointLabel(a.label, m.grain, true)}</span>
        </div>
      )}
      <span className="hf-sr-only" aria-live="polite">
        {focused && a
          ? `${pointLabel(a.label, m.grain, true)}: ${exactText(a, m.format)}`
          : ""}
      </span>
    </div>
  );
}

function BreakdownChart({ m, points }: { m: HomeMetric; points: Point[] }) {
  const [active, setActive] = useState<number | null>(null),
    [focused, setFocused] = useState(false);
  const max = Math.max(0, ...points.map((p) => p.value));
  const summary = `${m.title}: ${points
    .map((p) => `${p.label} ${formatMetric(p.value, m.format)}`)
    .join(", ")}`;
  const a = active === null ? null : points[active];
  return (
    <div className="wk-breakdown">
      <div
        className="wk-bars wk-chart-focus"
        role="img"
        aria-label={summary}
        tabIndex={0}
        onPointerLeave={() => setActive(null)}
        onFocus={() => {
          setFocused(true);
          setActive((i) => i ?? 0);
        }}
        onBlur={() => {
          setFocused(false);
          setActive(null);
        }}
        onKeyDown={(e) => {
          onStepKey(e, points.length, ["ArrowUp", "ArrowDown"], setActive);
        }}
      >
        {points.map((p, i) => {
          const pct = max > 0 ? (Math.max(0, p.value) / max) * 100 : 0;
          return (
            <div
              key={i}
              className={cx("wk-bar-row", active === i && "is-active")}
              onPointerEnter={() => setActive(i)}
            >
              <span className="wk-bar-label">{p.label}</span>
              <span className="wk-bar-track">
                <span
                  className="wk-bar"
                  style={{ width: `${pct}%`, minWidth: pct > 0 ? 2 : 0 }}
                />
              </span>
              <span className="wk-bar-value hf-num">
                {formatMetric(p.value, m.format)}
              </span>
              {/* Above the row, from its start: it always fits the card and
                  carries the full name when the label is cut short. */}
              {active === i && (
                <span className="wk-chart-tip">
                  <strong>{exactText(p, m.format)}</strong>
                  <span>{p.label}</span>
                </span>
              )}
            </div>
          );
        })}
      </div>
      <span className="hf-sr-only" aria-live="polite">
        {focused && a ? `${a.label}: ${exactText(a, m.format)}` : ""}
      </span>
    </div>
  );
}

function KpiTile({ m }: { m: HomeMetric }) {
  const r = m.result;
  if (r.error)
    return (
      <div className="wk-metric-tile" title={r.error}>
        <StatCard
          label={m.title}
          value={<span className="wk-metric-na">Couldn’t compute</span>}
          hint={m.dataset}
        />
      </div>
    );
  const value = r.value ?? null,
    previous = r.previous;
  let delta: Parameters<typeof StatCard>[0]["delta"];
  if (
    typeof value === "number" &&
    typeof previous === "number" &&
    previous > 0
  ) {
    const pct = Math.round(((value - previous) / previous) * 100);
    delta = {
      value: `${pct > 0 ? "+" : pct < 0 ? "−" : ""}${Math.abs(pct)}% vs prior 30 days`,
      trend: pct > 0 ? "up" : pct < 0 ? "down" : "flat",
      // Growth reads as good; other directions aren't guessed.
      good: pct === 0 ? undefined : pct > 0,
    };
  }
  const hint = m.window
    ? "Last 30 days" + (r.asOf ? " to " + shortDate(r.asOf) : "")
    : m.dataset;
  return (
    <div className="wk-metric-tile" title={m.why || undefined}>
      <StatCard
        label={m.title}
        value={formatMetric(value, m.format)}
        delta={delta}
        hint={
          <>
            {hint}
            {m.reference && <> · {m.reference}</>}
            {m.why && <span className="hf-sr-only">. {m.why}</span>}
          </>
        }
      />
    </div>
  );
}

function ChartCard({ m }: { m: HomeMetric }) {
  const r = m.result,
    points = r.points || [],
    last = points[points.length - 1];
  return (
    <Panel
      className="wk-metric-card"
      title={m.title}
      subtitle={m.why || `From ${m.dataset}`}
      actions={
        m.kind === "trend" && last && !r.error ? (
          <div className="wk-metric-latest">
            <strong>{formatMetric(last.value, m.format)}</strong>
            <small>{pointLabel(last.label, m.grain, true)}</small>
          </div>
        ) : undefined
      }
    >
      {r.error ? (
        <p className="wk-metric-na" title={r.error}>
          Couldn’t compute
        </p>
      ) : !points.length ? (
        <p className="wk-metric-na">No data yet</p>
      ) : m.kind === "trend" ? (
        <TrendChart m={m} points={points} />
      ) : (
        <BreakdownChart m={m} points={points} />
      )}
    </Panel>
  );
}

// Tiles in rows of up to four; 5 or 6 sit three across so no row is lonely.
const kpiColumns = (count: number) =>
  count <= 4 ? Math.max(2, count) : count <= 6 ? 3 : 4;

function MetricsSkeleton() {
  return (
    <div role="status" aria-label="Loading key metrics">
      <div className="wk-metrics-kpis">
        {[0, 1, 2, 3].map((i) => (
          <StatCard
            key={i}
            label={<Skeleton width={96} />}
            value=""
            loading
            hint={<Skeleton width={120} />}
          />
        ))}
      </div>
      <div className="wk-metrics-charts">
        {[0, 1].map((i) => (
          <Panel
            key={i}
            className="wk-metric-card"
            title={<Skeleton width={120} height={13} />}
            subtitle={<Skeleton width={220} />}
          >
            <Skeleton width="100%" height={H - 20} />
          </Panel>
        ))}
      </div>
    </div>
  );
}

// Home's "Key metrics": numbers the AI (or code, until AI is set up) picked
// from this workspace's data. Re-runs by itself when the data changes.
export function KeyMetrics() {
  const { id, role, notify } = useWorkspace();
  const qc = useQueryClient(),
    [pending, setPending] = useState(false);
  const query = useQuery({
    queryKey: [id, "home-metrics"],
    queryFn: () => api<HomeMetrics>(`/workspaces/${id}/home/metrics`),
    refetchInterval: (q) => (q.state.data?.generating ? 5000 : false),
  });
  // Suggestions from the industry playbook live on the Ontology page; Home
  // only points at them, in one quiet line.
  const business = useQuery({
    queryKey: [id, "business"],
    queryFn: () => api<BusinessInsights>(`/workspaces/${id}/business`),
    staleTime: 60000,
  });
  const suggestions = business.data?.suggestions.length || 0;
  const data = query.data;
  if (data?.status === "empty") return null;
  if (!data && query.error)
    return (
      <section aria-label="Key metrics" className="wk-metrics">
        <p className="wk-metrics-note">
          <CircleAlert size={14} aria-hidden /> Couldn’t load key metrics.{" "}
          <Button size="sm" variant="ghost" onClick={() => query.refetch()}>
            Retry
          </Button>
        </p>
      </section>
    );
  const busy = pending || !!data?.generating;
  const regenerate = async () => {
    setPending(true);
    try {
      qc.setQueryData(
        [id, "home-metrics"],
        await api<HomeMetrics>(`/workspaces/${id}/home/metrics/regenerate`, {}),
      );
    } catch (e) {
      notify((e as Error).message, true);
    } finally {
      setPending(false);
    }
  };
  const kpis = data?.metrics.filter((m) => m.kind === "kpi") || [],
    charts = data?.metrics.filter((m) => m.kind !== "kpi") || [];
  return (
    <section aria-label="Key metrics" className="wk-metrics">
      <div className="wk-metrics-head">
        <div>
          <h2>Key metrics</h2>
          {!data ? (
            <p>
              <Skeleton width={260} />
            </p>
          ) : data.source === "ai" ? (
            <p>Picked by AI from your data · updates when your data changes</p>
          ) : data.aiReady ? (
            <p>Picked from your schema · AI will tailor these</p>
          ) : (
            <p>
              Picked from your schema ·{" "}
              <Link to={paths.settings("models")}>set up AI</Link> to tailor
              these
            </p>
          )}
          {data?.generating ? (
            <p className="wk-metrics-note" role="status">
              <Sparkles size={13} aria-hidden className="wk-metrics-pulse" />
              AI is picking metrics for your data…
            </p>
          ) : (
            data?.error && (
              <p className="wk-metrics-note" title={data.error}>
                <CircleAlert size={13} aria-hidden className="is-warning" />
                Couldn’t pick new metrics: {data.error}
              </p>
            )
          )}
        </div>
        {data && role !== "viewer" && (
          <Button
            size="sm"
            variant="ghost"
            icon={RefreshCw}
            pending={busy}
            onClick={() => void regenerate()}
            title={
              data.aiReady
                ? "Ask AI to pick metrics again"
                : "Pick metrics from your schema again"
            }
          >
            {busy ? (data.aiReady ? "Picking…" : "Refreshing…") : "Regenerate"}
          </Button>
        )}
      </div>
      {!data ? (
        <MetricsSkeleton />
      ) : !data.metrics.length ? (
        <p className="wk-metrics-note">No metrics yet.</p>
      ) : (
        <>
          {kpis.length > 0 && (
            <div
              className="wk-metrics-kpis"
              style={{ "--wk-cols": kpiColumns(kpis.length) } as CSSProperties}
            >
              {kpis.map((m) => (
                <KpiTile key={m.id} m={m} />
              ))}
            </div>
          )}
          {suggestions > 0 && (
            <Link className="wk-metrics-more" to={paths.ontology()}>
              {Math.min(suggestions, 3)}{" "}
              {suggestions === 1 ? "suggestion" : "suggestions"} to see your
              customers more clearly
              <ChevronRight size={12} aria-hidden />
            </Link>
          )}
          {charts.length > 0 && (
            <div className="wk-metrics-charts">
              {charts.map((m) => (
                <ChartCard key={m.id} m={m} />
              ))}
            </div>
          )}
        </>
      )}
    </section>
  );
}

// The Analyst's chat answers draw their charts with these (styles in
// home.css, series colour from --wk-series).
export { TrendChart, BreakdownChart, exactText, type Point };
