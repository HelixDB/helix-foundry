import type { MockGraph, Props, Value } from "./store";

// Runs a v3 dynamic read request (the body a database takes on POST
// /v2/query) over a MockGraph, as Helix answers it: each entry's result is
// kept under its name, and `returns` become the response's keys. It covers
// the operations the explorer and the ontology reads use; any other is
// refused as Helix refuses a request it can't decode (400
// invalid_query_json), so a query the mock can't run fails loudly in tests.
//
// `order_by` and `order_by_multiple` sort as Helix does (see ordered()).
// Answers follow Helix's shapes: `values` rows hold only the properties a
// row has; `value_map` every property; `edge_properties` the edge's
// properties with `$from` and `$to`; `group_count` rows `{<property>, count}`;
// an entry bounded by `limit 1` that finds nothing is null; streams nobody
// projected come back as `{ $id }` rows.

type Ast = Record<string, any>;
type El = { kind: "node" | "edge"; id: number; score?: number };
type Result = El[] | Record<string, Value>[] | Value;
type Scope = {
  graph: MockGraph;
  params: Record<string, Value>;
  vars: Map<string, Result>;
};

export type QueryAnswer = { status: number; body: unknown };

class Refused extends Error {}
const refuse = (message: string): never => {
  throw new Refused(message);
};
const one = (ast: Ast, where: string): [string, Ast] => {
  const keys = Object.keys(ast || {});
  if (keys.length !== 1) refuse(`${where}: expected one operation`);
  return [keys[0]!, ast[keys[0]!]];
};
const isEls = (r: Result): r is El[] =>
  Array.isArray(r) &&
  r.every(
    (x) =>
      !!x &&
      typeof x === "object" &&
      "kind" in (x as object) &&
      "id" in (x as object),
  );

function elements(r: Result | undefined, where: string): El[] {
  if (r === undefined) refuse(`${where}: unknown variable`);
  if (Array.isArray(r) && r.length === 0) return [];
  if (!isEls(r!)) refuse(`${where}: not a stream of nodes or edges`);
  return r as El[];
}

// A literal (tagged PropertyValue) as plain JSON.
function constant(c: Ast): Value {
  const [tag, v] = one(c, "constant");
  if (tag === "null") return null;
  return v as Value;
}

function expr(scope: Scope, e: Ast, row?: El): Value {
  if ("property" in e) return row ? property(scope, row, e.property) : null;
  if ("constant" in e) return constant(e.constant);
  if ("param" in e) {
    if (!(e.param in scope.params)) refuse(`unknown parameter ${e.param}`);
    return scope.params[e.param]!;
  }
  if ("value" in e) return constant(e.value);
  if ("expr" in e) return expr(scope, e.expr, row);
  return refuse(`unsupported expression ${Object.keys(e)}`);
}

function property(scope: Scope, el: El, name: string): Value {
  if (name === "$id") return el.id;
  if (name === "$score") return el.score ?? null;
  if (el.kind === "node") {
    const n = scope.graph.nodes.get(el.id)!;
    return name === "$label" ? n.label : (n.props[name] ?? null);
  }
  const e = scope.graph.edges.get(el.id)!;
  if (name === "$label") return e.label;
  if (name === "$from") return e.from;
  if (name === "$to") return e.to;
  return e.props[name] ?? null;
}

const compare = (a: Value, b: Value) =>
  typeof a === "number" && typeof b === "number"
    ? a - b
    : typeof a === "string" && typeof b === "string"
      ? a < b
        ? -1
        : a > b
          ? 1
          : 0
      : NaN;

// Helix's sort (db execution/interpreter/stream/order.rs): a missing value
// sorts first ascending and last descending, values of one kind by value,
// and ties keep id order whichever the direction.
const ORDERS = new Set(["asc", "desc"]);
const KIND = ["boolean", "number", "string"];
function ordered(a: Value, b: Value): number {
  if (a === null || b === null) return a === b ? 0 : a === null ? -1 : 1;
  const ka = KIND.indexOf(typeof a),
    kb = KIND.indexOf(typeof b);
  if (ka !== kb) return ka - kb;
  const [x, y] = ka < 0 ? [JSON.stringify(a), JSON.stringify(b)] : [a, b];
  return x! < y! ? -1 : x! > y! ? 1 : 0;
}

