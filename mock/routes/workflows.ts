import type {
  CreateFoundryWorkflowRequest,
  FinishFoundryWorkflowRunRequest,
  FoundryWorkflow,
  FoundryWorkflowAccess,
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
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { onReset } from "../state";
import { isScope, ontologyOf } from "./ontologies";

// Workflows, as the backend serves them (api/workflows.go): an agent's task
// over one ontology. A run of one is started and finished by the agent (in
// tests, through /__mock/agent, mock/server.ts), and records every read and
// question of the ontology made with its id (workflowRunId on
// .../loads/{loadId}:read and .../questions, tracked() below) and every send
// to an API source (the /__mock/event-sources stand-in's workflowRunId), with
// the backend's caps and its once-per-question and once-per-send retries. A
// grantee gets no workflow route (./access.ts names none) and records
// nothing. Deleting the ontology leaves its workflows here, unread, as no
// route reaches them.

const BASE =
  "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows";

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
  workspaceId: string;
  ontologyId: string;
  workflowId: string;
  clientToken: string;
  startedAt: string;
  resultBytes: number;
  accesses: (FoundryWorkflowAccess & { dedupe?: string })[];
};

const state = {
  workflows: [] as StoredWorkflow[],
  runs: [] as StoredRun[],
};
onReset(() => {
  state.workflows = [];
  state.runs = [];
});

const bytes = (s: string) => new TextEncoder().encode(s).length;

function toWorkflow(w: StoredWorkflow): FoundryWorkflow {
  const runs = state.runs.filter((r) => r.workflowId === w.id);
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
  };
}

function toRun(r: StoredRun): FoundryWorkflowRun {
  const count = (k: keyof FoundryWorkflowAccess) =>
    r.accesses.filter((a) => a[k]).length;
  return {
    id: r.id,
    workflowId: r.workflowId,
    status: r.status,
    ...(r.input !== undefined && { input: r.input }),
    ...(r.output !== undefined && { output: r.output }),
    startedByUserId: r.startedByUserId,
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
): StoredRun | MockReply {
  return (
    state.runs.find(
      (r) =>
        r.id === runId &&
        r.ontologyId === ontologyId &&
        r.workflowId === workflowId,
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
  grantee: boolean,
): StoredRun | MockReply {
  if (grantee)
    return fail(
      "PERMISSION_DENIED",
      "workflow runs are not open to restricted members and keys",
    );
  const r = state.runs.find(
    (x) =>
      x.id === runId &&
      x.workspaceId === workspaceId &&
      (ontologyId === undefined || x.ontologyId === ontologyId),
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
  const run = openRun(
    req.params.workspaceId!,
    ontologyId,
    runId,
    !!req.grantee,
  );
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
    path: BASE,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      return ok({
        workflows: state.workflows
          .filter((w) => w.ontologyId === s.o.id)
          .toReversed()
          .map(toWorkflow),
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
      return ok({ workflow: toWorkflow(w) } satisfies FoundryWorkflowResponse);
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
      return ok({
        runs: state.runs
          .filter((r) => r.workflowId === w.id)
          .toReversed()
          .map(toRun),
      } satisfies ListFoundryWorkflowRunsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{workflowId}/runs/{id}`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const r = runOf(s.o.id, req.params.workflowId, req.params.id);
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
      const r = runOf(s.o.id, req.params.workflowId, req.params.id);
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
      return ok({ run: toRun(r) } satisfies FoundryWorkflowRunResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{workflowId}/runs/{id}/accesses`,
    handle: (req) => {
      const s = ontologyOf(req);
      if (!isScope(s)) return s;
      const r = runOf(s.o.id, req.params.workflowId, req.params.id);
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
