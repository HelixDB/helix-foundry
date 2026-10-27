import type { OntologyQuery } from "./wfe";

// The ontology agent's Read contract (contract/ontology_agent/read_request.json
// and read_result.json), as the WFE carries it: POST
// .../loads/{loadId}:read takes the reads as JSON (`readsJson`, each a
// ReadItem) and returns the engine's ReadResult as JSON (`resultJson`). The
// backend fills in the rest of the engine's ReadRequest (database, load,
// ontology, budget, `as_of`). The rules are evaluated when the read runs,
// never stored: a derived fact or flag holds for this load now, or the read
// says why it couldn't tell.
//
// The SPA builds reads and parses results here; the mock WFE answers with
// these shapes (mock/graph/rules.ts), and tests/unit/agent-contract.test.ts
// holds both to the schemas. Nothing in a result is trusted: each parser
// keeps only well-formed rows.

/** At most this many reads go in one request. */
export const MAX_READS = 20;

/**
 * An entity as the engine names it: its type, and its entity_key (a keyed
 * type) or norm_name (a type known by its name). The type says which.
 */
export type EntityRef = { type: string; id: string };

/** A stated fact: a link between two entities. */
export type FactRef = { link: string; subject: EntityRef; object: EntityRef };

export type Severity = "block" | "warn" | "info";

/**
 * One read of a request (ReadItem). `pred` strings are `<kind>:<name>` (see
 * Pred). A field left out takes the engine's default: a violations page of
 * 100 records from the first, evidence up to 10 pieces.
 */
export type Read =
  | { entity: EntityRef }
  | { pattern: { pred: string; args: (EntityRef | null)[] } }
  | { explain: { pred: string; args: EntityRef[] } }
  | {
      violations: {
        // A `[constraints.<name>]` name, `<link>.cardinality` or `<link>.required`.
        constraint?: string | null;
        entity?: EntityRef | null;
        // The `next` of the page before; null for the first page.
        after?: string | null;
        // 1 to 1,000.
        limit?: number;
      };
    }
  | { evidence: { fact: FactRef; limit?: number } }
  | { evidence: { entity: EntityRef; prop?: string | null; limit?: number } }
  | { query: OntologyQuery };

// ---------------------------------------------------------------- Preds

export type PredKind = "link" | "relation" | "derived" | "violation";
/** A predicate: a link, a relation, a rule's head, or a constraint's violations. */
export type Pred = { kind: PredKind; name: string };
/**
 * A proof step's predicate: a Pred, an entity's type, a link's `has`, or a
 * value rule's property `test` (`amount > 1000`) or two-property `compare`
 * (`amount > limit`), as the rule writes it.
 */
export type StepPred = {
  kind: PredKind | "type" | "has" | "test" | "compare";
  name: string;
};

const PRED_KINDS: readonly PredKind[] = [
  "link",
  "relation",
  "derived",
  "violation",
];
const STEP_KINDS: readonly StepPred["kind"][] = [
  ...PRED_KINDS,
  "type",
  "has",
  "test",
  "compare",
];

function kindAndName<K extends string>(
  s: unknown,
  kinds: readonly K[],
): { kind: K; name: string } | undefined {
  if (typeof s !== "string") return undefined;
  const i = s.indexOf(":");
  const kind = s.slice(0, i) as K,
    name = s.slice(i + 1);
  return i > 0 && name && kinds.includes(kind) ? { kind, name } : undefined;
}

/**
 * A pred string's kind and name, or undefined when it isn't one.
 *
 * @example
 * parsePred("derived:invoiced"); // { kind: "derived", name: "invoiced" }
 * parsePred("invoiced"); // undefined
 * parsePred("type:Customer"); // undefined: only proof steps name types
 */
export const parsePred = (s: unknown): Pred | undefined =>
  kindAndName(s, PRED_KINDS);

/**
 * A proof step's pred: a Pred, `type:<T>`, `has:<link>`, `test:<test>` or
 * `compare:<comparison>`.
 *
 * @example
 * parseStepPred("type:Customer"); // { kind: "type", name: "Customer" }
 * parseStepPred("test:amount > 1000"); // { kind: "test", name: "amount > 1000" }
 */
