import { FOUNDRY_LOAD } from "../../../../../shared/foundry-labels";
import type { DynamicQueryRequest } from "../../database-query";
import {
  countCost,
  countEntries,
  countOf,
  holds,
  isShared,
  nodeInScope,
  scopedLabel,
  type Scope,
  type Target,
} from "../../load-scope";
import type { IndexCatalogEntry } from "./types";

import {
  isRecord,
  keysForLabel,
  MAX_SHAPE_KEYS,
  rowsOf,
  toDbId,
  toGraphEdges,
  toGraphNode,
  toGraphNodes,
  type ProbedFetchShape,
} from "./normalize";
import {
  groupRowsByLabel,
  indexesByProperty,
  isHeavyValue,
  LabelSchemaBuilder,
} from "./schema";
import type {
  DegreeInfo,
  DegreeProbe,
  GraphData,
  GraphEdge,
  GraphNode,
  JsonValue,
  LabelCount,
  LabelSchema,
  LabelStats,
  NeighborhoodResult,
  NodeFetchShape,
  SearchHit,
} from "./types";

/**
 * v3 request builders and response parsers for the explorer. Every builder
 * returns a complete `DynamicQueryRequest` named `explorer_<op>`; ids travel
 * as typed parameters. Labels are constants in their predicates
 * (`$label == "User"`): the label bitmap then serves the read on every
 * engine, while a parameter only does once the planner specializes
 * parameters (helix-db c8e1ce9f9, v3.2.0), and is a full scan plus filter
 * before that.
 *
 * What stops a read early (measured on HelixDB v0.0.6, 300k nodes):
 * - A literal `limit` over a source stops once it is met; a limit given as a
 *   parameter (`{ expr: { param } }`) is applied after the whole input was
 *   read (`limit(nodes all, $n)` with n = 5: 3.6 s; a literal 500: 58 ms).
 *   So stream bounds (`limit`, `skip`) are always literals.
 * - A `where` in front of a limit also reads its whole input first (a
 *   100k-node label: 1.1 s per filtered page, 3.5 ms unfiltered), so bounded
 *   reads avoid filters: label pages skip instead of filtering on `$id`.
 *   One exception, on purpose: while edge labels are hidden (types.ts
 *   LabelExclusion), the reads of a node's edges (expansions, bounded
 *   degree counts) filter on `$label` in front of their limit, so a page or
 *   a count isn't used up by edges the canvas doesn't show (Foundry's
 *   mentions and evidence). Each such read then reads every edge on that
 *   side of the node, as an unbounded one would.
 * - `out_e`/`in_e` collect all of a node's edges whatever the limit (one
 *   key lookup per neighbor, then a record read per edge for most uses), so
 *   any read of a node's edges costs time in its degree. `out`/`in` (node to
 *   node) decode the node's adjacency list from one key and read nothing per
 *   edge: on a 67.5k-edge hub, `count(limit(in(hub), 1001))` takes ~20 ms
 *   against ~1 s for `count(limit(in_e(hub), 1001))` and 2 s for the
 *   per-label `group_count`. So the explorer probes a node's size with
 *   `out`/`in` first and touches its edges on its own only below
 *   `degreeThreshold`; above it, counting or connecting its edges is an
 *   explicit, warned action.
 * - A filter on an edge's endpoints (`$from`, `$to`) reads them per edge, so
 *   connecting a node costs one read per edge, not per neighbor. Parallel
 *   edges make the two differ by orders of magnitude, so connects are
 *   planned from a bounded edge count (one pair lookup per neighbor, no
 *   per-edge reads), never from the neighbor count alone.
 *
 * Every stream that can grow with the data is bounded, including per-node
 * expansions, so a supernode cannot blow a response past the ~4 MiB broker
 * limit. Nodes are fetched with `values` over a `NodeFetchShape` so vectors
 * and other heavy properties never ride along; only `value_map` (shape.keys
 * null) returns everything.
 *
 * Nothing the explorer runs on its own reads the whole graph. Labels are
 * found by a bounded discovery read and counted from the label bitmaps
 * (`count` over `nodes_where $label == "x"` is answered from the bitmap's
 * cardinality). `count`/`group_count` over `all` scan every node and edge;
 * only the explicit "Scan for all labels" does that.
 *
 * Every label read is scoped (../../load-scope.ts): in a load, its source is
 * `$label == "x" and foundry_load == "<load>"`, two bitmaps intersected, and
 * nothing reads `nodes`/`edges` `all` (discovery and the label scan run for
 * Other data only, whose labels are the database's own). A load's edges stay
 * inside it, so reads that follow edges (expansions, degree probes, connect)
 * need no filter; every node row carries `foundry_load`, so a node reached
 * outside the load is told apart.
 */

export interface ExplorerLimits {
  /** Most nodes in the first view. */
  sampleNodes: number;
  /** Most edges in the first view. */
  sampleEdges: number;
  /** Fewest nodes (or edges) sampled from each label that has any. */
  sampleMinPerLabel: number;
  /** Full rows probed per label to infer its schema. */
  probeRowsPerLabel: number;
  /** Node and edge labels probed (each); the rest are not probed. */
  maxProbeLabels: number;
  /** Edges per direction in one neighborhood expansion. */
  neighborLimit: number;
  /** Nodes per label page. */
  pageSize: number;
  /** Most nodes the canvas holds before the UI should stop adding. */
  maxCanvasNodes: number;
  /** Hits per search source (id, each text index, each contains label). */
  searchK: number;
  /** Edges per direction when connecting new nodes to the visible ones. */
  connectEdgeLimit: number;
  /** Most property keys in one `values` projection. */
  maxShapeKeys: number;
  /** Nodes and edges the label discovery reads from the start of each id space. */
  discoveryRows: number;
  /** Node and edge labels counted per request (each); the rest stay uncounted. */
  maxCountLabels: number;
  /**
   * Neighbors per direction above which a node counts as large ("1,000+"):
   * the explorer never reads its edges on its own (degree counts, connecting
   * it to the canvas, expanding it without a warning).
   */
  degreeThreshold: number;
  /**
   * Nodes whose size one degree probe request checks. Each costs time in its
   * adjacency size (a hub with millions of neighbors takes about a second),
   * so the explorer probes a few at a time and stops at the first batch that
   * finds a large node or runs slow.
   */
  probeBatch: number;
  /**
   * Neighbor pairs (summed over the nodes, by their probes) one edge-count
   * request may look up: counting a small node's edges costs one pair lookup
   * per neighbor, at most `degreeThreshold` per side.
   */
  edgeCountBudget: number;
  /** Nodes whose edges one edge-count request counts. */
  edgeCountBatch: number;
  /**
   * Edges (summed over the new nodes, by their bounded edge counts) one
   * connect request may read: connecting reads each edge's endpoints.
   */
  connectEdgeBudget: number;
}

export const EXPLORER_LIMITS: Readonly<ExplorerLimits> = Object.freeze({
  sampleNodes: 400,
  sampleEdges: 1200,
  sampleMinPerLabel: 3,
  probeRowsPerLabel: 2,
  maxProbeLabels: 30,
  neighborLimit: 50,
  pageSize: 50,
  maxCanvasNodes: 2000,
  searchK: 10,
  connectEdgeLimit: 1000,
  maxShapeKeys: MAX_SHAPE_KEYS,
  discoveryRows: 500,
  maxCountLabels: 60,
  degreeThreshold: 1000,
  probeBatch: 10,
  edgeCountBudget: 5000,
  edgeCountBatch: 50,
  connectEdgeBudget: 5000,
});

/**
 * A probe batch slower than this (database and network time) stops the
 * explorer's own probing, and the seeding's: it likely met hubs, and the
 * next batch could too.
 */
export const PROBE_SLOW_MS = 1500;

// AST helpers. Each operation wraps its input; there is no steps array.

type Ast = { [key: string]: JsonValue };
type ParamType = JsonValue;

const I64: ParamType = "i64";
const STRING: ParamType = "string";
const I64_ARRAY: ParamType = { array: "i64" };

/** A literal stream bound; see the note above on why bounds are never parameters. */
const bound = (count: number): Ast => ({
  literal: Math.max(0, Math.floor(count)),
});
const nodesAll = (): Ast => ({ nodes: { reference: "all" } });
const edgesAll = (): Ast => ({ edges: { reference: "all" } });
const nodesVar = (name: string): Ast => ({
  nodes: { reference: { var: name } },
});
const edgesVar = (name: string): Ast => ({
  edges: { reference: { var: name } },
});
const nodesParam = (param: string): Ast => ({
  nodes: { reference: { param } },
});
const edgesParam = (param: string): Ast => ({
  edges: { reference: { param } },
});
/** A label's nodes in the scope: `$label == "<label>"` (a constant) and the scope's filter. */
const nodesIn = (scope: Scope, label: string): Ast => ({
  nodes_where: { predicate: scopedLabel(scope, "node", label) },
});
const edgesIn = (scope: Scope, label: string): Ast => ({
  edges_where: { predicate: scopedLabel(scope, "edge", label) },
});
const limit = (input: Ast, count: number): Ast => ({
  limit: { input, count: bound(count) },
});
const skip = (input: Ast, count: number): Ast => ({
  skip: { input, count: bound(count) },
});
const where = (input: Ast, predicate: Ast): Ast => ({
  where: { input, predicate },
});
const idAfter = (param: string): Ast => ({
  gt: { left: { property: "$id" }, right: { param } },
});
/** `$label` doesn't start with `prefix` (a constant, like the labels); null for none. */
const labelNotPrefixed = (prefix: string | undefined): Ast | null =>
  prefix
    ? {
        not: {
          predicate: {
            starts_with: {
              value: { property: "$label" },
              prefix: { constant: { string: prefix } },
            },
          },
        },
      }
    : null;
