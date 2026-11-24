import { createHash, randomBytes } from "node:crypto";
import type {
  ApprovePartnerAppAuthorizationRequest,
  ApprovePartnerAppAuthorizationResponse,
  FoundryAccessTarget,
  GetPartnerAppAuthorizationResponse,
  ListPartnerAppInstallsResponse,
  PartnerAppInstall,
  PublicPartnerApp,
} from "../../src/shared/wfe";
import { fail, id, now, ok, type MockReply, type MockRoute } from "../http";
import {
  config,
  db,
  isReply,
  LOCAL_TOKEN,
  membership,
  onReset,
} from "../state";
import {
  makeAppKey,
  onKeyRevoked,
  parseTarget,
  revokeKey,
  setAppKeyGrants,
  sourcesLive,
} from "./access";
import { manages } from "./workspaces";

// Connected apps as hosted Helix serves them (the WFE's partner_apps.go and
// the backend's provider), in memory. Helix registered three apps:
// Northwind Insights (verified, read and ask), Contoso Sync (unverified,
// read only) and Fabrikam Legacy (disabled). An owner or admin approves an
// app's request on the site's /oauth/authorize (GET
// /v1/partner-apps/authorization, then POST
// .../partner-apps/authorization:approve: a one-time code, five minutes),
// and lists and revokes the workspace's installs. The app's token call is
// POST /__mock/partner-apps/exchange { clientId, clientSecret, code,
// codeVerifier, redirectUri? } (partnerExchange), under the backend's rules:
// the client is authenticated before anything is written, a code is spent
// once (presented again, it revokes the token family it made), its PKCE
// verifier must match, its approver must still own or administer the
// workspace, and every source must still be live. It makes the install (a
// key of Settings › Access, "hlxs_partner", that reads only its source
// grants, with Usage off) or updates the live one. POST
// /__mock/partner-apps/use { installId } is the app reading through it.
// Revoking the install, or its key anywhere, ends it, its tokens and its
// unused codes.
//
// Knobs, through POST /__mock/config: `partnerAppsAvailable: false` (Helix
// offers no connected apps: FAILED_PRECONDITION) and `partnerAppInstalled:
// true` (Acme already has Northwind reading the Stripe connector).

type Knobs = { partnerAppsAvailable?: boolean; partnerAppInstalled?: boolean };
const knobs = config as typeof config & Knobs;

const SCOPES = ["foundry.read", "foundry.ask"] as const;
type Scope = (typeof SCOPES)[number];

type App = PublicPartnerApp & {
  secret: string;
  redirectUris: string[];
  scopes: Scope[];
  disabled: boolean;
};
const clientId = (name: string) => "hlxpc_" + name.padEnd(43, "0").slice(0, 43);

/** The apps Helix registered, by name. */
export const PARTNER_APPS = {
  northwind: {
    clientId: clientId("northwind-insights"),
    secret: "hlxpk_northwind-secret",
    name: "Northwind Insights",
    publisher: "Northwind Labs",
    homepageUrl: "https://northwind.example",
    verified: true,
    redirectUris: ["https://northwind.example/helix/callback"],
    scopes: ["foundry.read", "foundry.ask"],
    disabled: false,
  },
  contoso: {
    clientId: clientId("contoso-sync"),
    secret: "hlxpk_contoso-secret",
    name: "Contoso Sync",
    publisher: "Contoso",
    homepageUrl: "https://contoso.example",
    verified: false,
    redirectUris: ["https://contoso.example/cb"],
    scopes: ["foundry.read"],
    disabled: false,
  },
  fabrikam: {
    clientId: clientId("fabrikam-legacy"),
    secret: "hlxpk_fabrikam-secret",
    name: "Fabrikam Legacy",
    publisher: "Fabrikam",
    homepageUrl: "https://fabrikam.example",
    verified: true,
    redirectUris: ["https://fabrikam.example/cb"],
    scopes: ["foundry.read"],
    disabled: true,
  },
} satisfies Record<string, App>;
const apps: App[] = Object.values(PARTNER_APPS);
/** The authorization server's issuer, as partneroauth.Issuer names dev's. */
export const PARTNER_ISSUER = "https://mcp.dev.helix-db.com/apps";

const CODE_TTL_MS = 5 * 60_000,
  MAX_STATE = 512,
  MAX_SOURCES = 1000,
  MAX_LIVE_GRANTS = 20;

