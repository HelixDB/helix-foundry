import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/web/api";
import { trackBeforeLeaving, trackEvent } from "@/web/analytics";
import {
  RELAY_ONLINE_MS,
  authorizeRelayLink,
  completeRelayLink,
  relayCancelUrl,
  relayContact,
  relayLinkRequest,
  relayLinksQuery,
  relayNameOk,
  relayQuery,
  relayView,
  revokeRelayLink,
  setSlackAccess,
  setSlackApprovals,
  startRelayLink,
  unlinkRelay,
  type RelayLinkRequest,
} from "@/web/areas/workspace/relay-api";

// Helix's Slack relay's calls and words (src/web/areas/workspace/relay-api.ts):
// what /relay/link accepts of a local install's request, where it sends the
// browser, the local install's link as its Settings › Slack reads it, each
// call's route and body, and the analytics each reports.

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

const refusal = (reason: string) =>
  new ApiError("refused", 400, 9, [
    { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
  ]);
const STATE = "s".repeat(16),
  CHALLENGE = "c".repeat(43),
  REDIRECT = "http://localhost:3000/settings/slack";
const kept = (over: Partial<{ redirectUri: string; name: string }> = {}) => ({
  kind: "relay_link" as const,
  state: STATE,
  codeChallenge: CHALLENGE,
  redirectUri: REDIRECT,
  name: "Ada’s laptop",
  at: 0,
  ...over,
});
const request: RelayLinkRequest = relayLinkRequest(kept())!;

describe("an install's name", () => {
  it("is text Helix keeps: not blank, at most 128 bytes, no control or bidi character", () => {
    expect(relayNameOk("Ada’s laptop")).toBe(true);
    expect(relayNameOk("é".repeat(64))).toBe(true);
    for (const name of [
      "",
      "   ",
      "é".repeat(65),
      "line\nbreak",
      "tab\there",
      "evil‮koobcam",
      "⁦isolate",
    ])
      expect(relayNameOk(name), JSON.stringify(name)).toBe(false);
  });
});

describe("a link request", () => {
  it("names the install's origin on this computer", () => {
    expect(request).toEqual({
      state: STATE,
      codeChallenge: CHALLENGE,
      redirectUri: REDIRECT,
      origin: "http://localhost:3000",
      name: "Ada’s laptop",
    });
  });
  it("is refused when it would send anything off this computer, or names nothing Helix keeps", () => {
    expect(
      relayLinkRequest(
        kept({ redirectUri: "https://evil.example/settings/slack" }),
      ),
    ).toBeNull();
    expect(relayLinkRequest(kept({ name: "a‮b" }))).toBeNull();
  });
  it("cancels back to the install, with its state", () => {
    expect(relayCancelUrl(request)).toBe(
      `${REDIRECT}?error=access_denied&state=${STATE}`,
    );
  });
});

describe("authorizing a link", () => {
  it("sends the request as the install made it, and the browser back to it", async () => {
    const back = `${REDIRECT}?code=k1&state=${STATE}`;
    call.mockResolvedValueOnce({ redirectUrl: back });
    await expect(authorizeRelayLink("ws 1", request)).resolves.toBe(back);
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws%201/foundry/relay-links:authorize",
      {
        state: STATE,
        codeChallenge: CHALLENGE,
        redirectUri: REDIRECT,
        name: "Ada’s laptop",
      },
    );
    expect(beacon).toHaveBeenCalledWith("relay_link_authorized");
  });
  it("never sends the browser anywhere but the install, with a code for its state", async () => {
    for (const redirectUrl of [
      undefined,
      "https://evil.example/?code=k1",
      `${REDIRECT}?state=${STATE}`,
      `${REDIRECT}?code=k1&state=other`,
      `${REDIRECT}x?code=k1&state=${STATE}`,
    ]) {
      call.mockResolvedValueOnce({ redirectUrl });
      await expect(
        authorizeRelayLink("ws", request),
        redirectUrl,
      ).rejects.toThrow(
        "Helix’s answer didn’t go back to that Foundry. Try again.",
      );
      expect(tracked).toHaveBeenLastCalledWith("relay_link_refused", {
        reason: "invalid",
      });
    }
    expect(beacon).not.toHaveBeenCalled();
  });
  it("reports why the WFE refused it", async () => {
    for (const [reason, word] of [
      ["FOUNDRY_SLACK_NOT_OFFERED", "not_offered"],
      ["FOUNDRY_RELAY_DISABLED", "unavailable"],
      ["FOUNDRY_SLACK_DISABLED", "unavailable"],
      ["SOMETHING_ELSE", "error"],
    ] as const) {
      const e = refusal(reason);
      call.mockRejectedValueOnce(e);
      await expect(authorizeRelayLink("ws", request)).rejects.toBe(e);
      expect(tracked).toHaveBeenLastCalledWith("relay_link_refused", {
        reason: word,
      });
    }
  });
});

