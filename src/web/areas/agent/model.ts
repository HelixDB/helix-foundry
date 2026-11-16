import type {
  AgentActionDisplays,
  AgentActionStatus,
  AgentAutomationRef,
  AgentConnectorRef,
  AgentDashboardRef,
  AgentDecision,
  AgentDecisionValue,
  AgentLoadRef,
  AgentNotRunReason,
  AgentQuestion,
  AgentOperation,
  AgentRef,
  AgentReview,
  AgentReviewEntity,
  AgentReviewSettle,
  AgentSpecDisplay,
  AgentVersionRef,
  AgentEvent,
  AgentPageContext,
  AgentRefusalReason,
  AgentStopReason,
  AgentHelperRuntime,
  AgentRuntime,
  FoundryAgentHost,
  FoundryAgentRuntime,
  FoundryAgentTurnStatus,
  FoundryAgentAction,
  ListAgentTurnEventsResponse,
} from "../../../shared/wfe";
import { AGENT_REFUSALS } from "../../../shared/wfe";
import { isRecord } from "../../../shared/json";
import { ApiError } from "../../api";
import { connectorLabel } from "../../kit/SourceLogo";
import { formatDate, plural } from "../../kit/util";
import { automationPaths } from "../automations/paths";
import { dashboardPaths } from "../dashboards/paths";
import { dataPaths } from "../data/paths";
import { reviewCopy } from "../documents/model";
import { entityId } from "../explore/model";
import { LOAD_LIMITS } from "../ontology/model";
import { ontologyPaths } from "../ontology/paths";
import { workspacePaths } from "../workspace/paths";

export type { AgentOperation };

// Founder's dock, as pure functions: what a turn's polled events show,
// what an action's card offers, what page the person is on, and which links
// in the model's text open the app. Everything the WFE sends is parsed
// without trust: the model's text and a tool's output are shown as plain
// text, and only in-app paths become links.

/** The thread the dock shows, per workspace. */
export const threadKey = (workspaceId: string) =>
  `foundry.agent.thread:${workspaceId}`;
/** A message's cap (SendFoundryAgentMessage). */
export const MAX_MESSAGE = 4000;

/**
 * How tall a composer's field is: its text's height, kept between
 * `minRows` and `maxRows` lines (plus its padding), and whether it then
 * scrolls (down only: the field wraps, so it never scrolls sideways).
 * `content` is the field's scrollHeight at height auto, padding included.
 *
 * @example
 * promptHeight({ line: 22, padding: 14, content: 36, minRows: 2, maxRows: 8 });
 * // { height: 58, scrolls: false }
 */
export function promptHeight({
  line,
  padding,
  content,
  minRows,
  maxRows,
}: {
  line: number;
  padding: number;
  content: number;
  minRows: number;
  maxRows: number;
}): { height: number; scrolls: boolean } {
  console.assert(minRows >= 1 && maxRows >= minRows, "rows out of order");
  const min = line * minRows + padding,
    max = line * maxRows + padding;
  // A pixel's rounding is no reason to show a scrollbar.
  return {
    height: Math.max(min, Math.min(content, max)),
    scrolls: content > max + 1,
  };
}

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
 * Explorer or Analyst, and the load they read (a load's page, or `?load=`),
 * a dashboard, or the page that starts a new workflow. Ids only, never
 * names; null anywhere else.
 *
 * @example
 * pageContext("/ontology/ont_1/loads/load_2", ""); // { ontologyId: "ont_1", loadId: "load_2" }
 * pageContext("/explore/ont_1/records", "?load=load_3"); // { ontologyId: "ont_1", loadId: "load_3" }
 * pageContext("/dashboards/dash_1", ""); // { dashboardId: "dash_1" }
 * pageContext("/workflows/new", "?thread=thr_1"); // { newAutomation: true }
 * pageContext("/ontology/new", ""); // null
 * pageContext("/data", ""); // null
 */
export function pageContext(
  pathname: string,
  search: string,
): AgentPageContext | null {
  const [, section, ontologyId = "", sub, loadId = ""] = pathname.split("/");
  if (section === "workflows")
    return ontologyId === "new" && sub === undefined
      ? { newAutomation: true }
      : null;
  if (section === "dashboards")
    return ID.test(ontologyId) && sub === undefined
      ? { dashboardId: ontologyId }
      : null;
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
      `/ontology(?:/${SEG}(?:/connections|/documents(?:/${SEG})?|/(?:loads|versions|generations)/${SEG}|/usage(?:/${SEG}(?:/runs/${SEG})?)?)?)?`,
      `/explore(?:/${SEG}(?:/records|/entities/${SEG})?)?`,
      `/ask(?:/${SEG})?`,
      `/dashboards(?:/${SEG})?`,
      `/workflows(?:/${SEG}(?:/runs/${SEG})?)?`,
      `/data(?:/documents/${SEG}|/connections/${SEG})?`,
      "/settings(?:/(?:ai|members|access|billing|enrichment))?",
    ].join("|") +
    ")$",
);
// A connector kind, in the link helix_foundry_connect gives (/data?add=<kind>).
const KIND = /^[a-z][a-z0-9_]{0,31}$/;
const CANDIDATE =
  /https?:\/\/[^\s<>"'`]+|(?<![\w/])\/(?:ontology|explore|ask|dashboards|workflows|data|settings)(?:[/?#][^\s<>"'`]*)?/g;
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
 * parseAgentEvent('{"type":"text","step":1,"text":"Hi"}'); // { type: "text", step: 1, text: "Hi" }
 * parseAgentEvent('{"type":"text","step":1,"text":3}'); // undefined
 * parseAgentEvent('{"type":"text_delta","step":0,"text":"Hi"}'); // undefined
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
    case "text_delta":
      return typeof x.text === "string" &&
        Number.isSafeInteger(x.step) &&
        (x.step as number) > 0
        ? { type: x.type, step: x.step as number, text: x.text }
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
            ...opt(
              "not_run",
              NOT_RUN.find((r) => r === x.not_run),
            ),
            ...opt("question", questionEvent(x.question)),
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
            ...(x.auto === true && { auto: true }),
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
            ...(x.auto === true && { auto: true }),
          }
        : undefined;
    }
    case "questions": {
      const g = isRecord(x.generation) ? x.generation : null;
      return Array.isArray(x.questions) && x.questions.every(isRecord)
        ? {
            type: "questions",
            // Read by the flow, which trusts none of it.
            questions: x.questions as AgentQuestion[],
            ...(g &&
              str(g.ontology_id) &&
              str(g.generation_id) &&
              Number.isSafeInteger(g.round) && {
                generation: {
                  ontology_id: g.ontology_id as string,
                  ontology_name: str(g.ontology_name) || "",
                  generation_id: g.generation_id as string,
                  ...opt("generation_title", str(g.generation_title)),
                  round: g.round as number,
                },
              }),
          }
        : undefined;
    }
    case "answers":
      return str(x.turn_id) && Array.isArray(x.answers)
        ? {
            type: "answers",
            turn_id: x.turn_id as string,
            answers: x.answers.filter(isRecord).map((a) => ({
              question_id: str(a.question_id) || "",
              options: Array.isArray(a.options)
                ? a.options.filter((o): o is string => typeof o === "string")
                : [],
              ...opt("text", str(a.text)),
            })),
          }
        : undefined;
    case "automation_report":
      return typeof x.summary === "string"
        ? {
            type: "automation_report",
            summary: x.summary,
            ...(typeof x.answer === "boolean" && { answer: x.answer }),
            ...(Number.isSafeInteger(x.count) && { count: x.count as number }),
          }
        : undefined;
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

