// Graph compute service: runs GDS-like algorithms and fraud recipes through
// the DuckDB executor and returns score tables / findings.
import { randomUUID } from "node:crypto";
import { execute } from "./executor.js";
import { datasetInputs, queryDatasets } from "./datasets.js";
import { assert } from "./security.js";
import { resource, transaction, now, type Store } from "./store.js";
import {
  datasetTable,
  type DatasetProfile,
} from "../../../packages/shared/src/index.js";
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
  const started = Date.now();
  const result = await execute({
    mode: "graph",
    scope,
    inputs: [input],
    version: randomUUID(),
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
  const table: ScoreTable = {
    algorithm: spec.kind,
    name: spec.name,
    columns: result.profile.columns.map((c) => c.name),
    rows: result.rows,
    durationMs: Date.now() - started,
  };
  if (spec.persist) {
    const ds = await persistScoreDataset(
      store,
      scope,
      spec.name,
      result.version,
      result.profile,
    );
    table.datasetId = ds.id;
  }
  return table;
}

// Keep the scores under the run's name: the first run creates the dataset
// from the executor's result file, a later run with the same name adds a new
// snapshot to it. Like an import's refresh, never through the worker.
export async function persistScoreDataset(
  store: Store,
  scope: string,
  name: string,
  version: string,
  profile: DatasetProfile,
) {
  const existing = (await store.list(scope, "dataset")).find(
    (d) => d.name === name,
  );
  return transaction(store, scope, async (tx) => {
    const ds = existing || resource(scope, "dataset", name, { versions: [] });
    tx.put(
      resource(scope, "version", name, { datasetId: ds.id, profile, file: version + ".parquet" }, version),
    );
    return tx.put({
      ...ds,
      revision: ds.revision + 1,
      updatedAt: now(),
      data: {
        ...ds.data,
        profile,
        activeVersion: version,
        pendingVersion: null,
        versions: [version, ...ds.data.versions],
        table: datasetTable(ds.id),
        lastSync: now(),
      },
    });
  });
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
  const out: FraudScanResult = {
    recipe: scan.recipe,
    name: scan.name,
    findings,
    scannedNodes: findings.length,
    scannedEdges: result.profile.rows,
    durationMs: Date.now() - started,
  };
  if (scan.persist) {
    const ds = await persistScoreDataset(
      store,
      scope,
      scan.name,
      result.version,
      result.profile,
    );
    out.datasetId = ds.id;
  }
  return out;
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
