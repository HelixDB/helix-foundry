import type { IndexCatalogEntry } from "./types";

import { classifyValue, isRecord } from "./normalize";
import type {
  ElementTarget,
  GraphData,
  IndexKind,
  JsonValue,
  LabelSchema,
  PropertySchema,
  ValueKind,
} from "./types";

/**
 * Schema inference. v3 has no introspection API, so label schemas come from
 * probed rows (a couple of full rows per label), the index catalog, and the
 * nodes and edges the explorer has loaded.
 */

/** Strings longer than this are too heavy to fetch for every node. */
export const HEAVY_STRING_LENGTH = 2048;
/** Objects and non-vector arrays whose JSON is longer than this are heavy. */
export const HEAVY_JSON_LENGTH = 4096;
/**
 * Keys that usually hold long text. One short probed value is not proof the
 * next node's is short too, so these are heavy from a lower threshold.
 */
const LONG_TEXT_KEY =
  /(^|_)(body|content|text|html|markdown|raw|transcript|chunk)$/i;
const LONG_TEXT_HINT_LENGTH = 512;

export function isHeavyValue(key: string, value: JsonValue): boolean {
  if (typeof value === "string") {
    if (value.length > HEAVY_STRING_LENGTH) return true;
    return value.length > LONG_TEXT_HINT_LENGTH && LONG_TEXT_KEY.test(key);
  }
  if (value === null || typeof value !== "object") return false;
  if (classifyValue(value) === "vector") return true;
  // Cheap upper bound before paying for JSON.stringify.
  if (Array.isArray(value) && value.length > HEAVY_JSON_LENGTH / 2) return true;
  return JSON.stringify(value).length > HEAVY_JSON_LENGTH;
}

function indexKind(entry: IndexCatalogEntry): IndexKind {
  if (entry.type === "equality") {
    return entry.target === "node" && entry.unique ? "unique" : "equality";
  }
  return entry.type;
}

/** Index kinds per `target:label` and property, from the catalog. */
export function indexesByProperty(
  entries: readonly IndexCatalogEntry[] = [],
): Map<string, Map<string, IndexKind[]>> {
  const result = new Map<string, Map<string, IndexKind[]>>();
  for (const entry of entries) {
    const scope = `${entry.target}:${entry.label}`;
    const byProperty = result.get(scope) ?? new Map<string, IndexKind[]>();
    const kinds = byProperty.get(entry.property) ?? [];
    const kind = indexKind(entry);
    if (!kinds.includes(kind)) kinds.push(kind);
    byProperty.set(entry.property, kinds);
    result.set(scope, byProperty);
  }
  return result;
}

interface PropertyTally {
  kinds: Map<ValueKind, number>;
  seen: number;
  heavy: boolean;
}

/** Accumulates observed rows for one label into a `LabelSchema`. */
export class LabelSchemaBuilder {
  private readonly tallies = new Map<string, PropertyTally>();
  private sampled = 0;

  constructor(
    readonly target: ElementTarget,
    readonly label: string,
  ) {}

  /** Adds one row's stored properties (`$`-prefixed keys are ignored). */
  addRow(row: Record<string, unknown>, omitted: readonly string[] = []): void {
    this.sampled += 1;
    for (const [key, value] of Object.entries(row)) {
      if (key.startsWith("$") || value === undefined) continue;
      const tally = this.tally(key);
      tally.seen += 1;
      const kind = classifyValue(value as JsonValue);
      tally.kinds.set(kind, (tally.kinds.get(kind) ?? 0) + 1);
      if (isHeavyValue(key, value as JsonValue)) tally.heavy = true;
    }
    // Keys a fetch left out on purpose are still present on the element.
    for (const key of omitted) {
      if (key in row) continue;
      const tally = this.tally(key);
      tally.seen += 1;
      tally.heavy = true;
    }
  }

