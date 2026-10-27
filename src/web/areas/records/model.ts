import { isFoundryLabel } from "../../../shared/foundry-labels";
import type { DynamicQueryRequest, JsonValue } from "../explore/database-query";
import {
  MAX_COUNTS,
  countCost,
  countEntries,
  countOf,
  scopedPredicates,
  type Scope,
} from "../explore/load-scope";
import { entityRow, type EntityIdentity } from "../explore/model";
import {
  isInternal,
  normalizeName,
  readable,
  type Naming,
  type Readable,
  type TypeNaming,
} from "../explore/naming";

// Records: an ontology's database as a table, one type of a load's version
// at a time. Every read is a v3 dynamic read query run with the user's
// session (POST /v1/databases:query-read, ../explore/database-query.ts),
// scoped to one load or to Other data (../explore/load-scope.ts); this
// module builds them and reads their answers, trusting nothing in them.
//
// - Types are counted from Helix's bitmaps (`count` over `$label ==` and
//   the load's `foundry_load ==`): no rows are read.
// - A page is `range(start, end)` over the type's nodes in database order,
//   or in the order of one column (`order_by`, which sorts every node of the
//   type, or every match). It reads one row more than a page, to know
//   whether another follows, and projects only the type's own props.
// - Search folds the text as the ontology agent folds names and looks for it
//   in the type's normalized names (`<prop>__nn`), its names and its key; a
//   filter is one equality, on the normalized name for a names prop. Both
//   scan the type's nodes; their matches are counted up to countCap.
// - Offset paging reads every row before the page, so pages stop at
//   maxOffset: past it, a search or a filter narrows the type instead.

export const RECORDS_LIMITS = Object.freeze({
  // Rows on a page.
  page: 50,
  // The deepest row a page starts at.
  maxOffset: 10_000,
  // Matches a search or filter counts, beyond which it says "1,000+".
  countCap: 1000,
  // Types counted, and listed, per ontology.
  maxTypes: 100,
  // Props shown and projected per type.
  maxColumns: 60,
});

// ---------------------------------------------------------------- AST

type Ast = { [key: string]: JsonValue };
type Param = { value: JsonValue; type: JsonValue };
type Entry = { name: string; root: Ast };

const literal = (n: number): Ast => ({ literal: Math.max(0, Math.floor(n)) });
const and = (...predicates: Ast[]): Ast =>
  predicates.length === 1 ? predicates[0]! : { and: { predicates } };
const or = (...predicates: Ast[]): Ast =>
  predicates.length === 1 ? predicates[0]! : { or: { predicates } };
const eq = (property: string, param: string): Ast => ({
  eq: { left: { property }, right: { param } },
});
const contains = (property: string, param: string): Ast => ({
  contains: { value: { property }, substring: { param } },
});
const count = (input: Ast): Ast => ({ count: { input } });
const limit = (input: Ast, n: number): Ast => ({
  limit: { input, count: literal(n) },
});

function request(
  name: string,
  entries: Entry[],
  params: Record<string, Param> = {},
): DynamicQueryRequest {
  return {
    request_type: "read",
    query_name: name,
    query: {
      read: {
        entries: entries.map((e) => ({
          query: { name: e.name, root: e.root },
        })),
        returns: entries.map((e) => e.name),
      },
    },
    parameters: Object.fromEntries(
      Object.entries(params).map(([k, p]) => [k, p.value]),
    ),
    parameter_types: Object.fromEntries(
      Object.entries(params).map(([k, p]) => [k, p.type]),
    ),
  };
}

type Row = Record<string, unknown>;
const isRow = (v: unknown): v is Row =>
  !!v && typeof v === "object" && !Array.isArray(v);
const rows = (v: unknown): Row[] => (Array.isArray(v) ? v.filter(isRow) : []);
const total = (v: unknown) =>
  typeof v === "number" && Number.isInteger(v) && v >= 0 ? v : null;

// ---------------------------------------------------------------- Types

/**
 * The types Records lists: the version's, by name, at most maxTypes. A
 * Foundry label is never one of them, even in a version that names one
 * (the agent refuses such names).
 *
 * @example
 * recordTypes({ version: null, types: new Map(), links: new Set() }); // []
 */
export function recordTypes(naming: Naming): TypeNaming[] {
  return [...naming.types.values()]
    .filter((t) => !isFoundryLabel("node", t.name))
    .sort((a, b) => a.name.localeCompare(b.name))
    .slice(0, RECORDS_LIMITS.maxTypes);
}

