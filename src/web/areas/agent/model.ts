import type {
  AgentActionDisplays,
  AgentActionStatus,
  AgentConnectorRef,
  AgentDecision,
  AgentDecisionValue,
  AgentLoadRef,
  AgentOperation,
  AgentRef,
  AgentReview,
  AgentReviewSettle,
  AgentVersionRef,
  AgentEvent,
  AgentPageContext,
  AgentRefusalReason,
  AgentStopReason,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  FoundryAi,
  ListAgentTurnEventsResponse,
} from "../../../shared/wfe";
import { AGENT_REFUSALS } from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";
import { ApiError } from "../../api";
import { connectorLabel } from "../../kit/SourceLogo";
import { formatDate, plural } from "../../kit/util";
import { dataPaths } from "../data/paths";
import { reviewCopy } from "../documents/model";
import { LOAD_LIMITS } from "../ontology/model";
import { ontologyPaths } from "../ontology/paths";

export type { AgentOperation };
import { inForce } from "../workspace/ai-api";

// The Assistant's dock, as pure functions: what a turn's polled events show,
// what an action's card offers, what page the person is on, and which links
// in the model's text open the app. Everything the WFE sends is parsed
// without trust: the model's text and a tool's output are shown as plain
// text, and only in-app paths become links.

/** The thread the dock shows, per workspace. */
export const threadKey = (workspaceId: string) =>
  `foundry.agent.thread:${workspaceId}`;
/** A message's cap (SendFoundryAgentMessage). */
export const MAX_MESSAGE = 4000;

const ID = /^[A-Za-z0-9_-]{1,128}$/;
const str = (x: unknown) => (typeof x === "string" ? x : undefined);
const parse = (text: string): unknown => {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
};

export const isLiveTurn = (t?: { status?: FoundryAgentTurnStatus } | null) =>
  t?.status === "queued" || t?.status === "running";

/**
 * Whether the Assistant runs on the workspace's AI setting in force: on
 * Claude alone for now (the backend's agent/llm fails a turn on any other
 * kind, Bedrock's Claude too); undefined with no provider, which AiGate
 * says.
 *
 * @example
 * onClaude({ default: { kind: "FOUNDRY_AI_KIND_ANTHROPIC" } }); // true
 * onClaude({ active: { id: "c", kind: "FOUNDRY_AI_KIND_OLLAMA" } }); // false
 * onClaude({}); // undefined
 */
export function onClaude(ai: FoundryAi): boolean | undefined {
  const f = inForce(ai);
  if (f.source === "none") return undefined;
  const kind = f.source === "workspace" ? f.config.kind : f.default.kind;
  return kind === "FOUNDRY_AI_KIND_ANTHROPIC";
}

// ---------------------------------------------------------------- page

/**
 * What the person is looking at, from the URL: an ontology's pages, its
 * Explorer or Analyst, and the load they read (a load's page, or `?load=`).
 * Ids only, never names; null anywhere else.
 *
 * @example
 * pageContext("/ontology/ont_1/loads/load_2", ""); // { ontologyId: "ont_1", loadId: "load_2" }
 * pageContext("/explore/ont_1/records", "?load=load_3"); // { ontologyId: "ont_1", loadId: "load_3" }
 * pageContext("/ontology/new", ""); // null
 * pageContext("/data", ""); // null
 */
export function pageContext(
  pathname: string,
  search: string,
): AgentPageContext | null {
  const [, section, ontologyId = "", sub, loadId = ""] = pathname.split("/");
  if (
    !["ontology", "explore", "ask"].includes(section || "") ||
    !ID.test(ontologyId) ||
    (section === "ontology" &&
      (ontologyId === "new" || ontologyId === "sample"))
  )
    return null;
  const load =
    section === "ontology"
      ? sub === "loads"
        ? loadId
        : ""
      : new URLSearchParams(search).get("load") || "";
  return ID.test(load) ? { ontologyId, loadId: load } : { ontologyId };
}

// ---------------------------------------------------------------- text

/** A run of the model's text: plain, or an in-app link. */
export type TextPart =
  | { kind: "text"; text: string }
  | { kind: "link"; text: string; to: string };

// The pages a link may open, as their routes are, each id one segment
// (SEG); nothing else is ever a link. /ontology/new and /ontology/sample,
// whose pages act on arrival, are no ontology's id here.
const SEG = "(?!(?:new|sample)(?:/|$))[A-Za-z0-9_-]{1,128}";
const LINKABLE = new RegExp(
  "^(?:" +
    [
      `/ontology(?:/${SEG}(?:/documents(?:/${SEG})?|/(?:loads|versions|generations)/${SEG})?)?`,
      `/explore(?:/${SEG}(?:/records|/entities/${SEG})?)?`,
      `/ask(?:/${SEG})?`,
      `/data(?:/documents/${SEG}|/connections/${SEG})?`,
      "/settings(?:/(?:ai|members|access|billing|enrichment))?",
    ].join("|") +
    ")$",
);
// A connector kind, in the link helix_foundry_connect gives (/data?add=<kind>).
const KIND = /^[a-z][a-z0-9_]{0,31}$/;
const CANDIDATE =
  /https?:\/\/[^\s<>"'`]+|(?<![\w/])\/(?:ontology|explore|ask|data|settings)(?:[/?#][^\s<>"'`]*)?/g;
const TRAILING = /[.,;:!?)\]'"]+$/;

/**
 * Where a link of the model's text goes in this app: a page LINKABLE
 * names, with only the query it may carry (a view's `load=<id>`, the
 * connect link's `add=<kind>` on /data) and no hash; undefined for any
 * other. So text can't switch the person's workspace (`?workspace=`), open
 * a dialog (`?new-load=1`) or pack words into a path.
 *
 * @example
 * inAppPath(new URL("https://f.example/explore/o_1?load=l_2&workspace=w")); // "/explore/o_1?load=l_2"
 * inAppPath(new URL("https://f.example/ontology/sample")); // undefined
 */
