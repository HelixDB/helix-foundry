import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import {
  compileTemplate,
  matchTemplate,
  type HttpMethod,
} from "../../src/shared/route-template";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_LOAD,
  FOUNDRY_NODES as N,
  isFoundryLabel,
} from "../../src/shared/foundry-labels";
import { isRecord } from "../../src/shared/json";
import type {
  AgentActionDisplays,
  AgentActionResponse,
  AgentActionStatus,
  AgentDecision,
  AgentEvent,
  AgentLoadRef,
  AgentOperation,
  AgentPageContext,
  AgentRuntime,
  AgentReview,
  AgentReviewEntity,
  AgentThreadResponse,
  AgentTurnResponse,
  AutomationAction,
  AutomationSpec,
  Connector,
  ConnectorKind,
  FoundryAccessTarget,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  FoundryAgentAnswers,
  FoundryAgentAsked,
  AgentQuestion,
  AgentQuestionAnswer,
  FoundryAgentEvent,
  FoundryAgentThread,
  FoundryAgentTurn,
  FoundryAutomation,
  FoundryAutomationRun,
  FoundryDashboard,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryLoad,
  FoundryOntology,
  FoundryQuestion,
  ListAgentThreadsResponse,
  ListAgentTurnEventsResponse,
  OntologyGeneration,
  Project,
  Tenant,
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
import {
  config,
  db,
  isReply,
  LOCAL_TOKEN,
  membership,
  onReset,
} from "../state";
import { answerWords, checkAnswers } from "../agent-questions";
import { guarded, restrictedMember } from "./access";
import {
  aiAssistantFailure,
  aiModel,
  aiRefusal,
  foundryPrecondition as refused,
} from "./ai";
import {
  agentHostRoutes,
  helperById,
  helperFailure,
  hostOffline,
  runtimeOf,
  workspaceRuntime,
} from "./agent-hosts";
import {
  WEEKDAYS,
  sentence as automationSentence,
  validZone,
  type Weekday,
} from "../../src/web/areas/automations/model";
import { automationRoutes, workspaceAutomations } from "./automations";
import {
  VISUALS,
  dashboardRoutes,
  publicLinks,
  storedDashboard,
} from "./dashboards";
import { connectionName, dataRoutes, workspaceConnectors } from "./data";
import { databaseRoutes, tenantGraph } from "./databases";
import { documentRoutes } from "./documents";
import { currentLoadId, loadRoutes, ontologyLoad, readLoad } from "./loads";
import {
  ontologiesRoutes,
  ontologyById,
  workspaceOntologies,
  type StoredOntology,
} from "./ontologies";
import {
  currentVersionOf,
  ontologyRoutes,
  ontologyVersion,
  ontologyVersions,
} from "./ontology";
import {
  askQuestion,
  listed,
  questionRoutes,
  storedQuestion,
} from "./questions";
import { reviewRoutes } from "./reviews";
import type { WorkflowCall } from "./workflows";
import { manages } from "./workspaces";
import { decisionTypes } from "../review-decisions";

// Founder (the Assistant in code), as the backend's API and
// foundry-agent-worker run it (backend/api/agent.go, backend/agent): a
// person's private threads, each
// message a turn that is queued, claimed and streams events the dock polls
// by afterSeq, in the backend's shapes (src/shared/wfe.ts "Agent"). The
// mock's agent is scripted, with the read tools' outputs in their shapes.
// It reads what a message asks for by its words and prepares one action of
// the 20 the backend allows (agent.Operations), its display as the
// backend's display.go names it:
// - "build a dashboard of invoices by status for Customer contracts"
//   (create_dashboard: tries a tile of the ontology named, else the page's,
//   else the first with a current load, counting its invoices by status
//   with helix_foundry_preview_dashboard_tile, the mock's real preview; a
//   tile that can't be read, nothing loaded or no invoices, says why). A
//   new dashboard a message asks for (the word "dashboard", whole) runs at
//   once, as the backend's auto-run rule allows (its card and result carry
//   `auto`): unpinned and unpublished, which the dock's
//   Undo deletes; anything else stays a card;
// - "make a dashboard that shows which invoices are unpaid or overdue"
//   (a dashboard and a question word: which, show, unpaid, overdue, billed,
//   how many) asks the Analyst first (helix_foundry_ask as the person, then
//   helix_foundry_get_question until it ends, both naming the question for
//   the dock), then makes a dashboard of its answer's tables and charts at
//   once, a tile each with the query it recorded; an answer with none makes
//   nothing. A limit the Analyst keeps (30 questions an hour, the month's
//   budget) is said to be the Analyst's;
// - a question of the data about invoices, customers or accounts ("which
//   customers …", "how many invoices …") goes to the Analyst the same way,
//   with a sentence around its answer, never the answer again;
// - on a dashboard's page, "pin" or "unpin" it, "publish" or "unpublish"
//   it (publish only while public links are on; unpublish whenever it is
//   published), "delete" it or "rename it to X" (pin_dashboard,
//   publish_dashboard, unpublish_dashboard, delete_dashboard,
//   update_dashboard, after helix_foundry_get_dashboard);
// - "every Monday at 9, sync Stripe and email me a summary of …" (or any
//   "automate …", "every day …", or anything written on the page that
//   starts a new automation, its page context's newAutomation):
//   create_automation of a schedule (UTC), or of documents arriving in
//   the first set of the workspace's own ("when … uploaded"),
//   that syncs the connection named (else the first, when asked to sync),
//   asks the Assistant for the summary asked for and emails it, after
//   helix_foundry_list_connectors; "run", "pause", "resume" or "delete" an
//   automation it names (run_automation, pause_automation,
//   resume_automation, delete_automation, after
//   helix_foundry_get_automation);
// - "create an ontology called X on database Y" (create_ontology; "on a new
//   database Y" makes it in the workspace's first project);
// - "generate" or "draft" (generate_ontology: the connections and sets the
//   message names, else every connection; "documents only" reads none; a
//   quoted text is the goal);
// - "help me set up an ontology" asks the person three questions of its
//   own through helix_foundry_ask_person (the goal, a choice of one; the
//   data, a choice of several from the workspace's connections; what to
//   leave out, in words, optional), ending the turn; their answers (an
//   answers turn) it reads back as a plan;
// - a draft that waits on its agent's questions (needs_input, the mock's
//   `ontologyQuestions`), once its generate or answers card ran, or asked
//   to "answer the draft's questions", is relayed through
//   helix_foundry_ask_person as its agent wrote it; the person's answers
//   go to it as they send them (use_defaults for a question left out),
//   before the answers turn, which says so: the Assistant never answers a
//   draft itself;
// - "make v3 current" (set_current_version: else the newest finished draft);
// - "load" (create_load: the current version if it loads, else the newest
//   finished draft); "make the previous load current" (set_current_load);
// - "preview" or "revise" (revise_version: removes the draft's items the
//   message names, else its first rule; "preview" is a dry run);
// - "sync Stripe" (sync_connector); "add the sample data" (add_sample_data);
// - "create a document set called X", "rename the set X to Y" and "move
//   <document> to <set>" (create_document_set, update_document_set,
//   move_document);
// - "settle the review about Globex" (settle_review, after
//   helix_foundry_list_reviews: links its first candidate, else the
//   decision its words name: "not in the data", "approve", "reject",
//   "remove", "set it to X");
// - "connect Slack" (helix_foundry_connect: a link to the kind's form, which
//   the dock opens in the app);
// - on a local install, "run new threads on my Claude subscription" (or
//   "on Codex", once its owner confirmed it in Settings, or "on Foundry's
//   model") and "revoke the helper" (set_assistant_runtime,
//   revoke_assistant_helper on the newest helper, after
//   helix_foundry_get_assistant_runtime); hosted, it says they are a local
//   install's;
// - anything else, it reads the ontologies, and the one named or on the
//   page, and says what they hold.
// The ontology an action acts on is the one named, else the page's, else
// the one this thread last acted on, else the newest (a card that warns it
// was not named). A member gets the read tools, and prepares only their own
// dashboards and automations (MEMBER_OPERATIONS: the backend decides at the
// click whose automation it is), as the backend gives them: anything else,
// connect included, the agent says an owner or admin does. A grantee (a
// restricted member: ./access.ts admits them to their own threads) gets
// Founder read only, as the worker's ReachGrants runs it (granteeScript):
// it lists the ontologies their grants hold, asks the Analyst about the one
// named, the page's or the first, and prepares, makes and relays nothing;
// their page context keeps only what their grants hold, and their threads
// made with the workspace's whole view (before their restriction, or while
// they were an owner or admin), or whose grants they lost, look absent. An automation's
// Assistant step (queueAutomationTurn, for ./automations.ts) is a turn of
// kind automation in its run's thread, out of the person's list: it reads
// the ontologies and, asked to load one, prepares its load, which runs at
// once when the step's allow approves it and waits as a card otherwise;
// then it reports, answering a yes-or-no instruction with
// `automationAnswer` ("yes") and a how-many one with `automationCount` (the
// ontologies'); `agentOutcome` fails it as any turn. A create or an edit of
// an automation that carries steps is approved by the click that ran it. Running a card calls the mock's own WFE routes as the person who
// clicked (each operation's REST call, as the backend's execute does), after
// the backend's checks: the person's own action, its hash, still waiting,
// its confirmation alive, the thread idle; then a continuation turn goes on
// from the result: a created ontology's draft is prepared next, a draft that
// finished is offered to make current, and a version made current to load.
// add_sample_data adds the sample's connection and set; the mock uploads
// none of its documents. A thread holds at most 3 waiting cards. It runs on
// any provider kind, as the worker does, but fails a turn at once, in the
// worker's words, while the setting's Assistant tool check failed (./ai.ts:
// a Questions model containing "notoolcall" or "toolrefused", a key
// containing "agentauth"). Every event takes `agentStepMs`, and its text
// streams first, as `text_delta` events of its step (a model call)
// `agentDeltaMs` apart (a quarter of a step), which the text drops as the
// worker's does;
// `agentOutcome` "fail" fails a turn after its
// first call, "budget_exhausted" stops it at its cap, "not_run" shows a
// second call a limit kept from running; `agentActionTtlMs` is how long a
// card's confirmation lives (5 minutes). `{"agentEnabled": false}` is a
// Foundry with foundry.agent.enabled off: every Assistant route (whileOn)
// is FAILED_PRECONDITION, FOUNDRY_AGENT_DISABLED.

type Knobs = {
  agentStepMs?: number;
  agentDeltaMs?: number;
  agentOutcome?: "fail" | "budget_exhausted" | "not_run";
  agentActionTtlMs?: number;
  agentEnabled?: boolean;
  automationAnswer?: "yes" | "no";
  automationCount?: number;
};
const knobs = config as typeof config & Knobs;

const STEP_MS = 400,
  ACTION_TTL_MS = 5 * 60_000,
  // An action executing this long reads unknown (FoundryAgentExecutingFor).
  EXECUTING_FOR_MS = 2 * 60_000,
  // How long a continuation waits for a draft before it says it still runs.
  WAIT_MS = 60_000,
  MAX_TEXT = 4000,
  MAX_TOKEN = 64,
  PER_HOUR = 30,
  MAX_PENDING = 3,
  PAGE = 50,
  TURNS = 100,
  EVENTS = 200,
  TITLE_CHARS = 80,
  ID = /^[A-Za-z0-9_-]{1,128}$/,
  MODEL = "claude-sonnet-5-5",
  // The site's origin, as the backend's mcp.foundry_url names it: the dock
  // opens a connect link by its path.
  SITE = "https://foundry.mock",
  // The sample's database as the mock's builds name it (next.config.ts).
  SAMPLE_HOST = "helix-dev-sample.cdmqgow4207f.us-east-2.rds.amazonaws.com";

/** What a step of a script sees and leaves for the next ones. */
type Context = {
  workspaceId: string;
  userId: string;
  turn: StoredTurn;
};
/**
 * One event of a script, decided when it is shown: null shows none, and
 * undefined holds the script until it is asked again (a draft still being
 * written).
 */
type Step = (c: Context) => Scripted | null | undefined;
/**
 * An event as a script writes it: text without its step, which `advance`
 * numbers and streams as the worker does (its deltas, then the text).
 */
type Scripted =
  | Exclude<AgentEvent, { type: "text" | "text_delta" }>
  | { type: "text"; text: string };

type StoredThread = {
  id: string;
  workspaceId: string;
  userId: string;
  // An automation's run's thread is out of its person's list.
  kind: "chat" | "automation";
  title: string;
  runtime: AgentRuntime;
  // The grant targets a restricted member's turns of it ran under, which
  // they must all still hold to read it; null for a thread made or run with
  // the workspace's whole view, which no restricted member reads
  // (model.FoundryAgentThread.Granted).
  granted: FoundryAccessTarget[] | null;
  createdAt: string;
  updatedAt: string;
  deletedAt: string | null;
};
type StoredTurn = {
  id: string;
  threadId: string;
  workspaceId: string;
  userId: string;
  kind: "message" | "action_result" | "answers" | "automation";
  text: string;
  actionId: string;
  // What it asked its person (a questions event), and an answers turn's answers.
  asked: FoundryAgentAsked | null;
  answers: FoundryAgentAnswers | null;
  pageContext: AgentPageContext | null;
  clientToken: string;
  status: FoundryAgentTurnStatus;
  error: string;
  attempts: number;
  steps: number;
  usdMicros: number;
  // When it was sent, in ms; when its next event is due.
  at: number;
  next: number;
  // When a step first held it.
  heldSince: number | null;
  stepMs: number;
  deltaMs: number;
  script: Step[];
  shown: number;
  // The text a step is streaming: what is left to send as deltas, and the
  // seqs of those sent, which its text drops.
  saying: { step: number; text: string; left: string[]; sent: number[] } | null;
  // The last seq written (deltas dropped leave gaps), and its event's type.
  seq: number;
  last: AgentEvent["type"] | "";
  events: FoundryAgentEvent[];
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
};
/** What a prepare records: an action of one operation, as the sink keeps it. */
type Plan = {
  [K in AgentOperation]: {
    operation: K;
    target: string;
    // The canonical payload, in the backend's field order.
    payload: Record<string, unknown>;
    display: AgentActionDisplays[K];
    named: boolean;
    // The ontology its target names, or "".
    ontologyId: string;
    // What the agent says once its card shows.
    said: string;
  };
}[AgentOperation];
type StoredAction = Plan & {
  id: string;
  turnId: string;
  threadId: string;
  workspaceId: string;
  userId: string;
  payloadHash: string;
  status: AgentActionStatus;
  // The operation's output, as the backend's FoundryOperationOutput.
  result: Record<string, unknown> | null;
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
  delete knobs.agentDeltaMs;
  delete knobs.agentOutcome;
  delete knobs.agentActionTtlMs;
  delete knobs.agentEnabled;
  delete knobs.automationAnswer;
  delete knobs.automationCount;
});

/** The Assistant's routes, as a Foundry with it off refuses every one (agentEnabled false). */
export const whileOn = (routes: MockRoute[]): MockRoute[] =>
  routes.map((r) => ({
    ...r,
    handle: (req) =>
      knobs.agentEnabled === false
        ? refused(
            req.params.workspaceId!,
            "FOUNDRY_AGENT_DISABLED",
            "Founder is off for this Foundry",
          )
        : r.handle(req),
  }));

const live = (t: { status: FoundryAgentTurnStatus }) =>
  t.status === "queued" || t.status === "running";
const iso = (ms: number) => new Date(ms).toISOString();
const canonical = (payload: Record<string, unknown>) => JSON.stringify(payload);
const hash = (p: Pick<Plan, "operation" | "target" | "payload">) =>
  createHash("sha256")
    .update(`${p.operation}\u0000${p.target}\u0000${canonical(p.payload)}`)
    .digest("hex");

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

/** Writes an event at the turn's next seq. */
function write(t: StoredTurn, e: AgentEvent) {
  t.seq += 1;
  t.last = e.type;
  t.events.push({ seq: t.seq, eventJson: JSON.stringify(e), createdAt: now() });
}

// A text's deltas: about four words each, which put together are the text.
const DELTAS = /(?:\s*\S+){1,4}\s*|\s+/g;

/**
 * Shows the events whose time has come, and ends the turn at its finished.
 * A model call starts at its reply's text, or its first call when it wrote
 * none: `steps` counts them, numbering its text. A step's text streams as
 * the worker writes it: deltas `deltaMs` apart, then the text, which drops
 * them in the same write (FinishFoundryAgentText).
 */
function advance(t: StoredTurn) {
  const at = Date.now();
  if (t.status === "queued" && at >= t.at + t.stepMs) {
    t.status = "running";
    t.attempts = 1;
    t.startedAt = iso(t.at + t.stepMs);
  }
  const c: Context = { workspaceId: t.workspaceId, userId: t.userId, turn: t };
  while (t.status === "running" && at >= t.next) {
    const saying = t.saying;
    if (saying) {
      const delta = saying.left.shift();
      if (delta !== undefined) {
        write(t, { type: "text_delta", step: saying.step, text: delta });
        saying.sent.push(t.seq);
        t.next += t.deltaMs;
        continue;
      }
      t.events = t.events.filter((e) => !saying.sent.includes(e.seq));
      t.saying = null;
      write(t, { type: "text", step: saying.step, text: saying.text });
      t.next += t.stepMs;
      continue;
    }
    if (t.shown >= t.script.length) return;
    const event = t.script[t.shown]!(c);
    if (event === undefined) {
      // Held: asked again a step from now.
      t.heldSince ??= at;
      t.next = at + t.stepMs;
      return;
    }
    t.heldSince = null;
    t.shown += 1;
    if (!event) continue;
    if (
      (event.type === "text" || event.type === "tool_call") &&
      t.last !== "text"
    ) {
      t.steps += 1;
      t.usdMicros += 1_200;
    }
    if (event.type === "text") {
      t.saying = {
        step: t.steps,
        text: event.text,
        left: event.text.match(DELTAS) ?? [],
        sent: [],
      };
      continue;
    }
    write(t, event);
    t.next += t.stepMs;
    if (event.type === "questions")
      t.asked = {
        questions: event.questions,
        ...(event.generation && {
          generation: {
            ontologyId: event.generation.ontology_id,
            generationId: event.generation.generation_id,
            round: event.generation.round,
          },
        }),
      };
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
  runtime: t.runtime,
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
  lastSeq: t.seq,
  attempts: t.attempts,
  createdAt: t.createdAt,
  startedAt: t.startedAt,
  finishedAt: t.finishedAt,
  ...(t.asked && { asked: structuredClone(t.asked) }),
  ...(t.answers && { answers: structuredClone(t.answers) }),
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
  targetNamedByUser: a.named,
  status: shown(a),
  resultJson: a.result ? JSON.stringify(a.result) : "",
  error: a.error,
  expiresAt: iso(a.expiresAt),
  decidedByUserId: a.decidedByUserId,
  decidedAt: a.decidedAt,
  createdAt: a.createdAt,
});

/**
 * The caller as Founder answers them: a member of the workspace, and, for a
 * grantee (a restricted member, admitted by ./access.ts to these routes
 * only), what its grants hold: its threads made with the whole view, or
 * whose grants it no longer holds, look absent, as the backend's agentThread
 * has it.
 */
function person(req: MockRequest) {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  return isReply(m)
    ? m
    : { m, userId: req.principal.user.id, grantee: req.grantee };
}
const isPerson = (
  x: ReturnType<typeof person>,
): x is Exclude<ReturnType<typeof person>, MockReply> => "userId" in x;

/** Whether a grantee may read thread `t`: one it made, all of whose grants it holds. */
const granteeReads = (t: StoredThread, grantee?: MockGrantee) =>
  !grantee || (!!t.granted && grantee.holdsAll(t.granted));

