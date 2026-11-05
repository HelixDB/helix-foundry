import type {
  AskCitation,
  AskEvent,
  AskQuestionRequest,
  AskResult,
  AskUsage,
  CitationTarget,
  FoundryQuestion,
  ListQuestionEventsResponse,
  ListQuestionsResponse,
  AskVisual,
  OntologyQuery,
  QueryOutput,
  QuestionBudget,
  QuestionEvent,
  QuestionResponse,
  QuestionStatus,
  VersionOf,
} from "../../src/shared/wfe";
import { entityRef, type EntityRef } from "../../src/shared/foundry-read";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_LOAD,
  FOUNDRY_NODES as N,
  isFoundryLabel,
} from "../../src/shared/foundry-labels";
import { ENRICHMENT_SOURCE } from "../../src/web/areas/ontology/enrichment-fields";
import { contractErrors } from "../agent-contract";
import { LoadGraph, type MockNode } from "../graph/store";
import { compile, evaluate, answer as readAnswer } from "../graph/rules";
import { viewOf, type View } from "../graph/view";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRoute,
  type MockSeen,
} from "../http";
import { PROD_STAMP, config, onReset } from "../state";
import { tenantGraph } from "./databases";
import { currentLoadId, ontologyLoad } from "./loads";
import { ontologyVersion } from "./ontology";
import { aiModel, aiProvider, aiRefusal } from "./ai";
import { isScope, ontologyOf, type StoredOntology } from "./ontologies";

// An ontology's questions, as the backend's API and foundry-ask-worker
// run them: a question about one ready or partial
// load (the current one when unset) is queued, claimed oldest first once
// its workspace runs fewer than 2 and the month's questions budget can hold
// its reservation ($0.50 of $30 a month; else it waits, queued), runs as
// the engine's Ask stream (events the page polls by afterSeq, with the
// question's status and last seq), and ends with the engine's result, its
// spend settled. Ask is refused past 30 questions an hour per workspace
// (RESOURCE_EXHAUSTED), and once what the month spent plus a question's cap
// passes the budget (FAILED_PRECONDITION), with the WFE's messages. A grantee
// also has limits of its own (`GRANTEE_LIMITS`, its live questions counting at
// their caps), which the list's budget gives instead of the workspace's, and
// its refusals don't tell it what the workspace asked or spent.
//
// The mock's agent answers from what the load wrote, in the engine's
// shapes exactly (contract/ontology_agent/ask_event.json and
// ask_result.json, as tests/unit/questions-mock.test.ts checks): the first
// fact a document quotes (the seeded contract's signed_with), its two
// entities, the fact, the quote and the table row behind the second, the
// co-signer the rules derive, the first record of a constraint the load
// breaks, and the query that found them. When the load holds invoices, it
// also counts them by status and ranks the largest, and its answer shows
// both after the paragraphs that introduce them (contract 3.11.0 visuals):
// a breakdown, drawn as bars, and a ranking of rows, asked as bars. Every
// query's output is computed from the load as the engine records it: a
// row's entities as {type, id, name}, a document's values as
// {"untrusted": …}, and every group's value as {"untrusted": …} too. Every
// question starts, as the engine's do, with its read of what the load holds
// (`load_profile`, recorded as e1 before the model's first call). The
// model's first call fails (a lookup by identifier the engine can't run
// yet); near the end it counts the subject's type with a graph query of its
// own (`helix_query`, a Helix v3 JSON read: no query record holds it),
// which the answer cites, then reads the subject's history (`history_of`),
// whose signed_on version it cites with when it held by valid time (contract
// 4.7.0's `valid`: the day the contract gives, as that `day`, `from` the
// next midnight UTC). A grantee asks through a view, even holding
// everything, and is offered neither: no graph query, no history. As the
// engine's, their events carry a summary and
// an evidence id, never what they read. Its first answer is refused (it
// cites an entity the evidence read didn't show) and its second, the last
// call, ends the question, its result carrying neither evidence nor error.
// Like the model, it names entities by handle (n1, n2, n3). Each event
// takes `askStepMs`.
// As the WFE will, question lists leave the queries' outputs out of
// `resultJson`; GET of one question keeps them.
// Test knobs through POST /__mock/config: `askOutcome` "cannot_answer" (a
// decline, which from 4.7.0 always says its `kind`: the missing signed_by
// link is undeclared_relation, and an asker who reads part of the load,
// asking about another time, is uncovered),
// "budget_exhausted" (stops at this question's cap) or "failed" (the model
// failed); `askMarkup` puts links, an image and HTML in the answer, as a
// misbehaving model might; `askVisuals: false` answers as a 3.10.0 engine
// did, without outputs or visuals; `askSpentUsdMicros` is what the month
// spent before.

type Outcome = "answered" | "cannot_answer" | "budget_exhausted" | "failed";
type Knobs = {
  askStepMs?: number;
  askOutcome?: Outcome;
  askMarkup?: boolean;
  askVisuals?: boolean;
  askSpentUsdMicros?: number;
  askGranteeSpentUsdMicros?: number;
};
const knobs = config as typeof config & Knobs;

// foundry.ask's grantee_monthly_usd and grantee_per_hour.
const GRANTEE_LIMITS = { monthlyUsdMicros: 10_000_000, hourly: 10 };
// foundry.ask: the workspace's month (dev's $30, or prod's $15 with
// MOCK_STAMP=prod) and a question's caps, the same on both.
export const ASK_LIMITS = {
  monthlyUsdMicros: PROD_STAMP ? 15_000_000 : 30_000_000,
  questionUsdMicros: 500_000,
  maxSteps: 20,
  maxWallSecs: 240,
  concurrent: 2,
  hourly: 30,
} as const;
const STEP_MS = 700,
  MODEL = "claude-sonnet-5-5",
  PAGE = 50;

/** What the agent will do: its events, then how it ends. */
type Script = {
  events: AskEvent[];
  status: QuestionStatus;
  result: AskResult | null;
  error: string;
  usdMicros: number;
};

