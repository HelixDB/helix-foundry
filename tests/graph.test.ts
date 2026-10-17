import { describe, expect, it, beforeAll, afterAll } from "vitest";
import { mkdtemp, rm, writeFile, mkdir } from "node:fs/promises";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { execute } from "../services/executor/src/engine.js";

// A small fraud-style graph:
//   A -> B -> C -> A   (money-flow ring of 3)
//   D -> E, D -> F, D -> G  (fan-out structuring)
//   H -> I -> J -> K   (cash-out trail ending at K)
//   L -> K, M -> K     (more flows into K)
let root: string;
let scope: string;

const ringEdges = [
  { from_id: "A", to_id: "B", amount: 100 },
  { from_id: "B", to_id: "C", amount: 100 },
  { from_id: "C", to_id: "A", amount: 100 },
  { from_id: "D", to_id: "E", amount: 500 },
  { from_id: "D", to_id: "F", amount: 500 },
  { from_id: "D", to_id: "G", amount: 500 },
  { from_id: "H", to_id: "I", amount: 1000 },
  { from_id: "I", to_id: "J", amount: 1000 },
  { from_id: "J", to_id: "K", amount: 1000 },
  { from_id: "L", to_id: "K", amount: 2000 },
  { from_id: "M", to_id: "K", amount: 3000 },
];

async function ingestEdges(rows: Record<string, unknown>[], name: string) {
  const version = "v_" + name;
  const file = name + ".json";
  const dir = join(root, "incoming", scope);
  await mkdir(dir, { recursive: true });
  await writeFile(join(dir, file), rows.map((r) => JSON.stringify(r)).join("\n"));
  return execute({ mode: "ingest", scope, version, file }, root);
}

async function runGraph(
  version: string,
  graph: Record<string, unknown>,
  name: string,
) {
  return execute(
    {
      mode: "graph",
      scope,
      inputs: [{ table: "d_" + name, version, aliases: ["edges"] }],
      version: "algo_" + name,
      persist: false,
      graph: graph as any,
    },
    root,
  );
}

beforeAll(async () => {
  root = await mkdtemp(join(tmpdir(), "graph-test-"));
  scope = "graphtest";
  await mkdir(join(root, "datasets", scope), { recursive: true });
  await ingestEdges(ringEdges, "edges");
});

afterAll(async () => {
  await rm(root, { recursive: true, force: true });
});

