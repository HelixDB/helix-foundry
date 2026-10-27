import { isFoundryStamp } from "../../../../../shared/foundry-labels";
import type { DatabaseQueryResult, JsonValue } from "../../database-query";
import type { IndexCatalogEntry } from "./types";

import type { ExplorerActivityEntry } from "../query/types";
import { classifyValue, VECTOR_MIN_LENGTH } from "./normalize";
import {
  EXPLORER_LIMITS,
  fallbackShape,
  isLargeProbe,
  mergeLabelStats,
} from "./queries";
import type {
  DegreeInfo,
  DegreeProbe,
  ElementTarget,
  GraphData,
  GraphEdge,
  GraphNode,
  LabelCount,
  LabelSchema,
  LabelStats,
  NeighborhoodResult,
  NodeFetchShape,
  Selection,
} from "./types";

/**
 * The explorer's client-side graph state, as pure functions: the node/edge
 * store and how results merge into it, the reducer behind useExplorerGraph,
 * and the derived views the panels read (visible graph, relationship groups,
 * on-canvas search). Nothing here talks to the network.
 */

export type QueryFailure = Extract<DatabaseQueryResult, { ok: false }>;

// ============ Store ============

export interface GraphStore {
  readonly nodes: ReadonlyMap<string, GraphNode>;
  readonly edges: ReadonlyMap<string, GraphEdge>;
}

export const EMPTY_STORE: GraphStore = Object.freeze({
  nodes: new Map<string, GraphNode>(),
  edges: new Map<string, GraphEdge>(),
});

export interface MergeNodeOptions {
  /**
   * The incoming node's `omitted` is the whole truth (it was fetched with a
   * shape that knows every key the existing node lists), so keys it fetched
   * and found absent stop counting as omitted. Otherwise omitted keys union.
   */
  replaceOmitted?: boolean;
}

/**
 * One node seen twice: properties accumulate (a later partial fetch never
 * erases what a full fetch brought), `complete` sticks once true, and keys
 * that have arrived drop out of `omitted`. A node's label never changes, so a
 * row with the same id and another label is a different element (an edge
 * row without endpoints, say: edge ids overlap node ids) and is ignored.
 */
export function mergeNode(
  existing: GraphNode | undefined,
  incoming: GraphNode,
  opts: MergeNodeOptions = {},
): GraphNode {
  if (!existing) return incoming;
  if (existing.label !== incoming.label) return existing;
  const properties = { ...existing.properties, ...incoming.properties };
  const complete = existing.complete || incoming.complete;
  const omitted = complete
    ? []
    : [
        ...new Set(
          opts.replaceOmitted
            ? incoming.omitted
            : [...existing.omitted, ...incoming.omitted],
        ),
      ].filter((key) => !(key in properties));
  if (
    complete === existing.complete &&
    omitted.length === existing.omitted.length &&
    omitted.every((key) => existing.omitted.includes(key)) &&
    Object.keys(properties).length ===
      Object.keys(existing.properties).length &&
    Object.entries(incoming.properties).every(
      ([key, value]) => existing.properties[key] === value,
    )
  ) {
    return existing;
  }
  return { ...existing, properties, omitted, complete };
}

export interface MergeResult {
  store: GraphStore;
  /** Node ids that were not in the store before. */
  addedNodes: string[];
  addedEdges: string[];
  /** New nodes refused because the store was at the cap. */
  droppedNodes: number;
  /** Edges skipped because an endpoint is not in the store (dangling or refused). */
  droppedEdges: number;
}

/**
 * Merges nodes and edges into the store, admitting new nodes in the order
 * given until `cap` is reached. An edge is kept only when both endpoints are
 * in the store afterwards, so the canvas never sees a dangling edge. Returns
 * the same store object when nothing changed.
 */
export function mergeGraph(
  store: GraphStore,
  incoming: Partial<GraphData>,
  opts: { cap: number } & MergeNodeOptions,
): MergeResult {
  let nodes: Map<string, GraphNode> | null = null;
  let edges: Map<string, GraphEdge> | null = null;
  const addedNodes: string[] = [];
  const addedEdges: string[] = [];
  let droppedNodes = 0;
  let droppedEdges = 0;
  const writableNodes = () => (nodes ??= new Map(store.nodes));
  const writableEdges = () => (edges ??= new Map(store.edges));
  const nodeMap = () => nodes ?? store.nodes;

  for (const node of incoming.nodes ?? []) {
    const existing = nodeMap().get(node.id);
    if (!existing) {
      if (nodeMap().size >= opts.cap) {
        droppedNodes += 1;
        continue;
      }
      writableNodes().set(node.id, node);
      addedNodes.push(node.id);
      continue;
    }
    const merged = mergeNode(existing, node, opts);
    if (merged !== existing) writableNodes().set(node.id, merged);
  }

  for (const edge of incoming.edges ?? []) {
    const map = nodeMap();
    if (!map.has(edge.source) || !map.has(edge.target)) {
      droppedEdges += 1;
      continue;
    }
    const existing = (edges ?? store.edges).get(edge.id);
    if (existing) {
      // Like node properties, edge properties accumulate: a later fetch with
      // fewer keys (light edge keys, or none after a too-large retry) never
      // erases what an earlier one brought.
      const same =
        existing.source === edge.source &&
        existing.target === edge.target &&
        existing.label === edge.label &&
        Object.entries(edge.properties).every(
          ([key, value]) => existing.properties[key] === value,
        );
      if (!same) {
        writableEdges().set(edge.id, {
          ...existing,
          ...edge,
          properties: { ...existing.properties, ...edge.properties },
        });
      }
      continue;
    }
    writableEdges().set(edge.id, edge);
    addedEdges.push(edge.id);
  }

  const changed = nodes !== null || edges !== null;
  return {
    store: changed
      ? { nodes: nodes ?? store.nodes, edges: edges ?? store.edges }
      : store,
    addedNodes,
    addedEdges,
    droppedNodes,
    droppedEdges,
  };
}

/** Removes nodes and every edge touching them. */
export function removeNodes(
  store: GraphStore,
  ids: Iterable<string>,
): GraphStore {
  const remove = new Set([...ids].filter((id) => store.nodes.has(id)));
  if (remove.size === 0) return store;
  const nodes = new Map(store.nodes);
  for (const id of remove) nodes.delete(id);
  const edges = new Map<string, GraphEdge>();
  for (const [id, edge] of store.edges) {
    if (!remove.has(edge.source) && !remove.has(edge.target))
      edges.set(id, edge);
  }
  return { nodes, edges };
}

