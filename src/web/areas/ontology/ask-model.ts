import type {
  AskBlock,
  AskCitation,
  AskEvent,
  AskFailure,
  AskResult,
  AskUsage,
  AskVisual,
  CitationTarget,
  FoundryQuestion,
  ListQuestionEventsResponse,
  OntologyQuery,
  OntologyVersion,
  QuestionBudget,
  QuestionStatus,
  QueryOutput,
  QueryWhere,
  VersionOf,
  VisualKind,
} from "../../../shared/wfe";
import { entityRef, type EntityRef } from "../../../shared/foundry-read";
import { formatDate, plural } from "../../kit/util";
import { sentence } from "../../api";
import { entityId } from "../explore/model";
import { normalizeName } from "../explore/naming";
import { refKey } from "../explore/rule-breaks";
import { ENRICHMENT_SOURCE } from "./enrichment-fields";
import { versionFormat } from "./format";
import { readAgentBundle, withLoad } from "./model";
import { ontologyPaths } from "./paths";

// The Ask panel's logic: a question's events
// folded into steps, a typed query as a readable path, an answer's blocks as
// plain text and citation chips, and what each citation opens. Everything
// here reads the engine's JSON (contract/ontology_agent/ask_event.json and
// ask_result.json) without trusting it: model and document text is only
// ever text, never markup, links or images.

const isObj = (x: unknown): x is Record<string, unknown> =>
  !!x && typeof x === "object" && !Array.isArray(x);
const str = (x: unknown) => (typeof x === "string" ? x : undefined);
const num = (x: unknown) =>
  typeof x === "number" && Number.isFinite(x) ? x : undefined;
const json = (s: string): unknown => {
  try {
    return JSON.parse(s);
  } catch {
    return undefined;
  }
};

// ---------------------------------------------------------------- parsing

/**
 * A citation's target, or undefined when it isn't one.
 *
 * @example
 * parseTarget({ row: { source: "src_1", mapping: "m", row_key: "cus_1" } }); // the row
 * parseTarget({ row: { source: "foundry_enrichment", mapping: "m", row_key: "" } }); // a profile, by no key
 * parseTarget({ version: { tx: "01J…", at: "2026-10-03T09:14:07Z", of: { entity: { type: "Customer", id: "cus_1" } } } }); // a past state
 * parseTarget({ link: "https://example.com" }); // undefined
 */
export function parseTarget(x: unknown): CitationTarget | undefined {
  if (!isObj(x)) return undefined;
  const [kind, v] = Object.entries(x)[0] ?? [];
  if (Object.keys(x).length !== 1 || !isObj(v)) return undefined;
  switch (kind) {
    case "entity": {
      const e = entityRef(v);
      return e && { entity: e };
    }
    case "fact": {
      const subject = entityRef(v.subject),
        object = entityRef(v.object),
        link = str(v.link);
      return subject && object && link
        ? { fact: { link, subject, object } }
        : undefined;
    }
    case "derived": {
      const pred = str(v.pred),
        args = Array.isArray(v.args) ? v.args.map(entityRef) : [];
      return pred && args.every((a) => a)
        ? { derived: { pred, args: args as EntityRef[] } }
        : undefined;
    }
    case "row": {
      // A People enrichment row has no key (contract 3.12.0): its key is the
      // provider's profile id, which a stored result never keeps. Every
      // other row has one.
      const source = str(v.source),
        rowKey = str(v.row_key) ?? "";
      return source && (rowKey === "") === (source === ENRICHMENT_SOURCE)
        ? { row: { source, mapping: str(v.mapping) || "", row_key: rowKey } }
        : undefined;
    }
    case "quote": {
      const doc = str(v.doc_id),
        section = num(v.section),
        start = num(v.start),
        end = num(v.end);
      return doc &&
        section !== undefined &&
        start !== undefined &&
        end !== undefined &&
        start >= 0 &&
        start < end
        ? { quote: { doc_id: doc, section, start, end } }
        : undefined;
    }
    case "violation": {
      const constraint = str(v.constraint),
        key = str(v.key);
      return constraint && key ? { violation: { constraint, key } } : undefined;
    }
    case "result": {
      const evidence = str(v.evidence_id);
      return evidence ? { result: { evidence_id: evidence } } : undefined;
    }
    case "version": {
      // The tx is opaque: only the engine reads it.
      const tx = str(v.tx),
        at = str(v.at),
        of = parseVersionOf(v.of);
      return tx &&
        at &&
        RFC_3339.test(at) &&
        !Number.isNaN(Date.parse(at)) &&
        of
        ? { version: { tx, at, of } }
        : undefined;
    }
    default:
      return undefined;
  }
}

/** RFC 3339's date-time: a date, a time and an offset. */
const RFC_3339 =
  /^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2})$/;

/**
 * What a version citation is of, or undefined when it isn't one. An entity
 * and a fact read as their own targets do.
 *
 * @example
 * parseVersionOf({ prop: { entity: { type: "Customer", id: "cus_1" }, prop: "tier" } }); // the prop
 * parseVersionOf({ violation: { constraint: "c", key: "k" } }); // undefined: a record is `record`
 */
function parseVersionOf(x: unknown): VersionOf | undefined {
  if (!isObj(x)) return undefined;
  const [kind, v] = Object.entries(x)[0] ?? [];
  if (Object.keys(x).length !== 1 || !isObj(v)) return undefined;
  switch (kind) {
    case "entity":
    case "fact": {
      const t = parseTarget(x);
      return t && ("entity" in t || "fact" in t) ? t : undefined;
    }
    case "prop": {
      const entity = entityRef(v.entity),
        prop = str(v.prop);
      return entity && prop ? { prop: { entity, prop } } : undefined;
    }
    case "record": {
      const constraint = str(v.constraint),
        key = str(v.key);
      return constraint && key ? { record: { constraint, key } } : undefined;
    }
    default:
      return undefined;
  }
}

const OPS = new Set<QueryWhere["op"]>([
  "=",
  "!=",
  "<",
  "<=",
  ">",
  ">=",
  "contains",
  "starts_with",
  "is_null",
  "not_null",
]);
const wheres = (x: unknown): QueryWhere[] | undefined => {
  if (x === undefined || x === null) return [];
  if (!Array.isArray(x)) return undefined;
  const list = x.map((w) =>
    isObj(w) && str(w.prop) && OPS.has(w.op as QueryWhere["op"])
      ? ({ prop: w.prop, op: w.op, value: w.value } as QueryWhere)
      : undefined,
  );
  return list.every((w) => w) ? (list as QueryWhere[]) : undefined;
};
const position = (x: unknown) => {
  const n = num(x);
  return n !== undefined && Number.isInteger(n) && n >= 0 ? n : undefined;
};

/**
 * A typed ontology query (the `query` tool's input), or undefined when the
 * shape isn't one: the page then shows its JSON only.
 */
export function parseQuery(x: unknown): OntologyQuery | undefined {
  if (!isObj(x) || !isObj(x.start) || !str(x.start.type)) return undefined;
  const startWhere = wheres(x.start.where);
  const steps = Array.isArray(x.steps ?? [])
    ? ((x.steps ?? []) as unknown[]).map((s) => {
        const where = isObj(s) ? wheres(s.where) : undefined;
        return isObj(s) &&
          str(s.via) &&
          str(s.to) &&
          (s.dir === "out" || s.dir === "in") &&
          where
          ? { via: s.via as string, dir: s.dir, to: s.to as string, where }
          : undefined;
      })
    : [undefined];
  const filters = Array.isArray(x.filters ?? [])
    ? ((x.filters ?? []) as unknown[]).map((f) => {
        const at = isObj(f) ? position(f.at) : undefined;
        if (!isObj(f) || at === undefined) return undefined;
        for (const k of ["holds", "not_holds", "violates"] as const) {
          const name = str(f[k]);
          if (name)
            return { at, [k]: name } as NonNullable<
              OntologyQuery["filters"]
            >[number];
        }
        return undefined;
      })
    : [undefined];
  const ret = parseReturn(x.return);
  if (!startWhere || !ret || !steps.every((s) => s) || !filters.every((f) => f))
    return undefined;
  const ids = Array.isArray(x.start.ids)
    ? x.start.ids.filter((i): i is string => typeof i === "string")
    : null;
  return {
    start: {
      type: x.start.type as string,
      where: startWhere,
      ids,
      text: str(x.start.text) ?? null,
    },
    steps: steps as NonNullable<OntologyQuery["steps"]>,
    filters: filters as NonNullable<OntologyQuery["filters"]>,
    return: ret,
  };
}

