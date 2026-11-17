import type {
  AutomationAction,
  AutomationNodeOutput,
  AutomationNodeStatus,
  AutomationPausedReason,
  AutomationResponse,
  AutomationRunCounts,
  AutomationRunResponse,
  AutomationRunStatus,
  AutomationRunSummary,
  AutomationSpec,
  AutomationStatus,
  AutomationTrigger,
  AutomationTriggerEvent,
  FoundryAutomation,
  FoundryAutomationRun,
  ListAutomationRunsResponse,
  ListAutomationsResponse,
} from "../../src/shared/wfe";
import {
  canonicalJson,
  edgesInto,
  nextFire,
  parseSpec,
  sameSteps,
  triggerOf,
  validZone,
} from "../../src/web/areas/automations/model";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import {
  LOCAL_TOKEN,
  config,
  db,
  isReply,
  membership,
  onReset,
  type Membership,
} from "../state";
import { foundryPrecondition as refused } from "./ai";
import {
  automationTurn,
  cancelAutomationTurns,
  queueAutomationTurn,
  waitingActions,
} from "./agent";
import { dashboardRoute, storedDashboard } from "./dashboards";
import { dataRoutes, syncsEnded, storedConnector } from "./data";
import { documentSets, documentsArrived } from "./documents";
import { loadsEnded, queueLoad } from "./loads";
import { ontologyById } from "./ontologies";
import { currentVersionOf, draftsEnded } from "./ontology";
import {
  automationDeleted,
  automationOntologies,
  automationRunOntologies,
  automationWorkflow,
  finishAutomationRuns,
  record,
  renameAutomationWorkflows,
  useOntology,
  WORKFLOW_CAPS,
  type AutomationRef,
} from "./workflows";
import { manages } from "./workspaces";

// Automations, as the backend's API, store and foundry-automation-worker
// run them (backend/api/automations.go, store/foundry_automations.go,
// workers/foundry-automation-worker), in their shapes (src/shared/wfe.ts
// "Automations"): a person's graph of steps, checked by the site's own port
// of the backend's ParseSpec (src/web/areas/automations/model.ts), which
// runs only once its owner approved it, and again after every edit of its
// steps (moving them on the canvas isn't one). Every member lists and reads
// every one and makes their own; its owner edits, approves, resumes and
// runs it; its owner or an owner or admin pauses, deletes it and cancels a
// run, and only they read what its steps found. The worker runs whenever
// the automations' routes are called (the mock moves on when looked at):
// - a schedule fires once when its time came, however long ago, its next
//   time the first after now; a run still going records a skipped one;
// - an event trigger finds, past its cursor, the syncs, loads, drafts and
//   documents its target ended or received (./data.ts, ./loads.ts,
//   ./ontology.ts, ./documents.ts), at once, not 2 minutes later as the
//   backend counts them, and fires one run for what matched, which waits
//   while a run is live;
// - a run walks one step every `automationStepMs` (500): an action runs the
//   mock's real sync, load or publish as the owner; an Assistant step
//   queues an Assistant turn of kind automation in the run's thread, run by
//   the mock's scripted agent (./agent.ts queueAutomationTurn), and waits
//   for it; a condition checks the step before it; a notify step "emails"
//   (a local build's records that it sent none). A failure stops the steps
//   after it.
// A failed run holds the next back 5 minutes, doubling; 3 in a row pause
// an active automation. A workspace keeps 50 automations and starts 200
// runs a day (`automationRunsPerDay`). `automationClockMs` moves the
// worker's clock this far ahead, so a test fires a schedule without
// waiting; `automationsDisabled` refuses every route as a stamp with
// automations off does. A run's uses of ontologies are recorded as the
// backend records them (./workflows.ts "automations' workflows"): an
// Assistant step's reads of one, a load of one or a publish of a dashboard
// reading one start the run of the automation's workflow of it, finished
// with the run's answer when it ends; approving steps that name one makes
// its workflow.

type Knobs = {
  automationClockMs?: number;
  automationStepMs?: number;
  automationRunsPerDay?: number;
  automationsDisabled?: boolean;
};
const knobs = config as typeof config & Knobs;

const STEP_MS = 500,
  MAX_PER_WORKSPACE = 50,
  RUNS_PER_DAY = 200,
  PAUSE_AFTER = 3,
  FAILURE_BACKOFF_MS = 5 * 60_000,
  DAY_MS = 24 * 3_600_000,
  PAGE = 50,
  MAX_TOKEN = 64;

type StoredAutomation = {
  id: string;
  workspaceId: string;
  ownerUserId: string;
  name: string;
  description: string;
  spec: AutomationSpec;
  revision: number;
  specRevision: number;
  status: AutomationStatus;
  pausedReason: AutomationPausedReason | null;
  pausedByUserId: string;
  pausedAt: string | null;
  approvedSpecRevision: number;
  approvedAt: string | null;
  // ms; a schedule's next time while active.
  nextFireAt: number | null;
  failures: number;
  backoffUntil: number | null;
  // ms; an event trigger finds what ended after it.
  cursor: number | null;
  clientToken: string;
  createdAt: string;
  updatedAt: string;
};
type StoredNode = {
  nodeId: string;
  status: AutomationNodeStatus;
  output: AutomationNodeOutput | null;
  error: string;
  turnId: string;
  // The sync or load an action step started and waits for.
  jobId?: string;
  usdMicros: number;
  startedAt: string;
  finishedAt: string | null;
};
type StoredRun = {
  id: string;
  automationId: string;
  workspaceId: string;
  ownerUserId: string;
  status: AutomationRunStatus;
  error: string;
  event: AutomationTriggerEvent;
  triggerKey: string;
  spec: AutomationSpec;
  specRevision: number;
  threadId: string;
  usdMicros: number;
  attempts: number;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
  nodes: StoredNode[];
  // ms: when its next step may go.
  next: number;
};

/**
 * The automation run that started a sync or a load: the run, its automation,
 * the automations that set it off, and how many runs led to the work (the
 * backend's started_by_automation_run_id and automation_chain_depth).
 */
type Cause = {
  runId: string;
  automationId: string;
  chain: string[];
  depth: number;
};

/** How many automation runs in a row may set each other off (maxChainDepth). */
const MAX_CHAIN_DEPTH = 3;

const state = {
  automations: [] as StoredAutomation[],
  // Newest first.
  runs: [] as StoredRun[],
  // By the sync's or load's id.
  causes: new Map<string, Cause>(),
};
onReset(() => {
  state.automations = [];
  state.runs = [];
  state.causes = new Map();
  delete knobs.automationClockMs;
  delete knobs.automationStepMs;
  delete knobs.automationRunsPerDay;
  delete knobs.automationsDisabled;
});

const iso = (ms: number) => new Date(ms).toISOString();
const clock = () => Date.now() + (knobs.automationClockMs ?? 0);
const live = (r: { status: AutomationRunStatus }) =>
  r.status === "queued" || r.status === "running";
const sentence = (text: string) => {
  const t = text.trim();
  if (!t) return "";
  const s = t.charAt(0).toUpperCase() + t.slice(1);
  return /[.!?]$/.test(s) ? s : s + ".";
};

// ---------------------------------------------------------------- the API's checks

type Caller = { m: Membership; userId: string; manager: boolean };