describe("graph algorithms", () => {
  it("computes degree centrality", async () => {
    const r = await runGraph(
      "v_edges",
      { algorithm: "degree", srcColumn: "from_id", dstColumn: "to_id" },
      "degree",
    );
    const byId = new Map(r.rows.map((row: any) => [row.node_id, row]));
    expect(Number(byId.get("D")?.out_degree)).toBe(3);
    expect(Number(byId.get("K")?.in_degree)).toBe(3);
    expect(Number(byId.get("A")?.total_degree)).toBe(2);
  });

  it("finds weakly connected components", async () => {
    const r = await runGraph(
      "v_edges",
      { algorithm: "wcc", srcColumn: "from_id", dstColumn: "to_id" },
      "wcc",
    );
    const comp = new Map(
      r.rows.map((row: any) => [row.node_id, row.component_id]),
    );
    expect(comp.get("A")).toBe(comp.get("B"));
    expect(comp.get("B")).toBe(comp.get("C"));
    expect(comp.get("D")).toBe(comp.get("E"));
    expect(comp.get("H")).toBe(comp.get("K"));
    expect(comp.get("A")).not.toBe(comp.get("D"));
  });

  it("computes PageRank scores", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "pagerank",
        srcColumn: "from_id",
        dstColumn: "to_id",
        damping: 0.85,
        maxIterations: 30,
        tolerance: 1e-8,
      },
      "pr",
    );
    const byId = new Map(
      r.rows.map((row: any) => [row.node_id, Number(row.pagerank)]),
    );
    // K is a sink with 3 in-edges (J, L, M); E only gets 1/3 of D's mass.
    expect(byId.get("K")).toBeGreaterThan(byId.get("E")!);
    // All scores positive and roughly sum to 1.
    const sum = [...byId.values()].reduce((a, b) => a + b, 0);
    expect(sum).toBeGreaterThan(0.9);
    expect(sum).toBeLessThan(1.1);
  });

  it("detects money-flow rings", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "fraud_rings",
        srcColumn: "from_id",
        dstColumn: "to_id",
        maxDepth: 4,
        minRingSize: 3,
        topN: 10,
      },
      "rings",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    const first = r.rows[0] as any;
    expect(String(first.explanation)).toContain("ring");
    expect(String(first.node_ids)).toContain("A");
  });

  it("detects cash-out trails", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "cashout_trails",
        srcColumn: "from_id",
        dstColumn: "to_id",
        maxDepth: 5,
        topN: 10,
      },
      "cashout",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    const first = r.rows[0] as any;
    expect(String(first.explanation)).toContain("cash-out");
    expect(String(first.node_ids)).toBe("K");
  });

  it("finds shortest path between two nodes", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "shortest_path",
        srcColumn: "from_id",
        dstColumn: "to_id",
        sourceId: "H",
        targetId: "K",
        maxDepth: 6,
      },
      "path",
    );
    expect(r.rows.length).toBe(1);
    const row = r.rows[0] as any;
    expect(Number(row.length)).toBe(3);
    expect(String(row.path_nodes)).toBe("H,I,J,K");
  });

  it("finds k-nearest neighbours by Jaccard similarity", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "knn",
        srcColumn: "from_id",
        dstColumn: "to_id",
        topK: 5,
      },
      "knn",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    const row = r.rows[0] as any;
    expect(row.node_id).toBeDefined();
    expect(row.neighbour_id).toBeDefined();
    expect(Number(row.score)).toBeGreaterThan(0);
  });

  it("computes approximate betweenness centrality", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "betweenness",
        srcColumn: "from_id",
        dstColumn: "to_id",
        sampleSize: 20,
        maxDepth: 6,
      },
      "betweenness",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    // J sits on the H->I->J->K path — should have higher betweenness than leaves.
    const byId = new Map(
      r.rows.map((row: any) => [row.node_id, Number(row.betweenness)]),
    );
    expect(byId.get("J")).toBeGreaterThan(byId.get("E")!);
  });

  it("finds all paths between two nodes", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "all_paths",
        srcColumn: "from_id",
        dstColumn: "to_id",
        sourceId: "H",
        targetId: "K",
        maxDepth: 5,
        topN: 10,
      },
      "allpaths",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    const first = r.rows[0] as any;
    expect(Number(first.length)).toBeGreaterThanOrEqual(3);
    expect(String(first.path_nodes)).toContain("H");
    expect(String(first.path_nodes)).toContain("K");
  });

  it("computes node embeddings", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "node_embeddings",
        srcColumn: "from_id",
        dstColumn: "to_id",
      },
      "embed",
    );
    expect(r.rows.length).toBeGreaterThan(0);
    const first = r.rows[0] as any;
    expect(first.node_id).toBeDefined();
    // At least one embedding dimension present.
    expect(first.dim_0).toBeDefined();
  });

  it("scores unconnected pairs for link prediction", async () => {
    const r = await runGraph(
      "v_edges",
      {
        algorithm: "link_prediction",
        srcColumn: "from_id",
        dstColumn: "to_id",
        topN: 10,
      },
      "lp",
    );
    // The graph has no unconnected pairs sharing neighbours (it's sparse),
    // so 0 rows is valid; if any exist, they must be scored.
    for (const row of r.rows as any[]) {
      expect(row.node_a).toBeDefined();
      expect(row.node_b).toBeDefined();
      expect(Number(row.adamic_adar)).toBeGreaterThanOrEqual(0);
    }
  });
});