/** `input` where every predicate holds; the input itself when none is given. */
const whereAll = (input: Ast, predicates: (Ast | null)[]): Ast => {
  const all = predicates.filter((p): p is Ast => p !== null);
  if (all.length === 0) return input;
  return where(
    input,
    all.length === 1 ? all[0]! : { and: { predicates: all } },
  );
};
const edgeStep = (
  op: "out_e" | "in_e",
  input: JsonValue,
  label?: string | null,
): Ast => ({
  [op]: label ? { input, label } : { input },
});
const outN = (input: Ast): Ast => ({ out_n: { input } });
const inN = (input: Ast): Ast => ({ in_n: { input } });
/** Node to neighbor nodes: decodes the adjacency list (one key), no per-edge reads. */
const neighbors = (op: "out" | "in", input: Ast): Ast => ({ [op]: { input } });
const dedup = (input: Ast): Ast => ({ dedup: { input } });
const inject = (input: Ast, variable: string): Ast => ({
  inject: { input, variable },
});
const count = (input: Ast): Ast => ({ count: { input } });
const groupCount = (input: Ast, property: string): Ast => ({
  group_count: { input, property },
});
const edgeProperties = (input: Ast): Ast => ({ edge_properties: { input } });
const valueMap = (input: Ast): Ast => ({ value_map: { input } });
const values = (input: Ast, properties: string[]): Ast => ({
  values: { input, properties },
});

/**
 * `$id`, `$label`, `foundry_load` and the shape's keys, or every stored
 * property when keys is null. The load always comes along, so a node outside
 * the scope (reached over an edge someone added) is told apart.
 */
function project(input: Ast, keys: string[] | null, extra: string[] = []): Ast {
  if (keys === null) return valueMap(input);
  // The load takes one of the shape's MAX_SHAPE_KEYS slots.
  const own = keys
    .filter((key) => key !== FOUNDRY_LOAD)
    .slice(0, MAX_SHAPE_KEYS - 1);
  return values(input, [
    ...new Set(["$id", "$label", FOUNDRY_LOAD, ...extra, ...own]),
  ]);
}

/** The edge keys a shape fetches; null means every edge property. */
function edgeKeysOf(shape: NodeFetchShape | undefined): string[] | null {
  const keys = (shape as ProbedFetchShape | undefined)?.edgeKeys;
  return Array.isArray(keys) ? keys : null;
}

/**
 * Edge rows with their endpoints: every property (edge_properties), or only
 * the shape's light edge keys when some edge label carries heavy values.
 */
function projectEdges(input: Ast, shape: NodeFetchShape | undefined): Ast {
  const keys = edgeKeysOf(shape);
  if (keys === null) return edgeProperties(input);
  return values(input, [
    ...new Set(["$id", "$label", "$from", "$to", FOUNDRY_LOAD, ...keys]),
  ]);
}

interface Entry {
  name: string;
  root: Ast;
  /** Unreturned entries are transaction-local variables. */
  returned?: boolean;
}

type Params = Record<string, { value: JsonValue; type: ParamType }>;

function readRequest(
  queryName: string,
  entries: Entry[],
  params: Params = {},
): DynamicQueryRequest {
  const parameters: Record<string, JsonValue> = {};
  const parameterTypes: Record<string, JsonValue> = {};
  for (const [name, param] of Object.entries(params)) {
    parameters[name] = param.value;
    parameterTypes[name] = param.type;
  }
  return {
    request_type: "read",
    query_name: queryName,
    query: {
      read: {
        entries: entries.map((entry) => ({
          query: { name: entry.name, root: entry.root },
        })),
        returns: entries
          .filter((entry) => entry.returned !== false)
          .map((entry) => entry.name),
      },
    },
    // Always typed: a JS 1.0 serializes as 1 and would be inferred as i64.
    parameters,
    parameter_types: parameterTypes,
  };
}

const positive = (value: number | undefined, fallback: number) =>
  value !== undefined && Number.isFinite(value) && value >= 1
    ? Math.floor(value)
    : fallback;

const validIds = (ids: readonly number[]) => [
  ...new Set(ids.filter((id) => toDbId(id) !== null)),
];

// Label discovery and counts

/**
 * Node or edge ids spread over the whole id space: four per power of two
 * from 2^9 up to 2^53 (the largest integer a JSON number carries exactly).
 * Ids come from a per-database sequence, and bulk loads write one label
 * after another, so a label holding a real share of the graph owns a run of
 * ids that some probe lands in. A probe on a missing id costs one point read
 * and returns nothing.
 */
export function labelProbeIds(): number[] {
  const ids: number[] = [];
  for (let exponent = 9; exponent < 53; exponent++) {
    for (let step = 0; step < 4; step++)
      ids.push(Math.floor(2 ** (exponent + step / 4)));
  }
  return [...new Set(ids)];
}

/**
 * Which labels exist, without reading the whole graph: the labels of the
 * first `discoveryRows` nodes and edges by id (a scan that stops after that
 * many rows) and of the nodes and edges at `labelProbeIds()` (point reads).
 * Labels it misses are found later (the index catalog, labels remembered
 * from earlier visits, whatever gets loaded) or by an explicit full scan.
 * Other data only: a load's labels are its version's.
 */
export function buildLabelDiscoveryRequest(
  opts: { rows?: number } = {},
): DynamicQueryRequest {
  const rows = positive(opts.rows, EXPLORER_LIMITS.discoveryRows);
  return readRequest(
    "explorer_label_discovery",
    [
      {
        name: "first_nodes",
        root: groupCount(limit(nodesAll(), rows), "$label"),
      },
      {
        name: "first_edges",
        root: groupCount(limit(edgesAll(), rows), "$label"),
      },
      {
        name: "probed_nodes",
        root: groupCount(nodesParam("probe_ids"), "$label"),
      },
      {
        name: "probed_edges",
        root: groupCount(edgesParam("probe_ids"), "$label"),
      },
    ],
    { probe_ids: { value: labelProbeIds(), type: I64_ARRAY } },
  );
}

export interface LabelDiscovery {
  nodes: string[];
  edges: string[];
  /**
   * The first-rows read came back short, so it saw every node (or edge) and
   * the label list on that side is complete.
   */
  nodesExhausted: boolean;
  edgesExhausted: boolean;
  /** No node at all: the database is empty. */
  empty: boolean;
}

export function parseLabelDiscovery(
  body: unknown,
  rows?: number,
): LabelDiscovery {
  const data = isRecord(body) ? body : {};
  const limitRows = positive(rows, EXPLORER_LIMITS.discoveryRows);
  const seen = (value: unknown) =>
    labelCounts(value).reduce((sum, entry) => sum + (entry.count ?? 0), 0);
  const names = (...values: unknown[]) =>
    [
      ...new Set(
        values.flatMap((value) =>
          labelCounts(value).map((entry) => entry.label),
        ),
      ),
    ]
      .filter((label) => label !== "")
      .sort((a, b) => a.localeCompare(b));
  const firstNodes = Array.isArray(data.first_nodes)
    ? seen(data.first_nodes)
    : null;
  const firstEdges = Array.isArray(data.first_edges)
    ? seen(data.first_edges)
    : null;
  return {
    nodes: names(data.first_nodes, data.probed_nodes),
    edges: names(data.first_edges, data.probed_edges),
    nodesExhausted: firstNodes !== null && firstNodes < limitRows,
    edgesExhausted: firstEdges !== null && firstEdges < limitRows,
    empty: firstNodes === 0,
  };
}

/** Distinct non-empty labels in the order given. */
function uniqueLabels(list: readonly string[]): string[] {
  return [
    ...new Set(
      list.filter((label) => typeof label === "string" && label !== ""),
    ),
  ];
}

/**
 * The labels a count request covers, in order, while each side's entries fit
 * `maxCountLabels`, and the ones left out.
 */
export function countedLabels(
  labels: {
    nodes: readonly string[];
    edges: readonly string[];
  },
  scope: Scope,
): {
  nodes: string[];
  edges: string[];
  dropped: { nodes: string[]; edges: string[] };
} {
  const cap = EXPLORER_LIMITS.maxCountLabels;
  const side = (target: Target, list: readonly string[]) => {
    const all = uniqueLabels(list);
    let used = 0,
      fit = 0;
    for (const label of all) {
      const cost = countCost(scope, target, label);
      if (used + cost > cap) break;
      used += cost;
      fit += 1;
    }
    return { kept: all.slice(0, fit), dropped: all.slice(fit) };
  };
  const nodes = side("node", labels.nodes);
  const edges = side("edge", labels.edges);
  return {
    nodes: nodes.kept,
    edges: edges.kept,
    dropped: { nodes: nodes.dropped, edges: edges.dropped },
  };
}

/**
 * Node and edge counts per label in the scope, each read from bitmaps (no
 * rows): the label's and the load's intersected, or for Other data the
 * label's less every load's. At most `maxCountLabels` counts per side. With
 * `probeRows`, the same request also probes a few full rows of the first
 * `maxProbeLabels` labels (see buildSchemaProbeRequest; parseSchemaProbe
 * reads them from this response), so the first view needs one request for
 * counts and schemas.
 */
export function buildLabelCountsRequest(
  labels: { nodes: readonly string[]; edges: readonly string[] },
  opts: { scope: Scope; probeRows?: number },
): DynamicQueryRequest {
  const { scope } = opts;
  const { nodes, edges } = countedLabels(labels, scope);
  const params: Params = {};
  const entries: Entry[] = [
    ...nodes.flatMap((label, index) =>
      countEntries(scope, "node", label, `node_count_${index}`, count),
    ),
    ...edges.flatMap((label, index) =>
      countEntries(scope, "edge", label, `edge_count_${index}`, count),
    ),
  ];
  const probeRows =
    opts.probeRows === undefined
      ? 0
      : positive(opts.probeRows, EXPLORER_LIMITS.probeRowsPerLabel);
  if (probeRows > 0 && entries.length > 0)
    entries.push(...probeEntries(nodes, edges, probeRows, scope));
  if (entries.length === 0) {
    // A batch needs at least one entry: count an empty id list (no reads).
    params.no_ids = { value: [], type: I64_ARRAY };
    entries.push({ name: "none", root: count(nodesParam("no_ids")) });
  }
  return readRequest(
    probeRows > 0 ? "explorer_labels" : "explorer_label_counts",
    entries,
    params,
  );
}

