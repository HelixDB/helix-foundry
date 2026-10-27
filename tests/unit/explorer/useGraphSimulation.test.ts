import { describe, expect, it } from "vitest";

import type {
  GraphData,
  GraphEdge,
  GraphNode,
} from "@/web/areas/explore/explorer/lib/types";
import { boundsOf } from "@/web/areas/explore/explorer/canvas/placement";
import { ForceLayout } from "@/web/areas/explore/explorer/canvas/useGraphSimulation";

function seeded(seed = 7) {
  return () => {
    seed = (seed * 16807) % 2147483647;
    return (seed - 1) / 2147483646;
  };
}

const node = (dbId: number, label = "User"): GraphNode => ({
  id: `n:${dbId}`,
  dbId,
  label,
  properties: { name: `node ${dbId}` },
  omitted: [],
  complete: true,
});

const edge = (
  dbId: number,
  source: number,
  target: number,
  label = "KNOWS",
): GraphEdge => ({
  id: `e:${dbId}`,
  dbId,
  label,
  source: `n:${source}`,
  target: `n:${target}`,
  properties: {},
});

function ring(count: number, labels = ["User", "Order", "Product"]): GraphData {
  const nodes = Array.from({ length: count }, (_, i) =>
    node(i, labels[i % labels.length]),
  );
  const edges = nodes.map((_, i) => edge(i, i, (i + 1) % count));
  return { nodes, edges };
}

const center = { x: 0, y: 0 };

