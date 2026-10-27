// Server-side auth configuration, read at request time. A missing or unsafe
// value makes sign-in, sign-out and /papi refuse to run (503) instead of
// running half-configured.

import { createHmac } from "node:crypto";
import { readFileSync } from "node:fs";

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

// Tokens read from FOUNDRY_LOCAL_TOKEN_FILE, by path. Only a token long
// enough is kept: a file missing, unreadable or still being written is read
// again on the next request.
const tokenFiles = new Map<string, string>();

const longEnough = (token: string) => Buffer.byteLength(token, "utf8") >= 32;

/**
 * The owner's token: FOUNDRY_LOCAL_TOKEN, else the contents of the file
 * FOUNDRY_LOCAL_TOKEN_FILE names (trimmed): the one a local install's first
 * start wrote into its secrets volume (foundry init-secrets). "" when
 * neither gives one.
 */
function localToken(env: NodeJS.ProcessEnv): string {
  if (env.FOUNDRY_LOCAL_TOKEN) return env.FOUNDRY_LOCAL_TOKEN;
  const path = env.FOUNDRY_LOCAL_TOKEN_FILE;
  if (!path) return "";
  const kept = tokenFiles.get(path);
  if (kept) return kept;
  try {
    const token = readFileSync(path, "utf8").trim();
    if (longEnough(token)) tokenFiles.set(path, token);
    return token;
  } catch {
    return "";
  }
}

/**
 * A local build's settings (src/shared/deployment.ts), or null when one is
 * missing or unsafe. The site must be a loopback origin: with no sign-in,
 * whoever reaches it is the owner. The API may be plain http on any host,
 * such as another container. The token is FOUNDRY_LOCAL_TOKEN, else read
 * from the file FOUNDRY_LOCAL_TOKEN_FILE names; it is read per request
 * until a read gives one, then kept.
 *
 * @example
 * localConfig({
 *   HELIX_WFE_ORIGIN: "http://pod:8081",
 *   FOUNDRY_ORIGIN: "http://localhost:3000",
 *   FOUNDRY_LOCAL_TOKEN: "t".repeat(32),
 * })?.origin; // "http://localhost:3000"
 * localConfig({
 *   HELIX_WFE_ORIGIN: "http://pod:8081",
 *   FOUNDRY_ORIGIN: "http://localhost:3000",
 *   FOUNDRY_LOCAL_TOKEN_FILE: "/secrets/site-token",
 * })?.token; // the file's contents, trimmed
 */
export function localConfig(
  env: NodeJS.ProcessEnv = process.env,
): LocalConfig | null {
  const wfe = parse(env.HELIX_WFE_ORIGIN),
    site = parse(env.FOUNDRY_ORIGIN),
    token = localToken(env);
  if (
    !wfe ||
    (wfe.protocol !== "http:" && wfe.protocol !== "https:") ||
    !site ||
    !isTrustedUrl(site) ||
    !LOOPBACK.has(site.hostname) ||
    site.origin + "/" !== site.href ||
    !longEnough(token)
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
