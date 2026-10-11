import { z } from "zod";
import { SourceSchema } from "./data.js";
import { PipelineSchema } from "./pipelines.js";
import { OntologySchema, RelationshipSchema } from "./ontology.js";
import { DashboardSchema } from "./workspace.js";
// Connector cards the sample data imitates; highlighted when it loads.
export const sampleSources = [
  "workos",
  "posthog",
  "planetscale",
  "stripe",
  "s3",
] as const;
export const RoleSchema = z.enum(["owner", "editor", "viewer"]);
export type Role = z.infer<typeof RoleSchema>;
export type Resource = {
  id: string;
  workspaceId: string;
  kind: string;
  name: string;
  description: string;
  revision: number;
  createdAt: string;
  updatedAt: string;
  data: Record<string, any>;
};
export const hostingProviders = ["neon", "supabase", "planetscale"] as const;
export const HostingProviderSchema = z.enum(hostingProviders);
export type HostingProvider = z.infer<typeof HostingProviderSchema>;
export const PlanetScaleTokenSchema = z
  .object({
    serviceTokenId: z
      .string()
      .trim()
      .min(1)
      .max(200)
      .regex(/^[A-Za-z0-9_-]+$/),
    serviceToken: z
      .string()
      .trim()
      .min(10)
      .max(4000)
      .regex(/^[\x21-\x39\x3b-\x7e]+$/),
  })
  .strict();
export type PlanetScaleToken = z.infer<typeof PlanetScaleTokenSchema>;
export const HostingTokenSchema = z.union([
  z.object({ apiKey: z.string().trim().min(10).max(4000) }).strict(),
  PlanetScaleTokenSchema,
]);
export const HostingSelectionSchema = z.object({
  accountId: z.string().min(1).max(100),
  path: z.array(z.string().min(1).max(200)).max(4).default([]),
  password: z.string().max(1000).optional(),
});
export type HostingSelection = z.infer<typeof HostingSelectionSchema>;
export type HostingOption = { id: string; name: string; description?: string };
export type HostingOptions = {
  title: string;
  options: HostingOption[];
  ready?: boolean;
  needsPassword?: boolean;
};
export type HostedConnection = {
  id: string;
  name: string;
  kind: "postgres" | "mysql";
  config: { hostedConnectionId: string };
  discovery: any;
};
export const ProviderSchema = z.object({
  provider: z.enum(["local", "claude", "openai"]),
  model: z.string().min(1).max(150),
  baseUrl: z.url().optional(),
  apiKey: z.string().optional(),
  allowRawData: z.boolean().default(false),
  dailyTokens: z.number().int().min(1000).max(10_000_000).default(200_000),
  maxCalls: z.number().int().min(1).max(20).default(20),
  timeoutSeconds: z.number().int().min(30).max(1800).default(600),
  continuous: z.boolean().default(true),
});
export type ProviderSettings = z.infer<typeof ProviderSchema>;
export const OnboardingProviderSchema = ProviderSchema.extend({
  advanceToData: z.boolean().optional(),
});
export type OnboardingProvider = z.infer<typeof OnboardingProviderSchema>;
export type ColumnProfile = {
  name: string;
  type: string;
  nulls: number;
  distinct: number;
  examples: unknown[];
  sensitive: boolean;
  primaryKey: boolean;
};
export type DatasetProfile = {
  rows: number;
  duplicateRows?: number;
  columns: ColumnProfile[];
  sample: Record<string, unknown>[];
  fingerprint: string;
};
export const BundleSchema = z.object({
  summary: z.string(),
  assumptions: z.array(z.string()),
  catalog: z.array(
    z.object({
      datasetId: z.string(),
      description: z.string(),
      tags: z.array(z.string()),
    }),
  ),
  pipelines: z.array(PipelineSchema).max(10),
  ontology: z.array(OntologySchema).max(20),
  relationships: z.array(RelationshipSchema).max(30),
  dashboards: z.array(DashboardSchema).max(5),
  actions: z
    .array(
      z.object({
        name: z.string(),
        objectType: z.string(),
        fields: z.array(z.string()),
      }),
    )
    .default([]),
});
export type Bundle = z.infer<typeof BundleSchema>;
export type ValidationReport = {
  valid: boolean;
  checks: { name: string; passed: boolean; detail: string }[];
  previews: Record<string, unknown>;
  inputVersions: Record<string, string>;
  validatedAt: string;
  baseGeneration?: string | null;
};
export type ChangeProposal = {
  bundle: Bundle;
  validation: ValidationReport;
  status: "draft" | "ready" | "rejected" | "published";
  hash: string;
  parentId?: string;
  publishedGeneration?: string;
};
export type ToolDefinition = {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
  permission: "read" | "draft";
};
export interface ModelProvider {
  generate(
    system: string,
    context: unknown,
    schema: Record<string, unknown>,
    signal?: AbortSignal,
  ): Promise<{ value: unknown; tokens: number }>;
}
export function datasetTable(id: string) {
  return `d_${id.replace(/-/g, "")}`;
}
export function jsonSafe(value: unknown): any {
  return JSON.parse(
    JSON.stringify(value, (_, v) => (typeof v === "bigint" ? v.toString() : v)),
  );
}