export function inAppPath(url: URL): string | undefined {
  const path = url.pathname.replace(/(.)\/$/, "$1");
  if (!LINKABLE.test(path)) return undefined;
  const q = new URLSearchParams(),
    load = url.searchParams.get("load") ?? "",
    add = url.searchParams.get("add") ?? "";
  if (/^\/(explore|ask)\//.test(path) && ID.test(load)) q.set("load", load);
  if (path === "/data" && KIND.test(add)) q.set("add", add);
  return path + (q.size ? "?" + q : "");
}

/**
 * The model's text with the links that open this app made links: an
 * address on `origin`, or a path, to a page inAppPath allows, with only
 * what it keeps of the query. Any other address stays text, never a link,
 * so text can't send the person elsewhere (or leak what it read through an
 * address).
 *
 * @example
 * textParts("See /ontology/ont_1/loads/l_2.", "https://foundry.example");
 * // [{ kind: "text", text: "See " },
 * //  { kind: "link", text: "/ontology/ont_1/loads/l_2", to: "/ontology/ont_1/loads/l_2" },
 * //  { kind: "text", text: "." }]
 * textParts("https://evil.example/ontology/x", "https://foundry.example");
 * // [{ kind: "text", text: "https://evil.example/ontology/x" }]
 */
export function textParts(text: string, origin: string): TextPart[] {
  const parts: TextPart[] = [];
  let from = 0;
  for (const m of text.matchAll(CANDIDATE)) {
    const raw = m[0].replace(TRAILING, ""),
      start = m.index,
      url = URL.parse(raw, origin),
      to = url && url.origin === origin ? inAppPath(url) : undefined;
    if (!to) continue;
    if (start > from)
      parts.push({ kind: "text", text: text.slice(from, start) });
    parts.push({ kind: "link", text: raw, to });
    from = start + raw.length;
  }
  if (from < text.length) parts.push({ kind: "text", text: text.slice(from) });
  return parts;
}

// ---------------------------------------------------------------- events

/**
 * One stored event's JSON as an AgentEvent, or undefined when it isn't one:
 * every field checked, nothing taken on trust.
 *
 * @example
 * parseAgentEvent('{"type":"text","text":"Hi"}'); // { type: "text", text: "Hi" }
 * parseAgentEvent('{"type":"text","text":3}'); // undefined
 */
export function parseAgentEvent(eventJson: string): AgentEvent | undefined {
  const x = parse(eventJson);
  if (!isRecord(x)) return undefined;
  switch (x.type) {
    case "started":
      return {
        type: "started",
        model: str(x.model) || "",
        attempt: typeof x.attempt === "number" ? x.attempt : 1,
      };
    case "text":
      return typeof x.text === "string"
        ? { type: "text", text: x.text }
        : undefined;
    case "tool_call":
      return str(x.tool_use_id) && str(x.name)
        ? {
            type: "tool_call",
            tool_use_id: x.tool_use_id as string,
            name: x.name as string,
            title: str(x.title) || "",
            input: x.input,
          }
        : undefined;
    case "tool_result":
      return str(x.tool_use_id)
        ? {
            type: "tool_result",
            tool_use_id: x.tool_use_id as string,
            name: str(x.name) || "",
            is_error: x.is_error === true,
            ...(str(x.error) !== undefined && { error: x.error as string }),
            ...(x.output !== undefined && { output: x.output }),
            ...(x.truncated === true && { truncated: true }),
            ...(x.not_run === true && { not_run: true }),
          }
        : undefined;
    case "action_card":
      return str(x.action_id) && str(x.operation)
        ? {
            type: "action_card",
            action_id: x.action_id as string,
            operation: x.operation as string,
            target: str(x.target) || "",
            payload: x.payload,
            payload_hash: str(x.payload_hash) || "",
            display: x.display,
            target_named_by_user: x.target_named_by_user === true,
            expires_at: str(x.expires_at) || "",
          }
        : undefined;
    case "action_result": {
      const status = actionStatus(x.status);
      return str(x.action_id) && status
        ? {
            type: "action_result",
            action_id: x.action_id as string,
            operation: str(x.operation) || "",
            status,
            ...(x.result !== undefined && { result: x.result }),
            ...(str(x.error) && { error: x.error as string }),
          }
        : undefined;
    }
    case "finished": {
      const reason = STOP_REASONS.find((r) => r === x.reason);
      return (x.status === "done" || x.status === "failed") && reason
        ? {
            type: "finished",
            status: x.status,
            reason,
            ...(str(x.error) && { error: x.error as string }),
          }
        : undefined;
    }
    default:
      return undefined;
  }
}

const ACTION_STATUSES: readonly AgentActionStatus[] = [
  "prepared",
  "executing",
  "succeeded",
  "failed",
  "unknown",
  "expired",
  "dismissed",
];
const actionStatus = (x: unknown) => ACTION_STATUSES.find((s) => s === x);

/** What a turn ended with, in words; none for an answer. */
export const STOP_TEXT: Record<AgentStopReason, string | null> = {
  answered: null,
  max_steps: "It stopped at its step limit. Ask it to go on.",
  wall_time: "It stopped at its time limit. Ask it to go on.",
  budget: "It stopped at this message’s spending cap.",
  max_tokens: "Its answer reached its length limit.",
  refusal: "The model declined to answer.",
  context: "This thread is too long for the model. Start a new thread.",
  failed: "It failed.",
};
const STOP_REASONS = Object.keys(STOP_TEXT) as AgentStopReason[];

/** One tool call and, once it returned, what it gave. */
export type AgentStep = {
  id: string;
  name: string;
  input: unknown;
  result?:
    | { kind: "returned"; output: unknown; truncated: boolean }
    | { kind: "failed"; error: string }
    // A limit kept it from running: not the tool's failure.
    | { kind: "not_run"; reason: string };
};

/** What a turn shows, in order. */
export type AgentBlock =
  | { kind: "text"; text: string }
  | { kind: "step"; step: AgentStep }
  // An action the agent prepared: its card reads the thread's action.
  | { kind: "card"; actionId: string; operation: string; named: boolean }
  // How the person's click ended, opening an action_result turn.
  | {
      kind: "ran";
      actionId: string;
      operation: string;
      status: AgentActionStatus;
      error?: string;
    }
  // A later attempt went on from what the first committed.
  | { kind: "retry"; attempt: number };

/** A turn's events so far, folded. */
export type TurnStream = {
  // The last seq folded in: the next poll reads after it.
  lastSeq: number;
  blocks: AgentBlock[];
  ended?: {
    status: "done" | "failed";
    reason: AgentStopReason;
    error?: string;
  };
  // The turn ended and every event it sent is folded: none follows.
  complete: boolean;
  // Events that didn't parse.
  unreadable: number;
};
export const EMPTY_STREAM: TurnStream = {
  lastSeq: 0,
  blocks: [],
  complete: false,
  unreadable: 0,
};

/**
 * Folds a page of polled events into the stream, in seq order. An event at
 * or before `lastSeq` was folded already (a poll that overlapped) and is
 * skipped. A later attempt resumes what the earlier committed, so its
 * blocks stay; a result without its call is a step of its own. Complete
 * once the page says the turn ended and its events reach its `lastSeq`.
 *
 * @example
 * const s = reduceTurnEvents(EMPTY_STREAM, {
 *   events: [
 *     { seq: 1, eventJson: '{"type":"tool_call","tool_use_id":"a","name":"helix_foundry_list_ontologies","title":"","input":{}}' },
 *     { seq: 2, eventJson: '{"type":"tool_result","tool_use_id":"a","name":"helix_foundry_list_ontologies","is_error":false,"output":{}}' },
 *     { seq: 3, eventJson: '{"type":"finished","status":"done","reason":"answered"}' },
 *   ],
 *   status: "done",
 *   lastSeq: 3,
 * });
 * s.blocks.length; // 1: the step, with its result
 * s.complete; // true
 */
export function reduceTurnEvents(
  stream: TurnStream,
  page: ListAgentTurnEventsResponse,
): TurnStream {
  const next = [...(page.events || [])]
    .filter((e) => e.seq > stream.lastSeq)
    .sort((a, b) => a.seq - b.seq)
    .reduce<TurnStream>((s, e) => {
      const at = { ...s, lastSeq: e.seq };
      const ev = parseAgentEvent(e.eventJson);
      if (!ev) return { ...at, unreadable: s.unreadable + 1 };
      const add = (block: AgentBlock) => ({
        ...at,
        blocks: [...s.blocks, block],
      });
      switch (ev.type) {
        case "started":
          return ev.attempt > 1
            ? add({ kind: "retry", attempt: ev.attempt })
            : at;
        case "text":
          return ev.text.trim() ? add({ kind: "text", text: ev.text }) : at;
        case "tool_call":
          return add({
            kind: "step",
            step: { id: ev.tool_use_id, name: ev.name, input: ev.input },
          });
        case "tool_result": {
          const result: AgentStep["result"] = ev.not_run
            ? {
                kind: "not_run",
                reason: (ev.error || "").replace(/^Not run:?\s*/i, ""),
              }
            : ev.is_error
              ? { kind: "failed", error: ev.error || "it failed" }
              : {
                  kind: "returned",
                  output: ev.output,
                  truncated: !!ev.truncated,
                };
          const i = s.blocks.findIndex(
            (b) =>
              b.kind === "step" &&
              b.step.id === ev.tool_use_id &&
              !b.step.result,
          );
          if (i < 0)
            return add({
              kind: "step",
              step: { id: ev.tool_use_id, name: ev.name, input: null, result },
            });
          return {
            ...at,
            blocks: s.blocks.map((b, j) =>
              j === i && b.kind === "step"
                ? { ...b, step: { ...b.step, result } }
                : b,
            ),
          };
        }
        case "action_card":
          return s.blocks.some(
            (b) => b.kind === "card" && b.actionId === ev.action_id,
          )
            ? at
            : add({
                kind: "card",
                actionId: ev.action_id,
                operation: ev.operation,
                named: ev.target_named_by_user,
              });
        case "action_result":
          return add({
            kind: "ran",
            actionId: ev.action_id,
            operation: ev.operation,
            status: ev.status,
            ...(ev.error && { error: ev.error }),
          });
        case "finished":
          return {
            ...at,
            ended: {
              status: ev.status,
              reason: ev.reason,
              ...(ev.error && { error: ev.error }),
            },
          };
      }
    }, stream);
  const complete =
    !!page.status && !isLiveTurn(page) && next.lastSeq >= (page.lastSeq || 0);
  return complete === next.complete ? next : { ...next, complete };
}

/** A tool's step, as the dock names it; the tool's own name for another. */
export const TOOL_LABELS: Record<string, string> = {
  helix_foundry_list_ontologies: "Listed the ontologies",
  helix_foundry_get_ontology: "Read an ontology",
  helix_foundry_get_version: "Read a version",
  helix_foundry_get_context: "Read the workspace",
  helix_foundry_read_guide: "Read Foundry’s guide",
  helix_foundry_get_generation: "Read a draft",
  helix_foundry_get_load: "Read a load",
  helix_foundry_get_sync: "Read a sync",
  helix_foundry_list_documents: "Listed documents",
  helix_foundry_list_connectors: "Listed connections",
  helix_foundry_list_connector_kinds: "Listed the kinds of connection",
  helix_foundry_list_review_decisions: "Listed review decisions",
  helix_foundry_list_reviews: "Listed the reviews to settle",
  helix_foundry_read_graph: "Read the graph",
  helix_foundry_ask: "Asked the Analyst",
  helix_foundry_get_question: "Read a question",
  helix_foundry_list_questions: "Listed questions",
  helix_foundry_connect: "Made a link to connect data",
  helix_list_projects: "Listed projects",
  helix_list_databases: "Listed databases",
  helix_foundry_prepare_operation: "Prepared an action",
};

/** The Analyst page an ask step's question is on, when its input names an ontology. */
export function analystLink(step: AgentStep): string | undefined {
  if (step.name !== "helix_foundry_ask" || !isRecord(step.input))
    return undefined;
  const o = str(step.input.ontology_id);
  return o && ID.test(o) ? ontologyPaths.ask(o) : undefined;
}

/** JSON as the dock shows it: indented, or as it came when it isn't JSON. */
export const pretty = (x: unknown) =>
  typeof x === "string" ? x : (JSON.stringify(x, null, 2) ?? "");

// ---------------------------------------------------------------- actions

/** An action as its card shows it: its operation and Foundry's display for it. */
export type AgentCard = {
  [K in AgentOperation]: { operation: K; display: AgentActionDisplays[K] };
}[AgentOperation];

const num = (x: unknown) =>
  typeof x === "number" && Number.isFinite(x) ? x : undefined;
const ids = (x: unknown) =>
  typeof x === "string" && ID.test(x) ? x : undefined;
const opt = <K extends string, V>(key: K, v: V | undefined) =>
  (v === undefined ? {} : { [key]: v }) as Partial<Record<K, V>>;
const list = <T>(x: unknown, item: (v: unknown) => T | undefined) => {
  if (!Array.isArray(x)) return undefined;
  const out = x.map(item);
  return out.every((v) => v !== undefined) ? (out as T[]) : undefined;
};

function ref(x: unknown): AgentRef | undefined {
  if (!isRecord(x)) return undefined;
  const id = ids(x.id);
  return id ? { id, ...opt("name", str(x.name) || undefined) } : undefined;
}
function connectorRef(x: unknown): AgentConnectorRef | undefined {
  const r = ref(x);
  return r && isRecord(x)
    ? { ...r, ...opt("kind", str(x.kind) || undefined) }
    : r;
}
function versionRef(x: unknown): AgentVersionRef | undefined {
  if (!isRecord(x)) return undefined;
  const id = ids(x.id),
    n = num(x.number);
  return id
    ? {
        id,
        ...opt("number", n && Number.isInteger(n) && n > 0 ? n : undefined),
      }
    : undefined;
}
function loadRef(x: unknown): AgentLoadRef | undefined {
  if (!isRecord(x)) return undefined;
  // A load that couldn't be read names no version: its id is empty.
  const id = ids(x.id),
    version =
      versionRef(x.version) ??
      (isRecord(x.version) && x.version.id === "" ? { id: "" } : undefined);
  return id && version
    ? {
        id,
        version,
        ...opt("status", str(x.status) || undefined),
        ...opt("created_at", str(x.created_at) || undefined),
      }
    : undefined;
}

const SETTLES: readonly AgentReviewSettle[] = [
  "resolve",
  "prop",
  "release",
  "stale",
  "none",
];
function review(x: unknown): AgentReview | undefined {
  if (!isRecord(x)) return undefined;
  const key = str(x.review_key),
    settle = SETTLES.find((s) => s === x.settle);
  if (!key || !settle) return undefined;
  const texts = (
    k:
      | "type"
      | "doc_id"
      | "quote"
      | "norm_name"
      | "entity_key"
      | "prop"
      | "link"
      | "statement"
      | "statement_hash",
  ) => opt(k, str(x[k]) || undefined);
  const int = (k: "section" | "start" | "end") => {
    const v = num(x[k]);
    return opt(
      k,
      v !== undefined && Number.isInteger(v) && v >= 0 ? v : undefined,
    );
  };
  const candidates = x.candidates === undefined ? [] : list(x.candidates, str);
  const values =
    x.values === undefined
      ? []
      : list(x.values, (v) =>
          isRecord(v) && "value" in v
            ? {
                value: v.value,
                ...opt("doc_id", str(v.doc_id) || undefined),
                ...opt("quote", str(v.quote) || undefined),
              }
            : undefined,
        );
  if (!candidates || !values) return undefined;
  return {
    review_key: key,
    reason: str(x.reason) || "",
    settle,
    ...texts("type"),
    ...texts("doc_id"),
    ...texts("quote"),
    ...int("section"),
    ...int("start"),
    ...int("end"),
    ...texts("norm_name"),
    ...opt("candidates", candidates.length ? candidates : undefined),
    ...texts("entity_key"),
    ...texts("prop"),
    ...opt("values", values.length ? values : undefined),
    ...texts("link"),
    ...texts("statement"),
    ...texts("statement_hash"),
    ...opt("confidence", num(x.confidence)),
  };
}
function decisionValue(x: unknown): AgentDecisionValue | undefined {
  if (!isRecord(x) || Object.keys(x).length !== 1) return undefined;
  if (typeof x.text === "string") return { text: x.text };
  if (typeof x.integer === "string" && /^-?\d{1,19}$/.test(x.integer))
    return { integer: x.integer };
  if (num(x.number) !== undefined) return { number: x.number as number };
  if (typeof x.boolean === "boolean") return { boolean: x.boolean };
  return undefined;
}
function decision(x: unknown): AgentDecision | undefined {
  if (!isRecord(x)) return undefined;
  switch (x.kind) {
    case "link": {
      const key = str(x.entity_key);
      return key
        ? { kind: "link", entity_key: key, candidate: x.candidate === true }
        : undefined;
    }
    case "set_value": {
      const value = decisionValue(x.value);
      return value ? { kind: "set_value", value } : undefined;
    }
    case "not_in_data":
    case "remove_value":
    case "approve":
    case "reject":
      return { kind: x.kind };
    default:
      return undefined;
  }
}

/** Each operation's display, read without trust: undefined when it isn't one. */
const DISPLAYS: {
  [K in AgentOperation]: (
    d: Record<string, unknown>,
  ) => AgentActionDisplays[K] | undefined;
} = {
  create_ontology: (d) => {
    const name = str(d.name);
    if (!name) return undefined;
    const database = ref(d.database),
      fresh = isRecord(d.new_database) ? d.new_database : undefined;
    if (!!database === !!fresh) return undefined;
    if (database) return { name, database };
    const n = str(fresh!.name),
      slug = str(fresh!.slug),
      project = ids(fresh!.project_id),
      newProject = str(fresh!.new_project);
    if (!n || !slug || !!project === !!newProject) return undefined;
    return {
      name,
      new_database: project
        ? { name: n, slug, project_id: project }
        : { name: n, slug, new_project: newProject! },
    };
  },
  generate_ontology: (d) => {
    const ontology = ref(d.ontology),
      read = (["every", "none", "chosen"] as const).find(
        (r) => r === d.connectors_read,
      ),
      connectors = list(d.connectors ?? [], connectorRef),
      sets = list(d.document_sets ?? [], ref),
      sources =
        d.event_sources === undefined
          ? []
          : list(d.event_sources, (e) => {
              const c = isRecord(e) ? connectorRef(e.connector) : undefined;
              const mode = isRecord(e) ? str(e.mode) : undefined;
              return c && mode ? { connector: c, mode } : undefined;
            });
    if (!ontology || !read || !connectors || !sets || !sources)
      return undefined;
    return {
      ontology,
      ...opt("goal", str(d.goal) || undefined),
      connectors_read: read,
      connectors,
      document_sets: sets,
      ...opt("event_sources", sources.length ? sources : undefined),
      ...opt("sample", str(d.sample) || undefined),
    };
  },
  set_current_version: (d) => {
    const ontology = ref(d.ontology),
      version = versionRef(d.version),
      current = d.current === undefined ? null : versionRef(d.current);
    if (!ontology || !version || current === undefined) return undefined;
    return { ontology, version, ...opt("current", current ?? undefined) };
  },
  create_load: (d) => {
    const ontology = ref(d.ontology),
      version = versionRef(d.version);
    return ontology && version ? { ontology, version } : undefined;
  },
  set_current_load: (d) => {
    const ontology = ref(d.ontology),
      load = loadRef(d.load),
      current = d.current === undefined ? null : loadRef(d.current);
    if (!ontology || !load || current === undefined) return undefined;
    return { ontology, load, ...opt("current", current ?? undefined) };
  },
  revise_version: (d) => {
    const ontology = ref(d.ontology),
      version = versionRef(d.version),
      edits = list(d.edits, (e) =>
        isRecord(e) &&
        (e.kind === "put" || e.kind === "remove") &&
        str(e.item_id)
          ? { kind: e.kind as "put" | "remove", item_id: e.item_id as string }
          : undefined,
      );
    if (
      !ontology ||
      !version ||
      !edits?.length ||
      typeof d.dry_run !== "boolean"
    )
      return undefined;
    return { ontology, version, edits, dry_run: d.dry_run };
  },
  sync_connector: (d) => {
    const connector = connectorRef(d.connector);
    return connector ? { connector } : undefined;
  },
  add_sample_data: (d) => {
    const ontology = ref(d.ontology);
    return ontology ? { ontology } : undefined;
  },
  create_document_set: (d) => {
    const ontology = ref(d.ontology),
      name = str(d.name);
    return ontology && name
      ? {
          ontology,
          name,
          ...opt("description", str(d.description) || undefined),
        }
      : undefined;
  },
  update_document_set: (d) => {
    const ontology = ref(d.ontology),
      set = ref(d.set),
      name = d.name === undefined ? null : str(d.name) || undefined,
      description = d.description === undefined ? null : str(d.description);
    if (!ontology || !set || name === undefined || description === undefined)
      return undefined;
    if (name === null && description === null) return undefined;
    return {
      ontology,
      set,
      ...opt("name", name ?? undefined),
      ...opt("description", description ?? undefined),
    };
  },
  move_document: (d) => {
    const ontology = ref(d.ontology),
      document = ref(d.document),
      to = ref(d.to);
    // The set it is in, unknown when the document couldn't be read.
    const from = ref(d.from) ?? { id: "" };
    return ontology && document && to
      ? { ontology, document, from, to }
      : undefined;
  },
  settle_review: (d) => {
    const ontology = ref(d.ontology),
      loadId = ids(d.load_id),
      r = review(d.review),
      document = d.document === undefined ? null : ref(d.document),
      decided = decision(d.decision);
    if (!ontology || !loadId || !r || document === undefined || !decided)
      return undefined;
    return {
      ontology,
      load_id: loadId,
      review: r,
      ...opt("document", document ?? undefined),
      decision: decided,
    };
  },
};

export const isAgentOperation = (x: string): x is AgentOperation =>
  Object.hasOwn(DISPLAYS, x);

/**
 * The card of an action: its operation, and Foundry's display for it read
 * without trust. Undefined for an operation this page doesn't know, or a
 * display that isn't its operation's: such a card is never run from here.
 *
 * @example
 * parseCard("sync_connector", '{"connector":{"id":"con_1","name":"Stripe","kind":"stripe"}}');
 * // { operation: "sync_connector", display: { connector: { id: "con_1", name: "Stripe", kind: "stripe" } } }
 * parseCard("sync_connector", '{"connector":{"id":"../x"}}'); // undefined
 * parseCard("drop_database", "{}"); // undefined
 */
export function parseCard(
  operation: string,
  display: unknown,
): AgentCard | undefined {
  const v = typeof display === "string" ? parse(display) : display;
  if (!isAgentOperation(operation) || !isRecord(v)) return undefined;
  const d = (DISPLAYS[operation] as (d: Record<string, unknown>) => unknown)(v);
  return d ? ({ operation, display: d } as AgentCard) : undefined;
}

/** A card's words: what it is, what it acts on, what running it does. */
export type CardView = {
  title: string;
  // What it acts on, as Foundry names it.
  facts: { label: string; value: string }[];
  // The document's words a review was read from: untrusted.
  quote?: string;
  does: string;
  cost?: string;
  // What to check before running it, beyond the "not named by you" warning.
  caution?: string;
  // The button that runs it.
  run: string;
  // What the "not named by you" warning names.
  target: string;
};

const nameOf = (r: AgentRef) => r.name || r.id;
const quoted = (r: AgentRef) => (r.name ? `“${r.name}”` : r.id);
const vN = (v: AgentVersionRef) => (v.number ? `v${v.number}` : "a version");
const listOf = (rs: AgentRef[]) => rs.map(nameOf).join(", ");
const MODES: Record<string, string> = {
  FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED: "as rows",
  FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION: "as documents",
};
const STATUSES: Record<string, string> = {
  ready: "ready",
  partial: "partial",
  failed: "failed",
  writing: "writing",
  queued: "queued",
};
function loadText(l: AgentLoadRef) {
  const at = Date.parse(l.created_at || "");
  return [
    l.version.number ? `v${l.version.number} load` : "A load",
    l.status && (STATUSES[l.status] || l.status),
    Number.isFinite(at) && `started ${formatDate(l.created_at)}`,
  ]
    .filter(Boolean)
    .join(", ");
}

/**
 * A decided value as the card says it: text quoted, numbers and yes/no as
 * they are.
 *
 * @example
 * valueText({ text: "GBP" }); // "“GBP”"
 * valueText({ boolean: false }); // "false"
 */
export function valueText(v: AgentDecisionValue) {
  if ("text" in v) return `“${v.text}”`;
  if ("integer" in v) return v.integer;
  if ("number" in v) return String(v.number);
  return String(v.boolean);
}

/**
 * A held statement as words, from its JSON and the link it would write:
 * "Contract “MSA” signed_with Customer “Globex”"; undefined when it names
 * neither end.
 *
 * @example
 * statementText('{"subject":{"type":"Commitment","name":"support"}}', "promised_to");
 * // "Commitment “support” promised_to"
 */
export function statementText(
  json?: string,
  link?: string,
): string | undefined {
  const x = json ? parse(json) : undefined;
  if (!isRecord(x)) return undefined;
  const end = (e: unknown) =>
    isRecord(e) && str(e.type)
      ? `${e.type}${str(e.name) ? ` “${e.name}”` : ""}`
      : undefined;
  const subject = end(x.subject),
    object = end(x.object);
  if (!subject && !object) return undefined;
  return [subject, link, object].filter(Boolean).join(" ");
}

/**
 * The name a resolve review decides, as the decision binds it: its
 * normalized name (its quote is the sentence it was read in).
 */
const reviewName = (r: AgentReview) => r.norm_name || r.quote || "this name";

const LOAD_CAPS = `Reading documents costs at most $${LOAD_LIMITS.jobUsd} for this load and $${LOAD_LIMITS.monthUsd} a month for the workspace.`;

/** Each operation's card, in the app's words; the run button names what it does. */
const VIEWS: {
  [K in AgentOperation]: (d: AgentActionDisplays[K]) => CardView;
} = {
  create_ontology: (d) => {
    const fresh = "new_database" in d ? d.new_database : undefined;
    return {
      title: `Create the ontology “${d.name}”`,
      facts: [
        { label: "Name", value: d.name },
        fresh
          ? { label: "Database", value: `New: ${fresh.name} (${fresh.slug})` }
          : {
              label: "Database",
              value: nameOf((d as { database: AgentRef }).database),
            },
        ...(fresh
          ? [
              {
                label: "Project",
                value:
                  "new_project" in fresh
                    ? `New: ${fresh.new_project}`
                    : fresh.project_id,
              },
            ]
          : []),
      ],
      does: fresh
        ? "Makes a new Helix database on the shared cluster, then an empty ontology on it. Nothing is drafted or loaded yet."
        : "Makes an empty ontology on this database, which no other ontology may then use. Nothing is drafted or loaded yet.",
      run: "Create",
      target: fresh
        ? fresh.name
        : nameOf((d as { database: AgentRef }).database),
    };
  },
  generate_ontology: (d) => ({
    title: d.sample
      ? `Draft the sample company’s version of ${quoted(d.ontology)}`
      : `Draft a new version of ${quoted(d.ontology)}`,
    facts: [
      { label: "Ontology", value: nameOf(d.ontology) },
      { label: "Goal", value: d.goal || "None: the agent decides" },
      {
        label: "Connections",
        value:
          d.connectors_read === "none"
            ? "None: documents only"
            : d.connectors_read === "every"
              ? `Every connection${d.connectors.length ? `: ${listOf(d.connectors)}` : " (none yet)"}`
              : listOf(d.connectors),
      },
      {
        label: "Document sets",
        value: d.document_sets.length ? listOf(d.document_sets) : "None",
      },
      ...(d.event_sources
        ? [
            {
              label: "API sources",
              value: d.event_sources
                .map(
                  (e) =>
                    `${nameOf(e.connector)} ${MODES[e.mode] || `(${e.mode})`}`,
                )
                .join(", "),
            },
          ]
        : []),
    ],
    does: d.sample
      ? "Takes the sample company’s ready-made draft as a new version, in seconds, with no AI. Nothing changes until you make it current."
      : "The ontology agent drafts a new version from these sources. Nothing changes until you make the draft current.",
    run: "Generate",
    target: nameOf(d.ontology),
  }),
  set_current_version: (d) => ({
    title: `Make ${vN(d.version)} current for ${quoted(d.ontology)}`,
    facts: [
      { label: "Ontology", value: nameOf(d.ontology) },
      { label: "Version", value: vN(d.version) },
      { label: "Current now", value: d.current ? vN(d.current) : "None yet" },
    ],
    does: "New loads write this version from then on. Loads already written stay as they are, and review decisions it doesn’t take are dropped.",
    run: "Make current",
    target: nameOf(d.ontology),
  }),
  create_load: (d) => ({
    title: `Load ${vN(d.version)} of ${quoted(d.ontology)}`,
    facts: [
      { label: "Ontology", value: nameOf(d.ontology) },
      { label: "Version", value: vN(d.version) },
    ],
    does: "Writes this version’s tables and documents into the ontology’s database, beside its other loads. The newest load that finishes ready becomes current.",
    cost: LOAD_CAPS,
    run: "Load",
    target: nameOf(d.ontology),
  }),
  set_current_load: (d) => ({
    title: `Make ${d.load.version.number ? `the v${d.load.version.number} load` : "a load"} current for ${quoted(d.ontology)}`,
    facts: [
      { label: "Ontology", value: nameOf(d.ontology) },
      { label: "Load", value: loadText(d.load) },
      {
        label: "Current now",
        value: d.current ? loadText(d.current) : "None yet",
      },
    ],
    does: "The Explorer, Records and the Analyst read this load from then on. Nothing is written or deleted.",
    run: "Make current",
    target: nameOf(d.ontology),
  }),
  revise_version: (d) => ({
    title: d.dry_run
      ? `Preview ${plural(d.edits.length, "edit")} to ${vN(d.version)} of ${quoted(d.ontology)}`
      : `Save ${plural(d.edits.length, "edit")} to ${vN(d.version)} of ${quoted(d.ontology)} as a new version`,
    facts: [
      { label: "Ontology", value: nameOf(d.ontology) },
      { label: "Version", value: vN(d.version) },
      {
        label: "Edits",
        value: d.edits
          .map((e) => `${e.kind === "put" ? "Change" : "Remove"} ${e.item_id}`)
          .join("; "),
      },
    ],
    does: d.dry_run
      ? "The ontology agent checks these edits against the version and its data, and saves nothing."
      : "The ontology agent checks these edits and saves them as a new version. The current version stays until you make the new one current.",
    run: d.dry_run ? "Preview" : "Save version",
    target: nameOf(d.ontology),
  }),
  sync_connector: (d) => ({
    title: `Sync ${quoted(d.connector)}`,
    facts: [
      { label: "Connection", value: nameOf(d.connector) },
      ...(d.connector.kind
        ? [{ label: "Kind", value: connectorLabel(d.connector.kind) }]
        : []),
    ],
    does: "Reads the connection’s tables from the source again. Loads that read them take the new rows at their next load.",
    run: "Sync",
    target: nameOf(d.connector),
  }),
  add_sample_data: (d) => ({
    title: `Add the sample company’s data for ${quoted(d.ontology)}`,
    facts: [{ label: "Ontology", value: nameOf(d.ontology) }],
    does: "Connects Acme Corp’s sample database and uploads its documents into the set “Acme Corp sample”, for drafting from. The data is made up; nothing of yours changes.",
    run: "Add sample data",
    target: nameOf(d.ontology),
  }),
  create_document_set: (d) => ({
    title: `Create the document set “${d.name}”`,
    facts: [
      { label: "Name", value: d.name },
      { label: "Description", value: d.description || "None" },
      { label: "For", value: nameOf(d.ontology) },
    ],
    does: "Makes an empty set of documents in the workspace. Upload documents into it from Data; the agent reads its description when it drafts.",
    run: "Create set",
    target: nameOf(d.ontology),
  }),
  update_document_set: (d) => ({
    title:
      d.name !== undefined && d.description === undefined
        ? `Rename the set ${quoted(d.set)} to “${d.name}”`
        : d.name === undefined
          ? `Change the description of the set ${quoted(d.set)}`
          : `Rename and describe the set ${quoted(d.set)}`,
    facts: [
      { label: "Set", value: nameOf(d.set) },
      ...(d.name !== undefined ? [{ label: "New name", value: d.name }] : []),
      ...(d.description !== undefined
        ? [
            {
              label: "Description",
              value: d.description || "Cleared",
            },
          ]
        : []),
    ],
    does: "Its documents stay where they are, and ontologies that read the set go on reading it. The agent reads the description when it drafts.",
    run: "Save",
    target: nameOf(d.set),
  }),
  move_document: (d) => ({
    title: `Move ${quoted(d.document)} to ${quoted(d.to)}`,
    facts: [
      { label: "Document", value: nameOf(d.document) },
      { label: "From", value: d.from.id ? nameOf(d.from) : "Unknown" },
      { label: "To", value: nameOf(d.to) },
    ],
    does: "Ontologies read it by the sets their versions read: those that read the new set take it, and those that read the old one drop it at their next load.",
    run: "Move",
    target: nameOf(d.document),
  }),
  settle_review: (d) => {
    const r = d.review,
      name = reviewName(r),
      statement = statementText(r.statement, r.link),
      what = r.type && r.prop ? `${r.type}’s ${r.prop}` : "the value";
    const decided: Record<
      AgentDecision["kind"],
      { title: string; does: string; run: string }
    > = {
      link: {
        title: `Link “${name}” to ${d.decision.kind === "link" ? d.decision.entity_key : ""}`,
        does: "The name is this entity in every load from the next one.",
        run: "Link",
      },
      not_in_data: {
        title: `Mark “${name}” as not in the data`,
        does: "The name is no one in the data: later loads leave it unlinked.",
        run: "Mark not in data",
      },
      set_value: {
        title: `Set ${what} to ${d.decision.kind === "set_value" ? valueText(d.decision.value) : ""}`,
        does: "Replaces the property’s value from the next load on, whatever documents or tables say.",
        run: "Set value",
      },
      remove_value: {
        title: `Remove ${what}`,
        does: "Leaves the property without a value from the next load on.",
        run: "Remove value",
      },
      approve: {
        title: "Approve the held statement",
        does: "Writes the statement into the current load now.",
        run: "Approve",
      },
      reject: {
        title: "Reject the held statement",
        does: "Writes nothing, and the review no longer shows.",
        run: "Reject",
      },
    };
    const words = decided[d.decision.kind];
    return {
      title: words.title,
      facts: [
        { label: "Ontology", value: nameOf(d.ontology) },
        ...(d.document
          ? [{ label: "Document", value: nameOf(d.document) }]
          : []),
        {
          label: "Review",
          value: reviewCopy(r.reason, r.candidates?.length).title,
        },
        ...(statement ? [{ label: "Statement", value: statement }] : []),
        ...(r.candidates?.length
          ? [{ label: "Suggested", value: r.candidates.join(", ") }]
          : []),
      ],
      ...opt("quote", r.quote),
      does: words.does,
      ...opt(
        "caution",
        d.decision.kind === "link" && !d.decision.candidate
          ? "This entity isn’t one the review suggested."
          : d.decision.kind === "set_value"
            ? "The decision holds in every later load, even after the tables change."
            : undefined,
      ),
      run: words.run,
      target: nameOf(d.ontology),
    };
  },
};

/**
 * A card's words, from its operation and display.
 *
 * @example
 * cardView({ operation: "create_load", display: { ontology: { id: "o", name: "Sales" }, version: { id: "v", number: 2 } } }).title;
 * // "Load v2 of “Sales”"
 */
export const cardView = (card: AgentCard): CardView =>
  (VIEWS[card.operation] as (d: unknown) => CardView)(card.display);

/**
 * What a card offers, from its action as read and this clock: Run or
 * Dismiss while it waits; Refresh or Dismiss once its confirmation ran out
 * (the WFE says so once read, and the clock before); nothing while it runs;
 * its end once decided. An action this page can't show (an operation it
 * doesn't know, a display that isn't its operation's) is never run from
 * it, only dismissed.
 */
export type CardState =
  | { kind: "waiting"; card: AgentCard }
  | { kind: "unreadable" }
  | { kind: "expired" }
  | { kind: "running" }
  | { kind: "ended"; status: "succeeded" | "failed" | "unknown" | "dismissed" };

/**
 * @example
 * const a = { id: "a", operation: "sync_connector", displayJson: '{"connector":{"id":"c"}}', status: "prepared" as const };
 * cardState({ ...a, expiresAt: "2030-01-01T00:00:00Z" }, Date.parse("2029-01-01")).kind; // "waiting"
 * cardState({ ...a, expiresAt: "2030-01-01T00:00:00Z" }, Date.parse("2031-01-01")).kind; // "expired"
 */
export function cardState(a: FoundryAgentAction, now: number): CardState {
  const status = actionStatus(a.status) ?? "unknown";
  switch (status) {
    case "prepared": {
      const expires = Date.parse(a.expiresAt || "");
      if (!Number.isFinite(expires) || now >= expires)
        return { kind: "expired" };
      const card = parseCard(a.operation || "", a.displayJson);
      return card ? { kind: "waiting", card } : { kind: "unreadable" };
    }
    case "expired":
      return { kind: "expired" };
    case "executing":
      return { kind: "running" };
    default:
      return { kind: "ended", status };
  }
}

/** The ontology an action's target names, `ontology:<workspace>/<id>`. */
const targetOntology = (a: FoundryAgentAction) =>
  /^ontology:[^/]+\/([A-Za-z0-9_-]{1,128})$/.exec(a.target || "")?.[1];

/**
 * Where a ran action's result opens, and the link's words: what it made or
 * changed. Read from the operation's output and the card without trust;
 * undefined when they name nothing to open.
 *
 * @example
 * resultLink({ id: "a", operation: "create_load", target: "ontology:ws/ont_1", resultJson: '{"result":{"id":"load_2"}}' });
 * // { to: "/ontology/ont_1/loads/load_2", label: "Open the load" }
 */
export function resultLink(
  a: FoundryAgentAction,
): { to: string; label: string } | undefined {
  const out = parse(a.resultJson || ""),
    result = isRecord(out) && isRecord(out.result) ? out.result : {},
    made = ids(result.id),
    o = targetOntology(a),
    card = parseCard(a.operation || "", a.displayJson);
  if (!card) return undefined;
  const on = (to: string | false | undefined, label: string) =>
    to ? { to, label } : undefined;
  switch (card.operation) {
    case "create_ontology":
      return on(made && ontologyPaths.ontology(made), "Open the ontology");
    case "generate_ontology":
      return on(
        o && made && ontologyPaths.generation(o, made),
        "Open the draft",
      );
    case "revise_version":
      return on(
        o && made && ontologyPaths.generation(o, made),
        card.display.dry_run ? "Open the preview" : "Open the revision",
      );
    case "set_current_version":
      return on(
        o && made && ontologyPaths.version(o, made),
        "Open the version",
      );
    case "create_load":
    case "set_current_load":
      return on(o && made && ontologyPaths.load(o, made), "Open the load");
    case "add_sample_data":
      return on(o && ontologyPaths.ontology(o), "Open the ontology");
    case "sync_connector":
      return on(
        dataPaths.connection(card.display.connector.id),
        "Open the connection",
      );
    case "create_document_set":
    case "update_document_set":
      return on(dataPaths.connections(), "Open Data");
    case "move_document": {
      const doc = isRecord(result.document)
        ? ids(result.document.id)
        : undefined;
      return on(doc && dataPaths.dataDocument(doc), "Open the document");
    }
    case "settle_review": {
      const doc = card.display.review.doc_id;
      return on(
        o && doc && ID.test(doc) && ontologyPaths.document(o, doc),
        "Open the document",
      );
    }
  }
}

/** The payload as Execute was shown it, indented; the stored text when it isn't JSON. */
export const payloadText = (a: FoundryAgentAction) => {
  const x = parse(a.payloadJson || "");
  return x === undefined ? a.payloadJson || "" : pretty(x);
};

/**
 * The form a connect step links: helix_foundry_connect's `/data?add=<kind>`
 * (its address is the site's, its workspace the thread's), as this app's
 * path; undefined for any other step, or an address that isn't that form.
 *
 * @example
 * connectLink({ id: "t", name: "helix_foundry_connect", input: {}, result: { kind: "returned", output: { url: "https://f.example/data?add=slack&workspace=ws_1", display_name: "Slack" }, truncated: false } });
 * // { to: "/data?add=slack", name: "Slack" }
 */
export function connectLink(
  step: AgentStep,
): { to: string; name: string } | undefined {
  if (step.name !== "helix_foundry_connect" || step.result?.kind !== "returned")
    return undefined;
  const out = step.result.output;
  const url = isRecord(out) ? URL.parse(str(out.url) || "") : null;
  if (!url || !/^https?:$/.test(url.protocol) || url.pathname !== "/data")
    return undefined;
  const to = inAppPath(url);
  if (!to?.startsWith("/data?add=")) return undefined;
  const kind = url.searchParams.get("add")!;
  return {
    to,
    name: (isRecord(out) && str(out.display_name)) || connectorLabel(kind),
  };
}

// ---------------------------------------------------------------- refusals

/**
 * The Assistant's reason for refusing a call (its ErrorInfo), if it gave
 * one of its own.
 */
export function agentRefusal(error: unknown): AgentRefusalReason | undefined {
  if (!(error instanceof ApiError)) return undefined;
  for (const d of error.details || []) {
    const reason = isRecord(d) ? d.reason : undefined;
    const known = AGENT_REFUSALS.find((r) => r === reason);
    if (known) return known;
  }
  return undefined;
}
