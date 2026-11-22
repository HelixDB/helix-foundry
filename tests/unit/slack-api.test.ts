import { beforeEach, describe, expect, it, vi } from "vitest";
import type * as wfe from "@/shared/wfe";
import { api, ApiError } from "@/web/api";
import { trackBeforeLeaving, trackEvent } from "@/web/analytics";
import {
  SLACK_REASONS,
  createSlackLink,
  deleteSlackLink,
  disconnectSlack,
  finishSlackInstall,
  installFailed,
  installFailure,
  linkFailure,
  linkRefusalText,
  previewSlackLink,
  refusalOfLink,
  relayRedirect,
  slackLinksQuery,
  slackQuery,
  slackReason,
  slackReturnCopy,
  slackReturnFailure,
  slackMode,
  slackView,
  startSlackInstall,
  updateSlackMode,
} from "@/web/areas/workspace/slack-api";

// Settings › Slack's calls and words (src/web/areas/workspace/slack-api.ts):
// the page's state from the WFE's answer, each call's route and body, the
// refusals read from their ErrorInfo, and the analytics each reports.

vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
vi.mock("@/web/analytics", () => ({
  trackEvent: vi.fn(),
  trackBeforeLeaving: vi.fn(),
}));
const call = vi.mocked(api),
  tracked = vi.mocked(trackEvent),
  beacon = vi.mocked(trackBeforeLeaving);

beforeEach(() => {
  call.mockReset();
  tracked.mockClear();
  beacon.mockClear();
});

/** A WFE refusal with the ErrorInfo `reason` (and metadata). */
const refusal = (reason: string, metadata?: Record<string, string>) =>
  new ApiError("refused", 400, 9, [
    {
      "@type": "type.googleapis.com/google.rpc.ErrorInfo",
      reason,
      ...(metadata && { metadata }),
    },
  ]);
const install: wfe.FoundrySlackInstall = {
  teamId: "T0ACME",
  teamName: "Acme Slack",
  installedByName: "Ada Lovelace",
};

