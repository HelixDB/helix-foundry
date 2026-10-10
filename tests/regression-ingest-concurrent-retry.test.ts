import { it, expect, vi } from "vitest";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
const executor = vi.hoisted(() => ({ run: vi.fn() }));
vi.mock("../apps/api/src/executor.js", () => ({ execute: executor.run }));
import { ingestFile } from "../apps/api/src/datasets.js";
const profile = {
  rows: 1,
  duplicateRows: 0,
  columns: [
    {
      name: "id",
      type: "VARCHAR",
      nulls: 0,
      distinct: 1,
      examples: ["a"],
      primaryKey: true,
      sensitive: false,
    },
  ],
  sample: [{ id: "a" }],
  fingerprint: "one",
};
it("publishes a durable version only once when concurrent retries overlap", async () => {
  const store = new MemoryStore();
  let entered = 0;
  let release!: () => void;
  const barrier = new Promise<void>((r) => {
    release = r;
  });
  executor.run.mockImplementation(async () => {
    if (++entered === 2) release();
    await barrier;
    return { profile, version: "snapshot_import", rows: profile.sample };
  });
  await Promise.all(
    [1, 2].map(() =>
      ingestFile(
        store,
        "fixture",
        "Orders",
        "input.jsonl",
        undefined,
        "orders",
        "import",
        undefined,
        "import",
      ),
    ),
  );
  const ds = await store.get("fixture", "orders");
  expect(ds?.data.versions).toEqual(["snapshot_import"]);
  expect(ds?.revision).toBe(2);
  expect(await store.list("fixture", "version")).toHaveLength(1);
});
