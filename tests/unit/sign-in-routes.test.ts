import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NextRequest, type NextResponse } from "next/server";
import {
  loginFlowId,
  openLoginTransaction,
  sealLoginTransaction,
  type LoginTransaction,
} from "@/server/login-transaction";
import { refusal, sentence, type PasswordAction } from "@/server/password";
import { verifyFailure } from "@/server/sign-in";

// The callback's two answers without tokens, over a stubbed WFE and AuthKit:
// WorkOS emailing a code (the callback keeps the login, /auth/verify-email
// takes the code) and the email's company requiring its single sign-on (a
// new login through it, once). And signing in with an email and password
// (/auth/password/*), which takes the same two detours.

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

const passwordRoutes = {
  "sign-in": () => import("@/app/auth/password/sign-in/route"),
  "sign-up": () => import("@/app/auth/password/sign-up/route"),
  forgot: () => import("@/app/auth/password/forgot/route"),
  reset: () => import("@/app/auth/password/reset/route"),
};

const password = async (
  action: PasswordAction,
  body: unknown,
  init: { origin?: string; raw?: string; cookies?: string } = {},
) => {
  const { POST } = await passwordRoutes[action]();
  const headers = new Headers({
    "content-type": "application/json",
    origin: init.origin ?? SITE,
  });
  if (init.cookies) headers.set("cookie", init.cookies);
  return POST(
    new NextRequest(new URL(`/auth/password/${action}`, SITE), {
      method: "POST",
      headers,
      body: init.raw ?? JSON.stringify(body),
    }),
  );
};