/**
 * How many nodes each type has in the scope, from bitmaps: the type's and
 * the load's intersected, or for Other data the type's less every load's.
 * At most maxTypes types, in requests of at most MAX_COUNTS counts (in
 * Other data a type a version names takes one per load besides its own,
 * see countCost), read one after another.
 */
export function typeCountsRequests(
  scope: Scope,
  types: string[],
): { request: DynamicQueryRequest; types: string[] }[] {
  const batches = types
    .slice(0, RECORDS_LIMITS.maxTypes)
    .reduce<{ types: string[]; cost: number }[]>((out, t) => {
      const cost = countCost(scope, "node", t),
        last = out[out.length - 1];
      if (last && last.cost + cost <= MAX_COUNTS) {
        last.types.push(t);
        last.cost += cost;
      } else out.push({ types: [t], cost });
      return out;
    }, []);
  return batches.map((b) => ({
    types: b.types,
    request: request(
      "foundry_record_counts",
      b.types.flatMap((t, i) =>
        countEntries(scope, "node", t, `count_${i}`, count),
      ),
    ),
  }));
}

/** Each type's count from typeCountsRequests' answers, or null when one doesn't say. */
export function parseTypeCounts(
  bodies: readonly unknown[],
  scope: Scope,
  batches: readonly { types: string[] }[],
): Map<string, number | null> {
  return new Map(
    batches.flatMap((b, k) => {
      const body = bodies[k];
      const data = isRow(body) ? body : {};
      return b.types.map(
        (t, i) =>
          [t, countOf(data, scope, "node", t, `count_${i}`, total)] as const,
      );
    }),
  );
}

// ---------------------------------------------------------------- Columns

/** The first column: each record's readable name. */
export const RECORD_COLUMN = "$record";

export type Column = {
  key: string;
  // The version's prop type ("string", "integer", ...), or "name".
  type: string;
  role: "record" | "key" | "names" | "prop";
};

/**
 * A type's columns: its readable name, then its key props, then its names
 * props, then its other props as the version lists them.
 *
 * @example
 * columnsOf({ name: "Customer", props: [{ name: "email", type: "string" }, { name: "id", type: "string" }], key: ["id"], names: [] }).map((c) => c.key); // ["$record", "id", "email"]
 */
export function columnsOf(t: TypeNaming): Column[] {
  const type = (p: string) =>
    t.props.find((x) => x.name === p)?.type || "string";
  const seen = new Set<string>();
  const once = (role: Column["role"]) => (p: string) =>
    seen.has(p) || p.startsWith("$") || p.endsWith("__nn")
      ? []
      : (seen.add(p), [{ key: p, type: type(p), role }]);
  return [
    { key: RECORD_COLUMN, type: "name", role: "record" as const },
    ...t.key.flatMap(once("key")),
    ...t.names.flatMap(once("names")),
    ...t.props.map((p) => p.name).flatMap(once("prop")),
  ].slice(0, RECORDS_LIMITS.maxColumns + 1);
}

/** The props a type shows beside its record column until columns are chosen. */
export const DEFAULT_COLUMNS = 4;

/**
 * The columns a type shows: its record column, then the chosen ones in
 * column order, or by default its first DEFAULT_COLUMNS other props (the
 * record column already stands for its key and names; the Columns menu,
 * the record column's sort and the filter still reach them).
 *
 * @example
 * shownColumns({ name: "Customer", props: [{ name: "email", type: "string" }, { name: "id", type: "string" }], key: ["id"], names: [] }).map((c) => c.key); // ["$record", "email"]
 */
export function shownColumns(
  t: TypeNaming,
  chosen?: readonly string[],
): Column[] {
  const all = columnsOf(t);
  const shown = new Set(
    chosen ||
      all
        .filter((c) => c.role === "prop")
        .slice(0, DEFAULT_COLUMNS)
        .map((c) => c.key),
  );
  return all.filter((c) => c.role === "record" || shown.has(c.key));
}

/**
 * A prop's name as a label, as helix-foundry writes field names.
 *
 * @example
 * fieldName("company_name"); // "Company name"
 * fieldName("amountDue"); // "Amount Due"
 */
export const fieldName = (s: string) =>
  s
    .replace(/_/g, " ")
    .replace(/([a-z])([A-Z])/g, "$1 $2")
    .replace(/^./, (c) => c.toUpperCase());

// A keyed type keeps `<prop>__nn` beside each names prop: what name
// searches, filters and sorts compare.
const folded = (t: TypeNaming, prop: string) =>
  t.key.length > 0 && t.names.includes(prop) ? `${prop}__nn` : undefined;

