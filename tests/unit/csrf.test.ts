import { describe, expect, it } from "vitest";
import {
  createCsrfToken,
  csrfAccepted,
  isUnsafeMethod,
  readCookie,
  sameOrigin,
  verifyCsrfToken,
} from "@/server/csrf";

const secret = "s".repeat(32);

describe("CSRF tokens", () => {
  it("verify for the session they were made for, until they expire", () => {
    const token = createCsrfToken(secret, "session_a", 1000);
    expect(verifyCsrfToken(token, secret, "session_a", 1000)).toBe(true);
    expect(verifyCsrfToken(token, secret, "session_b", 1000)).toBe(false);
    expect(verifyCsrfToken(token, "t".repeat(32), "session_a", 1000)).toBe(
      false,
    );
    expect(verifyCsrfToken(token, secret, "session_a", 1000 + 3600)).toBe(
      false,
    );
  });

  it("refuse tampered or malformed tokens", () => {
    const token = createCsrfToken(secret, "session_a", 1000);
    const [v, exp, nonce, sig] = token.split(".");
    for (const bad of [
      `${v}.${Number(exp) + 1}.${nonce}.${sig}`,
      `v2.${exp}.${nonce}.${sig}`,
      `${token}.extra`,
      `${v}.${exp}.${nonce}`,
      "",
      "v1.abc.def.ghi",
    ])
      expect(verifyCsrfToken(bad, secret, "session_a", 1000), bad).toBe(false);
  });

  it("need the header and the cookie to match", () => {
    const token = createCsrfToken(secret, "sid");
    const request = (headers: Record<string, string>) =>
      new Request("https://foundry.test/papi/v1/workspaces", {
        method: "POST",
        headers,
      });
    expect(
      csrfAccepted(
        request({ "x-helix-csrf": token, cookie: `a=1; csrf=${token}` }),
        "csrf",
        "x-helix-csrf",
        secret,
        "sid",
      ),
    ).toBe(true);
    const other = createCsrfToken(secret, "sid");
    expect(
      csrfAccepted(
        request({ "x-helix-csrf": token, cookie: `csrf=${other}` }),
        "csrf",
        "x-helix-csrf",
        secret,
        "sid",
      ),
    ).toBe(false);
    expect(
      csrfAccepted(
        request({ cookie: `csrf=${token}` }),
        "csrf",
        "x-helix-csrf",
        secret,
        "sid",
      ),
    ).toBe(false);
  });
});

describe("request helpers", () => {
  it("read one cookie", () => {
    const r = new Request("https://x.test", {
      headers: { cookie: "a=1; b=two%20words; c=%E0%A4%A" },
    });
    expect(readCookie(r, "b")).toBe("two words");
    expect(readCookie(r, "z")).toBeNull();
    expect(readCookie(r, "c")).toBeNull();
  });

  it("require this exact origin", () => {
    const r = (origin?: string) =>
      new Request("https://foundry.test/auth/logout", {
        method: "POST",
        headers: origin ? { origin } : {},
      });
    expect(sameOrigin(r("https://foundry.test"))).toBe(true);
    expect(sameOrigin(r("https://evil.test"))).toBe(false);
    expect(sameOrigin(r("http://foundry.test"))).toBe(false);
    expect(sameOrigin(r())).toBe(false);
  });

  it("know which methods change state", () => {
    expect(["GET", "head", "OPTIONS"].map(isUnsafeMethod)).toEqual([
      false,
      false,
      false,
    ]);
    expect(["POST", "put", "PATCH", "DELETE"].map(isUnsafeMethod)).toEqual([
      true,
      true,
      true,
      true,
    ]);
  });
});
