import { beforeEach, describe, expect, it } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { handleSlackPage, slackLinkToken } from "../../mock/routes/slack";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's Helix Foundry for Slack (mock/routes/slack.ts), as the
// backend's api/slack.go answers: adding the app through the mock Slack's
// code, the install list, who may add, link and unlink, the link tokens'
// rules, disconnecting, and the knobs.

process.env.MOCK_ORIGIN = "http://127.0.0.1:4999";
const compiled = routes.map((r) => ({ ...r, c: compileTemplate(r.path) }));
async function call(
  method: HttpMethod,
  path: string,
  body?: unknown,
  userId = "usr_ada",
): Promise<MockReply & { body: any }> {
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
      principal: { user: db.users.get(userId)!, sessionId: "s" },
    })) as MockReply & { body: any };
  }
  throw new Error(`no mock route for ${method} ${path}`);
}
const W = "/v1/workspaces/ws_acme/foundry/slack";
/** The app's message to a Slack user: a link token and its URL. */
const mint = (link: Parameters<typeof slackLinkToken>[0]) =>
  slackLinkToken(link).body as { token: string; url: string };
const reason = (r: { body: any }) => r.body.details?.[0]?.reason;
const knobs = config as typeof config & Record<string, unknown>;

/** Starts an install as `userId` and has the mock Slack allow it: its code and state. */
async function allow(userId = "usr_ada") {
  const start = await call("POST", `${W}:start-install`, {}, userId);
  expect(start.status).toBe(200);
  const url = new URL(start.body.authorizeUrl);
  expect(url.pathname).toBe("/mock/slack/authorize");
  expect(url.searchParams.get("redirect_uri")).toMatch(/\/slack\/installed$/);
  const state = url.searchParams.get("state")!;
  // The page's Allow, as handleSlackPage answers it.
  let location = "";
  const res = {
    writeHead: (_: number, h: { Location: string }) => (location = h.Location),
    end: () => {},
  };
  handleSlackPage(
    { method: "POST" } as never,
    res as never,
    new URL("http://mock/mock/slack/authorize"),
    new URLSearchParams({ state, choice: "allow" }).toString(),
  );
  const back = new URL(location);
  return {
    code: back.searchParams.get("code")!,
    state: back.searchParams.get("state")!,
  };
}
async function installAcme() {
  const r = await call(
    "POST",
    "/v1/foundry/slack:finish-install",
    await allow(),
  );
  expect(r.status).toBe(200);
  return r;
}

beforeEach(() => resetState());

