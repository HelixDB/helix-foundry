import { describe, expect, it } from "vitest";
import { toGraphML, toJSONLD, toDOT } from "../apps/api/src/graph-io.js";

const nodes = [
  { id: "A", label: "Alice", type: "Person" },
  { id: "B", label: "Bob", type: "Person" },
  { id: "C", label: "Acme", type: "Company" },
];
const edges = [
  { src: "A", dst: "B", name: "knows", weight: 1 },
  { src: "A", dst: "C", name: "works_at", weight: 2 },
];

describe("graph export", () => {
  it("exports GraphML with nodes and edges", () => {
    const ml = toGraphML(nodes, edges);
    expect(ml).toContain("<?xml");
    expect(ml).toContain('<graphml xmlns="http://graphml.graphdrawing.org/xmlns">');
    expect(ml).toContain('<node id="A">');
    expect(ml).toContain('<edge id="e0" source="A" target="B">');
    expect(ml).toContain("</graphml>");
  });

  it("exports JSON-LD with @context and @graph", () => {
    const ld = toJSONLD(nodes, edges);
    const doc = JSON.parse(ld);
    expect(doc["@context"]).toBeDefined();
    expect(doc["@graph"]).toHaveLength(5); // 3 nodes + 2 edges
    const nodeA = doc["@graph"].find((x: any) => x["@id"] === "A");
    expect(nodeA.name).toBe("Alice");
    expect(nodeA["@type"]).toBe("Person");
  });

  it("exports Graphviz DOT", () => {
    const dot = toDOT(nodes, edges);
    expect(dot).toContain("digraph G {");
    expect(dot).toContain('"A"');
    expect(dot).toContain("Alice");
    expect(dot).toContain('"A" -> "B"');
    expect(dot).toContain("knows");
    expect(dot).toContain("}");
  });
});
