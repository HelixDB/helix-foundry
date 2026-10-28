// Area schemas for Data Connection: sources, syncs, datasets, data health.
// Owned by the data area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index" would be circular.
// Type-only imports from "./index" are fine.
import { z } from "zod";

export const sourceKinds = [
  "upload",
  "postgres",
  "mysql",
  "neo4j",
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
