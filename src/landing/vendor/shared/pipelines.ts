// Area schemas for Pipeline Builder: pipelines, transforms, builds and schedules.
// Owned by the pipelines area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index" would be circular.
// Type-only imports from "./index" are fine.
import { z } from "zod";

export const PipelineNodeSchema = z.object({
  id: z.string(),
  type: z.enum([
    "source",
    "select",
    "rename",
    "cast",
    "filter",
    "derive",
    "deduplicate",
    "join",
    "aggregate",
    "sql",
  ]),
  inputs: z.array(z.string()),
  sql: z.string().optional(),
  datasetId: z.string().optional(),
  config: z.record(z.string(), z.unknown()).default({}),
});
export const PipelineSchema = z.object({
  name: z.string().min(1),
  description: z.string().default(""),
  inputs: z.array(z.string()).min(1),
  sql: z.string().min(1),
  nodes: z.array(PipelineNodeSchema).default([]),
  checks: z.array(z.object({ name: z.string(), sql: z.string() })).default([]),
});
