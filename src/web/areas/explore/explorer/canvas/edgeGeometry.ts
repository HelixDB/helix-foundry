// Edge shapes for the canvas, in screen pixels: straight lines, quadratic
// curves for parallel edges (multigraphs), and small loops for self-edges.
// Drawing and hit testing share these so a hover lands on what is drawn.

export interface EdgeRoute {
  /**
   * Signed bend in parallel steps, relative to the edge's own source → target
   * direction. 0 draws a straight line.
   */
  bend: number;
  /** Position among the node's self-loops, or -1 for an edge between two nodes. */
  loop: number;
}

export interface Point {
  x: number;
  y: number;
}

/**
 * Spreads parallel edges between the same two nodes (either direction) around
 * the straight line, and stacks self-loops. Offsets come from the edges' ids,
 * so an edge keeps its curve while its group doesn't change. Returned in input
 * order.
 */
export function assignEdgeRoutes(
  edges: ReadonlyArray<{ id: string; source: string; target: string }>,
): EdgeRoute[] {
  const groups = new Map<string, number[]>();
  for (let i = 0; i < edges.length; i++) {
    const { source, target } = edges[i];
    const key =
      source <= target
        ? `${source}\u0000${target}`
        : `${target}\u0000${source}`;
    const group = groups.get(key);
    if (group) group.push(i);
    else groups.set(key, [i]);
  }

  const routes: EdgeRoute[] = new Array(edges.length);
  for (const members of groups.values()) {
    members.sort((a, b) =>
      edges[a].id < edges[b].id ? -1 : edges[a].id > edges[b].id ? 1 : 0,
    );
    const first = edges[members[0]];
    if (first.source === first.target) {
      members.forEach((edgeIndex, i) => {
        routes[edgeIndex] = { bend: 0, loop: i };
      });
      continue;
    }
    const k = members.length;
    members.forEach((edgeIndex, i) => {
      const offset = i - (k - 1) / 2;
      const edge = edges[edgeIndex];
      // Offsets are laid out along the pair's canonical direction (lower id
      // first); an edge pointing the other way flips its own bend so the two
      // land on opposite sides.
      const canonical = edge.source <= edge.target;
      routes[edgeIndex] = {
        bend: offset === 0 ? 0 : canonical ? offset : -offset,
        loop: -1,
      };
    });
  }
  return routes;
}

/** How far apart, in px, neighboring parallel edges sit at their apex. */
export function parallelStep(lengthPx: number): number {
  return Math.min(28, Math.max(6, lengthPx * 0.15), lengthPx * 0.5);
}

/**
 * The quadratic control point for an edge from s to t bent by `bend` steps.
 * The curve's apex sits `bend * parallelStep` px off the straight line.
 * Writes into `out` to avoid allocating in the frame loop.
 */
export function curveControl(
  sx: number,
  sy: number,
  tx: number,
  ty: number,
  bend: number,
  out: Point,
): Point {
  const dx = tx - sx;
  const dy = ty - sy;
  const len = Math.hypot(dx, dy);
  const mx = (sx + tx) / 2;
  const my = (sy + ty) / 2;
  if (len < 1e-6 || bend === 0) {
    out.x = mx;
    out.y = my;
    return out;
  }
  // The apex of a quadratic is halfway to its control point.
  const offset = 2 * bend * parallelStep(len);
  out.x = mx + (-dy / len) * offset;
  out.y = my + (dx / len) * offset;
  return out;
}

/** Self-loops leave the node toward the upper right. */
const LOOP_ANGLE = -Math.PI / 4;

export interface LoopGeometry {
  cx: number;
  cy: number;
  radius: number;
  /** Arrow tip where the loop re-enters the node, and the direction it points. */
  tipX: number;
  tipY: number;
  angle: number;
}

/**
 * A loop for a node at (nx, ny) with radius r px: a circle through the node's
 * center, wider for each further loop on the same node.
 */
export function loopGeometry(
  nx: number,
  ny: number,
  r: number,
  index: number,
  out: LoopGeometry,
): LoopGeometry {
  const radius = Math.max(r, 3) * 1.2 + 7 + 6 * index;
  const ux = Math.cos(LOOP_ANGLE);
  const uy = Math.sin(LOOP_ANGLE);
  out.cx = nx + ux * radius;
  out.cy = ny + uy * radius;
  out.radius = radius;
  // Where the loop crosses the node's rim: circles of radius r (at N) and
  // `radius` (at C, |NC| = radius) meet at a = r²/2R along NC, h across it.
  const rim = Math.max(r, 3) + 1;
  const a = (rim * rim) / (2 * radius);
  const h = Math.sqrt(Math.max(0, rim * rim - a * a));
  const px = nx + ux * a - uy * h;
  const py = ny + uy * a + ux * h;
  // The tangent there, turned to point into the node.
  let txv = -(py - out.cy);
  let tyv = px - out.cx;
  if (txv * (nx - px) + tyv * (ny - py) < 0) {
    txv = -txv;
    tyv = -tyv;
  }
  out.tipX = px;
  out.tipY = py;
  out.angle = Math.atan2(tyv, txv);
  return out;
}

export function emptyLoop(): LoopGeometry {
  return { cx: 0, cy: 0, radius: 0, tipX: 0, tipY: 0, angle: 0 };
}

/**
 * The arrow tip at the target's rim and its direction, for a straight edge
 * (control at the midpoint) or a curve (the tangent at its end).
 */
export function arrowTip(
  cx: number,
  cy: number,
  tx: number,
  ty: number,
  targetRadius: number,
  out: { x: number; y: number; angle: number },
): boolean {
  const dx = tx - cx;
  const dy = ty - cy;
  const len = Math.hypot(dx, dy);
  if (len < 1e-6) return false;
  const back = targetRadius + 1;
  out.x = tx - (dx / len) * back;
  out.y = ty - (dy / len) * back;
  out.angle = Math.atan2(dy, dx);
  return true;
}

/** Point-to-segment squared distance. */
export function segmentDistance2(
  px: number,
  py: number,
  ax: number,
  ay: number,
  bx: number,
  by: number,
): number {
  const vx = bx - ax;
  const vy = by - ay;
  const wx = px - ax;
  const wy = py - ay;
  const c1 = vx * wx + vy * wy;
  if (c1 <= 0) return wx * wx + wy * wy;
  const c2 = vx * vx + vy * vy;
  if (c2 <= c1) {
    const dx = px - bx;
    const dy = py - by;
    return dx * dx + dy * dy;
  }
  const t = c1 / c2;
  const dx = px - (ax + t * vx);
  const dy = py - (ay + t * vy);
  return dx * dx + dy * dy;
}

const CURVE_SAMPLES = 12;

/** Squared distance from a point to a quadratic curve s → (control c) → t, by sampling. */
export function curveDistance2(
  px: number,
  py: number,
  sx: number,
  sy: number,
  cx: number,
  cy: number,
  tx: number,
  ty: number,
): number {
  let best = Infinity;
  let ax = sx;
  let ay = sy;
  for (let i = 1; i <= CURVE_SAMPLES; i++) {
    const t = i / CURVE_SAMPLES;
    const mt = 1 - t;
    const bx = mt * mt * sx + 2 * mt * t * cx + t * t * tx;
    const by = mt * mt * sy + 2 * mt * t * cy + t * t * ty;
    const d = segmentDistance2(px, py, ax, ay, bx, by);
    if (d < best) best = d;
    ax = bx;
    ay = by;
  }
  return best;
}
