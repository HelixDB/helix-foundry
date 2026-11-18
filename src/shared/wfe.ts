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
  // Required for AUTH_PROVIDER_SSO, which routes by its domain.
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
/**
 * v1ExchangeAuthCodeResponse. Tokens, or no tokens and one of: the code
 * WorkOS emailed to `email` is needed (emailVerificationRequired: VerifyEmail
 * under the same sessionId), or the email's company requires its single
 * sign-on (ssoRequired: a new login with AUTH_PROVIDER_SSO and `email`).
 */
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

/** v1VerifyEmailRequest: POST /v1/auth/verify-email, under the exchange's sessionId. */
export type VerifyEmailRequest = { sessionId: string; code: string };
/**
 * v1VerifyEmailResponse: tokens, or ssoRequired as an exchange answers it.
 * A wrong code is INVALID_ARGUMENT and keeps the session for another try;
 * an expired or unknown one is UNAUTHENTICATED.
 */
export type VerifyEmailResponse = {
  accessToken?: string;
  refreshToken?: string;
  expiresAt?: string;
  email?: string;
  workspaceId?: string;
  invitationOnboarding?: boolean;
  ssoRequired?: boolean;
};

/** v1SignInWithPasswordRequest: POST /v1/auth/password:sign-in */
export type SignInWithPasswordRequest = {
  email: string;
  password: string;
  application: AuthApplication;
};
/**
 * v1SignUpWithPasswordRequest: POST /v1/auth/password:sign-up. WorkOS makes
 * the user, then it signs in as SignInWithPassword does. An email with an
 * account is ALREADY_EXISTS, a password WorkOS refuses INVALID_ARGUMENT with
 * its reasons.
 */
export type SignUpWithPasswordRequest = {
  email: string;
  password: string;
  firstName?: string;
  lastName?: string;
  application: AuthApplication;
};
/**
 * v1PasswordAuthResponse, both password calls' answer: tokens, or none and
 * one of an exchange's two detours. emailVerificationRequired comes with the
 * `sessionId` VerifyEmail takes the emailed code under.
 */
export type PasswordAuthResponse = {
  accessToken?: string;
  refreshToken?: string;
  expiresAt?: string;
  email?: string;
  emailVerificationRequired?: boolean;
  sessionId?: string;
  workspaceId?: string;
  invitationOnboarding?: boolean;
  ssoRequired?: boolean;
};
/**
 * v1RequestPasswordResetRequest: POST /v1/auth/password:forgot. WorkOS emails
 * a link to the application's reset page (Foundry's /reset-password?token=),
 * and answers the same for an address without an account.
 */
export type RequestPasswordResetRequest = {
  email: string;
  application: AuthApplication;
};
/** v1ResetPasswordRequest: POST /v1/auth/password:reset, the emailed link's token. */
export type ResetPasswordRequest = { token: string; newPassword: string };

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

/**
 * v1ResourceScope: which projects a scope reaches. `mode` is "all", "none"
 * or "selected" (then `projectIds` names them); unset reads as none.
 */
export type ResourceScope = { mode?: string; projectIds?: string[] };

/**
 * v1ProjectAccess: a member's effective project policy. `write`, `queryRead`
 * or `queryWrite` reaching any project lets them read databases outside
 * Foundry, so they can't be restricted to source grants.
 */
export type ProjectAccess = {
  version?: string;
  read?: ResourceScope;
  write?: ResourceScope;
  queryRead?: ResourceScope;
  queryWrite?: ResourceScope;
};

