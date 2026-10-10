import { it, expect, vi, afterEach } from "vitest";
import { retryingFetch } from "../apps/api/src/retrying-fetch.js";
afterEach(() => vi.unstubAllGlobals());
it("preserves caller cancellation alongside the request timeout", async () => {
  const controller = new AbortController();
  controller.abort();
  vi.stubGlobal(
    "fetch",
    vi.fn(async (_u, init) => {
      if (init.signal.aborted) throw init.signal.reason;
      return new Response("ok");
    }),
  );
  await expect(
    retryingFetch(
      new URL("https://fixture.invalid"),
      {},
      { signal: controller.signal },
    ),
  ).rejects.toThrow();
});

vi.mock("node:timers/promises", async (importOriginal) => {
  const actual = await importOriginal<typeof import("node:timers/promises")>();
  return {
    ...actual,
    setTimeout: vi.fn((_delay: number, value: unknown, options: any) =>
      actual.setTimeout(0, value, options),
    ),
  };
});
import { setTimeout as sleep } from "node:timers/promises";
it("cancels a long Retry-After backoff without starting another request", async () => {
  const controller = new AbortController();
  const fetch = vi.fn(async () => {
    if (fetch.mock.calls.length === 1) {
      controller.abort();
      return new Response("", {
        status: 429,
        headers: { "retry-after": "30" },
      });
    }
    return new Response("ok");
  });
  vi.stubGlobal("fetch", fetch);
  await expect(
    retryingFetch(
      new URL("https://fixture.invalid"),
      {},
      { signal: controller.signal },
    ),
  ).rejects.toThrow();
  expect(fetch).toHaveBeenCalledOnce();
  expect(sleep).toHaveBeenCalledWith(30000, undefined, {
    signal: controller.signal,
  });
});