describe("ForceLayout.sync", () => {
  it("places every node of the first graph and reports it", () => {
    const layout = new ForceLayout(seeded());
    const result = layout.sync(ring(30), { center });
    expect(result).toMatchObject({
      added: 30,
      removed: 0,
      firstNodes: true,
      danglingEdges: 0,
    });
    expect(layout.nodes).toHaveLength(30);
    for (const n of layout.nodes)
      expect(Number.isFinite(n.x) && Number.isFinite(n.y)).toBe(true);
    expect(layout.alpha()).toBe(1);
    expect(layout.isActive()).toBe(true);
  });

  it("skips edges with a missing endpoint instead of throwing", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(5);
    graph.edges.push(edge(99, 0, 42));
    const result = layout.sync(graph, { center });
    expect(result.danglingEdges).toBe(1);
    expect(layout.edges).toHaveLength(5);
    expect(() => layout.warmUp(20, 1000)).not.toThrow();
  });

  it("never mutates the GraphNode / GraphEdge objects it is given", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(10);
    layout.sync(graph, { center });
    layout.warmUp(30, 1000);
    expect(graph.nodes[0]).not.toHaveProperty("x");
    expect(typeof graph.edges[0].source).toBe("string");
    expect(typeof graph.edges[0].target).toBe("string");
  });

  it("keeps existing positions and objects when nodes are added, and reheats gently", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(40);
    layout.sync(graph, { center });
    layout.warmUp(400, 5000);
    const before = new Map(
      layout.nodes.map((n) => [n.id, { x: n.x, y: n.y, ref: n }]),
    );
    const alphaBefore = layout.alpha();

    const next: GraphData = {
      nodes: [...graph.nodes, node(100), node(101)],
      edges: [...graph.edges, edge(100, 5, 100), edge(101, 100, 101)],
    };
    const result = layout.sync(next, { center: { x: 9999, y: 9999 } });
    expect(result).toMatchObject({ added: 2, removed: 0, firstNodes: false });
    for (const n of layout.nodes) {
      const prev = before.get(n.id);
      if (!prev) continue;
      expect(n).toBe(prev.ref);
      expect(n.x).toBe(prev.x);
      expect(n.y).toBe(prev.y);
    }
    expect(layout.alpha()).toBeGreaterThanOrEqual(Math.min(0.3, 1));
    expect(layout.alpha()).toBeLessThanOrEqual(Math.max(alphaBefore, 0.3));

    const anchor = layout.nodes[layout.indexById.get("n:5")!];
    const added = layout.nodes[layout.indexById.get("n:100")!];
    expect(Math.hypot(added.x! - anchor.x!, added.y! - anchor.y!)).toBeLessThan(
      60,
    );
  });

  it("holds untouched nodes in place while an expansion settles", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(120, ["A", "B", "C"]);
    for (let i = 0; i < 120; i += 3)
      graph.edges.push(edge(500 + i, i, (i + 40) % 120));
    layout.sync(graph, { center });
    while (layout.isActive()) layout.tick();
    const far = layout.nodes.filter(
      (n) => !["n:9", "n:10", "n:11", "n:49", "n:89"].includes(n.id),
    );
    const before = new Map(far.map((n) => [n.id, { x: n.x!, y: n.y! }]));

    const leaves = Array.from({ length: 30 }, (_, i) => node(1000 + i, "A"));
    const result = layout.sync(
      {
        nodes: [...graph.nodes, ...leaves],
        edges: [
          ...graph.edges,
          ...leaves.map((l, i) => edge(2000 + i, 10, l.dbId)),
        ],
      },
      { center },
    );
    expect(result.added).toBe(30);
    // The expanded node stays put too; only the new nodes move freely.
    const hub = layout.nodes[layout.indexById.get("n:10")!];
    const hubBefore = { x: hub.x!, y: hub.y! };
    expect(hub.anchorX).toBe(hub.x);
    expect(layout.nodes[layout.indexById.get("n:1000")!].anchorX).toBeNull();
    while (layout.isActive()) layout.tick();

    const moves = far
      .map((n) =>
        Math.hypot(n.x! - before.get(n.id)!.x, n.y! - before.get(n.id)!.y),
      )
      .sort((a, b) => a - b);
    expect(moves[Math.floor(moves.length / 2)]).toBeLessThan(8);
    expect(Math.hypot(hub.x! - hubBefore.x, hub.y! - hubBefore.y)).toBeLessThan(
      25,
    );
    // Anchors are let go once the layout is cold.
    expect(layout.nodes.every((n) => n.anchorX === null)).toBe(true);
    for (const leaf of leaves) {
      const n = layout.nodes[layout.indexById.get(leaf.id)!];
      expect(Math.hypot(n.x! - hub.x!, n.y! - hub.y!)).toBeLessThan(150);
    }
  });

  it("frees both ends of a new edge between nodes that were already there", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(12);
    layout.sync(graph, { center });
    layout.sync(
      { nodes: graph.nodes, edges: [...graph.edges, edge(77, 2, 8)] },
      { center },
    );
    const at = (id: string) => layout.nodes[layout.indexById.get(id)!];
    expect(at("n:2").anchorX).toBeNull();
    expect(at("n:8").anchorX).toBeNull();
    expect(at("n:5").anchorX).not.toBeNull();
  });

  it("relayout frees every node", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(10);
    layout.sync(graph, { center });
    layout.sync(
      {
        nodes: [...graph.nodes, node(50)],
        edges: [...graph.edges, edge(50, 1, 50)],
      },
      { center },
    );
    expect(layout.nodes.some((n) => n.anchorX !== null)).toBe(true);
    layout.relayout();
    expect(layout.nodes.every((n) => n.anchorX === null)).toBe(true);
    expect(layout.alpha()).toBeGreaterThanOrEqual(0.8);
  });

  it("is a no-op for the same graph", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(12);
    layout.sync(graph, { center });
    layout.warmUp(500, 5000);
    const structure = layout.structureVersion;
    const alpha = layout.alpha();
    layout.sync(
      { nodes: [...graph.nodes], edges: [...graph.edges] },
      { center },
    );
    expect(layout.structureVersion).toBe(structure);
    expect(layout.alpha()).toBe(alpha);
  });

  it("drops removed nodes and returns them to their old spot when they come back", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(9);
    layout.sync(graph, { center });
    layout.warmUp(100, 5000);
    const old = layout.nodes[layout.indexById.get("n:4")!];
    const spot = { x: old.x, y: old.y };

    const without = {
      nodes: graph.nodes.filter((n) => n.id !== "n:4"),
      edges: graph.edges.filter(
        (e) => e.source !== "n:4" && e.target !== "n:4",
      ),
    };
    const result = layout.sync(without, { center });
    expect(result.removed).toBe(1);
    expect(layout.indexById.has("n:4")).toBe(false);
    expect(
      layout.edges.every(
        (e) => layout.nodes[e.source] && layout.nodes[e.target],
      ),
    ).toBe(true);

    layout.sync(graph, { center });
    const back = layout.nodes[layout.indexById.get("n:4")!];
    expect({ x: back.x, y: back.y }).toEqual(spot);
  });

  it("survives a node changing label under the same id", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(12, ["A", "B"]);
    layout.sync(graph, { center });
    layout.warmUp(20, 1000);
    const relabeled = {
      nodes: graph.nodes.map((n, i) =>
        i === 3 ? { ...n, label: "Brand new" } : n,
      ),
      edges: graph.edges,
    };
    layout.sync(relabeled, { center });
    layout.reheat(0.5);
    layout.warmUp(50, 1000);
    expect(layout.nodes[3].label).toBe("Brand new");
    for (const n of layout.nodes)
      expect(Number.isFinite(n.x) && Number.isFinite(n.y)).toBe(true);
  });

  it("keeps the first of duplicate node ids and edge ids", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(
      {
        nodes: [node(1, "A"), node(1, "B"), node(2)],
        edges: [edge(1, 1, 2), edge(1, 2, 1)],
      },
      { center },
    );
    expect(layout.nodes.map((n) => n.label)).toEqual(["A", "User"]);
    expect(layout.edges).toHaveLength(1);
  });

  it("routes parallel edges and self-loops", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(
      {
        nodes: [node(1), node(2)],
        edges: [edge(1, 1, 2), edge(2, 1, 2), edge(3, 2, 1), edge(4, 1, 1)],
      },
      { center },
    );
    const bends = layout.edges.filter((e) => e.loop < 0).map((e) => e.bend);
    expect(bends).toEqual([-1, 0, -1]);
    expect(layout.edges.find((e) => e.id === "e:4")).toMatchObject({ loop: 0 });
    expect(layout.nodes.map((n) => n.degree)).toEqual([3, 3]);
  });

  it("goes back to idle when the graph empties", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(ring(5), { center });
    layout.sync({ nodes: [], edges: [] }, { center });
    expect(layout.isActive()).toBe(false);
    const result = layout.sync(ring(3), { center });
    expect(result.firstNodes).toBe(true);
  });
});

