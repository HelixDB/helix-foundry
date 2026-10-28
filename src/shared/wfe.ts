// WFE REST payloads, written by hand from the contracts (each type names its
// swagger definition): Foundry's own routes, /v1/foundry and
// /v1/workspaces/{workspaceId}/foundry (the Data and Ontology areas, the
// Foundry plan, usage, enrichment and AI models), from
// contract/foundry.swagger.json, and the rest from contract/wfe.swagger.json.
// grpc-gateway JSON: camelCase fields, enums as their names, int64 as strings,
// timestamps as RFC 3339 strings, and a field at its default value may be left
// out, so every field is optional unless the WFE always sets it. Every WFE
// type the app uses lives in this one module: areas add theirs under a
// heading of their own.

// The engine's entity ref, which its Ask JSON shares with its Read JSON.
import type { EntityRef } from "./foundry-read";

// ---------------------------------------------------------------- Common

/** rpcStatus: the body of every non-2xx WFE response. `code` is a gRPC code. */
export type RpcStatus = {
  code?: number;
  message?: string;
  details?: unknown[];
};

/** v1Timestamps */
export type Timestamps = {
  createdAt?: string;
  updatedAt?: string;
  deletedAt?: string;
  startedAt?: string;
  finishedAt?: string;
};

/** v1Page: list requests send it as `page.size` and `page.token` query params. */
export type Page = { size?: number; token?: string };

/** v1ResourceStatus */
export type ResourceStatus =
  | "RESOURCE_STATUS_UNSPECIFIED"
  | "RESOURCE_STATUS_PENDING"
  | "RESOURCE_STATUS_PROVISIONING"
  | "RESOURCE_STATUS_ACTIVE"
  | "RESOURCE_STATUS_DELETING"
  | "RESOURCE_STATUS_DELETED"
  | "RESOURCE_STATUS_FAILED";

// ---------------------------------------------------------------- Auth

/** v1AuthApplication. Foundry always signs in as AUTH_APPLICATION_FOUNDRY. */
export type AuthApplication =
  | "AUTH_APPLICATION_UNSPECIFIED"
  | "AUTH_APPLICATION_HELIX"
  | "AUTH_APPLICATION_FOUNDRY";
export const FOUNDRY_APPLICATION: AuthApplication = "AUTH_APPLICATION_FOUNDRY";

/** v1AuthProvider */
export type AuthProvider =
  | "AUTH_PROVIDER_UNSPECIFIED"
  | "AUTH_PROVIDER_GITHUB"
  | "AUTH_PROVIDER_GOOGLE"
  | "AUTH_PROVIDER_MICROSOFT"
  | "AUTH_PROVIDER_SSO";

/** v1StartLoginRequest: POST /v1/auth/login:start */
export type StartLoginRequest = {
  redirectUri: string;
  provider: AuthProvider;
  application: AuthApplication;
  invitationSession?: string;
  email?: string;
};
/** v1StartLoginResponse: the WorkOS authorize URL and the pending login. */
export type StartLoginResponse = { url?: string; sessionId?: string };

/** v1ExchangeAuthCodeRequest: POST /v1/auth/exchange */
export type ExchangeAuthCodeRequest = {
  sessionId: string;
  code: string;
  state: string;
};
/** v1ExchangeAuthCodeResponse */
export type ExchangeAuthCodeResponse = {
  accessToken?: string;
  refreshToken?: string;
  expiresAt?: string;
  email?: string;
  emailVerificationRequired?: boolean;
  workspaceId?: string;
  invitationOnboarding?: boolean;
  ssoRequired?: boolean;
};

/** v1RefreshSessionRequest: POST /v1/auth/refresh (the BFF refreshes through WorkOS). */
export type RefreshSessionRequest = {
  refreshToken: string;
  application: AuthApplication;
};
/** v1RefreshSessionResponse */
export type RefreshSessionResponse = {
  accessToken?: string;
  refreshToken?: string;
  expiresAt?: string;
};

// ---------------------------------------------------------------- Identity

/** v1User: a Helix user. `id` is the Helix user ID, not the WorkOS one. */
export type User = {
  id: string;
  email?: string;
  displayName?: string;
  timestamps?: Timestamps;
};

/** v1WorkspaceRole */
export type WorkspaceRole =
  | "WORKSPACE_ROLE_UNSPECIFIED"
  | "WORKSPACE_ROLE_ADMIN"
  | "WORKSPACE_ROLE_MEMBER"
  | "WORKSPACE_ROLE_OWNER";

/** v1WorkspaceMember. `projectAccess` is left out: Foundry does not use it. */
export type WorkspaceMember = {
  id?: string;
  workspaceId: string;
  user?: User;
  role?: WorkspaceRole;
  timestamps?: Timestamps;
};

/** v1WhoamiResponse: GET /v1/whoami. `workspaces` are the user's memberships. */
export type WhoamiResponse = {
  user: User;
  workspaces?: WorkspaceMember[];
};

// ---------------------------------------------------------------- Workspaces

/** v1WorkspaceSettings */
export type WorkspaceSettings = { aiInsightsEnabled?: boolean };

/** v1Workspace. Network fields (cidr, vpcId, subnets, tofuStateKey) are left out. */
export type Workspace = {
  id: string;
  slug?: string;
  displayName?: string;
  region?: string;
  status?: ResourceStatus;
  iconUrl?: string;
  settings?: WorkspaceSettings;
  timestamps?: Timestamps;
};

/** helixwfev1ListWorkspacesResponse: GET /v1/workspaces (the caller's workspaces). */
export type ListWorkspacesResponse = {
  workspaces?: Workspace[];
  nextPageToken?: string;
};

/**
 * helixwfev1CreateWorkspaceRequest: POST /v1/workspaces. `region` is
 * required; `slug` is lowercase letters, numbers, "-" and "_", starting with a
 * letter or number, at most 64 characters.
 */
export type CreateWorkspaceRequest = {
  slug: string;
  region: string;
  displayName?: string;
};

/** helixwfev1ListWorkspaceMembersResponse: GET /v1/workspaces/{workspaceId}/members */
export type ListWorkspaceMembersResponse = {
  members?: WorkspaceMember[];
  nextPageToken?: string;
};

/**
 * WfeServiceInviteWorkspaceMemberBody: POST
 * /v1/workspaces/{workspaceId}/members:invite, answered with `{}`. Needs
 * workspace.members.manage (owners and admins); inviting an owner needs
 * ownership management. A member or a pending invitation with the same email
 * is ALREADY_EXISTS.
 */
export type InviteWorkspaceMemberRequest = {
  email: string;
  role: WorkspaceRole;
};

// ---------------------------------------------------------------- Plan and billing

/**
 * v1FoundryTier: Free is the absence of a paid plan. Pro and Max are Autumn's
 * foundry-pro and foundry-max, one group, so a workspace holds at most one;
 * Max grants everything Pro grants.
 */
export type FoundryTier =
  | "FOUNDRY_TIER_UNSPECIFIED"
  | "FOUNDRY_TIER_FREE"
  | "FOUNDRY_TIER_PRO"
  | "FOUNDRY_TIER_MAX";
/** The tiers checkout sells. */
export type FoundryPaidTier = Extract<
  FoundryTier,
  "FOUNDRY_TIER_PRO" | "FOUNDRY_TIER_MAX"
>;

/** v1FoundryPlan: a workspace's Foundry plan as Autumn reports it now. */
export type FoundryPlan = {
  tier?: FoundryTier;
  // A paid plan with an unpaid invoice.
  pastDue?: boolean;
  // End of the paid period; unset on Free.
  currentPeriodEnd?: string;
  // The paid plan ends at currentPeriodEnd unless resumed.
  cancelAtPeriodEnd?: boolean;
};

/**
 * helixwfev1FoundryPlanResponse: GET /v1/foundry/plan?workspaceId=, and the
 * answer of plan:cancel and plan:resume.
 */
export type FoundryPlanResponse = { plan?: FoundryPlan };

/**
 * helixwfev1CreateFoundryProCheckoutRequest: POST /v1/foundry/plan:checkout.
 * `clientToken` is per click: two calls with the same token are one purchase.
 * `tier` is the plan to buy, Pro when unset. Asking for the tier already held
 * attaches nothing and returns the plan; asking for the other one switches.
 */
export type CreateFoundryProCheckoutRequest = {
  workspaceId: string;
  clientToken: string;
  promoCode?: string;
  tier?: FoundryPaidTier;
};
/**
 * helixwfev1CreateFoundryProCheckoutResponse. `checkoutUrl` is the Stripe
 * Checkout page, empty when a card on file was charged; `plan` can read FREE
 * for a moment after a charge.
 */
export type CreateFoundryProCheckoutResponse = {
  checkoutUrl?: string;
  plan?: FoundryPlan;
};

/**
 * helixwfev1CancelFoundryProRequest and helixwfev1ResumeFoundryProRequest:
 * they act on whichever paid plan the workspace holds.
 */
export type FoundryPlanChangeRequest = { workspaceId: string };

/**
 * helixwfev1OpenBillingPortalRequest: POST /v1/billing/portal, the hosted
 * portal of the workspace's billing customer (invoices, payment methods).
 */
export type OpenBillingPortalRequest = { workspaceId: string };
/** helixwfev1OpenBillingPortalResponse */
export type OpenBillingPortalResponse = { portalUrl?: string };

/**
 * google.rpc.ErrorInfo in RpcStatus.details. A Pro-only action on a Free
 * workspace is FAILED_PRECONDITION with reason FOUNDRY_PRO_REQUIRED; Max
 * passes every Pro gate.
 */
export type ErrorInfo = {
  "@type": "type.googleapis.com/google.rpc.ErrorInfo";
  reason?: string;
  domain?: string;
  metadata?: Record<string, string>;
};
export const FOUNDRY_PRO_REQUIRED = "FOUNDRY_PRO_REQUIRED";

// ---------------------------------------------------------------- Usage
// GET /v1/workspaces/{workspaceId}/foundry/usage: the workspace's AI spend
// this UTC calendar month by task, with each task's cap, as the caps count
// it (spend on the workspace's own model key included). Any member reads it.
// Amounts are millionths of a US dollar.

/**
 * v1FoundryUsageTask. API_SOURCES (extraction no person started) is part of
 * LOADS' spend. Drafts record tokens, not dollars, and titles no spend: no
 * task.
 */
export type FoundryUsageTask =
  | "FOUNDRY_USAGE_TASK_UNSPECIFIED"
  | "FOUNDRY_USAGE_TASK_LOADS"
  | "FOUNDRY_USAGE_TASK_API_SOURCES"
  | "FOUNDRY_USAGE_TASK_QUESTIONS";

/** v1FoundryUsageEntry: one task's month. */
export type FoundryUsageEntry = {
  task?: FoundryUsageTask;
  spentUsdMicros?: string;
  // What running work holds back of the cap beyond spent: only questions.
  reservedUsdMicros?: string;
  // Unset for a task without a cap.
  capUsdMicros?: string | null;
};

/** v1FoundryUsage */
export type FoundryUsage = {
  monthStart?: string;
  // The first of next month, 00:00 UTC, when every cap starts again.
  resetsAt?: string;
  // Every task's spend once (API_SOURCES is in LOADS), reservations left out.
  totalUsdMicros?: string;
  // In FoundryUsageTask's order.
  tasks?: FoundryUsageEntry[];
};

/** helixwfev1FoundryUsageResponse */
export type FoundryUsageResponse = { usage?: FoundryUsage };

// ---------------------------------------------------------------- Data: connectors and syncs
// Routes under /v1/workspaces/{workspaceId}/foundry/connectors....
// Credentials are
// write-only: requests carry them, responses never do. The WFE's gateway
// emits unpopulated fields, so an unset message or timestamp arrives as null
// and a write-only string as "".

/** The connector kinds the WFE's catalog holds (`v1Connector.kind`). */
export type ConnectorKindName =
  | "stripe"
  | "workos"
  | "posthog"
  | "github"
  | "linear"
  | "calcom"
  | "granola"
  | "postgres"
  | "mysql"
  | "s3"
  | "events";

/** v1S3ConnectorSetup: what a customer's IAM role must trust. */
export type S3ConnectorSetup = {
  // Helix's foundry-connector role.
  principalArn?: string;
  // This workspace's; required by the trust policy.
  externalId?: string;
};
/** v1ConnectorKind: GET .../foundry/connector-kinds, in display order. */
export type ConnectorKind = {
  kind: string;
  displayName?: string;
  requiresPro?: boolean;
  // False when the workspace's plan does not include the kind.
  available?: boolean;
  // s3 only, and only where role access is configured (proto oneof setup).
  s3?: S3ConnectorSetup | null;
};
/** helixwfev1ListConnectorKindsResponse */
export type ListConnectorKindsResponse = { kinds?: ConnectorKind[] };

