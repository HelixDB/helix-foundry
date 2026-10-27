import { createHash, createHmac, timingSafeEqual } from "node:crypto";
import { parseReturnPath } from "@/shared/return-path";

// A sign-in in progress: the WFE's pending-login ID and where to land
// afterwards, signed (WORKOS_COOKIE_PASSWORD) into a short-lived cookie named
// after the OAuth state, so parallel sign-ins in two tabs do not collide.

export const LOGIN_TRANSACTION_TTL_SECONDS = 10 * 60;

const FLOW_ID = /^[A-Za-z0-9_-]{43}$/;
const SESSION_ID = /^[A-Za-z0-9_-]{32,256}$/;

export type LoginTransaction = { sessionId: string; returnPath: string };
type Stored = LoginTransaction & { issuedAt: number };

/** sha256(state), the flow's cookie suffix. */
export function loginFlowId(state: string): string | null {
  if (!state || state.length > 2048) return null;
  return createHash("sha256").update(state).digest("base64url");
}

export function loginCookieName(prefix: string, flowId: string) {
  return FLOW_ID.test(flowId) ? prefix + flowId : null;
}

const sign = (secret: string, payload: string) =>
  createHmac("sha256", secret).update(payload).digest();

export function sealLoginTransaction(
  transaction: LoginTransaction,
  secret: string,
  now = Date.now(),
): string | null {
  if (
    !secret ||
    !SESSION_ID.test(transaction.sessionId) ||
    parseReturnPath(transaction.returnPath) !== transaction.returnPath
  )
    return null;
  const payload = Buffer.from(
    JSON.stringify({ ...transaction, issuedAt: now } satisfies Stored),
  ).toString("base64url");
  return `v1.${payload}.${sign(secret, payload).toString("base64url")}`;
}

export function openLoginTransaction(
  value: string,
  secret: string,
  now = Date.now(),
): LoginTransaction | null {
  if (!secret) return null;
  const [version, payload, signature, extra] = value.split(".");
  if (version !== "v1" || !payload || !signature || extra !== undefined)
    return null;
  const given = Buffer.from(signature, "base64url"),
    expected = sign(secret, payload);
  if (given.length !== expected.length || !timingSafeEqual(given, expected))
    return null;
  try {
    const stored = JSON.parse(
      Buffer.from(payload, "base64url").toString("utf8"),
    ) as Partial<Stored>;
    if (
      typeof stored.sessionId !== "string" ||
      !SESSION_ID.test(stored.sessionId) ||
      typeof stored.returnPath !== "string" ||
      parseReturnPath(stored.returnPath) !== stored.returnPath ||
      typeof stored.issuedAt !== "number" ||
      stored.issuedAt > now ||
      now - stored.issuedAt > LOGIN_TRANSACTION_TTL_SECONDS * 1000
    )
      return null;
    return { sessionId: stored.sessionId, returnPath: stored.returnPath };
  } catch {
    return null;
  }
}
