import { createHash } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { MockReply } from "../../mock/http";

// The mock's connected apps (mock/routes/partner-apps.ts), as hosted Helix
// answers: an app's request read and approved under the WFE's checks, the
// partner's token exchange under the backend's rules (the client first,
// PKCE, one use, the approver still allowed, sources still live), the
// install it makes or updates (a key reading only its grants, Usage off),
// and revoking it, here or through its key.

/** The mock's routes and state, loaded afresh: hosted, or a local API. */
async function load(local = false) {
  vi.resetModules();
  vi.stubEnv("MOCK_LOCAL_TOKEN", local ? "local-token" : "");
  const { routes } = await import("../../mock/routes");
  const state = await import("../../mock/state");
  const apps = await import("../../mock/routes/partner-apps");
  state.resetState();
  const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
  const call = async (
    method: HttpMethod,
    path: string,
    body?: unknown,
    userId = "usr_ada",
  ): Promise<MockReply & { body: any }> => {
    const url = new URL(path, "http://mock");
    for (const r of compiled) {
      const params = r.method === method && matchTemplate(r.c, url.pathname);
      if (!params) continue;
      return (await r.handle({
        method,
        path: url.pathname,
        params,
        query: url.searchParams,
        body,
        principal: { user: state.db.users.get(userId)!, sessionId: "s" },
      })) as MockReply & { body: any };
    }
    throw new Error(`no mock route for ${method} ${path}`);
  };
  // As POST /__mock/config does.
  const config = (knobs: Record<string, unknown>) => {
    Object.assign(state.config, knobs);
    state.applyConfig();
  };
  return { call, config, apps, state };
}
afterEach(() => vi.unstubAllEnvs());

const VERIFIER = "v".repeat(43),
  CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url"),
  W = "/v1/workspaces/ws_acme";
const STRIPE = { kind: "KIND_CONNECTOR", connectorId: "conn_acme_stripe" },
  SHOP = { kind: "KIND_CONNECTOR", connectorId: "conn_acme_shop" };

/** The mock's apps, and the bodies their requests send. */
async function setup(local = false) {
  const m = await load(local);
  const { northwind, contoso, fabrikam } = m.apps.PARTNER_APPS;
  const get = (over: Record<string, string> = {}, userId?: string) =>
    m.call(
      "GET",
      `/v1/partner-apps/authorization?${new URLSearchParams({
        clientId: northwind.clientId,
        redirectUri: northwind.redirectUris[0]!,
        scope: "foundry.read foundry.ask",
        ...over,
      })}`,
      undefined,
      userId,
    );
  const approveBody = (over: Record<string, unknown> = {}) => ({
    clientId: northwind.clientId,
    redirectUri: northwind.redirectUris[0],
    state: "partner-state",
    codeChallenge: CHALLENGE,
    scopes: ["foundry.read", "foundry.ask"],
    targets: [STRIPE],
    ...over,
  });
  const approve = (over: Record<string, unknown> = {}, userId?: string) =>
    m.call(
      "POST",
      `${W}/partner-apps/authorization:approve`,
      approveBody(over),
      userId,
    );
  /** Approves, then answers the code the browser went back with. */
  const code = async (over: Record<string, unknown> = {}, userId?: string) => {
    const r = await approve(over, userId);
    expect(r.status, JSON.stringify(r.body)).toBe(200);
    return new URL(r.body.redirectUrl).searchParams.get("code")!;
  };
  const exchange = (c: string, over: Record<string, unknown> = {}) =>
    m.apps.exchangePartnerCode({
      clientId: northwind.clientId,
      clientSecret: northwind.secret,
      code: c,
      codeVerifier: VERIFIER,
      ...over,
    }) as MockReply & { body: any };
  const installs = async () =>
    (await m.call("GET", `${W}/partner-apps/installs`)).body.installs as any[];
  const access = async () => (await m.call("GET", `${W}/foundry/access`)).body;
  const keyGrants = async (credentialId: string) =>
    ((await access()).grants as any[])
      .filter((g) => g.principal.id === credentialId)
      .map((g) => g.target.connectorId || g.target.documentSetId);
  return {
    ...m,
    northwind,
    contoso,
    fabrikam,
    get,
    approve,
    code,
    exchange,
    installs,
    access,
    keyGrants,
  };
}

