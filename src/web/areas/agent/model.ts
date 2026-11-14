import type {
  AgentActionDisplay,
  AgentActionStatus,
  AgentEvent,
  AgentPageContext,
  AgentRefusalReason,
  AgentStopReason,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  ListAgentTurnEventsResponse,
} from "../../../shared/wfe";
import { AGENT_REFUSALS } from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";
import { ApiError } from "../../api";
import { LOAD_LIMITS } from "../ontology/model";
import { ontologyPaths } from "../ontology/paths";

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

// The app's sections a link may open; nothing else is ever a link.
const IN_APP = /^\/(ontology|explore|ask|data|settings)(\/[\w.~%-]+)*\/?$/;
const CANDIDATE =
  /https?:\/\/[^\s<>"'`]+|(?<![\w/])\/(?:ontology|explore|ask|data|settings)(?:[/?#][^\s<>"'`]*)?/g;
const TRAILING = /[.,;:!?)\]'"]+$/;

/**
 * The model's text with the links that open this app made links: an
 * address on `origin`, or a path, under one of the app's sections. Any
 * other address stays text, never a link, so text can't send the person
 * elsewhere (or leak what it read through an address).
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
      url = URL.parse(raw, origin);
    if (!url || url.origin !== origin || !IN_APP.test(url.pathname)) continue;
    if (start > from)
      parts.push({ kind: "text", text: text.slice(from, start) });
    parts.push({
      kind: "link",
      text: raw,
      to: url.pathname + url.search + url.hash,
    });
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
            display: parseDisplay(x.display),
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
    | { kind: "failed"; error: string };
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
          const result: AgentStep["result"] = ev.is_error
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

/** How a card words what it runs; one entry per operation the agent may prepare. */
type OperationCopy = {
  title: (d: AgentActionDisplay) => string;
  does: string;
  cost: string;
  run: string;
};

/** The operations the agent may prepare (backend agent.Operations), with each card's fixed words. */
export const OPERATIONS = {
  create_load: {
    title: (d) =>
      `Load ${d.version ? `v${d.version}` : "a version"} of ${d.ontology_name || "an ontology"}`,
    does: "Writes this version’s tables and documents into the ontology’s database, beside its other loads. The newest load that finishes ready becomes current.",
    cost: `Reading documents costs at most $${LOAD_LIMITS.jobUsd} for this load and $${LOAD_LIMITS.monthUsd} a month for the workspace.`,
    run: "Load",
  },
} as const satisfies Record<string, OperationCopy>;
export type AgentOperation = keyof typeof OPERATIONS;
export const isAgentOperation = (x: string): x is AgentOperation =>
  Object.hasOwn(OPERATIONS, x);

/** Display JSON as the card may show it: only the fields Foundry names, each checked. */
export function parseDisplay(x: unknown): AgentActionDisplay {
  const v = typeof x === "string" ? parse(x) : x;
  if (!isRecord(v)) return {};
  return {
    ...(str(v.ontology_id) && ID.test(v.ontology_id as string)
      ? { ontology_id: v.ontology_id as string }
      : {}),
    ...(str(v.ontology_name)
      ? { ontology_name: v.ontology_name as string }
      : {}),
    ...(str(v.version_id) && ID.test(v.version_id as string)
      ? { version_id: v.version_id as string }
      : {}),
    ...(Number.isInteger(v.version) && (v.version as number) > 0
      ? { version: v.version as number }
      : {}),
  };
}

/**
 * What a card offers, from its action as read and this clock: Run or
 * Dismiss while it waits; Refresh or Dismiss once its confirmation ran out
 * (the WFE says so once read, and the clock before); nothing while it runs;
 * its end once decided. An operation this page doesn't know is never run
 * from it, only dismissed.
 */
export type CardState =
  | { kind: "waiting"; operation: AgentOperation }
  | { kind: "unknown_operation" }
  | { kind: "expired" }
  | { kind: "running" }
  | { kind: "ended"; status: "succeeded" | "failed" | "unknown" | "dismissed" };

/**
 * @example
 * cardState({ id: "a", operation: "create_load", status: "prepared", expiresAt: "2030-01-01T00:00:00Z" }, Date.parse("2029-01-01"));
 * // { kind: "waiting", operation: "create_load" }
 * cardState({ id: "a", operation: "create_load", status: "prepared", expiresAt: "2030-01-01T00:00:00Z" }, Date.parse("2031-01-01"));
 * // { kind: "expired" }
 */
export function cardState(a: FoundryAgentAction, now: number): CardState {
  const status = actionStatus(a.status) ?? "unknown";
  switch (status) {
    case "prepared": {
      const expires = Date.parse(a.expiresAt || "");
      if (!Number.isFinite(expires) || now >= expires)
        return { kind: "expired" };
      const op = a.operation || "";
      return isAgentOperation(op)
        ? { kind: "waiting", operation: op }
        : { kind: "unknown_operation" };
    }
    case "expired":
      return { kind: "expired" };
    case "executing":
      return { kind: "running" };
    default:
      return { kind: "ended", status };
  }
}

/** The ontology an action acts on: Foundry's name for it, else its target's id. */
export function actionOntology(a: FoundryAgentAction): string | undefined {
  const d = parseDisplay(a.displayJson);
  if (d.ontology_id) return d.ontology_id;
  const m = /^ontology:[^/]+\/([A-Za-z0-9_-]{1,128})$/.exec(a.target || "");
  return m?.[1];
}

/**
 * Where a ran action's result opens: a created load's page. Read from the
 * operation's output without trust; undefined when it names none.
 *
 * @example
 * resultLink({ id: "a", operation: "create_load", target: "ontology:ws/ont_1", resultJson: '{"result":{"id":"load_2"}}' });
 * // "/ontology/ont_1/loads/load_2"
 */
export function resultLink(a: FoundryAgentAction): string | undefined {
  const out = parse(a.resultJson || ""),
    ontology = actionOntology(a);
  const result = isRecord(out) && isRecord(out.result) ? out.result : undefined;
  const id = str(result?.id);
  if (!ontology || !id || !ID.test(id)) return undefined;
  switch (a.operation) {
    case "create_load":
      return ontologyPaths.load(ontology, id);
    default:
      return undefined;
  }
}

/** The payload as Execute was shown it, indented; the stored text when it isn't JSON. */
export const payloadText = (a: FoundryAgentAction) => {
  const x = parse(a.payloadJson || "");
  return x === undefined ? a.payloadJson || "" : pretty(x);
};

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
