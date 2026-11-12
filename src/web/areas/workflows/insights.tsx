// Workflows area: what a window of workflow runs came to (GET
// .../workflows:insights), in the pieces both pages build from. The overview
// (WorkflowsPage) answers whether the ontology pays off and what to change:
// a sentence, the impact cards and the chosen one's trend, then the
// workflows side by side and what to change. A workflow's page answers how
// its task goes and why: a strip of its numbers with where its runs went,
// then its runs, then what to change for it. Both narrow to one who started
// runs and a window; counts stand in for rates of too few runs (MIN_SAMPLE).
import { useMemo, useState, type ReactNode } from "react";
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
  FoundryWorkflowInsightIssue,
  FoundryWorkflowInsights,
  FoundryWorkflowInsightTotals,
} from "../../../shared/wfe";
import {
  Button,
  ButtonLink,
  EmptyState,
  FilterSelect,
  Panel,
  Segmented,
  SkeletonText,
  Tag,
  cx,
  formatNumber,
  formatRelative,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
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
  activeDays,
  callsPerRun,
  claimsOf,
  funnelSteps,
  funnelText,
  gapKindLabel,
  insightChange,
  insightSeries,
  issueKey,
  metricText,
  rollingDays,
  verdictDay,
  principalKey,
  starterLabel,
  suggestionGoal,
  verdictParts,
  verdictRuns,
  type InsightMetric,
  type Verdict,
} from "./model";

const METRICS = Object.keys(INSIGHT_METRICS) as InsightMetric[];
// The issues a list shows before Show all.
const ISSUES_SHOWN = 5;
const day = (date: string) =>
  new Date(`${date}T00:00:00Z`).toLocaleDateString("en-GB", {
    day: "numeric",
    month: "short",
    timeZone: "UTC",
  });

/** The window, metric and who started the runs a page shows, and the read of them. */
export function useInsights(workflowId?: string) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const [days, setDays] = useState(30),
    [metric, setMetric] = useState<InsightMetric>("helped"),
    [who, setWho] = useState<FoundryPrincipal>();
  const query = useQuery(
    workflowQueries.insights(ws, o, { workflowId, days, startedBy: who }),
  );
  // Everyone's, for the list of who started runs: the same query while no
  // one is chosen.
  const everyone = useQuery(
    workflowQueries.insights(ws, o, { workflowId, days }),
  );
  return { days, setDays, metric, setMetric, who, setWho, query, everyone };
}
export type InsightsState = ReturnType<typeof useInsights>;

/**
 * The names of who starts runs: the workspace's keys and members, read
 * where the user may (an owner or admin reads keys); none otherwise.
 */
