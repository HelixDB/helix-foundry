import type { LocalConfig } from "./config";
import { csrfCookieName } from "./cookies";
import { errorResponse, handlePapiRequest } from "./papi-handler";

// /papi in a local build (src/shared/deployment.ts). Every request with the
// expected Host is the owner's: it is forwarded with the token the API shares
// with this site, under the same allowlist, origin and CSRF checks as the
// hosted site. The token never expires and is never refreshed, so the API
// refusing it is a setup error that no sign-in would fix: the 401 is passed on.
export async function handleLocalPapiRequest(
  request: Request,
  segments: string[],
  config: LocalConfig,
  upstream: typeof fetch,
): Promise<Response> {
  if (request.headers.get("host") !== new URL(config.origin).host)
    return errorResponse(
      403,
      "HOST_REJECTED",
      `Open Foundry at ${config.origin}: it answers no other address.`,
    );
  // The standalone server builds request.url from the address it listens on
  // (0.0.0.0 in a container), not the Host header, so the origin check would
  // refuse every write. The Host is checked: rebuild the URL on the origin.
  const { pathname, search } = new URL(request.url);
  const canonical = new Request(new URL(pathname + search, config.origin), {
    method: request.method,
    headers: request.headers,
    body: request.body,
    duplex: "half",
  } as RequestInit);
  return handlePapiRequest(canonical, segments, {
    authenticate: async () => ({
      kind: "authenticated",
      accessToken: config.token,
      sessionId: "local",
    }),
    refresh: async () => ({ kind: "unauthenticated" }),
    signOut: (response) => response,
    fetch: upstream,
    wfeOrigin: config.wfeOrigin,
    csrfSecret: config.csrfSecret,
    // Loopback http: browsers refuse the __Host- prefix without https.
    csrfCookie: csrfCookieName({ secure: false }),
  });
}
