import type {
  ApprovePartnerAppAuthorizationRequest,
  FoundrySourceTarget,
  PartnerAppInstall,
} from "../../../shared/wfe";
import type { SlackReturn } from "./slack-return";

// Connected apps, pure: the scopes an app asks for and what they mean, the
// kept /oauth/authorize request the page may act on, the approval it sends,
// where Helix's answers may send the browser, and an install's lines in
// Settings › Connected apps.

/** The scopes a partner app's token may hold, in canonical order (partneroauth). */
export const PARTNER_SCOPES = ["foundry.read", "foundry.ask"] as const;
export type PartnerScope = (typeof PARTNER_SCOPES)[number];
/** At least one scope: what an approval allows. */
export type AllowedScopes = readonly [PartnerScope, ...PartnerScope[]];

/** Each scope's short name and what allowing it lets the app do. */
export const SCOPE_COPY: Record<PartnerScope, { label: string; text: string }> =
  {
    "foundry.read": {
      label: "Read",
      text: "Read the ontologies, schemas, graph and documents of the sources below",
    },
    "foundry.ask": {
      label: "Ask",
      text: "Ask questions of them, within this app’s own monthly Ask budget",
    },
  };

/** The most sources one install reads (Foundry's maxGrantsPerPrincipal). */
export const MAX_SOURCES = 1000;

/**
 * The scopes of `scopes` this build knows, once each, in canonical order.
 *
 * @example
 * scopesShown(["foundry.ask", "other", "foundry.read", "foundry.ask"]); // ["foundry.read", "foundry.ask"]
 */
export const scopesShown = (scopes: readonly string[] = []): PartnerScope[] =>
  PARTNER_SCOPES.filter((s) => scopes.includes(s));

/**
 * `scopes` as AllowedScopes: null when none is known.
 *
 * @example
 * allowedScopes(["foundry.ask"]); // ["foundry.ask"]
 * allowedScopes([]); // null
 */
export function allowedScopes(scopes: readonly string[]): AllowedScopes | null {
  const [first, ...rest] = scopesShown(scopes);
  return first ? [first, ...rest] : null;
}

/**
 * What an approval allows, in a word for analytics.
 *
 * @example
 * scopesEvent(["foundry.read", "foundry.ask"]); // "read_ask"
 */
export const scopesEvent = (
  scopes: AllowedScopes,
): "read" | "ask" | "read_ask" =>
  scopes.includes("foundry.read")
    ? scopes.includes("foundry.ask")
      ? "read_ask"
      : "read"
    : "ask";

/** A kept /oauth/authorize request the page may act on. */
export type PartnerRequest = Omit<
  Extract<SlackReturn, { kind: "app_authorize" }>,
  "kind" | "at"
>;

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

/**
 * Whether `uri` is a redirect URI Helix could have registered
 * (partneroauth.ValidateRedirectURI): absolute https, or http on loopback,
 * with no userinfo and no fragment.
 *
 * @example
 * redirectUriOk("https://northwind.example/helix/callback"); // true
 * redirectUriOk("http://northwind.example/cb"); // false
 * redirectUriOk("https://user@northwind.example/cb"); // false
 */
export function redirectUriOk(uri: string): boolean {
  let url: URL;
  try {
    url = new URL(uri);
  } catch {
    return false;
  }
  return (
    (url.protocol === "https:" ||
      (url.protocol === "http:" && LOOPBACK.has(url.hostname))) &&
    !url.username &&
    !url.password &&
    !uri.includes("#")
  );
}

/**
 * A kept request (slack-return.ts's app_authorize) the page may act on:
 * null when its redirect URI is none Helix could have registered, which the
 * page says instead of asking Helix.
 */
export function partnerRequest(
  kept: Extract<SlackReturn, { kind: "app_authorize" }>,
): PartnerRequest | null {
  const { clientId, redirectUri, scope, state, codeChallenge } = kept;
  return redirectUriOk(redirectUri)
    ? { clientId, redirectUri, scope, state, codeChallenge }
    : null;
}

