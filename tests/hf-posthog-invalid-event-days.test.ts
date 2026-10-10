import { it, expect, vi, afterEach } from "vitest";
import { importPosthog } from "../apps/api/src/posthog.js";
afterEach(() => vi.unstubAllGlobals());
it("rejects invalid event windows instead of completing an empty snapshot", async () => {
  const fetch = vi.fn();
  vi.stubGlobal("fetch", fetch);
  for (const eventDays of ["invalid", NaN, 0, -1, 1.5, 366])
    await expect(
      importPosthog(
        { apiKey: "phx_test", projectId: 7, table: "events", eventDays },
        async () => {},
      ),
    ).rejects.toThrow("Event days");
  expect(fetch).not.toHaveBeenCalled();
});