/**
 * A tool_result's question, every id checked: none when its ontology or its
 * own id isn't one; a load or thread id that isn't one is left out.
 */
function questionEvent(
  x: unknown,
): Extract<AgentEvent, { type: "tool_result" }>["question"] {
  if (!isRecord(x)) return undefined;
  const ontology_id = ids(x.ontology_id),
    id = ids(x.id);
  return ontology_id && id
    ? {
        ontology_id,
        id,
        ...opt("load_id", ids(x.load_id)),
        ...opt("thread_id", ids(x.thread_id)),
        ...opt("status", str(x.status) || undefined),
      }
    : undefined;
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
  // Its questions show, waiting for the person's answers.
  asked: null,
  max_steps: "It stopped at its step limit. Ask it to go on.",
  wall_time: "It stopped at its time limit. Ask it to go on.",
  budget: "It stopped at this message’s spending cap.",
  max_tokens: "Its answer reached its length limit.",
  refusal: "The model declined to answer.",
  context:
    "It ran out of room in the model’s context window. Ask for less at once, or start a new thread.",
  failed: "It failed.",
};
const STOP_REASONS = Object.keys(STOP_TEXT) as AgentStopReason[];

/** Why a call never ran, in words for the person. */
export const NOT_RUN_TEXT: Record<AgentNotRunReason, string> = {
  calls: "One reply runs at most 8 tool calls, and this was past them.",
  asks: "One message asks the Analyst at most 3 questions, and this was past them.",
  time: "Founder ran out of time for this message.",
  length: "The reply reached its length limit while writing it.",
  input: "The model wrote its input in a form Foundry can’t read.",
  asked: "It came after the turn’s questions, which end the turn.",
};
const NOT_RUN = Object.keys(NOT_RUN_TEXT) as AgentNotRunReason[];

/** The Analyst question an ask step's call returned: what the dock draws. */
export type AgentQuestionRef = {
  ontologyId: string;
  questionId: string;
  loadId?: string;
  threadId?: string;
};

/** One tool call and, once it returned, what it gave. */
export type AgentStep = {
  id: string;
  name: string;
  input: unknown;
  result?:
    | {
        kind: "returned";
        output: unknown;
        truncated: boolean;
        // The question it returned, as the event names it (any size).
        question?: AgentQuestionRef;
      }
    | { kind: "failed"; error: string }
    // It never ran: not the tool's failure.
    | { kind: "not_run"; reason: AgentNotRunReason };
};

/** What a turn shows, in order. */
export type AgentBlock =
  | { kind: "text"; text: string }
  | { kind: "step"; step: AgentStep }
  // An action the agent prepared: its card reads the thread's action.
  // `auto`: it ran at once, without a click (the card says how it ended).
  | {
      kind: "card";
      actionId: string;
      operation: string;
      named: boolean;
      auto: boolean;
    }
  // How the person's click ended, opening an action_result turn.
  | {
      kind: "ran";
      actionId: string;
      operation: string;
      status: AgentActionStatus;
      error?: string;
    }
  // The questions the turn asks its person (the agents' words, unread):
  // where they show in the reply.
  | { kind: "asked"; questions: AgentQuestion[]; generation?: string }
  // An automation's step reported what it found (untrusted, the model's).
  | { kind: "report"; summary: string; answer?: boolean; count?: number }
  // A later attempt went on from what the first committed.
  | { kind: "retry"; attempt: number };

