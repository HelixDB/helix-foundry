import { randomBytes } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type {
  AuthorizeConnectorSignInResponse,
  ConnectorConfig,
  ConnectorSignIn,
  ConnectorSignInFailure,
  ConnectorSignInResponse,
  ConnectorSignInStatus,
} from "../../src/shared/wfe";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { escape, page, send } from "../pages";
import { config, db, displayName, onReset } from "../state";
import { isFoundryPro, proRequired } from "./billing";
import {
  access,
  addSignedInConnector,
  isConnector,
  load,
  refusedFor,
  signConnectorInAgain,
  signInOffered,
  storedConnector,
} from "./data";
import { discover, kinds, prepare } from "./data-sources";

// Connecting a source by signing in at its provider, as the WFE's
// sign-in RPCs do (backend api/sign_ins.go), with WorkOS Pipes and the
// provider played by /mock/connect: a consent page with Authorize and
// Cancel, which sends the browser back to the site's /connect/callback with
// a connect code (or error=access_denied), as Pipes does.
//
// The backend's rules: owners and admins start a sign-in for a kind the
// stamp offers it for (`{"signIn": false}` and a local API offer none),
// with the connector's settings and no credential, or for a signed-in
// connector again, keeping its name and settings (one live per connector:
// another member's is SIGN_IN_IN_PROGRESS); at most 5 live per user and
// workspace, and 20 starts a user an hour in any (RESOURCE_EXHAUSTED); a
// client token returns what it started, once the request checks out. Only
// its starter authorizes (again after a decline or a failure, until it
// connects) and finishes it; it runs out 15 minutes after it started or was
// last authorized (`{"signInsRunOut": true}` runs every one out now, once).
// Finish ends it once: a repeat returns it as it stands. A
// code works once, for 90 seconds after the consent page gave it. A
// connector deleted under its sign-in ends it: Authorize is SIGN_IN_ENDED,
// and Finish fails it unavailable. Every kind Pipes signs in to does
// (GitHub, Linear, Slack, Notion, PostHog, Cal.com, Stripe, Google
// Workspace), with only the settings a sign-in has: a PostHog anywhere but
// US Cloud is refused, and so is a Google subject. Signed in, GitHub reads where
// Helix Foundry is installed: a named repository whose name ends in
// /missing is one it can't see, which connects anyway with a discovery
// problem. The account it reads with is named by the consent page's
// account field (by default its kind's identity: a GitHub login, a Slack
// team and bot, a Google email...), so signing in again under another one
// (case aside) changes the connector's identity; a Slack account naming
// xoxp vends a user token, which fails the sign-in settings_invalid.

// Each kind that signs in: its provider, what the consent page asks to
// read, and who its identity call says signed in (the account's label), from
// the user and their workspace.
type Who = { handle: string; email: string; name: string; team: string };
const PROVIDERS = {
  github: {
    name: "GitHub",
    reads: "read your repositories’ metadata, issues and pull requests",
    identity: (w: Who) => w.handle,
  },
  linear: {
    name: "Linear",
    reads: "read your Linear workspace",
    identity: (w: Who) => `${w.name} (${w.team})`,
  },
  slack: {
    name: "Slack",
    reads: "read the channels its bot is in, and your workspace's people",
    identity: (w: Who) => `${w.team} · @Helix Foundry`,
  },
  notion: {
    name: "Notion",
    reads: "read the pages you choose",
    identity: (w: Who) => w.team,
  },
  posthog: {
    name: "PostHog",
    reads: "read your project's events and persons",
    identity: (w: Who) => `${w.email} (${w.team})`,
  },
  calcom: {
    name: "Cal.com",
    reads: "read your bookings, event types and schedules",
    identity: (w: Who) => w.handle,
  },
  stripe: {
    name: "Stripe",
    reads: "read your Stripe account, with read-only access",
    identity: (w: Who) => w.team,
  },
  google: {
    name: "Google",
    reads: "read your Drive and Calendar",
    identity: (w: Who) => w.email,
  },
};
type SignInKind = keyof typeof PROVIDERS;

const EXPIRES_MS = 15 * 60_000,
  CODE_MS = 90_000,
  MAX_LIVE = 5,
  MAX_STARTS_AN_HOUR = 20;

type StoredSignIn = {
  id: string;
  workspaceId: string;
  kind: SignInKind;
  name: string;
  // The connector's settings, checked as Prepare checks them; no credential.
  settings: ConnectorConfig;
  // Set from the start when it signs a connector in again; once connected,
  // the connector it made.
  connectorId: string;
  again: boolean;
  status: Exclude<
    ConnectorSignInStatus,
    "CONNECTOR_SIGN_IN_STATUS_UNSPECIFIED"
  >;
  failure: ConnectorSignInFailure;
  discoveryProblem: string;
  identityChanged: boolean;
  createdBy: string;
  clientToken: string;
  createdAt: string;
  expiresAt: number;
  // The callback the last Authorize named.
  returnTo: string;
  // The code the consent page gave, when, and the account it chose.
  code?: { value: string; at: number; label: string; used: boolean };
};

