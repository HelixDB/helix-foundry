import { it, expect, vi, afterEach } from "vitest";
import { FoundryClient, FoundryError } from "../packages/sdk/src/index.js";
const client = () =>
  new FoundryClient({
    baseUrl: "https://foundry.invalid",
    workspaceId: "w",
    token: "fixture",
  });
afterEach(() => vi.unstubAllGlobals());
it("keeps status for gateway errors and accepts 204", async () => {
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        new Response("upstream failed", {
          status: 502,
          statusText: "Bad Gateway",
        }),
      )
      .mockResolvedValueOnce(new Response(null, { status: 204 })),
  );
  await expect(client().reject("p")).rejects.toMatchObject({
    status: 502,
    message: "Bad Gateway",
  });
  await expect(client().reject("p")).resolves.toBeUndefined();
});
it("handles empty upload responses", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue(new Response(null, { status: 204 })),
  );
  await expect(
    client().uploadForSetup(new Blob(["x"]), "x.csv", "op"),
  ).resolves.toBeUndefined();
});
