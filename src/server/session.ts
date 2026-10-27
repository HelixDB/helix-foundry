import "server-only";
import { createHash } from "node:crypto";
import {
  authkit,
  refreshSession,
  TokenRefreshError,
} from "@workos-inc/authkit-nextjs";
import type { NextRequest } from "next/server";
import type { BrowserSession } from "./papi-handler";
import { singleFlight } from "./single-flight";

// The sealed AuthKit session behind every /papi call and sign-out. AuthKit
// refreshes an expiring access token itself (30 s before a 300 s token ends);
// both that and the explicit refresh after an upstream 401 run single-flight
// per session cookie, because WorkOS refresh tokens are single use.

export type ResolvedSession = BrowserSession & {
  // Set-Cookie (a rotated session), Cache-Control and Vary from AuthKit.
  responseHeaders: Headers;
  // True when this resolution spent the refresh token.
  rotated: boolean;
};

const GRACE_MS = 30_000;
const resolving = singleFlight<ResolvedSession>({
  graceMs: GRACE_MS,
  keep: (s) => s.rotated && s.kind === "authenticated",
});
const refreshing = singleFlight<BrowserSession>({
  graceMs: GRACE_MS,
  keep: (s) => s.kind === "authenticated",
});

const keyOf = (cookie: string) =>
  createHash("sha256").update(cookie).digest("base64url");

function trusted(headers: Headers, sessionCookie: string) {
  const out = new Headers();
  // Only the session cookie: AuthKit's PKCE cookies belong to its hosted
  // sign-in, which Foundry does not use.
  for (const c of headers.getSetCookie())
    if (c.startsWith(sessionCookie + "=")) out.append("Set-Cookie", c);
  for (const name of ["Cache-Control", "Vary"]) {
    const v = headers.get(name);
    if (v) out.set(name, v);
  }
  return out;
}

/** Resolves the request's session, refreshing it once per cookie if needed. */
export function resolveSession(
  request: NextRequest,
  sessionCookie: string,
): Promise<ResolvedSession> {
  const cookie = request.cookies.get(sessionCookie)?.value;
  if (!cookie)
    return Promise.resolve({
      kind: "unauthenticated",
      responseHeaders: new Headers(),
      rotated: false,
    });
  return resolving(keyOf(cookie), async () => {
    let failure: "transient" | "terminal" | null = null;
    try {
      const result = await authkit(request, {
        onSessionRefreshError: ({ isTransient }) => {
          failure = isTransient ? "transient" : "terminal";
        },
      });
      const responseHeaders = trusted(result.headers, sessionCookie);
      const rotated = responseHeaders
        .getSetCookie()
        .some((c) => !/^[^=]+=;|Max-Age=0(?:;|$)/i.test(c));
      const { accessToken, sessionId, user } = result.session;
      if (failure === "transient")
        return { kind: "transient", responseHeaders, rotated: false };
      if (failure === "terminal" || !user || !accessToken || !sessionId)
        return { kind: "unauthenticated", responseHeaders, rotated: false };
      return {
        kind: "authenticated",
        accessToken,
        sessionId,
        responseHeaders,
        rotated,
      };
    } catch {
      // An unknown provider failure keeps the cookie so a retry can succeed.
      return {
        kind: "transient",
        responseHeaders: new Headers(),
        rotated: false,
      };
    }
  });
}

/** Refreshes after the WFE rejected a token AuthKit still thought valid. */
export function refreshAfterRejection(
  request: NextRequest,
  sessionCookie: string,
): Promise<BrowserSession> {
  const cookie = request.cookies.get(sessionCookie)?.value || "";
  return refreshing(keyOf(cookie), async () => {
    try {
      const s = await refreshSession();
      return s.user && s.accessToken && s.sessionId
        ? {
            kind: "authenticated",
            accessToken: s.accessToken,
            sessionId: s.sessionId,
          }
        : { kind: "unauthenticated" };
    } catch (error) {
      return error instanceof TokenRefreshError && !error.isTransient
        ? { kind: "unauthenticated" }
        : { kind: "transient" };
    }
  });
}