/**
 * Label stats from a counts response. Labels that turn out to have nothing
 * (deleted since they were remembered, an index on an unused label, a type
 * this load holds none of) are left out; labels past the cap keep a null
 * count. Totals add up the known counts, so they are lower bounds unless
 * `complete` says every label on that side is known and every count came
 * back.
 */
export function parseLabelCounts(
  body: unknown,
  labels: { nodes: readonly string[]; edges: readonly string[] },
  scope: Scope,
  complete: { nodes: boolean; edges: boolean } = { nodes: false, edges: false },
): LabelStats {
  const data = isRecord(body) ? body : {};
  const counted = countedLabels(labels, scope);
  const side = (target: Target, list: string[], dropped: string[]) => {
    const entries: LabelCount[] = [];
    list.forEach((label, index) => {
      const value = countOf(
        data,
        scope,
        target,
        label,
        `${target}_count_${index}`,
        total,
      );
      if (value === 0) return;
      entries.push({ label, count: value });
    });
    for (const label of dropped) entries.push({ label, count: null });
    return entries.sort((a, b) => a.label.localeCompare(b.label));
  };
  const nodeLabels = side("node", counted.nodes, counted.dropped.nodes);
  const edgeLabels = side("edge", counted.edges, counted.dropped.edges);
  const sum = (entries: LabelCount[]) =>
    entries.reduce((acc, entry) => acc + (entry.count ?? 0), 0);
  const unknown = (entries: LabelCount[]) =>
    entries.some((entry) => entry.count === null);
  return {
    nodeLabels,
    edgeLabels,
    nodeTotal: sum(nodeLabels),
    edgeTotal: sum(edgeLabels),
    partial: {
      nodes: !complete.nodes || unknown(nodeLabels),
      edges: !complete.edges || unknown(edgeLabels),
    },
  };
}

/**
 * Folds counts for more labels (seen after the boot) into existing stats.
 * Totals grow by what the new labels add; they stay lower bounds. `gone`
 * names labels those counts found empty (remembered from an earlier visit
 * and deleted since, say): an uncounted entry for one of them is dropped.
 */
export function mergeLabelStats(
  stats: LabelStats,
  more: LabelStats,
  gone: { nodes?: readonly string[]; edges?: readonly string[] } = {},
): LabelStats {
  const merge = (
    current: LabelCount[],
    extra: LabelCount[],
    empty: readonly string[] = [],
  ) => {
    const byLabel = new Map(current.map((entry) => [entry.label, entry]));
    for (const label of empty)
      if (byLabel.get(label)?.count === null) byLabel.delete(label);
    for (const entry of extra) {
      const existing = byLabel.get(entry.label);
      if (!existing || existing.count === null) byLabel.set(entry.label, entry);
    }
    return [...byLabel.values()].sort((a, b) => a.label.localeCompare(b.label));
  };
  const nodeLabels = merge(stats.nodeLabels, more.nodeLabels, gone.nodes);
  const edgeLabels = merge(stats.edgeLabels, more.edgeLabels, gone.edges);
  const sum = (entries: LabelCount[]) =>
    entries.reduce((acc, entry) => acc + (entry.count ?? 0), 0);
  return {
    nodeLabels,
    edgeLabels,
    nodeTotal: sum(nodeLabels),
    edgeTotal: sum(edgeLabels),
    partial: {
      nodes:
        (stats.partial?.nodes ?? false) ||
        nodeLabels.some((entry) => entry.count === null),
      edges:
        (stats.partial?.edges ?? false) ||
        edgeLabels.some((entry) => entry.count === null),
    },
  };
}

/** The labels a counts response found empty (each side), so they can be forgotten. */
export function emptyLabels(
  body: unknown,
  labels: { nodes: readonly string[]; edges: readonly string[] },
  scope: Scope,
): { nodes: string[]; edges: string[] } {
  const data = isRecord(body) ? body : {};
  const counted = countedLabels(labels, scope);
  const side = (target: Target, list: string[]) =>
    list.filter(
      (label, index) =>
        countOf(
          data,
          scope,
          target,
          label,
          `${target}_count_${index}`,
          total,
        ) === 0,
    );
  return {
    nodes: side("node", counted.nodes),
    edges: side("edge", counted.edges),
  };
}

// Full label scan (explicit only)

/**
 * Every label with its exact count, and exact totals: `group_count` and
 * `count` over every node and edge. This reads the whole graph (seconds to
 * minutes on a large database, and heavy on its CPU), so the explorer runs
 * it only when the user asks ("Scan for all labels"), never on its own, and
 * only for Other data: its counts include the loads', so the labels it finds
 * are counted again in the scope.
 */
export function buildLabelScanRequest(): DynamicQueryRequest {
  return readRequest("explorer_label_scan", [
    { name: "node_labels", root: groupCount(nodesAll(), "$label") },
    { name: "edge_labels", root: groupCount(edgesAll(), "$label") },
    { name: "node_total", root: count(nodesAll()) },
    { name: "edge_total", root: count(edgesAll()) },
  ]);
}

function labelCounts(value: unknown): LabelCount[] {
  const result: LabelCount[] = [];
  for (const row of rowsOf(value)) {
    if (!isRecord(row) || typeof row.$label !== "string") continue;
    const n = row.count;
    result.push({
      label: row.$label,
      count: typeof n === "number" && Number.isFinite(n) && n >= 0 ? n : null,
    });
  }
  return result.sort((a, b) => a.label.localeCompare(b.label));
}

const total = (value: unknown) =>
  typeof value === "number" && Number.isFinite(value) && value >= 0
    ? value
    : null;

/** Exact stats from a full label scan: every label, so no `partial`. */
export function parseLabelScan(body: unknown): LabelStats {
  const data = isRecord(body) ? body : {};
  return {
    nodeLabels: labelCounts(data.node_labels),
    edgeLabels: labelCounts(data.edge_labels),
    nodeTotal: total(data.node_total),
    edgeTotal: total(data.edge_total),
  };
}

// Schema probe

/**
 * Full rows (`rows` per label, a literal bound) of the first `maxProbeLabels`
 * node and edge labels. Shared by the probe and the counts request.
 */
function probeEntries(
  nodes: readonly string[],
  edges: readonly string[],
  rows: number,
  scope: Scope,
): Entry[] {
  const cap = EXPLORER_LIMITS.maxProbeLabels;
  return [
    ...nodes.slice(0, cap).map((label, index) => ({
      name: `node_${index}`,
      root: valueMap(limit(nodesIn(scope, label), rows)),
    })),
    ...edges.slice(0, cap).map((label, index) => ({
      name: `edge_${index}`,
      root: edgeProperties(limit(edgesIn(scope, label), rows)),
    })),
  ];
}

/**
 * A couple of full rows per label. These are the only routine fetches that
 * include heavy values, so the row count stays small (pass rowsPerLabel: 1
 * to retry after a too_large failure). buildLabelCountsRequest can carry the
 * same probe next to the label counts.
 */
export function buildSchemaProbeRequest(
  labels: { nodes: readonly string[]; edges: readonly string[] },
  opts: { scope: Scope; rowsPerLabel?: number },
): DynamicQueryRequest {
  const cap = EXPLORER_LIMITS.maxProbeLabels;
  const nodes = uniqueLabels(labels.nodes).slice(0, cap);
  const edges = uniqueLabels(labels.edges).slice(0, cap);
  const params: Params = {};
  const entries = probeEntries(
    nodes,
    edges,
    positive(opts.rowsPerLabel, EXPLORER_LIMITS.probeRowsPerLabel),
    opts.scope,
  );
  if (entries.length === 0) {
    // A batch needs at least one entry: count an empty id list (no reads).
    params.no_ids = { value: [], type: I64_ARRAY };
    entries.push({ name: "none", root: count(nodesParam("no_ids")) });
  }
  return readRequest("explorer_schema_probe", entries, params);
}

function returnedRows(body: unknown, prefix: string): unknown[] {
  if (!isRecord(body)) return [];
  const pattern = new RegExp(`^${prefix}_\\d+$`);
  return Object.entries(body)
    .filter(([key]) => pattern.test(key))
    .flatMap(([, value]) => rowsOf(value));
}

/** Sorts keys by how often the probe saw them (then by name) and caps the list. */
function rankKeys(
  seenByKey: Map<string, number>,
  exclude: ReadonlySet<string>,
): string[] {
  return [...seenByKey.entries()]
    .filter(([key]) => !exclude.has(key))
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .slice(0, EXPLORER_LIMITS.maxShapeKeys)
    .map(([key]) => key);
}

/**
 * The edge keys to project once some edge label carries heavy values
 * (vectors, long text): the light keys seen on any edge label. Null when no
 * edge value is heavy, so edges keep every property, including sparse keys
 * the probe rows lacked.
 */
function edgeFetchKeys(edgeSchemas: readonly LabelSchema[]): string[] | null {
  const heavy = new Set<string>();
  const seenByKey = new Map<string, number>();
  for (const schema of edgeSchemas) {
    for (const property of schema.properties) {
      if (property.heavy) heavy.add(property.key);
      else
        seenByKey.set(
          property.key,
          (seenByKey.get(property.key) ?? 0) + property.seen + 1,
        );
    }
  }
  return heavy.size === 0 ? null : rankKeys(seenByKey, heavy);
}

