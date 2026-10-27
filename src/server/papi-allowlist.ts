import {
  compileTemplate,
  type CompiledTemplate,
  type HttpMethod,
  type RouteTemplate,
} from "@/shared/route-template";

// THE /papi ALLOWLIST. The browser reaches the WFE only through these routes:
// one entry per method and path, written exactly as in
// contract/wfe.swagger.json. No wildcards and no prefixes: a {param} matches
// one path segment. Anything not listed gets 404 (or 405 when only the method
// differs) and never leaves the server.
//
// Adding a route: add one line in your area's section, then the matching
// mock route under mock/routes/. tests/papi-allowlist.test.ts fails if a line
// is not in the WFE contract. Do not add routes the UI does not call, and
// never add SSO management, role changes or member removal (they stay in the
// dashboard).

export type PapiArea =
  | "auth"
  | "workspace"
  | "data"
  | "ontology"
  | "databases"
  | "explore";
export type PapiRoute = {
  area: PapiArea;
  method: HttpMethod;
  path: RouteTemplate;
};

export const papiRoutes = [
  // Auth and identity
  { area: "auth", method: "GET", path: "/v1/whoami" },

  // Workspace: the caller's workspaces, and creating one.
  { area: "workspace", method: "GET", path: "/v1/workspaces" },
  { area: "workspace", method: "POST", path: "/v1/workspaces" },
  { area: "workspace", method: "GET", path: "/v1/workspaces/{id}" },
  // Members (read-only) and inviting by email.
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/members",
  },
  {
    area: "workspace",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/members:invite",
  },
  // Foundry plan and billing: Free or Pro, checkout, cancel, resume, and the
  // hosted billing portal of the workspace's billing customer.
  { area: "workspace", method: "GET", path: "/v1/foundry/plan" },
  { area: "workspace", method: "POST", path: "/v1/foundry/plan:checkout" },
  { area: "workspace", method: "POST", path: "/v1/foundry/plan:cancel" },
  { area: "workspace", method: "POST", path: "/v1/foundry/plan:resume" },
  { area: "workspace", method: "POST", path: "/v1/billing/portal" },
  // This month's AI spend by task, with each task's cap (the account menu).
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/usage",
  },
  // People enrichment: the setting and this month's credits, and the
  // owner's consent and monthly credit cap.
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/enrichment",
  },
  {
    area: "workspace",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/enrichment",
  },
  // AI models: where the workspace's model calls go, and an owner's change
  // (back to the default, or a provider tested before it is used).
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ai",
  },
  {
    area: "workspace",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/ai",
  },

  // Data: connectors (kinds, test, create, read, edit, delete), their syncs
  // and synced tables (carried on the connector).
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connector-kinds",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors:test",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{id}",
  },
  {
    area: "data",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{id}",
  },
  {
    area: "data",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{id}",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{id}:sync",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}/syncs",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connector-syncs/{id}",
  },

  // Data: an API source's send tokens (made and shown once here, listed,
  // revoked) and its recent sends. Its pieces are sent by agents, never here.
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}/tokens",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}/tokens",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}/tokens/{id}:revoke",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/connectors/{connectorId}/event-batches",
  },

  // Data: the workspace's documents. Upload into a set (create, then
  // :complete after the presigned PUT; the bytes never pass through here),
  // list, read with the ontologies that read it, its text (text types) and
  // its bytes (any type: PDF, Parquet), rename, move to another set and
  // delete.
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/documents",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/documents",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}",
  },
  {
    area: "data",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}",
  },
  {
    area: "data",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}:content",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}:bytes",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}:complete",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/documents/{id}:move",
  },
  // Document sets, the folders of documents: list (with the ontologies that
  // read each), create, rename or describe, and delete an empty one.
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/document-sets",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/document-sets",
  },
  {
    area: "data",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/document-sets/{setId}",
  },
  {
    area: "data",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/document-sets/{setId}",
  },
  // The Data tree's layout: folder names, custom folders and order, one
  // object per workspace, replaced at the revision read.
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/data-layout",
  },
  {
    area: "data",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/data-layout",
  },

  // Ontology: the workspace's ontologies, each bound to one Helix database,
  // and deleting one (Foundry's rows only, never the database).
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}",
  },
  {
    area: "ontology",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}",
  },
  // Per-item revision; the generated route returns a polled generation.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/versions/{versionId}:revise",
  },
  // An ontology's versions, current version and generations (a generation
  // drafts from the Data area's connectors). Versions are only generated
  // here, never written by hand (no POST .../versions).
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/versions",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/versions/{id}",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}:set-current",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}:generate",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/generations",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/generations/{id}",
  },
  // What an ontology reads of the workspace's documents, with its current
  // load's extraction of each, and one of them. Read only: documents and
  // their sets are written in the Data area.
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/documents",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/documents/{id}",
  },
  // Loads: writing a version's tables and the documents into the database,
  // choosing the one reads go to, and deleting one.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{id}",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{id}:set-current",
  },
  // Stopping a load's People enrichment lookups; the load goes on.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{id}:stop-enrichment",
  },
  // Reads of a load with its version's rules evaluated: derived facts,
  // flags, why one holds, and what the load's checks left out.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{loadId}:read",
  },
  {
    area: "ontology",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/loads/{id}",
  },
  // Questions: asking about a load, the threads asked, a question's events
  // and result, and cancelling one. The agent only reads.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions/{id}",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions/{id}/events",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/questions/{id}:cancel",
  },

  // Ontology: decisions, applied from the next load.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/reviews/{reviewId}:settle",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-decisions",
  },
  {
    area: "ontology",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-decisions/{id}",
  },

  // Databases: the workspace's projects and their databases (tenants), and
  // creating either, as helix-website does, to bind an ontology to one. The
  // WFE puts a new database on the shared cluster; Foundry never offers
  // dedicated clusters, so no /v1/clusters route.
  { area: "databases", method: "GET", path: "/v1/projects" },
  { area: "databases", method: "POST", path: "/v1/projects" },
  { area: "databases", method: "GET", path: "/v1/tenants" },
  { area: "databases", method: "POST", path: "/v1/tenants" },
  // Explore: reads of an ontology's database with the user's session, through
  // the query broker. Never query-write.
  { area: "explore", method: "POST", path: "/v1/databases:query-read" },
] as const satisfies readonly PapiRoute[];

type Compiled = PapiRoute & { compiled: CompiledTemplate };
const compiled: readonly Compiled[] = papiRoutes.map((r) => ({
  ...r,
  compiled: compileTemplate(r.path),
}));

/** The allowlisted route for `method` and `path`, if there is one. */
export function findPapiRoute(method: string, path: string) {
  const m = method.toUpperCase();
  return compiled.find((r) => r.method === m && r.compiled.regex.test(path));
}

/** Every method allowed on `path`, for a 405's Allow header. */
export function allowedMethods(path: string): HttpMethod[] {
  return [
    ...new Set(
      compiled.filter((r) => r.compiled.regex.test(path)).map((r) => r.method),
    ),
  ].sort();
}
