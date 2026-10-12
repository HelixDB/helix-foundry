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
it.skipIf(process.env.TEST_HELIX !== "1")(
  "stages, reads within bounds and removes a generation in real HelixDB",
  async () => {
    const store = new HelixStore();
    await store.init();
    const scope = "test-" + randomUUID(),
      generation = randomUUID(),
      other = randomUUID();
    const object = (g: string, i: number) =>
      resource(
        scope,
        "object",
        "Record " + i,
        { generation: g, logicalId: String(i).padStart(32, "0"), key: i },
        `${g}_${String(i).padStart(32, "0")}`,
      );
    const objects = Array.from({ length: 30 }, (_, i) => object(generation, i));
    const handles = await store.stageObjects(scope, objects, false);
    expect(handles.size).toBe(30);
    // Resuming skips what is there and returns the same handles.
    const again = await store.stageObjects(scope, objects.slice(0, 5), true);
    expect(again.get(objects[0].id)).toBe(handles.get(objects[0].id));
    // A star: record 0 links to every other record.
    const relations = objects.slice(1).map((o, i) => ({
      relation: resource(
        scope,
        "relation",
        "Links",
        { from: objects[0].id, to: o.id, generation },
        `${generation}_r${String(i).padStart(32, "0")}`,
      ),
      from: handles.get(objects[0].id)!,
      to: handles.get(o.id)!,
    }));
    expect(await store.stageRelations(scope, relations, false)).toBe(29);
    expect(await store.stageRelations(scope, relations, true)).toBe(0);
    const hood = await store.neighborhood(scope, [objects[0].id], 1, 10);
    expect(hood.nodes).toHaveLength(10);
    expect(hood.truncated).toBe(true);
    expect(hood.edges.length).toBeGreaterThanOrEqual(9);
    expect(
      hood.edges.every((e) => e.from === objects[0].id && e.name === "Links"),
    ).toBe(true);
    const counts = await store.linkCounts(scope, [
      objects[0].id,
      objects[1].id,
    ]);
    expect([...counts.values()]).toEqual([29, 1]);
    expect(
      await store.relationsBetween(
        scope,
        objects[0].id,
        objects[2].id,
        "Links",
      ),
    ).toEqual([relations[1].relation.id]);
    const page = await store.page(scope, generation + "_r", 10, 5);
    expect(page.total).toBe(29);
    expect(page.items.map((r: any) => r.id)).toEqual(
      relations.slice(10, 15).map((r) => r.relation.id),
    );
    expect(
      (await store.getMany(scope, [objects[3].id, "missing"])).map(
        (r) => r?.id,
      ),
    ).toEqual([objects[3].id, undefined]);
    // A second generation, then the first is removed in bounded batches.
    await store.stageObjects(scope, [object(other, 1)], false);
    expect((await store.generations(scope)).sort()).toEqual(
      [generation, other].sort(),
    );
    let removed = 0,
      n: number;
    while ((n = await store.dropGeneration(scope, generation, 7))) {
      expect(n).toBeLessThanOrEqual(7);
      removed += n;
    }
    expect(removed).toBe(59);
    expect(await store.generations(scope)).toEqual([other]);
    expect(await store.get(scope, objects[0].id)).toBeUndefined();
    while (await store.dropGeneration(scope, other, 100));
  },
);
