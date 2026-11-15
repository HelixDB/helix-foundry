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
  Connector,
  ConnectorKind,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  FoundryAgentEvent,
  FoundryAgentThread,
  FoundryAgentTurn,
  FoundryDocument,
  FoundryDocumentSet,
  FoundryLoad,
  ListAgentThreadsResponse,
  ListAgentTurnEventsResponse,
  OntologyGeneration,
  Project,
  SendAgentMessageRequest,
  Tenant,
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
import {
  config,
  db,
  isReply,
  LOCAL_TOKEN,
  membership,
  onReset,
} from "../state";
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
import { dataRoutes, workspaceConnectors } from "./data";
import { databaseRoutes, tenantGraph } from "./databases";
import { documentRoutes } from "./documents";
import { currentLoadId, loadRoutes, ontologyLoad } from "./loads";
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
import { reviewRoutes } from "./reviews";
import { manages } from "./workspaces";

// The Assistant, as the backend's API and foundry-agent-worker run it
// (backend/api/agent.go, backend/agent): a person's private threads, each
// message a turn that is queued, claimed and streams events the dock polls
// by afterSeq, in the backend's shapes (src/shared/wfe.ts "Agent"). The
// mock's agent is scripted, with the read tools' outputs in their shapes.
// It reads what a message asks for by its words and prepares one action of
// the 14 the backend allows (agent.Operations), its display as the
// backend's display.go names it:
// - "create an ontology called X on database Y" (create_ontology; "on a new
//   database Y" makes it in the workspace's first project);
// - "generate" or "draft" (generate_ontology: the connections and sets the
//   message names, else every connection; "documents only" reads none; a
//   quoted text is the goal);
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
//   "on Foundry's model") and "revoke the helper" (set_assistant_runtime,
//   revoke_assistant_helper on the newest helper, after
//   helix_foundry_get_assistant_runtime); hosted, it says they are a local
//   install's;
// - anything else, it reads the ontologies, and the one named or on the
//   page, and says what they hold.
// The ontology an action acts on is the one named, else the page's, else
// the one this thread last acted on, else the newest (a card that warns it
// was not named). A member gets the read tools only, as the backend gives
// them: no prepare and no connect, so the agent says an owner or admin
// does it. Running a card calls the mock's own WFE routes as the person who
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
// card's confirmation lives (5 minutes).

type Knobs = {
  agentStepMs?: number;
  agentDeltaMs?: number;
  agentOutcome?: "fail" | "budget_exhausted" | "not_run";
  agentActionTtlMs?: number;
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
  title: string;
  runtime: AgentRuntime;
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
});

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

// ---------------------------------------------------------------- the WFE

