import { createHash } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { MockReply } from "../../mock/http";

// The mock's Helix Slack relay (mock/routes/relay.ts) and a local API's
// Slack through it (mock/routes/slack.ts), as the backend's api/relay.go,
// api/relay_local.go and api/slack.go answer: Helix authorizing, listing and
// revoking a local install's link, the door's redeem and Add to Slack, and,
// on a local API, linking to Helix through the mock /relay/link, adding the
// app through it, the owner's own link, and who else may use it.

const ORIGIN = "http://localhost:3101";

/** The mock's routes and state, loaded afresh: hosted, or a local API. */
async function load(local = false) {
  vi.resetModules();
  vi.stubEnv("MOCK_LOCAL_TOKEN", local ? "local-token" : "");
  vi.stubEnv("MOCK_ORIGIN", "http://127.0.0.1:4999");
  vi.stubEnv("FOUNDRY_ORIGIN", ORIGIN);
  const { routes } = await import("../../mock/routes");
  const state = await import("../../mock/state");
  const relay = await import("../../mock/routes/relay");
  const slack = await import("../../mock/routes/slack");
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
  const config = (knobs: Record<string, unknown>) =>
    Object.assign(state.config, knobs);
  return { call, config, relay, slack, state };
}
afterEach(() => vi.unstubAllEnvs());

const W = "/v1/workspaces/ws_acme/foundry";
const reason = (r: { body: any }) => r.body.details?.[0]?.reason;
const VERIFIER = "v".repeat(43),
  CHALLENGE = createHash("sha256").update(VERIFIER).digest("base64url"),
  STATE = "state-of-the-install",
  REDIRECT = `${ORIGIN}/settings/slack`;
const request = (over: Record<string, unknown> = {}) => ({
  state: STATE,
  codeChallenge: CHALLENGE,
  redirectUri: REDIRECT,
  name: "Ada’s laptop",
  ...over,
});

/** A mock page's answer: its status, page, or where it redirected. */
function page(
  handle: (req: never, res: never, url: URL, raw: string) => boolean,
  method: "GET" | "POST",
  url: string,
  form = "",
) {
  const out = { status: 0, location: "", html: "" };
  const res = {
    writeHead: (status: number, h?: Record<string, string>) => {
      out.status = status;
      out.location = h?.Location || "";
    },
    end: (html?: string) => (out.html = html || ""),
  };
  const handled = handle(
    { method } as never,
    res as never,
    new URL(url, "http://127.0.0.1:4999"),
    form,
  );
  return { handled, ...out };
}

