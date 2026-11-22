import { describe, expect, it } from "vitest";
import type { ConnectorSlackStatus, SlackConnectorSetup } from "@/shared/wfe";
import {
  SLACK_APP_HISTORY_DAYS,
  connectorBadge,
  emptyForm,
  formFromConfig,
  hasCredential,
  readsThroughApp,
  slackAppStep,
  slackBadge,
  slackEta,
  slackNotice,
  slackStatusLine,
  toConfig,
  type ConnectionForm,
} from "@/web/areas/data/model";
import { slackReturnTo } from "@/web/areas/workspace/slack-api";

// A Slack connection through Helix's Slack app (areas/data/model.ts): the
// form's credential choice and what it sends, the app side's steps, the
// storage notice, the connection's status line, ETA and badge, and where
// an install returns.

const slackForm = (patch: Partial<ConnectionForm>) =>
  ({ ...emptyForm("slack"), ...patch }) as ConnectionForm;
const LIVE = "SLACK_FEED_STATE_LIVE";

describe("the Slack form's credential", () => {
  it("does as its examples say", () => {
    expect(readsThroughApp({ slackStatus: { state: LIVE } })).toBe(true);
    expect(readsThroughApp({ slackStatus: { switching: true } })).toBe(false);
    expect(readsThroughApp({})).toBe(false);
    expect(slackNotice(1)?.startsWith("Helix keeps a copy")).toBe(true);
    expect(slackNotice(2)).toBeNull();
    expect(slackNotice(undefined)).toBeNull();
    expect(slackNotice(0)).toBeNull();
  });

  it("starts on a token, and reads a stored connection's credential from its config", () => {
    expect(emptyForm("slack")).toMatchObject({
      credential: "token",
      noticeVersion: 0,
    });
    expect(
      formFromConfig("slack", {
        slack: { historyDays: 45, helixApp: { noticeVersion: 1 } },
      }),
    ).toEqual(slackForm({ credential: "app", historyDays: "45" }));
    expect(
      formFromConfig("slack", { slack: { botToken: "", historyDays: 200 } }),
    ).toEqual(slackForm({ historyDays: "200" }));
  });

  it("sends the app with the notice accepted, else no credential, which keeps the current one", () => {
    expect(
      toConfig(
        slackForm({ credential: "app", historyDays: "30", noticeVersion: 1 }),
      ),
    ).toEqual({
      slack: { historyDays: 30, helixApp: { noticeVersion: 1 } },
    });
    expect(
      toConfig(slackForm({ credential: "app", historyDays: "60" })),
    ).toEqual({ slack: { historyDays: 60 } });
    // A token form sends its token, blank to keep the stored one.
    expect(
      toConfig(slackForm({ botToken: " xoxb-1 ", noticeVersion: 1 })),
    ).toEqual({ slack: { botToken: "xoxb-1", historyDays: 90 } });
  });

  it("holds a credential through the app, and with a token only once one is typed", () => {
    expect(hasCredential(slackForm({ credential: "app" }))).toBe(true);
    expect(hasCredential(slackForm({ botToken: " " }))).toBe(false);
    expect(hasCredential(slackForm({ botToken: "xoxb-1" }))).toBe(true);
    expect(SLACK_APP_HISTORY_DAYS).toBe(90);
  });
});

