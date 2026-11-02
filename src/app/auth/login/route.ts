import { type NextRequest, NextResponse } from "next/server";
import { authConfig, isTrustedUrl } from "@/server/config";
import {
  cookieOptions,
  expireCookie,
  loginCookiePrefix,
} from "@/server/cookies";
import {
  LOGIN_TRANSACTION_TTL_SECONDS,
  loginCookieName,
  loginFlowId,
  sealLoginTransaction,
} from "@/server/login-transaction";
import { parseReturnPath } from "@/shared/return-path";
import {
  FOUNDRY_APPLICATION,
  type AuthProvider,
  type StartLoginRequest,
  type StartLoginResponse,
} from "@/shared/wfe";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

// GET /auth/login?provider=github&return_to=/data. The WFE builds the WorkOS
// authorize URL for the Foundry application and keeps the PKCE verifier; this
// route remembers the pending login in a signed cookie and redirects. The
// sign-in page navigates here, so a failure lands back on it with
// ?error=<reason>, as the callback's do.

const PROVIDERS: Record<string, AuthProvider> = {
  github: "AUTH_PROVIDER_GITHUB",
  google: "AUTH_PROVIDER_GOOGLE",
  microsoft: "AUTH_PROVIDER_MICROSOFT",
};

const NO_STORE = { "Cache-Control": "no-store" } as const;

// Earlier sign-ins still pending that a new one keeps, for other tabs.
const KEPT_LOGINS = 2;

function fail(
  request: NextRequest,
  reason: "request" | "unavailable",
  returnPath?: string,
) {
  const to = new URL("/login", request.url);
  to.searchParams.set("error", reason);
  if (returnPath && returnPath !== "/")
    to.searchParams.set("return_to", returnPath);
  return NextResponse.redirect(to, { status: 303, headers: NO_STORE });
}

export async function GET(request: NextRequest) {
  const params = request.nextUrl.searchParams;
  const providers = params.getAll("provider"),
    provider = PROVIDERS[providers[0] ?? ""];
  if (providers.length !== 1 || !provider) return fail(request, "request");
  const returns = params.getAll("return_to"),
    returnPath = returns.length ? parseReturnPath(returns[0]) : "/";
  if (returns.length > 1 || !returnPath) return fail(request, "request");
  const unavailable = () => fail(request, "unavailable", returnPath);

  const config = authConfig();
  if (!config) return unavailable();
  // Sign-in starts on the callback's origin so its cookie comes back there.
  if (request.nextUrl.origin !== config.redirectUri.origin) {
    const canonical = new URL(
      request.nextUrl.pathname + request.nextUrl.search,
      config.redirectUri.origin,
    );
    return NextResponse.redirect(canonical, { status: 307, headers: NO_STORE });
  }

  let started: StartLoginResponse;
  try {
    const body: StartLoginRequest = {
      redirectUri: config.redirectUri.toString(),
      provider,
      application: FOUNDRY_APPLICATION,
    };
    const res = await fetch(new URL("/v1/auth/login:start", config.wfeOrigin), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      cache: "no-store",
      redirect: "manual",
    });
    if (!res.ok) return unavailable();
    started = await res.json();
  } catch {
    return unavailable();
  }
  if (typeof started.url !== "string" || typeof started.sessionId !== "string")
    return unavailable();
  let authorize: URL;
  try {
    authorize = new URL(started.url);
  } catch {
    return unavailable();
  }
  if (!isTrustedUrl(authorize)) return unavailable();

  const prefix = loginCookiePrefix(config),
    flowId = loginFlowId(authorize.searchParams.get("state") || ""),
    name = flowId && loginCookieName(prefix, flowId),
    sealed = sealLoginTransaction(
      { sessionId: started.sessionId, returnPath },
      config.cookiePassword,
    );
  if (!name || !sealed) return unavailable();
  const response = NextResponse.redirect(authorize, {
    status: 307,
    headers: NO_STORE,
  });
  response.cookies.set({
    name,
    value: sealed,
    ...cookieOptions(config),
    maxAge: LOGIN_TRANSACTION_TTL_SECONDS,
  });
  // An abandoned sign-in's cookie stays until it expires, so a page starting
  // sign-in after sign-in could grow the Cookie header until the site refuses
  // every request. Only the newest few stay; browsers send the oldest first.
  request.cookies
    .getAll()
    .filter((c) => c.name.startsWith(prefix) && c.name !== name)
    .slice(0, -KEPT_LOGINS)
    .forEach((c) => expireCookie(response, c.name, config));
  return response;
}