/** A foundry_questions row, with its events. */
type StoredQuestion = {
  id: string;
  workspaceId: string;
  ontologyId: string;
  threadId: string;
  loadId: string;
  versionId: string;
  question: string;
  // Its EntityRef as canonical JSON, "" for none.
  focusJson: string;
  clientToken: string;
  askedByUserId: string;
  // The principal a grantee asked as (./access.ts); unset for anyone else.
  // A grantee sees only its own questions, as the backend's samePrincipal.
  grantee?: string;
  // What that grantee held of the load, when it wasn't all of it: its answer
  // reads while its grants still hold that.
  seen?: MockSeen;
  createdAt: string;
  status: QuestionStatus;
  attempts: number;
  // When it was claimed, in ms; 0 while queued.
  claimedAt: number;
  // When a claim last found its workspace too busy for it, in ms.
  heldAt: number;
  stepMs: number;
  script: Script;
  events: QuestionEvent[];
  resultJson: string;
  error: string;
  usdMicros: number;
  reservedMicros: number;
  startedAt: string | null;
  finishedAt: string | null;
};

const state = { questions: [] as StoredQuestion[] };
onReset(() => {
  state.questions = [];
  delete knobs.askStepMs;
  delete knobs.askOutcome;
  delete knobs.askMarkup;
  delete knobs.askVisuals;
  delete knobs.askSpentUsdMicros;
  delete knobs.askGranteeSpentUsdMicros;
});

const live = (q: StoredQuestion) =>
  q.status === "queued" || q.status === "running";
const month = () => new Date().toISOString().slice(0, 7);
const ofWorkspace = (workspaceId: string) =>
  state.questions.filter((q) => q.workspaceId === workspaceId);

/**
 * The month's spend and the claims held back, in millionths of a dollar: the
 * workspace's, or one grantee's.
 */
export function spend(workspaceId: string, grantee?: string) {
  const mine = ofWorkspace(workspaceId).filter(
    (q) =>
      q.createdAt.startsWith(month()) && (!grantee || q.grantee === grantee),
  );
  return {
    spent:
      (grantee
        ? knobs.askGranteeSpentUsdMicros || 0
        : knobs.askSpentUsdMicros || 0) +
      mine.reduce((n, q) => n + q.usdMicros, 0),
    reserved: mine.reduce((n, q) => n + q.reservedMicros, 0),
  };
}

/** What the list says of the budget: a grantee's own, or the workspace's. */
function budget(workspaceId: string, grantee?: string): QuestionBudget {
  const { spent, reserved } = spend(workspaceId, grantee);
  return {
    monthlyUsdMicros: String(
      grantee ? GRANTEE_LIMITS.monthlyUsdMicros : ASK_LIMITS.monthlyUsdMicros,
    ),
    spentUsdMicros: String(spent),
    reservedUsdMicros: String(reserved),
    questionUsdMicros: String(ASK_LIMITS.questionUsdMicros),
    maxSteps: ASK_LIMITS.maxSteps,
    maxWallSecs: ASK_LIMITS.maxWallSecs,
    concurrentLimit: ASK_LIMITS.concurrent,
    hourlyLimit: grantee ? GRANTEE_LIMITS.hourly : ASK_LIMITS.hourly,
  };
}

/** Why the workspace and, for a grantee, its own limits refuse a new question. */
function askRefusal(workspaceId: string, grantee?: string) {
  const hourAgo = Date.now() - 3_600_000,
    asked = ofWorkspace(workspaceId).filter(
      (q) => Date.parse(q.createdAt) >= hourAgo,
    ),
    fits = (spent: number, cap: number) =>
      spent + ASK_LIMITS.questionUsdMicros <= cap;
  if (asked.length >= ASK_LIMITS.hourly)
    return fail(
      "RESOURCE_EXHAUSTED",
      grantee
        ? "too many questions have been asked here in the last hour; try again later"
        : `the workspace has asked ${asked.length} questions in the last hour; try again later`,
    );
  if (!fits(spend(workspaceId).spent, ASK_LIMITS.monthlyUsdMicros))
    return fail(
      "FAILED_PRECONDITION",
      grantee
        ? "this workspace's questions are used up for the month"
        : SPENT_MONTH,
    );
  if (!grantee) return undefined;
  const ownHour = asked.filter((q) => q.grantee === grantee).length;
  if (ownHour >= GRANTEE_LIMITS.hourly)
    return fail(
      "RESOURCE_EXHAUSTED",
      `you have asked ${ownHour} questions in the last hour; try again later`,
    );
  // Its live questions count at their caps.
  const liveCaps =
    ofWorkspace(workspaceId).filter((q) => q.grantee === grantee && live(q))
      .length * ASK_LIMITS.questionUsdMicros;
  return fits(
    spend(workspaceId, grantee).spent + liveCaps,
    GRANTEE_LIMITS.monthlyUsdMicros,
  )
    ? undefined
    : fail(
        "FAILED_PRECONDITION",
        `you have spent this month's $${(GRANTEE_LIMITS.monthlyUsdMicros / 1e6).toFixed(2)} of questions`,
      );
}

// ---------------------------------------------------------------- the agent

const refOf = (n: MockNode): EntityRef => ({
  type: n.label,
  id: String(n.props.entity_key ?? n.props.norm_name ?? n.id),
});
const nameOf = (n: MockNode) => String(n.props.name ?? refOf(n).id);

/** The node an edge of `label` leads to from `from`, in the load. */
function along(g: LoadGraph, from: number, label: string) {
  for (const e of g.graph.outgoing.get(from) || []) {
    const edge = g.graph.edges.get(e);
    if (edge?.label === label) return g.node(edge.to);
  }
  return undefined;
}

/** The first quoted fact the load holds, with what cites it. */
function quotedFact(g: LoadGraph) {
  for (const ev of g.nodes()) {
    if (ev.label !== N.Evidence || typeof ev.props.quote !== "string") continue;
    const section = along(g, ev.id, E.FROM_SECTION);
    const fact = g.findEdge(
      String(ev.props.link),
      "fact_key",
      ev.props.fact_key ?? "",
    );
    const subject = fact && g.node(fact.from),
      object = fact && g.node(fact.to);
    if (!section || !fact || !subject || !object) continue;
    // The table row that states the object, if a row does: a synced
    // table's, since Ask cites a People enrichment profile without its key.
    const row = [...g.nodes()].find(
      (r) =>
        r.label === N.Evidence &&
        r.props.row_key !== undefined &&
        r.props.fact_key === undefined &&
        r.props.source !== ENRICHMENT_SOURCE &&
        along(g, r.id, E.ABOUT)?.id === object.id,
    );
    return { ev, section, link: fact.label, subject, object, row };
  }
  return undefined;
}

