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

export type ThreadEvidenceReport = {
  format: "helix-foundry.thread-evidence";
  version: 1;
  workspaceId: string;
  threadId: string;
  title: string;
  exportedAt: string;
  notice: string;
  turns: {
    id: string;
    question: string;
    status: string;
    createdAt: string;
    updatedAt: string;
    answer?: {
      title: string;
      sql: string;
      summary: string;
      highlights: string[];
      assumptions: string[];
      inputs: string[];
      citations: { datasetId: string; name: string; version: string }[];
      rows: Record<string, unknown>[];
      total: number | null;
      includedRows: number;
      truncated: boolean | null;
    };
  }[];
};
