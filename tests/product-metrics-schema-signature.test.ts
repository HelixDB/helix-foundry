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
it("distinguishes column tuples and ignores input ordering", () => {
  expect(schemaSignature([d([{ name: "a b", type: "C" }])])).not.toBe(
    schemaSignature([d([{ name: "a", type: "b C" }])]),
  );
  const a = d([
    { name: "a", type: "INTEGER" },
    { name: "b", type: "VARCHAR" },
  ]);
  const b = d([...a.data.profile.columns].reverse());
  expect(schemaSignature([a])).toBe(schemaSignature([b]));
});