/**
 * Label schemas from the probe rows plus the index catalog, and the fetch
 * shape every later node request uses: the light keys of all node labels
 * (a key heavy on any label is left out everywhere, since one `values` list
 * serves mixed-label streams), plus per-label maps for label-scoped requests
 * and the edge keys to fetch when some edge values are heavy.
 */
export function parseSchemaProbe(
  body: unknown,
  labels: { nodes: string[]; edges: string[] },
  indexes?: IndexCatalogEntry[],
): { schemas: LabelSchema[]; shape: NodeFetchShape } {
  const catalog = indexesByProperty(indexes);
  const nodeRows = groupRowsByLabel(returnedRows(body, "node"));
  const edgeRows = groupRowsByLabel(returnedRows(body, "edge"));

  const build = (
    target: "node" | "edge",
    label: string,
    rows: Record<string, unknown>[],
  ) => {
    const builder = new LabelSchemaBuilder(target, label);
    for (const row of rows) builder.addRow(row);
    return builder.build(catalog.get(`${target}:${label}`));
  };
  const nodeLabels = [...new Set([...labels.nodes, ...nodeRows.keys()])];
  const edgeLabels = [...new Set([...labels.edges, ...edgeRows.keys()])];
  const nodeSchemas = nodeLabels.map((label) =>
    build("node", label, nodeRows.get(label) ?? []),
  );
  const edgeSchemas = edgeLabels.map((label) =>
    build("edge", label, edgeRows.get(label) ?? []),
  );
  const schemas = [...nodeSchemas, ...edgeSchemas];

  if (nodeRows.size === 0) {
    // Nothing probed (empty database or an unreadable body): fetch everything.
    return { schemas, shape: fallbackShape() };
  }

  const heavy = new Set<string>();
  const heavyByLabel: Record<string, string[]> = {};
  const keysByLabel: Record<string, string[]> = {};
  const seenByKey = new Map<string, number>();
  for (const schema of nodeSchemas) {
    // A label whose probe came back empty is treated as unprobed.
    const wasProbed = nodeRows.has(schema.label);
    const heavyKeys = schema.properties
      .filter((p) => p.heavy)
      .map((p) => p.key);
    heavyKeys.forEach((key) => heavy.add(key));
    // Unprobed labels only know their catalog vectors; leave them out of the
    // per-label maps unless the catalog says something.
    if (wasProbed || heavyKeys.length)
      heavyByLabel[schema.label] = heavyKeys.sort();
    if (!wasProbed) continue;
    keysByLabel[schema.label] = schema.properties
      .filter((p) => !p.heavy)
      .map((p) => p.key)
      .slice(0, EXPLORER_LIMITS.maxShapeKeys);
    for (const property of schema.properties) {
      if (property.heavy) continue;
      seenByKey.set(
        property.key,
        (seenByKey.get(property.key) ?? 0) + property.seen + 1,
      );
    }
  }
  const keys = rankKeys(seenByKey, heavy);
  const edgeKeys = edgeFetchKeys(edgeSchemas);
  const shape: ProbedFetchShape = {
    keys,
    heavyKeys: [...heavy].sort(),
    heavyByLabel,
    keysByLabel,
    ...(edgeKeys ? { edgeKeys } : {}),
  };
  return { schemas, shape };
}

/** Fetch every property (value_map). Used until a probe succeeds. */
export function fallbackShape(): NodeFetchShape {
  return { keys: null, heavyKeys: [] };
}

/**
 * Only `$id` and `$label` for nodes, and only ids, labels and endpoints for
 * edges: the retry when a response with properties is too large. It keeps the
 * shape's per-label knowledge, so each node lists its label's keys as
 * omitted (the inspector then offers to load them).
 */
export function lightShape(shape: NodeFetchShape): NodeFetchShape {
  const probed = shape as ProbedFetchShape;
  const light: ProbedFetchShape = {
    keys: [],
    heavyKeys: shape.heavyKeys,
    edgeKeys: [],
    ...(probed.heavyByLabel ? { heavyByLabel: probed.heavyByLabel } : {}),
    ...(probed.keysByLabel ? { keysByLabel: probed.keysByLabel } : {}),
  };
  return light;
}

/** True when a shape already fetches no node or edge properties. */
export function isLightShape(shape: NodeFetchShape): boolean {
  return shape.keys?.length === 0 && edgeKeysOf(shape)?.length === 0;
}

/** Whether a shape fetches any edge properties (every one, or its light edge keys). */
export function fetchesEdgeProperties(shape: NodeFetchShape): boolean {
  const keys = edgeKeysOf(shape);
  return keys === null || keys.length > 0;
}

/**
 * The same node fetch with edges reduced to ids, labels and endpoints: the
 * retry when edge values the probe didn't see (an unprobed label, a sparse
 * long value) make a response too large.
 */
export function withoutEdgeProperties(shape: NodeFetchShape): NodeFetchShape {
  const light: ProbedFetchShape = {
    ...(shape as ProbedFetchShape),
    edgeKeys: [],
  };
  return light;
}

/**
 * Grows a probed shape with what fully loaded nodes (inspector "load full
 * node") reveal: sparse keys the probe rows lacked, and values that turn out
 * heavy. Returns the same object when nothing changes.
 */
export function extendShape(
  shape: NodeFetchShape,
  nodes: readonly GraphNode[],
): NodeFetchShape {
  if (shape.keys === null) return shape;
  const probed = shape as ProbedFetchShape;
  const keys = new Set(shape.keys);
  const heavy = new Set(shape.heavyKeys);
  const heavyByLabel: Record<string, string[]> = { ...probed.heavyByLabel };
  const keysByLabel: Record<string, string[]> = { ...probed.keysByLabel };
  let changed = false;
  for (const node of nodes) {
    if (!node.complete) continue;
    const labelHeavy = new Set(heavyByLabel[node.label] ?? []);
    const labelKeys = new Set(keysByLabel[node.label] ?? []);
    for (const [key, value] of Object.entries(node.properties)) {
      if (isHeavyValue(key, value)) {
        if (labelHeavy.has(key) && heavy.has(key)) continue;
        heavy.add(key);
        keys.delete(key);
        labelHeavy.add(key);
        labelKeys.delete(key);
        changed = true;
      } else if (!labelHeavy.has(key) && !labelKeys.has(key)) {
        labelKeys.add(key);
        if (!heavy.has(key)) keys.add(key);
        changed = true;
      }
    }
    heavyByLabel[node.label] = [...labelHeavy].sort();
    keysByLabel[node.label] = [...labelKeys];
  }
  if (!changed) return shape;
  const result: ProbedFetchShape = {
    keys: [...keys].slice(0, EXPLORER_LIMITS.maxShapeKeys),
    heavyKeys: [...heavy].sort(),
    heavyByLabel,
    keysByLabel,
    ...(probed.edgeKeys !== undefined ? { edgeKeys: probed.edgeKeys } : {}),
  };
  return result;
}

// First view

/**
 * Seeds per label for the first view: an even split of the seed budget,
 * smallest labels first so their leftovers go to the larger ones, at least
 * `sampleMinPerLabel` each. With more labels than the budget covers, the
 * largest labels win. A label without a count (over the count cap, say, or
 * remembered from an earlier visit and maybe deleted since) is unknown, not
 * huge: it comes after every counted label and gets the minimum.
 */
export function allocateSample(
  labels: LabelCount[],
  seedBudget: number,
): Map<string, number> {
  const minimum = EXPLORER_LIMITS.sampleMinPerLabel;
  const byName = (a: LabelCount, b: LabelCount) =>
    a.label.localeCompare(b.label);
  const usable = labels.filter(
    (label) => label.count !== 0 && label.label !== "",
  );
  const known = usable.filter((label) => label.count !== null);
  const unknown = usable.filter((label) => label.count === null).sort(byName);
  const size = (label: LabelCount) => label.count ?? 0;
  const chosen = [
    ...known.sort((a, b) => size(b) - size(a) || byName(a, b)),
    ...unknown,
  ].slice(0, Math.max(1, Math.floor(seedBudget / minimum)));
  const result = new Map<string, number>();
  let remaining = seedBudget;
  const unknownChosen = chosen.filter((label) => label.count === null);
  for (const label of unknownChosen) {
    result.set(label.label, minimum);
    remaining = Math.max(0, remaining - minimum);
  }
  const knownChosen = chosen
    .filter((label) => label.count !== null)
    .sort((a, b) => size(a) - size(b) || byName(a, b));
  knownChosen.forEach((label, index) => {
    const fair = Math.max(
      minimum,
      Math.floor(remaining / (knownChosen.length - index)),
    );
    const take = Math.min(size(label), fair);
    result.set(label.label, take);
    remaining = Math.max(0, remaining - take);
  });
  // Keep the input's label order stable for request building: known labels by size, then unknown.
  return new Map(
    [...knownChosen, ...unknownChosen].map((label) => [
      label.label,
      result.get(label.label) ?? minimum,
    ]),
  );
}

/**
 * The first view of Other data (a load's is seeded from its hub types:
 * ./seeding.ts): a few nodes of every node label (seeds, so each label
 * shows) and a few edges of every edge label with the nodes on both ends (so
 * the view is connected), every source scoped. Never the whole graph: the
 * scope is a part of the database, so a label's count says nothing of the
 * rest, and no label means no rows.
 *
 * Edges come straight off the edge label bitmaps rather than from expanding
 * the seeds: reading one node's edges costs time in its degree whatever the
 * limit, so a supernode among the seeds (often a label's first node, like a
 * "general" tag) made the first view as slow as its degree (a 67k-edge seed
 * took ~1 s of a 1.2 s sample on 1M nodes). Every read here is a literal
 * limit over a bitmap, so the cost depends on the budgets only, not on the
 * graph's size or degrees.
 */