/**
 * The properties a column sorts by: a names prop's normalized form, the
 * record column's first names prop (or its key, or the normalized name of a
 * type known by its name), or the prop itself. Empty for a column the type
 * doesn't have.
 *
 * @example
 * sortProperties({ name: "Customer", props: [], key: ["id"], names: ["company_name"] }, "$record"); // ["company_name__nn"]
 */
export function sortProperties(t: TypeNaming, column: string): string[] {
  if (column === RECORD_COLUMN) {
    const first = t.names[0];
    if (first) return [folded(t, first) || first];
    return t.key.length ? [...t.key] : ["norm_name"];
  }
  if (!columnsOf(t).some((c) => c.key === column)) return [];
  return [folded(t, column) || column];
}

// ---------------------------------------------------------------- The URL

/** What the Records page shows, as its URL holds it. `page` counts from 0. */
export type RecordsView = {
  type: string;
  search: string;
  filter: { field: string; value: string } | null;
  sort: { column: string; dir: "asc" | "desc" } | null;
  page: number;
  // The open record's entity id; "" for none.
  record: string;
};

export const MAX_PAGE = Math.floor(
  RECORDS_LIMITS.maxOffset / RECORDS_LIMITS.page,
);

/**
 * The view a URL's search params hold: ?type, q, field, value, sort, dir,
 * page (from 1) and record. Anything out of range reads as its default.
 *
 * @example
 * readView(new URLSearchParams("type=Customer&page=2")).page; // 1
 */
export function readView(params: URLSearchParams): RecordsView {
  const field = params.get("field") || "",
    sort = params.get("sort") || "";
  const page = Math.floor(Number(params.get("page") || 1) - 1);
  return {
    type: params.get("type") || "",
    search: params.get("q") || "",
    filter: field ? { field, value: params.get("value") || "" } : null,
    sort: sort
      ? { column: sort, dir: params.get("dir") === "desc" ? "desc" : "asc" }
      : null,
    page: Number.isFinite(page) ? Math.min(Math.max(page, 0), MAX_PAGE) : 0,
    record: params.get("record") || "",
  };
}

/** The search params of a view: readView's inverse, defaults left out. */
export function viewParams(v: RecordsView): Record<string, string> {
  return Object.fromEntries(
    Object.entries({
      type: v.type,
      q: v.search,
      field: v.filter?.field || "",
      value: v.filter?.field ? v.filter.value : "",
      sort: v.sort?.column || "",
      dir: v.sort?.dir === "desc" ? "desc" : "",
      page: v.page > 0 ? String(v.page + 1) : "",
      record: v.record,
    }).filter(([, value]) => value !== ""),
  );
}

// ---------------------------------------------------------------- Search and filter

const INTEGER = /^[+-]?\d+$/;

/**
 * What a search looks for in a type's nodes: "all" for no text, "none" when
 * nothing of the type can hold it (a number-only key and words), else a
 * predicate and its parameters.
 */
export type Search =
  | { kind: "all" }
  | { kind: "none" }
  | { kind: "match"; predicate: Ast; params: Record<string, Param> };

/**
 * The search for `text` in a type: its normalized names contain the text
 * folded; its names or stored name contain it as typed; its string key
 * props contain it as typed, upper-cased or lower-cased; a numeric key
 * equals it; a type known by its name has its normalized name contain it.
 */
export function searchOf(t: TypeNaming, text: string): Search {
  const q = text.trim();
  if (!q) return { kind: "all" };
  const qn = normalizeName(q),
    n = Number(q);
  const type = (p: string) =>
    t.props.find((x) => x.name === p)?.type || "string";
  const preds = new Map<string, Ast>(),
    params: Record<string, Param> = {};
  const add = (pred: Ast, param: string, value: JsonValue, kind: string) => {
    preds.set(JSON.stringify(pred), pred);
    params[param] = { value, type: kind };
  };
  for (const name of t.names) {
    const nn = folded(t, name);
    if (nn && qn) add(contains(nn, "qn"), "qn", qn, "string");
    add(contains(name, "q"), "q", q, "string");
  }
  for (const k of t.key) {
    if (type(k) === "integer") {
      if (INTEGER.test(q) && Number.isSafeInteger(n))
        add(eq(k, "qi"), "qi", n, "i64");
    } else if (type(k) === "float") {
      if (Number.isFinite(n)) add(eq(k, "qf"), "qf", n, "f64");
    } else {
      // `contains` keeps case, and keys are often upper or lower case
      // throughout: try those too, in the same scan.
      add(contains(k, "q"), "q", q, "string");
      const upper = q.toUpperCase(),
        lower = q.toLowerCase();
      if (upper !== q) add(contains(k, "qu"), "qu", upper, "string");
      if (lower !== q) add(contains(k, "ql"), "ql", lower, "string");
    }
  }
  if (!t.key.length) {
    if (qn) add(contains("norm_name", "qn"), "qn", qn, "string");
    add(contains("name", "q"), "q", q, "string");
  }
  return preds.size
    ? { kind: "match", predicate: or(...preds.values()), params }
    : { kind: "none" };
}