describe("ForceLayout simulation", () => {
  it("cools down and keeps every label's nodes near the graph", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(200, ["A", "B", "C", "D", "E"]);
    // Plus some disconnected nodes, which must not drift away.
    for (let i = 0; i < 20; i++) graph.nodes.push(node(1000 + i, "Loose"));
    layout.sync(graph, { center });
    let ticks = 0;
    while (layout.isActive() && ticks < 2000) {
      layout.tick();
      ticks++;
    }
    expect(layout.isActive()).toBe(false);
    expect(ticks).toBeLessThan(400);
    const b = boundsOf(layout.nodes)!;
    expect(b.maxX - b.minX).toBeLessThan(2500);
    expect(b.maxY - b.minY).toBeLessThan(2500);
    for (const n of layout.nodes)
      expect(Number.isFinite(n.x) && Number.isFinite(n.y)).toBe(true);
  });

  it("pins and releases nodes from the orchestrator's set", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(ring(6), { center });
    const n = layout.nodes[0];
    layout.syncPins(new Set([n.id]));
    expect(n.fx).toBe(n.x);
    expect(layout.isFixed(n.id)).toBe(true);
    layout.syncPins(new Set());
    expect(n.fx).toBeNull();
    layout.setPinned(n.id, true);
    expect(layout.isFixed(n.id)).toBe(true);
    layout.setPinned(n.id, false);
    expect(layout.isFixed(n.id)).toBe(false);
  });

  it("pins new nodes that arrive already pinned", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(ring(3), { center, pinned: new Set(["n:1"]) });
    expect(layout.isFixed("n:1")).toBe(true);
    expect(layout.isFixed("n:0")).toBe(false);
  });

  it("drags a node and leaves it fixed where it was dropped", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(ring(6), { center });
    layout.warmUp(500, 5000);
    const id = layout.nodes[2].id;
    layout.startDrag(id);
    expect(layout.isActive()).toBe(true);
    layout.dragTo(id, 400, -300);
    layout.tick();
    expect(layout.nodes[2]).toMatchObject({ x: 400, y: -300 });
    layout.endDrag(id, true);
    expect(layout.draggingId).toBeNull();
    layout.tick();
    expect(layout.nodes[2]).toMatchObject({
      x: 400,
      y: -300,
      fx: 400,
      fy: -300,
    });

    const other = layout.nodes[3].id;
    layout.startDrag(other);
    layout.dragTo(other, 0, 0);
    layout.endDrag(other, false);
    expect(layout.isFixed(other)).toBe(false);
  });

  it("survives a sync that removes the node being dragged", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(4);
    layout.sync(graph, { center });
    layout.startDrag("n:1");
    layout.sync(
      { nodes: graph.nodes.filter((n) => n.id !== "n:1"), edges: [] },
      { center },
    );
    expect(layout.draggingId).toBeNull();
    expect(() => layout.dragTo("n:1", 1, 1)).not.toThrow();
    expect(() => layout.endDrag("n:1", true)).not.toThrow();
  });

  it("cools down after the node being dragged leaves the graph", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(6);
    layout.sync(graph, { center });
    layout.warmUp(500, 5000);
    layout.startDrag("n:1");
    layout.sync(
      { nodes: graph.nodes.filter((n) => n.id !== "n:1"), edges: [] },
      { center },
    );
    // The canvas still ends the gesture on pointerup.
    layout.endDrag("n:1", true);
    let ticks = 0;
    while (layout.isActive() && ticks < 2000) {
      layout.tick();
      ticks++;
    }
    expect(layout.isActive()).toBe(false);
    expect(layout.alpha()).toBeLessThan(0.008);
  });

  it("cools down even when the removed node's drag is never ended", () => {
    const layout = new ForceLayout(seeded());
    const graph = ring(6);
    layout.sync(graph, { center });
    layout.startDrag("n:1");
    layout.sync(
      { nodes: graph.nodes.filter((n) => n.id !== "n:1"), edges: [] },
      { center },
    );
    let ticks = 0;
    while (layout.isActive() && ticks < 2000) {
      layout.tick();
      ticks++;
    }
    expect(layout.isActive()).toBe(false);
  });

  it("ending a stale drag leaves a drag on another node running", () => {
    const layout = new ForceLayout(seeded());
    layout.sync(ring(6), { center });
    layout.warmUp(500, 5000);
    layout.startDrag("n:2");
    layout.endDrag("n:1", true);
    expect(layout.draggingId).toBe("n:2");
    for (let i = 0; i < 400; i++) layout.tick();
    // The drag keeps the layout warm until it ends.
    expect(layout.isActive()).toBe(true);
    expect(layout.alpha()).toBeGreaterThan(0.2);
    layout.endDrag("n:2", true);
    while (layout.isActive()) layout.tick();
    expect(layout.alpha()).toBeLessThan(0.008);
  });
});
