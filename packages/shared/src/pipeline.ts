import { PipelineNodeSchema } from "./pipelines.js";
import { datasetTable } from "./index.js";
import { z } from "zod";
export class PipelineValidationError extends Error {}
export type PipelineNode = z.infer<typeof PipelineNodeSchema>;
const q = (v: string) => '"' + v.replaceAll('"', '""') + '"';
const lit = (v: string) => "'" + v.replaceAll("'", "''") + "'";
export function compilePipeline(nodes: PipelineNode[]) {
  if (!nodes.length) throw new PipelineValidationError("Add at least one node");
  const map = new Map(nodes.map((n) => [n.id, n]));
  if (map.size !== nodes.length)
    throw new PipelineValidationError("Node IDs must be unique");
  const visiting = new Set<string>(),
    done = new Set<string>(),
    ctes: string[] = [],
    inputs = new Set<string>();
  const ref = (id: string) => q("node_" + id);
  function visit(id: string) {
    if (done.has(id)) return;
    if (visiting.has(id))
      throw new PipelineValidationError("Pipeline has a cycle");
    const n = map.get(id);
    if (!n) throw new PipelineValidationError("Unknown node " + id);
    if (n.type === "source" && n.inputs.length !== 0)
      throw new PipelineValidationError("A source cannot have inputs");
    if (!["source", "sql", "join"].includes(n.type) && n.inputs.length !== 1)
      throw new PipelineValidationError("A transform needs exactly one input");
    visiting.add(id);
    n.inputs.forEach(visit);
    const c = n.config as any,
      source = n.inputs[0] ? ref(n.inputs[0]) : "",
      columns = (xs: string[]) => xs.map(q).join(", ");
    let sql = "";
    if (n.type === "source") {
      if (!n.datasetId)
        throw new PipelineValidationError("Select a source dataset");
      inputs.add(n.datasetId);
      sql = "SELECT * FROM " + q(datasetTable(n.datasetId));
    } else {
      if (!source && n.type !== "sql")
        throw new PipelineValidationError("Connect an input to " + id);
      switch (n.type) {
        case "select":
          sql = `SELECT ${columns(z.array(z.string()).min(1).parse(c.columns))} FROM ${source}`;
          break;
        case "rename":
          sql = `SELECT * RENAME (${Object.entries(
            z.record(z.string(), z.string()).parse(c.mapping),
          )
            .map(([a, b]) => q(a) + " AS " + q(b))
            .join(", ")}) FROM ${source}`;
          break;
        case "cast":
          sql = `SELECT * REPLACE (${Object.entries(
            z.record(z.string(), z.string()).parse(c.columns),
          )
            .map(([name, type]) => `CAST(${q(name)} AS ${type}) AS ${q(name)}`)
            .join(", ")}) FROM ${source}`;
          break;
        case "filter":
          sql = `SELECT * FROM ${source} WHERE ${z.string().min(1).parse(c.condition)}`;
          break;
        case "derive":
          sql = `SELECT *, ${Object.entries(
            z.record(z.string(), z.string()).parse(c.columns),
          )
            .map(([name, expr]) => `${expr} AS ${q(name)}`)
            .join(", ")} FROM ${source}`;
          break;
        case "deduplicate":
          sql = c.keys?.length
            ? `SELECT * FROM ${source} QUALIFY row_number() OVER (PARTITION BY ${columns(c.keys)}) = 1`
            : `SELECT DISTINCT * FROM ${source}`;
          break;
        case "join":
          if (n.inputs.length !== 2)
            throw new PipelineValidationError("A join needs two inputs");
          sql = `SELECT ${c.select || "a.*, b.*"} FROM ${source} a ${c.joinType === "left" ? "LEFT" : "INNER"} JOIN ${ref(n.inputs[1])} b ON ${z.string().min(1).parse(c.on)}`;
          break;
        case "aggregate": {
          const group = z.array(z.string()).parse(c.groupBy || []),
            metrics = Object.entries(
              z.record(z.string(), z.string()).parse(c.metrics),
            ).map(([name, expr]) => `${expr} AS ${q(name)}`);
          sql = `SELECT ${[...group.map(q), ...metrics].join(", ")} FROM ${source}${group.length ? " GROUP BY " + columns(group) : ""}`;
          break;
        }
        case "graph-filter": {
          // Filter rows by a score column threshold (e.g. pagerank > 0.01).
          const column = z.string().min(1).parse(c.column),
            op = z.enum([">", ">=", "<", "<=", "=", "!="]).parse(c.op || ">"),
            value = z.union([z.string(), z.number()]).parse(c.value);
          const v = typeof value === "number" ? String(value) : lit(value);
          sql = `SELECT * FROM ${source} WHERE ${q(column)} ${op} ${v}`;
          break;
        }
        case "graph-score": {
          // Join with a pre-computed score dataset (algorithm output).
          const scoreDatasetId = z.string().min(1).parse(c.scoreDatasetId),
            scoreKey = z.string().min(1).parse(c.scoreKey || "node_id"),
            inputKey = z.string().min(1).parse(c.inputKey || "id"),
            select = z.string().optional().parse(c.select);
          inputs.add(scoreDatasetId);
          sql = `SELECT ${select || "a.*, b.*"} FROM ${source} a INNER JOIN ${q(datasetTable(scoreDatasetId))} b ON a.${q(inputKey)} = b.${q(scoreKey)}`;
          break;
        }
        case "graph-embeddings": {
          // Join with a pre-computed embeddings dataset.
          const embedDatasetId = z.string().min(1).parse(c.embedDatasetId),
            embedKey = z.string().min(1).parse(c.embedKey || "node_id"),
            inputKey = z.string().min(1).parse(c.inputKey || "id"),
            dimensions = Number(z.number().int().min(2).max(64).default(8).parse(c.dimensions));
          inputs.add(embedDatasetId);
          const dims = Array.from({ length: dimensions }, (_, d) => `b.dim_${d}`);
          sql = `SELECT a.*, ${dims.join(", ")} FROM ${source} a INNER JOIN ${q(datasetTable(embedDatasetId))} b ON a.${q(inputKey)} = b.${q(embedKey)}`;
          break;
        }
        case "graph-similarity": {
          // Self-join to compute pairwise similarity (Jaccard on neighbour sets).
          const keyCol = z.string().min(1).parse(c.keyCol || "id"),
            neighbourCol = z.string().min(1).parse(c.neighbourCol || "neighbour"),
            topK = Number(z.number().int().min(1).max(100).default(10).parse(c.topK));
          sql = `WITH neighbours AS (SELECT ${q(keyCol)} AS id, ${q(neighbourCol)} AS nb FROM ${source}),
            pairs AS (
              SELECT a.id AS node_a, b.id AS node_b,
                CAST(count(*) AS DOUBLE) / CAST(
                  (SELECT count(*) FROM neighbours x WHERE x.id = a.id) +
                  (SELECT count(*) FROM neighbours y WHERE y.id = b.id) - count(*)
                AS DOUBLE) AS score
              FROM neighbours a
              JOIN neighbours b ON a.nb = b.nb AND a.id < b.id
              GROUP BY a.id, b.id
            )
            SELECT node_a, node_b, score FROM pairs
            QUALIFY row_number() OVER (PARTITION BY node_a ORDER BY score DESC) <= ${topK}`;
          break;
        }
        case "sql":
          sql = z.string().min(1).parse(n.sql);
          break;
      }
    }
    ctes.push(`${ref(id)} AS (${sql})`);
    visiting.delete(id);
    done.add(id);
  }
  nodes.forEach((n) => visit(n.id));
  const leaves = nodes.filter(
    (n) => !nodes.some((other) => other.inputs.includes(n.id)),
  );
  if (leaves.length !== 1)
    throw new PipelineValidationError(
      "Connect every branch to one output node",
    );
  return {
    inputs: [...inputs],
    sql: `WITH ${ctes.join(",\n")}\nSELECT * FROM ${ref(leaves[0].id)}`,
  };
}