describe("the page's state", () => {
  it("is installed whenever there is an install, with the caller's link", () => {
    expect(
      slackView({
        installAllowed: false,
        canManage: true,
        install,
        myLink: { slackUserId: "U1" },
      }),
    ).toEqual({
      kind: "installed",
      canManage: true,
      install,
      mode: { kind: "per_person" },
      myLink: { slackUserId: "U1" },
    });
    expect(slackView({ install, myLink: null })).toEqual({
      kind: "installed",
      canManage: false,
      install,
      mode: { kind: "per_person" },
      myLink: null,
    });
  });
  it("reads the install's mode: BOT is the shared bot, anything else per person", () => {
    const bot = (b: wfe.FoundrySlackBot | null | undefined) =>
      slackMode({ identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT", bot: b });
    expect(
      bot({ credentialId: "sc_1", credentialName: "Slack bot (Acme)" }),
    ).toEqual({
      kind: "bot",
      credentialId: "sc_1",
      credentialName: "Slack bot (Acme)",
      broken: false,
    });
    expect(bot({ credentialId: "sc_1", broken: true })).toEqual({
      kind: "bot",
      credentialId: "sc_1",
      credentialName: "",
      broken: true,
    });
    // A bot the WFE didn't describe has no key to link to.
    expect(bot(null)).toEqual({
      kind: "bot",
      credentialId: null,
      credentialName: "",
      broken: false,
    });
    expect(bot({ credentialId: "" })).toMatchObject({ credentialId: null });
    for (const identityMode of [
      "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON",
      "FOUNDRY_SLACK_IDENTITY_MODE_UNSPECIFIED",
      undefined,
    ] as const)
      expect(
        slackMode({ identityMode, bot: { credentialId: "sc_stale" } }),
      ).toEqual({ kind: "per_person" });
    expect(
      slackView({
        install: {
          ...install,
          identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
          bot: { credentialId: "sc_1" },
        },
      }),
    ).toMatchObject({
      kind: "installed",
      mode: { kind: "bot", credentialId: "sc_1" },
    });
  });
  it("offers Add to Slack only on the install list", () => {
    expect(slackView({ installAllowed: true, canManage: true })).toEqual({
      kind: "not_installed",
      canManage: true,
    });
    expect(slackView({ installAllowed: false, canManage: true })).toEqual({
      kind: "not_offered",
      canManage: true,
    });
    expect(slackView(undefined)).toEqual({
      kind: "not_offered",
      canManage: false,
    });
  });
  it("reads GET .../foundry/slack, and foundry.slack off as unavailable", async () => {
    call.mockResolvedValueOnce({ slack: { installAllowed: true } });
    await expect(slackQuery("ws 1").queryFn()).resolves.toEqual({
      kind: "not_installed",
      canManage: false,
    });
    expect(call).toHaveBeenCalledWith("/v1/workspaces/ws%201/foundry/slack");
    expect(slackQuery("ws").queryKey).toEqual(["ws", "foundry-slack"]);
    call.mockRejectedValueOnce(refusal("FOUNDRY_SLACK_DISABLED"));
    await expect(slackQuery("ws").queryFn()).resolves.toEqual({
      kind: "unavailable",
    });
    const other = new ApiError("down", 503);
    call.mockRejectedValueOnce(other);
    await expect(slackQuery("ws").queryFn()).rejects.toBe(other);
  });
  it("lists who is linked, under the setting's key", async () => {
    const q = slackLinksQuery("ws");
    expect(q.queryKey).toEqual(["ws", "foundry-slack", "links"]);
    call.mockResolvedValueOnce({
      links: [{ slackUserId: "U1" }],
      requests: [{ slackUserId: "U2", slackName: "Bea" }],
    });
    await expect(q.queryFn()).resolves.toEqual({
      links: [{ slackUserId: "U1" }],
      requests: [{ slackUserId: "U2", slackName: "Bea" }],
    });
    expect(call).toHaveBeenCalledWith("/v1/workspaces/ws/foundry/slack/links");
    call.mockResolvedValueOnce({});
    await expect(q.queryFn()).resolves.toEqual({ links: [], requests: [] });
  });
});

describe("a local install's Settings › Slack", () => {
  it("is on this computer, by http, and nothing else", () => {
    expect(relayRedirect("http://localhost:3000/settings/slack")).toBe(
      "http://localhost:3000",
    );
    expect(relayRedirect("http://127.0.0.1:65535/settings/slack")).toBe(
      "http://127.0.0.1:65535",
    );
    expect(relayRedirect("http://localhost/settings/slack")).toBe(
      "http://localhost",
    );
    for (const uri of [
      undefined,
      "",
      "http://localhost:65536/settings/slack",
      "http://localhost:0/settings/slack",
      "http://localhost:03000/settings/slack",
      "http://localhost:3000/settings/slack/",
      "http://localhost:3000/settings/slack#x",
      "http://user@localhost:3000/settings/slack",
      "http://[::1]:3000/settings/slack",
      "http://localhost:3000/settings/ai",
      "https://localhost:3000/settings/slack",
    ])
      expect(relayRedirect(uri), uri).toBeNull();
  });
});

describe("refusals", () => {
  it("are read from the first Slack ErrorInfo", () => {
    for (const reason of SLACK_REASONS)
      expect(slackReason(refusal(reason))).toBe(reason);
    expect(
      slackReason(
        new ApiError("x", 400, 9, [
          null,
          "text",
          { reason: "FOUNDRY_PRO_REQUIRED" },
          { reason: "FOUNDRY_SLACK_TEAM_TAKEN" },
        ]),
      ),
    ).toBe("FOUNDRY_SLACK_TEAM_TAKEN");
    expect(slackReason(new ApiError("x", 400))).toBeUndefined();
    expect(slackReason(new Error("offline"))).toBeUndefined();
  });
  it("name why an install failed", () => {
    const words: [string, string][] = [
      ["FOUNDRY_SLACK_INSTALL_EXPIRED", "expired"],
      ["FOUNDRY_SLACK_REFUSED", "refused"],
      ["FOUNDRY_SLACK_ENTERPRISE_INSTALL", "enterprise"],
      ["FOUNDRY_SLACK_SCOPES", "scopes"],
      ["FOUNDRY_SLACK_TEAM_TAKEN", "team_taken"],
      ["FOUNDRY_SLACK_DISCONNECTING", "disconnecting"],
      ["FOUNDRY_SLACK_INSTALLED", "installed"],
      ["FOUNDRY_SLACK_NOT_OFFERED", "not_offered"],
      ["FOUNDRY_AGENT_DISABLED", "agent_disabled"],
      ["FOUNDRY_SLACK_DISABLED", "unavailable"],
      ["FOUNDRY_SLACK_LINK_EXPIRED", "error"],
    ];
    for (const [reason, word] of words)
      expect(installFailure(refusal(reason)), reason).toBe(word);
    expect(installFailure(new TypeError("Failed to fetch"))).toBe("error");
    expect(slackReturnFailure("access_denied")).toBe("cancelled");
    expect(slackReturnFailure("invalid_scope")).toBe("refused");
  });
  it("say Slack's return in fixed words, never echoing its error", () => {
    expect(slackReturnCopy("access_denied")).toEqual({
      title: "Helix Foundry wasn’t added",
      text: "Adding it to Slack was cancelled. Add it again from Settings › Slack whenever you’re ready.",
    });
    const forged = "Your workspace is suspended; call 555-0100";
    for (const error of ["invalid_scope", forged]) {
      const copy = slackReturnCopy(error);
      expect(copy.title).toBe("Slack didn’t add Helix Foundry");
      expect(copy.text).toBe(
        "Slack didn’t add Helix Foundry. Add it again from Settings › Slack.",
      );
    }
  });
  it("name why a link failed, from the refusal in its metadata", () => {
    expect(linkFailure(refusal("FOUNDRY_SLACK_LINK_EXPIRED"))).toBe("expired");
    expect(
      linkFailure(
        refusal("FOUNDRY_SLACK_LINK_REFUSED", {
          refusal: "FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER",
        }),
      ),
    ).toBe("not_a_member");
    expect(
      linkFailure(
        refusal("FOUNDRY_SLACK_LINK_REFUSED", { refusal: "SOMETHING_NEW" }),
      ),
    ).toBe("error");
    expect(linkFailure(refusal("FOUNDRY_SLACK_LINK_REFUSED"))).toBe("error");
    expect(
      linkFailure(
        new ApiError("x", 400, 9, [
          { reason: "FOUNDRY_SLACK_LINK_REFUSED", metadata: "odd" },
        ]),
      ),
    ).toBe("error");
    expect(linkFailure(refusal("FOUNDRY_SLACK_DISABLED"))).toBe("error");
    expect(linkFailure(new Error("offline"))).toBe("error");
  });
  it("words a preview's refusal, and none when the account may be linked", () => {
    expect(refusalOfLink("FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH")).toBe(
      "email_mismatch",
    );
    expect(refusalOfLink("FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED")).toBe(
      "email_unconfirmed",
    );
    expect(refusalOfLink("FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED")).toBe(
      undefined,
    );
    expect(refusalOfLink(undefined)).toBe(undefined);
    expect(refusalOfLink("toString")).toBe(undefined);
    expect(linkRefusalText({ matchesYou: true })).toBeNull();
    expect(
      linkRefusalText({
        matchesYou: false,
        refusal: "FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER",
      }),
    ).toMatch(/isn’t a full member/);
    expect(
      linkRefusalText({
        matchesYou: false,
        refusal: "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED",
      }),
    ).toMatch(/hasn’t confirmed/);
    const mismatch =
      "This Slack account’s email isn’t your Foundry email. Sign in to Foundry as the person the Slack account is.";
    expect(
      linkRefusalText({
        matchesYou: false,
        refusal: "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH",
      }),
    ).toBe(mismatch);
    // An answer that says no without saying why reads as the mismatch.
    expect(linkRefusalText({})).toBe(mismatch);
  });
});

describe("adding the app", () => {
  it("starts at Slack's page, reporting before the page leaves", async () => {
    call.mockResolvedValueOnce({
      authorizeUrl: "https://slack.com/oauth/v2/authorize?state=s",
    });
    await expect(startSlackInstall("ws")).resolves.toBe(
      "https://slack.com/oauth/v2/authorize?state=s",
    );
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/slack:start-install",
      {},
    );
    expect(beacon).toHaveBeenCalledWith("slack_install_started");
    // Added (again) from the Data area, it comes back there.
    call.mockResolvedValueOnce({ authorizeUrl: "https://slack.com/a" });
    await startSlackInstall("ws", "/data/connections/c1");
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/slack:start-install",
      { returnPath: "/data/connections/c1" },
    );
  });
  it("never sends the browser to anything but an http(s) page", async () => {
    for (const authorizeUrl of [
      undefined,
      "not a url",
      "javascript:alert(1)",
    ]) {
      call.mockResolvedValueOnce({ authorizeUrl });
      await expect(
        startSlackInstall("ws"),
        String(authorizeUrl),
      ).rejects.toThrow("Slack’s page didn’t come back. Try again.");
    }
    expect(beacon).not.toHaveBeenCalled();
  });
  it("finishes with what Slack returned, once accepted", async () => {
    call.mockResolvedValueOnce({ slack: { workspaceId: "ws", install } });
    await expect(finishSlackInstall("c", "s")).resolves.toEqual({
      slack: { workspaceId: "ws", install },
      returnPath: "/settings/slack",
      returnTo: null,
    });
    expect(call).toHaveBeenCalledWith("/v1/foundry/slack:finish-install", {
      code: "c",
      state: "s",
    });
    expect(tracked).toHaveBeenCalledWith("slack_installed");
    call.mockResolvedValueOnce({});
    await expect(finishSlackInstall("c", "s")).resolves.toEqual({
      slack: {},
      returnPath: "/settings/slack",
      returnTo: null,
    });
  });
  it("sends a local install's back to its own Settings › Slack, and nowhere else", async () => {
    for (const [returnTo, kept] of [
      ["http://localhost:3000/settings/slack", true],
      ["http://127.0.0.1/settings/slack", true],
      ["https://localhost:3000/settings/slack", false],
      ["http://localhost:3000/settings/slack?x=1", false],
      ["http://localhost.evil.example/settings/slack", false],
      ["", false],
    ] as const) {
      call.mockResolvedValueOnce({ slack: {}, returnTo });
      expect((await finishSlackInstall("c", "s")).returnTo, returnTo).toBe(
        kept ? returnTo : null,
      );
    }
  });
  it("returns where the install was started from, and only where the WFE would send it", async () => {
    call.mockResolvedValueOnce({ slack: {}, returnPath: "/data?add=slack" });
    expect((await finishSlackInstall("c", "s")).returnPath).toBe(
      "/data?add=slack",
    );
    call.mockResolvedValueOnce({
      slack: {},
      returnPath: "https://evil.example/",
    });
    expect((await finishSlackInstall("c", "s")).returnPath).toBe(
      "/settings/slack",
    );
  });
  it("reports why a finish was refused", async () => {
    const e = refusal("FOUNDRY_SLACK_TEAM_TAKEN");
    call.mockRejectedValueOnce(e);
    await expect(finishSlackInstall("c", "s")).rejects.toBe(e);
    expect(tracked).toHaveBeenCalledWith("slack_install_failed", {
      reason: "team_taken",
    });
    expect(tracked).not.toHaveBeenCalledWith("slack_installed");
    installFailed("cancelled");
    expect(tracked).toHaveBeenLastCalledWith("slack_install_failed", {
      reason: "cancelled",
    });
  });
  it("disconnects, reporting it once accepted", async () => {
    call.mockResolvedValueOnce({ slack: { installAllowed: true } });
    await expect(disconnectSlack("ws")).resolves.toEqual({
      kind: "not_installed",
      canManage: false,
    });
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/slack:disconnect",
      {},
    );
    expect(tracked).toHaveBeenCalledWith("slack_disconnected");
    tracked.mockClear();
    call.mockRejectedValueOnce(new ApiError("nope", 403));
    await expect(disconnectSlack("ws")).rejects.toThrow("nope");
    expect(tracked).not.toHaveBeenCalled();
  });
});