/** v1StripeConnectorConfig: a secret (sk_) or restricted (rk_) key. */
export type StripeConnectorConfig = { apiKey?: string };
/** v1WorkOSConnectorConfig */
export type WorkOSConnectorConfig = { apiKey?: string };
/** v1PostHogConnectorConfig. `region` is us (default), eu or custom. */
export type PostHogConnectorConfig = {
  apiKey?: string;
  region?: string;
  // HTTPS origin of a self-hosted PostHog; custom region only.
  host?: string;
  // Filled in by a test when the key sees exactly one project.
  projectId?: string;
  // 1 to 365; 0 means 30.
  eventDays?: number;
};
/** v1GitHubConnectorConfig: a fine-grained (github_pat_) or classic (ghp_) token. */
export type GitHubConnectorConfig = {
  token?: string;
  // owner/name; empty syncs every repository the token reads, up to 100.
  repos?: string[];
  // Reviews of pull requests updated in this many days; 1 to 365, 0 means 90.
  reviewDays?: number;
};
/** v1LinearConnectorConfig: a personal API key (lin_api_). */
export type LinearConnectorConfig = { apiKey?: string };
/** v1CalcomConnectorConfig: a Cal.com API key (cal_live_… or cal_…), cloud only. */
export type CalcomConnectorConfig = { apiKey?: string };
/** v1GranolaConnectorConfig: a Granola personal or workspace API key (grn_…). */
export type GranolaConnectorConfig = {
  apiKey?: string;
  // Also keep each note's transcript as a document, in the connection's
  // Transcripts set beside its Notes. Off by default.
  transcripts?: boolean;
};
/** v1DatabaseConnectorConfig: PostgreSQL and MySQL. */
export type DatabaseConnectorConfig = {
  host?: string;
  // 0 means the database's default port.
  port?: number;
  database?: string;
  user?: string;
  password?: string;
  // PostgreSQL only; empty means public.
  schema?: string;
  // Tables to sync; empty means every table.
  tables?: string[];
  disableTls?: boolean;
  // PEM bundle trusted besides the system roots.
  ca?: string;
};
/** v1S3RoleAuth: an IAM role Helix assumes with the workspace's external ID. */
export type S3RoleAuth = { roleArn?: string };
/** v1S3KeyAuth: an access key; the only way to reach S3-compatible stores. */
export type S3KeyAuth = {
  accessKeyId?: string;
  // Write-only.
  secretAccessKey?: string;
  // HTTPS origin of an S3-compatible store; empty means AWS.
  endpoint?: string;
};
/**
 * v1S3ConnectorConfig: Parquet, JSON, JSON Lines and CSV files in a bucket.
 * `role` and `keys` are a proto oneof (auth), so exactly one is set.
 */
export type S3ConnectorConfig = {
  bucket?: string;
  // Folder; empty reads the whole bucket; ends with "/" once prepared.
  prefix?: string;
  // A test fills it in when empty; optional with an endpoint.
  region?: string;
  // Tables to sync; empty means every table.
  tables?: string[];
} & ({ role: S3RoleAuth } | { keys: S3KeyAuth });
/** v1EventSourceField: one top-level field of an API source's pieces. */
export type EventSourceField = {
  name: string;
  // string, number, boolean, object or array; null while it has only been
  // null. A field keeps the type it first had.
  type?: string;
};
/**
 * v1EventsConnectorConfig: an API source (kind events), which people and
 * their agents send pieces of data to: JSON objects with an id. It has no
 * credential.
 */
export type EventsConnectorConfig = {
  // What each piece is; at most 1000 characters. Left out of an update, it
  // stays as it is.
  description?: string;
  // Output only: the fields its pieces have carried, by byte order of name.
  // Ignored on create and update.
  fields?: EventSourceField[];
  // The top-level field holding each piece's text: the body of the piece's
  // document in the source's own set ("Pieces"), which entity extraction
  // reads. It names a field the pieces have carried as a string (or only as
  // null), or one they haven't carried yet, and locks it as a string, so
  // later pieces must send it as one. Left out of an update, it stays; ""
  // clears it, and the source then has no documents and no extraction.
  // Changing it queues one full sync, which re-renders the newest pieces,
  // and is FAILED_PRECONDITION while a sync of the source is live.
  textField?: string;
};
/**
 * v1EventSourceCapacity: how full an API source's table is, against the caps
 * every table has. int64 counts; rows and bytes include the pieces no sync
 * has read yet (a resent id counts again).
 */
export type EventSourceCapacity = {
  rows?: string;
  maxRows?: string;
  bytes?: string;
  maxBytes?: string;
};
/**
 * v1ConnectorConfig: a proto oneof, so exactly one kind is set. Responses
 * rebuild it from stored settings, so write-only fields come back empty.
 */
export type ConnectorConfig =
  | { stripe: StripeConnectorConfig }
  | { workos: WorkOSConnectorConfig }
  | { posthog: PostHogConnectorConfig }
  | { github: GitHubConnectorConfig }
  | { linear: LinearConnectorConfig }
  | { calcom: CalcomConnectorConfig }
  | { granola: GranolaConnectorConfig }
  | { postgres: DatabaseConnectorConfig }
  | { mysql: DatabaseConnectorConfig }
  | { s3: S3ConnectorConfig }
  | { events: EventsConnectorConfig };

/** v1ConnectorColumn. App connectors (Stripe, PostHog…) have names only. */
export type ConnectorColumn = {
  name: string;
  type?: string;
  nullable?: boolean;
};
/** v1ConnectorForeignKey */
export type ConnectorForeignKey = {
  column: string;
  referencedTable: string;
  referencedColumn: string;
};
/** v1ConnectorTable: a table a test discovered. */
export type ConnectorTable = {
  name: string;
  // Empty for app connectors until their first sync.
  columns?: ConnectorColumn[];
  primaryKey?: string[];
  foreignKeys?: ConnectorForeignKey[];
  // File sources only, from here down. parquet, json or csv.
  format?: string;
  fileCount?: number;
  // int64: the stored bytes of the table's files.
  bytes?: string;
  // At most 5 rows, each one JSON object (none for Parquet).
  preview?: string[];
  // Why a sample could not be read; the sync fails until it is fixed.
  problem?: string;
};
/** v1ConnectorDiscovery: what a source exposes to the credential. */
export type ConnectorDiscovery = {
  tables?: ConnectorTable[];
  // Objects the source has but the credential cannot read: "name" or
  // "name: reason".
  unavailable?: string[];
  // int64, file sources: the stored-byte limit across the tables synced.
  maxBytes?: string;
  // Settings that work but cost the customer, each worded with its fix.
  warnings?: string[];
};

/** v1SyncedTable: one table's newest snapshot. */
export type SyncedTable = {
  name: string;
  rowCount?: string;
  syncedAt?: string | null;
  syncId?: string;
  columns?: ConnectorColumn[];
  primaryKey?: string[];
  foreignKeys?: ConnectorForeignKey[];
};

/** v1ConnectorSyncStatus */
export type ConnectorSyncStatus =
  | "CONNECTOR_SYNC_STATUS_UNSPECIFIED"
  | "CONNECTOR_SYNC_STATUS_QUEUED"
  | "CONNECTOR_SYNC_STATUS_RUNNING"
  | "CONNECTOR_SYNC_STATUS_SUCCEEDED"
  | "CONNECTOR_SYNC_STATUS_FAILED";

/** v1ConnectorSync: one run copying a connector's tables into snapshots. */
export type ConnectorSync = {
  id: string;
  connectorId?: string;
  status?: ConnectorSyncStatus;
  attempts?: number;
  // Why the last attempt failed, written for the customer.
  error?: string;
  rowsSynced?: string;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
  // When a queued sync runs next; later than now while a retry waits.
  nextAttemptAt?: string | null;
};

/** v1Connector */
export type Connector = {
  id: string;
  workspaceId?: string;
  kind: string;
  name?: string;
  config?: ConnectorConfig | null;
  // Identifies the stored credential without revealing it: "sk_live…4f2a".
  secretHint?: string;
  createdAt?: string | null;
  updatedAt?: string | null;
  // Tables the last syncs copied, by name, each at its newest snapshot.
  tables?: SyncedTable[];
  // The newest sync, if any.
  lastSync?: ConnectorSync | null;
  // When it syncs by itself; unset for an API source, which syncs as its
  // pieces arrive.
  schedule?: ConnectorSchedule | null;
};

/**
 * v1ConnectorSchedule: when a connector syncs by itself. A sync's end sets
 * the next one, its finish plus the interval. int64 seconds.
 */
export type ConnectorSchedule = {
  // The owner's choice: "0" the plan's interval, "-1" manual only.
  everySeconds?: string;
  // The interval in force, after the plan's limits; "0" when manual only.
  effectiveEverySeconds?: string;
  nextSyncAt?: string | null;
  // Why automatic syncs stopped, for the customer; empty while they run.
  pausedReason?: string;
};

/** helixwfev1ListConnectorsResponse: GET .../foundry/connectors */
export type ListConnectorsResponse = { connectors?: Connector[] };
/**
 * helixwfev1ConnectorResponse: create, get and update. `discovery` is set
 * only when the call tested the credential.
 */
export type ConnectorResponse = {
  connector: Connector;
  discovery?: ConnectorDiscovery | null;
};

/** v1WfeServiceTestConnectorBody: POST .../foundry/connectors:test */
export type TestConnectorRequest = { config: ConnectorConfig };
/** helixwfev1TestConnectorResponse: the settings as they would be stored. */
export type TestConnectorResponse = {
  discovery?: ConnectorDiscovery | null;
  config?: ConnectorConfig | null;
};
/** v1WfeServiceCreateConnectorBody: POST .../foundry/connectors (tests first). */
export type CreateConnectorRequest = { name: string; config: ConnectorConfig };
/**
 * v1WfeServiceUpdateConnectorBody: PATCH .../foundry/connectors/{id}. A
 * config replaces settings and credential together, tested first; a blank
 * credential keeps the stored one.
 */
export type UpdateConnectorRequest = {
  name?: string;
  config?: ConnectorConfig;
};

/** helixwfev1ConnectorSyncResponse: POST ...:sync and GET .../connector-syncs/{id} */
export type ConnectorSyncResponse = { sync: ConnectorSync };
/** helixwfev1ListConnectorSyncsResponse: newest first; `limit` 1 to 100, 0 means 20. */
export type ListConnectorSyncsResponse = { syncs?: ConnectorSync[] };

// API sources (connectors of kind events) are made, changed and deleted
// through the connector routes, with `config.events`. Their send tokens and
// sends have routes of their own; no route sends pieces: agents do, through
// the Helix MCP.

/** v1FoundryEventSourceToken: a send-only credential of one API source. */
export type FoundryEventSourceToken = {
  id: string;
  connectorId?: string;
  // hx_ev_ and the next 6 characters, to tell tokens apart.
  prefix?: string;
  createdByUserId?: string;
  createdAt?: string | null;
  revokedAt?: string | null;
  lastUsedAt?: string | null;
};
/**
 * helixwfev1CreateFoundryEventSourceTokenResponse: POST .../connectors/{connectorId}/tokens
 * (owners and admins; at most 10 live per source).
 */
export type CreateFoundryEventSourceTokenResponse = {
  token: FoundryEventSourceToken;
  // hx_ev_ and 64 hex digits: returned this once and never stored.
  secret: string;
};
/** helixwfev1ListFoundryEventSourceTokensResponse: newest first, revoked included, at most 50. */
export type ListFoundryEventSourceTokensResponse = {
  tokens?: FoundryEventSourceToken[];
};
/** helixwfev1FoundryEventSourceTokenResponse: POST .../tokens/{id}:revoke (twice is no error). */
export type FoundryEventSourceTokenResponse = {
  token?: FoundryEventSourceToken | null;
};

/** v1FoundryEventBatch: one send's pieces. */
export type FoundryEventBatch = {
  id: string;
  // int64: the send's number in its source, from 1.
  seq?: string;
  pieces?: number;
  // int64: its pieces' lines, uncompressed.
  bytes?: string;
  // The earliest and latest piece time: its occurred_at, else when it arrived.
  firstAt?: string | null;
  lastAt?: string | null;
  createdAt?: string | null;
  // A sync has read it: its pieces are in the source's table.
  synced?: boolean;
};
/**
 * helixwfev1ListFoundryEventBatchesResponse: GET .../connectors/{connectorId}/event-batches,
 * newest first; `limit` at most 100, 0 means 20.
 */
