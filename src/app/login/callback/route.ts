import { type NextRequest, NextResponse } from "next/server";
import { authConfig } from "@/server/config";
import {
  cookieOptions,
  expireCookie,
  loginCookiePrefix,
} from "@/server/cookies";
import {
  LOGIN_TRANSACTION_TTL_SECONDS,
  loginCookieName,
  loginFlowId,
  openLoginTransaction,
  sealLoginTransaction,
} from "@/server/login-transaction";
import {
  exchangeFailure,
  finishSignIn,
  signInErrorPath,
  throughSso,
} from "@/server/sign-in";
import type {
  ExchangeAuthCodeRequest,
  ExchangeAuthCodeResponse,
} from "@/shared/wfe";
import type { SignInError } from "@/web/sign-in";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET /login/callback?code&state from WorkOS. The WFE exchanges the code for
// the pending login it started; the tokens become the sealed AuthKit session
// (finishSignIn). Two answers carry no tokens. WorkOS wants the code it
// emailed when it hasn't seen the address verified (a GitHub or Microsoft
// account, often): the pending login is kept for /login/verify, which sends
// the code to /auth/verify-email. The email's company requires its single
// sign-on: a new login goes through it, once. Every failure lands on the
// sign-in page with ?error=<reason>, one reason per cause, so sign_in_failed
// tells them apart: the provider's own error, a callback without its code or
// state, a browser that lost the login's cookie (an app's built-in browser,
// often) or sent one that no longer opens, and each way the exchange fails.

const NO_STORE = { "Cache-Control": "no-store" } as const;

function fail(
  request: NextRequest,
  reason: SignInError,
  cookie?: string | null,
  returnPath?: string,
) {
  const response = NextResponse.redirect(
    new URL(signInErrorPath(reason, returnPath), request.url),
    { status: 303, headers: NO_STORE },
  );
  const config = authConfig();
  if (cookie && config) expireCookie(response, cookie, config);
  return response;
}

export async function GET(request: NextRequest) {
  const config = authConfig();
  if (!config) return fail(request, "unavailable");
  const params = request.nextUrl.searchParams,
    code = params.get("code"),
    state = params.get("state");
  const flowId = state ? loginFlowId(state) : null,
    cookie = flowId ? loginCookieName(loginCookiePrefix(config), flowId) : null;
  // WorkOS returns with ?error= when the provider or the user declined.
  if (params.has("error")) return fail(request, "provider_error", cookie);
  if (!code || !state) return fail(request, "missing_code", cookie);
  if (!flowId || !cookie) return fail(request, "invalid_state");
  const sealed = request.cookies.get(cookie)?.value;
  if (!sealed) return fail(request, "missing_flow_cookie");
  const transaction = openLoginTransaction(sealed, config.cookiePassword);
  if (!transaction) return fail(request, "flow_cookie_invalid", cookie);

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
    // A denied email (the WFE's auth.allowed_emails) would only be refused
    // again; the others need a new sign-in.
    if (!res.ok)
      return fail(
        request,
        exchangeFailure(res.status, await res.json().catch(() => null)),
        cookie,
      );
    tokens = await res.json();
  } catch {
    return fail(request, "exchange_unavailable", cookie);
  }

  if (tokens.ssoRequired === true) {
    const sso = await throughSso(
      request,
      config,
      transaction,
      tokens.email,
      cookie,
    );
    if (!sso) return fail(request, "sso", cookie, transaction.returnPath);
    const response = NextResponse.redirect(sso.to, {
      status: 303,
      headers: NO_STORE,
    });
    sso.set(response);
    return response;
  }

  if (tokens.emailVerificationRequired === true) {
    // The WFE keeps the pending login for its code for as long as a new
    // one, so the cookie starts again too.
    const waiting = sealLoginTransaction(
      { ...transaction, verifyEmail: tokens.email ?? "" },
      config.cookiePassword,
    );
    if (!waiting) return fail(request, "verify_start_failed", cookie);
    const response = NextResponse.redirect(
      new URL(
        `/login/verify?${new URLSearchParams({ flow: flowId })}`,
        request.url,
      ),
      { status: 303, headers: NO_STORE },
    );
    response.cookies.set({
      name: cookie,
      value: waiting,
      ...cookieOptions(config),
      maxAge: LOGIN_TRANSACTION_TTL_SECONDS,
    });
    return response;
  }

  if (!(await finishSignIn(request, tokens)))
    return fail(request, "session_failed", cookie);
  const response = NextResponse.redirect(
    new URL(transaction.returnPath, request.url),
    { status: 303, headers: NO_STORE },
  );
  expireCookie(response, cookie, config);
  return response;
}
