import { beforeEach, describe, expect, it, vi } from "vitest";
import type { NextRequest } from "next/server";

// resolveSession and refreshAfterRejection over a stubbed AuthKit: one
// AuthKit call per session cookie at a time, rotated sessions reused, and
// refresh failures sorted into transient and terminal.

const authkit = vi.fn(),
  refreshSession = vi.fn();
class TokenRefreshError extends Error {
  constructor(readonly isTransient: boolean) {
    super("refresh failed");
  }
}
vi.mock("@workos-inc/authkit-nextjs", () => ({
  authkit: (...a: unknown[]) => authkit(...a),
  refreshSession: (...a: unknown[]) => refreshSession(...a),
  TokenRefreshError,
}));

const NAME = "foundry-session";
const request = (cookie?: string) =>
  ({
    cookies: {
      get: (n: string) =>
        n === NAME && cookie ? { value: cookie } : undefined,
    },
  }) as unknown as NextRequest;

const session = (token = "at-1") => ({
  user: { id: "user_1" },
  accessToken: token,
  sessionId: "sid-1",
});
const rotatedHeaders = () => {
  const h = new Headers();
  h.append("Set-Cookie", `${NAME}=sealed-2; Path=/; HttpOnly; Max-Age=2592000`);
  h.append("Set-Cookie", "wos-auth-verifier-x=pkce; Path=/");
  h.set("Vary", "Cookie");
  return h;
};

let load: () => Promise<typeof import("@/server/session")>;
beforeEach(() => {
  vi.resetModules();
  authkit.mockReset();
  refreshSession.mockReset();
  load = () => import("@/server/session");
});

describe("resolveSession", () => {
  it("is signed out without a cookie, and never asks AuthKit", async () => {
    const { resolveSession } = await load();
    expect((await resolveSession(request(), NAME)).kind).toBe(
      "unauthenticated",
    );
    expect(authkit).not.toHaveBeenCalled();
  });

  it("shares one AuthKit call between concurrent requests with one cookie", async () => {
    const { resolveSession } = await load();
    let finish!: (v: unknown) => void;
    authkit.mockReturnValueOnce(new Promise((r) => (finish = r)));
    const all = Promise.all(
      [1, 2, 3].map(() => resolveSession(request("sealed-1"), NAME)),
    );
    finish({ session: session("at-2"), headers: rotatedHeaders() });
    const results = await all;
    expect(authkit).toHaveBeenCalledTimes(1);
    for (const r of results)
      expect(r).toMatchObject({
        kind: "authenticated",
        accessToken: "at-2",
        rotated: true,
      });
    // Only the session cookie is passed on, not AuthKit's PKCE cookie.
    expect(results[0]!.responseHeaders.getSetCookie()).toEqual([
      `${NAME}=sealed-2; Path=/; HttpOnly; Max-Age=2592000`,
    ]);
  });

  it("reuses a rotated session for a late request with the old cookie", async () => {
    const { resolveSession } = await load();
    authkit.mockResolvedValueOnce({
      session: session("at-2"),
      headers: rotatedHeaders(),
    });
    await resolveSession(request("sealed-1"), NAME);
    const late = await resolveSession(request("sealed-1"), NAME);
    expect(late).toMatchObject({ accessToken: "at-2" });
    expect(authkit).toHaveBeenCalledTimes(1);
  });

  it("asks again when nothing rotated", async () => {
    const { resolveSession } = await load();
    authkit.mockResolvedValue({ session: session(), headers: new Headers() });
    await resolveSession(request("sealed-1"), NAME);
    const again = await resolveSession(request("sealed-1"), NAME);
    expect(again).toMatchObject({ kind: "authenticated", rotated: false });
    expect(authkit).toHaveBeenCalledTimes(2);
  });

  it("sorts refresh failures into transient and signed out", async () => {
    const { resolveSession } = await load();
    authkit.mockImplementationOnce(async (_req, opts) => {
      opts.onSessionRefreshError({ isTransient: true });
      return { session: { user: null }, headers: new Headers() };
    });
    expect((await resolveSession(request("a"), NAME)).kind).toBe("transient");
    authkit.mockImplementationOnce(async (_req, opts) => {
      opts.onSessionRefreshError({ isTransient: false });
      const h = new Headers();
      h.append("Set-Cookie", `${NAME}=; Path=/; Max-Age=0`);
      return { session: { user: null }, headers: h };
    });
    const terminal = await resolveSession(request("b"), NAME);
    expect(terminal).toMatchObject({ kind: "unauthenticated", rotated: false });
    expect(terminal.responseHeaders.getSetCookie()).toHaveLength(1);
    authkit.mockRejectedValueOnce(new Error("boom"));
    expect((await resolveSession(request("c"), NAME)).kind).toBe("transient");
  });
});

describe("refreshAfterRejection", () => {
  it("returns the new token, once per cookie", async () => {
    const { refreshAfterRejection } = await load();
    refreshSession.mockResolvedValue(session("at-3"));
    const [a, b] = await Promise.all([
      refreshAfterRejection(request("x"), NAME),
      refreshAfterRejection(request("x"), NAME),
    ]);
    expect(a).toEqual({
      kind: "authenticated",
      accessToken: "at-3",
      sessionId: "sid-1",
    });
    expect(b).toEqual(a);
    expect(refreshSession).toHaveBeenCalledTimes(1);
  });

  it("is signed out when the refresh token is dead, transient otherwise", async () => {
    const { refreshAfterRejection } = await load();
    refreshSession.mockRejectedValueOnce(new TokenRefreshError(false));
    expect((await refreshAfterRejection(request("y"), NAME)).kind).toBe(
      "unauthenticated",
    );
    refreshSession.mockRejectedValueOnce(new TokenRefreshError(true));
    expect((await refreshAfterRejection(request("z"), NAME)).kind).toBe(
      "transient",
    );
    refreshSession.mockResolvedValueOnce({ user: null });
    expect((await refreshAfterRejection(request("w"), NAME)).kind).toBe(
      "unauthenticated",
    );
  });
});