/** v1WorkspaceMember */
export type WorkspaceMember = {
  id?: string;
  workspaceId: string;
  user?: User;
  role?: WorkspaceRole;
  timestamps?: Timestamps;
  projectAccess?: ProjectAccess;
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

// ---------------------------------------------------------------- Directory sync

/**
 * v1WorkspaceDirectory: GET /v1/workspaces/{workspaceId}/directory, the
 * workspace's directory (SCIM through WorkOS) as its last sync saw it. Needs
 * workspace.members.manage (owners and admins). A workspace without the SSO
 * entitlement answers `{workspaceId}` alone (`entitled` false); `directory`
 * is unset until an IT admin connects one in the Admin Portal. Roles from
 * groups stay in the Helix dashboard: Foundry reads `groups` only to count
 * them.
 */
export type WorkspaceDirectory = {
  workspaceId?: string;
  entitled?: boolean;
  directory?: DirectoryConnection;
  rolesFromDirectory?: boolean;
  groups?: DirectoryGroup[];
  issues?: DirectoryIssue[];
  syncState?: DirectorySyncState;
  syncError?: string;
  lastSyncedAt?: string;
  userCount?: number;
  managedMemberCount?: number;
};

/** v1DirectoryConnection: `provider` is the name people know ("Okta"), or empty. */
export type DirectoryConnection = {
  id?: string;
  type?: string;
  provider?: string;
  name?: string;
  state?: DirectoryState;
};

export type DirectoryState =
  | "DIRECTORY_STATE_UNSPECIFIED"
  | "DIRECTORY_STATE_LINKED"
  | "DIRECTORY_STATE_VALIDATING"
  | "DIRECTORY_STATE_INVALID_CREDENTIALS"
  | "DIRECTORY_STATE_UNLINKED"
  | "DIRECTORY_STATE_DELETING";

export type DirectorySyncState =
  | "DIRECTORY_SYNC_STATE_UNSPECIFIED"
  | "DIRECTORY_SYNC_STATE_IDLE"
  | "DIRECTORY_SYNC_STATE_PENDING"
  | "DIRECTORY_SYNC_STATE_FAILED";

/** v1DirectoryGroup: `foundryTeamId` is the team it syncs to. */
export type DirectoryGroup = {
  id?: string;
  name?: string;
  memberCount?: number;
  role?: WorkspaceRole;
  foundryTeamId?: string;
};

/** v1DirectoryIssue: about a person (email, userId, displayName) or a group. */
export type DirectoryIssue = {
  kind?: DirectoryIssueKind;
  email?: string;
  userId?: string;
  displayName?: string;
  groupId?: string;
  groupName?: string;
};

export type DirectoryIssueKind =
  | "DIRECTORY_ISSUE_KIND_UNSPECIFIED"
  | "DIRECTORY_ISSUE_KIND_NO_EMAIL"
  | "DIRECTORY_ISSUE_KIND_NOT_PROVISIONED"
  | "DIRECTORY_ISSUE_KIND_PENDING_INVITATION"
  | "DIRECTORY_ISSUE_KIND_OWNER_KEPT"
  | "DIRECTORY_ISSUE_KIND_TOO_MANY_GROUPS"
  | "DIRECTORY_ISSUE_KIND_GROUP_TOO_LARGE"
  | "DIRECTORY_ISSUE_KIND_LEFT_OUT_RAW_ACCESS";

/**
 * WfeServiceCreateDirectoryPortalLinkBody: POST
 * /v1/workspaces/{workspaceId}/directory/portal-link. Needs
 * workspace.sso.manage (owners) and the entitlement (else
 * FAILED_PRECONDITION). `returnUrl`, an http(s) URL, is where the portal's
 * back link goes.
 */
export type CreateDirectoryPortalLinkRequest = { returnUrl?: string };

/** v1CreateDirectoryPortalLinkResponse: a single-use link, valid for minutes. */
export type CreateDirectoryPortalLinkResponse = { link?: string };

/**
 * POST /v1/workspaces/{workspaceId}/directory:sync with `{}`: asks for a sync
 * now (owners), answered with the WorkspaceDirectory. FAILED_PRECONDITION
 * when no directory is connected.
 */
export type SyncWorkspaceDirectoryRequest = Record<string, never>;

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
  | "FOUNDRY_USAGE_TASK_QUESTIONS"
  | "FOUNDRY_USAGE_TASK_WORKFLOW_ASSESSMENTS";

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

// ---------------------------------------------------------------- Feedback

/**
 * v1FeedbackCategory: what feedback is about. The site sends bug, missing
 * capability (an idea), confusing result and other from its Feedback
 * button, and connector request from Request a connector.
 */
export type FeedbackCategory =
  | "FEEDBACK_CATEGORY_BUG"
  | "FEEDBACK_CATEGORY_MISSING_CAPABILITY"
  | "FEEDBACK_CATEGORY_CONFUSING_RESULT"
  | "FEEDBACK_CATEGORY_OTHER"
  | "FEEDBACK_CATEGORY_CONNECTOR_REQUEST";

/**
 * v1SendFeedbackRequest (POST /v1/feedback): a person's feedback, which the
 * WFE stores and tells Helix's team of. `message` is 1 to 4096 bytes of
 * UTF-8; `workspaceId`, when set, is one the caller belongs to; `page` is
 * the path they were on (at most 255 bytes, starting "/").
 */
export type SendFeedbackRequest = {
  category: FeedbackCategory;
  message: string;
  workspaceId?: string;
  page?: string;
};

/** v1SendFeedbackResponse */
export type SendFeedbackResponse = { id?: string };

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
  | "slack"
  | "notion"
  | "google"
  | "databuddy"
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
/**
 * v1GitHubConnectorSetup: Helix Foundry's GitHub App, which a signed-in
 * GitHub connector reads private repositories through, where it is
 * installed.
 */
export type GitHubConnectorSetup = { installUrl?: string };
/** v1ConnectorKind: GET .../foundry/connector-kinds, in display order. */
export type ConnectorKind = {
  kind: string;
  displayName?: string;
  requiresPro?: boolean;
  // False when the workspace's plan does not include the kind.
  available?: boolean;
  // s3 only, and only where role access is configured (proto oneof setup).
  s3?: S3ConnectorSetup | null;
  // github only, where it connects by signing in (the same oneof).
  github?: GitHubConnectorSetup | null;
  // True when the caller can connect the kind by signing in at its
  // provider (StartConnectorSignIn) instead of entering a key: hosted, the
  // kinds Pipes signs in to that the stamp lists (GitHub, Linear, Slack,
  // Notion, PostHog, Cal.com, Stripe, Google Workspace), for a member of the
  // workspace's WorkOS organization.
  signIn?: boolean;
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
/** v1SlackConnectorConfig: the bot token (xoxb-…) of an app in a Slack workspace. */
export type SlackConnectorConfig = {
  // Write-only.
  botToken?: string;
  // Days of channel history to keep, 1 to 365; 0 means 90.
  historyDays?: number;
};
/** v1NotionConnectorConfig: an internal integration's secret (ntn_… or secret_…). */
export type NotionConnectorConfig = {
  // Write-only.
  token?: string;
};
/**
 * v1GoogleWorkspaceConnectorConfig: Drive, Calendar and the Directory, read
 * as one Workspace user through a service account with domain-wide
 * delegation.
 */
export type GoogleWorkspaceConnectorConfig = {
  // The service account's JSON key; write-only.
  serviceAccountKey?: string;
  // The Workspace user it acts as; an admin, to read the Directory's users.
  subject?: string;
  // Days of past calendar events to keep, 1 to 365; 0 means 90.
  eventDays?: number;
};
/** v1DatabuddyConnectorConfig: a Databuddy API key (dbdy_…) with the read:data scope. */
export type DatabuddyConnectorConfig = {
  // Write-only.
  apiKey?: string;
  // Website ids; empty reads every website the key reads.
  websiteIds?: string[];
  // Days of history to keep, 1 to 365; 0 means 90.
  historyDays?: number;
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
  | { slack: SlackConnectorConfig }
  | { notion: NotionConnectorConfig }
  | { google: GoogleWorkspaceConnectorConfig }
  | { databuddy: DatabuddyConnectorConfig }
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

/** v1ConnectorAccountState */
export type ConnectorAccountState =
  | "CONNECTOR_ACCOUNT_STATE_UNSPECIFIED"
  | "CONNECTOR_ACCOUNT_STATE_CONNECTED"
  // It doesn't sync until an owner or admin signs it in again; its tables
  // keep what the last sync read.
  | "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN";
/** v1ConnectorAccountBroken: why an account needs signing in again. */
export type ConnectorAccountBroken =
  | "CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED"
  // The member it reads as left the workspace.
  | "CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT"
  // The provider rejected its access, or it was revoked there.
  | "CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED"
  // Its access lapsed and couldn't be renewed.
  | "CONNECTOR_ACCOUNT_BROKEN_EXPIRED"
  // The broker (hosted: WorkOS Pipes) no longer has the account.
  | "CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING";
/**
 * v1ConnectorAccount: the provider account a connector connected by
 * signing in reads with, in place of a stored key. The credential stays
 * with the broker; the connector asks it for a token each time it reads.
 */
export type ConnectorAccount = {
  // Who the provider says signed in: a GitHub login; a Linear user and
  // workspace.
  label?: string;
  state?: ConnectorAccountState;
  // Unspecified while connected.
  broken?: ConnectorAccountBroken;
  brokenAt?: string | null;
  // The member whose access it reads with, and their name: "a former
  // member" once they have left the workspace.
  userId?: string;
  connectedBy?: string;
  connectedAt?: string | null;
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
  // Set when it was connected by signing in at its provider; its
  // `secretHint` is then empty.
  account?: ConnectorAccount | null;
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

/**
 * helixwfev1ListConnectorsResponse: GET .../foundry/connectors. With
 * `?needsSignIn=true` (ListConnectorsQuery), only the connectors whose
 * account needs signing in again, without their tables or syncs: the cheap
 * read the shell's notice of them makes.
 */
export type ListConnectorsResponse = { connectors?: Connector[] };
/** ListConnectors' query parameters. */
export type ListConnectorsQuery = { needsSignIn?: boolean };
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

// Connecting a source by signing in at its provider (hosted: through
// WorkOS Pipes) instead of entering a key: routes under
// /v1/workspaces/{workspaceId}/foundry/sign-ins. A user, or their agent,
// starts a sign-in with the connector's settings; that user authorizes it in
// their browser, which the provider sends back to the site's
// /connect/callback, which finishes it: the connector is made (or signed in
// again) and its first sync scheduled. Only its starter may authorize or
// finish it. No provider text is returned: failures are codes.

/** v1ConnectorSignInStatus */
export type ConnectorSignInStatus =
  | "CONNECTOR_SIGN_IN_STATUS_UNSPECIFIED"
  // Started; its link not yet followed.
  | "CONNECTOR_SIGN_IN_STATUS_PENDING"
  // Sent to the provider to authorize.
  | "CONNECTOR_SIGN_IN_STATUS_AUTHORIZING"
  // Its callback arrived and is being finished.
  | "CONNECTOR_SIGN_IN_STATUS_FINISHING"
  | "CONNECTOR_SIGN_IN_STATUS_CONNECTED"
  // The user declined at the provider. Authorizing again retries it.
  | "CONNECTOR_SIGN_IN_STATUS_DECLINED"
  // See `failure`. Authorizing again retries it.
  | "CONNECTOR_SIGN_IN_STATUS_FAILED"
  // 15 minutes passed since it started or was last authorized.
  | "CONNECTOR_SIGN_IN_STATUS_EXPIRED";
/** v1ConnectorSignInFailure: why a sign-in failed, for the site to word. */
export type ConnectorSignInFailure =
  | "CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED"
  // The provider or the broker answered with an error.
  | "CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR"
  // The callback's code had lapsed or was already used.
  | "CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED"
  // The account isn't this workspace's, or wasn't made by its starter.
  | "CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE"
  // The provider refused the token it issued.
  | "CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED"
  // The connector's settings were refused.
  | "CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID"
  // The broker couldn't be reached.
  | "CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE";
/** v1ConnectorSignIn */
export type ConnectorSignIn = {
  id: string;
  kind?: string;
  name?: string;
  status?: ConnectorSignInStatus;
  // Set with CONNECTOR_SIGN_IN_STATUS_FAILED.
  failure?: ConnectorSignInFailure;
  // The connector it made, or signs in again.
  connectorId?: string;
  // Set when the connector was made but can't read all it names, in
  // Foundry's words: a repository the GitHub App can't see, say.
  discoveryProblem?: string;
  // Signing in again changed the provider account the connector reads with.
  identityChanged?: boolean;
  // The caller started it: only they can authorize and finish it.
  mine?: boolean;
  createdAt?: string | null;
  expiresAt?: string | null;
  // The connector's settings, chosen before signing in; never a credential.
  config?: ConnectorConfig | null;
};
/**
 * FoundryServiceStartConnectorSignInBody: POST .../foundry/sign-ins (owners
 * and admins, for a kind with `signIn`). `config` holds the settings with
 * every credential field empty; `connectorId` signs that connector in
 * again instead of making one. The same `clientToken` returns the sign-in
 * the first call started.
 */
export type StartConnectorSignInRequest = {
  kind: string;
  name?: string;
  config?: ConnectorConfig;
  connectorId?: string;
  clientToken?: string;
};
/** v1ConnectorSignInResponse: start, get and finish. */
export type ConnectorSignInResponse = { signIn: ConnectorSignIn };
/**
 * FoundryServiceAuthorizeConnectorSignInBody: POST .../sign-ins/{id}:authorize,
 * by its starter, until it connects: again after a decline or a failure.
 * `returnTo` is the site's callback, which the WFE must list.
 */
export type AuthorizeConnectorSignInRequest = { returnTo: string };
/** v1AuthorizeConnectorSignInResponse: the provider's page to send the browser to. */
export type AuthorizeConnectorSignInResponse = { url: string };
/**
 * FoundryServiceFinishConnectorSignInBody: POST .../sign-ins/{id}:finish,
 * from the callback: its `connectCode`, or its `error` when it came back
 * without one (access_denied: declined). A repeat returns the sign-in as it
 * stands.
 */
export type FinishConnectorSignInRequest =
  | { connectCode: string }
  | { error: string };

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
  | "ONTOLOGY_GENERATION_STATUS_FAILED"
  // The agent asked questions only a person can answer: the generation
  // waits, still the ontology's one live generation, until they are
  // answered (:answer), it is cancelled (:cancel), or answersDueAt passes
  // and Helix answers each with the agent's default.
  | "ONTOLOGY_GENERATION_STATUS_NEEDS_INPUT"
  // Cancelled (:cancel) before it stored a draft.
  | "ONTOLOGY_GENERATION_STATUS_CANCELLED";

/** v1AgentQuestionOption: one answer a question offers. */
export type AgentQuestionOption = {
  label: string;
  // One line on what choosing it means; may be empty.
  description?: string;
};

/**
 * v1AgentQuestion: a question an agent asks the person: the ontology agent
 * of a generation that needs input, or the Assistant (FoundryAgentAsked).
 * With no options it takes an answer in words; with options (2 to 20,
 * labels unique) the person picks one, or one or more when `multiple`, and
 * may always answer in their own words instead (one) or as well (multiple).
 * Its header, text, options and recommended answer are the agent's words
 * and may quote the data: plain text only, never markup.
 */
export type AgentQuestion = {
  id: string;
  // A few words naming it, for a progress strip; may be empty.
  header?: string;
  text?: string;
  // Empty for an answer in words.
  options?: AgentQuestionOption[];
  // The person may pick several options; false without options.
  multiple?: boolean;
  // The answer the agent recommends, which a question left to it takes: an
  // option's label (shown first, marked), or, without options, its words;
  // empty for none.
  recommended?: string;
  // A blocking question needs an answer; another may be left to the agent.
  blocking?: boolean;
};

/**
 * v1AgentQuestionAnswer: the person's answer to an AgentQuestion: the
 * labels of the options chosen and their own words, at least one of the
 * two. To a question without options, text alone; to one of one option,
 * one option or text, not both; to one of several, one or more options
 * with text or without, or text alone. Text is 1 to 2000 characters once
 * trimmed. Anything else is INVALID_ARGUMENT.
 */
export type AgentQuestionAnswer = {
  questionId: string;
  options?: string[];
  text?: string;
};

/**
 * v1OntologyGenerationQuestion: a question the ontology agent asked about
 * its draft, the draft items it is about, and its answer once given, or
 * that it was left to the agent.
 */
export type OntologyGenerationQuestion = AgentQuestion & {
  // The draft items it is about ("type.Customer"); may be empty.
  items?: string[];
  // Unset while it waits, or when it was left to the agent.
  answer?: AgentQuestionAnswer | null;
  // Left to the agent (by the person, or the automatic answers once due),
  // which took its recommended answer, or decided, as its own choice.
  leftToAgent?: boolean;
};

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
  // The sample it drafts from in place of the AI ("acme"); "" for an AI
  // draft.
  sample?: string;
  // Asked for with GenerateOntologyRequest's loadWhenReady: its draft is
  // made current and loaded once stored, if no version is current by then.
  loadWhenReady?: boolean;
  // The questions the agent's last turn asked, in its order: those a
  // needs_input generation waits on, then, once answered, with their
  // answers. Empty when it asked nothing. A draft's own non-blocking
  // questions are in its version's definition.
  questions?: OntologyGenerationQuestion[];
  // Which round of questions they are, from 1: an answer names it, since
  // the agent may ask again (reusing ids) once answered.
  questionRound?: number;
  // When they were asked; unset without questions.
  askedAt?: string | null;
  // While needs_input: when Helix answers each with the agent's default
  // and carries on (foundry.ontology.answer_days after askedAt, 7 days by
  // default).
  answersDueAt?: string | null;
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
 * are left out), and its Markdown, MDX, text, WebVTT and PDFs whose text is
 * read may be routed. It needs one source of either kind: a synced table, a
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
  // Foundry's sample company ("acme", the only one) to draft from in
  // place of the AI: the generation stores its ready-made draft, mapped to
  // this workspace's connection to the sample's database, with no model
  // call. It needs exactly that connector in connectorIds and the set of
  // the sample's documents in documentSetIds, with no eventSources
  // (INVALID_ARGUMENT otherwise), and each of the sample's tables synced
  // (FAILED_PRECONDITION until then).
  sample?: string;
  // Once the draft is stored, while the ontology still has no current
  // version, make it current and load it, as set-current then create-load
  // would, and email the requester when that load ends. The new-ontology
  // wizard's first ontology sets it. Not with `sample`.
  loadWhenReady?: boolean;
};

/**
 * FoundryServiceAnswerOntologyGenerationBody: POST
 * .../generations/{id}:answer answers the round of questions a needs_input
 * generation waits on and queues it to carry on. Each answer names a
 * question asked, at most once, as AgentQuestionAnswer says. With
 * useDefaults, every question left out is left to the agent (leftToAgent),
 * which takes its recommended answer, or decides, as its own choice;
 * without, every blocking question must be answered, and one that isn't may
 * be left open (INVALID_ARGUMENT otherwise, and for a round below 1). A
 * generation not waiting, or waiting on another round than `round`, is
 * FAILED_PRECONDITION. It needs what :generate needs (managing
 * the ontology, an AI provider). POST .../generations/{id}:cancel (body
 * `{}`) ends a queued, running or waiting generation as cancelled; one that
 * ended is FAILED_PRECONDITION. Both answer OntologyGenerationResponse.
 */
export type AnswerOntologyGenerationRequest = {
  answers: AgentQuestionAnswer[];
  useDefaults?: boolean;
  round: number;
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

/**
 * A question the agent asked about its draft (turn_result's Question): no
 * options for an answer in words, else 2 or more, one or (`multiple`)
 * several chosen; `default` is the answer it recommends, an option's label
 * or, without options, its words.
 */
export type AgentDraftQuestion = {
  id: string;
  // Draft items it is about: "type.Order", "mapping.orders"...
  items: string[];
  text: string;
  header?: string | null;
  options?: { label: string; description?: string | null }[];
  multiple?: boolean;
  default?: string | null;
  blocking: boolean;
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
    // `because` (contract 4.19.0): "not_assessed", {user_asked: {said}} or
    // {documents_state: {route, item}}; readAgentBundle reads it.
    left_out?: { what: string; reason: string; because?: unknown }[];
    questions?: AgentDraftQuestion[];
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
  questions?: AgentDraftQuestion[];
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
  // A caller who reads through source grants holds only some of what its
  // current load holds, and reads that part.
  partialAccess?: boolean;
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
// the workspace (Markdown, MDX, text, JSON, CSV, TSV, JSONL, Parquet, PDF
// and WebVTT), each in one of its document sets (folders of the Data tree). Text
// holds 1 byte to 1 MiB (foundry.documents.max_bytes), a table file or a PDF
// 3 MiB (max_table_bytes, max_pdf_bytes). The bytes go straight to storage
// through a presigned PUT, never through /papi. Statuses are lowercase
// names. How ontologies read a document is its `read.kind`:
// - text (Markdown, MDX, text, WebVTT): a load reads the documents of the
//   sets its version's scope names (OntologyVersion.documentSetIds), for
//   what its routes say they fill; MDX as Markdown, its code blanked to
//   spaces of the same length, never compiled or run; a WebVTT transcript as
//   its turns, a line each with its start, speaker and words
//   (src/shared/transcript.ts).
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
 * .ndjson, .parquet, .pdf, .vtt. A rename may change the extension; the type stays.
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
  | "application/pdf"
  | "text/vtt";

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
  // How the model failed it, when code is model: quota, rate_limited,
  // unavailable, timeout, auth, ... (AgentModelFailure). Unset for every
  // other code, and for a failure recorded before Foundry kept the class.
  modelFailure?: string;
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
  // Values written: property values it stated that were written as evidence.
  attributes?: number;
  reviews?: number;
  // int64: what reading it cost, in millionths of a dollar.
  usdMicros?: string;
  finishedAt?: string | null;
  // Names linked by a person's choice for them in another document
  // (contract 4.22.0: an alias carried from a settled resolve).
  resolvedCarried?: number;
  // What the load made of each name decision it met here: the document's
  // own resolves, and the choices carried from other documents.
  names?: DocumentNameOutcome[];
};

/**
 * v1FoundryDocumentNameOutcome (contract 4.22.0): a name decision the load
 * met in a document. `decision` "resolve" is the document's own choice,
 * "alias" one carried from another document; `outcome` "written",
 * "not_asked" (nothing in the load waited on it), "not_named" (the document
 * doesn't name it) or "refused", with `reason` saying why. Each is an open
 * set: a newer engine may send others.
 */
export type DocumentNameOutcome = {
  decision?: string;
  type?: string;
  normName?: string;
  decisionId?: string;
  outcome?: string;
  reason?: string;
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
 * an uploaded text document's text (Markdown, MDX, text, JSON, CSV, TSV,
 * JSONL or WebVTT, as uploaded, at most its type's cap; bytes that are not
 * UTF-8 read as U+FFFD),
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
// A resend applies a person's decisions to its load: approved claims'
// documents, sent again from their saved extractions, and its chosen values.
export type JobKind = "load" | "extract" | "delete" | "resend";

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
  // What it made of its documents, by document set, in set id order. Only
  // GetFoundryLoad fills it (not the list, nor the answer to a change of a
  // load); unset for a load that holds no documents, for one made before
  // Foundry recorded a load's documents, and for a grantee. A few seconds
  // old at most while a job of the load runs.
  documentSets?: FoundryLoadDocumentSet[];
  // Its documents the model failed to read; unset when it failed none.
  unread?: FoundryLoadUnread | null;
};

/**
 * v1FoundryLoadUnread: a load's documents the model failed. None of their
 * names or facts are in the load, so it is never complete with them: a
 * ready or partial load reads "ready, N documents not read (reason)".
 */
export type FoundryLoadUnread = {
  // How many: those the model failed, and once no job writes the load,
  // those no job reached.
  documents?: number;
  // The model failure class most of them failed with; unset when unknown.
  modelFailure?: string;
  // A few words for why: "provider quota", "provider busy", ...
  reason?: string;
  // When Helix reads them again on its own; unset when the next job into
  // the load or a new load reads them.
  rereadAt?: string | null;
};

/**
 * v1FoundryLoadDocumentSet: what a load made of one document set's
 * documents (its uploaded text documents and PDFs that joined the load).
 * `documents` is read + duplicate + failed + notRead + waiting.
 */
export type FoundryLoadDocumentSet = {
  setId?: string;
  // An extraction route of the load's version reads the set: what the
  // version routes, not what came of it (`read` says that). Unset (false):
  // the load stores its documents, which stay searchable, and no model
  // reads them, so they add nothing to the graph.
  routed?: boolean;
  documents?: number;
  // Those a model read for the names and facts they hold.
  read?: number;
  duplicate?: number;
  failed?: number;
  // Those no route of the load's version reads (out_of_scope).
  notRead?: number;
  // Those the load has not extracted yet.
  waiting?: number;
  // (int64) Its documents' fact writes: a fact counts once for each
  // document that created it or added evidence to it.
  facts?: string;
  // (int64) Its documents' values written: property values each stated
  // that were written as evidence, a value counting once for each document.
  attributes?: string;
  // (int64) The reviews its documents left when the load last wrote each,
  // of every kind. Not the open reviews: one settled since stays counted
  // until the load writes the decision, and a disputed value's once chosen.
  reviews?: string;
  // (int64) The names matched to no one record (none, several, or
  // identifiers that disagree). Nearly every one left a review too.
  unresolved?: string;
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
export type ReadFoundryGraphRequest = {
  readsJson: string;
  // The day to read as of, YYYY-MM-DD, not after today (UTC); missing for
  // now (contract 4.28.0). Every read but view and evidence then fails
  // `current_only`, and evidence leaves out quotes of documents dated after
  // it and flags an undated document's quote `undated`.
  validAt?: string;
};

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
  // The day it is answered as of, YYYY-MM-DD; missing for when it was asked.
  validAt?: string;
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
  // The day to answer as of, YYYY-MM-DD, not after today (UTC); missing
  // for now. Entities are read as they were then and documents dated after
  // it left out; reads that can only show the load now aren't offered.
  // INVALID_ARGUMENT for a day that isn't one or is later; FAILED_PRECONDITION
  // for someone who holds only part of the load.
  validAt?: string;
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
 * A number computed from two props of one entity (contract 4.26.0): days
 * from one date or datetime to another, a difference, or a ratio. An entity
 * missing either prop (or dividing by zero) has none.
 */
export type QueryExpr =
  | { days_between: [string, string] }
  | { diff: [string, string] }
  | { ratio: [string, string] };

/**
 * What a column, an order or an aggregate reads of each entity: one prop,
 * or a number computed from two (`expr`); never both.
 */
export type QueryMeasure =
  | { prop: string; expr?: never }
  | { expr: QueryExpr; prop?: never };

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
    // Keys' plain values (contract 4.27.0): the value alone for a key of one
    // property, or an object of each key property's value.
    keys?: (string | number | Record<string, string | number>)[] | null;
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
          // A prop (or `name`) of the entity at `at`, or a number computed
          // from two of its props; the entity if neither.
          columns: { at: number; prop?: string | null; expr?: QueryExpr }[];
          order_by?: ({ at: number; desc?: boolean } & QueryMeasure) | null;
          limit?: number;
          // Rows, in order, to pass over first; at most 10,000.
          offset?: number;
        };
      }
    // The last position when `distinct_at` is null or left out.
    | { count: { distinct_at?: number | null } }
    | { group_count: { at: number; prop: string; limit?: number } }
    | {
        aggregate: {
          fn: "sum" | "avg" | "min" | "max";
          at: number;
          group_by?: { at: number; prop: string } | null;
          limit?: number;
        } & QueryMeasure;
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
  | {
      version: {
        tx: string;
        at: string;
        of: VersionOf;
        // When the state held in the world (contract 4.7.0), distinct from
        // at: from `from` until `to`. `day` is a source's day with no time
        // of day, whose `from` is the next midnight UTC.
        valid?: { from: string; to?: string; day?: string };
      };
    };

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
  // `value` is null for a mean, least or greatest of no values. `skipped`
  // (contract 4.26.0) counts the entities a computed measure had no value
  // for; left out when none were.
  | {
      kind: "aggregate";
      value: number | null;
      values: number;
      skipped?: number;
    }
  | {
      kind: "grouped_aggregate";
      // Greatest first, then by value, the entities without one last.
      groups: {
        value: unknown;
        aggregate: number | null;
        values: number;
        skipped?: number;
      }[];
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

/** Contract 4.7.0's classification of why a question cannot be answered. */
export type CannotAnswerKind =
  | "uncovered"
  | "after_as_of"
  | "undeclared_relation"
  | "undeclared_property"
  | "absent";

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
  | {
      status: "cannot_answer";
      reason: string;
      gap: string;
      kind?: CannotAnswerKind;
    }
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

// ---------------------------------------------------------------- Agent
// Routes under /v1/workspaces/{workspaceId}/foundry/agent/...: the
// Assistant, a conversation with a model over Foundry's own tools that acts
// as its person (backend/agent). Threads are private to their user: another
// user's thread, turn or action answers NOT_FOUND. A person's message queues
// a turn the foundry-agent-worker runs; its events are polled by `afterSeq`,
// as a question's are. What would change the workspace the model only
// prepares: an action its person runs (Execute, with the payload's hash they
// were shown), dismisses, or refreshes once its confirmation expired. A run
// action queues a continuation turn (kind action_result) the agent goes on
// from. Grantees are denied every route; a caller that isn't a person signed
// in is PERMISSION_DENIED. Refusals carry an ErrorInfo reason
// (AGENT_REFUSALS); the user's hourly messages are RESOURCE_EXHAUSTED.

/** v1FoundryAgentTurn.status. */
export type FoundryAgentTurnStatus =
  | "queued"
  | "running"
  | "done"
  | "failed"
  | "cancelled";

/** v1FoundryAgentAction.status, as read: a prepared action past its expiry reads expired, one executing for over 2 minutes unknown. */
export type AgentActionStatus =
  | "prepared"
  | "executing"
  | "succeeded"
  | "failed"
  | "unknown"
  | "expired"
  | "dismissed";

/** v1FoundryAgentPageContext: what the person was looking at, ids only. */
export type AgentPageContext = {
  ontologyId?: string;
  loadId?: string;
  // The dashboard open, if any.
  dashboardId?: string;
  // The person is on the page that starts a new automation.
  newAutomation?: boolean;
  // The IANA time zone the person's browser names (America/New_York): the
  // Assistant's default for a schedule. One the WFE can't read is dropped.
  timeZone?: string;
};

/**
 * Who runs a thread's turns: Foundry's own worker on the workspace's model
 * (api), or a helper on a local install's owner's computer driving the
 * Claude Code or Codex CLI they signed in to, on their subscription, at no
 * cost to the workspace. A thread keeps the runtime it started with.
 */
export type AgentRuntime = "api" | "claude_code" | "codex";

/** v1FoundryAgentThread */
export type FoundryAgentThread = {
  id: string;
  // The first message's start.
  title?: string;
  runtime?: AgentRuntime;
  createdAt?: string | null;
  updatedAt?: string | null;
};

/** v1FoundryAgentTurn */
export type FoundryAgentTurn = {
  id: string;
  threadId?: string;
  // automation: an automation's Assistant step, in its run's thread (a
  // thread of the run's owner's, out of their list of threads); answers:
  // the person's answers to the questions the turn before asked.
  kind?: "message" | "action_result" | "answers" | "automation";
  // The person's text; empty for any turn but a message.
  text?: string;
  // The action an action_result turn goes on from.
  actionId?: string;
  pageContext?: AgentPageContext | null;
  status?: FoundryAgentTurnStatus;
  error?: string;
  // int64 millionths of a dollar.
  usdMicros?: string;
  steps?: number;
  lastSeq?: number;
  // A queued turn with attempts above 0 is retrying.
  attempts?: number;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
  // What the turn asked its person (helix_foundry_ask_person), which it
  // ended waiting on; unset when it asked nothing. Answerable (send with
  // answers) while it is the thread's newest turn and done.
  asked?: FoundryAgentAsked | null;
  // An answers turn's: the turn it answers, and the person's answers.
  answers?: FoundryAgentAnswers | null;
};

/**
 * v1FoundryAgentAsked: what a turn asked its person: 1 to 4 questions of
 * the Assistant's own (ids q1, q2, ...), or a generation's waiting round,
 * relayed as its agent wrote it, under its own ids (`generation` set).
 */
export type FoundryAgentAsked = {
  questions?: AgentQuestion[];
  generation?: FoundryAgentAskedGeneration | null;
};
/**
 * v1FoundryAgentAskedGeneration: the generation whose round of questions a
 * turn relayed: sending the answers sends them to it, as the person, while
 * it still waits on that round (else FOUNDRY_AGENT_QUESTIONS_CLOSED).
 */
export type FoundryAgentAskedGeneration = {
  ontologyId?: string;
  generationId?: string;
  round?: number;
};
/**
 * v1FoundryAgentAnswers: the person's answers to the questions `turnId`
 * asked, one per question at most; a question left out (only one that isn't
 * blocking) is left to the Assistant.
 */
export type FoundryAgentAnswers = {
  turnId: string;
  answers: AgentQuestionAnswer[];
};

/** v1FoundryAgentAction: what the agent prepared, which only its person runs. */
export type FoundryAgentAction = {
  id: string;
  turnId?: string;
  threadId?: string;
  // As the MCP's prepare names them: create_load, ontology:<workspace>/<ontology>.
  operation?: string;
  target?: string;
  // The canonical payload, and the hash Execute sends back.
  payloadJson?: string;
  payloadHash?: string;
  // The names Foundry resolved: AgentActionDisplay as JSON.
  displayJson?: string;
  // False when the target's ids never appeared in the person's messages or page.
  targetNamedByUser?: boolean;
  status?: AgentActionStatus;
  // The operation's output once it ran (FoundryOperationOutput, untrusted).
  resultJson?: string;
  error?: string;
  expiresAt?: string | null;
  decidedByUserId?: string;
  decidedAt?: string | null;
  createdAt?: string | null;
};

// What a card names (backend/agent display.go), resolved by Foundry from
// the workspace when the action is prepared, never the model's words: one
// shape per operation, read by the action's operation, snake_case. A name
// that couldn't be read (gone since, or never there) is missing, its id kept.

/** One thing of the workspace: its id and its name. */
export type AgentRef = { id: string; name?: string };
/**
 * A connector, with its kind (postgres, stripe, ...); `sample` when it reads
 * the made-up sample company's database.
 */
export type AgentConnectorRef = AgentRef & { kind?: string; sample?: true };
/** An ontology version by its number (v3). */
export type AgentVersionRef = { id: string; number?: number };
/** A load: the version it wrote, its status and when it started. */
export type AgentLoadRef = {
  id: string;
  version: AgentVersionRef;
  status?: string;
  created_at?: string;
};
/** How settle_review settles a review (the MCP's `settle`). */
export type AgentReviewSettle =
  | "resolve"
  | "prop"
  | "release"
  | "stale"
  | "none";
/**
 * A review the current load holds, as helix_foundry_list_reviews gives it
 * (integration.FoundryReviewOutput): its quote, values and statement are a
 * document's words, untrusted.
 */
export type AgentReview = {
  review_key: string;
  reason: string;
  settle: AgentReviewSettle;
  type?: string;
  doc_id?: string;
  quote?: string;
  section?: number;
  start?: number;
  end?: number;
  norm_name?: string;
  candidates?: string[];
  entity_key?: string;
  prop?: string;
  values?: { value: unknown; doc_id?: string; quote?: string }[];
  link?: string;
  // JSON text {subject: {type, name}, object: {type, name}, props}.
  statement?: string;
  statement_hash?: string;
  confidence?: number;
  // The entities it concerns as the load holds them: its candidates, its
  // property's entity.
  entities?: AgentReviewEntity[];
};
/**
 * An entity a review concerns, by the stored identity its candidates and
 * entity_key name it by (a type without a key's normalized name), with its
 * stored name: untrusted.
 */
export type AgentReviewEntity = { type: string; name?: string } & (
  | { entity_key: string }
  | { norm_name: string }
);
/** A value settle_review sets: exactly one arm. */
export type AgentDecisionValue =
  | { text: string }
  | { integer: string }
  | { number: number }
  | { boolean: boolean };
/** What a settle_review decides (DecisionDisplay): a closed set. */
export type AgentDecision =
  // The name is entity_key; `candidate` false when the review didn't offer it.
  | { kind: "link"; entity_key: string; candidate: boolean }
  | { kind: "not_in_data" }
  | { kind: "set_value"; value: AgentDecisionValue }
  | { kind: "remove_value" }
  | { kind: "approve" }
  | { kind: "reject" };

/** Each operation's display, by operation. */
export type AgentActionDisplays = {
  // Exactly one of database (an existing one) and new_database.
  create_ontology: { name: string } & (
    | { database: AgentRef }
    | {
        new_database: {
          name: string;
          slug: string;
        } & ({ project_id: string } | { new_project: string });
      }
  );
  generate_ontology: {
    ontology: AgentRef;
    goal?: string;
    // every: connectors lists the workspace's, as they are now (API
    // sources only when named in event_sources); none: documents alone.
    connectors_read: "every" | "none" | "chosen";
    connectors: AgentConnectorRef[];
    document_sets: AgentRef[];
    event_sources?: { connector: AgentConnectorRef; mode: string }[];
    // "acme": the sample company's ready-made draft.
    sample?: string;
  };
  // current: the version current now; missing when none is.
  set_current_version: {
    ontology: AgentRef;
    version: AgentVersionRef;
    current?: AgentVersionRef;
  };
  create_load: { ontology: AgentRef; version: AgentVersionRef };
  set_current_load: {
    ontology: AgentRef;
    load: AgentLoadRef;
    current?: AgentLoadRef;
  };
  revise_version: {
    ontology: AgentRef;
    version: AgentVersionRef;
    edits: { kind: "put" | "remove"; item_id: string }[];
    dry_run: boolean;
  };
  sync_connector: { connector: AgentConnectorRef };
  add_sample_data: { ontology: AgentRef };
  create_document_set: {
    ontology: AgentRef;
    name: string;
    description?: string;
  };
  // set: as it is named now; a field left out stays, "" clears the description.
  update_document_set: {
    ontology: AgentRef;
    set: AgentRef;
    name?: string;
    description?: string;
  };
  move_document: {
    ontology: AgentRef;
    document: AgentRef;
    from: AgentRef;
    to: AgentRef;
  };
  settle_review: {
    ontology: AgentRef;
    load_id: string;
    review: AgentReview;
    document?: AgentRef;
    decision: AgentDecision;
  };
  // A local install's only. Where the workspace's new threads run after it
  // (runtime) and before (current), and whether a helper that runs runtime
  // is online (always, for api).
  set_assistant_runtime: {
    runtime: AgentRuntime;
    current: AgentRuntime;
    online: boolean;
  };
  // A local install's only: the helper, what it runs, whether it is online.
  revoke_assistant_helper: {
    host: AgentRef;
    runtimes: AgentHelperRuntime[];
    online: boolean;
  };
  // A dashboard's name and description as the Assistant wrote them.
  create_dashboard: {
    name: string;
    description?: string;
    spec: AgentSpecDisplay;
  };
  // What changes; a field missing stays as it is, "" clears the description.
  update_dashboard: {
    dashboard: AgentDashboardRef;
    name?: string;
    description?: string;
    spec?: AgentSpecDisplay;
  };
  delete_dashboard: { dashboard: AgentDashboardRef };
  // pinned: whether it pins the dashboard (else unpins it).
  pin_dashboard: { dashboard: AgentDashboardRef; pinned: boolean };
  // expires_in_days: when the link ends, in days from the publish (0 never);
  // missing keeps a live link's own, and a new link never ends.
  publish_dashboard: { dashboard: AgentDashboardRef; expires_in_days?: number };
  unpublish_dashboard: { dashboard: AgentDashboardRef };
  // Its name as the Assistant wrote it; its steps are the payload's spec.
  create_workflow: { name: string };
  // steps: whether the edit carries steps (the payload's spec), which the
  // person's click approves; without, it approves nothing.
  update_workflow: {
    automation: AgentAutomationRef;
    name?: string;
    steps: boolean;
  };
  pause_workflow: { automation: AgentAutomationRef };
  resume_workflow: { automation: AgentAutomationRef };
  delete_workflow: { automation: AgentAutomationRef };
  run_workflow: { automation: AgentAutomationRef };
};
/** An automation as it was when the action was prepared, with its status. */
export type AgentAutomationRef = AgentRef & { status?: AutomationStatus };
/** A dashboard as it was when the action was prepared. */
export type AgentDashboardRef = AgentRef & { pinned?: true; published?: true };
/**
 * A spec's tiles as a card names them: each tile's title, the ontology
 * whose load it reads, and its visual's kind (missing: as the result suits).
 */
export type AgentSpecDisplay = {
  tiles: { title: string; ontology: AgentRef; visual?: string }[];
};
/** The operations the Assistant may prepare (backend agent.Operations). */
export type AgentOperation = keyof AgentActionDisplays;

// Where a local install's Assistant runs, and its helpers: GET and PATCH
// .../agent/runtime, POST .../agent/host-pairings and
// .../agent/hosts/{id}:revoke. The owner's only (PERMISSION_DENIED
// otherwise); hosted Helix answers FAILED_PRECONDITION, reason
// FOUNDRY_AGENT_HOSTS_LOCAL_ONLY.

/** A runtime a helper runs: a CLI on its owner's subscription. */
export type AgentHelperRuntime = Exclude<AgentRuntime, "api">;

/**
 * v1FoundryAgentHostAccount: what one of a helper's CLIs is signed in to,
 * as the helper last found it.
 */
export type FoundryAgentHostAccount = {
  runtime?: AgentHelperRuntime;
  // signed_in; signed_out (its turns fail until the person signs in); or
  // unknown (the CLI didn't start).
  state?: "signed_in" | "signed_out" | "unknown";
  // The plan as the CLI names it (pro, max, team, plus, ...), only when
  // signed in on a subscription; missing for a CLI signed in some other way,
  // whose turns don't bill a subscription.
  subscription?: string;
  // The CLI's version as it says it (Codex's).
  version?: string;
  // A Codex older than FoundryAgentRuntime.codexMinVersion (or of no version
  // the helper could read): it runs no turn until updated.
  outdated?: boolean;
};

/** v1FoundryAgentHost: a helper paired with the install (install-wide, not per workspace). */
export type FoundryAgentHost = {
  id: string;
  // The machine's name, as the helper gave it.
  name?: string;
  // What it runs, as of its last call: an outdated Codex isn't listed (its
  // account says outdated).
  runtimes?: AgentHelperRuntime[];
  // What their CLIs are signed in to, as of its last call; empty until its
  // first call after pairing.
  accounts?: FoundryAgentHostAccount[];
  version?: string;
  // darwin-arm64, linux-x64, ...
  platform?: string;
  createdAt?: string | null;
  // Its last call; missing until its first after pairing, which starts
  // nothing (it runs once its run or install does).
  lastSeenAt?: string | null;
  // Seen within 30 seconds.
  online?: boolean;
  revokedAt?: string | null;
};
/** v1FoundryAgentRuntime */
export type FoundryAgentRuntime = {
  // Where the workspace's new threads run.
  runtime?: AgentRuntime;
  // Not revoked, newest first.
  hosts?: FoundryAgentHost[];
  // The helper runtimes an online helper runs now, sorted.
  online?: AgentHelperRuntime[];
  // When the owner confirmed, once, what codex runs with: their own Codex
  // setup (its MCP servers and plugins), its shell off and every command or
  // file change declined, on their ChatGPT plan. Missing until then.
  codexConfirmedAt?: string | null;
  // The oldest Codex a helper runs it on (x.y.z).
  codexMinVersion?: string;
};
/** GET and PATCH .../agent/runtime */
export type FoundryAgentRuntimeResponse = { runtime?: FoundryAgentRuntime };
/**
 * PATCH .../agent/runtime: a helper need not be online to be chosen. codex
 * needs confirmCodex once (it is kept), else FAILED_PRECONDITION, reason
 * FOUNDRY_AGENT_CODEX_UNCONFIRMED.
 */
export type UpdateFoundryAgentRuntimeRequest = {
  runtime: AgentRuntime;
  confirmCodex?: boolean;
};
/** POST .../agent/host-pairings: a one-time code, ten minutes, shown once. */
export type FoundryAgentHostPairingResponse = {
  // hxp_ and 43 characters.
  code?: string;
  expiresAt?: string | null;
  // npx -y @helixdb/foundry-agent-host@<version> pair <code>
  command?: string;
};
/** POST .../agent/hosts/{id}:revoke */
export type FoundryAgentHostResponse = { host?: FoundryAgentHost };

/**
 * POST .../agent/turns: a message (in a new thread when `threadId` is
 * empty), or the person's answers to the questions their thread's newest
 * turn asked: that turn must be done, having asked, with nothing after it
 * (FAILED_PRECONDITION, FOUNDRY_AGENT_QUESTIONS_CLOSED, otherwise); the
 * answers are checked as AgentQuestionAnswer says, every blocking question
 * answered (INVALID_ARGUMENT otherwise).
 */
export type SendAgentMessageRequest = {
  pageContext?: AgentPageContext;
  // At most 64 characters: the same token returns the turn it queued.
  clientToken: string;
} & (
  | {
      // Empty: a new thread.
      threadId?: string;
      // At most 4,000 characters.
      text: string;
      answers?: never;
    }
  | { threadId: string; answers: FoundryAgentAnswers; text?: never }
);
/** v1FoundryAgentTurnResponse: POST turns and turns/{id}:cancel. */
export type AgentTurnResponse = {
  turn: FoundryAgentTurn;
  thread: FoundryAgentThread;
};
/** GET .../agent/threads: the caller's, newest first, 50 a page. */
export type ListAgentThreadsResponse = {
  threads?: FoundryAgentThread[];
  nextPageToken?: string;
};
/** GET .../agent/threads/{id}: its newest 100 turns, oldest first, and every action prepared in it. */
export type AgentThreadResponse = {
  thread: FoundryAgentThread;
  turns?: FoundryAgentTurn[];
  actions?: FoundryAgentAction[];
};
/** v1FoundryAgentEvent: one event a turn shows, as JSON (AgentEvent). */
export type FoundryAgentEvent = {
  seq: number;
  eventJson: string;
  createdAt?: string | null;
};
/**
 * GET .../agent/turns/{id}/events?afterSeq=: once `status` is not queued or
 * running and the events reach `lastSeq`, none follows.
 */
export type ListAgentTurnEventsResponse = {
  events?: FoundryAgentEvent[];
  status?: FoundryAgentTurnStatus;
  lastSeq?: number;
};
/** POST .../agent/actions/{id}:execute */
export type ExecuteAgentActionRequest = { payloadHash: string };
/** v1FoundryAgentActionResponse: execute, refresh and dismiss. */
export type AgentActionResponse = {
  action: FoundryAgentAction;
  // The turn the agent goes on from the action's result in; unset unless it ran.
  continuation?: FoundryAgentTurn | null;
};

/** ErrorInfo reasons of the Assistant's FAILED_PRECONDITION refusals. */
export const AGENT_REFUSALS = [
  // foundry.agent.enabled is off.
  "FOUNDRY_AGENT_DISABLED",
  // The thread has a live turn, or an action of it is running.
  "FOUNDRY_AGENT_THREAD_BUSY",
  // The month's Assistant budget is spent.
  "FOUNDRY_AGENT_BUDGET",
  // 3 actions already wait in the thread.
  "FOUNDRY_AGENT_TOO_MANY_ACTIONS",
  // Execute or refresh of an action that isn't waiting.
  "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
  // The payload shown isn't the one prepared.
  "FOUNDRY_AGENT_ACTION_CHANGED",
  // The confirmation expired: refresh it.
  "FOUNDRY_AGENT_ACTION_EXPIRED",
  // A thread a helper runs (claude_code, codex) while no helper that runs
  // it has called in the last 30 seconds: nothing is queued.
  "FOUNDRY_AGENT_HOST_OFFLINE",
  // The runtime routes on a host without helpers (hosted Helix).
  "FOUNDRY_AGENT_HOSTS_LOCAL_ONLY",
  // codex, chosen before the owner confirmed what it runs with.
  "FOUNDRY_AGENT_CODEX_UNCONFIRMED",
  // Answers to questions no longer open: answered already, or the
  // conversation went on.
  "FOUNDRY_AGENT_QUESTIONS_CLOSED",
] as const;
export type AgentRefusalReason = (typeof AGENT_REFUSALS)[number];

/**
 * Why a turn ended (backend/agent StopReason): the model finished, it asked
 * its person (their answers come as the next turn), a cap (steps, wall
 * time, budget, its answer's length, its window), it declined, or it failed.
 */
export type AgentStopReason =
  | "answered"
  | "asked"
  | "max_steps"
  | "wall_time"
  | "budget"
  | "max_tokens"
  | "refusal"
  | "context"
  | "failed";

/**
 * Why a call the model wrote never ran (agent.NotRunReason): a reply's
 * calls past its first 8, a turn's asks past its 3, a call after the turn's
 * time ran out, one the reply was writing at its length limit, one whose
 * input isn't JSON, or one after the turn asked its person.
 */
export type AgentNotRunReason =
  | "calls"
  | "asks"
  | "time"
  | "length"
  | "input"
  | "asked";
/**
 * A turn's events (FoundryAgentEvent.eventJson), each with its `type`: a
 * closed set the page parses without trust. Text is the model's, shown as
 * plain text; a tool's output is untrusted data, at most 16 KiB.
 */
export type AgentEvent =
  | { type: "started"; model: string; attempt: number }
  // `step` counts a turn's model calls across its attempts, from 1. A
  // step's text, final once its call ended: one or more, which supersede
  // its deltas (the server drops them as it writes the text).
  | { type: "text"; step: number; text: string }
  // Text the step's call is writing, every 250 ms: appended in seq order.
  // Deltas no text follows (the stream broke) end at the next other event.
  | { type: "text_delta"; step: number; text: string }
  | {
      type: "tool_call";
      tool_use_id: string;
      name: string;
      title: string;
      input: unknown;
    }
  | {
      type: "tool_result";
      tool_use_id: string;
      name: string;
      is_error: boolean;
      error?: string;
      output?: unknown;
      truncated?: boolean;
      // Why the call never ran, which the page words (no `error`).
      not_run?: AgentNotRunReason;
      // The Analyst question a helix_foundry_ask or helix_foundry_get_question
      // call returned, kept whatever the output's size (`truncated` too).
      question?: {
        ontology_id: string;
        id: string;
        load_id?: string;
        thread_id?: string;
        status?: string;
      };
    }
  | {
      type: "action_card";
      action_id: string;
      operation: string;
      target: string;
      payload: unknown;
      payload_hash: string;
      // AgentActionDisplays[operation], parsed by the page.
      display: unknown;
      target_named_by_user: boolean;
      expires_at: string;
      // It ran at once, without a click: a new dashboard the person's own
      // message asked for, which the dock offers to undo.
      auto?: boolean;
    }
  | {
      // The first event of an action_result turn: how the person's click ended.
      type: "action_result";
      action_id: string;
      operation: string;
      status: AgentActionStatus;
      result?: unknown;
      error?: string;
      // How an action that ran at once ended (its card's `auto`), in the
      // turn that prepared it.
      auto?: boolean;
    }
  | {
      // The questions the turn asks its person (helix_foundry_ask_person),
      // which it ends waiting on: the Assistant's own, or a generation's
      // round, relayed as its agent wrote it (`generation` set).
      type: "questions";
      questions: AgentQuestion[];
      generation?: {
        ontology_id: string;
        ontology_name: string;
        generation_id: string;
        generation_title?: string;
        round: number;
      };
    }
  | {
      // The first event of an answers turn: the person's answers, as checked.
      type: "answers";
      turn_id: string;
      answers: { question_id: string; options: string[]; text?: string }[];
    }
  | {
      // An automation's step's report (its newest is the step's output):
      // untrusted, the model's words; answer true for yes.
      type: "automation_report";
      summary: string;
      answer?: boolean;
      count?: number;
    }
  | {
      type: "finished";
      status: "done" | "failed";
      reason: AgentStopReason;
      error?: string;
    };

// ---------------------------------------------------------------- Dashboards
// Routes under /v1/workspaces/{workspaceId}/foundry/dashboards...
// (backend/api/dashboards.go, backend/dashboard): tiles of typed queries over
// ontologies' loads, shown as tables and charts. A dashboard holds queries,
// never data: the viewer reads each tile through the tile's load's :read
// (POST .../ontologies/{ontologyId}/loads/{loadId}:read, a tile's
// `{"query": …}` as one read; the tiles of one load in one call), so it shows
// no one more than they may read. Every member lists and reads every
// dashboard, so its page (/dashboards/{id}) is how it is shared inside the
// workspace; a grantee of source grants only the pinned ones. A member
// creates dashboards and edits and deletes their own while unpinned; owners
// and admins edit and delete any, pin them in every member's sidebar, and,
// hosted only, publish a snapshot at a public link for people outside the
// workspace (canPublish; publishBlock says why not), which may expire and
// which an owner may turn off for the whole workspace
// (.../foundry/dashboard-settings). An edit and
// a publish name the revision read (int64, as a string): another is
// FAILED_PRECONDITION FOUNDRY_DASHBOARD_CHANGED. Refusals carry an ErrorInfo
// reason (DASHBOARD_REFUSALS); a change the caller may not make is
// PERMISSION_DENIED.

/** The ErrorInfo reasons of the dashboards' FAILED_PRECONDITION refusals. */
export const DASHBOARD_REFUSALS = [
  // An edit or publish of a revision another write has passed: read it again.
  "FOUNDRY_DASHBOARD_CHANGED",
  // The workspace's 200 dashboards, or 20 pinned ones.
  "FOUNDRY_DASHBOARD_LIMIT",
  // A tile reads the current load of an ontology that has none.
  "FOUNDRY_DASHBOARD_NOT_LOADED",
  // Publishing: a tile that can't be read (the message names it and why),
  // a dashboard with no tile, a snapshot past 512 KiB a tile or 2 MiB in all.
  "FOUNDRY_DASHBOARD_TILE_FAILED",
  "FOUNDRY_DASHBOARD_EMPTY",
  "FOUNDRY_DASHBOARD_TOO_LARGE",
  // Publishing or unpublishing as neither an owner nor an admin; publishing
  // where the stamp or build has no public links, or in a workspace whose
  // owner turned them off.
  "FOUNDRY_DASHBOARD_PUBLIC_ADMIN_ONLY",
  "FOUNDRY_DASHBOARD_PUBLIC_UNAVAILABLE",
  "FOUNDRY_DASHBOARD_PUBLIC_OFF",
  // Publishing an expired link again without choosing when the new one ends.
  "FOUNDRY_DASHBOARD_LINK_EXPIRED",
] as const;
export type DashboardRefusal = (typeof DASHBOARD_REFUSALS)[number];

/** A tile's load: whichever is current when it is read, or one pinned. */
export type DashboardTileLoad =
  | { kind: "current" }
  | { kind: "pinned"; load_id: string };

/**
 * One tile of a spec (backend/dashboard): a typed query of one ontology's
 * load and how to show it. The spec is the backend's JSON, in snake_case.
 * id: 1 to 32 of a-z, 0-9, - and _, starting with a letter or digit, unique;
 * title: 1 to 80 characters, no control characters; query: at most 3 KiB,
 * the read contract's Query (at most 200 rows, 100 groups); visual.kind left
 * out: as the output's shape suits, as an Analyst visual's.
 */
export type DashboardTile = {
  id: string;
  title: string;
  ontology_id: string;
  load: DashboardTileLoad;
  query: OntologyQuery;
  visual: { kind?: VisualKind };
};

/** A dashboard's spec: at most 20 tiles, each in exactly one row of 1 to 3. */
export type DashboardSpec = {
  v: 1;
  tiles: DashboardTile[];
  layout: { rows: string[][] };
};

/** v1FoundryDashboardShareStatus: whether a public link shows its snapshot now; only LIVE does. */
export type DashboardShareStatus =
  | "FOUNDRY_DASHBOARD_SHARE_STATUS_UNSPECIFIED"
  | "FOUNDRY_DASHBOARD_SHARE_STATUS_LIVE"
  // Past its expiresAt: publishing again makes a new link.
  | "FOUNDRY_DASHBOARD_SHARE_STATUS_EXPIRED"
  // An owner turned public links off for the workspace: back when turned on.
  | "FOUNDRY_DASHBOARD_SHARE_STATUS_WORKSPACE_OFF"
  // The stamp has public links off.
  | "FOUNDRY_DASHBOARD_SHARE_STATUS_UNAVAILABLE";

/** v1FoundryDashboardPublishBlock: why the caller may not publish; UNSPECIFIED when they may. */
export type DashboardPublishBlock =
  | "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNSPECIFIED"
  | "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_ADMIN_ONLY"
  | "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_WORKSPACE_OFF"
  | "FOUNDRY_DASHBOARD_PUBLISH_BLOCK_UNAVAILABLE";

/** v1FoundryDashboardLinkExpiry: when a publish ends its link; UNSPECIFIED keeps a live link's own. */
export type DashboardLinkExpiry =
  | "FOUNDRY_DASHBOARD_LINK_EXPIRY_UNSPECIFIED"
  | "FOUNDRY_DASHBOARD_LINK_EXPIRY_NEVER"
  | "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_7"
  | "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_30"
  | "FOUNDRY_DASHBOARD_LINK_EXPIRY_DAYS_90";

/** v1FoundryDashboardShare: a dashboard's public snapshot. */
export type FoundryDashboardShare = {
  // The link's token, for owners and admins only, and not once it expired:
  // the page is /share/<token>.
  token?: string;
  // int64: the revision it shows, older than the dashboard's after an edit.
  specRevision?: string;
  publishedAt?: string | null;
  firstPublishedAt?: string | null;
  publishedByUserId?: string;
  // When the link stops showing the snapshot; missing: never.
  expiresAt?: string | null;
  status?: DashboardShareStatus;
};

/** v1FoundryDashboard */
export type FoundryDashboard = {
  id: string;
  name?: string;
  description?: string;
  // DashboardSpec as JSON; empty in a list.
  specJson?: string;
  // int64, from 1: what an edit or a publish names.
  revision?: string;
  createdByUserId?: string;
  updatedByUserId?: string;
  pinned?: boolean;
  pinnedAt?: string | null;
  pinnedByUserId?: string;
  createdAt?: string | null;
  updatedAt?: string | null;
  share?: FoundryDashboardShare | null;
  // What the caller may do: change its name, description and tiles; pin or
  // unpin it; publish and republish it (false wherever public links are off);
  // delete it (an owner or admin only while it is published); unpublish it
  // (an owner or admin while it is published, links on or off).
  canEdit?: boolean;
  canPin?: boolean;
  canPublish?: boolean;
  canDelete?: boolean;
  canUnpublish?: boolean;
  // Why the caller may not publish it; missing or UNSPECIFIED when they may.
  publishBlock?: DashboardPublishBlock;
};

/** GET .../dashboards: every one (at most 200), pinned first, without specs. */
export type ListDashboardsResponse = {
  dashboards?: FoundryDashboard[];
  canCreate?: boolean;
};
/** v1FoundryDashboardResponse: every dashboard route but list, delete and preview. */
export type DashboardResponse = { dashboard: FoundryDashboard };
/** POST .../dashboards: unpinned; the same clientToken returns the one made. */
export type CreateDashboardRequest = {
  // 1 to 120 characters.
  name: string;
  // At most 1,000 characters.
  description?: string;
  // DashboardSpec as JSON; empty for no tiles yet.
  specJson?: string;
  // 1 to 64 characters.
  clientToken: string;
};
/** PATCH .../dashboards/{id}: each field set is changed, at least one. */
export type UpdateDashboardRequest = {
  expectedRevision: string;
  name?: string;
  description?: string;
  specJson?: string;
};
/**
 * POST .../dashboards/{id}:pin, owners and admins: at the revision read
 * (another is FOUNDRY_DASHBOARD_CHANGED), so what is pinned is what was seen.
 */
export type PinDashboardRequest = { pinned: boolean; expectedRevision: string };
/**
 * POST .../dashboards/{id}:add-question-visual: an answered question's query
 * result (evidenceId, such as e2) as a tile of its own row, last, with its
 * visual's kind and title (else the question's words). pinLoad reads the
 * question's load rather than the current one.
 */
export type AddDashboardQuestionVisualRequest = {
  expectedRevision: string;
  ontologyId: string;
  questionId: string;
  evidenceId: string;
  pinLoad?: boolean;
};
/** POST .../dashboards:preview-tile: a tile checked and read once, as the caller. */
export type PreviewDashboardTileRequest = { tileJson: string };
export type PreviewDashboardTileResponse = {
  // The tile as a spec keeps it.
  tileJson?: string;
  // The load it read: the current one, or the one it pins.
  loadId?: string;
  // {"ok": QueryOutput} or {"error": why it has none}, as :read gives it.
  outcomeJson?: string;
  asOf?: string;
};
/** POST .../dashboards/{id}:publish, owners and admins, hosted only, while the workspace has public links on. */
export type PublishDashboardRequest = {
  expectedRevision: string;
  // When the link ends, from now; missing keeps a live link's own (a first
  // one never ends; one that expired is refused FOUNDRY_DASHBOARD_LINK_EXPIRED).
  expiry?: DashboardLinkExpiry;
};

/** v1FoundryDashboardSettings: a workspace's dashboard settings. */
export type FoundryDashboardSettings = {
  // Whether its dashboards may be published at public links: on until an
  // owner turns them off; off, no link of the workspace shows.
  publicLinks?: boolean;
  // Whether the stamp offers public links at all.
  available?: boolean;
  // Owners only.
  canChange?: boolean;
  updatedByUserId?: string;
  updatedAt?: string | null;
};
/** GET and PATCH .../foundry/dashboard-settings. */
export type FoundryDashboardSettingsResponse = {
  settings?: FoundryDashboardSettings;
};
/** PATCH .../foundry/dashboard-settings, owners only. */
export type UpdateDashboardSettingsRequest = { publicLinks: boolean };

/**
 * A published dashboard as its public link shows it, from the
 * foundry-events door's GET /v1/shared-dashboards/{token} (NEXT_PUBLIC_HELIX_EVENTS_URL),
 * with no session: no workspace's, ontology's, load's or person's id. Each
 * tile's output is its query's answer as :read gave its publisher, strings
 * cut to 1,000 characters, a row's entities as {type, name} only (no id),
 * and each row's edges empty; its query only its shape (the type at each
 * position, each step's link and what it returns), never a value it tested
 * (start.ids, start.text, where, filters).
 */
export type DashboardSnapshot = {
  v: 1;
  name: string;
  description: string;
  published_at: string;
  layout: { rows: string[][] };
  tiles: {
    id: string;
    title: string;
    visual: { kind?: VisualKind };
    query: {
      start: { type: string };
      steps: { via: string; dir: "out" | "in"; to: string }[];
      return: OntologyQuery["return"];
    };
    as_of: string;
    output: QueryOutput;
  }[];
};

// ---------------------------------------------------------------- Automations
// Routes under /v1/workspaces/{workspaceId}/foundry/automations...
// (backend/api/automations.go, backend/automation): a person's graph of
// steps Foundry runs as them, its owner, when its trigger fires. Every member
// lists and reads every automation and creates their own; only its owner
// edits, approves, resumes and runs one; its owner or an owner or admin
// pauses, deletes and cancels a run. It runs only once its owner approved its
// spec here (:approve, never through an agent): new, and after every spec
// edit, it is needs_approval. An edit or an approval names the revision read
// (int64, as a string): another is FAILED_PRECONDITION
// FOUNDRY_AUTOMATION_CHANGED. Refusals carry an ErrorInfo reason
// (AUTOMATION_REFUSALS); one the caller may not make is PERMISSION_DENIED,
// and past the workspace's runs of the day RESOURCE_EXHAUSTED. "Workflows"
// are another thing: an outside agent's recorded runs.

/** The ErrorInfo reasons of the automations' FAILED_PRECONDITION refusals. */
export const AUTOMATION_REFUSALS = [
  // Automations are off on this stamp or build.
  "FOUNDRY_AUTOMATIONS_DISABLED",
  // An edit or approval of a revision another write has passed: read it again.
  "FOUNDRY_AUTOMATION_CHANGED",
  // The workspace's 50 automations.
  "FOUNDRY_AUTOMATION_LIMIT",
  // Resume or Run now while its spec waits for its owner's approval.
  "FOUNDRY_AUTOMATION_UNAPPROVED",
  // Run now while a run of it is live.
  "FOUNDRY_AUTOMATION_BUSY",
  // Pause while it waits for approval: it isn't running.
  "FOUNDRY_AUTOMATION_NOT_ACTIVE",
  // Cancel of a run that already ended.
  "FOUNDRY_AUTOMATION_RUN_ENDED",
] as const;
export type AutomationRefusal = (typeof AUTOMATION_REFUSALS)[number];

/**
 * An automation's trigger (backend/automation, snake_case): a schedule in an
 * IANA zone (every hour at minute 0 to 59, every day at "HH:MM", every week
 * on a day at "HH:MM"), work that ended on one target, or by hand only.
 */
export type AutomationTrigger =
  | { kind: "schedule"; every: "hour"; minute: number; time_zone: string }
  | { kind: "schedule"; every: "day"; at: string; time_zone: string }
  | {
      kind: "schedule";
      every: "week";
      weekday:
        | "monday"
        | "tuesday"
        | "wednesday"
        | "thursday"
        | "friday"
        | "saturday"
        | "sunday";
      at: string;
      time_zone: string;
    }
  | {
      kind: "sync_finished";
      connector_id: string;
      outcome: "succeeded" | "failed" | "any";
    }
  | { kind: "load_ready"; ontology_id: string }
  | { kind: "document_arrived"; set_id: string }
  | {
      kind: "generation_finished";
      ontology_id: string;
      outcome: "succeeded" | "failed" | "any";
    }
  | { kind: "manual" };

/**
 * An action, as an action step runs it and an Assistant step's allow
 * approves it: one operation on one fixed target. create_load loads the
 * ontology's current version; publish_dashboard refreshes its snapshot.
 */
export type AutomationAction =
  | { operation: "sync_connector"; connector_id: string }
  | { operation: "create_load"; ontology_id: string }
  | { operation: "publish_dashboard"; dashboard_id: string };

/** A node's kind and config. */
export type AutomationNodeConfig =
  | { kind: "trigger"; config: AutomationTrigger }
  // instruction: 1 to 2,000 characters; allow: at most 10, each once.
  | {
      kind: "assistant";
      config: { instruction: string; allow?: AutomationAction[] };
    }
  | { kind: "action"; config: AutomationAction }
  // answer: the Assistant step before it said yes or no; count: its count,
  // or a document_arrived or sync_finished trigger's.
  | {
      kind: "condition";
      config:
        | { check: "answer"; equals: "yes" | "no" }
        | {
            check: "count";
            op: "at_least" | "at_most" | "equals";
            value: number;
          };
    }
  // message: at most 500 characters.
  | { kind: "notify"; config: { message: string } };

/** One node of a spec: id 1 to 32 of a-z, 0-9, - and _, unique; position is the canvas's only. */
export type AutomationNode = AutomationNodeConfig & {
  id: string;
  position: { x: number; y: number };
};

/** An edge; branch exactly on an edge out of a condition, each at most once. */
export type AutomationEdge = {
  from: string;
  to: string;
  branch?: "true" | "false";
};

/**
 * An automation's spec: exactly one trigger with no edge in, every node
 * reached from it with no cycle, at most 50 nodes and 100 edges; a condition
 * has exactly one edge in.
 */
export type AutomationSpec = {
  v: 1;
  nodes: AutomationNode[];
  edges: AutomationEdge[];
};

export type AutomationStatus = "needs_approval" | "active" | "paused";
export type AutomationPausedReason =
  | "person"
  | "failures"
  | "owner_lost_access";
export type AutomationRunStatus =
  | "queued"
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "skipped";
export type AutomationNodeStatus =
  | "running"
  | "succeeded"
  | "failed"
  | "skipped"
  | "blocked";

/** What a step gave (outputJson): untrusted text from the workspace. */
export type AutomationNodeOutput = {
  summary?: string;
  // An Assistant step's yes (true) or no.
  answer?: boolean;
  count?: number;
  // The branch a condition took.
  branch?: boolean;
  // An action's result, as the MCP's tools return it.
  result?: unknown;
  // Actions an Assistant step left for the owner, as cards in the run's thread.
  pending?: number;
};

/** What fired a run (triggerEventJson). */
export type AutomationTriggerEvent = {
  kind: AutomationTrigger["kind"];
  at: string;
  sync_id?: string;
  load_id?: string;
  generation_id?: string;
  document_ids?: string[];
  outcome?: string;
  count?: number;
  rows?: number;
  by_user_id?: string;
  // The automations whose runs started the work that fired it, and those that
  // set them off; how many automation runs led to that work.
  caused_by_automations?: string[];
  chain_depth?: number;
};

/** v1FoundryAutomationRunNode: a step that started. */
export type FoundryAutomationRunNode = {
  nodeId: string;
  status?: AutomationNodeStatus;
  // outputJson and error: for its automation's owner, and owners and admins,
  // alone; another member sees how far the run went.
  outputJson?: string;
  error?: string;
  // An Assistant step's turn, in the run's thread (threadId): its owner's alone.
  turnId?: string;
  // int64.
  usdMicros?: string;
  startedAt?: string | null;
  finishedAt?: string | null;
};

/** v1FoundryAutomationRun */
export type FoundryAutomationRun = {
  id: string;
  automationId?: string;
  status?: AutomationRunStatus;
  // Why it failed, was cancelled or skipped, or why a queued one retries:
  // for its owner, and owners and admins, alone.
  error?: string;
  triggerEventJson?: string;
  // int64.
  specRevision?: string;
  // AutomationSpec as JSON, on a run got alone; none on a skipped run.
  specJson?: string;
  // The owner's Assistant thread its Assistant steps ran in: theirs alone.
  threadId?: string;
  // int64.
  usdMicros?: string;
  attempts?: number;
  createdAt?: string | null;
  startedAt?: string | null;
  finishedAt?: string | null;
  // Its steps that started, on a run got alone.
  nodes?: FoundryAutomationRunNode[];
  canCancel?: boolean;
  // The ontologies it used, by name, each with the workflow run that
  // records it and its verdict: on a run got alone.
  ontologies?: FoundryAutomationRunOntology[];
};

/**
 * v1FoundryAutomationRunOntology: an ontology one run of an automation used,
 * and how much it helped: the run of the automation's workflow of it.
 */
export type FoundryAutomationRunOntology = {
  ontologyId: string;
  ontologyName?: string;
  workflowId: string;
  workflowRunId: string;
  // open while the automation's run is live, then finished.
  status?: string;
  // queued, running, done, failed or skipped; missing before it is queued,
  // and for a run finished unassessed (cancelled).
  assessmentStatus?: string;
  // Once done: essential, helpful, marginal, unused or misleading.
  verdict?: string;
  // Finished without a read or question of the ontology: not assessed, so
  // no assessmentStatus or verdict.
  neverCalled?: boolean;
};

/**
 * v1FoundryAutomationOntology: an ontology an automation's runs use, the
 * workflow Foundry records them in (made at the first run that uses it, or
 * when its owner approves steps that name it), with its runs and verdicts,
 * or why Foundry records none (`untracked`).
 */
export type FoundryAutomationOntology = {
  ontologyId: string;
  ontologyName?: string;
  // Missing when untracked.
  workflowId?: string;
  // int64 as strings.
  runs?: string;
  finishedRuns?: string;
  // Of finishedRuns, those with no read or question of it: not assessed.
  neverCalledRuns?: string;
  lastRunAt?: string | null;
  verdicts?: FoundryWorkflowVerdictCount[];
  // What the ontology lacked, as the newest assessed run named it, at most
  // 5: on an automation got alone. Untrusted text.
  gaps?: string[];
  // workflows_full: the ontology holds 100 workflows, so Foundry couldn't
  // make the automation's.
  untracked?: "workflows_full";
  untrackedAt?: string | null;
};

/** v1FoundryAutomation */
export type FoundryAutomation = {
  id: string;
  name?: string;
  description?: string;
  // AutomationSpec as JSON; empty in a list.
  specJson?: string;
  // int64, from 1: what an edit or an approval names.
  revision?: string;
  // int64.
  specRevision?: string;
  ownerUserId?: string;
  status?: AutomationStatus;
  pausedReason?: AutomationPausedReason;
  pausedByUserId?: string;
  pausedAt?: string | null;
  // int64; missing or "0": never approved.
  approvedSpecRevision?: string;
  approvedAt?: string | null;
  triggerKind?: AutomationTrigger["kind"];
  triggerTarget?: string;
  // An active schedule's next time.
  nextRunAt?: string | null;
  failures?: number;
  backoffUntil?: string | null;
  lastRun?: FoundryAutomationRun | null;
  createdAt?: string | null;
  updatedAt?: string | null;
  canEdit?: boolean;
  canApprove?: boolean;
  canPause?: boolean;
  canResume?: boolean;
  canRun?: boolean;
  canDelete?: boolean;
  // When its newest failed run started: a list's only.
  lastFailedAt?: string | null;
  // Its runs of the days a list asked for (statsDays).
  stats?: AutomationRunCounts | null;
  // The ontologies its runs use, and how much each helped them.
  ontologies?: FoundryAutomationOntology[];
};

/**
 * v1FoundryAutomationRunCounts: runs started within some days, every one
 * (live, cancelled and skipped too), those that succeeded and failed, what
 * they spent (int64), and the actions their Assistant steps prepared that
 * still wait for the owner (counted for its owner, and owners and admins,
 * only; 0 for another member).
 */
export type AutomationRunCounts = {
  runs?: number;
  succeeded?: number;
  failed?: number;
  usdMicros?: string;
  waiting?: number;
};
/** v1FoundryAutomationRunDay: a day's runs in the list's time zone; other neither succeeded nor failed. */
export type AutomationRunDay = {
  // YYYY-MM-DD.
  date?: string;
  succeeded?: number;
  failed?: number;
  other?: number;
};
/** v1FoundryAutomationRunSummary: the workspace's runs of the last days, each day listed, oldest first. */
export type AutomationRunSummary = {
  days?: number;
  totals?: AutomationRunCounts;
  byDay?: AutomationRunDay[];
};

/**
 * GET .../automations[?statsDays=&timeZone=]: every one (at most 50), each
 * with its newest run and newest failure, without specs. statsDays (1 to
 * 30) counts each one's runs of that many days, today the last, by the days
 * of timeZone (IANA; UTC when empty), and sums them in summary.
 */
export type ListAutomationsResponse = {
  automations?: FoundryAutomation[];
  canCreate?: boolean;
  // Whether a notify step emails here (false on a local build).
  email?: boolean;
  summary?: AutomationRunSummary | null;
};
/** v1FoundryAutomationResponse: get, create, update, approve, pause and resume. */
export type AutomationResponse = { automation: FoundryAutomation };
/** POST .../automations: needs_approval; the same clientToken returns the one made. */
export type CreateAutomationRequest = {
  // 1 to 120 characters.
  name: string;
  // At most 1,000 characters.
  description?: string;
  // AutomationSpec as JSON.
  specJson: string;
  // 1 to 64 characters.
  clientToken: string;
};
/** PATCH .../automations/{id}: each field set is changed, at least one. */
export type UpdateAutomationRequest = {
  expectedRevision: string;
  name?: string;
  description?: string;
  specJson?: string;
};
/** POST .../automations/{id}:approve: its owner's click on the revision read. */
export type ApproveAutomationRequest = { expectedRevision: string };
/** POST .../automations/{id}:run: the same clientToken returns the run it queued. */
export type RunAutomationRequest = { clientToken: string };
/** GET .../automations/{automationId}/runs: newest first, 50 a page. */
export type ListAutomationRunsResponse = {
  runs?: FoundryAutomationRun[];
  nextPageToken?: string;
};
/** v1FoundryAutomationRunResponse: run, get run and cancel. */
export type AutomationRunResponse = { run: FoundryAutomationRun };

/** v1FoundryAutomationSuggestionIcon: the kind of thing a suggestion does. */
export type FoundryAutomationSuggestionIcon =
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_UNSPECIFIED"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_BRIEFING"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_WATCH"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_CHECK"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_ALERT"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_REFRESH"
  | "FOUNDRY_AUTOMATION_SUGGESTION_ICON_REPORT";
/**
 * v1FoundryAutomationSuggestionKind: what a suggestion asks Founder for: a
 * question about the data, a dashboard, a change to the ontology (a new
 * version) or a workflow. UNSPECIFIED (or missing) is a WFE before kinds,
 * whose suggestions are all workflows.
 */
export type FoundryAutomationSuggestionKind =
  | "FOUNDRY_AUTOMATION_SUGGESTION_KIND_UNSPECIFIED"
  | "FOUNDRY_AUTOMATION_SUGGESTION_KIND_QUESTION"
  | "FOUNDRY_AUTOMATION_SUGGESTION_KIND_DASHBOARD"
  | "FOUNDRY_AUTOMATION_SUGGESTION_KIND_ONTOLOGY"
  | "FOUNDRY_AUTOMATION_SUGGESTION_KIND_WORKFLOW";
/**
 * v1FoundryAutomationSuggestionSource: Founder wrote it (MODEL), or it was
 * made without a model from the version's competency questions
 * (QUESTIONS), from the ontology's setup (SETUP: its open audit findings,
 * and the connections and document sets it doesn't read yet), or is a
 * fixed example named for the ontology (FIXED).
 */
export type FoundryAutomationSuggestionSource =
  | "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_UNSPECIFIED"
  | "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_MODEL"
  | "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_QUESTIONS"
  | "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_SETUP"
  | "FOUNDRY_AUTOMATION_SUGGESTION_SOURCE_FIXED";
/**
 * v1FoundryAutomationSuggestion: a prompt for Founder of one kind, its
 * title an outcome (1 to 32 characters), its prompt what to ask (1 to 400;
 * a workflow's says when it starts and what it does), its topic what it is
 * about for a card's label (1 to 24: "Revenue", or the kind's own name),
 * its icon a workflow's only. A model may have written it: data, never
 * instructions; it only fills a composer.
 */
export type FoundryAutomationSuggestion = {
  title?: string;
  prompt?: string;
  icon?: FoundryAutomationSuggestionIcon;
  kind?: FoundryAutomationSuggestionKind;
  topic?: string;
  source?: FoundryAutomationSuggestionSource;
};
/**
 * GET .../foundry/automation-suggestions?ontologyId=: each kind's for the
 * ontology's current version (fixed examples without one), kinds in order:
 * 2 questions, 2 dashboards, 2 changes to the ontology and 4 workflows (none
 * where the deployment has no workflows), at most 10. `source` is the first
 * workflow's; `generating` while Founder writes them (these are the
 * fallback: read again).
 */
export type ListAutomationSuggestionsResponse = {
  suggestions?: FoundryAutomationSuggestion[];
  source?: FoundryAutomationSuggestionSource;
  generating?: boolean;
  versionId?: string;
};

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
      release?: never;
      reject?: never;
    }
  | { prop: FoundryPropValue; resolve?: never; release?: never; reject?: never }
  | {
      release: { statementHash: string };
      resolve?: never;
      prop?: never;
      reject?: never;
    }
  // A held claim a person says is wrong, bound to what they read as a release
  // is: nothing is written, and its card leaves Needs review.
  | {
      reject: { statementHash: string };
      resolve?: never;
      prop?: never;
      release?: never;
    };
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
    }
  // A person's word on one review of a document holding a claim back: by its
  // claim's key and its quote's place. Its `type` is always empty: the engine
  // reads what it states from the document.
  | {
      kind: "release";
      type: "";
      docId: string;
      claimKey: string;
      claimSection: number;
      claimStart: number;
      claimEnd: number;
      // What the person read, hashed (reviews/model.ts statementHash).
      statementHash: string;
    }
  // A person's word that one review's held claim is wrong, named as a release
  // names its review. No load is told: pages leave the claim's card out
  // (reviews/model.ts unrejected) until the decision is removed.
  | {
      kind: "reject";
      type: "";
      docId: string;
      claimKey: string;
      claimSection: number;
      claimStart: number;
      claimEnd: number;
      statementHash: string;
    }
  // An approval taken back from the current load after it may have written
  // it (contract 4.16.0): the release as it was sent, under its id. It is no
  // saved decision: the list shows it, with what became of it (`applied`),
  // while that load is current, and it can't be removed.
  | {
      kind: "withdraw";
      type: "";
      docId: string;
      claimKey: string;
      claimSection: number;
      claimStart: number;
      claimEnd: number;
      statementHash: string;
    };
/**
 * What became of a release, a resolve (contract 4.22.0: its word on its own
 * document; `refused` with `not_replayed` for a load read before names could
 * be settled in place, which the next load applies) or a prop decision in
 * the ontology's current load:
 * `pending` while a job has yet to apply it, `written` once a release's claim
 * is a fact, `refused` when it stays held (`reason` says why, in the engine's
 * word or the backend's, and may be one this site doesn't know), `missed`
 * when that load holds no review at its place, `applied` once the load was
 * sent a prop decision's value and the job that sent it succeeded (the
 * engine says nothing more of a value: a document dated after the decision
 * still beats it), `unconfirmed` when the job that applies it failed before
 * saying what became of it (it may or may not be written or sent, and no job
 * is coming; approving it again finishes it while its review is still in the
 * load, and if the claim was written there is no review left to approve,
 * only the fact; choosing a value again finishes a value, as the end of the
 * ontology's next job does), and empty when the load read it as it was
 * created and no job has said what became of it. A withdraw is `pending`
 * until the engine takes the approval back, then `withdrawn` (the claim is
 * held again, or, held for a conflict with a document since removed, stays
 * written as its document's own statement), `not_written` (the load held
 * nothing the approval wrote), `refused` (what it wrote stays: `differs`,
 * `too_large`, or a backend reason), or `unconfirmed`: no word came, with
 * `job_failed` (the ontology's next job sends it again), `document_failed`
 * (the next approval or take-back in its document does) or `not_replayed`
 * (its document's saved reading can't be reused: no word can come while the
 * load is current, and the next load won't write the approval).
 */
export type DecisionApplied = {
  status?:
    | "pending"
    | "written"
    | "refused"
    | "missed"
    | "applied"
    | "unconfirmed"
    | "withdrawn"
    | "not_written"
    | "";
  reason?: string;
  at?: string | null;
  // Of a release: removing it takes it back from the current load, which may
  // have written it (the backend's one rule); otherwise removing only drops it.
  takesBack?: boolean;
};
export type FoundryReviewDecision = ReviewDecision & {
  id: string;
  decidedByUserId?: string;
  decidedAt?: string | null;
  // Unset for a decision the current load's list doesn't hold.
  applied?: DecisionApplied | null;
  // How the review queue showed the card it was made on; unset (or empty)
  // for a decision made anywhere else.
  drawn?: ReviewDrawn | "";
};
/**
 * How the review queue showed a card: in its order of the facts each
 * decision writes, or drawn at random from a score band, which measures
 * precision.
 */
export type ReviewDrawn = "ranked" | "random";
export type SettleReviewRequest = {
  decision: FoundryReviewChoice;
  // A held claim's decision drawn at random is kept as a label of the claim
  // too, with the score it was read at.
  drawn?: ReviewDrawn;
};
export type ReviewDecisionResponse = { decision: FoundryReviewDecision };
// Making a version current drops the decisions it doesn't take, but keeps a
// resolve of a type it keeps whose entityKey its key no longer reads: the
// next load reviews that name again, and settling it replaces the decision.
export type ListReviewDecisionsResponse = {
  decisions?: FoundryReviewDecision[];
  nextPageToken?: string;
};
export type DeleteReviewDecisionResponse = Record<string, never>;
// POST and GET .../ontologies/{ontologyId}/review-labels: a person's word on
// a claim at its place, kept to measure precision and read into nothing
// else. A held claim's (claimKey and statementHash) is kept as its decision
// of the queue's random slice is saved; an accepted fact's quote (factKey)
// is labelled here. A re-check is a second person's word on a decision of a
// held claim or on someone else's label of a fact: `against` is the verdict
// it checks. One label per person and claim: saying it again replaces it.
export type ReviewVerdict = "right" | "wrong";
export type FoundryReviewLabel = {
  id: string;
  drawn: "random" | "recheck";
  docId: string;
  claimSection?: number;
  claimStart?: number;
  claimEnd?: number;
  claimKey?: string;
  statementHash?: string;
  factKey?: string;
  link?: string;
  // The verifier's score as the claim was read; unset when it had none.
  confidence?: number;
  verdict: ReviewVerdict;
  decisionId?: string;
  against?: ReviewVerdict;
  loadId?: string;
  decidedByUserId?: string;
  decidedAt?: string | null;
};
/** An accepted fact's quote in the current load, as its evidence holds it. */
export type LabelFact = {
  docId: string;
  section: number;
  start: number;
  end: number;
  factKey: string;
};
// A claim drawn at random (a fact's quote, or a held claim of the current
// load by its review), or a second person's re-check of a decision on a
// held claim or of another's label drawn at random.
export type SettleReviewLabelRequest = { verdict: ReviewVerdict } & (
  | { fact: LabelFact; drawn: "random" }
  | { reviewId: string; drawn: "random" }
  | { decisionId: string; drawn: "recheck" }
  | { labelId: string; drawn: "recheck" }
);
export type ReviewLabelResponse = { label: FoundryReviewLabel };
export type ListReviewLabelsResponse = {
  labels?: FoundryReviewLabel[];
  nextPageToken?: string;
};

// ---------------------------------------------------------------- Ontology: suggestions
// GET .../ontologies/{ontologyId}/heal-findings and POST
// .../heal-findings/{fingerprint}:dismiss: what audits of the ontology's
// current load found its ontology and mappings lack (self-healing), as the
// backend's foundry-heal-worker stored them. The list is the top 20 open
// ones by support; a dismissal holds until an audit finds twice the support,
// or a spelling it wasn't dismissed with.

/**
 * v1FoundryHealFinding. `findingJson` is the ontology agent's Finding
 * (audit_result.json `$defs/Finding`), its backing summed over the load:
 * parse it without trust (areas/suggestions/model.ts). `kind` and `status`
 * are the engine's and the backend's words; a later contract adds kinds.
 */
export type FoundryHealFinding = {
  fingerprint: string;
  // unmatched, unresolved, clash, row_errors, source_drift, or a later kind.
  kind: string;
  // open, dismissed or superseded.
  status: string;
  // int64 as strings: reviews (a row finding's rows), and documents.
  support?: string;
  sources?: string;
  loadId?: string;
  baseVersionId?: string;
  findingJson: string;
  dismissedByUserId?: string;
  supportAtDismissal?: string;
};
/** v1FoundryHealRun: one audit of a load. */
export type FoundryHealRun = {
  id: string;
  loadId?: string;
  // running, complete, incomplete (even a page of one document ran out of
  // time, the run did, or a graph job changed the load: nothing was stored,
  // and a later run audits again) or failed.
  status: string;
  pages?: number;
  cuts?: number;
  startedAt?: string;
  finishedAt?: string;
  // Whether it audited the load's rows: a complete run does when the load's
  // report holds them.
  rowsMeasured?: boolean;
};
export type ListHealFindingsResponse = {
  findings?: FoundryHealFinding[];
  lastRun?: FoundryHealRun;
  // The newest complete audit, of whichever load.
  lastCompleteRun?: FoundryHealRun;
  // Whether this Foundry audits loads (foundry.heal.enabled); false or unset,
  // no audit runs.
  enabled?: boolean;
};
export type HealFindingResponse = { finding: FoundryHealFinding };

// ---------------------------------------------------------------- Ontology: workflows
// .../ontologies/{ontologyId}/workflows: an agent's task over the ontology.
// Each run of one (started by the agent's MCP or HTTP calls, never by the
// site) records the agent's request (input), every read, question, send and
// upload it makes with the run's id, and its answer (output). The site
// creates, lists and deletes workflows and reads their runs.

/** v1FoundryWorkflow. */
export type FoundryWorkflow = {
  id: string;
  ontologyId?: string;
  name: string;
  description?: string;
  createdByUserId?: string;
  createdAt?: string;
  // int64 as strings: its runs kept (the newest 1,000), and of those the
  // finished ones.
  runs?: string;
  finishedRuns?: string;
  // When its newest run started; absent before any.
  lastRunAt?: string;
  // Its runs' assessments that are done, by verdict; a verdict no run has
  // is absent.
  verdicts?: FoundryWorkflowVerdictCount[];
  // The automation whose runs it records, for a workflow Foundry made (one
  // of each ontology an automation uses): only Foundry starts and finishes
  // its runs, and it is deleted only once the automation is. Missing for a
  // person's workflow.
  automation?: FoundryWorkflowAutomation | null;
};
/** v1FoundryWorkflowAutomation: the automation a workflow Foundry made records the runs of. */
export type FoundryWorkflowAutomation = {
  automationId: string;
  // Its name, as the workflow keeps it.
  name?: string;
  // Deleted since: the workflow keeps its runs as history.
  deleted?: boolean;
};
/** v1FoundryWorkflowVerdictCount. */
export type FoundryWorkflowVerdictCount = {
  verdict: string;
  // int64 as a string.
  runs?: string;
};
/**
 * v1FoundryWorkflowAssessment: a finished run's assessment by the ontology
 * agent's Assess (contract 4.15.0). `assessmentJson` is the engine's
 * Assessment (assess_result.json `$defs/Assessment`): parse it without trust
 * (areas/workflows/model.ts).
 */
export type FoundryWorkflowAssessment = {
  // queued, running, done, failed or skipped (the month's budget spent, or
  // a run queued before ones without a call went unassessed).
  status: string;
  // Once done: essential, helpful, marginal, unused or misleading.
  verdict?: string;
  assessmentJson?: string;
  // The calls whose results were left out to fit the model.
  leftOut?: number[];
  // Failed, skipped, or queued for a retry: why, for a person.
  error?: string;
  // int64 as a string, in millionths of a US dollar.
  usdMicros?: string;
  queuedAt?: string;
  finishedAt?: string;
};
/** v1FoundryWorkflowRunCounts: what a run recorded, at most 1,000 calls. */
export type FoundryWorkflowRunCounts = {
  accesses?: number;
  reads?: number;
  questions?: number;
  sends?: number;
  uploads?: number;
  // The pieces its sends carried.
  pieces?: number;
  // Reads stored without their result (over 256 KiB, or past the run's
  // 4 MiB).
  resultsOmitted?: number;
};
/** v1FoundryWorkflowRun. */
export type FoundryWorkflowRun = {
  id: string;
  workflowId?: string;
  // open (it records calls made with its id) or finished.
  status: string;
  // The request: empty until sent. The answer: empty while open.
  input?: string;
  output?: string;
  startedByUserId?: string;
  startedByServiceCredentialId?: string;
  startedAt?: string;
  finishedAt?: string;
  counts?: FoundryWorkflowRunCounts;
  // Once it finished, where assessments are on.
  assessment?: FoundryWorkflowAssessment;
  // The automation's run it records, for a run Foundry started (its
  // workflow's `automation` names the automation; started by its owner).
  automationRunId?: string;
};
/** v1FoundryWorkflowRead: a read of a load and what it returned. */
export type FoundryWorkflowRead = {
  loadId?: string;
  // The reads asked (read_request.json `reads` items), as sent.
  readsJson?: string;
  // The result returned (read_result.json); empty when left out.
  resultJson?: string;
  resultOmitted?: boolean;
};
/** v1FoundryWorkflowQuestion: its answer and steps are the question's. */
export type FoundryWorkflowQuestion = { loadId?: string; questionId?: string };
/** v1FoundryWorkflowSend: a send to an API source. */
export type FoundryWorkflowSend = {
  connectorId?: string;
  // int64 as a string: the send's number in its source.
  seq?: string;
  pieceIds?: string[];
};
/** v1FoundryWorkflowUpload: a document whose upload completed. */
export type FoundryWorkflowUpload = { documentId?: string; setId?: string };
/**
 * v1FoundryWorkflowInsightTotals: what a window's runs came to. `helped`
 * counts essential and helpful verdicts, `used` runs with a call to the
 * ontology; claims are done assessments'. Counts are int32 (numbers);
 * `assessmentUsdMicros` is int64 (a string).
 */
export type FoundryWorkflowInsightTotals = {
  runs?: number;
  open?: number;
  finished?: number;
  assessed?: number;
  helped?: number;
  used?: number;
  verdicts?: FoundryWorkflowVerdictCount[];
  claimsSupported?: number;
  claimsUnsupported?: number;
  claimsContradicted?: number;
  reads?: number;
  questions?: number;
  sends?: number;
  uploads?: number;
  assessmentUsdMicros?: string;
  gapKinds?: { kind?: string; runs?: number }[];
  // Finished runs that made no call to the ontology: counted here alone,
  // not in assessed, helped, verdicts, claims or gapKinds.
  neverCalled?: number;
};
/** v1FoundryWorkflowInsightDay: one UTC day, YYYY-MM-DD. */
export type FoundryWorkflowInsightDay = {
  date?: string;
  totals?: FoundryWorkflowInsightTotals;
};
/**
 * v1FoundryWorkflowInsightFunnel: each step the runs of the one before that
 * went on: started, finished, used (called the ontology), assessed, helped.
 */
export type FoundryWorkflowInsightFunnel = {
  started?: number;
  finished?: number;
  used?: number;
  assessed?: number;
  helped?: number;
};
/** v1FoundryWorkflowInsightIssue: a recurring gap (with its kind) or suggestion. */
export type FoundryWorkflowInsightIssue = {
  kind?: string;
  text?: string;
  runs?: number;
  lastRunId?: string;
  lastWorkflowId?: string;
  lastAt?: string;
  workflowIds?: string[];
};
/** v1FoundryWorkflowInsightSegment: one who started runs, or one workflow. */
export type FoundryWorkflowInsightSegment = {
  principal?: FoundryPrincipal;
  workflowId?: string;
  totals?: FoundryWorkflowInsightTotals;
  dailyRuns?: number[];
  lastRunAt?: string;
  topGap?: string;
  // Of who started runs: the automation Foundry started them for (its owner
  // is `principal`), counted apart from what its owner started themselves;
  // `workflowId` is its workflow, which filters to them.
  automation?: FoundryWorkflowAutomation | null;
};
/**
 * v1FoundryWorkflowInsights: GET .../ontologies/{ontologyId}/workflows:insights
 * ?days=&workflowId=&startedBy.kind=&startedBy.id=. The window's runs by the
 * day they started (`from`, YYYY-MM-DD, and `days`), the window before it,
 * the funnel, recurring gaps and suggestions, and the same by who started
 * them and by workflow (the latter only without `workflowId`).
 */
export type FoundryWorkflowInsights = {
  from?: string;
  days?: number;
  totals?: FoundryWorkflowInsightTotals;
  previous?: FoundryWorkflowInsightTotals;
  series?: FoundryWorkflowInsightDay[];
  funnel?: FoundryWorkflowInsightFunnel;
  gaps?: FoundryWorkflowInsightIssue[];
  suggestions?: FoundryWorkflowInsightIssue[];
  agents?: FoundryWorkflowInsightSegment[];
  workflows?: FoundryWorkflowInsightSegment[];
  truncated?: boolean;
};

/**
 * v1FoundryWorkflowAccess: one call a run recorded, by its place in the run;
 * exactly one of read, question, send and upload (the proto's oneof).
 */
export type FoundryWorkflowAccess = {
  seq: number;
  at?: string;
  read?: FoundryWorkflowRead;
  question?: FoundryWorkflowQuestion;
  send?: FoundryWorkflowSend;
  upload?: FoundryWorkflowUpload;
};
export type CreateFoundryWorkflowRequest = {
  name: string;
  description?: string;
  clientToken?: string;
};
export type FoundryWorkflowResponse = { workflow: FoundryWorkflow };
export type ListFoundryWorkflowsResponse = {
  workflows?: FoundryWorkflow[];
  nextPageToken?: string;
};
export type StartFoundryWorkflowRunRequest = {
  input?: string;
  clientToken?: string;
};
export type FinishFoundryWorkflowRunRequest = {
  input?: string;
  output: string;
};
export type FoundryWorkflowRunResponse = { run: FoundryWorkflowRun };
export type ListFoundryWorkflowRunsResponse = {
  runs?: FoundryWorkflowRun[];
  nextPageToken?: string;
};
export type ListFoundryWorkflowRunAccessesResponse = {
  accesses?: FoundryWorkflowAccess[];
  // Whether accesses follow the last one returned (after_seq pages them).
  more?: boolean;
};

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
  | "FOUNDRY_AI_KIND_OPENAI_COMPATIBLE"
  | "FOUNDRY_AI_KIND_COHERE";
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
  // Cohere's API, through its OpenAI-compatible Chat Completions.
  cohere?: Record<string, never>;
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
  // Whether the Assistant can run on it (its ask model's tool check, run once
  // the test passed while the Assistant is on). A failed check never fails
  // the setting; the Assistant refuses its turns, saying why.
  assistantCheck?: FoundryAiAssistantCheck;
};

