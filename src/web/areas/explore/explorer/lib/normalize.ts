import type {
  GraphData,
  GraphEdge,
  GraphNode,
  JsonValue,
  NodeFetchShape,
  ValueKind,
} from "./types";

/**
 * Turns v3 response rows into explorer graph elements. Every parser here is
 * tolerant: rows it does not recognize are skipped, nothing throws.
 *
 * Node rows are `{ $id, $label, ...properties }` (value_map / values). Edge
 * rows from edge_properties also carry `$from` (source) and `$to` (target).
 */

/** Numeric arrays longer than this are vectors (embeddings), not lists. */
export const VECTOR_MIN_LENGTH = 32;

/** Most property keys in one `values` projection. */
export const MAX_SHAPE_KEYS = 200;

/**
 * A fetch shape as `parseSchemaProbe` builds it. The extra per-label maps are
 * optional, so any `NodeFetchShape` is accepted wherever this one is.
 */
export interface ProbedFetchShape extends NodeFetchShape {
  /** Heavy keys per probed label; labels missing here were not probed. */
  heavyByLabel?: Record<string, string[]>;
  /** Light keys per probed label, for requests scoped to one label. */
  keysByLabel?: Record<string, string[]>;
  /**
   * Edge property keys to fetch alongside `$id`, `$label`, `$from` and `$to`.
   * Null or missing fetches every edge property (edge_properties); a list is
   * used once some edge label is known to carry heavy values.
   */
  edgeKeys?: string[] | null;
}

/**
 * The keys to fetch for nodes of one label: its own light keys (which may be
 * heavy on another label) plus the shared keys, in case the probe rows of
 * this label lacked a sparse property other labels showed.
 */
export function keysForLabel(
  shape: NodeFetchShape,
  label: string,
): string[] | null {
  if (shape.keys === null) return null;
  const own = (shape as ProbedFetchShape).keysByLabel?.[label];
  if (!own) return shape.keys;
  // Shared keys are light on every probed label.
  return [...new Set([...own, ...shape.keys])].slice(0, MAX_SHAPE_KEYS);
}

/** How a row was fetched: with the shape's shared keys, or its label's own keys (label-scoped requests). */
export interface RowFetch {
  scoped?: boolean;
}

export function nodeKey(dbId: number): string {
  return `n:${dbId}`;
}

