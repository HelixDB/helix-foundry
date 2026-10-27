// Hit testing for the explorer canvas. Nodes are found through a uniform grid
// in world space; edges are tested against the exact shapes the last frame
// drew, in screen space.

import { curveDistance2, segmentDistance2 } from "./edgeGeometry";

export interface Positioned {
  x?: number;
  y?: number;
}

const cellKey = (cx: number, cy: number) =>
  (cx + 1_048_576) * 2_097_152 + (cy + 1_048_576);

/**
 * Buckets node indices by world position. Rebuild it when positions change;
 * queries read the nodes' live coordinates, so a slightly stale grid only
 * risks missing a node that moved across a cell since the rebuild.
 */
export class SpatialGrid<T extends Positioned> {
  private cells = new Map<number, number[]>();
  private items: ReadonlyArray<T> = [];

  constructor(private readonly cellSize = 48) {}

  rebuild(items: ReadonlyArray<T>): void {
    this.items = items;
    this.cells.clear();
    const size = this.cellSize;
    for (let i = 0; i < items.length; i++) {
      const { x, y } = items[i];
      if (
        x === undefined ||
        y === undefined ||
        !Number.isFinite(x) ||
        !Number.isFinite(y)
      )
        continue;
      const key = cellKey(Math.floor(x / size), Math.floor(y / size));
      const cell = this.cells.get(key);
      if (cell) cell.push(i);
      else this.cells.set(key, [i]);
    }
  }

  /**
   * The item nearest (x, y) within `maxDist`, or null. With `reach`, an item
   * only counts within its own reach (larger nodes are easier to hit) and the
   * closest relative to its reach wins. Ties go to the later item: it is drawn
   * on top.
   */
  nearest(
    x: number,
    y: number,
    maxDist: number,
    reach?: (item: T) => number,
  ): T | null {
    const size = this.cellSize;
    const x0 = Math.floor((x - maxDist) / size);
    const x1 = Math.floor((x + maxDist) / size);
    const y0 = Math.floor((y - maxDist) / size);
    const y1 = Math.floor((y + maxDist) / size);
    let best = -1;
    let bestScore = reach ? 1 : maxDist * maxDist;
    for (let cx = x0; cx <= x1; cx++) {
      for (let cy = y0; cy <= y1; cy++) {
        const cell = this.cells.get(cellKey(cx, cy));
        if (!cell) continue;
        for (const i of cell) {
          const item = this.items[i];
          if (item.x === undefined || item.y === undefined) continue;
          const d2 = (item.x - x) ** 2 + (item.y - y) ** 2;
          let score = d2;
          if (reach) {
            const r = reach(item);
            score = r > 0 ? d2 / (r * r) : Infinity;
          }
          if (score < bestScore || (score === bestScore && i > best)) {
            bestScore = score;
            best = i;
          }
        }
      }
    }
    return best >= 0 ? this.items[best] : null;
  }
}

export const EDGE_LINE = 0;
export const EDGE_CURVE = 1;
export const EDGE_LOOP = 2;

/**
 * The edges one frame drew, with their screen geometry, so hover and clicks
 * test the exact shapes on screen. Later entries were drawn on top and win
 * ties.
 */
export class EdgeHitBuffer {
  private size = 0;
  private edge = new Int32Array(0);
  private kind = new Uint8Array(0);
  private coords = new Float64Array(0);

  reset(capacity: number): void {
    if (this.edge.length < capacity) {
      this.edge = new Int32Array(capacity);
      this.kind = new Uint8Array(capacity);
      this.coords = new Float64Array(capacity * 6);
    }
    this.size = 0;
  }

  get count(): number {
    return this.size;
  }

  /** The edge index of entry `i`. */
  edgeAt(i: number): number {
    return this.edge[i];
  }

  kindAt(i: number): number {
    return this.kind[i];
  }

  /**
   * Coordinate `k` of entry `i`. Lines: s.x, s.y, -, -, t.x, t.y. Curves:
   * s.x, s.y, c.x, c.y, t.x, t.y. Loops: center x, center y, radius.
   */
  coordAt(i: number, k: number): number {
    return this.coords[i * 6 + k];
  }

  pushLine(
    edgeIndex: number,
    sx: number,
    sy: number,
    tx: number,
    ty: number,
  ): void {
    this.push(edgeIndex, EDGE_LINE, sx, sy, 0, 0, tx, ty);
  }

  pushCurve(
    edgeIndex: number,
    sx: number,
    sy: number,
    cx: number,
    cy: number,
    tx: number,
    ty: number,
  ): void {
    this.push(edgeIndex, EDGE_CURVE, sx, sy, cx, cy, tx, ty);
  }

  pushLoop(edgeIndex: number, cx: number, cy: number, radius: number): void {
    this.push(edgeIndex, EDGE_LOOP, cx, cy, radius, 0, 0, 0);
  }