/** The first stored violation record of the load, and how many its constraint has. */
function firstBreak(g: LoadGraph) {
  const all = [...g.nodes()].filter((n) => n.label === N.Violation);
  const first = all
    .map((n) => ({
      constraint: String(n.props.constraint),
      key: String(n.props.violation_key),
    }))
    .sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))[0];
  return (
    first && {
      ...first,
      count: all.filter((n) => n.props.constraint === first.constraint).length,
    }
  );
}

// ---------------------------------------------------------------- outputs

/** An entity as a row's cell gives it: its type, id and the name it has. */
const entityCell = (n: MockNode) => ({ ...refOf(n), name: nameOf(n) });

/** A prop's value as an output gives it: a document's is untrusted text. */
function valueOf(n: MockNode, prop: string): unknown {
  const v = n.props[prop] ?? null;
  return v !== null && String(n.props.foundry_part ?? "").endsWith(":documents")
    ? { untrusted: String(v) }
    : v;
}

/** Each `link` edge of the load from a `from` node to a `to` node. */
function pairs(g: LoadGraph, from: string, link: string, to: string) {
  return [...g.nodes()]
    .filter((n) => n.label === from)
    .flatMap((a) =>
      (g.graph.outgoing.get(a.id) || []).flatMap((e) => {
        const edge = g.graph.edges.get(e),
          b = edge?.label === link ? g.node(edge.to) : undefined;
        return b?.label === to ? [[a, b] as const] : [];
      }),
    );
}

/** A row's edge for a fact a step walked. */
const factEdge = (step: number, link: string, a: MockNode, b: MockNode) => ({
  step,
  fact: { link, subject: refOf(a), object: refOf(b) },
});

const byEntity = (a: MockNode, b: MockNode) =>
  refOf(a).id < refOf(b).id ? -1 : refOf(a).id > refOf(b).id ? 1 : 0;

/**
 * The invoices of the load by status, and the 10 largest with who they
 * bill, as the agent's two last queries ask and the engine records them;
 * undefined when the load holds no invoices.
 */
function invoiceQueries(g: LoadGraph) {
  const invoices = [...g.nodes()].filter((n) => n.label === "Invoice");
  if (!invoices.length) return undefined;
  const counts = new Map<unknown, number>();
  for (const n of invoices)
    counts.set(
      n.props.status ?? null,
      (counts.get(n.props.status ?? null) || 0) + 1,
    );
  // Most first, then by value, those without one last.
  const groups = [...counts]
    .map(([value, count]) => ({ value, count }))
    .sort(
      (a, b) =>
        b.count - a.count ||
        (a.value === null
          ? 1
          : b.value === null
            ? -1
            : String(a.value).localeCompare(String(b.value))),
    );
  const statusAsked = {
    start: { type: "Invoice" },
    return: { group_count: { at: 0, prop: "status" } },
  };
  const statusRan: OntologyQuery = {
    start: { type: "Invoice", ids: null, text: null, where: [] },
    steps: [],
    filters: [],
    return: { group_count: { at: 0, prop: "status", limit: 50 } },
  };
  const billed = pairs(g, "Invoice", "billed_to", "Customer").sort(
    ([a, x], [b, y]) =>
      Number(b.props.amount_due ?? 0) - Number(a.props.amount_due ?? 0) ||
      byEntity(a, b) ||
      byEntity(x, y),
  );
  const columns = [{ at: 0 }, { at: 0, prop: "amount_due" }, { at: 1 }];
  const largestAsked = {
    start: { type: "Invoice" },
    steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
    return: {
      rows: {
        columns,
        order_by: { at: 0, prop: "amount_due", desc: true },
        limit: 10,
      },
    },
  };
  const largestRan: OntologyQuery = {
    start: { type: "Invoice", ids: null, text: null, where: [] },
    steps: [{ via: "billed_to", dir: "out", to: "Customer", where: [] }],
    filters: [],
    return: {
      rows: {
        columns: columns.map((c) => ({ prop: null, ...c })),
        order_by: { at: 0, prop: "amount_due", desc: true },
        limit: 10,
      },
    },
  };
  const largest = billed.slice(0, 10);
  return {
    total: invoices.length,
    top: groups[0]!,
    first: largest[0],
    status: {
      asked: statusAsked,
      ran: statusRan,
      output: {
        kind: "group_count",
        // The engine gives every group's value as a document's text: a
        // group is many entities', and it doesn't say whose.
        groups: groups.slice(0, 50).map(({ value, count }) => ({
          value:
            value === null
              ? null
              : {
                  untrusted:
                    typeof value === "string" ? value : JSON.stringify(value),
                },
          count,
        })),
        total_groups: groups.length,
      } satisfies QueryOutput,
    },
    largest: {
      asked: largestAsked,
      ran: largestRan,
      output: {
        kind: "rows",
        rows: largest.map(([invoice, customer]) => ({
          cells: [
            entityCell(invoice),
            valueOf(invoice, "amount_due"),
            entityCell(customer),
          ],
          edges: [factEdge(0, "billed_to", invoice, customer)],
        })),
        total: billed.length,
      } satisfies QueryOutput,
    },
  };
}

const usage = (u: Partial<AskUsage>): AskUsage => ({
  input: 0,
  cache_read: 0,
  cache_write: 0,
  output: 0,
  usd: 0,
  ...u,
});

