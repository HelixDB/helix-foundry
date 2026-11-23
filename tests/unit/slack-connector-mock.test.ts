import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { HttpMethod } from "@/shared/route-template";
import { compileTemplate, matchTemplate } from "@/shared/route-template";
import type { MockReply } from "../../mock/http";
import { routes } from "../../mock/routes";
import { slackFeedPost } from "../../mock/routes/data";
import { handleSlackPage } from "../../mock/routes/slack";
import { config, db, resetState } from "../../mock/state";

// The mock WFE's Slack data connector on Helix's Slack app
// (mock/routes/data.ts, slack.ts), as the backend's api/slack_connector.go
// answers: the Slack kind's setup, binding a connection with its refusals,
// its status and backfill, a token's staged switch and its flip, Switch
// now, back to a token, Slack's messages, an install's permissions,
// admin and return path, and a removed install.

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
const F = "/v1/workspaces/ws_acme/foundry";
const W = `${F}/slack`;
const reason = (r: { body: any }) => r.body.details?.[0]?.reason;
const knobs = config as typeof config & Record<string, unknown>;
const later = (ms: number) => vi.setSystemTime(Date.now() + ms);
const app = (notice = 1, days = 30) => ({
  slack: { historyDays: days, helixApp: { noticeVersion: notice } },
});
const TOKEN = "xoxb-1234-5678-" + "a".repeat(16) + "WXYZ";

/** Adds the app to Acme's Slack, coming back to `returnPath`. */
async function install(returnPath?: string) {
  const start = await call(
    "POST",
    `${W}:start-install`,
    returnPath === undefined ? {} : { returnPath },
  );
  expect(start.status).toBe(200);
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
    new URLSearchParams({ state, choice: "allow" }).toString(),
  );
  const back = new URL(location);
  const r = await call("POST", "/v1/foundry/slack:finish-install", {
    code: back.searchParams.get("code"),
    state: back.searchParams.get("state"),
  });
  expect(r.status).toBe(200);
  return r;
}
const slackKind = async (userId = "usr_ada") =>
  (
    await call("GET", `${F}/connector-kinds`, undefined, userId)
  ).body.kinds.find((k: any) => k.kind === "slack");
const connector = async (id: string) =>
  (await call("GET", `${F}/connectors/${id}`)).body.connector;

beforeEach(() => {
  resetState();
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-11-21T12:00:00Z"));
});
afterEach(() => vi.useRealTimers());

describe("the Slack kind's setup", () => {
  it("is offered where the feed is on, with the install once it is added", async () => {
    expect((await slackKind()).slack).toEqual({
      install: null,
      canInstall: true,
      inUseByConnectorId: "",
      installerAdmin: false,
      noticeVersion: 1,
    });
    // Members can't add it.
    expect((await slackKind("usr_alan")).slack.canInstall).toBe(false);
    await install();
    const setup = (await slackKind()).slack;
    expect(setup.install).toMatchObject({
      teamName: "Acme Slack",
      missingScopes: [],
      installerAdmin: true,
    });
    expect(setup.installerAdmin).toBe(true);
    knobs.slackFeedOff = true;
    expect((await slackKind()).slack).toBeUndefined();
  });

  it("says what an install lacks, whether a Slack admin made it, and Slack's limit", async () => {
    await install();
    knobs.slackScopesOld = true;
    knobs.slackInstallerMember = true;
    knobs.slackEventsLimited = true;
    const { install: i, installerAdmin } = (await slackKind()).slack;
    expect(i.missingScopes).toEqual(["channels:history", "groups:history"]);
    expect(installerAdmin).toBe(false);
    expect(Date.parse(i.eventsLimitedAt)).toBe(Date.now() - 3_600_000);
    // Settings › Slack shows the same install.
    expect((await call("GET", W)).body.slack.install).toMatchObject({
      missingScopes: ["channels:history", "groups:history"],
      installerAdmin: false,
    });
    // With the feed off, nothing is missing.
    knobs.slackFeedOff = true;
    expect((await call("GET", W)).body.slack.install.missingScopes).toEqual([]);
  });
});

