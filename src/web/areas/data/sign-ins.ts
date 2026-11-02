import type {
  Connector,
  ConnectorAccountBroken,
  ConnectorSignIn,
} from "../../../shared/wfe";
import { parseReturnPath } from "../../../shared/return-path";
import { formatDate } from "../../kit/util";

// Connecting GitHub or Linear by signing in at the provider: what this tab
// keeps between the click and the provider's return, what the callback does
// with its URL before any API call, where it returns once connected, and
// the words for each outcome and for an account that needs signing in
// again. Kept apart from the components so it is tested on its own.
//
// The provider (through WorkOS Pipes) returns to /connect/callback exactly,
// with no query of ours, so the sign-in's id waits in sessionStorage: this
// tab's alone. The connect code it brings is single use and lasts 90 s.

/** The callback's path, registered with WorkOS as a redirect URI. */
export const CALLBACK_PATH = "/connect/callback";
/** Where the sign-in this tab started waits for the provider's return. */
export const CONNECT_KEY = "foundry.connect.v1";
/** How long a connect code lasts after the provider's redirect. */
export const CODE_LIFETIME_MS = 90_000;

/** The providers that connect by signing in, by kind. */
const PROVIDERS: Record<string, string> = {
  github: "GitHub",
  linear: "Linear",
};
export const providerName = (kind: string) =>
  PROVIDERS[kind] || kind.charAt(0).toUpperCase() + kind.slice(1);

/** The sign-in this tab sent to the provider, and what came back. */
export type ConnectEntry = {
  workspaceId: string;
  signInId: string;
  kind: string;
  // Where to go once connected, with `connected=<connector id>`: the page
  // the form was opened over. None: the connector's own page.
  returnPath?: string;
  // Set when it signs this connector in again.
  connectorId?: string;
  // What the provider sent the callback (a code, or an error without one)
  // and when, moved here from the URL before anything else runs.
  code?: string;
  error?: string;
  at?: number;
};

const text = (v: unknown) => (typeof v === "string" && v ? v : undefined);

/** This tab's sessionStorage, or null where the browser blocks it. */
export function tabStorage(): Storage | null {
  try {
    return sessionStorage;
  } catch {
    return null;
  }
}

/** This tab's entry, if it holds a well-formed one. */
export function readConnect(storage: Storage | null): ConnectEntry | null {
  try {
    const v: unknown = JSON.parse(storage?.getItem(CONNECT_KEY) || "null");
    if (!v || typeof v !== "object") return null;
    const e = v as Record<string, unknown>;
    const workspaceId = text(e.workspaceId),
      signInId = text(e.signInId),
      kind = text(e.kind);
    if (!workspaceId || !signInId || !kind) return null;
    return {
      workspaceId,
      signInId,
      kind,
      returnPath: text(e.returnPath),
      connectorId: text(e.connectorId),
      code: text(e.code),
      error: text(e.error),
      at: typeof e.at === "number" ? e.at : undefined,
    };
  } catch {
    return null;
  }
}

export function writeConnect(storage: Storage | null, entry: ConnectEntry) {
  try {
    storage?.setItem(CONNECT_KEY, JSON.stringify(entry));
  } catch {
    // Storage blocked: the callback says to start again from this tab.
  }
}

export function forgetConnect(storage: Storage | null) {
  try {
    storage?.removeItem(CONNECT_KEY);
  } catch {
    // Storage blocked: nothing was kept.
  }
}

/**
 * The callback, before any API call: moves the provider's `connect_code`
 * (or its `error`) into this tab's entry with the time, then replaces the
 * URL with the bare callback path, so the code never reaches history,
 * analytics or a later page. A tab with no entry keeps nothing. True when
 * the URL held an answer.
 */
export function captureCallback(
  url: { pathname: string; search: string },
  storage: Storage | null,
  replace: (path: string) => void,
  now = Date.now(),
): boolean {
  if (url.pathname !== CALLBACK_PATH) return false;
  const q = new URLSearchParams(url.search),
    code = q.get("connect_code"),
    error = q.get("error");
  if (!url.search) return false;
  const entry = readConnect(storage);
  if (entry && (code || error))
    writeConnect(storage, {
      workspaceId: entry.workspaceId,
      signInId: entry.signInId,
      kind: entry.kind,
      returnPath: entry.returnPath,
      connectorId: entry.connectorId,
      ...(code ? { code } : { error: error || "unknown" }),
      at: now,
    });
  replace(CALLBACK_PATH);
  return !!(code || error);
}

