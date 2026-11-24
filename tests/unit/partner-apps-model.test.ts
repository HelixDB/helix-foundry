import { describe, expect, it, vi } from "vitest";
import type { PartnerAppInstall } from "@/shared/wfe";
import {
  allowedScopes,
  appCredentialIds,
  approveBody,
  approvedBack,
  cancelUrl,
  existingInstall,
  installLine,
  MAX_SOURCES,
  PARTNER_SCOPES,
  partnerRequest,
  redirectUriOk,
  SCOPE_COPY,
  scopesEvent,
  scopesShown,
  scopesText,
} from "@/web/areas/workspace/partner-apps-model";

// Connected apps' pure rules (src/web/areas/workspace/partner-apps-model.ts):
// the scopes and their copy, the kept request the consent page acts on, the
// approval it sends, where Helix's answers may send the browser, and an
// install's lines.

const REDIRECT = "https://northwind.example/helix/callback";
const request = {
  clientId: "hlxpc_" + "n".repeat(43),
  redirectUri: REDIRECT,
  scope: "foundry.read foundry.ask",
  state: "s1",
  codeChallenge: "c".repeat(43),
};
const install = (over: Partial<PartnerAppInstall> = {}): PartnerAppInstall => ({
  id: "pai_1",
  credentialId: "sc_1",
  app: { clientId: request.clientId, name: "Northwind", publisher: "N" },
  approvedByUserId: "usr_ada",
  approvedAt: "2026-11-20T10:00:00Z",
  ...over,
});

describe("scopes", () => {
  it("are read and ask, in that order, each with its copy", () => {
    expect(PARTNER_SCOPES).toEqual(["foundry.read", "foundry.ask"]);
    expect(SCOPE_COPY["foundry.read"].label).toBe("Read");
    expect(SCOPE_COPY["foundry.ask"].text).toContain("monthly Ask budget");
  });
  it("shows only known ones, once each, in canonical order", () => {
    expect(
      scopesShown(["foundry.ask", "other", "foundry.read", "foundry.ask"]),
    ).toEqual(["foundry.read", "foundry.ask"]);
    expect(scopesShown()).toEqual([]);
  });
  it("allows at least one, or none at all", () => {
    expect(allowedScopes(["foundry.ask"])).toEqual(["foundry.ask"]);
    expect(allowedScopes(["foundry.ask", "foundry.read"])).toEqual([
      "foundry.read",
      "foundry.ask",
    ]);
    expect(allowedScopes([])).toBeNull();
    expect(allowedScopes(["admin"])).toBeNull();
  });
  it("says what was allowed in a word", () => {
    expect(scopesEvent(["foundry.read", "foundry.ask"])).toBe("read_ask");
    expect(scopesEvent(["foundry.read"])).toBe("read");
    expect(scopesEvent(["foundry.ask"])).toBe("ask");
  });
  it("names an install's in words", () => {
    expect(scopesText(["foundry.ask", "foundry.read"])).toBe("Read, Ask");
    expect(scopesText(["foundry.ask"])).toBe("Ask");
    expect(scopesText([])).toBe("Nothing");
    expect(scopesText()).toBe("Nothing");
  });
});

describe("a kept request", () => {
  it("needs a redirect URI Helix could have registered", () => {
    expect(redirectUriOk(REDIRECT)).toBe(true);
    expect(redirectUriOk("https://n.example/cb?x=1")).toBe(true);
    expect(redirectUriOk("http://localhost:8080/cb")).toBe(true);
    expect(redirectUriOk("http://127.0.0.1/cb")).toBe(true);
    expect(redirectUriOk("http://[::1]:3000/cb")).toBe(true);
    for (const bad of [
      "http://northwind.example/cb",
      "https://user@northwind.example/cb",
      "https://user:pw@northwind.example/cb",
      "https://northwind.example/cb#frag",
      "javascript:alert(1)",
      "/helix/callback",
      "not a url",
    ])
      expect(redirectUriOk(bad), bad).toBe(false);
  });
  it("is acted on only with such a redirect URI", () => {
    const kept = { kind: "app_authorize" as const, ...request, at: 0 };
    expect(partnerRequest(kept)).toEqual(request);
    expect(
      partnerRequest({ ...kept, redirectUri: "http://evil.example/cb" }),
    ).toBeNull();
  });
});

