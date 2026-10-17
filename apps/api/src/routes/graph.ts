import type { FastifyInstance } from "fastify";
import {
  AlgorithmSpecSchema,
  FraudScanSchema,
  GraphPathRequestSchema,
  algorithmKinds,
  fraudRecipeKinds,
} from "../../../../packages/shared/src/graph.js";
import { runAlgorithm, runFraudScan, findPath } from "../graph.js";
import { toGraphML, toJSONLD, toDOT, introspectSchema, toAdjacencyMap, graphStats, filterByDegree } from "../graph-io.js";
import { executeOntologyQuery } from "../graphql.js";
import type { Store } from "../store.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";
import { z } from "zod";

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

  // Graph export: GraphML, JSON-LD, Graphviz DOT.
  app.get("/api/v1/workspaces/:w/graph/export/:format", async (req: any) => {
    const a = await access(req, "viewer");
    const format = String(req.params.format);
    const generation = (await store.get(a.scope, "workspace"))?.data
      .activeGeneration;
    const types = (await store.list(a.scope, "objectType"))
      .filter((t: any) => t.data.generation === generation)
      .map((t: any) => t.data.name as string);
    const objects = (await store.list(a.scope, "object"))
      .filter((o: any) => o.data.generation === generation)
      .slice(0, 2000);
    const nodes = objects.map((o: any) => ({
      id: String(o.data.logicalId ?? o.id),
      label: String(o.data.label ?? o.name),
      type: String(o.data.type ?? ""),
    }));
    const edges: { src: string; dst: string; name: string }[] = [];
    for (const o of objects) {
      const links = o.data.links ?? {};
      for (const [relName, targets] of Object.entries(links)) {
        const list = Array.isArray(targets) ? targets : [targets];
        for (const t of list) {
          edges.push({
            src: String(o.data.logicalId ?? o.id),
            dst: String(t),
            name: relName,
          });
        }
      }
    }
    if (format === "graphml")
      return { contentType: "application/xml", body: toGraphML(nodes, edges) };
    if (format === "jsonld" || format === "json-ld")
      return { contentType: "application/ld+json", body: toJSONLD(nodes, edges) };
    if (format === "dot" || format === "graphviz")
      return { contentType: "text/vnd.graphviz", body: toDOT(nodes, edges) };
    throw new Error(`Unknown format: ${format}. Use graphml, jsonld or dot.`);
  });

  // APOC-like: schema introspection.
  app.get("/api/v1/workspaces/:w/graph/introspect", async (req) => {
    const a = await access(req, "viewer");
    const generation = (await store.get(a.scope, "workspace"))?.data
      .activeGeneration;
    const objects = (await store.list(a.scope, "object"))
      .filter((o: any) => o.data.generation === generation)
      .slice(0, 5000);
    const nodes = objects.map((o: any) => ({
      id: String(o.data.logicalId ?? o.id),
      label: String(o.data.label ?? o.name),
      type: String(o.data.type ?? ""),
    }));
    const edges: { src: string; dst: string; name: string }[] = [];
    for (const o of objects) {
      const links = o.data.links ?? {};
      for (const [relName, targets] of Object.entries(links)) {
        const list = Array.isArray(targets) ? targets : [targets];
        for (const t of list)
          edges.push({ src: String(o.data.logicalId ?? o.id), dst: String(t), name: relName });
      }
    }
    return introspectSchema(nodes, edges);
  });

  // APOC-like: graph statistics.
  app.get("/api/v1/workspaces/:w/graph/stats", async (req) => {
    const a = await access(req, "viewer");
    const generation = (await store.get(a.scope, "workspace"))?.data
      .activeGeneration;
    const objects = (await store.list(a.scope, "object"))
      .filter((o: any) => o.data.generation === generation)
      .slice(0, 5000);
    const nodes = objects.map((o: any) => ({
      id: String(o.data.logicalId ?? o.id),
      type: String(o.data.type ?? ""),
    }));
    const edges: { src: string; dst: string; name: string }[] = [];
    for (const o of objects) {
      const links = o.data.links ?? {};
      for (const [relName, targets] of Object.entries(links)) {
        const list = Array.isArray(targets) ? targets : [targets];
        for (const t of list)
          edges.push({ src: String(o.data.logicalId ?? o.id), dst: String(t), name: relName });
      }
    }
    return graphStats(nodes, edges);
  });

  // APOC-like: adjacency map for a given node.
  app.get("/api/v1/workspaces/:w/graph/adjacency/:nodeId", async (req: any) => {
    const a = await access(req, "viewer");
    const nodeId = String(req.params.nodeId);
    const generation = (await store.get(a.scope, "workspace"))?.data
      .activeGeneration;
    const objects = (await store.list(a.scope, "object"))
      .filter((o: any) => o.data.generation === generation)
      .slice(0, 5000);
    const edges: { src: string; dst: string; name: string }[] = [];
    for (const o of objects) {
      const links = o.data.links ?? {};
      for (const [relName, targets] of Object.entries(links)) {
        const list = Array.isArray(targets) ? targets : [targets];
        for (const t of list)
          edges.push({ src: String(o.data.logicalId ?? o.id), dst: String(t), name: relName });
      }
    }
    const allNodes = objects.map((o: any) => ({
      id: String(o.data.logicalId ?? o.id),
      type: String(o.data.type ?? ""),
    }));
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