/** `node:User` / `edge:FOLLOWS`: how hidden labels and label focus are keyed. */
export function labelKey(target: ElementTarget, label: string): string {
  return `${target}:${label}`;
}

/** The graph the canvas draws: hidden labels and hidden nodes left out, edges only between shown nodes. */
export function visibleGraph(
  store: GraphStore,
  hiddenLabels: ReadonlySet<string>,
  hiddenNodes: ReadonlySet<string>,
): GraphData {
  const nodes: GraphNode[] = [];
  const shown = new Set<string>();
  for (const node of store.nodes.values()) {
    if (
      hiddenNodes.has(node.id) ||
      hiddenLabels.has(labelKey("node", node.label))
    )
      continue;
    nodes.push(node);
    shown.add(node.id);
  }
  const edges: GraphEdge[] = [];
  for (const edge of store.edges.values()) {
    if (!shown.has(edge.source) || !shown.has(edge.target)) continue;
    if (hiddenLabels.has(labelKey("edge", edge.label))) continue;
    edges.push(edge);
  }
  return { nodes, edges };
}

/**
 * The nodes that make room when the canvas is full: nodes hidden one by one
 * first, then nodes of hidden labels. `keep` ids (the nodes about to be
 * merged) are never picked.
 */
export function hiddenNodeIds(
  state: Pick<ExplorerState, "store" | "hiddenLabels" | "hiddenNodes">,
  keep: ReadonlySet<string> = new Set(),
): string[] {
  const result: string[] = [];
  for (const id of state.hiddenNodes) {
    if (state.store.nodes.has(id) && !keep.has(id)) result.push(id);
  }
  if (state.hiddenLabels.size > 0) {
    for (const node of state.store.nodes.values()) {
      if (state.hiddenNodes.has(node.id) || keep.has(node.id)) continue;
      if (state.hiddenLabels.has(labelKey("node", node.label)))
        result.push(node.id);
    }
  }
  return result;
}

/** How many more nodes fit under the cap, counting hidden nodes as room (they are removed to make it). */
export function canvasRoom(
  state: Pick<ExplorerState, "store" | "hiddenLabels" | "hiddenNodes">,
  cap: number,
): number {
  return cap - state.store.nodes.size + hiddenNodeIds(state).length;
}

/**
 * Label rows for the rail: the counted labels plus any label loaded or
 * probed since (one discovery missed, or written after the counts), whose
 * count is unknown unless a label page counted it.
 */
export function railLabels(
  state: Pick<ExplorerState, "overview" | "store" | "schemas" | "labelPages">,
): { nodes: LabelCount[]; edges: LabelCount[] } {
  const stats = state.overview.stats;
  const nodes = new Map<string, number | null>(
    (stats?.nodeLabels ?? []).map((entry) => [entry.label, entry.count]),
  );
  const edges = new Map<string, number | null>(
    (stats?.edgeLabels ?? []).map((entry) => [entry.label, entry.count]),
  );
  const nodeCount = (label: string) => state.labelPages[label]?.total ?? null;
  // A label page counts its label (from the bitmap), which fills in one the counts left out.
  for (const label of [...nodes.keys()])
    nodes.set(label, nodes.get(label) ?? nodeCount(label));
  for (const node of state.store.nodes.values()) {
    if (!nodes.has(node.label)) nodes.set(node.label, nodeCount(node.label));
  }
  for (const edge of state.store.edges.values())
    if (!edges.has(edge.label)) edges.set(edge.label, null);
  for (const schema of state.schemas) {
    if (schema.target === "node" && !nodes.has(schema.label))
      nodes.set(schema.label, nodeCount(schema.label));
    if (schema.target === "edge" && !edges.has(schema.label))
      edges.set(schema.label, null);
  }
  const rows = (map: Map<string, number | null>) =>
    [...map.entries()]
      .map(([label, count]) => ({ label, count }))
      .sort((a, b) => a.label.localeCompare(b.label));
  return { nodes: rows(nodes), edges: rows(edges) };
}

/**
 * Whether the database totals are floors ("at least"), per side: labels were
 * found by sampling rather than a full scan, or the rail lists labels without
 * a count (loaded since, or over the per-request cap). Every place that shows
 * a total (the bar, the overview, the sample notice) uses this, so they agree.
 */
export function totalsAreFloors(
  stats: LabelStats | null | undefined,
  labels: { nodes: readonly LabelCount[]; edges: readonly LabelCount[] },
): { nodes: boolean; edges: boolean } {
  return {
    nodes:
      Boolean(stats?.partial?.nodes) ||
      labels.nodes.some((entry) => entry.count === null),
    edges:
      Boolean(stats?.partial?.edges) ||
      labels.edges.some((entry) => entry.count === null),
  };
}

/** How many loaded nodes and edges carry each label. */
export function loadedLabelCounts(store: GraphStore): {
  nodes: Map<string, number>;
  edges: Map<string, number>;
} {
  const nodes = new Map<string, number>();
  const edges = new Map<string, number>();
  for (const node of store.nodes.values())
    nodes.set(node.label, (nodes.get(node.label) ?? 0) + 1);
  for (const edge of store.edges.values())
    edges.set(edge.label, (edges.get(edge.label) ?? 0) + 1);
  return { nodes, edges };
}

// ============ Degrees and expansion cursors ============

/**
 * A later neighborhood page has degree counts but no per-label counts (the
 * builder only asks for them on the first page); keep the earlier ones.
 */
export function mergeDegree(
  previous: DegreeInfo | undefined,
  next: DegreeInfo,
  hasLabels: boolean,
): DegreeInfo {
  if (hasLabels || !previous) return next;
  return {
    ...next,
    outByLabel: previous.outByLabel,
    inByLabel: previous.inByLabel,
  };
}

export type NodeSize = "small" | "large" | "unknown";

/**
 * What the explorer knows about one side of a node: "large" when it has more
 * than `degreeThreshold` relationships there (its edges are then read only on
 * an explicit, warned action), "small" when it has at most that, "unknown"
 * before a probe or a count.
 */