/** A filter's equality, or why its value can't be one. */
export type Filter =
  | { ok: true; property: string; value: JsonValue; type: string }
  | { ok: false; error: string };

/**
 * The equality a filter on `field` reads `raw` as, by the prop's type: a
 * whole number, a number, true or false, or text (a names prop compares
 * its normalized form, so case and punctuation don't matter).
 *
 * @example
 * filterOf({ name: "Invoice", props: [{ name: "amount_due", type: "integer" }], key: [], names: [] }, "amount_due", "54000"); // { ok: true, property: "amount_due", value: 54000, type: "i64" }
 */
export function filterOf(t: TypeNaming, field: string, raw: string): Filter {
  const column = columnsOf(t).find(
    (c) => c.key === field && c.key !== RECORD_COLUMN,
  );
  if (!column) return { ok: false, error: `${t.name} has no field ${field}.` };
  const v = raw.trim();
  if (!v) return { ok: false, error: "Enter a value to match." };
  switch (column.type) {
    case "integer": {
      const n = Number(v);
      return INTEGER.test(v) && Number.isSafeInteger(n)
        ? { ok: true, property: field, value: n, type: "i64" }
        : { ok: false, error: "Enter a whole number." };
    }
    case "float": {
      const n = Number(v);
      return Number.isFinite(n)
        ? { ok: true, property: field, value: n, type: "f64" }
        : { ok: false, error: "Enter a number." };
    }
    case "boolean": {
      const b = /^(true|yes)$/i.test(v)
        ? true
        : /^(false|no)$/i.test(v)
          ? false
          : undefined;
      return b === undefined
        ? { ok: false, error: "Enter true or false." }
        : { ok: true, property: field, value: b, type: "bool" };
    }
  }
  const nn = folded(t, field);
  if (nn) {
    const n = normalizeName(v);
    return n
      ? { ok: true, property: nn, value: n, type: "string" }
      : { ok: false, error: "Enter a name with letters or digits." };
  }
  return { ok: true, property: field, value: v, type: "string" };
}

// ---------------------------------------------------------------- A page

/** A page's read, or why the page can't be read. */
export type PageRead =
  | { kind: "read"; request: DynamicQueryRequest; offset: number }
  | { kind: "none" }
  | { kind: "refused"; reason: "filter"; message: string };

/** The properties a page projects: identity, stored name and the type's columns. */
const projection = (t: TypeNaming) => [
  "$id",
  "$label",
  "entity_key",
  "norm_name",
  "name",
  ...columnsOf(t)
    .filter((c) => c.role !== "record" && c.key !== "name")
    .map((c) => c.key),
];

/**
 * A view's filter once it has a value: a field picked with nothing typed
 * yet filters nothing.
 *
 * @example
 * activeFilter({ type: "Customer", search: "", filter: { field: "currency", value: " " }, sort: null, page: 0, record: "" }); // null
 */
export const activeFilter = (view: RecordsView) =>
  view.filter?.value.trim() ? view.filter : null;

/**
 * The read of one page of a type's records for a view: its rows (one more
 * than a page) and, for a search or filter, its matches counted up to
 * countCap. "none" when the search can't match anything of the type.
 */
export function pageRead(
  scope: Scope,
  t: TypeNaming,
  view: RecordsView,
): PageRead {
  const search = searchOf(t, view.search);
  if (search.kind === "none") return { kind: "none" };
  const params: Record<string, Param> =
    search.kind === "match" ? { ...search.params } : {};
  const where: Ast[] = scopedPredicates(scope, "node", t.name);
  const filter = activeFilter(view);
  if (filter) {
    const f = filterOf(t, filter.field, filter.value);
    if (!f.ok) return { kind: "refused", reason: "filter", message: f.error };
    params.fv = { value: f.value, type: f.type };
    where.push(eq(f.property, "fv"));
  }
  let stream: Ast = { nodes_where: { predicate: and(...where) } };
  if (search.kind === "match")
    stream = { where: { input: stream, predicate: search.predicate } };
  const matches = stream;
  const by = view.sort ? sortProperties(t, view.sort.column) : [];
  const dir = view.sort?.dir === "desc" ? "desc" : "asc";
  if (by.length === 1)
    stream = { order_by: { input: stream, property: by[0]!, order: dir } };
  else if (by.length > 1)
    stream = {
      order_by_multiple: { input: stream, orderings: by.map((p) => [p, dir]) },
    };
  const page = Math.min(Math.max(view.page, 0), MAX_PAGE),
    offset = page * RECORDS_LIMITS.page;
  const entries: Entry[] = [
    {
      name: "rows",
      root: {
        values: {
          input: {
            range: {
              input: stream,
              start: literal(offset),
              end: literal(offset + RECORDS_LIMITS.page + 1),
            },
          },
          properties: projection(t),
        },
      },
    },
  ];
  if (search.kind === "match" || filter)
    entries.push({
      name: "matched",
      root: count(limit(matches, RECORDS_LIMITS.countCap + 1)),
    });
  return {
    kind: "read",
    request: request("foundry_records", entries, params),
    offset,
  };
}

