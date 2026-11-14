import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, type NextResponse } from "next/server";
import {
  loginFlowId,
  openLoginTransaction,
  sealLoginTransaction,
  type LoginTransaction,
} from "@/server/login-transaction";
import { verifyFailure } from "@/server/sign-in";

// The callback's two answers without tokens, over a stubbed WFE and AuthKit:
// WorkOS emailing a code (the callback keeps the login, /auth/verify-email
// takes the code) and the email's company requiring its single sign-on (a
// new login through it, once).

const saveSession = vi.fn(),
  getUser = vi.fn();
vi.mock("@workos-inc/authkit-nextjs", () => ({
  getWorkOS: () => ({ userManagement: { getUser } }),
  saveSession: (...a: unknown[]) => saveSession(...a),
}));
vi.mock("next/server", async (original) => ({
  ...(await original<typeof import("next/server")>()),
  after: vi.fn(),
}));

const SITE = "http://localhost:3000",
  WFE = "http://localhost:4010",
  PASSWORD = "p".repeat(40),
  PREFIX = "foundry-login-",
  STATE = "s".repeat(43),
  SESSION = "a".repeat(40);
const flow = loginFlowId(STATE)!,
  cookie = PREFIX + flow;

// A WorkOS access token, for its subject only.
const accessToken = `h.${Buffer.from(JSON.stringify({ sub: "user_1" })).toString("base64url")}.s`;

type Call = { path: string; body: Record<string, unknown> };
let calls: Call[];
let answers: Record<string, (body: Record<string, unknown>) => Response>;

beforeEach(() => {
  vi.stubEnv("HELIX_WFE_ORIGIN", WFE);
  vi.stubEnv("NEXT_PUBLIC_WORKOS_REDIRECT_URI", `${SITE}/login/callback`);
  vi.stubEnv("WORKOS_CLIENT_ID", "client_1");
  vi.stubEnv("WORKOS_API_KEY", "sk_test");
  vi.stubEnv("WORKOS_COOKIE_PASSWORD", PASSWORD);
  vi.stubEnv("HELIX_CSRF_SECRET", "c".repeat(40));
  vi.stubEnv("WORKOS_COOKIE_NAME", "foundry-session");
  vi.stubEnv("WORKOS_COOKIE_DOMAIN", "");
  calls = [];
  answers = {};
  getUser.mockResolvedValue({ id: "user_1", email: "ada@example.com" });
  saveSession.mockReset();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: URL | string, init?: RequestInit) => {
      const path = new URL(input).pathname,
        body = JSON.parse(String(init?.body ?? "{}"));
      calls.push({ path, body });
      const reply = answers[path];
      if (!reply) throw new Error(`unexpected WFE call ${path}`);
      return reply(body);
    }),
  );
});
afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

const json = (body: unknown, status = 200) => Response.json(body, { status });
const grpc = (code: number, status: number) =>
  json({ code, message: "refused", details: [] }, status);

function loginCookie(transaction: Partial<LoginTransaction> = {}) {
  return sealLoginTransaction(
    { sessionId: SESSION, returnPath: "/data", ...transaction },
    PASSWORD,
  )!;
}

function request(
  path: string,
  init: { method?: string; body?: unknown; cookies?: string; origin?: string },
) {
  const headers = new Headers();
  if (init.cookies) headers.set("cookie", init.cookies);
  if (init.origin !== undefined) headers.set("origin", init.origin);
  if (init.body !== undefined) headers.set("content-type", "application/json");
  return new NextRequest(new URL(path, SITE), {
    method: init.method ?? "GET",
    headers,
    ...(init.body !== undefined && { body: JSON.stringify(init.body) }),
  });
}

const callback = async (sealed = loginCookie()) => {
  const { GET } = await import("@/app/login/callback/route");
  return GET(
    request(`/login/callback?code=c0de&state=${STATE}`, {
      cookies: `${cookie}=${sealed}`,
    }),
  );
};

const verify = async (body: unknown, sealed: string, origin = SITE) => {
  const { POST } = await import("@/app/auth/verify-email/route");
  return POST(
    request("/auth/verify-email", {
      method: "POST",
      body,
      origin,
      cookies: `${cookie}=${sealed}`,
    }),
  );
};

/** The value `response` sets `name` to, or "" when it expires it. */
const setCookie = (response: NextResponse | Response, name: string) =>
  (response as NextResponse).cookies.get(name)?.value;