describe("the app side's steps", () => {
  const install = { teamId: "T1", teamName: "Acme" };
  const setup = (patch: SlackConnectorSetup): SlackConnectorSetup => ({
    install,
    installerAdmin: true,
    canInstall: true,
    noticeVersion: 1,
    ...patch,
  });

  it("does as its examples say", () => {
    expect(slackAppStep({ canInstall: true })).toEqual({
      kind: "install",
      canInstall: true,
    });
    expect(
      slackAppStep({ install: { teamName: "Acme" }, installerAdmin: true }),
    ).toEqual({ kind: "ready", team: "Acme" });
  });

  it("adds the app first, which only an owner or admin offered it may", () => {
    expect(slackAppStep({})).toEqual({ kind: "install", canInstall: false });
    expect(slackAppStep({ install: null, canInstall: true })).toEqual({
      kind: "install",
      canInstall: true,
    });
  });

  it("adds it again for missing permissions before a non-admin's install", () => {
    expect(
      slackAppStep(
        setup({
          install: { ...install, missingScopes: ["channels:history"] },
          installerAdmin: false,
        }),
      ),
    ).toEqual({ kind: "reinstall", why: "scopes", canInstall: true });
    expect(
      slackAppStep(setup({ installerAdmin: false, canInstall: false })),
    ).toEqual({ kind: "reinstall", why: "admin", canInstall: false });
  });

  it("names the connection that reads the Slack workspace, unless it is the one edited", () => {
    const used = setup({ inUseByConnectorId: "conn_1" });
    expect(slackAppStep(used)).toEqual({
      kind: "in_use",
      connectorId: "conn_1",
    });
    expect(slackAppStep(used, "conn_2")).toEqual({
      kind: "in_use",
      connectorId: "conn_1",
    });
    expect(slackAppStep(used, "conn_1")).toEqual({
      kind: "ready",
      team: "Acme",
    });
  });

  it("names the Slack workspace by its name, else its id, else Slack", () => {
    expect(slackAppStep(setup({ install: { teamId: "T9" } }))).toEqual({
      kind: "ready",
      team: "T9",
    });
    expect(slackAppStep(setup({ install: {} }))).toEqual({
      kind: "ready",
      team: "Slack",
    });
  });
});

describe("a backfill's ETA", () => {
  it("does as its examples say", () => {
    expect(slackEta("90")).toBe("about 2 minutes");
    expect(slackEta("216000")).toBe("about 3 days");
    expect(slackEta("0")).toBeNull();
  });

  it("rounds up, in minutes under two hours, hours under two days, else days", () => {
    expect(slackEta("1")).toBe("about 1 minute");
    expect(slackEta("60")).toBe("about 1 minute");
    expect(slackEta(String(119 * 60))).toBe("about 119 minutes");
    expect(slackEta(String(120 * 60))).toBe("about 2 hours");
    expect(slackEta(String(120 * 60 + 1))).toBe("about 3 hours");
    expect(slackEta(String(47 * 3600 + 1))).toBe("about 48 hours");
    expect(slackEta(String(48 * 3600))).toBe("about 2 days");
    expect(slackEta(String(48 * 3600 + 60))).toBe("about 3 days");
  });

  it("can't tell from nothing, a negative or what isn't a number", () => {
    for (const s of [undefined, "", "-5", "soon", "Infinity"])
      expect(slackEta(s)).toBeNull();
  });
});

describe("a Slack connection's status line", () => {
  const NOW = Date.parse("2026-11-21T12:00:00Z");
  const line = (s: ConnectorSlackStatus) => slackStatusLine(s, NOW, "UTC");

  it("does as its examples say", () => {
    expect(slackStatusLine({ state: LIVE })).toBe("Live");
    expect(
      slackStatusLine({
        state: LIVE,
        backfill: { channels: 4, channelsDone: 1, etaSeconds: "7200" },
      }),
    ).toBe("Reading history: 1 of 4 channels, about 2 hours left");
    expect(
      slackStatusLine({
        switching: true,
        backfill: { channels: 4, channelsDone: 3 },
      }),
    ).toBe("Switching from your token: 3 of 4 channels read");
  });

  it("says what keeps it from reading before anything else", () => {
    const backfill = { channels: 2, channelsDone: 1 };
    expect(line({ state: "SLACK_FEED_STATE_NEEDS_REINSTALL", backfill })).toBe(
      "Needs new Slack permissions",
    );
    expect(line({ state: "SLACK_FEED_STATE_INSTALL_GONE", backfill })).toBe(
      "Helix Foundry was removed from Slack",
    );
    expect(line({ state: "SLACK_FEED_STATE_TEAM_CHANGED" })).toBe(
      "Reads a Slack workspace no longer connected",
    );
  });

  it("says when Slack asked it to wait, until that has passed", () => {
    const waiting = (at: string) => ({
      state: LIVE as typeof LIVE,
      switching: true,
      backfill: { channels: 4, channelsDone: 0, waitingUntil: at },
    });
    expect(line(waiting("2026-11-21T14:05:00Z"))).toMatch(
      /^Waiting for Slack until \d/,
    );
    expect(line(waiting("2026-11-21T14:05:00Z"))).toContain("05");
    expect(line(waiting("2026-11-21T11:00:00Z"))).toBe(
      "Switching from your token: 0 of 4 channels read",
    );
  });

  it("says a switch, and a backfill with no ETA, in counts alone", () => {
    expect(line({ switching: true })).toBe("Switching from your token");
    expect(line({ state: LIVE, backfill: { channels: 1 } })).toBe(
      "Reading history: 0 of 1 channel",
    );
    expect(line({ state: LIVE, backfill: {} })).toBe(
      "Reading history: 0 of 0 channels",
    );
    expect(line({})).toBe("Live");
  });
});