export function buildSampleRequest(opts: {
  scope: Scope;
  nodeLabels: LabelCount[] | null;
  /** Edge labels with counts; null or absent samples no edges. */
  edgeLabels?: LabelCount[] | null;
  shape: NodeFetchShape;
  nodeBudget?: number;
  edgeBudget?: number;
}): DynamicQueryRequest {
  const { scope } = opts;
  const nodeBudget = positive(opts.nodeBudget, EXPLORER_LIMITS.sampleNodes);
  const edgeBudget = positive(opts.edgeBudget, EXPLORER_LIMITS.sampleEdges);
  const keys = opts.shape.keys;
  const params: Params = {};
  const none = (): string => {
    // No label: an empty id list stands in (no reads).
    params.no_ids = { value: [], type: I64_ARRAY };
    return "no_ids";
  };

  // A fifth of the nodes are seeds; each sampled edge brings at most two
  // more, so seeds plus endpoints never exceed the node budget.
  const seedBudget = Math.max(1, Math.floor(nodeBudget / 5));
  const edgeTake = Math.max(
    1,
    Math.min(edgeBudget, Math.floor((nodeBudget - seedBudget) / 2)),
  );
  const entries: Entry[] = [];

  const seeds = opts.nodeLabels
    ? allocateSample(opts.nodeLabels, seedBudget)
    : new Map<string, number>();
  [...seeds.entries()].forEach(([label, take], index) => {
    entries.push({
      name: `seed_${index}`,
      root: limit(nodesIn(scope, label), take),
      returned: false,
    });
  });
  let seedRoot = seeds.size > 0 ? nodesVar("seed_0") : nodesParam(none());
  for (let index = 1; index < seeds.size; index++)
    seedRoot = inject(seedRoot, `seed_${index}`);
  entries.push({ name: "seeds", root: dedup(seedRoot), returned: false });

  const edgeSeeds = opts.edgeLabels
    ? allocateSample(opts.edgeLabels, edgeTake)
    : new Map<string, number>();
  [...edgeSeeds.entries()].forEach(([label, take], index) => {
    entries.push({
      name: `edge_seed_${index}`,
      root: limit(edgesIn(scope, label), take),
      returned: false,
    });
  });
  let edgeRoot =
    edgeSeeds.size > 0 ? edgesVar("edge_seed_0") : edgesParam(none());
  for (let index = 1; index < edgeSeeds.size; index++)
    edgeRoot = inject(edgeRoot, `edge_seed_${index}`);
  entries.push({
    name: "sampled_edges",
    root: dedup(edgeRoot),
    returned: false,
  });

  entries.push(
    { name: "targets", root: outN(edgesVar("sampled_edges")), returned: false },
    { name: "sources", root: inN(edgesVar("sampled_edges")), returned: false },
    {
      name: "nodes",
      // Endpoints first, so the node budget never cuts an edge's ends off.
      root: project(
        limit(
          dedup(inject(inject(nodesVar("sources"), "targets"), "seeds")),
          nodeBudget,
        ),
        keys,
      ),
    },
    {
      name: "edges",
      root: projectEdges(edgesVar("sampled_edges"), opts.shape),
    },
  );
  return readRequest("explorer_sample", entries, params);
}

/** Drops edges whose endpoints are not among `nodes` (the canvas cannot draw them). */
function keepConnected(nodes: GraphNode[], edges: GraphEdge[]): GraphEdge[] {
  const ids = new Set(nodes.map((node) => node.id));
  return edges.filter((edge) => ids.has(edge.source) && ids.has(edge.target));
}

export function parseSample(
  body: unknown,
  shape: NodeFetchShape,
): GraphData & { truncated: boolean } {
  const data = isRecord(body) ? body : {};
  const nodes = toGraphNodes(data.nodes, shape);
  const edges = toGraphEdges(data.edges);
  const connected = keepConnected(nodes, edges);
  // A sample never proves nothing was left out.
  return { nodes, edges: connected, truncated: true };
}

// Label pages

/**
 * One page of a label, ascending by id (label scans iterate a bitmap, so id
 * order is stable). Fetches pageSize + 1 rows to know whether another page
 * exists; the label total (a bitmap count) comes with the first page only.
 *
 * Later pages skip the `offset` rows already paged past rather than filter
 * on `$id > afterDbId`: a filter in front of a limit reads the whole label
 * before the limit applies (1.1 s for a 100k-node label, every page), while
 * `skip` stops after offset + pageSize rows. New nodes get larger ids, so
 * they only ever add pages at the end; a node deleted from earlier pages
 * shifts the rest back, so a page may skip that many nodes. parseLabelPage
 * drops rows at or below `afterDbId`, so nothing repeats.
 */
export function buildLabelPageRequest(opts: {
  scope: Scope;
  label: string;
  /** The last id of the previous page; null for the first page. */
  afterDbId: number | null;
  /** Rows of this label paged past already (the sum of earlier page sizes). */
  offset?: number;
  pageSize?: number;
  shape: NodeFetchShape;
}): DynamicQueryRequest {
  const pageSize = positive(opts.pageSize, EXPLORER_LIMITS.pageSize);
  const first = opts.afterDbId === null;
  const offset = first ? 0 : Math.max(0, Math.floor(opts.offset ?? 0));
  const source = nodesIn(opts.scope, opts.label);
  const rows = offset > 0 ? skip(source, offset) : source;
  const entries: Entry[] = [
    {
      name: "page",
      root: project(
        limit(rows, pageSize + 1),
        keysForLabel(opts.shape, opts.label),
      ),
    },
  ];
  // The total is a bitmap count, except for Other data under a label the
  // loads share (no index finds a missing load): the rail's count stands.
  const indexed =
    opts.scope.kind === "load" || !isShared(opts.scope, "node", opts.label);
  if (first && indexed) entries.push({ name: "total", root: count(source) });
  return readRequest("explorer_label_page", entries);
}

export function parseLabelPage(
  body: unknown,
  shape: NodeFetchShape,
  pageSize?: number,
  afterDbId: number | null = null,
): {
  nodes: GraphNode[];
  nextAfter: number | null;
  total: number | null;
  consumed: number;
} {
  const data = isRecord(body) ? body : {};
  const size = positive(pageSize, EXPLORER_LIMITS.pageSize);
  // Label pages fetch the label's own keys (keysForLabel).
  const rows = toGraphNodes(data.page, shape, { scoped: true });
  const pageRows = rows.slice(0, size);
  const hasMore = rows.length > size;
  return {
    nodes:
      afterDbId === null
        ? pageRows
        : pageRows.filter((node) => node.dbId > afterDbId),
    nextAfter:
      hasMore && pageRows.length ? pageRows[pageRows.length - 1].dbId : null,
    total: total(data.total),
    /** Rows this page moved past: what the next page's offset grows by. */
    consumed: pageRows.length,
  };
}

// Neighborhood

/**
 * Edges around one node, out and in separately, each capped at limit + 1 (the
 * extra row only tells the parser more exist), with the nodes on their other
 * end. An edge label narrows the expansion; afterEdgeDbId pages it by edge id.
 *
 * Reading a node's edges costs time in its degree whatever the limit (the
 * database collects them in edge id order first), so a supernode makes
 * every pass over its edges slow: the explorer expands a node it knows to be
 * large only after a warning, and never counts its degree here (that is the
 * degree probe's job, which reads no edges).
 */
export function buildNeighborhoodRequest(opts: {
  dbId: number;
  direction?: "both" | "out" | "in";
  edgeLabel?: string | null;
  limit?: number;
  afterEdgeDbId?: number | null;
  shape: NodeFetchShape;
  /**
   * Edge labels starting with this are left out in the read itself (types.ts
   * LabelExclusion), so a page holds only edges the canvas shows. It filters
   * in front of the limit on purpose, on every page: the read then takes in
   * every edge on the side before the limit applies (see the note at the
   * top).
   */
  excludeEdgePrefix?: string;
}): DynamicQueryRequest {
  const direction = opts.direction ?? "both";
  const edgeLabel = opts.edgeLabel || null;
  const firstPage =
    opts.afterEdgeDbId === null || opts.afterEdgeDbId === undefined;
  const keys = opts.shape.keys;
  const pageLimit = positive(opts.limit, EXPLORER_LIMITS.neighborLimit) + 1;
  const center = nodesVar("center");
  const entries: Entry[] = [
    { name: "center", root: nodesParam("node_id"), returned: false },
    { name: "node", root: project(center, keys) },
  ];
  const sides = [
    { op: "out_e", prefix: "out", other: outN, enabled: direction !== "in" },
    { op: "in_e", prefix: "in", other: inN, enabled: direction !== "out" },
  ] as const;
  for (const side of sides) {
    if (side.enabled) {
      const rows = `${side.prefix}_rows`;
      entries.push(
        {
          name: rows,
          // A filter in front of a limit reads the whole input before the limit
          // applies, so the first page has no cursor filter. The exclusion
          // filter is there on every page while edges are hidden, on purpose:
          // a page isn't used up by them, at the cost of reading every edge
          // on the side.
          root: limit(
            whereAll(edgeStep(side.op, center, edgeLabel), [
              firstPage ? null : idAfter("after"),
              labelNotPrefixed(opts.excludeEdgePrefix),
            ]),
            pageLimit,
          ),
          returned: false,
        },
        {
          name: `${side.prefix}_edges`,
          root: projectEdges(edgesVar(rows), opts.shape),
        },
        {
          name: `${side.prefix}_nodes`,
          root: project(dedup(side.other(edgesVar(rows))), keys),
        },
      );
    }
  }
  return readRequest("explorer_neighborhood", entries, {
    node_id: { value: toDbId(opts.dbId) ?? -1, type: I64 },
    ...(firstPage
      ? {}
      : { after: { value: toDbId(opts.afterEdgeDbId) ?? -1, type: I64 } }),
  });
}