export function datasetAliases(name: string) {
  return [
    ...new Map(
      [name, name.replace(/[^a-zA-Z0-9]+/g, "_")]
        .filter((x) => x.length > 0 && x.length <= 100)
        .map((x) => [x.toLowerCase(), x]),
    ).values(),
  ];
}

// Setup progress is optional workspace metadata; readiness is always derived on the server.
export const OnboardingUpdateSchema = z.object({
  step: z.enum(["ai", "data", "outcome", "review"]).optional(),
  deferred: z.boolean().optional(),
  goal: z.string().max(4000).optional(),
  excludedIds: z.array(z.string()).max(500).optional(),
});
export const OnboardingImportSchema = SourceSchema.extend({
  selections: z
    .array(
      z.object({
        table: z.string().optional(),
        schema: z.string().optional(),
        key: z.string().optional(),
      }),
    )
    .min(1)
    .max(100),
  operationId: z.string().min(8).max(100),
});
// POST /demo: advance moves setup to the Outcome step before the samples load.
export const DemoSchema = z.object({ advance: z.boolean().optional() });
export const OnboardingOutcomeSchema = z.object({
  goal: z.string().min(5).max(4000),
  parentId: z.string().optional(),
});
export const OutcomeRecommendationsSchema = z.object({
  description: z.string(),
  outcomes: z
    .array(
      z.object({
        title: z.string(),
        question: z.string(),
        goal: z.string().min(5),
        evidence: z
          .array(
            z.object({
              datasetId: z.string(),
              columns: z.array(z.string()).min(1),
            }),
          )
          .min(1),
      }),
    )
    .min(1)
    .max(3),
});
export type OutcomeRecommendations = z.infer<
  typeof OutcomeRecommendationsSchema
