import { getWorkOS, saveSession } from "@workos-inc/authkit-nextjs";
import { after, type NextRequest, type NextResponse } from "next/server";
import type { SignInError } from "@/web/sign-in";
import {
  FOUNDRY_APPLICATION,
  type AuthProvider,
  type StartLoginRequest,
  type StartLoginResponse,
} from "@/shared/wfe";
import { type AuthConfig, isTrustedUrl } from "./config";
import { cookieOptions, expireCookie, loginCookiePrefix } from "./cookies";
import {
  LOGIN_TRANSACTION_TTL_SECONDS,
  loginCookieName,
  loginFlowId,
  sealLoginTransaction,
  type LoginTransaction,
} from "./login-transaction";
import { remindersConfig, stopReminders } from "./reminders";

// The steps of a sign-in that /auth/login, /login/callback and
// /auth/verify-email share: starting one at the WFE (a provider's, or the
// email's single sign-on when its company requires it), and turning the
// tokens the WFE hands back into the sealed AuthKit session.

/** A login the WFE started: where to send the browser and its cookie. */
export type StartedLogin = {
  authorize: URL;
  cookieName: string;
  cookieValue: string;
};

/**
 * Starts a login for `provider` at the WFE, as the Foundry application.
 * AUTH_PROVIDER_SSO needs `email`, which picks the company's IdP. Null when
 * the WFE refuses or fails, or answers anything unsafe.
 */
export async function startLogin(
  config: AuthConfig,
  login: { provider: AuthProvider; email?: string; returnPath: string },
): Promise<StartedLogin | null> {
  let started: StartLoginResponse;
  try {
    const body: StartLoginRequest = {
      redirectUri: config.redirectUri.toString(),
      provider: login.provider,
      application: FOUNDRY_APPLICATION,
      ...(login.email && { email: login.email }),
    };
    const res = await fetch(new URL("/v1/auth/login:start", config.wfeOrigin), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
      redirect: "manual",
    });
    if (!res.ok) return null;
    started = await res.json();
  } catch {
    return null;
  }
  if (typeof started.url !== "string" || typeof started.sessionId !== "string")
    return null;
  let authorize: URL;
  try {
    authorize = new URL(started.url);
  } catch {
    return null;
  }
  if (!isTrustedUrl(authorize)) return null;
  const flowId = loginFlowId(authorize.searchParams.get("state") || ""),
    cookieName = flowId && loginCookieName(loginCookiePrefix(config), flowId),
    transaction: LoginTransaction = {
      sessionId: started.sessionId,
      returnPath: login.returnPath,
      ...(login.provider === "AUTH_PROVIDER_SSO" && { sso: true }),
    },
    cookieValue = sealLoginTransaction(transaction, config.cookiePassword);
  if (!cookieName || !cookieValue) return null;
  return { authorize, cookieName, cookieValue };
}

/**
 * Sends the browser through the single sign-on of `email`'s company: a new
 * login that replaces `cookie`'s. A login that already went through it, or
 * one the WFE can't start (no SSO set up for the domain), is "sso".
 */
export async function throughSso(
  request: NextRequest,
  config: AuthConfig,
  transaction: LoginTransaction,
  email: string | undefined,
  cookie: string,
): Promise<{ to: URL; set: (response: NextResponse) => void } | null> {
  if (transaction.sso || !email) return null;
  const started = await startLogin(config, {
    provider: "AUTH_PROVIDER_SSO",
    email,
    returnPath: transaction.returnPath,
  });
  if (!started) return null;
  return {
    to: started.authorize,
    set: (response) =>
      setLoginCookie(response, request, config, started, cookie),
  };
}

// Earlier sign-ins still pending that a new one keeps, for other tabs.
const KEPT_LOGINS = 2;

/**
 * Sets a started login's cookie on `response`. An abandoned sign-in's cookie
 * stays until it expires, so a page starting sign-in after sign-in could
 * grow the Cookie header until the site refuses every request: only the
 * newest few stay (browsers send the oldest first), and `replaces`, the
 * sign-in this one takes over from, goes.
 */
export function setLoginCookie(
  response: NextResponse,
  request: NextRequest,
  config: AuthConfig,
  started: StartedLogin,
  replaces?: string | null,
) {
  response.cookies.set({
    name: started.cookieName,
    value: started.cookieValue,
    ...cookieOptions(config),
    maxAge: LOGIN_TRANSACTION_TTL_SECONDS,
  });
  const prefix = loginCookiePrefix(config);
  const others = request.cookies
    .getAll()
    .filter(
      (c) =>
        c.name.startsWith(prefix) &&
        c.name !== started.cookieName &&
        c.name !== replaces,
    );
  for (const c of others.slice(0, -KEPT_LOGINS))
    expireCookie(response, c.name, config);
  if (replaces) expireCookie(response, replaces, config);
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

/**
 * Seals the WFE's tokens into the AuthKit session cookie. Once signed in,
 * the landing page's reminders to that address stop (./reminders.ts), after
 * the answer, so a sign-in never waits on them. False when the tokens are
 * incomplete or WorkOS doesn't know the user.
 */
export async function finishSignIn(
  request: NextRequest,
  tokens: { accessToken?: string; refreshToken?: string },
): Promise<boolean> {
  const { accessToken, refreshToken } = tokens;
  const userId = accessToken ? subject(accessToken) : null;
  if (!accessToken || !refreshToken || !userId) return false;
  let email: string;
  try {
    const user = await getWorkOS().userManagement.getUser(userId);
    await saveSession({ accessToken, refreshToken, user }, request);
    email = user.email;
  } catch {
    return false;
  }
  after(() => stopReminders(remindersConfig(), email));
  return true;
}

/** The sign-in page saying why, keeping where to land afterwards. */
export function signInErrorPath(reason: SignInError, returnPath?: string) {
  const q = new URLSearchParams({ error: reason });
  if (returnPath && returnPath !== "/") q.set("return_to", returnPath);
  return `/login?${q}`;
}

/**
 * Why VerifyEmail refused a code, from grpc-gateway's status and the gRPC
 * code in its body: `code` a wrong code (INVALID_ARGUMENT, the session
 * kept for another try), `unavailable` WorkOS or the WFE being away (the
 * session kept too), `denied` an email this stamp doesn't let in, and
 * `expired` anything else (UNAUTHENTICATED: expired, unknown or too many
 * tries), which needs a new sign-in.
 *
 * @example
 * verifyFailure(400, { code: 3 }); // "code"
 * verifyFailure(401, { code: 16 }); // "expired"
 */
export function verifyFailure(
  status: number,
  body: unknown,
): "code" | "unavailable" | "denied" | "expired" {
  const code =
    body && typeof body === "object" && "code" in body
      ? (body as { code: unknown }).code
      : undefined;
  if (status >= 500 || code === 14) return "unavailable";
  if (code === 3) return "code";
  if (status === 403 || code === 7) return "denied";
  return "expired";
}
