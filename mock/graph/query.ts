import { isRecord } from "../../src/shared/json";
import { HelixDateTime, type MockGraph, type Props, type Value } from "./store";

// Runs a v3 dynamic read request (the body a database takes on POST
// /v2/query) over a MockGraph, as Helix answers it: each entry's result is
// kept under its name, and `returns` become the response's keys. It covers
// the operations the explorer and the ontology reads use; any other is
// refused as Helix refuses a request it can't decode (400
// invalid_query_json), so a query the mock can't run fails loudly in tests.
//
// `order_by` and `order_by_multiple` sort as Helix does (see ordered()).
// A DateTime (store.ts HelixDateTime: a datetime prop a load wrote, a
// `date_time` parameter or a `{"date_time": ms}` constant) is no string, as
// in helix-db a29a8d4: it equals only a DateTime of the same millisecond
// (`neq` holds against anything else), compares only with DateTimes (`lt`
// and the like are false against text), sorts after numbers and before
// text, is never `contains`ed or `starts_with` anything, groups by its
// instant, is skipped by `aggregate_by`, and renders as `…mmmZ`.
// Parameters are typed as Helix types them once `parameter_types` is sent
// (see typedParams()); without it they are the JSON they are.
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

// A literal (tagged PropertyValue) as plain JSON, a DateTime as itself.
function constant(c: Ast): Value {
  const [tag, v] = one(c, "constant");
  if (tag === "null") return null;
  if (tag === "date_time")
    return Number.isSafeInteger(v)
      ? new HelixDateTime(v as unknown as number)
      : refuse("a date_time constant is milliseconds since the epoch");
  return v as Value;
}

// Helix's equality: a DateTime equals only a DateTime of the same instant.
const same = (a: Value, b: Value) =>
  a instanceof HelixDateTime || b instanceof HelixDateTime
    ? a instanceof HelixDateTime && b instanceof HelixDateTime && a.ms === b.ms
    : JSON.stringify(a) === JSON.stringify(b);

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
    : a instanceof HelixDateTime && b instanceof HelixDateTime
      ? a.ms - b.ms
      : typeof a === "string" && typeof b === "string"
        ? a < b
          ? -1
          : a > b
            ? 1
            : 0
        : NaN;

// Helix's sort (db execution/interpreter/stream/order.rs, the total order of
// property_value.rs): a missing value sorts first ascending and last
// descending, values by kind (booleans, numbers, DateTimes, then text) and
// within one by value, and ties keep id order whichever the direction.
const ORDERS = new Set(["asc", "desc"]);
const KIND = ["boolean", "number", "datetime", "string"];
const kindOf = (v: Value) =>
  v instanceof HelixDateTime ? "datetime" : typeof v;
function ordered(a: Value, b: Value): number {
  if (a === null || b === null) return a === b ? 0 : a === null ? -1 : 1;
  const ka = KIND.indexOf(kindOf(a)),
    kb = KIND.indexOf(kindOf(b));
  if (ka !== kb) return ka - kb;
  const [x, y] =
    ka < 0
      ? [JSON.stringify(a), JSON.stringify(b)]
      : a instanceof HelixDateTime && b instanceof HelixDateTime
        ? [a.ms, b.ms]
        : [a, b];
  return x! < y! ? -1 : x! > y! ? 1 : 0;
}