export function parseNeighborhood(
  body: unknown,
  dbId: number,
  shape: NodeFetchShape,
  opts: { limit?: number } = {},
): NeighborhoodResult {
  const data = isRecord(body) ? body : {};
  const max = positive(opts.limit, EXPLORER_LIMITS.neighborLimit);
  const center =
    toGraphNodes(data.node, shape).find((node) => node.dbId === dbId) ?? null;
  // Expansions don't count the degree (see buildNeighborhoodRequest).
  if (!center)
    return {
      center: null,
      nodes: [],
      edges: [],
      degree: null,
      truncated: false,
    };
  const outRows = rowsOf(data.out_edges);
  const inRows = rowsOf(data.in_edges);
  const truncated = outRows.length > max || inRows.length > max;
  // A self-loop is both an out and an in edge; toGraphEdges keeps it once.
  const edges = toGraphEdges(outRows.slice(0, max), inRows.slice(0, max));
  const referenced = new Set(
    edges.flatMap((edge) => [edge.source, edge.target]),
  );
  referenced.delete(center.id);
  const nodes = toGraphNodes(
    [...rowsOf(data.out_nodes), ...rowsOf(data.in_nodes)],
    shape,
  ).filter((node) => referenced.has(node.id));
  return { center, nodes, edges, degree: null, truncated };
}

// Degree

/**
 * How many neighbors a node has per direction, up to `degreeThreshold` + 1:
 * `count(limit(out(node), 1001))` and its `in` twin, per node. `out`/`in`
 * decode the node's adjacency list (one key) and read nothing per edge, so
 * this stays cheap on a hub (about 20 ms for 61k neighbors), unlike any read
 * of its edges. Still, the whole list is decoded before the limit applies,
 * so a node costs time in its neighbor count: up to `probeBatch` nodes per
 * request (a batch of hubs with millions of neighbors each takes seconds).
 * It counts distinct neighbors, a floor on the edge count: over the
 * threshold the node certainly has more edges than that, while under it
 * parallel edges may still make it large (see buildEdgeCountRequest). The
 * bound is a literal (see the note at the top). Each id is a literal in its
 * own entry (`{ ids: [id] }`): with one parameter per node, a 120-node probe
 * spent about as long binding parameters as reading (41 ms against 24 ms on
 * the local engine).
 */
export function buildDegreeProbeRequest(opts: {
  dbIds: number[];
}): DynamicQueryRequest {
  const ids = validIds(opts.dbIds).slice(0, EXPLORER_LIMITS.probeBatch);
  return readRequest("explorer_degree_probe", probeDegreeEntries(ids));
}

const PROBE_CAP = () => EXPLORER_LIMITS.degreeThreshold + 1;

function probeDegreeEntries(ids: readonly number[]): Entry[] {
  return ids.flatMap((id, index) => {
    const node: Ast = { nodes: { reference: { ids: [id] } } };
    return [
      {
        name: `probe_out_${index}`,
        root: count(limit(neighbors("out", node), PROBE_CAP())),
      },
      {
        name: `probe_in_${index}`,
        root: count(limit(neighbors("in", node), PROBE_CAP())),
      },
    ];
  });
}

/**
 * Neighbors per direction from a probe, capped at `degreeThreshold` + 1 (so
 * a value over the threshold means "more than that"). Nodes whose counts are
 * missing from the response are left out: their size stays unknown.
 */
export function parseDegreeProbe(
  body: unknown,
  dbIds: readonly number[],
): Map<number, DegreeProbe> {
  const data = isRecord(body) ? body : {};
  const ids = validIds(dbIds).slice(0, EXPLORER_LIMITS.probeBatch);
  const result = new Map<number, DegreeProbe>();
  ids.forEach((id, index) => {
    const out = total(data[`probe_out_${index}`]);
    const inbound = total(data[`probe_in_${index}`]);
    if (out === null || inbound === null) return;
    result.set(id, { out, in: inbound });
  });
  return result;
}

/**
 * Whether a probe says a side (or either side) has more relationships than
 * the threshold: more neighbors, or (once counted) more edges.
 */
export function isLargeProbe(probe: DegreeProbe, side?: "out" | "in"): boolean {
  const over = (name: "out" | "in") =>
    probe[name] > EXPLORER_LIMITS.degreeThreshold ||
    (probe.edges?.[name] ?? 0) > EXPLORER_LIMITS.degreeThreshold;
  return side ? over(side) : over("out") || over("in");
}

/**
 * How many edges a node has per direction, up to `degreeThreshold` + 1:
 * `count(limit(out_e(node), 1001))` and its `in_e` twin, per node, ids as
 * literals like the degree probe. Only for nodes the probe found small: the
 * engine looks up the edges of each neighbor pair (one lookup per neighbor,
 * so at most `degreeThreshold` per side) and reads nothing per edge, so this
 * is bounded where the connect it gates is not (connecting reads each edge's
 * endpoints, and parallel edges can make a node with few neighbors have
 * hundreds of thousands of edges). Never send it for a node of unknown or
 * large size: on a hub it costs one lookup per neighbor, millions of them.
 *
 * Residual cost (an engine limitation, not fixable here): the engine's
 * pair expansion (`expand_any_edge_ids`, helix-db `access/expand.rs`)
 * gathers every edge id of a neighbor pair into a set before the downstream
 * `limit(…, 1001)` applies. So a node with few neighbors but millions of
 * parallel edges to them passes the neighbor probe as small, and this count
 * (sent on its own by automatic connects) still costs time and memory in its
 * parallel-edge count: seconds and an id set of hundreds of MB on the worst
 * nodes. The fix belongs in the engine: stop pair expansion once a
 * downstream limit (or a count over one) is met. The only client-side
 * alternative is to never count edges on its own, which means no automatic
 * connect, so the explorer accepts this exception.
 */
export function buildEdgeCountRequest(opts: {
  dbIds: number[];
}): DynamicQueryRequest {
  const ids = validIds(opts.dbIds).slice(0, EXPLORER_LIMITS.edgeCountBatch);
  const entries: Entry[] = ids.flatMap((id, index) => {
    const node: Ast = { nodes: { reference: { ids: [id] } } };
    return [
      {
        name: `edges_out_${index}`,
        root: count(limit(edgeStep("out_e", node), PROBE_CAP())),
      },
      {
        name: `edges_in_${index}`,
        root: count(limit(edgeStep("in_e", node), PROBE_CAP())),
      },
    ];
  });
  return readRequest("explorer_edge_count", entries);
}

/** Edges per direction from an edge-count response; nodes whose counts are missing are left out. */
export function parseEdgeCount(
  body: unknown,
  dbIds: readonly number[],
): Map<number, { out: number; in: number }> {
  const data = isRecord(body) ? body : {};
  const ids = validIds(dbIds).slice(0, EXPLORER_LIMITS.edgeCountBatch);
  const result = new Map<number, { out: number; in: number }>();
  ids.forEach((id, index) => {
    const out = total(data[`edges_out_${index}`]);
    const inbound = total(data[`edges_in_${index}`]);
    if (out === null || inbound === null) return;
    result.set(id, { out, in: inbound });
  });
  return result;
}

/**
 * A node's edges per direction and edge label (the per-label counts add up to
 * the degree). `bounded` (the default) reads at most `degreeThreshold` + 1
 * edges per side: it is what the explorer runs on its own, and only for sides
 * the probe found small, so it costs at most ~1,000 pair lookups and edge
 * reads per side. Without `bounded` it reads every edge (the explicit "Count
 * relationships", whose cost grows with the node's degree).
 *
 * Residual cost of the bounded form (sent on its own when a node is
 * selected): like buildEdgeCountRequest, the engine expands each neighbor
 * pair's edge ids in full (`expand_any_edge_ids`, helix-db
 * `access/expand.rs`) before `limit(…, 1001)` applies, so a side with few
 * neighbors and millions of parallel edges costs time and memory in that
 * parallel-edge count, not in the bound. The fix is engine-side: stop pair
 * expansion once the downstream limit is met.
 */
export function buildDegreeRequest(opts: {
  dbId: number;
  sides?: readonly ("out" | "in")[];
  bounded?: boolean;
  /**
   * Edge labels starting with this are left out of the counts, as
   * buildNeighborhoodRequest leaves them out. The filter sits in front of
   * the bounded form's limit, so it then reads every edge on a side, not at
   * most `degreeThreshold` + 1 (the explorer still sends it only for sides
   * the probe found small).
   */
  excludeEdgePrefix?: string;
}): DynamicQueryRequest {
  const bounded = opts.bounded !== false;
  const sides = opts.sides ?? ["out", "in"];
  const center = nodesVar("center");
  const entries: Entry[] = [
    { name: "center", root: nodesParam("node_id"), returned: false },
  ];
  for (const side of ["out", "in"] as const) {
    if (!sides.includes(side)) continue;
    const edges = whereAll(
      edgeStep(side === "out" ? "out_e" : "in_e", center),
      [labelNotPrefixed(opts.excludeEdgePrefix)],
    );
    entries.push({
      name: `${side}_by_label`,
      root: groupCount(bounded ? limit(edges, PROBE_CAP()) : edges, "$label"),
    });
  }
  return readRequest(
    bounded ? "explorer_degree" : "explorer_degree_full",
    entries,
    {
      node_id: { value: toDbId(opts.dbId) ?? -1, type: I64 },
    },
  );
}

/**
 * The degree from a degree response. A side whose bounded count reached the
 * cap is a floor: `degreeThreshold` with `atLeast` set and no per-label
 * counts (the first 1,001 edges' labels would mislead). A side the request
 * left out is 0 when the probe found no neighbors there, and a floor
 * otherwise (the probe found it large).
 */