/** The caller as automations answer them: a member, while automations are on. */
export function caller(req: MockRequest): Caller | MockReply {
  const ws = req.params.workspaceId!;
  if (knobs.automationsDisabled)
    return refused(
      ws,
      "FOUNDRY_AUTOMATIONS_DISABLED",
      "workflows aren't available here",
    );
  const m = membership(ws, req.principal.user.id);
  if (isReply(m)) return m;
  work(ws);
  return { m, userId: m.userId, manager: manages(m) };
}
export const isCaller = (x: Caller | MockReply): x is Caller => "m" in x;

/**
 * A spec of the workspace as the API checks it: its shape (the backend's
 * ParseSpec, ported), and every connector, ontology, set and dashboard it
 * names the workspace's. Answers the spec canonical, or the refusal.
 */
function checkedSpec(ws: string, raw: unknown): AutomationSpec | MockReply {
  const read = parseSpec(typeof raw === "string" ? raw : "");
  if (!read.ok) return fail("INVALID_ARGUMENT", "spec_json: " + read.error);
  const spec = read.spec;
  const named: [string, string, boolean][] = [];
  const target = (a: AutomationAction) =>
    a.operation === "sync_connector"
      ? named.push([
          "connector",
          a.connector_id,
          storedConnector(a.connector_id)?.workspaceId === ws,
        ])
      : a.operation === "create_load"
        ? named.push([
            "ontology",
            a.ontology_id,
            ontologyById(a.ontology_id)?.workspaceId === ws,
          ])
        : named.push([
            "dashboard",
            a.dashboard_id,
            !!storedDashboard(ws, a.dashboard_id),
          ]);
  for (const n of spec.nodes) {
    if (n.kind === "action") target(n.config);
    if (n.kind === "assistant") (n.config.allow || []).forEach(target);
    if (n.kind !== "trigger") continue;
    const t = n.config;
    if (t.kind === "sync_finished")
      named.push([
        "connector",
        t.connector_id,
        storedConnector(t.connector_id)?.workspaceId === ws,
      ]);
    if (t.kind === "load_ready" || t.kind === "generation_finished")
      named.push([
        "ontology",
        t.ontology_id,
        ontologyById(t.ontology_id)?.workspaceId === ws,
      ]);
    if (t.kind === "document_arrived")
      named.push([
        "document set",
        t.set_id,
        documentSets(ws).some((s) => s.id === t.set_id),
      ]);
  }
  const missing = named.find(([, , ours]) => !ours);
  return missing
    ? fail(
        "INVALID_ARGUMENT",
        `spec_json names the ${missing[0]} ${missing[1]}, which this workspace hasn't`,
      )
    : spec;
}

/** Name and description as the API takes them: 1 to 120 and at most 1,000 characters, trimmed. */
function nameAndDescription(
  name: unknown,
  description: unknown,
): { name: string; description: string } | MockReply {
  const n = typeof name === "string" ? name.trim() : "",
    d = typeof description === "string" ? description.trim() : "";
  if (!n || [...n].length > 120)
    return fail("INVALID_ARGUMENT", "name is 1 to 120 characters");
  if ([...d].length > 1000)
    return fail("INVALID_ARGUMENT", "description is at most 1000 characters");
  return { name: n, description: d };
}

const triggerSpec = (a: { spec: AutomationSpec }) => triggerOf(a.spec).config;
const firstFire = (spec: AutomationSpec, at: number) => {
  const t = triggerOf(spec).config;
  return t.kind === "schedule" ? nextFire(t, at) : null;
};
const lastRun = (a: StoredAutomation) =>
  state.runs.find((r) => r.automationId === a.id);
// Runs are kept newest first; a past run (pastRuns) goes where its time is.
const lastFailed = (a: StoredAutomation) =>
  state.runs.find((r) => r.automationId === a.id && r.status === "failed");

function automationOut(
  a: StoredAutomation,
  c: Caller,
  withSpec: boolean,
): FoundryAutomation {
  const owner = a.ownerUserId === c.userId,
    t = triggerSpec(a),
    last = lastRun(a);
  const target =
    t.kind === "sync_finished"
      ? t.connector_id
      : t.kind === "load_ready" || t.kind === "generation_finished"
        ? t.ontology_id
        : t.kind === "document_arrived"
          ? t.set_id
          : "";
  return {
    id: a.id,
    name: a.name,
    description: a.description,
    ...(withSpec && { specJson: canonicalJson(a.spec) }),
    revision: String(a.revision),
    specRevision: String(a.specRevision),
    ownerUserId: a.ownerUserId,
    status: a.status,
    ...(a.pausedReason && { pausedReason: a.pausedReason }),
    ...(a.pausedByUserId && { pausedByUserId: a.pausedByUserId }),
    pausedAt: a.pausedAt,
    ...(a.approvedSpecRevision && {
      approvedSpecRevision: String(a.approvedSpecRevision),
    }),
    approvedAt: a.approvedAt,
    triggerKind: t.kind,
    ...(target && { triggerTarget: target }),
    nextRunAt: a.nextFireAt === null ? null : iso(a.nextFireAt),
    failures: a.failures,
    backoffUntil: a.backoffUntil === null ? null : iso(a.backoffUntil),
    lastRun: last ? runOut(last, a, c, false) : null,
    createdAt: a.createdAt,
    updatedAt: a.updatedAt,
    canEdit: owner,
    canApprove: owner && a.status === "needs_approval",
    canPause: (owner || c.manager) && a.status === "active",
    canResume: owner && a.status === "paused",
    canRun: owner && a.status !== "needs_approval" && !(last && live(last)),
    canDelete: owner || c.manager,
    ontologies: automationOntologies(a.id, withSpec),
  };
}

/** A run as its caller sees it: what its steps found, and why it failed, for its owner and managers alone. */
function runOut(
  r: StoredRun,
  a: StoredAutomation,
  c: Caller,
  withSteps: boolean,
): FoundryAutomationRun {
  const detail = a.ownerUserId === c.userId || c.manager,
    owner = a.ownerUserId === c.userId;
  return {
    id: r.id,
    automationId: r.automationId,
    status: r.status,
    ...(detail && r.error && { error: r.error }),
    triggerEventJson: JSON.stringify(r.event),
    specRevision: String(r.specRevision),
    // A skipped run runs nothing, so it keeps no spec.
    ...(withSteps &&
      r.status !== "skipped" && { specJson: canonicalJson(r.spec) }),
    ...(owner && r.threadId && { threadId: r.threadId }),
    usdMicros: String(r.usdMicros),
    attempts: r.attempts,
    createdAt: r.createdAt,
    startedAt: r.startedAt,
    finishedAt: r.finishedAt,
    ...(withSteps && {
      nodes: r.nodes.map((n) => ({
        nodeId: n.nodeId,
        status: n.status,
        ...(detail && n.output && { outputJson: JSON.stringify(n.output) }),
        ...(detail && n.error && { error: n.error }),
        ...(owner && n.turnId && { turnId: n.turnId }),
        usdMicros: String(n.usdMicros),
        startedAt: n.startedAt,
        finishedAt: n.finishedAt,
      })),
    }),
    canCancel: live(r) && detail,
    ...(withSteps && { ontologies: automationRunOntologies(r.id) }),
  };
}

function automationOf(ws: string, automationId: string) {
  return state.automations.find(
    (a) => a.id === automationId && a.workspaceId === ws,
  );
}

// ---------------------------------------------------------------- the worker

