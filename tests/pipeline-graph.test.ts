import { it, expect } from "vitest";
import { compilePipeline } from "../packages/shared/src/pipeline.js";
import { PipelineNodeSchema, datasetTable } from "../packages/shared/src/index.js";
import { DuckDBInstance } from "@duckdb/node-api";

// The graph pipeline nodes compile to SQL that runs against a score dataset,
// an embeddings dataset, or the input's own neighbour column.
it("compiles graph-score, graph-filter and graph-embeddings and executes them over DuckDB", async () => {
  const nodes = [
    { id: "source", type: "source", datasetId: "orders", inputs: [] },
    {
      id: "score",
      type: "graph-score",
      inputs: ["source"],
      config: { scoreDatasetId: "scores" },
    },
    {
      id: "top",
      type: "graph-filter",
      inputs: ["score"],
      config: { column: "score", op: ">", value: 0.5 },
    },
    {
      id: "embed",
      type: "graph-embeddings",
      inputs: ["top"],
      config: { embedDatasetId: "embeds", dimensions: 2 },
    },
  ].map((n) => PipelineNodeSchema.parse(n));
  expect(nodes.every((n) => PipelineNodeSchema.safeParse(n).success)).toBe(true);
  const p = compilePipeline(nodes);
  expect(p.inputs).toEqual(["orders", "scores", "embeds"]);
  // A number filter value keeps raw; the score dataset joins by node_id.
  expect(p.sql).toContain('"d_orders"');
  expect(p.sql).toContain('"d_scores" b ON a."id" = b."node_id"');
  expect(p.sql).toContain('WHERE "score" > 0.5');
  expect(p.sql).toContain("b.dim_0");
  const db = await DuckDBInstance.create(":memory:"),
    c = await db.connect();
  await c.run(
    `CREATE TABLE ${datasetTable("orders")} (id VARCHAR, customer VARCHAR, amount DOUBLE)`,
  );
  await c.run(
    `INSERT INTO ${datasetTable("orders")} VALUES ('1','ann',5), ('2','bob',1)`,
  );
  await c.run(
    `CREATE TABLE ${datasetTable("scores")} (node_id VARCHAR, score DOUBLE)`,
  );
  await c.run(
    `INSERT INTO ${datasetTable("scores")} VALUES ('1',0.9), ('2',0.1)`,
  );
  await c.run(
    `CREATE TABLE ${datasetTable("embeds")} (node_id VARCHAR, dim_0 DOUBLE, dim_1 DOUBLE)`,
  );
  await c.run(`INSERT INTO ${datasetTable("embeds")} VALUES ('1',0.4,0.8)`);
  const rows = (
    await c.runAndReadAll(p.sql)
  ).getRowObjectsJson();
  expect(rows).toEqual([
    {
      id: "1",
      customer: "ann",
      amount: 5,
      score: 0.9,
      node_id: "1",
      dim_0: 0.4,
      dim_1: 0.8,
    },
  ]);
  await c.closeSync();
  await db.closeSync();
});

it("compiles graph-similarity to a bounded Jaccard self-join", async () => {
  const nodes = [
    { id: "edges", type: "source", datasetId: "edges", inputs: [] },
    {
      id: "similar",
      type: "graph-similarity",
      inputs: ["edges"],
      config: { keyCol: "from_id", neighbourCol: "to_id", topK: 3 },
    },
  ].map((n) => PipelineNodeSchema.parse(n));
  const p = compilePipeline(nodes);
  expect(p.inputs).toEqual(["edges"]);
  expect(p.sql).toContain('"d_edges"');
  expect(p.sql).toContain("QUALIFY");
  expect(p.sql).toContain("<= 3");
});