export const parseStepPred = (s: unknown): StepPred | undefined =>
  kindAndName(s, STEP_KINDS);

/** A pred's string, as reads send it. */
export const predString = (p: Pred) => `${p.kind}:${p.name}`;

// ---------------------------------------------------------------- Results

/**
 * Why a read has no answer (Failure), by its code: `budget` (too deep to
 * compute live) and `hub` (an adjacency list past `per_list`) with the
 * limit, `too_broad` (a query past `scan_entities`) with the limit and the
 * count, `unknown_entity`, and `invalid` with a JSON pointer into a query.
 */
export type ReadFailure =
  | { code: "budget" | "hub"; message: string; limit: number }
  | { code: "too_broad"; message: string; limit: number; count: number }
  | { code: "unknown_entity"; message: string }
  | { code: "invalid"; message: string; pointer?: string };
export type ReadErrorCode = ReadFailure["code"];
/** Why one pred (or the live violations) of an entity read has no answer. */
export type PartFailure = Extract<
  ReadFailure,
  { code: "budget" | "hub" | "invalid" }
>;

/** How a fact rules give is held: only derived, or stated and derived as well. */
export type Basis = "derived" | "both";

/**
 * A fact rules give that names the entity read, at any place in `args`.
 * `holder` is the link that holds it (a link's own facts, and a relation's
 * through its member link), or null for one only rules give; `rules` name
 * what derives it (`rule:<name>`, `constraint:<name>`, `decl:<link>.<kind>`).
 */
export type DerivedFact = {
  pred: Pred;
  holder: string | null;
  args: EntityRef[];
  basis: Basis;
  rules: string[];
};
/** A flag: a one-place rule-defined predicate that holds for the entity. */
export type Flag = { name: string; rules: string[] };
/**
 * A violation naming the entity first, as the rules have it now. `stored`:
 * the load keeps a record of it (a sweep or commit wrote it); a self-link a
 * rule would make (`<link>.self_loop`) is never stored.
 */
export type LiveViolation = {
  constraint: string;
  severity: Severity;
  entities: EntityRef[];
  stored: boolean;
};
/**
 * A part of an entity read that couldn't be read: one pred, whose facts are
 * then all absent, or its live violations.
 */
export type PartError = { part: Pred | "violations"; failure: PartFailure };

export type EntityResult = {
  entity: EntityRef;
  derived: DerivedFact[];
  flags: Flag[];
  violations: LiveViolation[];
  errors: PartError[];
};

export type PatternRow = {
  args: EntityRef[];
  holder: string | null;
  basis: "stated" | Basis;
  rules: string[];
};
export type PatternResult = { rows: PatternRow[] };

/**
 * How a proof step holds: stated in the data (its evidence is the fact's),
 * by the stored property values (`property`: a `test:` or `compare:` step
 * that holds of them, its evidence the properties'), an entity's type, the same fact through its relation (`member`), by a
 * rule or a constraint, by a link's declaration (`decl:<owner>.<decl>`, such
 * as `decl:bills.inverse_of.billed_by`), or because no fact matches (`not`).
 */
export type Via =
  | { kind: "stated" | "property" | "type" | "member" | "not" }
  | { kind: "rule" | "constraint"; name: string }
  | { kind: "decl"; owner: string; decl: string };
/**
 * One step of a proof; `premises` are indexes of the steps it rests on. An
 * arg is null only in a `not` step, where the rule wrote `_`. `test` is the
 * property test the step stands for (`amount > 1000`, or `absent <prop>` in
 * a `not` step), when it is one.
 */
export type ExplainStep = {
  pred: StepPred;
  args: (EntityRef | null)[];
  via: Via;
  premises: number[];
  test?: string;
};
/** A proof; `root` is null when the fact doesn't hold. */
export type ExplainResult = { steps: ExplainStep[]; root: number | null };

/** A stored violation record (StoredViolation). */
export type ViolationRecord = {
  key: string;
  constraint: string;
  version: string;
  severity: Severity;
  entities: EntityRef[];
  // What opened it, such as `doc:<doc_id>`.
  openedBy: string;
};
/** A page of records in key order; `next` is the `after` of the next page, null on the last. */
export type ViolationsResult = {
  records: ViolationRecord[];
  next: string | null;
};