/** A turn's events so far, folded. */
export type TurnStream = {
  // The last seq folded in: the next poll reads after it.
  lastSeq: number;
  blocks: AgentBlock[];
  // The text the model is writing in its step's call, after the blocks:
  // its deltas so far, until the step's text supersedes them. One at most,
  // and none once the turn is complete (what a stopped turn left).
  live?: { step: number; text: string };
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
 * blocks stay; a result without its call is a step of its own. A step's
 * deltas add to `live`; any other event ends it, since deltas are written
 * only while a call streams: its text supersedes them (never shown twice),
 * or the stream broke, as it did once the page says the turn isn't running
 * (queued for a retry, or ended). Complete once the page says the turn
 * ended and its events reach its `lastSeq`, with no live text left.
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
      const ev = parseAgentEvent(e.eventJson);
      if (!ev) return { ...s, lastSeq: e.seq, unreadable: s.unreadable + 1 };
      if (ev.type === "text_delta")
        return {
          ...s,
          lastSeq: e.seq,
          live: {
            step: ev.step,
            text: (s.live?.step === ev.step ? s.live.text : "") + ev.text,
          },
        };
      const at = { ...s, lastSeq: e.seq, live: undefined };
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
            ? { kind: "not_run", reason: ev.not_run }
            : ev.is_error
              ? { kind: "failed", error: ev.error || "it failed" }
              : {
                  kind: "returned",
                  output: ev.output,
                  truncated: !!ev.truncated,
                  ...(ev.question && {
                    question: {
                      ontologyId: ev.question.ontology_id,
                      questionId: ev.question.id,
                      ...opt("loadId", ev.question.load_id),
                      ...opt("threadId", ev.question.thread_id),
                    },
                  }),
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
                auto: !!ev.auto,
              });
        case "action_result":
          // One that ran at once ends on its card, not as a line.
          if (ev.auto) return at;
          return add({
            kind: "ran",
            actionId: ev.action_id,
            operation: ev.operation,
            status: ev.status,
            ...(ev.error && { error: ev.error }),
          });
        case "questions":
          return add({
            kind: "asked",
            questions: ev.questions,
            ...opt(
              "generation",
              ev.generation &&
                (ev.generation.generation_title ||
                  `the draft of ${ev.generation.ontology_name || "an ontology"}`),
            ),
          });
        case "answers":
          // The person's answers show as their message (the turn's own).
          return at;
        case "automation_report":
          return add({
            kind: "report",
            summary: ev.summary,
            ...(ev.answer !== undefined && { answer: ev.answer }),
            ...(ev.count !== undefined && { count: ev.count }),
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
  // Deltas stream only while an attempt runs: a turn back in the queue to
  // retry, or ended, broke off what was live.
  const live = page.status === "running" ? next.live : undefined;
  return complete === next.complete && live === next.live
    ? next
    : { ...next, complete, live };
}

/**
 * The operations whose run starts work that goes on after it (a draft, a
 * load, a sync, a workflow's run): a ran one says it started, any other
 * that it succeeded.
 */
export const STARTS_WORK: ReadonlySet<string> = new Set([
  "generate_ontology",
  "revise_version",
  "create_load",
  "sync_connector",
  "add_sample_data",
  "run_workflow",
]);

/** The tool Founder asks its person with: shown as its questions, not a step. */
export const ASK_PERSON = "helix_foundry_ask_person";

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
  helix_foundry_get_assistant_runtime: "Read where Founder runs",
  helix_list_projects: "Listed projects",
  helix_list_databases: "Listed databases",
  helix_foundry_prepare_operation: "Prepared an action",
  helix_foundry_list_dashboards: "Listed the dashboards",
  helix_foundry_get_dashboard: "Read a dashboard",
  helix_foundry_preview_dashboard_tile: "Tried a dashboard tile",
  helix_foundry_list_workflows: "Listed the workflows",
  helix_foundry_get_workflow: "Read a workflow",
  helix_foundry_list_workflow_runs: "Listed a workflow’s runs",
  helix_foundry_get_workflow_run: "Read a workflow’s run",
  helix_foundry_workflow_report: "Reported to its workflow",
  helix_foundry_list_use_cases: "Listed the use cases",
  helix_foundry_create_use_case: "Made a use case",
  helix_foundry_start_use_case_run: "Started a use case run",
  helix_foundry_finish_use_case_run: "Finished a use case run",
  helix_foundry_get_use_case_run: "Read a use case run",
  [ASK_PERSON]: "Asked you",
};

/**
 * What a step's line says: its tool's label, except a prepare that ran at
 * once (a workflow's approved action, or a dashboard the person asked
 * for), which reads "Ran an action".
 *
 * @example
 * stepLabel({ id: "t", name: "helix_foundry_prepare_operation", input: {}, result: { kind: "returned", output: { status: "ran" }, truncated: false } });
 * // "Ran an action"
 * stepLabel({ id: "t", name: "helix_foundry_ask", input: {} });
 * // "Asked the Analyst"
 */
export function stepLabel(step: AgentStep): string {
  const out = step.result?.kind === "returned" ? step.result.output : undefined;
  if (
    step.name === "helix_foundry_prepare_operation" &&
    isRecord(out) &&
    out.status === "ran"
  )
    return "Ran an action";
  return TOOL_LABELS[step.name] || step.name;
}

/** The tools that return an Analyst question: its answer shows under them. */
const QUESTION_TOOLS: ReadonlySet<string> = new Set([
  "helix_foundry_ask",
  "helix_foundry_get_question",
]);

/**
 * The Analyst question a returned ask or get_question step names: the
 * event's `question` (kept however large the output), else the output's
 * `id`, `load_id` and `thread_id` with the input's `ontology_id`, every id
 * checked; undefined for any other step, or one that failed or never ran.
 *
 * @example
 * questionOf({ id: "t", name: "helix_foundry_ask", input: { ontology_id: "ont_1" }, result: { kind: "returned", output: { id: "q_1", load_id: "load_1" }, truncated: false } });
 * // { ontologyId: "ont_1", questionId: "q_1", loadId: "load_1" }
 * questionOf({ id: "t", name: "helix_foundry_ask", input: { ontology_id: "../x" }, result: { kind: "returned", output: { id: "q_1" }, truncated: false } });
 * // undefined
 */