/** v1FoundryAIAssistantCheck: what the Assistant's tool check of a setting found. */
export type FoundryAiAssistantCheck =
  // Not checked: the Assistant is off, the check couldn't run, or the test
  // hasn't passed. The Assistant runs on it.
  | "FOUNDRY_AI_ASSISTANT_CHECK_UNSPECIFIED"
  | "FOUNDRY_AI_ASSISTANT_CHECK_PASSED"
  // The provider refused the key on the Assistant's calls.
  | "FOUNDRY_AI_ASSISTANT_CHECK_AUTH"
  // The provider refused the request: a model it lacks there, or tools.
  | "FOUNDRY_AI_ASSISTANT_CHECK_REFUSED"
  // The model answered without calling the tool.
  | "FOUNDRY_AI_ASSISTANT_CHECK_NO_TOOL_CALL";

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

// ---------------------------------------------------------------- Access
// Routes under /v1/workspaces/{workspaceId}/foundry/access... (Foundry's
// source grants) and /v1/workspaces/{workspaceId}/service-credentials (the
// WFE's agents' keys). Owners and admins grant a principal (a member, a team
// or an agent's key) sources: a connector (its tables now and later, and the
// document sets it owns), one table, a document set, or People enrichment. A
// grantee reads the part of an ontology's current load its sources hold (all
// of it while every source that load holds is granted to it), and changes
// nothing: a restricted member (on every
// Foundry call while their role is member), a member without database query
// access on Ask's and the graph's reads, or an agent's key with grants.
// A workspace that grants nothing behaves as before. A member given sources,
// or on a team given any, is restricted to them, and stays so once they're
// taken away, until someone lifts it.