function answer(
  o: StoredOntology,
  loadId: string,
  outcome: Outcome,
  // A grantee who holds all of the load still asks through a view, offered
  // no helix_query, history_of or changes.
  viewed = false,
): Script {
  const g = new LoadGraph(tenantGraph(o.tenantId), loadId);
  const found = outcome === "cannot_answer" ? undefined : quotedFact(g);
  // The asking model in force, and the kind that serves it (./ai.ts).
  const started: AskEvent = {
    event: "started",
    model: aiModel(o.workspaceId, "ask") || MODEL,
    provider: aiProvider(o.workspaceId),
  };
  const callId = (n: number) => `toolu_${String(n).padStart(2, "0")}`;
  const call = (n: number, tool: string, input: unknown): AskEvent => ({
    event: "tool_call",
    call_id: callId(n),
    tool,
    input,
  });
  const recorded = (n: number, e: number, summary: string): AskEvent => ({
    event: "tool_result",
    call_id: callId(n),
    summary,
    evidence_id: `e${e}`,
  });
  const ended = (n: number, tool: string): AskEvent => ({
    event: "tool_result",
    call_id: callId(n),
    summary: `${tool}: ended the question`,
  });
  // What the load holds, which the engine reads and records as e1 before
  // the model's first call, its summary the engine's: how many of its reads
  // returned (here a count of each type and of each constraint's breaks,
  // all returned); every reply follows.
  const profileReads =
    new Set(
      [...g.nodes()]
        .filter((n) => !isFoundryLabel("node", n.label))
        .map((n) => n.label),
    ).size +
    new Set(
      [...g.nodes()]
        .filter((n) => n.label === N.Violation)
        .map((n) => String(n.props.constraint)),
    ).size;
  const profiled: AskEvent[] = [
    started,
    {
      event: "tool_call",
      call_id: "load_profile",
      tool: "load_profile",
      input: {},
    },
    {
      event: "tool_result",
      call_id: "load_profile",
      summary: `load_profile: ${profileReads} of ${profileReads} reads`,
      evidence_id: "e1",
    },
  ];
  // A 3.10.0 engine recorded no outputs and drew no visuals.
  const visuals = knobs.askVisuals !== false;
  const recordOf = (
    evidence: number,
    query: OntologyQuery,
    output: QueryOutput,
  ) => ({ evidence_id: `e${evidence}`, query, ...(visuals ? { output } : {}) });
  if (outcome === "failed")
    return {
      events: [...profiled, { event: "finished" }],
      status: "failed",
      result: {
        status: "failed",
        reason: "model_failed",
        queries: [],
        usage: usage({ input: 4_100, output: 0, usd: 0.0042 }),
      },
      error: "the model could not be reached; ask again",
      usdMicros: 4_200,
    };
  if (!found) {
    const type = [...g.nodes()].find((n) => n.props.entity_key !== undefined);
    const reason = "No link in this ontology records who signed a contract.",
      gap =
        "A signed_by link from Contract to the Person who signed it, filled from the contracts.",
      kind = "undeclared_relation";
    return {
      events: [
        ...profiled,
        call(1, "describe_type", { type: type?.label || "Contract" }),
        recorded(1, 2, `describe_type: ${type?.label || "Contract"}`),
        call(2, "cannot_answer", { reason, gap, kind }),
        ended(2, "cannot_answer"),
        { event: "finished" },
      ],
      status: "cannot_answer",
      result: {
        status: "cannot_answer",
        reason,
        gap,
        kind,
        queries: [],
        usage: usage({
          input: 6_100,
          cache_read: 4_000,
          output: 380,
          usd: 0.0212,
        }),
      },
      error: "",
      usdMicros: 21_200,
    };
  }
  const { ev, section, link, subject, object, row } = found;
  const subjectRef = refOf(subject),
    objectRef = refOf(object);
  // Another entity the subject is linked to the same way: a co-signer.
  const other = [...(g.graph.outgoing.get(subject.id) || [])]
    .map((e) => g.graph.edges.get(e)!)
    .filter((e) => e.label === link && e.to !== object.id)
    .map((e) => g.node(e.to))
    .find((n) => n !== undefined);
  const broken = firstBreak(g);
  // As the model wrote it, defaults left out; as the agent ran it, written out.
  const asked = {
    start: { type: subject.label },
    steps: [{ via: link, dir: "out", to: object.label }],
    return: {
      rows: {
        columns: [
          { at: 0 },
          { at: 1 },
          { at: 1, prop: "name" },
          { at: 0, prop: "signed_on" },
        ],
      },
    },
  };
  const query: OntologyQuery = {
    start: { type: subject.label, ids: null, text: null, where: [] },
    steps: [{ via: link, dir: "out", to: object.label, where: [] }],
    filters: [],
    return: {
      rows: {
        columns: [
          { at: 0, prop: null },
          { at: 1, prop: null },
          { at: 1, prop: "name" },
          { at: 0, prop: "signed_on" },
        ],
        limit: 50,
        order_by: null,
      },
    },
  };
  // Its rows, by their entities.
  const signed = pairs(g, subject.label, link, object.label).sort(
    ([a, x], [b, y]) => byEntity(a, b) || byEntity(x, y),
  );
  const signedOutput: QueryOutput = {
    kind: "rows",
    rows: signed.slice(0, 50).map(([a, b]) => ({
      cells: [
        entityCell(a),
        entityCell(b),
        valueOf(b, "name"),
        valueOf(a, "signed_on"),
      ],
      edges: [factEdge(0, link, a, b)],
    })),
    total: signed.length,
  };
  const invoices = invoiceQueries(g);
  // The graph query the model writes itself (a Helix v3 JSON read): the
  // load's entities of the subject's type, counted.
  const helix = invoices ? 10 : 8,
    history = helix + 1;
  const countRead = {
    count: {
      input: {
        nodes_where: {
          predicate: {
            and: {
              predicates: [
                {
                  eq: {
                    left: { property: "$label" },
                    right: { constant: { string: subject.label } },
                  },
                },
                {
                  eq: {
                    left: { property: FOUNDRY_LOAD },
                    right: { constant: { string: loadId } },
                  },
                },
              ],
            },
          },
        },
      },
    },
  };
  const ofSubject = [...g.nodes()].filter(
    (n) => n.label === subject.label,
  ).length;
  const reads: AskEvent[] = [
    ...profiled,
    call(1, "find_entities", {
      type: object.label,
      by: "identifier",
      value: objectRef.id,
      limit: 5,
    }),
    {
      event: "tool_result",
      call_id: "toolu_01",
      summary: "find_entities: failed",
      error: "can't look entities up by identifier yet",
    },
    call(2, "find_entities", {
      type: object.label,
      by: "name",
      value: nameOf(object),
      limit: 5,
    }),
    recorded(2, 2, `find_entities: 1 ${object.label} found`),
    call(3, "query", asked),
    recorded(
      3,
      3,
      `query: ${signedOutput.rows.length} of ${signedOutput.total} rows`,
    ),
  ];
  if (outcome === "budget_exhausted")
    return {
      events: [...reads, { event: "finished" }],
      status: "failed",
      result: {
        status: "budget_exhausted",
        queries: [recordOf(3, query, signedOutput)],
        usage: usage({ input: 61_000, output: 9_800, usd: 0.5 }),
      },
      error:
        "the question reached its limit of $0.50, 20 steps or 240 seconds before an answer; ask something narrower",
      usdMicros: ASK_LIMITS.questionUsdMicros,
    };
  const quote: CitationTarget = {
    quote: {
      doc_id: String(ev.props.doc_id),
      section: Number(section.props.ordinal),
      start: Number(ev.props.start),
      end: Number(ev.props.end),
    },
  };
  const more: AskEvent[] = [
    call(4, "evidence", {
      target: { fact: { link, subject: "n2", object: "n1" } },
      limit: 10,
    }),
    recorded(4, 4, `evidence: ${row ? 2 : 1} items`),
    ...(other
      ? [
          call(5, "explain", {
            pred: "relation:co_signed",
            args: ["n1", "n3"],
          }),
          recorded(5, 5, "explain: relation:co_signed: 3 steps"),
        ]
      : []),
    ...(broken
      ? [
          call(6, "violations", { constraint: broken.constraint, limit: 5 }),
          recorded(6, 6, "violations: 5 records, more"),
        ]
      : []),
    call(7, "find_entities", {
      type: subject.label,
      by: "name",
      value: nameOf(subject),
      limit: 5,
    }),
    recorded(7, 7, `find_entities: 1 ${subject.label} found`),
    ...(invoices
      ? [
          call(8, "query", invoices.status.asked),
          recorded(
            8,
            8,
            `query: ${invoices.status.output.groups.length} of ${invoices.status.output.total_groups} groups`,
          ),
          call(9, "query", invoices.largest.asked),
          recorded(
            9,
            9,
            `query: ${invoices.largest.output.rows.length} of ${invoices.largest.output.total} rows`,
          ),
        ]
      : []),
    // Under a view, whole or partial, the engine offers neither tool that
    // reads the graph as stored, helix_query and history_of.
    ...(viewed
      ? []
      : [
          // How many of the subject's type the load holds, read from the
          // graph itself: not a typed query, so no query record holds it.
          call(helix, "helix_query", { query: countRead, limit: 50 }),
          // A count is one value, as the engine words it.
          recorded(helix, helix, "helix_query: a value"),
          // What the load recorded of the subject, by transaction: its
          // build, then the contract's extract, which stated its signed_on.
          call(history, "history_of", { target: { entity: "n2" } }),
          recorded(history, history, "history_of: 2 steps"),
        ]),
  ];
  const cite = (marker: string, e: number, target: CitationTarget) =>
    ({ marker, evidence_id: `e${e}`, target }) satisfies AskCitation;
  const built = ontologyLoad(o.id, loadId),
    builtAt = built?.finishedAt || built?.createdAt || new Date().toISOString(),
    signedOn = /^\d{4}-\d{2}-\d{2}$/.test(String(subject.props.signed_on))
      ? String(subject.props.signed_on)
      : undefined;
  const citations: AskCitation[] = [
    cite("c1", 2, { entity: objectRef }),
    cite("c2", 3, { entity: subjectRef }),
    cite("c3", 3, { fact: { link, subject: subjectRef, object: objectRef } }),
    cite("c4", 4, quote),
    ...(row
      ? [
          cite("c5", 4, {
            row: {
              source: String(row.props.source),
              mapping: String(row.props.mapping),
              row_key: String(row.props.row_key),
            },
          }),
        ]
      : []),
    ...(other
      ? [
          cite("c6", 5, {
            derived: {
              pred: "relation:co_signed",
              args: [objectRef, refOf(other)],
            },
          }),
        ]
      : []),
    ...(broken
      ? [
          cite("c7", 6, {
            violation: { constraint: broken.constraint, key: broken.key },
          }),
        ]
      : []),
    cite("c8", 3, { result: { evidence_id: "e3" } }),
    ...(invoices
      ? [
          cite("c9", 8, { result: { evidence_id: "e8" } }),
          cite("c10", 9, { result: { evidence_id: "e9" } }),
        ]
      : []),
    ...(viewed
      ? []
      : [cite("c11", helix, { result: { evidence_id: `e${helix}` } })]),
    // The signed_on the contract's extract set, with when it held by valid
    // time (contract 4.7.0): a day with no time of day holds from the next
    // midnight UTC, its `day` the day the source gave.
    ...(signedOn && !viewed
      ? [
          cite("c12", history, {
            version: {
              tx: loadId,
              at: builtAt,
              of: { prop: { entity: subjectRef, prop: "signed_on" } },
              valid: {
                from: new Date(Date.parse(`${signedOn}T00:00:00Z`) + 86_400_000)
                  .toISOString()
                  .replace(".000Z", "Z"),
                day: signedOn,
              },
            },
          }),
        ]
      : []),
  ];
  const blocks = [
    {
      para: `${nameOf(object)} [c1] signed ${nameOf(subject)} [c2] [c3]; the contract says so [c4].`,
    },
    {
      list: [
        ...(row
          ? [`Its ${object.label} row ${String(row.props.row_key)} [c5]`]
          : []),
        ...(other
          ? [`It co-signed with ${nameOf(other)}, the rules derive [c6]`]
          : []),
        ...(broken
          ? [`${broken.count} records break ${broken.constraint} [c7]`]
          : []),
        `Every ${subject.label} with who signed it [c8]`,
        ...(viewed
          ? []
          : [
              `The load holds ${ofSubject} ${subject.label} ${ofSubject === 1 ? "entity" : "entities"} [c11]`,
            ]),
      ],
    },
    // A paragraph to introduce each visual.
    ...(invoices
      ? [
          {
            para: `${invoices.top.count} of the ${invoices.total} invoices are ${String(invoices.top.value ?? "without a status")} [c9].`,
          },
          {
            para: invoices.first
              ? `The largest is ${nameOf(invoices.first[0])}, for ${Number(invoices.first[0].props.amount_due ?? 0).toLocaleString("en-US")}, billed to ${nameOf(invoices.first[1])} [c10].`
              : "No invoice bills a customer [c10].",
          },
        ]
      : []),
    ...(signedOn && !viewed
      ? [{ para: `It was signed on ${signedOn}, its history says [c12].` }]
      : []),
    ...(knobs.askMarkup
      ? [
          {
            para: '<img src="x" onerror="alert(1)"> [Read more](https://example.com/phish) **now**',
          },
        ]
      : []),
  ];
  // The citations as the model writes them: entities by the handles its
  // results gave (the object n1, found first; the subject n2, from the
  // query; the co-signer n3), which the result resolves.
  const handles = new Map([
    [`${objectRef.type}\0${objectRef.id}`, "n1"],
    [`${subjectRef.type}\0${subjectRef.id}`, "n2"],
  ]);
  const handle = (r: EntityRef) => handles.get(`${r.type}\0${r.id}`) ?? "n3";
  // What a version is of, named the same way.
  const ofNamed = (of: VersionOf) =>
    "entity" in of
      ? { entity: handle(of.entity) }
      : "prop" in of
        ? { prop: { entity: handle(of.prop.entity), prop: of.prop.prop } }
        : "fact" in of
          ? {
              fact: {
                link: of.fact.link,
                subject: handle(of.fact.subject),
                object: handle(of.fact.object),
              },
            }
          : of;
  const written = citations.map((c) => ({
    ...c,
    target:
      "entity" in c.target
        ? { entity: handle(c.target.entity) }
        : "fact" in c.target
          ? {
              fact: {
                link: c.target.fact.link,
                subject: handle(c.target.fact.subject),
                object: handle(c.target.fact.object),
              },
            }
          : "derived" in c.target
            ? {
                derived: {
                  pred: c.target.derived.pred,
                  args: c.target.derived.args.map(handle),
                },
              }
            : "version" in c.target
              ? {
                  version: {
                    ...c.target.version,
                    of: ofNamed(c.target.version.of),
                  },
                }
              : c.target,
  }));
  // The invoices by status after the paragraph that counts them, as the
  // shape suits (bars); the largest after the one naming the first, asked
  // as bars.
  const shown: AskVisual[] =
    invoices && visuals
      ? [
          { evidence_id: "e8", after: 2, title: "Invoices by status" },
          {
            evidence_id: "e9",
            after: 3,
            kind: "bar",
            title: "Largest invoices",
          },
        ]
      : [];
  // Its first answer cites an entity the evidence read didn't show, and
  // is refused; it answers again.
  const refused = written.map((c) =>
    c.marker === "c2" ? { ...c, evidence_id: "e4" } : c,
  );
  const answering = viewed ? helix : history + 1;
  return {
    events: [
      ...reads,
      ...more,
      call(answering, "answer", {
        blocks,
        citations: refused,
        ...(shown.length ? { visuals: shown } : {}),
      }),
      {
        event: "tool_result",
        call_id: callId(answering),
        summary: "answer: refused",
        error:
          'the answer was refused; fix these and call answer again:\n- citation c2: e4 didn\'t show {"entity":"n2"}; cite a target exactly as a result shows it, or {"result":{"evidence_id":"e4"}}',
      },
      call(answering + 1, "answer", {
        blocks,
        citations: written,
        ...(shown.length ? { visuals: shown } : {}),
      }),
      ended(answering + 1, "answer"),
      { event: "finished" },
    ],
    status: "answered",
    result: {
      status: "answered",
      answer: { blocks, ...(shown.length ? { visuals: shown } : {}) },
      citations,
      queries: [
        recordOf(3, query, signedOutput),
        ...(invoices
          ? [
              recordOf(8, invoices.status.ran, invoices.status.output),
              recordOf(9, invoices.largest.ran, invoices.largest.output),
            ]
          : []),
      ],
      usage: usage({
        input: 18_200,
        cache_read: 12_000,
        cache_write: 0,
        output: 1_450,
        usd: 0.0734,
      }),
    },
    error: "",
    usdMicros: 73_400,
  };
}