const knobs = config as typeof config & { signInsRunOut?: boolean };
const signIns = new Map<string, StoredSignIn>();
onReset(() => {
  signIns.clear();
  delete knobs.signInsRunOut;
});

const ENDED = new Set<StoredSignIn["status"]>([
  "CONNECTOR_SIGN_IN_STATUS_CONNECTED",
  "CONNECTOR_SIGN_IN_STATUS_EXPIRED",
]);
/** Runs every sign-in out now, once asked: their 15 minutes, in a test. */
function runOut() {
  if (!knobs.signInsRunOut) return;
  delete knobs.signInsRunOut;
  for (const s of signIns.values())
    s.expiresAt = Math.min(s.expiresAt, Date.now() - 1);
}

// Past its expiry, a sign-in no callback is finishing reads EXPIRED.
function current(s: StoredSignIn): StoredSignIn {
  runOut();
  if (
    Date.now() > s.expiresAt &&
    s.status !== "CONNECTOR_SIGN_IN_STATUS_FINISHING" &&
    s.status !== "CONNECTOR_SIGN_IN_STATUS_CONNECTED"
  ) {
    s.status = "CONNECTOR_SIGN_IN_STATUS_EXPIRED";
    s.failure = "CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED";
  }
  return s;
}
const live = (s: StoredSignIn) => !ENDED.has(current(s).status);

const toSignIn = (s: StoredSignIn, userId: string): ConnectorSignIn => ({
  id: s.id,
  kind: s.kind,
  name: s.name,
  status: current(s).status,
  failure: s.failure,
  connectorId: s.connectorId,
  discoveryProblem: s.discoveryProblem,
  identityChanged: s.identityChanged,
  mine: s.createdBy === userId,
  createdAt: s.createdAt,
  expiresAt: new Date(s.expiresAt).toISOString(),
  config: s.settings,
});
const reply = (s: StoredSignIn, userId: string) =>
  ok({ signIn: toSignIn(s, userId) } satisfies ConnectorSignInResponse);

/** The site's callbacks Authorize accepts (foundry.connectors.sign_in_return_to). */
const returnTo = () =>
  new URL(
    "/connect/callback",
    process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI || "http://localhost:3000",
  ).toString();

// The account the consent page offers first: a GitHub login from the
// user's email, or a Linear user and their workspace.
function defaultAccount(s: StoredSignIn) {
  const user = db.users.get(s.createdBy);
  if (!user) return "";
  const handle = user.email.split("@")[0]!,
    team = db.workspaces.get(s.workspaceId)?.displayName || "Acme";
  return PROVIDERS[s.kind].identity({
    handle,
    email: user.email,
    name: displayName(user),
    team,
  });
}

function signInOf(req: MockRequest): StoredSignIn | MockReply {
  const s = signIns.get(req.params.id!);
  return s && s.workspaceId === req.params.workspaceId
    ? current(s)
    : fail("NOT_FOUND", "sign-in not found");
}
const isSignIn = (x: StoredSignIn | MockReply): x is StoredSignIn =>
  "kind" in x;
// Only its starter authorizes or finishes it.
const notYours = () =>
  refusedFor(
    "SIGN_IN_NOT_YOURS",
    "This link was made for someone else: ask your agent for a new one",
  );

const BASE = "/v1/workspaces/{workspaceId}/foundry/sign-ins" as const;

