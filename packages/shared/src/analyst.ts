// Area schemas for AIP Analyst: threads, messages, tool steps and saved analyses.
// Owned by the analyst area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index.js" would be circular.
// Type-only imports from "./index.js" are fine.
import { z } from "zod";

export const GoalSchema = z.object({
  goal: z.string().min(5).max(4000),
  intent: z.enum(["build", "answer", "auto"]).default("build"),
  parentId: z.string().optional(),
  context: z
    .object({ page: z.string().optional(), resourceId: z.string().optional() })
    .optional(),
});
export type AssistantRun = {
  intent?: "auto" | "answer" | "build";
  resolvedIntent?: "answer" | "build";
  goal: string;
  status:
    | "queued"
    | "running"
    | "awaiting_review"
    | "succeeded"
    | "failed"
    | "canceled";
  stage: string;
  events: { at: string; stage: string; message: string }[];
  calls: number;
  tokens: number;
  proposalId?: string;
  error?: string;
};

// A possible next investigation, supported by a published ontology property.
export type ChatFollowup = {
  id: string;
  label: string;
  question: string;
  ontology: { type: string; datasetId: string; property: string };
  version: string;
};
export type ChatFollowups = {
  runId: string | null;
  generation: string | null;
  items: ChatFollowup[];
};
