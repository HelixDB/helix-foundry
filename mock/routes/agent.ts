import { createHash } from "node:crypto";
import type {
  AgentActionDisplay,
  AgentActionResponse,
  AgentActionStatus,
  AgentEvent,
  AgentPageContext,
  AgentThreadResponse,
  AgentTurnResponse,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  FoundryAgentEvent,
  FoundryAgentThread,
  FoundryAgentTurn,
  ListAgentThreadsResponse,
  ListAgentTurnEventsResponse,
  SendAgentMessageRequest,
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
import { config, isReply, membership, onReset } from "../state";
import {
  aiModel,
  aiProvider,
  aiRefusal,
  foundryPrecondition as refused,
} from "./ai";
import { currentLoadId, ontologyLoad, queueLoad } from "./loads";
import {
  ontologyById,
  workspaceOntologies,
  type StoredOntology,
} from "./ontologies";
import { currentVersionOf, ontologyVersions } from "./ontology";
import { manages } from "./workspaces";

// The Assistant, as the backend's API and foundry-agent-worker run it
// (backend/api/agent.go, backend/agent): a person's private threads, each
// message a turn that is queued, claimed and streams events the dock polls
// by afterSeq, in the backend's shapes (src/shared/wfe.ts "Agent"). The
// mock's agent is scripted, with the read tools' outputs in their shapes:
// - asked to load an ontology ("load", "reload"), it reads the workspace's
//   ontologies, then prepares create_load of the one named, else the page's,
//   else the newest (a card that warns it was not named), on the current
//   version if it can be loaded, else the newest finished agent draft. A
//   member's prepare is refused, as the backend authorizes it as them.
// - anything else, it reads the ontologies, and the one named or on the
//   page, and says what they hold.
// Running a card is the mock's real load (./loads.ts queueLoad), after the
// backend's checks: the person's own action, its hash, still waiting, its
// confirmation alive, the thread idle; then a continuation turn reads the
// load and reports it. A thread holds at most 3 waiting cards. Every event
// takes `agentStepMs`; `agentOutcome` "fail" fails a turn after its first
// call, "budget_exhausted" stops it at its cap; `agentActionTtlMs` is how
// long a card's confirmation lives (5 minutes).

type Knobs = {
  agentStepMs?: number;
  agentOutcome?: "fail" | "budget_exhausted";
  agentActionTtlMs?: number;
};
const knobs = config as typeof config & Knobs;

const STEP_MS = 400,
  ACTION_TTL_MS = 5 * 60_000,
  // An action executing this long reads unknown (FoundryAgentExecutingFor).
  EXECUTING_FOR_MS = 2 * 60_000,
  MAX_TEXT = 4000,
  MAX_TOKEN = 64,
  PER_HOUR = 30,
  MAX_PENDING = 3,
  PAGE = 50,
  TURNS = 100,
  EVENTS = 200,
  TITLE_CHARS = 80,
  ID = /^[A-Za-z0-9_-]{1,128}$/,
  MODEL = "claude-sonnet-5-5";

/** What a step of a script sees and leaves for the next ones. */
type Context = {
  workspaceId: string;
  userId: string;
  turn: StoredTurn;
  // The action this turn's prepare recorded, once it did.
  actionId?: string;
};
/** One event of a script, decided when it is shown: null shows none. */
type Step = (c: Context) => AgentEvent | null;

type StoredThread = {
  id: string;
  workspaceId: string;
  userId: string;
  title: string;
  createdAt: string;
  updatedAt: string;
  deletedAt: string | null;
};
type StoredTurn = {
  id: string;
  threadId: string;
  workspaceId: string;
  userId: string;
  kind: "message" | "action_result";
  text: string;
  actionId: string;
  pageContext: AgentPageContext | null;
  clientToken: string;
  status: FoundryAgentTurnStatus;
  error: string;
  attempts: number;
  steps: number;
  usdMicros: number;
  // When it was queued, in ms, and each event's time.
  at: number;
  stepMs: number;
  script: Step[];
  shown: number;
  events: FoundryAgentEvent[];
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
};
type StoredAction = {
  id: string;
  turnId: string;
  threadId: string;
  workspaceId: string;
  userId: string;
  operation: "create_load";
  target: string;
  ontologyId: string;
  payload: { version_id: string };
  payloadHash: string;
  display: AgentActionDisplay;
  targetNamedByUser: boolean;
  status: AgentActionStatus;
  resultJson: string;
  error: string;
  expiresAt: number;
  decidedByUserId: string;
  decidedAt: string | null;
  createdAt: string;
};

const state = {
  threads: [] as StoredThread[],
  turns: [] as StoredTurn[],
  actions: [] as StoredAction[],
};
onReset(() => {
  state.threads = [];
  state.turns = [];
  state.actions = [];
  delete knobs.agentStepMs;
  delete knobs.agentOutcome;
  delete knobs.agentActionTtlMs;
});

const live = (t: { status: FoundryAgentTurnStatus }) =>
  t.status === "queued" || t.status === "running";
const iso = (ms: number) => new Date(ms).toISOString();
const canonical = (payload: { version_id: string }) =>
  JSON.stringify({ version_id: payload.version_id });
const hash = (payload: { version_id: string }) =>
  createHash("sha256").update(canonical(payload)).digest("hex");

/** The action as read: past its expiry it is expired, executing too long unknown. */
function shown(a: StoredAction, at = Date.now()): AgentActionStatus {
  if (a.status === "prepared" && at >= a.expiresAt) return "expired";
  if (
    a.status === "executing" &&
    a.decidedAt &&
    at - Date.parse(a.decidedAt) > EXECUTING_FOR_MS
  )
    return "unknown";
  return a.status;
}

/** Shows the events whose time has come, and ends the turn at its finished. */
function advance(t: StoredTurn) {
  const at = Date.now();
  if (t.status === "queued" && at >= t.at + t.stepMs) {
    t.status = "running";
    t.attempts = 1;
    t.startedAt = iso(t.at + t.stepMs);
  }
  const c: Context = {
    workspaceId: t.workspaceId,
    userId: t.userId,
    turn: t,
    actionId: state.actions.find((a) => a.turnId === t.id)?.id,
  };
  while (
    t.status === "running" &&
    t.shown < t.script.length &&
    at >= t.at + (t.shown + 2) * t.stepMs
  ) {
    const event = t.script[t.shown]!(c);
    t.shown += 1;
    if (!event) continue;
    t.events.push({
      seq: t.events.length + 1,
      eventJson: JSON.stringify(event),
      createdAt: now(),
    });
    if (event.type === "tool_call") {
      t.steps += 1;
      t.usdMicros += 1_200;
    }
    if (event.type === "finished") {
      t.status = event.status;
      t.error = event.error || "";
      t.finishedAt = now();
    }
  }
}
const advanceThread = (threadId: string) =>
  state.turns.filter((t) => t.threadId === threadId).forEach(advance);

const toThread = (t: StoredThread): FoundryAgentThread => ({
  id: t.id,
  title: t.title,
  runtime: "api",
  createdAt: t.createdAt,
  updatedAt: t.updatedAt,
});
const toTurn = (t: StoredTurn): FoundryAgentTurn => ({
  id: t.id,
  threadId: t.threadId,
  kind: t.kind,
  text: t.text,
  actionId: t.actionId,
  pageContext: t.pageContext,
  status: t.status,
  error: t.error,
  usdMicros: String(t.usdMicros),
  steps: t.steps,
  lastSeq: t.events.length,
  attempts: t.attempts,
  createdAt: t.createdAt,
  startedAt: t.startedAt,
  finishedAt: t.finishedAt,
});
const toAction = (a: StoredAction): FoundryAgentAction => ({
  id: a.id,
  turnId: a.turnId,
  threadId: a.threadId,
  operation: a.operation,
  target: a.target,
  payloadJson: canonical(a.payload),
  payloadHash: a.payloadHash,
  displayJson: JSON.stringify(a.display),
  targetNamedByUser: a.targetNamedByUser,
  status: shown(a),
  resultJson: a.resultJson,
  error: a.error,
  expiresAt: iso(a.expiresAt),
  decidedByUserId: a.decidedByUserId,
  decidedAt: a.decidedAt,
  createdAt: a.createdAt,
});

/**
 * The caller as the Assistant answers them: a member of the workspace (a
 * grantee never gets here: ./access.ts denies every agent route).
 */
function person(req: MockRequest) {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  return isReply(m) ? m : { m, userId: req.principal.user.id };
}
const isPerson = (
  x: ReturnType<typeof person>,
): x is Exclude<ReturnType<typeof person>, MockReply> => "userId" in x;

function threadOf(workspaceId: string, userId: string, threadId: string) {
  return state.threads.find(
    (t) =>
      t.id === threadId &&
      t.workspaceId === workspaceId &&
      t.userId === userId &&
      !t.deletedAt,
  );
}
function turnOf(workspaceId: string, userId: string, turnId: string) {
  const t = state.turns.find((x) => x.id === turnId);
  return t && threadOf(workspaceId, userId, t.threadId) ? t : undefined;
}
function actionOf(workspaceId: string, userId: string, actionId: string) {
  const a = state.actions.find((x) => x.id === actionId);
  return a && threadOf(workspaceId, userId, a.threadId) ? a : undefined;
}

/** Why the thread can't take a message or a click now. */
function busy(t: StoredThread) {
  advanceThread(t.id);
  if (state.turns.some((x) => x.threadId === t.id && live(x)))
    return "the Assistant is still answering in this thread";
  if (
    state.actions.some((a) => a.threadId === t.id && shown(a) === "executing")
  )
    return "an action of this thread is running";
  return undefined;
}

// ---------------------------------------------------------------- the agent

const ontologyOutput = (o: StoredOntology) => ({
  id: o.id,
  workspace_id: o.workspaceId,
  name: o.name,
  database_name: o.databaseName,
  project_id: o.projectId,
  current_version_id: currentVersionOf(o.id) || "",
  current_load_id: currentLoadId(o.id) || "",
});

/** The ontology a message names, by name or id. */
const named = (workspaceId: string, text: string) =>
  workspaceOntologies(workspaceId).find(
    (o) =>
      text.toLowerCase().includes(o.name.toLowerCase()) || text.includes(o.id),
  );

/** Whether the person named the ontology: in a message, or a page they were on. */
const namedByPerson = (threadId: string, o: StoredOntology) =>
  state.turns.some(
    (t) =>
      t.threadId === threadId &&
      t.kind === "message" &&
      (t.text.toLowerCase().includes(o.name.toLowerCase()) ||
        t.text.includes(o.id) ||
        t.pageContext?.ontologyId === o.id),
  );

/** The version a load would write: the current one if it loads, else the newest finished draft. */
function loadable(ontologyId: string) {
  const loads = (v: { definition: Record<string, unknown> }) =>
    v.definition.format === "agent_bundle" && v.definition.status === "done";
  const versions = ontologyVersions(ontologyId),
    current = versions.find((v) => v.id === currentVersionOf(ontologyId));
  return current && loads(current) ? current : versions.find(loads);
}

const versionNumber = (ontologyId: string, versionId: string) =>
  ontologyVersions(ontologyId).find((v) => v.id === versionId)?.version ?? 0;

function loadOutput(ontologyId: string, loadId: string) {
  const l = ontologyLoad(ontologyId, loadId);
  return l
    ? {
        id: l.id,
        version_id: l.versionId,
        version: `foundry-v${versionNumber(ontologyId, l.versionId)}`,
        status: l.status,
        current: currentLoadId(ontologyId) === l.id,
        created_at: l.createdAt,
        ...(l.finishedAt && { finished_at: l.finishedAt }),
        usd_micros: 0,
      }
    : null;
}

const started: Step = (c) => ({
  type: "started",
  model: aiModel(c.workspaceId, "ask") || MODEL,
  attempt: 1,
});
const call =
  (toolUseId: string, name: string, input: unknown): Step =>
  () => ({
    type: "tool_call",
    tool_use_id: toolUseId,
    name,
    title: name,
    input,
  });
const result =
  (toolUseId: string, name: string, output: (c: Context) => unknown): Step =>
  (c) => ({
    type: "tool_result",
    tool_use_id: toolUseId,
    name,
    is_error: false,
    output: { content_trust: "untrusted", ...(output(c) as object) },
  });
const said =
  (text: (c: Context) => string): Step =>
  (c) => ({ type: "text", text: text(c) });
const answered: Step = () => ({
  type: "finished",
  status: "done",
  reason: "answered",
});

/**
 * The script's end, or how `agentOutcome` stops it after its first call and
 * result (every script starts with them, but a failed load's continuation).
 */
function ending(steps: Step[]): Step[] {
  const after = steps.slice(0, 3);
  switch (knobs.agentOutcome) {
    case "fail":
      return [
        ...after,
        () => ({
          type: "finished",
          status: "failed",
          reason: "failed",
          error: "the model failed: overloaded",
        }),
      ];
    case "budget_exhausted":
      return [
        ...after,
        () => ({ type: "finished", status: "done", reason: "budget" }),
      ];
    default:
      return [...steps, answered];
  }
}

/** The worker's words for a provider it doesn't run (agent.Runner.Run). */
export const NOT_CLAUDE =
  "The Assistant runs on Claude for now. An owner can choose Claude in Settings › AI models.";

const LIST = "helix_foundry_list_ontologies",
  GET = "helix_foundry_get_ontology",
  GET_LOAD = "helix_foundry_get_load",
  PREPARE = "helix_foundry_prepare_operation";

/** What the agent does with a person's message. */
function messageScript(
  workspaceId: string,
  threadId: string,
  text: string,
  page: AgentPageContext | null,
): Step[] {
  const list = [
    call("tu_list", LIST, {}),
    result("tu_list", LIST, () => ({
      ontologies: workspaceOntologies(workspaceId).map(ontologyOutput),
    })),
  ];
  const loading =
    /\b(re)?load\b/i.test(text) &&
    !/\b(how many|which|what|when|why)\b/i.test(text);
  // A question about the ontologies (plural) is about all of them, whatever
  // page it was asked on; a load still goes to the page's ontology.
  const ontologies = workspaceOntologies(workspaceId),
    pageOntology = page?.ontologyId ? ontologyById(page.ontologyId) : undefined,
    asked =
      named(workspaceId, text) ??
      (pageOntology?.workspaceId === workspaceId &&
      (loading || !/\bontologies\b/i.test(text))
        ? pageOntology
        : undefined);
  if (loading) {
    const o = asked ?? ontologies.find((x) => loadable(x.id)) ?? ontologies[0];
    if (!o)
      return [
        started,
        ...list,
        said(
          () =>
            "This workspace has no ontology yet, so there is nothing to load. Create one first.",
        ),
      ];
    const v = loadable(o.id);
    if (!v)
      return [
        started,
        ...list,
        said(
          () =>
            `${o.name} has no finished draft to load yet. Generate one first.`,
        ),
      ];
    const target = `ontology:${workspaceId}/${o.id}`,
      payload = { version_id: v.id },
      input = { operation: "create_load", target, payload };
    return [
      started,
      ...list,
      call("tu_get", GET, { ontology_id: o.id }),
      result("tu_get", GET, () => ontologyOutput(o)),
      call("tu_prepare", PREPARE, input),
      ...prepare(o, threadId, target, payload, v.version),
    ];
  }
  if (!asked)
    return [
      started,
      ...list,
      said(() =>
        ontologies.length
          ? `This workspace has ${ontologies.length} ontolog${ontologies.length === 1 ? "y" : "ies"}: ${ontologies.map((o) => o.name).join(", ")}. Ask me about one of them, or to load one.`
          : "This workspace has no ontology yet.",
      ),
    ];
  return [
    started,
    ...list,
    call("tu_get", GET, { ontology_id: asked.id }),
    result("tu_get", GET, () => ontologyOutput(asked)),
    said(() => {
      const current = currentVersionOf(asked.id),
        version = current ? versionNumber(asked.id, current) : 0,
        load = currentLoadId(asked.id);
      return `${asked.name} is on the ${asked.databaseName} database. ${version ? `Its current version is v${version}` : "It has no current version"}, and ${load ? "it has a current load you can explore or ask about" : "nothing is loaded yet"}.`;
    }),
  ];
}

/**
 * The prepare's record, as the sink keeps it, then its result and the
 * agent's words: the card, or why there is none (a member, too many
 * waiting).
 */
function prepare(
  o: StoredOntology,
  threadId: string,
  target: string,
  payload: { version_id: string },
  version: number,
): Step[] {
  const outcome = new Map<string, string>();
  return [
    (c) => {
      const m = membership(c.workspaceId, c.userId);
      const pending = state.actions.filter(
        (a) => a.threadId === threadId && shown(a) === "prepared",
      ).length;
      const again = state.actions.find(
        (a) => a.turnId === c.turn.id && a.payloadHash === hash(payload),
      );
      if (isReply(m) || !manages(m))
        outcome.set(
          c.turn.id,
          "permission denied: loading needs an owner or admin",
        );
      else if (!again && pending >= MAX_PENDING)
        outcome.set(
          c.turn.id,
          `${pending} actions already wait for the person; let them run or dismiss those first`,
        );
      const refusal = outcome.get(c.turn.id);
      if (refusal)
        return {
          type: "tool_result",
          tool_use_id: "tu_prepare",
          name: PREPARE,
          is_error: true,
          error: refusal,
        };
      const a: StoredAction = again ?? {
        id: id("act"),
        turnId: c.turn.id,
        threadId,
        workspaceId: c.workspaceId,
        userId: c.userId,
        operation: "create_load",
        target,
        ontologyId: o.id,
        payload,
        payloadHash: hash(payload),
        display: {
          ontology_id: o.id,
          ontology_name: o.name,
          version_id: payload.version_id,
          version,
        },
        targetNamedByUser: namedByPerson(threadId, o),
        status: "prepared",
        resultJson: "",
        error: "",
        expiresAt: Date.now() + (knobs.agentActionTtlMs ?? ACTION_TTL_MS),
        decidedByUserId: "",
        decidedAt: null,
        createdAt: now(),
      };
      if (!again) state.actions.push(a);
      c.actionId = a.id;
      return {
        type: "action_card",
        action_id: a.id,
        operation: a.operation,
        target: a.target,
        payload: a.payload,
        payload_hash: a.payloadHash,
        display: a.display,
        target_named_by_user: a.targetNamedByUser,
        expires_at: iso(a.expiresAt),
      };
    },
    (c) => {
      const a = state.actions.find((x) => x.id === c.actionId);
      return a
        ? {
            type: "tool_result",
            tool_use_id: "tu_prepare",
            name: PREPARE,
            is_error: false,
            output: {
              action_id: a.id,
              status: "awaiting_person",
              expires_at: iso(a.expiresAt),
              next: "The person sees this action as a card and runs it, or not, with a click; you can't run it.",
            },
          }
        : null;
    },
    said((c) =>
      outcome.get(c.turn.id)?.startsWith("permission")
        ? `Only an owner or admin of this workspace can load ${o.name}, so I can't prepare that for you.`
        : outcome.has(c.turn.id)
          ? "Other actions still wait for you: run or dismiss those first, and I can prepare this one."
          : `I've prepared a load of v${version} of ${o.name}. Run it from the card when you're ready.`,
    ),
  ];
}

/** How the agent goes on from an action its person ran. */
function continuationScript(a: StoredAction): Step[] {
  const loadId = (
    JSON.parse(a.resultJson || "{}") as { result?: { id?: string } }
  ).result?.id;
  const name = a.display.ontology_name || "the ontology";
  if (!loadId)
    return [
      started,
      said(
        () =>
          `The load didn't start: ${a.error || "Foundry gave no reason"}. Ask me to prepare it again once that's fixed.`,
      ),
    ];
  return [
    started,
    call("tu_load", GET_LOAD, { ontology_id: a.ontologyId, load_id: loadId }),
    result("tu_load", GET_LOAD, () => ({
      load: loadOutput(a.ontologyId, loadId),
    })),
    said(() => {
      const l = loadOutput(a.ontologyId, loadId);
      return `The load of v${a.display.version} into ${name} is ${l?.status === "ready" ? "ready" : l?.status === "failed" ? "failed" : "writing"}. Follow it on its page: /ontology/${a.ontologyId}/loads/${loadId}`;
    }),
  ];
}

function queueTurn(
  thread: StoredThread,
  turn: Omit<
    StoredTurn,
    | "id"
    | "threadId"
    | "workspaceId"
    | "userId"
    | "status"
    | "error"
    | "attempts"
    | "steps"
    | "usdMicros"
    | "at"
    | "stepMs"
    | "shown"
    | "events"
    | "createdAt"
    | "startedAt"
    | "finishedAt"
  >,
  first?: AgentEvent,
) {
  const at = Date.now();
  const t: StoredTurn = {
    ...turn,
    id: id("turn"),
    threadId: thread.id,
    workspaceId: thread.workspaceId,
    userId: thread.userId,
    status: "queued",
    error: "",
    attempts: 0,
    steps: 0,
    usdMicros: 0,
    at,
    stepMs: knobs.agentStepMs ?? STEP_MS,
    shown: 0,
    events: first
      ? [{ seq: 1, eventJson: JSON.stringify(first), createdAt: iso(at) }]
      : [],
    createdAt: iso(at),
    startedAt: null,
    finishedAt: null,
  };
  thread.updatedAt = t.createdAt;
  state.turns.push(t);
  return t;
}

// ---------------------------------------------------------------- routes

const BASE = "/v1/workspaces/{workspaceId}/foundry/agent" as const;

/** Runs the caller's action: the backend's ExecuteFoundryAgentAction. */
function execute(req: MockRequest, userId: string) {
  const ws = req.params.workspaceId!;
  const payloadHash = String(req.body?.payloadHash || "");
  if (!/^[0-9a-fA-F]{64}$/.test(payloadHash))
    return fail(
      "INVALID_ARGUMENT",
      "payload_hash is the action's 64 hex digits",
    );
  const a = actionOf(ws, userId, req.params.id!);
  if (!a) return fail("NOT_FOUND", "action not found");
  const thread = threadOf(ws, userId, a.threadId)!;
  const status = shown(a);
  if (a.status !== "prepared")
    return refused(
      ws,
      "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
      `the action is ${status}`,
    );
  if (a.payloadHash !== payloadHash)
    return refused(
      ws,
      "FOUNDRY_AGENT_ACTION_CHANGED",
      "the action shown is not the one prepared; reload it",
    );
  if (status === "expired") {
    a.status = "expired";
    a.decidedAt = now();
    return refused(
      ws,
      "FOUNDRY_AGENT_ACTION_EXPIRED",
      "the action's confirmation expired; refresh it",
    );
  }
  const why = busy(thread);
  if (why) return refused(ws, "FOUNDRY_AGENT_THREAD_BUSY", why);
  Object.assign(a, {
    status: "executing",
    decidedByUserId: userId,
    decidedAt: now(),
  });
  // Authorized again as the person who clicked, as they are now; the
  // confirmation's id is the load's client token.
  const o = ontologyById(a.ontologyId),
    m = membership(ws, userId);
  const queued: ReturnType<typeof queueLoad> =
    !o || isReply(m) || !manages(m)
      ? fail("PERMISSION_DENIED", "permission denied")
      : queueLoad(o, a.payload.version_id, a.id);
  if ("clientToken" in queued) {
    a.status = "succeeded";
    a.resultJson = JSON.stringify({
      content_trust: "untrusted",
      operation: a.operation,
      target: a.target,
      result: loadOutput(a.ontologyId, queued.id),
    });
  } else {
    a.status = "failed";
    a.error = (queued.body as { message?: string }).message || "it failed";
  }
  const continuation = queueTurn(
    thread,
    {
      kind: "action_result",
      text: "",
      actionId: a.id,
      pageContext: null,
      clientToken: "",
      script: continuationScript(a),
    },
    {
      type: "action_result",
      action_id: a.id,
      operation: a.operation,
      status: a.status,
      ...(a.resultJson && { result: JSON.parse(a.resultJson) as unknown }),
      ...(a.error && { error: a.error }),
    },
  );
  continuation.script = ending(continuation.script);
  return ok({
    action: toAction(a),
    continuation: toTurn(continuation),
  } satisfies AgentActionResponse);
}

export const agentRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${BASE}/turns`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const ws = req.params.workspaceId!;
      const body = (req.body || {}) as Partial<SendAgentMessageRequest>;
      const text = (body.text || "").trim(),
        token = body.clientToken || "",
        page = body.pageContext;
      if (!token || token.length > MAX_TOKEN)
        return fail(
          "INVALID_ARGUMENT",
          `client_token is required, at most ${MAX_TOKEN} characters`,
        );
      if (!text || [...text].length > MAX_TEXT)
        return fail(
          "INVALID_ARGUMENT",
          `text is required, at most ${MAX_TEXT} characters`,
        );
      if (body.threadId && !ID.test(body.threadId))
        return fail("INVALID_ARGUMENT", "thread_id is malformed");
      if (
        (page?.ontologyId && !ID.test(page.ontologyId)) ||
        (page?.loadId && (!ID.test(page.loadId) || !page.ontologyId))
      )
        return fail(
          "INVALID_ARGUMENT",
          "page_context names a load of an ontology, by their ids",
        );
      const ai = aiRefusal(ws);
      if (ai) return ai;
      const again = state.turns.find(
        (t) =>
          t.workspaceId === ws &&
          t.userId === p.userId &&
          t.clientToken === token,
      );
      if (again) {
        if (again.text !== text)
          return fail(
            "INVALID_ARGUMENT",
            "client_token was already used for another message",
          );
        advance(again);
        return ok({
          turn: toTurn(again),
          thread: toThread(threadOf(ws, p.userId, again.threadId)!),
        } satisfies AgentTurnResponse);
      }
      const hourAgo = Date.now() - 3_600_000,
        sent = state.turns.filter(
          (t) =>
            t.workspaceId === ws &&
            t.userId === p.userId &&
            t.kind === "message" &&
            t.at >= hourAgo,
        ).length;
      if (sent >= PER_HOUR)
        return fail(
          "RESOURCE_EXHAUSTED",
          `you have sent ${sent} messages in the last hour; try again later`,
        );
      let thread = body.threadId
        ? threadOf(ws, p.userId, body.threadId)
        : undefined;
      if (body.threadId && !thread)
        return fail("NOT_FOUND", "thread not found");
      if (thread) {
        const why = busy(thread);
        if (why) return refused(ws, "FOUNDRY_AGENT_THREAD_BUSY", why);
      } else {
        const line = text.split("\n")[0]!.trim();
        thread = {
          id: id("thr"),
          workspaceId: ws,
          userId: p.userId,
          title:
            [...line].length <= TITLE_CHARS
              ? line
              : [...line]
                  .slice(0, TITLE_CHARS - 1)
                  .join("")
                  .trim() + "…",
          createdAt: now(),
          updatedAt: now(),
          deletedAt: null,
        };
        state.threads.push(thread);
      }
      const pageContext = page?.ontologyId
        ? {
            ontologyId: page.ontologyId,
            ...(page.loadId && { loadId: page.loadId }),
          }
        : null;
      const turn = queueTurn(thread, {
        kind: "message",
        text,
        actionId: "",
        pageContext,
        clientToken: token,
        script: [],
      });
      // The worker runs Claude alone for now (agent/llm): any other
      // provider fails the turn for good, before it starts, in its words.
      turn.script =
        aiProvider(ws) === "anthropic"
          ? ending(messageScript(ws, thread.id, text, pageContext))
          : [
              () => ({
                type: "finished",
                status: "failed",
                reason: "failed",
                error: NOT_CLAUDE,
              }),
            ];
      return ok({
        turn: toTurn(turn),
        thread: toThread(thread),
      } satisfies AgentTurnResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/threads`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const mine = state.threads
        .filter(
          (t) =>
            t.workspaceId === req.params.workspaceId &&
            t.userId === p.userId &&
            !t.deletedAt,
        )
        .sort(
          (a, b) =>
            b.updatedAt.localeCompare(a.updatedAt) || b.id.localeCompare(a.id),
        );
      // The tokens it returns: a later page's offset.
      const token = req.query.get("pageToken") || "",
        from = Number(token);
      if (token && (!/^[1-9]\d*$/.test(token) || from % PAGE !== 0))
        return fail(
          "INVALID_ARGUMENT",
          "page_token is not one this list returned",
        );
      const page = mine.slice(from, from + PAGE);
      return ok({
        threads: page.map(toThread),
        ...(from + PAGE < mine.length && {
          nextPageToken: String(from + PAGE),
        }),
      } satisfies ListAgentThreadsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/threads/{id}`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const t = threadOf(req.params.workspaceId!, p.userId, req.params.id!);
      if (!t) return fail("NOT_FOUND", "thread not found");
      advanceThread(t.id);
      return ok({
        thread: toThread(t),
        turns: state.turns
          .filter((x) => x.threadId === t.id)
          .slice(-TURNS)
          .map(toTurn),
        actions: state.actions.filter((a) => a.threadId === t.id).map(toAction),
      } satisfies AgentThreadResponse);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/threads/{id}`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const t = threadOf(req.params.workspaceId!, p.userId, req.params.id!);
      if (!t) return fail("NOT_FOUND", "thread not found");
      advanceThread(t.id);
      for (const x of state.turns)
        if (x.threadId === t.id && live(x))
          Object.assign(x, { status: "cancelled", finishedAt: now() });
      t.deletedAt = now();
      return ok({});
    },
  },
  {
    method: "GET",
    path: `${BASE}/turns/{id}/events`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const t = turnOf(req.params.workspaceId!, p.userId, req.params.id!);
      if (!t) return fail("NOT_FOUND", "turn not found");
      advance(t);
      const after = Number(req.query.get("afterSeq") || 0) || 0;
      return ok({
        events: t.events.filter((e) => e.seq > after).slice(0, EVENTS),
        status: t.status,
        lastSeq: t.events.length,
      } satisfies ListAgentTurnEventsResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/turns/{id}:cancel`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const ws = req.params.workspaceId!;
      const t = turnOf(ws, p.userId, req.params.id!);
      if (!t) return fail("NOT_FOUND", "turn not found");
      advance(t);
      if (t.status !== "cancelled") {
        if (!live(t))
          return fail("FAILED_PRECONDITION", "the turn already ended");
        Object.assign(t, { status: "cancelled", finishedAt: now() });
      }
      return ok({
        turn: toTurn(t),
        thread: toThread(threadOf(ws, p.userId, t.threadId)!),
      } satisfies AgentTurnResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/actions/{id}:execute`,
    handle: (req) => {
      const p = person(req);
      return isPerson(p) ? execute(req, p.userId) : p;
    },
  },
  {
    method: "POST",
    path: `${BASE}/actions/{id}:refresh`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const ws = req.params.workspaceId!;
      const a = actionOf(ws, p.userId, req.params.id!);
      if (!a) return fail("NOT_FOUND", "action not found");
      // Prepared again as the person, as they are now, before the action's
      // state is read: what they may no longer do is refused in the tool's
      // words.
      const m = membership(ws, p.userId);
      if (isReply(m) || !manages(m))
        return fail("FAILED_PRECONDITION", "permission denied");
      const status = shown(a);
      if (status !== "prepared" && status !== "expired")
        return refused(
          ws,
          "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
          `the action is ${status}`,
        );
      Object.assign(a, {
        status: "prepared",
        decidedByUserId: "",
        decidedAt: null,
        expiresAt: Date.now() + (knobs.agentActionTtlMs ?? ACTION_TTL_MS),
      });
      return ok({ action: toAction(a) } satisfies AgentActionResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/actions/{id}:dismiss`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const ws = req.params.workspaceId!;
      const a = actionOf(ws, p.userId, req.params.id!);
      if (!a) return fail("NOT_FOUND", "action not found");
      const status = shown(a);
      if (status !== "dismissed") {
        if (status !== "prepared" && status !== "expired")
          return refused(
            ws,
            "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
            `the action is ${status}`,
          );
        Object.assign(a, {
          status: "dismissed",
          decidedByUserId: p.userId,
          decidedAt: now(),
        });
      }
      return ok({ action: toAction(a) } satisfies AgentActionResponse);
    },
  },
];
