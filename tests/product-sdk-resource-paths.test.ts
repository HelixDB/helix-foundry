import { it, expect, vi, afterEach } from "vitest";
import { FoundryClient, FoundryError } from "../packages/sdk/src/index.js";
const client = () =>
  new FoundryClient({
    baseUrl: "https://foundry.invalid",
    workspaceId: "w",
    token: "fixture",
  });
afterEach(() => vi.unstubAllGlobals());
it("keeps identifiers in one path segment", async () => {
  const f = vi.fn().mockImplementation(async () => new Response("{}"));
  vi.stubGlobal("fetch", f);
  const c = client();
  await c.sync("a/b?x#y");
  await c.cancel("a/b?x#y");
  await c.previewDataset("a/b?x#y");
  for (const call of f.mock.calls) expect(call[0]).toContain("a%2Fb%3Fx%23y");
});