describe("the workspace's linked installs", () => {
  it("are listed, and nothing where the relay is off", async () => {
    const q = relayLinksQuery("ws");
    expect(q.queryKey).toEqual(["ws", "foundry-slack", "relay-links"]);
    call.mockResolvedValueOnce({ links: [{ id: "rl_1" }] });
    await expect(q.queryFn()).resolves.toEqual([{ id: "rl_1" }]);
    expect(call).toHaveBeenCalledWith("/v1/workspaces/ws/foundry/relay-links");
    call.mockResolvedValueOnce({});
    await expect(q.queryFn()).resolves.toEqual([]);
    for (const reason of ["FOUNDRY_RELAY_DISABLED", "FOUNDRY_SLACK_DISABLED"]) {
      call.mockRejectedValueOnce(refusal(reason));
      await expect(q.queryFn()).resolves.toBeNull();
    }
    const e = new ApiError("denied", 403, 7);
    call.mockRejectedValueOnce(e);
    await expect(q.queryFn()).rejects.toBe(e);
  });
  it("are revoked by id", async () => {
    call.mockResolvedValueOnce(undefined);
    await revokeRelayLink("ws", "rl/1");
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/relay-links/rl%2F1:revoke",
      {},
    );
    expect(tracked).toHaveBeenCalledWith("relay_link_revoked");
  });
});