describe("a Slack connection's badge", () => {
  const failed = { id: "s", status: "CONNECTOR_SYNC_STATUS_FAILED" as const };

  it("does as its example says", () => {
    expect(slackBadge({ state: LIVE }, null)).toEqual({
      status: "healthy",
      label: "Live",
    });
  });

  it("says why it can't read, over a failed sync", () => {
    expect(
      slackBadge({ state: "SLACK_FEED_STATE_NEEDS_REINSTALL" }, failed),
    ).toEqual({ status: "stale", label: "Needs new Slack permissions" });
    expect(
      slackBadge({ state: "SLACK_FEED_STATE_INSTALL_GONE" }, failed),
    ).toEqual({ status: "paused", label: "Removed from Slack" });
    expect(
      slackBadge({ state: "SLACK_FEED_STATE_TEAM_CHANGED" }, null),
    ).toEqual({ status: "stale", label: "Slack workspace changed" });
  });

  it("is a failed sync's, else reading its history, else live", () => {
    expect(slackBadge({ state: LIVE, backfill: {} }, failed)).toEqual({
      status: "failed",
      label: "Failed",
    });
    expect(slackBadge({ state: LIVE, backfill: {} }, undefined)).toEqual({
      status: "syncing",
      label: "Reading history",
    });
  });

  it("is the connection's badge through the app, but not while it switches", () => {
    expect(connectorBadge({ slackStatus: { state: LIVE } })).toEqual({
      status: "healthy",
      label: "Live",
    });
    expect(
      connectorBadge({ slackStatus: { state: LIVE, switching: true } }),
    ).toEqual({ status: "draft", label: "Not synced" });
    // An account to sign in again comes first (no Slack one has one).
    expect(
      connectorBadge({
        account: { state: "CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN" },
        slackStatus: { state: LIVE },
      }),
    ).toEqual({ status: "stale", label: "Needs signing in again" });
  });
});

describe("where an install returns", () => {
  it("does as its examples say", () => {
    expect(slackReturnTo("/data?add=slack")).toBe("/data?add=slack");
    expect(slackReturnTo("/data/connections/conn_1")).toBe(
      "/data/connections/conn_1",
    );
    expect(slackReturnTo("")).toBe("/settings/slack");
    expect(slackReturnTo("https://evil.example/")).toBeNull();
  });

  it("returns to Settings › Slack by default, and nowhere else the WFE wouldn't name", () => {
    expect(slackReturnTo(undefined)).toBe("/settings/slack");
    expect(slackReturnTo("/settings/slack")).toBe("/settings/slack");
    for (const p of [
      "/data/connections/",
      "/data/connections/a/b",
      "/data/connections/" + "a".repeat(65),
      "/data?add=github",
      "//evil.example/data/connections/x",
      "/data/connections/x?next=/y",
    ])
      expect(slackReturnTo(p)).toBeNull();
  });
});