/** v1FoundryPrincipalKind */
export type FoundryPrincipalKind =
  | "KIND_UNSPECIFIED"
  | "KIND_USER"
  | "KIND_SERVICE_CREDENTIAL"
  | "KIND_TEAM";

/** v1FoundryPrincipal: who a grant is for. `id` is a user's, a key's or a team's. */
export type FoundryPrincipal = { kind?: FoundryPrincipalKind; id?: string };

/** v1FoundryAccessTargetKind */
export type FoundryAccessTargetKind =
  | "KIND_UNSPECIFIED"
  | "KIND_CONNECTOR"
  | "KIND_TABLE"
  | "KIND_DOCUMENT_SET"
  | "KIND_ENRICHMENT";

/**
 * v1FoundryAccessTarget: what one grant gives. A connector names
 * `connectorId`, a table `connectorId` and `table`, a set `documentSetId`,
 * and People enrichment nothing.
 */
export type FoundryAccessTarget = {
  kind?: FoundryAccessTargetKind;
  connectorId?: string;
  table?: string;
  documentSetId?: string;
};

/** v1FoundryAccessGrant */
export type FoundryAccessGrant = {
  id?: string;
  principal?: FoundryPrincipal;
  target?: FoundryAccessTarget;
  createdByUserId?: string;
  createdAt?: string;
};