export function parseDegree(
  body: unknown,
  opts: {
    bounded?: boolean;
    sides?: readonly ("out" | "in")[];
    probe?: DegreeProbe;
  } = {},
): DegreeInfo {
  const data = isRecord(body) ? body : {};
  const bounded = opts.bounded !== false;
  const sides = opts.sides ?? ["out", "in"];
  const threshold = EXPLORER_LIMITS.degreeThreshold;
  const side = (name: "out" | "in") => {
    if (!sides.includes(name)) {
      if (opts.probe?.[name] === 0)
        return { degree: 0, byLabel: [] as LabelCount[], atLeast: false };
      return { degree: threshold, byLabel: [] as LabelCount[], atLeast: true };
    }
    const byLabel = labelCounts(data[`${name}_by_label`]);
    const degree = byLabel.reduce((acc, entry) => acc + (entry.count ?? 0), 0);
    if (bounded && degree > threshold)
      return { degree: threshold, byLabel: [] as LabelCount[], atLeast: true };
    return { degree, byLabel, atLeast: false };
  };
  const out = side("out");
  const inbound = side("in");
  return {
    outDegree: out.degree,
    inDegree: inbound.degree,
    outByLabel: out.byLabel,
    inByLabel: inbound.byLabel,
    ...(out.atLeast || inbound.atLeast
      ? { atLeast: { out: out.atLeast, in: inbound.atLeast } }
      : {}),
  };
}

/**
 * The afterEdgeDbId for the next page of a single-direction expansion (same
 * edge label): the largest edge id shown, since a node's edges come back in
 * ascending id order. Null when there is no next page. Page one direction at
 * a time; with "both", one cursor would skip edges on the other side.
 */
export function nextEdgeCursor(
  result: NeighborhoodResult,
  direction: "out" | "in",
): number | null {
  const center = result.center;
  if (!center || !result.truncated) return null;
  const ids = result.edges
    .filter(
      (edge) => (direction === "out" ? edge.source : edge.target) === center.id,
    )
    .map((edge) => edge.dbId);
  return ids.length ? Math.max(...ids) : null;
}

// Connecting loaded nodes

/**
 * Edges between newly added nodes and everything visible (including each
 * other), both directions, each capped at edgeLimit. The shape decides which
 * edge properties come along (all of them without one).
 *
 * The endpoint filter sits in front of the limit, so it runs on every edge of
 * the new nodes first, reading each edge's endpoints: one read per edge,
 * whatever edgeLimit says. The explorer sends it on its own only for nodes
 * whose bounded edge counts (buildEdgeCountRequest) fit a budget.
 */
export function buildConnectRequest(opts: {
  newDbIds: number[];
  visibleDbIds: number[];
  edgeLimit?: number;
  shape?: NodeFetchShape;
}): DynamicQueryRequest {
  const newIds = validIds(opts.newDbIds);
  const visibleIds = validIds([...opts.visibleDbIds, ...newIds]);
  const edgeLimit = positive(opts.edgeLimit, EXPLORER_LIMITS.connectEdgeLimit);
  const isIn = (endpoint: "$from" | "$to"): Ast => ({
    is_in: { value: { property: endpoint }, values: { param: "visible_ids" } },
  });
  return readRequest(
    "explorer_connect",
    [
      {
        name: "from_new",
        root: projectEdges(
          limit(
            where(edgeStep("out_e", nodesParam("new_ids")), isIn("$to")),
            edgeLimit,
          ),
          opts.shape,
        ),
      },
      {
        name: "into_new",
        root: projectEdges(
          limit(
            where(edgeStep("in_e", nodesParam("new_ids")), isIn("$from")),
            edgeLimit,
          ),
          opts.shape,
        ),
      },
    ],
    {
      new_ids: { value: newIds, type: I64_ARRAY },
      visible_ids: { value: visibleIds, type: I64_ARRAY },
    },
  );
}

export function parseConnect(body: unknown): GraphEdge[] {
  const data = isRecord(body) ? body : {};
  return toGraphEdges(data.from_new, data.into_new);
}

// Nodes by id

/**
 * Nodes by id, in the order given; missing ids are skipped. shape.keys null
 * fetches full rows. `probeDegree` adds the degree probe (see
 * buildDegreeProbeRequest) for the first `probeBatch` ids, so loading a node
 * also says whether it is large; parseDegreeProbe reads it from the response.
 */
export function buildNodesByIdRequest(opts: {
  dbIds: number[];
  shape: NodeFetchShape;
  probeDegree?: boolean;
}): DynamicQueryRequest {
  const ids = validIds(opts.dbIds);
  const probed = opts.probeDegree
    ? ids.slice(0, EXPLORER_LIMITS.probeBatch)
    : [];
  return readRequest(
    "explorer_nodes",
    [
      { name: "nodes", root: project(nodesParam("ids"), opts.shape.keys) },
      ...probeDegreeEntries(probed),
    ],
    { ids: { value: ids, type: I64_ARRAY } },
  );
}

export function parseNodes(body: unknown, shape: NodeFetchShape): GraphNode[] {
  return toGraphNodes(isRecord(body) ? body.nodes : null, shape);
}

// Search

/** Whether a key is heavy on a label, by the probe (or every label's heavy keys for unprobed ones). */
function isHeavyOn(shape: NodeFetchShape, label: string, key: string): boolean {
  const byLabel = (shape as ProbedFetchShape).heavyByLabel?.[label];
  return (byLabel ?? shape.heavyKeys).includes(key);
}

/**
 * A search request: an exact id lookup, BM25 over each active full_text
 * index, and a case-sensitive `contains` scan over the given keys per label,
 * each capped at k. Returns null when there is nothing to run.
 *
 * The id lookup and BM25 are index-backed and cheap; a `contains` scan with
 * few matches reads the whole label. The search box runs them as two
 * requests (`containsTargets: []`, then only `containsTargets`) so a slow
 * scan cannot fail the cheap sources; a contains-only request is named
 * `explorer_search_contains`. A text index the catalog lists but the
 * database lacks fails its request with `index_not_found`; retry with
 * `textIndexes: []`.
 */
export function buildSearchRequest(opts: {
  scope: Scope;
  text: string;
  idCandidate: number | null;
  textIndexes: { label: string; property: string }[];
  containsTargets: { label: string; keys: string[] }[];
  k?: number;
  /**
   * How many times k a text index returns: text search can't be scoped, so
   * its hits are filtered to the scope after the read (parseSearch). One per
   * load not deleted, plus one.
   */
  overFetch?: number;
  shape: NodeFetchShape;
}): DynamicQueryRequest | null {
  const text = opts.text.trim();
  const keys = opts.shape.keys;
  const k = positive(opts.k, EXPLORER_LIMITS.searchK);
  const textK = k * positive(opts.overFetch, 1);
  const params: Params = {};
  const entries: Entry[] = [];

  const id = opts.idCandidate === null ? null : toDbId(opts.idCandidate);
  if (id !== null) {
    params.id = { value: id, type: I64 };
    // A point read, then checked against the scope.
    entries.push({
      name: "by_id",
      root: project(where(nodesParam("id"), nodeInScope(opts.scope)), keys),
    });
  }

  if (text) {
    const seen = new Set<string>();
    opts.textIndexes.forEach((index) => {
      const scope = JSON.stringify([index.label, index.property]);
      if (!index.label || !index.property || seen.has(scope)) return;
      seen.add(scope);
      const search: Ast = {
        text_search_nodes: {
          label: index.label,
          property: index.property,
          query_text: { expr: { param: "text" } },
          k: bound(textK),
        },
      };
      // value_map drops `$score`; `values` keeps it when listed. Without a
      // shape the full row matters more than the score. Text search is
      // label-scoped, so the label's own keys come along; the indexed
      // property only when it is light (indexes often sit on long text).
      const labelKeys = keysForLabel(opts.shape, index.label);
      const keysWithProperty =
        labelKeys === null
          ? null
          : [
              ...new Set([
                ...(isHeavyOn(opts.shape, index.label, index.property)
                  ? []
                  : [index.property]),
                ...labelKeys,
              ]),
            ];
      entries.push({
        name: `text_${entries.filter((entry) => entry.name.startsWith("text_")).length}`,
        root: project(
          search,
          keysWithProperty,
          keysWithProperty ? ["$score"] : [],
        ),
      });
    });
    opts.containsTargets.forEach((target, index) => {
      const targetKeys = [...new Set(target.keys.filter(Boolean))];
      if (!target.label || targetKeys.length === 0) return;
      const matches = targetKeys.map((key) => ({
        contains: { value: { property: key }, substring: { param: "text" } },
      }));
      const predicate =
        matches.length === 1 ? matches[0] : { or: { predicates: matches } };
      entries.push({
        name: `contains_${index}`,
        root: project(
          limit(where(nodesIn(opts.scope, target.label), predicate), k),
          keysForLabel(opts.shape, target.label),
        ),
      });
    });
    if (entries.some((entry) => entry.name !== "by_id")) {
      params.text = { value: text, type: STRING };
    }
  }

  if (entries.length === 0) return null;
  const scanOnly = entries.every((entry) => entry.name.startsWith("contains_"));
  return readRequest(
    scanOnly ? "explorer_search_contains" : "explorer_search",
    entries,
    params,
  );
}

function numberedRows(
  data: Record<string, unknown>,
  prefix: string,
): unknown[][] {
  const pattern = new RegExp(`^${prefix}_(\\d+)$`);
  return Object.keys(data)
    .map((key) => ({ key, match: pattern.exec(key) }))
    .filter(
      (entry): entry is { key: string; match: RegExpExecArray } =>
        entry.match !== null,
    )
    .sort((a, b) => Number(a.match[1]) - Number(b.match[1]))
    .map((entry) => rowsOf(data[entry.key]));
}

/**
 * The key whose string value contains `text` (contains is case-sensitive),
 * trying the keys the request searched first.
 */
