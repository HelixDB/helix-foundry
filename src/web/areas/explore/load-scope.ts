import { FOUNDRY_LOAD, isFoundryLabel } from "../../../shared/foundry-labels";
import type { DynamicQueryRequest } from "./database-query";
import type { JsonValue } from "./explorer/lib/types";

// Which part of an ontology's database a read covers. Every node and edge a
// load writes carries the load's id as `foundry_load` (equality-indexed per
// label), so loads of different versions sit side by side and a read picks
// one of them:
// - a load: `$label == L and foundry_load == id`, two bitmaps intersected,
//   whatever the load's size;
// - Other data: what no load wrote. A label outside every load's version
//   holds nothing of Foundry's and is read plainly; a label some version
//   (or Foundry's own prefix) names is shared with the loads, and its other
//   data is the nodes without `foundry_load`.
// Labels and loads go in as constants, as the explorer's labels do, so the
// indexes serve the read without parameter specialization. Every builder of
// the Explorer, Records and the entity and document views takes its source
// from here; none reads the whole graph under a load.

/** Labels some load's version names, by side: shared between loads and other data. */
export type SharedLabels = {
  nodes: ReadonlySet<string>;
  edges: ReadonlySet<string>;
};

/**
 * What a read covers: one load, or the data no load wrote. `loads` (every
 * load not deleted) is what Other data's counts subtract.
 */
export type Scope =
  | { kind: "load"; id: string }
  | { kind: "other"; loads: readonly string[]; shared: SharedLabels };

export type OtherScope = Extract<Scope, { kind: "other" }>;
export type Target = "node" | "edge";

type Ast = { [key: string]: JsonValue };

/** `$label == "<label>"`, the label a constant. */
export const labelIs = (label: string): Ast => ({
  eq: { left: { property: "$label" }, right: { constant: { string: label } } },
});

/** `foundry_load == "<id>"`, the id a constant. */
export const inLoad = (id: string): Ast => ({
  eq: { left: { property: FOUNDRY_LOAD }, right: { constant: { string: id } } },
});

/** No `foundry_load`: written by no load. */
export const inNoLoad = (): Ast => ({ is_null: { property: FOUNDRY_LOAD } });

const and = (predicates: Ast[]): Ast =>
  predicates.length === 1 ? predicates[0]! : { and: { predicates } };

/**
 * Whether other data under `label` has to be told from the loads' by a
 * missing `foundry_load`: Foundry's own labels and every label a load's
 * version names.
 *
 * @example
 * isShared(other, "node", "FoundryMention"); // true
 */
export function isShared(
  scope: OtherScope,
  target: Target,
  label: string,
): boolean {
  return (
    isFoundryLabel(target, label) ||
    (target === "node" ? scope.shared.nodes : scope.shared.edges).has(label)
  );
}

/**
 * The predicate that keeps a row of `label` inside the scope, beside the
 * label's own: the load's id, a missing id for shared labels in Other data,
 * or none for labels no load writes.
 */
export function scopeFilter(
  scope: Scope,
  target: Target,
  label: string,
): Ast | null {
  if (scope.kind === "load") return inLoad(scope.id);
  return isShared(scope, target, label) ? inNoLoad() : null;
}

/**
 * The predicates of a scoped label read: `$label == label` and the scope's
 * filter, for a reader to `and` with its own (an identity lookup adds its
 * key: P(label, L) + `entity_key == $identity`).
 */
export function scopedPredicates(
  scope: Scope,
  target: Target,
  label: string,
): Ast[] {
  const filter = scopeFilter(scope, target, label);
  return filter ? [labelIs(label), filter] : [labelIs(label)];
}

/**
 * The predicate of a scoped label read. In a load it is P(label, L) of the
 * plan: a bitmap intersection.
 *
 * @example
 * scopedLabel({ kind: "load", id: "L1" }, "node", "Customer");
 * // { and: { predicates: [ $label == "Customer", foundry_load == "L1" ] } }
 */
export const scopedLabel = (scope: Scope, target: Target, label: string): Ast =>
  and(scopedPredicates(scope, target, label));

/**
 * The predicate a single node (a point read by id) must meet to be in the
 * scope, whatever its label: the load's id, or none at all.
 */
export const nodeInScope = (scope: Scope): Ast =>
  scope.kind === "load" ? inLoad(scope.id) : inNoLoad();

/** Whether a node or edge read back (its properties) is in the scope. */
export function holds(
  scope: Scope,
  properties: Readonly<Record<string, unknown>>,
): boolean {
  const load = properties[FOUNDRY_LOAD];
  return scope.kind === "load"
    ? load === scope.id
    : load === undefined || load === null;
}

/**
 * Other data under a label from bitmap counts: the label's count less what
 * every load holds under it. Null when a count is missing.
 *
 * @example
 * otherCount(120, [50, 50]); // 20
 */
