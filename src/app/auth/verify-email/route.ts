import { type NextRequest, NextResponse } from "next/server";
import { authConfig } from "@/server/config";
import { expireCookie, loginCookiePrefix } from "@/server/cookies";
import { sameOrigin } from "@/server/csrf";
import {
  loginCookieName,
  openLoginTransaction,
} from "@/server/login-transaction";
import { errorResponse } from "@/server/papi-handler";
import {
  finishSignIn,
  signInErrorPath,
  throughSso,
  verifyFailure,
} from "@/server/sign-in";
import type { VerifyEmailRequest, VerifyEmailResponse } from "@/shared/wfe";
import type { VerifyEmailAnswer } from "@/web/sign-in";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// The sign-in /login/callback kept for the code WorkOS emailed, named by its
// flow (the login cookie's suffix), as /login/verify?flow= shows it.
//
// GET /auth/verify-email?flow=: { email, returnTo } while it waits, else
// { redirectTo } the sign-in page saying it ran out.
//
// POST /auth/verify-email (same origin) { flow, code }: the WFE's VerifyEmail
// under the kept login. { redirectTo } where to go next: the page the
// sign-in started from once signed in, the company's single sign-on when it
// requires it, or the sign-in page saying why it ended. { error } when the
// page may try again: `code` a wrong code, `unavailable` the WFE or WorkOS
// away; the kept login stays for either.

const NO_STORE = { "Cache-Control": "no-store" } as const;
const MAX_BODY = 1024;
const CODE = /^[A-Za-z0-9]{1,32}$/;

const answer = (body: VerifyEmailAnswer, status = 200) =>
  NextResponse.json(body, { status, headers: NO_STORE });

/** The kept login `flow` names: its cookie and its transaction. */
function keptLogin(request: NextRequest, flow: unknown) {
  const config = authConfig();
  if (!config || typeof flow !== "string") return null;
  const cookie = loginCookieName(loginCookiePrefix(config), flow);
  if (!cookie) return null;
  const transaction = openLoginTransaction(
    request.cookies.get(cookie)?.value ?? "",
    config.cookiePassword,
  );
  return { config, cookie, transaction };
}

export async function GET(request: NextRequest) {
  const kept = keptLogin(request, request.nextUrl.searchParams.get("flow"));
  const waiting = kept?.transaction?.verifyEmail;
  if (!kept?.transaction || waiting === undefined)
    return answer({ redirectTo: signInErrorPath("verify") });
  return answer({
    email: waiting,
    returnTo: kept.transaction.returnPath,
  });
}

export async function POST(request: NextRequest) {
  if (!sameOrigin(request))
    return errorResponse(403, "ORIGIN_REJECTED", "Request origin rejected.");
  let body: { flow?: unknown; code?: unknown };
  try {
    const text = await request.text();
    if (text.length > MAX_BODY) throw new Error("too large");
    body = JSON.parse(text);
  } catch {
    return errorResponse(400, "INVALID_ARGUMENT", "Invalid request.");
  }
  const code =
    typeof body?.code === "string" ? body.code.replace(/\s+/g, "") : "";
  if (!CODE.test(code)) return answer({ error: "code" }, 400);

  const kept = keptLogin(request, body.flow);
  if (!kept) return answer({ redirectTo: signInErrorPath("verify") });
  const { config, cookie, transaction } = kept;
  // Ended, or never waiting for a code: start again.
  const ended = (reason: "verify" | "denied" | "callback" | "sso") => {
    const response = answer({
      redirectTo: signInErrorPath(reason, transaction?.returnPath),
    });
    expireCookie(response, cookie, config);
    return response;
  };
  if (!transaction || transaction.verifyEmail === undefined)
    return ended("verify");

  let verified: VerifyEmailResponse;
  try {
    const req: VerifyEmailRequest = {
      sessionId: transaction.sessionId,
      code,
    };
    const res = await fetch(
      new URL("/v1/auth/verify-email", config.wfeOrigin),
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(req),
        cache: "no-store",
        redirect: "manual",
      },
    );
    if (!res.ok) {
      const failure = verifyFailure(
        res.status,
        await res.json().catch(() => null),
      );
      if (failure === "code") return answer({ error: "code" }, 400);
      if (failure === "unavailable")
        return answer({ error: "unavailable" }, 503);
      return ended(failure === "denied" ? "denied" : "verify");
    }
    verified = await res.json();
  } catch {
    return answer({ error: "unavailable" }, 503);
  }

  if (verified.ssoRequired === true) {
    const sso = await throughSso(
      request,
      config,
      transaction,
      verified.email,
      cookie,
    );
    if (!sso) return ended("sso");
    const response = answer({ redirectTo: sso.to.toString() });
    sso.set(response);
    return response;
  }
  if (!(await finishSignIn(request, verified))) return ended("callback");
  const response = answer({ redirectTo: transaction.returnPath });
  expireCookie(response, cookie, config);
  return response;
}