describe("adding the app again", () => {
  it("starts again while the install lacks a permission or an admin's install", async () => {
    await install();
    expect(reason(await call("POST", `${W}:start-install`, {}))).toBe(
      "FOUNDRY_SLACK_INSTALLED",
    );
    knobs.slackScopesOld = true;
    expect((await call("POST", `${W}:start-install`, {})).status).toBe(200);
    delete knobs.slackScopesOld;
    knobs.slackInstallerMember = true;
    expect((await call("POST", `${W}:start-install`, {})).status).toBe(200);
    // Without the feed, a non-admin's install lacks nothing.
    knobs.slackFeedOff = true;
    expect(reason(await call("POST", `${W}:start-install`, {}))).toBe(
      "FOUNDRY_SLACK_INSTALLED",
    );
  });

  it("carries the return path it was started with back through Finish", async () => {
    expect((await install("/data?add=slack")).body.returnPath).toBe(
      "/data?add=slack",
    );
    knobs.slackScopesOld = true;
    expect((await install("/data/connections/conn_1")).body.returnPath).toBe(
      "/data/connections/conn_1",
    );
    expect((await install("/settings/slack")).body.returnPath).toBe("");
    for (const returnPath of [
      "https://evil.example/",
      "/data/connections/a/b",
      "/data?add=github",
    ]) {
      const r = await call("POST", `${W}:start-install`, { returnPath });
      expect(r.status).toBe(400);
    }
  });
});

