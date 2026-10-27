// Server-side auth configuration, read at request time. A missing or unsafe
// value makes sign-in, sign-out and /papi refuse to run (503) instead of
// running half-configured.

import { createHmac } from "node:crypto";

const LOOPBACK = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** https, or plain http on loopback only (dev:mock and e2e). */
export function isTrustedUrl(url: URL): boolean {
  return (
    url.protocol === "https:" ||
    (url.protocol === "http:" && LOOPBACK.has(url.hostname))
  );
}

export type AuthConfig = {
  wfeOrigin: string;
  redirectUri: URL;
  cookiePassword: string;
  csrfSecret: string;
  sessionCookie: string;
  // Cookies are Secure (and __Host- prefixed) unless the site is loopback http.
  secure: boolean;
};

function parse(value: string | undefined): URL | null {
  try {
    return value ? new URL(value) : null;
  } catch {
    return null;
  }
}

export type LocalConfig = {
  wfeOrigin: string;
  // The bearer token the API accepts as its one local owner.
  token: string;
  // Derived from the token, so a local copy has one secret to keep.
  csrfSecret: string;
  // The loopback origin the browser uses. A request with any other Host is
  // refused, so a page whose DNS name was rebound to 127.0.0.1 cannot drive
  // the API as the owner.
  origin: string;
};

/**
 * A local build's settings (src/shared/deployment.ts), or null when one is
 * missing or unsafe. The site must be a loopback origin: with no sign-in,
 * whoever reaches it is the owner. The API may be plain http on any host,
 * such as another container.
 *
 * @example
 * localConfig({
 *   HELIX_WFE_ORIGIN: "http://pod:8081",
 *   FOUNDRY_ORIGIN: "http://localhost:3000",
 *   FOUNDRY_LOCAL_TOKEN: "t".repeat(32),
 * })?.origin; // "http://localhost:3000"
 */
export function localConfig(
  env: NodeJS.ProcessEnv = process.env,
): LocalConfig | null {
  const wfe = parse(env.HELIX_WFE_ORIGIN),
    site = parse(env.FOUNDRY_ORIGIN),
    token = env.FOUNDRY_LOCAL_TOKEN || "";
  if (
    !wfe ||
    (wfe.protocol !== "http:" && wfe.protocol !== "https:") ||
    !site ||
    !isTrustedUrl(site) ||
    !LOOPBACK.has(site.hostname) ||
    site.origin + "/" !== site.href ||
    Buffer.byteLength(token, "utf8") < 32
  )
    return null;
  return {
    wfeOrigin: wfe.origin,
    token,
    csrfSecret: createHmac("sha256", token)
      .update("foundry-csrf")
      .digest("base64url"),
    origin: site.origin,
  };
}

export function authConfig(env: NodeJS.ProcessEnv = process.env) {
  const wfe = parse(env.HELIX_WFE_ORIGIN),
    redirectUri = parse(env.NEXT_PUBLIC_WORKOS_REDIRECT_URI);
  const cookiePassword = env.WORKOS_COOKIE_PASSWORD || "",
    csrfSecret = env.HELIX_CSRF_SECRET || "",
    sessionCookie = env.WORKOS_COOKIE_NAME || "";
  if (!wfe || !isTrustedUrl(wfe) || !redirectUri || !isTrustedUrl(redirectUri))
    return null;
  if (
    redirectUri.pathname !== "/login/callback" ||
    redirectUri.search ||
    redirectUri.hash
  )
    return null;
  const secure = redirectUri.protocol === "https:";
  if (
    !env.WORKOS_CLIENT_ID ||
    !env.WORKOS_API_KEY ||
    cookiePassword.length < 32 ||
    Buffer.byteLength(csrfSecret, "utf8") < 32 ||
    !sessionCookie ||
    // Host-only sessions: a Secure session cookie must be __Host- and no
    // cookie may name a domain (it would leak to every *.helix-db.com site).
    (secure && !sessionCookie.startsWith("__Host-")) ||
    env.WORKOS_COOKIE_DOMAIN
  )
    return null;
  return {
    wfeOrigin: wfe.origin,
    redirectUri,
    cookiePassword,
    csrfSecret,
    sessionCookie,
    secure,
  } satisfies AuthConfig;
}