/** Why the automation can't start a run now: one is live, or the day's runs are spent. */
function room(
  a: StoredAutomation,
): { reason: "busy" | "day"; why: string } | undefined {
  if (state.runs.some((r) => r.automationId === a.id && live(r)))
    return { reason: "busy", why: "a run of the workflow is still going" };
  const today = state.runs.filter(
    (r) =>
      r.workspaceId === a.workspaceId &&
      r.status !== "skipped" &&
      Date.parse(r.createdAt) >= Date.now() - DAY_MS,
  ).length;
  const cap = knobs.automationRunsPerDay ?? RUNS_PER_DAY;
  return today >= cap
    ? {
        reason: "day",
        why: `the workspace's workflows ran ${today} times in the last 24 hours, as many as they may`,
      }
    : undefined;
}

/** Stores a run of the automation's spec as it is, its trigger's step done with `output`. */
function insertRun(
  a: StoredAutomation,
  key: string,
  event: AutomationTriggerEvent,
  output: AutomationNodeOutput,
  skipped?: string,
): StoredRun {
  const at = now();
  const r: StoredRun = {
    id: id("arun"),
    automationId: a.id,
    workspaceId: a.workspaceId,
    ownerUserId: a.ownerUserId,
    status: skipped ? "skipped" : "queued",
    error: skipped || "",
    event,
    triggerKey: key,
    spec: structuredClone(a.spec),
    specRevision: a.specRevision,
    threadId: "",
    usdMicros: 0,
    attempts: 0,
    createdAt: at,
    startedAt: null,
    finishedAt: skipped ? at : null,
    nodes: skipped
      ? []
      : [
          {
            nodeId: triggerOf(a.spec).id,
            status: "succeeded",
            output,
            error: "",
            turnId: "",
            usdMicros: 0,
            startedAt: at,
            finishedAt: at,
          },
        ],
    next: Date.now() + (knobs.automationStepMs ?? STEP_MS),
  };
  state.runs.unshift(r);
  return r;
}

/**
 * A fire of the automation (store.FireFoundryAutomation): a key fired
 * before runs nothing; a schedule while a run is live, or past the day's
 * runs, records a skipped run; an event waits while one is live (false: the
 * cursor stays), and past the day's runs is skipped.
 */
function fire(
  a: StoredAutomation,
  key: string,
  event: AutomationTriggerEvent,
  output: AutomationNodeOutput,
  scheduled: boolean,
  loop = "",
): boolean {
  if (state.runs.some((r) => r.automationId === a.id && r.triggerKey === key))
    return true;
  // Work that would loop is recorded skipped at once.
  if (loop) {
    insertRun(a, key, event, output, loop);
    return true;
  }
  const busy = room(a);
  if (busy?.reason === "busy" && !scheduled) return false;
  insertRun(
    a,
    key,
    event,
    output,
    busy &&
      (busy.reason === "busy"
        ? "Skipped: the run before it was still going."
        : "Skipped: the workspace's workflows ran as many times as they may in 24 hours."),
  );
  return true;
}

/** The work an event trigger listens to that ended, oldest first, with how it ended. */
function ended(t: AutomationTrigger) {
  switch (t.kind) {
    case "sync_finished":
      return syncsEnded(t.connector_id);
    case "load_ready":
      return loadsEnded(t.ontology_id).map((e) => ({ ...e, rows: 0 }));
    case "generation_finished":
      return draftsEnded(t.ontology_id).map((e) => ({ ...e, rows: 0 }));
    case "document_arrived":
      return documentsArrived(t.set_id).map((d) => ({
        ...d,
        outcome: "arrived",
        rows: 0,
      }));
    default:
      return [];
  }
}

/** Fires what came due and walks the workspace's live runs, as the worker's ticks would have by now. */
function work(ws: string) {
  const at = clock();
  for (const a of state.automations) {
    if (a.workspaceId !== ws || a.status !== "active") continue;
    if (a.backoffUntil !== null && a.backoffUntil > at) continue;
    const t = triggerSpec(a);
    if (t.kind === "schedule") {
      if (a.nextFireAt === null || a.nextFireAt > at) continue;
      const due = a.nextFireAt;
      fire(
        a,
        "schedule:" + iso(due),
        { kind: "schedule", at: iso(due) },
        {
          summary: `Its schedule fired at ${iso(due).slice(0, 16).replace("T", " ")} (${t.time_zone}).`,
        },
        true,
      );
      a.nextFireAt = nextFire(t, Math.max(at, due));
      continue;
    }
    if (t.kind === "manual" || a.cursor === null) continue;
    const found = ended(t).filter((e) => e.at > a.cursor!);
    if (!found.length) continue;
    const matched = found.filter((e) =>
      t.kind === "load_ready"
        ? e.outcome === "ready" || e.outcome === "partial"
        : "outcome" in t && t.outcome !== "any"
          ? e.outcome === t.outcome
          : true,
    );
    const newest = found[found.length - 1]!;
    if (!matched.length) {
      a.cursor = newest.at;
      continue;
    }
    // Work its own runs started, directly or through automations they set
    // off, or past MAX_CHAIN_DEPTH runs, is skipped (the backend's loops).
    const what = t.kind === "load_ready" ? "load" : "sync";
    const loopOf = (id: string) => {
      const c = state.causes.get(id);
      if (!c) return "";
      if (c.automationId === a.id)
        return `Skipped: the ${what} was started by this workflow itself; running on it would loop.`;
      if (c.chain.includes(a.id))
        return `Skipped: the ${what} was started by a workflow this one set off; running on it would loop.`;
      if (c.depth > MAX_CHAIN_DEPTH)
        return `Skipped: the ${what} came from a chain of ${c.depth} workflows setting each other off; a chain stops after ${MAX_CHAIN_DEPTH}.`;
      return "";
    };
    const fired = matched.filter((e) => !loopOf(e.id));
    const loop = fired.length ? "" : loopOf(matched[matched.length - 1]!.id);
    const counted = fired.length ? fired : matched;
    const last = counted[counted.length - 1]!,
      count = counted.length;
    const causes = counted.flatMap((e) => {
      const c = state.causes.get(e.id);
      return c ? [c] : [];
    });
    const chain = [
      ...new Set(causes.flatMap((c) => [...c.chain, c.automationId])),
    ].sort();
    const depth = Math.max(0, ...causes.map((c) => c.depth));
    const event: AutomationTriggerEvent = {
      kind: t.kind,
      at: iso(last.at),
      outcome: last.outcome,
      count,
      ...(chain.length && { caused_by_automations: chain }),
      ...(depth && { chain_depth: depth }),
      ...(t.kind === "sync_finished" && { sync_id: last.id, rows: last.rows }),
      ...(t.kind === "load_ready" && { load_id: last.id }),
      ...(t.kind === "generation_finished" && { generation_id: last.id }),
      ...(t.kind === "document_arrived" && {
        document_ids: counted.map((e) => e.id),
      }),
    };
    const output: AutomationNodeOutput =
      t.kind === "sync_finished"
        ? {
            summary: `A sync of its connector ${last.outcome}.`,
            count: last.rows,
          }
        : t.kind === "load_ready"
          ? { summary: `A load of its ontology is ${last.outcome}.` }
          : t.kind === "generation_finished"
            ? { summary: `A draft of its ontology ${last.outcome}.` }
            : {
                summary:
                  count === 1
                    ? "A document arrived in its set."
                    : `${count} documents arrived in its set.`,
                count,
              };
    if (fire(a, `${t.kind}:${last.id}`, event, output, false, loop))
      a.cursor = newest.at;
  }
  state.runs
    .filter((r) => r.workspaceId === ws && live(r))
    .reverse()
    .forEach(advance);
}