/**
 * What the agent does for an asker who holds only some of the load's sources:
 * it reads what the load holds of those, and lists the first type it may read
 * through the same typed read a page makes. It has no `helix_query`, history
 * or changes, and is told nothing else.
 */
function partAnswer(
  o: StoredOntology,
  loadId: string,
  view: View,
  outcome: Outcome,
): Script {
  const g = new LoadGraph(tenantGraph(o.tenantId), loadId);
  const started: AskEvent = {
    event: "started",
    model: aiModel(o.workspaceId, "ask") || MODEL,
    provider: aiProvider(o.workspaceId),
  };
  const profiled: AskEvent[] = [
    started,
    {
      event: "tool_call",
      call_id: "load_profile",
      tool: "load_profile",
      input: {},
    },
    {
      event: "tool_result",
      call_id: "load_profile",
      summary: `load_profile: ${view.types.size} of ${view.types.size} reads`,
      evidence_id: "e1",
    },
  ];
  const types = [...view.types.keys()].sort();
  if (outcome === "failed" || !types.length)
    return {
      events: [...profiled, { event: "finished" }],
      status: "failed",
      result: {
        status: "failed",
        reason: "model_failed",
        queries: [],
        usage: usage({ input: 4_100, output: 0, usd: 0.0042 }),
      },
      error: "the model could not be reached; ask again",
      usdMicros: 4_200,
    };
  // The first type it can see anything of: what it can say.
  const model = evaluate(g, compile(view.ontologyToml));
  const readOf = (type: string) => {
    const query: OntologyQuery = {
      start: { type },
      return: { rows: { columns: [{ at: 0 }], limit: 100 } },
    };
    const read = readAnswer(model, { query }, { view });
    return {
      type,
      query,
      rows:
        "ok" in read
          ? (read.ok.result as { rows: { cells: unknown[] }[] }).rows
          : [],
    };
  };
  const reads = types.map(readOf),
    { type, query, rows } = reads.find((r) => r.rows.length) || reads[0]!;
  // `askOutcome` "cannot_answer" asks about another time, which such an
  // asker can't read (no `valid`, `past` or `as_of`): uncovered. With no
  // rows it can see, it holds no such entity: absent.
  const { reason, gap, kind } =
    outcome === "cannot_answer"
      ? {
          reason:
            "What held at another time isn't shared with the person asking: they read only current values.",
          gap: "Sources shared with them, read whole, that date what they state.",
          kind: "uncovered" as const,
        }
      : {
          reason:
            "The sources shared with the person asking hold nothing about this.",
          gap: "Sources shared with them that state it.",
          kind: "absent" as const,
        };
  if (!rows.length || outcome === "cannot_answer")
    return {
      events: [
        ...profiled,
        {
          event: "tool_call",
          call_id: "toolu_01",
          tool: "cannot_answer",
          input: { reason, gap, kind },
        },
        {
          event: "tool_result",
          call_id: "toolu_01",
          summary: "cannot_answer: ended the question",
        },
        { event: "finished" },
      ],
      status: "cannot_answer",
      result: {
        status: "cannot_answer",
        reason,
        gap,
        kind,
        queries: [],
        usage: usage({ input: 3_100, output: 120, usd: 0.0112 }),
      },
      error: "",
      usdMicros: 11_200,
    };
  const output: QueryOutput = {
    kind: "rows",
    rows: rows.map((r) => ({ cells: r.cells, edges: [] })),
    total: rows.length,
  };
  const blocks = [
    {
      para: `Of what the sources shared with you hold, these are the first ${type} records [c1].`,
    },
  ];
  const citations: AskCitation[] = [
    {
      marker: "c1",
      evidence_id: "e2",
      target: { result: { evidence_id: "e2" } },
    },
  ];
  return {
    events: [
      ...profiled,
      { event: "tool_call", call_id: "toolu_01", tool: "query", input: query },
      {
        event: "tool_result",
        call_id: "toolu_01",
        summary: `query: ${rows.length} rows`,
        evidence_id: "e2",
      },
      {
        event: "tool_call",
        call_id: "toolu_02",
        tool: "answer",
        input: { blocks, citations },
      },
      {
        event: "tool_result",
        call_id: "toolu_02",
        summary: "answer: ended the question",
      },
      { event: "finished" },
    ],
    status: "answered",
    result: {
      status: "answered",
      answer: { blocks },
      citations,
      queries: [
        {
          evidence_id: "e2",
          // As the engine records it, every field written out.
          query: {
            start: { type, where: [], ids: null, text: null },
            steps: [],
            filters: [],
            return: {
              rows: {
                columns: [{ at: 0, prop: null }],
                order_by: null,
                limit: 100,
              },
            },
          },
          output,
        },
      ],
      usage: usage({ input: 5_200, output: 240, usd: 0.0212 }),
    },
    error: "",
    usdMicros: 21_200,
  };
}