export function nodeSideSize(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeId: string,
  side: "out" | "in",
): NodeSize {
  const degree = state.degrees.get(nodeId);
  if (degree) {
    const count = side === "out" ? degree.outDegree : degree.inDegree;
    return degree.atLeast?.[side] || count > EXPLORER_LIMITS.degreeThreshold
      ? "large"
      : "small";
  }
  const probe = state.probes.get(nodeId);
  if (probe) return isLargeProbe(probe, side) ? "large" : "small";
  return "unknown";
}

/** "large" when either side is, "small" when both are, else "unknown". */
export function nodeSize(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeId: string,
): NodeSize {
  const out = nodeSideSize(state, nodeId, "out");
  const inbound = nodeSideSize(state, nodeId, "in");
  if (out === "large" || inbound === "large") return "large";
  return out === "small" && inbound === "small" ? "small" : "unknown";
}

/**
 * The edges connecting a node reads (one endpoint read per edge), when known:
 * its bounded degree, or the probe's bounded edge counts. Null for a node
 * whose edges were never counted (the neighbor count alone says nothing
 * about parallel edges) or that is large.
 */
export function connectCost(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeId: string,
): number | null {
  if (nodeSize(state, nodeId) !== "small") return null;
  const degree = state.degrees.get(nodeId);
  if (degree) return degree.outDegree + degree.inDegree;
  const edges = state.probes.get(nodeId)?.edges;
  return edges ? edges.out + edges.in : null;
}

/**
 * A floor on the edges connecting `nodeIds` reads: the counted edges of the
 * small ones, plus, unless `countedOnly`, the neighbor counts of small ones
 * whose edges weren't counted yet (each neighbor is at least one edge).
 * Large nodes and those of unknown size add nothing: no budget plans them.
 */
export function connectCostFloor(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeIds: readonly string[],
  opts: { countedOnly?: boolean } = {},
): number {
  let total = 0;
  for (const id of new Set(nodeIds)) {
    const cost = connectCost(state, id);
    if (cost !== null) total += cost;
    else if (!opts.countedOnly && nodeSize(state, id) === "small") {
      const probe = state.probes.get(id);
      if (probe) total += probe.out + probe.in;
    }
  }
  return total;
}

/** Whether a node is small by its neighbor probe but its edges were never counted (see buildEdgeCountRequest). */
export function needsEdgeCount(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeId: string,
): boolean {
  if (state.degrees.has(nodeId) || nodeSize(state, nodeId) !== "small")
    return false;
  return state.probes.get(nodeId)?.edges === undefined;
}

export interface ConnectPlan {
  /** Small nodes with counted edges, cheapest first, within the budget. */
  connect: string[];
  /** Every node left out: large, of unknown size or edge count, or over the budget. */
  skipped: string[];
  /** Skipped because they have more than `degreeThreshold` relationships on a side. */
  large: string[];
  /** Skipped because their size (or edge count) couldn't be checked. */
  unknown: string[];
}

/**
 * Which new nodes a connect request may read the edges of: small ones whose
 * edges were counted, cheapest first, while their summed edge counts fit
 * `connectEdgeBudget` (the first always fits: it has at most
 * 2 × `degreeThreshold`). The rest (large, too many at once, or of unknown
 * size) are skipped; the user can connect them explicitly.
 */
export function planConnect(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeIds: readonly string[],
  budget: number = EXPLORER_LIMITS.connectEdgeBudget,
): ConnectPlan {
  const { batches, large, unknown } = connectBatches(state, nodeIds, budget);
  const [first = [], ...rest] = batches;
  return {
    connect: first,
    skipped: [...rest.flat(), ...large, ...unknown],
    large,
    unknown,
  };
}

function sortBySize(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeIds: readonly string[],
): {
  small: { id: string; cost: number }[];
  large: string[];
  unknown: string[];
} {
  const small: { id: string; cost: number }[] = [];
  const large: string[] = [];
  const unknown: string[] = [];
  for (const id of new Set(nodeIds)) {
    const cost = connectCost(state, id);
    if (cost !== null) small.push({ id, cost });
    else if (nodeSize(state, id) === "large") large.push(id);
    else unknown.push(id);
  }
  small.sort((a, b) => a.cost - b.cost);
  return { small, large, unknown };
}

/**
 * Connect requests for nodes of known edge counts, packed cheapest first so
 * each request reads at most `budget` edges (a node over it alone), plus the
 * nodes that are large or of unknown size, which no budget can plan.
 */
export function connectBatches(
  state: Pick<ExplorerState, "degrees" | "probes">,
  nodeIds: readonly string[],
  budget: number = EXPLORER_LIMITS.connectEdgeBudget,
): { batches: string[][]; large: string[]; unknown: string[] } {
  const { small, large, unknown } = sortBySize(state, nodeIds);
  const batches: string[][] = [];
  let batch: string[] = [];
  let used = 0;
  for (const { id, cost } of small) {
    if (batch.length > 0 && used + cost > budget) {
      batches.push(batch);
      batch = [];
      used = 0;
    }
    batch.push(id);
    used += cost;
  }
  if (batch.length > 0) batches.push(batch);
  return { batches, large, unknown };
}

/** A count for display: "1,000+" when it is a floor. */
export function degreeText(count: number, atLeast = false): string {
  return atLeast
    ? `${count.toLocaleString("en-US")}+`
    : count.toLocaleString("en-US");
}

/** What a node's context menu says of its relationships (see menuRelationships). */
export interface MenuRelationships {
  /** Counts to show, in total and per side; unset while there is nothing to say. */
  total?: string;
  out?: string;
  in?: string;
  /** Sides known to have no relationships: their Expand rows are disabled. */
  empty: { out: boolean; in: boolean };
}

/**
 * What a node's context menu says of its relationships: the degree when
 * known, else only what the probe can say ("1,000+" for a large side, the
 * edge count once counted, empty where it found no neighbors; a small side's
 * neighbor count isn't its edge count, so on its own it says nothing). The
 * probe counts every edge, so while edge labels are hidden (`edgesHidden`:
 * Foundry's internals) the menu goes by the degree alone, which leaves them
 * out, and says nothing until it is read.
 *
 * @example
 * const probe = { out: 0, in: 2, edges: { out: 0, in: 4 } };
 * menuRelationships(undefined, probe, false);
 * // { total: "4", out: "0", in: "4", empty: { out: true, in: false } }
 * menuRelationships(undefined, probe, true);
 * // { empty: { out: false, in: false } }: the degree isn't read yet
 */