/** FoundryLoadAccessInputsTable */
export type FoundryLoadAccessTable = { connectorId?: string; table?: string };
/**
 * FoundryLoadAccessInputsDocumentSet: a set the load's documents, table
 * files or review decisions' documents are in now, with the connector that
 * owns it, if one does. An empty `documentSetId` is a document in no set,
 * which nothing covers.
 */
export type FoundryLoadAccessSet = {
  documentSetId?: string;
  connectorId?: string;
};

/**
 * v1FoundryLoadAccessInputs: what a load holds, as far as grants go.
 * `uncoverable`: it was made before grants, or applies a review decision
 * whose documents weren't recorded; loading again makes it shareable.
 */
export type FoundryLoadAccessInputs = {
  tables?: FoundryLoadAccessTable[];
  documentSets?: FoundryLoadAccessSet[];
  enrichment?: boolean;
  uncoverable?: boolean;
};

/**
 * v1FoundryOntologyAccess: a live ontology's current load as grants see it,
 * and the principals whose own grants cover it (a team's members read it
 * through the team; they aren't listed).
 */
export type FoundryOntologyAccess = {
  ontologyId?: string;
  loadId?: string;
  inputs?: FoundryLoadAccessInputs;
  readers?: FoundryPrincipal[];
  // Those whose own grants hold only some of it, who read that part.
  partialReaders?: FoundryPrincipal[];
};