describe("the mode", () => {
  it("PATCHes the setting, reporting the mode once accepted", async () => {
    const answer = {
      slack: {
        canManage: true,
        install: {
          ...install,
          identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT",
          bot: { credentialId: "sc_1" },
        },
      },
    } satisfies wfe.FoundrySlackResponse;
    call.mockResolvedValueOnce(answer);
    await expect(updateSlackMode("ws 1", "bot")).resolves.toMatchObject({
      kind: "installed",
      mode: { kind: "bot", credentialId: "sc_1" },
    });
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws%201/foundry/slack",
      { identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_BOT" },
      "PATCH",
    );
    expect(tracked).toHaveBeenCalledWith("slack_mode_changed", { mode: "bot" });
    tracked.mockClear();
    call.mockResolvedValueOnce({ slack: { install } });
    await expect(updateSlackMode("ws", "per_person")).resolves.toMatchObject({
      mode: { kind: "per_person" },
    });
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/slack",
      { identityMode: "FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON" },
      "PATCH",
    );
    expect(tracked).toHaveBeenCalledWith("slack_mode_changed", {
      mode: "per_person",
    });
    tracked.mockClear();
    const refused = new ApiError("hosted only", 400, 9, [
      { reason: "FOUNDRY_SLACK_BOT_HOSTED_ONLY" },
    ]);
    call.mockRejectedValueOnce(refused);
    await expect(updateSlackMode("ws", "bot")).rejects.toBe(refused);
    expect(slackReason(refused)).toBe("FOUNDRY_SLACK_BOT_HOSTED_ONLY");
    expect(tracked).not.toHaveBeenCalled();
  });
});