export function edgeKey(dbId: number): string {
  return `e:${dbId}`;
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

/** v3 ids are u64 sent as JSON numbers; anything else is not an id. */
export function toDbId(value: unknown): number | null {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0
    ? value
    : null;
}

/** Rows of a returned name: a list, a single row, or null (empty at-most-one). */
export function rowsOf(value: unknown): unknown[] {
  if (Array.isArray(value)) return value;
  if (isRecord(value)) return [value];
  return [];
}

function properties(row: Record<string, unknown>): Record<string, JsonValue> {
  const result: Record<string, JsonValue> = {};
  for (const [key, value] of Object.entries(row)) {
    // `$id`, `$label`, `$from`, `$to`, `$score`, `$distance` and other
    // virtual sources are not stored properties.
    if (key.startsWith("$") || value === undefined) continue;
    result[key] = value as JsonValue;
  }
  return result;
}

function endpoint(
  row: Record<string, unknown>,
  side: "$from" | "$to",
): number | null {
  const direct = row[side];
  if (isRecord(direct)) return toDbId(direct.$id);
  return toDbId(direct) ?? toDbId(row[`${side}.$id`]);
}

function isEdgeRow(row: Record<string, unknown>): boolean {
  return (
    "$from" in row || "$to" in row || "$from.$id" in row || "$to.$id" in row
  );
}

/**
 * Keys this node is known to have that the fetch left out: its label's heavy
 * keys, plus its own light keys the projection did not list (a key that is
 * heavy on another label is left out of mixed-label streams).
 */
function omittedKeys(
  label: string,
  shape: NodeFetchShape,
  fetch: RowFetch,
): string[] {
  if (shape.keys === null) return [];
  const probed = shape as ProbedFetchShape;
  const heavy = probed.heavyByLabel
    ? (probed.heavyByLabel[label] ?? [])
    : shape.heavyKeys;
  const own = probed.keysByLabel?.[label];
  if (!own?.length) return heavy;
  const fetched = new Set(
    (fetch.scoped ? keysForLabel(shape, label) : shape.keys) ?? [],
  );
  const missing = own.filter((key) => !fetched.has(key));
  return missing.length ? [...new Set([...heavy, ...missing])] : heavy;
}

export function toGraphNode(
  row: unknown,
  shape: NodeFetchShape,
  fetch: RowFetch = {},
): GraphNode | null {
  if (!isRecord(row) || isEdgeRow(row)) return null;
  const dbId = toDbId(row.$id);
  const label = row.$label;
  if (dbId === null || typeof label !== "string") return null;
  const props = properties(row);
  return {
    id: nodeKey(dbId),
    dbId,
    label,
    properties: props,
    omitted: omittedKeys(label, shape, fetch).filter((key) => !(key in props)),
    complete: shape.keys === null,
  };
}

export function toGraphEdge(row: unknown): GraphEdge | null {
  if (!isRecord(row)) return null;
  const dbId = toDbId(row.$id);
  const label = row.$label;
  const from = endpoint(row, "$from");
  const to = endpoint(row, "$to");
  if (
    dbId === null ||
    typeof label !== "string" ||
    from === null ||
    to === null
  ) {
    return null;
  }
  return {
    id: edgeKey(dbId),
    dbId,
    label,
    source: nodeKey(from),
    target: nodeKey(to),
    properties: properties(row),
  };
}

/** Parses node rows, dropping unknown rows and later duplicates. */
export function toGraphNodes(
  rows: unknown,
  shape: NodeFetchShape,
  fetch: RowFetch = {},
): GraphNode[] {
  const nodes: GraphNode[] = [];
  const seen = new Set<string>();
  for (const row of rowsOf(rows)) {
    const node = toGraphNode(row, shape, fetch);
    if (!node || seen.has(node.id)) continue;
    seen.add(node.id);
    nodes.push(node);
  }
  return nodes;
}

/** Parses edge rows from one or more returned names, deduplicated by edge id. */
export function toGraphEdges(...sources: unknown[]): GraphEdge[] {
  const edges: GraphEdge[] = [];
  const seen = new Set<string>();
  for (const source of sources) {
    for (const row of rowsOf(source)) {
      const edge = toGraphEdge(row);
      if (!edge || seen.has(edge.id)) continue;
      seen.add(edge.id);
      edges.push(edge);
    }
  }
  return edges;
}

/** The node id behind a store key (`n:12` → 12). */
export function dbIdOfKey(key: string): number | null {
  const match = /^[ne]:(\d+)$/.exec(key);
  return match ? toDbId(Number(match[1])) : null;
}

const MAX_WALK_DEPTH = 8;
const MAX_WALK_ROWS = 20_000;

/**
 * Collects the nodes and edges inside an arbitrary query result (the dock's
 * "Show in graph"). The body is an object keyed by return names whose values
 * are rows, lists of rows, or nested projections. Returns null when nothing
 * graph-like is found. `missingNodeDbIds` are edge endpoints the result did
 * not include, so the caller can fetch them.
 *
 * An edge row without `$from`/`$to` (value_map or values over edges) looks
 * like a node row, and edge ids overlap node ids. Pass the database's known
 * labels so such rows are skipped (counted in `skippedEdgeRows`) instead of
 * becoming phantom nodes: a row whose label is a known edge label and not a
 * known node label is an edge.
 */
export function extractGraphFromResult(
  body: unknown,
  labels: { nodeLabels?: Iterable<string>; edgeLabels?: Iterable<string> } = {},
): {
  graph: GraphData;
  missingNodeDbIds: number[];
  skippedEdgeRows: number;
} | null {
  const nodes = new Map<string, GraphNode>();
  const edges = new Map<string, GraphEdge>();
  const nodeLabels = new Set(labels.nodeLabels ?? []);
  const edgeLabels = new Set(labels.edgeLabels ?? []);
  const isEdgeLabel = (label: unknown) =>
    typeof label === "string" &&
    edgeLabels.has(label) &&
    !nodeLabels.has(label);
  let visited = 0;
  let skippedEdgeRows = 0;
  // Rows from arbitrary queries may hold any subset of a node's properties.
  const shape: NodeFetchShape = { keys: [], heavyKeys: [] };

  const walk = (value: unknown, depth: number) => {
    if (depth > MAX_WALK_DEPTH || visited > MAX_WALK_ROWS) return;
    if (Array.isArray(value)) {
      for (const item of value) walk(item, depth + 1);
      return;
    }
    if (!isRecord(value)) return;
    visited += 1;
    if ("$id" in value && "$label" in value) {
      const edge = toGraphEdge(value);
      if (edge) {
        if (!edges.has(edge.id)) edges.set(edge.id, edge);
        return;
      }
      if (isEdgeLabel(value.$label) && toDbId(value.$id) !== null) {
        skippedEdgeRows += 1;
        return;
      }
      const node = toGraphNode(value, shape);
      if (node) {
        const existing = nodes.get(node.id);
        if (!existing) nodes.set(node.id, node);
        // Two rows with one id but different labels are not the same element; keep the first.
        else if (existing.label === node.label) {
          nodes.set(node.id, {
            ...existing,
            properties: { ...existing.properties, ...node.properties },
          });
        }
        return;
      }
    }
    for (const child of Object.values(value)) walk(child, depth + 1);
  };

  walk(body, 0);
  if (nodes.size === 0 && edges.size === 0 && skippedEdgeRows === 0)
    return null;

  const missing = new Set<number>();
  for (const edge of edges.values()) {
    for (const key of [edge.source, edge.target]) {
      if (nodes.has(key)) continue;
      const dbId = dbIdOfKey(key);
      if (dbId !== null) missing.add(dbId);
    }
  }
  return {
    graph: { nodes: [...nodes.values()], edges: [...edges.values()] },
    missingNodeDbIds: [...missing].sort((a, b) => a - b),
    skippedEdgeRows,
  };
}

const CAPTION_KEYS = [
  "name",
  "title",
  "label",
  "display_name",
  "displayname",
  "full_name",
  "username",
  "handle",
  "email",
  "slug",
  "key",
];
const ID_LIKE_KEY = /(^|_)(id|uuid|key)$|[a-z](Id|ID|Uuid)$/;
const MAX_CAPTION_SOURCE = 160;
const MAX_CAPTION = 48;

function captionValue(value: JsonValue | undefined): string | null {
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  if (typeof value !== "string") return null;
  const text = value.replace(/\s+/g, " ").trim();
  return text && text.length <= MAX_CAPTION_SOURCE ? text : null;
}

function truncateCaption(text: string): string {
  return text.length > MAX_CAPTION
    ? `${text.slice(0, MAX_CAPTION - 1).trimEnd()}…`
    : text;
}

/**
 * A short display name for a node: the preferred key when it holds a short
 * value, then common naming keys (case-insensitive), then id-like strings,
 * else `Label #id`.
 */
export function captionFor(
  node: GraphNode,
  preferredKey?: string | null,
): string {
  const props = node.properties;
  if (preferredKey) {
    const preferred = captionValue(props[preferredKey]);
    if (preferred) return truncateCaption(preferred);
  }
  const byLowerKey = new Map<string, string>();
  for (const key of Object.keys(props)) {
    const lower = key.toLowerCase();
    if (!byLowerKey.has(lower)) byLowerKey.set(lower, key);
  }
  for (const candidate of CAPTION_KEYS) {
    const key = byLowerKey.get(candidate);
    if (key === undefined || typeof props[key] !== "string") continue;
    const text = captionValue(props[key]);
    if (text) return truncateCaption(text);
  }
  for (const [key, value] of Object.entries(props)) {
    if (!ID_LIKE_KEY.test(key) || typeof value !== "string") continue;
    const text = captionValue(value);
    if (text) return truncateCaption(text);
  }
  return `${node.label} #${node.dbId}`;
}

const ISO_DATETIME =
  /^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2}(\.\d{1,9})?)?(Z|[+-]\d{2}:?\d{2})$/;

/**
 * The display kind of a property value. v3 returns untagged JSON, so a
 * date_time is an ISO string and an f64 that happens to be whole is an
 * integer; this is a best guess from the value alone.
 */
export function classifyValue(value: JsonValue): ValueKind {
  if (value === null) return "null";
  switch (typeof value) {
    case "boolean":
      return "boolean";
    case "number":
      return Number.isInteger(value) ? "integer" : "float";
    case "string":
      return ISO_DATETIME.test(value) && !Number.isNaN(Date.parse(value))
        ? "datetime"
        : "string";
  }
  if (Array.isArray(value)) {
    return value.length > VECTOR_MIN_LENGTH &&
      value.every((item) => typeof item === "number")
      ? "vector"
      : "array";
  }
  return "object";
}
