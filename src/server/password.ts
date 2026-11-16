import { type NextRequest, NextResponse } from "next/server";
import { parseReturnPath } from "@/shared/return-path";
import {
  FOUNDRY_APPLICATION,
  type PasswordAuthResponse,
  type RequestPasswordResetRequest,
  type ResetPasswordRequest,
  type SignInWithPasswordRequest,
  type SignUpWithPasswordRequest,
} from "@/shared/wfe";
import type { PasswordAnswer } from "@/web/password";
import { type AuthConfig, authConfig } from "./config";
import { loginCookiePrefix } from "./cookies";
import { sameOrigin } from "./csrf";
import {
  loginCookieName,
  loginFlowId,
  sealLoginTransaction,
} from "./login-transaction";
import { errorResponse } from "./papi-handler";
import {
  finishSignIn,
  setLoginCookie,
  signInErrorPath,
  throughSso,
} from "./sign-in";

// Signing in with an email and password, through the WFE's password calls
// as the Foundry application. The browser POSTs its form (same origin) to
// /auth/password/<action>; the password goes to the WFE and nowhere else.
//
// sign-in { email, password, returnTo } and sign-up { name, email, password,
// returnTo } answer { redirectTo }: the page the sign-in started from once
// signed in, /login/verify when WorkOS emailed a code first (the login kept
// in a cookie as the callback keeps one, so /auth/verify-email takes it), or
// the company's single sign-on when it requires it. forgot { email } answers
// { sent } (WorkOS emails a link to /reset-password?token=, and says the same
// for an address without an account); reset { token, password } { reset }.
// Anything refused is { error } (PasswordAnswer).

export type PasswordAction = "sign-in" | "sign-up" | "forgot" | "reset";

const NO_STORE = { "Cache-Control": "no-store" } as const;
const MAX_BODY = 4096,
  MAX_EMAIL = 320,
  MAX_PASSWORD = 1024,
  MAX_NAME = 128,
  MAX_TOKEN = 2048,
  MAX_MESSAGE = 300;

// grpc-gateway's codes in a refusal's body.
const INVALID_ARGUMENT = 3,
  ALREADY_EXISTS = 6,
  PERMISSION_DENIED = 7,
  FAILED_PRECONDITION = 9,
  UNAUTHENTICATED = 16;

const answer = (body: PasswordAnswer, status = 200) =>
  NextResponse.json(body, { status, headers: NO_STORE });

const STATUS = {
  credentials: 401,
  denied: 403,
  exists: 409,
  refused: 400,
  unavailable: 503,
} as const;
const refuse = (refusal: Extract<PasswordAnswer, { error: string }>) =>
  answer(refusal, STATUS[refusal.error]);

/**
 * The WFE's message as a sentence: its clauses capitalised and stopped,
 * bounded. WorkOS's password rules come as sentences already.
 *
 * @example
 * sentence("reset link is invalid or expired"); // "Reset link is invalid or expired."
 */
export function sentence(message: string): string {
  const text = message
    .trim()
    .slice(0, MAX_MESSAGE)
    .split(/;\s+/)
    .map((c) => c.charAt(0).toUpperCase() + c.slice(1))
    .join(". ");
  return /[.!?]$/.test(text) ? text : `${text}.`;
}

/**
 * What the page says about a call the WFE refused, by its gRPC code and
 * message: `credentials` a wrong email or password (sign-in only, never
 * saying which), `exists` an email with an account (sign-up), `denied` an
 * email this stamp doesn't let in, `refused` the WFE's own reason (a
 * password WorkOS's rules refuse, a reset link that ran out), and
 * `unavailable` anything else.
 *
 * @example
 * refusal("sign-in", 16, "invalid email or password"); // { error: "credentials" }
 * refusal("sign-up", 6, "…"); // { error: "exists" }
 */
export function refusal(
  action: PasswordAction,
  code: number,
  message: string,
): Extract<PasswordAnswer, { error: string }> {
  if (code === PERMISSION_DENIED) return { error: "denied" };
  if (action === "sign-in" && code === UNAUTHENTICATED)
    return { error: "credentials" };
  if (action === "sign-up" && code === ALREADY_EXISTS)
    return { error: "exists" };
  if (
    message.trim() &&
    [INVALID_ARGUMENT, FAILED_PRECONDITION, UNAUTHENTICATED].includes(code)
  )
    return { error: "refused", message: sentence(message) };
  return { error: "unavailable" };
}

type Called =
  | { kind: "ok"; body: unknown }
  | { kind: "refused"; code: number; message: string }
  | { kind: "unavailable" };

async function callWfe(
  config: AuthConfig,
  path: string,
  body: unknown,
): Promise<Called> {
  try {
    const res = await fetch(new URL(path, config.wfeOrigin), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
      redirect: "manual",
    });
    const json: unknown = await res.json().catch(() => null);
    if (res.ok) return { kind: "ok", body: json };
    if (res.status >= 500 || res.status === 429) return { kind: "unavailable" };
    const fields = (json && typeof json === "object" ? json : {}) as {
      code?: unknown;
      message?: unknown;
    };
    return {
      kind: "refused",
      code: typeof fields.code === "number" ? fields.code : 0,
      message: typeof fields.message === "string" ? fields.message : "",
    };
  } catch {
    return { kind: "unavailable" };
  }
}