function matchingKey(
  node: GraphNode,
  text: string | undefined,
  searched: string[] = [],
): string | undefined {
  if (!text) return undefined;
  const keys = [...searched, ...Object.keys(node.properties)];
  return keys.find((key) => {
    const value = node.properties[key];
    return typeof value === "string" && value.includes(text);
  });
}

/**
 * Search hits in priority order: the id match, text hits (best score first
 * across indexes), then contains hits; a node found twice keeps its first,
 * strongest hit. Pass the builder's options to fill matchedKey.
 */
export function parseSearch(
  body: unknown,
  shape: NodeFetchShape,
  opts: {
    /** Text hits outside it are dropped; each index keeps its first k. */
    scope?: Scope;
    k?: number;
    text?: string;
    textIndexes?: { label: string; property: string }[];
    containsTargets?: { label: string; keys: string[] }[];
  } = {},
): SearchHit[] {
  const data = isRecord(body) ? body : {};
  const text = opts.text?.trim();
  const hits: SearchHit[] = [];
  const seen = new Set<string>();
  const add = (hit: SearchHit) => {
    if (seen.has(hit.node.id)) return;
    seen.add(hit.node.id);
    hits.push(hit);
  };

  for (const node of toGraphNodes(data.by_id, shape)) add({ node, via: "id" });

  const textIndexes = opts.textIndexes?.filter(
    (index, position, all) =>
      index.label &&
      index.property &&
      all.findIndex(
        (other) =>
          other.label === index.label && other.property === index.property,
      ) === position,
  );
  const textHits: SearchHit[] = [];
  const k = positive(opts.k, EXPLORER_LIMITS.searchK);
  numberedRows(data, "text").forEach((rows, position) => {
    const property = textIndexes?.[position]?.property;
    let kept = 0;
    for (const row of rows) {
      const node = toGraphNode(row, shape, { scoped: true });
      if (!node || kept >= k) continue;
      if (opts.scope && !holds(opts.scope, node.properties)) continue;
      kept += 1;
      const score =
        isRecord(row) && typeof row.$score === "number"
          ? row.$score
          : undefined;
      textHits.push({
        node,
        via: "text",
        ...(property ? { matchedKey: property } : {}),
        ...(score !== undefined ? { score } : {}),
      });
    }
  });
  // Array.prototype.sort is stable, so equal scores keep index order.
  textHits.sort((a, b) => (b.score ?? 0) - (a.score ?? 0)).forEach(add);

  for (const rows of numberedRows(data, "contains")) {
    for (const row of rows) {
      const node = toGraphNode(row, shape, { scoped: true });
      if (!node) continue;
      const searched = opts.containsTargets?.find(
        (target) => target.label === node.label,
      )?.keys;
      const matchedKey = matchingKey(node, text, searched);
      add({ node, via: "contains", ...(matchedKey ? { matchedKey } : {}) });
    }
  }
  return hits;
}

// Seeding: a load's first view (planned in ./seeding.ts)

/**
 * Seed candidates: a literal limit of nodes per hub type in the scope (a
 * bitmap read each), with the shape's keys for that type.
 */
export function buildSeedCandidatesRequest(opts: {
  scope: Scope;
  hubs: readonly { type: string; take: number }[];
  shape: NodeFetchShape;
}): DynamicQueryRequest {
  const params: Params = {};
  const entries: Entry[] = opts.hubs
    .filter((hub) => hub.take > 0)
    .map((hub, index) => ({
      name: `candidates_${index}`,
      root: project(
        limit(nodesIn(opts.scope, hub.type), hub.take),
        keysForLabel(opts.shape, hub.type),
      ),
    }));
  if (entries.length === 0) {
    params.no_ids = { value: [], type: I64_ARRAY };
    entries.push({ name: "candidates_0", root: nodesParam("no_ids") });
  }
  return readRequest("explorer_seed_candidates", entries, params);
}

export function parseSeedCandidates(
  body: unknown,
  shape: NodeFetchShape,
): GraphNode[] {
  const data = isRecord(body) ? body : {};
  const seen = new Set<string>();
  return numberedRows(data, "candidates").flatMap((rows) =>
    rows.flatMap((row) => {
      const node = toGraphNode(row, shape, { scoped: true });
      if (!node || seen.has(node.id)) return [];
      seen.add(node.id);
      return [node];
    }),
  );
}

/**
 * Neighbor counts of many nodes at once, each a literal bound of
 * `degreeThreshold` + 1 over the adjacency list (no edges read): out and in
 * over every label, as the degree probe counts them (what reading the node's
 * edges costs), and per link of its type (`out`/`in` with the label: what a
 * ring around it can show). For the seeding's batches.
 */
export function buildBoundedProbeRequest(
  nodes: readonly { dbId: number; sides: readonly Omit<RingSide, "take">[] }[],
): DynamicQueryRequest {
  const params: Params = {};
  const valid = nodes.filter((n) => toDbId(n.dbId) !== null);
  const entries: Entry[] = [
    ...probeDegreeEntries(valid.map((n) => n.dbId)),
    ...valid.flatMap((n, i) => {
      const node: Ast = { nodes: { reference: { ids: [n.dbId] } } };
      return n.sides.map((side, k) => ({
        name: `probe_link_${i}_${k}`,
        root: count(
          limit(
            { [side.side]: { input: node, label: side.label } },
            PROBE_CAP(),
          ),
        ),
      }));
    }),
  ];
  if (entries.length === 0) {
    params.no_ids = { value: [], type: I64_ARRAY };
    entries.push({ name: "none", root: count(nodesParam("no_ids")) });
  }
  return readRequest("explorer_seed_probe", entries, params);
}

/** A node's size as the seeding sees it: every neighbor, and those over its type's links. */
export interface SeedProbe extends DegreeProbe {
  linked: number;
}

export function parseBoundedProbe(
  body: unknown,
  nodes: readonly { dbId: number; sides: readonly unknown[] }[],
): Map<number, SeedProbe> {
  const data = isRecord(body) ? body : {};
  const result = new Map<number, SeedProbe>();
  nodes
    .filter((n) => toDbId(n.dbId) !== null)
    .forEach((n, i) => {
      const out = total(data[`probe_out_${i}`]);
      const inbound = total(data[`probe_in_${i}`]);
      const links = n.sides.map((_, k) => total(data[`probe_link_${i}_${k}`]));
      if (out === null || inbound === null || links.some((x) => x === null))
        return;
      result.set(n.dbId, {
        out,
        in: inbound,
        linked: links.reduce<number>((sum, x) => sum + (x ?? 0), 0),
      });
    });
  return result;
}

/** A side of one link to read from one node in a ring. */
export interface RingSide {
  label: string;
  side: "out" | "in";
  take: number;
}

/**
 * One ring around `nodes`: for each node, each of its links and directions
 * a literal limit of edges with the nodes at their other end. The per-link
 * limits spread a node's neighbors over its links; each read costs time in
 * the node's edges of that label, so the planner leaves large nodes out.
 */
export function buildRingRequest(opts: {
  nodes: readonly { dbId: number; sides: readonly RingSide[] }[];
  shape: NodeFetchShape;
}): DynamicQueryRequest {
  const params: Params = {};
  const entries: Entry[] = [];
  opts.nodes.forEach((node, i) => {
    if (toDbId(node.dbId) === null) return;
    const center: Ast = { nodes: { reference: { ids: [node.dbId] } } };
    node.sides.forEach((side, k) => {
      if (!side.label || side.take < 1) return;
      const name = `ring_${i}_${k}`;
      entries.push(
        {
          name,
          root: limit(
            edgeStep(
              side.side === "out" ? "out_e" : "in_e",
              center,
              side.label,
            ),
            side.take,
          ),
          returned: false,
        },
        {
          name: `${name}_edges`,
          root: projectEdges(edgesVar(name), opts.shape),
        },
        {
          name: `${name}_nodes`,
          root: project(
            dedup(
              side.side === "out" ? outN(edgesVar(name)) : inN(edgesVar(name)),
            ),
            opts.shape.keys,
          ),
        },
      );
    });
  });
  if (entries.length === 0) {
    params.no_ids = { value: [], type: I64_ARRAY };
    entries.push({ name: "none", root: count(nodesParam("no_ids")) });
  }
  return readRequest("explorer_ring", entries, params);
}

export function parseRing(body: unknown, shape: NodeFetchShape): GraphData {
  const data = isRecord(body) ? body : {};
  const pick = (suffix: string) =>
    Object.keys(data)
      .filter((key) => /^ring_\d+_\d+_/.test(key) && key.endsWith(suffix))
      .flatMap((key) => rowsOf(data[key]));
  return {
    nodes: toGraphNodes(pick("_nodes"), shape),
    edges: toGraphEdges(pick("_edges")),
  };
}

/**
 * One node of a type in the scope by its identity (`entity_key`, or
 * `norm_name` for a type known by its name): P(type, L) and the key, a
 * bitmap intersection and an index lookup, at most one row.
 */
export function buildIdentityRequest(opts: {
  scope: Scope;
  label: string;
  property: "entity_key" | "norm_name";
  value: string;
  shape: NodeFetchShape;
}): DynamicQueryRequest {
  const predicate: Ast = {
    and: {
      predicates: [
        scopedLabel(opts.scope, "node", opts.label),
        {
          eq: {
            left: { property: opts.property },
            right: { param: "identity" },
          },
        },
      ],
    },
  };
  return readRequest(
    "explorer_identity",
    [
      {
        name: "node",
        root: project(
          limit({ nodes_where: { predicate } }, 1),
          keysForLabel(opts.shape, opts.label),
        ),
      },
    ],
    { identity: { value: opts.value, type: STRING } },
  );
}

export function parseIdentity(
  body: unknown,
  shape: NodeFetchShape,
): GraphNode | null {
  const data = isRecord(body) ? body : {};
  return toGraphNodes(data.node, shape, { scoped: true })[0] ?? null;
}
