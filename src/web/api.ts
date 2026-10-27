import {
  CSRF_HEADER_NAME,
  CSRF_REJECTED_CODE,
  CSRF_RENEWAL_WINDOW_SECONDS,
  type CsrfTokenResponse,
} from "../shared/csrf-contract";
import { LOCAL } from "../shared/deployment";
import { signInPath } from "../shared/return-path";
import { SESSION_EXPIRES_HEADER } from "../shared/session-contract";

// Every WFE call goes through the same-origin /papi BFF, which adds the
// session's bearer token: `api("/v1/whoami")` reaches the WFE's /v1/whoami.
// Unsafe methods carry the CSRF token, renewed once if the BFF refuses it.
// A 401 means the session is gone, so the app returns to sign-in. A local
// build (src/shared/deployment.ts) has no session to lose: its token never
// runs out, and a 401 is reported like any other refusal.

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    // The BFF's code ("CSRF_REJECTED"), or the WFE's numeric gRPC code.
    readonly code?: string | number,
    // The WFE's RpcStatus.details, e.g. a google.rpc.ErrorInfo with a reason.
    readonly details?: unknown[],
  ) {
    super(message);
  }
}

// When the session's access token is out, or nearly, or not known yet, calls
// take turns under a Web Lock (shared by this origin's tabs) until one reports
// a fresh token, so the BFF spends the single-use refresh token once; see
// SESSION_EXPIRES_HEADER. `sessionExpiresAt` is on this tab's clock. The
// margin keeps a call from reaching the BFF after its token ran out; a token
// that is only nearly out is refreshed safely by parallel calls.
const SESSION_LOCK = "foundry-session",
  SESSION_MARGIN_MS = 10_000;
let sessionExpiresAt = 0;

async function papiFetch(url: string, init: RequestInit): Promise<Response> {
  if (LOCAL) return fetch(url, init);
  const fresh = () => Date.now() < sessionExpiresAt - SESSION_MARGIN_MS;
  const run = async () => {
    const res = await fetch(url, init),
      left = Number(res.headers.get(SESSION_EXPIRES_HEADER));
    if (res.status === 401) sessionExpiresAt = 0;
    else if (left > 0)
      // Answers can arrive out of order: an older token's must not win.
      sessionExpiresAt = Math.max(sessionExpiresAt, Date.now() + left * 1000);
    return res;
  };
  if (fresh()) return run();
  const alone = await navigator.locks.request(SESSION_LOCK, () =>
    fresh() ? null : run(),
  );
  return alone ?? run();
}

// One CSRF token per tab, fetched once and shared by concurrent requests.
// `generation` drops a token that arrives after clearCsrfToken().
let csrf: { token: string; renewAt: number } | null = null,
  csrfRequest: Promise<string> | null = null,
  generation = 0;

async function csrfToken(): Promise<string> {
  if (csrf && Date.now() < csrf.renewAt) return csrf.token;
  if (csrfRequest) return csrfRequest;
  const started = generation;
  const request = papiFetch("/papi/csrf", {
    credentials: "same-origin",
    cache: "no-store",
  })
    .then(async (res) => {
      const data = (await res.json().catch(() => null)) as
        | (Partial<CsrfTokenResponse> & { message?: string })
        | null;
      if (!res.ok || typeof data?.token !== "string")
        throw new ApiError(
          data?.message || "Couldn’t secure the request.",
          res.status,
        );
      const seconds = Math.max(
        0,
        (data.expiresInSeconds || 0) - CSRF_RENEWAL_WINDOW_SECONDS,
      );
      if (started === generation)
        csrf = { token: data.token, renewAt: Date.now() + seconds * 1000 };
      return data.token;
    })
    .finally(() => {
      if (csrfRequest === request) csrfRequest = null;
    });
  csrfRequest = request;
  return request;
}