  build(indexes?: Map<string, IndexKind[]>): LabelSchema {
    const properties: PropertySchema[] = [...this.tallies.entries()].map(
      ([key, tally]) => propertySchema(key, tally, indexes?.get(key)),
    );
    // Indexed properties the rows did not show still exist in the schema.
    for (const [key, kinds] of indexes ?? []) {
      if (this.tallies.has(key)) continue;
      properties.push({
        key,
        kinds: kinds.includes("vector") ? ["vector"] : [],
        heavy: kinds.includes("vector"),
        indexes: kinds,
        seen: 0,
      });
    }
    properties.sort((a, b) => b.seen - a.seen || a.key.localeCompare(b.key));
    return {
      target: this.target,
      label: this.label,
      sampled: this.sampled,
      properties,
    };
  }

  private tally(key: string): PropertyTally {
    let tally = this.tallies.get(key);
    if (!tally) {
      tally = { kinds: new Map(), seen: 0, heavy: false };
      this.tallies.set(key, tally);
    }
    return tally;
  }
}

function propertySchema(
  key: string,
  tally: PropertyTally,
  indexes: IndexKind[] = [],
): PropertySchema {
  const kinds = [...tally.kinds.entries()]
    .sort((a, b) => b[1] - a[1])
    .map(([kind]) => kind);
  return {
    key,
    kinds,
    heavy: tally.heavy || indexes.includes("vector"),
    indexes,
    seen: tally.seen,
  };
}

function mergeKinds(primary: ValueKind[], secondary: ValueKind[]): ValueKind[] {
  return [...new Set([...primary, ...secondary])];
}

function mergeLabelSchema(
  probed: LabelSchema,
  loaded: LabelSchema,
): LabelSchema {
  const byKey = new Map(
    probed.properties.map((property) => [property.key, property]),
  );
  const properties: PropertySchema[] = [];
  for (const property of loaded.properties) {
    const before = byKey.get(property.key);
    byKey.delete(property.key);
    if (!before) {
      properties.push(property);
      continue;
    }
    properties.push({
      key: property.key,
      kinds: mergeKinds(property.kinds, before.kinds),
      heavy: before.heavy || property.heavy,
      indexes: before.indexes.length ? before.indexes : property.indexes,
      seen: Math.max(before.seen, property.seen),
    });
  }
  properties.push(...byKey.values());
  properties.sort((a, b) => b.seen - a.seen || a.key.localeCompare(b.key));
  return {
    target: probed.target,
    label: probed.label,
    // Probe rows are usually among the loaded elements, so counts overlap;
    // the larger set is the better fill-rate denominator.
    sampled: Math.max(probed.sampled, loaded.sampled),
    properties,
  };
}

/**
 * Label schemas from the loaded graph, merged over the probed schemas (which
 * keep their heavy flags, index badges, and keys the loaded rows omitted).
 * Node schemas come first, each group sorted by label.
 */
export function inferLabelSchemas(
  graph: GraphData,
  probed: readonly LabelSchema[] = [],
  indexes?: readonly IndexCatalogEntry[],
): LabelSchema[] {
  const catalog = indexesByProperty(indexes);
  const builders = new Map<string, LabelSchemaBuilder>();
  const builder = (target: ElementTarget, label: string) => {
    const scope = `${target}:${label}`;
    let existing = builders.get(scope);
    if (!existing) {
      existing = new LabelSchemaBuilder(target, label);
      builders.set(scope, existing);
    }
    return existing;
  };
  for (const node of graph.nodes)
    builder("node", node.label).addRow(node.properties, node.omitted);
  for (const edge of graph.edges)
    builder("edge", edge.label).addRow(edge.properties);

  const result = new Map<string, LabelSchema>();
  for (const schema of probed)
    result.set(`${schema.target}:${schema.label}`, schema);
  for (const [scope, entry] of builders) {
    const loaded = entry.build(catalog.get(scope));
    const before = result.get(scope);
    result.set(scope, before ? mergeLabelSchema(before, loaded) : loaded);
  }
  return [...result.values()].sort(
    (a, b) =>
      (a.target === b.target ? 0 : a.target === "node" ? -1 : 1) ||
      a.label.localeCompare(b.label),
  );
}