function test(scope: Scope, p: Ast, el: El): boolean {
  const [op, a] = one(p, "predicate");
  const side = (k: string) => expr(scope, a[k], el);
  switch (op) {
    case "eq":
      return JSON.stringify(side("left")) === JSON.stringify(side("right"));
    case "neq":
      return JSON.stringify(side("left")) !== JSON.stringify(side("right"));
    case "gt":
      return compare(side("left"), side("right")) > 0;
    case "gte":
      return compare(side("left"), side("right")) >= 0;
    case "lt":
      return compare(side("left"), side("right")) < 0;
    case "lte":
      return compare(side("left"), side("right")) <= 0;
    case "and":
      return (a.predicates as Ast[]).every((q) => test(scope, q, el));
    case "or":
      return (a.predicates as Ast[]).some((q) => test(scope, q, el));
    case "not":
      return !test(scope, a.predicate ?? a, el);
    case "is_in": {
      const list = expr(scope, a.values, el);
      const v = JSON.stringify(expr(scope, a.value, el));
      return Array.isArray(list) && list.some((x) => JSON.stringify(x) === v);
    }
    case "contains": {
      const v = expr(scope, a.value, el),
        s = expr(scope, a.substring, el);
      return typeof v === "string" && typeof s === "string" && v.includes(s);
    }
    case "is_null":
      return property(scope, el, String(a.property)) === null;
    case "starts_with": {
      const v = expr(scope, a.value, el),
        s = expr(scope, a.prefix, el);
      return typeof v === "string" && typeof s === "string" && v.startsWith(s);
    }
    default:
      return refuse(`unsupported predicate ${op}`);
  }
}

function bound(scope: Scope, c: Ast): number {
  const v = "literal" in c ? c.literal : expr(scope, c);
  if (typeof v !== "number" || !Number.isInteger(v) || v < 0)
    refuse("a bound must be a non-negative integer");
  return v as number;
}

function ids(scope: Scope, ref: Ast | string): El["id"][] | "all" | El[] {
  if (ref === "all") return "all";
  if (typeof ref !== "object") return refuse("unsupported reference");
  if ("ids" in ref) return ref.ids as number[];
  if ("param" in ref) {
    const v = expr(scope, { param: ref.param });
    if (!Array.isArray(v) && typeof v !== "number")
      refuse(`parameter ${ref.param} is not an id list`);
    return (Array.isArray(v) ? v : [v]) as number[];
  }
  if ("var" in ref) return elements(scope.vars.get(ref.var), `var ${ref.var}`);
  return refuse("unsupported reference");
}

function source(scope: Scope, kind: El["kind"], ref: Ast | string): El[] {
  const all = kind === "node" ? scope.graph.nodes : scope.graph.edges;
  const r = ids(scope, ref);
  if (r === "all") return [...all.keys()].map((id) => ({ kind, id }));
  if (r.length && typeof r[0] === "object")
    return (r as El[]).filter((e) => e.kind === kind);
  return (r as number[])
    .filter((id) => all.has(id))
    .map((id) => ({ kind, id }));
}

function walk(
  scope: Scope,
  input: El[],
  dir: "out" | "in" | "both",
  label: string | undefined,
): { edge: number; other: number }[] {
  const g = scope.graph;
  return input
    .filter((e) => e.kind === "node")
    .flatMap((n) =>
      [
        ...(dir !== "in"
          ? (g.outgoing.get(n.id) || []).map((edge) => ({
              edge,
              other: g.edges.get(edge)!.to,
            }))
          : []),
        ...(dir !== "out"
          ? (g.incoming.get(n.id) || []).map((edge) => ({
              edge,
              other: g.edges.get(edge)!.from,
            }))
          : []),
      ].filter((x) => !label || g.edges.get(x.edge)!.label === label),
    );
}

const row = (scope: Scope, el: El, keys: string[]) =>
  Object.fromEntries(
    keys.flatMap((k) => {
      const v = property(scope, el, k);
      return v === null && !(k === "$id" || k === "$label") ? [] : [[k, v]];
    }),
  ) as Record<string, Value>;

function everything(scope: Scope, el: El): Record<string, Value> {
  if (el.kind === "node") {
    const n = scope.graph.nodes.get(el.id)!;
    return { $id: n.id, $label: n.label, ...n.props };
  }
  const e = scope.graph.edges.get(el.id)!;
  return { $id: e.id, $label: e.label, $from: e.from, $to: e.to, ...e.props };
}

function textScore(text: Value, query: string): number {
  if (typeof text !== "string") return 0;
  const words = query.toLowerCase().split(/\W+/).filter(Boolean),
    hay = text.toLowerCase();
  return words.reduce((n, w) => n + (hay.includes(w) ? 1 : 0), 0);
}