function parseReturn(x: unknown): OntologyQuery["return"] | undefined {
  if (!isObj(x)) return undefined;
  if (isObj(x.rows) && Array.isArray(x.rows.columns)) {
    const columns = x.rows.columns.map((c) =>
      isObj(c) && position(c.at) !== undefined
        ? {
            at: c.at as number,
            ...(str(c.prop) ? { prop: c.prop as string } : {}),
          }
        : undefined,
    );
    if (!columns.every((c) => c)) return undefined;
    const o = x.rows.order_by;
    return {
      rows: {
        columns: columns as { at: number; prop?: string }[],
        order_by:
          isObj(o) && position(o.at) !== undefined && str(o.prop)
            ? {
                at: o.at as number,
                prop: o.prop as string,
                desc: o.desc === true,
              }
            : null,
        ...(num(x.rows.limit) !== undefined
          ? { limit: x.rows.limit as number }
          : {}),
      },
    };
  }
  // A count at the last position leaves `distinct_at` out, or null.
  if (isObj(x.count))
    return x.count.distinct_at === undefined || x.count.distinct_at === null
      ? { count: { distinct_at: null } }
      : position(x.count.distinct_at) !== undefined
        ? { count: { distinct_at: x.count.distinct_at as number } }
        : undefined;
  if (
    isObj(x.group_count) &&
    position(x.group_count.at) !== undefined &&
    str(x.group_count.prop)
  )
    return {
      group_count: {
        at: x.group_count.at as number,
        prop: x.group_count.prop as string,
        ...(num(x.group_count.limit) !== undefined
          ? { limit: x.group_count.limit as number }
          : {}),
      },
    };
  const a = x.aggregate;
  if (
    isObj(a) &&
    ["sum", "avg", "min", "max"].includes(a.fn as string) &&
    position(a.at) !== undefined &&
    str(a.prop)
  ) {
    const g = a.group_by;
    return {
      aggregate: {
        fn: a.fn as "sum" | "avg" | "min" | "max",
        at: a.at as number,
        prop: a.prop as string,
        group_by:
          isObj(g) && position(g.at) !== undefined && str(g.prop)
            ? { at: g.at as number, prop: g.prop as string }
            : null,
        ...(num(a.limit) !== undefined ? { limit: a.limit as number } : {}),
      },
    };
  }
  return undefined;
}

/** One stored event's JSON as an AskEvent, or undefined when it isn't one. */
export function parseAskEvent(eventJson: string): AskEvent | undefined {
  const x = json(eventJson);
  if (!isObj(x)) return undefined;
  switch (x.event) {
    case "started":
      return { event: "started", model: str(x.model) || "" };
    case "tool_call":
      return str(x.call_id) && str(x.tool)
        ? {
            event: "tool_call",
            call_id: x.call_id as string,
            tool: x.tool as string,
            input: x.input,
          }
        : undefined;
    case "tool_result": {
      // Evidence for a recorded read, an error for a failed call, neither
      // for the call that ended the question; never both.
      const call_id = str(x.call_id),
        summary = str(x.summary) ?? "",
        evidence = str(x.evidence_id),
        error = str(x.error);
      if (!call_id || (evidence && error)) return undefined;
      return evidence
        ? { event: "tool_result", call_id, summary, evidence_id: evidence }
        : error
          ? { event: "tool_result", call_id, summary, error }
          : { event: "tool_result", call_id, summary };
    }
    case "finished":
      return { event: "finished" };
    default:
      return undefined;
  }
}

/** A number or null, as an aggregate is; undefined for anything else. */
const numberOrNull = (x: unknown) => (x === null ? null : num(x));

/**
 * A query's recorded output (contract 3.11.0), or undefined when it isn't
 * one: the page then has no visual of it. Values stay as they came, since
 * the page shows each only as text; a row lacking its edges has none.
 *
 * @example
 * parseOutput({ kind: "count", count: 12 }); // { kind: "count", count: 12 }
 * parseOutput({ kind: "group_count", groups: [{ value: { untrusted: "Net 30" }, count: 2 }], total_groups: 1, cut: null });
 * // { kind: "group_count", groups: [{ value: { untrusted: "Net 30" }, count: 2 }], total_groups: 1 }
 * parseOutput({ kind: "rows", rows: "<table>" }); // undefined
 */
export function parseOutput(x: unknown): QueryOutput | undefined {
  if (!isObj(x)) return undefined;
  const cut = str(x.cut) ? { cut: x.cut as string } : {};
  const list = <T>(
    items: unknown,
    item: (v: Record<string, unknown>) => T | undefined,
  ): T[] | undefined => {
    const all = Array.isArray(items)
      ? items.map((v) => (isObj(v) ? item(v) : undefined))
      : [undefined];
    return all.every((v) => v !== undefined) ? (all as T[]) : undefined;
  };
  switch (x.kind) {
    case "rows": {
      const total = position(x.total);
      const rows = list(x.rows, (r) =>
        Array.isArray(r.cells)
          ? { cells: r.cells, edges: Array.isArray(r.edges) ? r.edges : [] }
          : undefined,
      );
      return rows && total !== undefined
        ? { kind: "rows", rows, total, ...cut }
        : undefined;
    }
    case "count": {
      const count = position(x.count);
      return count === undefined ? undefined : { kind: "count", count, ...cut };
    }
    case "aggregate": {
      const value = numberOrNull(x.value),
        values = position(x.values);
      return value === undefined || values === undefined
        ? undefined
        : { kind: "aggregate", value, values, ...cut };
    }
    case "group_count":
    case "grouped_aggregate": {
      const total = position(x.total_groups);
      if (total === undefined) return undefined;
      if (x.kind === "group_count") {
        const groups = list(x.groups, (g) => {
          const count = position(g.count);
          return "value" in g && count !== undefined
            ? { value: g.value, count }
            : undefined;
        });
        return (
          groups && { kind: "group_count", groups, total_groups: total, ...cut }
        );
      }
      const groups = list(x.groups, (g) => {
        const aggregate = numberOrNull(g.aggregate),
          values = position(g.values);
        return "value" in g && aggregate !== undefined && values !== undefined
          ? { value: g.value, aggregate, values }
          : undefined;
      });
      return (
        groups && {
          kind: "grouped_aggregate",
          groups,
          total_groups: total,
          ...cut,
        }
      );
    }
    default:
      return undefined;
  }
}

const VISUAL_KINDS = new Set<VisualKind>(["table", "bar", "line", "figure"]);
// The engine's limits on an answer's visuals and their titles.
export const MAX_VISUALS = 4,
  MAX_VISUAL_TITLE = 80;

/**
 * An answer's visuals, as far as they can be read: each of a query result
 * of `queries`, once, after a block of the `blocks` the result has (`after`
 * as given, an index into the raw blocks, or after the last), at most
 * MAX_VISUALS. A visual that isn't one, names no query result of the
 * answer, repeats one or follows a block the answer lacks is left out; a
 * kind the page doesn't know is no hint (the output's shape decides), and a
 * title is plain text (plainText), cut to MAX_VISUAL_TITLE characters.
 *
 * @example
 * parseVisuals(
 *   [{ evidence_id: "e2", after: 0, kind: "pie", title: "**Top** customers" }, { evidence_id: "e2" }, { evidence_id: "e9" }],
 *   new Set(["e2"]),
 *   1,
 * ); // [{ evidence_id: "e2", after: 0, title: "Top customers" }]
 */
export function parseVisuals(
  x: unknown,
  queries: ReadonlySet<string>,
  blocks: number,
): AskVisual[] {
  const visuals = (Array.isArray(x) ? x : []).flatMap((v): AskVisual[] => {
    if (!isObj(v) || !queries.has(str(v.evidence_id) ?? "")) return [];
    const placed = v.after !== undefined && v.after !== null;
    const after = position(v.after);
    if (placed && (after === undefined || after >= blocks)) return [];
    const kind = VISUAL_KINDS.has(v.kind as VisualKind)
      ? (v.kind as VisualKind)
      : undefined;
    const title = [...plainText(str(v.title) ?? "").replace(/\s+/g, " ")];
    return [
      {
        evidence_id: v.evidence_id as string,
        ...(after !== undefined ? { after } : {}),
        ...(kind ? { kind } : {}),
        ...(title.length
          ? {
              title:
                title.length > MAX_VISUAL_TITLE
                  ? title.slice(0, MAX_VISUAL_TITLE - 1).join("") + "…"
                  : title.join(""),
            }
          : {}),
      },
    ];
  });
  return visuals
    .filter(
      (v, i) => visuals.findIndex((w) => w.evidence_id === v.evidence_id) === i,
    )
    .slice(0, MAX_VISUALS);
}

