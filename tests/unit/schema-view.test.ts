import { describe, expect, it } from "vitest";
import {
  PAD,
  ZOOM,
  clamp,
  fit,
  reveal,
  zoomAt,
} from "@/web/areas/ontology/schema-view";

const frame = { w: 800, h: 500 };

describe("fit", () => {
  it("shrinks a large schema to the frame, centred", () => {
    const v = fit({ w: 2000, h: 1000 }, frame);
    expect(v.k).toBeCloseTo((800 - 2 * PAD) / 2000);
    expect(v.x).toBeCloseTo(PAD);
    expect(v.y).toBeCloseTo((500 - 1000 * v.k) / 2);
  });
  it("never blows a small schema up", () => {
    expect(fit({ w: 300, h: 200 }, frame)).toEqual({ x: 250, y: 150, k: 1 });
  });
  it("stops at the least zoom, the schema's start in view", () => {
    const v = fit({ w: 100_000, h: 400 }, frame);
    expect(v).toEqual({ x: PAD, y: (500 - 400 * ZOOM.min) / 2, k: ZOOM.min });
  });
  it("fits by the tighter side", () => {
    expect(fit({ w: 400, h: 2000 }, frame).k).toBeCloseTo(
      (500 - 2 * PAD) / 2000,
    );
  });
});

describe("clamp", () => {
  const content = { w: 2000, h: 1000 };
  it("keeps an overflowing schema's edges no further in than PAD", () => {
    expect(clamp({ x: 300, y: 300, k: 1 }, content, frame)).toEqual({
      x: PAD,
      y: PAD,
      k: 1,
    });
    expect(clamp({ x: -5000, y: -5000, k: 1 }, content, frame)).toEqual({
      x: 800 - 2000 - PAD,
      y: 500 - 1000 - PAD,
      k: 1,
    });
  });
  it("leaves a pan within bounds alone", () => {
    const v = { x: -100, y: -50, k: 1 };
    expect(clamp(v, content, frame)).toEqual(v);
  });
  it("centres an axis the schema fits", () => {
    expect(clamp({ x: -100, y: -100, k: 0.2 }, content, frame)).toEqual({
      x: (800 - 400) / 2,
      y: (500 - 200) / 2,
      k: 0.2,
    });
  });
});

describe("zoomAt", () => {
  it("keeps the point under the pointer", () => {
    const v = { x: 40, y: 20, k: 0.5 },
      at: [number, number] = [300, 200],
      next = zoomAt(v, 1, at);
    // The schema point under `at` before and after.
    const before = [(at[0] - v.x) / v.k, (at[1] - v.y) / v.k],
      after = [(at[0] - next.x) / next.k, (at[1] - next.y) / next.k];
    expect(after[0]).toBeCloseTo(before[0]);
    expect(after[1]).toBeCloseTo(before[1]);
  });
  it("holds the scale within ZOOM", () => {
    expect(zoomAt({ x: 0, y: 0, k: 1 }, 50, [0, 0]).k).toBe(ZOOM.max);
    expect(zoomAt({ x: 0, y: 0, k: 1 }, 0.001, [0, 0]).k).toBe(ZOOM.min);
  });
});

describe("reveal", () => {
  const v = { x: 0, y: 0, k: 1 };
  it("leaves a box already in view alone", () => {
    expect(reveal(v, { x: 100, y: 100, w: 200, h: 100 }, frame)).toEqual(v);
  });
  it("moves a box past the far edge in, by as little as it can", () => {
    expect(reveal(v, { x: 700, y: 450, w: 200, h: 100 }, frame)).toEqual({
      x: 800 - PAD - 900,
      y: 500 - PAD - 550,
      k: 1,
    });
  });
  it("moves a box before the near edge in", () => {
    expect(
      reveal({ x: -500, y: 0, k: 0.5 }, { x: 100, y: 100, w: 100, h: 100 }, frame),
    ).toEqual({ x: PAD - 50, y: 0, k: 0.5 });
  });
  it("shows the start of a box too large to show whole", () => {
    expect(reveal(v, { x: 300, y: 0, w: 2000, h: 100 }, frame).x).toBe(
      PAD - 300,
    );
  });
});