export const signInRoutes: MockRoute[] = [
  {
    method: "POST",
    path: BASE,
    handle: (req) => {
      const denied = access(req, true);
      if (denied) return denied;
      const ws = req.params.workspaceId!,
        user = req.principal.user.id,
        body = (req.body || {}) as {
          kind?: string;
          name?: string;
          config?: Record<string, Record<string, unknown>> | null;
          connectorId?: string;
          clientToken?: string;
        };
      const kind = String(body.kind || "");
      if (!signInOffered(kind))
        return refusedFor(
          "SIGN_IN_UNAVAILABLE",
          `connecting ${kinds.find((k) => k.name === kind)?.displayName || "this kind"} by signing in isn't offered here`,
        );
      const k = kinds.find((x) => x.name === kind)!;
      if (k.requiresPro && !isFoundryPro(ws))
        return proRequired(ws, `${k.displayName} connectors need Foundry Pro`);
      // What it is for is checked first, then a client token seen before
      // returns what it started.
      let settings: ConnectorConfig, name: string;
      const connectorId = String(body.connectorId || "");
      if (connectorId) {
        // Signing a connector in again: its own name and settings.
        if (body.name || (body.config && Object.keys(body.config).length))
          return fail(
            "INVALID_ARGUMENT",
            "signing a connector in again keeps its name and settings; change them on its page",
          );
        const c = load(req, connectorId);
        if (!isConnector(c)) return c;
        if (c.kind !== kind || !c.account)
          return fail(
            "FAILED_PRECONDITION",
            `${c.name} doesn't sign in with ${k.displayName}`,
          );
        settings = c.source.settings;
        name = c.name;
      } else {
        const config = body.config || { [kind]: {} };
        if (Object.keys(config).some((key) => key !== kind))
          return fail("INVALID_ARGUMENT", `config must be ${kind}'s`);
        const part = config[kind] || {};
        if (
          part.token ||
          part.apiKey ||
          part.botToken ||
          part.serviceAccountKey
        )
          return fail(
            "INVALID_ARGUMENT",
            "a sign-in's settings carry no credential",
          );
        const prepared = prepare(config, ws, true);
        if ("code" in prepared) return fail(prepared.code, prepared.message);
        settings = prepared.settings;
        name = String(body.name || "").trim() || k.displayName;
        if (name.length > 120)
          return fail("INVALID_ARGUMENT", "name must be 1 to 120 characters");
      }
      const token = String(body.clientToken || "");
      const replayed =
        token &&
        [...signIns.values()].find(
          (s) =>
            s.workspaceId === ws &&
            s.createdBy === user &&
            s.clientToken === token,
        );
      if (replayed) return reply(replayed, user);
      // One live sign-in a connector: its starter's is returned.
      const open =
        connectorId &&
        [...signIns.values()].find(
          (s) => s.connectorId === connectorId && s.again && live(s),
        );
      if (open && open.createdBy === user) return reply(open, user);
      if (open) {
        const who = db.users.get(open.createdBy);
        return refusedFor(
          "SIGN_IN_IN_PROGRESS",
          `${who ? displayName(who) : "another member"} is signing this connector in already; try again once they finish`,
        );
      }
      // Live ones in this workspace; starts in the last hour in any.
      const hour = Date.now() - 3_600_000,
        theirs = [...signIns.values()].filter((s) => s.createdBy === user);
      if (
        theirs.filter((s) => s.workspaceId === ws && live(s)).length >=
          MAX_LIVE ||
        theirs.filter((s) => Date.parse(s.createdAt) > hour).length >=
          MAX_STARTS_AN_HOUR
      )
        return fail(
          "RESOURCE_EXHAUSTED",
          `too many sign-ins: at most ${MAX_LIVE} at once in a workspace, and ${MAX_STARTS_AN_HOUR} an hour`,
        );
      const s: StoredSignIn = {
        id: id("csi"),
        workspaceId: ws,
        kind: kind as SignInKind,
        name,
        settings,
        connectorId,
        again: !!connectorId,
        status: "CONNECTOR_SIGN_IN_STATUS_PENDING",
        failure: "CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED",
        discoveryProblem: "",
        identityChanged: false,
        createdBy: user,
        clientToken: token,
        createdAt: now(),
        expiresAt: Date.now() + EXPIRES_MS,
        returnTo: "",
      };
      signIns.set(s.id, s);
      return reply(s, user);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{id}`,
    handle: (req) => {
      const s = access(req, true) || signInOf(req);
      return isSignIn(s) ? reply(s, req.principal.user.id) : s;
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:authorize`,
    handle: (req) => {
      const s = access(req, true) || signInOf(req);
      if (!isSignIn(s)) return s;
      const user = req.principal.user.id;
      if (s.createdBy !== user) return notYours();
      if (
        s.status === "CONNECTOR_SIGN_IN_STATUS_CONNECTED" ||
        s.status === "CONNECTOR_SIGN_IN_STATUS_FINISHING" ||
        s.status === "CONNECTOR_SIGN_IN_STATUS_EXPIRED"
      )
        return refusedFor(
          "SIGN_IN_ENDED",
          s.status === "CONNECTOR_SIGN_IN_STATUS_EXPIRED"
            ? "this sign-in ran out; start again"
            : "this sign-in has already finished",
        );
      if (String(req.body?.returnTo || "") !== returnTo())
        return fail(
          "INVALID_ARGUMENT",
          "return_to is not one of foundry.connectors.sign_in_return_to",
        );
      if (s.again && !storedConnector(s.connectorId))
        return refusedFor(
          "SIGN_IN_ENDED",
          "the connector this sign-in was for was deleted",
        );
      s.status = "CONNECTOR_SIGN_IN_STATUS_AUTHORIZING";
      s.failure = "CONNECTOR_SIGN_IN_FAILURE_UNSPECIFIED";
      s.expiresAt = Date.now() + EXPIRES_MS;
      s.returnTo = returnTo();
      s.code = undefined;
      const url = new URL("/mock/connect", process.env.MOCK_ORIGIN);
      url.searchParams.set("sign_in", s.id);
      return ok({
        url: url.toString(),
      } satisfies AuthorizeConnectorSignInResponse);
    },
  },
  {
    method: "POST",
    path: `${BASE}/{id}:finish`,
    handle: (req) => {
      const s = access(req, true) || signInOf(req);
      if (!isSignIn(s)) return s;
      const user = req.principal.user.id;
      if (s.createdBy !== user) return notYours();
      // Claimed once: anything but AUTHORIZING is returned as it stands.
      if (s.status !== "CONNECTOR_SIGN_IN_STATUS_AUTHORIZING")
        return reply(s, user);
      s.status = "CONNECTOR_SIGN_IN_STATUS_FINISHING";
      const failed = (failure: ConnectorSignInFailure) => {
        s.status = "CONNECTOR_SIGN_IN_STATUS_FAILED";
        s.failure = failure;
        return reply(s, user);
      };
      const code = String(req.body?.connectCode || "");
      if (!code) {
        if (req.body?.error === "access_denied") {
          s.status = "CONNECTOR_SIGN_IN_STATUS_DECLINED";
          return reply(s, user);
        }
        return failed("CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR");
      }
      // The connector signed in again is read before the code is spent.
      const again = s.again ? storedConnector(s.connectorId) : undefined;
      if (s.again && !again?.account)
        return failed("CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE");
      const given = s.code;
      if (
        !given ||
        given.used ||
        given.value !== code ||
        Date.now() - given.at > CODE_MS
      )
        return failed("CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED");
      given.used = true;
      // A Slack account named for a user token vends one (xoxp-), which
      // reads as its person: refused as a pasted one is.
      if (s.kind === "slack" && /xoxp/i.test(given.label))
        return failed("CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID");
      // Pipes saved the account; the token reads as the settings ask.
      const prepared = prepare(s.settings, s.workspaceId, true);
      if ("code" in prepared)
        return failed("CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID");
      const found = discover(prepared);
      s.discoveryProblem = "code" in found ? found.message : "";
      const account = {
        id: id("acct"),
        userId: user,
        label: given.label,
        connectedAt: now(),
      };
      if (again) s.identityChanged = signConnectorInAgain(again, account);
      else
        s.connectorId = addSignedInConnector(s.workspaceId, s.name, prepared, {
          ...account,
          generation: 1,
        }).id;
      s.status = "CONNECTOR_SIGN_IN_STATUS_CONNECTED";
      return reply(s, user);
    },
  },
];

/**
 * The provider's consent page that Authorize's URL opens (/mock/connect):
 * Authorize sends the browser back to the site's callback with a connect
 * code, Cancel with error=access_denied. False if it isn't that page.
 */
export function handleConnectPage(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  raw: string,
): boolean {
  if (url.pathname !== "/mock/connect") return false;
  const form = new URLSearchParams(raw),
    asked =
      req.method === "POST"
        ? form.get("sign_in")
        : url.searchParams.get("sign_in"),
    s = signIns.get(asked || "");
  if (
    !s ||
    current(s).status !== "CONNECTOR_SIGN_IN_STATUS_AUTHORIZING" ||
    !s.returnTo
  ) {
    send(res, 404, page("Mock sign-in", "<h1>This sign-in has expired</h1>"));
    return true;
  }
  const provider = PROVIDERS[s.kind].name;
  if (req.method === "GET") {
    send(
      res,
      200,
      page(
        `Authorize Helix Foundry · ${provider}`,
        `<h1>Authorize Helix Foundry</h1><p>Mock ${provider} sign-in. Helix Foundry (dev) asks to ${PROVIDERS[s.kind].reads}, for ${escape(s.name)}.</p>
<form method="post" action="/mock/connect"><input type="hidden" name="sign_in" value="${escape(s.id)}">
<label>${provider} account <input name="account" value="${escape(defaultAccount(s))}"></label>
<button type="submit" name="action" value="authorize">Authorize</button>
<button type="submit" name="action" value="cancel" class="secondary">Cancel</button></form>`,
      ),
    );
    return true;
  }
  const back = new URL(s.returnTo);
  if (form.get("action") === "authorize") {
    const value = randomBytes(12).toString("hex");
    s.code = {
      value,
      at: Date.now(),
      label: (form.get("account") || "").trim() || defaultAccount(s),
      used: false,
    };
    back.searchParams.set("connect_code", value);
  } else back.searchParams.set("error", "access_denied");
  res.writeHead(303, { Location: back.toString() });
  res.end();
  return true;
}