export function menuRelationships(
  degree: DegreeInfo | undefined,
  probe: DegreeProbe | undefined,
  edgesHidden: boolean,
): MenuRelationships {
  if (degree) {
    const atLeast = degree.atLeast ?? { out: false, in: false };
    return {
      total: degreeText(
        degree.outDegree + degree.inDegree,
        atLeast.out || atLeast.in,
      ),
      out: degreeText(degree.outDegree, atLeast.out),
      in: degreeText(degree.inDegree, atLeast.in),
      empty: { out: degree.outDegree === 0, in: degree.inDegree === 0 },
    };
  }
  const known = edgesHidden ? undefined : probe;
  if (!known) return { empty: { out: false, in: false } };
  const floor = degreeText(EXPLORER_LIMITS.degreeThreshold, true);
  const side = (name: "out" | "in") =>
    isLargeProbe(known, name)
      ? floor
      : known.edges && degreeText(known.edges[name]);
  return {
    total: isLargeProbe(known)
      ? floor
      : known.edges && degreeText(known.edges.out + known.edges.in),
    out: side("out"),
    in: side("in"),
    empty: { out: known.out === 0, in: known.in === 0 },
  };
}

/** Where one side of an expansion stands: not loaded yet, fully loaded, or the edge id to page after. */
export type SideCursor = "unstarted" | "done" | number;

export interface ExpansionState {
  out: SideCursor;
  in: SideCursor;
}

export const UNSTARTED_EXPANSION: ExpansionState = Object.freeze({
  out: "unstarted",
  in: "unstarted",
});

/** Expansions are tracked per node and edge label (`*` = every label). */
export function expansionKey(
  nodeId: string,
  edgeLabel: string | null | undefined,
): string {
  return `${nodeId}|${edgeLabel || "*"}`;
}

/**
 * The cursor for one side after a page of `limit` edges per side. Fewer than
 * `limit` edges on that side, or a response that says nothing was cut off,
 * means the side is done; otherwise page after the largest edge id (a node's
 * edges come back in ascending id order).
 */
export function sideCursorAfter(
  result: NeighborhoodResult,
  side: "out" | "in",
  limit: number,
): SideCursor {
  const center = result.center;
  if (!center || !result.truncated) return "done";
  const ids = result.edges
    .filter(
      (edge) => (side === "out" ? edge.source : edge.target) === center.id,
    )
    .map((edge) => edge.dbId);
  if (ids.length < limit) return "done";
  return Math.max(...ids);
}

// ============ Inspector views ============

export interface RelationshipGroup {
  direction: "out" | "in";
  edgeLabel: string;
  /** Edges of this group in the store. */
  loaded: number;
  /** The node's true edge count for this group, when its degree is known. */
  total: number | null;
  /** Loaded neighbors, one per edge, in edge id order. */
  neighbors: { edgeId: string; nodeId: string }[];
}

/**
 * The selected node's relationships grouped by direction and edge label:
 * what is loaded, plus groups the degree says exist but nothing of which is
 * loaded yet. Out groups first, then in; each by label.
 */
export function relationshipGroups(
  store: GraphStore,
  nodeId: string,
  degree?: DegreeInfo,
): RelationshipGroup[] {
  const groups = new Map<string, RelationshipGroup>();
  const group = (direction: "out" | "in", edgeLabel: string) => {
    const key = `${direction}:${edgeLabel}`;
    let existing = groups.get(key);
    if (!existing) {
      existing = {
        direction,
        edgeLabel,
        loaded: 0,
        total: null,
        neighbors: [],
      };
      groups.set(key, existing);
    }
    return existing;
  };
  const edges = [...store.edges.values()]
    .filter((edge) => edge.source === nodeId || edge.target === nodeId)
    .sort((a, b) => a.dbId - b.dbId);
  for (const edge of edges) {
    if (edge.source === nodeId) {
      const entry = group("out", edge.label);
      entry.loaded += 1;
      entry.neighbors.push({ edgeId: edge.id, nodeId: edge.target });
    }
    if (edge.target === nodeId) {
      const entry = group("in", edge.label);
      entry.loaded += 1;
      entry.neighbors.push({ edgeId: edge.id, nodeId: edge.source });
    }
  }
  if (degree) {
    for (const { label, count } of degree.outByLabel)
      group("out", label).total = count;
    for (const { label, count } of degree.inByLabel)
      group("in", label).total = count;
  }
  return [...groups.values()].sort(
    (a, b) =>
      (a.direction === b.direction ? 0 : a.direction === "out" ? -1 : 1) ||
      a.edgeLabel.localeCompare(b.edgeLabel),
  );
}

/** A node as JSON for "Copy as JSON": its id, label and stored properties, nothing the canvas added. */
export function nodeToJson(node: GraphNode): Record<string, JsonValue> {
  return { $id: node.dbId, $label: node.label, ...node.properties };
}

export function edgeToJson(
  edge: GraphEdge,
  store: GraphStore,
): Record<string, JsonValue> {
  const endpoint = (key: string) =>
    store.nodes.get(key)?.dbId ?? Number(key.slice(2));
  return {
    $id: edge.dbId,
    $label: edge.label,
    $from: endpoint(edge.source),
    $to: endpoint(edge.target),
    ...edge.properties,
  };
}

// ============ Selection history ============

const HISTORY_LIMIT = 50;

function sameSelection(a: Selection | null, b: Selection | null): boolean {
  return (
    a === b || (a !== null && b !== null && a.kind === b.kind && a.id === b.id)
  );
}

/** Selection history for the inspector's back button: the previous selection is pushed when it changes. */
export function pushSelectionHistory(
  history: Selection[],
  previous: Selection | null,
  next: Selection | null,
): Selection[] {
  if (!previous || sameSelection(previous, next)) return history;
  const trimmed = history.filter((entry) => !sameSelection(entry, previous));
  return [...trimmed, previous].slice(-HISTORY_LIMIT);
}

/** Drops history entries whose element is no longer in the store. */
export function pruneSelectionHistory(
  history: Selection[],
  store: GraphStore,
): Selection[] {
  const next = history.filter((entry) =>
    entry.kind === "node"
      ? store.nodes.has(entry.id)
      : store.edges.has(entry.id),
  );
  return next.length === history.length ? history : next;
}

// ============ Search ============

export interface CanvasMatch {
  node: GraphNode;
  /** The property path that matched; null when the caption or id did. */
  matchedKey: string | null;
  /** A short excerpt around the match, for property matches. */
  snippet: string | null;
}