describe("Helix's side", () => {
  it("authorizes a link for an owner or admin, back to the install with a code", async () => {
    const { call } = await load();
    const r = await call("POST", `${W}/relay-links:authorize`, request());
    expect(r.status).toBe(200);
    const back = new URL(r.body.redirectUrl);
    expect(`${back.origin}${back.pathname}`).toBe(REDIRECT);
    expect(back.searchParams.get("state")).toBe(STATE);
    expect(back.searchParams.get("code")).toMatch(/^[\w-]{43}$/);
  });
  it("checks the state, then the caller, then the install list, then the rest", async () => {
    const { call, config } = await load();
    const authorize = (body: unknown, user = "usr_ada") =>
      call("POST", `${W}/relay-links:authorize`, body, user);
    expect(
      (await authorize(request({ state: "short" }), "usr_alan")).status,
    ).toBe(400);
    expect((await authorize(request(), "usr_alan")).status).toBe(403);
    config({ slackInstallAllowed: false });
    expect(reason(await authorize(request({ codeChallenge: "x" })))).toBe(
      "FOUNDRY_SLACK_NOT_OFFERED",
    );
    config({ slackInstallAllowed: true });
    for (const bad of [
      { codeChallenge: "x" },
      { redirectUri: "https://evil.example/settings/slack" },
      { redirectUri: "http://localhost:99999/settings/slack" },
      { name: "" },
      { name: "a‮b" },
      { name: "é".repeat(65) },
    ]) {
      const r = await authorize(request(bad));
      expect(r.status, JSON.stringify(bad)).toBe(400);
    }
    expect((await authorize({})).status).toBe(400);
  });
  it("is off where the relay is", async () => {
    const { call, config } = await load();
    for (const knob of [{ relayAvailable: false }, { slackAvailable: false }]) {
      config(knob);
      for (const r of [
        await call("POST", `${W}/relay-links:authorize`, request()),
        await call("GET", `${W}/relay-links`),
        await call("POST", `${W}/relay-links/rl_1:revoke`, {}),
      ])
        expect(reason(r)).toBe("FOUNDRY_RELAY_DISABLED");
      config({ relayAvailable: true, slackAvailable: true });
    }
    // Strangers get the WFE's refusal first.
    expect(
      (await call("GET", "/v1/workspaces/ws_other/foundry/relay-links")).status,
    ).toBe(403);
  });
  it("links the install once its code is redeemed with the verifier, then lists and revokes it", async () => {
    const { call, relay } = await load();
    const code = new URL(
      (
        await call("POST", `${W}/relay-links:authorize`, request())
      ).body.redirectUrl,
    ).searchParams.get("code")!;
    expect(
      relay.redeemRelayCode({ code, codeVerifier: "w".repeat(43) }).status,
    ).toBe(404);
    expect(relay.redeemRelayCode({ code }).status).toBe(404);
    const redeemed = relay.redeemRelayCode({
      code,
      codeVerifier: VERIFIER,
      teams: ["Laptop Slack"],
    });
    expect(redeemed.status).toBe(200);
    // Once only.
    expect(relay.redeemRelayCode({ code, codeVerifier: VERIFIER }).status).toBe(
      404,
    );
    const { linkId } = redeemed.body as { linkId: string };
    const list = await call("GET", `${W}/relay-links`);
    expect(list.body.links).toEqual([
      expect.objectContaining({
        id: linkId,
        name: "Ada’s laptop",
        createdByUserId: "usr_ada",
        createdByName: "Ada Lovelace",
        installs: [expect.objectContaining({ teamName: "Laptop Slack" })],
        deliveriesToday: "12",
        callsToday: "30",
      }),
    ]);
    // Members don't see or revoke them.
    expect(
      (await call("GET", `${W}/relay-links`, undefined, "usr_alan")).status,
    ).toBe(403);
    expect(
      (await call("POST", `${W}/relay-links/${linkId}:revoke`, {}, "usr_alan"))
        .status,
    ).toBe(403);
    expect(
      (await call("POST", `${W}/relay-links/${linkId}:revoke`, {})).status,
    ).toBe(200);
    expect((await call("GET", `${W}/relay-links`)).body.links).toEqual([]);
    expect(
      (await call("POST", `${W}/relay-links/${linkId}:revoke`, {})).status,
    ).toBe(404);
  });
  it("finishes a linked install's Add to Slack on the link, back on the install", async () => {
    const { call, relay, slack } = await load();
    const code = new URL(
      (
        await call("POST", `${W}/relay-links:authorize`, request())
      ).body.redirectUrl,
    ).searchParams.get("code")!;
    const { linkId } = relay.redeemRelayCode({ code, codeVerifier: VERIFIER })
      .body as { linkId: string };
    expect(relay.relaySlackInstall({ linkId: "nope" }).status).toBe(401);
    const started = relay.relaySlackInstall({ linkId });
    const state = new URL(
      (started.body as { authorizeUrl: string }).authorizeUrl,
    ).searchParams.get("state")!;
    const allowed = page(
      slack.handleSlackPage,
      "POST",
      "/mock/slack/authorize",
      new URLSearchParams({ state, choice: "allow" }).toString(),
    );
    const back = new URL(allowed.location);
    expect(back.pathname).toBe("/slack/installed");
    const finished = await call("POST", "/v1/foundry/slack:finish-install", {
      code: back.searchParams.get("code"),
      state,
    });
    expect(finished.body.returnTo).toBe(REDIRECT);
    // The workspace's own Slack is untouched; the link serves the team.
    expect(finished.body.slack.install).toBeNull();
    expect(
      (await call("GET", `${W}/relay-links`)).body.links[0].installs,
    ).toEqual([expect.objectContaining({ teamName: "Laptop Slack" })]);
  });
  it("refuses a local install's routes", async () => {
    const { call } = await load();
    for (const r of [
      await call("GET", `${W}/relay`),
      await call("POST", `${W}/relay:start-link`, {}),
      await call("POST", `${W}/relay:complete-link`, {
        code: "c",
        state: STATE,
      }),
      await call("PATCH", `${W}/relay`, { slackApprovals: false }),
      await call("POST", `${W}/relay:unlink`, {}),
    ])
      expect(reason(r)).toBe("FOUNDRY_RELAY_LOCAL_ONLY");
    expect(
      reason(
        await call("PUT", `${W}/slack/links/U0BEA/access`, {
          access: "FOUNDRY_SLACK_LINK_ACCESS_READ",
        }),
      ),
    ).toBe("FOUNDRY_SLACK_LOCAL_ONLY");
  });
});

