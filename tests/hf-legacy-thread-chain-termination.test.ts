import { it, expect } from "vitest";
import { threadOf } from "../apps/api/src/assistant.js";
import { resource } from "../apps/api/src/store.js";
it("resolves deep chains and gives every cycle member the same thread", () => {
  const rows = new Map();
  for (let i = 0; i < 15000; i++)
    rows.set(
      String(i),
      resource(
        "test",
        "run",
        "legacy",
        { context: { resourceId: String(i + 1) } },
        String(i),
      ),
    );
  expect(threadOf(rows.get("0"), rows)).toBe("14999");
  const a = resource("test", "run", "a", { context: { resourceId: "b" } }, "a"),
    b = resource("test", "run", "b", { context: { resourceId: "a" } }, "b");
  const cycle = new Map([
    ["a", a],
    ["b", b],
  ]);
  expect(threadOf(a, cycle)).toBe("a");
  expect(threadOf(b, cycle)).toBe("a");
});