export type ListFoundryEventBatchesResponse = {
  batches?: FoundryEventBatch[];
  // int64: pieces in batches no sync has read yet.
  pendingPieces?: string;
  capacity?: EventSourceCapacity | null;
};

// ---------------------------------------------------------------- Ontology: versions and generations

/** v1OntologySource */
export type OntologySource =
  | "ONTOLOGY_SOURCE_UNSPECIFIED"
  | "ONTOLOGY_SOURCE_MANUAL"
  | "ONTOLOGY_SOURCE_GENERATED";

/**
 * v1OntologyFormat: what a version's definition holds. A version without it
 * (a WFE from before the field) is FOUNDRY_V1.
 */
export type OntologyFormat =
  | "ONTOLOGY_FORMAT_UNSPECIFIED"
  | "ONTOLOGY_FORMAT_FOUNDRY_V1"
  | "ONTOLOGY_FORMAT_AGENT_BUNDLE";

/**
 * v1OntologyVersion: one immutable version of an ontology. `definition` is
 * left out of lists; its shape is FoundryDefinition or AgentBundleDefinition,
 * as `format` says.
 */
export type OntologyVersion = {
  id: string;
  workspaceId?: string;
  ontologyId?: string;
  version?: number;
  definition?: Record<string, unknown> | null;
  source?: OntologySource;
  generationId?: string;
  createdByUserId?: string;
  createdAt?: string | null;
  isCurrent?: boolean;
  format?: OntologyFormat;
  // Its scope: the document sets its loads read (a generated version's are
  // its generation's picks, a revised one's its parent's plus the sets its
  // revised routes name); empty reads none.
  documentSetIds?: string[];
  // A revised version's draft items came without their evidence cards: they
  // measure the ontology's load, and the caller can't query its database
  // (database.query.read on its project). Get, current and set-current only.
  evidenceWithheld?: boolean;
};

/**
 * helixwfev1OntologyVersionResponse: GET .../ontologies/{ontologyId}/versions/{id}
 * and POST .../ontologies/{ontologyId}:set-current. The current version's id
 * is the ontology's `currentVersionId`.
 */
export type OntologyVersionResponse = { version?: OntologyVersion | null };

/** helixwfev1ListOntologyVersionsResponse: newest first; `limit` at most 100. */
export type ListOntologyVersionsResponse = { versions?: OntologyVersion[] };

/** v1WfeServiceSetCurrentOntologyVersionBody: a stale expectation is FAILED_PRECONDITION. */
export type SetCurrentOntologyVersionRequest = {
  versionId: string;
  // The version the caller saw as current; empty for none.
  expectedCurrentVersionId?: string;
};

/** v1OntologyGenerationStatus */
export type OntologyGenerationStatus =
  | "ONTOLOGY_GENERATION_STATUS_UNSPECIFIED"
  | "ONTOLOGY_GENERATION_STATUS_QUEUED"
  | "ONTOLOGY_GENERATION_STATUS_RUNNING"
  | "ONTOLOGY_GENERATION_STATUS_SUCCEEDED"
  | "ONTOLOGY_GENERATION_STATUS_FAILED";

/** v1OntologyGenerationTable: one synced table a generation drafts from. */
export type OntologyGenerationTable = { connectorId?: string; table?: string };

/**
 * v1OntologyGenerationDocument: a table file a generation drafts from, and
 * the set it was in then. Its mappings name it by its document id.
 */
export type OntologyGenerationDocument = { id?: string; setId?: string };

/** v1OntologyGeneration: one request to draft an ontology version. */
export type OntologyGeneration = {
  id: string;
  workspaceId?: string;
  ontologyId?: string;
  status?: OntologyGenerationStatus;
  goal?: string;
  // One line of plain text, at most 60 characters, describing the goal; set
  // when the generation is created. Older generations have none.
  title?: string;
  tables?: OntologyGenerationTable[];
  // The version that was current when it was requested; the draft extends
  // it. Unset for a revision, whose parent is revision.parentVersionId.
  baseVersionId?: string;
  attempts?: number;
  // Why the last attempt failed, written for the customer.
  error?: string;
  // The draft it stored, once succeeded; it is not made current.
  resultVersionId?: string;
  model?: string;
  requestedByUserId?: string;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
  // When a queued generation runs next; later than now while a retry waits.
  nextAttemptAt?: string | null;
  // The document sets it drafts from, its version's scope; empty: none.
  documentSetIds?: string[];
  // The table files of those sets it drafts from beside the tables, each a
  // table of its own; a revision's are its parent's.
  documents?: OntologyGenerationDocument[];
  // Set exactly for a revision of a version (ReviseOntologyVersion).
  revision?: OntologyRevision | null;
  // The sample it drafts from in place of the AI ("kestrel"); "" for an AI
  // draft.
  sample?: string;
};

/**
 * v1OntologyRevision: what a revision generation revises, and what came of
 * it. Once it ended, GET .../generations/{id} has its result, `resultJson`
 * (OntologyReviseResult, without the definition, refusals included), or,
 * for a caller who can't query the ontology's database (database.query.read
 * on its project), `resultWithheld`: the result measures its load. Lists
 * have neither.
 */
export type OntologyRevision = {
  // The version it revises.
  parentVersionId?: string;
  // A preview: it stores no version.
  dryRun?: boolean;
} & (
  | { resultJson?: string; resultWithheld?: never }
  | { resultWithheld: true; resultJson?: never }
);

/** helixwfev1OntologyGenerationResponse */
export type OntologyGenerationResponse = {
  generation?: OntologyGeneration | null;
};

/** helixwfev1ListOntologyGenerationsResponse: newest first; `limit` at most 100. */
export type ListOntologyGenerationsResponse = {
  generations?: OntologyGeneration[];
};

/**
 * v1FoundryEventSourceMode: how a generation reads an API source. The site
 * never sends UNSPECIFIED, which the WFE refuses.
 */
export type FoundryEventSourceMode =
  // Its table, through mappings: pieces reach the graph when the ontology
  // refreshes.
  | "FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"
  // Its pieces as text documents of its own set, extracted as they arrive.
  | "FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION";
/** v1FoundryEventSourcePick: an API source a generation drafts from, and how. */
export type FoundryEventSourcePick = {
  connectorId: string;
  mode: FoundryEventSourceMode;
};

/**
 * v1WfeServiceGenerateOntologyBody: POST .../ontologies/{ontologyId}:generate.
 * Drafts from every synced table of the chosen connectors (all when empty,
 * none with `noConnectors`) and the chosen document sets: each set's table
 * files that have been read are tables of their own (files still being read
 * are left out), and its Markdown, MDX, text and PDFs whose text is read
 * may be routed. It needs one source of either kind: a synced table, a
 * table file, or a set holding text (FAILED_PRECONDITION without one); tables and table files together count
 * toward the stamp's cap. A goal is at most 2000 characters; one live
 * generation per ontology (ALREADY_EXISTS).
 */
export type GenerateOntologyRequest = {
  goal?: string;
  connectorIds?: string[];
  // The document sets the draft reads and may route, which become its
  // version's scope: the agent reads the newest few documents of each.
  // Empty means none; at most 20 (INVALID_ARGUMENT past that).
  documentSetIds?: string[];
  // Draft from the sets alone, even when the workspace has connectors;
  // connectorIds must then be empty (INVALID_ARGUMENT otherwise).
  noConnectors?: boolean;
  // API sources to draft from, each once, with how this ontology reads it.
  // connectorIds names no API source, and its empty default never includes
  // one; noConnectors governs connectorIds only. A structured source counts
  // as one table, once its table has rows (FAILED_PRECONDITION before). An
  // extraction source reads its own document set, one of the 20 sets, and
  // needs a textField and a synced piece (FAILED_PRECONDITION without
  // either); documentSetIds never names that set (INVALID_ARGUMENT).
  eventSources?: FoundryEventSourcePick[];
  // Foundry's sample company ("kestrel", the only one) to draft from in
  // place of the AI: the generation stores its ready-made draft, mapped to
  // this workspace's connection to the sample's database, with no model
  // call. It needs exactly that connector in connectorIds and the set of
  // the sample's documents in documentSetIds, with no eventSources
  // (INVALID_ARGUMENT otherwise), and each of the sample's tables synced
  // (FAILED_PRECONDITION until then).
  sample?: string;
};

/**
 * v1OntologyEdit: one edit of a version's draft, put an item or remove one.
 * `put` (v1OntologyPut) is the item's whole TOML snippet, replacing the item
 * if the draft has it; an item id is at most 256 bytes, and a revision's
 * snippets add up to at most 1 MiB.
 */
export type OntologyEdit =
  | { put: { itemId: string; toml: string }; remove?: never }
  | { remove: { itemId: string }; put?: never };
/** WfeServiceReviseOntologyVersionBody; returns OntologyGenerationResponse. */
export type ReviseOntologyVersionRequest = {
  edits: OntologyEdit[];
  dryRun?: boolean;
};
/**
 * The engine's Revise JSON (engine 3.7.0's revise_result.json, as the backend stores it in
 * `revision.resultJson`, with no definition). `budget`: measuring on the load
 * needed more than its budget, a refusal with that one finding and no cards.
 */
export type AgentRuleCounts = { count: number; evaluated: number };
export type OntologyReviseFinding = {
  code:
    | "EditNotAllowed"
    | "InvalidDefinition"
    | "InvalidEdit"
    | "Validation"
    | "Evaluation"
    | "Lint"
    | "no_comparable_load"
    | "budget";
  item_id: string | null;
  message: string;
};
export type OntologyReviseCard = {
  item: string;
  verdict: "rule" | "warn" | "refused";
  card: AgentEvidenceCard;
};
/** Worker reports omit the definition; only revised results have rule counts. */
export type OntologyReviseResult = {
  findings: OntologyReviseFinding[];
  cards: OntologyReviseCard[];
} & (
  | { status: "revised"; rules: AgentRuleCounts }
  | { status: "refused"; rules?: never }
);

// A version's `definition`, in either format. These are stored JSON, not
// swagger messages: the keys are snake_case as written.

/** Definition v1 (the backend's ontology package). */
export type FoundryDefinition = {
  entities?: {
    name: string;
    description?: string;
    source?: { connector_id?: string; table?: string };
    primary_key?: string;
    properties?: { name: string; type?: string; description?: string }[];
  }[];
  relationships?: {
    name: string;
    from: string;
    to: string;
    from_property?: string;
    to_property?: string;
    // one_to_one, one_to_many, many_to_one or many_to_many
    cardinality?: string;
  }[];
  rules?: {
    name: string;
    description?: string;
    entity?: string;
    // A SQL boolean expression.
    expression?: string;
  }[];
  constraints?: {
    name: string;
    entity?: string;
    // unique, not_null, reference or check
    kind?: string;
    properties?: string[];
    expression?: string;
  }[];
};

/** How an agent turn ended (ontology-agent TurnResult status). */
export type AgentTurnStatus = "done" | "needs_input" | "budget_exhausted";

/** A question the agent asked about its draft. */
export type AgentQuestion = {
  id: string;
  // Draft items it is about: "type.Order", "mapping.orders"...
  items?: string[];
  text: string;
  options?: string[];
  default?: string | null;
  blocking?: boolean;
};

/** The engine's EvidenceCard (ontology-agent turn_result schema); source text is untrusted. */
export type AgentEvidenceCard = {
  rev: number;
  sample: string;
  instances: number;
  confirmed: number;
  added: number;
  contradicted: number;
  unmeasurable: boolean;
  precision: number | null;
  failure_rate_bound: number | null;
  violations: {
    constraint: string;
    severity: "block" | "warn" | "info";
    before: number;
    after: number;
  }[];
  subjects: number;
  violation_rate: number | null;
  review_facts: number;
  unrepresentable: number;
  counterexamples: {
    fact: string;
    proof: string[];
    // The source and mapping behind the premises; a card never names rows.
    rows: { source: string; mapping: string }[];
  }[];
  reason: string;
  // What the counts leave out: rows the item needs missing from the dry
  // run's sample (`rows`: a mapping that can fill it or, for a rule, rows
  // stating a head it derived), the document preview (`documents`), both, a
  // comparable load (Revise had none), or entities of the load over the read
  // budget.
  // Counts are never a full-data estimate.
  unmeasured?:
    | "rows"
    | "documents"
    | "rows, documents"
    | "load"
    | "entities"
    | null;
};

/** One item of the agent's draft, its snippet as canonical TOML. */
export type AgentDraftItem = {
  toml: string;
  status?: "proposed" | "user_edited" | "approved";
  rationale?: string;
  serves?: string[];
  evidence_ids?: string[];
  basis?: string | null;
  pending?: string | null;
  verdict?: "rule" | "warn" | "refused" | null;
  card?: AgentEvidenceCard | null;
  // An `enrichment.*` item's measure of its clues, when the agent drafted
  // it; Revise drops it when an edit changes the item or its type.
  clues?: AgentEnrichmentClues | null;
};

