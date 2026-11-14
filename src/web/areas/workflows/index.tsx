// Workflows area: an ontology's workflows (an agent's task) and what their
// runs recorded, lazy-loaded by the Ontology area's route table under
// /ontology/:ontologyId/workflows. Owners and admins create and delete a
// workflow; anyone else reads them. An agent starts and finishes a run
// through the Helix MCP, with the workflow's id, and passes the run's id to
// every read, question, send and upload it makes for the request: the run's
// page shows the request, those calls and the answer side by side. Nothing
// here starts a run.
import "./workflows.css";
import { useMemo, useState } from "react";
import {
  Link,
  useNavigate,
  useParams,
  useSearchParams,
} from "react-router-dom";
import {
  useInfiniteQuery,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  ChevronDown,
  ChevronUp,
  Copy,
  Plus,
  RefreshCw,
  Trash2,
  Workflow,
  X,
} from "lucide-react";
import type { FoundryWorkflow, FoundryWorkflowRun } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import {
  Button,
  ConfirmDialog,
  DataGrid,
  Dialog,
  EmptyState,
  IconButton,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  Segmented,
  SkeletonText,
  StatusBadge,
  Tag,
  cx,
  formatDate,
  formatNumber,
  formatRelative,
  useNow,
  useTitle,
  type Crumb,
} from "../../kit";
import { paths } from "../../paths";
import { RefreshFailed, canManage, useWorkspace } from "../../ui";
import { MCP_URL } from "../ontology/agent-setup";
import { MAX_GOAL } from "../ontology/model";
import { ontologyName, useOntology } from "../ontology/scope";
import {
  OPEN_RUN_POLL_MS,
  assessRun,
  createWorkflow,
  deleteWorkflow,
  workflowQueries,
} from "./api";
import {
  ChangesPanel,
  ImpactCards,
  InsightsPending,
  InsightsToolbar,
  Spark,
  StatStrip,
  TrendPanel,
  VerdictBar,
  useInsights,
  useStarterNames,
  type InsightsState,
} from "./insights";
import {
  INSIGHT_METRICS,
  RUN_FILTERS,
  VERDICT_LABELS,
  accessTitle,
  accessView,
  assessmentOf,
  countsText,
  isRunFilter,
  metricText,
  preview,
  readText,
  runDuration,
  runShown,
  starterLabel,
  starterOf,
  suggestionGoal,
  verdictRuns,
  windowStart,
  workflowPrompt,
  type AccessView,
  type RunFilter,
  type Verdict,
} from "./model";

function useCrumbs(): Crumb[] {
  const { id: o, ontology } = useOntology();
  return [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
    { label: "Workflows", to: paths.workflows(o) },
  ];
}

function useCopy() {
  const { notify } = useWorkspace();
  return (text: string, what: string) =>
    navigator.clipboard
      .writeText(text)
      .then(() => notify(`Copied the ${what}.`))
      .catch(() => notify(`Couldn’t copy the ${what}.`, true));
}

// What a run's assessment found, as its row shows it: its verdict once
// done, else where the run or its assessment stands.
function RunVerdict({ run: r }: { run: FoundryWorkflowRun }) {
  if (r.status === "open")
    return <StatusBadge status="running" label="Open" size="sm" />;
  const a = r.assessment;
  if (a?.status === "done" && a.verdict && a.verdict in VERDICT_LABELS) {
    const v = VERDICT_LABELS[a.verdict as Verdict];
    return <Tag tone={v.tone}>{v.label}</Tag>;
  }
  const words: Record<string, string> = {
    queued: "Assessing…",
    running: "Assessing…",
    failed: "Not assessed",
    skipped: "Not assessed",
  };
  return (
    <span className="on-muted">{(a && words[a.status]) || "Finished"}</span>
  );
}

const runStatus = (r: FoundryWorkflowRun) =>
  r.status === "finished" ? (
    <StatusBadge status="succeeded" label="Finished" size="sm" />
  ) : (
    <StatusBadge status="running" label="Open" size="sm" />
  );

