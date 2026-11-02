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