const SNIPPET_RADIUS = 28;
const MAX_MATCH_DEPTH = 3;

function excerpt(text: string, index: number, length: number): string {
  const start = Math.max(0, index - SNIPPET_RADIUS);
  const end = Math.min(text.length, index + length + SNIPPET_RADIUS);
  const body = text.slice(start, end).replace(/\s+/g, " ");
  return `${start > 0 ? "…" : ""}${body}${end < text.length ? "…" : ""}`;
}

/** A short excerpt of `value` around the first case-insensitive match of `text` (or its start). */
export function snippetFor(value: string, text: string): string {
  const needle = text.trim().toLowerCase();
  const index = needle ? value.toLowerCase().indexOf(needle) : -1;
  if (index >= 0) return excerpt(value, index, needle.length);
  const start = value.replace(/\s+/g, " ").trim();
  return start.length > SNIPPET_RADIUS * 2
    ? `${start.slice(0, SNIPPET_RADIUS * 2)}…`
    : start;
}

/**
 * The first property (depth-first, nested objects as `a.b`) whose scalar
 * value contains `needle`. Objects are walked, never stringified, so
 * "object" doesn't match every node with a nested property; vectors are
 * skipped.
 */
function matchValue(
  value: JsonValue,
  needle: string,
  path: string,
  depth: number,
): { key: string; snippet: string } | null {
  if (value === null) return null;
  if (typeof value === "string") {
    const index = value.toLowerCase().indexOf(needle);
    return index >= 0
      ? { key: path, snippet: excerpt(value, index, needle.length) }
      : null;
  }
  if (typeof value === "number" || typeof value === "boolean") {
    const text = String(value);
    return text.toLowerCase().includes(needle)
      ? { key: path, snippet: text }
      : null;
  }
  if (depth >= MAX_MATCH_DEPTH) return null;
  if (Array.isArray(value)) {
    if (value.length > VECTOR_MIN_LENGTH && classifyValue(value) === "vector")
      return null;
    for (const item of value) {
      const hit = matchValue(item, needle, path, depth + 1);
      if (hit) return hit;
    }
    return null;
  }
  for (const [key, child] of Object.entries(value)) {
    const hit = matchValue(child, needle, `${path}.${key}`, depth + 1);
    if (hit) return hit;
  }
  return null;
}

/**
 * Case-insensitive search over loaded nodes: caption, then id, then property
 * values. Caption matches rank first (exact, then prefix, then anywhere).
 * Foundry's stamps are not searched: the Inspector hides them, and a
 * `foundry_tx` is a ULID, so a short needle would match every node one
 * request wrote.
 */