// /ontology/:ontologyId/workflows: whether the ontology pays off across its
// workflows, and what to change in it. A sentence and the impact cards lead,
// then the workflows side by side (a row opens its runs), then what to change.
export function WorkflowsPage() {
  const { id: ws, role, partial } = useWorkspace(),
    { id: o } = useOntology();
  useTitle("Workflows");
  const crumbs = useCrumbs(),
    navigate = useNavigate();
  const list = useQuery(workflowQueries.list(ws, o));
  const insights = useInsights();
  const names = useMemo(
    () => new Map((list.data?.workflows || []).map((w) => [w.id, w.name])),
    [list.data],
  );
  const [creating, setCreating] = useState(false);
  const header = (
    <PageHeader
      breadcrumbs={crumbs.slice(0, 2).concat({ label: "Workflows" })}
      title="Workflows"
      subtitle="How much this ontology helps your agents’ tasks, and what to change in it."
      actions={
        canManage(role) && (
          <Button
            variant="primary"
            icon={Plus}
            onClick={() => setCreating(true)}
          >
            New workflow
          </Button>
        )
      }
    />
  );
  const dialog = creating && (
    <NewWorkflowDialog
      onClose={() => setCreating(false)}
      onCreated={(w) => navigate(paths.workflow(o, w.id))}
    />
  );
  if (list.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={5} />
        {dialog}
      </Page>
    );
  if (!list.data)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Workflow}
          title="Couldn’t load the workflows"
          text={list.error?.message}
          action={<Button onClick={() => void list.refetch()}>Retry</Button>}
        />
        {dialog}
      </Page>
    );
  const workflows = list.data.workflows || [];
  const data = insights.query.data;
  const days = data?.days || insights.days;
  const segments = new Map(
    (data?.workflows || []).map((w) => [w.workflowId, w]),
  );
  return (
    <Page>
      {header}
      <div className="on-stack">
        <RefreshFailed query={list} />
        {!workflows.length ? (
          <EmptyState
            icon={Workflow}
            title="No workflows yet"
            text={
              canManage(role)
                ? "Create one for each task an agent does with this ontology, then give the agent its prompt. What its runs come to shows here."
                : "An owner or admin creates one for each task an agent does with this ontology."
            }
          />
        ) : (
          <>
            <section className="wf-insights" aria-label="Insights">
              <InsightsToolbar state={insights} own={partial.includes(o)} />
              <RefreshFailed query={insights.query} />
              {!data ? (
                <InsightsPending state={insights} />
              ) : (data.totals?.runs || 0) > 0 ? (
                <>
                  <ImpactCards state={insights} data={data} />
                  <TrendPanel data={data} metric={insights.metric} />
                </>
              ) : (
                <EmptyState
                  size="sm"
                  icon={Workflow}
                  title={`No runs in the last ${days} days`}
                  text="Once an agent runs a workflow, what its runs come to shows here."
                />
              )}
            </section>
            <Panel
              title="Workflows"
              subtitle={`What each workflow’s runs came to in the last ${days} days. Open one for its runs.`}
              padded={false}
            >
              <DataGrid<FoundryWorkflow>
                className="wf-grid"
                label="Workflows"
                rows={workflows}
                getRowKey={(w) => w.id}
                onRowClick={(w) => navigate(paths.workflow(o, w.id))}
                rowHeight={60}
                columns={[
                  {
                    key: "name",
                    label: "Workflow",
                    type: "string",
                    width: 220,
                    render: (w) => (
                      <span className="wf-row-name">
                        <Link
                          className="on-row-link on-cell-text"
                          to={paths.workflow(o, w.id)}
                        >
                          {w.name}
                        </Link>
                        {w.description && (
                          <span
                            className="on-cell-text on-muted"
                            title={w.description}
                          >
                            {w.description}
                          </span>
                        )}
                      </span>
                    ),
                  },
                  {
                    key: "runs",
                    label: "Runs",
                    type: "integer",
                    width: 130,
                    sortValue: (w) => segments.get(w.id)?.totals?.runs || 0,
                    render: (w) => {
                      const seg = segments.get(w.id);
                      const daily = seg?.dailyRuns || [];
                      return (
                        <span className="wf-row-trend">
                          {daily.filter((n) => n > 0).length >= 2 && (
                            <Spark
                              values={daily}
                              label={`${w.name}: runs a day`}
                            />
                          )}
                          <span className="wf-row-n">
                            {formatNumber(seg?.totals?.runs || 0)}
                          </span>
                        </span>
                      );
                    },
                  },
                  {
                    key: "helped",
                    label: "Ontology helped",
                    type: "string",
                    width: 170,
                    sortValue: (w) =>
                      INSIGHT_METRICS.helped.value(
                        segments.get(w.id)?.totals,
                      ) ?? -1,
                    render: (w) => {
                      const t = segments.get(w.id)?.totals;
                      return (
                        <span className="wf-row-helped">
                          <VerdictBar totals={t} />
                          {metricText("helped", t).text}
                        </span>
                      );
                    },
                  },
                  {
                    key: "backed",
                    label: "Claims grounded",
                    type: "string",
                    align: "right",
                    width: 120,
                    sortValue: (w) =>
                      INSIGHT_METRICS.backed.value(
                        segments.get(w.id)?.totals,
                      ) ?? -1,
                    render: (w) =>
                      metricText("backed", segments.get(w.id)?.totals).text,
                  },
                  {
                    key: "misleading",
                    label: "Misleading",
                    type: "string",
                    align: "right",
                    width: 100,
                    sortValue: (w) =>
                      verdictRuns(segments.get(w.id)?.totals, "misleading"),
                    render: (w) => {
                      const n = verdictRuns(
                        segments.get(w.id)?.totals,
                        "misleading",
                      );
                      return n > 0 ? (
                        <Link
                          className="wf-alert-link"
                          to={`${paths.workflow(o, w.id)}?verdict=misleading`}
                          onClick={(e) => e.stopPropagation()}
                        >
                          {formatNumber(n)}
                        </Link>
                      ) : (
                        <span className="on-muted">0</span>
                      );
                    },
                  },
                  {
                    key: "lastRunAt",
                    label: "Last run",
                    type: "string",
                    width: 100,
                    sortValue: (w) => w.lastRunAt || "",
                    render: (w) => (
                      <span className="on-muted">
                        {w.lastRunAt ? formatRelative(w.lastRunAt) : "Never"}
                      </span>
                    ),
                  },
                ]}
              />
            </Panel>
            {data && <ChangesPanel data={data} workflowNames={names} />}
          </>
        )}
      </div>
      {dialog}
    </Page>
  );
}