/** The agent's DraftView: the draft with its rendered files. */
export type AgentDraftView = {
  ontology_toml?: string;
  mappings_toml?: string;
  draft?: {
    rev?: number;
    base_version?: string | null;
    items?: Record<string, AgentDraftItem>;
    left_out?: { what: string; reason: string }[];
    questions?: AgentQuestion[];
  };
};

/**
 * agent_bundle (the backend's ontologyagent Definition):
 * the ontology agent's draft as its start turn rendered it.
 */
export type AgentBundleDefinition = {
  format: "agent_bundle";
  parent_version_id?: string;
  rules?: AgentRuleCounts | null;
  status?: AgentTurnStatus;
  ontology_toml?: string;
  mappings_toml?: string;
  draft?: AgentDraftView;
  questions?: AgentQuestion[];
  summary?: string | null;
  usage?: {
    input?: number;
    cache_read?: number;
    cache_write?: number;
    output?: number;
    usd?: number;
  };
};

// ---------------------------------------------------------------- Ontology: ontologies
// Routes under /v1/workspaces/{workspaceId}/foundry/ontologies...: a
// workspace holds ontologies, each bound to one Helix database (a GA tenant)
// in one of the workspace's projects, and one ontology per database. Its
// generations, versions, current version and loads belong to it; documents
// are the workspace's, read by the sets its versions' scopes name. Deleting
// an ontology removes Foundry's rows only, never the database, and leaves
// the workspace's documents.

/** v1FoundryOntology */
export type FoundryOntology = {
  id: string;
  name?: string;
  // projectId, tenantId and databaseName are empty for a member who can't
  // read the project's databases.
  projectId?: string;
  tenantId?: string;
  // The database's name, as the tenant names it.
  databaseName?: string;
  // Unset (or "") while no version is current.
  currentVersionId?: string;
  createdAt?: string | null;
  // The load reads go to: the newest that finished writing, unless another
  // was made current since. Unset before any load finished.
  currentLoad?: {
    id: string;
    version?: string;
    finishedAt?: string | null;
  } | null;
  // The newest load job (kind "load"), live or finished.
  lastLoad?: {
    jobId?: string;
    status?: JobStatus;
    finishedAt?: string | null;
  } | null;
  // The job running or queued now, of any kind; pages poll while it is set.
  activeJob?: {
    kind?: JobKind;
    loadId?: string;
    phase?: JobPhase;
    progress?: JobProgress | null;
  } | null;
  // The uploaded documents its reading version's scope holds (the current
  // load's version, else the current version); 0 with neither.
  counts?: { documents?: number } | null;
  // Whether a changed table it reads loads its current version again by
  // itself (a load of origin refresh), at most once per the plan's interval.
  autoRefresh?: boolean;
  // With autoRefresh: when the refresh it owes may start, since a table its
  // current load reads changed or a connector's document it read was
  // replaced or removed; unset when it owes none.
  refreshDueAt?: string | null;
  // Why an owed refresh waits: "busy" (a job is live); empty otherwise.
  refreshWaiting?: string;
};

/**
 * v1WfeServiceCreateFoundryOntologyBody: POST .../foundry/ontologies. The
 * tenant must be a live database of the workspace the caller may write to,
 * and not bound to another ontology (ALREADY_EXISTS). The same
 * `clientToken` returns the same ontology.
 */
export type CreateOntologyRequest = {
  name: string;
  tenantId: string;
  clientToken: string;
};

/** helixwfev1FoundryOntologyResponse: create, get and delete. */
export type OntologyResponse = { ontology: FoundryOntology };

/** helixwfev1ListFoundryOntologiesResponse: newest first. */
export type ListOntologiesResponse = {
  ontologies?: FoundryOntology[];
  nextPageToken?: string;
};

// ---------------------------------------------------------------- Data: documents
// Routes under /v1/workspaces/{workspaceId}/foundry/documents...: files of
// the workspace (Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet and
// PDF), each in one of its document sets (folders of the Data tree). Text
// holds 1 byte to 1 MiB (foundry.documents.max_bytes), a table file or a PDF
// 3 MiB (max_table_bytes, max_pdf_bytes). The bytes go straight to storage
// through a presigned PUT, never through /papi. Statuses are lowercase
// names. How ontologies read a document is its `read.kind`:
// - text (Markdown, MDX, text): a load reads the documents of the sets its
//   version's scope names (OntologyVersion.documentSetIds), for what its
//   routes say they fill; MDX as Markdown, its code blanked to spaces of the
//   same length, never compiled or run.
// - table (CSV, TSV, JSON, JSONL, Parquet): read once, when its upload
//   completes, as a table of its own that a version's mappings name by the
//   document's id. Never extracted, it joins no load's documents.
// - pdf: read once, when its upload completes: its text extracted in
//   reading order, its pages separated by a form feed (`\f`). Once that read
//   is READY, loads and generations read the text as plain text, and
//   :content serves it. A PDF with no text layer (scanned), or one that is
//   damaged, locked or too long, fails its read (`error` says why), and a
//   load records it as failed with the read's code.
// Completing an upload or moving a document has every ontology whose live
// or current load reads its set read it (text and PDFs only; a PDF once its
// text is read, READING until then), and says what happened for each
// (`reads`).
// A connector's syncs also write documents: the text of each row of one of
// its tables (a Granola note), into the connector's own sets (see Data:
// document sets). Such a document has a `source`, is only uploaded or
// deleted (once replaced or removed at the source), and is the connector's:
// renaming, moving or deleting it is FAILED_PRECONDITION ("this document
// belongs to the <name> connection; change its settings there"). The 200
// documents a workspace holds, the same-bytes answer of a create and the
// workspace's list without `setId` or `ids` count uploads only.

/**
 * v1WfeServiceCreateFoundryDocumentBody.mediaType, mapped from the file's
 * extension: .md and .markdown, .mdx, .txt, .json, .csv, .tsv, .jsonl and
 * .ndjson, .parquet, .pdf. A rename may change the extension; the type stays.
 */
export type DocumentMediaType =
  | "text/markdown"
  | "text/mdx"
  | "text/plain"
  | "application/json"
  | "text/csv"
  | "text/tab-separated-values"
  | "application/x-ndjson"
  | "application/vnd.apache.parquet"
  | "application/pdf";

/** v1FoundryDocument.status: the upload lifecycle. */
export type DocumentStatus = "pending_upload" | "uploaded" | "deleted";

/**
 * v1FoundryDocumentExtraction.status. No extraction means not read yet;
 * out_of_scope: the load's version routes its set nowhere, so it was stored
 * with its sections and nothing was read from it.
 */
export type ExtractionStatus =
  | "extracted"
  | "duplicate"
  | "failed"
  | "out_of_scope";

/**
 * v1FoundryExtractionError: why reading a document failed. `unsupported` or
 * `too_large` for a PDF: its read failed so (FoundryFileRead.error says why).
 */
export type ExtractionError = {
  code?: "too_large" | "unsupported" | "model" | "store";
  // The next load tries again.
  retryable?: boolean;
};

/** v1FoundryDocumentExtraction: what an ontology's current load read from it. */
export type DocumentExtraction = {
  status?: ExtractionStatus;
  // The document with the same text, for a duplicate.
  duplicateOf?: string;
  error?: ExtractionError | null;
  sections?: number;
  entities?: number;
  mentions?: number;
  facts?: number;
  reviews?: number;
  // int64: what reading it cost, in millionths of a dollar.
  usdMicros?: string;
  finishedAt?: string | null;
};

/**
 * v1FoundryDocumentOntology: one ontology a document is associated with:
 * its current load holds the document, or its reading version (the current
 * load's, else the current version) has the document's set in scope.
 */
export type DocumentOntology = {
  ontologyId?: string;
  // The reading version's routes name the document's set: it is read for
  // what it holds, not only stored.
  routed?: boolean;
  // What its current load made of it; unset until that load read it.
  extraction?: DocumentExtraction | null;
};

/** v1FoundryReadKind: how ontologies read a type of document. */
export type FoundryReadKind =
  | "FOUNDRY_READ_KIND_UNSPECIFIED"
  | "FOUNDRY_READ_KIND_TEXT"
  | "FOUNDRY_READ_KIND_TABLE"
  | "FOUNDRY_READ_KIND_PDF";

/**
 * v1FoundryFileReadState: what reading a document has come to. READY:
 * ontologies can read it (text, or a file whose read succeeded: a PDF's text
 * is then on :content); READING: being read, or waiting to be (a generation
 * started now leaves it out; a load waits for a PDF and reads it once its
 * read ends); FAILED: it can't be read, `error` says why (a scanned PDF: no
 * text to read), and a load records a PDF failed with the same code.
 */
export type FoundryFileReadState =
  | "FOUNDRY_FILE_READ_STATE_UNSPECIFIED"
  | "FOUNDRY_FILE_READ_STATE_READY"
  | "FOUNDRY_FILE_READ_STATE_READING"
  | "FOUNDRY_FILE_READ_STATE_FAILED";

/** v1FoundryFileReadMode: how deeply a table file or a PDF was read. */
export type FoundryFileReadMode =
  | "FOUNDRY_FILE_READ_MODE_UNSPECIFIED"
  | "FOUNDRY_FILE_READ_MODE_STANDARD"
  | "FOUNDRY_FILE_READ_MODE_DEEP";

/**
 * v1FoundryFileRead: how ontologies read a document, and what reading it
 * has come to; unset until its upload completes, and once deleted.
 */
export type FoundryFileRead = {
  kind?: FoundryReadKind;
  state?: FoundryFileReadState;
  // int64: a table file's rows, once read.
  rows?: string;
  // A PDF's pages, once read.
  pages?: number;
  // The read shown, for a table file or a PDF; unset for text.
  mode?: FoundryFileReadMode;
  // Why it can't be read, exactly when FAILED.
  error?: {
    // too_large (past a reading limit: a PDF of more than 150 pages, or
    // whose text is past 1 MiB) or unsupported (not what its type says; a
    // PDF that is damaged, encrypted, or scanned with no text, whose message
    // is "Scanned PDF: no text to read").
    code?: string;
    // What to fix, for the user.
    message?: string;
  } | null;
  // A READING read that failed for a reason that isn't the file's (such as
  // storage) and waits to be read again.
  retrying?: boolean;
};

/**
 * v1FoundryDocumentSource: the row of a connector's table a document is the
 * text of. New text at the source is a new document that replaces it; a new
 * version with the same text changes it in place.
 */
export type FoundryDocumentSource = {
  // The row's key at the source (a Granola note's id), unique among its
  // set's documents that are not deleted.
  externalId?: string;
  // The source's version of the row's text.
  version?: string;
  // The item's page at the source, an absolute https URL; empty when none.
  url?: string;
  // The item's own time at the source (a note's creation), which orders the
  // set's documents, newest first.
  at?: string | null;
};

/** v1FoundryDocument */
export type FoundryDocument = {
  id: string;
  name?: string;
  mediaType?: string;
  // int64
  sizeBytes?: string;
  // Lowercase hex SHA-256 of the bytes.
  sha256?: string;
  status?: DocumentStatus;
  createdAt?: string | null;
  // The document set it is in.
  setId?: string;
  // Read through an ontology's routes: what its current load made of it.
  // Unset on the workspace's routes, and until that load read it.
  extraction?: DocumentExtraction | null;
  // The ontologies associated with it, on the workspace's GET only.
  ontologies?: DocumentOntology[];
  // How ontologies read it; unset until its upload completes.
  read?: FoundryFileRead | null;
  // Where a connector's document comes from; unset for an upload.
  source?: FoundryDocumentSource | null;
};

/**
 * v1WfeServiceCreateFoundryDocumentBody: POST .../foundry/documents. At most
 * its type's cap (1 MiB of text, 3 MiB for a table file or a PDF) and 200
 * documents a workspace. The same `clientToken`, or the same
 * bytes (by `sha256`) anywhere in the workspace, return the document already
 * there (`existing`), in the set it is in, which may not be `setId`.
 */
export type CreateDocumentRequest = {
  name: string;
  mediaType: DocumentMediaType;
  // int64, sent as a string.
  sizeBytes: string;
  sha256: string;
  clientToken: string;
  // A live set of the workspace (NOT_FOUND otherwise).
  setId: string;
};

/**
 * v1FoundryDocumentUpload: a presigned PUT of the bytes. Every header must be
 * sent as given (they are signed, with the size and the SHA-256).
 */