function run(scope: Scope, ast: Ast): Result {
  const [op, a] = one(ast, "operation");
  const input = () => elements(run(scope, a.input), op);
  switch (op) {
    case "nodes":
      return source(scope, "node", a.reference);
    case "edges":
      return source(scope, "edge", a.reference);
    case "nodes_where":
      return source(scope, "node", "all").filter((e) =>
        test(scope, a.predicate, e),
      );
    case "edges_where":
      return source(scope, "edge", "all").filter((e) =>
        test(scope, a.predicate, e),
      );
    case "where":
      return input().filter((e) => test(scope, a.predicate, e));
    case "limit":
      return input().slice(0, bound(scope, a.count));
    case "skip":
      return input().slice(bound(scope, a.count));
    case "range":
      return input().slice(bound(scope, a.start), bound(scope, a.end));
    case "order_by":
    case "order_by_multiple": {
      const keys = (
        op === "order_by" ? [[a.property, a.order]] : a.orderings
      ) as [string, string][];
      if (
        !Array.isArray(keys) ||
        !keys.length ||
        keys.some(([p, o]) => typeof p !== "string" || !ORDERS.has(o)) ||
        new Set(keys.map(([p]) => p)).size !== keys.length
      )
        refuse(`${op}: bad ordering`);
      return [...input()].sort((x, y) => {
        for (const [p, o] of keys) {
          const c = ordered(property(scope, x, p), property(scope, y, p));
          if (c) return o === "desc" ? -c : c;
        }
        return x.id - y.id;
      });
    }
    case "dedup": {
      const seen = new Set<string>();
      return input().filter((e) => {
        const k = `${e.kind}:${e.id}`;
        return !seen.has(k) && !!seen.add(k);
      });
    }
    case "inject":
      return [
        ...input(),
        ...elements(scope.vars.get(a.variable), `var ${a.variable}`),
      ];
    case "out":
    case "in":
    case "both":
      return walk(scope, input(), op, a.label).map((x) => ({
        kind: "node" as const,
        id: x.other,
      }));
    case "out_e":
    case "in_e":
    case "both_e":
      return walk(
        scope,
        input(),
        op.slice(0, -2) as "out" | "in" | "both",
        a.label,
      ).map((x) => ({ kind: "edge" as const, id: x.edge }));
    case "out_n":
    case "in_n":
      return input()
        .filter((e) => e.kind === "edge")
        .map((e) => {
          const edge = scope.graph.edges.get(e.id)!;
          return {
            kind: "node" as const,
            id: op === "out_n" ? edge.to : edge.from,
          };
        });
    case "count":
      return input().length;
    case "exists":
      return input().length > 0;
    case "group_count": {
      const counts = new Map<string, { value: Value; count: number }>();
      for (const e of input()) {
        const v = property(scope, e, a.property),
          k = JSON.stringify(v);
        const c = counts.get(k) || { value: v, count: 0 };
        c.count += 1;
        counts.set(k, c);
      }
      return [...counts.values()]
        .sort((x, y) => String(x.value).localeCompare(String(y.value)))
        .map((c) => ({ [a.property]: c.value, count: c.count }));
    }
    case "values":
      return input().map((e) => row(scope, e, a.properties));
    case "value_map":
      return input().map((e) =>
        a.properties
          ? row(scope, e, ["$id", "$label", ...a.properties])
          : everything(scope, e),
      );
    case "edge_properties":
      return input()
        .filter((e) => e.kind === "edge")
        .map((e) => everything(scope, e));
    case "text_search_nodes": {
      const query = String(expr(scope, a.query_text));
      return source(scope, "node", "all")
        .filter((e) => scope.graph.nodes.get(e.id)!.label === a.label)
        .map((e) => ({
          ...e,
          score: textScore(property(scope, e, a.property), query),
        }))
        .filter((e) => e.score > 0)
        .sort((x, y) => y.score - x.score || x.id - y.id)
        .slice(0, bound(scope, a.k));
    }
    default:
      return refuse(`unsupported operation ${op}`);
  }
}

// An entry whose stream is bounded by `limit 1` returns null when empty.
function singular(ast: Ast): boolean {
  const [op, a] = one(ast, "operation");
  if (op === "values" || op === "value_map" || op === "edge_properties")
    return singular(a.input);
  return op === "limit" && a.count?.literal === 1;
}

function answer(scope: Scope, r: Result, ast: Ast): unknown {
  if (Array.isArray(r) && r.length === 0 && singular(ast)) return null;
  return isEls(r) && r.length ? r.map((e) => ({ $id: e.id })) : (r as unknown);
}

/** Runs `request` over `graph`; never throws. */
export function runQuery(graph: MockGraph, request: unknown): QueryAnswer {
  try {
    const req = request as Ast;
    if (req?.request_type !== "read" || !req.query?.read)
      refuse("only read requests run here");
    const params = (req.parameters || {}) as Props;
    const scope: Scope = { graph, params, vars: new Map() };
    const roots = new Map<string, Ast>();
    for (const entry of req.query.read.entries as Ast[]) {
      if (!entry.query) refuse("unsupported entry");
      roots.set(entry.query.name, entry.query.root);
      scope.vars.set(entry.query.name, run(scope, entry.query.root));
    }
    const body: Record<string, unknown> = {};
    for (const name of req.query.read.returns as string[]) {
      if (!scope.vars.has(name)) refuse(`unknown return ${name}`);
      body[name] = answer(scope, scope.vars.get(name)!, roots.get(name)!);
    }
    return { status: 200, body };
  } catch (e) {
    if (e instanceof Refused)
      return {
        status: 400,
        body: { error: "invalid_query_json", msg: e.message },
      };
    throw e;
  }
}
