import { describe, expect, it } from "vitest";
import {
  LOGIN_TRANSACTION_TTL_SECONDS,
  loginCookieName,
  loginFlowId,
  openLoginTransaction,
  sealLoginTransaction,
} from "@/server/login-transaction";
import { parseReturnPath, signInPath } from "@/shared/return-path";

const secret = "c".repeat(40);
const sessionId = "a".repeat(40);

describe("login transactions", () => {
  it("round-trip while fresh", () => {
    const sealed = sealLoginTransaction(
      { sessionId, returnPath: "/data?tab=x" },
      secret,
      1_000,
    )!;
    expect(openLoginTransaction(sealed, secret, 2_000)).toEqual({
      sessionId,
      returnPath: "/data?tab=x",
    });
    expect(
      openLoginTransaction(
        sealed,
        secret,
        1_000 + LOGIN_TRANSACTION_TTL_SECONDS * 1000 + 1,
      ),
    ).toBeNull();
    expect(openLoginTransaction(sealed, secret, 999)).toBeNull();
    expect(openLoginTransaction(sealed, "d".repeat(40), 2_000)).toBeNull();
  });

  it("refuse tampering and unsafe destinations", () => {
    const sealed = sealLoginTransaction(
      { sessionId, returnPath: "/" },
      secret,
    )!;
    const [v, payload, sig] = sealed.split(".");
    const forged = Buffer.from(
      JSON.stringify({ sessionId, returnPath: "//evil.test", issuedAt: 1 }),
    ).toString("base64url");
    expect(openLoginTransaction(`${v}.${forged}.${sig}`, secret)).toBeNull();
    expect(openLoginTransaction(`${v}.${payload}`, secret)).toBeNull();
    expect(openLoginTransaction(`${sealed}.x`, secret)).toBeNull();
    expect(
      sealLoginTransaction(
        { sessionId, returnPath: "https://evil.test" },
        secret,
      ),
    ).toBeNull();
    expect(
      sealLoginTransaction({ sessionId: "short", returnPath: "/" }, secret),
    ).toBeNull();
    expect(sealLoginTransaction({ sessionId, returnPath: "/" }, "")).toBeNull();
  });

  it("name one cookie per sign-in flow", () => {
    const flow = loginFlowId("state-1")!;
    expect(flow).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(loginFlowId("state-2")).not.toBe(flow);
    expect(loginCookieName("__Host-foundry-login-", flow)).toBe(
      "__Host-foundry-login-" + flow,
    );
    expect(loginCookieName("p-", "../x")).toBeNull();
    expect(loginFlowId("")).toBeNull();
    expect(loginFlowId("x".repeat(2049))).toBeNull();
  });
});

describe("return paths", () => {
  it.each([
    ["/", "/"],
    ["/data", "/data"],
    [
      "/ontology/versions/v1?format=raw#types",
      "/ontology/versions/v1?format=raw#types",
    ],
  ])("keep %s", (input, output) => expect(parseReturnPath(input)).toBe(output));

  it.each([
    null,
    "",
    "data",
    "//evil.test",
    "/\\evil.test",
    "https://evil.test/",
    "/login",
    "/login?return_to=/",
    "/auth/logout",
    "/papi/v1/whoami",
    "/a/../b",
    "/" + "x".repeat(2048),
  ])("refuse %s", (input) => expect(parseReturnPath(input)).toBeNull());

  it("build the sign-in link", () => {
    expect(signInPath("/data?x=1")).toBe("/login?return_to=%2Fdata%3Fx%3D1");
    expect(signInPath("/")).toBe("/login");
    expect(signInPath("//evil.test")).toBe("/login");
  });
});
