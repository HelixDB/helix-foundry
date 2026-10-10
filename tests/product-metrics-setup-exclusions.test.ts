import { it, expect, vi, afterEach } from "vitest";
import {
  schemaSignature,
  metricsDatasets,
  compileMetric,
  normalizeSpec,
} from "../apps/api/src/metrics.js";
import { MemoryStore, resource, transaction } from "../apps/api/src/store.js";
const d = (columns: any[]) =>
  resource(
    "w",
    "dataset",
    "Orders",
    { activeVersion: "v", profile: { columns, rows: 10 } },
    "d",
  );
const spec: any = {
  title: "Orders",
  why: "",
  kind: "kpi",
  datasetId: "d",
  measure: "count",
  format: "number",
  scale: "none",
};
afterEach(() => vi.unstubAllGlobals());
it("omits metrics for data excluded through its import or source", async () => {
  const s = new MemoryStore();
  await transaction(s, "w", async (tx) => {
    tx.put(
      resource(
        "w",
        "workspace",
        "w",
        { onboarding: { excludedIds: ["source", "job"] } },
        "workspace",
      ),
    );
    for (const [id, link] of [
      ["one", { sourceId: "source" }],
      ["two", { importJobId: "job" }],
      ["kept", {}],
    ] as const)
      tx.put(
        resource(
          "w",
          "dataset",
          id,
          { ...d([{ name: "id", type: "INTEGER" }]).data, ...link },
          id,
        ),
      );
  });
  expect((await metricsDatasets(s, "w")).map((x) => x.id)).toEqual(["kept"]);
});
