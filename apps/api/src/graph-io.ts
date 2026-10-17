// Graph export utilities: GraphML, JSON-LD and Graphviz DOT formats.
// Pure string transforms from node/edge tables — no external dependencies.

type NodeRow = { id: string; label?: string; type?: string; [k: string]: unknown };
type EdgeRow = { src: string; dst: string; name?: string; weight?: number; [k: string]: unknown };

const xml = (s: string) =>
  s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

/**
 * Export a graph as GraphML (XML standard for graph interchange).
 * Compatible with Neo4j, Gephi, yEd, Cytoscape.
 */
export function toGraphML(nodes: NodeRow[], edges: EdgeRow[]): string {
  const nodeKeys = new Set<string>();
  for (const n of nodes) for (const k of Object.keys(n)) if (k !== "id") nodeKeys.add(k);
  const edgeKeys = new Set<string>();
  for (const e of edges) for (const k of Object.keys(e)) if (!["src", "dst"].includes(k)) edgeKeys.add(k);

  const lines: string[] = [];
  lines.push(`<?xml version="1.0" encoding="UTF-8"?>`);
  lines.push(`<graphml xmlns="http://graphml.graphdrawing.org/xmlns">`);
  for (const k of nodeKeys)
    lines.push(`  <key id="n_${xml(k)}" for="node" attr.name="${xml(k)}" attr.type="string"/>`);
  for (const k of edgeKeys)
    lines.push(`  <key id="e_${xml(k)}" for="edge" attr.name="${xml(k)}" attr.type="string"/>`);
  lines.push(`  <graph id="G" edgedefault="directed">`);
  for (const n of nodes) {
    lines.push(`    <node id="${xml(n.id)}">`);
    for (const k of nodeKeys) {
      if (n[k] !== undefined && n[k] !== null)
        lines.push(`      <data key="n_${xml(k)}">${xml(String(n[k]))}</data>`);
    }
    lines.push(`    </node>`);
  }
  for (let i = 0; i < edges.length; i++) {
    const e = edges[i];
    lines.push(`    <edge id="e${i}" source="${xml(e.src)}" target="${xml(e.dst)}">`);
    for (const k of edgeKeys) {
      if (e[k] !== undefined && e[k] !== null)
        lines.push(`      <data key="e_${xml(k)}">${xml(String(e[k]))}</data>`);
    }
    lines.push(`    </edge>`);
  }
  lines.push(`  </graph>`);
  lines.push(`</graphml>`);
  return lines.join("\n");
}

/**
 * Export a graph as JSON-LD with a simple schema.org-style context.
 * Suitable for semantic web / linked-data tooling.
 */
export function toJSONLD(nodes: NodeRow[], edges: EdgeRow[]): string {
  const doc = {
    "@context": {
      "@vocab": "https://helix-foundry.local/graph#",
      id: "@id",
      src: "https://helix-foundry.local/graph#source",
      dst: "https://helix-foundry.local/graph#target",
      name: "https://schema.org/name",
      label: "https://schema.org/name",
      type: "https://schema.org/additionalType",
      weight: "https://helix-foundry.local/graph#weight",
    },
    "@graph": [
      ...nodes.map((n) => ({
        "@id": n.id,
        "@type": n.type || "Node",
        name: n.label ?? n.id,
        ...Object.fromEntries(
          Object.entries(n).filter(([k]) => !["id", "label", "type"].includes(k)),
        ),
      })),
      ...edges.map((e, i) => ({
        "@id": `edge_${i}`,
        "@type": "Relationship",
        src: e.src,
        dst: e.dst,
        name: e.name ?? "related",
        ...(e.weight !== undefined ? { weight: e.weight } : {}),
      })),
    ],
  };
  return JSON.stringify(doc, null, 2);
}

/**
 * Export a graph as Graphviz DOT.
 * Renders with `dot`, `neato`, or any Graphviz-compatible tool.
 */
export function toDOT(nodes: NodeRow[], edges: EdgeRow[]): string {
  const lines: string[] = [];
  lines.push(`digraph G {`);
  lines.push(`  rankdir=LR;`);
  lines.push(`  node [shape=ellipse, style=filled, fillcolor=lightblue];`);
  for (const n of nodes) {
    const label = n.label ?? n.id;
    const attrs = [`label="${xml(String(label))}"`];
    if (n.type) attrs.push(`fillcolor="${typeColor(String(n.type))}"`);
    lines.push(`  "${xml(n.id)}" [${attrs.join(", ")}];`);
  }
  for (const e of edges) {
    const attrs: string[] = [];
    if (e.name) attrs.push(`label="${xml(e.name)}"`);
    if (e.weight !== undefined) attrs.push(`penwidth="${Math.min(5, Math.max(1, Number(e.weight)))}"`);
    lines.push(`  "${xml(e.src)}" -> "${xml(e.dst)}"${attrs.length ? ` [${attrs.join(", ")}]` : ""};`);
  }
  lines.push(`}`);
  return lines.join("\n");
}

