import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/web/api";
import { agentQueries } from "@/web/areas/agent/queries";

// The threads and runtime queries' contract: the WFE's reply, or "off" where
// it has Founder off. The refusal is data so it holds through a refetch,
// which clears a query's error while it runs; any other error stays one.

vi.mock("@/web/api", async (actual) => ({
  ...(await actual<typeof import("@/web/api")>()),
  api: vi.fn(),
}));
const call = vi.mocked(api);
const refusal = (reason: string) =>
  new ApiError("refused", 412, 9, [
    { "@type": "type.googleapis.com/google.rpc.ErrorInfo", reason },
  ]);
const queries = {
  threads: () => agentQueries.threads("ws").queryFn(),
  runtime: () => agentQueries.runtime("ws").queryFn(),
};

beforeEach(() => call.mockReset());

describe("agentQueries.threads", () => {
  it("reads the caller's threads", async () => {
    const threads = { threads: [{ id: "thr_1", title: "Hi" }] };
    call.mockResolvedValueOnce(threads);
    await expect(queries.threads()).resolves.toBe(threads);
    expect(call.mock.calls).toEqual([
      ["/v1/workspaces/ws/foundry/agent/threads"],
    ]);
  });
});

describe("agentQueries.runtime", () => {
  it("reads where new threads run, else nothing set", async () => {
    const runtime = { runtime: "claude_code" as const, online: [] };
    call.mockResolvedValueOnce({ runtime }).mockResolvedValueOnce({});
    await expect(queries.runtime()).resolves.toBe(runtime);
    await expect(queries.runtime()).resolves.toEqual({});
    expect(call.mock.calls[0]).toEqual([
      "/v1/workspaces/ws/foundry/agent/runtime",
    ]);
  });
});

describe.each(["threads", "runtime"] as const)("agentQueries.%s", (name) => {
  it('is "off" where the WFE has Founder off', async () => {
    call.mockRejectedValueOnce(refusal("FOUNDRY_AGENT_DISABLED"));
    await expect(queries[name]()).resolves.toBe("off");
  });

  it.each([
    ["another refusal", refusal("FOUNDRY_AGENT_QUESTIONS_CLOSED")],
    ["a failed read", new ApiError("unavailable", 503)],
    ["a network error", new TypeError("Failed to fetch")],
  ])("throws %s", async (_, error) => {
    call.mockRejectedValueOnce(error);
    await expect(queries[name]()).rejects.toBe(error);
  });
});
