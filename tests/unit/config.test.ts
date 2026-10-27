import { describe, expect, it } from "vitest";
import { authConfig, isTrustedUrl, localConfig } from "@/server/config";
import { csrfCookieName, loginCookiePrefix } from "@/server/cookies";
import { withSessionHeaders } from "@/server/session-headers";

const prod = {
  HELIX_WFE_ORIGIN: "https://api.dev.helix-db.com",
  NEXT_PUBLIC_WORKOS_REDIRECT_URI:
    "https://dev.foundry.helix-db.com/login/callback",
  WORKOS_CLIENT_ID: "client_x",
  WORKOS_API_KEY: "sk_x",
  WORKOS_COOKIE_PASSWORD: "p".repeat(32),
  WORKOS_COOKIE_NAME: "__Host-foundry-session",
  HELIX_CSRF_SECRET: "s".repeat(32),
} as unknown as NodeJS.ProcessEnv;

describe("auth configuration", () => {
  it("accepts a complete https setup with host-only cookies", () => {
    const c = authConfig(prod)!;
    expect(c.secure).toBe(true);
    expect(c.wfeOrigin).toBe("https://api.dev.helix-db.com");
    expect(c.sessionCookie).toBe("__Host-foundry-session");
    expect(csrfCookieName(c)).toBe("__Host-foundry-csrf");
    expect(loginCookiePrefix(c)).toBe("__Host-foundry-login-");
  });

  it("drops the __Host- prefix only on loopback http", () => {
    const c = authConfig({
      ...prod,
      HELIX_WFE_ORIGIN: "http://127.0.0.1:4010",
      NEXT_PUBLIC_WORKOS_REDIRECT_URI: "http://localhost:3000/login/callback",
      WORKOS_COOKIE_NAME: "foundry-session",
    })!;
    expect(c.secure).toBe(false);
    expect(csrfCookieName(c)).toBe("foundry-csrf");
  });

  it.each([
    ["no WFE", { HELIX_WFE_ORIGIN: "" }],
    ["plain-http WFE", { HELIX_WFE_ORIGIN: "http://api.example.com" }],
    [
      "plain-http site",
      {
        NEXT_PUBLIC_WORKOS_REDIRECT_URI:
          "http://foundry.example.com/login/callback",
      },
    ],
    [
      "wrong callback path",
      { NEXT_PUBLIC_WORKOS_REDIRECT_URI: "https://f.example.com/callback" },
    ],
    [
      "callback with a query",
      {
        NEXT_PUBLIC_WORKOS_REDIRECT_URI:
          "https://f.example.com/login/callback?x=1",
      },
    ],
    ["short cookie password", { WORKOS_COOKIE_PASSWORD: "short" }],
    ["short CSRF secret", { HELIX_CSRF_SECRET: "short" }],
    ["no client ID", { WORKOS_CLIENT_ID: "" }],
    ["no API key", { WORKOS_API_KEY: "" }],
    [
      "a cookie without __Host- on https",
      { WORKOS_COOKIE_NAME: "foundry-session" },
    ],
    ["a shared cookie domain", { WORKOS_COOKIE_DOMAIN: ".helix-db.com" }],
  ])("refuses %s", (_name, change) => {
    expect(
      authConfig({ ...prod, ...change } as unknown as NodeJS.ProcessEnv),
    ).toBeNull();
  });

  it("trusts https and loopback http only", () => {
    expect(isTrustedUrl(new URL("https://x.test"))).toBe(true);
    expect(isTrustedUrl(new URL("http://localhost:1"))).toBe(true);
    expect(isTrustedUrl(new URL("http://127.0.0.1"))).toBe(true);
    expect(isTrustedUrl(new URL("http://x.test"))).toBe(false);
    expect(isTrustedUrl(new URL("javascript:alert(1)"))).toBe(false);
  });
});

describe("session response headers", () => {
  it("add AuthKit's rotated cookie unless the route set its own", () => {
    const authkit = new Headers();
    authkit.append("Set-Cookie", "s=rotated; Path=/");
    authkit.append("Set-Cookie", "other=1; Path=/");
    authkit.set("Cache-Control", "private, no-store");
    authkit.set("Vary", "cookie");
    const own = new Headers({ Vary: "Accept, Cookie" });
    own.append("Set-Cookie", "s=; Max-Age=0");
    const out = withSessionHeaders(
      new Response("x", { headers: own }),
      authkit,
    );
    expect(out.headers.getSetCookie()).toEqual([
      "other=1; Path=/",
      "s=; Max-Age=0",
    ]);
    expect(out.headers.get("cache-control")).toBe("private, no-store");
    expect(out.headers.get("vary")).toBe("Accept, Cookie");
  });
});

describe("local configuration", () => {
  const local = {
    HELIX_WFE_ORIGIN: "http://pod:8081",
    FOUNDRY_ORIGIN: "http://localhost:3000",
    FOUNDRY_LOCAL_TOKEN: "t".repeat(32),
  } as unknown as NodeJS.ProcessEnv;

  it("accepts a loopback site and an API on any host", () => {
    const c = localConfig(local)!;
    expect(c).toMatchObject({
      wfeOrigin: "http://pod:8081",
      token: "t".repeat(32),
      origin: "http://localhost:3000",
    });
    // Derived from the token, and never the token itself.
    expect(c.csrfSecret).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(c.csrfSecret).toBe(localConfig(local)!.csrfSecret);
    expect(
      localConfig({ ...local, FOUNDRY_LOCAL_TOKEN: "u".repeat(32) })!
        .csrfSecret,
    ).not.toBe(c.csrfSecret);
    expect(
      localConfig({ ...local, FOUNDRY_ORIGIN: "http://[::1]:3000" })!.origin,
    ).toBe("http://[::1]:3000");
    expect(
      localConfig({ ...local, HELIX_WFE_ORIGIN: "https://api.example.com/" })!
        .wfeOrigin,
    ).toBe("https://api.example.com");
  });

  it.each([
    ["no API", { HELIX_WFE_ORIGIN: "" }],
    ["a non-http API", { HELIX_WFE_ORIGIN: "ftp://pod" }],
    ["no site origin", { FOUNDRY_ORIGIN: "" }],
    [
      "a site reachable from the network",
      { FOUNDRY_ORIGIN: "http://192.168.1.9:3000" },
    ],
    [
      "an https site that is not loopback",
      { FOUNDRY_ORIGIN: "https://foundry.example.com" },
    ],
    [
      "a site origin with a path",
      { FOUNDRY_ORIGIN: "http://localhost:3000/app" },
    ],
    [
      "a site origin with a query",
      { FOUNDRY_ORIGIN: "http://localhost:3000/?x=1" },
    ],
    ["no token", { FOUNDRY_LOCAL_TOKEN: "" }],
    ["a short token", { FOUNDRY_LOCAL_TOKEN: "t".repeat(31) }],
  ])("refuses %s", (_name, change) => {
    expect(
      localConfig({ ...local, ...change } as unknown as NodeJS.ProcessEnv),
    ).toBeNull();
  });
});
