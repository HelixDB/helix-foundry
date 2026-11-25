import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/web/api";
import { trackEvent } from "@/web/analytics";
import {
  approve,
  authorizationQuery,
  installsQuery,
  revokeInstall,
} from "@/web/areas/workspace/partner-apps-api";

// Connected apps' calls (src/web/areas/workspace/partner-apps-api.ts): each
// call's route and body, the request's state and challenge kept out of
// every URL, and the refusal approve reports.

vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
vi.mock("@/web/analytics", () => ({ trackEvent: vi.fn() }));
const call = vi.mocked(api),
  tracked = vi.mocked(trackEvent);

beforeEach(() => {
  call.mockReset();
  tracked.mockClear();
});

const request = {
  clientId: "hlxpc_" + "n".repeat(43),
  redirectUri: "https://northwind.example/helix/callback?x=1",
  scope: "foundry.read foundry.ask",
};
const body = {
  ...request,
  state: "secret-state",
  codeChallenge: "c".repeat(43),
  scopes: ["foundry.read"],
  targets: [{ kind: "KIND_CONNECTOR" as const, connectorId: "c1" }],
};

describe("the app's request", () => {
  it("is read without its state, and never again", async () => {
    call.mockResolvedValue({ app: { name: "Northwind" } });
    const q = authorizationQuery(request);
    expect(q.retry).toBe(false);
    expect(q.staleTime).toBe(Infinity);
    expect(q.queryKey).toEqual([
      "partner-apps",
      "authorization",
      request.clientId,
      request.redirectUri,
      request.scope,
    ]);
    expect(await q.queryFn()).toEqual({ app: { name: "Northwind" } });
    const path = call.mock.calls[0]![0];
    expect(path.startsWith("/v1/partner-apps/authorization?")).toBe(true);
    const sent = new URLSearchParams(path.split("?")[1]);
    expect(Object.fromEntries(sent)).toEqual(request);
    expect(path).not.toContain("state");
  });
});

describe("approving", () => {
  it("posts the body to the workspace's route and answers where to go", async () => {
    call.mockResolvedValue({
      redirectUrl: "https://n.example/cb?code=hlxpa_x",
    });
    expect(await approve("ws 1", body)).toBe(
      "https://n.example/cb?code=hlxpa_x",
    );
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws%201/partner-apps/authorization:approve",
      body,
    );
    expect(tracked).not.toHaveBeenCalled();
    call.mockResolvedValue({});
    expect(await approve("ws", body)).toBe("");
  });
  it("reports a refusal in a word, then throws it", async () => {
    for (const [error, reason] of [
      [new ApiError("deleted", 404), "refused"],
      [new ApiError("slow", 429), "rate_limit"],
      [new ApiError("down", 503), "error"],
      [new TypeError("Failed to fetch"), "error"],
    ] as const) {
      call.mockRejectedValueOnce(error);
      await expect(approve("ws", body)).rejects.toBe(error);
      expect(tracked).toHaveBeenLastCalledWith("partner_app_approve_refused", {
        reason,
      });
    }
  });
});

describe("installs", () => {
  it("are listed per workspace, none when the WFE leaves them out", async () => {
    const q = installsQuery("ws_acme");
    expect(q.queryKey).toEqual(["ws_acme", "partner-apps", "installs"]);
    call.mockResolvedValue({ installs: [{ id: "pai_1" }] });
    expect(await q.queryFn()).toEqual([{ id: "pai_1" }]);
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws_acme/partner-apps/installs",
    );
    call.mockResolvedValue({});
    expect(await q.queryFn()).toEqual([]);
  });
  it("are revoked by id", async () => {
    call.mockResolvedValue({});
    await revokeInstall("ws_acme", "pai/1");
    expect(call).toHaveBeenCalledWith(
      "/v1/workspaces/ws_acme/partner-apps/installs/pai%2F1",
      undefined,
      "DELETE",
    );
  });
});
