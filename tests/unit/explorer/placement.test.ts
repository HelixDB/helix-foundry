import { describe, expect, it } from "vitest";

import {
  boundsOf,
  placeNewNodes,
  type XY,
} from "@/web/areas/explore/explorer/canvas/placement";

function seeded(seed = 42) {
  return () => {
    seed = (seed * 16807) % 2147483647;
    return (seed - 1) / 2147483646;
  };
}

function adjacency(pairs: [string, string][]) {
  const map = new Map<string, string[]>();
  for (const [a, b] of pairs) {
    map.set(a, [...(map.get(a) ?? []), b]);
    map.set(b, [...(map.get(b) ?? []), a]);
  }
  return map;
}

const dist = (a: XY, b: XY) => Math.hypot(a.x - b.x, a.y - b.y);

describe("placeNewNodes", () => {
  it("places a new node next to its placed neighbor", () => {
    const placed = new Map([["hub", { x: 500, y: 500 }]]);
    const result = placeNewNodes({
      ids: ["leaf"],
      placed,
      neighbors: adjacency([["hub", "leaf"]]),
      labelOf: () => "A",
      center: { x: 0, y: 0 },
      random: seeded(),
    });
    expect(dist(result.get("leaf")!, { x: 500, y: 500 })).toBeLessThan(40);
  });

  it("spreads a hub's many new neighbors out instead of piling them up", () => {
    const placed = new Map([["hub", { x: 0, y: 0 }]]);
    const ids = Array.from({ length: 200 }, (_, i) => `leaf${i}`);
    const result = placeNewNodes({
      ids,
      placed,
      neighbors: adjacency(ids.map((id) => ["hub", id] as [string, string])),
      labelOf: () => "A",
      center: { x: 0, y: 0 },
      random: seeded(),
    });
    const points = [...result.values()];
    let closest = Infinity;
    for (let i = 0; i < points.length; i++) {
      expect(dist(points[i], { x: 0, y: 0 })).toBeGreaterThan(20);
      for (let j = i + 1; j < points.length; j++)
        closest = Math.min(closest, dist(points[i], points[j]));
    }
    expect(closest).toBeGreaterThan(4);
    expect(
      Math.max(...points.map((p) => dist(p, { x: 0, y: 0 }))),
    ).toBeLessThan(160);
  });

  it("puts a node linked to two placed nodes between them", () => {
    const placed = new Map([
      ["a", { x: 0, y: 0 }],
      ["b", { x: 200, y: 0 }],
    ]);
    const result = placeNewNodes({
      ids: ["mid"],
      placed,
      neighbors: adjacency([
        ["a", "mid"],
        ["b", "mid"],
      ]),
      labelOf: () => "A",
      center: { x: 999, y: 999 },
      random: seeded(),
    });
    expect(dist(result.get("mid")!, { x: 100, y: 0 })).toBeLessThan(15);
  });

  it("grows chains of new nodes outward from the placed graph", () => {
    const placed = new Map([["root", { x: 0, y: 0 }]]);
    const result = placeNewNodes({
      ids: ["c", "b", "a"],
      placed,
      neighbors: adjacency([
        ["root", "a"],
        ["a", "b"],
        ["b", "c"],
      ]),
      labelOf: () => "A",
      center: { x: 5000, y: 5000 },
      random: seeded(),
    });
    expect(result.size).toBe(3);
    for (const p of result.values())
      expect(dist(p, { x: 0, y: 0 })).toBeLessThan(120);
    expect(dist(result.get("a")!, result.get("b")!)).toBeLessThan(40);
  });

  it("puts disconnected nodes near their label, else around the center", () => {
    const placed = new Map([["u1", { x: -800, y: 0 }]]);
    const labels: Record<string, string> = {
      u1: "User",
      u2: "User",
      o1: "Order",
    };
    const result = placeNewNodes({
      ids: ["u2", "o1"],
      placed,
      neighbors: new Map(),
      labelOf: (id) => labels[id],
      center: { x: 300, y: 300 },
      random: seeded(),
    });
    expect(dist(result.get("u2")!, { x: -800, y: 0 })).toBeLessThan(30);
    expect(dist(result.get("o1")!, { x: 300, y: 300 })).toBeLessThan(5);
  });

  it("places a whole first graph around the center without stacking nodes", () => {
    const ids = Array.from({ length: 300 }, (_, i) => `n${i}`);
    const pairs: [string, string][] = ids
      .slice(1)
      .map((id, i) => [ids[Math.floor(i / 3)], id]);
    const result = placeNewNodes({
      ids,
      placed: new Map(),
      neighbors: adjacency(pairs),
      labelOf: () => "A",
      center: { x: 0, y: 0 },
      random: seeded(),
    });
    expect(result.size).toBe(300);
    const keys = new Set(
      [...result.values()].map((p) => `${p.x.toFixed(3)},${p.y.toFixed(3)}`),
    );
    expect(keys.size).toBe(300);
    const b = boundsOf(result.values())!;
    expect(Math.max(b.maxX - b.minX, b.maxY - b.minY)).toBeLessThan(2000);
  });
});

describe("boundsOf", () => {
  it("skips unplaced points and returns null for none", () => {
    expect(
      boundsOf([{ x: 1, y: 2 }, {}, { x: -3, y: 5 }, { x: NaN, y: 0 }]),
    ).toEqual({
      minX: -3,
      minY: 2,
      maxX: 1,
      maxY: 5,
    });
    expect(boundsOf([{}])).toBeNull();
  });
});