// Contract 4.0.0's model failures past unavailable and refused, which were
// model_failed before it. Helix's Claude's read as they always did: the
// WFE words every one as unavailable, Helix's to fix. A workspace's own
// provider's that an owner can act on, the WFE words in full sentences
// naming the provider, its model and Settings › AI models, which a
// question's view says instead.
const MODEL_FAILURES = new Set<AskFailure>([
  "model_auth",
  "model_permission",
  "model_not_found",
  "model_rate_limited",
  "model_quota",
  "model_unsupported",
  "model_truncated",
  "model_malformed",
  "model_timeout",
  "model_changed",
  "model_private_host",
]);
const FAILURES = new Set<AskFailure>([
  "model_failed",
  "model_refused",
  ...MODEL_FAILURES,
  "no_answer",
  "load_unknown",
  "load_mismatch",
  "store_failed",
]);

/**
 * A question's result JSON as an AskResult, or undefined when it isn't one
 * (an unknown status, a failure without a known reason). Blocks, citations,
 * queries and visuals that don't parse are left out, and so is an output
 * that doesn't (parseOutput); usage it lacks is 0. A visual's `after` then
 * indexes the blocks kept: one after a block left out follows the block
 * before it, or the first when none is. A result of an earlier contract has
 * no outputs or visuals.
 */
export function parseAskResult(resultJson: string): AskResult | undefined {
  const x = json(resultJson);
  if (!isObj(x)) return undefined;
  const queries = (Array.isArray(x.queries) ? x.queries : []).flatMap((q) => {
    const query = isObj(q) ? parseQuery(q.query) : undefined;
    const output = isObj(q) ? parseOutput(q.output) : undefined;
    return isObj(q) && str(q.evidence_id) && query
      ? [
          {
            evidence_id: q.evidence_id as string,
            query,
            ...(output && { output }),
          },
        ]
      : [];
  });
  const u = isObj(x.usage) ? x.usage : {};
  const common = {
    queries,
    usage: {
      input: num(u.input) ?? 0,
      cache_read: num(u.cache_read) ?? 0,
      cache_write: num(u.cache_write) ?? 0,
      output: num(u.output) ?? 0,
      usd: num(u.usd) ?? 0,
    },
  };
  switch (x.status) {
    case "answered": {
      const answer = isObj(x.answer) ? x.answer : {};
      const raw = Array.isArray(answer.blocks) ? answer.blocks : [];
      const read = raw.map((b): AskBlock | undefined => {
        if (!isObj(b)) return undefined;
        if (typeof b.para === "string") return { para: b.para };
        if (Array.isArray(b.list))
          return {
            list: b.list.filter((i): i is string => typeof i === "string"),
          };
        return undefined;
      });
      const blocks = read.filter((b): b is AskBlock => !!b);
      // A raw block's index among the blocks kept, or the one before it.
      const kept = (at: number) =>
        Math.max(0, read.slice(0, at + 1).filter(Boolean).length - 1);
      const visuals = parseVisuals(
        answer.visuals,
        new Set(queries.map((q) => q.evidence_id)),
        raw.length,
      ).map((v) =>
        typeof v.after === "number" ? { ...v, after: kept(v.after) } : v,
      );
      const citations = (Array.isArray(x.citations) ? x.citations : []).flatMap(
        (c): AskCitation[] => {
          const target = isObj(c) ? parseTarget(c.target) : undefined;
          return isObj(c) && str(c.marker) && str(c.evidence_id) && target
            ? [
                {
                  marker: c.marker as string,
                  evidence_id: c.evidence_id as string,
                  target,
                },
              ]
            : [];
        },
      );
      return {
        status: "answered",
        answer: { blocks, ...(visuals.length ? { visuals } : {}) },
        citations,
        ...common,
      };
    }
    case "cannot_answer":
      return {
        status: "cannot_answer",
        reason: str(x.reason) || "",
        gap: str(x.gap) || "",
        ...common,
      };
    case "budget_exhausted":
      return { status: "budget_exhausted", ...common };
    case "failed":
      return FAILURES.has(x.reason as AskFailure)
        ? { status: "failed", reason: x.reason as AskFailure, ...common }
        : undefined;
    default:
      return undefined;
  }
}

const UNREACHED = "The model couldn’t be reached, after retries.";

/**
 * Why a question failed, for a person. A model failure past unavailable and
 * refused reads as unavailable, as on Helix's Claude, wherever the WFE has
 * no words of a workspace's own provider for it (questionView prefers
 * those).
 */
export const FAILURE_TEXT: Record<AskFailure, string> = {
  model_failed: UNREACHED,
  model_refused: "The model declined to answer.",
  model_auth: UNREACHED,
  model_permission: UNREACHED,
  model_not_found: UNREACHED,
  model_rate_limited: UNREACHED,
  model_quota: UNREACHED,
  model_unsupported: UNREACHED,
  model_truncated: UNREACHED,
  model_malformed: UNREACHED,
  model_timeout: UNREACHED,
  model_changed: UNREACHED,
  model_private_host: UNREACHED,
  no_answer: "The model stopped without an answer.",
  load_unknown: "The load is gone, or being deleted.",
  load_mismatch: "The load holds another version than the one asked about.",
  store_failed: "The graph couldn’t be read.",
};

/**
 * What a question spent, in a line.
 *
 * @example
 * usageText({ input: 6100, cache_read: 12000, cache_write: 0, output: 1450, usd: 0.0734 }); // "$0.07 · 18,100 tokens in (12,000 cached), 1,450 out"
 */
export function usageText(u: AskUsage): string {
  const n = (x: number) => x.toLocaleString("en-US");
  const cached = u.cache_read + u.cache_write;
  return `$${u.usd.toFixed(2)} · ${n(u.input + cached)} tokens in${cached ? ` (${n(cached)} cached)` : ""}, ${n(u.output)} out`;
}

// ---------------------------------------------------------------- the stream

/**
 * How a call ended: recorded as evidence the answer may cite, a repeat of a
 * read an earlier step recorded (answered as that step's evidence, its
 * summary the earlier step's), failed (or refused) with why, or ended the
 * question (`answer`, `cannot_answer`).
 */
export type StepResult =
  | { kind: "recorded"; summary: string; evidenceId: string }
  | { kind: "repeated"; summary: string; evidenceId: string }
  | { kind: "failed"; summary: string; error: string }
  | { kind: "ended"; summary: string };

/** One tool call and, once it returned, its result. */
export type AskStep = {
  callId: string;
  tool: string;
  input: unknown;
  // The input as a typed query, for a `query` call that parses.
  query?: OntologyQuery;
  result?: StepResult;
};

/** A question's events so far, folded. */
export type AskStream = {
  // The last seq folded in: the next poll reads after it.
  lastSeq: number;
  model: string;
  steps: AskStep[];
  finished: boolean;
  // The question ended and every event it sent is folded: none follows.
  complete: boolean;
  // Events that didn't parse.
  unreadable: number;
};

export const EMPTY_STREAM: AskStream = {
  lastSeq: 0,
  model: "",
  steps: [],
  finished: false,
  complete: false,
  unreadable: 0,
};

/**
 * Folds a page of polled events into the stream, in seq order. An event at
 * or before `lastSeq` was folded already (a poll that overlapped) and is
 * skipped; a result without its call is kept as a step of its own. A
 * retried question's events go on from a new `started` (ListQuestionEvents),
 * and the engine numbers evidence from e1 again, so `started` begins the
 * steps afresh: the steps shown are the latest attempt's, the one its result
 * and citations come from. A read the model repeats is answered as the
 * earlier step's evidence, and shown as a repeat of it. The stream is
 * complete once the page says the question ended and its events reach the
 * page's `lastSeq`.
 *
 * @example
 * const s = reduceEvents(EMPTY_STREAM, {
 *   events: [
 *     { seq: 1, eventJson: '{"event":"tool_call","call_id":"a","tool":"query","input":{}}' },
 *     { seq: 2, eventJson: '{"event":"tool_result","call_id":"a","evidence_id":"e1","summary":"query: 3 of 3 rows"}' },
 *   ],
 *   status: "running",
 *   lastSeq: 2,
 * });
 * s.steps[0].result; // { kind: "recorded", summary: "query: 3 of 3 rows", evidenceId: "e1" }
 * s.complete; // false: it is still running
 * const again = reduceEvents(s, {
 *   events: [
 *     { seq: 3, eventJson: '{"event":"tool_call","call_id":"b","tool":"query","input":{}}' },
 *     { seq: 4, eventJson: '{"event":"tool_result","call_id":"b","evidence_id":"e1","summary":"query: as e1"}' },
 *   ],
 * });
 * again.steps[1].result; // { kind: "repeated", summary: "query: 3 of 3 rows", evidenceId: "e1" }
 */
