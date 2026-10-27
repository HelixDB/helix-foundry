import { getWorkOS } from "@workos-inc/authkit-nextjs";
import { type NextRequest, NextResponse } from "next/server";
import { authConfig } from "@/server/config";
import { csrfCookieName, expireCookie } from "@/server/cookies";
import { csrfAccepted, sameOrigin } from "@/server/csrf";
import { errorResponse } from "@/server/papi-handler";
import { resolveSession } from "@/server/session";
import { withSessionHeaders } from "@/server/session-headers";
import { CSRF_HEADER_NAME, CSRF_REJECTED_CODE } from "@/shared/csrf-contract";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// POST /auth/logout (same origin, CSRF header). Expires Foundry's cookies and
// answers { logoutUrl }: the WorkOS URL that ends the WorkOS session and then
// returns to /login. The browser navigates there.

export async function POST(request: NextRequest) {
  const config = authConfig();
  if (!config)
    return errorResponse(
      503,
      "MISCONFIGURED",
      "Authentication is not configured.",
    );
  if (!sameOrigin(request))
    return errorResponse(403, "ORIGIN_REJECTED", "Request origin rejected.");
  const session = await resolveSession(request, config.sessionCookie);
  if (session.kind === "transient")
    return withSessionHeaders(
      errorResponse(
        503,
        "AUTH_REFRESH_UNAVAILABLE",
        "Authentication is temporarily unavailable.",
      ),
      session.responseHeaders,
    );

  let logoutUrl = "/login";
  if (session.kind === "authenticated") {
    if (
      !csrfAccepted(
        request,
        csrfCookieName(config),
        CSRF_HEADER_NAME,
        config.csrfSecret,
        session.sessionId,
      )
    )
      return withSessionHeaders(
        errorResponse(403, CSRF_REJECTED_CODE, "CSRF validation failed."),
        session.responseHeaders,
      );
    try {
      logoutUrl = getWorkOS().userManagement.getLogoutUrl({
        sessionId: session.sessionId,
        returnTo: new URL("/login", request.url).toString(),
      });
    } catch {
      // Signing out here still wins if the WorkOS URL cannot be built.
    }
  }
  const response = NextResponse.json(
    { logoutUrl },
    { headers: { "Cache-Control": "no-store" } },
  );
  expireCookie(response, config.sessionCookie, config);
  expireCookie(response, csrfCookieName(config), config);
  return withSessionHeaders(response, session.responseHeaders);
}
