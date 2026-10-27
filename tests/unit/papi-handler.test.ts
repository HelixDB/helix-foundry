import { describe, expect, it, vi } from "vitest";
import { createCsrfToken, verifyCsrfToken } from "@/server/csrf";
import {
  handlePapiRequest,
  papiPath,
  type BrowserSession,
  type PapiDependencies,
} from "@/server/papi-handler";

const ORIGIN = "https://foundry.test";
const secret = "x".repeat(32);
const signedIn: BrowserSession = {
  kind: "authenticated",
  accessToken: "token-1",
  sessionId: "sid-1",
};

function deps(over: Partial<PapiDependencies> = {}) {
  const upstream = vi.fn(async (_url: URL | RequestInfo, _init?: RequestInit) =>
    Response.json({ ok: true }, { headers: { "x-request-id": "r1" } }),
  );
  const d: PapiDependencies = {
    authenticate: vi.fn(async () => signedIn),
    refresh: vi.fn(
      async (): Promise<BrowserSession> => ({
        kind: "authenticated",
        accessToken: "token-2",
        sessionId: "sid-1",
      }),
    ),
    signOut: vi.fn((r: Response) => {
      const headers = new Headers(r.headers);
      headers.set("x-signed-out", "1");
      return new Response(r.body, { status: r.status, headers });
    }),
    fetch: upstream as unknown as typeof fetch,
    wfeOrigin: "https://wfe.test",
    csrfSecret: secret,
    csrfCookie: "__Host-foundry-csrf",
    ...over,
  };
  return { d, upstream: (over.fetch as typeof upstream) || upstream };
}

function req(
  path: string,
  init: RequestInit & { csrf?: string | false; origin?: string | null } = {},
) {
  const { csrf, origin = ORIGIN, ...rest } = init;
  const headers = new Headers(rest.headers);
  if (origin) headers.set("origin", origin);
  const token =
    csrf === false ? undefined : (csrf ?? createCsrfToken(secret, "sid-1"));
  if (token) {
    headers.set("x-helix-csrf", token);
    headers.set("cookie", `__Host-foundry-csrf=${token}`);
  }
  return new Request(ORIGIN + "/papi" + path, { ...rest, headers });
}
const segments = (path: string) => path.split("?")[0]!.slice(1).split("/");
const call = (
  path: string,
  init?: Parameters<typeof req>[1],
  over?: Partial<PapiDependencies>,
) => {
  const { d, upstream } = deps(over);
  return handlePapiRequest(req(path, init), segments(path), d).then((res) => ({
    res,
    d,
    upstream,
  }));
};

describe("papiPath", () => {
  it("joins plain segments and refuses anything else", () => {
    expect(papiPath(["v1", "workspaces", "ws_1"])).toBe("/v1/workspaces/ws_1");
    expect(papiPath(["v1", "databases:query-read"])).toBe(
      "/v1/databases:query-read",
    );
    for (const bad of [
      [],
      ["v1", ".."],
      ["v1", "."],
      ["v1", "a/b"],
      ["v1", "a b"],
      ["v1", ""],
    ])
      expect(papiPath(bad)).toBeNull();
  });
});