export type DocumentUpload = {
  url: string;
  method?: string;
  headers?: Record<string, string>;
  expiresAt?: string | null;
};

/**
 * helixwfev1CreateFoundryDocumentResponse. No upload once the document is
 * uploaded. `existing`: the workspace had it already (the same client token,
 * or the same bytes), in `document.setId`.
 */
export type CreateDocumentResponse = {
  document: FoundryDocument;
  upload?: DocumentUpload | null;
  existing?: boolean;
};

/** helixwfev1FoundryDocumentResponse: get, rename and delete. */
export type DocumentResponse = { document: FoundryDocument };

/**
 * v1FoundryDocumentReadOutcome: what completing or moving a document did for
 * one ontology whose live or current load reads its set. QUEUED: an extract
 * into the current load; OWED: a live job reads it, or queues the extract
 * that does; NO_BUDGET: this month's budget is spent, the next load or
 * extract reads it; HELD: the current load holds it already (a move) and
 * keeps what it read until the next load; SKIPPED: the caller may not write
 * the ontology's database; READING: a PDF whose text is still being read:
 * the load holds it, and the read's end has the ontology read it (queued,
 * or owed by a live job).
 */
export type DocumentReadOutcome =
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_UNSPECIFIED"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_OWED"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_NO_BUDGET"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_HELD"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_SKIPPED"
  | "FOUNDRY_DOCUMENT_READ_OUTCOME_READING";

/** v1FoundryDocumentRead: the outcome for one ontology. */
export type DocumentRead = {
  ontologyId?: string;
  outcome?: DocumentReadOutcome;
};

/**
 * helixwfev1FoundryDocumentReadsResponse: POST .../documents/{id}:complete
 * and :move. `reads` has one entry per ontology whose live or current load
 * reads the document's set; none when no ontology does. Completing twice
 * reads nothing again.
 */
export type DocumentReadsResponse = {
  document: FoundryDocument;
  reads?: DocumentRead[];
};

/**
 * GET .../documents, one of three lists: the workspace's uploads (no query),
 * one set's documents (`setId`, uploads or a connector's), or these documents
 * (`ids`). Each is newest first (a connector's document by its `source.at`),
 * without deleted documents, 50 a page (`pageToken`), but `ids`: at most 100,
 * repeated (`ids=a&ids=b`), of either origin, deleted included, in the order
 * asked, on one page, with no `setId` or `pageToken` (INVALID_ARGUMENT
 * otherwise). An id the workspace has no document for is left out.
 */
export type ListDocumentsQuery =
  | { setId?: string; pageToken?: string }
  | { ids: readonly string[] };

/** helixwfev1ListFoundryDocumentsResponse: newest first, 50 a page. */
export type ListDocumentsResponse = {
  documents?: FoundryDocument[];
  nextPageToken?: string;
};

/**
 * v1WfeServiceMoveFoundryDocumentBody: POST .../documents/{id}:move, to
 * another live set of the workspace. A load keeps what it read: the move
 * applies to it from the next load. Answers DocumentReadsResponse.
 */
export type MoveDocumentRequest = { setId: string };

/**
 * v1WfeServiceUpdateFoundryDocumentBody: PATCH .../documents/{id}. The name
 * as an upload's: 1 to 255 characters once trimmed, with no slash,
 * backslash or control character; its extension may change.
 */
export type UpdateDocumentRequest = { name: string };

/**
 * helixwfev1GetFoundryDocumentContentResponse: GET .../documents/{id}:content,
 * an uploaded text document's text (Markdown, MDX, text, JSON, CSV, TSV or
 * JSONL, at most its type's cap; bytes that are not UTF-8 read as U+FFFD),
 * and a PDF's text once its read is READY, its pages separated by a form
 * feed (`\f`), at most 1 MiB. FAILED_PRECONDITION for a document that isn't
 * uploaded, a PDF whose read isn't READY ("the PDF's text is still being
 * read; try again shortly", "the PDF's text can't be read: <why>"), and
 * Parquet: "<format> documents have no text; use :bytes".
 */
export type DocumentContentResponse = { text?: string };

/**
 * helixwfev1GetFoundryDocumentBytesResponse: GET .../documents/{id}:bytes, an
 * uploaded document's bytes, of any type, at most its type's cap; read access, as
 * :content. `data` is base64 (grpc-gateway's JSON for bytes).
 * FAILED_PRECONDITION for a document that isn't uploaded.
 */
export type DocumentBytesResponse = { mediaType?: string; data?: string };

/**
 * GET .../ontologies/{ontologyId}/documents?status=: what the ontology reads
 * (its current load's documents, and the uploaded documents of its reading
 * version's scope), each with its extraction in the current load, kept to
 * those the current load holds so: waiting (not read yet), extracted, review
 * (extracted, with reviews), duplicate, failed or out_of_scope.
 */
export type OntologyDocumentStatus =
  | "waiting"
  | "extracted"
  | "review"
  | "duplicate"
  | "failed"
  | "out_of_scope";

// ---------------------------------------------------------------- Data: document sets
// Routes under /v1/workspaces/{workspaceId}/foundry/document-sets...: the
// workspace's documents are grouped in sets, each document in exactly one.
// A version reads the sets of its scope, and its routes (mappings.toml
// [extractions.*]) name the sets whose documents they read for what they
// hold; a document in a set of the scope no route names is stored, not read
// (out_of_scope). Names are unique in the workspace, case and accents aside
// (ALREADY_EXISTS); a workspace creates up to 20 sets; a set is deleted only
// when it holds no document (FAILED_PRECONDITION).
// A connector's syncs make a set of their own per stream (`connector`): one
// per connector and stream for as long as the connector lives, named by the
// stream ("Notes"). It takes no place among the 20 or their names, and is
// the connector's: changing or deleting it, and uploading or moving a
// document into it, is FAILED_PRECONDITION ("this set belongs to the <name>
// connection; change its settings there"). Deleting the connector deletes
// it. A generation's pick of sets counts it. An API source with a text field
// has one ("Pieces", stream `pieces`): a Markdown document per piece, the
// newest `maxDocuments` by receipt, which a generation reads only through
// the source's extraction pick.

/** v1FoundryDocumentSetReader: an ontology whose reading version has the set in scope. */
export type DocumentSetReader = {
  ontologyId?: string;
  // Its reading version's routes name the set.
  routed?: boolean;
};

/**
 * v1FoundryDocumentSetConnector: the connector and stream whose documents a
 * set holds. The user calls the set "<connectorName> / <name>".
 */
export type DocumentSetConnector = {
  connectorId?: string;
  // The kind's name for what the set holds (notes); it never changes.
  stream?: string;
  // The most documents a sync of everything keeps: the newest at the source.
  // A sync of changes may add past it until the next one, except in an API
  // source's set, which never holds more.
  maxDocuments?: number;
  // The connector's name when the set was read.
  connectorName?: string;
  // The connector's kind (granola, events); it never changes.
  connectorKind?: string;
  // int64 µ$: what work no person started (refreshes, and extracts no one
  // asked for) has spent extracting the set's documents this UTC month. "0"
  // for a set without an allowance.
  monthSystemUsdMicros?: string;
  // int64 µ$: the set's allowance, the most that work may spend on it in a
  // UTC month; set only for a set with one (an API source's). Once it is
  // spent the set is paused.
  maxMonthSystemUsdMicros?: string;
  // Whether the set's syncs write no documents now: it holds some, and work
  // no person started can spend no more extracting it this UTC month (its
  // allowance, its workspace's for all its API sources, or the workspace's
  // month's share for such work is spent). The graph keeps the pieces it
  // extracted; the newest wait for next month or a load a person starts,
  // and a sync a person starts (a text field's change) still writes them.
  // False for a set without an allowance.
  paused?: boolean;
};

/** v1FoundryDocumentSet */
export type FoundryDocumentSet = {
  id: string;
  // 1 to 120 characters.
  name?: string;
  // What its documents hold, in the user's words; the ontology agent reads
  // it. At most 1000 characters.
  description?: string;
  // Its documents that are not deleted.
  documents?: number;
  createdAt?: string | null;
  // On lists: the ontologies whose reading version (the current load's,
  // else the current version) has it in scope.
  readBy?: DocumentSetReader[];
  // The connector whose syncs write it; unset for a set a person made.
  connector?: DocumentSetConnector | null;
};

/** helixwfev1ListFoundryDocumentSetsResponse: oldest first. */
export type ListDocumentSetsResponse = { sets?: FoundryDocumentSet[] };

/** v1WfeServiceCreateFoundryDocumentSetBody: POST .../document-sets. */
export type CreateDocumentSetRequest = { name: string; description?: string };

/**
 * v1WfeServiceUpdateFoundryDocumentSetBody: PATCH .../document-sets/{setId}.
 * Each field sent is set; one left out stays.
 */
export type UpdateDocumentSetRequest = { name?: string; description?: string };

/** helixwfev1FoundryDocumentSetResponse: create and update. */
export type DocumentSetResponse = { set: FoundryDocumentSet };

/** helixwfev1DeleteFoundryDocumentSetResponse: empty. */
export type DeleteDocumentSetResponse = Record<string, never>;

// ---------------------------------------------------------------- Data: layout
// GET and PUT /v1/workspaces/{workspaceId}/foundry/data-layout: how the
// workspace's Data tree is arranged, one JSON object per workspace. The
// backend stores it as it is, beyond requiring an object of at most 64 KiB
// as protojson writes it; the site owns its shape
// (src/web/areas/data/layout.ts). `revision` counts writes: a PUT names the
// revision it read, and FAILED_PRECONDITION means another write came first,
// a first write at "0" included. Reading needs workspace.read, writing
// workspace.connectors.manage.

/** helixwfev1FoundryDataLayoutResponse: GET and PUT. {} and "0" before the first write. */
export type DataLayoutResponse = {
  layout?: Record<string, unknown> | null;
  // int64: how many writes it has had.
  revision?: string;
};

/** v1WfeServicePutFoundryDataLayoutBody: PUT .../data-layout. */
export type PutDataLayoutRequest = {
  layout: Record<string, unknown>;
  // int64: the revision read.
  expectedRevision: string;
};

// ---------------------------------------------------------------- Ontology: loads
// Routes under .../ontologies/{ontologyId}/loads...: a load is one write of a
// version into the ontology's database. Its id is the id of the job that
// creates it, and every node and edge it writes carries that id as
// `foundry_load`, so loads of different versions sit side by side. The
// current load serves reads; a document uploaded later is read into it by an
// extract job. Foundry keeps the current load and the newest others (five in
// all) and deletes the rest in the background, with delete jobs.

/** v1FoundryLoad.status */
export type LoadStatus =
  | "writing"
  | "ready"
  | "partial"
  | "failed"
  | "deleting"
  | "delete_failed";

/**
 * v1FoundryGraphJob.status. Cancelled: a delete a new load went before (it
 * runs again after), or any job of a deleted ontology.
 */
export type JobStatus =
  | "queued"
  | "running"
  | "succeeded"
  | "budget_exhausted"
  | "failed"
  | "cancelled";

/** v1FoundryGraphJob.phase; a delete job only ever deletes. */
export type JobPhase =
  | "provisioning"
  | "ingest"
  | "documents"
  | "finishing"
  | "deleting";

/**
 * v1FoundryGraphJob.kind: a load job writes a version's tables, then reads
 * the documents; an extract, queued by an upload or a change of current load,
 * only reads documents into its load; a delete removes a load.
 */
export type JobKind = "load" | "extract" | "delete";

/** v1FoundryGraphJob.progress */
export type JobProgress = {
  chunksDone?: number;
  documentsDone?: number;
  documentsTotal?: number;
};

/** v1FoundryGraphJob: one job of a load. */
export type GraphJob = {
  id: string;
  kind?: JobKind;
  // The load it writes or deletes, and that load's version.
  loadId?: string;
  versionId?: string;
  status?: JobStatus;
  phase?: JobPhase;
  progress?: JobProgress | null;
  // int64: what reading documents has cost, in millionths of a dollar.
  usdMicros?: string;
  // Why the last attempt failed, written for the customer.
  error?: string;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
};

/** v1FoundryLoad: one write of a version, with its newest jobs. */
export type FoundryLoad = {
  id: string;
  versionId?: string;
  // The version it writes, as the graph records it: "foundry-v<number>".
  version?: string;
  status?: LoadStatus;
  current?: boolean;
  createdAt?: string | null;
  finishedAt?: string | null;
  // Its newest jobs, newest first: at most 20.
  jobs?: GraphJob[];
  // How many jobs it has had, and (int64) what all of them spent, in
  // millionths of a dollar: a current load gains an extract per upload.
  jobCount?: number;
  usdMicros?: string;
  // (uint64) The violation records its tables left open, as the ingest
  // counted them; unset while it writes them, or when a retried ingest
  // couldn't count every chunk. Extraction changes the live records.
  ingestViolations?: string;
  // Its People enrichment step; unset when its version enriches no one.
  enrichment?: FoundryLoadEnrichment | null;
};