/**
 * v1FoundryTeam: members hold the team's grants. Names are unique, 1 to 120
 * characters. `directory` is set on a team synced from a directory group:
 * its name and members are the group's, and only its grants change here.
 */
export type FoundryTeam = {
  id?: string;
  name?: string;
  memberUserIds?: string[];
  createdByUserId?: string;
  createdAt?: string;
  directory?: FoundryTeamDirectory;
};

/**
 * v1FoundryTeamDirectory: the directory group a synced team follows.
 * `provider` names the directory as people know it ("Okta"); `leftOutUserIds`
 * are the group's members the team leaves out while it holds grants (plain
 * members with raw access, whom its grants can't confine).
 */
export type FoundryTeamDirectory = {
  directoryId?: string;
  groupId?: string;
  provider?: string;
  leftOutUserIds?: string[];
};

/**
 * v1ListFoundryAccessResponse: GET .../foundry/access, owners and admins.
 * Grants oldest first; ontologies those with a current load; teams by name.
 */
export type ListFoundryAccessResponse = {
  grants?: FoundryAccessGrant[];
  ontologies?: FoundryOntologyAccess[];
  teams?: FoundryTeam[];
  restrictedUserIds?: string[];
  // The principals whose Usage permission is off; every other one has it
  // on. A member is refused it while they or any of their teams has it off.
  workflowsOff?: FoundryPrincipal[];
};