export function questionOf(step: AgentStep): AgentQuestionRef | undefined {
  if (!QUESTION_TOOLS.has(step.name) || step.result?.kind !== "returned")
    return undefined;
  if (step.result.question) return step.result.question;
  const out = step.result.output,
    input = step.input;
  if (!isRecord(out) || !isRecord(input)) return undefined;
  const ontologyId = ids(input.ontology_id),
    questionId = ids(out.id);
  return ontologyId && questionId
    ? {
        ontologyId,
        questionId,
        ...opt("loadId", ids(out.load_id)),
        ...opt("threadId", ids(out.thread_id)),
      }
    : undefined;
}

/**
 * Where each question's answer shows in a turn's blocks: under the last
 * step naming it (the index of its block), so an ask and the get_question
 * that waited on it show the answer once.
 *
 * @example
 * const ask = { kind: "step" as const, step: { id: "a", name: "helix_foundry_ask", input: { ontology_id: "o" }, result: { kind: "returned" as const, output: { id: "q_1" }, truncated: false } } };
 * answerBlocks([ask, { ...ask, step: { ...ask.step, id: "b" } }]); // Set { 1 }
 */
export function answerBlocks(
  blocks: readonly AgentBlock[],
): ReadonlySet<number> {
  const last = new Map<string, number>();
  blocks.forEach((b, i) => {
    const q = b.kind === "step" ? questionOf(b.step) : undefined;
    if (q) last.set(`${q.ontologyId}/${q.questionId}`, i);
  });
  return new Set(last.values());
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
    ? {
        ...r,
        ...opt("kind", str(x.kind) || undefined),
        ...opt("sample", x.sample === true ? (true as const) : undefined),
      }
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

function dashboardRef(x: unknown): AgentDashboardRef | undefined {
  const r = ref(x);
  return r && isRecord(x)
    ? {
        ...r,
        ...opt("pinned", x.pinned === true ? (true as const) : undefined),
        ...opt("published", x.published === true ? (true as const) : undefined),
      }
    : r;
}
const AUTOMATION_STATUSES = ["needs_approval", "active", "paused"] as const;
function automationRef(x: unknown): AgentAutomationRef | undefined {
  const r = ref(x);
  return r && isRecord(x)
    ? {
        ...r,
        ...opt(
          "status",
          AUTOMATION_STATUSES.find((s) => s === x.status),
        ),
      }
    : r;
}
function specDisplay(x: unknown): AgentSpecDisplay | undefined {
  const tiles = isRecord(x)
    ? list(x.tiles ?? [], (t) => {
        const ontology = isRecord(t) ? ref(t.ontology) : undefined,
          title = isRecord(t) ? str(t.title) : undefined;
        return ontology && title !== undefined
          ? {
              title,
              ontology,
              ...opt(
                "visual",
                str((t as { visual?: unknown }).visual) || undefined,
              ),
            }
          : undefined;
      })
    : undefined;
  return tiles ? { tiles } : undefined;
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
  const entities = x.entities === undefined ? [] : list(x.entities, entity);
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
  if (!candidates || !entities || !values) return undefined;
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
    ...opt("entities", entities.length ? entities : undefined),
  };
}
function entity(x: unknown): AgentReviewEntity | undefined {
  if (!isRecord(x)) return undefined;
  const type = str(x.type),
    key = str(x.entity_key),
    norm = str(x.norm_name),
    name = opt("name", str(x.name) || undefined);
  // Exactly one identity.
  if (!type || !key === !norm) return undefined;
  return key
    ? { type, entity_key: key, ...name }
    : { type, norm_name: norm!, ...name };
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
  set_assistant_runtime: (d) => {
    const runtime = runtimeOf(d.runtime),
      current = runtimeOf(d.current);
    return runtime && current && typeof d.online === "boolean"
      ? { runtime, current, online: d.online }
      : undefined;
  },
  revoke_assistant_helper: (d) => {
    const host = ref(d.host),
      runtimes = list(d.runtimes, (r) => helperRuntimeOf(r));
    return host && runtimes && typeof d.online === "boolean"
      ? { host, runtimes, online: d.online }
      : undefined;
  },
  create_dashboard: (d) => {
    const name = str(d.name),
      spec = specDisplay(d.spec);
    return name && spec
      ? {
          name,
          ...opt("description", str(d.description) || undefined),
          spec,
        }
      : undefined;
  },
  update_dashboard: (d) => {
    const dashboard = dashboardRef(d.dashboard),
      name = d.name === undefined ? null : str(d.name) || undefined,
      description = d.description === undefined ? null : str(d.description),
      spec = d.spec === undefined ? null : specDisplay(d.spec);
    if (
      !dashboard ||
      name === undefined ||
      description === undefined ||
      spec === undefined
    )
      return undefined;
    if (name === null && description === null && spec === null)
      return undefined;
    return {
      dashboard,
      ...opt("name", name ?? undefined),
      ...opt("description", description ?? undefined),
      ...opt("spec", spec ?? undefined),
    };
  },
  delete_dashboard: (d) => {
    const dashboard = dashboardRef(d.dashboard);
    return dashboard ? { dashboard } : undefined;
  },
  pin_dashboard: (d) => {
    const dashboard = dashboardRef(d.dashboard);
    return dashboard && typeof d.pinned === "boolean"
      ? { dashboard, pinned: d.pinned }
      : undefined;
  },
  publish_dashboard: (d) => {
    const dashboard = dashboardRef(d.dashboard);
    return dashboard ? { dashboard } : undefined;
  },
  unpublish_dashboard: (d) => {
    const dashboard = dashboardRef(d.dashboard);
    return dashboard ? { dashboard } : undefined;
  },
  create_workflow: (d) => {
    const name = str(d.name);
    return name ? { name } : undefined;
  },
  update_workflow: (d) => {
    const automation = automationRef(d.automation),
      name = d.name === undefined ? null : str(d.name) || undefined;
    if (!automation || name === undefined || typeof d.steps !== "boolean")
      return undefined;
    return { automation, ...opt("name", name ?? undefined), steps: d.steps };
  },
  pause_workflow: (d) => {
    const automation = automationRef(d.automation);
    return automation ? { automation } : undefined;
  },
  resume_workflow: (d) => {
    const automation = automationRef(d.automation);
    return automation ? { automation } : undefined;
  },
  delete_workflow: (d) => {
    const automation = automationRef(d.automation);
    return automation ? { automation } : undefined;
  },
  run_workflow: (d) => {
    const automation = automationRef(d.automation);
    return automation ? { automation } : undefined;
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
  // What it acts on, as Foundry names it: words, or entities each opening
  // its page when the load names it.
  facts: (
    | { label: string; value: string }
    | { label: string; entities: { text: string; to?: string }[] }
  )[];
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
// As the payload names them (generate_ontology's event_sources mode).
const MODES: Record<string, string> = {
  structured: "as rows",
  extraction: "as documents",
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

/**
 * An entity a review names by its stored identity, as the card shows it: by
 * the name the load stored, opening its page, when the review says which
 * entity it is; by the identity alone otherwise.
 *
 * @example
 * const r = { review_key: "r", reason: "unresolved", settle: "resolve" as const, type: "Customer", entities: [{ type: "Customer", entity_key: "scus_1", name: "Globex" }] };
 * reviewEntity("o", r, "scus_1"); // { text: "Globex", to: "/explore/o/entities/Q3VzdG9tZXIAazpzY3VzXzE" }
 * reviewEntity("o", r, "scus_9"); // { text: "scus_9" }
 */
export function reviewEntity(
  ontologyId: string,
  r: AgentReview,
  key: string,
): { text: string; to?: string } {
  const e = r.entities?.find(
    (e) =>
      e.type === r.type &&
      ("entity_key" in e ? e.entity_key : e.norm_name) === key,
  );
  if (!e) return { text: key };
  const identity =
    "entity_key" in e
      ? { type: e.type, key: e.entity_key }
      : { type: e.type, name: e.norm_name };
  return {
    text: e.name || key,
    to: ontologyPaths.entity(ontologyId, entityId(identity)),
  };
}

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
    // The sample's made-up records are only for the sample's own draft.
    ...opt(
      "caution",
      !d.sample && d.connectors.some((c) => c.sample)
        ? `${listOf(d.connectors.filter((c) => c.sample))} is the made-up sample company’s data: this draft would hold its records beside yours.`
        : undefined,
    ),
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
      named = (key: string) => reviewEntity(d.ontology.id, r, key),
      owner = r.entity_key ? named(r.entity_key) : undefined,
      linked =
        d.decision.kind === "link" ? named(d.decision.entity_key) : undefined,
      what =
        r.type && r.prop
          ? `${owner?.to ? owner.text : r.type}’s ${r.prop}`
          : "the value";
    const decided: Record<
      AgentDecision["kind"],
      { title: string; does: string; run: string }
    > = {
      link: {
        title: `Link “${name}” to ${linked?.text}`,
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
        ...(owner ? [{ label: "Entity", entities: [owner] }] : []),
        ...(linked ? [{ label: "Links to", entities: [linked] }] : []),
        ...(r.candidates?.length
          ? [{ label: "Suggested", entities: r.candidates.map(named) }]
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
  set_assistant_runtime: (d) => ({
    title: `Run new Founder threads on ${RUNTIMES[d.runtime].on}`,
    facts: [
      { label: "Now", value: RUNTIMES[d.current].title },
      { label: "From then on", value: RUNTIMES[d.runtime].title },
      ...(d.runtime === "api"
        ? []
        : [
            {
              label: "Helper",
              value: d.online ? "Running" : "Not running",
            },
          ]),
    ],
    does:
      d.runtime === "api"
        ? "New threads run on the workspace’s AI model and count toward its Founder budget. Threads already started keep where they run."
        : `New threads run on ${RUNTIMES[d.runtime].on} through the helper on your computer, at no cost to the workspace. Threads already started keep where they run.`,
    ...opt(
      "caution",
      d.runtime !== "api" && !d.online
        ? `No helper running ${RUNTIMES[d.runtime].cli} is online: start it on your computer before you send.`
        : d.runtime === "codex"
          ? "Codex runs with your own Codex setup, its shell off, on your ChatGPT plan, as you confirmed in Settings › AI models."
          : undefined,
    ),
    run: "Switch",
    target: "this workspace",
  }),
  revoke_assistant_helper: (d) => ({
    title: `Revoke the helper ${quoted(d.host)}`,
    facts: [
      { label: "Helper", value: nameOf(d.host) },
      {
        label: "Runs",
        value: d.runtimes.length
          ? d.runtimes.map((r) => RUNTIMES[r].cli).join(", ")
          : "Nothing yet",
      },
      { label: "Status", value: d.online ? "Running" : "Not running" },
    ],
    does: "It stops running Founder’s turns at once, and a turn it is running ends. To run more, pair it again from Settings › AI models.",
    run: "Revoke",
    target: nameOf(d.host),
  }),
  create_dashboard: (d) => ({
    title: `Make the dashboard “${d.name}”`,
    facts: [
      { label: "Name", value: `“${d.name}” (as Founder wrote it)` },
      ...tileFacts(d.spec),
    ],
    does: "Makes a dashboard of these tiles. Everyone in the workspace sees it, and each reads its tiles as themselves.",
    run: "Create",
    target: d.name,
  }),
  update_dashboard: (d) => ({
    title: `Change the dashboard ${quoted(d.dashboard)}`,
    facts: [
      { label: "Dashboard", value: dashboardText(d.dashboard) },
      ...(d.name !== undefined
        ? [{ label: "New name", value: `“${d.name}”` }]
        : []),
      ...(d.description !== undefined
        ? [
            {
              label: "New description",
              value: d.description ? `“${d.description}”` : "None",
            },
          ]
        : []),
      ...(d.spec ? tileFacts(d.spec) : []),
    ],
    does: "Changes its name, description or tiles to what the full request holds. Everyone who opens it sees the change.",
    run: "Save",
    target: nameOf(d.dashboard),
  }),
  delete_dashboard: (d) => ({
    title: `Delete the dashboard ${quoted(d.dashboard)}`,
    facts: [{ label: "Dashboard", value: dashboardText(d.dashboard) }],
    does: "Deletes the dashboard for everyone in the workspace, and its public link if it has one (only an owner or admin deletes a published one). The data its tiles read stays.",
    run: "Delete",
    target: nameOf(d.dashboard),
  }),
  pin_dashboard: (d) => ({
    title: `${d.pinned ? "Pin" : "Unpin"} the dashboard ${quoted(d.dashboard)} for everyone`,
    facts: [{ label: "Dashboard", value: dashboardText(d.dashboard) }],
    does: "Pinned dashboards lead every member’s list of dashboards, and are the ones people reading through shared sources see.",
    run: d.pinned ? "Pin" : "Unpin",
    target: nameOf(d.dashboard),
  }),
  publish_dashboard: (d) => ({
    title: d.dashboard.published
      ? `Refresh the public snapshot of ${quoted(d.dashboard)}`
      : `Publish the dashboard ${quoted(d.dashboard)} at a public link`,
    facts: [{ label: "Dashboard", value: dashboardText(d.dashboard) }],
    does: "Anyone with the link sees a snapshot of what its tiles read now, as you can read it, without signing in. Nothing else of the workspace shows. The link is on the dashboard’s page, never given to Founder.",
    caution: "Anyone who has the link sees this data.",
    run: "Publish",
    target: nameOf(d.dashboard),
  }),
  unpublish_dashboard: (d) => ({
    title: `Unpublish the dashboard ${quoted(d.dashboard)}`,
    facts: [{ label: "Dashboard", value: dashboardText(d.dashboard) }],
    does: "Its public link stops showing anything at once, for everyone who has it.",
    run: "Unpublish",
    target: nameOf(d.dashboard),
  }),
  create_workflow: (d) => ({
    title: `Set up the workflow “${d.name}”`,
    facts: [{ label: "Name", value: `“${d.name}” (as Founder wrote it)` }],
    does: "Saves this workflow and turns it on. You can pause or change it on its page.",
    cost: "Its Founder steps spend the workspace’s workflows budget.",
    run: "Create and turn on",
    target: d.name,
  }),
  update_workflow: (d) => ({
    title: `Change the workflow ${quoted(d.automation)}`,
    facts: [
      { label: "Workflow", value: automationText(d.automation) },
      ...(d.name !== undefined
        ? [{ label: "New name", value: `“${d.name}”` }]
        : []),
    ],
    does: d.steps
      ? "Saves these changes to the workflow, and your click approves its steps."
      : "Saves these changes to the workflow. Its steps don’t change, so this approves nothing.",
    run: "Save",
    target: nameOf(d.automation),
  }),
  pause_workflow: (d) => ({
    title: `Pause the workflow ${quoted(d.automation)}`,
    facts: [{ label: "Workflow", value: automationText(d.automation) }],
    does: "It stops running when it would start. A run already going finishes.",
    run: "Pause",
    target: nameOf(d.automation),
  }),
  resume_workflow: (d) => ({
    title: `Turn the workflow ${quoted(d.automation)} back on`,
    facts: [{ label: "Workflow", value: automationText(d.automation) }],
    does: "It runs again each time it starts, from now on.",
    run: "Turn on",
    target: nameOf(d.automation),
  }),
  delete_workflow: (d) => ({
    title: `Delete the workflow ${quoted(d.automation)}`,
    facts: [{ label: "Workflow", value: automationText(d.automation) }],
    does: "Deletes the workflow and its runs. Nothing it already did is undone.",
    run: "Delete",
    target: nameOf(d.automation),
  }),
  run_workflow: (d) => ({
    title: `Run the workflow ${quoted(d.automation)} now`,
    facts: [{ label: "Workflow", value: automationText(d.automation) }],
    does: "Runs its approved steps once, now, as you.",
    run: "Run now",
    target: nameOf(d.automation),
  }),
};

/** A workflow as a card names it, with whether it runs. */
const automationText = (a: AgentAutomationRef) =>
  [
    nameOf(a),
    a.status === "active"
      ? "on"
      : a.status === "paused"
        ? "paused"
        : a.status === "needs_approval" && "waits for approval",
  ]
    .filter(Boolean)
    .join(" · ");

/** A dashboard as a card names it, with whether it is pinned and published. */
const dashboardText = (d: AgentDashboardRef) =>
  [
    nameOf(d),
    d.pinned && "pinned for everyone",
    d.published && "at a public link",
  ]
    .filter(Boolean)
    .join(" · ");

/** A spec's tiles as a card's facts: how many, and each with the ontology it reads. */
const tileFacts = (spec: AgentSpecDisplay) => [
  {
    label: "Tiles",
    value: spec.tiles.length
      ? spec.tiles
          .map((t) => `${t.title || "Untitled"} (${nameOf(t.ontology)})`)
          .join(", ")
      : "None",
  },
];

/**
 * A card's words, from its operation and display; `auto` for one that ran
 * at once, without a click (a new dashboard the person asked for), whose
 * card says what was made.
 *
 * @example
 * cardView({ operation: "create_load", display: { ontology: { id: "o", name: "Sales" }, version: { id: "v", number: 2 } } }).title;
 * // "Load v2 of “Sales”"
 * cardView({ operation: "create_dashboard", display: { name: "Invoices", spec: { tiles: [] } } }, true).title;
 * // "Made the dashboard “Invoices”"
 */
export function cardView(card: AgentCard, auto = false): CardView {
  const view = (VIEWS[card.operation] as (d: unknown) => CardView)(
    card.display,
  );
  return auto && card.operation === "create_dashboard"
    ? {
        ...view,
        title: `Made the dashboard “${card.display.name}”`,
        does: "Founder made it because you asked for a dashboard. It isn’t pinned or published, and Undo deletes it.",
      }
    : view;
}

/**
 * The dashboard a ran create_dashboard made, from its output; undefined
 * for any other action, or an output that names none.
 *
 * @example
 * madeDashboard({ id: "a", operation: "create_dashboard", resultJson: '{"result":{"id":"dash_1"}}' }); // "dash_1"
 * madeDashboard({ id: "a", operation: "create_load", resultJson: '{"result":{"id":"load_1"}}' }); // undefined
 */
export function madeDashboard(a: FoundryAgentAction): string | undefined {
  if (a.operation !== "create_dashboard") return undefined;
  const out = parse(a.resultJson || "");
  return isRecord(out) && isRecord(out.result) ? ids(out.result.id) : undefined;
}

/**
 * How an action that ran at once stands, its card's words: making while it
 * runs (or before its record says it ran), made, undone once what it made
 * is `gone` (Undo, or deleted since), failed, or unknown.
 */
export type AutoState = "making" | "made" | "undone" | "failed" | "unknown";

/**
 * @example
 * autoState({ id: "a", status: "executing" }, false); // "making"
 * autoState({ id: "a", status: "succeeded" }, true); // "undone"
 */
export function autoState(a: FoundryAgentAction, gone: boolean): AutoState {
  switch (actionStatus(a.status)) {
    case "prepared":
    case "executing":
      return "making";
    case "succeeded":
      return gone ? "undone" : "made";
    case "failed":
      return "failed";
    default:
      return "unknown";
  }
}

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
    case "set_assistant_runtime":
    case "revoke_assistant_helper":
      return on(workspacePaths.settings("ai"), "Open Settings › AI models");
    case "create_dashboard":
      return on(made && dashboardPaths.dashboard(made), "Open the dashboard");
    case "update_dashboard":
    case "pin_dashboard":
    case "publish_dashboard":
    case "unpublish_dashboard":
      return on(
        dashboardPaths.dashboard(card.display.dashboard.id),
        "Open the dashboard",
      );
    case "delete_dashboard":
    case "delete_workflow":
      return undefined;
    case "create_workflow":
      return on(made && automationPaths.automation(made), "Open the workflow");
    case "update_workflow":
    case "pause_workflow":
    case "resume_workflow":
      return on(
        automationPaths.automation(card.display.automation.id),
        "Open the workflow",
      );
    case "run_workflow":
      return on(
        made && automationPaths.automationRun(card.display.automation.id, made),
        "Open the run",
      );
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

// ---------------------------------------------------------------- runtime

/**
 * Where Founder runs a thread, as people name it: its title (a
 * setting's), what it runs "on" in a sentence, and the CLI a helper drives.
 */
export const RUNTIMES: Record<
  AgentRuntime,
  { title: string; on: string; cli: string; login: string }
> = {
  api: {
    title: "Foundry’s AI model",
    on: "the workspace’s AI model",
    cli: "",
    login: "",
  },
  claude_code: {
    title: "My Claude subscription",
    on: "your Claude subscription",
    cli: "Claude Code",
    login: "run claude, then /login",
  },
  codex: {
    title: "My ChatGPT subscription (Codex)",
    on: "your ChatGPT subscription",
    cli: "Codex",
    login: "run codex login",
  },
};

const runtimeOf = (x: unknown): AgentRuntime | undefined =>
  x === "api" || x === "claude_code" || x === "codex" ? x : undefined;
const helperRuntimeOf = (x: unknown): AgentHelperRuntime | undefined =>
  x === "claude_code" || x === "codex" ? x : undefined;

/**
 * The helper's package, as the backend's agent.PairCommand pins it: the
 * command that runs it, and the one that keeps it running at every login.
 */
export const HELPER_PACKAGE = "@helixdb/foundry-agent-host@0.1.0";
export const HELPER_COMMANDS = {
  run: `npx -y ${HELPER_PACKAGE} run`,
  install: `npx -y ${HELPER_PACKAGE} install`,
} as const;

/**
 * What a helper's CLI is signed in to, as the settings say it: a closed
 * set read without trust from its accounts (the runtime's, if any), with
 * the CLI's version when it said one. An outdated Codex runs nothing,
 * whatever it is signed in to.
 */
export type HelperAccount = (
  | { kind: "subscription"; plan: string }
  | { kind: "other" }
  | { kind: "signed_out" }
  | { kind: "unknown" }
  // Not said yet: it hasn't called since it paired.
  | { kind: "unchecked" }
  // A Codex older than Foundry runs: it runs no turn until updated.
  | { kind: "outdated" }
) & { version?: string };

/** A version as the settings show it: printable, short. */
const versionOf = (v: unknown) =>
  typeof v === "string" && /^[\x20-\x7e]{1,64}$/.test(v) ? v : undefined;

/**
 * @example
 * const h = { id: "h", accounts: [{ runtime: "claude_code" as const, state: "signed_in" as const, subscription: "max" }] };
 * helperAccount(h, "claude_code"); // { kind: "subscription", plan: "max" }
 * helperAccount({ id: "h" }, "claude_code"); // { kind: "unchecked" }
 * helperAccount({ id: "h", accounts: [{ runtime: "codex", state: "signed_in", version: "0.150.0", outdated: true }] }, "codex");
 * // { kind: "outdated", version: "0.150.0" }
 */
export function helperAccount(
  host: FoundryAgentHost,
  runtime: AgentHelperRuntime,
): HelperAccount {
  const a = (host.accounts || []).find((x) => x.runtime === runtime);
  const version = versionOf(a?.version);
  const v = version ? { version } : {};
  if (a?.outdated) return { kind: "outdated", ...v };
  switch (a?.state) {
    case "signed_in":
      return a.subscription
        ? { kind: "subscription", plan: a.subscription, ...v }
        : { kind: "other", ...v };
    case "signed_out":
      return { kind: "signed_out", ...v };
    case "unknown":
      return { kind: "unknown", ...v };
    default:
      return { kind: "unchecked" };
  }
}

/**
 * A subscription's plan as people name it: "max" is "Max", "team" "Team".
 *
 * @example
 * planName("max"); // "Max"
 * planName("claude_max_20x"); // "Claude max 20x"
 */
export const planName = (plan: string) => {
  const words = plan.replace(/[_-]+/g, " ").trim();
  return words.charAt(0).toUpperCase() + words.slice(1);
};

/** How to update Codex, as the settings and the dock say it. */
export const CODEX_UPDATE = "npm i -g @openai/codex@latest";

/**
 * A helper's account in words, and whether its turns would bill the
 * subscription: its plan, or why not. Codex says its version and its plan
 * as ChatGPT names it; min is the oldest Codex Foundry runs.
 *
 * @example
 * accountText({ kind: "subscription", plan: "max" }, "claude_code");
 * // { text: "Claude Code is signed in on a Max subscription", ok: true }
 * accountText({ kind: "subscription", plan: "plus", version: "0.169.0" }, "codex");
 * // { text: "Codex 0.169.0 is signed in on a ChatGPT Plus plan", ok: true }
 */
export function accountText(
  a: HelperAccount,
  runtime: AgentHelperRuntime,
  min = "",
): { text: string; ok: boolean } {
  const { cli, login } = RUNTIMES[runtime];
  const named = a.version ? `${cli} ${a.version}` : cli;
  switch (a.kind) {
    case "subscription":
      return {
        text:
          runtime === "codex"
            ? `${named} is signed in on a ChatGPT ${planName(a.plan)} plan`
            : `${named} is signed in on a ${planName(a.plan)} subscription`,
        ok: true,
      };
    case "other":
      return {
        text:
          runtime === "codex"
            ? `${named} is signed in, but not with a ChatGPT plan: its turns bill that account`
            : `${named} is signed in, but not on a subscription: its turns bill that account`,
        ok: false,
      };
    case "signed_out":
      return {
        text: `${named} isn’t signed in: on that computer, ${login}`,
        ok: false,
      };
    case "unknown":
      return {
        text: `${named} didn’t start, so its sign-in is unknown`,
        ok: false,
      };
    case "outdated":
      return {
        text: `${named} is older than ${min || "Foundry runs"}${min ? ", the oldest Foundry runs" : ""}: on that computer, update it (${CODEX_UPDATE})`,
        ok: false,
      };
    case "unchecked":
      return { text: `${cli}’s sign-in isn’t known yet`, ok: true };
  }
}

/**
 * Whether a thread can be sent to where it runs: a closed set. `ready` is
 * Foundry's worker, or a helper's runtime with a helper running it;
 * `stopped` a helper's with one paired that runs it but none running (start
 * it); `outdated` a helper's whose helper runs but with a Codex too old
 * (update it); `unstarted` a helper's whose helper runs but whose Codex
 * didn't start, so said no version (see why on that computer); `unpaired` a
 * helper's that no paired helper runs (pair one first: starting one would
 * fail).
 */
export type RuntimeState =
  | "ready"
  | "stopped"
  | "outdated"
  | "unstarted"
  | "unpaired";

/** Where a thread runs, whether it can be sent to, and, when its CLI is too old or didn't start, which helper's (and how old). */
export type ThreadRuntime = {
  runtime: AgentRuntime;
  state: RuntimeState;
  helper?: { host: string; version?: string; min: string };
};

/**
 * Where a thread runs, and whether it can be sent to now: api always; a
 * helper's while a helper that runs it is online (the WFE refuses it
 * otherwise, FOUNDRY_AGENT_HOST_OFFLINE). `setting` is the workspace's,
 * read on a local install only; unknown (undefined) when it can't be read.
 *
 * @example
 * threadRuntime("claude_code", { runtime: "api", online: [] });
 * // { runtime: "claude_code", state: "unpaired" }
 * threadRuntime("claude_code", { runtime: "api", online: [], hosts: [{ id: "h", runtimes: ["claude_code"] }] });
 * // { runtime: "claude_code", state: "stopped" }
 * threadRuntime(undefined, { runtime: "claude_code", online: ["claude_code"] });
 * // { runtime: "claude_code", state: "ready" }
 * threadRuntime(undefined, undefined); // { runtime: "api", state: "ready" }
 */
export function threadRuntime(
  thread: AgentRuntime | undefined,
  setting: FoundryAgentRuntime | undefined,
): ThreadRuntime {
  const runtime = thread ?? setting?.runtime ?? "api";
  // Unread: not known to be offline, so the dock doesn't say it is.
  if (runtime === "api" || !setting || (setting.online || []).includes(runtime))
    return { runtime, state: "ready" };
  const hosts = setting.hosts || [];
  // A helper running whose Codex is too old (updating it is what's
  // wanted), or didn't start and said no version (Foundry runs neither).
  const blocked = (h: FoundryAgentHost) => {
    const a = helperAccount(h, runtime);
    return a.kind === "outdated"
      ? "outdated"
      : runtime === "codex" && a.kind === "unknown" && !a.version
        ? "unstarted"
        : undefined;
  };
  const stuck = hosts.find((h) => h.online && blocked(h));
  if (stuck)
    return {
      runtime,
      state: blocked(stuck)!,
      helper: {
        host: stuck.name || "your computer",
        version: helperAccount(stuck, runtime).version,
        min: setting.codexMinVersion || "",
      },
    };
  return {
    runtime,
    state: hosts.some((h) => (h.runtimes || []).includes(runtime) || blocked(h))
      ? "stopped"
      : "unpaired",
  };
}

// ---------------------------------------------------------------- refusals

/**
 * Founder's reason for refusing a call (its ErrorInfo), if it gave
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