/**
 * The callback the provider returns to: on the origin of the stamp's WorkOS
 * redirect URI, which WorkOS lists, so a preview deployment never sends one
 * it doesn't (the return then lands on the stamp, without this tab's
 * entry). Without one, this page's origin.
 */
export function callbackUrl(
  redirectUri = process.env.NEXT_PUBLIC_WORKOS_REDIRECT_URI,
  origin = typeof location === "undefined" ? "" : location.origin,
) {
  const base = URL.parse(redirectUri || "")?.origin || origin;
  return new URL(CALLBACK_PATH, base).toString();
}

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);
/**
 * Whether the browser may be sent to `url` to authorize: https only, but
 * for the mock's consent page while the site itself runs on loopback http
 * (dev:mock, e2e).
 */
export function isProviderUrl(url: string, page: string) {
  const u = URL.parse(url),
    p = URL.parse(page);
  if (!u) return false;
  if (u.protocol === "https:") return true;
  return (
    u.protocol === "http:" &&
    p?.protocol === "http:" &&
    LOOPBACK.has(u.hostname) &&
    LOOPBACK.has(p.hostname)
  );
}

/** Whether the code came back too long ago to finish. */
export const codeStale = (entry: ConnectEntry, now = Date.now()) =>
  !!entry.code && entry.at !== undefined && now - entry.at > CODE_LIFETIME_MS;

/**
 * Where the callback goes once connected, in the workspace the sign-in
 * was started in: the entry's return path (a path in this app, never the
 * callback's) with `connected=<connectorId>`, which the wizard and
 * Generate choose; else the connector's page.
 */
export function connectedPath(
  entry: ConnectEntry,
  connectorId: string,
  connectionPath: (id: string) => string,
) {
  const back = parseReturnPath(entry.returnPath);
  const url =
    back && !back.startsWith("/connect/")
      ? new URL(back, "https://foundry.invalid")
      : new URL(connectionPath(connectorId), "https://foundry.invalid");
  if (back && !back.startsWith("/connect/"))
    url.searchParams.set("connected", connectorId);
  url.searchParams.set("workspace", entry.workspaceId);
  return url.pathname + url.search + url.hash;
}

/**
 * Where Start again goes when a sign-in can't be retried, in its
 * workspace: the connector it signed in again, else the page it was
 * started from, else its kind's form in Data.
 */
export function startAgainPath(
  entry: ConnectEntry,
  paths: {
    connection: (id: string) => string;
    connections: (kind: string) => string;
  },
) {
  const back = parseReturnPath(entry.returnPath);
  const url = new URL(
    entry.connectorId
      ? paths.connection(entry.connectorId)
      : back && !back.startsWith("/connect/")
        ? back
        : paths.connections(entry.kind),
    "https://foundry.invalid",
  );
  url.searchParams.set("workspace", entry.workspaceId);
  return url.pathname + url.search + url.hash;
}

/** How a sign-in that didn't connect is worded, and what it offers. */
export type SignInFailure = {
  title: string;
  text: string;
  // "again": authorize the same sign-in again; "start": a new one.
  retry: "again" | "start";
};

/**
 * What a sign-in that ended without connecting says, from its status and
 * failure code (no provider text ever comes back). `stale`: its code was
 * older than a code lasts when it reached the callback.
 */
