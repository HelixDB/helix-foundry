import { describe, expect, it } from "vitest";

import {
  EdgeHitBuffer,
  nearestInDirection,
  nearestTo,
  SpatialGrid,
} from "@/web/areas/explore/explorer/canvas/hitTest";

describe("SpatialGrid", () => {
  const nodes = [
    { id: "a", x: 0, y: 0 },
    { id: "b", x: 100, y: 0 },
    { id: "c", x: 47, y: 2 },
    { id: "d", x: -1000, y: 5000 },
    { id: "e" },
  ];

  it("finds the nearest node within range across cell borders", () => {
    const grid = new SpatialGrid<(typeof nodes)[number]>(32);
    grid.rebuild(nodes);
    expect(grid.nearest(50, 0, 10)?.id).toBe("c");
    expect(grid.nearest(3, 1, 10)?.id).toBe("a");
    expect(grid.nearest(-1001, 4999, 5)?.id).toBe("d");
    expect(grid.nearest(300, 300, 20)).toBeNull();
  });

  it("matches a linear scan on random points", () => {
    let seed = 1;
    const random = () =>
      ((seed = (seed * 16807) % 2147483647) - 1) / 2147483646;
    const points = Array.from({ length: 500 }, (_, i) => ({
      id: i,
      x: random() * 2000 - 1000,
      y: random() * 2000 - 1000,
    }));
    const grid = new SpatialGrid<(typeof points)[number]>(48);
    grid.rebuild(points);
    for (let k = 0; k < 200; k++) {
      const x = random() * 2000 - 1000;
      const y = random() * 2000 - 1000;
      let best: number | null = null;
      let bestD = 60 * 60;
      for (const p of points) {
        const d = (p.x - x) ** 2 + (p.y - y) ** 2;
        if (d < bestD) {
          bestD = d;
          best = p.id;
        }
      }
      expect(grid.nearest(x, y, 60)?.id ?? null).toBe(best);
    }
  });

  it("lets bigger items reach further and ranks by distance relative to reach", () => {
    const items = [
      { id: "hub", x: 0, y: 0, r: 20 },
      { id: "leaf", x: 30, y: 0, r: 8 },
    ];
    const grid = new SpatialGrid<(typeof items)[number]>(16);
    grid.rebuild(items);
    const reach = (item: (typeof items)[number]) => item.r;
    expect(grid.nearest(15, 0, 40, reach)?.id).toBe("hub");
    // Closer to the hub's center, but well inside the leaf's reach relative to its size.
    expect(grid.nearest(26, 0, 40, reach)?.id).toBe("leaf");
    expect(grid.nearest(0, 25, 40, reach)).toBeNull();
  });

  it("reads live positions after nodes move", () => {
    const moving = [{ x: 0, y: 0 }];
    const grid = new SpatialGrid<(typeof moving)[number]>(48);
    grid.rebuild(moving);
    moving[0].x = 5;
    expect(grid.nearest(5, 0, 1)).toBe(moving[0]);
  });
});

describe("EdgeHitBuffer", () => {
  it("hits lines, curves and loops by their drawn shape", () => {
    const hits = new EdgeHitBuffer();
    hits.reset(1);
    hits.pushLine(0, 0, 0, 100, 0);
    hits.pushCurve(1, 0, 100, 50, 140, 100, 100);
    hits.pushLoop(2, 300, 300, 10);
    expect(hits.count).toBe(3);
    expect(hits.nearest(50, 3, 5)).toBe(0);
    expect(hits.nearest(50, 120, 5)).toBe(1);
    expect(hits.nearest(50, 100, 5)).toBe(-1);
    expect(hits.nearest(310, 300, 5)).toBe(2);
    expect(hits.nearest(300, 300, 5)).toBe(-1);
    expect(hits.edgeAt(1)).toBe(1);
    expect(hits.coordAt(2, 2)).toBe(10);
  });

  it("prefers the edge drawn last on ties and resets between frames", () => {
    const hits = new EdgeHitBuffer();
    hits.reset(2);
    hits.pushLine(4, 0, 0, 10, 0);
    hits.pushLine(9, 0, 0, 10, 0);
    expect(hits.nearest(5, 1, 3)).toBe(9);
    hits.reset(2);
    expect(hits.nearest(5, 1, 3)).toBe(-1);
  });
});

describe("keyboard navigation", () => {
  const nodes = [
    { x: 0, y: 0 },
    { x: 100, y: 0 },
    { x: 60, y: 50 },
    { x: 0, y: -80 },
    { x: -200, y: 10 },
  ];

  it("moves to the nearest node in the arrow's direction", () => {
    // In line beats closer but off to the side...
    expect(nearestInDirection(nodes, { x: 0, y: 0 }, 1, 0, 0)).toBe(1);
    // ...unless the side node is much closer.
    expect(
      nearestInDirection([...nodes, { x: 30, y: 10 }], { x: 0, y: 0 }, 1, 0, 0),
    ).toBe(5);
    expect(nearestInDirection(nodes, { x: 0, y: 0 }, 0, -1, 0)).toBe(3);
    expect(nearestInDirection(nodes, { x: 0, y: 0 }, -1, 0, 0)).toBe(4);
  });

  it("ignores nodes outside the cone and returns -1 when nothing is there", () => {
    expect(nearestInDirection(nodes, { x: 100, y: 0 }, 1, 0, 1)).toBe(-1);
    expect(nearestInDirection([{ x: 10, y: 100 }], { x: 0, y: 0 }, 1, 0)).toBe(
      -1,
    );
  });

  it("finds the node closest to a point", () => {
    expect(nearestTo(nodes, { x: 90, y: 5 })).toBe(1);
    expect(nearestTo([], { x: 0, y: 0 })).toBe(-1);
  });
});
