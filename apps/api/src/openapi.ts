import { z } from "zod";
import {
  SourceSchema,
  OnboardingStateSchema,
  ResourceSchema,
  OnboardingUpdateSchema,
  OnboardingImportSchema,
  OnboardingOutcomeSchema,
  OnboardingProviderSchema,
  HostingSelectionSchema,
  HostingTokenSchema,
  OntologyConfirmSchema,
} from "../../../packages/shared/src/index.js";
import type { RequestSchemas } from "./routes/deps.js";
import { dataSchemas } from "./routes/data.js";
import { pipelinesSchemas } from "./routes/pipelines.js";
import { ontologySchemas } from "./routes/ontology.js";
import { analystSchemas } from "./routes/analyst.js";
import { workspaceSchemas } from "./routes/workspace.js";
// Request bodies for the docs, keyed "METHOD /path". Area route modules export
// their own maps; setup (onboarding, hosting) stays here.
const schemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/hosting/:provider/authorize": z.object({
    returnTo: z.enum(["onboarding", "sources"]).optional(),
  }),
  "POST /api/v1/workspaces/:w/hosting/:provider/token": HostingTokenSchema,
  "POST /api/v1/workspaces/:w/hosting/:provider/options":
    HostingSelectionSchema,
  "POST /api/v1/workspaces/:w/hosting/:provider/connect":
    HostingSelectionSchema,
  "PATCH /api/v1/workspaces/:w/onboarding": OnboardingUpdateSchema,
  "POST /api/v1/workspaces/:w/onboarding/provider": OnboardingProviderSchema,
  "POST /api/v1/workspaces/:w/onboarding/sources": OnboardingImportSchema,
  "PUT /api/v1/workspaces/:w/onboarding/sources/:id": SourceSchema,
  "POST /api/v1/workspaces/:w/onboarding/build": OnboardingOutcomeSchema,
  "POST /api/v1/workspaces/:w/onboarding/recommendations": z.object({}),
  "POST /api/v1/workspaces/:w/onboarding/ontology": z.object({}),
  "POST /api/v1/workspaces/:w/onboarding/ontology/confirm":
    OntologyConfirmSchema,
  ...dataSchemas,
  ...pipelinesSchemas,
  ...ontologySchemas,
  ...analystSchemas,
  ...workspaceSchemas,
};
export function openapiTransform({ schema, url, route }: any) {
  const body = schemas[`${route.method} ${url}`],
    params = [...url.matchAll(/:([a-zA-Z]+)/g)].map((m) => m[1]);
  return {
    url,
    schema: {
      ...schema,
      ...(url.endsWith("/onboarding/uploads")
        ? {
            description:
              "Durably stage one complete CSV, JSON, JSONL or Parquet file (256 MB max). Returns an asynchronous import job. Reuse Idempotency-Key to retry the same upload.",
            consumes: ["multipart/form-data"],
            body: {
              type: "object",
              required: ["file"],
              properties: { file: { type: "string", format: "binary" } },
            },
            headers: {
              type: "object",
              required: ["idempotency-key"],
              properties: {
                "idempotency-key": {
                  type: "string",
                  minLength: 8,
                  maxLength: 100,
                },
              },
            },
          }
        : {}),
      ...(url.endsWith("/sources/sqlite")
        ? {
            description:
              "Stage one SQLite database file (.db, .sqlite or .sqlite3, 256 MB max) for this workspace. Returns its file name; pass it as config.file with kind sqlite to test a source, list its tables and import them. Uploading the same file again returns the same name.",
            consumes: ["multipart/form-data"],
            body: {
              type: "object",
              required: ["file"],
              properties: { file: { type: "string", format: "binary" } },
            },
          }
        : {}),
      ...(url.endsWith("/onboarding") && route.method === "GET"
        ? {
            description:
              "Returns persisted setup progress plus server-derived readiness, sample loading progress, complete snapshots, imports, recommendations, the suggested ontology, reviewed proposal and published dashboard. Completion cannot be set by clients.",
          }
        : {}),
      ...(url.includes("/onboarding")
        ? {
            response: {
              200: z.toJSONSchema(
                url.endsWith("/onboarding") ||
                  url.includes("/onboarding/ontology")
                  ? OnboardingStateSchema
                  : url.endsWith("/sources")
                    ? z.array(
                        z.object({
                          source: ResourceSchema,
                          job: ResourceSchema,
                        }),
                      )
                    : ResourceSchema,
                { target: "draft-7" },
              ),
            },
          }
        : {}),
      ...(url.startsWith("/api/v1/workspaces/")
        ? {
            security: [{}, { token: [] }],
            tags: [url.split("/")[5] || "workspace"],
          }
        : { tags: ["installation"] }),
      ...(params.length
        ? {
            params: {
              type: "object",
              properties: Object.fromEntries(
                params.map((p) => [p, { type: "string" }]),
              ),
              required: params,
            },
          }
        : {}),
      ...(body ? { body: z.toJSONSchema(body, { target: "draft-7" }) } : {}),
    },
  };
}
