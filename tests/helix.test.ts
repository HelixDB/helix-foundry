import { it, expect } from "vitest";
import { randomUUID } from "node:crypto";
import {
  HelixStore,
  MemoryStore,
  transaction,
  resource,
} from "../apps/api/src/store.js";
it.skipIf(process.env.TEST_HELIX !== "1")(
  "persists atomic scoped changes and graph relationships in real HelixDB",
  async () => {
    const store = new HelixStore();
    await store.init();
    const scope = "test-" + randomUUID();
    await transaction(store, scope, async (tx) => {
      tx.put(resource(scope, "object", "Alice", {}, "a"));
      tx.put(resource(scope, "object", "Bob", {}, "b"));
    });
    await transaction(store, scope, async (tx) => {
      tx.put(
        resource(
          scope,
          "relation",
          "Knows",
          { from: "a", to: "b", generation: "g" },
          "rel",
        ),
      );
    });
    expect(await store.neighbors(scope, "a")).toEqual(["b"]);
    expect(await store.get("other", "a")).toBeUndefined();
    const v = await store.version(scope);
    expect(
      await store.commit(scope, v - 1, [
        { put: resource(scope, "test", "stale", {}, "stale") },
      ]),
    ).toBe(false);
    expect(await store.get(scope, "stale")).toBeUndefined();
    await transaction(store, scope, async (tx) => tx.delete("rel"));
    expect(await store.neighbors(scope, "a")).toEqual([]);
    const reopened = new HelixStore();
    expect((await reopened.get(scope, "a"))?.name).toBe("Alice");
    await transaction(store, scope, async (tx) => {
      for (const r of await tx.list()) tx.delete(r.id);
    });
  },
);
it("returns each in-memory neighbor once, as HelixDB does", async () => {
  const store = new MemoryStore();
  await transaction(store, "test", async (tx) => {
    for (const [i, from, to] of [
      [1, "a", "b"],
      [2, "a", "b"],
      [3, "b", "a"],
    ] as const)
      tx.put(resource("test", "relation", "link", { from, to }, String(i)));
  });
  expect(await store.neighbors("test", "a")).toEqual(["b"]);
});