describe("reading an app's request", () => {
  it("answers the app, what it asks for, and where Cancel goes", async () => {
    const { get, northwind } = await setup();
    const r = await get({ state: "s1" });
    expect(r.status).toBe(200);
    expect(r.body).toEqual({
      app: {
        clientId: northwind.clientId,
        name: "Northwind Insights",
        publisher: "Northwind Labs",
        homepageUrl: "https://northwind.example",
        verified: true,
      },
      scopes: ["foundry.read", "foundry.ask"],
      redirectOrigin: "https://northwind.example",
      denyUrl:
        "https://northwind.example/helix/callback?error=access_denied&state=s1&iss=https%3A%2F%2Fmcp.dev.helix-db.com%2Fapps",
    });
    // Without a state (the site keeps it), the deny URL has none.
    expect(new URL((await get()).body.denyUrl).searchParams.has("state")).toBe(
      false,
    );
  });
  it("reads an empty scope as all the app's, in canonical order", async () => {
    const { get, contoso } = await setup();
    expect((await get({ scope: "" })).body.scopes).toEqual([
      "foundry.read",
      "foundry.ask",
    ]);
    expect(
      (await get({ scope: "foundry.ask foundry.read" })).body.scopes,
    ).toEqual(["foundry.read", "foundry.ask"]);
    const r = await get({
      clientId: contoso.clientId,
      redirectUri: contoso.redirectUris[0]!,
      scope: "",
    });
    expect(r.body.scopes).toEqual(["foundry.read"]);
    expect(r.body.app.verified).toBe(false);
  });
  it("refuses an app Helix doesn't offer, as the WFE does", async () => {
    const { get, contoso, fabrikam, config } = await setup();
    const refused = async (
      over: Record<string, string>,
      status: number,
      message: string,
    ) => {
      const r = await get(over);
      expect(r.status, message).toBe(status);
      expect(r.body.message).toBe(message);
    };
    await refused(
      { clientId: "hlxpc_" + "x".repeat(43) },
      400,
      "This app isn't registered with Helix",
    );
    await refused(
      {
        clientId: fabrikam.clientId,
        redirectUri: fabrikam.redirectUris[0]!,
      },
      400,
      "This app isn't registered with Helix",
    );
    await refused(
      { redirectUri: "https://northwind.example/other" },
      400,
      "redirect_uri is not registered for this app",
    );
    await refused(
      { scope: "foundry.admin" },
      400,
      "scope foundry.admin isn't one this app may ask for",
    );
    await refused(
      {
        clientId: contoso.clientId,
        redirectUri: contoso.redirectUris[0]!,
        scope: "foundry.ask",
      },
      400,
      "scope foundry.ask isn't one this app may ask for",
    );
    await refused({ state: "s".repeat(513) }, 400, "state is too long");
    config({ partnerAppsAvailable: false });
    await refused({}, 400, "Connected apps aren't offered here");
  });
  it("is refused by a local API", async () => {
    const { get } = await setup(true);
    const r = await get();
    expect(r.body.message).toBe("Connected apps aren't offered here");
  });
});

