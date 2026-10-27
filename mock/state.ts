import type { Workspace, WorkspaceRole } from "../src/shared/wfe";
import { fail, type MockReply } from "./http";

// The mock's in-memory state: users, WorkOS sessions, workspaces and
// memberships. Areas keep their own state in their route module and register
// a reset with onReset(), so POST /__mock/reset clears everything.

export type MockUser = {
  // Helix user ID (WhoamiResponse.user.id).
  id: string;
  // WorkOS user ID (the access token's `sub`).
  workosId: string;
  email: string;
  firstName: string;
  lastName: string;
  createdAt: string;
};
export type Membership = {
  id: string;
  workspaceId: string;
  userId: string;
  role: WorkspaceRole;
  createdAt: string;
};
// A WorkOS session: its current refresh token rotates on every refresh.
export type MockSession = { id: string; userId: string; refreshToken: string };
// A sign-in between login:start and exchange.
export type PendingLogin = {
  sessionId: string;
  state: string;
  redirectUri: string;
  provider: string;
  code?: string;
  userId?: string;
};

export const db = {
  users: new Map<string, MockUser>(),
  sessions: new Map<string, MockSession>(),
  logins: new Map<string, PendingLogin>(),
  workspaces: new Map<string, Workspace>(),
  memberships: [] as Membership[],
};

// Counters the e2e suite reads through GET /__mock/stats.
export const stats = {
  loginStarts: 0,
  refreshes: 0,
  // Every /v1 request that reached the mock: "GET /v1/whoami 200".
  requests: [] as string[],
};

// MOCK_LOCAL_TOKEN makes this mock a local build's API
// (src/shared/deployment.ts): that bearer token is Ada, access tokens are
// refused, and every workspace has every feature, as with no billing.
export const LOCAL_TOKEN = process.env.MOCK_LOCAL_TOKEN || null;

// MOCK_STAMP=prod (pnpm docs:media) serves prod's stamp where it differs from
// dev's: its workspace region and the monthly caps (prod's configuration),
// as the docs describe them. Unset, the mock is dev's.
export const PROD_STAMP = process.env.MOCK_STAMP === "prod";
const REGION = PROD_STAMP ? "us-east-1" : "us-east-2";

// Settings the e2e suite changes through POST /__mock/config.
export const config = {
  // A local API refuses the site's token, as when the two were given
  // different ones.
  refuseLocalToken: false,
  accessTokenTtlSeconds: 300,
  allowedRegions: [REGION],
  // Foundry Pro and Max checkout charges a card on file instead of opening
  // Checkout.
  checkoutCardOnFile: false,
  // The WFE's auth.allowed_emails: exact addresses and "@domain" rules. Empty
  // lets everyone sign in.
  allowedEmails: [] as string[],
  // A local install as it first starts: Ada's one workspace is "My
  // workspace", with nothing in it, in place of Acme (applyConfig), as the
  // local API seeds it. A reset brings Acme back.
  freshInstall: false,
};

const resets: (() => void)[] = [];
/** Registers an area's reset; it runs on every reset, after the seed. */
export function onReset(reset: () => void) {
  resets.push(reset);
}

// Ada already has a workspace, so dev:mock opens straight into the shell.
// Anyone else signing in starts with none.
export const SEED_EMAIL = "ada@example.com";
const SEED_TIME = "2026-10-01T09:00:00.000Z";

export function resetState() {
  db.users.clear();
  db.sessions.clear();
  db.logins.clear();
  db.workspaces.clear();
  db.memberships.length = 0;
  stats.loginStarts = 0;
  stats.refreshes = 0;
  stats.requests.length = 0;
  config.refuseLocalToken = false;
  config.accessTokenTtlSeconds = 300;
  // A local API's one region (next.config.ts).
  config.allowedRegions = LOCAL_TOKEN ? ["local"] : [REGION];
  config.checkoutCardOnFile = false;
  config.allowedEmails = [];
  config.freshInstall = false;
  const ada: MockUser = {
    id: "usr_ada",
    workosId: "user_01ADA",
    email: SEED_EMAIL,
    firstName: "Ada",
    lastName: "Lovelace",
    createdAt: SEED_TIME,
  };
  db.users.set(ada.id, ada);
  db.workspaces.set("ws_acme", {
    id: "ws_acme",
    slug: "acme",
    displayName: "Acme",
    region: REGION,
    status: "RESOURCE_STATUS_ACTIVE",
    timestamps: { createdAt: SEED_TIME, updatedAt: SEED_TIME },
  });
  db.memberships.push({
    id: "mem_ada_acme",
    workspaceId: "ws_acme",
    userId: ada.id,
    role: "WORKSPACE_ROLE_OWNER",
    createdAt: SEED_TIME,
  });
  for (const reset of resets) reset();
}

/** What a POST /__mock/config does beyond setting its knobs. */
export function applyConfig() {
  if (!config.freshInstall || !db.workspaces.has("ws_acme")) return;
  db.workspaces.delete("ws_acme");
  const kept = db.memberships.filter((m) => m.workspaceId !== "ws_acme");
  db.memberships.splice(0, db.memberships.length, ...kept);
  const at = new Date().toISOString();
  db.workspaces.set("ws_my", {
    id: "ws_my",
    slug: "my-workspace",
    displayName: "My workspace",
    region: config.allowedRegions[0],
    status: "RESOURCE_STATUS_ACTIVE",
    timestamps: { createdAt: at, updatedAt: at },
  });
  db.memberships.push({
    id: "mem_ada_my",
    workspaceId: "ws_my",
    userId: "usr_ada",
    role: "WORKSPACE_ROLE_OWNER",
    createdAt: at,
  });
}

export const displayName = (u: MockUser) =>
  [u.firstName, u.lastName].filter(Boolean).join(" ") || u.email;

/**
 * The caller's membership of a workspace, or the WFE's refusal. Area routes
 * under /v1/workspaces/{workspaceId}/... start with this.
 */
export function membership(
  workspaceId: string,
  userId: string,
): Membership | MockReply {
  const m = db.memberships.find(
    (x) => x.workspaceId === workspaceId && x.userId === userId,
  );
  return m && db.workspaces.has(workspaceId)
    ? m
    : fail("PERMISSION_DENIED", "permission denied");
}
export const isReply = (x: Membership | MockReply): x is MockReply =>
  "status" in x;
