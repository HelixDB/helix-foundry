// Graph compute service: runs GDS-like algorithms and fraud recipes through
// the DuckDB executor and returns score tables / findings.
import { randomUUID } from "node:crypto";
import { execute } from "./executor.js";
import { datasetInputs, queryDatasets } from "./datasets.js";
import { assert } from "./security.js";
import type { Store } from "./store.js";
import type {
  AlgorithmSpec,
  FraudScan,
  GraphPathRequest,
  ScoreTable,
  FraudScanResult,
  GraphPath,
} from "../../../packages/shared/src/graph.js";

// How the executor's graph mode names its algorithm field.
type GraphAlgo =
  | "degree"
  | "wcc"
  | "pagerank"
  | "louvain"
  | "node_similarity"
  | "knn"
  | "betweenness"
  | "shortest_path"
  | "all_paths"
  | "node_embeddings"
  | "link_prediction"
  | "vector_search"
  | "fraud_rings"
  | "structuring"
  | "cashout_trails"
  | "account_takeover"
  | "collusion";

const fraudAlgo: Record<string, GraphAlgo> = {
  money_flow_rings: "fraud_rings",
  structuring: "structuring",
  cashout_trails: "cashout_trails",
  account_takeover: "account_takeover",
  collusion: "collusion",
};

export async function runAlgorithm(
  store: Store,
  scope: string,
  spec: AlgorithmSpec,
): Promise<ScoreTable> {
  const [input] = await datasetInputs(store, scope, [spec.projection.edgeDatasetId]);
  const version = randomUUID();
  const result = await execute({
    mode: "graph",
    scope,
    inputs: [input],
    version,
    persist: spec.persist,
    graph: {
      algorithm: spec.kind as GraphAlgo,
      srcColumn: spec.projection.srcColumn,
      dstColumn: spec.projection.dstColumn,
      weightColumn: spec.projection.weightColumn,
      damping: spec.params.damping,
      maxIterations: spec.params.maxIterations,
      tolerance: spec.params.tolerance,
      topK: spec.params.topK,
      maxDepth: spec.params.maxDepth,
      sourceId: spec.params.sourceId,
      targetId: spec.params.targetId,
      sampleSize: spec.params.sampleSize,
    },
  });
  return {
    algorithm: spec.kind,
    name: spec.name,
    columns: result.profile.columns.map((c) => c.name),
    rows: result.rows,
    durationMs: undefined,
  };
}

export async function runFraudScan(
  store: Store,
  scope: string,
  scan: FraudScan,
): Promise<FraudScanResult> {
  const [input] = await datasetInputs(store, scope, [scan.projection.edgeDatasetId]);
  const version = randomUUID();
  const algo = fraudAlgo[scan.recipe];
  assert(algo, "Unknown fraud recipe", 400);
  const started = Date.now();
  const result = await execute({
    mode: "graph",
    scope,
    inputs: [input],
    version,
    persist: scan.persist,
    graph: {
      algorithm: algo,
      srcColumn: scan.projection.srcColumn,
      dstColumn: scan.projection.dstColumn,
      amountColumn: scan.params.amountColumn,
      threshold: scan.params.threshold,
      maxDepth: scan.params.maxDepth,
      minRingSize: scan.params.minRingSize,
      topN: scan.params.topN,
    },
  });
  const findings = result.rows.map((row: any) => ({
    recipe: scan.recipe,
    findingId: String(row.finding_id ?? ""),
    severity: (row.severity ?? "medium") as "low" | "medium" | "high" | "critical",
    score: Number(row.score ?? 0),
    nodeIds: String(row.node_ids ?? "").split(",").filter(Boolean),
    edgeIds: [],
    amount: row.total_amount != null ? Number(row.total_amount) : undefined,
    explanation: String(row.explanation ?? ""),
    detail: row,
  }));
  return {
    recipe: scan.recipe,
    name: scan.name,
    findings,
    scannedNodes: findings.length,
    scannedEdges: result.profile.rows,
    durationMs: Date.now() - started,
  };
}

export async function findPath(
  store: Store,
  scope: string,
  req: GraphPathRequest,
): Promise<GraphPath | null> {
  const [input] = await datasetInputs(store, scope, [req.projection.edgeDatasetId]);
  const result = await execute({
    mode: "graph",
    scope,
    inputs: [input],
    version: randomUUID(),
    persist: false,
    graph: {
      algorithm: "shortest_path",
      srcColumn: req.projection.srcColumn,
      dstColumn: req.projection.dstColumn,
      sourceId: req.sourceId,
      targetId: req.targetId,
      maxDepth: req.maxDepth,
    },
  });
  const row = result.rows[0];
  if (!row || row.length == null) return null;
  return {
    nodeIds: String(row.path_nodes ?? "").split(",").filter(Boolean),
    edgeIds: [],
    length: Number(row.length),
  };
}
