import {
  CSRF_HEADER_NAME,
  CSRF_REJECTED_CODE,
  CSRF_TOKEN_TTL_SECONDS,
  type CsrfTokenResponse,
} from "@/shared/csrf-contract";
import { SESSION_EXPIRES_HEADER } from "@/shared/session-contract";
import { allowedMethods, findPapiRoute } from "./papi-allowlist";
import {
  createCsrfToken,
  csrfAccepted,
  isUnsafeMethod,
  sameOrigin,
} from "./csrf";

// The /papi BFF: the browser's only way to the WFE. In order, a request is
// refused unless (1) its path is a plain WFE path on the allowlist, (2) it
// has a session, (3) an unsafe method comes from this origin with a valid CSRF
// token. It is then forwarded with the session's bearer token, and retried
// once after a refresh if the WFE answers 401. Every answer that used a token
// says how long it has left (SESSION_EXPIRES_HEADER). Everything it needs is
// passed in, so tests drive it without Next or WorkOS.

export type BrowserSession =
  | { kind: "authenticated"; accessToken: string; sessionId: string }
  | { kind: "unauthenticated" }
  | { kind: "transient" };

export type PapiDependencies = {
  // Resolves the session; only called for allowlisted paths.
  authenticate: () => Promise<BrowserSession>;
  // Spends the refresh token; called at most once per request.
  refresh: () => Promise<BrowserSession>;
  // Expires the session and CSRF cookies on the response.
  signOut: (response: Response) => Response;
  fetch: typeof fetch;
  wfeOrigin: string;
  csrfSecret: string;
  csrfCookie: string;
};

const REQUEST_HEADERS = new Set([
  "accept",
  "accept-language",
  "content-type",
  "if-match",
  "if-none-match",
  "range",
]);
const RESPONSE_HEADERS = new Set([
  "cache-control",
  "content-disposition",
  "content-language",
  "content-range",
  "content-type",
  "etag",
  "last-modified",
  "retry-after",
  "vary",
  "x-request-id",
]);
const SEGMENT = /^[A-Za-z0-9._:-]+$/;

const NO_STORE = { "Cache-Control": "no-store" } as const;
export function errorResponse(status: number, code: string, message: string) {
  return Response.json(
    { code, message, details: [] },
    { status, headers: NO_STORE },
  );
}

/** "/v1/..." from the catch-all segments, or null for anything odd. */
export function papiPath(segments: string[]): string | null {
  if (
    !segments.length ||
    segments.some((s) => !SEGMENT.test(s) || s === "." || s === "..")
  )
    return null;
  return "/" + segments.join("/");
}

/** Puts the access token's remaining seconds, from its `exp`, on `response`. */
function withExpiry(response: Response, accessToken: string) {
  try {
    const { exp } = JSON.parse(
      Buffer.from(accessToken.split(".")[1] || "", "base64url").toString(),
    ) as { exp?: unknown };
    if (typeof exp === "number")
      response.headers.set(
        SESSION_EXPIRES_HEADER,
        String(Math.max(0, Math.floor(exp - Date.now() / 1000))),
      );
  } catch {
    // Not a JWT: the SPA keeps sending its calls one at a time.
  }
  return response;
}

function pick(headers: Headers, allowed: Set<string>) {
  const out = new Headers();
  for (const [name, value] of headers)
    if (allowed.has(name.toLowerCase())) out.append(name, value);
  return out;
}

// Connector tests wait up to 30 s for a customer's database; a minute covers
// that and still frees the function when the WFE hangs.
const UPSTREAM_TIMEOUT_MS = 60_000;

function relay(upstream: Response) {
  const headers = pick(upstream.headers, RESPONSE_HEADERS);
  headers.set("Cache-Control", "no-store");
  // A redirect from the WFE is never followed or passed on.
  if (upstream.status >= 300 && upstream.status < 400)
    return errorResponse(502, "UPSTREAM_REDIRECT", "Unexpected API redirect.");
  const empty = upstream.status === 204 || upstream.status === 304;
  return new Response(empty ? null : upstream.body, {
    status: upstream.status,
    statusText: upstream.statusText,
    headers,
  });
}