  private push(
    edgeIndex: number,
    kind: number,
    a: number,
    b: number,
    c: number,
    d: number,
    e: number,
    f: number,
  ) {
    if (this.size >= this.edge.length) this.grow();
    const i = this.size++;
    this.edge[i] = edgeIndex;
    this.kind[i] = kind;
    const o = i * 6;
    this.coords[o] = a;
    this.coords[o + 1] = b;
    this.coords[o + 2] = c;
    this.coords[o + 3] = d;
    this.coords[o + 4] = e;
    this.coords[o + 5] = f;
  }

  private grow() {
    const capacity = Math.max(16, this.edge.length * 2);
    const edge = new Int32Array(capacity);
    const kind = new Uint8Array(capacity);
    const coords = new Float64Array(capacity * 6);
    edge.set(this.edge);
    kind.set(this.kind);
    coords.set(this.coords);
    this.edge = edge;
    this.kind = kind;
    this.coords = coords;
  }

  /** The edge index nearest (px, py) within `tolerance` px, or -1. */
  nearest(px: number, py: number, tolerance: number): number {
    let best = -1;
    let bestD2 = tolerance * tolerance;
    const c = this.coords;
    for (let i = 0; i < this.size; i++) {
      const o = i * 6;
      let d2: number;
      switch (this.kind[i]) {
        case EDGE_LOOP: {
          const r = c[o + 2];
          const dx = px - c[o];
          const dy = py - c[o + 1];
          if (Math.abs(dx) > r + tolerance || Math.abs(dy) > r + tolerance)
            continue;
          const d = Math.abs(Math.hypot(dx, dy) - r);
          d2 = d * d;
          break;
        }
        case EDGE_CURVE: {
          const minX = Math.min(c[o], c[o + 2], c[o + 4]) - tolerance;
          const maxX = Math.max(c[o], c[o + 2], c[o + 4]) + tolerance;
          const minY = Math.min(c[o + 1], c[o + 3], c[o + 5]) - tolerance;
          const maxY = Math.max(c[o + 1], c[o + 3], c[o + 5]) + tolerance;
          if (px < minX || px > maxX || py < minY || py > maxY) continue;
          d2 = curveDistance2(
            px,
            py,
            c[o],
            c[o + 1],
            c[o + 2],
            c[o + 3],
            c[o + 4],
            c[o + 5],
          );
          break;
        }
        default: {
          const minX = Math.min(c[o], c[o + 4]) - tolerance;
          const maxX = Math.max(c[o], c[o + 4]) + tolerance;
          const minY = Math.min(c[o + 1], c[o + 5]) - tolerance;
          const maxY = Math.max(c[o + 1], c[o + 5]) + tolerance;
          if (px < minX || px > maxX || py < minY || py > maxY) continue;
          d2 = segmentDistance2(px, py, c[o], c[o + 1], c[o + 4], c[o + 5]);
        }
      }
      if (d2 <= bestD2) {
        bestD2 = d2;
        best = this.edge[i];
      }
    }
    return best;
  }
}

/**
 * For arrow-key navigation: the node to move to from `from` in direction
 * (dx, dy). Candidates must lie within 60° of the direction; among them the
 * nearest wins, with sideways distance counting double so the move follows
 * the arrow. Returns -1 when nothing lies that way.
 */
export function nearestInDirection(
  nodes: ReadonlyArray<Positioned>,
  from: { x: number; y: number },
  dx: number,
  dy: number,
  skip = -1,
): number {
  const len = Math.hypot(dx, dy);
  if (len === 0) return -1;
  const ux = dx / len;
  const uy = dy / len;
  const maxSlope = Math.tan(Math.PI / 3);
  let best = -1;
  let bestScore = Infinity;
  for (let i = 0; i < nodes.length; i++) {
    if (i === skip) continue;
    const { x, y } = nodes[i];
    if (x === undefined || y === undefined) continue;
    const vx = x - from.x;
    const vy = y - from.y;
    const along = vx * ux + vy * uy;
    if (along <= 1e-6) continue;
    const across = Math.abs(vx * uy - vy * ux);
    if (across > along * maxSlope) continue;
    const score = along + 2 * across;
    if (score < bestScore) {
      bestScore = score;
      best = i;
    }
  }
  return best;
}

/** Index of the node nearest a world point, scanning all nodes. -1 when there are none. */
export function nearestTo(
  nodes: ReadonlyArray<Positioned>,
  point: { x: number; y: number },
): number {
  let best = -1;
  let bestD2 = Infinity;
  for (let i = 0; i < nodes.length; i++) {
    const { x, y } = nodes[i];
    if (x === undefined || y === undefined) continue;
    const d2 = (x - point.x) ** 2 + (y - point.y) ** 2;
    if (d2 < bestD2) {
      bestD2 = d2;
      best = i;
    }
  }
  return best;
}