function NewWorkflowDialog({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (w: FoundryWorkflow) => void;
}) {
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const [name, setName] = useState(""),
    [description, setDescription] = useState(""),
    [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [token] = useState(() => crypto.randomUUID());
  return (
    <Dialog title="New workflow" size="sm" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setPending(true);
          setError("");
          try {
            const { workflow } = await createWorkflow(ws, o, {
              name,
              description,
              clientToken: token,
            });
            trackEvent("workflow_created");
            await qc.invalidateQueries({
              queryKey: workflowQueries.list(ws, o).queryKey,
            });
            onCreated(workflow);
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setPending(false);
          }
        }}
      >
        <label className="hf-field">
          <span>Name</span>
          <input
            required
            data-autofocus
            maxLength={100}
            placeholder="e.g. Support triage"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>
        <label className="hf-field">
          <span>What the agent does (optional)</span>
          <textarea
            maxLength={2000}
            rows={3}
            placeholder="e.g. Answers customers’ billing tickets"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        {error && (
          <p role="alert" className="hf-form-error">
            {error}
          </p>
        )}
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button
            type="submit"
            variant="primary"
            pending={pending}
            disabled={!name.trim()}
          >
            Create workflow
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

// /ontology/:ontologyId/workflows/:workflowId: how this task goes and why.
// Until its first run, connecting the agent; then a strip of its numbers
// with where its runs went, its runs, and what to change for it.
export function WorkflowPage() {
  const { workflowId = "" } = useParams();
  const { id: ws, name: workspaceName, role, partial } = useWorkspace(),
    { id: o, ontology } = useOntology();
  // A grantee holding only part of the ontology sees only the runs it started.
  const own = partial.includes(o);
  const crumbs = useCrumbs(),
    navigate = useNavigate(),
    qc = useQueryClient(),
    copy = useCopy();
  const workflow = useQuery(workflowQueries.workflow(ws, o, workflowId));
  const insights = useInsights(workflowId);
  // A gap or suggestion narrowing the runs to those that named it.
  const [issue, setIssue] = useState<{ key: string; text: string }>();
  const [connecting, setConnecting] = useState(false);
  const [deleting, setDeleting] = useState(false),
    [pending, setPending] = useState(false),
    [deleteError, setDeleteError] = useState("");
  const w = workflow.data?.workflow;
  useTitle(w?.name || "Workflow");
  const header = (
    <PageHeader
      breadcrumbs={[...crumbs, { label: w?.name || "Workflow" }]}
      title={w?.name || "Workflow"}
      subtitle={w?.description || undefined}
      actions={
        w && (
          <>
            {Number(w.runs || 0) > 0 && (
              <Button icon={Copy} onClick={() => setConnecting(true)}>
                Connect agent
              </Button>
            )}
            {canManage(role) && (
              <Button icon={Trash2} onClick={() => setDeleting(true)}>
                Delete
              </Button>
            )}
          </>
        )
      }
    />
  );
  if (workflow.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={6} />
      </Page>
    );
  if (!w)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Workflow}
          title="Couldn’t load the workflow"
          text={workflow.error?.message}
          action={
            <Button onClick={() => void workflow.refetch()}>Retry</Button>
          }
        />
      </Page>
    );
  const prompt = workflowPrompt({
    mcpUrl: MCP_URL,
    workspaceId: ws,
    ontologyId: o,
    ontologyName: ontologyName(ontology),
    workflowId: w.id,
    workflowName: w.name,
  });
  const ran = Number(w.runs || 0) > 0;
  const data = insights.query.data;
  const copyPrompt = (
    <Button
      size="sm"
      icon={Copy}
      onClick={() => {
        trackEvent("workflow_prompt_copied");
        void copy(prompt, "prompt");
      }}
    >
      Copy prompt
    </Button>
  );
  const connection = (
    <>
      <KeyValue
        items={[
          {
            label: "Workflow ID",
            value: (
              <span className="wf-id">
                <code>{w.id}</code>
                <Button
                  size="sm"
                  variant="ghost"
                  icon={Copy}
                  aria-label="Copy workflow ID"
                  onClick={() => void copy(w.id, "workflow ID")}
                />
              </span>
            ),
          },
          {
            label: "Created",
            value: w.createdAt ? formatDate(w.createdAt) : "—",
          },
        ]}
      />
      <pre className="on-code wf-prompt">{prompt}</pre>
    </>
  );
  return (
    <Page>
      {header}
      <div className="on-stack">
        <RefreshFailed query={workflow} />
        {ran ? (
          <section className="wf-insights" aria-label="Insights">
            <InsightsToolbar state={insights} own={own} />
            <RefreshFailed query={insights.query} />
            {data ? (
              <StatStrip data={data} />
            ) : (
              <InsightsPending state={insights} />
            )}
          </section>
        ) : (
          <Panel
            title="Connect your agent"
            subtitle={`Give the agent this prompt, with the Helix MCP added. In ${workspaceName}, each request it takes on becomes a run here.`}
            actions={copyPrompt}
          >
            {connection}
          </Panel>
        )}
        <WorkflowRuns
          workflowId={w.id}
          own={own}
          insights={insights}
          issue={issue}
          onIssue={setIssue}
        />
        {ran && data && (
          <ChangesPanel
            data={data}
            issue={issue?.key}
            onIssue={(next) => {
              setIssue(next);
              if (next)
                document
                  .getElementById("wf-runs")
                  ?.scrollIntoView({ behavior: "smooth", block: "start" });
            }}
          />
        )}
      </div>
      {connecting && (
        <Dialog
          title="Connect your agent"
          description={`Give the agent this prompt, with the Helix MCP added. In ${workspaceName}, each request it takes on becomes a run here.`}
          size="lg"
          onClose={() => setConnecting(false)}
          footer={copyPrompt}
        >
          {connection}
        </Dialog>
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${w.name}?`}
          description="Its runs and what they recorded go with it. An agent still using its ID can’t start runs any more."
          confirmLabel="Delete workflow"
          tone="danger"
          pending={pending}
          onCancel={() => setDeleting(false)}
          onConfirm={async () => {
            setPending(true);
            setDeleteError("");
            try {
              await deleteWorkflow(ws, o, w.id);
              trackEvent("workflow_deleted");
              qc.removeQueries({
                queryKey: workflowQueries.workflow(ws, o, w.id).queryKey,
              });
              await qc.invalidateQueries({
                queryKey: workflowQueries.list(ws, o).queryKey,
              });
              navigate(paths.workflows(o));
            } catch (err) {
              setDeleteError((err as Error).message);
              setPending(false);
            }
          }}
        >
          {deleteError && (
            <p role="alert" className="hf-form-error">
              {deleteError}
            </p>
          )}
        </ConfirmDialog>
      )}
    </Page>
  );
}

/**
 * A workflow's runs of the window, newest first: each its request, what its
 * assessment said of it, its claims grounded and when it started, opening to
 * its claims and gaps. Narrowed by who started them (the toolbar), what the
 * assessment found (`?verdict=`, which the overview links) and a gap or
 * suggestion (What to change). Read 50 at a time as the backend pages them:
 * the window's runs past the pages read come with Load more.
 */
function WorkflowRuns({
  workflowId,
  own,
  insights,
  issue,
  onIssue,
}: {
  workflowId: string;
  own: boolean;
  insights: InsightsState;
  issue?: { key: string; text: string };
  onIssue: (issue?: { key: string; text: string }) => void;
}) {
  const { id: ws, user } = useWorkspace(),
    { id: o } = useOntology();
  const names = useStarterNames();
  const runs = useInfiniteQuery(workflowQueries.runs(ws, o, workflowId));
  const [params, setParams] = useSearchParams();
  const asked = params.get("verdict");
  const filter: RunFilter = isRunFilter(asked) ? asked : "all";
  const [opened, setOpened] = useState<string[]>([]),
    [limit, setLimit] = useState(RUNS_SHOWN);
  const now = useNow(60_000);
  const from = windowStart(insights.days, new Date(now));
  const loaded = (runs.data?.pages || []).flatMap((p) => p.runs || []);
  const shown = loaded.filter((r) =>
    runShown(r, { from, who: insights.who, filter, issue: issue?.key }),
  );
  const oldest = loaded.at(-1)?.startedAt;
  // Runs of the window may be on pages not read yet.
  const unread = runs.hasNextPage && !!oldest && Date.parse(oldest) >= from;
  const more = shown.length > limit || unread;
  // The window's runs by filter, as the strip counts them.
  const t = insights.query.data?.totals;
  const counts: Partial<Record<RunFilter, number>> = t
    ? {
        all: t.runs || 0,
        helped: t.helped || 0,
        not_helped: verdictRuns(t, "marginal") + verdictRuns(t, "unused"),
        misleading: verdictRuns(t, "misleading"),
        pending: (t.runs || 0) - (t.assessed || 0),
      }
    : {};
  const choose = (f: string) =>
    setParams(
      (p) => {
        if (f === "all") p.delete("verdict");
        else p.set("verdict", f);
        return p;
      },
      { replace: true },
    );
  return (
    <Panel
      title={own ? "Your runs" : "Runs"}
      subtitle={[
        `Newest first, from the last ${insights.days} days.`,
        own &&
          "You can read only part of this ontology, so you see the runs you started.",
      ]
        .filter(Boolean)
        .join(" ")}
      padded={false}
      className="wf-runs-panel"
    >
      <div id="wf-runs" className="wf-runs-bar">
        <Segmented
          label="Show runs"
          value={filter}
          onChange={choose}
          options={(Object.keys(RUN_FILTERS) as RunFilter[]).map((f) => ({
            value: f,
            label: RUN_FILTERS[f],
            count: counts[f],
          }))}
        />
        {issue && (
          <span className="wf-runs-issue">
            <span>
              Naming <q>{issue.text}</q>
            </span>
            <IconButton
              size="sm"
              icon={X}
              label="Show every run"
              onClick={() => onIssue(undefined)}
            />
          </span>
        )}
      </div>
      <RefreshFailed query={runs} />
      {runs.isPending ? (
        <div className="on-pad">
          <SkeletonText lines={4} />
        </div>
      ) : !loaded.length ? (
        <EmptyState
          size="sm"
          icon={Workflow}
          title={runs.error ? "Couldn’t load the runs" : "No runs yet"}
          text={
            runs.error?.message ||
            "Once the agent starts a run with this workflow’s ID, it shows here."
          }
        />
      ) : (
        <>
          {!shown.length ? (
            <p className="wf-runs-none">
              {unread
                ? "None of the runs read so far match."
                : `No runs match in the last ${insights.days} days.`}
            </p>
          ) : (
            <>
              <div className="wf-run wf-runs-head" aria-hidden>
                <span>Request</span>
                <span>Verdict</span>
                <span className="is-right">Claims grounded</span>
                <span className="is-right">Started</span>
                <span />
              </div>
              <ul className="wf-runs" aria-label="Runs">
                {shown.slice(0, limit).map((r) => {
                  const a = assessmentOf(r.assessment?.assessmentJson);
                  const backed = (a?.claims || []).filter(
                    (c) => c.support === "supported",
                  ).length;
                  const open = opened.includes(r.id);
                  const request = preview(r.input);
                  return (
                    <li key={r.id} className={cx("wf-run", open && "is-open")}>
                      <span className="wf-run-main">
                        <Link
                          className="wf-run-request"
                          to={paths.workflowRun(o, workflowId, r.id)}
                        >
                          {request || <em>Not sent yet</em>}
                        </Link>
                        <span className="wf-run-summary">
                          {a?.summary ||
                            (r.status === "open"
                              ? "The agent is still recording its calls."
                              : r.assessment?.error || "")}
                        </span>
                        <span className="wf-run-meta">
                          {countsText(r.counts)} ·{" "}
                          {starterLabel(starterOf(r), names, user.id)}
                        </span>
                      </span>
                      <span className="wf-run-verdict">
                        <RunVerdict run={r} />
                      </span>
                      <span className="wf-run-claims">
                        {a?.claims.length
                          ? `${backed} of ${a.claims.length}`
                          : "—"}
                      </span>
                      <span className="wf-run-at">
                        {r.startedAt ? formatRelative(r.startedAt) : "—"}
                      </span>
                      <span className="wf-run-toggle">
                        {a && (
                          <IconButton
                            size="sm"
                            icon={open ? ChevronUp : ChevronDown}
                            label={`${open ? "Hide" : "Show"} what the assessment found`}
                            aria-expanded={open}
                            onClick={() =>
                              setOpened((was) =>
                                open
                                  ? was.filter((x) => x !== r.id)
                                  : [...was, r.id],
                              )
                            }
                          />
                        )}
                      </span>
                      {open && a && (
                        <div className="wf-run-detail wf-assessment">
                          {!!a.claims.length && (
                            <section aria-label="Claims">
                              <h4>The answer’s claims</h4>
                              <ul>
                                {a.claims.map((c, i) => (
                                  <li key={i}>
                                    <Tag tone={SUPPORT[c.support].tone}>
                                      {SUPPORT[c.support].label}
                                    </Tag>{" "}
                                    {c.text}
                                  </li>
                                ))}
                              </ul>
                            </section>
                          )}
                          {!!a.gaps.length && (
                            <section aria-label="Gaps">
                              <h4>What the ontology lacked</h4>
                              <ul>
                                {a.gaps.map((g, i) => (
                                  <li key={i}>
                                    <Tag>{g.label}</Tag> {g.what}
                                  </li>
                                ))}
                              </ul>
                            </section>
                          )}
                          <Link to={paths.workflowRun(o, workflowId, r.id)}>
                            Open the run: its calls and answer
                          </Link>
                        </div>
                      )}
                    </li>
                  );
                })}
              </ul>
            </>
          )}
          {more && (
            <div className="wf-runs-more">
              <span className="on-muted">
                {shown.length > limit
                  ? `${formatNumber(limit)} of ${formatNumber(shown.length)}${unread ? "+" : ""} shown`
                  : `${formatNumber(shown.length)} shown`}
              </span>
              <Button
                size="sm"
                pending={runs.isFetchingNextPage}
                onClick={() => {
                  setLimit(limit + RUNS_MORE);
                  if (shown.length < limit + RUNS_MORE && unread)
                    void runs.fetchNextPage();
                }}
              >
                Show more runs
              </Button>
            </div>
          )}
        </>
      )}
    </Panel>
  );
}

// The runs the list shows at first, and how many more at a time.
const RUNS_SHOWN = 10,
  RUNS_MORE = 20;

// How a claim's support reads.
const SUPPORT = {
  supported: { label: "Supported", tone: "success" },
  unsupported: { label: "Unsupported", tone: "warning" },
  contradicted: { label: "Contradicted", tone: "danger" },
} as const;

// /ontology/:ontologyId/workflows/:workflowId/runs/:runId
export function WorkflowRunPage() {
  const { workflowId = "", runId = "" } = useParams();
  const { id: ws } = useWorkspace(),
    { id: o } = useOntology();
  const crumbs = useCrumbs();
  const workflow = useQuery(workflowQueries.workflow(ws, o, workflowId)),
    run = useQuery(workflowQueries.run(ws, o, workflowId, runId)),
    r = run.data?.run,
    // An open run's calls are asked again whenever the run is.
    // Asked again whenever the run counts another call, and every few
    // seconds while it is open, keeping what it showed until then.
    accessesQuery = workflowQueries.accesses(ws, o, workflowId, runId),
    accesses = useQuery({
      ...accessesQuery,
      queryKey: [...accessesQuery.queryKey, r?.counts?.accesses ?? 0],
      placeholderData: (previous) => previous,
      enabled: !!r,
      refetchInterval: r?.status === "open" ? OPEN_RUN_POLL_MS : false,
    });
  useTitle("Run");
  const name = workflow.data?.workflow.name || "Workflow";
  const header = (
    <PageHeader
      breadcrumbs={[
        ...crumbs,
        { label: name, to: paths.workflow(o, workflowId) },
        { label: "Run" },
      ]}
      title="Run"
      subtitle={
        r &&
        [
          r.startedAt && `Started ${formatDate(r.startedAt)}`,
          runDuration(r) && `took ${runDuration(r)}`,
        ]
          .filter(Boolean)
          .join(", ")
      }
      meta={r && runStatus(r)}
    />
  );
  if (run.isPending)
    return (
      <Page>
        {header}
        <SkeletonText lines={8} />
      </Page>
    );
  if (!r)
    return (
      <Page>
        {header}
        <EmptyState
          icon={Workflow}
          title="Couldn’t load the run"
          text={run.error?.message}
          action={<Button onClick={() => void run.refetch()}>Retry</Button>}
        />
      </Page>
    );
  const views = (accesses.data || []).flatMap((a) => accessView(a) ?? []);
  return (
    <Page>
      {header}
      <div className="on-stack">
        <RefreshFailed query={run} />
        <Panel title="Request">
          {r.input ? (
            <p className="wf-text">{r.input}</p>
          ) : (
            <p className="on-muted">
              The agent sends the request when it starts or finishes the run.
            </p>
          )}
        </Panel>
        <Panel
          title="What the agent read and wrote"
          subtitle={countsText(r.counts)}
          padded={false}
        >
          <RefreshFailed query={accesses} />
          {accesses.isPending ? (
            <div className="on-pad">
              <SkeletonText lines={4} />
            </div>
          ) : !views.length ? (
            <EmptyState
              size="sm"
              icon={Workflow}
              title={
                accesses.error
                  ? "Couldn’t load what it recorded"
                  : "Nothing recorded"
              }
              text={
                accesses.error?.message ||
                (r.status === "open"
                  ? "Calls the agent makes with this run’s ID show here as it makes them."
                  : "The agent made no Foundry call with this run’s ID.")
              }
            />
          ) : (
            <ol className="wf-timeline" aria-label="Recorded calls">
              {views.map((v) => (
                <li key={v.seq}>
                  <AccessItem view={v} />
                </li>
              ))}
            </ol>
          )}
        </Panel>
        <Panel title="Answer">
          {r.output ? (
            <p className="wf-text">{r.output}</p>
          ) : (
            <p className="on-muted">
              The run is open: the agent sends its answer when it finishes it.
            </p>
          )}
        </Panel>
        {r.assessment && <AssessmentPanel run={r} workflowId={workflowId} />}
      </div>
    </Page>
  );
}

function AccessItem({ view: v }: { view: AccessView }) {
  const { id: o } = useOntology();
  return (
    <article
      className="wf-access"
      id={`call-${v.seq}`}
      aria-label={`${v.seq}. ${accessTitle(v)}`}
    >
      <header>
        <span className="wf-seq">{v.seq}</span>
        <strong>{accessTitle(v)}</strong>
        {v.at && (
          <time className="on-muted" dateTime={v.at}>
            {formatDate(v.at)}
          </time>
        )}
      </header>
      {v.kind === "read" && (
        <>
          <p className="on-muted">
            {readText(v)} · <Link to={paths.load(o, v.loadId)}>its load</Link>
          </p>
          {!!v.entities.length && (
            <ul className="wf-entities" aria-label="Entities it returned">
              {v.entities.map((e) => (
                <li key={e.type + "\u0000" + e.id}>
                  <Tag>
                    {e.type}: {e.name || e.id}
                  </Tag>
                </li>
              ))}
            </ul>
          )}
          <details>
            <summary>Reads and result</summary>
            <pre className="on-code">{pretty(v.readsJson)}</pre>
            {v.resultJson && (
              <pre className="on-code">{pretty(v.resultJson)}</pre>
            )}
          </details>
        </>
      )}
      {v.kind === "question" && (
        <p className="on-muted">
          <Link to={paths.ask(o, { thread: v.questionId })}>
            Open it in Analyst
          </Link>
        </p>
      )}
      {v.kind === "send" && (
        <p className="on-muted">
          {v.pieceIds.slice(0, 10).join(", ")}
          {v.pieceIds.length > 10 &&
            ` and ${v.pieceIds.length - 10} more`} ·{" "}
          <Link to={paths.connection(v.connectorId)}>its API source</Link>
        </p>
      )}
      {v.kind === "upload" && (
        <p className="on-muted">
          <Link to={paths.dataDocument(v.documentId)}>Open the document</Link>
        </p>
      )}
    </article>
  );
}

// JSON indented, or as it was when it isn't JSON.
function pretty(json: string): string {
  try {
    return JSON.stringify(JSON.parse(json), null, 2);
  } catch {
    return json;
  }
}

// What the assessment of a finished run says: how much the ontology helped,
// which of the answer's claims the calls bear out, what the ontology lacked,
// and changes to draft. An owner or admin asks for it again.
function AssessmentPanel({
  run: r,
  workflowId,
}: {
  run: FoundryWorkflowRun;
  workflowId: string;
}) {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology(),
    qc = useQueryClient();
  const [pending, setPending] = useState(false),
    [error, setError] = useState("");
  const a = r.assessment!;
  const assessment =
    a.status === "done" ? assessmentOf(a.assessmentJson) : null;
  const live = a.status === "queued" || a.status === "running";
  const again = canManage(role) && !live && (
    <Button
      size="sm"
      icon={RefreshCw}
      pending={pending}
      onClick={async () => {
        setPending(true);
        setError("");
        try {
          const done = await assessRun(ws, o, workflowId, r.id);
          qc.setQueryData(
            workflowQueries.run(ws, o, workflowId, r.id).queryKey,
            done,
          );
        } catch (err) {
          setError((err as Error).message);
        } finally {
          setPending(false);
        }
      }}
    >
      Assess again
    </Button>
  );
  const calls = (seqs: number[]) =>
    seqs.length > 0 && (
      <span className="wf-calls">
        {seqs.map((seq) => (
          <a key={seq} href={`#call-${seq}`}>
            #{seq}
          </a>
        ))}
      </span>
    );
  const verdict = assessment && VERDICT_LABELS[assessment.verdict];
  return (
    <Panel
      title="How much the ontology helped"
      subtitle={
        verdict ? <Tag tone={verdict.tone}>{verdict.label}</Tag> : undefined
      }
      actions={again}
    >
      {error && (
        <p role="alert" className="on-error-text">
          {error}
        </p>
      )}
      {live ? (
        <p className="on-muted" role="status">
          Assessing the run: comparing its answer with what it read.
        </p>
      ) : !assessment ? (
        <p className="on-muted">
          {a.error || "The assessment couldn’t be read."}
        </p>
      ) : (
        <div className="wf-assessment">
          {assessment.summary && <p>{assessment.summary}</p>}
          {!!assessment.claims.length && (
            <section aria-label="Claims">
              <h4>The answer’s claims</h4>
              <ul>
                {assessment.claims.map((c, i) => (
                  <li key={i}>
                    <Tag tone={SUPPORT[c.support].tone}>
                      {SUPPORT[c.support].label}
                    </Tag>{" "}
                    {c.text} {calls(c.calls)}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {!!assessment.gaps.length && (
            <section aria-label="Gaps">
              <h4>What the ontology lacked</h4>
              <ul>
                {assessment.gaps.map((g, i) => (
                  <li key={i}>
                    <Tag>{g.label}</Tag> {g.what} {calls(g.calls)}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {!!assessment.suggestions.length && (
            <section aria-label="Suggestions">
              <h4>Changes that would help</h4>
              <ul>
                {assessment.suggestions.map((s, i) => (
                  <li key={i}>
                    {s}{" "}
                    {canManage(role) && (
                      <Link
                        to={paths.generate(o, {
                          goal: suggestionGoal([s], assessment.gaps, MAX_GOAL),
                        })}
                      >
                        Draft a version with this
                      </Link>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          )}
          {!!a.leftOut?.length && (
            <p className="on-muted">
              Left out to fit the model (their results, or the whole call):{" "}
              {a.leftOut.map((n) => `#${n}`).join(", ")}.
            </p>
          )}
        </div>
      )}
    </Panel>
  );
}