export function reduceEvents(
  stream: AskStream,
  page: ListQuestionEventsResponse,
): AskStream {
  const next = [...(page.events || [])]
    .filter((e) => e.seq > stream.lastSeq)
    .sort((a, b) => a.seq - b.seq)
    .reduce((s, e) => {
      const next = { ...s, lastSeq: e.seq };
      const ev = parseAskEvent(e.eventJson);
      if (!ev) return { ...next, unreadable: s.unreadable + 1 };
      switch (ev.event) {
        case "started":
          return {
            ...next,
            model: ev.model,
            steps: [],
            finished: false,
            unreadable: 0,
          };
        case "finished":
          return { ...next, finished: true };
        case "tool_call":
          return {
            ...next,
            steps: [
              ...s.steps,
              {
                callId: ev.call_id,
                tool: ev.tool,
                input: ev.input,
                ...(ev.tool === "query" && parseQuery(ev.input)
                  ? { query: parseQuery(ev.input) }
                  : {}),
              },
            ],
          };
        case "tool_result": {
          // What the step that recorded this evidence first found, if one did.
          const earlier = s.steps.find(
            (st) =>
              st.result?.kind === "recorded" &&
              st.result.evidenceId === ev.evidence_id,
          )?.result?.summary;
          const result: StepResult =
            ev.evidence_id !== undefined
              ? earlier !== undefined
                ? {
                    kind: "repeated",
                    summary: earlier,
                    evidenceId: ev.evidence_id,
                  }
                : {
                    kind: "recorded",
                    summary: ev.summary,
                    evidenceId: ev.evidence_id,
                  }
              : ev.error !== undefined
                ? { kind: "failed", summary: ev.summary, error: ev.error }
                : { kind: "ended", summary: ev.summary };
          const at = s.steps.findIndex(
            (st) => st.callId === ev.call_id && !st.result,
          );
          return {
            ...next,
            steps:
              at < 0
                ? [
                    ...s.steps,
                    { callId: ev.call_id, tool: "", input: null, result },
                  ]
                : s.steps.map((st, i) => (i === at ? { ...st, result } : st)),
          };
        }
      }
    }, stream);
  const complete =
    !!page.status &&
    !isLiveQuestion(page) &&
    next.lastSeq >= (page.lastSeq || 0);
  return complete === next.complete ? next : { ...next, complete };
}

/** What a tool does, for a step's heading. */
export const TOOL_LABELS: Record<string, string> = {
  // The engine's own step, before the model's first call.
  load_profile: "Read what the load holds",
  describe_type: "Read a type",
  find_entities: "Found entities",
  get_entity: "Read an entity",
  query: "Queried",
  helix_query: "Queried the graph directly",
  explain: "Explained",
  evidence: "Read the evidence",
  violations: "Read rule breaks",
  history_of: "Read the history",
  changes: "Read the changes",
  // The call, not its outcome: a refused answer is answered again.
  answer: "Answer",
  cannot_answer: "Can’t answer",
};
export const toolLabel = (tool: string) =>
  TOOL_LABELS[tool] ?? (tool ? tool.replace(/_/g, " ") : "A result");

/** The tools whose accepted call ends the question. */
export const isEnding = (tool: string) =>
  tool === "answer" || tool === "cannot_answer";

/**
 * A step's heading: what its tool does (a search without a type searched
 * every type), or, for the call that ended the question, how it ended.
 *
 * @example
 * stepHeading({ callId: "a", tool: "answer", input: {}, result: { kind: "ended", summary: "answer: ended the question" } }); // "Answered"
 * stepHeading({ callId: "b", tool: "answer", input: {} }); // "Answer"
 * stepHeading({ callId: "c", tool: "find_entities", input: { by: "text", value: "Acme" } }); // "Searched every type"
 */
export function stepHeading(step: AskStep): string {
  if (
    step.tool === "find_entities" &&
    !str(isObj(step.input) ? step.input.type : undefined)
  )
    return "Searched every type";
  if (step.result?.kind !== "ended") return toolLabel(step.tool);
  return step.tool === "cannot_answer" ? "Couldn’t answer" : "Answered";
}

/**
 * A step's result in a line, after its heading: what it found (its summary
 * without the tool's name, which heads the step), that it repeated an
 * earlier step's read and what that found, why it failed, that an answer
 * was refused (refusalText says why), or nothing for the call that ended
 * the question, whose heading says how.
 *
 * @example
 * resultText({ callId: "a", tool: "query", input: {}, result: { kind: "recorded", summary: "query: 3 of 40 rows", evidenceId: "e2" } }); // "3 of 40 rows"
 * resultText({ callId: "c", tool: "query", input: {}, result: { kind: "repeated", summary: "query: 3 of 40 rows", evidenceId: "e2" } }); // "Same as an earlier step: 3 of 40 rows"
 * resultText({ callId: "b", tool: "answer", input: {}, result: { kind: "ended", summary: "answer: ended the question" } }); // ""
 */
export function resultText(step: AskStep): string {
  const r = step.result;
  if (!r || r.kind === "ended") return "";
  if (r.kind === "failed")
    return step.tool === "answer" ? "Refused" : `Failed: ${r.error}`;
  const own = `${step.tool}: `;
  const found = r.summary.startsWith(own)
    ? r.summary.slice(own.length)
    : r.summary;
  return r.kind === "repeated" ? `Same as an earlier step: ${found}` : found;
}

/**
 * Why the engine refused an answer, for a person: the engine's words are
 * for the model, which then answers again. `corrected` once a later call
 * ended the question.
 *
 * @example
 * refusalText("the answer was refused; fix these and call answer again:\n- citation c2: e3 didn't show {\"entity\":\"n9\"}", true); // "An answer cited something no step had shown; the agent corrected it."
 * refusalText("the answer was refused; fix these and call answer again:\n- visual 1: e9 is not a result of this question", false); // "An answer’s table or chart didn’t pass the checks every visual must; the agent was told to fix it."
 */
export function refusalText(error: string, corrected: boolean): string {
  const what = /didn.t show/.test(error)
    ? "An answer cited something no step had shown"
    : /^- visual \d+:/m.test(error)
      ? "An answer’s table or chart didn’t pass the checks every visual must"
      : "An answer didn’t pass the checks every answer must";
  return `${what}; ${corrected ? "the agent corrected it." : "the agent was told to fix it."}`;
}

// ---------------------------------------------------------------- entities

/** What a question's steps tell the page about the entities they show. */
export type Known = {
  // A handle (such as n3) as the entity the accepted answer resolved it to.
  entity: (handle: unknown) => EntityRef | undefined;
  // An entity's name: as its page names it, else what the agent looked it
  // up by, else its id as is.
  name: (r: EntityRef) => string;
};

export const NOTHING_KNOWN: Known = {
  entity: () => undefined,
  name: (r) => r.id,
};

/**
 * Every entity an answer's citations name, once each: a cited entity, a
 * fact's subject and object, a derived fact's (or live rule break's) args,
 * and those of a recorded version's subject (a prop's entity). These are
 * the entities a page names (knownOf's `named`).
 *
 * @example
 * const order = { type: "Order", id: "ord 2" }, ada = { type: "Customer", id: "cus_1" };
 * citedEntities([
 *   { marker: "c1", evidence_id: "e1", target: { entity: order } },
 *   { marker: "c2", evidence_id: "e2", target: { fact: { link: "placed_by", subject: order, object: ada } } },
 * ]); // [order, ada]
 */
export function citedEntities(citations: readonly AskCitation[]): EntityRef[] {
  const refs = citations.flatMap(({ target }) => {
    // A recorded version names what its subject names.
    const t = "version" in target ? target.version.of : target;
    return "entity" in t
      ? [t.entity]
      : "fact" in t
        ? [t.fact.subject, t.fact.object]
        : "prop" in t
          ? [t.prop.entity]
          : "derived" in t
            ? t.derived.args
            : [];
  });
  return [...new Map(refs.map((r) => [refKey(r), r])).values()];
}

