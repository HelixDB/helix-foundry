import { PipelineNodeSchema } from "./pipelines.js";
import { datasetTable } from "./index.js";
import { z } from "zod";
export type PipelineNode = z.infer<typeof PipelineNodeSchema>;
const q = (v: string) => '"' + v.replaceAll('"', '""') + '"';
export function compilePipeline(nodes: PipelineNode[]) {
  if (!nodes.length) throw new Error("Add at least one node");
  const map = new Map(nodes.map((n) => [n.id, n]));
  if (map.size !== nodes.length) throw new Error("Node IDs must be unique");
  const visiting = new Set<string>(),
    done = new Set<string>(),
    ctes: string[] = [],
    inputs = new Set<string>();
  const ref = (id: string) => q("node_" + id);
  function visit(id: string) {
    if (done.has(id)) return;
    if (visiting.has(id)) throw new Error("Pipeline has a cycle");
    const n = map.get(id);
    if (!n) throw new Error("Unknown node " + id);
    if (n.type === "source" && n.inputs.length !== 0)
      throw new Error("A source cannot have inputs");
    if (!["source", "sql", "join"].includes(n.type) && n.inputs.length !== 1)
      throw new Error("A transform needs exactly one input");
    visiting.add(id);
    n.inputs.forEach(visit);
    const c = n.config as any,
      source = n.inputs[0] ? ref(n.inputs[0]) : "",
      columns = (xs: string[]) => xs.map(q).join(", ");
    let sql = "";
    if (n.type === "source") {
      if (!n.datasetId) throw new Error("Select a source dataset");
      inputs.add(n.datasetId);
      sql = "SELECT * FROM " + q(datasetTable(n.datasetId));
    } else {
      if (!source && n.type !== "sql")
        throw new Error("Connect an input to " + id);
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
          if (n.inputs.length !== 2) throw new Error("A join needs two inputs");
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
    throw new Error("Connect every branch to one output node");
  return {
    inputs: [...inputs],
    sql: `WITH ${ctes.join(",\n")}\nSELECT * FROM ${ref(leaves[0].id)}`,
  };
}