>;
export type OnboardingUpdate = z.infer<typeof OnboardingUpdateSchema>;
export type OnboardingImport = z.infer<typeof OnboardingImportSchema>;
export const ResourceSchema = z.object({
  id: z.string(),
  workspaceId: z.string(),
  kind: z.string(),
  name: z.string(),
  description: z.string(),
  revision: z.number(),
  createdAt: z.string(),
  updatedAt: z.string(),
  data: z.record(z.string(), z.any()),
});
// A suggested ontology computed from the connected data: real-world entities, each
// consolidated from one or more source datasets joined by matching keys, and the
// links between them. Display metadata lives here, outside BundleSchema.
export const OntologyMemberSchema = z.object({
  datasetId: z.string(),
  datasetName: z.string(),
  // Where the dataset came from: a sample source or connector kind, or "upload".
  source: z.string(),
  key: z.string(),
  rows: z.number(),
  // Rows of this dataset that belong to the entity (all rows for the anchor).
  matched: z.number(),
  // How a merged dataset joins the entity; null for the anchor dataset.
  via: z
    .object({
      column: z.string(),
      targetDatasetId: z.string(),
      targetColumn: z.string(),
    })
    .nullable(),
});
export const SuggestedEntitySchema = z.object({
  id: z.string(),
  name: z.string().min(1).max(60),
  description: z.string(),
  // Activity streams (e.g. product events) feed metrics but are not stored as records.
  kind: z.enum(["record", "activity"]),
  anchorDatasetId: z.string(),
  key: z.string(),
  rows: z.number(),
  members: z.array(OntologyMemberSchema).min(1),
  properties: z.array(z.string()),
});
export const SuggestedLinkSchema = z.object({
  id: z.string(),
  name: z.string(),
  from: z.string(),
  to: z.string(),
  fromDatasetId: z.string(),
  fromColumn: z.string(),
  toDatasetId: z.string(),
  toColumn: z.string(),
  // Non-null source values and how many matched the target.
  totalRows: z.number(),
  matchedRows: z.number(),
  caseInsensitive: z.boolean(),
  evidence: z.enum(["declared", "name", "values"]),
});
export const OntologyNoteSchema = z.object({
  kind: z.enum(["merged", "unmatched", "case", "unlinked"]),
  text: z.string(),
  // What the note is about, so it is dropped with an excluded entity or link.
  entities: z.array(z.string()).optional(),
  links: z.array(z.string()).optional(),
});
export const SuggestedOntologySchema = z.object({
  signature: z.string(),
  hash: z.string(),
  summary: z.string(),
  // True once the AI has written the summary and descriptions.
  described: z.boolean(),
  entities: z.array(SuggestedEntitySchema),
  links: z.array(SuggestedLinkSchema),
  notes: z.array(OntologyNoteSchema),
  unplaced: z.array(z.string()),
});
export type SuggestedOntology = z.infer<typeof SuggestedOntologySchema>;
export type SuggestedEntity = z.infer<typeof SuggestedEntitySchema>;
export type SuggestedLink = z.infer<typeof SuggestedLinkSchema>;
export const OntologyConfirmSchema = z.object({
  hash: z.string(),
  excluded: z.array(z.string()).max(50).default([]),
  names: z.record(z.string(), z.string().min(1).max(60)).default({}),
});
export type OntologyConfirm = z.infer<typeof OntologyConfirmSchema>;
export const OntologyEditsSchema = z.object({
  excluded: z.array(z.string()),
  names: z.record(z.string(), z.string()),
});
export const SampleProgressSchema = z.object({
  total: z.number(),
  loaded: z.number(),
  pending: z.array(z.string()),
});
export const OnboardingStateSchema = z.object({
  step: z.enum(["ai", "data", "outcome", "review"]),
  deferred: z.boolean(),
  goal: z.string(),
  complete: z.boolean(),
  dashboardId: z.string().optional(),
  excludedIds: z.array(z.string()),
  datasets: z.array(ResourceSchema),
  sources: z.array(ResourceSchema),
  imports: z.array(ResourceSchema),
  providerJob: ResourceSchema.optional(),
  providerReady: z.boolean(),
  inputsReady: z.boolean(),
  blockers: z.array(z.string()),
  signature: z.string(),
  recommendationRun: ResourceSchema.optional(),
  buildRun: ResourceSchema.optional(),
  recommendations: OutcomeRecommendationsSchema.optional(),
  proposal: ResourceSchema.optional(),
  stale: z.boolean(),
  // Sample datasets still being loaded, when sample data was requested.
  sampleProgress: SampleProgressSchema.optional(),
  // Suggested ontology for the current data, and its AI description run.
  ontology: SuggestedOntologySchema.optional(),
  ontologyRun: ResourceSchema.optional(),
  ontologyConfirmed: z.boolean(),
  // The review edits confirmed for the current suggestion, to restore them.
  ontologyEdits: OntologyEditsSchema.optional(),
});

export type OnboardingState = z.infer<typeof OnboardingStateSchema>;

// Area schema modules; each area owns its file.
export * from "./data.js";
export * from "./pipelines.js";
export * from "./ontology.js";
export * from "./analyst.js";
export * from "./workspace.js";

export * from "./record-collections.js";