describe("the /papi BFF", () => {
  it("refuses paths off the allowlist before touching the session", async () => {
    const { res, d, upstream } = await call("/v1/clusters");
    expect(res.status).toBe(404);
    expect((await res.json()).code).toBe("NOT_FOUND");
    expect(d.authenticate).not.toHaveBeenCalled();
    expect(upstream).not.toHaveBeenCalled();
  });

  it("answers 405 with Allow when only the method is wrong", async () => {
    const { res, upstream } = await call("/v1/workspaces/ws_1", {
      method: "DELETE",
    });
    expect(res.status).toBe(405);
    expect(res.headers.get("allow")).toBe("GET");
    expect(upstream).not.toHaveBeenCalled();
  });

  it("needs a session", async () => {
    const out = await call(
      "/v1/whoami",
      {},
      {
        authenticate: async () => ({ kind: "unauthenticated" }),
      },
    );
    expect(out.res.status).toBe(401);
    expect(out.upstream).not.toHaveBeenCalled();
    const transient = await call(
      "/v1/whoami",
      {},
      {
        authenticate: async () => ({ kind: "transient" }),
      },
    );
    expect(transient.res.status).toBe(503);
  });

  it("forwards with the bearer token and only allowlisted headers", async () => {
    const { res, upstream } = await call("/v1/workspaces?page.size=5", {
      headers: {
        authorization: "Bearer from-browser",
        "x-forwarded-for": "1.2.3.4",
        accept: "application/json",
      },
    });
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(res.headers.get("x-request-id")).toBe("r1");
    const [url, init] = upstream.mock.calls[0]!;
    expect(String(url)).toBe("https://wfe.test/v1/workspaces?page.size=5");
    const headers = new Headers(init!.headers);
    expect(headers.get("authorization")).toBe("Bearer token-1");
    expect(headers.get("x-forwarded-for")).toBeNull();
    expect(headers.get("cookie")).toBeNull();
    expect(headers.get("accept")).toBe("application/json");
    expect(init!.redirect).toBe("manual");
  });

  it("forwards an unsafe request with a valid CSRF token and its body", async () => {
    const { res, upstream } = await call("/v1/workspaces", {
      method: "POST",
      body: JSON.stringify({ slug: "x" }),
      headers: { "content-type": "application/json" },
    });
    expect(res.status).toBe(200);
    const init = upstream.mock.calls[0]![1]!;
    expect(init.method).toBe("POST");
    expect(new TextDecoder().decode(init.body as ArrayBuffer)).toBe(
      '{"slug":"x"}',
    );
  });

  it("refuses an unsafe request from another origin, or none", async () => {
    for (const origin of ["https://evil.test", null]) {
      const { res, upstream, d } = await call("/v1/workspaces", {
        method: "POST",
        origin,
      });
      expect(res.status).toBe(403);
      expect((await res.json()).code).toBe("ORIGIN_REJECTED");
      expect(upstream).not.toHaveBeenCalled();
      expect(d.authenticate).not.toHaveBeenCalled();
    }
  });

  it("refuses an unsafe request without a valid CSRF token", async () => {
    const otherSession = createCsrfToken(secret, "sid-2");
    for (const csrf of [false, "v1.1.x.y", otherSession] as const) {
      const { res, upstream } = await call("/v1/workspaces", {
        method: "POST",
        csrf,
      });
      expect(res.status, String(csrf)).toBe(403);
      expect((await res.json()).code).toBe("CSRF_REJECTED");
      expect(upstream).not.toHaveBeenCalled();
    }
    // Header and cookie disagree.
    const { d, upstream } = deps();
    const a = createCsrfToken(secret, "sid-1"),
      b = createCsrfToken(secret, "sid-1");
    const r = new Request(ORIGIN + "/papi/v1/workspaces", {
      method: "POST",
      headers: {
        origin: ORIGIN,
        "x-helix-csrf": a,
        cookie: `__Host-foundry-csrf=${b}`,
      },
    });
    expect((await handlePapiRequest(r, ["v1", "workspaces"], d)).status).toBe(
      403,
    );
    expect(upstream).not.toHaveBeenCalled();
  });

  it("issues a CSRF token bound to the session", async () => {
    const { res } = await call("/csrf", { csrf: false });
    expect(res.status).toBe(200);
    const { token, expiresInSeconds } = await res.json();
    expect(expiresInSeconds).toBe(3600);
    expect(verifyCsrfToken(token, secret, "sid-1")).toBe(true);
    const cookie = res.headers.get("set-cookie")!;
    expect(cookie).toContain(
      `__Host-foundry-csrf=${encodeURIComponent(token)}`,
    );
    expect(cookie).toMatch(/HttpOnly/);
    expect(cookie).toMatch(/Secure/);
    expect(cookie).toMatch(/SameSite=Lax/);
    expect(cookie).not.toMatch(/Domain=/i);
    const post = await call("/csrf", { method: "POST" });
    expect(post.res.status).toBe(405);
  });

  it("refuses to issue a CSRF token without a session", async () => {
    const { res } = await call(
      "/csrf",
      {},
      {
        authenticate: async () => ({ kind: "unauthenticated" }),
      },
    );
    expect(res.status).toBe(401);
  });

  it("refreshes once and retries when the WFE answers 401", async () => {
    const upstream = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ code: 16 }, { status: 401 }))
      .mockResolvedValueOnce(Response.json({ ok: 2 }));
    const { res, d } = await call(
      "/v1/workspaces",
      { method: "POST", body: "{}" },
      { fetch: upstream as unknown as typeof fetch },
    );
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: 2 });
    expect(d.refresh).toHaveBeenCalledTimes(1);
    const retry = upstream.mock.calls[1]![1] as RequestInit;
    expect(new Headers(retry.headers).get("authorization")).toBe(
      "Bearer token-2",
    );
    expect(new TextDecoder().decode(retry.body as ArrayBuffer)).toBe("{}");
  });

  it("signs out when the refresh token is gone", async () => {
    const upstream = vi.fn(async () =>
      Response.json({ code: 16 }, { status: 401 }),
    );
    const { res, d } = await call(
      "/v1/whoami",
      {},
      {
        fetch: upstream as unknown as typeof fetch,
        refresh: async () => ({ kind: "unauthenticated" }),
      },
    );
    expect(res.status).toBe(401);
    expect(res.headers.get("x-signed-out")).toBe("1");
    expect(d.signOut).toHaveBeenCalledTimes(1);
    expect(upstream).toHaveBeenCalledTimes(1);
  });

  it("answers 503 when the refresh is unavailable", async () => {
    const upstream = vi.fn(async () => new Response(null, { status: 401 }));
    const { res, d } = await call(
      "/v1/whoami",
      {},
      {
        fetch: upstream as unknown as typeof fetch,
        refresh: async () => ({ kind: "transient" }),
      },
    );
    expect(res.status).toBe(503);
    expect(d.signOut).not.toHaveBeenCalled();
  });

  it("never passes on an upstream redirect", async () => {
    const upstream = vi.fn(
      async () =>
        new Response(null, {
          status: 302,
          headers: { location: "https://evil.test" },
        }),
    );
    const { res } = await call(
      "/v1/whoami",
      {},
      {
        fetch: upstream as unknown as typeof fetch,
      },
    );
    expect(res.status).toBe(502);
    expect(res.headers.get("location")).toBeNull();
  });

  it("drops upstream headers that are not allowlisted", async () => {
    const upstream = vi.fn(
      async () =>
        new Response("{}", {
          headers: {
            "content-type": "application/json",
            "set-cookie": "evil=1",
            "access-control-allow-origin": "*",
          },
        }),
    );
    const { res } = await call(
      "/v1/whoami",
      {},
      {
        fetch: upstream as unknown as typeof fetch,
      },
    );
    expect(res.headers.get("set-cookie")).toBeNull();
    expect(res.headers.get("access-control-allow-origin")).toBeNull();
    expect(res.headers.get("content-type")).toBe("application/json");
  });

  it("relays WFE errors as they are", async () => {
    const upstream = vi.fn(async () =>
      Response.json({ code: 7, message: "permission denied" }, { status: 403 }),
    );
    const { res } = await call(
      "/v1/workspaces/ws_9",
      {},
      {
        fetch: upstream as unknown as typeof fetch,
      },
    );
    expect(res.status).toBe(403);
    expect(await res.json()).toEqual({ code: 7, message: "permission denied" });
  });
  it("answers its own error when the WFE cannot be reached", async () => {
    const down = vi.fn(async () => {
      throw new TypeError("fetch failed");
    });
    const { res } = await call(
      "/v1/whoami",
      {},
      { fetch: down as unknown as typeof fetch },
    );
    expect(res.status).toBe(502);
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({
      code: "UPSTREAM_UNAVAILABLE",
      message: "The API is unavailable.",
      details: [],
    });
  });

  it("gives up on a WFE that does not answer in time", async () => {
    const hung = vi.fn(async (_url: URL | RequestInfo, init?: RequestInit) => {
      expect(init?.signal).toBeInstanceOf(AbortSignal);
      throw new DOMException("The operation timed out.", "TimeoutError");
    });
    const { res } = await call(
      "/v1/workspaces",
      { method: "POST", body: "{}" },
      { fetch: hung as unknown as typeof fetch },
    );
    expect(res.status).toBe(504);
    expect((await res.json()).code).toBe("UPSTREAM_TIMEOUT");
    expect(hung).toHaveBeenCalledTimes(1);
  });

  it("answers its own error when the retry after a refresh fails", async () => {
    const upstream = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockRejectedValueOnce(new TypeError("fetch failed"));
    const { res, d } = await call(
      "/v1/whoami",
      {},
      { fetch: upstream as unknown as typeof fetch },
    );
    expect(res.status).toBe(502);
    expect(d.refresh).toHaveBeenCalledTimes(1);
  });
  it("says how long the token it used has left", async () => {
    const jwt = (exp: number) =>
      "h." +
      Buffer.from(JSON.stringify({ sub: "u", exp })).toString("base64url") +
      ".s";
    const now = Math.floor(Date.now() / 1000);
    const session =
      (accessToken: string) => async (): Promise<BrowserSession> => ({
        kind: "authenticated",
        accessToken,
        sessionId: "sid-1",
      });
    const left = (res: Response) =>
      Number(res.headers.get("x-foundry-session-expires-in"));

    const plain = await call(
      "/v1/whoami",
      {},
      { authenticate: session(jwt(now + 120)) },
    );
    expect(left(plain.res)).toBeGreaterThanOrEqual(119);
    expect(left(plain.res)).toBeLessThanOrEqual(120);
    const csrf = await call(
      "/csrf",
      {},
      { authenticate: session(jwt(now + 60)) },
    );
    expect(left(csrf.res)).toBeGreaterThanOrEqual(59);
    const expired = await call(
      "/v1/whoami",
      {},
      { authenticate: session(jwt(now - 5)) },
    );
    expect(expired.res.headers.get("x-foundry-session-expires-in")).toBe("0");
    // Not a JWT: nothing to say.
    const opaque = await call("/v1/whoami");
    expect(opaque.res.headers.has("x-foundry-session-expires-in")).toBe(false);

    // After a refresh, the new token's time.
    const upstream = vi
      .fn()
      .mockResolvedValueOnce(new Response(null, { status: 401 }))
      .mockResolvedValueOnce(Response.json({ ok: 1 }));
    const retried = await call(
      "/v1/whoami",
      {},
      {
        authenticate: session(jwt(now + 100)),
        refresh: session(jwt(now + 300)),
        fetch: upstream as unknown as typeof fetch,
      },
    );
    expect(left(retried.res)).toBeGreaterThanOrEqual(299);
    // Signed out: nothing.
    const gone = await call(
      "/v1/whoami",
      {},
      {
        authenticate: session(jwt(now + 100)),
        refresh: async () => ({ kind: "unauthenticated" }),
        fetch: vi.fn(
          async () => new Response(null, { status: 401 }),
        ) as unknown as typeof fetch,
      },
    );
    expect(gone.res.headers.has("x-foundry-session-expires-in")).toBe(false);
  });
});