export function signInFailure(
  s: Pick<ConnectorSignIn, "status" | "failure" | "kind">,
  stale = false,
): SignInFailure {
  const provider = providerName(s.kind || "");
  if (s.status === "CONNECTOR_SIGN_IN_STATUS_EXPIRED")
    return {
      title: "This sign-in ran out: start again",
      text: `A sign-in lasts 15 minutes from when it was started or last sent to ${provider}. Nothing was connected.`,
      retry: "start",
    };
  if (s.status === "CONNECTOR_SIGN_IN_STATUS_DECLINED")
    return {
      title: `${provider} wasn’t authorized`,
      text: `Helix Foundry was declined at ${provider}, so nothing was connected. Try again to authorize it.`,
      retry: "again",
    };
  if (s.status !== "CONNECTOR_SIGN_IN_STATUS_FAILED")
    return {
      title: "The sign-in didn’t finish",
      text: `Foundry didn’t get ${provider}’s answer, so nothing was connected. Try again.`,
      retry: "again",
    };
  if (stale || s.failure === "CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED")
    return {
      title: "The sign-in took too long",
      text: `${provider}’s answer ran out before Foundry could use it, so nothing was connected. Try again, and finish within a minute or so.`,
      retry: "again",
    };
  switch (s.failure) {
    case "CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE":
      return {
        title: "That sign-in wasn’t yours or this workspace’s",
        text: `${provider} returned an account that this workspace, or you, didn’t make, so Foundry let it go. Try again from this tab.`,
        retry: "again",
      };
    case "CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED":
      return {
        title: `${provider} refused the access it gave`,
        text: `${provider} turned down the access it had just given Helix Foundry, so nothing was connected. Try again.`,
        retry: "again",
      };
    case "CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID":
      return {
        title: "The connection’s settings were refused",
        text: `${provider} accepted the sign-in, but not the settings chosen for it, so nothing was connected. Start again with other settings.`,
        retry: "start",
      };
    case "CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE":
      return {
        title: "Signing in is unavailable right now",
        text: `Foundry couldn’t reach the service that keeps ${provider} sign-ins. Try again in a moment.`,
        retry: "again",
      };
    default:
      return {
        title: `${provider} couldn’t finish the sign-in`,
        text: `${provider} answered with an error, so nothing was connected. Try again.`,
        retry: "again",
      };
  }
}

// ---------------------------------------------------------------- Accounts

/** Whether a connector's account needs signing in again. */
export const needsSignIn = (c: Pick<Connector, "account">) =>
  c.account?.state === "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN";

// The member's name, unless the WFE no longer names them.
const FORMER = /^a former member$/i;
const memberName = (name?: string) =>
  name && !FORMER.test(name.trim()) ? name.trim() : "";

/**
 * Whose access a signed-in connector reads with: "Reads with Ada
 * Lovelace’s GitHub access (ada-l)".
 */
export function accountLine(c: Pick<Connector, "kind" | "account">) {
  const a = c.account;
  if (!a) return "";
  const who = memberName(a.connectedBy) || "a former member",
    label = a.label?.trim();
  return `Reads with ${who}’s ${providerName(c.kind)} access${label ? ` (${label})` : ""}`;
}

/** Why an account needs signing in again, as a sentence. */
export function brokenWhy(c: Pick<Connector, "kind" | "account">) {
  const a = c.account,
    provider = providerName(c.kind),
    who = memberName(a?.connectedBy);
  const reasons: Record<ConnectorAccountBroken, string> = {
    CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT: who
      ? `${who}, who connected it, left the workspace.`
      : "The member who connected it left the workspace.",
    CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED: `${provider} no longer accepts the access it was given.`,
    CONNECTOR_ACCOUNT_BROKEN_EXPIRED: `Its access to ${provider} lapsed and couldn’t be renewed.`,
    CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING: `Its sign-in with ${provider} no longer exists.`,
    CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED: `Its sign-in with ${provider} needs renewing.`,
  };
  return reasons[a?.broken || "CONNECTOR_ACCOUNT_BROKEN_UNSPECIFIED"];
}

/**
 * What the shell's dialog and banner say of the connectors that need
 * signing in again: one by name, with why and since when; several as a
 * count and their names, which Data lists.
 */
export function brokenNotice(
  broken: readonly Pick<Connector, "id" | "kind" | "name" | "account">[],
) {
  const name = (c: (typeof broken)[number]) => c.name || providerName(c.kind);
  if (broken.length === 1) {
    const c = broken[0]!,
      at = c.account?.brokenAt;
    return {
      title: `${name(c)} stopped syncing`,
      text: `${brokenWhy(c)} Its data is frozen as of ${at ? formatDate(at) : "its last sync"}.`,
    };
  }
  const names = broken.map(name),
    list =
      names.length > 1
        ? `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`
        : names.join("");
  return {
    title: `${broken.length} connections stopped syncing`,
    text: `${list} need signing in again. Their data is frozen as of their last syncs.`,
  };
}

/** One break of one connector: a new break is a new key. */
export const breakKey = (c: Pick<Connector, "id" | "account">) =>
  `${c.id}:${c.account?.brokenAt || ""}`;
