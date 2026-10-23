// Area schemas for Data Connection: sources, syncs, datasets, data health.
// Owned by the data area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index.js" would be circular.
// Type-only imports from "./index.js" are fine.
import { z } from "zod";

export const sourceKinds = [
  "upload",
  "postgres",
  "mysql",
  "neo4j",
  "sqlite",
  "rest",
  "s3",
  "stripe",
  "workos",
  "posthog",
] as const;
export const SourceSchema = z.object({
  name: z.string().min(1).max(120),
  kind: z.enum(sourceKinds),
  config: z.record(z.string(), z.unknown()).default({}),
  schedule: z
    .string()
    .nullable()
    .optional()
    .describe(
      "UTC snapshot fallback schedule. Working PostgreSQL/MySQL CDC takes precedence; defaults to every five minutes when CDC is unavailable, hourly for Stripe and WorkOS, and every six hours for PostHog.",
    ),
});
// What the source routes take. Files go through the upload endpoint instead.
export type ConnectedSourceInput = z.input<typeof SourceSchema> & {
  kind: Exclude<(typeof sourceKinds)[number], "upload">;
};
export type SourceSyncState = {
  mode: "auto" | "cdc" | "snapshot";
  status: "checking" | "starting" | "live" | "retrying" | "ready";
  reason?: string | null;
  lastEvent?: string;
  changes?: number;
  applied?: number;
};

// A DuckDB INTERVAL arrives as {months, days, micros}, micros as text since
// it can pass 2^53. It reads as DuckDB writes it ("1 year 2 months 3 days
// 04:05:06.789", "-01:30:00") and orders as DuckDB's ORDER BY does: whole
// days of micros carry into days, then whole 30 days into months, and
// months, then days, then micros compare.
export type IntervalValue = {
  months: number;
  days: number;
  micros: string | number | bigint;
};
export function isInterval(v: unknown): v is IntervalValue {
  if (!v || typeof v !== "object" || Array.isArray(v)) return false;
  const { months, days, micros } = v as Record<string, unknown>;
  return (
    Object.keys(v).length === 3 &&
    Number.isInteger(months) &&
    Number.isInteger(days) &&
    (typeof micros === "bigint" ||
      Number.isInteger(micros) ||
      (typeof micros === "string" && /^-?\d+$/.test(micros)))
  );
}
const unit = (n: number, name: string) =>
  `${n} ${name}${n === 1 || n === -1 ? "" : "s"}`;
export function formatInterval(v: IntervalValue) {
  const years = Math.trunc(v.months / 12),
    months = v.months - years * 12,
    parts: string[] = [];
  if (years) parts.push(unit(years, "year"));
  if (months) parts.push(unit(months, "month"));
  if (v.days) parts.push(unit(v.days, "day"));
  let micros = BigInt(v.micros);
  if (micros || !parts.length) {
    const sign = micros < 0n ? "-" : "";
    if (micros < 0n) micros = -micros;
    const two = (n: bigint) => String(n).padStart(2, "0"),
      fraction = micros % 1_000_000n;
    parts.push(
      `${sign}${two(micros / 3_600_000_000n)}:${two((micros / 60_000_000n) % 60n)}:${two((micros / 1_000_000n) % 60n)}` +
        (fraction
          ? "." + String(fraction).padStart(6, "0").replace(/0+$/, "")
          : ""),
    );
  }
  return parts.join(" ");
}
const DAY = 86_400_000_000n;
// BigInt division truncates toward zero, as DuckDB's does.
function normalized(v: IntervalValue) {
  const micros = BigInt(v.micros),
    days = BigInt(v.days) + micros / DAY;
  return [BigInt(v.months) + days / 30n, days % 30n, micros % DAY];
}
export function compareIntervals(a: IntervalValue, b: IntervalValue) {
  const x = normalized(a),
    y = normalized(b);
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] < y[i] ? -1 : 1;
  return 0;
}