type Code = {
  clientId: string;
  workspaceId: string;
  userId: string;
  redirectUri: string;
  challenge: string;
  scopes: Scope[];
  targets: FoundryAccessTarget[];
  expires: number;
  used: boolean;
  // The token family its exchange made.
  grantId: string | null;
};
type Install = {
  id: string;
  clientId: string;
  workspaceId: string;
  credentialId: string;
  scopes: Scope[];
  approvedByUserId: string;
  approvedAt: string;
  createdAt: string;
  lastUsedAt: string | null;
  revokedAt: string | null;
};
/** A token family: one exchange and the refreshes after it. */
type Family = {
  id: string;
  installId: string;
  createdAt: number;
  revokedReason: string | null;
};

const state = {
  codes: new Map<string, Code>(),
  installs: [] as Install[],
  families: [] as Family[],
  seeded: false,
};
onReset(() => {
  state.codes.clear();
  state.installs = [];
  state.families = [];
  state.seeded = false;
  delete knobs.partnerAppsAvailable;
  delete knobs.partnerAppInstalled;
});

const NOT_OFFERED = "Connected apps aren't offered here";
const offered = () => !LOCAL_TOKEN && knobs.partnerAppsAvailable !== false;
const appOf = (clientId: unknown) =>
  apps.find((a) => a.clientId === clientId && !a.disabled);
const publicOf = (a: App): PublicPartnerApp => ({
  clientId: a.clientId,
  name: a.name,
  publisher: a.publisher,
  homepageUrl: a.homepageUrl,
  verified: a.verified,
});
const sha256url = (s: string) =>
  createHash("sha256").update(s).digest("base64url");
const secret = (prefix: string) =>
  prefix + randomBytes(32).toString("base64url");

/** The scopes `scope` names (space-delimited; empty for all the app's), or why not. */
function scopesOf(app: App, scope: unknown): Scope[] | string {
  const named =
    typeof scope === "string" ? scope.split(" ").filter(Boolean) : [];
  if (!named.length) return [...app.scopes];
  for (const s of named)
    if (!app.scopes.includes(s as Scope))
      return `scope ${s} isn't one this app may ask for`;
  return SCOPES.filter((s) => named.includes(s));
}

/** Where the browser goes back to: the redirect URI with `params` and iss. */
function back(redirectUri: string, params: Record<string, string>) {
  const url = new URL(redirectUri);
  for (const [k, v] of Object.entries(params)) url.searchParams.set(k, v);
  url.searchParams.set("iss", PARTNER_ISSUER);
  return url.toString();
}

/** The app and redirect URI a request names, or the WFE's refusal. */
function checkRequest(clientId: unknown, redirectUri: unknown) {
  const app = appOf(clientId);
  if (!app)
    return fail("INVALID_ARGUMENT", "This app isn't registered with Helix");
  if (
    typeof redirectUri !== "string" ||
    !app.redirectUris.includes(redirectUri)
  )
    return fail(
      "INVALID_ARGUMENT",
      "redirect_uri is not registered for this app",
    );
  return app;
}

const refused = (x: App | MockReply): x is MockReply => "status" in x;

/** The live install of `clientId` in the workspace, if there is one. */
const liveInstall = (workspaceId: string, clientId: string) =>
  state.installs.find(
    (i) =>
      i.workspaceId === workspaceId && i.clientId === clientId && !i.revokedAt,
  );

/** Revokes a family, its tokens with it. */
function revokeFamily(f: Family, reason: string) {
  f.revokedReason ??= reason;
}

/**
 * Ends an install, as RevokePartnerAppInstall and the key's revoke do:
 * its families, and every unused code of its app for the workspace.
 */
function endInstall(i: Install) {
  i.revokedAt = now();
  for (const f of state.families)
    if (f.installId === i.id) revokeFamily(f, "install_revoked");
  for (const c of state.codes.values())
    if (c.clientId === i.clientId && c.workspaceId === i.workspaceId)
      c.used = true;
}
onKeyRevoked((workspaceId, credentialId) => {
  const i = state.installs.find(
    (x) =>
      x.workspaceId === workspaceId &&
      x.credentialId === credentialId &&
      !x.revokedAt,
  );
  if (i) endInstall(i);
});

/** `partnerAppInstalled`: Acme's Northwind install reading Stripe, once. */
function seedIfAsked() {
  if (
    !knobs.partnerAppInstalled ||
    state.seeded ||
    !db.workspaces.has("ws_acme")
  )
    return;
  state.seeded = true;
  const app = PARTNER_APPS.northwind,
    at = new Date(Date.now() - 3 * 86_400_000).toISOString();
  const credentialId = makeAppKey("ws_acme", app.name, "usr_ada");
  setAppKeyGrants(
    "ws_acme",
    credentialId,
    [{ kind: "KIND_CONNECTOR", connectorId: "conn_acme_stripe" }],
    "usr_ada",
  );
  state.installs.push({
    id: "pai_acme_northwind",
    clientId: app.clientId,
    workspaceId: "ws_acme",
    credentialId,
    scopes: [...app.scopes],
    approvedByUserId: "usr_ada",
    approvedAt: at,
    createdAt: at,
    lastUsedAt: new Date(Date.now() - 2 * 3_600_000).toISOString(),
    revokedAt: null,
  });
}