describe("adding the app", () => {
  it("shows nothing added at first, to owners and members", async () => {
    const ada = await call("GET", W);
    expect(ada.body.slack).toEqual({
      workspaceId: "ws_acme",
      installAllowed: true,
      canManage: true,
      install: null,
      myLink: null,
    });
    expect(
      (await call("GET", W, undefined, "usr_alan")).body.slack.canManage,
    ).toBe(false);
  });
  it("finishes once, for the member who started it", async () => {
    const back = await allow();
    // Another member can't spend Ada's state.
    const alan = await call(
      "POST",
      "/v1/foundry/slack:finish-install",
      back,
      "usr_alan",
    );
    expect(reason(alan)).toBe("FOUNDRY_SLACK_INSTALL_EXPIRED");
    const r = await call("POST", "/v1/foundry/slack:finish-install", back);
    expect(r.status).toBe(200);
    expect(r.body.slack.install).toMatchObject({
      teamName: "Acme Slack",
      installedByUserId: "usr_ada",
      installedByName: "Ada Lovelace",
    });
    // Spent.
    const again = await call("POST", "/v1/foundry/slack:finish-install", back);
    expect(reason(again)).toBe("FOUNDRY_SLACK_INSTALL_EXPIRED");
    // Installed: no second install starts.
    expect(reason(await call("POST", `${W}:start-install`, {}))).toBe(
      "FOUNDRY_SLACK_INSTALLED",
    );
  });
  it("refuses a code Slack didn't give, and an empty answer", async () => {
    const { state } = await allow();
    expect(
      reason(
        await call("POST", "/v1/foundry/slack:finish-install", {
          code: "made-up",
          state,
        }),
      ),
    ).toBe("FOUNDRY_SLACK_REFUSED");
    expect(
      (await call("POST", "/v1/foundry/slack:finish-install", {})).status,
    ).toBe(400);
  });
  it("returns a cancel to the site with access_denied", async () => {
    const start = await call("POST", `${W}:start-install`, {});
    const state = new URL(start.body.authorizeUrl).searchParams.get("state")!;
    let location = "";
    handleSlackPage(
      { method: "POST" } as never,
      {
        writeHead: (_: number, h: { Location: string }) =>
          (location = h.Location),
        end: () => {},
      } as never,
      new URL("http://mock/mock/slack/authorize"),
      new URLSearchParams({ state, choice: "cancel" }).toString(),
    );
    expect(new URL(location).searchParams.get("error")).toBe("access_denied");
    expect(
      handleSlackPage(
        { method: "GET" } as never,
        {} as never,
        new URL("http://mock/elsewhere"),
        "",
      ),
    ).toBe(false);
  });
  it("is for owners and admins only", async () => {
    expect(
      (await call("POST", `${W}:start-install`, {}, "usr_alan")).status,
    ).toBe(403);
    knobs.alanRole = "WORKSPACE_ROLE_ADMIN";
    expect(
      (await call("POST", `${W}:start-install`, {}, "usr_alan")).status,
    ).toBe(200);
  });
  it("follows the install list, Founder and another workspace's hold", async () => {
    knobs.slackInstallAllowed = false;
    expect((await call("GET", W)).body.slack.installAllowed).toBe(false);
    expect(reason(await call("POST", `${W}:start-install`, {}))).toBe(
      "FOUNDRY_SLACK_NOT_OFFERED",
    );
    knobs.slackInstallAllowed = true;
    knobs.agentEnabled = false;
    expect(reason(await call("POST", `${W}:start-install`, {}))).toBe(
      "FOUNDRY_AGENT_DISABLED",
    );
    delete knobs.agentEnabled;
    knobs.slackTeamTaken = true;
    const r = await call(
      "POST",
      "/v1/foundry/slack:finish-install",
      await allow(),
    );
    expect(reason(r)).toBe("FOUNDRY_SLACK_TEAM_TAKEN");
  });
  it("is off everywhere while foundry.slack is", async () => {
    knobs.slackAvailable = false;
    expect(reason(await call("GET", W))).toBe("FOUNDRY_SLACK_DISABLED");
    expect(
      reason(
        await call("POST", "/v1/foundry/slack:finish-install", {
          code: "c",
          state: "s",
        }),
      ),
    ).toBe("FOUNDRY_SLACK_DISABLED");
  });
});

describe("linking", () => {
  it("needs an install for a link token", () => {
    expect(slackLinkToken({ email: "ada@example.com" }).status).toBe(400);
  });
  it("links the caller whose email the Slack user has, once", async () => {
    await installAcme();
    const { token, url } = mint({
      email: "Ada@Example.com",
      handle: "ada",
    });
    expect(url).toBe(`/slack/link?workspace=ws_acme#t=${token}`);
    const preview = await call("POST", `${W}/links:preview`, { token });
    expect(preview.body).toEqual({
      teamName: "Acme Slack",
      slackUserId: "U0ADA",
      slackName: "ada",
      slackHandle: "ada",
      slackEmailMasked: "A•••@Example.com",
      matchesYou: true,
      refusal: "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED",
    });
    const linked = await call("POST", `${W}/links`, { token });
    expect(linked.body.link).toMatchObject({
      slackUserId: "U0ADA",
      userId: "usr_ada",
      userName: "Ada Lovelace",
    });
    expect((await call("GET", W)).body.slack.myLink.slackUserId).toBe("U0ADA");
    expect(reason(await call("POST", `${W}/links`, { token }))).toBe(
      "FOUNDRY_SLACK_LINK_EXPIRED",
    );
    expect(
      (await call("POST", `${W}/links:preview`, { token: "nope" })).status,
    ).toBe(400);
  });
  it("refuses someone else's email, a guest and an unconfirmed email", async () => {
    await installAcme();
    const cases = [
      [
        { email: "grace@example.com" },
        "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH",
      ],
      [
        { email: "ada@example.com", guest: true },
        "FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER",
      ],
      [
        { email: "ada@example.com", unconfirmed: true },
        "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED",
      ],
    ] as const;
    for (const [who, refusal] of cases) {
      const { token } = mint(who);
      const preview = await call("POST", `${W}/links:preview`, { token });
      expect(preview.body).toMatchObject({ matchesYou: false, refusal });
      const r = await call("POST", `${W}/links`, { token });
      expect(reason(r)).toBe("FOUNDRY_SLACK_LINK_REFUSED");
      expect(r.body.details[0].metadata.refusal).toBe(refusal);
    }
    knobs.slackEmailMismatch = true;
    const { token } = mint({ email: "ada@example.com" });
    expect(
      (await call("POST", `${W}/links:preview`, { token })).body.refusal,
    ).toBe("FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH");
  });
  it("keeps one live token per Slack user, and one link per person", async () => {
    await installAcme();
    const first = mint({ email: "ada@example.com" }).token;
    const second = mint({ email: "ada@example.com" }).token;
    expect(
      reason(await call("POST", `${W}/links:preview`, { token: first })),
    ).toBe("FOUNDRY_SLACK_LINK_EXPIRED");
    await call("POST", `${W}/links`, { token: second });
    const other = mint({
      email: "ada@example.com",
      slackUser: "U0ADA2",
    }).token;
    await call("POST", `${W}/links`, { token: other });
    const links = await call("GET", `${W}/links`);
    expect(links.body.links.map((l: any) => l.slackUserId)).toEqual(["U0ADA2"]);
  });
  it("lets members link and unlink themselves, and managers anyone", async () => {
    await installAcme();
    const ada = mint({ email: "ada@example.com" }).token,
      alan = mint({ email: "alan@example.com" }).token;
    await call("POST", `${W}/links`, { token: ada });
    expect(
      (await call("POST", `${W}/links`, { token: alan }, "usr_alan")).status,
    ).toBe(200);
    expect(
      (await call("GET", `${W}/links`, undefined, "usr_alan")).status,
    ).toBe(403);
    expect(
      (await call("DELETE", `${W}/links/U0ADA`, undefined, "usr_alan")).status,
    ).toBe(403);
    expect(
      (await call("DELETE", `${W}/links/U0ALAN`, undefined, "usr_alan")).status,
    ).toBe(200);
    expect((await call("DELETE", `${W}/links/U0ALAN`)).status).toBe(404);
    expect((await call("DELETE", `${W}/links/U0ADA`)).status).toBe(200);
    expect((await call("GET", `${W}/links`)).body.links).toEqual([]);
  });
});

