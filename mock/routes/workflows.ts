import type {
  CreateFoundryWorkflowRequest,
  FinishFoundryWorkflowRunRequest,
  FoundryWorkflow,
  FoundryWorkflowAccess,
  FoundryWorkflowAssessment,
  FoundryWorkflowResponse,
  FoundryWorkflowRun,
  FoundryWorkflowRunResponse,
  ListFoundryWorkflowRunAccessesResponse,
  ListFoundryWorkflowRunsResponse,
  ListFoundryWorkflowsResponse,
  StartFoundryWorkflowRunRequest,
} from "../../src/shared/wfe";
import {
  fail,
  id,
  now,
  ok,
  type MockGrantee,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { config, onReset } from "../state";
import { workflowDemo } from "../fixtures/workflow-demo";
import { WORKFLOWS_OFF, workflowsOn } from "./access";
import { isScope, ontologyOf } from "./ontologies";
import { insights, starterOf } from "./workflow-insights";

// Workflows, as the backend serves them (api/workflows.go): an agent's task
// over one ontology. A run of one is started and finished by the agent (in
// tests, through /__mock/agent, mock/server.ts), and records every read and
// question of the ontology made with its id (workflowRunId on
// .../loads/{loadId}:read and .../questions, tracked() below) and every send
// to an API source (the /__mock/event-sources stand-in's workflowRunId), with
// the backend's caps and its once-per-question and once-per-send retries. A
// grantee (a restricted member) reaches them with its Workflows permission
// (./access.ts), and, holding only part of the ontology, sees and records
// under only the runs it started (own). Finishing a run queues its assessment, which ends `assessMs` later
// with the mock agent's assessment (mockAssessment), held to the engine's
// assess_result.json in the unit tests; an owner or admin asks for one again
// (:assess). Deleting the ontology leaves its workflows here, unread, as no
// route reaches them.

const BASE =
  "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows";

// A page of runs, as the backend's (api/workflows.go workflowRunsPage).
export const RUNS_PAGE = 50;

// The backend's caps (store/model/foundry_workflow.go).
export const WORKFLOW_CAPS = {
  workflows: 100,
  name: 100,
  description: 2000,
  runsKept: 1000,
  accesses: 1000,
  input: 64 << 10,
  output: 256 << 10,
  result: 256 << 10,
  runResults: 4 << 20,
};

type StoredWorkflow = FoundryWorkflow & {
  workspaceId: string;
  ontologyId: string;
  clientToken: string;
  createdAt: string;
};
type StoredRun = FoundryWorkflowRun & {
  // Who started it: a user, or an agent's key.
  startedByServiceCredentialId?: string;
  workspaceId: string;
  ontologyId: string;
  workflowId: string;
  clientToken: string;
  startedAt: string;
  resultBytes: number;
  accesses: (FoundryWorkflowAccess & { dedupe?: string })[];
  // Its assessment once it finished, and when a live one ends.
  assessment?: FoundryWorkflowAssessment & { startsAt: number; endsAt: number };
};

// Knobs, through POST /__mock/config: `assessEnabled: false` (the
// deployment's foundry.workflows.assess.enabled off: finishing queues none),
// `assessMs` (how long an assessment runs, 1500 by default), and
// `assessOutcome: "fail" | "skip"` (the model refuses, or the month's
// budget is spent).
type Knobs = {
  assessEnabled?: boolean;
  assessMs?: number;
  assessOutcome?: "fail" | "skip";
  // Seeds Customer contracts' demo workflows (../fixtures/workflow-demo.ts),
  // as MOCK_WORKFLOW_DEMO does at start.
  workflowDemo?: boolean;
};
const knobs = config as typeof config & Knobs;
onReset(() => {
  delete knobs.assessEnabled;
  delete knobs.assessMs;
  delete knobs.assessOutcome;
  delete knobs.workflowDemo;
});

/** What one assessment spends in the mock, in millionths of a dollar. */
export const ASSESSMENT_USD_MICROS = 4200;
/** The backend's default monthly budget for assessments. */
export const ASSESSMENTS_CAP_MICROS = 20_000_000;

const state = {
  workflows: [] as StoredWorkflow[],
  runs: [] as StoredRun[],
  demo: false,
};
onReset(() => {
  state.workflows = [];
  state.runs = [];
  state.demo = false;
});

const bytes = (s: string) => new TextEncoder().encode(s).length;

/**
 * The engine's Assessment (assess_result.json `$defs/Assessment`) of a run,
 * as the mock agent writes it from the run's calls: its reads that answered
 * and its answered questions support the answer's first sentence; a read
 * that failed, or a question the ontology couldn't answer, is a gap, with a
 * suggestion to close it. No call is unused.
 */
export function mockAssessment(r: {
  output?: string;
  accesses: FoundryWorkflowAccess[];
}): object {
  const sentence =
    (r.output || "")
      .split(/(?<=[.!?])\s/)[0]
      ?.trim()
      .slice(0, 500) || "The answer";
  const answered: number[] = [],
    missing: number[] = [];
  for (const a of r.accesses) {
    if (a.read) {
      const results =
        (JSON.parse(a.read.resultJson || "{}") as { results?: object[] })
          .results || [];
      if (results.some((x) => "ok" in x)) answered.push(a.seq);
      if (results.some((x) => "error" in x)) missing.push(a.seq);
    } else if (a.question) answered.push(a.seq);
  }
  if (!r.accesses.length)
    return {
      verdict: "unused",
      summary:
        "The agent made no call to the ontology, so nothing in its answer came from it.",
      claims: [{ text: sentence, support: "unsupported", calls: [] }],
      gaps: [],
      suggestions: [],
    };
  return {
    verdict:
      answered.length > 1
        ? "essential"
        : answered.length
          ? "helpful"
          : "marginal",
    summary: answered.length
      ? "What the agent read of the ontology backs its answer."
      : "The agent read the ontology, but what it read doesn't bear on its answer.",
    claims: [
      {
        text: sentence,
        support: answered.length ? "supported" : "unsupported",
        calls: answered,
      },
    ],
    gaps: missing.length
      ? [
          {
            kind: "missing_data",
            what: "An entity the agent read for isn't in the load",
            calls: missing,
          },
        ]
      : [],
    suggestions: missing.length
      ? ["Load the source that holds the entities the agent looked for."]
      : [],
  };
}

/** Ends a live assessment whose time has come, as the worker would. */
function settle(r: StoredRun) {
  const a = r.assessment;
  if (!a || (a.status !== "queued" && a.status !== "running")) return;
  if (Date.now() < a.endsAt) {
    // A worker claims it a third of the way in.
    a.status = Date.now() >= a.startsAt ? "running" : "queued";
    return;
  }
  const finishedAt = now();
  if (knobs.assessOutcome === "skip") {
    Object.assign(a, {
      status: "skipped",
      error:
        "this workspace's $20.00 of run assessments for the month is spent",
      finishedAt,
    });
    return;
  }
  if (knobs.assessOutcome === "fail") {
    Object.assign(a, {
      status: "failed",
      error: "the model declined to assess this run",
      finishedAt,
      usdMicros: String(Number(a.usdMicros || 0) + 100),
    });
    return;
  }
  const assessment = mockAssessment(r) as { verdict: string };
  Object.assign(a, {
    status: "done",
    verdict: assessment.verdict,
    assessmentJson: JSON.stringify(assessment),
    leftOut: r.accesses.filter((x) => x.read?.resultOmitted).map((x) => x.seq),
    error: "",
    finishedAt,
    usdMicros: String(Number(a.usdMicros || 0) + ASSESSMENT_USD_MICROS),
  });
}

/** Queues r's assessment, as finishing it or asking again does. */
function queueAssessment(r: StoredRun) {
  const ms = knobs.assessMs ?? 1500;
  r.assessment = {
    status: "queued",
    queuedAt: now(),
    usdMicros: r.assessment?.usdMicros || "0",
    startsAt: Date.now() + ms / 3,
    endsAt: Date.now() + ms,
  };
}

/** What a workspace's assessments charged this month. */
export function assessmentSpend(workspaceId: string): number {
  const at = new Date(),
    month = new Date(
      Date.UTC(at.getUTCFullYear(), at.getUTCMonth(), 1),
    ).toISOString();
  return state.runs
    .filter(
      (r) =>
        r.workspaceId === workspaceId &&
        (r.assessment?.finishedAt || "") >= month,
    )
    .reduce((n, r) => n + Number(r.assessment?.usdMicros || 0), 0);
}

/**
 * The member whose runs alone a call sees: a grantee's, while it holds only
 * part of the ontology's current load (the backend's ownRunsOnly).
 */
function own(req: MockRequest): string | undefined {
  const g = req.grantee;
  return g && (!g.load || g.load.partial) ? g.principal : undefined;
}

/** Whether `r` is a run `by` (a principal key, as own gives) started. */
const startedBy = (r: StoredRun, by: string | undefined) => {
  const s = starterOf(r);
  return !by || by === `${s.kind}:${s.id}`;
};

/**
 * Seeds Customer contracts' demo workflows and runs once, when the demo is
 * asked for (MOCK_WORKFLOW_DEMO or the workflowDemo knob).
 */
function ensureDemo() {
  if (state.demo || !(process.env.MOCK_WORKFLOW_DEMO || knobs.workflowDemo))
    return;
  state.demo = true;
  const demo = workflowDemo(new Date());
  const at = new Date(Date.now() - 46 * 86_400_000).toISOString();
  for (const w of demo.workflows)
    state.workflows.push({
      ...w,
      ontologyId: "ont_acme_contracts",
      workspaceId: "ws_acme",
      clientToken: w.id,
      createdByUserId: "usr_ada",
      createdAt: at,
      runs: "0",
      finishedRuns: "0",
    });
  for (const d of demo.runs) {
    const accesses: StoredRun["accesses"] = [];
    for (let i = 0; i < d.reads + d.questions; i++)
      accesses.push({
        seq: accesses.length + 1,
        at: d.startedAt,
        ...(i < d.reads
          ? { read: { loadId: "load_demo", readsJson: "[]", resultJson: "{}" } }
          : {
              question: { loadId: "load_demo", questionId: `q_${d.id}_${i}` },
            }),
      });
    state.runs.push({
      id: d.id,
      workflowId: d.workflowId,
      workspaceId: "ws_acme",
      ontologyId: "ont_acme_contracts",
      clientToken: d.id,
      status: d.output === undefined ? "open" : "finished",
      input: d.input,
      ...(d.output !== undefined && { output: d.output }),
      ...(d.startedByUserId && { startedByUserId: d.startedByUserId }),
      ...(d.startedByServiceCredentialId && {
        startedByServiceCredentialId: d.startedByServiceCredentialId,
      }),
      startedAt: d.startedAt,
      ...(d.finishedAt && { finishedAt: d.finishedAt }),
      resultBytes: 0,
      accesses,
      ...(d.verdict && {
        assessment: {
          status: "done",
          verdict: d.verdict,
          assessmentJson: d.assessmentJson,
          usdMicros: String(d.usdMicros),
          queuedAt: d.finishedAt,
          finishedAt: d.finishedAt,
          startsAt: 0,
          endsAt: 0,
        },
      }),
      ...(d.queued && {
        assessment: {
          status: "running",
          queuedAt: d.finishedAt,
          usdMicros: "0",
          startsAt: 0,
          endsAt: Date.now() + 3_600_000,
        },
      }),
    } as StoredRun);
  }
}

function toWorkflow(w: StoredWorkflow, by?: string): FoundryWorkflow {
  const runs = state.runs.filter(
    (r) => r.workflowId === w.id && startedBy(r, by),
  );
  runs.forEach(settle);
  const verdicts = ["essential", "helpful", "marginal", "unused", "misleading"]
    .map((verdict) => ({
      verdict,
      runs: String(
        runs.filter(
          (r) =>
            r.assessment?.status === "done" && r.assessment.verdict === verdict,
        ).length,
      ),
    }))
    .filter((v) => v.runs !== "0");
  const last = runs
    .map((r) => r.startedAt)
    .sort()
    .at(-1);
  return {
    id: w.id,
    ontologyId: w.ontologyId,
    name: w.name,
    description: w.description || "",
    createdByUserId: w.createdByUserId,
    createdAt: w.createdAt,
    runs: String(runs.length),
    finishedRuns: String(runs.filter((r) => r.status === "finished").length),
    ...(last && { lastRunAt: last }),
    ...(verdicts.length && { verdicts }),
  };
}

function toRun(r: StoredRun): FoundryWorkflowRun {
  settle(r);
  const count = (k: keyof FoundryWorkflowAccess) =>
    r.accesses.filter((a) => a[k]).length;
  const assessment =
    r.assessment && (({ startsAt: _, endsAt: __, ...a }) => a)(r.assessment);
  return {
    id: r.id,
    workflowId: r.workflowId,
    status: r.status,
    ...(r.input !== undefined && { input: r.input }),
    ...(r.output !== undefined && { output: r.output }),
    ...(r.startedByServiceCredentialId
      ? { startedByServiceCredentialId: r.startedByServiceCredentialId }
      : { startedByUserId: r.startedByUserId }),
    startedAt: r.startedAt,
    ...(r.finishedAt && { finishedAt: r.finishedAt }),
    counts: {
      accesses: r.accesses.length,
      reads: count("read"),
      questions: count("question"),
      sends: count("send"),
      uploads: count("upload"),
      pieces: r.accesses.reduce(
        (n, a) => n + (a.send?.pieceIds?.length || 0),
        0,
      ),
      resultsOmitted: r.accesses.filter((a) => a.read?.resultOmitted).length,
    },
    ...(assessment && { assessment }),
  };
}

function workflowOf(
  ontologyId: string,
  workflowId: string | undefined,
): StoredWorkflow | MockReply {
  return (
    state.workflows.find(
      (w) => w.id === workflowId && w.ontologyId === ontologyId,
    ) || fail("NOT_FOUND", "workflow not found")
  );
}

function runOf(
  ontologyId: string,
  workflowId: string | undefined,
  runId: string | undefined,
  by?: string,
): StoredRun | MockReply {
  return (
    state.runs.find(
      (r) =>
        r.id === runId &&
        r.ontologyId === ontologyId &&
        r.workflowId === workflowId &&
        startedBy(r, by),
    ) || fail("NOT_FOUND", "workflow run not found")
  );
}

const isReplyOf = (x: object): x is MockReply => "status" in x && !("id" in x);

/** The run's input as sent: undefined for none, or why it is refused. */
function inputOf(input: unknown): string | undefined | MockReply {
  if (input === undefined || input === "") return undefined;
  if (
    typeof input !== "string" ||
    !input.trim() ||
    bytes(input) > WORKFLOW_CAPS.input
  )
    return fail(
      "INVALID_ARGUMENT",
      `input is at most ${WORKFLOW_CAPS.input} bytes of UTF-8, not only spaces`,
    );
  return input;
}

/** What a call recorded under a run: one of read, question, send, upload. */
export type WorkflowCall =
  | { read: { loadId: string; readsJson: string; resultJson: string } }
  | { question: { loadId: string; questionId: string } }
  | { send: { connectorId: string; seq: number; pieceIds: string[] } }
  | { upload: { documentId: string; setId: string } };

/**
 * The open run a call names to record what it accessed under, checked
 * before the call runs: a reply refusing it, or the run. `ontologyId` is
 * the ontology a call reads, or undefined for a call of the workspace (a
 * send).
 */
export function openRun(
  workspaceId: string,
  ontologyId: string | undefined,
  runId: string,
  grantee?: MockGrantee,
): StoredRun | MockReply {
  if (grantee && !workflowsOn(workspaceId, grantee.principal.split(":")[1]!))
    return fail("PERMISSION_DENIED", WORKFLOWS_OFF);
  const by =
    grantee && (!grantee.load || grantee.load.partial)
      ? grantee.principal
      : undefined;
  const r = state.runs.find(
    (x) =>
      x.id === runId &&
      x.workspaceId === workspaceId &&
      (ontologyId === undefined || x.ontologyId === ontologyId) &&
      startedBy(x, by),
  );
  if (!r) return fail("NOT_FOUND", "workflow run not found");
  if (r.status === "finished")
    return fail(
      "FAILED_PRECONDITION",
      "the workflow run finished: start another to record more",
    );
  if (r.accesses.length >= WORKFLOW_CAPS.accesses)
    return fail(
      "RESOURCE_EXHAUSTED",
      `a workflow run records up to ${WORKFLOW_CAPS.accesses} calls`,
    );
  return r;
}

/** Records call under run, once per question, send or upload. */
export function record(run: StoredRun, call: WorkflowCall) {
  const dedupe =
    "question" in call
      ? `question:${call.question.questionId}`
      : "send" in call
        ? `send:${call.send.connectorId}:${call.send.seq}`
        : "upload" in call
          ? `upload:${call.upload.documentId}`
          : undefined;
  if (dedupe && run.accesses.some((a) => a.dedupe === dedupe)) return;
  const access: FoundryWorkflowAccess & { dedupe?: string } = {
    seq: run.accesses.length + 1,
    at: now(),
    ...(dedupe && { dedupe }),
  };
  if ("read" in call) {
    const size = bytes(call.read.resultJson);
    const kept =
      size <= WORKFLOW_CAPS.result &&
      run.resultBytes + size <= WORKFLOW_CAPS.runResults;
    if (kept) run.resultBytes += size;
    access.read = {
      loadId: call.read.loadId,
      readsJson: call.read.readsJson,
      ...(kept
        ? { resultJson: call.read.resultJson }
        : { resultOmitted: true }),
    };
  } else if ("question" in call) access.question = call.question;
  else if ("send" in call)
    access.send = { ...call.send, seq: String(call.send.seq) };
  else access.upload = call.upload;
  run.accesses.push(access);
}

/**
 * A route's reply, recorded under the run its body names in
 * workflowRunId: the run is checked before `handle` runs, and `call` reads
 * what to record from its reply once it succeeded.
 */
export async function tracked(
  req: MockRequest,
  ontologyId: string | undefined,
  handle: () => MockReply | Promise<MockReply>,
  call: (body: unknown) => WorkflowCall,
): Promise<MockReply> {
  const runId = (req.body as { workflowRunId?: unknown } | undefined)
    ?.workflowRunId;
  if (typeof runId !== "string" || !runId) return handle();
  const run = openRun(req.params.workspaceId!, ontologyId, runId, req.grantee);
  if (isReplyOf(run)) return run;
  const reply = await handle();
  if (reply.status === 200) record(run, call(reply.body));
  return reply;
}

export const workflowRoutes: MockRoute[] = [
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const body = (req.body || {}) as Partial<CreateFoundryWorkflowRequest>;
      const name = (body.name || "").trim(),
        description = (body.description || "").trim(),
        clientToken = body.clientToken || id("wft");
      if (!name || [...name].length > WORKFLOW_CAPS.name)
        return fail(
          "INVALID_ARGUMENT",
          `name must be 1 to ${WORKFLOW_CAPS.name} characters`,
        );
      if ([...description].length > WORKFLOW_CAPS.description)
        return fail(
          "INVALID_ARGUMENT",
          `description must be at most ${WORKFLOW_CAPS.description} characters`,
        );
      const again = state.workflows.find(
        (w) => w.ontologyId === s.o.id && w.clientToken === clientToken,
      );
      if (again)
        return again.name === name && again.description === description
          ? ok({
              workflow: toWorkflow(again),
            } satisfies FoundryWorkflowResponse)
          : fail(
              "INVALID_ARGUMENT",
              "client_token was already used for another workflow",
            );
      if (
        state.workflows.filter((w) => w.ontologyId === s.o.id).length >=
        WORKFLOW_CAPS.workflows
      )
        return fail(
          "FAILED_PRECONDITION",
          `an ontology holds up to ${WORKFLOW_CAPS.workflows} workflows; delete one first`,
        );
      const w: StoredWorkflow = {
        id: id("wf"),
        workspaceId: s.m.workspaceId,
        ontologyId: s.o.id,
        name,
        description,
        clientToken,
        createdByUserId: req.principal.user.id,
        createdAt: now(),
      };
      state.workflows.push(w);
      return ok({ workflow: toWorkflow(w) } satisfies FoundryWorkflowResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}:insights`,
    handle: (req) => {
      ensureDemo();
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const workflowId = req.query.get("workflowId") || undefined;
      if (workflowId) {
        const w = workflowOf(s.o.id, workflowId);
        if (isReplyOf(w)) return w;
      }
      const days = Number(req.query.get("days") || 0);
      if (!Number.isInteger(days) || days < 0 || days > 90)
        return fail("INVALID_ARGUMENT", "days is 1 to 90");
      const kind = req.query.get("startedBy.kind"),
        who = req.query.get("startedBy.id");
      const by = own(req) || (kind && who ? `${kind}:${who}` : undefined);
      const runs = state.runs
        .filter(
          (r) =>
            r.ontologyId === s.o.id &&
            (!workflowId || r.workflowId === workflowId) &&
            startedBy(r, by),
        )
        .map((r) => ({ ...toRun(r), startedAt: r.startedAt }));
      return ok(
        insights(runs, { days: days || 30, now: new Date(), workflowId }),
      );
    },
  },
  {
    method: "GET",
    path: BASE,
    handle: (req) => {
      ensureDemo();
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      return ok({
        workflows: state.workflows
          .filter((w) => w.ontologyId === s.o.id)
          .toReversed()
          .map((w) => toWorkflow(w, own(req))),
      } satisfies ListFoundryWorkflowsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const w = workflowOf(s.o.id, req.params.id);
      if (isReplyOf(w)) return w;
      return ok({
        workflow: toWorkflow(w, own(req)),
      } satisfies FoundryWorkflowResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const w = workflowOf(s.o.id, req.params.id);
      if (isReplyOf(w)) return w;
      state.workflows = state.workflows.filter((x) => x !== w);
      state.runs = state.runs.filter((r) => r.workflowId !== w.id);
      return ok({});
    },
  },
  {
    method: "POST",
    path: `${BASE}/{workflowId}/runs`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const body = (req.body || {}) as Partial<StartFoundryWorkflowRunRequest>;
      const input = inputOf(body.input);
      if (typeof input === "object") return input;
      const w = workflowOf(s.o.id, req.params.workflowId);
      if (isReplyOf(w)) return w;
      const clientToken = body.clientToken || id("wfrt");
      const again = state.runs.find(
        (r) => r.workflowId === w.id && r.clientToken === clientToken,
      );
      if (again)
        return again.input === input
          ? ok({ run: toRun(again) } satisfies FoundryWorkflowRunResponse)
          : fail(
              "INVALID_ARGUMENT",
              "client_token was already used for another run",
            );
      const r: StoredRun = {
        id: id("wfr"),
        workflowId: w.id,
        workspaceId: s.m.workspaceId,
        ontologyId: s.o.id,
        clientToken,
        status: "open",
        ...(input !== undefined && { input }),
        startedByUserId: req.principal.user.id,
        startedAt: now(),
        resultBytes: 0,
        accesses: [],
      };
      state.runs.push(r);
      // A workflow keeps its newest runs.
      const runs = state.runs.filter((x) => x.workflowId === w.id);
      if (runs.length > WORKFLOW_CAPS.runsKept)
        state.runs = state.runs.filter((x) => x !== runs[0]);
      return ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{workflowId}/runs`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const w = workflowOf(s.o.id, req.params.workflowId);
      if (isReplyOf(w)) return w;
      // Newest first by when they started, RUNS_PAGE a page, as the backend
      // pages them (runs started in the same instant newest made first); the
      // token names the last run of the page before.
      const all = state.runs
        .filter((r) => r.workflowId === w.id && startedBy(r, own(req)))
        .toReversed()
        .toSorted((a, b) =>
          (b.startedAt || "").localeCompare(a.startedAt || ""),
        );
      const token = req.query.get("pageToken") || "";
      const from = token ? all.findIndex((r) => r.id === token) + 1 : 0;
      if (token && !from) return fail("INVALID_ARGUMENT", "invalid page_token");
      const page = all.slice(from, from + RUNS_PAGE);
      return ok({
        runs: page.map(toRun),
        ...(from + RUNS_PAGE < all.length && {
          nextPageToken: page.at(-1)!.id,
        }),
      } satisfies ListFoundryWorkflowRunsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{workflowId}/runs/{id}`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const r = runOf(s.o.id, req.params.workflowId, req.params.id, own(req));
      if (isReplyOf(r)) return r;
      return ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{workflowId}/runs/{id}:finish`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const body = (req.body || {}) as Partial<FinishFoundryWorkflowRunRequest>;
      const input = inputOf(body.input);
      if (typeof input === "object") return input;
      const output = body.output || "";
      if (!output.trim() || bytes(output) > WORKFLOW_CAPS.output)
        return fail(
          "INVALID_ARGUMENT",
          `output is required, at most ${WORKFLOW_CAPS.output} bytes of UTF-8`,
        );
      const r = runOf(s.o.id, req.params.workflowId, req.params.id, own(req));
      if (isReplyOf(r)) return r;
      if (r.status === "finished") {
        return r.output === output && (input === undefined || input === r.input)
          ? ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse)
          : fail(
              "FAILED_PRECONDITION",
              "the run already finished with another answer",
            );
      }
      if (r.input === undefined && input === undefined)
        return fail(
          "INVALID_ARGUMENT",
          "input is required: the run was started without it",
        );
      if (r.input !== undefined && input !== undefined && r.input !== input)
        return fail(
          "INVALID_ARGUMENT",
          "input differs from the one the run was started with",
        );
      Object.assign(r, {
        status: "finished",
        input: r.input ?? input,
        output,
        finishedAt: now(),
      });
      if (knobs.assessEnabled !== false) queueAssessment(r);
      return ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{workflowId}/runs/{id}:assess`,
    handle: (req) => {
      if (knobs.assessEnabled === false)
        return fail("FAILED_PRECONDITION", "run assessments are off here");
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      const r = runOf(s.o.id, req.params.workflowId, req.params.id, own(req));
      if (isReplyOf(r)) return r;
      settle(r);
      if (r.status !== "finished")
        return fail(
          "FAILED_PRECONDITION",
          "the run is still open: it is assessed once it finishes",
        );
      if (
        r.assessment?.status === "queued" ||
        r.assessment?.status === "running"
      )
        return fail("FAILED_PRECONDITION", "the run is being assessed already");
      queueAssessment(r);
      return ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{workflowId}/runs/{id}/accesses`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const r = runOf(s.o.id, req.params.workflowId, req.params.id, own(req));
      if (isReplyOf(r)) return r;
      const after = Math.max(Number(req.query.get("afterSeq") || 0), 0);
      const page = r.accesses.filter((a) => a.seq > after);
      return ok({
        accesses: page.slice(0, 100).map(({ dedupe: _, ...a }) => a),
        more: page.length > 100,
      } satisfies ListFoundryWorkflowRunAccessesResponse);
    },
  },
];
