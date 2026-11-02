import { createHash } from "node:crypto";
import { parse } from "smol-toml";
import {
  FOUNDRY_EDGES as E,
  FOUNDRY_NODES as N,
  isFoundryLabel,
} from "../../src/shared/foundry-labels";
import {
  MAX_READS,
  type EntityRef,
  type Read,
  type Severity,
} from "../../src/shared/foundry-read";
import { contractErrors } from "../agent-contract";
import type { LoadGraph, MockNode } from "./store";
import {
  MAX_SETTLED_ROWS,
  admit,
  nameUnsafe,
  narrows,
  settled,
  settles,
  widened,
  type View,
} from "./view";

// The mock's stand-in for the ontology agent's rules on the read path:
// a load's version's rules, link declarations and
// constraints evaluated over the load's stated facts, bottom-up, stratum by
// stratum, with the first derivation of each fact kept for "why". The
// engine evaluates on demand and within a budget; the mock's graphs are
// small, so it evaluates the whole load per request, and its answers have
// the engine's shapes exactly (contract/ontology_agent/read_result.json, as
// tests/unit/agent-contract.test.ts checks).
//
// The datalog it reads is the engine's without values: `head(x, y) :- a(x,
// z), not b(z, y).`, variables and `_` only, type atoms (`Customer(c)`), one
// or more clauses per rule. A rule or constraint with values (a property
// compared, a date) is the engine's alone: the mock leaves it out. A link or relation is declared `inverse_of`,
// `symmetric`, `transitive` or `implies`; each declaration is a rule of its
// own, named as the engine names it (`decl:in.transitive`,
// `decl:contains.inverse_of.in`, `decl:based_in.implies.located`). A
// constraint's head is `violation(...)`; its rules are `constraint:<name>`.
//
// A link declared `cardinality = "one"` is checked as the engine checks it,
// a constraint of its own (`<link>.cardinality`, block, by the rule
// `decl:<link>.cardinality`): `violation(x, a, b) :- link(x, a), link(x,
// b), a < b.`, one violation per conflicting pair (the mock orders entities
// by type, then key or name).
//
// A rule never derives a link or relation from an entity to itself: such a
// fact is a self-loop, counted as not checked and shown live on the
// entity's page as `<pred>.self_loop` (warn), never stored. A sweep
// (sweepViolations, at the end of each load and extract job) stores one
// FoundryViolation {violation_key, constraint, version, severity, entities,
// opened_by} per violation, -FOUNDRY_CONCERNS {position}-> each entity it
// names, as the engine's sweep does.

type Atom = { pred: string; args: string[] };
type Literal = { neg: boolean; atom: Atom };
// `via` names it as reads do: `rule:<name>`, `constraint:<name>` or
// `decl:<owner>.<what>`.
// `lt`: the first variable's entity orders before the second's.
type Rule = { via: string; head: Atom; body: Literal[]; lt?: [string, string] };
type Constraint = { name: string; severity: Severity; rules: Rule[] };

export type Program = {
  types: Set<string>;
  // Each type's props and names props, and whether a text search may start
  // from it (it has `names`, or is known by its name).
  props: Map<string, Set<string>>;
  names: Map<string, string[]>;
  searchable: Set<string>;
  // Pred strings ("link:x", "relation:x") of what may never self-loop.
  binary: Set<string>;
  rules: Rule[];
  constraints: Constraint[];
};

type Fact = {
  pred: string;
  args: number[];
  stated: boolean;
  // The rules that derive it, by their `via`.
  rules: Set<string>;
  // Its first derivation: the rule, the facts it rests on, and what didn't
  // hold (an arg the rule wrote `_` is null).
  derivation?: {
    via: string;
    premises: string[];
    nots: { pred: string; args: (number | null)[] }[];
  };
};

export type Model = {
  g: LoadGraph;
  program: Program;
  facts: Map<string, Fact>;
  // Self-loops a rule would make, once per pred and entity.
  selfLoops: { pred: string; node: number }[];
  violations: {
    constraint: string;
    severity: Severity;
    args: number[];
    key: string;
  }[];
};