export function matchLoadedNodes(
  nodes: readonly GraphNode[],
  text: string,
  caption: (node: GraphNode) => string,
  limit = 8,
): CanvasMatch[] {
  const needle = text.trim().toLowerCase().replace(/^#/, "");
  if (!needle) return [];
  const ranked: { match: CanvasMatch; rank: number; order: number }[] = [];
  nodes.forEach((node, order) => {
    const name = caption(node).toLowerCase();
    let rank = -1;
    let matchedKey: string | null = null;
    let snippet: string | null = null;
    if (name === needle) rank = 0;
    else if (name.startsWith(needle)) rank = 1;
    else if (String(node.dbId) === needle) rank = 0;
    else if (name.includes(needle)) rank = 2;
    else if (String(node.dbId).startsWith(needle)) rank = 3;
    else if (node.label.toLowerCase() === needle) rank = 5;
    else {
      for (const [key, value] of Object.entries(node.properties)) {
        if (isFoundryStamp("node", node.label, key)) continue;
        const hit = matchValue(value, needle, key, 0);
        if (hit) {
          rank = 4;
          matchedKey = hit.key;
          snippet = hit.snippet;
          break;
        }
      }
    }
    if (rank >= 0)
      ranked.push({ match: { node, matchedKey, snippet }, rank, order });
  });
  return ranked
    .sort((a, b) => a.rank - b.rank || a.order - b.order)
    .slice(0, limit)
    .map((entry) => entry.match);
}

/** A node id typed in the search box: digits, optionally after `#`. */
export function parseIdCandidate(text: string): number | null {
  const match = /^\s*#?(\d{1,16})\s*$/.exec(text);
  if (!match) return null;
  const value = Number(match[1]);
  return Number.isSafeInteger(value) ? value : null;
}

/**
 * Node full_text indexes from the catalog (BM25 search sources). Indexes
 * partitioned by a tenant property are left out: searching one needs a
 * tenant value the explorer doesn't have, and without it the database fails
 * the whole request.
 */
export function textIndexesFor(
  indexes: readonly IndexCatalogEntry[] | undefined,
): { label: string; property: string }[] {
  const seen = new Set<string>();
  const result: { label: string; property: string }[] = [];
  for (const entry of indexes ?? []) {
    if (entry.target !== "node" || entry.type !== "full_text") continue;
    if (entry.tenantProperty != null) continue;
    const key = `${entry.label}\u0000${entry.property}`;
    if (seen.has(key)) continue;
    seen.add(key);
    result.push({ label: entry.label, property: entry.property });
  }
  return result;
}

/** Node and edge labels the index catalog names: labels that exist (or did), known without a query. */
export function indexLabels(
  indexes: readonly IndexCatalogEntry[] | undefined,
): { nodes: string[]; edges: string[] } {
  const nodes = new Set<string>();
  const edges = new Set<string>();
  for (const entry of indexes ?? []) {
    if (!entry.label) continue;
    (entry.target === "edge" ? edges : nodes).add(entry.label);
  }
  return { nodes: [...nodes].sort(), edges: [...edges].sort() };
}

const NAME_KEYS = [
  "name",
  "title",
  "label",
  "username",
  "email",
  "slug",
  "display_name",
  "handle",
];
const MAX_CONTAINS_KEYS = 3;
const MAX_CONTAINS_LABELS = 8;

/**
 * Labels and string keys for the database `contains` fallback: the light
 * string properties of each probed label, naming keys first, a few per
 * label. Labels already covered by a text index on the same key are skipped
 * for that key. Foundry's stamps are skipped too: every node a load wrote
 * carries them, so they would take the few slots from the data's keys.
 */
export function containsTargetsFor(
  schemas: readonly LabelSchema[],
  textIndexes: readonly { label: string; property: string }[] = [],
): { label: string; keys: string[] }[] {
  const result: { label: string; keys: string[] }[] = [];
  for (const schema of schemas) {
    if (schema.target !== "node") continue;
    const indexed = new Set(
      textIndexes
        .filter((index) => index.label === schema.label)
        .map((index) => index.property),
    );
    const keys = schema.properties
      .filter(
        (property) =>
          !property.heavy &&
          property.kinds[0] === "string" &&
          !indexed.has(property.key) &&
          !isFoundryStamp("node", schema.label, property.key),
      )
      .map((property) => property.key)
      .sort((a, b) => {
        const rank = (key: string) => {
          const index = NAME_KEYS.indexOf(key.toLowerCase());
          return index < 0 ? NAME_KEYS.length : index;
        };
        return rank(a) - rank(b);
      })
      .slice(0, MAX_CONTAINS_KEYS);
    if (keys.length > 0) result.push({ label: schema.label, keys });
    if (result.length >= MAX_CONTAINS_LABELS) break;
  }
  return result;
}

// ============ Explorer state ============

export type OpStatus = "idle" | "loading" | "ready" | "error";

/**
 * idle: not started; no-access: the user can't query this database;
 * loading: first view on its way; ready: the canvas can be used (it may be
 * empty after Clear); empty: the database has no nodes; error: the first view
 * failed (see bootError).
 */
export type BootPhase =
  | "idle"
  | "no-access"
  | "loading"
  | "ready"
  | "empty"
  | "error";

export interface LabelPageState {
  /** Keyset cursor for the next page; null before the first one. */
  nextAfter: number | null;
  exhausted: boolean;
  /**
   * Label rows the pages moved past so far (some may have been on the canvas
   * already): the offset the next page skips.
   */
  loaded: number;
  status: "idle" | "loading" | "error";
  /** The label's node count, from the first page. */
  total: number | null;
}

/** What a notice's button does: an explicit follow-up the explorer would not run on its own. */
export type ExplorerNoticeAction =
  | {
      kind: "connect";
      label: string;
      nodeIds: string[];
      /**
       * The user was told these nodes are large (or of unknown size) and
       * connecting reads every one of their relationships: connect them all.
       * Without it, "Connect" checks sizes first and asks again for large ones.
       */
      confirmed?: boolean;
    }
  | {
      kind: "expand";
      label: string;
      nodeId: string;
      direction?: "both" | "out" | "in";
      edgeLabel?: string | null;
    };

export interface ExplorerNotice {
  id: string;
  tone: "info" | "warning";
  message: string;
  action?: ExplorerNoticeAction;
}

export interface ExplorerState {
  phase: BootPhase;
  bootError: QueryFailure | null;
  /**
   * Which step the boot failed at: finding and counting labels (cheap, so a
   * failure there means the database or its gateway is in trouble) or the
   * first view (which a smaller sample may fix).
   */
  bootFailedAt: "labels" | "sample" | null;
  overview: {
    status: OpStatus;
    stats: LabelStats | null;
    error: QueryFailure | null;
  };
  schemas: LabelSchema[];
  /** The fetch shape every node request uses; extended (never rebuilt) as full nodes reveal keys. */
  shape: NodeFetchShape;
  sample: {
    status: OpStatus;
    /** Not everything is on the canvas. */
    truncated: boolean;
    /** Loaded without properties because the full sample was too large. */
    light: boolean;
    error: QueryFailure | null;
  };
  store: GraphStore;
  labelPages: Readonly<Record<string, LabelPageState>>;
  degrees: ReadonlyMap<string, DegreeInfo>;
  degreeStatus: ReadonlyMap<string, "loading" | "error">;
  /** Degree probes (neighbors per direction, no edges read), per node. */
  probes: ReadonlyMap<string, DegreeProbe>;
  expansions: ReadonlyMap<string, ExpansionState>;
  hiddenLabels: ReadonlySet<string>;
  hiddenNodes: ReadonlySet<string>;
  pinned: ReadonlySet<string>;
  /** Operation key → the activity id of its request in flight. */
  pending: ReadonlyMap<string, string>;
  /** Requests the explorer ran, newest first. */
  activity: ExplorerActivityEntry[];
  notices: ExplorerNotice[];
}

export const ACTIVITY_LIMIT = 50;

export function initialExplorerState(
  hiddenLabels: Iterable<string> = [],
): ExplorerState {
  return {
    phase: "idle",
    bootError: null,
    bootFailedAt: null,
    overview: { status: "idle", stats: null, error: null },
    schemas: [],
    shape: fallbackShape(),
    sample: { status: "idle", truncated: false, light: false, error: null },
    store: EMPTY_STORE,
    labelPages: {},
    degrees: new Map(),
    degreeStatus: new Map(),
    probes: new Map(),
    expansions: new Map(),
    hiddenLabels: new Set(hiddenLabels),
    hiddenNodes: new Set(),
    pinned: new Set(),
    pending: new Map(),
    activity: [],
    notices: [],
  };
}

export type ExplorerAction =
  | { type: "boot"; noAccess: boolean; hiddenLabels?: Iterable<string> }
  | {
      type: "boot/failed";
      failure: QueryFailure;
      noAccess?: boolean;
      stage?: "labels" | "sample";
    }
  | { type: "boot/empty" }
  | { type: "overview/loading" }
  | { type: "overview/loaded"; stats: LabelStats }
  /** Counts for labels seen after the boot, folded into the current stats. */
  | {
      type: "overview/merged";
      stats: LabelStats;
      gone?: { nodes?: string[]; edges?: string[] };
    }
  | { type: "overview/failed"; failure: QueryFailure }
  /** A retried count was cancelled: put back the status it had before. */
  | { type: "overview/restore"; status: OpStatus; error: QueryFailure | null }
  | { type: "schema/loaded"; schemas: LabelSchema[]; shape: NodeFetchShape }
  | { type: "shape/extended"; shape: NodeFetchShape }
  | { type: "sample/loading" }
  | {
      type: "sample/loaded";
      merge: MergeResult;
      truncated: boolean;
      light: boolean;
    }
  | { type: "sample/failed"; failure: QueryFailure }
  | { type: "merged"; merge: MergeResult; cap: number }
  | { type: "labelPage/loading"; label: string }
  | {
      type: "labelPage/loaded";
      label: string;
      nextAfter: number | null;
      total: number | null;
      count: number;
    }
  | { type: "labelPage/failed"; label: string }
  | { type: "degree/loading"; nodeId: string }
  | {
      type: "degree/loaded";
      nodeId: string;
      degree: DegreeInfo;
      hasLabels: boolean;
    }
  | { type: "degree/failed"; nodeId: string }
  | { type: "degree/cancelled"; nodeId: string }
  | { type: "probe/loaded"; probes: ReadonlyMap<string, DegreeProbe> }
  | {
      type: "probe/edges";
      edges: ReadonlyMap<string, { out: number; in: number }>;
    }
  | { type: "expansion"; key: string; out?: SideCursor; in?: SideCursor }
  | { type: "pending/start"; op: string; id: string }
  | { type: "pending/end"; op: string; id: string }
  | { type: "activity/start"; entry: ExplorerActivityEntry }
  | {
      type: "activity/end";
      id: string;
      ok: boolean | null;
      status: number | null;
      elapsedMs: number | null;
    }
  | { type: "nodes/hide"; ids: string[] }
  | { type: "nodes/show"; ids: string[] }
  | { type: "label/hidden"; key: string; hidden: boolean }
  | { type: "hidden/reset" }
  | { type: "nodes/remove"; ids: string[] }
  | { type: "canvas/clear" }
  | { type: "pin"; id: string; pinned: boolean }
  | { type: "notice/add"; notice: ExplorerNotice }
  | { type: "notice/dismiss"; id: string };

export const CAP_NOTICE_ID = "cap";

function capNotice(dropped: number, cap: number): ExplorerNotice {
  const nodes =
    dropped === 1
      ? "1 node wasn't"
      : `${dropped.toLocaleString("en-US")} nodes weren't`;
  return {
    id: CAP_NOTICE_ID,
    tone: "warning",
    message: `The canvas holds up to ${cap.toLocaleString("en-US")} nodes, so ${nodes} added. Hide or clear some to load more.`,
  };
}

function withNotice(
  notices: ExplorerNotice[],
  notice: ExplorerNotice,
): ExplorerNotice[] {
  return [...notices.filter((entry) => entry.id !== notice.id), notice];
}

function setWith<T>(
  set: ReadonlySet<T>,
  value: T,
  present: boolean,
): ReadonlySet<T> {
  if (set.has(value) === present) return set;
  const next = new Set(set);
  if (present) next.add(value);
  else next.delete(value);
  return next;
}

function mapWith<K, V>(
  map: ReadonlyMap<K, V>,
  key: K,
  value: V | undefined,
): ReadonlyMap<K, V> {
  if (value === undefined && !map.has(key)) return map;
  const next = new Map(map);
  if (value === undefined) next.delete(key);
  else next.set(key, value);
  return next;
}

/** Drops per-node state for nodes that left the store. */
function forgetNodes(state: ExplorerState, store: GraphStore): ExplorerState {
  const keep = <V>(map: ReadonlyMap<string, V>) => {
    const next = new Map([...map].filter(([id]) => store.nodes.has(id)));
    return next.size === map.size ? map : next;
  };
  const keepSet = (set: ReadonlySet<string>) => {
    const next = new Set([...set].filter((id) => store.nodes.has(id)));
    return next.size === set.size ? set : next;
  };
  const expansions = new Map(
    [...state.expansions].filter(([key]) =>
      store.nodes.has(key.slice(0, key.indexOf("|"))),
    ),
  );
  return {
    ...state,
    store,
    hiddenNodes: keepSet(state.hiddenNodes),
    pinned: keepSet(state.pinned),
    degreeStatus: keep(state.degreeStatus),
    expansions:
      expansions.size === state.expansions.size ? state.expansions : expansions,
  };
}

export function explorerReducer(
  state: ExplorerState,
  action: ExplorerAction,
): ExplorerState {
  switch (action.type) {
    case "boot": {
      // Hidden labels are a preference and survive a refresh; everything loaded does not.
      const fresh = initialExplorerState(
        action.hiddenLabels ?? state.hiddenLabels,
      );
      return {
        ...fresh,
        activity: state.activity,
        phase: action.noAccess ? "no-access" : "loading",
      };
    }
    case "boot/failed":
      return {
        ...state,
        phase:
          action.noAccess || action.failure.kind === "forbidden"
            ? "no-access"
            : "error",
        bootError: action.failure,
        bootFailedAt: action.stage ?? "labels",
      };
    case "boot/empty":
      return {
        ...state,
        phase: "empty",
        sample: { ...state.sample, status: "ready" },
      };
    case "overview/loading":
      return {
        ...state,
        overview: {
          status: "loading",
          stats: state.overview.stats,
          error: null,
        },
      };
    case "overview/loaded":
      return {
        ...state,
        overview: { status: "ready", stats: action.stats, error: null },
      };
    case "overview/merged": {
      const stats = state.overview.stats
        ? mergeLabelStats(state.overview.stats, action.stats, action.gone)
        : action.stats;
      return { ...state, overview: { ...state.overview, stats } };
    }
    case "overview/failed":
      return {
        ...state,
        overview: {
          status: "error",
          stats: state.overview.stats,
          error: action.failure,
        },
      };
    case "overview/restore":
      if (state.overview.status !== "loading") return state;
      return {
        ...state,
        overview: {
          status: action.status,
          stats: state.overview.stats,
          error: action.error,
        },
      };
    case "schema/loaded":
      return { ...state, schemas: action.schemas, shape: action.shape };
    case "shape/extended":
      return action.shape === state.shape
        ? state
        : { ...state, shape: action.shape };
    case "sample/loading":
      return {
        ...state,
        sample: {
          status: "loading",
          truncated: false,
          light: false,
          error: null,
        },
      };
    case "sample/loaded":
      return {
        ...state,
        phase: "ready",
        bootError: null,
        bootFailedAt: null,
        store: action.merge.store,
        sample: {
          status: "ready",
          truncated: action.truncated || action.merge.droppedNodes > 0,
          light: action.light,
          error: null,
        },
      };
    case "sample/failed":
      return {
        ...state,
        phase: action.failure.kind === "forbidden" ? "no-access" : "error",
        bootError: action.failure,
        bootFailedAt: "sample",
        sample: {
          status: "error",
          truncated: false,
          light: false,
          error: action.failure,
        },
      };
    case "merged": {
      const { merge } = action;
      const notices =
        merge.droppedNodes > 0
          ? withNotice(state.notices, capNotice(merge.droppedNodes, action.cap))
          : state.notices;
      if (merge.store === state.store && notices === state.notices)
        return state;
      // A failed first view, or an empty database the user has since written
      // to, becomes a usable canvas once anything lands on it (a label page,
      // a query result shown in the graph).
      const recovered =
        (state.phase === "error" || state.phase === "empty") &&
        merge.addedNodes.length > 0;
      return {
        ...state,
        store: merge.store,
        notices,
        ...(recovered
          ? { phase: "ready" as const, bootError: null, bootFailedAt: null }
          : {}),
      };
    }
    case "labelPage/loading": {
      const page = state.labelPages[action.label] ?? {
        nextAfter: null,
        exhausted: false,
        loaded: 0,
        status: "idle" as const,
        total: null,
      };
      return {
        ...state,
        labelPages: {
          ...state.labelPages,
          [action.label]: { ...page, status: "loading" },
        },
      };
    }
    case "labelPage/loaded": {
      const page = state.labelPages[action.label];
      return {
        ...state,
        labelPages: {
          ...state.labelPages,
          [action.label]: {
            nextAfter: action.nextAfter,
            exhausted: action.nextAfter === null,
            loaded: (page?.loaded ?? 0) + action.count,
            status: "idle",
            total: action.total ?? page?.total ?? null,
          },
        },
      };
    }
    case "labelPage/failed": {
      const page = state.labelPages[action.label];
      if (!page) return state;
      return {
        ...state,
        labelPages: {
          ...state.labelPages,
          [action.label]: { ...page, status: "error" },
        },
      };
    }
    case "degree/loading":
      return {
        ...state,
        degreeStatus: mapWith(state.degreeStatus, action.nodeId, "loading"),
      };
    case "degree/loaded":
      return {
        ...state,
        degrees: mapWith(
          state.degrees,
          action.nodeId,
          mergeDegree(
            state.degrees.get(action.nodeId),
            action.degree,
            action.hasLabels,
          ),
        ),
        degreeStatus: mapWith(state.degreeStatus, action.nodeId, undefined),
      };
    case "degree/failed":
      return {
        ...state,
        degreeStatus: mapWith(state.degreeStatus, action.nodeId, "error"),
      };
    case "probe/loaded": {
      if (action.probes.size === 0) return state;
      const probes = new Map(state.probes);
      // A node probed again (loaded by id, say) keeps the edge counts it had.
      for (const [id, probe] of action.probes)
        probes.set(id, { ...probes.get(id), ...probe });
      return { ...state, probes };
    }
    case "probe/edges": {
      // Edge counts only refine a neighbor probe; a node without one (removed meanwhile) is skipped.
      const probes = new Map(state.probes);
      let changed = false;
      for (const [id, edges] of action.edges) {
        const probe = probes.get(id);
        if (!probe) continue;
        probes.set(id, { ...probe, edges });
        changed = true;
      }
      return changed ? { ...state, probes } : state;
    }
    case "degree/cancelled": {
      if (state.degreeStatus.get(action.nodeId) !== "loading") return state;
      return {
        ...state,
        degreeStatus: mapWith(state.degreeStatus, action.nodeId, undefined),
      };
    }
    case "expansion": {
      const previous = state.expansions.get(action.key) ?? UNSTARTED_EXPANSION;
      const next: ExpansionState = {
        out: action.out ?? previous.out,
        in: action.in ?? previous.in,
      };
      if (
        next.out === previous.out &&
        next.in === previous.in &&
        state.expansions.has(action.key)
      )
        return state;
      return {
        ...state,
        expansions: mapWith(state.expansions, action.key, next),
      };
    }
    case "pending/start":
      return {
        ...state,
        pending: mapWith(state.pending, action.op, action.id),
      };
    case "pending/end":
      if (state.pending.get(action.op) !== action.id) return state;
      return {
        ...state,
        pending: mapWith(state.pending, action.op, undefined),
      };
    case "activity/start":
      return {
        ...state,
        activity: [action.entry, ...state.activity].slice(0, ACTIVITY_LIMIT),
      };
    case "activity/end": {
      const index = state.activity.findIndex((entry) => entry.id === action.id);
      if (index < 0) return state;
      const activity = [...state.activity];
      activity[index] = {
        ...activity[index],
        ok: action.ok,
        status: action.status,
        elapsedMs: action.elapsedMs,
      };
      return { ...state, activity };
    }
    case "nodes/hide": {
      const ids = action.ids.filter(
        (id) => state.store.nodes.has(id) && !state.hiddenNodes.has(id),
      );
      if (ids.length === 0) return state;
      return { ...state, hiddenNodes: new Set([...state.hiddenNodes, ...ids]) };
    }
    case "nodes/show": {
      const ids = action.ids.filter((id) => state.hiddenNodes.has(id));
      if (ids.length === 0) return state;
      const hiddenNodes = new Set(state.hiddenNodes);
      for (const id of ids) hiddenNodes.delete(id);
      return { ...state, hiddenNodes };
    }
    case "label/hidden": {
      const hiddenLabels = setWith(
        state.hiddenLabels,
        action.key,
        action.hidden,
      );
      return hiddenLabels === state.hiddenLabels
        ? state
        : { ...state, hiddenLabels };
    }
    case "hidden/reset":
      if (state.hiddenLabels.size === 0 && state.hiddenNodes.size === 0)
        return state;
      return { ...state, hiddenLabels: new Set(), hiddenNodes: new Set() };
    case "nodes/remove": {
      const store = removeNodes(state.store, action.ids);
      return store === state.store ? state : forgetNodes(state, store);
    }
    case "canvas/clear":
      return {
        ...forgetNodes(state, EMPTY_STORE),
        labelPages: {},
        expansions: new Map(),
        // The cap notice and the offers to connect or expand nodes are about nodes that just left.
        notices: state.notices.filter(
          (notice) => notice.id !== CAP_NOTICE_ID && !notice.action,
        ),
        sample: { ...state.sample, truncated: false },
      };
    case "pin": {
      if (action.pinned && !state.store.nodes.has(action.id)) return state;
      const pinned = setWith(state.pinned, action.id, action.pinned);
      return pinned === state.pinned ? state : { ...state, pinned };
    }
    case "notice/add":
      return { ...state, notices: withNotice(state.notices, action.notice) };
    case "notice/dismiss": {
      const notices = state.notices.filter((notice) => notice.id !== action.id);
      return notices.length === state.notices.length
        ? state
        : { ...state, notices };
    }
  }
}
