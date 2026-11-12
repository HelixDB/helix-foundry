// Workflows area: what a window's runs came to, at a glance. Cards for runs,
// how often the ontology helped, the claims reads backed and calls a run,
// each with its trend and its change from the window before; the chosen
// one's chart (runs stacked by verdict, else the metric by day); where runs
// went from start to the ontology helping; the gaps their assessments named
// most; and the changes they suggested, which an owner or admin drafts a
// version from. Chips narrow it all to one who started runs. The ontology's
// page and a workflow's page both lead with it (GET .../workflows:insights).
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import type {
  FoundryPrincipal,
  FoundryWorkflowInsights,
  FoundryWorkflowInsightTotals,
} from "../../../shared/wfe";
import {
  ButtonLink,
  EmptyState,
  Panel,
  Segmented,
  SkeletonText,
  Tag,
  cx,
  formatRelative,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, canManage, useWorkspace } from "../../ui";
import { MAX_GOAL } from "../ontology/model";
import { useOntology } from "../ontology/scope";
import { keysQuery } from "../workspace/access-api";
import { membersQuery } from "../workspace/members";
import { workflowQueries } from "./api";
import {
  INSIGHT_METRICS,
  INSIGHT_WINDOWS,
  VERDICTS,
  VERDICT_LABELS,
  funnelSteps,
  gapKindLabel,
  insightChange,
  insightSeries,
  starterLabel,
  suggestionGoal,
  verdictDay,
  verdictParts,
  type InsightMetric,
} from "./model";

const METRICS = Object.keys(INSIGHT_METRICS) as InsightMetric[];
const day = (date: string) =>
  new Date(`${date}T00:00:00Z`).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    timeZone: "UTC",
  });
const percent = (v: number) => `${Math.round(v * 100)}%`;

/** The window, metric and who started the runs a page shows, and the read of them. */
export function useInsights(workflowId?: string) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const [days, setDays] = useState(30),
    [metric, setMetric] = useState<InsightMetric>("runs"),
    [who, setWho] = useState<FoundryPrincipal>();
  const query = useQuery(
    workflowQueries.insights(ws, o, { workflowId, days, startedBy: who }),
  );
  return { days, setDays, metric, setMetric, who, setWho, query };
}
export type InsightsState = ReturnType<typeof useInsights>;

/**
 * The names of who starts runs: the workspace's keys and members, read
 * where the user may (an owner or admin reads keys); none otherwise.
 */
function useStarterNames() {
  const { id: ws, role } = useWorkspace();
  const keys = useQuery({
      ...keysQuery(ws),
      enabled: canManage(role),
      retry: false,
    }),
    members = useInfiniteQuery({ ...membersQuery(ws), retry: false });
  return useMemo(() => {
    const out = new Map<string, string>();
    for (const k of keys.data || []) if (k.id && k.name) out.set(k.id, k.name);
    for (const page of members.data?.pages || [])
      for (const m of page.members || [])
        if (m.user?.id)
          out.set(m.user.id, m.user.displayName || m.user.email || "");
    return out;
  }, [keys.data, members.data]);
}

/** The window's runs by verdict, as a thin bar: the share of each. */
export function VerdictBar({
  totals,
}: {
  totals?: FoundryWorkflowInsightTotals;
}) {
  const parts = verdictParts(totals);
  if (!parts.length) return <span className="wf-vbar is-empty" aria-hidden />;
  return (
    <span
      className="wf-vbar"
      role="img"
      aria-label={parts
        .map(
          (p) => `${p.runs} ${VERDICT_LABELS[p.verdict].label.toLowerCase()}`,
        )
        .join(", ")}
    >
      {parts.map((p) => (
        <span
          key={p.verdict}
          style={{
            flexGrow: p.share,
            background: `var(--hf-verdict-${p.verdict})`,
          }}
        />
      ))}
    </span>
  );
}

