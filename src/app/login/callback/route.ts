import { getWorkOS, saveSession } from "@workos-inc/authkit-nextjs";
import { type NextRequest, NextResponse } from "next/server";
import { authConfig } from "@/server/config";
import { expireCookie, loginCookiePrefix } from "@/server/cookies";
import {
  loginCookieName,
  loginFlowId,
  openLoginTransaction,
} from "@/server/login-transaction";
import type {
  ExchangeAuthCodeRequest,
  ExchangeAuthCodeResponse,
} from "@/shared/wfe";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET /login/callback?code&state from WorkOS. The WFE exchanges the code for
// the pending login it started; the tokens become the sealed AuthKit session.
// Every failure lands on the sign-in page with ?error=<reason>.

type Failure = "callback" | "denied" | "verify" | "sso";

function fail(request: NextRequest, reason: Failure, cookie?: string | null) {
  const response = NextResponse.redirect(
    new URL(`/login?error=${reason}`, request.url),
    { status: 303, headers: { "Cache-Control": "no-store" } },
  );
  const config = authConfig();
  if (cookie && config) expireCookie(response, cookie, config);
  return response;
}

// The WorkOS user ID: the access token's subject.
function subject(accessToken: string): string | null {
  try {
    const claims = JSON.parse(
      Buffer.from(accessToken.split(".")[1] || "", "base64url").toString(),
    ) as { sub?: unknown };
    return typeof claims.sub === "string" && claims.sub ? claims.sub : null;
  } catch {
    return null;
  }
}

export async function GET(request: NextRequest) {
  const config = authConfig();
  const code = request.nextUrl.searchParams.get("code"),
    state = request.nextUrl.searchParams.get("state");
  const flowId = state ? loginFlowId(state) : null,
    cookie =
      flowId && config
        ? loginCookieName(loginCookiePrefix(config), flowId)
        : null;
  const transaction =
    cookie && config
      ? openLoginTransaction(
          request.cookies.get(cookie)?.value ?? "",
          config.cookiePassword,
        )
      : null;
  if (!config || !code || !state || !transaction)
    return fail(request, "callback", cookie);

  let tokens: ExchangeAuthCodeResponse;
  try {
    const body: ExchangeAuthCodeRequest = {
      sessionId: transaction.sessionId,
      code,
      state,
    };
    const res = await fetch(new URL("/v1/auth/exchange", config.wfeOrigin), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
      redirect: "manual",
    });
    // PERMISSION_DENIED: the stamp does not let this email sign in (the
    // WFE's auth.allowed_emails). Retrying would only refuse it again.
    if (!res.ok)
      return fail(request, res.status === 403 ? "denied" : "callback", cookie);
    tokens = await res.json();
  } catch {
    return fail(request, "callback", cookie);
  }
  // Password and SSO sign-in are not offered yet; say why instead of looping.
  if (tokens.ssoRequired === true) return fail(request, "sso", cookie);
  if (tokens.emailVerificationRequired === true)
    return fail(request, "verify", cookie);
  const { accessToken, refreshToken } = tokens;
  const userId = accessToken ? subject(accessToken) : null;
  if (!accessToken || !refreshToken || !userId)
    return fail(request, "callback", cookie);
  try {
    const user = await getWorkOS().userManagement.getUser(userId);
    await saveSession({ accessToken, refreshToken, user }, request);
  } catch {
    return fail(request, "callback", cookie);
  }

  const response = NextResponse.redirect(
    new URL(transaction.returnPath, request.url),
    { status: 303, headers: { "Cache-Control": "no-store" } },
  );
  expireCookie(response, cookie!, config);
  return response;
}