/** The caller's membership when it owns or administers the workspace, where offered. */
function manager(workspaceId: string, userId: string) {
  const m = membership(workspaceId, userId);
  if (isReply(m)) return m;
  if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
  if (!offered()) return fail("FAILED_PRECONDITION", NOT_OFFERED);
  seedIfAsked();
  return m;
}

const viewOf = (i: Install): PartnerAppInstall => {
  const app = apps.find((a) => a.clientId === i.clientId)!;
  return {
    id: i.id,
    app: publicOf(app),
    credentialId: i.credentialId,
    scopes: [...i.scopes],
    approvedByUserId: i.approvedByUserId,
    createdAt: i.createdAt,
    approvedAt: i.approvedAt,
    ...(i.lastUsedAt && { lastUsedAt: i.lastUsedAt }),
    ...(app.disabled && { appDisabled: true }),
  };
};

export const partnerAppRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/partner-apps/authorization",
    handle: ({ query }) => {
      if (!offered()) return fail("FAILED_PRECONDITION", NOT_OFFERED);
      const app = checkRequest(query.get("clientId"), query.get("redirectUri"));
      if (refused(app)) return app;
      const scopes = scopesOf(app, query.get("scope"));
      if (typeof scopes === "string") return fail("INVALID_ARGUMENT", scopes);
      // The state is optional here: the site keeps it in the browser.
      const s = query.get("state") || "";
      if (s.length > MAX_STATE)
        return fail("INVALID_ARGUMENT", "state is too long");
      const redirectUri = query.get("redirectUri")!;
      return ok({
        app: publicOf(app),
        scopes,
        redirectOrigin: new URL(redirectUri).origin,
        denyUrl: back(redirectUri, {
          error: "access_denied",
          ...(s && { state: s }),
        }),
      } satisfies GetPartnerAppAuthorizationResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/partner-apps/authorization:approve",
    handle: ({ params, body, principal: { user } }) => {
      const m = manager(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const req = (body ||
        {}) as Partial<ApprovePartnerAppAuthorizationRequest>;
      const app = checkRequest(req.clientId, req.redirectUri);
      if (refused(app)) return app;
      if (
        typeof req.state !== "string" ||
        !req.state ||
        req.state.length > MAX_STATE
      )
        return fail("INVALID_ARGUMENT", "state is 1 to 512 characters");
      if (
        typeof req.codeChallenge !== "string" ||
        !/^[A-Za-z0-9_-]{43}$/.test(req.codeChallenge)
      )
        return fail(
          "INVALID_ARGUMENT",
          "code_challenge is an S256 PKCE challenge",
        );
      const asked = Array.isArray(req.scopes) ? req.scopes : [];
      if (!asked.length || asked.some((s) => !app.scopes.includes(s as Scope)))
        return fail(
          "INVALID_ARGUMENT",
          "scopes are some of what this app may ask for",
        );
      const targets: FoundryAccessTarget[] = [];
      for (const t of req.targets || []) {
        const parsed = parseTarget(t);
        if (typeof parsed === "string") return fail("INVALID_ARGUMENT", parsed);
        if (!targets.some((x) => JSON.stringify(x) === JSON.stringify(parsed)))
          targets.push(parsed);
      }
      if (!targets.length || targets.length > MAX_SOURCES)
        return fail("INVALID_ARGUMENT", "choose 1 to 1000 sources");
      if (!sourcesLive(m.workspaceId, targets))
        return fail("NOT_FOUND", "One of the sources you chose was deleted");
      const code = secret("hlxpa_");
      state.codes.set(code, {
        clientId: app.clientId,
        workspaceId: m.workspaceId,
        userId: user.id,
        redirectUri: req.redirectUri!,
        challenge: req.codeChallenge,
        scopes: SCOPES.filter((s) => asked.includes(s)),
        targets,
        expires: Date.now() + CODE_TTL_MS,
        used: false,
        grantId: null,
      });
      return ok({
        redirectUrl: back(req.redirectUri!, { code, state: req.state }),
      } satisfies ApprovePartnerAppAuthorizationResponse);
    },
  },
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/partner-apps/installs",
    handle: ({ params, principal: { user } }) => {
      const m = manager(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const used = (i: Install) => i.lastUsedAt || i.approvedAt;
      return ok({
        installs: state.installs
          .filter((i) => i.workspaceId === m.workspaceId && !i.revokedAt)
          .sort((a, b) => used(b).localeCompare(used(a)))
          .map(viewOf),
      } satisfies ListPartnerAppInstallsResponse);
    },
  },
  {
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/partner-apps/installs/{installId}",
    handle: ({ params, principal: { user } }) => {
      const m = manager(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const i = state.installs.find(
        (x) =>
          x.id === params.installId &&
          x.workspaceId === m.workspaceId &&
          !x.revokedAt,
      );
      if (!i) return fail("NOT_FOUND", "no such connected app");
      // Revoking its key ends it (onKeyRevoked).
      revokeKey(m.workspaceId, i.credentialId);
      if (!i.revokedAt) endInstall(i);
      return ok({});
    },
  },
];

const oauthError = (status: number, error: string): MockReply => ({
  status,
  body: { error },
});

/**
 * The app's token call for a code (POST /__mock/partner-apps/exchange), as
 * the backend's ExchangePartnerOAuthCode answers: tokens, or an OAuth
 * error. Its client is authenticated before anything is read or written.
 */
export function exchangePartnerCode(body: {
  clientId?: string;
  clientSecret?: string;
  code?: string;
  codeVerifier?: string;
  redirectUri?: string;
}): MockReply {
  const app = apps.find((a) => a.clientId === body.clientId);
  if (!app || app.disabled || app.secret !== body.clientSecret)
    return oauthError(401, "invalid_client");
  const invalid = oauthError(400, "invalid_grant");
  if (!body.code?.startsWith("hlxpa_")) return invalid;
  const c = state.codes.get(body.code);
  // Another client's code: refused, and nothing written.
  if (!c || c.clientId !== app.clientId) return invalid;
  if (c.used) {
    const f = state.families.find((x) => x.id === c.grantId);
    if (f) revokeFamily(f, "code_reuse");
    return invalid;
  }
  const approver = db.memberships.find(
    (x) => x.workspaceId === c.workspaceId && x.userId === c.userId,
  );
  if (
    c.expires < Date.now() ||
    (body.redirectUri !== undefined && body.redirectUri !== c.redirectUri) ||
    !body.codeVerifier ||
    sha256url(body.codeVerifier) !== c.challenge ||
    !approver ||
    !manages(approver) ||
    !db.workspaces.has(c.workspaceId)
  ) {
    c.used = true;
    return invalid;
  }
  // A source deleted since consent: nothing is written.
  if (!sourcesLive(c.workspaceId, c.targets)) return invalid;
  const scopes = c.scopes.filter((s) => app.scopes.includes(s));
  if (!scopes.length) return oauthError(400, "invalid_scope");
  const at = now();
  let install = liveInstall(c.workspaceId, app.clientId);
  if (install) {
    install.scopes = scopes;
    install.approvedByUserId = c.userId;
    install.approvedAt = at;
  } else {
    install = {
      id: id("pai"),
      clientId: app.clientId,
      workspaceId: c.workspaceId,
      credentialId: makeAppKey(c.workspaceId, app.name, c.userId),
      scopes,
      approvedByUserId: c.userId,
      approvedAt: at,
      createdAt: at,
      lastUsedAt: null,
      revokedAt: null,
    };
    state.installs.push(install);
  }
  setAppKeyGrants(c.workspaceId, install.credentialId, c.targets, c.userId);
  const live = state.families.filter(
    (f) => f.installId === install.id && !f.revokedReason,
  );
  if (live.length >= MAX_LIVE_GRANTS) revokeFamily(live[0]!, "superseded");
  const family: Family = {
    id: id("pog"),
    installId: install.id,
    createdAt: Date.now(),
    revokedReason: null,
  };
  state.families.push(family);
  c.used = true;
  c.grantId = family.id;
  return ok({
    accessToken: secret("hlxpt_"),
    refreshToken: secret("hlxpr_"),
    scope: scopes.join(" "),
    workspaceId: c.workspaceId,
  });
}

/** The app reading through its install (POST /__mock/partner-apps/use): its last use. */
export function usePartnerInstall(body: { installId?: string }): MockReply {
  const i = state.installs.find((x) => x.id === body.installId && !x.revokedAt);
  if (!i) return oauthError(401, "invalid_token");
  i.lastUsedAt = now();
  return ok({});
}

/** The live token families of an install (tests). */
export const liveFamilies = (installId: string) =>
  state.families.filter((f) => f.installId === installId && !f.revokedReason)
    .length;