/** Forgets the CSRF token, e.g. after signing out. */
export function clearCsrfToken() {
  generation++;
  csrf = null;
  csrfRequest = null;
}

async function send(url: string, init: RequestInit, unsafe: boolean) {
  if (!unsafe) return papiFetch(url, init);
  const attempt = async (token: string) =>
    papiFetch(url, {
      ...init,
      headers: { ...(init.headers as object), [CSRF_HEADER_NAME]: token },
    });
  const token = await csrfToken(),
    res = await attempt(token);
  if (res.status !== 403) return res;
  const body = await res
    .clone()
    .json()
    .catch(() => null);
  if (body?.code !== CSRF_REJECTED_CODE) return res;
  if (csrf?.token === token) csrf = null;
  return attempt(await csrfToken());
}

/** Sends the browser to sign-in, coming back to the current page. */
export function goToSignIn() {
  clearCsrfToken();
  const { pathname, search, hash } = window.location;
  window.location.assign(signInPath(pathname + search + hash));
}

export async function api<T = any>(
  path: string,
  body?: unknown,
  method?: string,
  extraHeaders?: Record<string, string>,
  // `signIn: false` reports a 401 to the caller instead of leaving the page;
  // `signal` aborts the call.
  options: { signIn?: boolean; signal?: AbortSignal } = {},
): Promise<T> {
  const m = (method || (body === undefined ? "GET" : "POST")).toUpperCase(),
    signIn = options.signIn !== false && !LOCAL;
  const res = await send(
    "/papi" + path,
    {
      method: m,
      credentials: "same-origin",
      cache: "no-store",
      headers: {
        ...(body === undefined ? {} : { "content-type": "application/json" }),
        ...extraHeaders,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: options.signal,
    },
    !["GET", "HEAD"].includes(m),
  ).catch((e) => {
    // The CSRF handshake itself found no session.
    if (e instanceof ApiError && e.status === 401 && signIn) goToSignIn();
    throw e;
  });
  const data =
    res.status === 204
      ? undefined
      : await res.json().catch(() => (res.ok ? undefined : null));
  if (res.status === 401 && signIn) goToSignIn();
  if (!res.ok)
    throw new ApiError(
      data?.message || data?.error || "Request failed",
      res.status,
      data?.code,
      Array.isArray(data?.details) ? data.details : undefined,
    );
  return data as T;
}

export const number = (n: number | undefined) =>
  new Intl.NumberFormat("en-US").format(n || 0);
export const relative = (date: string) => {
  const min = Math.floor((Date.now() - Date.parse(date)) / 60000);
  return min < 1
    ? "just now"
    : min < 60
      ? `${min}m ago`
      : min < 1440
        ? `${Math.floor(min / 60)}h ago`
        : new Date(date).toLocaleDateString();
};

/**
 * A WFE message as a sentence. The WFE's own messages are lowercase
 * fragments, which get a capital and a period. Copy it writes as a full
 * sentence (ending in . ! or ?), as for a workspace's own AI provider,
 * stays as written: it may start with a model's name.
 *
 * @example
 * sentence("the load is gone"); // "The load is gone."
 * sentence("claude-opus-5-5 can’t do what this task needs (tool calls or structured answers)."); // unchanged
 */
export const sentence = (message: string) =>
  /[.!?]$/.test(message)
    ? message
    : message.charAt(0).toUpperCase() + message.slice(1) + ".";

/** Ends the session here and at WorkOS, then lands on the sign-in page. */
export async function signOut() {
  let target = "/login";
  try {
    const res = await send(
      "/auth/logout",
      { method: "POST", credentials: "same-origin", cache: "no-store" },
      true,
    );
    const data = await res.json().catch(() => null);
    if (res.ok && typeof data?.logoutUrl === "string") target = data.logoutUrl;
  } catch {
    // Without a session there is nothing to end: go to sign-in anyway.
  }
  clearCsrfToken();
  window.location.assign(target);
}