/** A node's state, for Spec.Next. */
const stateOf = (r: StoredRun, nodeId: string) =>
  r.nodes.find((n) => n.nodeId === nodeId);

/** Spec.Next: what the run does next, one step at a time in the spec's order. */
function nextStep(
  r: StoredRun,
  order: string[],
):
  | { kind: "run" | "wait"; id: string }
  | { kind: "skip"; id: string; afterFailure: boolean }
  | { kind: "done"; failed: boolean } {
  let failed = false;
  for (const nodeId of order) {
    const s = stateOf(r, nodeId)?.status;
    if (s === "running") return { kind: "wait", id: nodeId };
    if (s === "failed") {
      failed = true;
      continue;
    }
    if (s) continue;
    const into = edgesInto(r.spec, nodeId);
    if (
      into.some((e) => {
        const from = stateOf(r, e.from)?.status;
        return from === "failed" || from === "blocked";
      })
    )
      return { kind: "skip", id: nodeId, afterFailure: true };
    const taken = into.some((e) => {
      const from = stateOf(r, e.from);
      if (from?.status !== "succeeded") return false;
      return !e.branch || from.output?.branch === (e.branch === "true");
    });
    return taken || !into.length
      ? { kind: "run", id: nodeId }
      : { kind: "skip", id: nodeId, afterFailure: false };
  }
  return { kind: "done", failed };
}

/** Ends a step, adding its spend to the run's. */
function endNode(
  r: StoredRun,
  nodeId: string,
  status: AutomationNodeStatus,
  output: AutomationNodeOutput | null,
  error = "",
  usdMicros = 0,
) {
  const n = stateOf(r, nodeId) ?? {
    nodeId,
    status,
    output: null,
    error: "",
    turnId: "",
    usdMicros: 0,
    startedAt: now(),
    finishedAt: null,
  };
  Object.assign(n, { status, output, error, usdMicros, finishedAt: now() });
  if (!r.nodes.includes(n)) r.nodes.push(n);
  r.usdMicros += usdMicros;
}

/** The owner, as a request of theirs, for the routes an action step calls. */
const asOwner = (
  r: StoredRun,
  params: Record<string, string>,
  body?: unknown,
): MockRequest => ({
  method: "POST",
  path: "",
  params: { workspaceId: r.workspaceId, ...params },
  query: new URLSearchParams(),
  body,
  principal: { user: db.users.get(r.ownerUserId)!, sessionId: "automation" },
});
const message = (reply: MockReply) =>
  sentence((reply.body as { message?: string }).message || "it failed");

/**
 * Runs an action step once, as the owner approved it: the mock's real sync,
 * load or publish. A sync or a load is marked as the run's, and the step
 * waits for it (waitJob).
 */
function runAction(r: StoredRun, nodeId: string, a: AutomationAction) {
  const done = (summary: string, result: unknown) =>
    endNode(r, nodeId, "succeeded", { summary, result });
  const failed = (why: string) => endNode(r, nodeId, "failed", null, why);
  const awaiting = (
    what: "sync" | "load",
    result: { id: string; [field: string]: unknown },
  ) => {
    state.causes.set(result.id, {
      runId: r.id,
      automationId: r.automationId,
      chain: r.event.caused_by_automations ?? [],
      depth: (r.event.chain_depth ?? 0) + 1,
    });
    r.nodes.push({
      nodeId,
      status: "running",
      output: { summary: `Waiting for the ${what}…`, result },
      error: "",
      turnId: "",
      jobId: result.id,
      usdMicros: 0,
      startedAt: now(),
      finishedAt: null,
    });
  };
  switch (a.operation) {
    case "sync_connector": {
      const route = dataRoutes.find(
        (x) => x.method === "POST" && x.path.endsWith("/connectors/{id}:sync"),
      )!;
      const reply = route.handle(
        asOwner(r, { id: a.connector_id }, {}),
      ) as MockReply;
      return reply.status === 200
        ? awaiting("sync", (reply.body as { sync: { id: string } }).sync)
        : failed(message(reply));
    }
    case "create_load": {
      uses(r, [a.ontology_id]);
      const o = ontologyById(a.ontology_id);
      if (!o || o.workspaceId !== r.workspaceId)
        return failed("Its ontology is gone.");
      const m = membership(r.workspaceId, r.ownerUserId);
      if (isReply(m) || !manages(m)) return failed("Permission denied.");
      const version = currentVersionOf(o.id);
      if (!version)
        return failed("Its ontology has no current version to load.");
      const queued = queueLoad(o, version, `${r.id}:${nodeId}`);
      return "clientToken" in queued
        ? awaiting("load", {
            id: queued.id,
            version_id: queued.versionId,
            status: queued.status,
          })
        : failed(message(queued));
    }
    case "publish_dashboard": {
      const d = storedDashboard(r.workspaceId, a.dashboard_id);
      if (!d) return failed("Its dashboard is gone.");
      // It refreshes what a person published: never an edit made since.
      if (d.publishedRevision === undefined)
        return failed(
          "Its dashboard isn't published: publish it on its page, and the workflow refreshes its snapshot from then on.",
        );
      if (d.publishedRevision !== d.revision)
        return failed(
          "Its dashboard was changed since it was published: publish it again on its page to put the change at its link.",
        );
      uses(r, d.ontologyIds);
      const reply = dashboardRoute(
        "POST",
        "/{id}:publish",
      )(asOwner(r, { id: d.id }, { expectedRevision: String(d.revision) }));
      return reply.status === 200
        ? done("Published its dashboard's snapshot again.", { id: d.id })
        : failed(message(reply));
    }
  }
}

/**
 * Reads the sync or load a running action step waits for and, once it ended,
 * ends the step as it did (the backend's waitJob); false while it runs.
 */
function waitJob(r: StoredRun, n: StoredNode): boolean {
  const config = r.spec.nodes.find((x) => x.id === n.nodeId)?.config as
    | AutomationAction
    | undefined;
  const result = n.output?.result;
  if (config?.operation === "sync_connector") {
    const sync = syncsEnded(config.connector_id).find((e) => e.id === n.jobId);
    if (!sync) return false;
    if (sync.outcome === "succeeded")
      endNode(r, n.nodeId, "succeeded", {
        summary: `Synced its connector: it wrote ${sync.rows} rows.`,
        count: sync.rows,
        result,
      });
    else
      endNode(
        r,
        n.nodeId,
        "failed",
        null,
        sync.error
          ? `The sync failed: ${sentence(sync.error)}`
          : "The sync failed.",
      );
    return true;
  }
  if (config?.operation === "create_load") {
    const load = loadsEnded(config.ontology_id).find((e) => e.id === n.jobId);
    if (!load) return false;
    if (load.outcome === "ready" || load.outcome === "partial")
      endNode(r, n.nodeId, "succeeded", {
        summary:
          load.outcome === "ready"
            ? "Loaded its ontology's current version: the load is ready."
            : "Loaded its ontology's current version: the load is partial, its tables in and some documents unread.",
        result,
      });
    else
      endNode(
        r,
        n.nodeId,
        "failed",
        null,
        load.outcome === "failed"
          ? "The load failed."
          : "Its load was deleted before the step saw it end.",
      );
    return true;
  }
  endNode(
    r,
    n.nodeId,
    "failed",
    null,
    "It waits for work it can't have started.",
  );
  return true;
}

/** The automation, as its workflows name it. */
const refOf = (a: StoredAutomation): AutomationRef => ({
  workspaceId: a.workspaceId,
  automationId: a.id,
  name: a.name,
  ownerUserId: a.ownerUserId,
});