/**
 * What the steps tell about entities. The model names entities by the
 * handles results gave it (n1, n2, ...), never by id; the citations of its
 * accepted answer, as the model wrote them and as the result resolved them,
 * pair the handles it cited with entities, by marker. An entity is named as
 * its page names it when the page read that (`named`, by refKey: one
 * lookup of the cited entities in the question's load). Else, a named
 * type's id being its normalized name ("ord 0000002"), a lookup by name, of
 * the entity's type or of every type, whose value normalizes to an
 * entity's id names that entity as the lookup wrote it ("ORD-0000002"); a
 * lookup of its type comes first. Anything else keeps its id: a keyed
 * type's key ("cus_1") until its page's name is read.
 *
 * @example
 * const known = knownOf(
 *   [{ callId: "a", tool: "find_entities", input: { type: "Order", by: "name", value: "ORD-0002" }, result: { kind: "recorded", summary: "1 Order found", evidenceId: "e1" } }],
 *   [],
 *   new Map([[refKey({ type: "Customer", id: "cus_1" }), "Ada Lovelace"]]),
 * );
 * known.name({ type: "Order", id: "ord 0002" }); // "ORD-0002"
 * known.name({ type: "Order", id: "ord 0003" }); // "ord 0003"
 * known.name({ type: "Customer", id: "cus_1" }); // "Ada Lovelace"
 */
export function knownOf(
  steps: readonly AskStep[],
  citations: readonly AskCitation[],
  named: ReadonlyMap<string, string> = new Map(),
): Known {
  const answered = steps.findLast(
    (s) => s.tool === "answer" && s.result?.kind === "ended",
  );
  const written =
    isObj(answered?.input) && Array.isArray(answered.input.citations)
      ? answered.input.citations
      : [];
  const byMarker = new Map(citations.map((c) => [c.marker, c.target]));
  const handles = new Map(
    written
      .flatMap((w): [unknown, EntityRef | undefined][] => {
        const t = isObj(w) ? byMarker.get(str(w.marker) ?? "") : undefined;
        const x = isObj(w) && isObj(w.target) ? w.target : {};
        if (!t) return [];
        if ("entity" in t) return [[x.entity, t.entity]];
        if ("fact" in t && isObj(x.fact))
          return [
            [x.fact.subject, t.fact.subject],
            [x.fact.object, t.fact.object],
          ];
        if ("derived" in t && isObj(x.derived) && Array.isArray(x.derived.args))
          return x.derived.args.map((h, i) => [h, t.derived.args[i]]);
        return [];
      })
      .flatMap(([h, r]) =>
        typeof h === "string" && r ? [[h, r] as const] : [],
      ),
  );
  const names = new Map(
    steps.flatMap((s) => {
      const i =
        s.tool === "find_entities" &&
        s.result?.kind === "recorded" &&
        isObj(s.input)
          ? s.input
          : {};
      // Every type's lookup is keyed by no type, which no type is.
      const type = str(i.type) ?? "",
        value = str(i.value)?.replace(/\s+/g, " ").trim();
      return i.by === "name" && value
        ? [[refKey({ type, id: normalizeName(value) }), value] as const]
        : [];
    }),
  );
  return {
    entity: (h) => (typeof h === "string" ? handles.get(h) : undefined),
    name: (r) =>
      named.get(refKey(r)) ??
      names.get(refKey(r)) ??
      names.get(refKey({ type: "", id: r.id })) ??
      r.id,
  };
}

/** An entity in words: its type and name. */
const refText = (r: EntityRef, known: Pick<Known, "name">) =>
  `${r.type} ${known.name(r)}`;

/** A value the model wrote, quoted and cut short. */
const quoted = (v: string) => {
  const t = v.replace(/\s+/g, " ").trim();
  return `“${t.length > 80 ? t.slice(0, 79) + "…" : t}”`;
};

/** A predicate without its kind: `relation:co_signed` as "co_signed". */
const predName = (pred: string) => pred.slice(pred.indexOf(":") + 1);

/**
 * What an evidence or history read is of, in words: a fact, a rule break's
 * record, or an entity or, with `prop`, one of its properties.
 */
const targetText = (i: Record<string, unknown>, known: Known) => {
  const t = isObj(i.target) ? i.target : {};
  const prop = str(i.prop);
  if (isObj(t.fact) && str(t.fact.link)) {
    const s = known.entity(t.fact.subject),
      o = known.entity(t.fact.object);
    return s && o
      ? `${refText(s, known)} ${t.fact.link} ${refText(o, known)}`
      : `a ${t.fact.link} fact`;
  }
  const c = isObj(t.record) ? str(t.record.constraint) : undefined;
  if (c) return `a break of ${c}`;
  const e = known.entity(t.entity);
  const who = e ? refText(e, known) : "an entity";
  return prop ? `the ${prop} of ${who}` : who;
};

/**
 * What a step asked for, in words, or undefined when there is nothing to
 * say: a query reads as its path (queryText), a graph query shows as its
 * code (graphQuery), the read of what the load holds asks for nothing, and
 * the calls that end the question show as the answer or callout under the
 * steps. Entities are named as knownOf names them, never by the model's
 * handles. A step's raw input stays behind its JSON.
 *
 * @example
 * stepInput({ callId: "a", tool: "find_entities", input: { type: "Order", by: "name", value: "ORD-2" } }, NOTHING_KNOWN); // "Order named “ORD-2”"
 * stepInput({ callId: "c", tool: "find_entities", input: { by: "text", value: "Acme" } }, NOTHING_KNOWN); // "Matching “Acme”"
 * stepInput({ callId: "b", tool: "evidence", input: { target: { fact: { link: "placed_by", subject: "n1", object: "n2" } } } }, NOTHING_KNOWN); // "For a placed_by fact"
 * stepInput({ callId: "d", tool: "history_of", input: { target: { entity: "n1" }, prop: "status", before: "01JB2", limit: 20 } }, NOTHING_KNOWN); // "History of the status of an entity, further back"
 * stepInput({ callId: "e", tool: "changes", input: { since: "2026-10-01T00:00:00Z", limit: 10 } }, NOTHING_KNOWN); // "Changes since “2026-10-01T00:00:00Z”"
 */
export function stepInput(step: AskStep, known: Known): string | undefined {
  const i = isObj(step.input) ? step.input : {};
  const type = str(i.type);
  switch (step.tool) {
    case "describe_type":
      return type && `The ${type} type`;
    case "find_entities": {
      // Without a type it searched every type, as its heading says.
      const value = str(i.value);
      if (value === undefined) return type;
      const how =
        i.by === "name"
          ? `named ${quoted(value)}`
          : i.by === "identifier"
            ? `with the identifier ${quoted(value)}`
            : `matching ${quoted(value)}`;
      return type
        ? `${type} ${how}`
        : how.charAt(0).toUpperCase() + how.slice(1);
    }
    case "get_entity": {
      const e = known.entity(i.entity);
      if (e) return refText(e, known);
      // Its result says "a <Type>" once it read one, maybe with more after.
      const read =
        step.result?.kind === "recorded"
          ? /^(?:get_entity: )?an? ([^\s,;:()]+)/.exec(step.result.summary)?.[1]
          : undefined;
      return `One ${read ?? "entity"}`;
    }
    case "helix_query": {
      const limit = position(i.limit);
      return limit === undefined
        ? undefined
        : `At most ${plural(limit, "result")}`;
    }
    case "evidence":
      return `For ${targetText(i, known)}`;
    case "history_of":
      // `before` pages back from a transaction the last page ended at.
      return `History of ${targetText(i, known)}${str(i.before) ? ", further back" : ""}`;
    case "changes": {
      // `after` pages on from a transaction the last page ended at.
      const since = str(i.since)?.trim(),
        until = str(i.until)?.trim();
      const range =
        since && until
          ? `Changes between ${quoted(since)} and ${quoted(until)}`
          : since
            ? `Changes since ${quoted(since)}`
            : until
              ? `Changes until ${quoted(until)}`
              : "Every change";
      return `${range}${str(i.after) ? ", continued" : ""}`;
    }
    case "explain": {
      const pred = str(i.pred);
      if (!pred) return undefined;
      const args = Array.isArray(i.args) ? i.args.map(known.entity) : [];
      const who =
        args.length && args.every((a) => a)
          ? ` for ${(args as EntityRef[]).map((a) => refText(a, known)).join(" and ")}`
          : "";
      return `Why ${predName(pred)} holds${who}`;
    }
    case "violations": {
      const c = str(i.constraint);
      return c
        ? `Breaks of ${c}${type ? ` naming a ${type}` : ""}`
        : type
          ? `Breaks naming a ${type}`
          : "Every rule break";
    }
    default:
      return undefined;
  }
}

