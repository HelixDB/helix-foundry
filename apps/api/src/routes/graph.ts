import type { FastifyInstance } from "fastify";
import {
  AlgorithmSpecSchema,
  FraudScanSchema,
  GraphPathRequestSchema,
  algorithmKinds,
  fraudRecipeKinds,
} from "../../../../packages/shared/src/graph.js";
import { runAlgorithm, runFraudScan, findPath } from "../graph.js";
import { toGraphML, toJSONLD, toDOT, introspectSchema, toAdjacencyMap, graphStats } from "../graph-io.js";
import { executeOntologyQuery } from "../graphql.js";
import { AppError } from "../security.js";
import type { Store } from "../store.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";
import { z } from "zod";
import type { Resource } from "../../../../packages/shared/src/index.js";

export const graphSchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/graph/algorithms": AlgorithmSpecSchema,
  "POST /api/v1/workspaces/:w/graph/fraud/scan": FraudScanSchema,
  "POST /api/v1/workspaces/:w/graph/paths": GraphPathRequestSchema,
  "POST /api/v1/workspaces/:w/graphql": z.object({
    query: z.string().min(1).max(20000),
    variables: z.record(z.string(), z.unknown()).optional(),
  }),
};

// Graph compute: GDS-like algorithms, fraud recipes and pathfinding.

// The published ontology as a node list and edge list, rebuilt only when its
// generation or the records changed (60 s TTL), so four APOC-like endpoints
// share one load instead of walking up to 5000 objects per request.
type GraphNodes = { id: string; label: string; type: string }[];
type GraphEdges = { src: string; dst: string; name: string }[];
const graphCache = new Map<string, { at: number; nodes: GraphNodes; edges: GraphEdges }>();
const GRAPH_CACHE_MS = 60_000;

export async function publishedGraph(
  store: Store,
  scope: string,
  limit: number,
): Promise<{ nodes: GraphNodes; edges: GraphEdges }> {
  const generation = (await store.get(scope, "workspace"))?.data
    .activeGeneration;
  const key = `${scope}|${generation}|${limit}`;
  const cached = graphCache.get(key);
  if (cached && Date.now() - cached.at < GRAPH_CACHE_MS)
    return cached;
  const objects: Resource[] = (await store.list(scope, "object"))
    .filter((o: any) => o.data.generation === generation)
    .slice(0, limit);
  const nodes: GraphNodes = objects.map((o: any) => ({
    id: String(o.data.logicalId ?? o.id),
    label: String(o.data.label ?? o.name),
    type: String(o.data.type ?? ""),
  }));
  // Edges live in `relation` records (from/to hold object ids), the same
  // source the explorer's /graph route reads; never on the objects themselves.
  const idToLogical = new Map(
    objects.map((o: any) => [o.id, String(o.data.logicalId ?? o.id)]),
  );
  const edges: GraphEdges = [];
  for (const r of await store.list(scope, "relation")) {
    const src = idToLogical.get((r.data as any).from),
      dst = idToLogical.get((r.data as any).to);
    if (src && dst)
      edges.push({ src, dst, name: String(r.name ?? "related") });
  }
  while (graphCache.size > 16) {
    const oldest = graphCache.keys().next().value;
    if (!oldest) break;
    graphCache.delete(oldest);
  }
  const built = { at: Date.now(), nodes, edges };
  graphCache.set(key, built);
  return built;
}