/** A form field: a string of at most `max` characters, else null. */
function field(form: Record<string, unknown>, key: string, max: number) {
  const value = form[key];
  return typeof value === "string" && value.length <= max ? value : null;
}

/** "Ada Lovelace" as WorkOS's first and last name; null when blank. */
function splitName(raw: string) {
  const [firstName = "", ...rest] = raw.trim().split(/\s+/);
  if (!firstName) return null;
  return rest.length
    ? { firstName, lastName: rest.join(" ") }
    : { firstName };
}

/**
 * Where a password sign-in goes once the WFE has answered it: tokens sign
 * in, a code WorkOS emailed keeps the login for /login/verify, and a company
 * that requires its single sign-on starts a login through it.
 */
async function signedIn(
  request: NextRequest,
  config: AuthConfig,
  auth: PasswordAuthResponse,
  returnPath: string,
) {
  if (auth.ssoRequired === true) {
    const sso = await throughSso(
      request,
      config,
      { returnPath },
      auth.email,
      null,
    );
    if (!sso) return answer({ redirectTo: signInErrorPath("sso", returnPath) });
    const response = answer({ redirectTo: sso.to.toString() });
    sso.set(response);
    return response;
  }
  if (auth.emailVerificationRequired === true) {
    const sessionId = auth.sessionId ?? "",
      flow = loginFlowId(sessionId),
      cookieName = flow && loginCookieName(loginCookiePrefix(config), flow),
      cookieValue = sealLoginTransaction(
        { sessionId, returnPath, verifyEmail: auth.email ?? "" },
        config.cookiePassword,
      );
    if (!flow || !cookieName || !cookieValue)
      return refuse({ error: "unavailable" });
    const response = answer({
      redirectTo: `/login/verify?${new URLSearchParams({ flow })}`,
    });
    setLoginCookie(response, request, config, { cookieName, cookieValue });
    return response;
  }
  if (!(await finishSignIn(request, auth)))
    return refuse({ error: "unavailable" });
  return answer({ redirectTo: returnPath });
}

/** The /auth/password/<action> routes. */
export async function handlePassword(
  request: NextRequest,
  action: PasswordAction,
): Promise<Response> {
  if (!sameOrigin(request))
    return errorResponse(403, "ORIGIN_REJECTED", "Request origin rejected.");
  let form: Record<string, unknown>;
  try {
    const text = await request.text();
    if (text.length > MAX_BODY) throw new Error("too large");
    const parsed: unknown = JSON.parse(text);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed))
      throw new Error("not an object");
    form = parsed as Record<string, unknown>;
  } catch {
    return errorResponse(400, "INVALID_ARGUMENT", "Invalid request.");
  }
  const config = authConfig();
  if (!config) return refuse({ error: "unavailable" });

  const email = field(form, "email", MAX_EMAIL)?.trim() ?? "",
    password = field(form, "password", MAX_PASSWORD) ?? "",
    validEmail = email.includes("@");
  const invalid = (message: string) => refuse({ error: "refused", message });

  if (action === "forgot") {
    if (!validEmail) return invalid("Enter your email.");
    const body: RequestPasswordResetRequest = {
      email,
      application: FOUNDRY_APPLICATION,
    };
    const called = await callWfe(config, "/v1/auth/password:forgot", body);
    if (called.kind === "ok") return answer({ sent: true });
    return refuse(
      called.kind === "refused"
        ? refusal(action, called.code, called.message)
        : { error: "unavailable" },
    );
  }

  if (!password) return invalid("Enter a password.");
  if (action === "reset") {
    const token = field(form, "token", MAX_TOKEN)?.trim() ?? "";
    if (!token)
      return invalid("This reset link is incomplete. Request a new one.");
    const body: ResetPasswordRequest = { token, newPassword: password };
    const called = await callWfe(config, "/v1/auth/password:reset", body);
    if (called.kind === "ok") return answer({ reset: true });
    return refuse(
      called.kind === "refused"
        ? refusal(action, called.code, called.message)
        : { error: "unavailable" },
    );
  }

  if (!validEmail) return invalid("Enter your email.");
  const returnPath =
    parseReturnPath(field(form, "returnTo", MAX_BODY)) ?? "/";
  let called: Called;
  if (action === "sign-up") {
    const name = splitName(field(form, "name", MAX_NAME) ?? "");
    if (!name) return invalid("Enter your name.");
    const body: SignUpWithPasswordRequest = {
      email,
      password,
      ...name,
      application: FOUNDRY_APPLICATION,
    };
    called = await callWfe(config, "/v1/auth/password:sign-up", body);
  } else {
    const body: SignInWithPasswordRequest = {
      email,
      password,
      application: FOUNDRY_APPLICATION,
    };
    called = await callWfe(config, "/v1/auth/password:sign-in", body);
  }
  if (called.kind === "unavailable") return refuse({ error: "unavailable" });
  if (called.kind === "refused")
    return refuse(refusal(action, called.code, called.message));
  return signedIn(
    request,
    config,
    (called.body ?? {}) as PasswordAuthResponse,
    returnPath,
  );
}