/**
 * Stored evidence: a quote's offsets are UTF-8 bytes into its section. A
 * quote read for a property carries the value it states (document data,
 * never markup); a fact's quote has none.
 */
export type EvidenceItem =
  | {
      kind: "row";
      source: string;
      mapping: string;
      rowKey: string;
      confidence: number;
    }
  | {
      kind: "quote";
      documentId: string;
      section: number;
      start: number;
      end: number;
      quote: string;
      confidence: number;
      value?: string | number | boolean;
    };
export type EvidenceResult = { items: EvidenceItem[] };

/** One read's outcome, raw: its `ok` is parsed by the read's own parser. */
export type Outcome<T> =
  | { ok: true; value: T }
  | { ok: false; error: ReadFailure };
export type ReadStats = { instances: number; wallMs: number };
export type ReadResult = {
  asOf: string;
  results: Outcome<unknown>[];
  stats: ReadStats;
};

type Row = Record<string, unknown>;
const isRow = (v: unknown): v is Row =>
  !!v && typeof v === "object" && !Array.isArray(v);
const list = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const str = (v: unknown) => (typeof v === "string" ? v : undefined);
const count = (v: unknown) =>
  typeof v === "number" && Number.isInteger(v) && v >= 0 ? v : undefined;
const strings = (v: unknown) =>
  list(v).filter((x): x is string => typeof x === "string");
const severity = (v: unknown): Severity | undefined =>
  v === "block" || v === "warn" || v === "info" ? v : undefined;
const basis = (v: unknown): Basis | undefined =>
  v === "derived" || v === "both" ? v : undefined;
const isKind = (v: unknown, kind: string): v is Row =>
  isRow(v) && v.kind === kind;
/** A holder: `link:<l>` as the link's name, null as null, else undefined. */
const holderOf = (v: unknown): string | null | undefined => {
  if (v === null) return null;
  const p = parsePred(v);
  return p?.kind === "link" ? p.name : undefined;
};

/** An EntityRef, or undefined when `v` isn't one. */
export function entityRef(v: unknown): EntityRef | undefined {
  if (!isRow(v)) return undefined;
  const type = str(v.type),
    id = str(v.id);
  return type && id ? { type, id } : undefined;
}
const refs = (v: unknown): EntityRef[] | undefined => {
  const out = list(v).map(entityRef);
  return Array.isArray(v) && out.every((r) => r !== undefined)
    ? (out as EntityRef[])
    : undefined;
};

const MALFORMED: ReadFailure = {
  code: "invalid",
  message: "The read failed.",
};

/**
 * A Failure, or `invalid` when it isn't one: a code without the fields that
 * go with it is no answer to trust.
 *
 * @example
 * parseFailure({ code: "too_broad", message: "m", limit: 20000, count: 31000 }); // as given
 * parseFailure({ code: "budget", message: "m" }); // { code: "invalid", message: "The read failed." }
 */
export function parseFailure(v: unknown): ReadFailure {
  if (!isRow(v)) return MALFORMED;
  const message = str(v.message) || "The read failed.",
    limit = count(v.limit),
    n = count(v.count);
  switch (v.code) {
    case "budget":
    case "hub":
      return limit === undefined ? MALFORMED : { code: v.code, message, limit };
    case "too_broad":
      return limit === undefined || n === undefined
        ? MALFORMED
        : { code: "too_broad", message, limit, count: n };
    case "unknown_entity":
      return { code: "unknown_entity", message };
    case "invalid": {
      const pointer = str(v.pointer);
      return { code: "invalid", message, ...(pointer ? { pointer } : {}) };
    }
    default:
      return MALFORMED;
  }
}

/**
 * A read response's envelope, or undefined when it isn't one: each result
 * an `ok` (still raw) or a failure. The results are in the reads' order.
 *
 * @example
 * parseReadResult('{"as_of":"2026-10-19","results":[{"error":{"code":"budget","message":"too deep","limit":64}}],"stats":{"instances":0,"wall_ms":3}}')
 *   ?.results[0]; // { ok: false, error: { code: "budget", message: "too deep", limit: 64 } }
 */