describe("an exchange WorkOS wants the emailed code for", () => {
  beforeEach(() => {
    answers["/v1/auth/exchange"] = () =>
      json({ email: "grace@example.com", emailVerificationRequired: true });
  });

  it("keeps the sign-in and asks for the code", async () => {
    const res = await callback();
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(
      `${SITE}/login/verify?flow=${flow}`,
    );
    const kept = openLoginTransaction(setCookie(res, cookie)!, PASSWORD);
    expect(kept).toEqual({
      sessionId: SESSION,
      returnPath: "/data",
      verifyEmail: "grace@example.com",
    });
    expect(saveSession).not.toHaveBeenCalled();

    // The page reads which address it waits for.
    const { GET } = await import("@/app/auth/verify-email/route");
    const waiting = await GET(
      request(`/auth/verify-email?flow=${flow}`, {
        cookies: `${cookie}=${setCookie(res, cookie)}`,
      }),
    );
    expect(waiting.headers.get("cache-control")).toBe("no-store");
    expect(await waiting.json()).toEqual({
      email: "grace@example.com",
      returnTo: "/data",
    });
  });

  it("signs in once the code is right, keeping the sign-in for a wrong one", async () => {
    const sealed = loginCookie({ verifyEmail: "grace@example.com" });
    answers["/v1/auth/verify-email"] = (body) =>
      body.code === "424242"
        ? json({ accessToken, refreshToken: "rt_1" })
        : grpc(3, 400);

    const wrong = await verify({ flow, code: "111111" }, sealed);
    expect(wrong.status).toBe(400);
    expect(await wrong.json()).toEqual({ error: "code" });
    expect(setCookie(wrong, cookie)).toBeUndefined();

    const right = await verify({ flow, code: " 424 242 " }, sealed);
    expect(await right.json()).toEqual({ redirectTo: "/data" });
    expect(setCookie(right, cookie)).toBe("");
    expect(calls.at(-1)).toEqual({
      path: "/v1/auth/verify-email",
      body: { sessionId: SESSION, code: "424242" },
    });
    expect(saveSession).toHaveBeenCalledWith(
      {
        accessToken,
        refreshToken: "rt_1",
        user: { id: "user_1", email: "ada@example.com" },
      },
      expect.anything(),
    );
  });

  it.each([
    [grpc(16, 401), "/login?error=verify&return_to=%2Fdata"],
    [grpc(7, 403), "/login?error=denied&return_to=%2Fdata"],
  ])(
    "ends the sign-in when the WFE no longer can (%#)",
    async (reply, redirectTo) => {
      answers["/v1/auth/verify-email"] = () => reply;
      const res = await verify(
        { flow, code: "424242" },
        loginCookie({ verifyEmail: "" }),
      );
      expect(await res.json()).toEqual({ redirectTo });
      expect(setCookie(res, cookie)).toBe("");
    },
  );

  it("says to try again while the WFE is away", async () => {
    answers["/v1/auth/verify-email"] = () => grpc(14, 503);
    const res = await verify(
      { flow, code: "424242" },
      loginCookie({ verifyEmail: "" }),
    );
    expect(res.status).toBe(503);
    expect(await res.json()).toEqual({ error: "unavailable" });
    expect(setCookie(res, cookie)).toBeUndefined();
  });

  it("checks no code for a sign-in that isn't waiting for one, or another origin", async () => {
    const notWaiting = await verify({ flow, code: "424242" }, loginCookie());
    expect(await notWaiting.json()).toEqual({
      redirectTo: "/login?error=verify&return_to=%2Fdata",
    });
    const forged = await verify(
      { flow, code: "424242" },
      loginCookie({ verifyEmail: "" }),
      "https://evil.example",
    );
    expect(forged.status).toBe(403);
    expect(calls).toEqual([]);
  });
});

describe("an exchange whose company requires single sign-on", () => {
  const SSO_URL = `${WFE}/mock/authorize?state=${"t".repeat(43)}`;
  beforeEach(() => {
    answers["/v1/auth/exchange"] = () =>
      json({ email: "grace@sso.example", ssoRequired: true });
    answers["/v1/auth/login:start"] = () =>
      json({ url: SSO_URL, sessionId: "b".repeat(40) });
  });

  it("starts a sign-in through it, in place of the first", async () => {
    const res = await callback();
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(SSO_URL);
    expect(calls.at(-1)).toEqual({
      path: "/v1/auth/login:start",
      body: {
        redirectUri: `${SITE}/login/callback`,
        provider: "AUTH_PROVIDER_SSO",
        application: "AUTH_APPLICATION_FOUNDRY",
        email: "grace@sso.example",
      },
    });
    const next = PREFIX + loginFlowId("t".repeat(43));
    expect(openLoginTransaction(setCookie(res, next)!, PASSWORD)).toEqual({
      sessionId: "b".repeat(40),
      returnPath: "/data",
      sso: true,
    });
    expect(setCookie(res, cookie)).toBe("");
  });

  it("stops when the single sign-on is refused for it too", async () => {
    const res = await callback(loginCookie({ sso: true }));
    expect(res.headers.get("location")).toBe(
      `${SITE}/login?error=sso&return_to=%2Fdata`,
    );
    expect(calls.map((c) => c.path)).toEqual(["/v1/auth/exchange"]);
  });

  it("says so when the WFE can't start it", async () => {
    answers["/v1/auth/login:start"] = () => grpc(9, 400);
    const res = await callback();
    expect(res.headers.get("location")).toBe(
      `${SITE}/login?error=sso&return_to=%2Fdata`,
    );
  });
});

describe("verifyFailure", () => {
  it.each([
    [400, { code: 3 }, "code"],
    [401, { code: 16 }, "expired"],
    [400, { code: 9 }, "expired"],
    [403, { code: 7 }, "denied"],
    [503, { code: 14 }, "unavailable"],
    [502, null, "unavailable"],
    [400, "not json", "expired"],
  ] as const)("reads %s %o as %s", (status, body, failure) => {
    expect(verifyFailure(status, body)).toBe(failure);
  });
});