describe("a connection through the app", () => {
  it("is refused as the WFE refuses a binding, in its order", async () => {
    const create = (config: object) =>
      call("POST", `${F}/connectors`, { name: "Slack", config });
    expect(reason(await create(app(2)))).toBe("FOUNDRY_SLACK_APP_NOTICE");
    expect(reason(await create(app()))).toBe("FOUNDRY_SLACK_APP_NOT_INSTALLED");
    await install();
    knobs.slackScopesOld = true;
    expect(reason(await create(app()))).toBe("FOUNDRY_SLACK_APP_SCOPES");
    delete knobs.slackScopesOld;
    knobs.slackInstallerMember = true;
    expect(reason(await create(app()))).toBe("FOUNDRY_SLACK_APP_ADMIN");
    delete knobs.slackInstallerMember;
    const days = await create(app(1, 91));
    expect(days.body.message).toBe(
      "keep 1 to 90 days of history through Helix Foundry's Slack app",
    );
    expect((await create(app())).status).toBe(200);
    const twice = await create(app());
    expect(reason(twice)).toBe("FOUNDRY_SLACK_APP_IN_USE");
    expect(twice.body.message).toBe("Slack already reads this Slack workspace");
    expect(twice.body.details[0].metadata.connector_id).toMatch(/^conn_/);
    knobs.slackFeedOff = true;
    expect(reason(await create(app()))).toBe("FOUNDRY_SLACK_FEED_DISABLED");
    // Members bind nothing.
    const alan = await call(
      "POST",
      `${F}/connectors`,
      { name: "Slack", config: app() },
      "usr_alan",
    );
    expect(alan.status).toBe(403);
  });

  it("tests through the install, refused as a binding is but for the notice", async () => {
    const test = (config: object) =>
      call("POST", `${F}/connectors:test`, { config });
    expect(reason(await test(app()))).toBe("FOUNDRY_SLACK_APP_NOT_INSTALLED");
    await install();
    const r = await test(app(0));
    expect(r.status).toBe(200);
    expect(r.body.config).toEqual({
      slack: { historyDays: 30, helixApp: { noticeVersion: 0, now: false } },
    });
  });

  it("is pushed: bound, read and backfilled, with no key, schedule or second connection", async () => {
    await install();
    const made = await call("POST", `${F}/connectors`, {
      name: "Slack",
      config: app(),
    });
    const c = made.body.connector;
    expect(c).toMatchObject({
      secretHint: "",
      schedule: null,
      config: {
        slack: { historyDays: 30, helixApp: { noticeVersion: 1, now: false } },
      },
      slackStatus: {
        teamName: "Acme Slack",
        state: "SLACK_FEED_STATE_LIVE",
        switching: false,
        lossyDays: 0,
        eventsLimitedAt: null,
        deletedAt: null,
        backfill: { channels: 4, channelsDone: 0, etaSeconds: "216000" },
      },
    });
    // Its first sync is queued, and reads the channels' days.
    expect(c.lastSync.status).toBe("CONNECTOR_SYNC_STATUS_QUEUED");
    later(5_000);
    const synced = await connector(c.id);
    expect(synced.lastSync.status).toBe("CONNECTOR_SYNC_STATUS_SUCCEEDED");
    expect(synced.slackStatus.backfill).toMatchObject({ channelsDone: 0 });
    expect((await slackKind()).slack.inUseByConnectorId).toBe(c.id);
    later(25_000);
    expect((await connector(c.id)).slackStatus.backfill).toMatchObject({
      channelsDone: 2,
      etaSeconds: String(Math.ceil(216000 / 2)),
    });
    knobs.slackLossyDays = 2;
    knobs.slackEventsLimited = true;
    later(30_000);
    const done = (await connector(c.id)).slackStatus;
    expect(done.backfill).toBeNull();
    expect(done.lossyDays).toBe(2);
    expect(done.eventsLimitedAt).not.toBeNull();
  });

  it("syncs a message Slack sends, into its channel's day", async () => {
    expect(slackFeedPost({ channel: "general", text: "Hi" }).status).toBe(400);
    await install();
    const c = (
      await call("POST", `${F}/connectors`, { name: "Slack", config: app() })
    ).body.connector;
    later(5_000);
    const rows = async () =>
      Number(
        (await connector(c.id)).tables.find((t: any) => t.name === "messages")
          .rowCount,
      );
    const before = await rows();
    expect(slackFeedPost({ channel: "nowhere", text: "Hi" }).status).toBe(400);
    expect(
      slackFeedPost({ channel: "general", text: "Lunch at noon" }).body,
    ).toEqual({ channel: "C0MOCKGEN" });
    later(5_000);
    expect(await rows()).toBe(before + 1);
    const days = await call(
      "GET",
      `${F}/documents?setId=${(await call("GET", `${F}/document-sets`)).body.sets.find((s: any) => s.connector?.connectorId === c.id).id}`,
    );
    const today = days.body.documents.find((d: any) =>
      d.name.startsWith("#general, 2026-11-21"),
    );
    expect(today).toBeDefined();
  });

  it("changes its days through the app with no credential, and refuses a busy one", async () => {
    await install();
    const c = (
      await call("POST", `${F}/connectors`, { name: "Slack", config: app() })
    ).body.connector;
    const patch = (config: object) =>
      call("PATCH", `${F}/connectors/${c.id}`, { config });
    expect((await patch({ slack: { historyDays: 45 } })).status).toBe(400);
    later(5_000);
    const r = await patch({ slack: { historyDays: 45 } });
    expect(r.status).toBe(200);
    expect(r.body.connector.config.slack.historyDays).toBe(45);
    expect(r.body.connector.lastSync.status).toBe(
      "CONNECTOR_SYNC_STATUS_QUEUED",
    );
    later(5_000);
    expect((await patch({ slack: { historyDays: 120 } })).body.message).toMatch(
      /1 to 90 days/,
    );
    // A stale notice is refused even where nothing binds again.
    expect(reason(await patch(app(3)))).toBe("FOUNDRY_SLACK_APP_NOTICE");
  });

  it("goes back to a token, tested", async () => {
    await install();
    const c = (
      await call("POST", `${F}/connectors`, { name: "Slack", config: app() })
    ).body.connector;
    later(5_000);
    const bad = await call("PATCH", `${F}/connectors/${c.id}`, {
      config: { slack: { botToken: "xoxb-1-bad" } },
    });
    expect(bad.status).toBe(400);
    const r = await call("PATCH", `${F}/connectors/${c.id}`, {
      config: { slack: { botToken: TOKEN, historyDays: 200 } },
    });
    expect(r.body.connector).toMatchObject({
      secretHint: "xoxb…WXYZ",
      slackStatus: null,
      config: { slack: { botToken: "", historyDays: 200 } },
    });
    expect((await slackKind()).slack.inUseByConnectorId).toBe("");
  });

  it("says its install is gone, and when its data goes, and its syncs fail", async () => {
    await install();
    const c = (
      await call("POST", `${F}/connectors`, { name: "Slack", config: app() })
    ).body.connector;
    later(5_000);
    expect((await call("POST", `${W}:disconnect`, {})).status).toBe(200);
    const gone = (await connector(c.id)).slackStatus;
    expect(gone.state).toBe("SLACK_FEED_STATE_INSTALL_GONE");
    expect(gone.teamName).toBe("Acme Slack");
    expect(Date.parse(gone.deletedAt)).toBe(Date.now() + 7 * 86_400_000);
    const sync = await call("POST", `${F}/connectors/${c.id}:sync`, {});
    expect(sync.status).toBe(200);
    later(5_000);
    expect((await connector(c.id)).lastSync.error).toMatch(
      /^Helix Foundry was removed from Acme Slack: add it again in Settings › Slack/,
    );
    // Added again, the same Slack workspace reads again.
    await install();
    expect((await connector(c.id)).slackStatus.state).toBe(
      "SLACK_FEED_STATE_LIVE",
    );
    knobs.slackScopesOld = true;
    expect((await connector(c.id)).slackStatus).toMatchObject({
      state: "SLACK_FEED_STATE_NEEDS_REINSTALL",
      missingScopes: ["channels:history", "groups:history"],
    });
  });
});