/**
 * FoundryServiceSetFoundryAccessGrantsBody: PUT .../foundry/access/grants
 * replaces the principal's grants (none takes them all away); a connector or
 * set not in the workspace is NOT_FOUND. `workflows` sets its Usage
 * permission (left out, it stays as it is: on unless turned off): the
 * use cases of the ontologies it reads, its own runs, and every run of one it
 * reads all of. Answers the principal's grants and permission now.
 */
export type SetFoundryAccessGrantsRequest = {
  principal: FoundryPrincipal;
  targets: FoundryAccessTarget[];
  workflows?: boolean;
};
/** v1SetFoundryAccessGrantsResponse */
export type SetFoundryAccessGrantsResponse = {
  grants?: FoundryAccessGrant[];
  workflows?: boolean;
};

/**
 * v1FoundryAccessChange: one change the site warns about, exactly one of a
 * load of a version (or making it current), deleting a connector, or moving
 * a document to another set.
 */
export type FoundryAccessChange =
  | { loadVersion: { ontologyId: string; versionId: string } }
  | { deleteConnector: string }
  | { moveDocument: { documentId: string; documentSetId: string } };

/** FoundryServicePreviewFoundryAccessChangeBody: POST .../foundry/access:preview */
export type PreviewFoundryAccessChangeRequest = { change: FoundryAccessChange };
/** v1FoundryAccessLoss: a principal that would stop reading an ontology through grants. */
export type FoundryAccessLoss = {
  principal?: FoundryPrincipal;
  ontologyId?: string;
  // It would still read the part its grants hold.
  keepsPart?: boolean;
};
/** v1PreviewFoundryAccessChangeResponse */
export type PreviewFoundryAccessChangeResponse = {
  losses?: FoundryAccessLoss[];
};