/**
 * Records the run's uses of ontologies: the open workflow run of each, or
 * undefined where it goes untracked.
 */
function uses(r: StoredRun, ontologyIds: string[]) {
  const a = state.automations.find((x) => x.id === r.automationId);
  return ontologyIds.map((ontologyId) =>
    a
      ? useOntology(refOf(a), r.id, ontologyId, () => requestOf(a, r))
      : undefined,
  );
}

/** Makes the workflows of the ontologies its steps name, once approved. */
function track(a: StoredAutomation) {
  const named = new Set<string>();
  for (const n of a.spec.nodes) {
    const actions =
      n.kind === "action"
        ? [n.config]
        : n.kind === "assistant"
          ? n.config.allow || []
          : [];
    for (const x of actions) {
      if (x.operation === "create_load") named.add(x.ontology_id);
      if (x.operation === "publish_dashboard") {
        const d = storedDashboard(a.workspaceId, x.dashboard_id);
        for (const o of d?.ontologyIds ?? []) named.add(o);
      }
    }
  }
  for (const o of named) automationWorkflow(refOf(a), o);
}

/** A run in words, as its workflow runs' request (the backend's automation.Request). */
function requestOf(a: StoredAutomation, r: StoredRun): string {
  const name = (what: string, idOf: string, n?: string) =>
    n ? `the ${what} "${n}"` : `the ${what} ${idOf}`;
  const action = (x: AutomationAction) =>
    x.operation === "sync_connector"
      ? `sync ${name("connector", x.connector_id, storedConnector(x.connector_id)?.name)}`
      : x.operation === "create_load"
        ? `load the current version of ${name("ontology", x.ontology_id, ontologyById(x.ontology_id)?.name)}`
        : `publish ${name("dashboard", x.dashboard_id, storedDashboard(a.workspaceId, x.dashboard_id)?.name)} again at its link`;
  const read = parseSpec(canonicalJson(r.spec));
  const order = read.ok ? read.order : r.spec.nodes.map((n) => n.id);
  const fired = r.nodes.find((n) => n.nodeId === order[0])?.output?.summary;
  // What fired it first: it tells one run of an automation from another.
  const lines = [
    `${fired ? `${fired} ` : ""}The workflow "${a.name}" ran these steps, in order:`,
  ];
  let step = 0;
  for (const nodeId of order) {
    const n = r.spec.nodes.find((x) => x.id === nodeId);
    if (!n || n.kind === "trigger") continue;
    step += 1;
    const text =
      n.kind === "assistant"
        ? `Founder step "${n.id}": ${n.config.instruction}${n.config.allow?.length ? ` (It may ${n.config.allow.map(action).join("; ")} unasked.)` : ""}`
        : n.kind === "action"
          ? `Action step "${n.id}": ${action(n.config)}.`
          : n.kind === "condition"
            ? `Condition "${n.id}": go on one way if ${n.config.check === "answer" ? `the step before answered ${n.config.equals}` : `the step before counted ${{ at_least: "at least", at_most: "at most", equals: "exactly" }[n.config.op]} ${n.config.value}`}, the other way if not.`
            : `Notify step "${n.id}": email its owner${n.config.message ? ` "${n.config.message}"` : ""}, with what the Founder steps before it said.`;
    lines.push(`${step}. ${text}`);
  }
  return lines.join("\n");
}

/** What a run found, in words, as its workflow runs' answer (the worker's runState.answer). */
function answerOf(r: StoredRun, status: "succeeded" | "failed", error: string) {
  const read = parseSpec(canonicalJson(r.spec));
  const order = read.ok ? read.order : [];
  const lines = [
    status === "succeeded" ? "The run succeeded." : `The run failed: ${error}`,
  ];
  if (!order.length) return lines[0]!;
  lines.push("What its steps found:");
  for (const nodeId of order.slice(1)) {
    const n = stateOf(r, nodeId),
      kind = r.spec.nodes.find((x) => x.id === nodeId)?.kind;
    if (!n) continue;
    const summary = n.output?.summary || n.error;
    lines.push(
      `- The ${kind} step "${nodeId}" ${n.status}${summary ? `: ${summary}` : ""}` +
        (n.output?.answer !== undefined
          ? ` (It answered ${n.output.answer ? "yes" : "no"}.)`
          : "") +
        (n.output?.count !== undefined && kind === "assistant"
          ? ` (It counted ${n.output.count}.)`
          : ""),
    );
  }
  return lines.join("\n");
}

/** Walks a live run as far as its clock allows (Task.advance). */
function advance(r: StoredRun) {
  const a = state.automations.find((x) => x.id === r.automationId);
  const read = parseSpec(canonicalJson(r.spec));
  if (!a || !read.ok) return;
  const stepMs = knobs.automationStepMs ?? STEP_MS;
  while (live(r) && Date.now() >= r.next) {
    r.next += stepMs;
    if (r.status === "queued") {
      Object.assign(r, { status: "running", attempts: 1, startedAt: now() });
      continue;
    }
    if (isReply(membership(r.workspaceId, r.ownerUserId))) {
      finish(
        r,
        a,
        "failed",
        "Its owner can no longer act in this workspace, so it paused.",
        true,
      );
      return;
    }
    const step = nextStep(r, read.order);
    if (step.kind === "done") {
      const first = read.order
        .map((nodeId) => stateOf(r, nodeId))
        .find((n) => n?.status === "failed");
      const kind =
        first && r.spec.nodes.find((n) => n.id === first.nodeId)?.kind;
      finish(
        r,
        a,
        step.failed ? "failed" : "succeeded",
        step.failed && first
          ? `The ${kind} step "${first.nodeId}" failed: ${first.error}`
          : "",
      );
      return;
    }
    if (step.kind === "skip") {
      endNode(r, step.id, step.afterFailure ? "blocked" : "skipped", null);
      continue;
    }
    if (step.kind === "wait") {
      const n = stateOf(r, step.id)!;
      if (n.jobId) {
        if (!waitJob(r, n)) {
          // Read again once the work may have ended.
          r.next = Date.now();
          return;
        }
        continue;
      }
      const turn = automationTurn(n.turnId);
      if (!turn) {
        endNode(r, step.id, "failed", null, "Its Founder turn is gone.");
        continue;
      }
      if (turn.live) {
        // Read again once the turn may have moved on.
        r.next = Date.now();
        return;
      }
      if (turn.status !== "done")
        endNode(
          r,
          step.id,
          "failed",
          null,
          turn.error
            ? sentence(turn.error)
            : `Founder's turn was ${turn.status}.`,
          turn.usdMicros,
        );
      else
        endNode(
          r,
          step.id,
          "succeeded",
          {
            summary: turn.report?.summary || turn.said,
            ...(turn.report?.answer !== undefined && {
              answer: turn.report.answer,
            }),
            ...(turn.report?.count !== undefined && {
              count: turn.report.count,
            }),
            ...(turn.pending && { pending: turn.pending }),
          },
          "",
          turn.usdMicros,
        );
      continue;
    }
    const node = r.spec.nodes.find((n) => n.id === step.id)!;
    switch (node.kind) {
      case "assistant": {
        const queued = queueAutomationTurn({
          workspaceId: r.workspaceId,
          userId: r.ownerUserId,
          threadId: r.threadId,
          title: a.name,
          instruction: node.config.instruction,
          allow: node.config.allow || [],
          use: (ontologyId) => {
            const run = uses(r, [ontologyId])[0];
            // A run that holds all it may records nothing more; the call
            // goes on (the backend's trackedRun).
            return (
              run &&
              ((call) => {
                if (run.accesses.length < WORKFLOW_CAPS.accesses)
                  record(run, call);
              })
            );
          },
        });
        r.threadId = queued.threadId;
        r.nodes.push({
          nodeId: node.id,
          status: "running",
          output: null,
          error: "",
          turnId: queued.turnId,
          usdMicros: 0,
          startedAt: now(),
          finishedAt: null,
        });
        break;
      }
      case "action":
        runAction(r, node.id, node.config);
        break;
      case "condition": {
        const before = stateOf(r, edgesInto(r.spec, node.id)[0]!.from)?.output;
        const c = node.config;
        const value = c.check === "answer" ? before?.answer : before?.count;
        if (value === undefined) {
          endNode(
            r,
            node.id,
            "failed",
            null,
            c.check === "answer"
              ? "Founder gave no yes-or-no answer."
              : "The step before it gave no count.",
          );
          break;
        }
        const branch =
          c.check === "answer"
            ? value === (c.equals === "yes")
            : c.op === "at_least"
              ? (value as number) >= c.value
              : c.op === "at_most"
                ? (value as number) <= c.value
                : value === c.value;
        endNode(r, node.id, "succeeded", { branch });
        break;
      }
      case "notify":
        endNode(r, node.id, "succeeded", {
          summary: LOCAL_TOKEN
            ? "Not emailed: this install sends no email."
            : "Emailed its owner.",
        });
        break;
      case "trigger":
        endNode(r, node.id, "succeeded", null);
    }
  }
}