function test(scope: Scope, p: Ast, el: El): boolean {
  const [op, a] = one(p, "predicate");
  const side = (k: string) => expr(scope, a[k], el);
  switch (op) {
    case "eq":
      return same(side("left"), side("right"));
    case "neq":
      return !same(side("left"), side("right"));
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
      const v = expr(scope, a.value, el);
      return Array.isArray(list) && list.some((x) => same(x, v));
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

// Helix's planner refuses a property selected twice (DuplicatePropertySelection):
// the whole read fails, however the rows would have read.
const selected = (keys: string[]) => {
  const twice = keys.find((k, i) => keys.indexOf(k) !== i);
  return twice === undefined
    ? keys
    : refuse(`property ${twice} selected twice`);
};

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
        // A DateTime and text of the same instant are two groups.
        const v = property(scope, e, a.property),
          k = `${kindOf(v)}:${JSON.stringify(v)}`;
        const c = counts.get(k) || { value: v, count: 0 };
        c.count += 1;
        counts.set(k, c);
      }
      return [...counts.values()]
        .sort((x, y) => String(x.value).localeCompare(String(y.value)))
        .map((c) => ({ [a.property]: c.value, count: c.count }));
    }
    case "aggregate_by": {
      // Helix's aggregateBy (stream/aggregate.rs): of the property's values
      // the numbers (an integer, a float, or decimal text that reads as
      // one), a DateTime and any other kind skipped, in one row named
      // `<property>_<Function>`; a min, max or mean of none is 0.
      const values = input().flatMap((e) => {
        const v = property(scope, e, a.property);
        return typeof v === "number"
          ? [v]
          : typeof v === "string" &&
              /^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$/.test(v)
            ? [Number(v)]
            : [];
      });
      const sum = values.reduce((x, y) => x + y, 0);
      const fn = String(a.function);
      const value = {
        count: values.length,
        sum,
        min: values.length ? values.reduce((x, y) => Math.min(x, y)) : 0,
        max: values.length ? values.reduce((x, y) => Math.max(x, y)) : 0,
        mean: values.length ? sum / values.length : 0,
      }[fn];
      if (value === undefined || typeof a.property !== "string")
        refuse(`aggregate_by: bad function ${fn} or property`);
      return [
        { [`${a.property}_${fn[0]!.toUpperCase()}${fn.slice(1)}`]: value! },
      ];
    }
    case "values": {
      const keys = selected(a.properties);
      return input().map((e) => row(scope, e, keys));
    }
    case "value_map": {
      const keys = a.properties && selected(a.properties);
      return input().map((e) =>
        keys ? row(scope, e, ["$id", "$label", ...keys]) : everything(scope, e),
      );
    }
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

// A parameter's value as Helix takes it once its type is declared
// (query.rs normalize_typed_value): of the families the site sends, a
// string, a boolean, an integer, a number, an array of strings or integers,
// and a `date_time`, RFC 3339 text that becomes its DateTime, in UTC to the
// millisecond. A value of another kind is refused as Helix refuses it, and
// a type the mock doesn't model is refused too.
function typedParam(name: string, type: unknown, v: Value): Value {
  const fits =
    type === "string"
      ? typeof v === "string"
      : type === "bool"
        ? typeof v === "boolean"
        : type === "i64"
          ? Number.isSafeInteger(v)
          : type === "f64"
            ? typeof v === "number"
            : type === "date_time"
              ? typeof v === "string" && HelixDateTime.parse(v) !== undefined
              : undefined;
  if (fits === undefined) {
    const of = isRecord(type) ? type.array : undefined;
    if (Object.keys(type || {}).length !== 1 || (of !== "string" && of !== "i64"))
      refuse(`parameter '${name}': the mock doesn't model type ${JSON.stringify(type)}`);
    return Array.isArray(v)
      ? v.map((x, i) => typedParam(`${name}[${i}]`, of, x))
      : refuse(`parameter '${name}' expected array of ${of}, but received ${kindOf(v)}`);
  }
  if (!fits)
    refuse(`parameter '${name}' expected ${type}, but received ${kindOf(v)}`);
  return type === "date_time" ? HelixDateTime.parse(v as string)! : v;
}

// A request's parameters as Helix reads them (query.rs
// validate_raw_request): untyped without `parameter_types`; with it every
// value is declared and every declaration has a value
// (ParameterNameMismatch), each typed by typedParam.
function typedParams(req: Ast): Props {
  const values = (req.parameters || {}) as Props;
  if (req.parameter_types === undefined) return values;
  const types = req.parameter_types as Record<string, unknown>;
  const names = (xs: string[]) => `[${xs.map((x) => JSON.stringify(x)).join(", ")}]`;
  const missing = Object.keys(types).filter((k) => !(k in values)),
    extra = Object.keys(values).filter((k) => !(k in types));
  if (missing.length || extra.length)
    refuse(
      `parameter schema names do not match values (missing values: ${names(missing)}, extra values: ${names(extra)})`,
    );
  return Object.fromEntries(
    Object.entries(values).map(([k, v]) => [k, typedParam(k, types[k], v)]),
  );
}

/** Runs `request` over `graph`; never throws. */
export function runQuery(graph: MockGraph, request: unknown): QueryAnswer {
  try {
    const req = request as Ast;
    if (req?.request_type !== "read" || !req.query?.read)
      refuse("only read requests run here");
    const params = typedParams(req);
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
    // As the wire carries it: a DateTime as its text.
    return { status: 200, body: JSON.parse(JSON.stringify(body)) };
  } catch (e) {
    if (e instanceof Refused)
      return {
        status: 400,
        body: { error: "invalid_query_json", msg: e.message },
      };
    throw e;
  }
}