/**
 * v1WfeServiceCreateFoundryLoadBody: POST .../loads, for an agent draft whose
 * status is done. FAILED_PRECONDITION while a load or extract is live; a live
 * delete gives way. The same `clientToken` returns the same load.
 */
export type CreateLoadRequest = { versionId: string; clientToken: string };

/**
 * helixwfev1FoundryLoadResponse: create, get, :set-current (ready or partial
 * loads only) and delete (not the current load, nor one being written: then
 * FAILED_PRECONDITION). Deleting a load also deletes the edges anyone added
 * to its nodes.
 */
export type LoadResponse = { load: FoundryLoad };

/** helixwfev1ListFoundryLoadsResponse: newest first, every load not deleted yet. */
export type ListLoadsResponse = { loads?: FoundryLoad[] };

/**
 * v1WfeServiceReadFoundryGraphBody: POST .../loads/{loadId}:read. The
 * ontology agent's reads as JSON (src/shared/foundry-read.ts): 1 to 20, run
 * with the load's version's rules evaluated as they are read. The WFE
 * refuses malformed reads (INVALID_ARGUMENT), a load of another ontology or
 * one deleted (NOT_FOUND), one being deleted (FAILED_PRECONDITION), and more
 * reads at once than the workspace may run (RESOURCE_EXHAUSTED).
 */
export type ReadFoundryGraphRequest = { readsJson: string };

/** helixwfev1ReadFoundryGraphResponse: the engine's ReadResult as JSON. */
export type ReadFoundryGraphResponse = { resultJson?: string };

// ---------------------------------------------------------------- Ontology: questions
// Routes under .../ontologies/{ontologyId}/questions...: a question about
// one load, answered by the ontology agent's read-only query agent (the
// engine's `Ask` RPC) in the backend's foundry-ask-worker. A question
// is queued, claimed with its cap reserved once the workspace runs fewer
// questions than it may and the month can hold it, and streams events the
// page polls by `afterSeq`; its result is the engine's JSON as stored.
// Questions of one thread share `threadId` (the first question's id), and
// the worker sends the thread's last five answers as history.

/** v1FoundryQuestion.status: the backend's lifecycle, not the engine's result status. */
export type QuestionStatus =
  | "queued"
  | "running"
  | "answered"
  | "cannot_answer"
  | "failed"
  | "cancelled";

/** v1FoundryQuestion */
export type FoundryQuestion = {
  id: string;
  loadId?: string;
  // The load's version, whose definition the agent was given.
  versionId?: string;
  // The first question's id; a new thread's first question is its own.
  threadId?: string;
  question?: string;
  // What "Ask about this" asked about: an EntityRef as JSON, empty for none.
  focusJson?: string;
  status?: QuestionStatus;
  // The engine's AskResult JSON (snake_case): set once answered or
  // cannot_answer, and for a failed question whose agent sent one.
  resultJson?: string;
  // Why it failed or is being retried, for the user (lowercase, no period).
  error?: string;
  // Attempts claimed so far: a queued question with attempts above 0 is
  // waiting to be retried, and `error` says why.
  attempts?: number;
  // int64 millionths of a dollar: what its attempts spent, and what a
  // running question holds back of the month's budget (its cap).
  usdMicros?: string;
  reservedMicros?: string;
  // The seq of its newest event; 0 (left out) before any.
  lastSeq?: number;
  askedByUserId?: string;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
};

/**
 * v1FoundryQuestionBudget: the workspace's own questions budget (separate
 * from loads'), and the caps every question runs under. Ask is refused once
 * what the month spent plus a question's cap passes the budget; a queued
 * question waits while running questions' reservations leave too little.
 */
export type QuestionBudget = {
  // int64 millionths of a dollar, this calendar month (UTC).
  monthlyUsdMicros?: string;
  spentUsdMicros?: string;
  reservedUsdMicros?: string;
  // Per question.
  questionUsdMicros?: string;
  maxSteps?: number;
  maxWallSecs?: number;
  // Per workspace.
  concurrentLimit?: number;
  hourlyLimit?: number;
};

/**
 * v1WfeServiceAskFoundryQuestionBody: POST .../questions. A question is at
 * most 4,000 characters, `focusJson` an EntityRef as JSON of at most 4 KiB,
 * and `clientToken` at most 64 characters (INVALID_ARGUMENT otherwise; also
 * for a token used for another question). FAILED_PRECONDITION for a load
 * that isn't ready or partial, or once the workspace has spent this month's
 * questions budget; RESOURCE_EXHAUSTED past its questions an hour. Each
 * refusal's message is written for the user. The same `clientToken` returns
 * the question it queued.
 */
export type AskQuestionRequest = {
  // Empty asks the current load.
  loadId?: string;
  // The first question of the thread to continue; empty starts one.
  threadId?: string;
  question: string;
  focusJson?: string;
  clientToken: string;
};

/**
 * helixwfev1FoundryQuestionResponse: ask, get and :cancel. Cancel ends a
 * queued or running question (a running one is charged what it reserved);
 * one that ended otherwise is FAILED_PRECONDITION.
 */
export type QuestionResponse = { question: FoundryQuestion };

/**
 * helixwfev1ListFoundryQuestionsResponse: GET .../questions?threadId&pageToken,
 * newest first, 50 a page, with the workspace's budget this month.
 */
export type ListQuestionsResponse = {
  questions?: FoundryQuestion[];
  nextPageToken?: string;
  budget?: QuestionBudget | null;
};

/** v1FoundryQuestionEvent: one stored event; `eventJson` is an AskEvent. */
export type QuestionEvent = {
  seq: number;
  eventJson: string;
  createdAt?: string | null;
};

/**
 * helixwfev1ListFoundryQuestionEventsResponse: GET
 * .../questions/{id}/events?afterSeq, up to 200 in seq order, with the
 * question's status read first: once it is neither queued nor running and
 * the events reach `lastSeq`, none follows. A retried question's events go
 * on from its last attempt's, from a new `started`.
 */
export type ListQuestionEventsResponse = {
  events?: QuestionEvent[];
  status?: QuestionStatus;
  lastSeq?: number;
};

// The engine's Ask JSON (contract/ontology_agent/ask_event.json and
// ask_result.json), stored as written: snake_case.
// Every value is model or document text: the page parses it without trust.
// The mock emits exactly these shapes, checked against the schemas.

/** A `where` condition; values are typed by the prop, plus "today±Nd". */
export type QueryWhere = {
  prop: string;
  op:
    | "="
    | "!="
    | "<"
    | "<="
    | ">"
    | ">="
    | "contains"
    | "starts_with"
    | "is_null"
    | "not_null";
  value?: unknown;
};

/**
 * A typed ontology query (extraction::query::typed): the `query` tool's
 * input as the model wrote it (defaults left out), a query read, and a
 * result's `queries` (every field written, entity ids resolved).
 */
export type OntologyQuery = {
  start: {
    type: string;
    where?: QueryWhere[];
    ids?: string[] | null;
    text?: string | null;
  };
  steps?: {
    via: string;
    dir: "out" | "in";
    to: string;
    where?: QueryWhere[];
  }[];
  // `at` is a position: 0 the start, n the n-th step's end.
  filters?: ({ at: number } & (
    | { holds: string }
    | { not_holds: string }
    | { violates: string }
  ))[];
  return:
    | {
        rows: {
          // A prop (or `name`) of the entity at `at`; the entity if none.
          columns: { at: number; prop?: string | null }[];
          order_by?: { at: number; prop: string; desc?: boolean } | null;
          limit?: number;
        };
      }
    // The last position when `distinct_at` is null or left out.
    | { count: { distinct_at?: number | null } }
    | { group_count: { at: number; prop: string; limit?: number } }
    | {
        aggregate: {
          fn: "sum" | "avg" | "min" | "max";
          at: number;
          prop: string;
          group_by?: { at: number; prop: string } | null;
          limit?: number;
        };
      };
};

/**
 * One event of an Ask stream (ask_event.json), `started` first and
 * `finished` last. A call's `tool_result` has `evidence_id` for a read
 * recorded as evidence, `error` for a call that failed or was refused, and
 * neither for the `answer` or `cannot_answer` call that ended the question.
 */
export type AskEvent =
  // `provider` is the kind that serves `model` (contract 4.0.0); an engine
  // before it named none.
  | { event: "started"; model: string; provider?: AgentProviderKind }
  | { event: "tool_call"; call_id: string; tool: string; input: unknown }
  | ({ event: "tool_result"; call_id: string; summary: string } & (
      | { evidence_id: string; error?: never }
      | { error: string; evidence_id?: never }
      | { evidence_id?: never; error?: never }
    ))
  | { event: "finished" };

/** An answer's block: plain text with [cN] markers, no links, images or HTML. */
export type AskBlock = { para: string } | { list: string[] };

/**
 * What a recorded version is of: an entity, one of its props, a stated fact,
 * or a violation record (open or not), by its constraint and key.
 */
export type VersionOf =
  | { entity: EntityRef }
  | { prop: { entity: EntityRef; prop: string } }
  | { fact: { link: string; subject: EntityRef; object: EntityRef } }
  | { record: { constraint: string; key: string } };

/** What a citation points at; quote offsets are UTF-8 bytes into a section. */
export type CitationTarget =
  | { entity: EntityRef }
  | { fact: { link: string; subject: EntityRef; object: EntityRef } }
  | { derived: { pred: string; args: EntityRef[] } }
  // row_key is empty for a People enrichment row (`foundry_enrichment`,
  // contract 3.12.0), whose key is the provider's profile id, and only then.
  | { row: { source: string; mapping: string; row_key: string } }
  | { quote: { doc_id: string; section: number; start: number; end: number } }
  // A stored violation record, by its constraint and key.
  | { violation: { constraint: string; key: string } }
  // A tool result as a whole, such as a count.
  | { result: { evidence_id: string } }
  // A past state the load recorded: its transaction (an opaque ULID), when
  // it committed (RFC 3339), and what it is a state of.
  | { version: { tx: string; at: string; of: VersionOf } };

/** A citation: its marker ("c1" for [c1]), the tool result it came from, and its target. */
export type AskCitation = {
  marker: string;
  evidence_id: string;
  target: CitationTarget;
};

/**
 * A query's output as a question records it (ask_result.json RecordedOutput:
 * the Read call's query answer, by `kind`), before the engine cut it for
 * the model: at most 200 rows or 100 groups, every string at most 1,000
 * characters, and `cut` saying what the question's 512 KiB bound left out
 * ("12 rows left out"). A row's cell is the entity at a column without a
 * prop (`{type, id, name}`) or the prop's value, null when absent; a value
 * a document gave is `{"untrusted": text}`. Every group's value but null is
 * `{"untrusted": text}` too, a number's as JSON writes it, whatever gave
 * it: a group is many entities'. Every value is data, shown only as text.
 */
export type QueryOutput = (
  | {
      kind: "rows";
      rows: { cells: unknown[]; edges: unknown[] }[];
      // How many distinct rows there are in all.
      total: number;
    }
  | { kind: "count"; count: number }
  | {
      kind: "group_count";
      // Most first, then by value, the entities without one last (`null`).
      groups: { value: unknown; count: number }[];
      total_groups: number;
    }
  // `value` is null for a mean, least or greatest of no values.
  | { kind: "aggregate"; value: number | null; values: number }
  | {
      kind: "grouped_aggregate";
      // Greatest first, then by value, the entities without one last.
      groups: { value: unknown; aggregate: number | null; values: number }[];
      total_groups: number;
    }
) & { cut?: string | null };

/** How a visual asks to be shown: a hint, a table when the output doesn't suit it. */
export type VisualKind = "table" | "bar" | "line" | "figure";

/**
 * A query result an answer shows as a table or chart (ask_result.json
 * Visual): the result's evidence id (one visual per result), the block it
 * follows (`after`, from 0; after the last when left out), how to show it
 * (left out: as its output's shape suits) and a plain-text title of at most
 * 80 characters. What it shows is the query's recorded output, never text
 * the model wrote.
 */
export type AskVisual = {
  evidence_id: string;
  after?: number | null;
  kind?: VisualKind | null;
  title?: string | null;
};

/**
 * Why a question failed (ask_result.json Failure). A model call that failed
 * for good says how, as AgentModelFailure does, prefixed `model_` (contract
 * 4.0.0); one the provider was unavailable for is `model_failed`.
 */