/** One record as a page shows it. */
export type RecordRow = {
  dbId: number;
  // Its entity id and identity: every entity a load writes has one.
  entity?: { id: string; identity: EntityIdentity };
  name: string;
  by: Readable["by"];
  // The type's props it holds (never the layout's own).
  props: Record<string, unknown>;
};

export type RecordsPage = {
  rows: RecordRow[];
  // Another page follows.
  more: boolean;
  // A search's or filter's matches: `capped` when there are more than countCap.
  matched: { count: number; capped: boolean } | null;
};

/** Reads pageRead's answer. */
export function parsePage(
  body: unknown,
  naming: Naming,
  t: TypeNaming,
): RecordsPage {
  const data = isRow(body) ? body : {};
  const found = rows(data.rows).flatMap((r): RecordRow[] => {
    const dbId = r.$id;
    if (typeof dbId !== "number" || !Number.isInteger(dbId)) return [];
    const e = entityRow({ ...r, $label: t.name });
    const name = readable(naming, t.name, r, dbId);
    return [
      {
        dbId,
        entity: e && { id: e.id, identity: e.identity },
        name: name.name,
        by: name.by,
        props: Object.fromEntries(
          Object.entries(r).filter(
            ([k]) =>
              !k.startsWith("$") &&
              !isInternal(naming, "node", t.name, k) &&
              (k !== "name" || t.props.some((p) => p.name === "name")),
          ),
        ),
      },
    ];
  });
  const matched = total(data.matched);
  return {
    rows: found.slice(0, RECORDS_LIMITS.page),
    more: found.length > RECORDS_LIMITS.page,
    matched:
      matched === null
        ? null
        : {
            count: Math.min(matched, RECORDS_LIMITS.countCap),
            capped: matched > RECORDS_LIMITS.countCap,
          },
  };
}

/**
 * Where a page sits: its first and last row (from 1), out of what's known,
 * and whether it can go back or on. `total` is the type's count, or the
 * matches of a search or filter; `capped` means there are more than it.
 *
 * @example
 * pageSpan(1, 14, 64, false).text; // "51–64 of 64"
 */
export function pageSpan(
  page: number,
  shown: number,
  total: number | null,
  capped: boolean,
  more = false,
) {
  const first = shown ? page * RECORDS_LIMITS.page + 1 : 0,
    last = page * RECORDS_LIMITS.page + shown;
  const of =
    total === null
      ? ""
      : ` of ${new Intl.NumberFormat("en-US").format(total)}${capped ? "+" : ""}`;
  const n = (x: number) => new Intl.NumberFormat("en-US").format(x);
  return {
    first,
    last,
    // An empty page past the last still says how many there are.
    text: shown ? `${n(first)}–${n(last)}${of}` : total ? `0${of}` : "",
    prev: page > 0,
    next: more && page < MAX_PAGE,
    // More rows than paging reaches: a search or a filter gets to the rest.
    deep: more && page >= MAX_PAGE,
  };
}

/**
 * A record's properties for its detail: the type's props in column order
 * (empty ones too), then anything else it holds that isn't the layout's.
 */
export function detailProps(
  naming: Naming,
  t: TypeNaming | undefined,
  label: string,
  props: Readonly<Record<string, unknown>>,
): [string, unknown][] {
  const declared = t
    ? columnsOf(t)
        .filter((c) => c.role !== "record")
        .map((c) => c.key)
    : [];
  const rest = Object.keys(props)
    .filter(
      (k) =>
        !declared.includes(k) &&
        !k.startsWith("$") &&
        k !== "name" &&
        !isInternal(naming, "node", label, k),
    )
    .sort((a, b) => a.localeCompare(b));
  return [...declared, ...rest].map((k) => [k, props[k]]);
}