/** Runs a day, as a tiny line. */
export function Spark({
  values,
  label,
}: {
  values: (number | undefined)[];
  label: string;
}) {
  const data = values.map((value, i) => ({ i, value }));
  return (
    <div className="wf-spark" role="img" aria-label={label}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart
          data={data}
          margin={{ top: 2, right: 0, bottom: 0, left: 0 }}
        >
          <Area
            type="monotone"
            dataKey="value"
            stroke="var(--hf-chart-1)"
            strokeWidth={2}
            fill="var(--hf-chart-1)"
            fillOpacity={0.1}
            connectNulls
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

function ChartTip({
  active,
  label,
  rows,
}: {
  active?: boolean;
  label?: string;
  rows: { key: string; label: string; value: string; color?: string }[];
}) {
  if (!active || !rows.length) return null;
  return (
    <div className="wf-tip">
      <strong>{label && day(label)}</strong>
      {rows.map((r) => (
        <span key={r.key}>
          <span>
            {r.color && <i style={{ background: r.color }} aria-hidden />}
            {r.label}
          </span>
          <span>{r.value}</span>
        </span>
      ))}
    </div>
  );
}

/** The chosen metric by day: runs stacked by verdict, else the metric as a line. */
function MainChart({
  insights,
  metric,
}: {
  insights: FoundryWorkflowInsights;
  metric: InsightMetric;
}) {
  const m = INSIGHT_METRICS[metric];
  const series = insights.series || [];
  const tickEvery = Math.max(1, Math.ceil(series.length / 8));
  const ticks = series
    .filter((_, i) => i % tickEvery === 0)
    .map((d) => d.date || "");
  const axis = {
    stroke: "var(--hf-border)",
    tick: { fill: "var(--hf-muted)", fontSize: 11 },
    tickLine: false,
  };
  if (metric === "runs") {
    const data = series.map(verdictDay);
    const keys = [...VERDICTS, "pending"] as const;
    const color = (k: string) =>
      k === "pending" ? "var(--hf-faint)" : `var(--hf-verdict-${k})`;
    const name = (k: string) =>
      k === "pending"
        ? "Not assessed yet"
        : VERDICT_LABELS[k as (typeof VERDICTS)[number]].label;
    return (
      <>
        <ul className="wf-legend" aria-label="Verdicts">
          {keys.map((k) => (
            <li key={k}>
              <i style={{ background: color(k) }} aria-hidden />
              {name(k)}
            </li>
          ))}
        </ul>
        <div className="wf-chart" role="img" aria-label="Runs a day by verdict">
          <ResponsiveContainer width="100%" height="100%">
            <BarChart
              data={data}
              margin={{ top: 4, right: 4, bottom: 0, left: -18 }}
              barCategoryGap={2}
            >
              <CartesianGrid
                vertical={false}
                stroke="var(--hf-border)"
                strokeOpacity={0.5}
              />
              <XAxis
                dataKey="date"
                ticks={ticks}
                tickFormatter={day}
                {...axis}
              />
              <YAxis allowDecimals={false} {...axis} axisLine={false} />
              <Tooltip
                cursor={{ fill: "var(--hf-border)", fillOpacity: 0.4 }}
                content={({ active, label, payload }) => (
                  <ChartTip
                    active={active}
                    label={String(label ?? "")}
                    rows={(payload || [])
                      .filter((p) => Number(p.value) > 0)
                      .reverse()
                      .map((p) => ({
                        key: String(p.dataKey),
                        label: name(String(p.dataKey)),
                        value: String(p.value),
                        color: color(String(p.dataKey)),
                      }))}
                  />
                )}
              />
              {keys.map((k, i) => (
                <Bar
                  key={k}
                  dataKey={k}
                  stackId="runs"
                  fill={color(k)}
                  stroke="var(--hf-bg)"
                  strokeWidth={1}
                  radius={i === keys.length - 1 ? [3, 3, 0, 0] : 0}
                  isAnimationActive={false}
                />
              ))}
            </BarChart>
          </ResponsiveContainer>
        </div>
      </>
    );
  }
  const data = series.map((d) => ({
    date: d.date || "",
    value: m.value(d.totals),
  }));
  return (
    <div className="wf-chart" role="img" aria-label={`${m.label} by day`}>
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart
          data={data}
          margin={{ top: 4, right: 4, bottom: 0, left: -18 }}
        >
          <CartesianGrid
            vertical={false}
            stroke="var(--hf-border)"
            strokeOpacity={0.5}
          />
          <XAxis dataKey="date" ticks={ticks} tickFormatter={day} {...axis} />
          <YAxis
            {...axis}
            axisLine={false}
            domain={m.share ? [0, 1] : [0, "auto"]}
            tickFormatter={(v: number) => (m.share ? percent(v) : String(v))}
          />
          <Tooltip
            cursor={{ stroke: "var(--hf-border-strong)" }}
            content={({ active, label, payload }) => (
              <ChartTip
                active={active}
                label={String(label ?? "")}
                rows={
                  payload?.[0]?.value === undefined
                    ? [{ key: "none", label: "No runs", value: "" }]
                    : [
                        {
                          key: "value",
                          label: m.label,
                          value: m.format(Number(payload[0].value)),
                        },
                      ]
                }
              />
            )}
          />
          <Area
            type="monotone"
            dataKey="value"
            stroke="var(--hf-chart-1)"
            strokeWidth={2}
            fill="var(--hf-chart-1)"
            fillOpacity={0.1}
            connectNulls
            dot={false}
            activeDot={{ r: 4 }}
            isAnimationActive={false}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  );
}

/** The one-line totals under a card's value. */
function cardDetail(metric: InsightMetric, t?: FoundryWorkflowInsightTotals) {
  const n = (v?: number) => (v || 0).toLocaleString("en-US");
  switch (metric) {
    case "runs":
      return `${n(t?.open)} open · ${n(t?.assessed)} assessed`;
    case "helped":
      return `${n(t?.helped)} of ${n(t?.assessed)} assessed runs`;
    case "backed": {
      const all =
        (t?.claimsSupported || 0) +
        (t?.claimsUnsupported || 0) +
        (t?.claimsContradicted || 0);
      return `${n(t?.claimsSupported)} of ${n(all)} claims · ${n(t?.claimsContradicted)} contradicted`;
    }
    case "calls":
      return `${n(t?.reads)} reads · ${n(t?.questions)} questions`;
  }
}

/**
 * A page's insights: who started runs, the window, the cards, the chosen
 * metric's chart, where runs went, the gaps and the suggested changes.
 * `workflowNames` names the ontology's workflows (its page lists them).
 */
export function InsightsBoard({
  state,
  workflowNames,
  own,
}: {
  state: InsightsState;
  workflowNames?: ReadonlyMap<string, string>;
  own: boolean;
}) {
  const { role, user } = useWorkspace(),
    { id: o } = useOntology();
  const names = useStarterNames();
  const { query, metric, setMetric, days, setDays, who, setWho } = state;
  const data = query.data;
  const window = (
    <Segmented
      label="Window"
      value={String(days)}
      onChange={(v) => setDays(Number(v))}
      options={INSIGHT_WINDOWS.map((d) => ({
        value: String(d),
        label: `${d} days`,
      }))}
    />
  );
  if (!data)
    return (
      <section className="wf-insights" aria-label="Insights">
        <div className="wf-insights-bar">{window}</div>
        <RefreshFailed query={query} />
        {query.isPending ? (
          <SkeletonText lines={6} />
        ) : (
          <EmptyState
            size="sm"
            icon={Sparkles}
            title="Couldn’t read the trends"
            text={query.error?.message}
          />
        )}
      </section>
    );
  const t = data.totals;
  const agents = data.agents || [];
  const same = (p?: FoundryPrincipal) =>
    !!who && p?.kind === who.kind && p?.id === who.id;
  return (
    <section className="wf-insights" aria-label="Insights">
      <div className="wf-insights-bar">
        {!own && agents.length > 0 && (
          <div className="wf-agents" role="group" aria-label="Who ran them">
            <button
              type="button"
              className={cx("wf-agent", !who && "is-active")}
              aria-pressed={!who}
              onClick={() => setWho(undefined)}
            >
              <span className="wf-agent-name">Everyone</span>
              <span className="wf-agent-meta">
                {who ? "All runs" : `${t?.runs || 0} runs`}
              </span>
            </button>
            {agents.map((a) => {
              const helped = INSIGHT_METRICS.helped.value(a.totals);
              return (
                <button
                  key={`${a.principal?.kind}:${a.principal?.id}`}
                  type="button"
                  className={cx("wf-agent", same(a.principal) && "is-active")}
                  aria-pressed={same(a.principal)}
                  onClick={() =>
                    setWho(same(a.principal) ? undefined : a.principal)
                  }
                >
                  <span className="wf-agent-name">
                    {starterLabel(a.principal, names, user.id)}
                  </span>
                  <span className="wf-agent-meta">
                    {a.totals?.runs || 0} runs
                    {helped !== undefined && ` · ${percent(helped)} helped`}
                  </span>
                </button>
              );
            })}
          </div>
        )}
        {window}
      </div>
      <RefreshFailed query={query} />
      {!t?.runs ? (
        <EmptyState
          size="sm"
          icon={Sparkles}
          title={`No runs in the last ${days} days`}
          text="Once an agent runs this workflow, what its runs come to shows here."
        />
      ) : (
        <>
          <div className="wf-cards" role="group" aria-label="Metrics">
            {METRICS.map((key) => {
              const m = INSIGHT_METRICS[key];
              const value = m.value(t);
              const change = insightChange(key, t, data.previous);
              return (
                <button
                  key={key}
                  type="button"
                  className={cx("wf-card", metric === key && "is-active")}
                  aria-pressed={metric === key}
                  onClick={() => setMetric(key)}
                >
                  <Spark
                    values={insightSeries(key, data.series).map((d) => d.value)}
                    label={`${m.label} by day`}
                  />
                  <span className="wf-card-label">{m.label}</span>
                  <span className="wf-card-value">
                    {value === undefined ? "—" : m.format(value)}
                  </span>
                  <span className="wf-card-detail">
                    {change && (
                      <span className={`wf-change is-${change.direction}`}>
                        {change.text}
                      </span>
                    )}
                    {cardDetail(key, t)}
                  </span>
                </button>
              );
            })}
          </div>
          <Panel
            title={
              metric === "runs"
                ? "Runs by how much the ontology helped"
                : `${INSIGHT_METRICS[metric].label}, by day`
            }
            subtitle={`The last ${days} days, by the day each run started.${data.truncated ? " The newest 5,000 runs." : ""}`}
          >
            <MainChart insights={data} metric={metric} />
          </Panel>
          <div className="wf-grid3">
            <Panel
              title="Where runs go"
              subtitle="From start to the ontology helping."
            >
              <ol className="wf-funnel">
                {funnelSteps(data.funnel).map((s) => (
                  <li key={s.key}>
                    {s.left && s.left.runs > 0 && (
                      <p className="wf-left">
                        ▼ {s.left.runs} {s.left.why} ({percent(s.left.share)})
                      </p>
                    )}
                    <div className="wf-step">
                      <span>{s.label}</span>
                      <span className="wf-step-n">
                        {s.runs} · {percent(s.share)}
                      </span>
                    </div>
                    <span className="wf-step-bar" aria-hidden>
                      <span style={{ width: `${s.share * 100}%` }} />
                    </span>
                  </li>
                ))}
              </ol>
            </Panel>
            <Panel
              title="What the ontology lacks"
              subtitle="Gaps the assessments named, most runs first."
            >
              {!data.gaps?.length ? (
                <p className="on-muted">No gaps named in this window.</p>
              ) : (
                <>
                  <ul className="wf-kinds" aria-label="Gaps by kind">
                    {(t.gapKinds || []).map((k) => (
                      <li key={k.kind}>
                        <span>{gapKindLabel(k.kind)}</span>
                        <span className="wf-kind-bar" aria-hidden>
                          <span
                            style={{
                              width: `${((k.runs || 0) / Math.max(...(t.gapKinds || []).map((x) => x.runs || 0))) * 100}%`,
                            }}
                          />
                        </span>
                        <span className="wf-kind-n">{k.runs}</span>
                      </li>
                    ))}
                  </ul>
                  <ul className="wf-issues" aria-label="Recurring gaps">
                    {data.gaps.slice(0, 5).map((g) => (
                      <li key={g.text}>
                        <span className="wf-issue-text">
                          <Tag>{gapKindLabel(g.kind)}</Tag> {g.text}
                        </span>
                        <span className="wf-issue-meta">
                          {g.runs} {g.runs === 1 ? "run" : "runs"}
                          {g.lastAt && ` · latest ${formatRelative(g.lastAt)}`}
                          {g.lastRunId && g.lastWorkflowId && (
                            <>
                              {" · "}
                              <Link
                                to={paths.workflowRun(
                                  o,
                                  g.lastWorkflowId,
                                  g.lastRunId,
                                )}
                              >
                                Latest run
                              </Link>
                            </>
                          )}
                        </span>
                      </li>
                    ))}
                  </ul>
                </>
              )}
            </Panel>
            <Panel
              title="Suggested changes"
              subtitle="What the assessments would change, most runs first."
            >
              {!data.suggestions?.length ? (
                <p className="on-muted">No changes suggested in this window.</p>
              ) : (
                <ul className="wf-issues" aria-label="Suggested changes">
                  {data.suggestions.slice(0, 6).map((s) => (
                    <li key={s.text}>
                      <span className="wf-issue-text">{s.text}</span>
                      <span className="wf-issue-meta">
                        {s.runs} {s.runs === 1 ? "run" : "runs"}
                        {workflowNames &&
                          (s.workflowIds || []).length > 0 &&
                          ` · ${(s.workflowIds || [])
                            .map((id) => workflowNames.get(id) || "A workflow")
                            .join(", ")}`}
                      </span>
                      {canManage(role) && (
                        <ButtonLink
                          size="sm"
                          className="wf-issue-action"
                          to={paths.generate(o, {
                            goal: suggestionGoal(s.text || "", [], MAX_GOAL),
                          })}
                        >
                          Draft version
                        </ButtonLink>
                      )}
                    </li>
                  ))}
                </ul>
              )}
            </Panel>
          </div>
        </>
      )}
    </section>
  );
}