describe("linking", () => {
  const preview: wfe.PreviewFoundrySlackLinkResponse = {
    teamName: "Acme Slack",
    slackUserId: "U0ADA",
    slackHandle: "ada",
    slackEmailMasked: "a•••@example.com",
    matchesYou: true,
    refusal: "FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED",
  };
  it("previews without reporting a link that may be made", async () => {
    call.mockResolvedValueOnce(preview);
    await expect(previewSlackLink("ws", "hx_sl_t")).resolves.toEqual(preview);
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/slack/links:preview",
      { token: "hx_sl_t" },
    );
    expect(tracked).not.toHaveBeenCalled();
  });
  it("reports a preview's refusal, and an expired link", async () => {
    call.mockResolvedValueOnce({
      ...preview,
      matchesYou: false,
      refusal: "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH",
    });
    await previewSlackLink("ws", "hx_sl_t");
    expect(tracked).toHaveBeenLastCalledWith("slack_link_refused", {
      reason: "email_mismatch",
    });
    call.mockResolvedValueOnce({ matchesYou: false });
    await previewSlackLink("ws", "hx_sl_t");
    expect(tracked).toHaveBeenLastCalledWith("slack_link_refused", {
      reason: "error",
    });
    call.mockRejectedValueOnce(refusal("FOUNDRY_SLACK_LINK_EXPIRED"));
    await expect(previewSlackLink("ws", "hx_sl_t")).rejects.toThrow();
    expect(tracked).toHaveBeenLastCalledWith("slack_link_refused", {
      reason: "expired",
    });
    tracked.mockClear();
    // Any other failure isn't a refusal of the link.
    call.mockRejectedValueOnce(new ApiError("down", 503));
    await expect(previewSlackLink("ws", "hx_sl_t")).rejects.toThrow("down");
    expect(tracked).not.toHaveBeenCalled();
  });
  it("links, reporting it once accepted", async () => {
    call.mockResolvedValueOnce({ link: { slackUserId: "U0ADA" } });
    await expect(createSlackLink("ws", "hx_sl_t")).resolves.toEqual({
      slackUserId: "U0ADA",
    });
    expect(call).toHaveBeenCalledWith("/v1/workspaces/ws/foundry/slack/links", {
      token: "hx_sl_t",
    });
    expect(tracked).toHaveBeenCalledWith("slack_linked");
    call.mockResolvedValueOnce({});
    await expect(createSlackLink("ws", "hx_sl_t")).resolves.toEqual({});
  });
  it("reports why a link was refused", async () => {
    call.mockRejectedValueOnce(
      refusal("FOUNDRY_SLACK_LINK_REFUSED", {
        refusal: "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED",
      }),
    );
    await expect(createSlackLink("ws", "hx_sl_t")).rejects.toThrow();
    expect(tracked).toHaveBeenCalledWith("slack_link_refused", {
      reason: "email_unconfirmed",
    });
    expect(tracked).not.toHaveBeenCalledWith("slack_linked");
  });
  it("unlinks a Slack user by id", async () => {
    call.mockResolvedValueOnce(undefined);
    await deleteSlackLink("ws", "U/1");
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/slack/links/U%2F1",
      undefined,
      "DELETE",
    );
  });
});
