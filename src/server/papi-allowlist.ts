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
// never add sign-in, SSO management, role changes or member removal (they
// stay in the dashboard). A connector's sign-in at its provider is not that
// sign-in (see Data). Connecting a directory is the one exception to SSO
// management (see Workspace): its roles from groups stay in the dashboard.

export type PapiArea =
  | "auth"
  | "workspace"
  | "data"
  | "ontology"
  | "databases"
  | "explore"
  | "agent"
  | "dashboards"
  | "automations"
  | "access"
  | "slack"
  | "apps";
export type PapiRoute = {
  area: PapiArea;
  method: HttpMethod;
  path: RouteTemplate;
};

export const papiRoutes = [
  // Auth and identity
  { area: "auth", method: "GET", path: "/v1/whoami" },

  // Feedback: the Feedback button's and Request a connector's.
  { area: "workspace", method: "POST", path: "/v1/feedback" },

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
  // Directory sync (Settings › Access › Teams): its status, the WorkOS Admin
  // Portal link an owner connects a directory through, and a sync now. Its
  // roles from groups are not here: they stay in the dashboard.
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/directory",
  },
  {
    area: "workspace",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/directory/portal-link",
  },
  {
    area: "workspace",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/directory:sync",
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
  // (back to the default, or a provider tested before it is used). A local
  // install also looks again for its default as the page opens, and lists
  // an Ollama server's models for its card.
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
  {
    area: "workspace",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ai/detect",
  },
  {
    area: "workspace",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ai/ollama-models",
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

  // Data: connecting a source by signing in at its provider (start,
  // read, the provider's page to authorize at, and finishing from
  // /connect/callback). These sign a connector in to the user's account at
  // that provider, through WorkOS Pipes: they aren't Helix's sign-in, which
  // this file never allows.
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/sign-ins",
  },
  {
    area: "data",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/sign-ins/{id}",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/sign-ins/{id}:authorize",
  },
  {
    area: "data",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/sign-ins/{id}:finish",
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
  // The new-ontology wizard's company step: research a company's site, then
  // draft a goal and a name from it and the answers.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/company-research",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/company-research/{researchId}:draft-goal",
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
  // A generation waiting on the agent's questions: answered (or with the
  // agent's defaults), or cancelled, which any live one may be.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/generations/{id}:answer",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/generations/{id}:cancel",
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

  // Agent: the Assistant. A person's threads with it, their messages (turns)
  // and turns' events, stopping a turn, and running, refreshing or
  // dismissing an action it prepared. Only the person runs an action: the
  // agent's own execute is never here.
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/turns",
  },
  {
    area: "agent",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/threads",
  },
  {
    area: "agent",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/threads/{id}",
  },
  {
    area: "agent",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/threads/{id}",
  },
  {
    area: "agent",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/turns/{id}/events",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/turns/{id}:cancel",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/actions/{id}:execute",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/actions/{id}:refresh",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/actions/{id}:dismiss",
  },
  // Where a local install's Assistant runs (its worker, or a helper on the
  // owner's Claude Code or Codex subscription), pairing a helper with a
  // one-time code, and revoking one. Hosted Helix refuses them.
  {
    area: "agent",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/runtime",
  },
  {
    area: "agent",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/runtime",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/host-pairings",
  },
  {
    area: "agent",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/agent/hosts/{id}:revoke",
  },

  // Dashboards: tiles of typed queries over ontologies' loads, each read by
  // the viewer through a load's :read. Listed and read by any member; made,
  // edited, deleted, pinned and given an Analyst answer's visual; and,
  // hosted only, published at a public link and unpublished. Trying a tile
  // (:preview-tile) is the Helix MCP's and the Assistant's, not the site's.
  // The public link's snapshot is the foundry-events door's, which the
  // share page's browser asks with no session, never through here.
  {
    area: "dashboards",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards",
  },
  {
    area: "dashboards",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards",
  },
  {
    area: "dashboards",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}",
  },
  {
    area: "dashboards",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}",
  },
  {
    area: "dashboards",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}",
  },
  {
    area: "dashboards",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}:pin",
  },
  {
    area: "dashboards",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}:add-question-visual",
  },
  {
    area: "dashboards",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}:publish",
  },
  {
    area: "dashboards",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboards/{id}:unpublish",
  },
  // The workspace's dashboard settings: whether its dashboards may be
  // published at public links. Every member reads them; owners change them.
  {
    area: "dashboards",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboard-settings",
  },
  {
    area: "dashboards",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/dashboard-settings",
  },

  // Automations: a person's graph of steps Foundry runs as them when its
  // trigger fires. Listed and read by every member; made by any member, then
  // edited, approved (its owner's click here, never an agent's), run by hand
  // and resumed by its owner; paused, deleted and a run cancelled by its
  // owner or an owner or admin. Its runs and their steps are read here too.
  {
    area: "automations",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automations",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations",
  },
  {
    area: "automations",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}",
  },
  {
    area: "automations",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}",
  },
  {
    area: "automations",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}:approve",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}:pause",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}:resume",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{id}:run",
  },
  {
    area: "automations",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{automationId}/runs",
  },
  {
    area: "automations",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{automationId}/runs/{id}",
  },
  {
    area: "automations",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/automations/{automationId}/runs/{id}:cancel",
  },
  // The new automation page's suggestions, written for an ontology's
  // current version.
  {
    area: "automations",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/automation-suggestions",
  },

  // Ontology: decisions, applied from the next load.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/reviews/{reviewId}:settle",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/reviews:batchSettle",
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
  // Ontology: labels of claims the review walk drew, to measure precision.
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-labels",
  },
  // Ontology: review cards, the graph worker's index of what each current
  // load asks a person to decide: the counts, the walk's pages, and its spot
  // checks (read, and drawn once at the finish).
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/review-summary",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-cards",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-spot-checks",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/review-spot-checks:draw",
  },

  // Ontology: suggestions, what audits of the current load found it lacks.
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/heal-findings",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/heal-findings/{fingerprint}:dismiss",
  },

  // Ontology: workflows, an agent's task, and what its runs recorded. Runs
  // are started and finished by agents, never the site.
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows:insights",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{id}",
  },
  {
    area: "ontology",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{id}",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{workflowId}/runs",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{workflowId}/runs/{id}",
  },
  {
    area: "ontology",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{workflowId}/runs/{id}/accesses",
  },
  {
    area: "ontology",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/ontologies/{ontologyId}/workflows/{workflowId}/runs/{id}:assess",
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

  // Access: source grants (owners and admins grant a member, a team or an
  // agent's key the workspace's sources, and read who reads what through
  // them), who a change would cut off, restricting a member, teams, what
  // the caller itself reads through grants (any member), and the
  // connections whose sources' own permissions narrow grants (owners and
  // admins).
  {
    area: "access",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/access",
  },
  {
    area: "access",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/access/grants",
  },
  {
    area: "access",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/access:preview",
  },
  {
    area: "access",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/access/me",
  },
  {
    area: "access",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/access/sources",
  },
  {
    area: "access",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/access/members/{userId}",
  },
  {
    area: "access",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/access/teams",
  },
  {
    area: "access",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/access/teams/{teamId}",
  },
  {
    area: "access",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/access/teams/{teamId}",
  },
  // Agents' keys (the WFE's workspace service credentials): listed, made
  // (the token shown once) and revoked, to grant them sources. Never read
  // one by id or changed here: a key's project grants stay in the Helix
  // dashboard.
  {
    area: "access",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/service-credentials",
  },
  {
    area: "access",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/service-credentials",
  },
  {
    area: "access",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/service-credentials/{id}",
  },

  // Slack (Settings › Slack, /slack/installed, /slack/link): Helix's own
  // Slack app. Owners and admins add it (start, then finish once Slack
  // returns to the site), switch it between per person and the shared bot,
  // see who is linked and disconnect it; each person previews and makes
  // their own link from the token the app sent them, and unlinks it.
  {
    area: "slack",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/slack",
  },
  {
    area: "slack",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/slack",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/slack:start-install",
  },
  { area: "slack", method: "POST", path: "/v1/foundry/slack:finish-install" },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/slack:disconnect",
  },
  {
    area: "slack",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/slack/links",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/slack/links:preview",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/slack/links",
  },
  {
    area: "slack",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/foundry/slack/links/{slackUserId}",
  },
  // A local install's Slack, through Helix's relay: its owner links it to
  // Helix (start, then complete with what Helix handed back), turns
  // approving changes from Slack on or off, unlinks it, and allows Slack
  // users who asked to read and ask as them, or removes them. Helix refuses
  // these.
  {
    area: "slack",
    method: "PUT",
    path: "/v1/workspaces/{workspaceId}/foundry/slack/links/{slackUserId}/access",
  },
  {
    area: "slack",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/relay",
  },
  {
    area: "slack",
    method: "PATCH",
    path: "/v1/workspaces/{workspaceId}/foundry/relay",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/relay:start-link",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/relay:complete-link",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/relay:unlink",
  },
  // Helix's side (/relay/link, Settings › Slack): owners and admins
  // authorize a local install's link, list the workspace's linked installs
  // and revoke them. A local install refuses these.
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/relay-links:authorize",
  },
  {
    area: "slack",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/foundry/relay-links",
  },
  {
    area: "slack",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/foundry/relay-links/{linkId}:revoke",
  },

  // Connected apps: partner integrations Helix registered. Owners and admins
  // approve one on /oauth/authorize, then list and revoke installs. An
  // install's sources are its credential's Foundry grants (the access routes).
  { area: "apps", method: "GET", path: "/v1/partner-apps/authorization" },
  {
    area: "apps",
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/partner-apps/authorization:approve",
  },
  {
    area: "apps",
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/partner-apps/installs",
  },
  {
    area: "apps",
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/partner-apps/installs/{installId}",
  },
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