// ---------------------------------------------------------------- the worker

const SPENT_MONTH = `the workspace has spent this month's $${(ASK_LIMITS.monthlyUsdMicros / 1e6).toFixed(2)} of questions`;

/**
 * Brings a question up to now: its claim, the events due, and its end. A
 * claim fails a question the month can no longer fit, and holds one back
 * while its workspace runs as many as it may or their reservations leave
 * too little; otherwise it runs, reserving its cap.
 */
function advance(q: StoredQuestion) {
  const t = Date.now();
  if (q.status === "queued") {
    const due = Date.parse(q.createdAt) + q.stepMs;
    if (t < due) return;
    const { spent, reserved } = spend(q.workspaceId);
    if (spent + ASK_LIMITS.questionUsdMicros > ASK_LIMITS.monthlyUsdMicros) {
      Object.assign(q, {
        status: "failed",
        error: SPENT_MONTH,
        finishedAt: now(),
      });
      return;
    }
    const running = ofWorkspace(q.workspaceId).filter(
      (x) => x.status === "running",
    ).length;
    if (
      running >= ASK_LIMITS.concurrent ||
      spent + reserved + ASK_LIMITS.questionUsdMicros >
        ASK_LIMITS.monthlyUsdMicros
    ) {
      q.heldAt = t;
      return;
    }
    q.status = "running";
    q.attempts += 1;
    q.claimedAt = Math.max(due, q.heldAt);
    q.startedAt = new Date(q.claimedAt).toISOString();
    q.reservedMicros = ASK_LIMITS.questionUsdMicros;
  }
  if (q.status !== "running") return;
  const due = Math.min(
    q.script.events.length,
    Math.floor((t - q.claimedAt) / q.stepMs) + 1,
  );
  while (q.events.length < due) {
    const seq = q.events.length + 1;
    q.events.push({
      seq,
      eventJson: JSON.stringify(q.script.events[seq - 1]),
      createdAt: new Date(q.claimedAt + (seq - 1) * q.stepMs).toISOString(),
    });
  }
  if (t >= q.claimedAt + q.script.events.length * q.stepMs) {
    q.status = q.script.status;
    q.resultJson = q.script.result ? JSON.stringify(q.script.result) : "";
    q.error = q.script.error;
    q.usdMicros = q.script.usdMicros;
    q.reservedMicros = 0;
    q.finishedAt = now();
  }
}

