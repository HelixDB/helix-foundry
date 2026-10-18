// Area schemas for Graph compute: projections, algorithms, fraud recipes.
// Owned by the graph area. Import values only from "zod" (or other area modules):
// index.ts re-exports this file, so a value import from "./index.js" would be circular.
// Type-only imports from "./index.js" are fine.
import { z } from "zod";

// A projected graph: node id set + weighted edge list, ready for algorithms.
export const GraphProjectionSchema = z.object({
  name: z.string().min(1).max(120),
  description: z.string().default(""),
  // Dataset holding edges with at least src/dst columns.
  edgeDatasetId: z.string().min(1),
  srcColumn: z.string().min(1).default("from_id"),
  dstColumn: z.string().min(1).default("to_id"),
  weightColumn: z.string().optional(),
  // Optional dataset holding node properties keyed by id.
  nodeDatasetId: z.string().optional(),
  nodeIdColumn: z.string().optional(),
  // Restrict to these relationship/edge kinds when the edge dataset has a type column.
  edgeTypes: z.array(z.string()).default([]),
  typeColumn: z.string().optional(),
});
export type GraphProjection = z.infer<typeof GraphProjectionSchema>;

export const algorithmKinds = [
  "degree",
  "wcc",
  "pagerank",
  "louvain",
  "betweenness",
  "node_similarity",
  "knn",
  "shortest_path",
  "all_paths",
  "node_embeddings",
  "link_prediction",
  "vector_search",
] as const;
export const AlgorithmKindSchema = z.enum(algorithmKinds);
export type AlgorithmKind = z.infer<typeof AlgorithmKindSchema>;

export const AlgorithmSpecSchema = z.object({
  kind: AlgorithmKindSchema,
  name: z.string().min(1).max(120),
  projection: GraphProjectionSchema,
  // Algorithm-specific parameters.
  params: z
    .object({
      damping: z.number().min(0).max(1).default(0.85),
      maxIterations: z.number().int().min(1).max(100).default(20),
      tolerance: z.number().min(1e-9).max(1).default(1e-6),
      topK: z.number().int().min(1).max(100).default(10),
      maxDepth: z.number().int().min(1).max(10).default(6),
      sourceId: z.string().optional(),
      targetId: z.string().optional(),
      sampleSize: z.number().int().min(10).max(10_000).default(200),
    })
    .default({
      damping: 0.85,
      maxIterations: 20,
      tolerance: 1e-6,
      topK: 10,
      maxDepth: 6,
      sampleSize: 200,
    }),
  // Persist scores as a dataset for dashboards / further pipelines.
  persist: z.boolean().default(true),
});
export type AlgorithmSpec = z.infer<typeof AlgorithmSpecSchema>;

// Result of running an algorithm: one row per node (or pair for similarity).
export const ScoreTableSchema = z.object({
  algorithm: AlgorithmKindSchema,
  name: z.string(),
  columns: z.array(z.string()),
  rows: z.array(z.record(z.string(), z.unknown())),
  datasetId: z.string().optional(),
  durationMs: z.number().optional(),
});
export type ScoreTable = z.infer<typeof ScoreTableSchema>;

export const fraudRecipeKinds = [
  "money_flow_rings",
  "structuring",
  "cashout_trails",
  "account_takeover",
  "collusion",
] as const;
export const FraudRecipeKindSchema = z.enum(fraudRecipeKinds);
export type FraudRecipeKind = z.infer<typeof FraudRecipeKindSchema>;

export const FraudScanSchema = z.object({
  recipe: FraudRecipeKindSchema,
  name: z.string().min(1).max(120),
  projection: GraphProjectionSchema,
  params: z
    .object({
      maxDepth: z.number().int().min(2).max(8).default(5),
      amountColumn: z.string().optional(),
      threshold: z.number().min(0).optional(),
      minRingSize: z.number().int().min(2).max(10).default(3),
      topN: z.number().int().min(1).max(500).default(50),
    })
    .default({
      maxDepth: 5,
      minRingSize: 3,
      topN: 50,
    }),
  persist: z.boolean().default(true),
});
export type FraudScan = z.infer<typeof FraudScanSchema>;

export const FraudFindingSchema = z.object({
  recipe: FraudRecipeKindSchema,
  findingId: z.string(),
  severity: z.enum(["low", "medium", "high", "critical"]),
  score: z.number(),
  nodeIds: z.array(z.string()),
  edgeIds: z.array(z.string()).default([]),
  amount: z.number().optional(),
  explanation: z.string(),
  detail: z.record(z.string(), z.unknown()).default({}),
});
export type FraudFinding = z.infer<typeof FraudFindingSchema>;

export const FraudScanResultSchema = z.object({
  recipe: FraudRecipeKindSchema,
  name: z.string(),
  findings: z.array(FraudFindingSchema),
  datasetId: z.string().optional(),
  scannedNodes: z.number(),
  scannedEdges: z.number(),
  durationMs: z.number(),
});
export type FraudScanResult = z.infer<typeof FraudScanResultSchema>;

// Pathfinding request/result.
export const GraphPathRequestSchema = z.object({
  projection: GraphProjectionSchema,
  sourceId: z.string().min(1),
  targetId: z.string().min(1),
  maxDepth: z.number().int().min(1).max(10).default(6),
  edgeTypes: z.array(z.string()).default([]),
});
export type GraphPathRequest = z.infer<typeof GraphPathRequestSchema>;

export const GraphPathSchema = z.object({
  nodeIds: z.array(z.string()),
  edgeIds: z.array(z.string()).default([]),
  length: z.number(),
  weight: z.number().optional(),
});
export type GraphPath = z.infer<typeof GraphPathSchema>;
