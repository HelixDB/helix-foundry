import { FOUNDRY_LOAD } from "../../../shared/foundry-labels";
import type { DynamicQueryRequest } from "./database-query";
import type { JsonValue } from "./explorer/lib/types";

// Which part of an ontology's database a read covers: one load. Every node
// and edge a load writes carries the load's id as `foundry_load`
// (equality-indexed per label), so loads of different versions sit side by
// side and a read picks one of them: `$label == L and foundry_load == id`,
// two bitmaps intersected, whatever the load's size. Labels and loads go in as
// constants, as the explorer's labels do, so the indexes serve the read
// without parameter specialization. Every builder of the Explorer, Records and
// the entity and document views takes its source from here; none reads the
// whole graph.

/** What a read covers: the load with this id. */
export type Scope = { id: string };

type Ast = { [key: string]: JsonValue };

/** `$label == "<label>"`, the label a constant. */
const labelIs = (label: string): Ast => ({
  eq: { left: { property: "$label" }, right: { constant: { string: label } } },
});

/** `foundry_load == "<id>"`, the id a constant. */
export const inLoad = (id: string): Ast => ({
  eq: { left: { property: FOUNDRY_LOAD }, right: { constant: { string: id } } },
});

/**
 * The predicates of a scoped label read, for a reader to `and` with its own
 * (an identity lookup adds its key: P(label, L) + `entity_key == $identity`).
 */
export const scopedPredicates = (scope: Scope, label: string): Ast[] => [
  labelIs(label),
  inLoad(scope.id),
];

/**
 * The predicate of a scoped label read, P(label, L) of the plan: a bitmap
 * intersection.
 *
 * @example
 * scopedLabel({ id: "L1" }, "Customer");
 * // { and: { predicates: [ $label == "Customer", foundry_load == "L1" ] } }
 */
export const scopedLabel = (scope: Scope, label: string): Ast => ({
  and: { predicates: scopedPredicates(scope, label) },
});

/** Whether a node or edge read back (its properties) is in the scope. */
export const holds = (
  scope: Scope,
  properties: Readonly<Record<string, unknown>>,
): boolean => properties[FOUNDRY_LOAD] === scope.id;

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
  const scope: Scope = { id: load };
  const all = [
    ...labels.nodes.map((label) => ({ source: "nodes_where", label })),
    ...labels.edges.map((label) => ({ source: "edges_where", label })),
  ];
  const batches: (typeof all)[] = [];
  for (let i = 0; i < all.length; i += MAX_COUNTS)
    batches.push(all.slice(i, i + MAX_COUNTS));
  return batches.map((batch) => {
    const names = batch.map((_, i) => `count_${i}`);
    return {
      request: {
        request_type: "read",
        query_name: "foundry_load_counts",
        query: {
          read: {
            entries: batch.map(({ source, label }, i) => ({
              query: {
                name: names[i]!,
                root: {
                  count: {
                    input: {
                      [source]: { predicate: scopedLabel(scope, label) },
                    },
                  },
                },
              },
            })),
            returns: names,
          },
        },
        parameters: {},
        parameter_types: {},
      },
      nodes: batch
        .filter((b) => b.source === "nodes_where")
        .map((b) => b.label),
      edges: batch
        .filter((b) => b.source === "edges_where")
        .map((b) => b.label),
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