const VAR = /^(?:_|[a-z][A-Za-z0-9_]*)$/;
const ATOM = /^(not\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(([^()]*)\)$/;
const factKey = (pred: string, args: number[]) =>
  `${pred}\u001f${args.join("\u001f")}`;

type Table = Record<string, unknown>;
const tables = (v: unknown): [string, Table][] =>
  v && typeof v === "object"
    ? Object.entries(v as Table).filter(
        (e): e is [string, Table] => !!e[1] && typeof e[1] === "object",
      )
    : [];
const texts = (v: unknown) =>
  (Array.isArray(v) ? v : typeof v === "string" ? [v] : []).filter(
    (x): x is string => typeof x === "string",
  );

/** Splits a body at its top-level commas: atoms keep theirs. */
function literals(body: string): string[] {
  const out: string[] = [];
  let depth = 0,
    from = 0;
  for (let i = 0; i < body.length; i++) {
    if (body[i] === "(") depth++;
    else if (body[i] === ")") depth--;
    else if (body[i] === "," && depth === 0) {
      out.push(body.slice(from, i));
      from = i + 1;
    }
  }
  out.push(body.slice(from));
  return out.map((s) => s.trim()).filter(Boolean);
}

/**
 * The version's rules as a program: each name resolved to a type atom, a
 * link, a relation or a derived predicate. Throws on datalog the mock can't
 * read, so a fixture it can't evaluate fails loudly.
 */
export function compile(ontologyToml: string): Program {
  const doc = parse(ontologyToml) as Table;
  const types = new Set(tables(doc.types).map(([n]) => n));
  const links = tables(doc.links),
    relations = tables(doc.relations);
  const kinds = new Map<string, string>([
    ...links.map(([n]) => [n, "link"] as const),
    ...relations.map(([n]) => [n, "relation"] as const),
  ]);
  const pred = (name: string, arity: number, head?: string) => {
    if (head) return head;
    if (types.has(name) && arity === 1) return `type:${name}`;
    return `${kinds.get(name) || "derived"}:${name}`;
  };
  const clauses = (via: string, datalog: string, head?: string): Rule[] =>
    datalog
      .split(/\.\s*(?=[A-Za-z_]|$)/)
      .map((c) => c.trim())
      .filter(Boolean)
      .map((clause) => {
        const [h, b = ""] = clause.split(":-");
        const atom = (s: string, asHead = false): Literal => {
          const m = ATOM.exec(s.trim());
          const args = (m?.[3] || "").split(",").map((a) => a.trim());
          if (!m || !args.every((a) => VAR.test(a)))
            throw new Error(`the mock can't read \`${s.trim()}\` in ${via}`);
          return {
            neg: !!m[1],
            atom: {
              pred: pred(m[2]!, args.length, asHead ? head : undefined),
              args,
            },
          };
        };
        return {
          via,
          head: atom(h!, true).atom,
          body: literals(b).map((l) => atom(l)),
        };
      });
  const declared = [...links, ...relations].flatMap(([name, t]) => {
    const p = `${kinds.get(name)}:${name}`,
      of = (owner: string, kind: string) => ({ via: `decl:${owner}.${kind}` });
    const pos = (q: string, ...args: string[]): Literal => ({
      neg: false,
      atom: { pred: q, args },
    });
    const rules: Rule[] = [];
    for (const inverse of texts(t.inverse_of)) {
      const q = `${kinds.get(inverse) || "link"}:${inverse}`;
      // Each side derives the other, a rule owned by its body's link.
      rules.push(
        {
          ...of(inverse, `inverse_of.${name}`),
          head: { pred: p, args: ["a", "b"] },
          body: [pos(q, "b", "a")],
        },
        {
          ...of(name, `inverse_of.${inverse}`),
          head: { pred: q, args: ["a", "b"] },
          body: [pos(p, "b", "a")],
        },
      );
    }
    if (t.symmetric === true)
      rules.push({
        ...of(name, "symmetric"),
        head: { pred: p, args: ["a", "b"] },
        body: [pos(p, "b", "a")],
      });
    if (t.transitive === true)
      rules.push({
        ...of(name, "transitive"),
        head: { pred: p, args: ["a", "c"] },
        body: [pos(p, "a", "b"), pos(p, "b", "c")],
      });
    for (const implied of texts(t.implies))
      rules.push({
        ...of(name, `implies.${implied}`),
        head: {
          pred: `${kinds.get(implied) || "link"}:${implied}`,
          args: ["a", "b"],
        },
        body: [pos(p, "a", "b")],
      });
    return rules;
  });
  const cardinality = links
    .filter(([, t]) => t.cardinality === "one")
    .map(([name]): Constraint => {
      const link = (...args: string[]): Literal => ({
        neg: false,
        atom: { pred: `link:${name}`, args },
      });
      return {
        name: `${name}.cardinality`,
        severity: "block",
        rules: [
          {
            via: `decl:${name}.cardinality`,
            head: {
              pred: `violation:${name}.cardinality`,
              args: ["x", "a", "b"],
            },
            body: [link("x", "a"), link("x", "b")],
            lt: ["a", "b"],
          },
        ],
      };
    });
  return {
    types,
    props: new Map(
      tables(doc.types).map(([n, t]) => [
        n,
        new Set(
          Object.keys(t.props && typeof t.props === "object" ? t.props : {}),
        ),
      ]),
    ),
    names: new Map(tables(doc.types).map(([n, t]) => [n, texts(t.names)])),
    searchable: new Set(
      tables(doc.types)
        .filter(([, t]) => texts(t.names).length > 0 || !texts(t.key).length)
        .map(([n]) => n),
    ),
    binary: new Set([...kinds].map(([n, k]) => `${k}:${n}`)),
    rules: [
      ...declared,
      ...tables(doc.rules).flatMap(([name, r]) =>
        readable(() => clauses(`rule:${name}`, String(r.datalog || ""))),
      ),
    ],
    constraints: [
      ...tables(doc.constraints).flatMap(([name, c]): Constraint[] => {
        const rules = readable(() =>
          clauses(
            `constraint:${name}`,
            String(c.datalog || ""),
            `violation:${name}`,
          ),
        );
        return rules.length
          ? [
              {
                name,
                severity:
                  c.severity === "block" || c.severity === "info"
                    ? c.severity
                    : "warn",
                rules,
              },
            ]
          : [];
      }),
      ...cardinality,
    ],
  };
}

/** `read`'s rules, or none when their datalog has values the mock can't read. */
function readable(read: () => Rule[]): Rule[] {
  try {
    return read();
  } catch {
    return [];
  }
}

/** Each derived pred's stratum: after what it reads, past what it negates. */
function strata(rules: Rule[]): Rule[][] {
  const level = new Map<string, number>();
  const at = (p: string) => level.get(p) || 0;
  for (let changed = true, rounds = 0; changed; rounds++) {
    if (rounds > rules.length + 1)
      throw new Error("the mock's rules negate through a cycle");
    changed = false;
    for (const r of rules) {
      const need = Math.max(
        0,
        ...r.body.map((l) => at(l.atom.pred) + (l.neg ? 1 : 0)),
      );
      if (need > at(r.head.pred)) {
        level.set(r.head.pred, need);
        changed = true;
      }
    }
  }
  const out: Rule[][] = [];
  for (const r of rules) (out[at(r.head.pred)] ||= []).push(r);
  return out.filter(Boolean);
}

const isEntity = (n: MockNode, program: Program) =>
  program.types.has(n.label) &&
  (typeof n.props.entity_key === "string" ||
    typeof n.props.norm_name === "string");

/** Evaluates the program over one load's stated facts. */
export function evaluate(g: LoadGraph, program: Program): Model {
  const facts = new Map<string, Fact>();
  const byPred = new Map<string, Fact[]>();
  const add = (f: Fact) => {
    facts.set(factKey(f.pred, f.args), f);
    byPred.set(f.pred, [...(byPred.get(f.pred) || []), f]);
  };
  for (const n of g.nodes())
    if (isEntity(n, program))
      add({
        pred: `type:${n.label}`,
        args: [n.id],
        stated: true,
        rules: new Set(),
      });
  for (const e of g.graph.edges.values()) {
    if (e.props.foundry_load !== g.load || isFoundryLabel("edge", e.label))
      continue;
    const p = `link:${e.label}`;
    if (!program.binary.has(p) || facts.has(factKey(p, [e.from, e.to])))
      continue;
    add({ pred: p, args: [e.from, e.to], stated: true, rules: new Set() });
  }
  const selfLoops = new Map<string, { pred: string; node: number }>();

  // Every binding of a rule's body, with the facts it used.
  const bindings = (r: Rule) => {
    type B = { vars: Map<string, number>; used: string[] };
    let out: B[] = [{ vars: new Map(), used: [] }];
    for (const { neg, atom } of r.body) {
      const next: B[] = [];
      for (const b of out) {
        const matches = (byPred.get(atom.pred) || []).filter((f) =>
          atom.args.every((a, i) => {
            if (a === "_") return true;
            const bound = b.vars.get(a);
            return bound === undefined ? !neg : bound === f.args[i];
          }),
        );
        if (neg) {
          if (!matches.length) next.push(b);
          continue;
        }
        for (const f of matches) {
          const vars = new Map(b.vars);
          let fits = true;
          atom.args.forEach((a, i) => {
            if (a === "_") return;
            const bound = vars.get(a);
            if (bound === undefined) vars.set(a, f.args[i]!);
            else if (bound !== f.args[i]) fits = false;
          });
          if (fits)
            next.push({ vars, used: [...b.used, factKey(f.pred, f.args)] });
        }
      }
      out = next;
    }
    // The engine's order of entities: by type, then key or name.
    const order = (a: number) => {
      const n = g.node(a)!;
      return `${n.label}\u001f${canonical(n)}`;
    };
    const lt = r.lt;
    return out
      .filter(
        (b) => !lt || order(b.vars.get(lt[0])!) < order(b.vars.get(lt[1])!),
      )
      .map((b) => ({
        ...b,
        nots: r.body
          .filter((l) => l.neg)
          .map((l) => ({
            pred: l.atom.pred,
            args: l.atom.args.map((a) => (a === "_" ? null : b.vars.get(a)!)),
          })),
      }));
  };

  const fire = (r: Rule) => {
    let added = false;
    for (const b of bindings(r)) {
      const args = r.head.args.map((a) => b.vars.get(a));
      if (args.some((a) => a === undefined))
        throw new Error(`${r.via}: its head has an unbound variable`);
      const at = args as number[];
      if (
        program.binary.has(r.head.pred) &&
        at.length === 2 &&
        at[0] === at[1]
      ) {
        selfLoops.set(`${r.head.pred}\u001f${at[0]}`, {
          pred: r.head.pred,
          node: at[0]!,
        });
        continue;
      }
      const key = factKey(r.head.pred, at);
      const derivation = { via: r.via, premises: b.used, nots: b.nots };
      const f = facts.get(key);
      if (f) {
        f.rules.add(r.via);
        if (!f.derivation) {
          f.derivation = derivation;
          added = true;
        }
        continue;
      }
      add({
        pred: r.head.pred,
        args: at,
        stated: false,
        rules: new Set([r.via]),
        derivation,
      });
      added = true;
    }
    return added;
  };

  for (const stratum of strata(program.rules))
    for (let more = true; more; ) more = stratum.map(fire).some(Boolean);
  const model: Model = {
    g,
    program,
    facts,
    selfLoops: [...selfLoops.values()],
    violations: [],
  };
  for (const c of program.constraints) {
    c.rules.forEach(fire);
    for (const f of byPred.get(`violation:${c.name}`) || [])
      model.violations.push({
        constraint: c.name,
        severity: c.severity,
        args: f.args,
        key: violationKey(model, c.name, f.args),
      });
  }
  return model;
}

/** The engine's canonical form of an entity: \u0002 and its key, or its name. */
const canonical = (n: MockNode) =>
  typeof n.props.entity_key === "string"
    ? `\u0002${n.props.entity_key}`
    : String(n.props.norm_name);
const entities = (m: Model, args: number[]) =>
  args.map((a) => {
    const n = m.g.node(a)!;
    return `${n.label}\u001f${canonical(n)}`;
  });
const violationKey = (m: Model, constraint: string, args: number[]) =>
  createHash("sha256")
    .update([constraint, ...entities(m, args)].join("\u001e"))
    .digest("hex");

/** An entity node as the engine names it. */
export function refOf(n: MockNode): EntityRef {
  return {
    type: n.label,
    id: String(n.props.entity_key ?? n.props.norm_name),
  };
}

/** The load's entity an EntityRef names, if it holds one. */
function nodeOf(m: Model, ref: unknown): MockNode | undefined {
  const r = ref as Partial<EntityRef> | null;
  if (!r || typeof r.type !== "string" || typeof r.id !== "string")
    return undefined;
  const n =
    m.g.find(r.type, "entity_key", r.id) || m.g.find(r.type, "norm_name", r.id);
  return n && isEntity(n, m.program) ? n : undefined;
}

/**
 * Brings the load's violation records up to date with the model, as the
 * engine's sweep does: every record is written again, so a closed
 * violation's record goes.
 */
export function sweepViolations(m: Model, version: string): void {
  for (const n of [...m.g.nodes()].filter((x) => x.label === N.Violation))
    m.g.graph.removeNode(n.id);
  for (const v of m.violations) {
    const id = m.g.addNode(N.Violation, {
      violation_key: v.key,
      constraint: v.constraint,
      version,
      severity: v.severity,
      entities: entities(m, v.args),
      opened_by: "sweep",
    });
    v.args.forEach((a, position) =>
      m.g.addEdge(E.CONCERNS, id, a, { position }),
    );
  }
}

/** The load's stored violation records, in key order. */
function records(m: Model) {
  return [...m.g.nodes()]
    .filter((n) => n.label === N.Violation)
    .map((n) => ({
      key: String(n.props.violation_key),
      constraint: String(n.props.constraint),
      version: String(n.props.version),
      severity: n.props.severity as Severity,
      openedBy: String(n.props.opened_by),
      args: (m.g.graph.outgoing.get(n.id) || [])
        .map((e) => m.g.graph.edges.get(e)!)
        .filter((e) => e.label === E.CONCERNS)
        .sort((a, b) => Number(a.props.position) - Number(b.props.position))
        .map((e) => e.to),
    }))
    .sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
}

const basisOf = (f: Fact) =>
  f.stated ? (f.derivation ? "both" : "stated") : "derived";
const refs = (m: Model, args: number[]) => args.map((a) => refOf(m.g.node(a)!));
// A link's facts are its own; a relation's or a rule's only rules give
// (the mock has no relation members).
const holderOf = (pred: string) => (pred.startsWith("link:") ? pred : null);
const rulesOf = (f: Fact) => [...f.rules].sort();
const byPredThenArgs = (m: Model) => (a: Fact, b: Fact) =>
  a.pred.localeCompare(b.pred) ||
  refs(m, a.args)
    .map((r) => r.id)
    .join("\u0000")
    .localeCompare(
      refs(m, b.args)
        .map((r) => r.id)
        .join("\u0000"),
    );

/**
 * A proof of a fact, as an explain read answers it: premises first, each
 * step once. A stated fact is its own proof, derived as well or not; a type
 * atom is a `type` step; a negated atom that held is a `not` step.
 */
function explain(m: Model, root: Fact | undefined) {
  const steps: {
    pred: string;
    args: (EntityRef | null)[];
    via: string;
    premises: number[];
  }[] = [];
  if (!root) return { kind: "explain", steps, root: null };
  const index = new Map<string, number>();
  const push = (s: (typeof steps)[number]) => steps.push(s) - 1;
  const step = (f: Fact): number => {
    const key = factKey(f.pred, f.args);
    const seen = index.get(key);
    if (seen !== undefined) return seen;
    const d = f.stated ? undefined : f.derivation;
    const premises = d
      ? [
          ...d.premises.map((p) => step(m.facts.get(p)!)),
          ...d.nots.map((n) =>
            push({
              pred: n.pred,
              args: n.args.map((a) =>
                a === null ? null : refOf(m.g.node(a)!),
              ),
              via: "not",
              premises: [],
            }),
          ),
        ]
      : [];
    const at = push({
      pred: f.pred,
      args: refs(m, f.args),
      via: d ? d.via : f.pred.startsWith("type:") ? "type" : "stated",
      premises,
    });
    index.set(key, at);
    return at;
  };
  return { kind: "explain", steps, root: step(root) };
}

/** A failure as the engine words it, with the fields its code carries. */
type Failure =
  | { code: "budget" | "hub"; message: string; limit: number }
  | { code: "unknown_entity"; message: string }
  | { code: "not_visible" | "invalid"; message: string; pointer?: string };
type Answer = { ok: Record<string, unknown> } | { error: Failure };
const invalid = (message: string): Answer => ({
  error: { code: "invalid", message },
});
/** A part or read told to fail with `code`, as the engine says it. */
const told = (code: "budget" | "hub" | "invalid"): Failure =>
  code === "invalid"
    ? { code, message: "the program can't evaluate it" }
    : code === "budget"
      ? {
          code,
          message: "the read needs more Rounds than its budget of 64",
          limit: 64,
        }
      : {
          code,
          message: "an adjacency list has more than 10000 facts",
          limit: 10000,
        };

/** The program's constraint a violations read names, as reads name it. */
const constraintOf = (m: Model, name: string) =>
  m.program.constraints.find((c) => c.name === name);

/** The evidence nodes about an entity. */
const aboutOf = (m: Model, id: number) =>
  [...m.g.nodes()].filter(
    (x) =>
      x.label === N.Evidence &&
      (m.g.graph.outgoing.get(x.id) || []).some((k) => {
        const edge = m.g.graph.edges.get(k)!;
        return edge.label === E.ABOUT && edge.to === id;
      }),
  );

/**
 * Whether an entity is one the reader may see: its type is full, or a row the
 * reader holds states it, or a document it holds names it. A keyed entity some
 * other row states isn't shown by a document: its id is its key, which is that
 * row's to say.
 */
function entityVisible(m: Model, view: View, n: MockNode) {
  const reach = view.types.get(n.label);
  if (reach === "full") return true;
  if (!reach) return false;
  const sources = aboutOf(m, n.id).flatMap((x) =>
    typeof x.props.source === "string" ? [String(x.props.source)] : [],
  );
  if (sources.some(view.holdsSource)) return true;
  if (view.keyed.has(n.label) && sources.length) return false;
  return [...m.g.nodes()].some(
    (x) =>
      x.label === N.Mention &&
      view.holdsDocument(String(x.props.doc_id)) &&
      (m.g.graph.outgoing.get(x.id) || []).some((k) => {
        const edge = m.g.graph.edges.get(k)!;
        return edge.label === E.MENTION_OF && edge.to === n.id;
      }),
  );
}

/** Whether a fact is one the reader may see: a row or quote it holds states it. */
function factVisible(m: Model, view: View, link: string, s: number, o: number) {
  const reach = view.links.get(link);
  if (reach === "full") return true;
  if (!reach) return false;
  const edge = (m.g.graph.outgoing.get(s) || [])
    .map((id) => m.g.graph.edges.get(id)!)
    .find((x) => x.label === link && x.to === o);
  return (
    !!edge &&
    [...m.g.nodes()].some(
      (x) =>
        x.label === N.Evidence &&
        x.props.fact_key === edge.props.fact_key &&
        (typeof x.props.source === "string"
          ? view.holdsSource(String(x.props.source))
          : view.holdsDocument(String(x.props.doc_id))),
    )
  );
}

/** A property of an entity as the reader sees it. */
function settle(m: Model, view: View, n: MockNode, prop: string): unknown {
  const about = aboutOf(m, n.id);
  const s = settled(
    view,
    about
      .filter(
        (x) =>
          typeof x.props.source === "string" &&
          Array.isArray(x.props.props) &&
          x.props.props.includes(prop),
      )
      .map((x) => String(x.props.source)),
    about
      .filter((x) => typeof x.props.quote === "string" && x.props.prop === prop)
      .map((x) => ({
        documentId: String(x.props.doc_id),
        value: x.props.value,
      })),
  );
  return s.from === "stored"
    ? (n.props[prop] ?? null)
    : s.from === "quoted"
      ? s.value
      : null;
}

/**
 * One read's answer over the model, in the engine's shapes. `errors` makes
 * a part of entity reads fail with that code (the mock's knob for a pred too
 * deep to compute): a pred string, or `violations`. `view` is what a reader
 * who holds only some of the load's sources may see (./view.ts): the model
 * is then of its projected ontology.
 */
export function answer(
  m: Model,
  read: Read,
  opts: {
    errors?: Record<string, "budget" | "hub" | "invalid">;
    view?: View;
  } = {},
): Answer {
  const errors = opts.errors || {};
  const view = opts.view?.whole ? undefined : opts.view;
  if ("view" in read)
    return {
      ok: {
        kind: "view",
        ontology_toml: view?.ontologyToml ?? "",
        mappings_toml: view?.mappingsToml ?? "",
        partial: view?.partial ?? [],
      },
    };
  // Every entity a read names must be one the load holds.
  const named: unknown[] =
    "entity" in read
      ? [read.entity]
      : "pattern" in read
        ? read.pattern.args.filter(Boolean)
        : "explain" in read
          ? read.explain.args
          : "violations" in read
            ? [read.violations.entity].filter(Boolean)
            : "evidence" in read
              ? "fact" in read.evidence
                ? [read.evidence.fact.subject, read.evidence.fact.object]
                : [read.evidence.entity]
              : [];
  for (const r of named) {
    const ref = r as EntityRef;
    if (!m.program.types.has(ref.type))
      return invalid(`\`${ref.type}\` is not a type`);
    const held = nodeOf(m, ref);
    // One the reader may not see is one the load doesn't hold, for it.
    if (!held || (view && !entityVisible(m, view, held)))
      return {
        error: {
          code: "unknown_entity",
          message: `the load holds no ${ref.type} ${ref.id}`,
        },
      };
  }
  if ("entity" in read) {
    const n = nodeOf(m, read.entity)!;
    const mine = [...m.facts.values()]
      .filter(
        (f) =>
          f.derivation &&
          !f.pred.startsWith("type:") &&
          !f.pred.startsWith("violation:") &&
          !(f.pred in errors) &&
          f.args.includes(n.id),
      )
      .sort(byPredThenArgs(m));
    const stored = new Set(records(m).map((r) => r.key));
    return {
      ok: {
        kind: "entity",
        entity: refOf(n),
        derived: mine
          .filter((f) => f.args.length >= 2)
          .map((f) => ({
            pred: f.pred,
            holder: holderOf(f.pred),
            args: refs(m, f.args),
            basis: basisOf(f),
            rules: rulesOf(f),
          })),
        flags: mine
          .filter((f) => f.args.length === 1)
          .map((f) => ({
            name: f.pred.slice(f.pred.indexOf(":") + 1),
            rules: rulesOf(f),
          })),
        // The violations naming it first, self-loops among them.
        violations:
          "violations" in errors
            ? []
            : [
                ...m.violations
                  .filter((v) => v.args[0] === n.id)
                  .map((v) => ({
                    constraint: v.constraint,
                    severity: v.severity,
                    entities: refs(m, v.args),
                    stored: stored.has(v.key),
                  })),
                ...m.selfLoops
                  .filter((s) => s.node === n.id)
                  .map((s) => ({
                    constraint: `${s.pred.slice(s.pred.indexOf(":") + 1)}.self_loop`,
                    severity: "warn",
                    entities: [refOf(n)],
                    stored: false,
                  })),
              ],
        errors: Object.entries(errors).map(([pred, code]) => ({
          pred,
          ...told(code),
        })),
      },
    };
  }
  if ("pattern" in read) {
    const { pred, args } = read.pattern;
    if (!args.some(Boolean)) return invalid("a pattern needs a bound argument");
    const want = args.map((a) => (a ? nodeOf(m, a)!.id : null));
    return {
      ok: {
        kind: "pattern",
        rows: [...m.facts.values()]
          .filter(
            (f) =>
              f.pred === pred &&
              f.args.length === want.length &&
              want.every((w, i) => w === null || w === f.args[i]) &&
              (!view ||
                !pred.startsWith("link:") ||
                factVisible(
                  m,
                  view,
                  pred.slice("link:".length),
                  f.args[0]!,
                  f.args[1]!,
                )),
          )
          .sort(byPredThenArgs(m))
          .map((f) => ({
            args: refs(m, f.args),
            holder: holderOf(f.pred),
            basis: basisOf(f),
            rules: rulesOf(f),
          })),
      },
    };
  }
  if ("explain" in read) {
    if (
      view &&
      read.explain.pred.startsWith("link:") &&
      view.links.get(read.explain.pred.slice("link:".length)) !== "full"
    )
      return {
        error: {
          code: "not_visible",
          message: `\`${read.explain.pred}\` is only partly in the sources you can read`,
        },
      };
    const ids = read.explain.args.map((a) => nodeOf(m, a)!.id);
    return {
      ok: explain(m, m.facts.get(factKey(read.explain.pred, ids))),
    };
  }
  if ("violations" in read) {
    const { constraint, entity, after, limit = 100 } = read.violations;
    if (limit < 1 || limit > 1000)
      return invalid("a page holds 1 to 1,000 records");
    if (constraint && !constraintOf(m, constraint))
      return invalid(`no constraint \`${constraint}\` has stored records`);
    const n = entity ? nodeOf(m, entity) : undefined;
    const page = records(m)
      .filter(
        (r) =>
          (!constraint || r.constraint === constraint) &&
          (!n || r.args.includes(n.id)) &&
          (!after || r.key > after) &&
          (!view || !!constraintOf(m, r.constraint)),
      )
      .slice(0, limit);
    return {
      ok: {
        kind: "violations",
        records: page.map((r) => ({
          violation_key: r.key,
          constraint: r.constraint,
          version: r.version,
          severity: r.severity,
          entities: refs(m, r.args),
          opened_by: r.openedBy,
        })),
        // The engine gives `next` for a full page.
        next: page.length === limit ? page.at(-1)!.key : null,
      },
    };
  }
  if ("evidence" in read) {
    const e = read.evidence;
    const limit = e.limit ?? 10;
    if (limit < 1 || limit > 100)
      return invalid("evidence comes 1 to 100 pieces at a time");
    const evidence = [...m.g.nodes()].filter((x) => x.label === N.Evidence);
    const quote = (x: MockNode) => {
      const section = (m.g.graph.outgoing.get(x.id) || [])
        .map((id) => m.g.graph.edges.get(id)!)
        .find((y) => y.label === E.FROM_SECTION);
      return {
        kind: "quote",
        doc_id: String(x.props.doc_id),
        section: Number((section && m.g.node(section.to)?.props.ordinal) ?? 0),
        start: Number(x.props.start),
        end: Number(x.props.end),
        quote: String(x.props.quote),
        confidence: Number(x.props.confidence ?? 1),
      };
    };
    const row = (x: MockNode) => ({
      kind: "row",
      source: String(x.props.source),
      mapping: String(x.props.mapping),
      row_key: String(x.props.row_key),
      confidence: Number(x.props.confidence ?? 1),
    });
    if (!("fact" in e)) {
      // The rows that state the entity; with `prop`, the rows that set it,
      // then each document quote stating a value of it, with the value.
      const id = nodeOf(m, e.entity)!.id;
      const about = (x: MockNode) =>
        (m.g.graph.outgoing.get(x.id) || []).some((k) => {
          const edge = m.g.graph.edges.get(k)!;
          return edge.label === E.ABOUT && edge.to === id;
        });
      const prop = e.prop ?? null;
      const mine = evidence.filter(about);
      const rows = mine
        .filter(
          (x) =>
            typeof x.props.source === "string" &&
            (prop === null ||
              (Array.isArray(x.props.props) && x.props.props.includes(prop))),
        )
        .map(row);
      const quotes =
        prop === null
          ? []
          : mine
              .filter(
                (x) =>
                  typeof x.props.quote === "string" && x.props.prop === prop,
              )
              .map((x) => ({ ...quote(x), value: x.props.value }));
      return {
        ok: {
          kind: "evidence",
          items: [...rows, ...quotes]
            .filter((i) => !view || heldEvidence(view, i))
            .slice(0, limit),
        },
      };
    }
    const s = nodeOf(m, e.fact.subject)!,
      o = nodeOf(m, e.fact.object)!;
    const edge = [...(m.g.graph.outgoing.get(s.id) || [])]
      .map((id) => m.g.graph.edges.get(id)!)
      .find((x) => x.label === e.fact.link && x.to === o.id);
    const of = evidence.filter(
      (x) => !!edge && x.props.fact_key === edge.props.fact_key,
    );
    const quotes = of
      .filter((x) => typeof x.props.quote === "string")
      .map(quote);
    const rows = of.filter((x) => typeof x.props.source === "string").map(row);
    return {
      ok: {
        kind: "evidence",
        items: [...quotes, ...rows]
          .filter((i) => !view || heldEvidence(view, i))
          .slice(0, limit),
      },
    };
  }
  return typedQuery(m, read.query, view);
}

/** Whether the reader holds what a piece of evidence comes from. */
const heldEvidence = (
  view: View,
  i: { kind: string; source?: string; doc_id?: string },
) =>
  i.kind === "row"
    ? view.holdsSource(String(i.source))
    : view.holdsDocument(String(i.doc_id));

type Query = Extract<Read, { query: unknown }>["query"];
type Where = NonNullable<Query["start"]["where"]>;

/** Whether a node passes every condition, as the engine tests them. */
function passes(n: MockNode, where: Where): boolean {
  return where.every((c) => {
    const v = n.props[c.prop] ?? null;
    if (c.op === "is_null") return v === null;
    if (c.op === "not_null") return v !== null;
    // A comparison never holds for an absent value, `!=` included.
    if (v === null) return false;
    const want = c.value as string | number | boolean;
    switch (c.op) {
      case "=":
        return v === want;
      case "!=":
        return v !== want;
      case "<":
        return (v as number) < (want as number);
      case "<=":
        return (v as number) <= (want as number);
      case ">":
        return (v as number) > (want as number);
      case ">=":
        return (v as number) >= (want as number);
      case "contains":
        return typeof v === "string" && v.includes(String(want));
      case "starts_with":
        return typeof v === "string" && v.startsWith(String(want));
    }
  });
}

/** Words of a name or a search, folded. */
const words = (s: string) =>
  s
    .toLowerCase()
    .split(/[^\p{L}\p{N}]+/u)
    .filter(Boolean);

/**
 * A typed query's answer over the model, as the engine gives it: the start
 * type's entities (by ids, the best 100 of a text search of their names, or
 * all), each step along a link, relation or two-place derived predicate
 * from or to each, the filters, then rows or a count. The mock answers rows
 * and counts; a prop the type doesn't have, or a limit out of range, is
 * `invalid` with a pointer to it.
 */
function typedQuery(m: Model, q: Query, view?: View): Answer {
  const steps = q.steps || [],
    filters = q.filters || [];
  const bad = (pointer: string, message: string): Answer => ({
    error: { code: "invalid", message, pointer },
  });
  const types = [q.start.type, ...steps.map((s) => s.to)];
  for (const [i, t] of types.entries())
    if (!m.program.types.has(t))
      return bad(
        i ? `/steps/${i - 1}/to` : "/start/type",
        `\`${t}\` is not a type`,
      );
  const known = (at: number, prop: string) =>
    prop === "name" || !!m.program.props.get(types[at]!)?.has(prop);
  const conditions: [string, number, Where][] = [
    ["/start/where", 0, q.start.where || []],
    ...steps.map((s, i): [string, number, Where] => [
      `/steps/${i}/where`,
      i + 1,
      s.where || [],
    ]),
  ];
  for (const [pointer, at, where] of conditions)
    for (const [k, c] of where.entries())
      if (!known(at, c.prop))
        return bad(`${pointer}/${k}/prop`, `\`${c.prop}\` is no property`);
  const preds = steps.map((s) =>
    m.program.binary.has(`link:${s.via}`)
      ? `link:${s.via}`
      : m.program.binary.has(`relation:${s.via}`)
        ? `relation:${s.via}`
        : `derived:${s.via}`,
  );
  const { ids = null, text = null } = q.start;
  if (ids && text) return bad("/start", "a start takes ids or text, not both");
  if (
    text !== null &&
    (!words(text).length || !m.program.searchable.has(q.start.type))
  )
    return bad(
      "/start/text",
      "a text search needs a word and a type with names",
    );
  const ret = q.return;
  if (!("rows" in ret) && !("count" in ret))
    return invalid("the mock answers rows and counts only");
  const last = steps.length;
  if ("rows" in ret) {
    const { columns, order_by = null, limit = 50, offset = 0 } = ret.rows;
    if (limit < 1 || limit > 200)
      return bad("/return/rows/limit", "rows come 1 to 200 at a time");
    if (offset > 10_000)
      return bad("/return/rows/offset", "offset must be at most 10000");
    for (const [k, c] of columns.entries())
      if (c.at > last || (c.prop != null && !known(c.at, c.prop)))
        return bad(`/return/rows/columns/${k}`, "no such column");
    if (order_by && (order_by.at > last || !known(order_by.at, order_by.prop)))
      return bad("/return/rows/order_by", "no such order");
  }
  if (view) {
    const refused = admit(view, q);
    if (refused) return { error: refused };
  }
  // Position 0.
  let start = [...m.g.nodes()].filter(
    (n) =>
      n.label === q.start.type &&
      isEntity(n, m.program) &&
      (!ids || ids.includes(refOf(n).id)) &&
      passes(n, q.start.where || []),
  );
  if (text !== null) {
    const want = new Set(words(text));
    // Query words its name or names props hold.
    const score = (n: MockNode) => {
      const have = new Set(
        [
          n.props.name,
          ...(m.program.names.get(n.label) || []).map((p) => n.props[p]),
        ].flatMap((v) => (typeof v === "string" ? words(v) : [])),
      );
      return [...want].filter((w) => have.has(w)).length;
    };
    start = start
      .map((n) => [n, score(n)] as const)
      .filter(([, s]) => s > 0)
      .sort(
        ([a, x], [b, y]) => y - x || canonical(a).localeCompare(canonical(b)),
      )
      .slice(0, 100)
      .map(([n]) => n);
  }
  // Every path: a node per position, and the fact each step went along.
  type Path = { nodes: number[]; facts: Fact[] };
  let paths: Path[] = start.map((n) => ({ nodes: [n.id], facts: [] }));
  steps.forEach((s, i) => {
    const out = s.dir === "out";
    const along = [...m.facts.values()].filter(
      (f) => f.pred === preds[i] && f.args.length === 2,
    );
    paths = paths.flatMap((p) => {
      const at = p.nodes[p.nodes.length - 1]!;
      return along.flatMap((f) => {
        const [from, to] = out ? f.args : [f.args[1]!, f.args[0]!];
        const n = from === at ? m.g.node(to!) : undefined;
        return n && n.label === s.to && passes(n, s.where || [])
          ? [{ nodes: [...p.nodes, n.id], facts: [...p.facts, f] }]
          : [];
      });
    });
  });
  const stored = new Set(
    records(m).flatMap((r) => r.args.map((a) => `${r.constraint}\u001f${a}`)),
  );
  paths = paths.filter((p) =>
    filters.every((f) => {
      const n = p.nodes[f.at];
      if (n === undefined) return false;
      if ("holds" in f) return m.facts.has(factKey(`derived:${f.holds}`, [n]));
      if ("not_holds" in f)
        return !m.facts.has(factKey(`derived:${f.not_holds}`, [n]));
      return stored.has(`${f.violates}\u001f${n}`);
    }),
  );
  const echo = {
    start: {
      type: q.start.type,
      where: (q.start.where || []).map((c) => ({ value: null, ...c })),
      ids,
      text,
    },
    steps: steps.map((s) => ({
      ...s,
      where: (s.where || []).map((c) => ({ value: null, ...c })),
    })),
    filters,
    return:
      "rows" in ret
        ? {
            rows: {
              columns: ret.rows.columns.map((c) => ({
                at: c.at,
                prop: c.prop ?? null,
              })),
              order_by: ret.rows.order_by
                ? { desc: false, ...ret.rows.order_by }
                : null,
              limit: ret.rows.limit ?? 50,
              ...(ret.rows.offset ? { offset: ret.rows.offset } : {}),
            },
          }
        : { count: { distinct_at: ret.count.distinct_at ?? null } },
  };
  // What a narrowed read matched would say how much it can't see.
  const narrowed = !!view && narrows(view, q);
  const stats = {
    matched: narrowed ? 0 : start.length,
    patterns: narrowed ? 0 : steps.length,
    terminal: "rust",
  };
  if ("count" in ret) {
    const at = ret.count.distinct_at ?? last;
    return {
      ok: {
        kind: "query",
        query_echo: echo,
        result: {
          kind: "count",
          count: new Set(paths.map((p) => p.nodes[at])).size,
        },
        stats,
      },
    };
  }
  const { columns, order_by = null, limit = 50, offset = 0 } = ret.rows;
  const named = [...new Set(columns.map((c) => c.at))];
  const value = (n: MockNode, prop: string) =>
    prop === "name" ? (n.props.name ?? null) : (n.props[prop] ?? null);
  // Distinct over the positions the columns name.
  const rows = [
    ...new Map(
      paths.map((p) => [named.map((at) => p.nodes[at]).join(","), p]),
    ).values(),
  ];
  const entityOrder = (p: Path) =>
    named.map((at) => canonical(m.g.node(p.nodes[at]!)!)).join("\u0000");
  rows.sort((a, b) => {
    if (order_by) {
      const x = value(m.g.node(a.nodes[order_by.at]!)!, order_by.prop),
        y = value(m.g.node(b.nodes[order_by.at]!)!, order_by.prop);
      // Absent values first ascending, last descending.
      const c =
        x === y
          ? 0
          : x === null
            ? -1
            : y === null
              ? 1
              : (x as number | string) < (y as number | string)
                ? -1
                : 1;
      if (c) return order_by.desc ? -c : c;
    }
    return entityOrder(a).localeCompare(entityOrder(b));
  });
  // For a reader who sees only some of them: of every row within reach, those
  // it may see, up to what it asked, with what is hidden not counted.
  const partly = (at: number, prop?: string | null) =>
    narrowed && settles(view!, types[at]!, prop);
  const page = !narrowed
    ? rows.slice(offset, offset + limit)
    : rows
        .slice(0, widened(view!, q))
        .filter(
          (p) =>
            named.every(
              (at) =>
                view!.types.get(types[at]!) !== "partial" ||
                entityVisible(m, view!, m.g.node(p.nodes[at]!)!),
            ) &&
            steps.every((s, i) => {
              if (!named.includes(i) || !named.includes(i + 1)) return true;
              const f = p.facts[i]!;
              return (
                !f.stated ||
                !f.pred.startsWith("link:") ||
                factVisible(m, view!, s.via, f.args[0]!, f.args[1]!)
              );
            }),
        )
        .slice(
          0,
          columns.some((c) => partly(c.at, c.prop))
            ? Math.min(limit, MAX_SETTLED_ROWS)
            : limit,
        );
  const nameOf = (n: MockNode) =>
    view && nameUnsafe(view, n.label)
      ? refOf(n).id
      : String(n.props.name ?? refOf(n).id);
  return {
    ok: {
      kind: "query",
      query_echo: echo,
      result: {
        kind: "rows",
        rows: page.map((p) => ({
          cells: columns.map((c) => {
            const n = m.g.node(p.nodes[c.at]!)!;
            return c.prop == null
              ? { ...refOf(n), name: nameOf(n) }
              : c.prop === "name"
                ? nameOf(n)
                : partly(c.at, c.prop)
                  ? settle(m, view!, n, c.prop)
                  : value(n, c.prop);
          }),
          // Each step whose two positions the columns both name.
          edges: steps.flatMap((_, i) => {
            if (!named.includes(i) || !named.includes(i + 1)) return [];
            const f = p.facts[i]!;
            return [
              f.stated && f.pred.startsWith("link:")
                ? {
                    step: i,
                    fact: {
                      link: f.pred.slice("link:".length),
                      subject: refOf(m.g.node(f.args[0]!)!),
                      object: refOf(m.g.node(f.args[1]!)!),
                    },
                  }
                : { step: i, derived: { pred: f.pred, args: refs(m, f.args) } },
            ];
          }),
        })),
        total: narrowed ? page.length : rows.length,
      },
      stats,
    },
  };
}

/**
 * Reads `readsJson` as the engine's reads, or a message for
 * INVALID_ARGUMENT: the engine refuses a request whose reads don't fit its
 * schema before it reads anything.
 */
export function readsOf(readsJson: unknown): Read[] | string {
  let reads: unknown;
  try {
    reads = JSON.parse(String(readsJson ?? ""));
  } catch {
    return "readsJson isn't JSON";
  }
  if (!Array.isArray(reads) || !reads.length)
    return "readsJson must be a list of reads";
  if (reads.length > MAX_READS) return `at most ${MAX_READS} reads at once`;
  for (const [i, r] of reads.entries()) {
    const problems = contractErrors("read_request#/$defs/ReadItem", r);
    if (problems.length) return `read ${i}: ${problems[0]}`;
  }
  return reads as Read[];
}