/** Links the local install to Helix through the mock /relay/link. */
async function linkLocal(m: Awaited<ReturnType<typeof load>>) {
  const start = await m.call("POST", `${W}/relay:start-link`, {});
  const url = new URL(start.body.authorizeUrl);
  expect(url.pathname).toBe("/mock/relay/link");
  expect(url.searchParams.get("redirect_uri")).toBe(REDIRECT);
  const state = url.searchParams.get("state")!;
  const shown = page(m.relay.handleRelayPage, "GET", url.pathname + url.search);
  expect(shown.status).toBe(200);
  expect(shown.html).toContain("Link the Foundry on Ada’s laptop to Acme");
  const linked = page(
    m.relay.handleRelayPage,
    "POST",
    "/mock/relay/link",
    new URLSearchParams({
      state,
      redirect_uri: REDIRECT,
      choice: "link",
    }).toString(),
  );
  const back = new URL(linked.location);
  expect(`${back.origin}${back.pathname}`).toBe(REDIRECT);
  return { code: back.searchParams.get("code")!, state };
}

/** Adds the app to the local install through Helix, as the mock Slack's Allow. */
async function installLocal(m: Awaited<ReturnType<typeof load>>) {
  const start = await m.call("POST", `${W}/slack:start-install`, {});
  expect(start.status).toBe(200);
  const state = new URL(start.body.authorizeUrl).searchParams.get("state")!;
  const allowed = page(
    m.slack.handleSlackPage,
    "POST",
    "/mock/slack/authorize",
    new URLSearchParams({ state, choice: "allow" }).toString(),
  );
  expect(allowed.location).toBe(REDIRECT);
}