/**
 * A graph query's code (a `helix_query` call's Helix v3 JSON read), as JSON
 * laid out to read, or undefined for any other step or an input without
 * one. It is shown as is, as code: the page reads nothing into it.
 *
 * @example
 * graphQuery({ callId: "a", tool: "helix_query", input: { query: { count: { input: { nodes: { reference: "all" } } } } } });
 * // '{\n  "count": {\n    "input": {\n      "nodes": {\n        "reference": "all"\n      }\n    }\n  }\n}'
 * graphQuery({ callId: "b", tool: "query", input: { query: {} } }); // undefined
 */
export function graphQuery(step: AskStep): string | undefined {
  const i = isObj(step.input) ? step.input : {};
  return step.tool === "helix_query" && i.query !== undefined
    ? JSON.stringify(i.query, null, 2)
    : undefined;
}

/**
 * A result citation in words: what the step that recorded it read, and
 * that step's number, as the steps list shows it. A query's kind is what
 * it returns (`ran` holds the queries as the agent ran them).
 *
 * @example
 * resultLabel([{ callId: "a", tool: "evidence", input: {}, result: { kind: "recorded", summary: "3 items", evidenceId: "e1" } }], "e1"); // "The evidence read in step 1"
 * resultLabel([{ callId: "p", tool: "load_profile", input: {}, result: { kind: "recorded", summary: "load_profile: 4 of 4 reads", evidenceId: "e1" } }], "e1"); // "The overview of the load in step 1"
 */
export function resultLabel(
  steps: readonly AskStep[],
  evidenceId: string,
  ran: ReadonlyMap<string, OntologyQuery> = new Map(),
): string {
  const at = steps.findIndex(
    (s) => s.result?.kind === "recorded" && s.result.evidenceId === evidenceId,
  );
  const step = steps[at];
  if (!step) return "A step’s result";
  const r = (ran.get(evidenceId) ?? step.query)?.return;
  const FN = { sum: "sum", avg: "average", min: "lowest", max: "highest" };
  const what =
    step.tool === "query"
      ? !r || "rows" in r
        ? "The query"
        : "count" in r
          ? "The count"
          : "group_count" in r
            ? "The counts"
            : `The ${FN[r.aggregate.fn]}`
      : ((
          {
            load_profile: "The overview of the load",
            describe_type: "The type read",
            find_entities: "The search",
            get_entity: "The entity read",
            helix_query: "The graph query",
            explain: "The explanation",
            evidence: "The evidence read",
            violations: "The rule breaks read",
            history_of: "The history read",
            changes: "The changes read",
          } as Record<string, string>
        )[step.tool] ?? "The result");
  return `${what} in step ${at + 1}`;
}

// ---------------------------------------------------------------- queries

const valueText = (v: unknown) =>
  typeof v === "string"
    ? /^today([+-]\d+d)?$/.test(v)
      ? v
      : JSON.stringify(v)
    : typeof v === "number" || typeof v === "boolean"
      ? String(v)
      : JSON.stringify(v ?? null);

const whereText = (w: QueryWhere) => {
  switch (w.op) {
    case "is_null":
      return `${w.prop} is empty`;
    case "not_null":
      return `${w.prop} is set`;
    case "contains":
      return `${w.prop} contains ${valueText(w.value)}`;
    case "starts_with":
      return `${w.prop} starts with ${valueText(w.value)}`;
    default:
      return `${w.prop} ${w.op} ${valueText(w.value)}`;
  }
};
const whereClause = (ws: QueryWhere[] = []) =>
  ws.length ? ` where ${ws.map(whereText).join(" and ")}` : "";

/** A typed query, read aloud: its path, its filters and what it returns. */
export type QueryText = { path: string; filters: string[]; returns: string };

/**
 * How a query names its positions and columns: a position by its type (a
 * type at more than one position gets its position: "Customer@2"), a
 * column by its position and prop ("Invoice.amount"), or the position alone
 * for the entity there.
 *
 * @example
 * const { at, col } = queryNames({
 *   start: { type: "Customer" },
 *   steps: [{ via: "refers", dir: "out", to: "Customer" }],
 *   return: { count: {} },
 * });
 * at(1); // "Customer@1"
 * col({ at: 0, prop: "name" }); // "Customer@0.name"
 */
export function queryNames(q: OntologyQuery) {
  const types = [q.start.type, ...(q.steps || []).map((s) => s.to)];
  const at = (i: number) => {
    const t = types[i];
    if (t === undefined) return `position ${i}`;
    return types.filter((x) => x === t).length > 1 ? `${t}@${i}` : t;
  };
  const col = (c: { at: number; prop?: string | null }) =>
    c.prop ? `${at(c.at)}.${c.prop}` : at(c.at);
  return { at, col, last: types.length - 1 };
}

/** What an aggregate's function computes, at the start of a phrase. */
export const AGGREGATE_FN = {
  sum: "Sum",
  avg: "Average",
  min: "Lowest",
  max: "Highest",
} as const;

/**
 * A typed query as a readable path, with its filters and what it returns,
 * named as queryNames names them.
 *
 * @example
 * queryText({
 *   start: { type: "Invoice", where: [{ prop: "status", op: "=", value: "open" }] },
 *   steps: [{ via: "billed_to", dir: "out", to: "Customer" }],
 *   return: { count: { distinct_at: 1 } },
 * }).path; // 'Invoice where status = "open" → billed_to → Customer'
 */
export function queryText(q: OntologyQuery): QueryText {
  const { at, col, last } = queryNames(q);
  const start =
    at(0) +
    (q.start.ids?.length ? ` (${q.start.ids.join(", ")})` : "") +
    (q.start.text ? ` matching ${JSON.stringify(q.start.text)}` : "") +
    whereClause(q.start.where);
  const path = (q.steps || []).reduce(
    (p, s, i) =>
      p +
      (s.dir === "out" ? ` → ${s.via} → ` : ` ← ${s.via} ← `) +
      at(i + 1) +
      whereClause(s.where),
    start,
  );
  const filters = (q.filters || []).map((f) =>
    "holds" in f
      ? `${at(f.at)} holds ${f.holds}`
      : "not_holds" in f
        ? `${at(f.at)} doesn’t hold ${f.not_holds}`
        : `${at(f.at)} breaks ${f.violates}`,
  );
  const r = q.return;
  const returns =
    "rows" in r
      ? [
          `Rows: ${r.rows.columns.map(col).join(", ")}`,
          ...(r.rows.order_by
            ? [
                `sorted by ${col(r.rows.order_by)}${r.rows.order_by.desc ? ", highest first" : ""}`,
              ]
            : []),
          ...(r.rows.limit !== undefined ? [`at most ${r.rows.limit}`] : []),
        ].join(" · ")
      : "count" in r
        ? `Count of distinct ${at(r.count.distinct_at ?? last)}`
        : "group_count" in r
          ? `Count by ${at(r.group_count.at)}.${r.group_count.prop}` +
            (r.group_count.limit !== undefined
              ? ` · top ${r.group_count.limit}`
              : "")
          : `${AGGREGATE_FN[r.aggregate.fn]} of ${at(r.aggregate.at)}.${r.aggregate.prop}` +
            (r.aggregate.group_by ? ` by ${col(r.aggregate.group_by)}` : "");
  return { path, filters, returns };
}

// ---------------------------------------------------------------- answers

/**
 * Model text as plain text: HTML tags and Markdown images go, a Markdown
 * link keeps only its words, and emphasis and code marks go. The page never
 * renders model text as anything but text; this keeps markup from showing
 * as noise.
 *
 * @example
 * plainText('<b>Paid</b> ![x](https://e.com/x.png) [see](https://e.com) **now**'); // "Paid see now"
 */