/**
 * Ends the run and counts it toward its automation's failures in a row
 * (store.FinishFoundryAutomationRun): a success clears them; a failure
 * holds the next run back, doubling up to a day, and pauses an active
 * automation at the third; an owner who lost access pauses it at once.
 */
function finish(
  r: StoredRun,
  a: StoredAutomation,
  status: "succeeded" | "failed",
  error: string,
  lostAccess = false,
) {
  Object.assign(r, { status, error, finishedAt: now() });
  finishAutomationRuns(r.id, answerOf(r, status, error), true);
  if (status === "succeeded") {
    Object.assign(a, { failures: 0, backoffUntil: null });
    return;
  }
  a.failures += 1;
  if (lostAccess) {
    if (a.status === "active") pause(a, "owner_lost_access", "");
    return;
  }
  if (a.failures < PAUSE_AFTER || a.status !== "active") {
    a.backoffUntil =
      clock() + Math.min(FAILURE_BACKOFF_MS * 2 ** (a.failures - 1), DAY_MS);
    return;
  }
  pause(a, "failures", "");
  a.backoffUntil = null;
}

function pause(a: StoredAutomation, why: AutomationPausedReason, by: string) {
  Object.assign(a, {
    status: "paused",
    pausedReason: why,
    pausedByUserId: by,
    pausedAt: now(),
    nextFireAt: null,
    cursor: null,
    updatedAt: now(),
  });
}

/** Makes it active as it is (the backend's activate): no pause, no failures, its schedule's first time, its cursor now. */
function activate(a: StoredAutomation, approved: boolean) {
  Object.assign(a, {
    status: "active",
    pausedReason: null,
    pausedByUserId: "",
    pausedAt: null,
    failures: 0,
    backoffUntil: null,
    nextFireAt: firstFire(a.spec, clock()),
    // Work ends on the real clock: its cursor is now, however far ahead
    // the schedules' clock was moved.
    cursor: triggerSpec(a).kind === "schedule" ? null : Date.now(),
    updatedAt: now(),
    ...(approved && {
      approvedSpecRevision: a.specRevision,
      approvedAt: now(),
    }),
  });
}

/** Cancels a run's live steps and the Assistant turn it waits on. */
function cancelRun(r: StoredRun, why: string, answer: string) {
  finishAutomationRuns(r.id, answer, false);
  cancelAutomationTurns(r.nodes.map((n) => n.turnId).filter(Boolean));
  for (const n of r.nodes)
    if (n.status === "running")
      Object.assign(n, {
        status: "failed",
        error: "The run was cancelled.",
        finishedAt: now(),
      });
  Object.assign(r, { status: "cancelled", error: why, finishedAt: now() });
}

// ---------------------------------------------------------------- routes

const BASE = "/v1/workspaces/{workspaceId}/foundry/automations" as const;

/** The caller's automation, or why not. */
function found(req: MockRequest, param = "id") {
  return (
    automationOf(req.params.workspaceId!, req.params[param]!) ??
    fail("NOT_FOUND", "workflow not found")
  );
}
const isAutomation = (x: StoredAutomation | MockReply): x is StoredAutomation =>
  "spec" in x;

/** The owner's automation at `expected`, or the store's refusal. */
function owned(
  ws: string,
  a: StoredAutomation,
  c: Caller,
  expected: unknown,
): MockReply | undefined {
  if (a.ownerUserId !== c.userId)
    return fail("PERMISSION_DENIED", "only its owner changes a workflow");
  if (String(expected) !== String(a.revision))
    return refused(
      ws,
      "FOUNDRY_AUTOMATION_CHANGED",
      `the workflow is at revision ${a.revision}, not ${String(expected)}: read it again`,
    );
  return undefined;
}

const reply = (a: StoredAutomation, c: Caller) =>
  ok({ automation: automationOut(a, c, true) } satisfies AutomationResponse);

// ---------------------------------------------------------------- run stats

const MAX_STATS_DAYS = 30;

/** The day `at` falls on in `zone`, as YYYY-MM-DD. */
const dayIn = (at: number, zone: string) =>
  new Intl.DateTimeFormat("en-CA", {
    timeZone: zone,
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
  }).format(at);

/** The last `days` days of `zone`, today the last, as YYYY-MM-DD. */
function lastDays(days: number, zone: string): string[] {
  const [y, m, d] = dayIn(Date.now(), zone).split("-").map(Number);
  return Array.from({ length: days }, (_, i) =>
    new Date(Date.UTC(y!, m! - 1, d! - (days - 1 - i)))
      .toISOString()
      .slice(0, 10),
  );
}

/**
 * The runs' counts as the backend counts them (store
 * FoundryAutomationRunStats): every run, those that succeeded and failed,
 * what they spent, and the actions their Assistant steps left waiting.
 */
function counts(runs: StoredRun[]): AutomationRunCounts {
  return {
    runs: runs.length,
    succeeded: runs.filter((r) => r.status === "succeeded").length,
    failed: runs.filter((r) => r.status === "failed").length,
    usdMicros: String(runs.reduce((sum, r) => sum + r.usdMicros, 0)),
    waiting: waitingActions(
      runs.flatMap((r) => r.nodes.map((n) => n.turnId).filter(Boolean)),
    ),
  };
}

/**
 * The list's statsDays and timeZone, checked as the backend checks them:
 * the days' dates, oldest first, and the zone; null without statsDays; a
 * reply for a refusal.
 */
function statsDays(
  req: MockRequest,
): { dates: string[]; zone: string } | null | MockReply {
  const raw = req.query.get("statsDays") ?? req.query.get("stats_days"),
    zone = req.query.get("timeZone") ?? req.query.get("time_zone") ?? "";
  const days = raw === null ? 0 : Number(raw);
  if (!days && zone)
    return fail("INVALID_ARGUMENT", "time_zone goes with stats_days");
  if (!Number.isInteger(days) || days < 0 || days > MAX_STATS_DAYS)
    return fail("INVALID_ARGUMENT", `stats_days is 1 to ${MAX_STATS_DAYS}`);
  if (zone && !validZone(zone))
    return fail(
      "INVALID_ARGUMENT",
      `time_zone "${zone}" is not an IANA time zone, such as Europe/London`,
    );
  return days
    ? { dates: lastDays(days, zone || "UTC"), zone: zone || "UTC" }
    : null;
}