describe("a local install", () => {
  it("links to Helix through its /relay/link, once", async () => {
    const m = await load(true);
    expect((await m.call("GET", `${W}/relay`)).body.relay).toEqual({
      linked: false,
    });
    const { code, state } = await linkLocal(m);
    expect(
      reason(
        await m.call("POST", `${W}/relay:complete-link`, {
          code: "other",
          state,
        }),
      ),
    ).toBe("FOUNDRY_RELAY_LINK_EXPIRED");
    const done = await m.call("POST", `${W}/relay:complete-link`, {
      code,
      state,
    });
    expect(done.body.relay).toEqual(
      expect.objectContaining({
        linked: true,
        accountEmail: "ada@example.com",
        hostedOrigin: "https://ingest.helix-db.com",
        slackApprovals: true,
      }),
    );
    // Spent.
    expect(
      reason(await m.call("POST", `${W}/relay:complete-link`, { code, state })),
    ).toBe("FOUNDRY_RELAY_LINK_EXPIRED");
    expect(
      (await m.call("POST", `${W}/relay:complete-link`, { state: "x" })).status,
    ).toBe(400);
  });
  it("hands a cancel back, and refuses a page it never started or a foreign return", async () => {
    const m = await load(true);
    const url = new URL(
      (await m.call("POST", `${W}/relay:start-link`, {})).body.authorizeUrl,
    );
    const state = url.searchParams.get("state")!;
    const cancelled = page(
      m.relay.handleRelayPage,
      "POST",
      "/mock/relay/link",
      new URLSearchParams({
        state,
        redirect_uri: REDIRECT,
        choice: "cancel",
      }).toString(),
    );
    expect(new URL(cancelled.location).searchParams.get("error")).toBe(
      "access_denied",
    );
    expect(
      page(m.relay.handleRelayPage, "GET", "/mock/relay/link?state=nope")
        .status,
    ).toBe(404);
    expect(
      page(
        m.relay.handleRelayPage,
        "POST",
        "/mock/relay/link",
        new URLSearchParams({
          state,
          redirect_uri: "https://evil.example/settings/slack",
          choice: "link",
        }).toString(),
      ).status,
    ).toBe(404);
    expect(page(m.relay.handleRelayPage, "GET", "/other").handled).toBe(false);
  });
  it("says why completing failed, by the knob", async () => {
    const m = await load(true);
    const { code, state } = await linkLocal(m);
    m.config({ relayLinkOutcome: "unavailable" });
    expect(
      (await m.call("POST", `${W}/relay:complete-link`, { code, state }))
        .status,
    ).toBe(503);
    m.config({ relayLinkOutcome: "expired" });
    expect(
      reason(await m.call("POST", `${W}/relay:complete-link`, { code, state })),
    ).toBe("FOUNDRY_RELAY_LINK_EXPIRED");
  });
  it("reads as linked, online or not, by the knobs, and turns approvals off", async () => {
    const m = await load(true);
    expect(
      (await m.call("PATCH", `${W}/relay`, { slackApprovals: false })).status,
    ).toBe(404);
    m.config({ relayLinked: true, relayOnline: false });
    const r = (await m.call("GET", `${W}/relay`)).body.relay;
    expect(r.linked).toBe(true);
    expect(Date.now() - Date.parse(r.lastOkAt)).toBeGreaterThan(9 * 60_000);
    expect(
      (await m.call("PATCH", `${W}/relay`, { slackApprovals: false })).body
        .relay.slackApprovals,
    ).toBe(false);
    // Only the owner, an owner or admin, manages it.
    expect(
      (await m.call("GET", `${W}/relay`, undefined, "usr_alan")).status,
    ).toBe(403);
  });
  it("adds Slack only once linked, through Helix, back to Settings › Slack", async () => {
    const m = await load(true);
    expect(reason(await m.call("POST", `${W}/slack:start-install`, {}))).toBe(
      "FOUNDRY_RELAY_NOT_LINKED",
    );
    m.config({ relayLinked: true });
    await installLocal(m);
    const slack = (await m.call("GET", `${W}/slack`)).body.slack;
    expect(slack.install.teamName).toBe("Acme Slack");
    expect(slack.myLink).toBeNull();
    // A local install has no shared bot.
    expect(
      reason(
        await m.call("PATCH", `${W}/slack`, {
          identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
        }),
      ),
    ).toBe("FOUNDRY_SLACK_BOT_HOSTED_ONLY");
  });
  it("a cancel on Slack's page goes back too, adding nothing", async () => {
    const m = await load(true);
    m.config({ relayLinked: true });
    const state = new URL(
      (await m.call("POST", `${W}/slack:start-install`, {})).body.authorizeUrl,
    ).searchParams.get("state")!;
    expect(
      page(
        m.slack.handleSlackPage,
        "POST",
        "/mock/slack/authorize",
        new URLSearchParams({ state, choice: "cancel" }).toString(),
      ).location,
    ).toBe(REDIRECT);
    expect((await m.call("GET", `${W}/slack`)).body.slack.install).toBeNull();
  });
  it("links only the Slack account that added it, as the owner, with no email", async () => {
    const m = await load(true);
    m.config({ relayLinked: true });
    await installLocal(m);
    const other = m.slack.slackLinkToken({ email: "x@y.z", slackUser: "U0BEA" })
      .body as { token: string };
    expect(
      reason(
        await m.call("POST", `${W}/slack/links:preview`, {
          token: other.token,
        }),
      ),
    ).toBe("FOUNDRY_SLACK_LINK_EXPIRED");
    const mine = m.slack.slackLinkToken({ email: "ada@example.com" }).body as {
      token: string;
    };
    const preview = await m.call("POST", `${W}/slack/links:preview`, {
      token: mine.token,
    });
    expect(preview.body).toEqual(
      expect.objectContaining({
        slackUserId: m.slack.LOCAL_INSTALLER,
        slackEmailMasked: "",
        matchesYou: true,
      }),
    );
    const linked = await m.call("POST", `${W}/slack/links`, {
      token: mine.token,
    });
    expect(linked.body.link.access).toBe("FOUNDRY_SLACK_LINK_ACCESS_FULL");
    expect(
      (await m.call("GET", `${W}/slack`)).body.slack.myLink.slackUserId,
    ).toBe(m.slack.LOCAL_INSTALLER);
  });
  it("lets the owner allow who asked, read-only, and remove them", async () => {
    const m = await load(true);
    m.config({ relayLinked: true });
    // No install yet: nothing to ask.
    expect(
      m.slack.slackAccessRequest({ slackUser: "U0BEA", name: "Bea" }).status,
    ).toBe(400);
    await installLocal(m);
    const mine = m.slack.slackLinkToken({ email: "ada@example.com" }).body as {
      token: string;
    };
    await m.call("POST", `${W}/slack/links`, { token: mine.token });
    for (const body of [
      { slackUser: m.slack.LOCAL_INSTALLER },
      { slackUser: "lower" },
      {},
    ])
      expect(
        m.slack.slackAccessRequest(body).status,
        JSON.stringify(body),
      ).toBe(400);
    expect(
      m.slack.slackAccessRequest({ slackUser: "U0BEA", name: "Bea" }).body,
    ).toEqual({
      requested: true,
    });
    // Asking again keeps the first.
    m.slack.slackAccessRequest({ slackUser: "U0BEA", name: "Bea again" });
    let people = (await m.call("GET", `${W}/slack/links`)).body;
    expect(people.requests).toEqual([
      expect.objectContaining({ slackUserId: "U0BEA", slackName: "Bea" }),
    ]);
    const access = (user: string, value: string, as = "usr_ada") =>
      m.call("PUT", `${W}/slack/links/${user}/access`, { access: value }, as);
    expect(
      (await access("U0BEA", "FOUNDRY_SLACK_LINK_ACCESS_FULL")).status,
    ).toBe(400);
    expect((await access("bad", "FOUNDRY_SLACK_LINK_ACCESS_READ")).status).toBe(
      400,
    );
    expect(
      reason(
        await access(m.slack.LOCAL_INSTALLER, "FOUNDRY_SLACK_LINK_ACCESS_NONE"),
      ),
    ).toBe("FOUNDRY_SLACK_OWNER_LINK");
    expect(
      (await access("U0NOBODY", "FOUNDRY_SLACK_LINK_ACCESS_READ")).status,
    ).toBe(404);
    const allowed = await access("U0BEA", "FOUNDRY_SLACK_LINK_ACCESS_READ");
    expect(allowed.body.link).toEqual(
      expect.objectContaining({
        slackUserId: "U0BEA",
        userId: "usr_ada",
        access: "FOUNDRY_SLACK_LINK_ACCESS_READ",
      }),
    );
    // Allowing again keeps the one link; asking once allowed asks nothing.
    await access("U0BEA", "FOUNDRY_SLACK_LINK_ACCESS_READ");
    expect(
      m.slack.slackAccessRequest({ slackUser: "U0BEA", name: "Bea" }).body,
    ).toEqual({ requested: false });
    people = (await m.call("GET", `${W}/slack/links`)).body;
    expect(people.requests).toEqual([]);
    expect(people.links.map((l: { access: string }) => l.access)).toEqual([
      "FOUNDRY_SLACK_LINK_ACCESS_FULL",
      "FOUNDRY_SLACK_LINK_ACCESS_READ",
    ]);
    // The owner's link stays theirs, not the read-only one's.
    expect(
      (await m.call("GET", `${W}/slack`)).body.slack.myLink.slackUserId,
    ).toBe(m.slack.LOCAL_INSTALLER);
    // Relinking the owner keeps who they allowed.
    const again = m.slack.slackLinkToken({ email: "ada@example.com" }).body as {
      token: string;
    };
    await m.call("POST", `${W}/slack/links`, { token: again.token });
    expect((await m.call("GET", `${W}/slack/links`)).body.links).toHaveLength(
      2,
    );
    expect(
      (await access("U0BEA", "FOUNDRY_SLACK_LINK_ACCESS_NONE")).body,
    ).toEqual({});
    expect((await m.call("GET", `${W}/slack/links`)).body.links).toHaveLength(
      1,
    );
    // A request is dismissed the same way.
    m.slack.slackAccessRequest({ slackUser: "U0CAL", name: "Cal" });
    await access("U0CAL", "FOUNDRY_SLACK_LINK_ACCESS_NONE");
    expect((await m.call("GET", `${W}/slack/links`)).body.requests).toEqual([]);
  });
  it("refuses access changes to a member, and before Slack is added", async () => {
    const m = await load(true);
    const put = (as: string) =>
      m.call(
        "PUT",
        `${W}/slack/links/U0BEA/access`,
        { access: "FOUNDRY_SLACK_LINK_ACCESS_READ" },
        as,
      );
    expect((await put("usr_ada")).status).toBe(404);
    m.config({ slackAvailable: false });
    expect(reason(await put("usr_ada"))).toBe("FOUNDRY_SLACK_DISABLED");
  });
  it("forgets its Slack when unlinked, and when linked again", async () => {
    const m = await load(true);
    const first = await linkLocal(m);
    await m.call("POST", `${W}/relay:complete-link`, first);
    await installLocal(m);
    // Linking again replaces the link and forgets its Slack.
    const second = await linkLocal(m);
    await m.call("POST", `${W}/relay:complete-link`, second);
    expect((await m.call("GET", `${W}/slack`)).body.slack.install).toBeNull();
    await installLocal(m);
    m.slack.slackAccessRequest({ slackUser: "U0BEA", name: "Bea" });
    const unlinked = await m.call("POST", `${W}/relay:unlink`, {});
    expect(unlinked.body.relay).toEqual({ linked: false });
    expect((await m.call("GET", `${W}/slack`)).body.slack.install).toBeNull();
    expect((await m.call("GET", `${W}/slack/links`)).body.requests).toEqual([]);
    // Unlinking what isn't linked changes nothing; a seeded link unlinks for good.
    expect((await m.call("POST", `${W}/relay:unlink`, {})).status).toBe(200);
    m.config({ relayLinked: true });
    await m.call("POST", `${W}/relay:unlink`, {});
    expect((await m.call("GET", `${W}/relay`)).body.relay.linked).toBe(false);
  });
  it("refuses Helix's side", async () => {
    const m = await load(true);
    expect(
      reason(await m.call("POST", `${W}/relay-links:authorize`, request())),
    ).toBe("FOUNDRY_RELAY_DISABLED");
  });
});
