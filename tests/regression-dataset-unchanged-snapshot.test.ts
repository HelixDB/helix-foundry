import { it, expect, vi } from "vitest";
import { MemoryStore } from "../apps/api/src/store.js";
const executor = vi.hoisted(() => ({ run: vi.fn() }));
vi.mock("../apps/api/src/executor.js", () => ({ execute: executor.run }));
import { ingestFile } from "../apps/api/src/datasets.js";

// A scheduled refresh of a table that did not change used to mint a new
// version and a new Parquet file every time. The version list grew without
// bound, the setup signature moved, pending reviews went stale within one
// refresh cycle, and a publication that overlapped a refresh was aborted
// with "Inputs changed during publication". The content fingerprint already
// tells the two cases apart.
const profile = (fingerprint: string) => ({
  rows: 2,
  duplicateRows: 0,
  columns: [
    {
      name: "id",
      type: "VARCHAR",
      nulls: 0,
      distinct: 2,
      examples: ["a", "b"],
      primaryKey: true,
      sensitive: false,
    },
  ],
  sample: [{ id: "a" }, { id: "b" }],
  fingerprint,
});

async function ingest(store: MemoryStore, version: string, fp: string) {
  executor.run.mockResolvedValueOnce({
    profile: profile(fp),
    version,
    rows: [{ id: "a" }],
  });
  return ingestFile(
    store,
    "fixture",
    "Orders",
    "input.jsonl",
    "source_orders",
    "orders",
    version,
  );
}

it("keeps the active version when a refresh fingerprints the same", async () => {
  const store = new MemoryStore();
  const first = await ingest(store, "v1", "same");
  expect(first.data.activeVersion).toBe("v1");
  expect(first.data.versions).toEqual(["v1"]);

  const second = await ingest(store, "v2", "same");
  expect(second.data.activeVersion).toBe("v1");
  expect(second.data.versions).toEqual(["v1"]);
  expect(second.revision).toBe(first.revision + 1);
  expect(second.data.lastSync).toBeDefined();
  expect(await store.get("fixture", "v2")).toBeUndefined();
  expect(
    (await store.list("fixture", "finding")).filter(
      (f) => f.data.kind === "refresh",
    ),
  ).toHaveLength(0);

  const third = await ingest(store, "v3", "different");
  expect(third.data.activeVersion).toBe("v3");
  expect(third.data.versions).toEqual(["v3", "v1"]);
  expect(await store.get("fixture", "v3")).toBeDefined();
  expect(
    (await store.list("fixture", "finding")).filter(
      (f) => f.data.kind === "refresh",
    ),
  ).toHaveLength(1);
});

it("still stages a changed schema for review", async () => {
  const store = new MemoryStore();
  await ingest(store, "v1", "same");
  executor.run.mockResolvedValueOnce({
    profile: {
      ...profile("same"),
      columns: [
        ...profile("same").columns,
        {
          name: "total",
          type: "BIGINT",
          nulls: 0,
          distinct: 2,
          examples: [1, 2],
          primaryKey: false,
          sensitive: false,
        },
      ],
    },
    version: "v2",
    rows: [],
  });
  const drifted = await ingestFile(
    store,
    "fixture",
    "Orders",
    "input.jsonl",
    "source_orders",
    "orders",
    "v2",
  );
  expect(drifted.data.activeVersion).toBe("v1");
  expect(drifted.data.pendingVersion).toBe("v2");
  expect(drifted.data.versions).toEqual(["v2", "v1"]);
});