describe("an approval", () => {
  const target = { kind: "KIND_CONNECTOR" as const, connectorId: "c1" };
  it("sends the request with the scopes allowed and the sources chosen", () => {
    expect(
      approveBody(request, ["foundry.ask", "foundry.read"], [target]),
    ).toEqual({
      clientId: request.clientId,
      redirectUri: REDIRECT,
      state: "s1",
      codeChallenge: request.codeChallenge,
      scopes: ["foundry.read", "foundry.ask"],
      targets: [target],
    });
  });
  it("is never sent without a scope or a source, or past the cap", () => {
    expect(approveBody(request, [], [target])).toBeNull();
    expect(approveBody(request, ["foundry.read"], [])).toBeNull();
    const many = Array.from({ length: MAX_SOURCES + 1 }, (_, i) => ({
      kind: "KIND_CONNECTOR" as const,
      connectorId: `c${i}`,
    }));
    expect(approveBody(request, ["foundry.read"], many)).toBeNull();
    expect(
      approveBody(request, ["foundry.read"], many.slice(0, MAX_SOURCES))
        ?.targets,
    ).toHaveLength(MAX_SOURCES);
  });
});

describe("Helix's answers", () => {
  it("go back to the app's redirect URI with a code and the state", () => {
    expect(
      approvedBack(`${REDIRECT}?code=hlxpa_x&state=s1&iss=i`, request),
    ).toBe(true);
    expect(
      approvedBack("https://n.example/cb?code=hlxpa_x&state=s&iss=i", {
        redirectUri: "https://n.example/cb",
        state: "s",
      }),
    ).toBe(true);
    for (const bad of [
      `${REDIRECT}?code=hlxpa_x&state=other`,
      `${REDIRECT}?code=hlxpt_x&state=s1`,
      `${REDIRECT}?state=s1`,
      `https://evil.example/helix/callback?code=hlxpa_x&state=s1`,
      `${REDIRECT}/more?code=hlxpa_x&state=s1`,
      "not a url",
      "",
    ])
      expect(approvedBack(bad, request), bad).toBe(false);
  });
  it("cancel to the deny URL with the request's state", () => {
    expect(
      cancelUrl("https://n.example/cb?error=access_denied&iss=i", {
        redirectUri: "https://n.example/cb",
        state: "s",
      }),
    ).toBe("https://n.example/cb?error=access_denied&iss=i&state=s");
    // A deny URL that carries a state already gets the request's.
    expect(
      cancelUrl(`${REDIRECT}?error=access_denied&state=&iss=i`, request),
    ).toBe(`${REDIRECT}?error=access_denied&state=s1&iss=i`);
    for (const bad of [
      undefined,
      "",
      "https://evil.example/helix/callback?error=access_denied",
      `${REDIRECT}?code=hlxpa_x`,
      "nope",
    ])
      expect(cancelUrl(bad, request), String(bad)).toBeNull();
  });
});

describe("installs", () => {
  it("find the app's live install, and their keys", () => {
    const a = install(),
      b = install({
        id: "pai_2",
        credentialId: "sc_2",
        app: { clientId: "hlxpc_other", name: "Other", publisher: "O" },
      });
    expect(existingInstall([b, a], request.clientId)).toBe(a);
    expect(existingInstall([b], request.clientId)).toBeUndefined();
    expect([...appCredentialIds([a, b])]).toEqual(["sc_1", "sc_2"]);
    expect(appCredentialIds([]).size).toBe(0);
  });
  it("say who approved them, when, and when they were last used", () => {
    const name = (u: string) => (u === "usr_ada" ? "Ada" : u),
      date = (at: string) => at.slice(0, 10);
    expect(installLine(install(), name, date)).toBe(
      "Approved by Ada · 2026-11-20 · Never used",
    );
    expect(
      installLine(install({ lastUsedAt: "2026-11-23T08:00:00Z" }), name, date),
    ).toBe("Approved by Ada · 2026-11-20 · Last used 2026-11-23");
    expect(
      installLine(
        install({
          approvedByUserId: undefined,
          approvedAt: undefined,
          createdAt: "2026-11-01T00:00:00Z",
        }),
        name,
        date,
      ),
    ).toBe("Approved by someone · 2026-11-01 · Never used");
    expect(
      installLine(
        { id: "i", credentialId: "c", approvedByUserId: "u", approvedAt: "t" },
        () => "Ada",
        () => "3 Nov",
      ),
    ).toBe("Approved by Ada · 3 Nov · Never used");
    expect(
      installLine(
        install({ approvedAt: undefined, createdAt: undefined }),
        name,
        date,
      ),
    ).toBe("Approved by Ada · Never used");
  });
});

describe("the build's flag", () => {
  const flag = async (env: Record<string, string>) => {
    vi.resetModules();
    for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
    try {
      return (await import("@/web/areas/workspace/partner-apps")).partnerApps;
    } finally {
      vi.unstubAllEnvs();
    }
  };
  it("offers connected apps where the build says so, never locally", async () => {
    expect(await flag({ NEXT_PUBLIC_FOUNDRY_APPS: "1" })).toBe(true);
    expect(await flag({ NEXT_PUBLIC_FOUNDRY_APPS: "" })).toBe(false);
    expect(
      await flag({
        NEXT_PUBLIC_FOUNDRY_APPS: "1",
        NEXT_PUBLIC_FOUNDRY_MODE: "local",
      }),
    ).toBe(false);
  });
});
