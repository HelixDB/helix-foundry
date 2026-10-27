import { describe, expect, it } from "vitest";

import {
  arrowTip,
  assignEdgeRoutes,
  curveControl,
  curveDistance2,
  emptyLoop,
  loopGeometry,
  segmentDistance2,
} from "@/web/areas/explore/explorer/canvas/edgeGeometry";

const e = (id: string, source: string, target: string) => ({
  id,
  source,
  target,
});

describe("assignEdgeRoutes", () => {
  it("keeps a lone edge straight", () => {
    expect(assignEdgeRoutes([e("e:1", "n:1", "n:2")])).toEqual([
      { bend: 0, loop: -1 },
    ]);
  });

  it("spreads parallel edges symmetrically, the middle one straight", () => {
    const routes = assignEdgeRoutes([
      e("e:1", "n:1", "n:2"),
      e("e:2", "n:1", "n:2"),
      e("e:3", "n:1", "n:2"),
    ]);
    expect(routes.map((r) => r.bend)).toEqual([-1, 0, 1]);
  });

  it("bends opposite directions onto opposite sides", () => {
    const [ab, ba] = assignEdgeRoutes([
      e("e:1", "n:1", "n:2"),
      e("e:2", "n:2", "n:1"),
    ]);
    // Same sign relative to each edge's own direction = opposite sides on screen.
    expect(ab.bend).toBe(-0.5);
    expect(ba.bend).toBe(-0.5);
    const c1 = curveControl(0, 0, 100, 0, ab.bend, { x: 0, y: 0 });
    const c2 = curveControl(100, 0, 0, 0, ba.bend, { x: 0, y: 0 });
    expect(Math.sign(c1.y)).toBe(-Math.sign(c2.y));
  });

  it("is stable regardless of input order", () => {
    const edges = [
      e("e:3", "n:1", "n:2"),
      e("e:1", "n:1", "n:2"),
      e("e:2", "n:2", "n:1"),
    ];
    const byId = (list: ReturnType<typeof e>[]) => {
      const routes = assignEdgeRoutes(list);
      return Object.fromEntries(
        list.map((edge, i) => [edge.id, routes[i].bend]),
      );
    };
    expect(byId(edges)).toEqual(byId([...edges].reverse()));
  });

  it("stacks self-loops", () => {
    const routes = assignEdgeRoutes([
      e("e:2", "n:1", "n:1"),
      e("e:1", "n:1", "n:1"),
      e("e:3", "n:1", "n:2"),
    ]);
    expect(routes).toEqual([
      { bend: 0, loop: 1 },
      { bend: 0, loop: 0 },
      { bend: 0, loop: -1 },
    ]);
  });
});

describe("curves and loops", () => {
  it("puts the control point on the midpoint for straight edges", () => {
    expect(curveControl(0, 0, 10, 10, 0, { x: 0, y: 0 })).toEqual({
      x: 5,
      y: 5,
    });
    expect(curveControl(3, 3, 3, 3, 1, { x: 0, y: 0 })).toEqual({ x: 3, y: 3 });
  });

  it("draws loops that start and end on the node's rim", () => {
    const loop = loopGeometry(100, 100, 6, 0, emptyLoop());
    expect(Math.hypot(loop.cx - 100, loop.cy - 100)).toBeCloseTo(loop.radius);
    expect(Math.hypot(loop.tipX - 100, loop.tipY - 100)).toBeCloseTo(7);
    expect(Math.hypot(loop.tipX - loop.cx, loop.tipY - loop.cy)).toBeCloseTo(
      loop.radius,
    );
    // The arrow points into the node.
    const ahead = {
      x: loop.tipX + Math.cos(loop.angle),
      y: loop.tipY + Math.sin(loop.angle),
    };
    expect(Math.hypot(ahead.x - 100, ahead.y - 100)).toBeLessThan(7);
    expect(loopGeometry(100, 100, 6, 1, emptyLoop()).radius).toBeGreaterThan(
      loop.radius,
    );
  });

  it("places arrow tips on the target rim", () => {
    const tip = { x: 0, y: 0, angle: 0 };
    expect(arrowTip(0, 0, 100, 0, 6, tip)).toBe(true);
    expect(tip.x).toBeCloseTo(93);
    expect(tip.angle).toBeCloseTo(0);
    expect(arrowTip(5, 5, 5, 5, 6, tip)).toBe(false);
  });

  it("measures distance to segments and curves", () => {
    expect(segmentDistance2(5, 3, 0, 0, 10, 0)).toBeCloseTo(9);
    expect(segmentDistance2(-3, 4, 0, 0, 10, 0)).toBeCloseTo(25);
    // Apex of the curve (0,0) → control (50,40) → (100,0) is (50, 20).
    expect(
      Math.sqrt(curveDistance2(50, 20, 0, 0, 50, 40, 100, 0)),
    ).toBeLessThan(0.5);
    expect(
      Math.sqrt(curveDistance2(50, 0, 0, 0, 50, 40, 100, 0)),
    ).toBeGreaterThan(15);
  });
});