describe("a token's switch to the app", () => {
  async function tokenConnection() {
    await install();
    const c = (
      await call("POST", `${F}/connectors`, {
        name: "Team Slack",
        config: { slack: { botToken: TOKEN, historyDays: 200 } },
      })
    ).body.connector;
    expect(c.slackStatus).toBeNull();
    later(5_000);
    return c;
  }

  it("is staged: it syncs by its token until the backfill ends, then reads through the app", async () => {
    const c = await tokenConnection();
    const patch = (config: object) =>
      call("PATCH", `${F}/connectors/${c.id}`, { config });
    // Kept without the notice, as the WFE keeps a credential unset.
    expect((await patch({ slack: { historyDays: 30 } })).status).toBe(200);
    expect(reason(await patch(app(0)))).toBe("FOUNDRY_SLACK_APP_NOTICE");
    // Its full read of the new days is live: the switch waits for it.
    expect((await patch(app(1, 60))).status).toBe(400);
    later(5_000);
    const r = await patch(app(1, 60));
    expect(r.status).toBe(200);
    expect(r.body.connector).toMatchObject({
      secretHint: "xoxb…WXYZ",
      config: { slack: { historyDays: 60, helixApp: { noticeVersion: 1 } } },
      slackStatus: { switching: true, state: "SLACK_FEED_STATE_LIVE" },
    });
    // Still a token's: a schedule, and no sync queued by the switch.
    expect(r.body.connector.schedule).not.toBeNull();
    // Its days, while staged, need no notice.
    expect((await patch({ slack: { historyDays: 50 } })).status).toBe(200);
    later(60_000);
    const flipped = await connector(c.id);
    expect(flipped).toMatchObject({
      secretHint: "",
      schedule: null,
      config: { slack: { historyDays: 50, helixApp: { noticeVersion: 1 } } },
      slackStatus: { switching: false, backfill: null },
    });
    expect(flipped.lastSync.status).toBe("CONNECTOR_SYNC_STATUS_QUEUED");
  });

  it("flips at once with now, and refuses a second connection of the Slack workspace", async () => {
    const c = await tokenConnection();
    expect(
      (await call("POST", `${F}/connectors`, { name: "S", config: app() }))
        .status,
    ).toBe(200);
    const inUse = await call("PATCH", `${F}/connectors/${c.id}`, {
      config: app(),
    });
    expect(reason(inUse)).toBe("FOUNDRY_SLACK_APP_IN_USE");
    const other = (await call("GET", `${F}/connectors`)).body.connectors.find(
      (x: any) => x.name === "S",
    );
    later(5_000);
    expect((await call("DELETE", `${F}/connectors/${other.id}`)).status).toBe(
      200,
    );
    await call("PATCH", `${F}/connectors/${c.id}`, { config: app() });
    const now = await call("PATCH", `${F}/connectors/${c.id}`, {
      config: {
        slack: { historyDays: 30, helixApp: { noticeVersion: 1, now: true } },
      },
    });
    expect(now.body.connector).toMatchObject({
      secretHint: "",
      slackStatus: { switching: false },
    });
    expect(now.body.connector.lastSync.status).toBe(
      "CONNECTOR_SYNC_STATUS_QUEUED",
    );
  });
});
