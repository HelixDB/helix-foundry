// Per-database explorer preferences in localStorage. Every access is wrapped:
// storage can be missing (SSR), blocked (privacy settings) or full, and a
// preference that can't be read or saved is simply not remembered.

import type { DatabaseQueryTarget } from "../../database-query";

/** `foundry.explorer.tenant:<id>`: one namespace per database. */
export function explorerStorageKey(target: DatabaseQueryTarget): string {
  return `foundry.explorer.tenant:${target.tenantId}`;
}

export function readStored<T>(
  key: string,
  validate: (value: unknown) => T | null,
): T | null {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return null;
    return validate(JSON.parse(raw));
  } catch {
    return null;
  }
}

export function writeStored(key: string, value: unknown): void {
  try {
    if (value === undefined || value === null)
      window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Not remembered; nothing else depends on it.
  }
}

/** Whether Foundry's own labels show: one choice per browser, every database. */
export const SHOW_INTERNALS_KEY = "foundry.explorer.showInternals";

export function asBoolean(value: unknown): boolean | null {
  return typeof value === "boolean" ? value : null;
}

export function asStringArray(value: unknown): string[] | null {
  return Array.isArray(value) && value.every((item) => typeof item === "string")
    ? value
    : null;
}

export function asStringRecord(value: unknown): Record<string, string> | null {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return null;
  const entries = Object.entries(value).filter(
    (entry): entry is [string, string] => typeof entry[1] === "string",
  );
  return Object.fromEntries(entries);
}

export interface StoredPane {
  open: boolean;
  width: number;
}

export function asPane(value: unknown): StoredPane | null {
  if (value === null || typeof value !== "object") return null;
  const { open, width } = value as Record<string, unknown>;
  if (
    typeof open !== "boolean" ||
    typeof width !== "number" ||
    !Number.isFinite(width)
  )
    return null;
  return { open, width };
}

// ============ Remembered labels ============
//
// No query lists a database's labels without reading the whole graph, so the
// explorer remembers every label it has seen, per database, and counts them
// (cheaply, from the label bitmaps) on the next visit.

export interface RememberedLabels {
  nodes: string[];
  edges: string[];
}

/** Labels kept per side; a database with more is unusual and the rest are rediscovered. */
export const REMEMBERED_LABELS_MAX = 200;

export function labelsStorageKey(storageKey: string): string {
  return `${storageKey}.labels`;
}

export function asRememberedLabels(value: unknown): RememberedLabels | null {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    return null;
  const { nodes, edges } = value as Record<string, unknown>;
  const list = (items: unknown) =>
    (asStringArray(items) ?? []).filter((label) => label !== "");
  return { nodes: list(nodes), edges: list(edges) };
}

export function readRememberedLabels(storageKey: string): RememberedLabels {
  return (
    readStored(labelsStorageKey(storageKey), asRememberedLabels) ?? {
      nodes: [],
      edges: [],
    }
  );
}

/**
 * Adds labels to the remembered ones and drops `forget` (labels counted at
 * zero). `replace` starts from nothing (after a full scan, which lists
 * every label). Writes only when something changed.
 */
export function rememberLabels(
  storageKey: string,
  seen: Partial<RememberedLabels>,
  opts: { forget?: Partial<RememberedLabels>; replace?: boolean } = {},
): RememberedLabels {
  const current = opts.replace
    ? { nodes: [], edges: [] }
    : readRememberedLabels(storageKey);
  const side = (
    existing: string[],
    add: string[] = [],
    drop: string[] = [],
  ) => {
    const removed = new Set(drop);
    return [...new Set([...existing, ...add])]
      .filter((label) => label !== "" && !removed.has(label))
      .sort((a, b) => a.localeCompare(b))
      .slice(0, REMEMBERED_LABELS_MAX);
  };
  const next = {
    nodes: side(current.nodes, seen.nodes, opts.forget?.nodes),
    edges: side(current.edges, seen.edges, opts.forget?.edges),
  };
  const before = readRememberedLabels(storageKey);
  const same = (a: string[], b: string[]) =>
    a.length === b.length && a.every((label, index) => label === b[index]);
  if (!same(before.nodes, next.nodes) || !same(before.edges, next.edges)) {
    writeStored(labelsStorageKey(storageKey), next);
  }
  return next;
}
