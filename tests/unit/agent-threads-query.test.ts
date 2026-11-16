import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/web/api";
import { agentQueries } from "@/web/areas/agent/queries";

// The threads query's contract: the WFE's threads, or "off" where it has
// Founder off. The refusal is data so it holds through a refetch, which
// clears a query's error while it runs; any other error stays an error.

vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
const call = vi.mocked(api);
const refusal = (reason: string) =>
  new ApiError("refused", 412, 9, [
    { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
  ]);

describe("agentQueries.threads", () => {
  beforeEach(() => call.mockReset());

  it("reads the caller's threads", async () => {
    const threads = { threads: [{ id: "thr_1", title: "Hi" }] };
    call.mockResolvedValueOnce(threads);
    await expect(agentQueries.threads("ws").queryFn()).resolves.toBe(threads);
    expect(call.mock.calls).toEqual([
      ["/v1/workspaces/ws/foundry/agent/threads"],
    ]);
  });

  it('is "off" where the WFE has Founder off', async () => {
    call.mockRejectedValueOnce(refusal("FOUNDRY_AGENT_DISABLED"));
    await expect(agentQueries.threads("ws").queryFn()).resolves.toBe("off");
  });

  it.each([
    ["another refusal", refusal("FOUNDRY_AGENT_QUESTIONS_CLOSED")],
    ["a failed read", new ApiError("unavailable", 503)],
    ["a network error", new TypeError("Failed to fetch")],
  ])("throws %s", async (_, error) => {
    call.mockRejectedValueOnce(error);
    await expect(agentQueries.threads("ws").queryFn()).rejects.toBe(error);
  });
});
