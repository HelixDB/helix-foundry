import type { JsonValue } from "../../database-query";

export type { JsonValue };

/**
 * One index of a database, as helix-website reads them from the WFE's index
 * catalog. Foundry has no catalog route, so its explorer runs without one:
 * search falls back to contains scans, and no index badges show.
 */
interface IndexCatalogEntryBase {
  indexId: string;
  label: string;
  property: string;
}
export type IndexCatalogEntry =
  | (IndexCatalogEntryBase & {
      target: "node";
      type: "equality";
      unique: boolean;
    })
  | (IndexCatalogEntryBase & { target: "edge"; type: "equality" })
  | (IndexCatalogEntryBase & {
      target: "node" | "edge";
      type: "range";
      direction: "ascending" | "descending";
    })
  | (IndexCatalogEntryBase & {
      target: "node" | "edge";
      type: "vector" | "full_text";
      tenantProperty: string | null;
    });

/**
 * A node as the explorer holds it. Node and edge ids are separate u64
 * sequences in v3, so the store keys them `n:<dbId>` / `e:<dbId>`.
 */
export interface GraphNode {
  /** Store key: `n:<dbId>`. */
  id: string;
  /** The database's u64 node id, as the JSON number v3 returns and accepts. */
  dbId: number;
  /** `$label`. */
  label: string;
  /** Stored properties without `$id` / `$label`. */
  properties: Record<string, JsonValue>;
  /**
   * Property keys left out of this fetch because their values are heavy
   * (vectors, huge strings). Empty when `complete`.
   */
  omitted: string[];
  /** True once every stored property has been fetched (value_map). */
  complete: boolean;
}

export interface GraphEdge {
  /** Store key: `e:<dbId>`. */
  id: string;
  dbId: number;
  label: string;
  /** Store key of `$from` (the edge's source node). */
  source: string;
  /** Store key of `$to` (the edge's target node). */
  target: string;
  properties: Record<string, JsonValue>;
}

export interface GraphData {
  nodes: GraphNode[];
  edges: GraphEdge[];
}

export type Selection =
  | { kind: "node"; id: string }
  | { kind: "edge"; id: string };

export type ElementTarget = "node" | "edge";

/**
 * Labels the explorer leaves out (Foundry's own, until the user shows them):
 * node labels starting with `nodePrefix` and edge labels starting with
 * `edgePrefix` are never counted, loaded, searched or expanded over, so the
 * label rail, the totals, the search and the canvas don't know them. The
 * reads of a node's edges leave them out themselves (`starts_with` on
 * `$label`). `notice` says why a node asked for by id doesn't show.
 */
export interface LabelExclusion {
  nodePrefix: string;
  edgePrefix: string;
  notice: string;
}

export interface LabelCount {
  label: string;
  /** null when unknown (counts timed out or were not requested). */
  count: number | null;
}

export interface LabelStats {
  nodeLabels: LabelCount[];
  edgeLabels: LabelCount[];
  /** Sum of the label counts, or the scanned total. A lower bound when `partial`. */
  nodeTotal: number | null;
  edgeTotal: number | null;
  /**
   * Set when the label list on a side may be missing labels: labels were
   * found by sampling (discovery, the index catalog, labels remembered from
   * earlier visits) rather than a full scan, or some were not counted. That
   * side's total is then "at least". Absent after a full label scan.
   */
  partial?: { nodes: boolean; edges: boolean };
}

export type ValueKind =
  | "string"
  | "integer"
  | "float"
  | "boolean"
  | "null"
  | "datetime"
  | "vector"
  | "array"
  | "object";

export type IndexKind =
  | "equality"
  | "unique"
  | "range"
  | "vector"
  | "full_text";

export interface PropertySchema {
  key: string;
  /** Kinds observed in the probe/sample, most frequent first. */
  kinds: ValueKind[];
  /** True when values are too large to fetch for every node (vectors, huge strings/objects). */
  heavy: boolean;
  /** Indexes on this label+property, from the index catalog. */
  indexes: IndexKind[];
  /** How many probed rows had this key. */
  seen: number;
}

export interface LabelSchema {
  target: ElementTarget;
  label: string;
  /** Rows probed to build this schema. */
  sampled: number;
  properties: PropertySchema[];
}

/**
 * Which properties to fetch for nodes. `keys: null` fetches everything
 * (value_map); otherwise only `$id`, `$label` and these keys (values).
 */
export interface NodeFetchShape {
  keys: string[] | null;
  /** Keys known to be heavy; recorded in GraphNode.omitted when a row is fetched without them. */
  heavyKeys: string[];
}

export interface DegreeInfo {
  outDegree: number;
  inDegree: number;
  outByLabel: LabelCount[];
  inByLabel: LabelCount[];
  /**
   * Sides whose count is a floor: the node has more relationships than the
   * explorer reads on its own (`EXPLORER_LIMITS.degreeThreshold`), shown as
   * "1,000+", with no per-label counts. "Count relationships" (explicit)
   * replaces them with exact ones.
   */
  atLeast?: { out: boolean; in: boolean };
}

/**
 * Distinct neighbors per direction from the cheap degree probe (no edges
 * read), capped at `degreeThreshold` + 1: a value over the threshold means
 * "more than that".
 */
export interface DegreeProbe {
  out: number;
  in: number;
  /**
   * Edges per direction, capped the same way: a bounded count of the node's
   * edges (`count(limit(out_e(n), 1001))`), read only once the neighbor
   * counts say it is small. Parallel edges (repeated transactions, events)
   * make it larger than the neighbor count, and connecting a node costs one
   * read per edge, so connects are planned with it.
   */
  edges?: { out: number; in: number };
}

export interface NeighborhoodResult {
  center: GraphNode | null;
  nodes: GraphNode[];
  edges: GraphEdge[];
  /** Always null: expansions don't count the degree (the degree probe does, without reading edges). */
  degree: DegreeInfo | null;
  /** True when the limit cut off edges in either direction. */
  truncated: boolean;
}

export interface SearchHit {
  node: GraphNode;
  via: "id" | "text" | "contains";
  /** The property that matched, when known. */
  matchedKey?: string;
  /** BM25 score for text hits. */
  score?: number;
}
