import { describe, expect, it } from "vitest";

import {
  excludes,
  shownGraph,
  shownLabels,
  shownStats,
} from "@/web/areas/explore/explorer/lib/exclusion";
import { asBoolean } from "@/web/areas/explore/explorer/lib/storage";
import type {
  GraphEdge,
  GraphNode,
  LabelExclusion,
} from "@/web/areas/explore/explorer/lib/types";

// What the explorer leaves out when Foundry's own labels are hidden: every
// filter is pure, and without an exclusion each returns its input.

const foundry: LabelExclusion = {
  nodePrefix: "Foundry",
  edgePrefix: "FOUNDRY_",
  notice: "hidden",
};
const node = (dbId: number, label: string): GraphNode => ({
  id: `n:${dbId}`,
  dbId,
  label,
  properties: {},
  omitted: [],
  complete: true,
});
const edge = (dbId: number, label: string, from: number, to: number) =>
  ({
    id: `e:${dbId}`,
    dbId,
    label,
    source: `n:${from}`,
    target: `n:${to}`,
    properties: {},
  }) satisfies GraphEdge;

describe("excludes", () => {
  it("leaves out labels starting with their side's prefix", () => {
    expect(excludes(foundry, "node", "FoundryChunk")).toBe(true);
    expect(excludes(foundry, "node", "FoundryLater")).toBe(true);
    expect(excludes(foundry, "edge", "FOUNDRY_ABOUT")).toBe(true);
    expect(excludes(foundry, "node", "Customer")).toBe(false);
    expect(excludes(foundry, "edge", "signed_with")).toBe(false);
    // Each side has its own prefix.
    expect(excludes(foundry, "edge", "FoundryChunk")).toBe(false);
    expect(excludes(foundry, "node", "FOUNDRY_ABOUT")).toBe(false);
    // Nothing without an exclusion.
    expect(excludes(undefined, "node", "FoundryChunk")).toBe(false);
  });
});

describe("shownLabels", () => {
  it("drops excluded labels per side, keeping the order", () => {
    expect(
      shownLabels(
        {
          nodes: ["Customer", "FoundryChunk", "Contract"],
          edges: ["FOUNDRY_ABOUT", "signed_with"],
        },
        foundry,
      ),
    ).toEqual({ nodes: ["Customer", "Contract"], edges: ["signed_with"] });
    const all = { nodes: ["FoundryChunk"], edges: ["FOUNDRY_ABOUT"] };
    expect(shownLabels(all, undefined)).toEqual(all);
  });
});

describe("shownGraph", () => {
  it("drops excluded nodes, their edges and excluded edges", () => {
    const graph = {
      nodes: [
        node(1, "Customer"),
        node(2, "FoundryMention"),
        node(3, "Contract"),
      ],
      edges: [
        edge(10, "FOUNDRY_MENTION_OF", 2, 1),
        edge(11, "signed_with", 3, 1),
        // An excluded edge between shown nodes.
        edge(12, "FOUNDRY_LATER", 3, 1),
        // A shown label touching an excluded node.
        edge(13, "cites", 2, 3),
      ],
    };
    const shown = shownGraph(graph, foundry);
    expect(shown.nodes?.map((n) => n.label)).toEqual(["Customer", "Contract"]);
    expect(shown.edges?.map((e) => e.label)).toEqual(["signed_with"]);
    expect(shownGraph(graph, undefined)).toBe(graph);
  });

  it("keeps a side it wasn't given absent", () => {
    expect(shownGraph({ edges: [edge(1, "x", 1, 2)] }, foundry)).toEqual({
      edges: [edge(1, "x", 1, 2)],
    });
    expect(shownGraph({ nodes: [node(1, "FoundryChunk")] }, foundry)).toEqual({
      nodes: [],
    });
  });
});

describe("shownStats", () => {
  it("drops excluded labels and what they added to the totals", () => {
    const stats = {
      nodeLabels: [
        { label: "Customer", count: 64 },
        { label: "FoundryChunk", count: 3 },
        { label: "FoundryMention", count: null },
      ],
      edgeLabels: [
        { label: "FOUNDRY_ABOUT", count: 64 },
        { label: "signed_with", count: 2 },
      ],
      nodeTotal: 67,
      edgeTotal: 66,
      partial: { nodes: true, edges: false },
    };
    expect(shownStats(stats, foundry)).toEqual({
      nodeLabels: [{ label: "Customer", count: 64 }],
      edgeLabels: [{ label: "signed_with", count: 2 }],
      nodeTotal: 64,
      edgeTotal: 2,
      partial: { nodes: true, edges: false },
    });
    expect(shownStats(stats, undefined)).toBe(stats);
  });

  it("takes a scan's excluded counts from its exact totals, never below zero", () => {
    // A full scan's totals count every node, not only the listed labels.
    const scan = {
      nodeLabels: [
        { label: "Customer", count: 2 },
        { label: "FoundryDocument", count: 1 },
      ],
      edgeLabels: [{ label: "FOUNDRY_IN_DOCUMENT", count: 5 }],
      nodeTotal: 10,
      edgeTotal: 3,
    };
    expect(shownStats(scan, foundry)).toEqual({
      nodeLabels: [{ label: "Customer", count: 2 }],
      edgeLabels: [],
      nodeTotal: 9,
      edgeTotal: 0,
    });
    expect(
      shownStats({ ...scan, nodeTotal: null, edgeTotal: null }, foundry),
    ).toMatchObject({ nodeTotal: null, edgeTotal: null });
  });
});

describe("the remembered choice to show them", () => {
  it("reads only a boolean", () => {
    expect(asBoolean(true)).toBe(true);
    expect(asBoolean(false)).toBe(false);
    for (const v of ["true", 1, null, {}]) expect(asBoolean(v)).toBeNull();
  });
});