let compiled: (MockRoute & { c: ReturnType<typeof compileTemplate> })[];
/**
 * A WFE call as `userId` makes it, through the mock's own routes: how a
 * tool reads and an action runs, as the backend's do through Foundry's API.
 * Every route it calls answers at once.
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
  ].map((r) => ({ ...r, c: compileTemplate(r.path) }));
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
      throw new Error(`the Assistant's ${method} ${path} answers later`);
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

// A helper's turn runs on its CLI, as the backend names it.
const started: Step = (c) => ({
  type: "started",
  model:
    state.threads.find((t) => t.id === c.turn.threadId)?.runtime ===
    "claude_code"
      ? "Claude Code"
      : aiModel(c.workspaceId, "ask") || MODEL,
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
  PREPARE = "helix_foundry_prepare_operation";

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
  set_assistant_runtime: "choose where the Assistant runs",
  revoke_assistant_helper: "revoke a helper",
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

/** What the agent does with a person's message. */
function messageScript(a: Ask, manager: boolean): Step[] {
  const list = [
    call("tu_list", LIST, {}),
    result("tu_list", LIST, () => ({
      ontologies: workspaceOntologies(a.ws).map(ontologyOutput),
    })),
  ];
  const t = a.text;
  const intent: AgentOperation | "connect" | undefined =
    /\brevoke\b.*\bhelper\b/i.test(t)
      ? "revoke_assistant_helper"
      : /\b(run|switch|move)\b.*\b(subscription|claude code|foundry['’]s model)\b/i.test(
            t,
          )
        ? "set_assistant_runtime"
        : /\bconnect\b/i.test(t)
          ? "connect"
          : /\bcreate\b.*\bontology\b/i.test(t)
            ? "create_ontology"
            : /\b(settle|resolve)\b.*\breview\b|\breview about\b/i.test(t)
              ? "settle_review"
              : /\bsync\b/i.test(t)
                ? "sync_connector"
                : /\bsample\b/i.test(t)
                  ? "add_sample_data"
                  : /\bmove\b/i.test(t)
                    ? "move_document"
                    : /\b(rename|describe)\b.*\bset\b/i.test(t)
                      ? "update_document_set"
                      : /\b(create|make|add)\b.*\bset\b/i.test(t)
                        ? "create_document_set"
                        : /\b(revise|preview)\b/i.test(t)
                          ? "revise_version"
                          : /\b(generate|draft)\b/i.test(t)
                            ? "generate_ontology"
                            : /\bload\b.*\bcurrent\b/i.test(t)
                              ? "set_current_load"
                              : /\bcurrent\b/i.test(t)
                                ? "set_current_version"
                                : /\b(re)?load\b/i.test(t) &&
                                    !/\b(how many|which|what|when|why)\b/i.test(
                                      t,
                                    )
                                  ? "create_load"
                                  : undefined;
  if (intent && !manager)
    return [
      started,
      ...list,
      said(
        () =>
          `Only an owner or admin of this workspace can ${
            intent === "connect"
              ? "connect data"
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
    case undefined:
      return [started, ...list, ...describe(a)];
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
          "Where the Assistant runs is chosen on a local install of Foundry only: here it runs on the workspace’s AI model.",
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
        const to = /\bclaude\b/i.test(a.text) ? "claude_code" : "api";
        const online = to === "api" || (now.online || []).includes(to);
        if (to === now.runtime)
          return to === "api"
            ? "New threads already run on the workspace’s AI model."
            : "New threads already run on your Claude subscription.";
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
              : `I’ve prepared running new threads on your Claude subscription.${online ? "" : " No helper is running yet: start it on your computer first."} Switch from the card when you’re ready.`,
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
  intent: Exclude<
    AgentOperation,
    "create_ontology" | "set_assistant_runtime" | "revoke_assistant_helper"
  >,
  a: Ask,
  o: StoredOntology,
  named: boolean,
): Step[] {
  const version = /\bv(\d+)\b/i.exec(a.text)?.[1];
  const numbered = (n: string | undefined) =>
    n ? ontologyVersions(o.id).find((v) => String(v.version) === n) : undefined;
  switch (intent) {
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
            : `New threads run on your Claude subscription now${(out.online as string[] | undefined)?.includes(a.display.runtime) ? "" : ", once its helper runs on your computer"}. This thread keeps running where it started.`,
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
  >,
  first?: AgentEvent,
) {
  const at = Date.now(),
    stepMs = knobs.agentStepMs ?? STEP_MS;
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
    a.status = "succeeded";
    a.result = {
      content_trust: "untrusted",
      operation: a.operation,
      target: a.target,
      result: output(a, reply.body),
    };
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
  if (isReply(m) || !manages(m)) return "permission denied";
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
      // A thread keeps its runtime; a new one takes the workspace's. A
      // helper's runs on its owner's subscription, not the workspace's model.
      const runtime = thread?.runtime ?? workspaceRuntime(ws);
      const offline = hostOffline(ws, runtime);
      if (offline) return offline;
      const ai = runtime === "api" ? aiRefusal(ws) : undefined;
      if (ai) return ai;
      if (thread) {
        const busyWhy = busy(thread);
        if (busyWhy) return refused(ws, "FOUNDRY_AGENT_THREAD_BUSY", busyWhy);
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
          runtime,
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
        : ending(
            messageScript(
              {
                ws,
                userId: p.userId,
                threadId: thread.id,
                text,
                page: pageContext,
              },
              manages(p.m),
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