export type AskFailure =
  | "model_failed"
  | "model_refused"
  | "model_auth"
  | "model_permission"
  | "model_not_found"
  | "model_rate_limited"
  | "model_quota"
  | "model_unsupported"
  | "model_truncated"
  | "model_malformed"
  | "model_timeout"
  | "model_changed"
  | "model_private_host"
  | "no_answer"
  | "load_unknown"
  | "load_mismatch"
  | "store_failed";

/** What a question spent: tokens by kind, and US dollars. */
export type AskUsage = {
  input: number;
  cache_read: number;
  cache_write: number;
  output: number;
  usd: number;
};

/**
 * The end of an Ask (ask_result.json), by `status`: an answer with its
 * citations; why the ontology can't answer and what it lacks (`gap`); a
 * cap reached first; or a failure's reason. Every query it ran and what it
 * spent come with each.
 */
export type AskResult = (
  | {
      status: "answered";
      // At most 4 visuals (contract 3.11.0); none before it.
      answer: { blocks: AskBlock[]; visuals?: AskVisual[] };
      citations: AskCitation[];
    }
  | { status: "cannot_answer"; reason: string; gap: string }
  | { status: "budget_exhausted" }
  | { status: "failed"; reason: AskFailure }
) & {
  // Every query it ran, by the evidence id of its result, with its output
  // (contract 3.11.0) unless the 512 KiB bound dropped it. Question lists
  // leave outputs out: GET of one question carries them.
  queries: {
    evidence_id: string;
    query: OntologyQuery;
    output?: QueryOutput | null;
  }[];
  usage: AskUsage;
};
export type AskResultStatus = AskResult["status"];

// ---------------------------------------------------------------- Helix databases
// The Helix control plane's own routes, as helix-website calls them:
// projects and their databases (tenants) to bind an ontology to, and reads
// of a database with the user's session through the query broker. Foundry
// never offers dedicated clusters: a new database goes on the shared
// cluster, and only tenants are databases here.

/** v1Project */
export type Project = {
  id: string;
  workspaceId?: string;
  slug?: string;
  displayName?: string;
  timestamps?: Timestamps;
};

/** helixwfev1ListProjectsResponse: GET /v1/projects?workspaceId&page.token */
export type ListProjectsResponse = {
  projects?: Project[];
  nextPageToken?: string;
};

/** helixwfev1CreateProjectRequest: POST /v1/projects. */
export type CreateProjectRequest = {
  workspaceId: string;
  slug: string;
  displayName: string;
};

/** v1WorkspaceGateReason: why a disabled database's queries are paused. */
export type WorkspaceGateReason =
  | "WORKSPACE_GATE_REASON_UNSPECIFIED"
  | "WORKSPACE_GATE_REASON_NO_SUBSCRIPTION"
  | "WORKSPACE_GATE_REASON_SUBSCRIPTION_UNPAID"
  | "WORKSPACE_GATE_REASON_QUOTA_EXHAUSTED"
  | "WORKSPACE_GATE_REASON_CREDIT_EXHAUSTED"
  | "WORKSPACE_GATE_REASON_WORKSPACE_DELINQUENT"
  | "WORKSPACE_GATE_REASON_WORKSPACE_OVER_LIMIT";

/** v1Tenant: a database on a cluster, owned by a project. */
export type Tenant = {
  id: string;
  clusterId?: string;
  workspaceId?: string;
  projectId?: string;
  name?: string;
  slug?: string;
  status?: ResourceStatus;
  timestamps?: Timestamps;
  queryState?:
    | "TENANT_QUERY_STATE_UNSPECIFIED"
    | "TENANT_QUERY_STATE_ENABLED"
    | "TENANT_QUERY_STATE_DISABLED";
  gateReason?: WorkspaceGateReason;
};

/** helixwfev1ListTenantsResponse: GET /v1/tenants?projectId&page.token, newest first. */
export type ListTenantsResponse = {
  tenants?: Tenant[];
  nextPageToken?: string;
};

/**
 * helixwfev1CreateTenantRequest: POST /v1/tenants. Without a clusterId the
 * WFE places it on the shared cluster, which needs the workspace's Helix
 * subscription: FAILED_PRECONDITION with reason NO_WORKSPACE_SUBSCRIPTION.
 */
export type CreateTenantRequest = {
  projectId: string;
  name: string;
  slug: string;
};

/**
 * helixwfev1CreateTenantResponse. `token` is the database's first key, shown
 * once by the dashboard; Foundry never reads or keeps it.
 */
export type CreateTenantResponse = { tenant?: Tenant };

/** The ErrorInfo reason of a CreateTenant refused for want of a subscription. */
export const NO_WORKSPACE_SUBSCRIPTION = "NO_WORKSPACE_SUBSCRIPTION";

/** v1DatabaseReference: a tenant (Foundry never sends a cluster). */
export type DatabaseReference = { tenantId: string };

/**
 * helixwfev1ExecuteDatabaseQueryRequest: POST /v1/databases:query-read.
 * `queryJson` is base64 of the UTF-8 v3 dynamic query request.
 */
export type ExecuteDatabaseQueryRequest = {
  database: DatabaseReference;
  queryJson: string;
};

/**
 * helixwfev1ExecuteDatabaseQueryResponse: the database's own answer to
 * POST /v2/query. `responseJson` is base64 of its body.
 */
export type ExecuteDatabaseQueryResponse = {
  responseJson?: string | null;
  statusCode: number;
  contentType?: string | null;
};

// ---------------------------------------------------------------- Ontology: review decisions
// The WFE proto: identity is derived from the current load's review by the
// backend. The browser sends only a choice, never an entity's identity.

type PropValueArms = {
  null: null;
  // A string, date (YYYY, YYYY-MM or YYYY-MM-DD) or RFC 3339 datetime.
  text: string;
  // An int64 as its decimal digits, so one past 2^53 keeps every digit.
  integer: string;
  number: number;
  boolean: boolean;
};
/**
 * v1FoundryPropValue: a prop decision's value, exactly one arm. `null`
 * removes the property; any other must be of the JSON kind the property's
 * type takes (no coercion): text for string, date and datetime, integer
 * for integer, integer or number for float. A decision reads back a whole
 * number within int64 as integer, even a float property's.
 */
export type FoundryPropValue = {
  [K in keyof PropValueArms]: Pick<PropValueArms, K> & {
    [Other in Exclude<keyof PropValueArms, K>]?: never;
  };
}[keyof PropValueArms];
export type FoundryReviewChoice =
  | {
      resolve:
        | { entityKey: string; notInData?: never }
        | { notInData: true; entityKey?: never };
      prop?: never;
    }
  | { prop: FoundryPropValue; resolve?: never };
export type ReviewDecision =
  | {
      kind: "resolve";
      type: string;
      docId: string;
      normName: string;
      entityKey?: string;
    }
  | {
      kind: "prop";
      type: string;
      entityKey: string;
      prop: string;
      value: FoundryPropValue;
    };
export type FoundryReviewDecision = ReviewDecision & {
  id: string;
  decidedByUserId?: string;
  decidedAt?: string | null;
};
export type SettleReviewRequest = { decision: FoundryReviewChoice };
export type ReviewDecisionResponse = { decision: FoundryReviewDecision };
// Making a version current drops the decisions it doesn't take, but keeps a
// resolve of a type it keeps whose entityKey its key no longer reads: the
// next load reviews that name again, and settling it replaces the decision.
export type ListReviewDecisionsResponse = {
  decisions?: FoundryReviewDecision[];
  nextPageToken?: string;
};
export type DeleteReviewDecisionResponse = Record<string, never>;

// ---------------------------------------------------------------- People enrichment
// GET and PATCH /v1/workspaces/{workspaceId}/foundry/enrichment and POST
// .../ontologies/{ontologyId}/loads/{id}:stop-enrichment. An owner allows
// People enrichment for the workspace with a monthly credit cap; a load of a
// version whose mappings.toml has an [enrichments] item then looks the
// people of one type up on context.dev before its tables load, and applies
// the profiles found with them.

/**
 * v1FoundryEnrichmentAvailability: DISABLED while Helix has lookups off for
 * the stamp (loads still apply profiles found before), PRO_REQUIRED on Free,
 * UNSPECIFIED (or unset) while Helix can't read the workspace's plan
 * (turning it off still works).
 */
export type FoundryEnrichmentAvailability =
  | "FOUNDRY_ENRICHMENT_AVAILABILITY_UNSPECIFIED"
  | "FOUNDRY_ENRICHMENT_AVAILABILITY_DISABLED"
  | "FOUNDRY_ENRICHMENT_AVAILABILITY_PRO_REQUIRED"
  | "FOUNDRY_ENRICHMENT_AVAILABILITY_AVAILABLE";

/** v1FoundryEnrichment: the workspace's setting and this month's credits. */
export type FoundryEnrichment = {
  availability?: FoundryEnrichmentAvailability;
  // The owner's consent: without it loads look nobody up and apply no
  // profiles, and the ontology agent never proposes enrichment.
  enabled?: boolean;
  // int64 credits in the calendar month (UTC): the cap (0 until an owner
  // sets it), what was charged (unanswered calls included), and what a
  // running load holds back.
  monthlyCreditCap?: string;
  creditsUsed?: string;
  creditsReserved?: string;
  // When the month's charges start again from 0: the first of next month.
  capResetsAt?: string | null;
  // The Helix user ID of the owner who turned it on, and when; unset while
  // it is off.
  enabledBy?: string;
  enabledAt?: string | null;
  // int32: ontologies whose enriched values a clean load has still to
  // remove since the owner turned it off; Helix starts those loads.
  purgesPending?: number;
};

/** helixwfev1FoundryEnrichmentResponse: GET, and PATCH's answer. */
export type FoundryEnrichmentResponse = { enrichment?: FoundryEnrichment };

/**
 * v1WfeServiceUpdateFoundryEnrichmentBody: PATCH, owners only. Every change
 * is kept with the notice the owner was shown (`noticeVersion`, required, at
 * most 32 characters). Turning it on needs Helix's flag (FAILED_PRECONDITION)
 * and Foundry Pro (FAILED_PRECONDITION, reason FOUNDRY_PRO_REQUIRED); turning
 * it off is always allowed, cancels running lookups, deletes the stored
 * profiles and loads each ontology that applied some again without them.
 */
export type UpdateFoundryEnrichmentRequest = {
  enabled: boolean;
  // int64, at most 100,000.
  monthlyCreditCap: string;
  noticeVersion: string;
};

/**
 * v1FoundryLoadEnrichment.status: preparing while the load finds its people,
 * queued until the enrichment worker claims it (a lookup slot), running
 * while it looks people up, then how it ended. `off`: the workspace's consent
 * was off, so no profiles apply; `failed`: the profiles found before apply;
 * `cancelled`: consent was turned off or the ontology deleted meanwhile, and
 * nothing was published.
 */
export type FoundryLoadEnrichmentStatus =
  | "preparing"
  | "queued"
  | "running"
  | "done"
  | "partial"
  | "off"
  | "failed"
  | "cancelled";

/**
 * v1FoundryLoadEnrichment.stoppedBy, set on a `partial` step only: the credit
 * cap, context.dev (out of credits, or not answering), its time limit, no
 * free lookup slot, someone stopped it (the WFE doesn't say who), the
 * workspace left Pro, or Helix paused lookups. The rest are looked up on the
 * next load.
 */
export type FoundryEnrichmentStop =
  | "cap"
  | "provider_credits"
  | "provider"
  | "time"
  | "busy"
  | "stopped"
  | "pro"
  | "flag";

/**
 * v1FoundryLoadEnrichment: a load's People enrichment step. Every count is
 * int64: `people` = `skipped` + `carried` + `toLookUp`.
 */
export type FoundryLoadEnrichment = {
  status?: FoundryLoadEnrichmentStatus;
  stoppedBy?: FoundryEnrichmentStop;
  // Everyone of the enriched type the tables hold; those left out
  // (`skipped`: no full name, or no usable email or LinkedIn URL); and those
  // who lost a clue another person holds too.
  people?: string;
  noName?: string;
  noClue?: string;
  skipped?: string;
  shared?: string;
  // Reused from an earlier load, at no cost.
  carried?: string;
  // To be looked up this load, and how far it got: matches found, held back
  // by the checks, not found, and unanswered (charged, not resent). A step
  // that published after its load stopped waiting for it (past its deadline
  // and the grace) counts what it found then, though the load applied only
  // the profiles found before: `applied` and `unmatched` say what it applied.
  toLookUp?: string;
  lookedUp?: string;
  found?: string;
  held?: string;
  notFound?: string;
  uncertain?: string;
  // Credits charged, unanswered calls included.
  credits?: string;
  // The profiles the load applied that bound their person, and those that
  // bound no one (the person changed since the lookup); 0 until its tables
  // are in.
  applied?: string;
  unmatched?: string;
  publishedAt?: string | null;
};

