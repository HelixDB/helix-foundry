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
it("rejects fixed-size numeric and temporal arrays while accepting scalars", () => {
  const data = d([
    { name: "amount", type: "INTEGER[3]" },
    { name: "created", type: "TIMESTAMP[2]" },
  ]);
  expect(
    compileMetric({ ...spec, measure: "sum", column: "amount" }, data),
  ).toHaveProperty("error");
  expect(
    compileMetric({ ...spec, kind: "trend", timeColumn: "created" }, data),
  ).toHaveProperty("error");
  expect(
    compileMetric(
      { ...spec, measure: "sum", column: "amount" },
      d([{ name: "amount", type: "INTEGER" }]),
    ),
  ).toHaveProperty("sql");
});
