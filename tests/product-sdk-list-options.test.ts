import { it, expect, vi, afterEach } from "vitest";
import { FoundryClient, FoundryError } from "../packages/sdk/src/index.js";
const client = () =>
  new FoundryClient({
    baseUrl: "https://foundry.invalid",
    workspaceId: "w",
    token: "fixture",
  });
afterEach(() => vi.unstubAllGlobals());
it("omits unset options while preserving zero and empty search", async () => {
  const f = vi.fn().mockResolvedValue(new Response("{}"));
  vi.stubGlobal("fetch", f);
  await client().list("dataset", { offset: 0, limit: undefined, search: "" });
  const u = new URL(f.mock.calls[0][0]);
  expect(u.searchParams.has("limit")).toBe(false);
  expect(u.searchParams.get("offset")).toBe("0");
  expect(u.searchParams.get("search")).toBe("");
});
