import { it, expect } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
it("returns one neighbor for parallel and reverse relationships", async () => {
  const s = new MemoryStore();
  await transaction(s, "test", async (tx) => {
    for (const [i, from, to] of [
      [1, "a", "b"],
      [2, "a", "b"],
      [3, "b", "a"],
    ] as const)
      tx.put(resource("test", "relation", "link", { from, to }, String(i)));
  });
  expect(await s.neighbors("test", "a")).toEqual(["b"]);
});
