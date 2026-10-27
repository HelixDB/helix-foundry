import { describe, expect, it, vi } from "vitest";

import {
  centerOn,
  clampScale,
  createPanZoom,
  FIT_MAX_SCALE,
  fitTransform,
  interpolateTransform,
  MAX_SCALE,
  MIN_SCALE,
  screenToWorld,
  worldToScreen,
  zoomAround,
} from "@/web/areas/explore/explorer/canvas/usePanZoom";

const viewport = { width: 800, height: 600 };

describe("view math", () => {
  it("round-trips screen and world coordinates", () => {
    const v = { tx: 120, ty: -40, scale: 2.5 };
    const w = screenToWorld(v, 300, 200);
    expect(worldToScreen(v, w.x, w.y)).toEqual({ x: 300, y: 200 });
  });

  it("clamps scale, including non-finite values", () => {
    expect(clampScale(100)).toBe(MAX_SCALE);
    expect(clampScale(0)).toBe(MIN_SCALE);
    expect(clampScale(Infinity)).toBe(1);
    expect(clampScale(NaN)).toBe(1);
  });

  it("zooms around a fixed screen point", () => {
    const v = { tx: 10, ty: 20, scale: 1 };
    const before = screenToWorld(v, 400, 300);
    const next = zoomAround(v, 400, 300, 2);
    expect(next.scale).toBe(2);
    const after = screenToWorld(next, 400, 300);
    expect(after.x).toBeCloseTo(before.x);
    expect(after.y).toBeCloseTo(before.y);
  });

  it("centers a world point", () => {
    const v = centerOn({ x: 50, y: -20 }, viewport, 2);
    expect(worldToScreen(v, 50, -20)).toEqual({ x: 400, y: 300 });
  });
});

describe("fitTransform", () => {
  it("fits bounds inside the padded viewport", () => {
    const v = fitTransform(
      { minX: -500, minY: -100, maxX: 500, maxY: 100 },
      viewport,
      { padding: 50 },
    )!;
    expect(v.scale).toBeCloseTo(0.7);
    expect(worldToScreen(v, 0, 0)).toEqual({ x: 400, y: 300 });
    expect(worldToScreen(v, -500, 0).x).toBeCloseTo(50);
  });

  it("handles a single point by centering at the max fit scale", () => {
    const v = fitTransform(
      { minX: 10, minY: 10, maxX: 10, maxY: 10 },
      viewport,
    )!;
    expect(v.scale).toBe(FIT_MAX_SCALE);
    expect(worldToScreen(v, 10, 10)).toEqual({ x: 400, y: 300 });
  });

  it("handles nodes on a line using the axis with extent", () => {
    const v = fitTransform(
      { minX: 0, minY: 5, maxX: 2000, maxY: 5 },
      viewport,
      { padding: 50 },
    )!;
    expect(v.scale).toBeCloseTo(700 / 2000);
    expect(Number.isFinite(v.tx) && Number.isFinite(v.ty)).toBe(true);
  });

  it("returns null without a viewport", () => {
    expect(
      fitTransform(
        { minX: 0, minY: 0, maxX: 1, maxY: 1 },
        { width: 0, height: 0 },
      ),
    ).toBeNull();
  });
});

describe("interpolateTransform", () => {
  it("hits both ends and zooms geometrically", () => {
    const a = centerOn({ x: 0, y: 0 }, viewport, 1);
    const b = centerOn({ x: 100, y: 0 }, viewport, 4);
    expect(interpolateTransform(a, b, 0, viewport)).toEqual(a);
    expect(interpolateTransform(a, b, 1, viewport)).toEqual(b);
    const mid = interpolateTransform(a, b, 0.5, viewport);
    expect(mid.scale).toBeCloseTo(2);
    expect(screenToWorld(mid, 400, 300).x).toBeCloseTo(50);
  });
});

describe("createPanZoom", () => {
  it("notifies and bumps the version on changes", () => {
    const notify = vi.fn();
    const pz = createPanZoom(notify);
    pz.panBy(5, 6);
    pz.zoomAt(0, 0, 2);
    expect(pz.transformRef.current).toEqual({ tx: 10, ty: 12, scale: 2 });
    expect(pz.version()).toBe(2);
    expect(notify).toHaveBeenCalledTimes(2);
  });

  it("animates over frames and a new animation replaces the old one", () => {
    const pz = createPanZoom(() => {});
    pz.animateTo({ tx: 100, ty: 0, scale: 1 }, { viewport, duration: 100 });
    expect(pz.isAnimating()).toBe(true);
    expect(pz.step(1000)).toBe(true);
    expect(pz.step(1050)).toBe(true);
    pz.animateTo({ tx: -100, ty: 0, scale: 1 }, { viewport, duration: 100 });
    expect(pz.destination().tx).toBe(-100);
    pz.step(2000);
    expect(pz.step(2100)).toBe(false);
    expect(pz.transformRef.current.tx).toBeCloseTo(-100);
    expect(pz.isAnimating()).toBe(false);
  });

  it("user input cancels a running animation", () => {
    const pz = createPanZoom(() => {});
    pz.animateTo({ tx: 100, ty: 0, scale: 1 }, { viewport, duration: 100 });
    pz.panBy(1, 1);
    expect(pz.isAnimating()).toBe(false);
    expect(pz.step(0)).toBe(false);
  });

  it("jumps straight there when animation is off", () => {
    const pz = createPanZoom(() => {});
    pz.animateTo({ tx: 7, ty: 8, scale: 3 }, { viewport, animate: false });
    expect(pz.isAnimating()).toBe(false);
    expect(pz.transformRef.current).toEqual({ tx: 7, ty: 8, scale: 3 });
  });
});
