import { createHmac, randomBytes, timingSafeEqual } from "node:crypto";
import { CSRF_TOKEN_TTL_SECONDS } from "@/shared/csrf-contract";

// HMAC CSRF tokens bound to the WorkOS session ID:
// v1.<expiresAt>.<nonce>.<hmac(secret, v1, sessionId, expiresAt, nonce)>.

const VERSION = "v1";
const BASE64URL_256_BIT = /^[A-Za-z0-9_-]{43}$/;

function signature(
  secret: string,
  sessionId: string,
  expiresAt: number,
  nonce: string,
) {
  return createHmac("sha256", secret)
    .update(VERSION)
    .update("\0")
    .update(sessionId)
    .update("\0")
    .update(String(expiresAt))
    .update("\0")
    .update(nonce)
    .digest("base64url");
}

export function createCsrfToken(
  secret: string,
  sessionId: string,
  nowSeconds = Math.floor(Date.now() / 1000),
) {
  const nonce = randomBytes(32).toString("base64url"),
    expiresAt = nowSeconds + CSRF_TOKEN_TTL_SECONDS;
  return `${VERSION}.${expiresAt}.${nonce}.${signature(secret, sessionId, expiresAt, nonce)}`;
}

export function verifyCsrfToken(
  token: string,
  secret: string,
  sessionId: string,
  nowSeconds = Math.floor(Date.now() / 1000),
): boolean {
  const [version, expiry, nonce, supplied, ...extra] = token.split(".");
  if (
    version !== VERSION ||
    extra.length > 0 ||
    !/^\d+$/.test(expiry || "") ||
    !BASE64URL_256_BIT.test(nonce || "") ||
    !BASE64URL_256_BIT.test(supplied || "")
  )
    return false;
  const expiresAt = Number(expiry);
  if (!Number.isSafeInteger(expiresAt) || expiresAt <= nowSeconds) return false;
  const a = Buffer.from(supplied!, "utf8"),
    b = Buffer.from(signature(secret, sessionId, expiresAt, nonce!), "utf8");
  return a.length === b.length && timingSafeEqual(a, b);
}

/** One cookie's value from the request's Cookie header. */
export function readCookie(request: Request, name: string): string | null {
  for (const part of (request.headers.get("cookie") || "").split(";")) {
    const [key, ...value] = part.trim().split("=");
    if (key !== name) continue;
    try {
      return decodeURIComponent(value.join("="));
    } catch {
      return null;
    }
  }
  return null;
}

/** The header token matches the cookie token and verifies for this session. */
export function csrfAccepted(
  request: Request,
  cookieName: string,
  headerName: string,
  secret: string,
  sessionId: string,
) {
  const header = request.headers.get(headerName),
    cookie = readCookie(request, cookieName);
  return (
    !!header &&
    !!cookie &&
    header === cookie &&
    verifyCsrfToken(header, secret, sessionId)
  );
}

/** Unsafe methods must come from this origin (checked before CSRF). */
export function sameOrigin(request: Request): boolean {
  const origin = request.headers.get("origin");
  return origin !== null && origin === new URL(request.url).origin;
}

export const isUnsafeMethod = (method: string) =>
  !["GET", "HEAD", "OPTIONS"].includes(method.toUpperCase());