/** Brings the workspace's questions up to now, oldest first, as claims go. */
const advanceAll = (workspaceId: string) =>
  [...ofWorkspace(workspaceId)].reverse().forEach(advance);

/** A result as lists carry it: its queries without their outputs. */
function listed(resultJson: string): string {
  if (!resultJson) return resultJson;
  const r = JSON.parse(resultJson) as AskResult;
  return JSON.stringify({
    ...r,
    queries: r.queries.map(({ output: _, ...rest }) => rest),
  });
}

/** A question as GET of one gives it, or (`list`) as a list does. */
const toQuestion = (q: StoredQuestion, list = false): FoundryQuestion => ({
  id: q.id,
  loadId: q.loadId,
  versionId: q.versionId,
  threadId: q.threadId,
  question: q.question,
  focusJson: q.focusJson,
  status: q.status,
  resultJson: list ? listed(q.resultJson) : q.resultJson,
  error: q.error,
  usdMicros: String(q.usdMicros),
  lastSeq: q.events.length,
  askedByUserId: q.askedByUserId,
  createdAt: q.createdAt,
  startedAt: q.startedAt,
  finishedAt: q.finishedAt,
  attempts: q.attempts,
  reservedMicros: String(q.reservedMicros),
});

/** The ontology, read by someone who may query its database. */
function scope(req: Parameters<MockRoute["handle"]>[0]) {
  const s = ontologyOf(req);
  if (
    isScope(s) &&
    !req.grantee &&
    (config as { ontologyDatabaseHidden?: boolean }).ontologyDatabaseHidden
  )
    return fail("PERMISSION_DENIED", "permission denied");
  return s;
}

/**
 * Whether a question is one the caller may see: any, but a grantee's own only,
 * and only while its grants hold what it read (readableQuestions).
 */
const visible = (req: Parameters<MockRoute["handle"]>[0], q: StoredQuestion) =>
  !req.grantee ||
  (q.grantee === req.grantee.principal &&
    (!q.seen || req.grantee.stillSees(q.seen)));

function question(
  req: Parameters<MockRoute["handle"]>[0],
): StoredQuestion | MockReply {
  const s = scope(req);
  if (!isScope(s)) return s;
  advanceAll(s.m.workspaceId);
  const q = state.questions.find(
    (x) => x.id === req.params.id && x.ontologyId === s.o.id && visible(req, x),
  );
  return q || fail("NOT_FOUND", "question not found");
}
const isQuestion = (x: StoredQuestion | MockReply): x is StoredQuestion =>
  "threadId" in x;

/**
 * The focus a question carries as canonical JSON, or undefined unless it is
 * exactly the engine's EntityRef with both set, as the WFE checks it.
 */