/**
 * v1GetMyFoundryAccessResponse: GET .../foundry/access/me, any member.
 * `grantee`: this caller reads only through grants (a restricted member);
 * `ontologyIds`: the live ontologies whose current loads its grants hold any
 * of, `partialOntologyIds` those of them it holds only some of; `workflows`:
 * it may use those ontologies' use cases (always, for a caller that isn't a
 * grantee), seeing only the runs it started of one it holds only some of.
 */
export type GetMyFoundryAccessResponse = {
  grantee?: boolean;
  hasGrants?: boolean;
  ontologyIds?: string[];
  partialOntologyIds?: string[];
  workflows?: boolean;
};

/**
 * FoundryServiceSetFoundryMemberRestrictedBody: PUT
 * .../foundry/access/members/{userId}. Only a plain member without project
 * write or query access can be restricted (FAILED_PRECONDITION otherwise).
 */
export type SetFoundryMemberRestrictedRequest = { restricted: boolean };
/** v1SetFoundryMemberRestrictedResponse */
export type SetFoundryMemberRestrictedResponse = { restricted?: boolean };

/**
 * FoundryServiceCreateFoundryTeamBody and FoundryServiceUpdateFoundryTeamBody:
 * POST .../foundry/access/teams, PUT .../teams/{teamId} (renames and
 * replaces its members). A taken name is ALREADY_EXISTS; at most 100 teams
 * and 1,000 members a team.
 */
export type FoundryTeamRequest = { name: string; memberUserIds: string[] };
/** v1FoundryTeamResponse */
export type FoundryTeamResponse = { team?: FoundryTeam };

/** v1ServiceCredentialPermission: a key's project grant. */
export type ServiceCredentialPermission =
  | "SERVICE_CREDENTIAL_PERMISSION_UNSPECIFIED"
  | "SERVICE_CREDENTIAL_PERMISSION_PROJECT_READ"
  | "SERVICE_CREDENTIAL_PERMISSION_PROJECT_WRITE"
  | "SERVICE_CREDENTIAL_PERMISSION_DATABASE_QUERY_READ"
  | "SERVICE_CREDENTIAL_PERMISSION_DATABASE_QUERY_WRITE";

/** v1ServiceCredentialGrant: what a key may do in one project. */
export type ServiceCredentialGrant = {
  projectId?: string;
  permissions?: ServiceCredentialPermission[];
};

/**
 * v1ServiceCredential: a workspace's key, an agent's. Its token is shown
 * once, when it is made; `tokenPrefix` names it after. `legacyReportOwner`
 * is left out: Foundry lists workspace keys only.
 */
export type ServiceCredential = {
  id: string;
  name?: string;
  tokenPrefix?: string;
  workspaceOwner?: { workspaceId?: string };
  grants?: ServiceCredentialGrant[];
  createdBy?: string;
  expiresAt?: string | null;
  revokedAt?: string | null;
  lastUsedAt?: string | null;
  timestamps?: Timestamps;
};

/** helixwfev1ListServiceCredentialsResponse: GET /v1/workspaces/{workspaceId}/service-credentials */
export type ListServiceCredentialsResponse = {
  credentials?: ServiceCredential[];
};

/**
 * v1WfeServiceCreateServiceCredentialBody: POST
 * /v1/workspaces/{workspaceId}/service-credentials. Foundry makes keys with
 * no project grants: they reach Foundry only through source grants.
 */
export type CreateServiceCredentialRequest = {
  name: string;
  grants?: ServiceCredentialGrant[];
};
/** helixwfev1CreateServiceCredentialResponse: `token` is shown once. */
export type CreateServiceCredentialResponse = {
  credential?: ServiceCredential;
  token?: string;
};
