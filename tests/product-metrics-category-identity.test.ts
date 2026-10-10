import { it, expect, vi, afterEach } from "vitest";
import { compileMetric, normalizeSpec } from "../apps/api/src/metrics.js";
import { resource } from "../apps/api/src/store.js";
import { DuckDBInstance } from "@duckdb/node-api";
import { datasetTable } from "../packages/shared/src/index.js";
afterEach(() => vi.unstubAllGlobals());
it("counts case-distinct categories separately and prefers exact observed values", async () => {
  const dataset = resource(
    "w",
    "dataset",
    "Orders",
    {
      profile: {
        columns: [{ name: "status", type: "VARCHAR", distinct: 2 }],
        sample: [{ status: "paid" }, { status: "PAID" }],
      },
    },
    "d",
  );
  const spec: any = {
    title: "Paid",
    why: "",
    kind: "kpi",
    datasetId: "d",
    measure: "count",
    format: "number",
    scale: "none",
    filter: { column: "status", value: "PAID" },
  };
  expect(normalizeSpec(spec, dataset)?.filter?.value).toBe("PAID");
  const db = await DuckDBInstance.create(":memory:", {
    autoinstall_known_extensions: "false",
    autoload_known_extensions: "false",
  });
  const c = await db.connect();
  try {
    await c.run(`CREATE TABLE "${datasetTable("d")}" (status VARCHAR)`);
    await c.run(`INSERT INTO "${datasetTable("d")}" VALUES ('paid'),('PAID')`);
    const query = compileMetric(spec, dataset);
    if ("error" in query) throw new Error(query.error);
    const r = await c.runAndReadAll(query.sql);
    expect(r.getRowObjects()[0].value).toBe(1);
  } finally {
    c.closeSync();
    db.closeSync();
  }
});
