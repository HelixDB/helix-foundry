import { describe, expect, it } from "vitest";

import { placeTooltip } from "@/web/areas/explore/explorer/canvas/tooltip";

const viewport = { width: 800, height: 600 };
const size = { w: 120, h: 40 };

describe("placeTooltip", () => {
  it("sits above a node, clear of its radius", () => {
    expect(
      placeTooltip({
        pointer: { x: 0, y: 0 },
        node: { x: 400, y: 300, radius: 6 },
        viewport,
        size,
      }),
    ).toEqual({
      visible: true,
      left: 340,
      top: 300 - 16 - 40,
    });
  });

  it("flips below the anchor near the top and stays inside the canvas", () => {
    const place = placeTooltip({
      pointer: { x: 0, y: 0 },
      node: { x: 5, y: 10, radius: 6 },
      viewport,
      size,
    });
    expect(place).toEqual({ visible: true, left: 8, top: 10 + 28 });
    const corner = placeTooltip({
      pointer: { x: 800, y: 600 },
      node: null,
      viewport,
      size,
    });
    expect(corner).toEqual({
      visible: true,
      left: 800 - 120 - 8,
      top: 600 - 14 - 40,
    });
  });

  it("hides while the hovered node's center is off the canvas", () => {
    expect(
      placeTooltip({
        pointer: { x: 3, y: 50 },
        node: { x: -2, y: 50, radius: 6 },
        viewport,
        size,
      }).visible,
    ).toBe(false);
  });

  it("is visible for an edge (pointer anchor) even right after an off-canvas node hid it", () => {
    // The caller writes `visible` on every placement, so a tooltip hidden for
    // an off-canvas node comes back for the next edge.
    placeTooltip({
      pointer: { x: 3, y: 50 },
      node: { x: -2, y: 50, radius: 6 },
      viewport,
      size,
    });
    expect(
      placeTooltip({ pointer: { x: 200, y: 200 }, node: null, viewport, size }),
    ).toEqual({
      visible: true,
      left: 140,
      top: 200 - 14 - 40,
    });
  });
});
