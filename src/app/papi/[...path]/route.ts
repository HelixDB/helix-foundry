import { type NextRequest, NextResponse } from "next/server";
import { authConfig, localConfig } from "@/server/config";
import { csrfCookieName, expireCookie } from "@/server/cookies";
import { handleLocalPapiRequest } from "@/server/local";
import { errorResponse, handlePapiRequest } from "@/server/papi-handler";
import {
  refreshAfterRejection,
  resolveSession,
  type ResolvedSession,
} from "@/server/session";
import { withSessionHeaders } from "@/server/session-headers";
import { LOCAL } from "@/shared/deployment";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

type Context = { params: Promise<{ path: string[] }> };

async function route(request: NextRequest, context: Context) {
  if (LOCAL) {
    const local = localConfig();
    return local
      ? handleLocalPapiRequest(
          request,
          (await context.params).path,
          local,
          fetch,
        )
      : errorResponse(503, "MISCONFIGURED", "Local mode is not configured.");
  }
  const config = authConfig();
  if (!config)
    return errorResponse(
      503,
      "MISCONFIGURED",
      "Authentication is not configured.",
    );
  const { path } = await context.params;
  let resolved: ResolvedSession | undefined;
  const response = await handlePapiRequest(request, path, {
    authenticate: async () =>
      (resolved = await resolveSession(request, config.sessionCookie)),
    // A token AuthKit just issued is not refreshed again: the WFE rejecting
    // it means the session is gone.
    refresh: async () =>
      resolved?.rotated
        ? { kind: "unauthenticated" }
        : refreshAfterRejection(request, config.sessionCookie),
    signOut: (r) => {
      const out = new NextResponse(r.body, {
        status: r.status,
        statusText: r.statusText,
        headers: r.headers,
      });
      expireCookie(out, config.sessionCookie, config);
      expireCookie(out, csrfCookieName(config), config);
      return out;
    },
    fetch,
    wfeOrigin: config.wfeOrigin,
    csrfSecret: config.csrfSecret,
    csrfCookie: csrfCookieName(config),
  });
  return resolved
    ? withSessionHeaders(response, resolved.responseHeaders)
    : response;
}

export const GET = route;
export const POST = route;
export const PUT = route;
export const PATCH = route;
export const DELETE = route;