/** The WFE's answer, relayed, or the BFF's own error when it gave none. */
async function forward(
  request: Request,
  body: ArrayBuffer | undefined,
  path: string,
  accessToken: string,
  deps: PapiDependencies,
) {
  const target = new URL(path, deps.wfeOrigin);
  target.search = new URL(request.url).search;
  const headers = pick(request.headers, REQUEST_HEADERS);
  headers.set("Authorization", `Bearer ${accessToken}`);
  try {
    return relay(
      await deps.fetch(target, {
        method: request.method.toUpperCase(),
        headers,
        body,
        redirect: "manual",
        cache: "no-store",
        signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
      }),
    );
  } catch (error) {
    return error instanceof DOMException && error.name === "TimeoutError"
      ? errorResponse(
          504,
          "UPSTREAM_TIMEOUT",
          "The API took too long to answer.",
        )
      : errorResponse(502, "UPSTREAM_UNAVAILABLE", "The API is unavailable.");
  }
}

export async function handlePapiRequest(
  request: Request,
  segments: string[],
  deps: PapiDependencies,
): Promise<Response> {
  const path = papiPath(segments);
  if (!path) return errorResponse(404, "NOT_FOUND", "API route not found.");
  const method = request.method.toUpperCase();
  const csrfRoute = path === "/csrf";
  if (csrfRoute && method !== "GET")
    return new Response(null, { status: 405, headers: { Allow: "GET" } });
  if (!csrfRoute && !findPapiRoute(method, path)) {
    const allow = allowedMethods(path);
    return allow.length
      ? new Response(null, {
          status: 405,
          headers: { Allow: allow.join(", ") },
        })
      : errorResponse(404, "NOT_FOUND", "API route not found.");
  }
  if (isUnsafeMethod(method) && !sameOrigin(request))
    return errorResponse(403, "ORIGIN_REJECTED", "Request origin rejected.");

  const session = await deps.authenticate();
  if (session.kind === "transient")
    return errorResponse(
      503,
      "AUTH_REFRESH_UNAVAILABLE",
      "Authentication is temporarily unavailable.",
    );
  if (session.kind === "unauthenticated")
    return errorResponse(401, "UNAUTHENTICATED", "Authentication required.");

  if (csrfRoute) {
    const token = createCsrfToken(deps.csrfSecret, session.sessionId);
    const body: CsrfTokenResponse = {
      token,
      expiresInSeconds: CSRF_TOKEN_TTL_SECONDS,
    };
    const secure = deps.csrfCookie.startsWith("__Host-") ? "; Secure" : "";
    return withExpiry(
      Response.json(body, {
        headers: {
          ...NO_STORE,
          "Set-Cookie": `${deps.csrfCookie}=${encodeURIComponent(token)}; Path=/; HttpOnly${secure}; SameSite=Lax; Max-Age=${CSRF_TOKEN_TTL_SECONDS}`,
        },
      }),
      session.accessToken,
    );
  }
  if (
    isUnsafeMethod(method) &&
    !csrfAccepted(
      request,
      deps.csrfCookie,
      CSRF_HEADER_NAME,
      deps.csrfSecret,
      session.sessionId,
    )
  )
    return errorResponse(403, CSRF_REJECTED_CODE, "CSRF validation failed.");

  const body =
    method === "GET" || method === "HEAD"
      ? undefined
      : await request.arrayBuffer();
  const first = await forward(request, body, path, session.accessToken, deps);
  if (first.status !== 401) return withExpiry(first, session.accessToken);

  const refreshed = await deps.refresh();
  if (refreshed.kind === "transient")
    return errorResponse(
      503,
      "AUTH_REFRESH_UNAVAILABLE",
      "Authentication is temporarily unavailable.",
    );
  if (refreshed.kind === "unauthenticated") return deps.signOut(first);
  return withExpiry(
    await forward(request, body, path, refreshed.accessToken, deps),
    refreshed.accessToken,
  );
}