/**
 * The workspace's runs of `dates` in `zone`: `totals`, the listed
 * automations' counts as the caller reads them, and each day's.
 */
function summary(
  runs: StoredRun[],
  totals: AutomationRunCounts[],
  dates: string[],
  zone: string,
): AutomationRunSummary {
  const days = new Map(
    dates.map((date) => [date, { date, succeeded: 0, failed: 0, other: 0 }]),
  );
  for (const r of runs) {
    const day = days.get(dayIn(Date.parse(r.createdAt), zone))!;
    if (r.status === "succeeded") day.succeeded++;
    else if (r.status === "failed") day.failed++;
    else day.other++;
  }
  return {
    days: dates.length,
    totals: {
      runs: totals.reduce((n, t) => n + t.runs!, 0),
      succeeded: totals.reduce((n, t) => n + t.succeeded!, 0),
      failed: totals.reduce((n, t) => n + t.failed!, 0),
      usdMicros: String(totals.reduce((n, t) => n + Number(t.usdMicros), 0)),
      waiting: totals.reduce((n, t) => n + t.waiting!, 0),
    },
    byDay: [...days.values()],
  };
}

/**
 * Records ended runs of an automation as though they had run `ago` ms
 * before now (POST /__mock/automations/{id}/past-runs, `{runs: [{status,
 * ago, usdMicros?}]}`): its history, for the overview's days. Each runs
 * nothing; a failed one says why.
 */
export function pastRuns(
  automationId: string,
  body: {
    runs?: {
      status: Exclude<AutomationRunStatus, "queued" | "running">;
      ago: number;
      usdMicros?: number;
    }[];
  },
): { status: number; body: unknown } {
  const a = state.automations.find((x) => x.id === automationId);
  if (!a) return { status: 404, body: { message: "workflow not found" } };
  for (const p of body.runs || []) {
    const at = iso(Date.now() - p.ago);
    state.runs.push({
      id: id("arun"),
      automationId: a.id,
      workspaceId: a.workspaceId,
      ownerUserId: a.ownerUserId,
      status: p.status,
      error:
        p.status === "succeeded" ? "" : `It ${p.status} long ago (a past run).`,
      event: { kind: "manual", at },
      triggerKey: `past:${state.runs.length}`,
      spec: structuredClone(a.spec),
      specRevision: a.specRevision,
      threadId: "",
      usdMicros: p.usdMicros ?? 0,
      attempts: 1,
      createdAt: at,
      startedAt: at,
      finishedAt: at,
      nodes: [],
      next: 0,
    });
  }
  state.runs.sort(
    (x, y) =>
      y.createdAt.localeCompare(x.createdAt) || y.id.localeCompare(x.id),
  );
  return { status: 200, body: {} };
}

/**
 * Drops an ended run of an automation as the backend's retention prunes it
 * (POST /__mock/automations/{id}/runs/{runId}/prune): the workflow runs it
 * started stay, as they outlive it.
 */
export function pruneRun(
  automationId: string,
  runId: string,
): { status: number; body: unknown } {
  const r = state.runs.find(
    (x) => x.id === runId && x.automationId === automationId,
  );
  if (!r) return { status: 404, body: { message: "workflow run not found" } };
  if (live(r))
    return { status: 400, body: { message: "the run hasn't ended" } };
  state.runs = state.runs.filter((x) => x !== r);
  return { status: 200, body: {} };
}