export function registerGraphRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access } = deps;

  // Catalog of what the graph layer can do.
  app.get("/api/v1/workspaces/:w/graph/schema", async (req) => {
    await access(req);
    return {
      algorithms: algorithmKinds.map((k) => ({
        kind: k,
        description: descriptions[k] || k,
      })),
      fraudRecipes: fraudRecipeKinds.map((k) => ({
        kind: k,
        description: fraudDescriptions[k] || k,
      })),
    };
  });

  app.post("/api/v1/workspaces/:w/graph/algorithms", async (req) => {
    const a = await access(req, "viewer"),
      spec = AlgorithmSpecSchema.parse(req.body);
    return runAlgorithm(store, a.scope, spec);
  });

  app.post("/api/v1/workspaces/:w/graph/fraud/scan", async (req) => {
    const a = await access(req, "viewer"),
      scan = FraudScanSchema.parse(req.body);
    return runFraudScan(store, a.scope, scan);
  });

  app.post("/api/v1/workspaces/:w/graph/paths", async (req) => {
    const a = await access(req, "viewer"),
      p = GraphPathRequestSchema.parse(req.body);
    return findPath(store, a.scope, p);
  });

  // GraphQL: query the published ontology as a graph.
  app.post("/api/v1/workspaces/:w/graphql", async (req) => {
    const a = await access(req, "viewer"),
      b = z
        .object({
          query: z.string().min(1).max(20000),
          variables: z.record(z.string(), z.unknown()).optional(),
        })
        .parse(req.body);
    return executeOntologyQuery(store, a.scope, b.query, b.variables);
  });

  // Graph export: GraphML, JSON-LD, Graphviz DOT. The body is sent with its
  // own content type and a filename, so the browser downloads a real file.
  app.get("/api/v1/workspaces/:w/graph/export/:format", async (req: any, reply) => {
    const a = await access(req, "viewer");
    const format = String(req.params.format).toLowerCase();
    const { nodes, edges } = await publishedGraph(store, a.scope, 2000);
    let contentType: string, ext: string, body: string;
    if (format === "graphml") {
      contentType = "application/xml";
      ext = "graphml";
      body = toGraphML(nodes, edges);
    } else if (format === "jsonld" || format === "json-ld") {
      contentType = "application/ld+json";
      ext = "jsonld";
      body = toJSONLD(nodes, edges);
    } else if (format === "dot" || format === "graphviz") {
      contentType = "text/vnd.graphviz";
      ext = "dot";
      body = toDOT(nodes, edges);
    } else
      throw new AppError(
        400,
        `Unknown export format "${format}". Use graphml, jsonld or dot.`,
      );
    reply.header("content-type", `${contentType}; charset=utf-8`);
    reply.header("content-disposition", `attachment; filename="graph.${ext}"`);
    return body;
  });

  // APOC-like: schema introspection.
  app.get("/api/v1/workspaces/:w/graph/introspect", async (req) => {
    const a = await access(req, "viewer");
    const { nodes, edges } = await publishedGraph(store, a.scope, 5000);
    return introspectSchema(nodes, edges);
  });

  // APOC-like: graph statistics.
  app.get("/api/v1/workspaces/:w/graph/stats", async (req) => {
    const a = await access(req, "viewer");
    const { nodes, edges } = await publishedGraph(store, a.scope, 5000);
    return graphStats(nodes, edges);
  });

  // APOC-like: adjacency list for the whole graph, so a client can filter down
  // to one node's neighbours.
  app.get("/api/v1/workspaces/:w/graph/adjacency/:nodeId", async (req: any) => {
    const a = await access(req, "viewer");
    const nodeId = String(req.params.nodeId);
    const { edges } = await publishedGraph(store, a.scope, 5000);
    const adjacency = toAdjacencyMap(edges, "both");
    return {
      nodeId,
      neighbours: adjacency[nodeId] || [],
      degree: (adjacency[nodeId] || []).length,
    };
  });
}

const descriptions: Record<string, string> = {
  degree: "In, out and total degree per node.",
  wcc: "Weakly connected components (community partitions).",
  pagerank: "PageRank centrality score per node.",
  louvain: "Louvain-style community detection.",
  betweenness: "Approximate betweenness centrality (sampled BFS).",
  node_similarity: "Jaccard similarity between node neighbour sets (top-k pairs).",
  knn: "k-nearest neighbours by Jaccard similarity.",
  shortest_path: "Bounded shortest path between two nodes.",
  all_paths: "All simple paths between two nodes up to maxDepth.",
  node_embeddings: "Node embeddings via random walks + co-occurrence + truncated SVD.",
  link_prediction: "Score unconnected node pairs by common neighbours, Adamic-Adar, Jaccard.",
  vector_search: "Vector similarity search: top-k nearest neighbours by cosine similarity of embeddings.",
};

const fraudDescriptions: Record<string, string> = {
  money_flow_rings: "Detect money-flow rings / circular fund movements.",
  structuring: "Detect structuring: many small transactions near a threshold.",
  cashout_trails: "Detect cash-out trails: flows ending at high-fan-in sinks.",
  account_takeover: "Detect account takeover: sudden fan-out burst pattern.",
  collusion: "Detect collusion: dense triangles / clique-like subgraphs.",
};
