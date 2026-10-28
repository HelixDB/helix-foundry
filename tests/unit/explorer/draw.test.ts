import { describe, expect, it } from "vitest";

import { CANVAS_THEME } from "@/web/areas/explore/explorer/lib/theme";
import {
  drawScene,
  FrameScratch,
  type SceneInput,
} from "@/web/areas/explore/explorer/canvas/draw";
import { EdgeHitBuffer } from "@/web/areas/explore/explorer/canvas/hitTest";
import type { SimNode } from "@/web/areas/explore/explorer/canvas/useGraphSimulation";

interface Op {
  op: "fill" | "stroke";
  style: unknown;
  /** The rects and arcs of the path that was filled or stroked, by centre. */
  shapes: { kind: "rect" | "arc"; x: number; y: number }[];
}

/** A 2D context that records every fill and stroke with its path. */
function recordingContext() {
  const ops: Op[] = [];
  let shapes: Op["shapes"] = [];
  const state: Record<string | symbol, unknown> = {};
  const methods: Record<string, (...args: never[]) => unknown> = {
    beginPath: () => {
      shapes = [];
    },
    rect: (x: number, y: number, w: number, h: number) => {
      shapes.push({ kind: "rect", x: x + w / 2, y: y + h / 2 });
    },
    arc: (x: number, y: number) => {
      shapes.push({ kind: "arc", x, y });
    },
    fill: () => {
      ops.push({ op: "fill", style: state.fillStyle, shapes: [...shapes] });
    },
    stroke: () => {
      ops.push({ op: "stroke", style: state.strokeStyle, shapes: [...shapes] });
    },
    measureText: (text: string) => ({ width: text.length * 6 }),
  };
  const ctx = new Proxy(state, {
    get(target, key) {
      if (key in target) return target[key];
      if (typeof key === "string" && key in methods) return methods[key];
      return () => {};
    },
    set(target, key, value) {
      target[key] = value;
      return true;
    },
  }) as unknown as CanvasRenderingContext2D;
  return { ctx, ops };
}

const simNode = (i: number, x: number, y: number): SimNode => ({
  id: `n:${i}`,
  label: "User",
  group: 0,
  degree: 0,
  anchorX: null,
  anchorY: null,
  x,
  y,
});

function scene(overrides: Partial<SceneInput>): SceneInput {
  const nodes = [simNode(0, 0, 0), simNode(1, 60, 0)];
  return {
    layout: {
      nodes,
      edges: [],
      indexById: new Map(nodes.map((n, i) => [n.id, i])),
    },
    palette: ["#7ECBFF"],
    colorIds: [0, 0],
    sizes: [1, 1],
    captionAt: () => "",
    view: { tx: 100, ty: 100, scale: 1 },
    width: 400,
    height: 300,
    dpr: 1,
    selectedNode: -1,
    selectedEdge: -1,
    hoveredNode: -1,
    hoveredEdge: -1,
    emphasis: null,
    pinned: new Set(["n:0", "n:1"]),
    outside: new Set(),
    fontFamily: "sans-serif",
    measure: new Map(),
    hits: new EdgeHitBuffer(),
    scratch: new FrameScratch(),
    ...overrides,
  };
}

/** The pin notch fills, with the screen x of the nodes whose notch each one draws. */
function notchFills(ops: Op[]) {
  return ops
    .map((op, index) => ({ op, index }))
    .filter(({ op }) => op.op === "fill" && op.style === CANVAS_THEME.pinned);
}

/** The notch sits on its node's upper right corner: which node (by screen x 100 or 160) a rect belongs to. */
function nodeOf(rectX: number): number {
  return rectX < 130 ? 0 : 1;
}

describe("drawScene pin notches", () => {
  it("paints the selected node's notch over its selection ring", () => {
    const { ctx, ops } = recordingContext();
    drawScene(ctx, scene({ selectedNode: 0 }));
    const ring = ops.findIndex(
      (op) => op.op === "stroke" && op.style === CANVAS_THEME.selection,
    );
    expect(ring).toBeGreaterThanOrEqual(0);
    const fills = notchFills(ops);
    const selected = fills.find(({ op }) =>
      op.shapes.some((s) => nodeOf(s.x) === 0),
    );
    const other = fills.find(({ op }) =>
      op.shapes.some((s) => nodeOf(s.x) === 1),
    );
    expect(selected && selected.index).toBeGreaterThan(ring);
    // The plain node's notch goes down with the plain nodes, before the highlights.
    expect(other && other.index).toBeLessThan(ring);
    // Each notch is drawn once.
    expect(fills.flatMap(({ op }) => op.shapes)).toHaveLength(2);
  });

  it("paints the hovered node's notch over its hover ring and enlarged disc", () => {
    const { ctx, ops } = recordingContext();
    drawScene(ctx, scene({ hoveredNode: 1 }));
    const hoverFill = ops.findIndex(
      (op) =>
        op.op === "fill" &&
        op.style === "#7ECBFF" &&
        op.shapes.length === 1 &&
        op.shapes[0].x === 160,
    );
    expect(hoverFill).toBeGreaterThanOrEqual(0);
    const fills = notchFills(ops);
    const hovered = fills.find(({ op }) =>
      op.shapes.some((s) => nodeOf(s.x) === 1),
    );
    expect(hovered && hovered.index).toBeGreaterThan(hoverFill);
    expect(fills.flatMap(({ op }) => op.shapes)).toHaveLength(2);
  });

  it("draws no notch for unpinned nodes", () => {
    const { ctx, ops } = recordingContext();
    drawScene(ctx, scene({ pinned: new Set(), selectedNode: 0 }));
    expect(notchFills(ops)).toHaveLength(0);
  });
});

describe("drawScene nodes outside the load", () => {
  it("rings each with a dashed stroke, and nothing else", () => {
    const dashes: number[][] = [];
    const { ctx, ops } = recordingContext();
    (ctx as unknown as { setLineDash: (d: number[]) => void }).setLineDash = (
      d: number[],
    ) => dashes.push(d);
    drawScene(ctx, scene({ pinned: new Set(), outside: new Set(["n:1"]) }));
    expect(dashes).toContainEqual([3, 2]);
    const rings = (list: Op[]) =>
      list.filter(
        (op) =>
          op.op === "stroke" &&
          op.shapes.length === 1 &&
          op.shapes[0]!.kind === "rect" &&
          op.shapes[0]!.x === 160,
      );
    expect(rings(ops)).toHaveLength(1);
    const plain = recordingContext();
    drawScene(plain.ctx, scene({ pinned: new Set(), outside: new Set() }));
    expect(rings(plain.ops)).toHaveLength(0);
  });
});
