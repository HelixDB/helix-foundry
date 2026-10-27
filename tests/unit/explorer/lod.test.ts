import { describe, expect, it } from "vitest";

import {
  degreeScale,
  GRID_SPACING,
  MAX_DEGREE_SCALE,
  gridSpacing,
  nodeHitRadiusPx,
  nodeRadiusPx,
  placeCaptions,
  showArrows,
  showCaptions,
  truncateCaption,
} from "@/web/areas/explore/explorer/canvas/lod";

describe("level of detail", () => {
  it("sizes nodes at 6px at scale 1 and clamps the extremes", () => {
    expect(nodeRadiusPx(1)).toBe(6);
    expect(nodeRadiusPx(0.01)).toBe(2);
    expect(nodeRadiusPx(8)).toBe(10);
    expect(nodeRadiusPx(0.25)).toBe(3);
  });

  it("grows hubs with the log of their degree, capped", () => {
    expect(degreeScale(0)).toBe(1);
    expect(degreeScale(8)).toBe(1);
    expect(degreeScale(16)).toBeCloseTo(1.25);
    expect(degreeScale(64)).toBeCloseTo(1.75);
    expect(degreeScale(10_000)).toBe(MAX_DEGREE_SCALE);
  });

  it("draws arrows only when zoomed in and not too dense", () => {
    expect(showArrows(0.6, 100)).toBe(true);
    expect(showArrows(0.5, 100)).toBe(false);
    expect(showArrows(2, 10_000)).toBe(false);
  });

  it("draws captions only when zoomed in on a moderate number of nodes", () => {
    expect(showCaptions(1.1, 400)).toBe(true);
    expect(showCaptions(1, 100)).toBe(false);
    expect(showCaptions(3, 401)).toBe(false);
    // A few nodes get captions from further out.
    expect(showCaptions(0.6, 12)).toBe(true);
    expect(showCaptions(0.3, 12)).toBe(false);
  });

  it("coarsens the grid so dots never crowd", () => {
    expect(gridSpacing(1)).toBe(GRID_SPACING);
    expect(gridSpacing(0.5)).toBe(GRID_SPACING * 2);
    expect(gridSpacing(0.05) * 0.05).toBeGreaterThanOrEqual(18);
  });

  it("gives touch a larger hit target", () => {
    expect(nodeHitRadiusPx(6, "touch")).toBeGreaterThan(
      nodeHitRadiusPx(6, "mouse"),
    );
    expect(nodeHitRadiusPx(2, "mouse")).toBe(9);
  });

  it("truncates and collapses whitespace in captions", () => {
    expect(truncateCaption("  hello \n world ")).toBe("hello world");
    const long = truncateCaption("x".repeat(100), 10);
    expect(long).toHaveLength(10);
    expect(long.endsWith("…")).toBe(true);
  });
});

describe("placeCaptions", () => {
  it("drops captions that overlap a higher-priority one", () => {
    const placed = placeCaptions([
      { x: 0, y: 0, w: 50, h: 14 },
      { x: 30, y: 5, w: 50, h: 14 },
      { x: 100, y: 0, w: 50, h: 14 },
      { x: -200, y: -200, w: 10, h: 10 },
    ]);
    expect(placed).toEqual([true, false, true, true]);
  });

  it("checks boxes that span several grid cells", () => {
    const placed = placeCaptions([
      { x: 0, y: 0, w: 300, h: 14 },
      { x: 250, y: 10, w: 20, h: 14 },
    ]);
    expect(placed).toEqual([true, false]);
  });
});