export interface EdgeEndpointPair {
  sourceLabel: string;
  targetLabel: string;
  /** Loaded edges with this label between these node labels. */
  count: number;
}

export interface EdgeLabelPairs {
  edgeLabel: string;
  /** Loaded edges with this label (including ones whose endpoints are not loaded). */
  count: number;
  /** Most common pair first. */
  pairs: EdgeEndpointPair[];
}

/** Which node labels each edge label connects, as seen in the loaded graph. */
export function edgeLabelPairs(graph: GraphData): EdgeLabelPairs[] {
  const labelOf = new Map(graph.nodes.map((node) => [node.id, node.label]));
  const byEdgeLabel = new Map<
    string,
    { count: number; pairs: Map<string, EdgeEndpointPair> }
  >();
  for (const edge of graph.edges) {
    let entry = byEdgeLabel.get(edge.label);
    if (!entry) {
      entry = { count: 0, pairs: new Map() };
      byEdgeLabel.set(edge.label, entry);
    }
    entry.count += 1;
    const sourceLabel = labelOf.get(edge.source);
    const targetLabel = labelOf.get(edge.target);
    if (sourceLabel === undefined || targetLabel === undefined) continue;
    const key = JSON.stringify([sourceLabel, targetLabel]);
    const pair = entry.pairs.get(key) ?? { sourceLabel, targetLabel, count: 0 };
    pair.count += 1;
    entry.pairs.set(key, pair);
  }
  return [...byEdgeLabel.entries()]
    .map(([edgeLabel, entry]) => ({
      edgeLabel,
      count: entry.count,
      pairs: [...entry.pairs.values()].sort(
        (a, b) =>
          b.count - a.count ||
          a.sourceLabel.localeCompare(b.sourceLabel) ||
          a.targetLabel.localeCompare(b.targetLabel),
      ),
    }))
    .sort((a, b) => a.edgeLabel.localeCompare(b.edgeLabel));
}

export interface LabelRelationship {
  edgeLabel: string;
  /** "out": this label is the source; "in": this label is the target. */
  direction: "out" | "in";
  /** Labels on the other end, most common first. */
  otherLabels: string[];
  count: number;
}

/**
 * The relationships a node label takes part in, for the label inspector.
 * A self-referencing edge label (User FOLLOWS User) appears once per direction.
 */
export function relationshipsForLabel(
  pairs: readonly EdgeLabelPairs[],
  label: string,
): LabelRelationship[] {
  const result: LabelRelationship[] = [];
  for (const entry of pairs) {
    for (const direction of ["out", "in"] as const) {
      const matching = entry.pairs.filter((pair) =>
        direction === "out"
          ? pair.sourceLabel === label
          : pair.targetLabel === label,
      );
      if (matching.length === 0) continue;
      result.push({
        edgeLabel: entry.edgeLabel,
        direction,
        otherLabels: matching.map((pair) =>
          direction === "out" ? pair.targetLabel : pair.sourceLabel,
        ),
        count: matching.reduce((sum, pair) => sum + pair.count, 0),
      });
    }
  }
  return result.sort(
    (a, b) => b.count - a.count || a.edgeLabel.localeCompare(b.edgeLabel),
  );
}

/** Probe rows for one label, keyed by `$label` so a stray row cannot mislabel a schema. */
export function groupRowsByLabel(
  rows: unknown[],
): Map<string, Record<string, unknown>[]> {
  const result = new Map<string, Record<string, unknown>[]>();
  for (const row of rows) {
    if (!isRecord(row) || typeof row.$label !== "string") continue;
    const list = result.get(row.$label) ?? [];
    list.push(row);
    result.set(row.$label, list);
  }
  return result;
}