export const automationRoutes: MockRoute[] = [
  {
    method: "GET",
    path: BASE,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!,
        stats = statsDays(req);
      if (stats && "status" in stats) return stats;
      const within = stats
        ? state.runs.filter(
            (r) =>
              r.workspaceId === ws &&
              dayIn(Date.parse(r.createdAt), stats.zone) >= stats.dates[0]!,
          )
        : [];
      // The actions its runs left waiting are its owner's: only they, and
      // owners and admins, count them, as the backend's list does.
      const automations = state.automations
        .filter((a) => a.workspaceId === ws)
        .sort((x, y) => y.updatedAt.localeCompare(x.updatedAt))
        .map((a) => {
          const mine = within.filter((r) => r.automationId === a.id),
            n = counts(mine);
          return {
            ...automationOut(a, c, false),
            lastFailedAt: lastFailed(a)?.createdAt ?? null,
            ...(stats && {
              stats:
                a.ownerUserId === c.userId || c.manager
                  ? n
                  : { ...n, waiting: 0 },
            }),
          };
        });
      return ok({
        automations,
        canCreate: true,
        email: !LOCAL_TOKEN,
        ...(stats && {
          summary: summary(
            within,
            automations.map((a) => a.stats!),
            stats.dates,
            stats.zone,
          ),
        }),
      } satisfies ListAutomationsResponse);
    },
  },
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!,
        body = req.body || {},
        token = String(body.clientToken || "");
      if (!token || token.length > MAX_TOKEN)
        return fail(
          "INVALID_ARGUMENT",
          `client_token is required, at most ${MAX_TOKEN} characters`,
        );
      const named = nameAndDescription(body.name, body.description);
      if ("status" in named) return named;
      const again = state.automations.find(
        (a) =>
          a.workspaceId === ws &&
          a.ownerUserId === c.userId &&
          a.clientToken === token,
      );
      if (again) return reply(again, c);
      const spec = checkedSpec(ws, body.specJson);
      if ("status" in spec) return spec;
      if (
        state.automations.filter((a) => a.workspaceId === ws).length >=
        MAX_PER_WORKSPACE
      )
        return refused(
          ws,
          "FOUNDRY_AUTOMATION_LIMIT",
          `a workspace keeps at most ${MAX_PER_WORKSPACE} workflows; delete one first`,
        );
      const a: StoredAutomation = {
        id: id("aut"),
        workspaceId: ws,
        ownerUserId: c.userId,
        ...named,
        spec,
        revision: 1,
        specRevision: 1,
        status: "needs_approval",
        pausedReason: null,
        pausedByUserId: "",
        pausedAt: null,
        approvedSpecRevision: 0,
        approvedAt: null,
        nextFireAt: null,
        failures: 0,
        backoffUntil: null,
        cursor: null,
        clientToken: token,
        createdAt: now(),
        updatedAt: now(),
      };
      state.automations.push(a);
      return reply(a, c);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const a = found(req);
      return isAutomation(a) ? reply(a, c) : a;
    },
  },
  {
    method: "PATCH",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!,
        body = req.body || {};
      if (!/^[1-9]\d*$/.test(String(body.expectedRevision ?? "")))
        return fail(
          "INVALID_ARGUMENT",
          "id and expected_revision are required",
        );
      const a = found(req);
      if (!isAutomation(a)) return a;
      const has = (k: string) => body[k] !== undefined;
      if (!has("name") && !has("description") && !has("specJson"))
        return fail(
          "INVALID_ARGUMENT",
          "set at least one of name, description and spec_json",
        );
      const named = nameAndDescription(
        has("name") ? body.name : a.name,
        has("description") ? body.description : a.description,
      );
      if ("status" in named) return named;
      const spec = has("specJson") ? checkedSpec(ws, body.specJson) : a.spec;
      if ("status" in spec) return spec;
      const refusal = owned(ws, a, c, body.expectedRevision);
      if (refusal) return refusal;
      Object.assign(a, named);
      renameAutomationWorkflows(a.id, a.name);
      a.revision += 1;
      if (has("specJson") && canonicalJson(spec) !== canonicalJson(a.spec)) {
        // The same steps laid out anew keep the spec revision approved, as
        // the WFE's store does (LayoutOnly): what was approved runs as it was.
        const layoutOnly = sameSteps(a.spec, spec);
        a.spec = spec;
        if (!layoutOnly)
          Object.assign(a, {
            specRevision: a.specRevision + 1,
            status: "needs_approval",
            pausedReason: null,
            pausedByUserId: "",
            pausedAt: null,
            nextFireAt: null,
            cursor: null,
          });
      }
      a.updatedAt = now();
      return reply(a, c);
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const a = found(req);
      if (!isAutomation(a)) return a;
      if (a.ownerUserId !== c.userId && !c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only its owner, or an owner or admin, deletes a workflow",
        );
      for (const r of state.runs)
        if (r.automationId === a.id && live(r))
          cancelRun(
            r,
            "Deleted.",
            "The workflow was deleted before its run finished.",
          );
      state.runs = state.runs.filter((r) => r.automationId !== a.id);
      state.automations = state.automations.filter((x) => x !== a);
      automationDeleted(a.id);
      return ok({});
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:approve`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!;
      if (!/^[1-9]\d*$/.test(String(req.body?.expectedRevision ?? "")))
        return fail("INVALID_ARGUMENT", "expected_revision is required");
      const a = found(req);
      if (!isAutomation(a)) return a;
      // Its targets checked again: what it names may be gone since it was saved.
      const spec = checkedSpec(ws, canonicalJson(a.spec));
      if ("status" in spec) return spec;
      const refusal = owned(ws, a, c, req.body.expectedRevision);
      if (refusal) return refusal;
      if (
        a.status !== "needs_approval" &&
        a.approvedSpecRevision === a.specRevision
      )
        return reply(a, c);
      a.revision += 1;
      activate(a, true);
      track(a);
      return reply(a, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:pause`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!;
      const a = found(req);
      if (!isAutomation(a)) return a;
      if (a.ownerUserId !== c.userId && !c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only its owner, or an owner or admin, pauses a workflow",
        );
      if (a.status === "needs_approval")
        return refused(
          ws,
          "FOUNDRY_AUTOMATION_NOT_ACTIVE",
          "the workflow waits for its owner's approval: it isn't running",
        );
      if (a.status === "active") {
        a.revision += 1;
        pause(a, "person", c.userId);
      }
      return reply(a, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:resume`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!;
      const a = found(req);
      if (!isAutomation(a)) return a;
      if (a.ownerUserId !== c.userId)
        return fail("PERMISSION_DENIED", "only its owner resumes a workflow");
      if (a.status === "needs_approval")
        return refused(
          ws,
          "FOUNDRY_AUTOMATION_UNAPPROVED",
          "the workflow waits for your approval: approve it first",
        );
      if (a.status === "paused") {
        a.revision += 1;
        activate(a, false);
      }
      return reply(a, c);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:run`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!,
        token = String(req.body?.clientToken || "");
      if (!token || token.length > MAX_TOKEN)
        return fail(
          "INVALID_ARGUMENT",
          `client_token is required, at most ${MAX_TOKEN} characters`,
        );
      const a = found(req);
      if (!isAutomation(a)) return a;
      const key = "manual:" + token;
      const again = state.runs.find(
        (r) => r.automationId === a.id && r.triggerKey === key,
      );
      if (again)
        return ok({
          run: runOut(again, a, c, false),
        } satisfies AutomationRunResponse);
      if (a.ownerUserId !== c.userId)
        return fail("PERMISSION_DENIED", "only its owner runs a workflow");
      if (a.status === "needs_approval")
        return refused(
          ws,
          "FOUNDRY_AUTOMATION_UNAPPROVED",
          "the workflow waits for your approval: approve it first",
        );
      const busy = room(a);
      if (busy?.reason === "busy")
        return refused(ws, "FOUNDRY_AUTOMATION_BUSY", busy.why);
      if (busy) return fail("RESOURCE_EXHAUSTED", busy.why);
      const r = insertRun(
        a,
        key,
        { kind: "manual", at: now(), by_user_id: c.userId },
        { summary: "Run by hand." },
      );
      return ok({
        run: runOut(r, a, c, false),
      } satisfies AutomationRunResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{automationId}/runs`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const a = found(req, "automationId");
      if (!isAutomation(a)) return a;
      const token = req.query.get("pageToken") || "",
        from = Number(token);
      if (token && (!/^[1-9]\d*$/.test(token) || from % PAGE !== 0))
        return fail(
          "INVALID_ARGUMENT",
          "page_token is not one this list returned",
        );
      const mine = state.runs.filter((r) => r.automationId === a.id);
      return ok({
        runs: mine.slice(from, from + PAGE).map((r) => runOut(r, a, c, false)),
        ...(from + PAGE < mine.length && {
          nextPageToken: String(from + PAGE),
        }),
      } satisfies ListAutomationRunsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{automationId}/runs/{id}`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const a = found(req, "automationId");
      if (!isAutomation(a)) return a;
      const r = state.runs.find(
        (x) => x.id === req.params.id && x.automationId === a.id,
      );
      return r
        ? ok({ run: runOut(r, a, c, true) } satisfies AutomationRunResponse)
        : fail("NOT_FOUND", "run not found");
    },
  },
  {
    method: "POST",
    path: `${BASE}/{automationId}/runs/{id}:cancel`,
    handle: (req) => {
      const c = caller(req);
      if (!isCaller(c)) return c;
      const ws = req.params.workspaceId!;
      const a = found(req, "automationId");
      if (!isAutomation(a)) return a;
      const r = state.runs.find(
        (x) => x.id === req.params.id && x.automationId === a.id,
      );
      if (!r) return fail("NOT_FOUND", "run not found");
      if (a.ownerUserId !== c.userId && !c.manager)
        return fail(
          "PERMISSION_DENIED",
          "only its owner, or an owner or admin, cancels a workflow's run",
        );
      if (r.status !== "cancelled") {
        if (!live(r))
          return refused(
            ws,
            "FOUNDRY_AUTOMATION_RUN_ENDED",
            `the run already ended: it ${r.status}`,
          );
        cancelRun(
          r,
          "Cancelled by a person.",
          "A person cancelled the workflow's run before it finished.",
        );
      }
      return ok({ run: runOut(r, a, c, true) } satisfies AutomationRunResponse);
    },
  },
];

/**
 * An automation route's handler by its method and its path after
 * .../foundry/automations, for the Assistant's cards, which call it as
 * their person (./agent.ts).
 */
export function automationRoute(method: MockRoute["method"], suffix: string) {
  const route = automationRoutes.find(
    (r) => r.method === method && r.path === BASE + suffix,
  );
  if (!route) throw new Error(`no automation route ${method} ${suffix}`);
  return route.handle as (req: MockRequest) => MockReply;
}

/** The workspace's automations, for the Assistant's tools (./agent.ts). */
export function workspaceAutomations(ws: string) {
  work(ws);
  return state.automations
    .filter((a) => a.workspaceId === ws)
    .map((a) => ({
      id: a.id,
      name: a.name,
      status: a.status,
      revision: a.revision,
      ownerUserId: a.ownerUserId,
    }));
}
