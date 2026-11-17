import { type NextRequest, NextResponse } from "next/server";
import { authConfig } from "@/server/config";
import { setLoginCookie, signInErrorPath, startLogin } from "@/server/sign-in";
import { parseReturnPath } from "@/shared/return-path";
import type { AuthProvider } from "@/shared/wfe";

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

function fail(
  request: NextRequest,
  reason: "request" | "unavailable",
  returnPath?: string,
) {
  return NextResponse.redirect(
    new URL(signInErrorPath(reason, returnPath), request.url),
    { status: 303, headers: NO_STORE },
  );
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

  const started = await startLogin(config, { provider, returnPath });
  if (!started) return unavailable();
  const response = NextResponse.redirect(started.authorize, {
    status: 307,
    headers: NO_STORE,
  });
  setLoginCookie(response, request, config, started);
  return response;
}