describe("signing in with a password", () => {
  const form = {
    email: " ada@example.com ",
    password: "correct-horse-battery",
    returnTo: "/data",
  };

  it("signs in as the Foundry application and returns to the page", async () => {
    answers["/v1/auth/password:sign-in"] = () =>
      json({ accessToken, refreshToken: "rt_1", email: "ada@example.com" });
    const res = await password("sign-in", form);
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe("no-store");
    expect(await res.json()).toEqual({ redirectTo: "/data" });
    expect(calls).toEqual([
      {
        path: "/v1/auth/password:sign-in",
        body: {
          email: "ada@example.com",
          password: "correct-horse-battery",
          application: "AUTH_APPLICATION_FOUNDRY",
        },
      },
    ]);
    expect(saveSession).toHaveBeenCalledWith(
      {
        accessToken,
        refreshToken: "rt_1",
        user: { id: "user_1", email: "ada@example.com" },
      },
      expect.anything(),
    );
  });

  it.each([["//evil.example"], ["https://evil.example"], [42], [undefined]])(
    "lands on / for a return path that isn't one (%o)",
    async (returnTo) => {
      answers["/v1/auth/password:sign-in"] = () =>
        json({ accessToken, refreshToken: "rt_1" });
      const res = await password("sign-in", { ...form, returnTo });
      expect(await res.json()).toEqual({ redirectTo: "/" });
    },
  );

  it.each([
    [grpc(16, 401), 401, { error: "credentials" }],
    [grpc(7, 403), 403, { error: "denied" }],
    [grpc(14, 503), 503, { error: "unavailable" }],
    [json({ code: 8, message: "slow down" }, 429), 503, { error: "unavailable" }],
    [
      json({ code: 3, message: "password is too long" }, 400),
      400,
      { error: "refused", message: "Password is too long." },
    ],
  ])("says why the WFE refused it (%#)", async (reply, status, body) => {
    answers["/v1/auth/password:sign-in"] = () => reply;
    const res = await password("sign-in", form);
    expect(res.status).toBe(status);
    expect(await res.json()).toEqual(body);
    expect(saveSession).not.toHaveBeenCalled();
  });

  it("says sign-in is away when the WFE can't be reached or WorkOS doesn't know the user", async () => {
    answers["/v1/auth/password:sign-in"] = () => {
      throw new TypeError("fetch failed");
    };
    expect(await (await password("sign-in", form)).json()).toEqual({
      error: "unavailable",
    });
    answers["/v1/auth/password:sign-in"] = () =>
      json({ accessToken, refreshToken: "rt_1" });
    getUser.mockRejectedValueOnce(new Error("not found"));
    const res = await password("sign-in", form);
    expect(res.status).toBe(503);
    expect(saveSession).not.toHaveBeenCalled();
  });

  it("keeps the sign-in for the emailed code, which /auth/verify-email then takes", async () => {
    answers["/v1/auth/password:sign-in"] = () =>
      json({
        email: "grace@example.com",
        emailVerificationRequired: true,
        sessionId: SESSION,
      });
    const res = await password("sign-in", form);
    const kept = PREFIX + loginFlowId(SESSION)!;
    expect(await res.json()).toEqual({
      redirectTo: `/login/verify?flow=${loginFlowId(SESSION)}`,
    });
    const sealed = setCookie(res, kept)!;
    expect(openLoginTransaction(sealed, PASSWORD)).toEqual({
      sessionId: SESSION,
      returnPath: "/data",
      verifyEmail: "grace@example.com",
    });
    expect(saveSession).not.toHaveBeenCalled();

    answers["/v1/auth/verify-email"] = () =>
      json({ accessToken, refreshToken: "rt_2" });
    const { POST } = await import("@/app/auth/verify-email/route");
    const verified = await POST(
      request("/auth/verify-email", {
        method: "POST",
        body: { flow: loginFlowId(SESSION), code: "424242" },
        origin: SITE,
        cookies: `${kept}=${sealed}`,
      }),
    );
    expect(await verified.json()).toEqual({ redirectTo: "/data" });
    expect(calls.at(-1)).toEqual({
      path: "/v1/auth/verify-email",
      body: { sessionId: SESSION, code: "424242" },
    });
  });

  it("refuses a code detour without a session to keep", async () => {
    answers["/v1/auth/password:sign-in"] = () =>
      json({ email: "grace@example.com", emailVerificationRequired: true });
    const res = await password("sign-in", form);
    expect(res.status).toBe(503);
    expect(await res.json()).toEqual({ error: "unavailable" });
  });

  it("goes through the company's single sign-on when it requires it", async () => {
    const SSO_URL = `${WFE}/mock/authorize?state=${"t".repeat(43)}`;
    answers["/v1/auth/password:sign-in"] = () =>
      json({ email: "grace@sso.example", ssoRequired: true });
    answers["/v1/auth/login:start"] = () =>
      json({ url: SSO_URL, sessionId: "b".repeat(40) });
    const res = await password("sign-in", form);
    expect(await res.json()).toEqual({ redirectTo: SSO_URL });
    expect(calls.at(-1)?.body).toMatchObject({
      provider: "AUTH_PROVIDER_SSO",
      email: "grace@sso.example",
    });
    const next = PREFIX + loginFlowId("t".repeat(43));
    expect(openLoginTransaction(setCookie(res, next)!, PASSWORD)).toEqual({
      sessionId: "b".repeat(40),
      returnPath: "/data",
      sso: true,
    });

    answers["/v1/auth/login:start"] = () => grpc(9, 400);
    const refused = await password("sign-in", form);
    expect(await refused.json()).toEqual({
      redirectTo: "/login?error=sso&return_to=%2Fdata",
    });
  });

  it.each([
    ["another origin", { origin: "https://evil.example" }, 403],
    ["a body too large", { raw: JSON.stringify({ x: "x".repeat(5000) }) }, 400],
    ["a body that isn't an object", { raw: "[1]" }, 400],
    ["a body that isn't JSON", { raw: "email=a" }, 400],
  ])("refuses %s without calling the WFE", async (_, init, status) => {
    const res = await password("sign-in", form, init);
    expect(res.status).toBe(status);
    expect(calls).toEqual([]);
  });

  it.each([
    [{ ...form, email: "ada" }, "Enter your email."],
    [{ ...form, email: "a@".padEnd(321, "x") }, "Enter your email."],
    [{ ...form, password: "" }, "Enter a password."],
    [{ ...form, password: 7 }, "Enter a password."],
  ])("asks for what's missing without calling the WFE (%#)", async (body, message) => {
    const res = await password("sign-in", body);
    expect(res.status).toBe(400);
    expect(await res.json()).toEqual({ error: "refused", message });
    expect(calls).toEqual([]);
  });

  it("says sign-in is away while the site isn't set up for it", async () => {
    vi.stubEnv("WORKOS_COOKIE_PASSWORD", "");
    const res = await password("sign-in", form);
    expect(res.status).toBe(503);
    expect(calls).toEqual([]);
  });
});

describe("creating an account with a password", () => {
  const form = {
    name: "  Grace  Brewster Hopper ",
    email: "grace@example.com",
    password: "a-long-password",
  };

  it("names the user and signs in", async () => {
    answers["/v1/auth/password:sign-up"] = () =>
      json({ accessToken, refreshToken: "rt_1" });
    const res = await password("sign-up", form);
    expect(await res.json()).toEqual({ redirectTo: "/" });
    expect(calls[0]).toEqual({
      path: "/v1/auth/password:sign-up",
      body: {
        email: "grace@example.com",
        password: "a-long-password",
        firstName: "Grace",
        lastName: "Brewster Hopper",
        application: "AUTH_APPLICATION_FOUNDRY",
      },
    });
  });

  it("sends a one-word name as the first name only", async () => {
    answers["/v1/auth/password:sign-up"] = () =>
      json({ accessToken, refreshToken: "rt_1" });
    await password("sign-up", { ...form, name: "Grace" });
    expect(calls[0]?.body).not.toHaveProperty("lastName");
    expect(calls[0]?.body.firstName).toBe("Grace");
  });

  it.each([[""], ["   "], ["x".repeat(129)], [undefined]])(
    "asks for a name (%o)",
    async (name) => {
      const res = await password("sign-up", { ...form, name });
      expect(await res.json()).toEqual({
        error: "refused",
        message: "Enter your name.",
      });
      expect(calls).toEqual([]);
    },
  );

  it.each([
    [grpc(6, 409), 409, { error: "exists" }],
    [
      json(
        { code: 3, message: "Password must be at least 10 characters long." },
        400,
      ),
      400,
      {
        error: "refused",
        message: "Password must be at least 10 characters long.",
      },
    ],
    [
      json(
        {
          code: 16,
          message: "account created but sign-in failed; try signing in",
        },
        401,
      ),
      400,
      {
        error: "refused",
        message: "Account created but sign-in failed. Try signing in.",
      },
    ],
  ])("says why the WFE refused it (%#)", async (reply, status, body) => {
    answers["/v1/auth/password:sign-up"] = () => reply;
    const res = await password("sign-up", form);
    expect(res.status).toBe(status);
    expect(await res.json()).toEqual(body);
  });
});