export function parseReadResult(json: string): ReadResult | undefined {
  let body: unknown;
  try {
    body = JSON.parse(json);
  } catch {
    return undefined;
  }
  if (!isRow(body) || !Array.isArray(body.results)) return undefined;
  const stats = isRow(body.stats) ? body.stats : {};
  return {
    asOf: str(body.as_of) || "",
    results: body.results.map(
      (r): Outcome<unknown> =>
        isRow(r) && "ok" in r
          ? { ok: true, value: r.ok }
          : { ok: false, error: parseFailure(isRow(r) ? r.error : undefined) },
    ),
    stats: {
      instances: count(stats.instances) ?? 0,
      wallMs: count(stats.wall_ms) ?? 0,
    },
  };
}

/** An outcome with its `ok` parsed; an `ok` that doesn't parse is `invalid`. */
export function outcomeOf<T>(
  o: Outcome<unknown> | undefined,
  parse: (ok: unknown) => T | undefined,
): Outcome<T> {
  if (!o)
    return {
      ok: false,
      error: { code: "invalid", message: "The read had no result." },
    };
  if (!o.ok) return o;
  const value = parse(o.value);
  return value === undefined
    ? {
        ok: false,
        error: { code: "invalid", message: "The read's result is malformed." },
      }
    : { ok: true, value };
}

/** An entity read's `ok`: its derived facts, flags, live violations and failed parts. */
export function parseEntityResult(ok: unknown): EntityResult | undefined {
  const entity = isKind(ok, "entity") ? entityRef(ok.entity) : undefined;
  if (!isRow(ok) || !entity) return undefined;
  return {
    entity,
    derived: list(ok.derived).flatMap((d): DerivedFact[] => {
      if (!isRow(d)) return [];
      const pred = parsePred(d.pred),
        holder = holderOf(d.holder),
        args = refs(d.args),
        b = basis(d.basis);
      return pred && holder !== undefined && args && args.length >= 2 && b
        ? [{ pred, holder, args, basis: b, rules: strings(d.rules) }]
        : [];
    }),
    flags: list(ok.flags).flatMap((f): Flag[] => {
      const name = isRow(f) ? str(f.name) : undefined;
      return isRow(f) && name ? [{ name, rules: strings(f.rules) }] : [];
    }),
    violations: list(ok.violations).flatMap((v): LiveViolation[] => {
      if (!isRow(v)) return [];
      const constraint = str(v.constraint),
        s = severity(v.severity),
        entities = refs(v.entities);
      return constraint && s && entities && typeof v.stored === "boolean"
        ? [{ constraint, severity: s, entities, stored: v.stored }]
        : [];
    }),
    errors: list(ok.errors).flatMap((e): PartError[] => {
      if (!isRow(e)) return [];
      const part = e.pred === "violations" ? "violations" : parsePred(e.pred),
        failure = parseFailure(e);
      // An entity read's parts fail only by `budget`, `hub` or `invalid`.
      return part &&
        (failure.code === "budget" ||
          failure.code === "hub" ||
          failure.code === "invalid")
        ? [{ part, failure }]
        : [];
    }),
  };
}

/** A pattern read's `ok`: the facts that match, each with its basis and rules. */
export function parsePatternResult(ok: unknown): PatternResult | undefined {
  if (!isKind(ok, "pattern")) return undefined;
  return {
    rows: list(ok.rows).flatMap((r): PatternRow[] => {
      if (!isRow(r)) return [];
      const args = refs(r.args),
        holder = holderOf(r.holder),
        b = r.basis === "stated" ? "stated" : basis(r.basis);
      return args && holder !== undefined && b
        ? [{ args, holder, basis: b, rules: strings(r.rules) }]
        : [];
    }),
  };
}

/**
 * A step's `via`, or undefined when it isn't one.
 *
 * @example
 * parseVia("decl:bills.inverse_of.billed_by"); // { kind: "decl", owner: "bills", decl: "inverse_of.billed_by" }
 * parseVia("constraint:paid_on_time"); // { kind: "constraint", name: "paid_on_time" }
 */