export function otherCount(
  total: number | null,
  perLoad: readonly (number | null)[],
): number | null {
  if (total === null || perLoad.some((n) => n === null)) return null;
  return Math.max(
    0,
    total - perLoad.reduce<number>((sum, n) => sum + (n ?? 0), 0),
  );
}

/** The scope's key for caches and query keys: the load id, or "other". */
export const scopeKey = (scope: Scope) =>
  scope.kind === "load" ? scope.id : "other";

/**
 * The bitmap counts a label takes in the scope: one, or for Other data under
 * a shared label, the label's and one per load (Other data is the
 * difference, see otherCount). Each costs a bitmap's cardinality, no rows.
 */
export const countCost = (scope: Scope, target: Target, label: string) =>
  scope.kind === "other" && isShared(scope, target, label)
    ? 1 + scope.loads.length
    : 1;

/**
 * A label's count entries in the scope (see countCost), named `name` and
 * `name_load_<k>`; `count` wraps a source in the reader's count op.
 */
export function countEntries(
  scope: Scope,
  target: Target,
  label: string,
  name: string,
  count: (input: Ast) => Ast,
): { name: string; root: Ast }[] {
  const source = (predicate: Ast): Ast =>
    target === "node"
      ? { nodes_where: { predicate } }
      : { edges_where: { predicate } };
  if (scope.kind === "load" || !isShared(scope, target, label))
    return [{ name, root: count(source(scopedLabel(scope, target, label))) }];
  return [
    { name, root: count(source(labelIs(label))) },
    ...scope.loads.map((load, k) => ({
      name: `${name}_load_${k}`,
      root: count(source(and([labelIs(label), inLoad(load)]))),
    })),
  ];
}

/** A label's count in the scope from countEntries' answers; null when one is missing. */
export function countOf(
  data: Readonly<Record<string, unknown>>,
  scope: Scope,
  target: Target,
  label: string,
  name: string,
  total: (value: unknown) => number | null,
): number | null {
  if (scope.kind === "load" || !isShared(scope, target, label))
    return total(data[name]);
  return otherCount(
    total(data[name]),
    scope.loads.map((_, k) => total(data[`${name}_load_${k}`])),
  );
}

/** At most this many counts go in one request (the Explorer's maxCountLabels). */
export const MAX_COUNTS = 60;

/**
 * What one load holds: its version's types (nodes) and links (facts), each a
 * bitmap count in the load, in requests of at most MAX_COUNTS. The Loads
 * page reads these; the Explorer's rail counts the same way.
 */
export function loadCountRequests(
  load: string,
  labels: { nodes: readonly string[]; edges: readonly string[] },
): { request: DynamicQueryRequest; nodes: string[]; edges: string[] }[] {
  const scope: Scope = { kind: "load", id: load };
  const all = [
    ...labels.nodes.map((label) => ({ target: "node" as const, label })),
    ...labels.edges.map((label) => ({ target: "edge" as const, label })),
  ];
  const batches: (typeof all)[] = [];
  for (let i = 0; i < all.length; i += MAX_COUNTS)
    batches.push(all.slice(i, i + MAX_COUNTS));
  return batches.map((batch) => {
    const entries = batch.flatMap(({ target, label }, i) =>
      countEntries(scope, target, label, `count_${i}`, (input) => ({
        count: { input },
      })),
    );
    return {
      request: {
        request_type: "read",
        query_name: "foundry_load_counts",
        query: {
          read: {
            entries: entries.map((e) => ({
              query: { name: e.name, root: e.root },
            })),
            returns: entries.map((e) => e.name),
          },
        },
        parameters: {},
        parameter_types: {},
      },
      nodes: batch.filter((b) => b.target === "node").map((b) => b.label),
      edges: batch.filter((b) => b.target === "edge").map((b) => b.label),
    };
  });
}

/** Each label's count from loadCountRequests' answers; null when missing. */
export function parseLoadLabelCounts(
  bodies: readonly unknown[],
  batches: readonly { nodes: string[]; edges: string[] }[],
): { nodes: Map<string, number | null>; edges: Map<string, number | null> } {
  const nodes = new Map<string, number | null>(),
    edges = new Map<string, number | null>();
  batches.forEach((batch, b) => {
    const body = bodies[b];
    const data =
      body && typeof body === "object" && !Array.isArray(body)
        ? (body as Record<string, unknown>)
        : {};
    const n = (i: number) => {
      const v = data[`count_${i}`];
      return typeof v === "number" && Number.isFinite(v) && v >= 0 ? v : null;
    };
    batch.nodes.forEach((label, i) => nodes.set(label, n(i)));
    batch.edges.forEach((label, i) =>
      edges.set(label, n(batch.nodes.length + i)),
    );
  });
  return { nodes, edges };
}
