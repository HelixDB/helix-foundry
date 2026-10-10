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
it("returns the same dataset metadata that is committed", async () => {
  const store = new MemoryStore();
  executor.run.mockResolvedValue({
    profile,
    version: "one",
    rows: profile.sample,
  });
  const first = await ingestFile(
    store,
    "fixture",
    "Orders",
    "input.jsonl",
    undefined,
    "orders",
  );
  expect(first).toEqual(await store.get("fixture", "orders"));
  const second = await ingestFile(
    store,
    "fixture",
    "Orders",
    "input.jsonl",
    undefined,
    "orders",
  );
  expect(second.revision).toBe(first.revision + 1);
  expect(second).toEqual(await store.get("fixture", "orders"));
});