/** The person's thread, unless deleted or not the grantee's to read. */
function threadOf(
  workspaceId: string,
  userId: string,
  threadId: string,
  grantee?: MockGrantee,
) {
  return state.threads.find(
    (t) =>
      t.id === threadId &&
      t.workspaceId === workspaceId &&
      t.userId === userId &&
      !t.deletedAt &&
      granteeReads(t, grantee),
  );
}
function turnOf(
  workspaceId: string,
  userId: string,
  turnId: string,
  grantee?: MockGrantee,
) {
  const t = state.turns.find((x) => x.id === turnId);
  return t && threadOf(workspaceId, userId, t.threadId, grantee)
    ? t
    : undefined;
}
/**
 * NOT_FOUND for a turn turnOf doesn't find: "thread not found" when it is
 * the person's in a thread the grantee may not read, as the backend's
 * agentTurn says it.
 */
function turnMissing(workspaceId: string, userId: string, turnId: string) {
  const mine = state.turns.some(
    (x) =>
      x.id === turnId && x.workspaceId === workspaceId && x.userId === userId,
  );
  return fail("NOT_FOUND", mine ? "thread not found" : "turn not found");
}
function actionOf(workspaceId: string, userId: string, actionId: string) {
  const a = state.actions.find((x) => x.id === actionId);
  return a && threadOf(workspaceId, userId, a.threadId) ? a : undefined;
}

/** Why the thread can't take a message or a click now. */
function busy(t: StoredThread) {
  advanceThread(t.id);
  if (state.turns.some((x) => x.threadId === t.id && live(x)))
    return "Founder is still answering in this thread";
  if (
    state.actions.some((a) => a.threadId === t.id && shown(a) === "executing")
  )
    return "an action of this thread is running";
  return undefined;
}

// ---------------------------------------------------------------- the WFE

let compiled: (MockRoute & { c: ReturnType<typeof compileTemplate> })[];
/**
 * A WFE call as `userId` makes it, through the mock's own routes: how a
 * tool reads and an action runs, as the backend's do through Foundry's API.
 * Each route runs the access check first (./access.ts guarded), so a
 * grantee's calls are narrowed as the WFE narrows a tool's. Every route it
 * calls answers at once.
 */