describe("the install's link to Helix", () => {
  it("reads linked only when the WFE says so", () => {
    expect(relayView(undefined)).toEqual({ kind: "unlinked" });
    expect(relayView({ linked: false, accountEmail: "x" })).toEqual({
      kind: "unlinked",
    });
    expect(relayView({ linked: true })).toEqual({
      kind: "linked",
      hostedOrigin: "",
      accountEmail: "",
      linkedAt: null,
      lastOkAt: null,
      approvals: false,
    });
    expect(
      relayView({
        linked: true,
        hostedOrigin: "https://ingest.helix-db.com",
        accountEmail: "ada@example.com",
        linkedAt: "2026-11-20T00:00:00Z",
        lastOkAt: "2026-11-21T00:00:00Z",
        slackApprovals: true,
      }),
    ).toEqual({
      kind: "linked",
      hostedOrigin: "https://ingest.helix-db.com",
      accountEmail: "ada@example.com",
      linkedAt: "2026-11-20T00:00:00Z",
      lastOkAt: "2026-11-21T00:00:00Z",
      approvals: true,
    });
  });
  it("is in contact while Helix answered within RELAY_ONLINE_MS", () => {
    const at = "2026-11-21T00:00:00.000Z",
      t = Date.parse(at);
    expect(relayContact(null, t)).toEqual({ kind: "never" });
    expect(relayContact("not a time", t)).toEqual({ kind: "never" });
    expect(relayContact(at, t + RELAY_ONLINE_MS)).toEqual({ kind: "online" });
    // A clock apart: a time from the future is in contact.
    expect(relayContact(at, t - 5_000)).toEqual({ kind: "online" });
    expect(relayContact(at, t + RELAY_ONLINE_MS + 1)).toEqual({
      kind: "offline",
      since: at,
    });
  });
  it("is read, and started with Helix's page, an http(s) address only", async () => {
    const q = relayQuery("ws");
    expect(q.queryKey).toEqual(["ws", "foundry-slack", "relay"]);
    call.mockResolvedValueOnce({ relay: { linked: false } });
    await expect(q.queryFn()).resolves.toEqual({ kind: "unlinked" });
    expect(call).toHaveBeenCalledWith("/v1/workspaces/ws/foundry/relay");

    call.mockResolvedValueOnce({
      authorizeUrl: "https://foundry.helix-db.com/relay/link?state=s",
    });
    await expect(startRelayLink("ws")).resolves.toBe(
      "https://foundry.helix-db.com/relay/link?state=s",
    );
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/relay:start-link",
      {},
    );
    expect(tracked).toHaveBeenCalledWith("relay_link_started");
    for (const authorizeUrl of [
      undefined,
      "not a url",
      "javascript:alert(1)",
    ]) {
      call.mockResolvedValueOnce({ authorizeUrl });
      await expect(startRelayLink("ws"), authorizeUrl).rejects.toThrow(
        "Helix’s page didn’t come back. Try again.",
      );
    }
  });
  it("is completed with what Helix handed back, saying why not", async () => {
    call.mockResolvedValueOnce({
      relay: { linked: true, accountEmail: "ada@example.com" },
    });
    await expect(completeRelayLink("ws", "k1", STATE)).resolves.toMatchObject({
      kind: "linked",
      accountEmail: "ada@example.com",
    });
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/relay:complete-link",
      { code: "k1", state: STATE },
    );
    expect(tracked).toHaveBeenCalledWith("relay_linked");
    for (const [e, reason] of [
      [refusal("FOUNDRY_RELAY_LINK_EXPIRED"), "expired"],
      [new ApiError("down", 503, 14), "error"],
    ] as const) {
      call.mockRejectedValueOnce(e);
      await expect(completeRelayLink("ws", "k1", STATE)).rejects.toBe(e);
      expect(tracked).toHaveBeenLastCalledWith("relay_link_failed", { reason });
    }
  });
  it("turns approving from Slack on and off, and unlinks", async () => {
    call.mockResolvedValueOnce({
      relay: { linked: true, slackApprovals: false },
    });
    await expect(setSlackApprovals("ws", false)).resolves.toMatchObject({
      approvals: false,
    });
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/relay",
      { slackApprovals: false },
      "PATCH",
    );
    expect(tracked).toHaveBeenCalledWith("slack_approvals_changed", {
      approvals: "off",
    });
    call.mockResolvedValueOnce({
      relay: { linked: true, slackApprovals: true },
    });
    await setSlackApprovals("ws", true);
    expect(tracked).toHaveBeenLastCalledWith("slack_approvals_changed", {
      approvals: "on",
    });

    call.mockResolvedValueOnce({ relay: { linked: false } });
    await expect(unlinkRelay("ws")).resolves.toEqual({ kind: "unlinked" });
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/relay:unlink",
      {},
    );
    expect(tracked).toHaveBeenLastCalledWith("relay_unlinked");
  });
});

describe("who may use a local install in Slack", () => {
  it("is allowed read-only, or removed", async () => {
    call.mockResolvedValueOnce({
      link: { slackUserId: "U0BEA", access: "FOUNDRY_SLACK_LINK_ACCESS_READ" },
    });
    await expect(setSlackAccess("ws", "U0BEA", "read")).resolves.toEqual({
      slackUserId: "U0BEA",
      access: "FOUNDRY_SLACK_LINK_ACCESS_READ",
    });
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws/foundry/slack/links/U0BEA/access",
      { access: "FOUNDRY_SLACK_LINK_ACCESS_READ" },
      "PUT",
    );
    expect(tracked).toHaveBeenCalledWith("slack_access_changed", {
      access: "read",
    });
    call.mockResolvedValueOnce({});
    await expect(setSlackAccess("ws", "U0BEA", "none")).resolves.toBeNull();
    expect(call).toHaveBeenLastCalledWith(
      "/v1/workspaces/ws/foundry/slack/links/U0BEA/access",
      { access: "FOUNDRY_SLACK_LINK_ACCESS_NONE" },
      "PUT",
    );
    expect(tracked).toHaveBeenLastCalledWith("slack_access_changed", {
      access: "none",
    });
  });
});
