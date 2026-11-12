// Workflows area: an ontology's workflows (an agent's task) and what their
// runs recorded, lazy-loaded by the Ontology area's route table under
// /ontology/:ontologyId/workflows. Owners and admins create and delete a
// workflow; anyone else reads them. An agent starts and finishes a run
// through the Helix MCP, with the workflow's id, and passes the run's id to
// every read, question, send and upload it makes for the request: the run's
// page shows the request, those calls and the answer side by side. Nothing
// here starts a run.
import "./workflows.css";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Copy, Plus, RefreshCw, Trash2, Workflow } from "lucide-react";
import type {
  FoundryWorkflow,
  FoundryWorkflowAssessment,
  FoundryWorkflowRun,
} from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import {
  Button,
  ConfirmDialog,
  DataGrid,
  Dialog,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  StatusBadge,
  Tag,
  formatDate,
  formatRelative,
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
  VERDICT_LABELS,
  accessTitle,
  accessView,
  assessmentOf,
  countsText,
  preview,
  readText,
  runDuration,
  suggestionGoal,
  verdictsText,
  workflowPrompt,
  type AccessView,
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

// A run's assessment in a cell: its verdict once done, else where it stands.
function AssessmentCell({ a }: { a?: FoundryWorkflowAssessment }) {
  if (!a) return <span className="on-muted">—</span>;
  if (a.status === "done" && a.verdict && a.verdict in VERDICT_LABELS) {
    const v = VERDICT_LABELS[a.verdict as Verdict];
    return <Tag tone={v.tone}>{v.label}</Tag>;
  }
  const words: Record<string, string> = {
    queued: "Assessing…",
    running: "Assessing…",
    failed: "Not assessed",
    skipped: "Not assessed",
  };
  return <span className="on-muted">{words[a.status] || "—"}</span>;
}

const runStatus = (r: FoundryWorkflowRun) =>
  r.status === "finished" ? (
    <StatusBadge status="succeeded" label="Finished" size="sm" />
  ) : (
    <StatusBadge status="running" label="Open" size="sm" />
  );

// /ontology/:ontologyId/workflows
export function WorkflowsPage() {
  const { id: ws, role } = useWorkspace(),
    { id: o } = useOntology();
  useTitle("Workflows");
  const crumbs = useCrumbs(),
    navigate = useNavigate();
  const list = useQuery(workflowQueries.list(ws, o));
  const [creating, setCreating] = useState(false);
  const header = (
    <PageHeader
      breadcrumbs={crumbs.slice(0, 2).concat({ label: "Workflows" })}
      title="Workflows"
      subtitle="An agent’s task over this ontology. Each run records the agent’s request, what it read and wrote here, and its answer."
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
                ? "Create one for each task an agent does with this ontology, then give the agent its prompt."
                : "An owner or admin creates one for each task an agent does with this ontology."
            }
          />
        ) : (
          <DataGrid<FoundryWorkflow>
            label="Workflows"
            rows={workflows}
            getRowKey={(w) => w.id}
            rowHeight={44}
            onRowClick={(w) => navigate(paths.workflow(o, w.id))}
            columns={[
              {
                key: "name",
                label: "Name",
                render: (w) => (
                  <Link
                    className="on-row-link on-cell-text"
                    to={paths.workflow(o, w.id)}
                  >
                    {w.name}
                  </Link>
                ),
              },
              {
                key: "runs",
                label: "Runs",
                align: "right",
                width: 90,
                sortValue: (w) => Number(w.runs || 0),
                render: (w) => w.runs || "0",
              },
              {
                key: "finishedRuns",
                label: "Finished",
                align: "right",
                width: 100,
                sortValue: (w) => Number(w.finishedRuns || 0),
                render: (w) => w.finishedRuns || "0",
              },
              {
                key: "lastRunAt",
                label: "Last run",
                width: 150,
                render: (w) =>
                  w.lastRunAt ? formatRelative(w.lastRunAt) : "Never",
              },
            ]}
          />
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

// /ontology/:ontologyId/workflows/:workflowId
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
  const workflow = useQuery(workflowQueries.workflow(ws, o, workflowId)),
    runs = useQuery(workflowQueries.runs(ws, o, workflowId));
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
        w &&
        canManage(role) && (
          <Button icon={Trash2} onClick={() => setDeleting(true)}>
            Delete
          </Button>
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
  const list = runs.data?.runs || [];
  return (
    <Page>
      {header}
      <div className="on-stack">
        <RefreshFailed query={workflow} />
        <Panel
          title="Connect your agent"
          subtitle={`Give the agent this prompt, with the Helix MCP added. In ${workspaceName}, each request it takes on becomes a run here.`}
          actions={
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
          }
        >
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
        </Panel>
        <Panel
          title={own ? "Your runs" : "Runs"}
          subtitle={[
            verdictsText(w.verdicts) &&
              `Assessed: ${verdictsText(w.verdicts)}.`,
            "The newest first.",
            own &&
              "You can read only part of this ontology, so you see the runs you started.",
          ]
            .filter(Boolean)
            .join(" ")}
          padded={false}
        >
          <RefreshFailed query={runs} />
          {runs.isPending ? (
            <div className="on-pad">
              <SkeletonText lines={4} />
            </div>
          ) : !list.length ? (
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
            <DataGrid<FoundryWorkflowRun>
              label="Runs"
              rows={list}
              getRowKey={(r) => r.id}
              rowHeight={44}
              onRowClick={(r) => navigate(paths.workflowRun(o, w.id, r.id))}
              columns={[
                {
                  key: "input",
                  label: "Request",
                  render: (r) => (
                    <Link
                      className="on-row-link on-cell-text"
                      to={paths.workflowRun(o, w.id, r.id)}
                    >
                      {preview(r.input) || <em>Not sent yet</em>}
                    </Link>
                  ),
                },
                {
                  key: "status",
                  label: "Status",
                  width: 120,
                  render: runStatus,
                },
                {
                  key: "assessment",
                  label: "Ontology",
                  width: 140,
                  render: (r) => <AssessmentCell a={r.assessment} />,
                },
                {
                  key: "counts",
                  label: "Recorded",
                  width: 240,
                  render: (r) => (
                    <span className="on-cell-text">{countsText(r.counts)}</span>
                  ),
                },
                {
                  key: "startedAt",
                  label: "Started",
                  width: 150,
                  render: (r) =>
                    r.startedAt ? formatRelative(r.startedAt) : "—",
                },
              ]}
            />
          )}
        </Panel>
      </div>
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
                    <Tag
                      tone={
                        c.support === "supported"
                          ? "success"
                          : c.support === "contradicted"
                            ? "danger"
                            : "warning"
                      }
                    >
                      {c.support === "supported"
                        ? "Supported"
                        : c.support === "contradicted"
                          ? "Contradicted"
                          : "Unsupported"}
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
                          goal: suggestionGoal(s, assessment.gaps, MAX_GOAL),
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