describe("approving", () => {
  it("sends the browser back with a one-time code, the state and the issuer", async () => {
    const { approve } = await setup();
    const r = await approve();
    expect(r.status).toBe(200);
    const back = new URL(r.body.redirectUrl);
    expect(back.origin + back.pathname).toBe(
      "https://northwind.example/helix/callback",
    );
    expect(back.searchParams.get("code")).toMatch(/^hlxpa_[A-Za-z0-9_-]{43}$/);
    expect(back.searchParams.get("state")).toBe("partner-state");
    expect(back.searchParams.get("iss")).toBe(
      "https://mcp.dev.helix-db.com/apps",
    );
  });
  it("is for owners and admins of the workspace", async () => {
    const { approve, config } = await setup();
    expect((await approve({}, "usr_alan")).status).toBe(403);
    expect((await approve({}, "usr_eve")).status).toBe(403);
    config({ alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect((await approve({}, "usr_alan")).status).toBe(200);
  });
  it("checks everything the WFE checks", async () => {
    const { approve, fabrikam, config } = await setup();
    const refused = async (
      over: Record<string, unknown>,
      status: number,
      message: string,
    ) => {
      const r = await approve(over);
      expect(r.status, message).toBe(status);
      expect(r.body.message).toBe(message);
    };
    await refused(
      { clientId: fabrikam.clientId },
      400,
      "This app isn't registered with Helix",
    );
    await refused(
      { redirectUri: "https://northwind.example/helix/callback/" },
      400,
      "redirect_uri is not registered for this app",
    );
    await refused({ state: "" }, 400, "state is 1 to 512 characters");
    await refused(
      { state: "s".repeat(513) },
      400,
      "state is 1 to 512 characters",
    );
    await refused(
      { codeChallenge: "plain" },
      400,
      "code_challenge is an S256 PKCE challenge",
    );
    await refused(
      { scopes: [] },
      400,
      "scopes are some of what this app may ask for",
    );
    await refused(
      { scopes: ["foundry.read", "foundry.write"] },
      400,
      "scopes are some of what this app may ask for",
    );
    await refused({ targets: [] }, 400, "choose 1 to 1000 sources");
    await refused(
      { targets: [{ kind: "KIND_CONNECTOR" }] },
      400,
      `access: a connector grant can't name connector "", table "" and set ""`,
    );
    await refused(
      {
        targets: Array.from({ length: 1001 }, (_, i) => ({
          kind: "KIND_TABLE",
          connectorId: "conn_acme_stripe",
          table: `t${i}`,
        })),
      },
      400,
      "choose 1 to 1000 sources",
    );
    await refused(
      { targets: [{ kind: "KIND_CONNECTOR", connectorId: "conn_gone" }] },
      404,
      "One of the sources you chose was deleted",
    );
    await refused(
      {
        targets: [{ kind: "KIND_DOCUMENT_SET", documentSetId: "set_gone" }],
      },
      404,
      "One of the sources you chose was deleted",
    );
    config({ partnerAppsAvailable: false });
    await refused({}, 400, "Connected apps aren't offered here");
  });
  it("takes each source once", async () => {
    const { code, exchange, installs, keyGrants } = await setup();
    expect(
      (await exchange(await code({ targets: [STRIPE, STRIPE, SHOP] }))).status,
    ).toBe(200);
    const [i] = await installs();
    expect(await keyGrants(i.credentialId)).toEqual([
      "conn_acme_stripe",
      "conn_acme_shop",
    ]);
  });
});

describe("the partner's token exchange", () => {
  it("makes the install: a key reading only its sources, with Usage off", async () => {
    const { code, exchange, installs, access, keyGrants, call } = await setup();
    const r = await exchange(await code({ scopes: ["foundry.read"] }));
    expect(r.status).toBe(200);
    expect(r.body.accessToken).toMatch(/^hlxpt_/);
    expect(r.body.refreshToken).toMatch(/^hlxpr_/);
    expect(r.body.scope).toBe("foundry.read");
    expect(r.body.workspaceId).toBe("ws_acme");
    const list = await installs();
    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({
      app: { name: "Northwind Insights", verified: true },
      scopes: ["foundry.read"],
      approvedByUserId: "usr_ada",
    });
    expect(list[0].lastUsedAt).toBeUndefined();
    expect(list[0].appDisabled).toBeUndefined();
    const keys = (await call("GET", `${W}/service-credentials`)).body
      .credentials as any[];
    const key = keys.find((k) => k.id === list[0].credentialId);
    expect(key).toMatchObject({
      name: "App · Northwind Insights",
      tokenPrefix: "hlxs_partner",
      grants: [],
    });
    expect(await keyGrants(key.id)).toEqual(["conn_acme_stripe"]);
    expect((await access()).workflowsOff).toContainEqual({
      kind: "KIND_SERVICE_CREDENTIAL",
      id: key.id,
    });
  });
  it("authenticates the client before reading the code, and writes nothing otherwise", async () => {
    const { code, exchange, contoso, fabrikam, installs, apps } = await setup();
    const c = await code();
    expect((await exchange(c, { clientSecret: "wrong" })).body).toEqual({
      error: "invalid_client",
    });
    expect((await exchange(c, { clientSecret: "wrong" })).status).toBe(401);
    expect(
      (
        await exchange(c, {
          clientId: fabrikam.clientId,
          clientSecret: fabrikam.secret,
        })
      ).status,
    ).toBe(401);
    expect((await exchange(c, { clientId: "hlxpc_nobody" })).status).toBe(401);
    // Another app's code: refused, and still the right app's to use.
    expect(
      (
        await exchange(c, {
          clientId: contoso.clientId,
          clientSecret: contoso.secret,
        })
      ).body,
    ).toEqual({ error: "invalid_grant" });
    expect(await installs()).toEqual([]);
    expect((await exchange(c)).status).toBe(200);
    const [i] = await installs();
    expect(apps.liveFamilies(i.id)).toBe(1);
  });
  it("refuses a code that isn't one, without a write", async () => {
    const { exchange } = await setup();
    expect((await exchange("hlxpt_x")).body).toEqual({
      error: "invalid_grant",
    });
    expect((await exchange("hlxpa_unknown")).body).toEqual({
      error: "invalid_grant",
    });
  });
  it("spends a code on a wrong verifier or redirect URI", async () => {
    const { code, exchange, installs } = await setup();
    for (const over of [
      { codeVerifier: "w".repeat(43) },
      { codeVerifier: "" },
      { redirectUri: "https://northwind.example/other" },
    ]) {
      const c = await code();
      expect((await exchange(c, over)).body, JSON.stringify(over)).toEqual({
        error: "invalid_grant",
      });
      // Spent: the right verifier no longer works.
      expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    }
    expect(await installs()).toEqual([]);
  });
  it("lasts five minutes", async () => {
    const { code, exchange } = await setup();
    const c = await code();
    const now = Date.now();
    vi.spyOn(Date, "now").mockReturnValue(now + 5 * 60_000 + 1);
    try {
      expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    } finally {
      vi.restoreAllMocks();
    }
    expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
  });
  it("revokes the family a code made when it is presented again", async () => {
    const { code, exchange, installs, apps } = await setup();
    const c = await code();
    expect((await exchange(c)).status).toBe(200);
    const [i] = await installs();
    expect(apps.liveFamilies(i.id)).toBe(1);
    expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    expect(apps.liveFamilies(i.id)).toBe(0);
  });
  it("needs its approver to still own or administer the workspace", async () => {
    const { code, exchange, config, installs } = await setup();
    config({ alanRole: "WORKSPACE_ROLE_ADMIN" });
    const c = await code({}, "usr_alan");
    config({ alanRole: "WORKSPACE_ROLE_MEMBER" });
    expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    config({ alanRole: "WORKSPACE_ROLE_ADMIN" });
    expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    expect(await installs()).toEqual([]);
  });
  it("leaves nothing behind when a source was deleted since consent", async () => {
    const { code, exchange, installs, call } = await setup();
    const c = await code({ targets: [STRIPE, SHOP] });
    expect(
      (await call("DELETE", `${W}/foundry/connectors/conn_acme_shop`)).status,
    ).toBe(200);
    expect((await exchange(c)).body).toEqual({ error: "invalid_grant" });
    expect(await installs()).toEqual([]);
    const keys = (await call("GET", `${W}/service-credentials`)).body
      .credentials as any[];
    expect(keys.filter((k) => k.tokenPrefix === "hlxs_partner")).toEqual([]);
  });
  it("updates the live install on a new consent: its scopes and sources, the same key", async () => {
    const { code, exchange, installs, keyGrants } = await setup();
    await exchange(await code({ targets: [STRIPE] }));
    const [first] = await installs();
    await exchange(await code({ scopes: ["foundry.ask"], targets: [SHOP] }));
    const list = await installs();
    expect(list).toHaveLength(1);
    expect(list[0].id).toBe(first.id);
    expect(list[0].credentialId).toBe(first.credentialId);
    expect(list[0].scopes).toEqual(["foundry.ask"]);
    expect(await keyGrants(first.credentialId)).toEqual(["conn_acme_shop"]);
  });
  it("keeps 20 token families an install, revoking the oldest", async () => {
    const { code, exchange, installs, apps } = await setup();
    for (let n = 0; n < 21; n++) await exchange(await code());
    const [i] = await installs();
    expect(apps.liveFamilies(i.id)).toBe(20);
  });
});

describe("an install", () => {
  it("is used by the app, newest first", async () => {
    const { code, exchange, installs, apps, contoso, call } = await setup();
    await exchange(await code());
    // Approved a moment later: Contoso's install is the newer.
    await new Promise((r) => setTimeout(r, 5));
    const c = await call("POST", `${W}/partner-apps/authorization:approve`, {
      clientId: contoso.clientId,
      redirectUri: contoso.redirectUris[0],
      state: "s",
      codeChallenge: CHALLENGE,
      scopes: ["foundry.read"],
      targets: [SHOP],
    });
    await apps.exchangePartnerCode({
      clientId: contoso.clientId,
      clientSecret: contoso.secret,
      code: new URL(c.body.redirectUrl).searchParams.get("code")!,
      codeVerifier: VERIFIER,
    });
    const before = await installs();
    expect(before.map((i) => i.app.name)).toEqual([
      "Contoso Sync",
      "Northwind Insights",
    ]);
    const northwind = before[1];
    await new Promise((r) => setTimeout(r, 5));
    expect(apps.usePartnerInstall({ installId: northwind.id }).status).toBe(
      200,
    );
    const after = await installs();
    expect(after[0].id).toBe(northwind.id);
    expect(after[0].lastUsedAt).toBeDefined();
    expect(apps.usePartnerInstall({ installId: "pai_nope" }).status).toBe(401);
  });
  it("ends when revoked: its key, its sources, its tokens and its unused codes", async () => {
    const { code, exchange, installs, apps, call, keyGrants } = await setup();
    await exchange(await code());
    const [i] = await installs();
    const pending = await code();
    expect(
      (await call("DELETE", `${W}/partner-apps/installs/${i.id}`)).status,
    ).toBe(200);
    expect(await installs()).toEqual([]);
    expect(await keyGrants(i.credentialId)).toEqual([]);
    expect(apps.liveFamilies(i.id)).toBe(0);
    const keys = (await call("GET", `${W}/service-credentials`)).body
      .credentials as any[];
    expect(keys.find((k) => k.id === i.credentialId).revokedAt).toBeTruthy();
    expect(apps.usePartnerInstall({ installId: i.id }).status).toBe(401);
    // A code approved before the revoke brings nothing back.
    expect((await exchange(pending)).body).toEqual({ error: "invalid_grant" });
    expect(await installs()).toEqual([]);
    // Gone: a second revoke finds nothing.
    expect(
      (await call("DELETE", `${W}/partner-apps/installs/${i.id}`)).status,
    ).toBe(404);
    // A new consent makes a new install, with a new key.
    await exchange(await code());
    const [again] = await installs();
    expect(again.id).not.toBe(i.id);
    expect(again.credentialId).not.toBe(i.credentialId);
  });
  it("ends when its key is revoked in Settings › Access", async () => {
    const { code, exchange, installs, apps, call } = await setup();
    await exchange(await code());
    const [i] = await installs();
    expect(
      (await call("DELETE", `${W}/service-credentials/${i.credentialId}`))
        .status,
    ).toBe(200);
    expect(await installs()).toEqual([]);
    expect(apps.liveFamilies(i.id)).toBe(0);
  });
  it("is listed and revoked by owners and admins only, where offered", async () => {
    const { call, config } = await setup();
    expect(
      (await call("GET", `${W}/partner-apps/installs`, undefined, "usr_alan"))
        .status,
    ).toBe(403);
    expect(
      (
        await call(
          "DELETE",
          `${W}/partner-apps/installs/pai_x`,
          undefined,
          "usr_alan",
        )
      ).status,
    ).toBe(403);
    expect(
      (await call("GET", `/v1/workspaces/ws_other/partner-apps/installs`))
        .status,
    ).toBe(403);
    config({ partnerAppsAvailable: false });
    expect((await call("GET", `${W}/partner-apps/installs`)).status).toBe(400);
  });
  it("is seeded on Acme with `partnerAppInstalled`: Northwind reading Stripe", async () => {
    const { config, installs, keyGrants } = await setup();
    expect(await installs()).toEqual([]);
    config({ partnerAppInstalled: true });
    const list = await installs();
    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({
      id: "pai_acme_northwind",
      app: { name: "Northwind Insights" },
      scopes: ["foundry.read", "foundry.ask"],
    });
    expect(list[0].lastUsedAt).toBeDefined();
    expect(await keyGrants(list[0].credentialId)).toEqual(["conn_acme_stripe"]);
    // Once: a second read seeds nothing more.
    expect(await installs()).toHaveLength(1);
  });
  it("shows a suspended app's install as such", async () => {
    const { code, exchange, installs, apps } = await setup();
    await exchange(await code());
    (apps.PARTNER_APPS.northwind as { disabled: boolean }).disabled = true;
    try {
      expect((await installs())[0].appDisabled).toBe(true);
    } finally {
      (apps.PARTNER_APPS.northwind as { disabled: boolean }).disabled = false;
    }
  });
});
