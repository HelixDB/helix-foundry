import { it, expect } from "vitest";
import { compilePipeline } from "../packages/shared/src/pipeline.js";
import { PipelineNodeSchema } from "../packages/shared/src/index.js";
import { DuckDBInstance } from "@duckdb/node-api";
it("executes visual selection, renaming, casting, filtering, derivation, deduplication, joins and aggregation", async () => {
  const nodes = [
    { id: "source", type: "source", datasetId: "orders", inputs: [] },
    {
      id: "select",
      type: "select",
      inputs: ["source"],
      config: { columns: ["id", "customer", "amount"] },
    },
    {
      id: "rename",
      type: "rename",
      inputs: ["select"],
      config: { mapping: { amount: "gross" } },
    },
    {
      id: "cast",
      type: "cast",
      inputs: ["rename"],
      config: { columns: { gross: "DOUBLE" } },
    },
    {
      id: "filter",
      type: "filter",
      inputs: ["cast"],
      config: { condition: "gross > 0" },
    },
    {
      id: "derive",
      type: "derive",
      inputs: ["filter"],
      config: { columns: { net: "gross * 0.9" } },
    },
    {
      id: "dedup",
      type: "deduplicate",
      inputs: ["derive"],
      config: { keys: ["id"] },
    },
    { id: "customers", type: "source", datasetId: "customers", inputs: [] },
    {
      id: "join",
      type: "join",
      inputs: ["dedup", "customers"],
      config: { on: "a.customer = b.customer", select: "a.*, b.region" },
    },
    {
      id: "sum",
      type: "aggregate",
      inputs: ["join"],
      config: { groupBy: ["region"], metrics: { revenue: "sum(net)" } },
    },
  ].map((n) => PipelineNodeSchema.parse(n));
  const p = compilePipeline(nodes),
    db = await DuckDBInstance.create(":memory:"),
    c = await db.connect();
  try {
    await c.run(
      "CREATE TABLE d_orders AS SELECT * FROM (VALUES (1,'A',10),(1,'A',10),(2,'A',20),(3,'B',-10)) t(id,customer,amount)",
    );
    await c.run(
      "CREATE TABLE d_customers AS SELECT * FROM (VALUES ('A','EU'),('B','NA')) t(customer,region)",
    );
    expect((await c.runAndReadAll(p.sql)).getRowObjectsJson()).toEqual([
      { region: "EU", revenue: 27 },
    ]);
  } finally {
    c.closeSync();
    db.closeSync();
  }
});
it("rejects cyclic, disconnected and dangling graphs", () => {
  for (const nodes of [
    [{ id: "a", type: "filter", inputs: ["a"], config: { condition: "true" } }],
    [
      { id: "a", type: "source", datasetId: "a", inputs: [] },
      { id: "b", type: "source", datasetId: "b", inputs: [] },
    ],
    [
      {
        id: "a",
        type: "filter",
        inputs: ["missing"],
        config: { condition: "true" },
      },
    ],
  ])
    expect(() =>
      compilePipeline(nodes.map((n) => PipelineNodeSchema.parse(n))),
    ).toThrow();
});
it("rejects extra source and unary inputs rather than silently ignoring branches", () => {
  const node = (n: unknown) => PipelineNodeSchema.parse(n);
  const a = node({ id: "a", type: "source", datasetId: "a", inputs: [] }),
    b = node({ id: "b", type: "source", datasetId: "b", inputs: [] });
  expect(() =>
    compilePipeline([
      a,
      node({ id: "b", type: "source", datasetId: "b", inputs: ["a"] }),
    ]),
  ).toThrow("source cannot have inputs");
  expect(() =>
    compilePipeline([
      a,
      b,
      node({
        id: "filter",
        type: "filter",
        inputs: ["a", "b"],
        config: { condition: "true" },
      }),
    ]),
  ).toThrow("exactly one input");
  // Joins and SQL nodes take several inputs.
  expect(
    compilePipeline([
      a,
      b,
      node({
        id: "join",
        type: "join",
        inputs: ["a", "b"],
        config: { on: "true" },
      }),
    ]).inputs,
  ).toEqual(["a", "b"]);
  expect(
    compilePipeline([
      a,
      b,
      node({
        id: "sql",
        type: "sql",
        inputs: ["a", "b"],
        sql: "SELECT * FROM node_a UNION ALL SELECT * FROM node_b",
      }),
    ]).inputs,
  ).toEqual(["a", "b"]);
});