export function plainText(text: string): string {
  return text
    .replace(/!\[[^\]]*\]\([^)]*\)/g, "")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/<\/?[a-zA-Z][^>]*>/g, "")
    .replace(/\*\*|__|`/g, "")
    .replace(/^[ \t]*#{1,6}[ \t]+/gm, "")
    .replace(/[ \t]{2,}/g, " ")
    .trim();
}

/** A run of an answer: text, or a citation chip by its number (from 1). */
export type AnswerPart = { text: string } | { cite: number };
/** A block shown, by `at`, its index among the answer's blocks. */
export type AnswerBlock = { at: number } & (
  | { kind: "para"; parts: AnswerPart[] }
  | { kind: "list"; items: AnswerPart[][] }
);
export type AnswerView = {
  blocks: AnswerBlock[];
  // The citations the text cites, numbered by first use.
  sources: { n: number; citation: AskCitation }[];
};

const MARKER = /\[(c\d+)\]/g;

/**
 * An answer's blocks as plain text and citation chips. Markers become chips
 * numbered by first use; a marker no citation resolves is dropped, and so is
 * every bit of markup (plainText). A block left with nothing to show is
 * left out; each block shown keeps its index (`at`), which visuals follow.
 *
 * @example
 * const v = answerView([{ para: "Globex [c2] signed [c1]." }], [
 *   { marker: "c1", evidence_id: "e1", target: { entity: { type: "Contract", id: "msa" } } },
 *   { marker: "c2", evidence_id: "e2", target: { entity: { type: "Customer", id: "cus_1" } } },
 * ]);
 * v.blocks[0]; // { at: 0, kind: "para", parts: [{ text: "Globex" }, { cite: 1 }, { text: "signed" }, { cite: 2 }, { text: "." }] }
 */
export function answerView(
  blocks: readonly AskBlock[],
  citations: readonly AskCitation[],
): AnswerView {
  const byMarker = new Map(citations.map((c) => [c.marker, c]));
  const numbers = new Map<string, number>();
  const parts = (text: string): AnswerPart[] =>
    text.split(MARKER).flatMap((piece, i): AnswerPart[] => {
      if (i % 2 === 1) {
        if (!byMarker.has(piece)) return [];
        if (!numbers.has(piece)) numbers.set(piece, numbers.size + 1);
        return [{ cite: numbers.get(piece)! }];
      }
      const t = plainText(piece);
      return t ? [{ text: t }] : [];
    });
  const view = blocks.flatMap((b, at): AnswerBlock[] => {
    if ("para" in b) {
      const p = parts(b.para);
      return p.length ? [{ at, kind: "para", parts: p }] : [];
    }
    const items = b.list.map(parts).filter((p) => p.length);
    return items.length ? [{ at, kind: "list", items }] : [];
  });
  return {
    blocks: view,
    sources: [...numbers].map(([marker, n]) => ({
      n,
      citation: byMarker.get(marker)!,
    })),
  };
}

// ---------------------------------------------------------------- citations

/** What a page knows to name and link a citation. */
export type CitationContext = {
  ontologyId: string;
  // The load the question asked about: links read it.
  loadId: string;
  // Whether a type of the load's version is keyed; undefined if it doesn't know it.
  keyed: (type: string) => boolean | undefined;
  docName: (docId: string) => string;
  // A source id's table, once named, and its page in Data: a synced table,
  // or a table file of a document set.
  table: (
    source: string,
  ) => { to: string; table: string; connector: string } | undefined;
  // An entity's name: as its page names it, else as the question's steps
  // tell it (knownOf).
  name: (r: EntityRef) => string;
  // A result citation's words: the step that recorded it (resultLabel).
  result: (evidenceId: string) => string;
};

/** `relation:co_signed` as "co signed". */
const predText = (pred: string) =>
  pred.slice(pred.indexOf(":") + 1).replace(/_/g, " ");

/**
 * A citation in words, for its chip's name and the sources list.
 *
 * @example
 * citationText({ quote: { doc_id: "d1", section: 0, start: 4, end: 9 } }, { docName: () => "contract.md" } as never); // "Quote in contract.md"
 * citationText({ entity: { type: "Order", id: "ord 2" } }, { name: () => "ORD-2" } as never); // "Order ORD-2"
 * citationText({ row: { source: "foundry_enrichment", mapping: "enrichment_people", row_key: "" } }, {} as never); // "A People enrichment profile"
 * citationText({ version: { tx: "01J…", at: "2026-10-03T09:14:07Z", of: { record: { constraint: "must_sign", key: "9f2c" } } } }, {} as never); // "As recorded Oct 3, 2026, 09:14 AM: a break of must_sign" (en-US, UTC)
 */
export function citationText(t: CitationTarget, ctx: CitationContext): string {
  const ref = (r: EntityRef) => refText(r, ctx);
  if ("entity" in t) return ref(t.entity);
  if ("fact" in t)
    return `${ref(t.fact.subject)} ${t.fact.link} ${ref(t.fact.object)}`;
  if ("derived" in t && t.derived.pred.startsWith("violation:"))
    return `A break of ${t.derived.pred.slice("violation:".length)}: ${t.derived.args.map(ref).join(", ")}`;
  if ("derived" in t)
    return `${predText(t.derived.pred)}: ${t.derived.args.map(ref).join(", ")} (derived)`;
  // A profile the load's People enrichment found, which Ask cites without
  // its id.
  if ("row" in t && t.row.source === ENRICHMENT_SOURCE)
    return "A People enrichment profile";
  if ("row" in t) {
    const table = ctx.table(t.row.source);
    return `${table ? `${table.connector} · ${table.table}` : "A table"} row ${t.row.row_key}`;
  }
  if ("quote" in t) return `Quote in ${ctx.docName(t.quote.doc_id)}`;
  if ("violation" in t) return `A break of ${t.violation.constraint}`;
  if ("version" in t) {
    const of = t.version.of;
    const subject =
      "prop" in of
        ? `the ${of.prop.prop} of ${ref(of.prop.entity)}`
        : "record" in of
          ? `a break of ${of.record.constraint}`
          : citationText(of, ctx);
    return `As recorded ${formatDate(t.version.at)}: ${subject}`;
  }
  return ctx.result(t.result.evidence_id);
}

/**
 * Where a citation opens: an entity's page (a fact its subject's Facts, a
 * derived fact its first entity's Derived, and a live rule break, derived
 * `violation:<c>`, its first entity's Needs review), a document at the
 * quoted span, the row's table (synced, or a file; a People enrichment
 * profile: the load that applied it), or the load's rule breaks of the
 * constraint from the cited record, each reading the question's load. A
 * recorded version opens its subject as the load holds it now (a prop its
 * entity, a fact its subject's Facts); a version of a violation record
 * opens nothing, since the load may no longer hold that record. A query's
 * result opens in the page (undefined), as does an entity of a type the
 * version doesn't know.
 */
export function citationHref(
  t: CitationTarget,
  ctx: CitationContext,
): string | undefined {
  const o = ctx.ontologyId;
  const entity = (r: EntityRef, tab?: string) => {
    const keyed = ctx.keyed(r.type);
    if (keyed === undefined) return undefined;
    const e = entityId(
      keyed ? { type: r.type, key: r.id } : { type: r.type, name: r.id },
    );
    return withLoad(ontologyPaths.entity(o, e, tab), ctx.loadId);
  };
  if ("entity" in t) return entity(t.entity);
  if ("fact" in t) return entity(t.fact.subject, "facts");
  if ("derived" in t)
    return (
      t.derived.args[0] &&
      entity(
        t.derived.args[0],
        t.derived.pred.startsWith("violation:") ? "reviews" : "derived",
      )
    );
  if ("violation" in t)
    return ontologyPaths.loadRuleBreaks(
      o,
      ctx.loadId,
      t.violation.constraint,
      t.violation.key,
    );
  if ("row" in t && t.row.source === ENRICHMENT_SOURCE)
    return ontologyPaths.load(o, ctx.loadId);
  if ("row" in t) {
    return ctx.table(t.row.source)?.to;
  }
  if ("quote" in t)
    return withLoad(
      ontologyPaths.document(o, t.quote.doc_id, undefined, t.quote),
      ctx.loadId,
    );
  if ("version" in t) {
    const of = t.version.of;
    if ("record" in of) return undefined;
    return citationHref("prop" in of ? { entity: of.prop.entity } : of, ctx);
  }
  return undefined;
}

// ---------------------------------------------------------------- questions

/** The WFE's limit on a question, in characters (code points). */
export const MAX_QUESTION = 4000;

/** A result of one status. */
export type Ended<S extends AskResult["status"]> = Extract<
  AskResult,
  { status: S }
>;

/** A question as the page shows it. */
export type QuestionView =
  | { kind: "waiting" }
  | { kind: "queued" }
  // An attempt failed (`error` says why) and the worker will try again.
  | { kind: "retrying"; error: string }
  | { kind: "running" }
  | { kind: "answered"; result: Ended<"answered"> }
  | { kind: "cannot_answer"; result: Ended<"cannot_answer"> }
  | { kind: "budget_exhausted"; result: Ended<"budget_exhausted"> }
  | { kind: "failed"; error: string; result?: AskResult }
  | { kind: "cancelled" };

/** Millionths of a dollar as a number, 0 when unset or unreadable. */
const micros = (s?: string) => Number(s || 0) || 0;

/**
 * Whether the month's questions budget can't hold another question's
 * reservation: a queued question then waits.
 */
export function budgetFull(b?: QuestionBudget | null): boolean {
  if (!b) return false;
  return (
    micros(b.spentUsdMicros) +
      micros(b.reservedUsdMicros) +
      micros(b.questionUsdMicros) >
    micros(b.monthlyUsdMicros)
  );
}

/**
 * What a question's row says: queued (waiting for the month's budget, or
 * for another attempt after one failed), running, or how it ended. The
 * engine's result decides an ended one's kind, so a budget stop the backend
 * records as failed shows as one, and a failure says the engine's reason:
 * a model failure past unavailable and refused in the WFE's words when they
 * are a workspace provider's full sentence, which names it and what an
 * owner can do (Helix's Claude's lowercase words say it is unavailable).
 */
export function questionView(
  q: FoundryQuestion,
  budget?: QuestionBudget | null,
): QuestionView {
  switch (q.status) {
    case "queued":
      return (q.attempts || 0) > 0
        ? {
            kind: "retrying",
            error: sentence(q.error || "the last attempt stopped"),
          }
        : budgetFull(budget)
          ? { kind: "waiting" }
          : { kind: "queued" };
    case "running":
      return { kind: "running" };
    case "cancelled":
      return { kind: "cancelled" };
  }
  const result = q.resultJson ? parseAskResult(q.resultJson) : undefined;
  if (result?.status === "answered") return { kind: "answered", result };
  if (result?.status === "cannot_answer")
    return { kind: "cannot_answer", result };
  if (result?.status === "budget_exhausted")
    return { kind: "budget_exhausted", result };
  if (result?.status === "failed")
    return {
      kind: "failed",
      error:
        MODEL_FAILURES.has(result.reason) && /[.!?]$/.test(q.error || "")
          ? q.error!
          : FAILURE_TEXT[result.reason],
      result,
    };
  return {
    kind: "failed",
    error: q.error
      ? sentence(q.error)
      : q.status === "answered" || q.status === "cannot_answer"
        ? "Its answer couldn’t be read."
        : "It stopped before answering.",
  };
}

/** Whether a question (or its events' page) is queued or running. */
export const isLiveQuestion = (q?: { status?: QuestionStatus } | null) =>
  q?.status === "queued" || q?.status === "running";

/**
 * What a question was asked about (its `focusJson`), or undefined for none
 * or JSON that isn't an EntityRef.
 *
 * @example
 * questionFocus({ id: "q1", focusJson: '{"type":"Customer","id":"cus_1"}' }); // { type: "Customer", id: "cus_1" }
 * questionFocus({ id: "q2" }); // undefined
 */
export const questionFocus = (q: FoundryQuestion): EntityRef | undefined =>
  q.focusJson ? entityRef(json(q.focusJson)) : undefined;

/** Questions of one thread, oldest first. */
export type Thread = {
  id: string;
  questions: FoundryQuestion[];
  // When its newest question was asked.
  last: string;
  live: boolean;
};

/**
 * Questions grouped by thread, the thread with the newest question first.
 * A question without a thread is a thread of its own.
 */
export function threadsOf(questions: readonly FoundryQuestion[]): Thread[] {
  const byThread = new Map<string, FoundryQuestion[]>();
  for (const q of questions) {
    const t = q.threadId || q.id;
    byThread.set(t, [...(byThread.get(t) || []), q]);
  }
  return [...byThread]
    .map(([id, qs]) => {
      const sorted = [...qs].sort((a, b) =>
        (a.createdAt || "").localeCompare(b.createdAt || ""),
      );
      return {
        id,
        questions: sorted,
        last: sorted[sorted.length - 1]!.createdAt || "",
        live: sorted.some(isLiveQuestion),
      };
    })
    .sort((a, b) => b.last.localeCompare(a.last));
}

/** A heading of the thread list: when its threads were last asked in. */
export type ThreadGroup = {
  label: "Today" | "Previous 7 days" | "Older";
  threads: Thread[];
};

/**
 * Threads, newest first as threadsOf sorts them, grouped by when their
 * newest question was asked, in local time: today (or later, for a clock
 * behind the server's), the 7 days before today, then older or undated.
 * Each group keeps its threads' order; empty groups are left out.
 *
 * @example
 * const at = (id: string, last: Date) => ({ id, questions: [], last: last.toISOString(), live: false });
 * threadGroups(
 *   [at("a", new Date(2026, 9, 22, 9)), at("b", new Date(2026, 9, 15, 0)), at("c", new Date(2026, 9, 14, 23))],
 *   new Date(2026, 9, 22, 15),
 * ).map((g) => [g.label, g.threads.map((t) => t.id)]);
 * // [["Today", ["a"]], ["Previous 7 days", ["b"]], ["Older", ["c"]]]
 */
export function threadGroups(
  threads: readonly Thread[],
  now: Date,
): ThreadGroup[] {
  const day = (back: number) =>
    new Date(now.getFullYear(), now.getMonth(), now.getDate() - back).getTime();
  const today = day(0),
    week = day(7);
  // An unreadable date is NaN, which no comparison holds: Older.
  const labelOf = (t: Thread): ThreadGroup["label"] => {
    const at = Date.parse(t.last);
    return at >= today ? "Today" : at >= week ? "Previous 7 days" : "Older";
  };
  return (["Today", "Previous 7 days", "Older"] as const)
    .map((label) => ({
      label,
      threads: threads.filter((t) => labelOf(t) === label),
    }))
    .filter((g) => g.threads.length > 0);
}

/**
 * Up to four questions a new thread may start from: an agent draft's
 * competency questions, the ones it was drafted to answer, each once. None
 * for a Definition v1 or a version not read.
 *
 * @example
 * askSuggestions({ id: "v1", format: "ONTOLOGY_FORMAT_FOUNDRY_V1" }); // []
 * askSuggestions(undefined); // []
 */
export function askSuggestions(v?: OntologyVersion | null): string[] {
  if (!v || versionFormat(v) !== "agent_bundle") return [];
  return [
    ...new Set(
      readAgentBundle(v.definition)
        .competency.map((c) => c.question.trim())
        .filter(Boolean),
    ),
  ].slice(0, 4);
}

/**
 * The ontology an Ask path is about, or "" for /ask itself, which opens
 * the ontology last opened, for a segment that isn't valid
 * percent-encoding, and for any other path.
 *
 * @example
 * askOntologyId("/ask/ont_acme%20contracts"); // "ont_acme contracts"
 * askOntologyId("/ask"); // ""
 * askOntologyId("/ask/ont%ZZ"); // ""
 * askOntologyId("/explore/ont_1"); // ""
 */
export function askOntologyId(pathname: string): string {
  const m = /^\/ask\/([^/]+)/.exec(pathname);
  if (!m) return "";
  try {
    return decodeURIComponent(m[1]!);
  } catch {
    return "";
  }
}

/** What the Ask page shows. */
export type AskView =
  | { kind: "thread"; id: string }
  | { kind: "new" }
  // A narrow screen's list of threads, where a wide one has its panel.
  | { kind: "threads" };

/**
 * What the Ask page shows, from its URL, the screen and how many threads
 * the list holds (undefined while it's read or when it couldn't be): a
 * thread; a new thread, for `?new` or `?focus`, or by default where the
 * threads are beside it (a wide screen) or there are none; else, on a
 * narrow screen, the threads, which also say why they couldn't be read.
 *
 * @example
 * askView(new URLSearchParams("thread=q1"), true, 3); // { kind: "thread", id: "q1" }
 * askView(new URLSearchParams(""), true, 3); // { kind: "threads" }
 * askView(new URLSearchParams("new=1"), true, 3); // { kind: "new" }
 * askView(new URLSearchParams(""), true, 0); // { kind: "new" }
 * askView(new URLSearchParams(""), false, 3); // { kind: "new" }
 */
export function askView(
  params: URLSearchParams,
  narrow: boolean,
  threads: number | undefined,
): AskView {
  const thread = params.get("thread");
  if (thread) return { kind: "thread", id: thread };
  return !narrow || params.has("new") || params.get("focus") || threads === 0
    ? { kind: "new" }
    : { kind: "threads" };
}

/**
 * The goal a new generation starts from when the ontology can't answer: the
 * question, and what it lacks.
 */
export function draftGoal(
  question: string,
  result: Ended<"cannot_answer">,
): string {
  return [
    `Make the ontology able to answer: “${question.trim()}”`,
    result.gap ? `It lacks: ${plainText(result.gap)}` : "",
    result.reason ? `Why it couldn’t: ${plainText(result.reason)}` : "",
  ]
    .filter(Boolean)
    .join("\n");
}