export function parseVia(v: unknown): Via | undefined {
  if (
    v === "stated" ||
    v === "property" ||
    v === "type" ||
    v === "member" ||
    v === "not"
  )
    return { kind: v };
  const s = str(v) || "";
  const named = /^(rule|constraint):(.+)$/.exec(s);
  if (named)
    return { kind: named[1] as "rule" | "constraint", name: named[2]! };
  const m = /^decl:([^.]+)\.(.+)$/.exec(s);
  return m ? { kind: "decl", owner: m[1]!, decl: m[2]! } : undefined;
}

/**
 * An explain read's `ok`: a proof whose steps name their premises by index.
 * A proof with a malformed step, a premise out of range or a cycle is
 * refused whole: half a proof would mislead.
 */
export function parseExplainResult(ok: unknown): ExplainResult | undefined {
  if (!isKind(ok, "explain") || !Array.isArray(ok.steps)) return undefined;
  const steps: ExplainStep[] = [];
  for (const s of ok.steps) {
    if (!isRow(s) || !Array.isArray(s.args)) return undefined;
    const pred = parseStepPred(s.pred),
      via = parseVia(s.via),
      args = s.args.map((a) =>
        a === null && via?.kind === "not" ? null : entityRef(a),
      ),
      premises = list(s.premises).map(count);
    if (
      !pred ||
      !via ||
      args.some((a) => a === undefined) ||
      premises.some((p) => p === undefined)
    )
      return undefined;
    const test = str(s.test);
    steps.push({
      pred,
      args: args as (EntityRef | null)[],
      via,
      premises: premises as number[],
      ...(test ? { test } : {}),
    });
  }
  if (steps.some((s) => s.premises.some((p) => p >= steps.length)))
    return undefined;
  const root = ok.root === null ? null : count(ok.root);
  if (root === undefined || (root !== null && root >= steps.length))
    return undefined;
  // No step rests on itself, however far down.
  const state = new Map<number, "open" | "done">();
  const acyclic = (i: number): boolean => {
    if (state.get(i) === "done") return true;
    if (state.get(i) === "open") return false;
    state.set(i, "open");
    const fine = steps[i]!.premises.every(acyclic);
    state.set(i, "done");
    return fine;
  };
  return steps.every((_, i) => acyclic(i)) ? { steps, root } : undefined;
}

/** A violations read's `ok`: a page of stored records in key order. */
export function parseViolationsResult(
  ok: unknown,
): ViolationsResult | undefined {
  if (!isKind(ok, "violations")) return undefined;
  return {
    records: list(ok.records).flatMap((v): ViolationRecord[] => {
      if (!isRow(v)) return [];
      const key = str(v.violation_key),
        constraint = str(v.constraint),
        s = severity(v.severity),
        entities = refs(v.entities);
      return key && constraint && s && entities
        ? [
            {
              key,
              constraint,
              version: str(v.version) || "",
              severity: s,
              entities,
              openedBy: str(v.opened_by) || "",
            },
          ]
        : [];
    }),
    next: str(ok.next) || null,
  };
}

/** An evidence read's `ok`: the quotes, then the rows, that state a fact or a prop. */
export function parseEvidenceResult(ok: unknown): EvidenceResult | undefined {
  if (!isKind(ok, "evidence")) return undefined;
  return {
    items: list(ok.items).flatMap((e): EvidenceItem[] => {
      if (!isRow(e)) return [];
      const confidence = typeof e.confidence === "number" ? e.confidence : 0;
      if (e.kind === "row") {
        const source = str(e.source),
          rowKey = str(e.row_key);
        return source && rowKey
          ? [
              {
                kind: "row",
                source,
                mapping: str(e.mapping) || "",
                rowKey,
                confidence,
              },
            ]
          : [];
      }
      const documentId = str(e.doc_id),
        section = count(e.section),
        start = count(e.start),
        end = count(e.end),
        value = e.value;
      return e.kind === "quote" &&
        documentId &&
        section !== undefined &&
        start !== undefined &&
        end !== undefined &&
        start <= end
        ? [
            {
              kind: "quote",
              documentId,
              section,
              start,
              end,
              quote: str(e.quote) || "",
              confidence,
              ...(typeof value === "string" ||
              typeof value === "boolean" ||
              (typeof value === "number" && Number.isFinite(value))
                ? { value }
                : {}),
            },
          ]
        : [];
    }),
  };
}
