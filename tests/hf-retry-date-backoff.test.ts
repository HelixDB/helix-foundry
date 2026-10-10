import { it, expect, vi, afterEach } from "vitest";
import { retryingFetch } from "../apps/api/src/retrying-fetch.js";
vi.mock("node:timers/promises", () => ({ setTimeout: vi.fn(async () => {}) }));
import { setTimeout as sleep } from "node:timers/promises";
afterEach(() => {
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});
it("honors an HTTP-date Retry-After rather than retrying after 250ms", async () => {
  vi.spyOn(Date, "now").mockReturnValue(Date.UTC(2026, 0, 1));
  vi.stubGlobal(
    "fetch",
    vi
      .fn()
      .mockResolvedValueOnce(
        new Response("", {
          status: 429,
          headers: { "retry-after": "Thu, 01 Jan 2026 00:00:05 GMT" },
        }),
      )
      .mockResolvedValueOnce(new Response("ok")),
  );
  await retryingFetch(new URL("https://fixture.invalid"), {});
  expect(sleep).toHaveBeenCalledWith(5000);
  vi.restoreAllMocks();
});