function wfe(
  userId: string,
  method: HttpMethod,
  path: string,
  body?: unknown,
): MockReply & { body: any } {
  compiled ??= [
    ...ontologiesRoutes,
    ...ontologyRoutes,
    ...loadRoutes,
    ...dataRoutes,
    ...documentRoutes,
    ...reviewRoutes,
    ...databaseRoutes,
    ...agentHostRoutes,
    ...dashboardRoutes,
    ...automationRoutes,
    // Reads only: asking is askQuestion's (its POST answers later).
    ...questionRoutes,
  ]
    .map(guarded)
    .map((r) => ({ ...r, c: compileTemplate(r.path) }));
  const url = new URL(path, "http://mock");
  for (const r of compiled) {
    const params = r.method === method && matchTemplate(r.c, url.pathname);
    if (!params) continue;
    const reply = r.handle({
      method,
      path: url.pathname,
      params,
      query: url.searchParams,
      body,
      principal: { user: db.users.get(userId)!, sessionId: "agent" },
    });
    if (reply instanceof Promise)
      throw new Error(`Founder's ${method} ${path} answers later`);
    return reply as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const WS = (ws: string) => `/v1/workspaces/${ws}/foundry`;
const ONT = (ws: string, o: string) => `${WS(ws)}/ontologies/${o}`;
/** The reply's message, as a tool's error says it. */
const why = (r: MockReply) =>
  (r.body as { message?: string } | undefined)?.message || "it failed";

/** The WFE's camelCase answer with snake_case keys, as the MCP's outputs name them. */
function snake(x: unknown): unknown {
  if (Array.isArray(x)) return x.map(snake);
  if (!isRecord(x)) return x;
  return Object.fromEntries(
    Object.entries(x).map(([k, v]) => [
      k.replace(/[A-Z]/g, (c) => "_" + c.toLowerCase()),
      snake(v),
    ]),
  );
}

// ---------------------------------------------------------------- reads

const ontologyOutput = (o: StoredOntology) => ({
  id: o.id,
  workspace_id: o.workspaceId,
  name: o.name,
  database_name: o.databaseName,
  project_id: o.projectId,
  current_version_id: currentVersionOf(o.id) || "",
  current_load_id: currentLoadId(o.id) || "",
});

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

/** The workspace's live databases, with their projects, as helix_list_databases lists them. */
function databases(ws: string, userId: string) {
  const projects = (wfe(userId, "GET", `/v1/projects?workspaceId=${ws}`).body
    ?.projects || []) as Project[];
  return projects.flatMap((p) =>
    (
      (wfe(userId, "GET", `/v1/tenants?projectId=${p.id}`).body?.tenants ||
        []) as Tenant[]
    )
      .filter((t) => t.status !== "RESOURCE_STATUS_DELETED")
      .map((t) => ({ ...t, project: p })),
  );
}
const sets = (ws: string, userId: string) =>
  (wfe(userId, "GET", `${WS(ws)}/document-sets`).body?.sets ||
    []) as FoundryDocumentSet[];
const uploads = (ws: string, userId: string) =>
  (wfe(userId, "GET", `${WS(ws)}/documents`).body?.documents ||
    []) as FoundryDocument[];
const kinds = (ws: string, userId: string) =>
  (wfe(userId, "GET", `${WS(ws)}/connector-kinds`).body?.kinds ||
    []) as ConnectorKind[];

const HELD = ["low_confidence", "unverified", "conflict"],
  RESOLVED = [
    "unresolved",
    "ambiguous",
    "collision",
    "identifier_conflict",
    "adjudicated",
    "legal_form",
    "part_name",
    "own_name",
  ];
/**
 * The reviews an ontology's current load holds, as helix_foundry_list_reviews
 * gives them (integration.FoundryReviewOutput), with what settles each.
 */
function reviewsOf(o: StoredOntology): AgentReview[] {
  const load = currentLoadId(o.id);
  if (!load) return [];
  const text = (v: unknown) => (typeof v === "string" ? v : ""),
    graph = tenantGraph(o.tenantId);
  // The entities a review concerns, as ListFoundryReviews names them.
  const concerned = (review: number): AgentReviewEntity[] =>
    (graph.outgoing.get(review) || []).flatMap((e) => {
      const edge = graph.edges.get(e),
        n = edge?.label === E.CONCERNS ? graph.nodes.get(edge.to) : undefined;
      if (!n || isFoundryLabel("node", n.label)) return [];
      const key = text(n.props.entity_key),
        norm = text(n.props.norm_name),
        name = text(n.props.name);
      if (!key === !norm) return [];
      return [
        {
          type: n.label,
          ...(key ? { entity_key: key } : { norm_name: norm }),
          ...(name && { name }),
        },
      ];
    });
  return [...graph.nodes.values()]
    .filter((n) => n.label === N.Review && n.props[FOUNDRY_LOAD] === load)
    .map(({ id: node, props: r }) => {
      const entities = concerned(node);
      const reason = text(r.reason),
        statement = text(r.statement),
        link = text(r.suggested_link);
      const settle: AgentReview["settle"] = HELD.includes(reason)
        ? text(r.doc_id) && text(r.claim_key) && link && statement
          ? "release"
          : "stale"
        : reason === "prop_conflict"
          ? text(r.type) && text(r.entity_key) && text(r.prop)
            ? "prop"
            : "stale"
          : RESOLVED.includes(reason)
            ? text(r.type) && text(r.doc_id) && text(r.norm_name)
              ? "resolve"
              : "stale"
            : "none";
      const values = Array.isArray(r.values)
        ? r.values.map((v) => {
            const q = typeof v === "string" ? JSON.parse(v) : v;
            return {
              value: q?.value,
              ...(q?.doc_id && { doc_id: q.doc_id }),
              ...(q?.quote && { quote: q.quote }),
            };
          })
        : [];
      return {
        review_key: text(r.review_key),
        reason,
        settle,
        ...(text(r.type) && { type: text(r.type) }),
        ...(text(r.doc_id) && { doc_id: text(r.doc_id) }),
        ...(text(r.quote) && { quote: text(r.quote) }),
        ...(typeof r.section === "number" && { section: r.section }),
        ...(typeof r.start === "number" && { start: r.start }),
        ...(typeof r.end === "number" && { end: r.end }),
        ...(text(r.norm_name) && { norm_name: text(r.norm_name) }),
        ...(Array.isArray(r.candidates) &&
          r.candidates.length && { candidates: r.candidates.map(String) }),
        ...(text(r.entity_key) && { entity_key: text(r.entity_key) }),
        ...(text(r.prop) && { prop: text(r.prop) }),
        ...(values.length && { values }),
        ...(link && { link }),
        ...(statement && {
          statement,
          statement_hash: createHash("sha256")
            .update(`${link}\u001f${statement}`)
            .digest("hex"),
        }),
        ...(typeof r.confidence === "number" && { confidence: r.confidence }),
        ...(entities.length && { entities }),
      };
    });
}

// ---------------------------------------------------------------- words

const lower = (s: string) => s.toLowerCase();
const says = (text: string, word: string) =>
  !!word && lower(text).includes(lower(word));
/** The text after `lead` up to a comma, `stop` or the end, unquoted. */
function after(text: string, lead: RegExp, stop = /[,;]/) {
  const m = new RegExp(lead.source + /\s+["“]?(.+)$/.source, "i").exec(text);
  if (!m) return undefined;
  const rest = m[1]!
    .split(stop)[0]!
    .replace(/["”.!?]+$/, "")
    .trim();
  return rest || undefined;
}
/** The first quoted text: a description. */
const quotedText = (text: string) =>
  /[“"]([^”"]+)[”"]/.exec(text)?.[1]?.trim() || undefined;
/** A generation's goal: what follows "goal", quoted or not. */
const goalOf = (text: string) =>
  /\bgoal:?\s*["“]?([^"”]+)["”]?/i.exec(text)?.[1]?.trim() || undefined;

/** Whether the person named any of `words` in this thread, or was on the ontology's page. */
const namedByPerson = (threadId: string, words: string[], ontologyId = "") =>
  state.turns.some(
    (t) =>
      t.threadId === threadId &&
      t.kind === "message" &&
      (words.some((w) => says(t.text, w)) ||
        (!!ontologyId && t.pageContext?.ontologyId === ontologyId)),
  );

/** The ontology a message names, by name or id. */
const named = (workspaceId: string, text: string) =>
  workspaceOntologies(workspaceId).find(
    (o) => says(text, o.name) || text.includes(o.id),
  );

/** The ontology this thread last acted on: an action's target, or the one it created. */
function threadOntology(threadId: string) {
  for (const a of [...state.actions].reverse()) {
    if (a.threadId !== threadId) continue;
    const made =
      a.operation === "create_ontology" && a.result
        ? (a.result.result as { id?: string } | undefined)?.id
        : undefined;
    const o = ontologyById(made || a.ontologyId);
    if (o && !o.deletedAt) return o;
  }
  return undefined;
}

/** The ontology a message acts on, and whether the person named it. */
function ontologyFor(
  workspaceId: string,
  threadId: string,
  text: string,
  page: AgentPageContext | null,
  prefer?: (o: StoredOntology) => boolean,
) {
  const all = workspaceOntologies(workspaceId),
    pageOntology = page?.ontologyId ? ontologyById(page.ontologyId) : undefined;
  const o =
    named(workspaceId, text) ??
    (pageOntology?.workspaceId === workspaceId ? pageOntology : undefined) ??
    threadOntology(threadId) ??
    (prefer && all.find(prefer)) ??
    all[0];
  return o
    ? { o, named: namedByPerson(threadId, [o.name, o.id], o.id) }
    : undefined;
}

/** The version a load would write: the current one if it loads, else the newest finished draft. */
const loads = (v: { definition: Record<string, unknown> }) =>
  v.definition.format === "agent_bundle" && v.definition.status === "done";
function loadable(ontologyId: string) {
  const versions = ontologyVersions(ontologyId),
    current = versions.find((v) => v.id === currentVersionOf(ontologyId));
  return current && loads(current) ? current : versions.find(loads);
}

const ref = (o: StoredOntology) => ({ id: o.id, name: o.name });
const vref = (ontologyId: string, versionId: string) => ({
  id: versionId,
  number: versionNumber(ontologyId, versionId),
});
function lref(ontologyId: string, loadId: string): AgentLoadRef {
  const l = ontologyLoad(ontologyId, loadId);
  return {
    id: loadId,
    version: l ? vref(ontologyId, l.versionId) : { id: "" },
    ...(l && { status: l.status, created_at: l.createdAt }),
  };
}

// ---------------------------------------------------------------- steps

/** A helper's runtime in a sentence. */
const RUNTIME_ON = {
  claude_code: "your Claude subscription",
  codex: "your ChatGPT subscription (Codex)",
} as const;

// A helper's turn runs on its CLI, as the backend names it.
const started: Step = (c) => ({
  type: "started",
  model:
    {
      claude_code: "Claude Code",
      codex: "Codex",
      api: undefined,
    }[state.threads.find((t) => t.id === c.turn.threadId)?.runtime ?? "api"] ??
    (aiModel(c.workspaceId, "ask") || MODEL),
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
 * result (every script starts with them, but a failed action's continuation).
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
    case "not_run":
      // A ninth call of one reply: shown, never run (agent.cutCalls).
      return [
        ...after,
        call("tu_cut", LIST, {}),
        () => ({
          type: "tool_result",
          tool_use_id: "tu_cut",
          name: LIST,
          is_error: true,
          not_run: "calls",
        }),
        ...steps.slice(3),
        answered,
      ];
    default:
      return [...steps, answered];
  }
}

const LIST = "helix_foundry_list_ontologies",
  GET = "helix_foundry_get_ontology",
  GET_LOAD = "helix_foundry_get_load",
  GET_GENERATION = "helix_foundry_get_generation",
  GET_SYNC = "helix_foundry_get_sync",
  CONNECTORS = "helix_foundry_list_connectors",
  DOCUMENTS = "helix_foundry_list_documents",
  REVIEWS = "helix_foundry_list_reviews",
  DATABASES = "helix_list_databases",
  CONNECT = "helix_foundry_connect",
  RUNTIME = "helix_foundry_get_assistant_runtime",
  PREVIEW = "helix_foundry_preview_dashboard_tile",
  GET_DASHBOARD = "helix_foundry_get_dashboard",
  GET_AUTOMATION = "helix_foundry_get_automation",
  PREPARE = "helix_foundry_prepare_operation",
  READ = "helix_foundry_read_graph",
  ASK = "helix_foundry_ask",
  GET_QUESTION = "helix_foundry_get_question";

/** What a member, who manages nothing, prepares: their own dashboards and automations (integration.memberOperations). */
const MEMBER_OPERATIONS: readonly (
  | AgentOperation
  | "connect"
  | "relay_questions"
)[] = [
  "create_dashboard",
  "update_dashboard",
  "delete_dashboard",
  "create_automation",
  "update_automation",
  "pause_automation",
  "resume_automation",
  "delete_automation",
  "run_automation",
];

/** What an owner or admin does that a member asked for, in the agent's words. */
const VERBS: Record<AgentOperation, string> = {
  create_ontology: "create an ontology",
  generate_ontology: "draft a version",
  set_current_version: "make a version current",
  create_load: "load an ontology",
  set_current_load: "make a load current",
  revise_version: "revise a version",
  sync_connector: "sync a connection",
  add_sample_data: "add the sample data",
  create_document_set: "create a document set",
  update_document_set: "change a document set",
  move_document: "move a document",
  settle_review: "settle a review",
  set_assistant_runtime: "choose where Founder runs",
  revoke_assistant_helper: "revoke a helper",
  create_dashboard: "make a dashboard",
  update_dashboard: "change a dashboard",
  delete_dashboard: "delete a dashboard",
  pin_dashboard: "pin a dashboard",
  publish_dashboard: "publish a dashboard",
  unpublish_dashboard: "unpublish a dashboard",
  create_automation: "set up an automation",
  update_automation: "change an automation",
  pause_automation: "pause an automation",
  resume_automation: "resume an automation",
  delete_automation: "delete an automation",
  run_automation: "run an automation",
};

/**
 * The prepare of a plan decided when it is shown, as the sink records it,
 * then its result and the agent's words: the card, or why there is none
 * (the review it decides isn't one the load holds as read, too many
 * waiting). A string is the agent's answer when there is nothing to prepare.
 */
function prepare(decide: (c: Context) => Plan | string): Step[] {
  const plans = new Map<string, Plan | string>(),
    outcome = new Map<string, string>(),
    cards = new Map<string, StoredAction>();
  const plan = (c: Context) => {
    if (!plans.has(c.turn.id)) plans.set(c.turn.id, decide(c));
    return plans.get(c.turn.id)!;
  };
  return [
    (c) => {
      const p = plan(c);
      return typeof p === "string"
        ? { type: "text", text: p }
        : {
            type: "tool_call",
            tool_use_id: "tu_prepare",
            name: PREPARE,
            title: PREPARE,
            input: {
              operation: p.operation,
              target: p.target,
              payload: p.payload,
            },
          };
    },
    (c) => {
      const p = plan(c);
      if (typeof p === "string") return null;
      const h = hash(p);
      const again = state.actions.find(
        (a) => a.turnId === c.turn.id && a.payloadHash === h,
      );
      const pending = state.actions.filter(
        (a) => a.threadId === c.turn.threadId && shown(a) === "prepared",
      ).length;
      const refusal =
        p.operation === "settle_review" && !again
          ? settleRefusal(p)
          : !again && pending >= MAX_PENDING
            ? `${pending} actions already wait for the person; let them run or dismiss those first`
            : undefined;
      if (refusal) {
        outcome.set(c.turn.id, refusal);
        return {
          type: "tool_result",
          tool_use_id: "tu_prepare",
          name: PREPARE,
          is_error: true,
          error: refusal,
        };
      }
      const a: StoredAction = again ?? {
        ...p,
        id: id("act"),
        turnId: c.turn.id,
        threadId: c.turn.threadId,
        workspaceId: c.workspaceId,
        userId: c.userId,
        payloadHash: h,
        status: "prepared",
        result: null,
        error: "",
        expiresAt: Date.now() + (knobs.agentActionTtlMs ?? ACTION_TTL_MS),
        decidedByUserId: "",
        decidedAt: null,
        createdAt: now(),
      };
      if (!again) state.actions.push(a);
      cards.set(c.turn.id, a);
      return {
        type: "action_card",
        action_id: a.id,
        operation: a.operation,
        target: a.target,
        payload: a.payload,
        payload_hash: a.payloadHash,
        display: a.display,
        target_named_by_user: a.named,
        expires_at: iso(a.expiresAt),
      };
    },
    (c) => {
      const a = cards.get(c.turn.id);
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
    (c) => {
      const p = plan(c);
      if (typeof p === "string") return null;
      const refusal = outcome.get(c.turn.id);
      return {
        type: "text",
        text: !refusal
          ? p.said
          : refusal.includes("wait for the person")
            ? "Other actions still wait for you: run or dismiss those first, and I can prepare this one."
            : `I couldn't prepare that: ${refusal}.`,
      };
    },
  ];
}

/**
 * Why settle_review's card is refused before its person sees it, as the
 * backend's describeSettle: the review must be in the current load, settled
 * by that kind of decision, and read as the statement it names.
 */
function settleRefusal(p: Extract<Plan, { operation: "settle_review" }>) {
  const o = ontologyById(p.ontologyId),
    r =
      o &&
      reviewsOf(o).find((x) => x.review_key === p.display.review.review_key);
  if (!r)
    return `the current load holds no review ${p.display.review.review_key}: list them again with ${REVIEWS}`;
  const takes: Record<AgentDecision["kind"], AgentReview["settle"]> = {
    link: "resolve",
    not_in_data: "resolve",
    set_value: "prop",
    remove_value: "prop",
    approve: "release",
    reject: "release",
  };
  if (r.settle === "stale")
    return "this review lacks what settling it reads, as a load from before decisions wrote it: a new load writes it again";
  if (r.settle === "none") return "no decision settles this review";
  if (r.settle !== takes[p.display.decision.kind])
    return `this review takes a ${r.settle} decision, as ${REVIEWS} says`;
  return undefined;
}

// ---------------------------------------------------------------- plans

type Ask = {
  ws: string;
  userId: string;
  threadId: string;
  text: string;
  page: AgentPageContext | null;
};

/** A plan on an ontology's target. */
function onOntology<K extends AgentOperation>(
  ws: string,
  o: StoredOntology,
  named: boolean,
  operation: K,
  payload: Record<string, unknown>,
  display: AgentActionDisplays[K],
  said: string,
): Plan {
  return {
    operation,
    target: `ontology:${ws}/${o.id}`,
    payload,
    display,
    named,
    ontologyId: o.id,
    said,
  } as Plan;
}

function createOntology(a: Ask): Step[] {
  const name = after(a.text, /\b(?:called|named)/, /\s+on\s+|[,;]/i);
  const fresh = /\bnew database\b/i.test(a.text),
    wanted = after(a.text, /\bdatabase/);
  const list = databases(a.ws, a.userId);
  return [
    call("tu_dbs", DATABASES, { workspace_id: a.ws }),
    result("tu_dbs", DATABASES, () => ({
      databases: list.map((t) => ({
        id: t.id,
        name: t.name,
        slug: t.slug,
        project_id: t.projectId,
        project_name: t.project.displayName,
        reference: `tenant:${t.id}`,
      })),
    })),
    ...prepare((): Plan | string => {
      if (!name || !wanted)
        return "Tell me what to call the ontology and which database it goes on, as in “create an ontology called Orders graph on database orders”.";
      if (fresh) {
        const project = list[0]?.project;
        if (!project)
          return "This workspace has no project to make a database in yet.";
        const slug = lower(wanted)
          .replace(/[^a-z0-9]+/g, "-")
          .replace(/^-|-$/g, "")
          .slice(0, 63);
        return {
          operation: "create_ontology",
          target: `workspace:${a.ws}`,
          payload: {
            name,
            database: { name: wanted, slug, project_id: project.id },
          },
          display: {
            name,
            new_database: { name: wanted, slug, project_id: project.id },
          },
          named: true,
          ontologyId: "",
          said: `I've prepared “${name}” on a new database, ${wanted}. Run it from the card, and I'll draft its first version next.`,
        };
      }
      const t = list.find(
        (x) =>
          lower(x.slug || "") === lower(wanted) ||
          lower(x.name || "") === lower(wanted),
      );
      if (!t)
        return `There is no database called ${wanted} in this workspace. Ask me to make it on a new database ${wanted}, or name one of these: ${list.map((x) => x.slug).join(", ")}.`;
      const bound = workspaceOntologies(a.ws).find((o) => o.tenantId === t.id);
      if (bound)
        return `${t.name} already holds the ontology ${bound.name}: a database holds one ontology. Choose another database.`;
      return {
        operation: "create_ontology",
        target: `tenant:${t.id}`,
        payload: { name },
        display: {
          name,
          database: { id: t.id, name: t.name || t.slug || t.id },
        },
        named: namedByPerson(a.threadId, [t.name || "", t.slug || ""]),
        ontologyId: "",
        said: `I've prepared “${name}” on the ${t.name} database. Run it from the card, and I'll draft its first version next.`,
      };
    }),
  ];
}

/** The draft of `o`: what the message names, else every connection. */
function generatePlan(
  a: Ask,
  o: StoredOntology,
  named: boolean,
  goal?: string,
): Plan {
  const connectors = workspaceConnectors(a.ws).filter(
      (c) => c.kind !== "events",
    ),
    chosen = connectors.filter(
      (c) => says(a.text, c.name || "") || says(a.text, c.kind),
    ),
    none = /\b(documents only|no connect(?:ions|ors))\b/i.test(a.text),
    picked = sets(a.ws, a.userId).filter(
      (s) => !s.connector && says(a.text, s.name || ""),
    );
  // Every connector reads the sample's made-up data too, which the backend
  // refuses (agent.Cards): the agent then names the others.
  const own = connectors.filter((c) => !isSample(c));
  const read = none
    ? "none"
    : chosen.length || own.length < connectors.length
      ? "chosen"
      : "every";
  const shown = read === "none" ? [] : read === "chosen" ? chosen : connectors;
  if (read === "chosen" && !chosen.length) shown.push(...own);
  return onOntology(
    a.ws,
    o,
    named,
    "generate_ontology",
    {
      ...(goal && { goal }),
      ...(read === "chosen" && { connector_ids: shown.map((c) => c.id) }),
      ...(picked.length && { document_set_ids: picked.map((s) => s.id) }),
      ...(none && { no_connectors: true }),
    },
    {
      ontology: ref(o),
      ...(goal && { goal }),
      connectors_read: read,
      connectors: shown.map((c) => ({
        id: c.id,
        name: c.name,
        kind: c.kind,
        ...(isSample(c) && { sample: true as const }),
      })),
      document_sets: picked.map((s) => ({ id: s.id, name: s.name })),
    },
    `I've prepared a draft of ${o.name} from ${read === "none" ? "its documents alone" : read === "every" ? "every connection" : shown.map((c) => c.name).join(" and ")}. Run it from the card, and I'll offer to make it current once it's done.`,
  );
}

/** Making `versionId` of `o` current. */
const currentPlan = (
  ws: string,
  o: StoredOntology,
  named: boolean,
  versionId: string,
): Plan => {
  const current = currentVersionOf(o.id);
  const number = versionNumber(o.id, versionId);
  return onOntology(
    ws,
    o,
    named,
    "set_current_version",
    { version_id: versionId, expected_current_version_id: current || "" },
    {
      ontology: ref(o),
      version: vref(o.id, versionId),
      ...(current && { current: vref(o.id, current) }),
    },
    `I've prepared making v${number} current for ${o.name}. Run it from the card, and I'll offer to load it next.`,
  );
};

/** Loading `versionId` of `o`. */
const loadPlan = (
  ws: string,
  o: StoredOntology,
  named: boolean,
  versionId: string,
): Plan => {
  const number = versionNumber(o.id, versionId);
  return onOntology(
    ws,
    o,
    named,
    "create_load",
    { version_id: versionId },
    { ontology: ref(o), version: vref(o.id, versionId) },
    `I've prepared a load of v${number} of ${o.name}. Run it from the card when you're ready.`,
  );
};

/** Who sends a message: what Founder may prepare for them. */
type Who = "manager" | "member" | "grantee";
// No words ask for update_automation: an automation's edits are its page's.
type Intent =
  | Exclude<AgentOperation, "update_automation">
  | "connect"
  | "relay_questions";

/** What each change a message may ask for reads as, tried in order. */
const INTENTS: readonly (readonly [RegExp, Intent])[] = [
  [
    /\banswers?\b.*\bquestions?\b|\buse (?:the|its) (?:defaults|suggestions)\b/i,
    "relay_questions",
  ],
  [/\brevoke\b.*\bhelper\b/i, "revoke_assistant_helper"],
  [
    /\b(run|switch|move)\b.*\b(subscription|claude code|codex|foundry['’]s model)\b/i,
    "set_assistant_runtime",
  ],
  [/\bconnect\b/i, "connect"],
  [/\bcreate\b.*\bontology\b/i, "create_ontology"],
  [/\b(settle|resolve)\b.*\breview\b|\breview about\b/i, "settle_review"],
  [/\bsync\b/i, "sync_connector"],
  [/\bsample\b/i, "add_sample_data"],
  [/\bmove\b/i, "move_document"],
  [/\b(rename|describe)\b.*\bset\b/i, "update_document_set"],
  [/\b(create|make|add)\b.*\bset\b/i, "create_document_set"],
  [/\b(revise|preview)\b/i, "revise_version"],
  [/\b(generate|draft)\b/i, "generate_ontology"],
  [/\bload\b.*\bcurrent\b/i, "set_current_load"],
  [/\bcurrent\b/i, "set_current_version"],
];

/**
 * The change a message asks for, by its words, if any: an automation's,
 * a new dashboard, what it asks of the dashboard on the page, else the
 * first of INTENTS, else a load that isn't a question.
 */
function intentOf(
  ws: string,
  t: string,
  automated: AutomationAsk | undefined,
  asked: DashboardAsk | undefined,
): Intent | undefined {
  if (automated) return automated.kind;
  if (/\bdashboard\b/i.test(t) && /\b(build|make|create)\b/i.test(t))
    return "create_dashboard";
  if (asked) return asked.operation;
  // A question of the data may say "current" ("each contact's current
  // title"): it isn't a make-current.
  const listed = INTENTS.find(
    ([words, intent]) =>
      words.test(t) &&
      !(intent === "set_current_version" && asksData(ws, t)),
  );
  if (listed) return listed[1];
  return /\b(re)?load\b/i.test(t) &&
    !/\b(how many|which|what|when|why)\b/i.test(t)
    ? "create_load"
    : undefined;
}

/**
 * Where the mock's Founder sends a message: an operation's intent, with
 * the automation or dashboard ask it named; "setup_questions" (what it
 * asks first); "ask_dashboard" (the Analyst asked, its answer's charts made
 * a dashboard); "ask" (a question of the data) or "describe". messageScript
 * runs it; the unit tests send Founder's suggestions through it
 * (messageIntent).
 */
function messageRoute(a: Pick<Ask, "ws" | "text" | "page">, manager: boolean) {
  const t = a.text;
  const board = a.page?.dashboardId
      ? storedDashboard(a.ws, a.page.dashboardId)
      : undefined,
    asked = (board && dashboardAsk(t)) || undefined;
  // What the Assistant needs to know first, asked in the dock.
  if (manager && /\bset ?up\b.*\bontolog|\bhelp me\b.*\bontolog/i.test(t))
    return { intent: "setup_questions" as const };
  const automated = automationAsk(a.ws, t, !!a.page?.newAutomation);
  // A dashboard of what the data says (which invoices, how many, what they
  // show): the Analyst asked first, its answer's charts made a dashboard.
  // Asking is a read, and a member makes their own dashboards.
  if (
    !automated &&
    !asked &&
    /\bdashboards?\b/i.test(t) &&
    /\b(which|shows?|unpaid|overdue|billed|how many)\b/i.test(t)
  )
    return { intent: "ask_dashboard" as const };
  const intent = intentOf(a.ws, t, automated, asked);
  if (intent) return { intent, automated, board, asked };
  // A question of the data goes to the Analyst (while a load holds it);
  // anything else is read with the read tools and answered.
  return {
    intent: asksData(a.ws, t) ? ("ask" as const) : ("describe" as const),
  };
}

/**
 * Where the mock's Founder sends a message an owner or admin writes on
 * Founder's page of `ontologyId`: an operation it prepares (or makes, a
 * dashboard), a question for the Analyst ("ask", or "ask_dashboard" for a
 * dashboard of its answer), or "describe".
 */
export const messageIntent = (ws: string, text: string, ontologyId = "") =>
  messageRoute({ ws, text, page: ontologyId ? { ontologyId } : null }, true)
    .intent;

/** What the agent does with a person's message. */
function messageScript(a: Ask, who: Who): Step[] {
  if (who === "grantee") return granteeScript(a);
  const manager = who === "manager";
  const list = [
    call("tu_list", LIST, {}),
    result("tu_list", LIST, () => ({
      ontologies: workspaceOntologies(a.ws).map(ontologyOutput),
    })),
  ];
  const t = a.text;
  const route = messageRoute(a, manager);
  if (route.intent === "setup_questions")
    return [started, ...list, ...setupQuestions(a)];
  if (route.intent === "ask_dashboard")
    return [started, ...list, ...askDashboard(a, who)];
  if (route.intent === "ask" || route.intent === "describe") {
    // A question of the data, about invoices, customers or accounts, of an
    // ontology with a current load, goes to the Analyst.
    const o = route.intent === "ask" ? dashboardOntology(a) : undefined;
    return [
      started,
      ...list,
      ...(o && currentLoadId(o.id) ? askData(a, o, who) : describe(a)),
    ];
  }
  const { intent, automated, board, asked } = route;
  if (!manager && !MEMBER_OPERATIONS.includes(intent))
    return [
      started,
      ...list,
      said(
        () =>
          `Only an owner or admin of this workspace can ${
            intent === "connect"
              ? "connect data"
              : intent === "relay_questions"
                ? "answer a draft's questions"
                : intent === "create_load" && named(a.ws, t)
                  ? `load ${named(a.ws, t)!.name}`
                  : VERBS[intent]
          }, so I can't prepare that for you.`,
      ),
    ];
  switch (intent) {
    case "set_assistant_runtime":
    case "revoke_assistant_helper":
      return [started, ...list, ...runtimeOperation(intent, a)];
    case "connect":
      return [started, ...list, ...connect(a)];
    case "create_ontology":
      return [started, ...list, ...createOntology(a)];
    case "create_dashboard":
      return [started, ...list, ...buildDashboard(a)];
    case "create_automation":
      return [started, ...automationCreate(a)];
    case "pause_automation":
    case "resume_automation":
    case "delete_automation":
    case "run_automation":
      return [
        started,
        ...automationOperation(
          a,
          intent,
          (automated as Extract<AutomationAsk, { automation: unknown }>)
            .automation,
        ),
      ];
    case "update_dashboard":
    case "delete_dashboard":
    case "pin_dashboard":
    case "publish_dashboard":
    case "unpublish_dashboard":
      return [started, ...onDashboard(a, board!, asked!)];
  }
  const found = ontologyFor(
    a.ws,
    a.threadId,
    t,
    a.page,
    intent === "create_load" ? (o) => !!loadable(o.id) : undefined,
  );
  if (!found)
    return [
      started,
      ...list,
      said(() => "This workspace has no ontology yet. Create one first."),
    ];
  const { o } = found;
  const read = [
    call("tu_get", GET, { ontology_id: o.id }),
    result("tu_get", GET, () => ontologyOutput(o)),
  ];
  return [started, ...list, ...read, ...operationOf(intent, a, o, found.named)];
}

/**
 * Where the Assistant runs, on the workspace's target: read with
 * helix_foundry_get_assistant_runtime, then a card. A local install's
 * only: hosted registers neither the tool nor the operations.
 */
function runtimeOperation(
  intent: "set_assistant_runtime" | "revoke_assistant_helper",
  a: Ask,
): Step[] {
  if (!LOCAL_TOKEN)
    return [
      said(
        () =>
          "Where Founder runs is chosen on a local install of Foundry only: here it runs on the workspace’s AI model.",
      ),
    ];
  const read = () => {
    const r = runtimeOf(a.ws).runtime!;
    return {
      content_trust: "untrusted",
      runtime: r.runtime,
      online: r.online,
      hosts: (r.hosts || []).map((h) => ({
        id: h.id,
        name: h.name,
        runtimes: h.runtimes,
        accounts: h.accounts,
        last_seen_at: h.lastSeenAt,
        online: h.online,
      })),
      // As the backend's GetAssistantRuntimeOutput.
      codex_confirmed: !!r.codexConfirmedAt,
      codex_min_version: r.codexMinVersion,
      settings_url: "/settings/ai",
    };
  };
  const target = `workspace:${a.ws}`;
  return [
    call("tu_runtime", RUNTIME, { workspace_id: a.ws }),
    result("tu_runtime", RUNTIME, read),
    ...prepare(() => {
      const now = runtimeOf(a.ws).runtime!;
      if (intent === "set_assistant_runtime") {
        const to = /\bclaude\b/i.test(a.text)
          ? "claude_code"
          : /\b(codex|chatgpt)\b/i.test(a.text)
            ? "codex"
            : "api";
        const online = to === "api" || (now.online || []).includes(to);
        if (to === now.runtime)
          return to === "api"
            ? "New threads already run on the workspace’s AI model."
            : `New threads already run on ${RUNTIME_ON[to]}.`;
        // As the backend's validation: what codex runs with is confirmed in Settings, never through a card.
        if (to === "codex" && !now.codexConfirmedAt)
          return "Codex runs with your own Codex setup, on your ChatGPT plan: choose it first in Settings › AI models, where you confirm that once.";
        return {
          operation: "set_assistant_runtime",
          target,
          payload: { runtime: to },
          display: { runtime: to, current: now.runtime!, online },
          named: true,
          ontologyId: "",
          said:
            to === "api"
              ? "I’ve prepared switching new threads back to the workspace’s AI model. Switch from the card when you’re ready."
              : `I’ve prepared running new threads on ${RUNTIME_ON[to]}.${online ? "" : " No helper is running yet: start it on your computer first."} Switch from the card when you’re ready.`,
        };
      }
      const host = (now.hosts || [])[0];
      if (!host)
        return "No helper is paired with this install. Pair one in Settings › AI models.";
      const h = helperById(host.id)!;
      return {
        operation: "revoke_assistant_helper",
        target,
        payload: { host_id: h.id },
        display: {
          host: { id: h.id, name: h.name },
          runtimes: h.runtimes || [],
          online: !!h.online,
        },
        named: /\bhelper\b/i.test(a.text),
        ontologyId: "",
        said: `I’ve prepared revoking ${h.name || "the helper"}. Revoke it from the card if that’s the one.`,
      };
    }),
  ];
}

/** The reads and the prepare of an operation on `o`. */
function operationOf(
  intent:
    | Exclude<
        AgentOperation,
        | "create_ontology"
        | "set_assistant_runtime"
        | "revoke_assistant_helper"
        | `${string}_dashboard`
        | `${string}_automation`
      >
    | "relay_questions",
  a: Ask,
  o: StoredOntology,
  named: boolean,
): Step[] {
  const version = /\bv(\d+)\b/i.exec(a.text)?.[1];
  const numbered = (n: string | undefined) =>
    n ? ontologyVersions(o.id).find((v) => String(v.version) === n) : undefined;
  switch (intent) {
    case "relay_questions":
      return relayQuestions(a, o);
    case "generate_ontology":
      return [
        call("tu_connectors", CONNECTORS, { workspace_id: a.ws }),
        result("tu_connectors", CONNECTORS, () => ({
          connectors: workspaceConnectors(a.ws).map((c) => ({
            id: c.id,
            name: c.name,
            kind: c.kind,
          })),
        })),
        ...prepare(() => generatePlan(a, o, named, goalOf(a.text))),
      ];
    case "set_current_version":
      return prepare(() => {
        const v = version
          ? numbered(version)
          : ontologyVersions(o.id).find(
              (x) => loads(x) && x.id !== currentVersionOf(o.id),
            );
        if (!v)
          return version
            ? `${o.name} has no v${version}.`
            : `${o.name} has no finished draft that isn't current already.`;
        if (v.id === currentVersionOf(o.id))
          return `v${v.version} is already ${o.name}'s current version.`;
        return currentPlan(a.ws, o, named, v.id);
      });
    case "create_load":
      return prepare(() => {
        const v = version ? numbered(version) : loadable(o.id);
        return v
          ? loadPlan(a.ws, o, named, v.id)
          : version
            ? `${o.name} has no v${version}.`
            : `${o.name} has no finished draft to load yet. Generate one first.`;
      });
    case "set_current_load":
      return prepare(() => {
        const current = currentLoadId(o.id);
        const l = (
          (wfe(a.userId, "GET", `${ONT(a.ws, o.id)}/loads`).body?.loads ||
            []) as FoundryLoad[]
        ).find(
          (x) =>
            x.id !== current &&
            (x.status === "ready" || x.status === "partial"),
        );
        if (!l) return `${o.name} has no other ready load to make current.`;
        const shownLoad = lref(o.id, l.id);
        return onOntology(
          a.ws,
          o,
          named,
          "set_current_load",
          { load_id: l.id },
          {
            ontology: ref(o),
            load: shownLoad,
            ...(current && { current: lref(o.id, current) }),
          },
          `I've prepared making the v${shownLoad.version.number} load current for ${o.name}. Run it from the card when you're ready.`,
        );
      });
    case "revise_version":
      return prepare(() => {
        const v = version
          ? numbered(version)
          : ontologyVersions(o.id).find((x) => x.id === currentVersionOf(o.id));
        const items = Object.keys(
          ((v?.definition.draft as { draft?: { items?: object } } | undefined)
            ?.draft?.items || {}) as object,
        );
        if (!v || !loads(v) || !items.length)
          return `${o.name} has no finished agent draft to revise.`;
        const chosen = items.filter((i) => says(a.text, i));
        const edits = chosen.length
          ? chosen
          : items.filter((i) => /^(rule|constraint)\./.test(i)).slice(0, 1);
        if (!edits.length)
          return `Name the draft's items to remove: ${items.slice(0, 5).join(", ")}.`;
        const dry = /\bpreview\b/i.test(a.text);
        return onOntology(
          a.ws,
          o,
          named,
          "revise_version",
          {
            version_id: v.id,
            edits: edits.map((i) => ({ remove: { item_id: i } })),
            ...(dry && { dry_run: true }),
          },
          {
            ontology: ref(o),
            version: vref(o.id, v.id),
            edits: edits.map((i) => ({ kind: "remove" as const, item_id: i })),
            dry_run: dry,
          },
          `I've prepared ${dry ? "a preview of" : ""} removing ${edits.join(", ")} from v${v.version} of ${o.name}. Run it from the card when you're ready.`.replace(
            "prepared  ",
            "prepared ",
          ),
        );
      });
    case "sync_connector":
      return [
        call("tu_connectors", CONNECTORS, { workspace_id: a.ws }),
        result("tu_connectors", CONNECTORS, () => ({
          connectors: workspaceConnectors(a.ws).map((c) => ({
            id: c.id,
            name: c.name,
            kind: c.kind,
          })),
        })),
        ...prepare((): Plan | string => {
          const c = workspaceConnectors(a.ws).find(
            (x) =>
              x.kind !== "events" &&
              (says(a.text, x.name || "") || says(a.text, x.kind)),
          );
          if (!c)
            return `Which connection? This workspace has ${workspaceConnectors(
              a.ws,
            )
              .map((x) => x.name)
              .join(", ")}.`;
          return {
            operation: "sync_connector",
            target: `connector:${a.ws}/${c.id}`,
            payload: {},
            display: { connector: { id: c.id, name: c.name, kind: c.kind } },
            named: namedByPerson(a.threadId, [c.name || "", c.kind]),
            ontologyId: "",
            said: `I've prepared a sync of ${c.name}. Run it from the card when you're ready.`,
          };
        }),
      ];
    case "add_sample_data":
      return prepare(() =>
        onOntology(
          a.ws,
          o,
          named,
          "add_sample_data",
          {},
          { ontology: ref(o) },
          `I've prepared adding the sample company's data for ${o.name}. Run it from the card when you're ready.`,
        ),
      );
    case "create_document_set":
      return prepare(() => {
        const name = after(a.text, /\b(?:called|named)/);
        if (!name)
          return "What should the set be called? Say “create a document set called Contracts”.";
        const description = quotedText(a.text.replace(name, ""));
        return onOntology(
          a.ws,
          o,
          named,
          "create_document_set",
          { name, ...(description && { description }) },
          { ontology: ref(o), name, ...(description && { description }) },
          `I've prepared the set “${name}”. Run it from the card, then upload documents into it from Data.`,
        );
      });
    case "update_document_set":
      return [
        ...setsRead(a),
        ...prepare(() => {
          const set = sets(a.ws, a.userId).find(
            (s) => !s.connector && says(a.text, s.name || ""),
          );
          if (!set) return "Which set? Name it as Data shows it.";
          const name = /\brename\b/i.test(a.text)
            ? after(a.text, /\bto/)
            : undefined;
          const description = /\bdescribe\b/i.test(a.text)
            ? (quotedText(a.text) ?? "")
            : undefined;
          if (name === undefined && description === undefined)
            return `What should change about ${set.name}?`;
          return onOntology(
            a.ws,
            o,
            named,
            "update_document_set",
            {
              set_id: set.id,
              ...(name !== undefined && { name }),
              ...(description !== undefined && { description }),
            },
            {
              ontology: ref(o),
              set: { id: set.id, name: set.name },
              ...(name !== undefined && { name }),
              ...(description !== undefined && { description }),
            },
            `I've prepared the change to ${set.name}. Run it from the card when you're ready.`,
          );
        }),
      ];
    case "move_document":
      return [
        call("tu_documents", DOCUMENTS, { workspace_id: a.ws }),
        result("tu_documents", DOCUMENTS, () => ({
          documents: uploads(a.ws, a.userId).map((d) => ({
            id: d.id,
            name: d.name,
            set_id: d.setId,
          })),
        })),
        ...setsRead(a),
        ...prepare(() => {
          const doc = uploads(a.ws, a.userId).find((d) =>
            says(a.text, d.name || ""),
          );
          const to = after(a.text, /\bto/);
          const all = sets(a.ws, a.userId),
            set = to && all.find((s) => !s.connector && says(to, s.name || ""));
          if (!doc) return "Which document? Name it as Data shows it.";
          if (!set) return "Which set should it go to?";
          const from = all.find((s) => s.id === doc.setId);
          return onOntology(
            a.ws,
            o,
            named,
            "move_document",
            { document_id: doc.id, set_id: set.id },
            {
              ontology: ref(o),
              document: { id: doc.id, name: doc.name },
              from: { id: doc.setId || "", ...(from && { name: from.name }) },
              to: { id: set.id, name: set.name },
            },
            `I've prepared moving ${doc.name} to ${set.name}. Run it from the card when you're ready.`,
          );
        }),
      ];
    case "settle_review":
      return [
        call("tu_reviews", REVIEWS, { ontology_id: o.id }),
        result("tu_reviews", REVIEWS, () => ({
          load_id: currentLoadId(o.id) || "",
          reviews: reviewsOf(o),
        })),
        ...prepare(() => settlePlan(a, o, named)),
      ];
  }
}

const setsRead = (a: Ask) => [
  call("tu_sets", GET, { workspace_id: a.ws, document_sets: true }),
  result("tu_sets", GET, () => ({
    document_sets: sets(a.ws, a.userId).map((s) => ({
      id: s.id,
      name: s.name,
      documents: s.documents,
    })),
  })),
];

/** The review the message is about, and the decision its words name. */
function settlePlan(a: Ask, o: StoredOntology, named: boolean): Plan | string {
  const load = currentLoadId(o.id);
  if (!load)
    return `${o.name} has no current load, so nothing waits for review.`;
  const all = reviewsOf(o).filter((r) => r.settle !== "none");
  const about = (after(a.text, /\babout/) || "")
    .replace(
      new RegExp(
        `\\s*\\bin ${o.name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\b`,
        "i",
      ),
      "",
    )
    .replace(/:.*$/, "")
    .trim();
  const words = lower(about)
    .split(/\W+/)
    .filter((w) => w.length > 2);
  const r =
    all.find((x) =>
      words.some((w) =>
        lower(`${x.quote} ${x.norm_name} ${x.statement || ""}`).includes(w),
      ),
    ) ?? (words.length ? undefined : all[0]);
  if (!r)
    return all.length
      ? `No review of the current load of ${o.name} is about ${about}.`
      : `The current load of ${o.name} has no review to settle.`;
  if (r.settle === "stale")
    return "That review is from a load before decisions: load again, and I can settle it then.";
  const value = after(a.text, /\bset (?:it|the value) to/);
  let decision: AgentDecision;
  let choice: Record<string, unknown>;
  if (r.settle === "release") {
    const reject = /\breject\b/i.test(a.text);
    decision = { kind: reject ? "reject" : "approve" };
    choice = {
      [reject ? "reject" : "release"]: { statement_hash: r.statement_hash },
    };
  } else if (r.settle === "prop") {
    if (/\bremove\b/i.test(a.text)) {
      decision = { kind: "remove_value" };
      choice = { prop: { null: true } };
    } else {
      const v =
        value ??
        String((r.values?.[0]?.value as string | number | undefined) ?? "");
      decision = { kind: "set_value", value: { text: v } };
      choice = { prop: { text: v } };
    }
  } else if (/not in (the )?data/i.test(a.text) || !r.candidates?.length) {
    decision = { kind: "not_in_data" };
    choice = { resolve: { not_in_data: true } };
  } else {
    const key = r.candidates[0]!;
    decision = { kind: "link", entity_key: key, candidate: true };
    choice = { resolve: { entity_key: key } };
  }
  const docName = r.doc_id
    ? uploads(a.ws, a.userId).find((d) => d.id === r.doc_id)?.name
    : undefined;
  return onOntology(
    a.ws,
    o,
    named,
    "settle_review",
    { review_id: r.review_key, decision: choice },
    {
      ontology: ref(o),
      load_id: load,
      review: r,
      ...(r.doc_id && {
        document: { id: r.doc_id, ...(docName && { name: docName }) },
      }),
      decision,
    },
    `I've prepared settling the review of “${r.norm_name || r.quote || "a statement"}”. Run it from the card when you're ready.`,
  );
}

/** helix_foundry_connect, link-only: the kind's form, where the person enters its credentials. */
function connect(a: Ask): Step[] {
  const offered = kinds(a.ws, a.userId),
    k = offered.find(
      (x) =>
        x.kind !== "events" &&
        (new RegExp(`\\b${x.kind}\\b`, "i").test(a.text) ||
          says(a.text, x.displayName || "")),
    );
  if (!k)
    return [
      said(
        () =>
          `Which kind of data? Foundry connects ${offered
            .filter((x) => x.kind !== "events")
            .map((x) => x.displayName)
            .join(", ")}.`,
      ),
    ];
  const display = k.displayName || k.kind;
  return [
    call("tu_connect", CONNECT, { workspace_id: a.ws, kind: k.kind }),
    result("tu_connect", CONNECT, () => ({
      url: `${SITE}/data?${new URLSearchParams({ add: k.kind, workspace: a.ws })}`,
      kind: k.kind,
      display_name: display,
      available: k.available !== false,
      next: `Give the user this link to open in a browser signed in to Foundry: it opens the ${display} form, where they enter the credentials and save. Then helix_foundry_list_connectors lists the new connector.`,
    })),
    said(
      () =>
        `Connect ${display} from the link above: its form opens here, and your credentials go straight to Foundry, never through me. Tell me once it's saved and I'll sync it.`,
    ),
  ];
}

/** A question: what the ontologies hold. */
function describe(a: Ask): Step[] {
  const ontologies = workspaceOntologies(a.ws),
    pageOntology = a.page?.ontologyId
      ? ontologyById(a.page.ontologyId)
      : undefined,
    // A question about the ontologies (plural) is about all of them,
    // whatever page it was asked on.
    asked =
      named(a.ws, a.text) ??
      (pageOntology?.workspaceId === a.ws && !/\bontologies\b/i.test(a.text)
        ? pageOntology
        : undefined);
  if (!asked)
    return [
      said(() =>
        ontologies.length
          ? `This workspace has ${ontologies.length} ontolog${ontologies.length === 1 ? "y" : "ies"}: ${ontologies.map((o) => o.name).join(", ")}. Ask me about one of them, or to load one.`
          : "This workspace has no ontology yet.",
      ),
    ];
  return [
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

/** A dashboard as a card names it. */
const dref = (d: NonNullable<ReturnType<typeof storedDashboard>>) => ({
  id: d.id,
  name: d.name,
  ...(d.pinned && { pinned: true as const }),
  ...(d.published && { published: true as const }),
});

/**
 * The ontology a dashboard reads: the one named, else the page's, else the
 * first with a current load, else the first.
 */
function dashboardOntology(a: Ask) {
  const all = workspaceOntologies(a.ws),
    page = a.page?.ontologyId ? ontologyById(a.page.ontologyId) : undefined;
  return (
    named(a.ws, a.text) ??
    (page?.workspaceId === a.ws ? page : undefined) ??
    all.find((x) => currentLoadId(x.id)) ??
    all[0]
  );
}

/**
 * A dashboard of an ontology's invoices by status: its tile tried as the
 * person (the mock's real preview), then create_dashboard of it, made at
 * once since the person asked for a dashboard (autoCreateDashboard), or why
 * it can't be read.
 */
function buildDashboard(a: Ask): Step[] {
  const o = dashboardOntology(a);
  if (!o)
    return [
      said(
        () =>
          "This workspace has no ontology yet, so there is no data for a dashboard. Create one first.",
      ),
    ];
  const tile = {
    id: "t1",
    title: "Invoices by status",
    ontology_id: o.id,
    load: { kind: "current" },
    query: {
      start: { type: "Invoice" },
      return: { group_count: { at: 0, prop: "status" } },
    },
    visual: { kind: "bar" },
  };
  // Tried once, when the turn first shows its result.
  let tried: ReturnType<typeof wfe> | undefined;
  const preview = () =>
    (tried ??= wfe(a.userId, "POST", `${WS(a.ws)}/dashboards:preview-tile`, {
      tileJson: JSON.stringify(tile),
    }));
  const outcome = () => {
    const r = preview();
    return r.status === 200
      ? (JSON.parse(r.body.outcomeJson || "{}") as {
          ok?: unknown;
          error?: { message?: string };
        })
      : undefined;
  };
  const payload = {
    name: "Invoices by status",
    description: `${o.name}: its invoices, counted by status.`,
    spec: { v: 1, tiles: [tile], layout: { rows: [["t1"]] } },
  };
  return [
    call("tu_preview", PREVIEW, { tile }),
    () => {
      const r = preview();
      return r.status !== 200
        ? {
            type: "tool_result",
            tool_use_id: "tu_preview",
            name: PREVIEW,
            is_error: true,
            error: why(r),
          }
        : {
            type: "tool_result",
            tool_use_id: "tu_preview",
            name: PREVIEW,
            is_error: false,
            output: {
              content_trust: "untrusted",
              tile: JSON.parse(r.body.tileJson || "{}") as unknown,
              load_id: r.body.loadId,
              as_of: r.body.asOf,
              outcome: outcome(),
            },
          };
    },
    // Only a tile that reads goes on a dashboard.
    ...dashboardSteps(a, () => {
      const read = outcome();
      if (!read?.ok)
        return `I couldn't read invoices by status from ${o.name}: ${read ? read.error?.message || "it has no answer" : why(preview())}.`;
      return {
        operation: "create_dashboard",
        target: `workspace:${a.ws}`,
        payload,
        display: {
          name: payload.name,
          description: payload.description,
          spec: {
            tiles: [{ title: tile.title, ontology: ref(o), visual: "bar" }],
          },
        },
        named: namedByPerson(a.threadId, [payload.name]),
        ontologyId: "",
        said: `with one bar chart of the invoices in ${o.name} by status`,
      };
    }),
  ];
}

type DashboardPlan = Extract<Plan, { operation: "create_dashboard" }>;
/**
 * Whether Founder makes a new dashboard without a card, as the backend's
 * autoRun allows it: in a person's message that asks for a dashboard (the
 * word, whole), on the workspace the turn is in (every plan here targets
 * it). Everything else, pinning, publishing and deleting included, stays a
 * card.
 */
const askedForDashboard = (text: string) => /\bdashboards?\b/i.test(text);

/**
 * A create_dashboard plan's steps: made at once when the person asked for a
 * dashboard (autoCreateDashboard), else prepared as a card. `said` of a plan
 * made at once is what the dashboard shows, after its name.
 */
function dashboardSteps(
  a: Ask,
  decide: (c: Context) => DashboardPlan | string,
): Step[] {
  return askedForDashboard(a.text)
    ? autoCreateDashboard(a, decide)
    : prepare((c) => {
        const p = decide(c);
        return typeof p === "string"
          ? p
          : {
              ...p,
              said: `I've prepared a dashboard${p.said ? ` ${p.said}` : ""}. Create it from the card when you're ready.`,
            };
      });
}

// What the tool tells the model of a dashboard made at once (the backend's
// ranAutoNext).
const RAN_AUTO_NEXT =
  "Foundry made this new dashboard at once, because the person asked for one; it isn't pinned or published, and they can undo it from the conversation. Give them its link (/dashboards/<id>) in a sentence.";

/**
 * create_dashboard run at once, as the backend's sink runs an action its
 * auto-run rule allows: the prepare, its card (auto), stored executing and
 * decided by the person; then its run as the person and its result (auto);
 * then the prepare's result and the agent's words, with the link. A new
 * dashboard is unpinned and unpublished: only its maker sees it in their
 * list until it's pinned, and the dock's Undo deletes it. A string is the
 * agent's answer when there is nothing to make.
 */
function autoCreateDashboard(
  a: Ask,
  decide: (c: Context) => DashboardPlan | string,
): Step[] {
  let plan: DashboardPlan | string | undefined;
  let ran: StoredAction | undefined;
  const planned = (c: Context) => (plan ??= decide(c));
  return [
    (c) => {
      const p = planned(c);
      return typeof p === "string"
        ? { type: "text", text: p }
        : {
            type: "tool_call",
            tool_use_id: "tu_prepare",
            name: PREPARE,
            title: PREPARE,
            input: {
              operation: p.operation,
              target: p.target,
              payload: p.payload,
            },
          };
    },
    (c) => {
      const p = planned(c);
      if (typeof p === "string") return null;
      const x: StoredAction = {
        ...p,
        id: id("act"),
        turnId: c.turn.id,
        threadId: a.threadId,
        workspaceId: a.ws,
        userId: c.userId,
        payloadHash: hash(p),
        status: "executing",
        result: null,
        error: "",
        expiresAt: Date.now() + (knobs.agentActionTtlMs ?? ACTION_TTL_MS),
        decidedByUserId: c.userId,
        decidedAt: now(),
        createdAt: now(),
      };
      state.actions.push(x);
      ran = x;
      return {
        type: "action_card",
        action_id: x.id,
        operation: x.operation,
        target: x.target,
        payload: x.payload,
        payload_hash: x.payloadHash,
        display: x.display,
        target_named_by_user: x.named,
        expires_at: iso(x.expiresAt),
        auto: true,
      };
    },
    (c) => {
      const x = ran;
      if (!x) return null;
      const reply = run(x, c.userId);
      if (reply.status === 200)
        Object.assign(x, {
          status: "succeeded",
          result: {
            content_trust: "untrusted",
            operation: x.operation,
            target: x.target,
            result: output(x, reply.body),
          },
        });
      else Object.assign(x, { status: "failed", error: why(reply) });
      return {
        type: "action_result",
        action_id: x.id,
        operation: x.operation,
        status: x.status,
        ...(x.result && { result: x.result }),
        ...(x.error && { error: x.error }),
        auto: true,
      };
    },
    () => {
      const x = ran;
      return x
        ? {
            type: "tool_result",
            tool_use_id: "tu_prepare",
            name: PREPARE,
            is_error: false,
            output: {
              action_id: x.id,
              status: "ran",
              outcome: x.status,
              ...(x.result && { result: x.result.result }),
              ...(x.error && { error: x.error }),
              next: RAN_AUTO_NEXT,
            },
          }
        : null;
    },
    () => {
      const x = ran,
        p = plan;
      if (!x || !p || typeof p === "string") return null;
      const made = x.result?.result as
        | { id?: string; name?: string }
        | undefined;
      const name = made?.name || p.display.name;
      return {
        type: "text",
        text:
          x.status === "succeeded" && made?.id
            ? `I made the dashboard “${name}”${p.said ? `, ${p.said}` : ""}: /dashboards/${made.id}. It isn't pinned or published; Undo deletes it.`
            : `The dashboard “${name}” wasn't made: ${x.error || "it failed"}.`,
      };
    },
  ];
}

// ---------------------------------------------------------------- the Analyst

/** What asking the Analyst came to, as the steps after it read it. */
type Asked = {
  // The question once asked, or the reply that refused it.
  question?: FoundryQuestion;
  refused?: MockReply;
  // How it ended (empty while it runs), with its result JSON, outputs kept.
  status: string;
  resultJson: string;
  error: string;
};

const QUESTION_LIVE = ["queued", "running"];

/**
 * A question as helix_foundry_ask and helix_foundry_get_question give it
 * (integration.FoundryQuestionOutput): its result's queries without their
 * outputs, as lists give it.
 */
function questionOutput(q: FoundryQuestion) {
  const status = q.status || "queued";
  return {
    content_trust: "untrusted",
    id: q.id,
    thread_id: q.threadId || q.id,
    load_id: q.loadId || "",
    question: q.question || "",
    status,
    done: !QUESTION_LIVE.includes(status),
    ...(q.resultJson && {
      result: JSON.parse(listed(q.resultJson)) as unknown,
    }),
    ...(q.error && { error: q.error }),
    attempts: q.attempts ?? 0,
    usd_micros: Number(q.usdMicros || 0),
    created_at: q.createdAt || "",
    ...(q.finishedAt && { finished_at: q.finishedAt }),
  };
}

/** The question a tool_result names, whatever its output's size. */
const questionRef = (ontologyId: string, q: FoundryQuestion) => ({
  ontology_id: ontologyId,
  id: q.id,
  ...(q.loadId && { load_id: q.loadId }),
  thread_id: q.threadId || q.id,
  ...(q.status && { status: q.status }),
});

/**
 * What Founder says when the Analyst refused its question: a limit the
 * Analyst keeps (30 questions an hour, the month's question budget; a
 * grantee's own 10 an hour and month's $10) is the limit on questions,
 * said in those words, not Founder's own. People never read the name
 * Analyst: Founder's answers from the data are its own.
 */
function askRefusal(r: MockReply, o: StoredOntology, who: Who) {
  const message = why(r).replace(/\.$/, "");
  if (who === "grantee" && (r.status === 429 || /\bmonth\b/.test(message)))
    return `Your question was refused: ${message}. That's the limit on your questions, not on me.`;
  if (r.status === 429)
    return "This workspace has asked its 30 questions of the data this hour: that limit is on questions, not on me. Ask again later.";
  if (/\bmonth\b/.test(message))
    return "This workspace has used the month's budget for questions of the data: that limit is on questions, not on me. It starts again next month.";
  if (/\bno load to ask\b/.test(message))
    return `${o.name} has nothing loaded to ask yet. Load it, and I can answer questions about it.`;
  return `I couldn't ask that of the data: ${message}.`;
}

/**
 * Asks the Analyst the person's message about `o`'s current load, as the
 * person (helix_foundry_ask: the question is theirs, in the Analyst's
 * threads), then reads it with helix_foundry_get_question until it ends,
 * holding the turn while it runs (WAIT_MS at most). Both results name the
 * question (`question`), which the dock shows as the Analyst answers it.
 * What it came to is `asked`, for the steps after.
 */
function askSteps(a: Ask, o: StoredOntology): { steps: Step[]; asked: Asked } {
  const asked: Asked = { status: "", resultJson: "", error: "" };
  const input = { workspace_id: a.ws, ontology_id: o.id };
  // Asked as the WFE's POST is, access check first: a grantee asks its
  // grants' load, under its own limits, and the question is its own.
  const ask = guarded({
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions",
    handle: askQuestion,
  });
  const steps: Step[] = [
    call("tu_ask", ASK, { ...input, question: a.text }),
    (c) => {
      const reply = ask.handle({
        method: "POST",
        path: `${ONT(a.ws, o.id)}/questions`,
        params: { workspaceId: a.ws, ontologyId: o.id },
        query: new URLSearchParams(),
        body: { question: a.text, clientToken: `agent-${c.turn.id}` },
        principal: { user: db.users.get(c.userId)!, sessionId: "agent" },
      }) as MockReply;
      if (reply.status !== 200) {
        asked.refused = reply;
        return {
          type: "tool_result",
          tool_use_id: "tu_ask",
          name: ASK,
          is_error: true,
          error: why(reply),
        };
      }
      const q = (reply.body as { question: FoundryQuestion }).question;
      asked.question = q;
      return {
        type: "tool_result",
        tool_use_id: "tu_ask",
        name: ASK,
        is_error: false,
        output: questionOutput(q),
        question: questionRef(o.id, q),
      };
    },
    (c) =>
      asked.question
        ? call("tu_question", GET_QUESTION, {
            ...input,
            question_id: asked.question.id,
          })(c)
        : null,
    (c) => {
      const q = asked.question;
      if (!q) return null;
      const stored = storedQuestion(a.ws, o.id, q.id);
      if (
        stored &&
        QUESTION_LIVE.includes(stored.status) &&
        Date.now() - (c.turn.heldSince ?? Date.now()) <= WAIT_MS
      )
        return undefined;
      const read = wfe(c.userId, "GET", `${ONT(a.ws, o.id)}/questions/${q.id}`);
      if (read.status !== 200)
        return {
          type: "tool_result",
          tool_use_id: "tu_question",
          name: GET_QUESTION,
          is_error: true,
          error: why(read),
        };
      const got = read.body.question as FoundryQuestion;
      Object.assign(asked, {
        status: QUESTION_LIVE.includes(got.status || "")
          ? ""
          : got.status || "",
        resultJson: got.resultJson || "",
        error: got.error || "",
      });
      return {
        type: "tool_result",
        tool_use_id: "tu_question",
        name: GET_QUESTION,
        is_error: false,
        output: questionOutput(got),
        question: questionRef(o.id, got),
      };
    },
  ];
  return { steps, asked };
}

/** Founder's words around the Analyst's answer: never the answer again. */
function aroundAnswer(asked: Asked, o: StoredOntology, who: Who) {
  if (asked.refused) return askRefusal(asked.refused, o, who);
  const from =
    who === "grantee"
      ? `what's shared with you of ${o.name}`
      : `the current load of ${o.name}`;
  switch (asked.status) {
    case "answered":
      return `That's the answer, from ${from}.`;
    case "cannot_answer":
      return `I couldn't answer that from ${from}: the reply says what's missing.`;
    case "failed":
      return `The question failed: ${asked.error || "it ended without an answer"}.`;
    case "cancelled":
      return "The question was cancelled before it was answered.";
    default:
      return "Still working on it: the answer shows here once it's done.";
  }
}

/**
 * Whether a message asks the data: about invoices, customers, accounts,
 * contracts or people, or a question that names one of the workspace's
 * ontologies ("What does Renewals hold? List its types.").
 */
const asksData = (ws: string, text: string) =>
  (/\b(which|how many|what|who|show)\b/i.test(text) &&
    /\b(invoices?|customers?|accounts?|contracts?|contacts?|people|compan(?:y|ies))\b/i.test(
      text,
    )) ||
  (/^\s*(which|what|who|how|when|where|are|is|do|does)\b[^]*\?/i.test(text) &&
    !!named(ws, text));

/** A question of the data: the Analyst's answer, and a sentence around it. */
function askData(a: Ask, o: StoredOntology, who: Who): Step[] {
  const { steps, asked } = askSteps(a, o);
  return [...steps, said(() => aroundAnswer(asked, o, who))];
}

/** What Founder says of a change a grantee asked for (granteePrompt's rule). */
const READ_ONLY =
  "Your access lets me read and ask about the data shared with you, not change anything in this workspace, so I can't do that. Someone who manages it can.";
/** What follows the Analyst's answer when a grantee asked for a dashboard. */
const MADE_NONE =
  " Your access doesn't let me make dashboards, so I made none.";

/**
 * A grantee's message, as the worker's ReachGrants runs it: the ontologies
 * its grants hold (helix_foundry_list_ontologies through the access check,
 * as they read them: no database), then the Analyst asked about the one
 * named, the page's (its page context keeps only what it holds) or the
 * first, whatever the message. A change it asks for, a draft's questions
 * included, Founder says it can't make; a dashboard asked for, it asks and
 * makes none. Nothing is prepared, made or asked of the person.
 */
function granteeScript(a: Ask): Step[] {
  const t = a.text;
  const read = wfe(a.userId, "GET", `${WS(a.ws)}/ontologies`);
  const shared: FoundryOntology[] = read.body?.ontologies || [];
  const list = [
    call("tu_list", LIST, {}),
    result("tu_list", LIST, () => ({ ontologies: snake(shared) })),
  ];
  const dashboard = askedForDashboard(t),
    // What it asks of a dashboard, on its page or naming one.
    change = a.page?.dashboardId || dashboard ? dashboardAsk(t) : undefined,
    intent = intentOf(a.ws, t, automationAsk(a.ws, t, false), change);
  if (intent === "relay_questions")
    return [
      started,
      ...list,
      said(
        () =>
          "You can't answer a draft's questions: an owner or admin answers them on the generation's page in Foundry.",
      ),
    ];
  if (intent && (change || intent !== "create_dashboard"))
    return [started, ...list, said(() => READ_ONLY)];
  const pick =
    shared.find((o) => says(t, o.name || "") || t.includes(o.id)) ??
    shared.find((o) => o.id === a.page?.ontologyId) ??
    shared[0];
  const o = pick && ontologyById(pick.id);
  if (!o)
    return [
      started,
      ...list,
      said(
        () =>
          "Nothing is shared with you yet, so there's nothing I can ask about.",
      ),
    ];
  const { steps, asked } = askSteps(a, o);
  return [
    started,
    ...list,
    ...steps,
    said(
      () => `${aroundAnswer(asked, o, "grantee")}${dashboard ? MADE_NONE : ""}`,
    ),
  ];
}

/**
 * A dashboard of what the data says: asked of the Analyst first (the
 * ontology a dashboard reads), then each table or chart of its answer made a
 * tile, as :add-question-visual makes one (the query it recorded, its kind
 * and title, the current load), one a row, in a dashboard made at once
 * (autoCreateDashboard). An answer that isn't one, or shows no table or
 * chart, makes nothing, and Founder says why.
 */
function askDashboard(a: Ask, who: Who): Step[] {
  const o = dashboardOntology(a);
  if (!o)
    return [
      said(
        () =>
          "This workspace has no ontology yet, so there is no data for a dashboard. Create one first.",
      ),
    ];
  const { steps, asked } = askSteps(a, o);
  return [
    ...steps,
    ...dashboardSteps(a, () => {
      if (asked.refused) return askRefusal(asked.refused, o, who);
      if (asked.status !== "answered")
        return `${aroundAnswer(asked, o, who).replace(/\.$/, "")}, so I made no dashboard.`;
      const r = JSON.parse(asked.resultJson || "{}") as {
        answer?: {
          visuals?: {
            evidence_id: string;
            kind?: string | null;
            title?: string | null;
          }[];
        };
        queries?: { evidence_id: string; query: unknown }[];
      };
      const tiles = (r.answer?.visuals || [])
        .flatMap((v) => {
          const recorded = r.queries?.find(
            (x) => x.evidence_id === v.evidence_id,
          );
          return recorded ? [{ v, query: recorded.query }] : [];
        })
        .map(({ v, query }, i) => {
          const visual: { kind?: string } =
            v.kind && VISUALS.includes(v.kind) ? { kind: v.kind } : {};
          return {
            id: `t${i + 1}`,
            title: v.title?.trim() || "Answer",
            ontology_id: o.id,
            load: { kind: "current" },
            query,
            visual,
          };
        });
      if (!tiles.length)
        return "The answer shows no table or chart to put on a dashboard, so I made none.";
      const name = tiles[0]!.title,
        description = `${o.name}: the answer, as tables and charts.`;
      return {
        operation: "create_dashboard",
        target: `workspace:${a.ws}`,
        payload: {
          name,
          description,
          spec: { v: 1, tiles, layout: { rows: tiles.map((t) => [t.id]) } },
        },
        display: {
          name,
          description,
          spec: {
            tiles: tiles.map((t) => ({
              title: t.title,
              ontology: ref(o),
              ...(t.visual.kind && { visual: t.visual.kind }),
            })),
          },
        },
        named: namedByPerson(a.threadId, [name]),
        ontologyId: "",
        said: "",
      };
    }),
  ];
}

type DashboardAsk =
  | { operation: "update_dashboard"; name: string }
  | { operation: "pin_dashboard"; pinned: boolean }
  | {
      operation:
        | "delete_dashboard"
        | "publish_dashboard"
        | "unpublish_dashboard";
    };

/** What the person asks of the dashboard they are on, if anything the agent prepares. */
function dashboardAsk(text: string): DashboardAsk | undefined {
  const rename = /\brename\b.*?\bto\s+["“]?([^"”]+?)["”]?\s*$/i.exec(text);
  if (rename) return { operation: "update_dashboard", name: rename[1]! };
  if (/\bunpublish\b/i.test(text)) return { operation: "unpublish_dashboard" };
  if (/\bpublish\b/i.test(text)) return { operation: "publish_dashboard" };
  if (/\bunpin\b/i.test(text))
    return { operation: "pin_dashboard", pinned: false };
  if (/\bpin\b/i.test(text))
    return { operation: "pin_dashboard", pinned: true };
  if (/\bdelete\b/i.test(text)) return { operation: "delete_dashboard" };
  return undefined;
}

/** Reads the dashboard the person is on, then prepares what they asked of it. */
function onDashboard(
  a: Ask,
  d: NonNullable<ReturnType<typeof storedDashboard>>,
  ask: DashboardAsk,
): Step[] {
  const read = [
    call("tu_dash", GET_DASHBOARD, { dashboard_id: d.id }),
    result("tu_dash", GET_DASHBOARD, () => ({
      id: d.id,
      name: d.name,
      revision: d.revision,
      pinned: d.pinned,
    })),
  ];
  if (ask.operation === "publish_dashboard" && !publicLinks())
    return [
      ...read,
      said(
        () =>
          "Public links aren't available here, so I can't publish this dashboard.",
      ),
    ];
  if (ask.operation === "unpublish_dashboard" && !d.published)
    return [
      ...read,
      said(() => `“${d.name}” isn't published, so there is no link to end.`),
    ];
  const dashboard = dref(d),
    target = `dashboard:${a.ws}/${d.id}`,
    named =
      namedByPerson(a.threadId, [d.name, d.id]) ||
      state.turns.some(
        (t) => t.threadId === a.threadId && t.pageContext?.dashboardId === d.id,
      ),
    run = " Run it from the card when you're ready.";
  const plan = (): Plan => {
    switch (ask.operation) {
      case "update_dashboard":
        return {
          operation: ask.operation,
          target,
          payload: { expected_revision: d.revision, name: ask.name },
          display: { dashboard, name: ask.name },
          named,
          ontologyId: "",
          said: `I've prepared renaming “${d.name}” to “${ask.name}”.${run}`,
        };
      case "pin_dashboard":
        return {
          operation: ask.operation,
          target,
          payload: { expected_revision: d.revision, pinned: ask.pinned },
          display: { dashboard, pinned: ask.pinned },
          named,
          ontologyId: "",
          said: `I've prepared ${ask.pinned ? "pinning" : "unpinning"} “${d.name}” for everyone.${run}`,
        };
      case "publish_dashboard":
        return {
          operation: ask.operation,
          target,
          payload: { expected_revision: d.revision },
          display: { dashboard },
          named,
          ontologyId: "",
          said: `I've prepared publishing “${d.name}” at a public link: anyone who has the link will see a snapshot of its data, without signing in.${run}`,
        };
      case "unpublish_dashboard":
      case "delete_dashboard":
        return {
          operation: ask.operation,
          target,
          payload: {},
          display: { dashboard },
          named,
          ontologyId: "",
          said:
            ask.operation === "delete_dashboard"
              ? `I've prepared deleting “${d.name}”.${run}`
              : `I've prepared unpublishing “${d.name}”: its link will show nothing.${run}`,
        };
    }
  };
  return [...read, ...prepare(plan)];
}

/** A dashboard as the operations give it back (integration.FoundryDashboardOutput). */
function dashboardOutput(ws: string, d: FoundryDashboard) {
  return {
    id: d.id,
    workspace_id: ws,
    name: d.name,
    description: d.description,
    revision: Number(d.revision),
    pinned: !!d.pinned,
    can_edit: !!d.canEdit,
    can_pin: !!d.canPin,
    can_publish: !!d.canPublish,
    can_delete: !!d.canDelete,
    can_unpublish: !!d.canUnpublish,
    // Never its link: a tool's output reaches the model and its transcripts.
    ...(d.share && {
      share: {
        published: true,
        spec_revision: Number(d.share.specRevision),
        published_at: d.share.publishedAt,
      },
    }),
  };
}

// ---------------------------------------------------------------- questions

const ASK_PERSON = "helix_foundry_ask_person",
  // What the tool tells the model once it asked (agent.askedThen).
  ASKED_THEN =
    "The person sees the questions now, one at a time. End your turn: write nothing more. Their answers come as the next message.";

/**
 * Asks the person through helix_foundry_ask_person and ends the turn
 * waiting for their answers (finished, reason asked), as the backend's
 * asker does: the call, the questions it shows, its result. `ask` decides
 * when the step comes: null asks nothing (the steps after it go on).
 */
function askPerson(
  input: (c: Context) => unknown,
  ask: (c: Context) => Extract<Scripted, { type: "questions" }> | null,
): Step[] {
  const asked = new Map<string, ReturnType<typeof ask>>();
  const of = (c: Context) => {
    if (!asked.has(c.turn.id)) asked.set(c.turn.id, ask(c));
    return asked.get(c.turn.id)!;
  };
  return [
    (c) => (of(c) ? call("tu_ask", ASK_PERSON, input(c))(c) : null),
    (c) => of(c),
    (c) =>
      of(c)
        ? result("tu_ask", ASK_PERSON, () => ({
            asked: of(c)!.questions.length,
            then: ASKED_THEN,
          }))(c)
        : null,
    (c) =>
      of(c) ? { type: "finished", status: "done", reason: "asked" } : null,
  ];
}

/** The questions a waiting generation of `o` asks, relayed as its agent wrote them. */
function relayed(
  o: StoredOntology,
  g: OntologyGeneration | undefined,
): Extract<Scripted, { type: "questions" }> | null {
  if (g?.status !== "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT") return null;
  return {
    type: "questions",
    questions: (g.questions || []).map(
      ({ items: _items, answer: _answer, ...q }) => q,
    ),
    generation: {
      ontology_id: o.id,
      ontology_name: o.name,
      generation_id: g.id,
      ...(g.title && { generation_title: g.title }),
      round: g.questionRound || 1,
    },
  };
}

/**
 * Relays the questions the newest generation of `o` waits on: read with
 * helix_foundry_get_generation, then asked (the person answers in the dock,
 * and the answers turn prepares them as a card); none waiting, it says so.
 */
function relayQuestions(a: Ask, o: StoredOntology): Step[] {
  let read: OntologyGeneration | undefined;
  const waiting = () => {
    if (read) return read;
    const g = (
      wfe(a.userId, "GET", `${ONT(a.ws, o.id)}/generations`).body
        ?.generations as OntologyGeneration[] | undefined
    )?.find((x) => x.status === "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT");
    // The list leaves the questions out: read the generation itself.
    return (read =
      g &&
      (wfe(a.userId, "GET", `${ONT(a.ws, o.id)}/generations/${g.id}`).body
        ?.generation as OntologyGeneration | undefined));
  };
  return [
    call("tu_generation", GET_GENERATION, { ontology_id: o.id }),
    result("tu_generation", GET_GENERATION, () => ({
      generation: snake(waiting() ?? null),
    })),
    ...askPerson(
      () => ({
        generation: { ontology_id: o.id, generation_id: waiting()?.id },
      }),
      () => relayed(o, waiting()),
    ),
    said(() => `No draft of ${o.name} is waiting for answers.`),
  ];
}

/**
 * "Help me set up an ontology": the Assistant asks what it needs to know
 * first (its own questions: the goal, a choice of one; the data, a choice of
 * several from the workspace's connections; what to leave out, in words,
 * optional).
 */
function setupQuestions(a: Ask): Step[] {
  const connectors = workspaceConnectors(a.ws).slice(0, 6);
  const data =
    connectors.length >= 2
      ? connectors.map((c) => ({
          label: c.name || c.id,
          description: `${(c.kind || "").charAt(0).toUpperCase()}${(c.kind || "").slice(1)} connection`,
        }))
      : [
          {
            label: "Connected databases and apps",
            description: "What Data syncs.",
          },
          {
            label: "Uploaded documents",
            description: "Contracts, notes and other files.",
          },
        ];
  const questions: AgentQuestion[] = [
    {
      id: "q1",
      header: "Goal",
      text: "What should the new ontology answer first?",
      options: [
        {
          label: "Customer health",
          description: "Who pays, how much, and who is at risk of leaving.",
        },
        {
          label: "Revenue and renewals",
          description: "Invoices, contracts and when they renew.",
        },
        {
          label: "Support load",
          description: "Tickets, who raises them and how fast they close.",
        },
      ],
      multiple: false,
      recommended: "Customer health",
      blocking: true,
    },
    {
      id: "q2",
      header: "Data",
      text: "Which of your data should it read?",
      options: data,
      multiple: true,
      recommended: data[0]!.label,
      blocking: true,
    },
    {
      id: "q3",
      header: "Leave out",
      text: "Is there anything it should leave out?",
      options: [],
      multiple: false,
      recommended: "",
      blocking: false,
    },
  ];
  return [
    call("tu_connectors", CONNECTORS, { workspace_id: a.ws }),
    result("tu_connectors", CONNECTORS, () => ({
      connectors: workspaceConnectors(a.ws).map((c) => ({
        id: c.id,
        name: c.name,
        kind: c.kind,
      })),
    })),
    said(() => "A few questions first, so the draft fits what you need."),
    ...askPerson(
      () => ({
        questions: questions.map((q) => ({
          header: q.header,
          text: q.text,
          ...(q.options?.length && { options: q.options }),
          ...(q.multiple && { multiple: true }),
          ...(q.recommended && { recommended: q.recommended }),
          ...(!q.blocking && { optional: true }),
        })),
      }),
      () => ({ type: "questions", questions }),
    ),
  ];
}

/**
 * An answers turn: the person's answers to the questions `asking` asked.
 * A relayed generation's were sent to it as the person before the turn, so
 * it says so; the Assistant's own it reads back as the plan they make.
 */
function answersScript(
  a: Ask,
  asking: StoredTurn,
  answers: AgentQuestionAnswer[],
): Step[] {
  const asked = asking.asked!,
    g = asked.generation;
  const words = (q: AgentQuestion) => {
    const x = answers.find((y) => y.questionId === q.id);
    return x ? answerWords(x) : "";
  };
  // The time zone the mock asked for: the automation the asking turn's
  // message described, in the zone the label names.
  if (!g && asked.questions?.[0]?.header === ZONE_HEADER) {
    const label = answers[0]?.options?.[0] ?? "";
    const zone =
      ZONE_CHOICES.find((z) => z.label === label)?.zone ??
      (validZone(answers[0]?.text) ? answers[0]!.text! : "UTC");
    return [
      started,
      ...automationCreate(
        { ...a, text: asking.text, page: asking.pageContext },
        zone,
      ),
    ];
  }
  if (!g) {
    const [goal, data, leave] = (asked.questions || []).map(words);
    return [
      started,
      said(
        () =>
          `Here's what I'll set up: an ontology for ${goal || "what you asked"}, reading ${data || "your data"}${leave ? `, leaving out ${leave}` : ""}. Tell me its name and database, and I'll prepare it: for example, "create an ontology called Customer health on database orders".`,
      ),
    ];
  }
  // Foundry sent them to the draft before this turn: nothing to prepare.
  return [
    started,
    said(
      () =>
        `Your answers went to the draft${answers.length < (asked.questions || []).length ? ", and it decides the ones you left itself" : ""}: it carries on from them, and its page shows how it goes.`,
    ),
  ];
}

// ---------------------------------------------------------------- automations

type AutomationAsk =
  | { kind: "create_automation" }
  | {
      kind:
        | "pause_automation"
        | "resume_automation"
        | "delete_automation"
        | "run_automation";
      automation: ReturnType<typeof workspaceAutomations>[number];
    };

/** What the person asks about automations, if anything: a new one, or an action on one they name. */
function automationAsk(
  ws: string,
  text: string,
  creating: boolean,
): AutomationAsk | undefined {
  const named = workspaceAutomations(ws).find((x) => says(text, x.name));
  const op = /\b(pause|resume|delete|run)\b/i.exec(text)?.[1]?.toLowerCase();
  if (named && op)
    return {
      kind: `${op as "pause" | "resume" | "delete" | "run"}_automation`,
      automation: named,
    };
  // On the page that starts one, whatever the person writes describes it;
  // elsewhere a schedule ("Every night at 2am, …") or an event it starts on
  // ("When new documents arrive, …") does.
  return creating ||
    /\bautomat(e|ion)\b/i.test(text) ||
    /\bevery\s+(hour|day|morning|evening|night|weekday|weekend|week|month|quarter|monday|tuesday|wednesday|thursday|friday|saturday|sunday)\b/i.test(
      text,
    ) ||
    /^\s*(when|whenever)\b[^.?!]*\b(arrives?|(?:are|is|gets?) uploaded|(?:are|is|gets?) added|(?:are|is) ready|finish(?:es)?|ends?|fails?|succeeds?)\b/i.test(
      text,
    )
    ? { kind: "create_automation" }
    : undefined;
}

/** An automation as a card names it. */
const aref = (x: ReturnType<typeof workspaceAutomations>[number]) => ({
  id: x.id,
  name: x.name,
  status: x.status,
});

/**
 * The time zones the mock offers when it has to ask for one: labels in
 * words, as the prompt has the Assistant write them, and the IANA id it
 * writes into the spec itself.
 */
const ZONE_CHOICES: readonly { label: string; zone: string }[] = [
  { label: "London (UK time)", zone: "Europe/London" },
  { label: "New York (US Eastern)", zone: "America/New_York" },
  { label: "Los Angeles (US Pacific)", zone: "America/Los_Angeles" },
  { label: "Berlin (Central Europe)", zone: "Europe/Berlin" },
];

/** The question the mock asks when no time zone came with the message. */
const ZONE_HEADER = "Time zone";

/**
 * Sets up what the person asked for as an automation's spec: a schedule
 * in the person's time zone (the page context's, as the browser sent it;
 * `zone` once they chose one), or, when documents are uploaded, their
 * arrival in the workspace's first set of its own; a sync of the
 * connection named (else the first, when asked to sync), the Assistant's
 * summary of what they asked about, an instruction naming the ontology it
 * reads by name and never by id, and an email; then prepares
 * create_automation. A day's or a week's schedule with no zone known asks
 * for one first, its options in words.
 */
function automationCreate(a: Ask, chosen?: string): Step[] {
  const zone = chosen || a.page?.timeZone || "";
  const text = a.text,
    connectors = workspaceConnectors(a.ws).filter((c) => c.kind !== "events"),
    connector =
      connectors.find((c) => says(text, c.name || c.kind)) ??
      (/\bsync\b/i.test(text) ? connectors[0] : undefined);
  const clock = /\bat\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?/i.exec(text);
  const hour =
    ((Number(clock?.[1] ?? 9) % 12) +
      (clock?.[3]?.toLowerCase() === "pm" ||
      (!clock?.[3] && Number(clock?.[1] ?? 9) === 12)
        ? 12
        : 0)) %
    24;
  const at = `${String(hour).padStart(2, "0")}:${clock?.[2] ?? "00"}`,
    weekday = WEEKDAYS.find((d) => new RegExp(`\\b${d}\\b`, "i").test(text));
  // "When new contracts are uploaded": the workspace's first set of its own.
  const uploaded = /\bwhen\b[^.]*\b(uploaded|arrives?)\b/i.test(text)
    ? sets(a.ws, a.userId).find((s) => !s.connector)
    : undefined;
  const hourly = /\bevery\s+hour\b/i.test(text);
  if (!uploaded && !hourly && !zone) {
    const when =
      weekday || /\bevery\s+week\b/i.test(text)
        ? `${(weekday ?? "monday").replace(/^./, (c) => c.toUpperCase())} `
        : "";
    const questions: AgentQuestion[] = [
      {
        id: "q1",
        header: ZONE_HEADER,
        text: `Which time zone should the ${when}${at} run keep?`,
        options: ZONE_CHOICES.map((z) => ({ label: z.label, description: "" })),
        multiple: false,
        recommended: "",
        blocking: true,
      },
    ];
    return askPerson(
      () => ({
        questions: questions.map((q) => ({
          header: q.header,
          text: q.text,
          options: q.options,
        })),
      }),
      () => ({ type: "questions", questions }),
    );
  }
  const time_zone = zone || "UTC";
  const trigger = uploaded
    ? ({ kind: "document_arrived", set_id: uploaded.id } as const)
    : hourly
      ? ({ kind: "schedule", every: "hour", minute: 0, time_zone } as const)
      : weekday || /\bevery\s+week\b/i.test(text)
        ? ({
            kind: "schedule",
            every: "week",
            weekday: (weekday ?? "monday") as Weekday,
            at,
            time_zone,
          } as const)
        : ({ kind: "schedule", every: "day", at, time_zone } as const);
  const about = /\b(?:summary|report)\s+of\s+(.+?)(?:[.!?]|$)/i.exec(text)?.[1];
  const nodes: AutomationSpec["nodes"] = [
    { id: "start", kind: "trigger", config: trigger, position: { x: 0, y: 0 } },
  ];
  const add = (node: Omit<AutomationSpec["nodes"][number], "position">) =>
    nodes.push({
      ...node,
      position: { x: 0, y: nodes.length * 150 },
    } as AutomationSpec["nodes"][number]);
  if (connector && /\bsync\b/i.test(text))
    add({
      id: "sync",
      kind: "action",
      config: { operation: "sync_connector", connector_id: connector.id },
    });
  // The ontology the person named, by its name: people read and edit the
  // instruction, and the step finds it by name when it runs.
  const reads = named(a.ws, text);
  add({
    id: "summarise",
    kind: "assistant",
    config: {
      instruction:
        (reads ? `Read the current load of the ${reads.name} ontology. ` : "") +
        (about
          ? `Summarise ${about.trim()}, in a few plain sentences.`
          : `Do what was asked: ${text.trim()}`),
    },
  });
  if (/\b(e-?mails?|mail)\b/i.test(text))
    add({ id: "email", kind: "notify", config: { message: "" } });
  const spec: AutomationSpec = {
    v: 1,
    nodes,
    edges: nodes.slice(1).map((n, i) => ({ from: nodes[i]!.id, to: n.id })),
  };
  const name =
    /\bcalled\s+["“]?([^"”]+?)["”]?(?:[.,]|$)/i.exec(text)?.[1] ||
    `${trigger.kind === "document_arrived" ? "Document" : trigger.every === "week" ? "Weekly" : trigger.every === "day" ? "Daily" : "Hourly"} ${about ? "summary" : "check"}`;
  const payload = { name, description: "", spec };
  const words = automationSentence(
    spec,
    {
      connector: (id) => connectionName(id),
      ontology: () => undefined,
      set: (id) => (id === uploaded?.id ? uploaded.name : undefined),
      dashboard: () => undefined,
    },
    !LOCAL_TOKEN,
  );
  return [
    call("tu_connectors", CONNECTORS, { workspace_id: a.ws }),
    result("tu_connectors", CONNECTORS, () => ({
      connectors: connectors.map((c) => ({
        id: c.id,
        name: c.name,
        kind: c.kind,
      })),
    })),
    ...prepare(() => ({
      operation: "create_automation",
      target: `workspace:${a.ws}`,
      payload,
      display: { name },
      named: namedByPerson(a.threadId, [name]),
      ontologyId: "",
      said: `I've prepared “${name}”: ${words} Nothing runs until you click Create and turn on, which approves these steps to run as you.`,
    })),
  ];
}

/** Reads the automation the person named, then prepares what they asked of it. */
function automationOperation(
  a: Ask,
  operation:
    | "pause_automation"
    | "resume_automation"
    | "delete_automation"
    | "run_automation",
  x: ReturnType<typeof workspaceAutomations>[number],
): Step[] {
  const words = {
    run_automation: `I've prepared running “${x.name}” now, once, as you.`,
    pause_automation: `I've prepared pausing “${x.name}”: it won't run when it fires until it is resumed.`,
    resume_automation: `I've prepared resuming “${x.name}”: it will run whenever it fires again.`,
    delete_automation: `I've prepared deleting “${x.name}” and its runs.`,
  };
  return [
    call("tu_automation", GET_AUTOMATION, { automation_id: x.id }),
    result("tu_automation", GET_AUTOMATION, () => ({
      automation: {
        id: x.id,
        name: x.name,
        status: x.status,
        revision: x.revision,
      },
    })),
    ...prepare(
      () =>
        ({
          operation,
          target: `automation:${a.ws}/${x.id}`,
          payload: {},
          display: { automation: aref(x) },
          named: namedByPerson(a.threadId, [x.name, x.id]),
          ontologyId: "",
          said: `${words[operation]} Run it from the card when you're ready.`,
        }) as Plan,
    ),
  ];
}

/**
 * An automation's Assistant step, as its turn runs: it reads the
 * ontologies; asked to load one (the one named, else the first that can),
 * it prepares the load, which runs at once as the owner when the step's
 * `allow` approves it (stored executing, decided by its owner), and waits
 * as a card otherwise; then it reports.
 */
function automationScript(
  ws: string,
  threadId: string,
  instruction: string,
  allow: AutomationAction[],
  use: AutomationUse,
): Step[] {
  const ontologies = workspaceOntologies(ws);
  const list = [
    call("tu_list", LIST, {}),
    result("tu_list", LIST, () => ({
      ontologies: ontologies.map(ontologyOutput),
    })),
  ];
  const o = /\b(re)?load\b/i.test(instruction)
    ? (named(ws, instruction) ?? ontologies.find((x) => loadable(x.id)))
    : undefined;
  const v = o && loadable(o.id);
  const steps: Step[] = [started, ...list];
  const did = new Map<string, string>();
  // An ontology its instruction names, loaded, it reads: a count of the
  // entities of a type of it, recorded under its run's use of it.
  const reads = named(ws, instruction);
  const readLoadId = reads && currentLoadId(reads.id);
  // The type its instruction names (invoices: Invoice), else the first.
  const types = readLoadId
    ? Object.keys(
        decisionTypes(
          ontologyVersion(
            reads.id,
            ontologyLoad(reads.id, readLoadId)?.versionId || "",
          )?.definition as Record<string, unknown> | undefined,
        ),
      )
    : [];
  // (The ontology's own name aside: "Customer contracts" names no Contract.)
  const asked = reads
    ? instruction.replace(
        new RegExp(reads.name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi"),
        " ",
      )
    : "";
  const readType = types.find((t) => says(asked, t)) ?? types[0];
  if (reads && readLoadId && readType) {
    const readsJson = JSON.stringify([
      { query: { start: { type: readType }, return: { count: {} } } },
    ]);
    let counted: number | undefined;
    steps.push(
      call("tu_read", READ, {
        ontology_id: reads.id,
        reads: JSON.parse(readsJson),
      }),
      (c) => {
        const record = use(reads.id);
        const reply = readLoad(
          {
            method: "POST",
            path: "",
            params: {
              workspaceId: ws,
              ontologyId: reads.id,
              loadId: readLoadId,
            },
            query: new URLSearchParams(),
            body: { readsJson },
            principal: { user: db.users.get(c.userId)!, sessionId: "agent" },
          },
          readsJson,
        );
        const resultJson =
          reply.status === 200
            ? String((reply.body as { resultJson?: string }).resultJson || "")
            : "";
        if (resultJson) {
          record?.({ read: { loadId: readLoadId, readsJson, resultJson } });
          const first = (
            JSON.parse(resultJson) as { results?: { ok?: unknown }[] }
          ).results?.[0]?.ok;
          const n = JSON.stringify(first ?? "").match(/\d+/)?.[0];
          if (n !== undefined) counted = Number(n);
        }
        return {
          type: "tool_result",
          tool_use_id: "tu_read",
          name: READ,
          is_error: !resultJson,
          ...(resultJson
            ? {
                output: {
                  content_trust: "untrusted",
                  ontology_id: reads.id,
                  load_id: readLoadId,
                  result: JSON.parse(resultJson),
                },
              }
            : { error: why(reply) }),
        };
      },
      (c) => {
        if (counted !== undefined)
          did.set(
            `read:${c.turn.id}`,
            `${reads.name} holds ${counted} ${readType}${counted === 1 ? "" : "s"}.`,
          );
        return null;
      },
    );
  }
  if (o && v) {
    const plan = onOntology(
      ws,
      o,
      false,
      "create_load",
      { version_id: v.id },
      { ontology: ref(o), version: vref(o.id, v.id) },
      "",
    );
    // Preparing a load of it uses it, whether it runs or waits.
    steps.push(() => {
      use(o.id);
      return null;
    });
    if (
      allow.some((x) => x.operation === "create_load" && x.ontology_id === o.id)
    ) {
      // Approved: stored executing, decided by its owner, run at once.
      let ran: StoredAction | undefined;
      steps.push(
        call("tu_prepare", PREPARE, {
          operation: plan.operation,
          target: plan.target,
          payload: plan.payload,
        }),
        (c) => {
          const x: StoredAction = {
            ...plan,
            id: id("act"),
            turnId: c.turn.id,
            threadId,
            workspaceId: ws,
            userId: c.userId,
            payloadHash: hash(plan),
            status: "executing",
            result: null,
            error: "",
            expiresAt: Date.now() + ACTION_TTL_MS,
            decidedByUserId: c.userId,
            decidedAt: now(),
            createdAt: now(),
          };
          state.actions.push(x);
          ran = x;
          const reply = run(x, c.userId);
          if (reply.status === 200)
            Object.assign(x, {
              status: "succeeded",
              result: {
                content_trust: "untrusted",
                operation: x.operation,
                target: x.target,
                result: output(x, reply.body),
              },
            });
          else Object.assign(x, { status: "failed", error: why(reply) });
          did.set(
            c.turn.id,
            x.status === "succeeded"
              ? `Started a load of v${v.version} of ${o.name}.`
              : `The load of ${o.name} didn't start: ${x.error}.`,
          );
          return {
            type: "action_result",
            action_id: x.id,
            operation: x.operation,
            status: x.status,
            ...(x.result && { result: x.result }),
            ...(x.error && { error: x.error }),
          };
        },
        () => ({
          type: "tool_result",
          tool_use_id: "tu_prepare",
          name: PREPARE,
          is_error: false,
          output: {
            action_id: ran!.id,
            status: "ran",
            outcome: ran!.status,
            ...(ran!.error && { error: ran!.error }),
          },
        }),
      );
    } else
      steps.push(
        // The prepare, its card and its result, without the agent's words.
        ...prepare(() => plan).slice(0, 3),
        (c) => {
          did.set(
            c.turn.id,
            state.actions.some((x) => x.turnId === c.turn.id)
              ? `A load of v${v.version} of ${o.name} waits for you to run it.`
              : `I couldn't prepare a load of ${o.name}.`,
          );
          return null;
        },
      );
  }
  steps.push((c) => {
    const answer = /\byes or no\b|\?/i.test(instruction)
      ? (knobs.automationAnswer ?? "yes") === "yes"
      : undefined;
    const count = /\bhow many\b/i.test(instruction)
      ? (knobs.automationCount ?? ontologies.length)
      : undefined;
    const summary = [
      ontologies.length
        ? `The workspace has ${ontologies.length} ontolog${ontologies.length === 1 ? "y" : "ies"}: ${ontologies.map((x) => x.name).join(", ")}.`
        : "The workspace has no ontology yet.",
      did.get(`read:${c.turn.id}`) ?? "",
      did.get(c.turn.id) ?? "",
      answer === undefined
        ? ""
        : answer
          ? "The answer is yes."
          : "The answer is no.",
    ]
      .filter(Boolean)
      .join(" ");
    return {
      type: "automation_report",
      summary,
      ...(answer !== undefined && { answer }),
      ...(count !== undefined && { count }),
    };
  });
  return steps;
}

/**
 * A use of an ontology by an automation's run (./automations.ts): what
 * records a call under the workflow run of it, or undefined where the use
 * goes untracked. Foundry gives it; the script never names a run.
 */
export type AutomationUse = (
  ontologyId: string,
) => ((call: WorkflowCall) => void) | undefined;

/**
 * Queues an automation's Assistant step as a turn of kind automation in its
 * run's thread (a new one, out of the person's list, for its first step),
 * as its owner, on Foundry's worker: the backend's
 * StartFoundryAutomationAssistant. An owner restricted to source grants
 * since fails it for good, as the worker's ReachGrants does, and so does a
 * setting whose tool check failed, as it fails any turn.
 */
export function queueAutomationTurn(p: {
  workspaceId: string;
  userId: string;
  threadId: string;
  title: string;
  instruction: string;
  allow: AutomationAction[];
  use: AutomationUse;
}): { threadId: string; turnId: string } {
  let thread = state.threads.find((t) => t.id === p.threadId);
  if (!thread) {
    thread = {
      id: id("thr"),
      workspaceId: p.workspaceId,
      userId: p.userId,
      kind: "automation",
      title: [...p.title].slice(0, TITLE_CHARS).join(""),
      runtime: "api",
      granted: null,
      createdAt: now(),
      updatedAt: now(),
      deletedAt: null,
    };
    state.threads.push(thread);
  }
  const turn = queueTurn(thread, {
    kind: "automation",
    text: "",
    actionId: "",
    pageContext: null,
    clientToken: "",
    script: [],
  });
  const unusable = restrictedMember(p.workspaceId, p.userId)
    ? "The automation's owner now reads only through source grants; an owner or admin can take it over"
    : aiAssistantFailure(p.workspaceId);
  turn.script = unusable
    ? [
        () => ({
          type: "finished",
          status: "failed",
          reason: "failed",
          error: unusable,
        }),
      ]
    : ending(
        automationScript(
          p.workspaceId,
          thread.id,
          p.instruction,
          p.allow,
          p.use,
        ),
      );
  return { threadId: thread.id, turnId: turn.id };
}

/**
 * An automation's step's turn as the worker reads it: still live, or how it
 * ended, with its newest report, its last words, its spend and the actions
 * it left waiting for the owner; undefined once gone.
 */
export function automationTurn(turnId: string):
  | { live: true }
  | {
      live: false;
      status: FoundryAgentTurnStatus;
      error: string;
      usdMicros: number;
      report?: { summary: string; answer?: boolean; count?: number };
      said: string;
      pending: number;
    }
  | undefined {
  const t = state.turns.find((x) => x.id === turnId);
  if (!t) return undefined;
  advance(t);
  if (live(t)) return { live: true };
  const events = t.events.map((e) => JSON.parse(e.eventJson) as AgentEvent);
  const report = events.findLast((e) => e.type === "automation_report"),
    text = events.findLast((e) => e.type === "text");
  return {
    live: false,
    status: t.status,
    error: t.error,
    usdMicros: t.usdMicros,
    ...(report?.type === "automation_report" && {
      report: {
        summary: report.summary,
        ...(report.answer !== undefined && { answer: report.answer }),
        ...(report.count !== undefined && { count: report.count }),
      },
    }),
    said: text?.type === "text" ? text.text.trim() : "",
    pending: state.actions.filter(
      (a) => a.turnId === t.id && shown(a) === "prepared",
    ).length,
  };
}

/**
 * The actions these turns prepared that still wait for their person, as the
 * backend counts them for the automations' list: stored as prepared (one
 * whose confirmation ran out still waits for a Refresh).
 */
export const waitingActions = (turnIds: string[]) =>
  state.actions.filter(
    (a) => turnIds.includes(a.turnId) && a.status === "prepared",
  ).length;

/** Cancels an automation's steps' live turns: its run was cancelled or deleted. */
export function cancelAutomationTurns(turnIds: string[]) {
  for (const t of state.turns)
    if (turnIds.includes(t.id)) {
      advance(t);
      if (live(t))
        Object.assign(t, {
          status: "cancelled",
          error: "the run was cancelled",
          finishedAt: now(),
        });
    }
}

/** An automation as the operations give it back (integration.FoundryAutomationOutput). */
const automationOutput = (ws: string, x: FoundryAutomation) => ({
  id: x.id,
  workspace_id: ws,
  name: x.name,
  description: x.description,
  revision: Number(x.revision),
  owner_user_id: x.ownerUserId,
  status: x.status,
  ...(x.pausedReason && { paused_reason: x.pausedReason }),
  trigger_kind: x.triggerKind,
  ...(x.nextRunAt && { next_run_at: x.nextRunAt }),
  failures: x.failures ?? 0,
  can_edit: !!x.canEdit,
  can_pause: !!x.canPause,
  can_resume: !!x.canResume,
  can_run: !!x.canRun,
  can_delete: !!x.canDelete,
});

/**
 * Approves the automation a create or an edit the person just ran saved,
 * as the backend's approveOnClick: their click on a card that showed steps
 * approves exactly those. An edit without steps approves nothing. Saved but
 * not approved, the action still succeeded, and says why.
 */
function approveOnClick(
  a: StoredAction,
  userId: string,
  body: { automation?: FoundryAutomation },
): { body: unknown; error: string } {
  if (
    (a.operation !== "create_automation" &&
      a.operation !== "update_automation") ||
    a.payload.spec === undefined
  )
    return { body, error: "" };
  const made = body.automation!;
  const approved = wfe(
    userId,
    "POST",
    `${WS(a.workspaceId)}/automations/${made.id}:approve`,
    { expectedRevision: made.revision },
  );
  return approved.status === 200
    ? { body: approved.body, error: "" }
    : {
        body,
        error: `it was saved, but not approved (${why(approved)}): approve it on its page in Foundry`,
      };
}

// ---------------------------------------------------------------- results

/** How the agent goes on from an action its person ran. */
function continuationScript(a: StoredAction, ask: Ask): Step[] {
  const out = (a.result?.result || {}) as Record<string, any>;
  if (a.status !== "succeeded")
    return [
      started,
      said(
        () =>
          `That didn't run: ${a.error || "Foundry gave no reason"}. Ask me to prepare it again once that's fixed.`,
      ),
    ];
  const o = ontologyById(a.ontologyId);
  switch (a.operation) {
    case "create_ontology": {
      const made = ontologyById(String(out.id || ""));
      if (!made) return [started, said(() => "The ontology was created.")];
      return [
        started,
        call("tu_get", GET, { ontology_id: made.id }),
        result("tu_get", GET, () => ontologyOutput(made)),
        ...prepare(() =>
          generatePlan(
            { ...ask, text: "" },
            made,
            true,
            goalOf(firstMessage(a.threadId)),
          ),
        ),
      ];
    }
    case "generate_ontology": {
      const g = String(out.id || "");
      let read: OntologyGeneration | undefined;
      const generation = () =>
        (read = wfe(
          a.userId,
          "GET",
          `${ONT(a.workspaceId, a.ontologyId)}/generations/${g}`,
        ).body?.generation as OntologyGeneration | undefined);
      const ended = () =>
        ![
          "ONTOLOGY_GENERATION_STATUS_QUEUED",
          "ONTOLOGY_GENERATION_STATUS_RUNNING",
        ].includes(generation()?.status || "");
      return [
        started,
        call("tu_generation", GET_GENERATION, {
          ontology_id: a.ontologyId,
          generation_id: g,
        }),
        // Read again until the draft ends, a minute at most.
        (c) =>
          ended() || Date.now() - (c.turn.heldSince ?? Date.now()) > WAIT_MS
            ? result("tu_generation", GET_GENERATION, () => ({
                generation: snake(read),
              }))(c)
            : undefined,
        // A draft that waits on its agent's questions: asked in the dock.
        ...(o
          ? askPerson(
              () => ({
                generation: { ontology_id: o.id, generation_id: g },
              }),
              () => relayed(o, read),
            )
          : []),
        ...prepare(() => {
          const v = read?.resultVersionId;
          if (!o) return "The ontology is gone.";
          if (read?.status === "ONTOLOGY_GENERATION_STATUS_FAILED")
            return `The draft failed: ${read.error || "the agent gave no reason"}. Ask me to try again.`;
          if (!v)
            return `The draft of ${o.name} is still being written. Ask me to make it current once it's done: /ontology/${o.id}/generations/${g}`;
          return currentPlan(a.workspaceId, o, true, v);
        }),
      ];
    }
    case "set_current_version": {
      const v = String(out.id || "");
      return [
        started,
        ...prepare(() =>
          o && loads(ontologyVersion(o.id, v) || { definition: {} })
            ? loadPlan(a.workspaceId, o, true, v)
            : `v${versionNumber(a.ontologyId, v)} is current now.`,
        ),
      ];
    }
    case "create_load":
    case "set_current_load": {
      const loadId = String(out.id || "");
      return [
        started,
        call("tu_load", GET_LOAD, {
          ontology_id: a.ontologyId,
          load_id: loadId,
        }),
        result("tu_load", GET_LOAD, () => ({
          load: loadOutput(a.ontologyId, loadId),
        })),
        said(() => {
          const l = loadOutput(a.ontologyId, loadId),
            name = o?.name || "the ontology";
          return a.operation === "set_current_load"
            ? `The ${l?.version.replace("foundry-", "")} load is ${name}'s current load now: /ontology/${a.ontologyId}/loads/${loadId}`
            : `The load of v${a.display.version.number} into ${name} is ${l?.status === "ready" ? "ready" : l?.status === "failed" ? "failed" : "writing"}. Follow it on its page: /ontology/${a.ontologyId}/loads/${loadId}`;
        }),
      ];
    }
    case "sync_connector": {
      const c = a.display.connector;
      return [
        started,
        call("tu_sync", GET_SYNC, {
          workspace_id: a.workspaceId,
          sync_id: out.id,
        }),
        result("tu_sync", GET_SYNC, () => ({ sync: out })),
        said(
          () =>
            `The sync of ${c.name || "the connection"} started. Its page shows when it ends: /data/connections/${c.id}`,
        ),
      ];
    }
    case "revise_version":
      return [
        started,
        said(
          () =>
            `The ${a.display.dry_run ? "preview" : "revision"} is running: /ontology/${a.ontologyId}/generations/${out.id}`,
        ),
      ];
    case "add_sample_data":
      return [
        started,
        said(
          () =>
            "The sample company's connection syncs now, and its set is ready. Ask me to draft from them once the sync ends.",
        ),
      ];
    case "create_document_set":
      return [
        started,
        said(
          () =>
            `The set “${a.display.name}” is made. Upload documents into it from /data`,
        ),
      ];
    case "update_document_set":
      return [started, said(() => "Saved.")];
    case "move_document":
      return [
        started,
        said(
          () =>
            `${a.display.document.name || "The document"} is in ${a.display.to.name || "the set"} now.`,
        ),
      ];
    case "settle_review":
      return [
        started,
        said(() =>
          a.display.decision.kind === "approve"
            ? "The statement is in the current load now."
            : "The decision is saved. It applies from the next load: ask me to load again when you're ready.",
        ),
      ];
    case "set_assistant_runtime":
      return [
        started,
        said(() =>
          a.display.runtime === "api"
            ? "New threads run on the workspace’s AI model now. This thread keeps running where it started."
            : `New threads run on ${RUNTIME_ON[a.display.runtime as "claude_code" | "codex"]} now${(out.online as string[] | undefined)?.includes(a.display.runtime) ? "" : ", once its helper runs on your computer"}. This thread keeps running where it started.`,
        ),
      ];
    case "revoke_assistant_helper":
      return [
        started,
        said(
          () =>
            `${a.display.host.name || "The helper"} is revoked: it runs no more turns. Pair it again from Settings › AI models to run more.`,
        ),
      ];
    case "create_automation":
    case "update_automation": {
      const name = String(
          out.name ||
            ("name" in a.display && a.display.name) ||
            "the automation",
        ),
        link = `/automations/${String(out.id)}`,
        on = out.status === "active";
      const made = a.operation === "create_automation";
      return [
        started,
        said(() =>
          a.error
            ? `“${name}” is ${made ? "saved" : "changed"}, but ${a.error}: ${link}`
            : made
              ? `“${name}” is set up${on ? " and on" : ""}. Open it to see its runs, or run it now: ${link}`
              : `“${name}” is changed${on ? " and on" : ""}: ${link}`,
        ),
      ];
    }
    case "pause_automation":
    case "resume_automation":
    case "delete_automation":
    case "run_automation": {
      const name = String(
          out.name || a.display.automation.name || "the automation",
        ),
        link = `/automations/${a.display.automation.id}`;
      const words = {
        pause_automation: `“${name}” is paused: it runs only when it is run by hand. ${link}`,
        resume_automation: `“${name}” is on again: ${link}`,
        delete_automation: `“${name}” is deleted, with its runs.`,
        run_automation: `“${name}” is running now. Follow it: ${link}/runs/${String(out.id)}`,
      };
      return [started, said(() => words[a.operation])];
    }
    case "create_dashboard":
      return [
        started,
        said(
          () =>
            `Your dashboard “${out.name || a.display.name}” is made. Open it to see its tiles, then pin or publish it from there: /dashboards/${out.id}`,
        ),
      ];
    case "update_dashboard":
    case "delete_dashboard":
    case "pin_dashboard":
    case "publish_dashboard":
    case "unpublish_dashboard": {
      const name = String(
          out.name || a.display.dashboard.name || "the dashboard",
        ),
        link = `/dashboards/${a.display.dashboard.id}`;
      const words: Record<typeof a.operation, string> = {
        update_dashboard: `“${name}” is changed: ${link}`,
        delete_dashboard: `“${name}” is deleted.`,
        pin_dashboard: `“${name}” is ${out.pinned ? "pinned" : "unpinned"} for everyone: ${link}`,
        publish_dashboard: `“${name}” is published. Anyone who has its link sees the snapshot; the link is on its page: ${link}`,
        unpublish_dashboard: `“${name}” is unpublished: its link shows nothing now.`,
      };
      return [started, said(() => words[a.operation])];
    }
  }
}

const firstMessage = (threadId: string) =>
  state.turns.find((t) => t.threadId === threadId && t.kind === "message")
    ?.text || "";

/** The sample's database, as the sample's page connects it. */
/** Whether a connector reads the sample's database, as SampleDatabase.Holds tells. */
function isSample(c: Connector) {
  const db = c.config && "postgres" in c.config ? c.config.postgres : undefined;
  return (
    db?.host === SAMPLE_HOST &&
    db.database === "acme" &&
    db.user === "acme_reader"
  );
}

const SAMPLE_CONFIG = () => ({
  postgres: {
    host: SAMPLE_HOST,
    port: 5432,
    database: "acme",
    user: "acme_reader",
    password: "acme",
    ca: readFileSync(
      new URL("../../public/samples/rds-ca.pem", import.meta.url),
      "utf8",
    ),
  },
});

/** The action's REST call, as the person who clicked: the WFE's answer. */
function run(a: StoredAction, userId: string): MockReply {
  const ws = a.workspaceId,
    at = ONT(ws, a.ontologyId),
    p = a.payload as Record<string, any>;
  switch (a.operation) {
    case "create_ontology": {
      let tenantId = a.target.replace(/^tenant:/, "");
      if (p.database) {
        const t = wfe(userId, "POST", "/v1/tenants", {
          projectId: p.database.project_id,
          name: p.database.name,
          slug: p.database.slug,
        });
        if (t.status !== 200) return t;
        tenantId = t.body.tenant.id;
      }
      return wfe(userId, "POST", `${WS(ws)}/ontologies`, {
        tenantId,
        name: p.name,
        clientToken: a.id,
      });
    }
    case "generate_ontology":
      return wfe(userId, "POST", `${at}:generate`, {
        goal: p.goal || "",
        connectorIds: p.connector_ids || [],
        documentSetIds: p.document_set_ids || [],
        ...(p.no_connectors && { noConnectors: true }),
      });
    case "set_current_version":
      return wfe(userId, "POST", `${at}:set-current`, {
        versionId: p.version_id,
        expectedCurrentVersionId: p.expected_current_version_id,
      });
    case "create_load":
      return wfe(userId, "POST", `${at}/loads`, {
        versionId: p.version_id,
        clientToken: a.id,
      });
    case "set_current_load":
      return wfe(userId, "POST", `${at}/loads/${p.load_id}:set-current`, {});
    case "revise_version":
      return wfe(userId, "POST", `${at}/versions/${p.version_id}:revise`, {
        edits: (p.edits as { remove: { item_id: string } }[]).map((e) => ({
          remove: { itemId: e.remove.item_id },
        })),
        dryRun: !!p.dry_run,
      });
    case "sync_connector":
      return wfe(
        userId,
        "POST",
        `${WS(ws)}/connectors/${a.display.connector.id}:sync`,
        {},
      );
    case "add_sample_data": {
      const c = wfe(userId, "POST", `${WS(ws)}/connectors`, {
        name: "Acme Corp",
        config: SAMPLE_CONFIG(),
      });
      if (c.status !== 200) return c;
      const sync = wfe(
        userId,
        "POST",
        `${WS(ws)}/connectors/${c.body.connector.id}:sync`,
        {},
      );
      const set = wfe(userId, "POST", `${WS(ws)}/document-sets`, {
        name: "Acme Corp sample",
        description:
          "Acme Corp, a made-up field-service scheduling software company.",
      });
      return set.status !== 200
        ? set
        : ok({
            connectorId: c.body.connector.id,
            syncId: sync.body?.sync?.id || "",
            documentSetId: set.body.set.id,
            documents: [],
            next: "Draft from the sample's connection and set once its sync ends.",
          });
    }
    case "create_document_set":
      return wfe(userId, "POST", `${WS(ws)}/document-sets`, {
        name: p.name,
        ...(p.description && { description: p.description }),
      });
    case "update_document_set":
      return wfe(userId, "PATCH", `${WS(ws)}/document-sets/${p.set_id}`, {
        ...(p.name !== undefined && { name: p.name }),
        ...(p.description !== undefined && { description: p.description }),
      });
    case "move_document":
      return wfe(userId, "POST", `${WS(ws)}/documents/${p.document_id}:move`, {
        setId: p.set_id,
      });
    case "settle_review": {
      const d = p.decision;
      return wfe(userId, "POST", `${at}/reviews/${p.review_id}:settle`, {
        decision: d.resolve
          ? {
              resolve: d.resolve.not_in_data
                ? { notInData: true }
                : { entityKey: d.resolve.entity_key },
            }
          : d.prop
            ? { prop: d.prop.null ? { null: null } : d.prop }
            : d.release
              ? { release: { statementHash: d.release.statement_hash } }
              : { reject: { statementHash: d.reject.statement_hash } },
      });
    }
    case "set_assistant_runtime":
      return wfe(userId, "PATCH", `${WS(ws)}/agent/runtime`, {
        runtime: p.runtime,
      });
    case "revoke_assistant_helper":
      return wfe(
        userId,
        "POST",
        `${WS(ws)}/agent/hosts/${p.host_id}:revoke`,
        {},
      );
    case "create_automation":
      return wfe(userId, "POST", `${WS(ws)}/automations`, {
        name: p.name,
        description: p.description ?? "",
        ...(p.spec !== undefined && { specJson: JSON.stringify(p.spec) }),
        clientToken: a.id,
      });
    case "update_automation":
      return wfe(
        userId,
        "PATCH",
        `${WS(ws)}/automations/${a.display.automation.id}`,
        {
          expectedRevision: String(p.expected_revision),
          ...(p.name !== undefined && { name: p.name }),
          ...(p.description !== undefined && { description: p.description }),
          ...(p.spec !== undefined && { specJson: JSON.stringify(p.spec) }),
        },
      );
    case "pause_automation":
    case "resume_automation":
    case "delete_automation":
    case "run_automation": {
      const at = `${WS(ws)}/automations/${a.display.automation.id}`;
      return a.operation === "delete_automation"
        ? wfe(userId, "DELETE", at)
        : a.operation === "run_automation"
          ? wfe(userId, "POST", `${at}:run`, { clientToken: a.id })
          : wfe(
              userId,
              "POST",
              `${at}:${a.operation === "pause_automation" ? "pause" : "resume"}`,
              {},
            );
    }
    case "create_dashboard":
      return wfe(userId, "POST", `${WS(ws)}/dashboards`, {
        name: p.name,
        description: p.description ?? "",
        ...(p.spec !== undefined && { specJson: JSON.stringify(p.spec) }),
        clientToken: a.id,
      });
  }
  // The other dashboard operations act on the one their target names.
  const board = `${WS(ws)}/dashboards/${a.display.dashboard.id}`;
  switch (a.operation) {
    case "update_dashboard":
      return wfe(userId, "PATCH", board, {
        expectedRevision: String(p.expected_revision),
        ...(p.name !== undefined && { name: p.name }),
        ...(p.description !== undefined && { description: p.description }),
        ...(p.spec !== undefined && { specJson: JSON.stringify(p.spec) }),
      });
    case "delete_dashboard":
      return wfe(userId, "DELETE", board);
    case "pin_dashboard":
      return wfe(userId, "POST", `${board}:pin`, {
        expectedRevision: String(p.expected_revision),
        pinned: p.pinned,
      });
    case "publish_dashboard":
      return wfe(userId, "POST", `${board}:publish`, {
        expectedRevision: String(p.expected_revision),
      });
    case "unpublish_dashboard":
      return wfe(userId, "POST", `${board}:unpublish`, {});
  }
}

/** The WFE's answer as the operation's output: the one object it made or changed. */
function output(a: StoredAction, body: any) {
  switch (a.operation) {
    case "create_load":
    case "set_current_load":
      return loadOutput(a.ontologyId, body.load.id);
    case "move_document":
      return snake(body);
    case "add_sample_data":
      return snake(body);
    // As the backend's SetAssistantRuntimeOperation and assistantHost.
    case "set_assistant_runtime":
      return { runtime: body.runtime.runtime, online: body.runtime.online };
    case "delete_dashboard":
      return { deleted: true, dashboard_id: a.display.dashboard.id };
    case "create_automation":
    case "update_automation":
    case "pause_automation":
    case "resume_automation":
      return automationOutput(a.workspaceId, body.automation);
    case "delete_automation":
      return { deleted: true, automation_id: a.display.automation.id };
    case "run_automation": {
      const r = body.run as FoundryAutomationRun;
      return {
        id: r.id,
        automation_id: r.automationId,
        status: r.status,
        usd_micros: 0,
        created_at: r.createdAt,
      };
    }
    case "create_dashboard":
    case "update_dashboard":
    case "pin_dashboard":
    case "publish_dashboard":
    case "unpublish_dashboard":
      return dashboardOutput(a.workspaceId, body.dashboard);
    case "revoke_assistant_helper":
      return {
        id: body.host.id,
        name: body.host.name,
        runtimes: body.host.runtimes,
        last_seen_at: body.host.lastSeenAt,
        online: false,
      };
    default: {
      const [only] = Object.values(body as object);
      return snake(only);
    }
  }
}

function queueTurn(
  thread: StoredThread,
  turn: Pick<
    StoredTurn,
    "kind" | "text" | "actionId" | "pageContext" | "clientToken" | "script"
  > &
    Partial<Pick<StoredTurn, "answers">>,
  first?: AgentEvent,
) {
  const at = Date.now(),
    stepMs = knobs.agentStepMs ?? STEP_MS;
  const t: StoredTurn = {
    asked: null,
    answers: null,
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
    heldSince: null,
    stepMs,
    deltaMs: knobs.agentDeltaMs ?? stepMs / 4,
    next: at + 2 * stepMs,
    shown: 0,
    saying: null,
    seq: first ? 1 : 0,
    last: first?.type ?? "",
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
  const busyWhy = busy(thread);
  if (busyWhy) return refused(ws, "FOUNDRY_AGENT_THREAD_BUSY", busyWhy);
  Object.assign(a, {
    status: "executing",
    decidedByUserId: userId,
    decidedAt: now(),
  });
  // Authorized again as the person who clicked, as they are now: the
  // operation's own routes check them.
  const reply = run(a, userId);
  if (reply.status === 200) {
    const approval = approveOnClick(
      a,
      userId,
      reply.body as { automation?: FoundryAutomation },
    );
    a.status = "succeeded";
    a.result = {
      content_trust: "untrusted",
      operation: a.operation,
      target: a.target,
      result: output(a, approval.body),
    };
    a.error = approval.error;
  } else {
    a.status = "failed";
    a.error = why(reply);
  }
  // A helper's thread goes on only while a helper runs it (the backend's
  // agentContinues): a turn queued for none would only wait.
  if (thread.runtime !== "api" && hostOffline(ws, thread.runtime))
    return ok({ action: toAction(a) } satisfies AgentActionResponse);
  const continuation = queueTurn(
    thread,
    {
      kind: "action_result",
      text: "",
      actionId: a.id,
      pageContext: null,
      clientToken: "",
      script: [],
    },
    {
      type: "action_result",
      action_id: a.id,
      operation: a.operation,
      status: a.status,
      ...(a.result && { result: a.result }),
      ...(a.error && { error: a.error }),
    },
  );
  continuation.script = ending(
    continuationScript(a, {
      ws,
      userId,
      threadId: thread.id,
      text: "",
      page: null,
    }),
  );
  return ok({
    action: toAction(a),
    continuation: toTurn(continuation),
  } satisfies AgentActionResponse);
}

/** Prepares the action again as the person, as they are now: their role, and what it acts on. */
function refreshRefusal(a: StoredAction, userId: string) {
  const m = membership(a.workspaceId, userId);
  if (isReply(m) || (!manages(m) && !MEMBER_OPERATIONS.includes(a.operation)))
    return "permission denied";
  if (a.ontologyId && !ontologyById(a.ontologyId))
    return `this workspace holds no ontology ${a.ontologyId}: read its ontologies again`;
  if (a.operation === "settle_review") return settleRefusal(a);
  return undefined;
}

/**
 * The card drawn again, as the backend's Refresh draws it: its ontology as
 * named now, a review as the current load holds it, and the load current
 * now in place of the one current then.
 */
function redraw(a: StoredAction) {
  if ("automation" in a.display) {
    const id = a.display.automation.id,
      x = workspaceAutomations(a.workspaceId).find((y) => y.id === id);
    if (x) a.display.automation = aref(x);
  }
  if ("dashboard" in a.display) {
    const d = storedDashboard(a.workspaceId, a.display.dashboard.id);
    if (d) a.display.dashboard = dref(d);
  }
  const o = a.ontologyId ? ontologyById(a.ontologyId) : undefined;
  if (!o) return;
  if ("ontology" in a.display) a.display.ontology = ref(o);
  if (a.operation === "settle_review") {
    const r = reviewsOf(o).find(
      (x) => x.review_key === a.display.review.review_key,
    );
    if (r) a.display.review = r;
    a.display.load_id = currentLoadId(o.id) || a.display.load_id;
  }
  if (a.operation === "set_current_load") {
    const current = currentLoadId(o.id);
    if (current) a.display.current = lref(o.id, current);
    else delete a.display.current;
  }
}

export const agentRoutes: MockRoute[] = [
  {
    method: "POST",
    path: `${BASE}/turns`,
    handle: (req) => {
      const p = person(req);
      if (!isPerson(p)) return p;
      const ws = req.params.workspaceId!;
      const body = (req.body || {}) as Partial<
        Record<"threadId" | "text" | "clientToken", string> & {
          answers: Partial<FoundryAgentAnswers>;
          pageContext: AgentPageContext;
        }
      >;
      const text = (body.text || "").trim(),
        token = body.clientToken || "",
        page = body.pageContext,
        replying = body.answers !== undefined;
      if (!token || token.length > MAX_TOKEN)
        return fail(
          "INVALID_ARGUMENT",
          `client_token is required, at most ${MAX_TOKEN} characters`,
        );
      if (body.threadId && !ID.test(body.threadId))
        return fail("INVALID_ARGUMENT", "thread_id is malformed");
      if (replying && body.text !== undefined)
        return fail("INVALID_ARGUMENT", "text or answers, not both");
      if (!replying && (!text || [...text].length > MAX_TEXT))
        return fail(
          "INVALID_ARGUMENT",
          `text is required, at most ${MAX_TEXT} characters`,
        );
      // Answers: to the questions a turn of the thread asked, checked
      // against them (agentquestion.Check, blocking ones required).
      let reply: { asking: StoredTurn; answers: AgentQuestionAnswer[] } | null =
        null;
      if (replying) {
        if (!body.threadId)
          return fail(
            "INVALID_ARGUMENT",
            "thread_id is required with answers: the thread of the turn they answer",
          );
        const asking = turnOf(
          ws,
          p.userId,
          String(body.answers?.turnId || ""),
          p.grantee,
        );
        if (!asking)
          return turnMissing(ws, p.userId, String(body.answers?.turnId || ""));
        if (asking.threadId !== body.threadId)
          return fail("NOT_FOUND", "turn not found");
        if (!asking.asked)
          return refused(
            ws,
            "FOUNDRY_AGENT_QUESTIONS_CLOSED",
            "that turn asked you nothing",
          );
        // A grantee answers no draft's questions (Founder relays none to
        // it; one asked while it managed the workspace stays closed).
        if (p.grantee && asking.asked.generation)
          return refused(
            ws,
            "FOUNDRY_AGENT_QUESTIONS_CLOSED",
            "a draft's questions are answered by an owner or admin, on the generation's page in Foundry",
          );
        const checked = checkAnswers(
          asking.asked.questions || [],
          body.answers?.answers ?? [],
          false,
        );
        if (typeof checked === "string")
          return fail("INVALID_ARGUMENT", checked);
        // No question is left to the agent without useDefaults.
        reply = { asking, answers: checked as AgentQuestionAnswer[] };
      }
      if (
        (page?.ontologyId && !ID.test(page.ontologyId)) ||
        (page?.loadId && (!ID.test(page.loadId) || !page.ontologyId)) ||
        (page?.dashboardId && !ID.test(page.dashboardId))
      )
        return fail(
          "INVALID_ARGUMENT",
          "page_context names a load of an ontology, or a dashboard, by their ids",
        );
      const again = state.turns.find(
        (t) =>
          t.workspaceId === ws &&
          t.userId === p.userId &&
          t.clientToken === token,
      );
      if (again) {
        const was = threadOf(ws, p.userId, again.threadId, p.grantee);
        if (!was) return fail("NOT_FOUND", "thread not found");
        if (
          again.kind !== (reply ? "answers" : "message") ||
          again.text !== (reply ? "" : text) ||
          JSON.stringify(again.answers) !==
            JSON.stringify(
              reply
                ? { turnId: reply.asking.id, answers: reply.answers }
                : null,
            )
        )
          return fail(
            "INVALID_ARGUMENT",
            "client_token was already used for another message",
          );
        advance(again);
        return ok({
          turn: toTurn(again),
          thread: toThread(was),
        } satisfies AgentTurnResponse);
      }
      let thread = body.threadId
        ? threadOf(ws, p.userId, body.threadId, p.grantee)
        : undefined;
      if (body.threadId && !thread)
        return fail("NOT_FOUND", "thread not found");
      // A thread keeps its runtime; a new one takes the workspace's. A
      // helper's runs on its owner's subscription, not the workspace's model.
      const runtime = thread?.runtime ?? workspaceRuntime(ws);
      const offline = hostOffline(ws, runtime);
      if (offline) return offline;
      const ai = runtime === "api" ? aiRefusal(ws) : undefined;
      if (ai) return ai;
      // A relayed generation's answers go to it first, as the person, while
      // it still waits on that round; once they have, a refusal of the turn
      // says the draft has them.
      const relayed = reply?.asking.asked?.generation;
      if (reply && relayed) {
        const newest = state.turns
          .filter((t) => t.threadId === reply!.asking.threadId)
          .at(-1);
        if (newest !== reply.asking || reply.asking.status !== "done")
          return refused(
            ws,
            "FOUNDRY_AGENT_QUESTIONS_CLOSED",
            "these questions are closed: they were answered, or the conversation went on",
          );
        const at = `${ONT(ws, relayed.ontologyId || "")}/generations/${relayed.generationId || ""}`;
        const read = wfe(p.userId, "GET", at).body?.generation as
          | OntologyGeneration
          | undefined;
        if (
          read?.status !== "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT" ||
          read.questionRound !== relayed.round
        )
          return refused(
            ws,
            "FOUNDRY_AGENT_QUESTIONS_CLOSED",
            "the draft isn't waiting for these answers any more: they were answered on its page, or it moved on",
          );
        const left = (reply.asking.asked!.questions || []).some(
          (q) => !reply!.answers.some((x) => x.questionId === q.id),
        );
        const sentTo = wfe(p.userId, "POST", `${at}:answer`, {
          answers: reply.answers,
          round: relayed.round,
          ...(left && { useDefaults: true }),
        });
        if (sentTo.status !== 200)
          return fail(
            "FAILED_PRECONDITION",
            `your answers weren't sent to the draft: ${sentTo.body?.message ?? "it refused them"}`,
          );
      }
      const draftHas = (why: string) =>
        relayed
          ? `your answers reached the draft, which carries on from them, but Founder can't reply: ${why}`
          : why;
      const hourAgo = Date.now() - 3_600_000,
        sent = state.turns.filter(
          (t) =>
            t.workspaceId === ws &&
            t.userId === p.userId &&
            (t.kind === "message" || t.kind === "answers") &&
            t.at >= hourAgo,
        ).length;
      if (sent >= PER_HOUR)
        return fail(
          "RESOURCE_EXHAUSTED",
          draftHas(
            `you have sent ${sent} messages in the last hour; try again later`,
          ),
        );
      if (thread) {
        const busyWhy = busy(thread);
        if (busyWhy)
          return refused(ws, "FOUNDRY_AGENT_THREAD_BUSY", draftHas(busyWhy));
        // Answerable while the asking turn is the thread's newest, done.
        const newest = state.turns
          .filter((t) => t.threadId === thread!.id)
          .at(-1);
        if (
          reply &&
          (newest !== reply.asking || reply.asking.status !== "done")
        )
          return refused(
            ws,
            "FOUNDRY_AGENT_QUESTIONS_CLOSED",
            draftHas(
              "these questions are closed: they were answered, or the conversation went on",
            ),
          );
      } else {
        const line = text.split("\n")[0]!.trim();
        thread = {
          id: id("thr"),
          workspaceId: ws,
          userId: p.userId,
          kind: "chat",
          title:
            [...line].length <= TITLE_CHARS
              ? line
              : [...line]
                  .slice(0, TITLE_CHARS - 1)
                  .join("")
                  .trim() + "…",
          runtime,
          // A restricted member's holds what their grants give now.
          granted: p.grantee ? [...p.grantee.targets] : null,
          createdAt: now(),
          updatedAt: now(),
          deletedAt: null,
        };
        state.threads.push(thread);
      }

      // A grantee's page names only what its grants hold: an ontology they
      // hold, its load theirs cover, a pinned dashboard; it starts no
      // automation (the backend's filter, so Founder never reads the rest).
      const covered = p.grantee?.covered.get(page?.ontologyId || ""),
        pinned = storedDashboard(ws, page?.dashboardId || "")?.pinned;
      const seen: AgentPageContext | undefined = p.grantee
        ? {
            ontologyId: covered ? page?.ontologyId : undefined,
            loadId: covered?.loadId === page?.loadId ? page?.loadId : undefined,
            dashboardId: pinned ? page?.dashboardId : undefined,
            timeZone: page?.timeZone,
          }
        : page;
      // A zone that isn't one is dropped, never refused, as the WFE does.
      const zone =
        typeof seen?.timeZone === "string" && validZone(seen.timeZone)
          ? seen.timeZone
          : "";
      const pageContext =
        seen?.ontologyId || seen?.dashboardId || seen?.newAutomation || zone
          ? {
              ...(seen?.ontologyId && { ontologyId: seen.ontologyId }),
              ...(seen?.loadId && { loadId: seen.loadId }),
              ...(seen?.dashboardId && { dashboardId: seen.dashboardId }),
              ...(seen?.newAutomation && { newAutomation: true }),
              ...(zone && { timeZone: zone }),
            }
          : null;
      // Its turn runs as the person is now (the worker's threadReach): the
      // whole view's makes the thread no grantee's, for good; a grantee's
      // adds what it holds.
      if (!p.grantee) thread.granted = null;
      else if (thread.granted) {
        const had = new Set(thread.granted.map((t) => JSON.stringify(t)));
        thread.granted.push(
          ...p.grantee.targets.filter((t) => !had.has(JSON.stringify(t))),
        );
      }
      const turn = queueTurn(
        thread,
        {
          kind: reply ? "answers" : "message",
          text: reply ? "" : text,
          actionId: "",
          pageContext,
          clientToken: token,
          script: [],
          ...(reply && {
            answers: { turnId: reply.asking.id, answers: reply.answers },
          }),
        },
        reply
          ? {
              type: "answers",
              turn_id: reply.asking.id,
              answers: reply.answers.map((x) => ({
                question_id: x.questionId,
                options: x.options || [],
                ...(x.text && { text: x.text }),
              })),
            }
          : undefined,
      );
      // The worker runs on every provider kind, but not on a setting whose
      // tool check failed: it fails the turn for good, before it starts,
      // in its words (agent.checkFailure).
      const unusable =
        runtime === "api" ? aiAssistantFailure(ws) : helperFailure(runtime);
      turn.script = unusable
        ? [
            () => ({
              type: "finished",
              status: "failed",
              reason: "failed",
              error: unusable,
            }),
          ]
        : reply
          ? ending(
              answersScript(
                {
                  ws,
                  userId: p.userId,
                  threadId: thread.id,
                  text: "",
                  page: pageContext,
                },
                reply.asking,
                reply.answers,
              ),
            )
          : ending(
              messageScript(
                {
                  ws,
                  userId: p.userId,
                  threadId: thread.id,
                  text,
                  page: pageContext,
                },
                p.grantee ? "grantee" : manages(p.m) ? "manager" : "member",
              ),
            );
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
            t.kind === "chat" &&
            !t.deletedAt &&
            granteeReads(t, p.grantee),
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
      const t = threadOf(
        req.params.workspaceId!,
        p.userId,
        req.params.id!,
        p.grantee,
      );
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
      const t = threadOf(
        req.params.workspaceId!,
        p.userId,
        req.params.id!,
        p.grantee,
      );
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
      const t = turnOf(
        req.params.workspaceId!,
        p.userId,
        req.params.id!,
        p.grantee,
      );
      if (!t)
        return turnMissing(req.params.workspaceId!, p.userId, req.params.id!);
      advance(t);
      const after = Number(req.query.get("afterSeq") || 0) || 0;
      return ok({
        events: t.events.filter((e) => e.seq > after).slice(0, EVENTS),
        status: t.status,
        lastSeq: t.seq,
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
      const t = turnOf(ws, p.userId, req.params.id!, p.grantee);
      if (!t) return turnMissing(ws, p.userId, req.params.id!);
      advance(t);
      if (t.status !== "cancelled") {
        if (!live(t))
          return fail("FAILED_PRECONDITION", "the turn already ended");
        Object.assign(t, { status: "cancelled", finishedAt: now() });
      }
      return ok({
        turn: toTurn(t),
        thread: toThread(threadOf(ws, p.userId, t.threadId, p.grantee)!),
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
      const no = refreshRefusal(a, p.userId);
      if (no) return fail("FAILED_PRECONDITION", no);
      const status = shown(a);
      if (status !== "prepared" && status !== "expired")
        return refused(
          ws,
          "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
          `the action is ${status}`,
        );
      redraw(a);
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