/** WfeServiceStopFoundryLoadEnrichmentBody's answer: the load, stopping. */
export type StopLoadEnrichmentResponse = LoadResponse;

/**
 * The agent's measure of an enrichment item's clues over the tables it
 * drafted from (stored JSON, snake_case): people of the type, those with a
 * usable email or LinkedIn URL, each clue's count, those without a full name,
 * and values two people share or one person has two of.
 */
export type AgentEnrichmentClues = {
  people: number;
  usable: number;
  email: number;
  linkedin_url: number;
  no_name: number;
  shared: number;
  conflicting: number;
};

// ---------------------------------------------------------------- AI models
// GET and PUT /v1/workspaces/{workspaceId}/foundry/ai: where a workspace's
// model calls go (drafting ontologies, reading documents, answering
// questions, naming generations). Unset `active` is the deployment's
// default: Helix's Claude when hosted, the install's provider (or none)
// locally: the one its .env names (FOUNDRY_AI_PROVIDER), else the one it
// found on its machine (`detected`). An owner's save is tested before it is
// used (`pending`); the setting in force stays until the test passes.
// Hosted, bringing a key needs Foundry Pro or Max (FAILED_PRECONDITION,
// reason FOUNDRY_PRO_REQUIRED); a local install always may. Model work while
// the setting resolves to none (a local install without a default) is
// FAILED_PRECONDITION with reason FOUNDRY_AI_NOT_CONFIGURED: generating,
// asking and completing an upload.
// A local install's only (FAILED_PRECONDITION elsewhere):
// - POST .../foundry/ai/detect, body {}: looks again for the install's
//   default, answering as GET does; FAILED_PRECONDITION too where its .env
//   names its default.
// - GET .../foundry/ai/ollama-models?baseUrl=<url>: the models an Ollama
//   server has; INVALID_ARGUMENT for an address a save would refuse.

/**
 * v1FoundryAIKind: every kind is declared; a deployment offers those its
 * agent implements (FoundryAi.offered).
 */
export type FoundryAiKind =
  | "FOUNDRY_AI_KIND_UNSPECIFIED"
  | "FOUNDRY_AI_KIND_ANTHROPIC"
  | "FOUNDRY_AI_KIND_OPENAI"
  | "FOUNDRY_AI_KIND_AZURE_OPENAI"
  | "FOUNDRY_AI_KIND_BEDROCK"
  | "FOUNDRY_AI_KIND_OLLAMA"
  | "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE";
/** The kinds a provider can be: every FoundryAiKind but UNSPECIFIED. */
export type FoundryAiProviderKind = Exclude<
  FoundryAiKind,
  "FOUNDRY_AI_KIND_UNSPECIFIED"
>;

/** v1FoundryAICompatAPI: the API an OpenAI-compatible server speaks. */
export type FoundryAiCompatApi =
  | "FOUNDRY_AI_COMPAT_API_UNSPECIFIED"
  | "FOUNDRY_AI_COMPAT_API_CHAT_COMPLETIONS"
  | "FOUNDRY_AI_COMPAT_API_RESPONSES";

/**
 * v1FoundryAIProvider: one message per kind, holding that kind's endpoint
 * settings and never a key. Exactly one is set. int64 fields are strings.
 */
export type FoundryAiProvider = {
  anthropic?: Record<string, never>;
  openai?: Record<string, never>;
  // https://<resource>.openai.azure.com, normalized to .../openai/v1.
  azureOpenai?: { endpoint?: string };
  // An AWS region, like us-east-1.
  bedrock?: { region?: string; fips?: boolean };
  // The context window models load with; "0" or unset is 65536.
  ollama?: { baseUrl?: string; contextTokens?: string };
  // baseUrl includes its /v1. maxCallsInFlight is a uint32, so a JSON number
  // (the swagger's int64 format is how it writes every uint32); 0 or unset
  // is 4.
  openaiCompatible?: {
    baseUrl?: string;
    api?: FoundryAiCompatApi;
    maxCallsInFlight?: number;
  };
};

/**
 * v1FoundryAIRates: US dollars per million tokens, as decimal strings with
 * at most three decimals ("0.075"); cached input empty when not priced apart.
 */
export type FoundryAiRates = {
  inputUsdPerMtok?: string;
  outputUsdPerMtok?: string;
  cachedInputUsdPerMtok?: string;
};

/**
 * v1FoundryAIModel's price, one of: Helix's price table for the model
 * (FAILED_PRECONDITION, reason FOUNDRY_AI_PRICE_REQUIRED, when it has none),
 * the table's row for another model of the kind (an Azure deployment's
 * underlying model, a Bedrock inference profile's), the owner's own rates,
 * or no cost counted (Ollama and OpenAI-compatible servers only).
 */
export type FoundryAiPrice =
  | { table: Record<string, never> }
  | { tableAs: string }
  | { rates: FoundryAiRates }
  | { free: Record<string, never> };

/** v1FoundryAIModel as the WFE answers it: a price left out is the table. */
export type FoundryAiModel = {
  name?: string;
  table?: Record<string, never>;
  tableAs?: string;
  rates?: FoundryAiRates;
  free?: Record<string, never>;
};

/**
 * v1FoundryAIModels: a model per task: drafting ontologies, answering
 * questions, reading documents and checking the facts read.
 */
export type FoundryAiModels = {
  draft?: FoundryAiModel;
  ask?: FoundryAiModel;
  extract?: FoundryAiModel;
  verify?: FoundryAiModel;
};
/** The tasks a workspace's models are chosen for. */
export type FoundryAiRole = keyof FoundryAiModels;

/** v1FoundryAIProbeState: where a saved provider is in its test. */
export type FoundryAiProbeState =
  | "FOUNDRY_AI_PROBE_STATE_UNSPECIFIED"
  | "FOUNDRY_AI_PROBE_STATE_QUEUED"
  | "FOUNDRY_AI_PROBE_STATE_RUNNING"
  | "FOUNDRY_AI_PROBE_STATE_PASSED"
  | "FOUNDRY_AI_PROBE_STATE_FAILED"
  // Replaced by a newer save, or by going back to the default, first.
  | "FOUNDRY_AI_PROBE_STATE_SUPERSEDED";

/** v1FoundryAIKey: whether a kind's calls carry an API key (Ollama's take none). */
export type FoundryAiKey =
  | "FOUNDRY_AI_KEY_UNSPECIFIED"
  | "FOUNDRY_AI_KEY_REQUIRED"
  | "FOUNDRY_AI_KEY_OPTIONAL"
  | "FOUNDRY_AI_KEY_NONE";

/** v1FoundryAIConfig: a provider an owner saved. */
export type FoundryAiConfig = {
  id: string;
  kind?: FoundryAiKind;
  provider?: FoundryAiProvider;
  models?: FoundryAiModels;
  // Which key it was saved with, e.g. sk-ant…4f2a; empty for none.
  keyHint?: string;
  // Whether the key is still stored: only the setting's and a pending one's are.
  hasKey?: boolean;
  probeState?: FoundryAiProbeState;
  // Why a failed test failed: an agent ModelFailure (AgentModelFailure) and
  // its check's ProbeDetail (AgentProbeDetail).
  probeFailure?: string;
  probeDetail?: string;
  createdAt?: string;
  createdByName?: string;
  // The provider as failure copy names it: "Claude", or a custom server's host.
  label?: string;
};

/** v1FoundryAIDefault: the deployment's provider. */
export type FoundryAiDefault = {
  kind?: FoundryAiKind;
  models?: FoundryAiModels;
  label?: string;
};

/**
 * v1FoundryAIOffer: a kind an owner may choose, with the models a save of it
 * gets where it names none (unset for a kind whose owner names every model).
 */
export type FoundryAiOffer = {
  kind?: FoundryAiKind;
  suggested?: FoundryAiModels;
  key?: FoundryAiKey;
};

/**
 * v1FoundryAIDetected: how a local install that names no default found one
 * on its machine, looking for an ANTHROPIC_API_KEY, then an OPENAI_API_KEY,
 * where it was started; an Ollama it reaches (the bundled one, then this
 * computer's) with a model that calls tools; LM Studio on this computer
 * serving a model.
 */
export type FoundryAiDetected = {
  // For the owner: what is used and why (with a `default`), or what was
  // looked for and how to set a provider up (without). Never a key.
  summary?: string;
  detectedAt?: string;
};

/** v1FoundryAI: a workspace's AI setting. */
export type FoundryAi = {
  // The provider in force; unset is the default.
  active?: FoundryAiConfig;
  // The deployment's provider; unset is none, which only a local install has.
  default?: FoundryAiDefault;
  // A save waiting for or in its test.
  pending?: FoundryAiConfig;
  // The newest save, when its test failed after the setting last changed.
  lastFailed?: FoundryAiConfig;
  offered?: FoundryAiOffer[];
  // Endpoints may be http and private addresses: a local install.
  privateHostsAllowed?: boolean;
  // Hosted, on a plan without own keys: saving one is refused
  // (FOUNDRY_PRO_REQUIRED) until the workspace is on Foundry Pro or Max.
  // Going back to the default never is.
  proRequired?: boolean;
  // A local install that names no default: how it found `default`, or that
  // it found none. Unset hosted, and where the install's .env names one.
  detected?: FoundryAiDetected;
};

/** v1FoundryAIResponse: GET, PUT's and detect's answer. */
export type FoundryAiResponse = { ai?: FoundryAi };

/** v1FoundryOllamaModel: a model an Ollama server has pulled. */
export type FoundryOllamaModel = {
  name?: string;
  // Its size on disk, int64 as a string.
  sizeBytes?: string;
  // Like "4.0B"; empty when the server doesn't say.
  parameterSize?: string;
  // For the owner: why Foundry can't run on it, as a sentence; empty when
  // it can, or when the server can't say.
  unusable?: string;
};

/**
 * v1FoundryOllamaServer: what an Ollama server has, its models usable ones
 * first, the most capable first, then the rest by name.
 */
export type FoundryOllamaServer = {
  // Empty when it didn't answer.
  version?: string;
  // For the owner: why its models can't be listed, or can't be told apart
  // (a server too old to say which call tools), and how to fix it, as a
  // sentence; empty when they can.
  problem?: string;
  models?: FoundryOllamaModel[];
};

/** v1ListFoundryOllamaModelsResponse */
export type ListFoundryOllamaModelsResponse = { server?: FoundryOllamaServer };

/**
 * v1WfeServiceUpdateFoundryAIBody: PUT, owners only. Back to the default at
 * once, or a provider saved and tested before it is used. Unset models get
 * the kind's suggestions; a blank key keeps the setting's where it still
 * belongs to the endpoint. A model Helix can't price needs rates
 * (FOUNDRY_AI_PRICE_REQUIRED); saves are capped per hour (RESOURCE_EXHAUSTED).
 */
export type UpdateFoundryAiBody =
  | { useDefault: Record<string, never> }
  | {
      provider: FoundryAiProvider;
      models?: { [R in FoundryAiRole]?: { name: string } & FoundryAiPrice };
      apiKey?: string;
    };

/** ErrorInfo reasons of the AI routes and the model work they gate. */
export const FOUNDRY_AI_NOT_CONFIGURED = "FOUNDRY_AI_NOT_CONFIGURED";
export const FOUNDRY_AI_PRICE_REQUIRED = "FOUNDRY_AI_PRICE_REQUIRED";

// The engine's words for model failures (contract/ontology_agent
// probe_result.json), which a failed test's probeFailure and probeDetail
// carry, and the provider kinds its Ask events name.

/** ModelFailure: why a model call failed for good. */
export type AgentModelFailure =
  | "refused"
  | "unavailable"
  | "auth"
  | "permission"
  | "not_found"
  | "rate_limited"
  | "quota"
  | "context_too_long"
  | "unsupported"
  | "truncated"
  | "malformed"
  | "timeout"
  | "model_changed"
  | "private_host";

/** ProbeDetail: what a failed check saw. */
export type AgentProbeDetail =
  | "tools_rejected"
  | "tool_call_missing"
  | "schema_rejected"
  | "json_invalid"
  | "text_missing"
  | "context_too_small"
  | "unpriced"
  | "none";

/** ProviderKind: which API a model is reached through. */
export type AgentProviderKind =
  | "anthropic"
  | "openai"
  | "azure_openai"
  | "bedrock"
  | "ollama"
  | "openai_compatible";