// Consistent colors for node types in DOT export.
function typeColor(type: string): string {
  const palette = [
    "lightblue", "lightgreen", "lightsalmon", "lightyellow",
    "lightpink", "lightcyan", "lavender", "wheat",
  ];
  let hash = 0;
  for (let i = 0; i < type.length; i++) hash = (hash * 31 + type.charCodeAt(i)) | 0;
  return palette[Math.abs(hash) % palette.length];
}

// ─── APOC-like utilities ────────────────────────────────────────────────

export type SchemaIntrospection = {
  nodeTypes: { name: string; count: number; properties: string[] }[];
  edgeTypes: { name: string; count: number; properties: string[] }[];
  totalNodes: number;
  totalEdges: number;
};

/**
 * Introspect a graph schema: node types, edge types, properties and counts.
 * Equivalent to apoc.meta.schema / db.schema.visualization.
 */
export function introspectSchema(nodes: NodeRow[], edges: EdgeRow[]): SchemaIntrospection {
  const nodeTypes = new Map<string, { count: number; properties: Set<string> }>();
  for (const n of nodes) {
    const type = n.type || "Node";
    const entry = nodeTypes.get(type) || { count: 0, properties: new Set<string>() };
    entry.count++;
    for (const k of Object.keys(n)) if (!["id", "label", "type"].includes(k)) entry.properties.add(k);
    nodeTypes.set(type, entry);
  }
  const edgeTypes = new Map<string, { count: number; properties: Set<string> }>();
  for (const e of edges) {
    const type = e.name || "RELATED";
    const entry = edgeTypes.get(type) || { count: 0, properties: new Set<string>() };
    entry.count++;
    for (const k of Object.keys(e)) if (!["src", "dst", "name"].includes(k)) entry.properties.add(k);
    edgeTypes.set(type, entry);
  }
  return {
    nodeTypes: [...nodeTypes.entries()].map(([name, v]) => ({
      name, count: v.count, properties: [...v.properties].sort(),
    })),
    edgeTypes: [...edgeTypes.entries()].map(([name, v]) => ({
      name, count: v.count, properties: [...v.properties].sort(),
    })),
    totalNodes: nodes.length,
    totalEdges: edges.length,
  };
}

/**
 * Reshape a flat edge list into an adjacency map.
 * Equivalent to apoc.convert.toMap / graph refactoring utilities.
 */
export function toAdjacencyMap(
  edges: EdgeRow[],
  direction: "out" | "in" | "both" = "out",
): Record<string, string[]> {
  const map: Record<string, string[]> = {};
  for (const e of edges) {
    if (direction === "out" || direction === "both") {
      (map[e.src] ||= []).push(e.dst);
    }
    if (direction === "in" || direction === "both") {
      (map[e.dst] ||= []).push(e.src);
    }
  }
  // Deduplicate.
  for (const k of Object.keys(map)) map[k] = [...new Set(map[k])];
  return map;
}

/**
 * Compute graph-level statistics: density, degree distribution, components.
 * Equivalent to apoc.meta.stats.
 */
export function graphStats(nodes: NodeRow[], edges: EdgeRow[]) {
  const n = nodes.length, e = edges.length;
  const maxEdges = n * (n - 1);
  const outDeg = new Map<string, number>(), inDeg = new Map<string, number>();
  for (const edge of edges) {
    outDeg.set(edge.src, (outDeg.get(edge.src) || 0) + 1);
    inDeg.set(edge.dst, (inDeg.get(edge.dst) || 0) + 1);
  }
  const degrees = nodes.map((nd) => (outDeg.get(nd.id) || 0) + (inDeg.get(nd.id) || 0));
  degrees.sort((a, b) => b - a);
  return {
    nodeCount: n,
    edgeCount: e,
    density: maxEdges > 0 ? e / maxEdges : 0,
    avgDegree: n > 0 ? (2 * e) / n : 0,
    maxDegree: degrees[0] || 0,
    minDegree: degrees[degrees.length - 1] || 0,
    isolatedNodes: degrees.filter((d) => d === 0).length,
    directed: true,
  };
}

/**
 * Filter a graph by degree threshold (keep only nodes with degree >= min).
 * Equivalent to apoc.path.subgraphAll with pruning.
 */
export function filterByDegree(
  nodes: NodeRow[],
  edges: EdgeRow[],
  minDegree = 2,
): { nodes: NodeRow[]; edges: EdgeRow[] } {
  const deg = new Map<string, number>();
  for (const e of edges) {
    deg.set(e.src, (deg.get(e.src) || 0) + 1);
    deg.set(e.dst, (deg.get(e.dst) || 0) + 1);
  }
  const kept = nodes.filter((n) => (deg.get(n.id) || 0) >= minDegree);
  const keptIds = new Set(kept.map((n) => n.id));
  return {
    nodes: kept,
    edges: edges.filter((e) => keptIds.has(e.src) && keptIds.has(e.dst)),
  };
}