function focusOf(raw: string): string | undefined {
  let x: unknown;
  try {
    x = JSON.parse(raw);
  } catch {
    return undefined;
  }
  const ref = entityRef(x);
  return ref && !contractErrors("ask_request#/$defs/EntityRef", x).length
    ? JSON.stringify({ type: ref.type, id: ref.id })
    : undefined;
}

const BASE =
  "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions" as const;

export const questionRoutes: MockRoute[] = [
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const s = scope(req);
      if (!isScope(s)) return s;
      const unset = aiRefusal(s.m.workspaceId);
      if (unset) return unset;
      const body = (req.body || {}) as Partial<AskQuestionRequest>;
      const clientToken = body.clientToken || "",
        text = (body.question || "").trim();
      if (!clientToken || clientToken.length > 64)
        return fail(
          "INVALID_ARGUMENT",
          "client_token is required, at most 64 characters",
        );
      if (!text || [...text].length > 4000)
        return fail(
          "INVALID_ARGUMENT",
          "question is required, at most 4000 characters",
        );
      if (
        body.focusJson &&
        new TextEncoder().encode(body.focusJson).length > 4096
      )
        return fail("INVALID_ARGUMENT", "focus_json is at most 4096 bytes");
      const focusJson = body.focusJson ? focusOf(body.focusJson) : "";
      if (focusJson === undefined)
        return fail(
          "INVALID_ARGUMENT",
          'focus_json: a focus is {"type": ..., "id": ...}, both set',
        );
      advanceAll(s.m.workspaceId);
      const again = state.questions.find(
        (q) => q.ontologyId === s.o.id && q.clientToken === clientToken,
      );
      if (again)
        return again.question === text && visible(req, again)
          ? ok({ question: toQuestion(again) } satisfies QuestionResponse)
          : fail(
              "INVALID_ARGUMENT",
              "client_token was already used for another question",
            );
      // A grantee asks only the load its grants were checked against.
      const granted = req.grantee?.load?.loadId;
      if (granted && body.loadId && body.loadId !== granted)
        return fail("NOT_FOUND", "load not found");
      const loadId = granted || body.loadId || currentLoadId(s.o.id);
      if (!loadId)
        return fail(
          "FAILED_PRECONDITION",
          "the ontology has no load to ask yet; load a version first",
        );
      const load = ontologyLoad(s.o.id, loadId);
      if (!load) return fail("NOT_FOUND", "load not found");
      if (load.status !== "ready" && load.status !== "partial")
        return fail(
          "FAILED_PRECONDITION",
          "only a ready or partial load can be asked",
        );
      // A thread is named by its first question.
      if (
        body.threadId &&
        !state.questions.some(
          (q) =>
            q.ontologyId === s.o.id &&
            q.id === body.threadId &&
            q.threadId === q.id &&
            visible(req, q),
        )
      )
        return fail("NOT_FOUND", "thread not found");
      const refusal = askRefusal(s.m.workspaceId, req.grantee?.principal);
      if (refusal) return refusal;
      const qid = id("q");
      // Of a load it holds only some of, the agent is told what it holds.
      const def = (ontologyVersion(s.o.id, load.versionId)?.definition ||
        {}) as { ontology_toml?: unknown; mappings_toml?: unknown };
      const view = req.grantee?.load?.partial
        ? viewOf(
            req.grantee.visibility(s.o.id),
            String(def.ontology_toml || ""),
            String(def.mappings_toml || ""),
          )
        : undefined;
      const q: StoredQuestion = {
        id: qid,
        workspaceId: s.m.workspaceId,
        ontologyId: s.o.id,
        threadId: body.threadId || qid,
        loadId: load.id,
        versionId: load.versionId,
        question: text,
        focusJson,
        clientToken,
        askedByUserId: req.principal.user.id,
        ...(req.grantee ? { grantee: req.grantee.principal } : {}),
        ...(view && req.grantee ? { seen: req.grantee.seen(s.o.id) } : {}),
        createdAt: now(),
        status: "queued",
        attempts: 0,
        claimedAt: 0,
        heldAt: 0,
        stepMs: knobs.askStepMs ?? STEP_MS,
        script: view
          ? partAnswer(s.o, load.id, view, knobs.askOutcome || "answered")
          : answer(s.o, load.id, knobs.askOutcome || "answered", !!req.grantee),
        events: [],
        resultJson: "",
        error: "",
        usdMicros: 0,
        reservedMicros: 0,
        startedAt: null,
        finishedAt: null,
      };
      state.questions.unshift(q);
      return ok({ question: toQuestion(q) } satisfies QuestionResponse);
    },
  },
  {
    method: "GET",
    path: BASE,
    handle: (req) => {
      const s = scope(req);
      if (!isScope(s)) return s;
      advanceAll(s.m.workspaceId);
      const thread = req.query.get("threadId") || "";
      const all = state.questions.filter(
        (q) =>
          q.ontologyId === s.o.id &&
          (!thread || q.threadId === thread) &&
          visible(req, q),
      );
      const from = Number(req.query.get("pageToken") || 0) || 0;
      return ok({
        questions: all.slice(from, from + PAGE).map((q) => toQuestion(q, true)),
        nextPageToken: from + PAGE < all.length ? String(from + PAGE) : "",
        budget: budget(s.m.workspaceId, req.grantee?.principal),
      } satisfies ListQuestionsResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const q = question(req);
      return isQuestion(q)
        ? ok({ question: toQuestion(q) } satisfies QuestionResponse)
        : q;
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}/events`,
    handle: (req) => {
      const after = Number(req.query.get("afterSeq") || 0) || 0;
      if (after < 0)
        return fail("INVALID_ARGUMENT", "after_seq must not be negative");
      const q = question(req);
      if (!isQuestion(q)) return q;
      // The status is read first: once it ended, every event is stored.
      return ok({
        events: q.events.filter((e) => e.seq > after).slice(0, 200),
        status: q.status,
        lastSeq: q.events.length,
      } satisfies ListQuestionEventsResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:cancel`,
    handle: (req) => {
      const q = question(req);
      if (!isQuestion(q)) return q;
      if (q.status !== "cancelled" && !live(q))
        return fail("FAILED_PRECONDITION", "the question already ended");
      // A running question is charged what it reserved.
      if (live(q))
        Object.assign(q, {
          status: "cancelled",
          usdMicros: q.usdMicros + q.reservedMicros,
          reservedMicros: 0,
          finishedAt: now(),
        });
      return ok({ question: toQuestion(q) } satisfies QuestionResponse);
    },
  },
];
