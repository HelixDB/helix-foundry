import { expect, it } from "vitest";
import { randomUUID } from "node:crypto";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
import { compactStoredSamples } from "../apps/api/src/dataset-samples.js";
import { SAMPLE_VALUE_LIMIT } from "../packages/shared/src/index.js";

const document = "x".repeat(20_000);
const profile = (value: unknown) => ({
  rows: 1,
  duplicateRows: 0,
  fingerprint: "v2:fixed",
  columns: [
    {
      name: "document",
      type: "VARCHAR",
      nulls: 0,
      distinct: 1,
      examples: [value],
      sensitive: false,
      primaryKey: false,
    },
  ],
  sample: [{ id: 1, document: value }],
});

it("compacts sample values stored by earlier releases, once per workspace", async () => {
  const store = new MemoryStore(),
    scope = "samples-" + randomUUID();
  await transaction(store, scope, async (tx) => {
    tx.put(
      resource(
        scope,
        "dataset",
        "Documents",
        { activeVersion: "v1", profile: profile(document) },
        "documents",
      ),
    );
    tx.put(
      resource(
        scope,
        "version",
        "Documents",
        { datasetId: "documents", profile: profile(document) },
        "v1",
      ),
    );
    tx.put(
      resource(
        scope,
        "dataset",
        "Small",
        { activeVersion: "v2", profile: profile("short") },
        "small",
      ),
    );
  });
  const small = await store.get(scope, "small");

  await compactStoredSamples(store, scope);
  const cut = "x".repeat(SAMPLE_VALUE_LIMIT) + "…";
  for (const id of ["documents", "v1"]) {
    const r = (await store.get(scope, id))!;
    expect(r.data.profile.sample).toEqual([{ id: 1, document: cut }]);
    expect(r.data.profile.columns[0].examples).toEqual([cut]);
    expect(r.data.profile.fingerprint).toBe("v2:fixed");
    expect(r.revision).toBe(2);
  }
  expect((await store.get(scope, "documents"))?.data.activeVersion).toBe("v1");
  // A profile with nothing to compact is not rewritten.
  expect(await store.get(scope, "small")).toEqual(small);
  expect(
    (await store.get(scope, "migration_state"))?.data.storedSamples,
  ).toBeTruthy();

  // Once done, it does nothing.
  const version = await store.version(scope);
  await compactStoredSamples(store, scope);
  expect(await store.version(scope)).toBe(version);
});