export function useStarterNames() {
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

/**
 * Who started the runs (everyone, or one of them) and the window: one row,
 * its controls one height. A grantee sees only their own runs, so no who.
 */
export function InsightsToolbar({
  state,
  own,
}: {
  state: InsightsState;
  own: boolean;
}) {
  const { user } = useWorkspace();
  const names = useStarterNames();
  const { days, setDays, who, setWho, everyone } = state;
  const agents = everyone.data?.agents || [];
  return (
    <div className="wf-toolbar">
      {!own && agents.length > 0 && (
        <FilterSelect
          label="Started by"
          value={principalKey(who)}
          onChange={(key) =>
            setWho(
              agents.find((a) => principalKey(a.principal) === key)?.principal,
            )
          }
          options={[
            {
              value: "",
              label: "Everyone",
              hint: plural(everyone.data?.totals?.runs || 0, "run"),
            },
            ...agents.map((a) => ({
              value: principalKey(a.principal),
              label: starterLabel(a.principal, names, user.id),
              hint: plural(a.totals?.runs || 0, "run"),
            })),
          ]}
        />
      )}
      <Segmented
        label="Window"
        value={String(days)}
        onChange={(v) => setDays(Number(v))}
        options={INSIGHT_WINDOWS.map((d) => ({
          value: String(d),
          label: `${d} days`,
        }))}
      />
    </div>
  );
}

/** While the insights read, or when they couldn't be. */
export function InsightsPending({ state }: { state: InsightsState }) {
  const { query } = state;
  return query.isPending ? (
    <SkeletonText lines={4} />
  ) : (
    <EmptyState
      size="sm"
      icon={Sparkles}
      title="Couldn’t read what the runs came to"
      text={query.error?.message}
      action={<Button onClick={() => void query.refetch()}>Retry</Button>}
    />
  );
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

/** Whether a window holds runs the ontology misled, or claims what was read contradicts. */
const needsLook = (t?: FoundryWorkflowInsightTotals) =>
  verdictRuns(t, "misleading") + (t?.claimsContradicted || 0) > 0;

/** The line under a metric's value: what it is of. */
function metricDetail(
  metric: InsightMetric,
  t: FoundryWorkflowInsightTotals | undefined,
  few: boolean,
) {
  const assessed = t?.assessed || 0;
  switch (metric) {
    case "runs": {
      const calls = callsPerRun(t);
      return `${formatNumber(t?.open || 0)} open · ${formatNumber(assessed)} assessed${calls === undefined ? "" : ` · ${calls.toFixed(1)} calls a run`}`;
    }
    case "helped":
      return few
        ? `assessed ${assessed === 1 ? "run" : "runs"}, too few for a rate`
        : `of ${plural(assessed, "assessed run")}`;
    case "backed":
      return few
        ? "claims backed by what agents read"
        : `of ${plural(claimsOf(t), "claim")}, backed by what agents read`;
    case "misleading":
      return `${plural(t?.claimsContradicted || 0, "claim")} contradicted · ${plural(verdictRuns(t, "unused"), "run")} never used it`;
  }
}

/**
 * The overview's impact cards, one height: how often the ontology helped,
 * how much of the answers what the agents read backed, and what misled.
 * With a trend to show, a card charts its metric below.
 */
export function ImpactCards({
  state,
  data,
}: {
  state: InsightsState;
  data: FoundryWorkflowInsights;
}) {
  const { metric, setMetric } = state;
  const t = data.totals;
  const trend = activeDays(data.series) >= 2;
  return (
    <div className="wf-cards" role="group" aria-label="Metrics">
      {METRICS.map((key) => {
        const m = INSIGHT_METRICS[key];
        const value = metricText(key, t);
        const change = insightChange(key, t, data.previous);
        const body = (
          <>
            <span className="wf-card-label">{m.label}</span>
            <span className="wf-card-value">{value.text}</span>
            <span className="wf-card-detail">
              {change && change.tone !== "flat" && (
                <span
                  className={`wf-change is-${change.tone}`}
                  title={`From the ${data.days} days before`}
                >
                  {change.text}
                </span>
              )}
              {metricDetail(key, t, value.few)}
            </span>
          </>
        );
        const className = cx(
          "wf-card",
          key === "misleading" && needsLook(t) && "is-alert",
          trend && metric === key && "is-active",
        );
        return trend ? (
          <button
            key={key}
            type="button"
            className={className}
            aria-pressed={metric === key}
            onClick={() => setMetric(key)}
          >
            {body}
          </button>
        ) : (
          <div key={key} className={className}>
            {body}
          </div>
        );
      })}
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

/** The chosen metric by day, in a panel; nothing until two days have runs. */
export function TrendPanel({
  data,
  metric,
}: {
  data: FoundryWorkflowInsights;
  metric: InsightMetric;
}) {
  if (activeDays(data.series) < 2) return null;
  const m = INSIGHT_METRICS[metric];
  const rolling = m.share ? rollingDays(data.days || 0) : 1;
  const series = insightSeries(metric, data.series, rolling);
  const tickEvery = Math.max(1, Math.ceil(series.length / 8));
  const ticks = series.filter((_, i) => i % tickEvery === 0).map((d) => d.date);
  const axis = {
    stroke: "var(--hf-border)",
    tick: { fill: "var(--hf-muted)", fontSize: 11 },
    tickLine: false,
  };
  if (metric === "runs") {
    // Runs a day, stacked by verdict: how many, and how the ontology did.
    const days = (data.series || []).map(verdictDay);
    const keys = [...VERDICTS, "pending"].filter((k) =>
      days.some((d) => Number(d[k]) > 0),
    );
    const color = (k: string) =>
      k === "pending" ? "var(--hf-border-strong)" : `var(--hf-verdict-${k})`;
    const name = (k: string) =>
      k === "pending" ? "Not assessed yet" : VERDICT_LABELS[k as Verdict].label;
    return (
      <Panel
        title="Runs a day, by how much the ontology helped"
        subtitle={`The last ${data.days} days, by the day each run started.${data.truncated ? " The newest 5,000 runs." : ""}`}
      >
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
              data={days}
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
                  radius={i === keys.length - 1 ? [2, 2, 0, 0] : 0}
                  isAnimationActive={false}
                />
              ))}
            </BarChart>
          </ResponsiveContainer>
        </div>
      </Panel>
    );
  }
  return (
    <Panel
      title={`${m.label}, by day`}
      subtitle={`The last ${data.days} days, by the day each run started${rolling > 1 ? `; each day over the ${rolling} days up to it` : ""}.${data.truncated ? " The newest 5,000 runs." : ""}`}
    >
      <div className="wf-chart" role="img" aria-label={`${m.label} by day`}>
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart
            data={series}
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
              allowDecimals={!m.share ? false : undefined}
              domain={m.share ? [0, 1] : [0, "auto"]}
              tickFormatter={(v: number) => (m.share ? m.format(v) : String(v))}
            />
            <Tooltip
              cursor={{ stroke: "var(--hf-border-strong)" }}
              content={({ active, label, payload }) => (
                <ChartTip
                  active={active}
                  label={String(label ?? "")}
                  rows={
                    payload?.[0]?.value === undefined
                      ? [{ key: "none", label: "No assessed runs", value: "" }]
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
              stroke={
                m.better === "down"
                  ? "var(--hf-verdict-misleading)"
                  : "var(--hf-chart-1)"
              }
              strokeWidth={2}
              fill={
                m.better === "down"
                  ? "var(--hf-verdict-misleading)"
                  : "var(--hf-chart-1)"
              }
              fillOpacity={0.1}
              connectNulls
              dot={false}
              activeDot={{ r: 4 }}
              isAnimationActive={false}
            />
          </AreaChart>
        </ResponsiveContainer>
      </div>
    </Panel>
  );
}

/**
 * A workflow's numbers in one strip of equal cells, and under them where its
 * runs went: one sentence when none left on the way, else each step with
 * what left before it.
 */
export function StatStrip({ data }: { data: FoundryWorkflowInsights }) {
  const t = data.totals;
  const cells = METRICS.map((key) => {
    const value = metricText(key, t);
    return {
      key,
      label: INSIGHT_METRICS[key].label,
      value: value.text,
      detail: metricDetail(key, t, value.few),
      change: insightChange(key, t, data.previous),
      alert: key === "misleading" && needsLook(t),
    };
  });
  const flow = funnelText(data.funnel);
  return (
    <section className="wf-strip" aria-label="Numbers">
      <dl>
        {cells.map((c) => (
          <div
            key={c.key}
            className={cx("wf-strip-cell", c.alert && "is-alert")}
          >
            <dt>{c.label}</dt>
            <dd className="wf-strip-value">{c.value}</dd>
            <dd className="wf-strip-detail">
              {c.change && c.change.tone !== "flat" && (
                <span
                  className={`wf-change is-${c.change.tone}`}
                  title={`From the ${data.days} days before`}
                >
                  {c.change.text}
                </span>
              )}
              {c.detail}
            </dd>
          </div>
        ))}
      </dl>
      {flow ? (
        <p className="wf-flow-text">{flow}</p>
      ) : (
        (t?.runs || 0) > 0 && (
          <ol className="wf-flow" aria-label="Where runs go">
            {funnelSteps(data.funnel).map((s) => (
              <li key={s.key}>
                <span className="wf-flow-label">{s.label}</span>
                <span className="wf-flow-n">{formatNumber(s.runs)}</span>
                <span className="wf-flow-left">
                  {s.left && s.left.runs > 0
                    ? `${formatNumber(s.left.runs)} ${s.left.why}`
                    : " "}
                </span>
              </li>
            ))}
          </ol>
        )
      )}
    </section>
  );
}

/** A list of issues, the first ISSUES_SHOWN, then all on asking. */
function IssueList({
  label,
  items,
  render,
}: {
  label: string;
  items: FoundryWorkflowInsightIssue[];
  render: (issue: FoundryWorkflowInsightIssue) => ReactNode;
}) {
  const [all, setAll] = useState(false);
  const shown = all ? items : items.slice(0, ISSUES_SHOWN);
  return (
    <>
      <ul className="wf-issues" aria-label={label}>
        {shown.map((issue) => (
          <li key={issue.text}>{render(issue)}</li>
        ))}
      </ul>
      {items.length > ISSUES_SHOWN && (
        <button
          type="button"
          className="wf-more-link"
          onClick={() => setAll(!all)}
        >
          {all ? "Show fewer" : `Show all ${items.length}`}
        </button>
      )}
    </>
  );
}

/**
 * What to change: the gaps the assessments named and the changes they
 * suggested, side by side in one panel, most runs first. An owner or admin
 * ticks suggestions and drafts one version with them. On a workflow's page
 * (`onIssue`), an issue's runs narrow its runs list.
 */
export function ChangesPanel({
  data,
  workflowNames,
  issue,
  onIssue,
}: {
  data: FoundryWorkflowInsights;
  workflowNames?: ReadonlyMap<string, string>;
  // The issueKey of the issue narrowing the runs list, if one does.
  issue?: string;
  onIssue?: (issue?: { key: string; text: string }) => void;
}) {
  const { role } = useWorkspace(),
    { id: o } = useOntology();
  const [chosen, setChosen] = useState<string[]>([]);
  const gaps = data.gaps || [],
    suggestions = data.suggestions || [];
  if (!gaps.length && !suggestions.length) return null;
  const manage = canManage(role);
  // What a gap or suggestion came to: its runs (on a workflow's page, a
  // button narrowing the runs to them), its workflows, and the latest.
  const meta = (s: FoundryWorkflowInsightIssue) => {
    const key = issueKey(s.text || "");
    const runs = plural(s.runs || 0, "run");
    return (
      <span className="wf-issue-meta">
        {onIssue ? (
          <button
            type="button"
            className="wf-issue-runs"
            aria-pressed={issue === key}
            onClick={() =>
              onIssue(issue === key ? undefined : { key, text: s.text || "" })
            }
          >
            {issue === key ? `Showing its ${runs}` : `Show its ${runs}`}
          </button>
        ) : (
          runs
        )}
        {workflowNames &&
          (s.workflowIds || []).length > 0 &&
          ` · ${(s.workflowIds || [])
            .map((id) => workflowNames.get(id) || "A workflow")
            .join(", ")}`}
        {s.lastAt && s.lastRunId && s.lastWorkflowId && (
          <>
            {" · latest "}
            <Link to={paths.workflowRun(o, s.lastWorkflowId, s.lastRunId)}>
              {formatRelative(s.lastAt)}
            </Link>
          </>
        )}
      </span>
    );
  };
  const picked = suggestions.filter((s) => chosen.includes(s.text || ""));
  return (
    <Panel
      title="What to change"
      subtitle={`What the assessments of the last ${data.days} days found the ontology lacked, and the changes they suggested. Most runs first.`}
      padded={false}
    >
      <div className="wf-changes">
        <section aria-label="What the ontology lacks">
          <h4>What the ontology lacks</h4>
          {!gaps.length ? (
            <p className="on-muted">No gaps named.</p>
          ) : (
            <IssueList
              label="Recurring gaps"
              items={gaps}
              render={(g) => (
                <div
                  className={cx(
                    "wf-issue",
                    issue === issueKey(g.text || "") && "is-active",
                  )}
                >
                  <span className="wf-issue-kind">
                    <Tag>{gapKindLabel(g.kind)}</Tag>
                  </span>
                  <span className="wf-issue-text">{g.text}</span>
                  {meta(g)}
                </div>
              )}
            />
          )}
        </section>
        <section aria-label="Suggested changes">
          <h4>Suggested changes</h4>
          {!suggestions.length ? (
            <p className="on-muted">No changes suggested.</p>
          ) : (
            <IssueList
              label="Suggested changes"
              items={suggestions}
              render={(s) => {
                const text = s.text || "";
                return (
                  <div
                    className={cx(
                      "wf-issue",
                      issue === issueKey(text) && "is-active",
                    )}
                  >
                    <span className="wf-issue-kind">
                      {manage && (
                        <input
                          type="checkbox"
                          aria-label={`Draft with: ${text}`}
                          checked={chosen.includes(text)}
                          onChange={(e) =>
                            setChosen((was) =>
                              e.target.checked
                                ? [...was, text]
                                : was.filter((x) => x !== text),
                            )
                          }
                        />
                      )}
                    </span>
                    <span className="wf-issue-text">{text}</span>
                    {meta(s)}
                  </div>
                );
              }}
            />
          )}
          {manage && suggestions.length > 0 && (
            <div className="wf-changes-foot">
              <span className="on-muted">
                {picked.length
                  ? `${plural(picked.length, "change")} chosen`
                  : "Tick the changes to draft a version with."}
              </span>
              {picked.length ? (
                <ButtonLink
                  size="sm"
                  variant="primary"
                  to={paths.generate(o, {
                    goal: suggestionGoal(
                      picked.map((s) => s.text || ""),
                      [],
                      MAX_GOAL,
                    ),
                  })}
                >
                  Draft a version
                </ButtonLink>
              ) : (
                <Button size="sm" variant="primary" disabled>
                  Draft a version
                </Button>
              )}
            </div>
          )}
        </section>
      </div>
    </Panel>
  );
}