describe("disconnecting", () => {
  it("removes the install, its links and its tokens, for managers", async () => {
    await installAcme();
    const linked = mint({ email: "ada@example.com" }).token;
    await call("POST", `${W}/links`, { token: linked });
    const waiting = mint({ email: "alan@example.com" }).token;
    expect((await call("POST", `${W}:disconnect`, {}, "usr_alan")).status).toBe(
      403,
    );
    const r = await call("POST", `${W}:disconnect`, {});
    expect(r.body.slack).toMatchObject({ install: null, myLink: null });
    expect(
      reason(
        await call(
          "POST",
          `${W}/links:preview`,
          { token: waiting },
          "usr_alan",
        ),
      ),
    ).toBe("FOUNDRY_SLACK_LINK_EXPIRED");
    expect((await call("POST", `${W}:disconnect`, {})).status).toBe(404);
    expect((await call("DELETE", `${W}/links/U0ADA`)).status).toBe(404);
    // Added again, no one is linked.
    await installAcme();
    expect((await call("GET", `${W}/links`)).body.links).toEqual([]);
  });
});

describe("bot mode", () => {
  const K = "/v1/workspaces/ws_acme/service-credentials";
  const keys = async () =>
    (await call("GET", K)).body.credentials as {
      id: string;
      name: string;
      grants: unknown[];
      revokedAt: string | null;
    }[];
  // The bot's keys, beside Acme's own agent's.
  const bots = async () =>
    (await keys()).filter((k) => k.name.startsWith("Slack bot"));
  const mode = (identityMode: unknown, userId = "usr_ada") =>
    call("PATCH", W, { identityMode }, userId);

  it("starts per person, with no bot", async () => {
    const r = await installAcme();
    expect(r.body.slack.install).toMatchObject({
      identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
      bot: null,
    });
  });
  it("refuses an unknown mode, a member and a workspace without an install", async () => {
    expect((await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT")).status).toBe(404);
    await installAcme();
    for (const bad of [
      "FOUNDRY_SLACK_IDENTITY_MODE_UNSPECIFIED",
      "BOT",
      undefined,
    ])
      expect((await mode(bad)).status).toBe(400);
    expect(
      (await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT", "usr_alan")).status,
    ).toBe(403);
    expect(await bots()).toEqual([]);
  });
  it("makes the bot a key with no grants, once, and turns it off by revoking it", async () => {
    await installAcme();
    const on = await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    expect(on.status).toBe(200);
    const [key] = await bots();
    expect(key).toMatchObject({
      name: "Slack bot (Acme Slack)",
      grants: [],
      revokedAt: null,
    });
    expect(on.body.slack.install).toMatchObject({
      identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
      bot: {
        credentialId: key!.id,
        credentialName: "Slack bot (Acme Slack)",
        broken: false,
        scope: "FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE",
      },
    });
    // The mode it has changes nothing: no second key.
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    expect(await bots()).toHaveLength(1);
    // Members read the mode too.
    expect(
      (await call("GET", W, undefined, "usr_alan")).body.slack.install.bot
        .credentialId,
    ).toBe(key!.id);

    const off = await mode("FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON");
    expect(off.body.slack.install).toMatchObject({
      identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
      bot: null,
    });
    expect((await bots())[0]!.revokedAt).not.toBeNull();
    // Again, nothing changes; on again, a new key.
    expect((await mode("FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON")).status).toBe(
      200,
    );
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    expect((await bots()).filter((k) => !k.revokedAt)).toHaveLength(1);
  });
  it("reads the whole workspace unless limited to its sources, which owners and admins change in place", async () => {
    const scoped = (
      identityMode: unknown,
      botScope: unknown,
      userId = "usr_ada",
    ) => call("PATCH", W, { identityMode, botScope }, userId);
    const scopeOf = async () =>
      (await call("GET", W, undefined, "usr_alan")).body.slack.install.bot
        .scope;
    await installAcme();
    // A scope goes with bot mode only, and is one of the two.
    expect(
      (
        await scoped(
          "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
          "FOUNDRY_SLACK_BOT_SCOPE_SOURCES",
        )
      ).status,
    ).toBe(400);
    expect(
      (await scoped("FOUNDRY_SLACK_IDENTITY_MODE_BOT", "EVERYTHING")).status,
    ).toBe(400);
    expect(await bots()).toEqual([]);
    // Turned on limited, it starts limited.
    await scoped(
      "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
      "FOUNDRY_SLACK_BOT_SCOPE_SOURCES",
    );
    expect(await scopeOf()).toBe("FOUNDRY_SLACK_BOT_SCOPE_SOURCES");
    const [key] = await bots();
    // Widened in place: the same key; and no scope keeps it.
    await scoped(
      "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
      "FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE",
    );
    expect(await scopeOf()).toBe("FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE");
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    expect(await scopeOf()).toBe("FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE");
    expect(await bots()).toEqual([key]);
    // A member reads it and can't change it.
    expect(
      (
        await scoped(
          "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
          "FOUNDRY_SLACK_BOT_SCOPE_SOURCES",
          "usr_alan",
        )
      ).status,
    ).toBe(403);
    expect(await scopeOf()).toBe("FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE");
  });
  it("cuts the key's name to 64 characters", async () => {
    db.workspaces.get("ws_acme")!.displayName = "A".repeat(80);
    await installAcme();
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    const [key] = await bots();
    expect([...key!.name]).toHaveLength(64);
    expect(key!.name).toMatch(/^Slack bot \(A+…\)$/);
  });
  it("is broken once its key is revoked in Access, or by the knob", async () => {
    await installAcme();
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    knobs.slackBotBroken = true;
    expect((await call("GET", W)).body.slack.install.bot.broken).toBe(true);
    delete knobs.slackBotBroken;
    const [key] = await bots();
    expect((await call("DELETE", `${K}/${key!.id}`)).status).toBe(200);
    expect((await call("GET", W)).body.slack.install.bot).toEqual({
      credentialId: key!.id,
      credentialName: "",
      broken: true,
      scope: "FOUNDRY_SLACK_BOT_SCOPE_WORKSPACE",
    });
    // Off and on again gives it a new key that works.
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON");
    const on = await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    expect(on.body.slack.install.bot.broken).toBe(false);
  });
  it("revokes the bot's key on a disconnect, and an install again starts per person", async () => {
    await installAcme();
    await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT");
    await call("POST", `${W}:disconnect`, {});
    expect((await bots())[0]!.revokedAt).not.toBeNull();
    const again = await installAcme();
    expect(again.body.slack.install).toMatchObject({
      identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
      bot: null,
    });
  });
  it("refuses every mode change where foundry.slack is off", async () => {
    await installAcme();
    knobs.slackAvailable = false;
    expect(reason(await mode("FOUNDRY_SLACK_IDENTITY_MODE_BOT"))).toBe(
      "FOUNDRY_SLACK_DISABLED",
    );
  });
});
