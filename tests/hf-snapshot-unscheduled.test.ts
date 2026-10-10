import { it, expect } from "vitest";
import { scheduledRefresh } from "../apps/api/src/source-sync.js";
it("does not refresh snapshots with no scheduled timestamp", () => {
  for (const nextRun of [null, undefined, "", NaN])
    expect(
      scheduledRefresh({
        data: { sync: { mode: "snapshot" }, nextRun },
      } as any),
    ).toBe(false);
  expect(
    scheduledRefresh({
      data: { sync: { mode: "snapshot" }, nextRun: 0 },
    } as any),
  ).toBe(true);
});