describe("resetting a password", () => {
  it("asks the WFE to email the Foundry application's link", async () => {
    answers["/v1/auth/password:forgot"] = () => json({});
    const res = await password("forgot", { email: "ada@example.com" });
    expect(await res.json()).toEqual({ sent: true });
    expect(calls).toEqual([
      {
        path: "/v1/auth/password:forgot",
        body: {
          email: "ada@example.com",
          application: "AUTH_APPLICATION_FOUNDRY",
        },
      },
    ]);
  });

  it("asks for an email, and says when the WFE can't send one", async () => {
    const blank = await password("forgot", { email: "" });
    expect(await blank.json()).toEqual({
      error: "refused",
      message: "Enter your email.",
    });
    expect(calls).toEqual([]);
    answers["/v1/auth/password:forgot"] = () => grpc(14, 503);
    const away = await password("forgot", { email: "ada@example.com" });
    expect(away.status).toBe(503);
    answers["/v1/auth/password:forgot"] = () =>
      json({ code: 3, message: "a valid email is required" }, 400);
    expect(
      await (await password("forgot", { email: "ada@example" })).json(),
    ).toEqual({ error: "refused", message: "A valid email is required." });
  });

  it("sets the new password from the emailed link's token", async () => {
    answers["/v1/auth/password:reset"] = () => json({});
    const res = await password("reset", {
      token: " tok_1 ",
      password: "a-new-password",
    });
    expect(await res.json()).toEqual({ reset: true });
    expect(calls).toEqual([
      {
        path: "/v1/auth/password:reset",
        body: { token: "tok_1", newPassword: "a-new-password" },
      },
    ]);
    expect(saveSession).not.toHaveBeenCalled();
  });

  it("says why a reset was refused", async () => {
    answers["/v1/auth/password:reset"] = () =>
      json({ code: 3, message: "reset link is invalid or expired" }, 400);
    const res = await password("reset", { token: "t", password: "p" });
    expect(await res.json()).toEqual({
      error: "refused",
      message: "Reset link is invalid or expired.",
    });
    answers["/v1/auth/password:reset"] = () => grpc(14, 503);
    expect(
      (await password("reset", { token: "t", password: "p" })).status,
    ).toBe(503);
  });

  it("refuses a link without a token, or no password, without calling the WFE", async () => {
    const noToken = await password("reset", { password: "a-new-password" });
    expect(await noToken.json()).toEqual({
      error: "refused",
      message: "This reset link is incomplete. Request a new one.",
    });
    const noPassword = await password("reset", { token: "t" });
    expect(await noPassword.json()).toEqual({
      error: "refused",
      message: "Enter a password.",
    });
    expect(calls).toEqual([]);
  });
});

describe("refusal and sentence", () => {
  it.each([
    ["sign-in", 16, "x", { error: "credentials" }],
    ["sign-up", 16, "x", { error: "refused", message: "X." }],
    ["sign-up", 6, "x", { error: "exists" }],
    ["sign-in", 6, "x", { error: "unavailable" }],
    ["reset", 7, "", { error: "denied" }],
    ["reset", 9, "gone", { error: "refused", message: "Gone." }],
    ["reset", 3, "  ", { error: "unavailable" }],
    ["forgot", 0, "teapot", { error: "unavailable" }],
  ] as const)("reads %s's %i %o", (action, code, message, answer) => {
    expect(refusal(action, code, message)).toEqual(answer);
  });

  it.each([
    ["reset link is invalid or expired", "Reset link is invalid or expired."],
    ["Already a sentence!", "Already a sentence!"],
    ["one; two;  three", "One. Two. Three."],
    ["x".repeat(400), `${"X" + "x".repeat(299)}.`],
  ])("makes %o a sentence", (message, text) => {
    expect(sentence(message)).toBe(text);
  });
});
