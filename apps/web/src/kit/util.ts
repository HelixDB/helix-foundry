import { createContext, useContext, useEffect, useState } from "react";

export const cx = (...names: (string | false | null | undefined)[]) =>
  names.filter(Boolean).join(" ");

// Column categories derived from DuckDB / JSON types.
export type ColumnKind =
  | "string"
  | "integer"
  | "number"
  | "boolean"
  | "date"
  | "timestamp"
  | "json";
export function columnKind(type?: string): ColumnKind {
  const t = (type || "").toUpperCase();
  if (!t) return "string";
  if (/STRUCT|MAP|LIST|JSON|\[\]|UNION/.test(t)) return "json";
  if (/^(TINY|SMALL|BIG|HUGE|U)?INT|INTEGER|UBIGINT|USMALLINT/.test(t))
    return "integer";
  if (/DOUBLE|FLOAT|DECIMAL|NUMERIC|REAL/.test(t)) return "number";
  if (t.startsWith("BOOL")) return "boolean";
  if (t.startsWith("TIMESTAMP") || t === "DATETIME") return "timestamp";
  if (t === "DATE") return "date";
  return "string";
}
const isoDate = /^\d{4}-\d{2}-\d{2}(?:[T ][\d:.]+(?:Z|[+-]\d{2}:?\d{2})?)?$/;
// Guesses a kind from sample values when no schema is available.
export function inferKind(values: unknown[]): ColumnKind {
  const present = values.filter((v) => v !== null && v !== undefined);
  if (!present.length) return "string";
  if (present.every((v) => typeof v === "boolean")) return "boolean";
  if (present.every((v) => typeof v === "number" || typeof v === "bigint"))
    return present.every((v) => Number.isInteger(Number(v)))
      ? "integer"
      : "number";
  if (present.every((v) => typeof v === "object")) return "json";
  if (present.every((v) => typeof v === "string" && isoDate.test(v)))
    return present.some((v) => (v as string).length > 10)
      ? "timestamp"
      : "date";
  return "string";
}
const integer = new Intl.NumberFormat("en-US");
const decimal = new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 });
const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});
export const formatNumber = (n: number | bigint | string | null | undefined) =>
  n === null || n === undefined || n === ""
    ? "—"
    : Number.isInteger(Number(n))
      ? integer.format(
          typeof n === "bigint"
            ? n
            : typeof n === "string" && /^[+-]?\d+$/.test(n)
              ? BigInt(n)
              : Number(n),
        )
      : decimal.format(Number(n));
export const formatCompact = (n: number) =>
  Math.abs(n) < 10000 ? integer.format(n) : compact.format(n);
export function formatDate(value: unknown, withTime = true) {
  // "2026-09-16" is a calendar day: read it as local midnight, not UTC.
  const day =
    typeof value === "string" && /^\d{4}-\d{2}-\d{2}$/.test(value)
      ? value.split("-").map(Number)
      : undefined;
  const d = day
    ? new Date(day[0], day[1] - 1, day[2])
    : new Date(value as string);
  if (Number.isNaN(d.getTime())) return String(value);
  return withTime
    ? d.toLocaleString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      })
    : d.toLocaleDateString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
      });
}
export function formatRelative(
  value: string | number | Date,
  now = Date.now(),
) {
  const t = new Date(value).getTime();
  if (Number.isNaN(t)) return "";
  const s = Math.round((now - t) / 1000);
  if (s < 45) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.round(h / 24);
  if (d < 7) return `${d}d ago`;
  return formatDate(value, false);
}
// Display text for any cell value; null stays null so callers can style it.
export function formatValue(value: unknown, kind: ColumnKind): string | null {
  if (value === null || value === undefined) return null;
  switch (kind) {
    case "integer":
    case "number":
      return typeof value === "number" ||
        typeof value === "bigint" ||
        (typeof value === "string" && value.trim() !== "" && !isNaN(+value))
        ? formatNumber(value as number)
        : String(value);
    case "boolean":
      return String(value);
    case "date":
      return formatDate(value, false);
    case "timestamp":
      return formatDate(value, true);
    case "json":
      return typeof value === "string" ? value : JSON.stringify(value);
    default:
      return typeof value === "object" ? JSON.stringify(value) : String(value);
  }
}
export function compareValues(a: unknown, b: unknown, kind: ColumnKind) {
  const na = a === null || a === undefined,
    nb = b === null || b === undefined;
  if (na || nb) return na === nb ? 0 : na ? 1 : -1;
  if (kind === "integer") {
    const exact = (v: unknown) =>
      typeof v === "bigint" ||
      (typeof v === "number" && Number.isFinite(v) && Number.isInteger(v)) ||
      (typeof v === "string" && /^[+-]?\d+$/.test(v));
    if (exact(a) && exact(b)) {
      const x = BigInt(a as string | number | bigint),
        y = BigInt(b as string | number | bigint);
      return x < y ? -1 : x > y ? 1 : 0;
    }
  }
  if (kind === "integer" || kind === "number") return Number(a) - Number(b);
  if (kind === "date" || kind === "timestamp")
    return Date.parse(String(a)) - Date.parse(String(b));
  if (kind === "boolean") return Number(a) - Number(b);
  const sa = typeof a === "object" ? JSON.stringify(a) : String(a),
    sb = typeof b === "object" ? JSON.stringify(b) : String(b);
  return sa.localeCompare(sb, undefined, { numeric: true });
}
export const plural = (n: number, one: string, many = one + "s") =>
  `${formatNumber(n)} ${n === 1 ? one : many}`;

// Storage can be unavailable (private mode, blocked); callers keep working.
export function readStorage(key: string, fallback = "") {
  try {
    return localStorage.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}
export function writeStorage(key: string, value: string | null) {
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    /* ignored: preference only */
  }
}
export function useStoredState(key: string, fallback: string) {
  const [value, setValue] = useState(() => readStorage(key, fallback));
  useEffect(() => writeStorage(key, value), [key, value]);
  return [value, setValue] as const;
}
// Told about every page title; the shell uses it for "recently visited".
export const PageVisitContext = createContext<(title: string) => void>(
  () => {},
);
export function useTitle(title?: string) {
  const visit = useContext(PageVisitContext);
  useEffect(() => {
    if (!title) return;
    const previous = document.title;
    document.title = `${title} · Helix Foundry`;
    visit(title);
    return () => {
      document.title = previous;
    };
  }, [title]);
}
// Tracks a media query, e.g. "(max-width: 800px)".
export function useMedia(query: string) {
  const get = () =>
    typeof window !== "undefined" && !!window.matchMedia?.(query).matches;
  const [matches, setMatches] = useState(get);
  useEffect(() => {
    const list = window.matchMedia?.(query);
    if (!list) return;
    const change = () => setMatches(list.matches);
    change();
    list.addEventListener("change", change);
    return () => list.removeEventListener("change", change);
  }, [query]);
  return matches;
}
// Re-renders every `ms` so relative times stay fresh.
export function useNow(ms = 30000) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [ms]);
  return now;
}
