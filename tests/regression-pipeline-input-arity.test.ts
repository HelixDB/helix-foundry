import { it, expect } from "vitest";
import { compilePipeline } from "../packages/shared/src/pipeline.js";
import { PipelineNodeSchema } from "../packages/shared/src/index.js";
const node = (n: unknown) => PipelineNodeSchema.parse(n);
it("rejects extra source and unary inputs rather than silently ignoring branches", () => {
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