/**
 * What Approve sends for `request`: the scopes allowed and the sources
 * chosen. Null with no scope, no source or more than MAX_SOURCES, which the
 * page never sends.
 */
export function approveBody(
  request: PartnerRequest,
  scopes: readonly PartnerScope[],
  targets: readonly FoundrySourceTarget[],
): ApprovePartnerAppAuthorizationRequest | null {
  const allowed = allowedScopes(scopes);
  if (!allowed || targets.length === 0 || targets.length > MAX_SOURCES)
    return null;
  return {
    clientId: request.clientId,
    redirectUri: request.redirectUri,
    state: request.state,
    codeChallenge: request.codeChallenge,
    scopes: [...allowed],
    targets: [...targets],
  };
}

/** A URL with its query and fragment cut off, or null when it isn't one. */
const base = (u: string) => {
  try {
    const url = new URL(u);
    return url.origin + url.pathname;
  } catch {
    return null;
  }
};

/**
 * Whether Helix's answer to an approval goes back to the app as it asked:
 * its redirect URI, with a code (hlxpa_) and the request's state.
 *
 * @example
 * approvedBack("https://n.example/cb?code=hlxpa_x&state=s&iss=i", { redirectUri: "https://n.example/cb", state: "s" }); // true
 */
export function approvedBack(
  url: string,
  request: Pick<PartnerRequest, "redirectUri" | "state">,
): boolean {
  const at = base(url);
  if (!at || at !== base(request.redirectUri)) return false;
  const q = new URL(url).searchParams;
  return (
    q.get("state") === request.state && !!q.get("code")?.startsWith("hlxpa_")
  );
}

/**
 * Where Cancel sends the browser: Helix's deny URL (the redirect URI with
 * error=access_denied and iss) with the request's state, which the site
 * never sends Helix's GET; null when it isn't the app's redirect URI.
 *
 * @example
 * cancelUrl("https://n.example/cb?error=access_denied&iss=i", { redirectUri: "https://n.example/cb", state: "s" }); // "https://n.example/cb?error=access_denied&iss=i&state=s"
 */
export function cancelUrl(
  denyUrl: string | undefined,
  request: Pick<PartnerRequest, "redirectUri" | "state">,
): string | null {
  if (!denyUrl || base(denyUrl) !== base(request.redirectUri)) return null;
  const url = new URL(denyUrl);
  if (url.searchParams.get("error") !== "access_denied") return null;
  url.searchParams.set("state", request.state);
  return url.toString();
}

/** The live install of the app `clientId`, if the workspace has one. */
export const existingInstall = (
  installs: readonly PartnerAppInstall[],
  clientId: string,
) => installs.find((i) => i.app?.clientId === clientId);

/** The installs' credentials, which Access › Agents leaves to Connected apps. */
export const appCredentialIds = (installs: readonly PartnerAppInstall[]) =>
  new Set(installs.map((i) => i.credentialId));

/**
 * An install's scopes in words.
 *
 * @example
 * scopesText(["foundry.ask", "foundry.read"]); // "Read, Ask"
 * scopesText([]); // "Nothing"
 */
export const scopesText = (scopes: readonly string[] = []) =>
  scopesShown(scopes)
    .map((s) => SCOPE_COPY[s].label)
    .join(", ") || "Nothing";

/**
 * An install's line of who approved it and when, and its last use:
 * `approver` names the approver's user id, `date` a time.
 *
 * @example
 * installLine({ id: "i", credentialId: "c", approvedByUserId: "u", approvedAt: "t" }, () => "Ada", () => "3 Nov"); // "Approved by Ada · 3 Nov · Never used"
 */
export function installLine(
  install: PartnerAppInstall,
  approver: (userId: string) => string,
  date: (at: string) => string,
): string {
  const at = install.approvedAt || install.createdAt;
  return [
    `Approved by ${install.approvedByUserId ? approver(install.approvedByUserId) : "someone"}`,
    at && date(at),
    install.lastUsedAt ? `Last used ${date(install.lastUsedAt)}` : "Never used",
  ]
    .filter(Boolean)
    .join(" · ");
}
