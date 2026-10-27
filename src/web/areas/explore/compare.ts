import type { DynamicQueryRequest } from "./database-query";
import { scopedLabel } from "./load-scope";

// Comparing two loads (?load=A&compare=B): what changed between them, read
// from the database. Counts per type and link come from bitmaps (no rows);
// a type's entities are compared only when the user opens it, by their keys
// (`entity_key`, or `norm_name` for a type known by its name), at most
// keysPerSide per load. A key renamed between versions (or a renamed type)
// reads as everything removed and everything added.

export const COMPARE_LIMITS = Object.freeze({
  // Keys read per load and type; over it, only counts are shown.
  keysPerSide: 5000,
  // Entities listed per kind of change.
  listed: 50,
});

export type Side = {
  nodes: Map<string, number | null>;
  edges: Map<string, number | null>;
};

/** One type or link in the count diff. */
export type CountRow = {
  target: "node" | "edge";
  label: string;
  a: number | null;
  b: number | null;
  // In both loads' versions, or only in the second ("added") or first ("removed").
  change: "both" | "added" | "removed";
};

/**
 * The count of each type and link in two loads, the ones only one version
 * names marked added or removed; types first, then links, by name.
 *
 * @example
 * countRows(
 *   { nodes: new Map([["A", 2]]), edges: new Map() },
 *   { nodes: new Map([["A", 3], ["B", 1]]), edges: new Map() },
 * ).map((r) => r.change); // ["both", "added"]
 */
export function countRows(a: Side, b: Side): CountRow[] {
  const rows = (target: "node" | "edge", x: Side["nodes"], y: Side["nodes"]) =>
    [...new Set([...x.keys(), ...y.keys()])].sort().map(
      (label): CountRow => ({
        target,
        label,
        a: x.get(label) ?? null,
        b: y.get(label) ?? null,
        change: !x.has(label) ? "added" : !y.has(label) ? "removed" : "both",
      }),
    );
  return [...rows("node", a.nodes, b.nodes), ...rows("edge", a.edges, b.edges)];
}

/**
 * A type's keys in one load: `entity_key` and `norm_name` of at most
 * keysPerSide + 1 of its nodes (the extra row says there are more), from the
 * type's bitmap in the load.
 */
export function keysRequest(load: string, type: string): DynamicQueryRequest {
  const root = {
    values: {
      input: {
        limit: {
          input: {
            nodes_where: {
              predicate: scopedLabel({ kind: "load", id: load }, "node", type),
            },
          },
          count: { literal: COMPARE_LIMITS.keysPerSide + 1 },
        },
      },
      properties: ["entity_key", "norm_name"],
    },
  };
  return {
    request_type: "read",
    query_name: "foundry_compare_keys",
    query: {
      read: { entries: [{ query: { name: "keys", root } }], returns: ["keys"] },
    },
    parameters: {},
    parameter_types: {},
  };
}

/** An entity in the diff: what finds it on the canvas, and its name. */
export type KeyItem = {
  property: "entity_key" | "norm_name";
  value: string;
  name: string;
};

/** A type's keys in one load, by identity; capped when there were more. */
export type Keys = { items: Map<string, KeyItem>; capped: boolean };

export function parseKeys(body: unknown): Keys {
  const rows =
    body &&
    typeof body === "object" &&
    Array.isArray((body as { keys?: unknown }).keys)
      ? ((body as { keys: unknown[] }).keys as unknown[])
      : [];
  const items = new Map<string, KeyItem>();
  for (const row of rows.slice(0, COMPARE_LIMITS.keysPerSide)) {
    if (!row || typeof row !== "object") continue;
    const r = row as Record<string, unknown>;
    const key = typeof r.entity_key === "string" ? r.entity_key : undefined;
    const norm = typeof r.norm_name === "string" ? r.norm_name : undefined;
    if (key !== undefined)
      items.set(`k:${key}`, {
        property: "entity_key",
        value: key,
        name: norm || key,
      });
    else if (norm !== undefined)
      items.set(`n:${norm}`, {
        property: "norm_name",
        value: norm,
        name: norm,
      });
  }
  return { items, capped: rows.length > COMPARE_LIMITS.keysPerSide };
}

/**
 * What changed in a type from load A to load B, by key: added (only in B),
 * removed (only in A) and changed (a keyed entity whose name differs), the
 * first `listed` of each by name, with the full counts. Null when either
 * side was capped: then only counts compare.
 */
export function diffKeys(
  a: Keys,
  b: Keys,
): {
  added: KeyItem[];
  removed: KeyItem[];
  changed: KeyItem[];
  counts: { added: number; removed: number; changed: number };
} | null {
  if (a.capped || b.capped) return null;
  const only = (x: Keys, y: Keys) =>
    [...x.items.entries()]
      .filter(([id]) => !y.items.has(id))
      .map(([, item]) => item);
  const added = only(b, a),
    removed = only(a, b);
  const changed = [...b.items.entries()].flatMap(([id, item]) => {
    const before = a.items.get(id);
    return before && item.property === "entity_key" && before.name !== item.name
      ? [item]
      : [];
  });
  const first = (list: KeyItem[]) =>
    [...list]
      .sort(
        (x, y) =>
          x.name.localeCompare(y.name) || x.value.localeCompare(y.value),
      )
      .slice(0, COMPARE_LIMITS.listed);
  return {
    added: first(added),
    removed: first(removed),
    changed: first(changed),
    counts: {
      added: added.length,
      removed: removed.length,
      changed: changed.length,
    },
  };
}
