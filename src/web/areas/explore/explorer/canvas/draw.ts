// Draws one frame of the explorer canvas. Everything is batched: edges into
// one path per style, nodes into one path per color, and only what is inside
// the viewport is drawn. Paths are built on the context itself rather than in
// Path2D objects, so a frame allocates next to nothing.

import { CANVAS_THEME, withAlpha } from "../lib/theme";
import {
  arrowTip,
  curveControl,
  emptyLoop,
  loopGeometry,
  type Point,
} from "./edgeGeometry";
import { EDGE_CURVE, EDGE_LOOP, EdgeHitBuffer } from "./hitTest";
import {
  arrowLengthPx,
  fastNodeShapes,
  gridSpacing,
  nodeRadiusPx,
  placeCaptions,
  showArrows,
  showCaptions,
  type Box,
} from "./lod";
import type { ForceLayout } from "./useGraphSimulation";
import type { ViewTransform } from "./usePanZoom";

const TAU = Math.PI * 2;
const GRID_DOT_PX = 1.5;
const CAPTION_HEIGHT = 14;

/** Typed arrays reused across frames. */
export class FrameScratch {
  sx = new Float64Array(0);
  sy = new Float64Array(0);
  /** Node radius in px this frame. */
  radius = new Float64Array(0);
  visible = new Uint8Array(0);
  /** Node indices bucketed by color, for one fill per color. */
  order = new Int32Array(0);
  /** Style of each drawn edge (index into the hit buffer). */
  edgeStyle = new Uint8Array(0);

  ensure(n: number): void {
    if (this.sx.length >= n) return;
    const size = Math.max(n, Math.ceil(this.sx.length * 1.5));
    this.sx = new Float64Array(size);
    this.sy = new Float64Array(size);
    this.radius = new Float64Array(size);
    this.visible = new Uint8Array(size);
    this.order = new Int32Array(size);
  }

  ensureEdges(n: number): void {
    if (this.edgeStyle.length < n)
      this.edgeStyle = new Uint8Array(
        Math.max(n, Math.ceil(this.edgeStyle.length * 1.5)),
      );
  }
}

export interface SceneInput {
  layout: Pick<ForceLayout, "nodes" | "edges" | "indexById">;
  /** The distinct node colors. */
  palette: readonly string[];
  /** Index into `palette` per node, aligned with `layout.nodes`. */
  colorIds: ArrayLike<number>;
  /** Size multiplier per node (hubs are larger), aligned with `layout.nodes`. */
  sizes: ArrayLike<number>;
  /** Caption for a node index, already truncated. */
  captionAt: (index: number) => string;
  view: ViewTransform;
  width: number;
  height: number;
  dpr: number;
  selectedNode: number;
  selectedEdge: number;
  hoveredNode: number;
  hoveredEdge: number;
  /** 1 for nodes in the emphasis set, aligned with `layout.nodes`; null when nothing is emphasized. */
  emphasis: Uint8Array | null;
  pinned: ReadonlySet<string>;
  /** Nodes outside what the explorer reads: a dashed ring around each. */
  outside: ReadonlySet<string>;
  /** CSS font-family for captions. */
  fontFamily: string;
  /** Text width cache for the caption font. */
  measure: Map<string, number>;
  /** Filled with the edges this frame draws, for hit testing. */
  hits: EdgeHitBuffer;
  scratch: FrameScratch;
}

export interface SceneStats {
  visibleNodes: number;
  visibleEdges: number;
  captions: boolean;
  arrows: boolean;
}

function captionFont(fontFamily: string): string {
  return `500 11px ${fontFamily}`;
}

export function drawScene(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
): SceneStats {
  const { width, height, dpr, view } = scene;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.globalAlpha = 1;
  ctx.fillStyle = CANVAS_THEME.background;
  ctx.fillRect(0, 0, width, height);
  drawGrid(ctx, width, height, dpr, view);

  const nodes = scene.layout.nodes;
  const r = nodeRadiusPx(view.scale);
  scene.scratch.ensure(nodes.length);
  const { sx, sy, radius, visible } = scene.scratch;
  const emphasis = scene.emphasis;
  let visibleNodes = 0;
  let captionable = 0;
  for (let i = 0; i < nodes.length; i++) {
    const node = nodes[i];
    const x = (node.x ?? 0) * view.scale + view.tx;
    const y = (node.y ?? 0) * view.scale + view.ty;
    const ri = r * (scene.sizes[i] ?? 1);
    sx[i] = x;
    sy[i] = y;
    radius[i] = ri;
    const margin = ri + 6;
    const inside =
      x >= -margin &&
      x <= width + margin &&
      y >= -margin &&
      y <= height + margin;
    visible[i] = inside ? 1 : 0;
    if (!inside) continue;
    visibleNodes++;
    if (emphasis === null || emphasis[i] === 1) captionable++;
  }

  const visibleEdges = collectEdges(scene);
  const arrows = showArrows(view.scale, visibleEdges);
  drawEdges(ctx, scene, r, arrows);
  drawNodes(ctx, scene, r);
  const captions = showCaptions(view.scale, captionable);
  drawCaptions(ctx, scene, captions);
  return { visibleNodes, visibleEdges, captions, arrows };
}

function drawGrid(
  ctx: CanvasRenderingContext2D,
  width: number,
  height: number,
  dpr: number,
  v: ViewTransform,
) {
  const spacing = gridSpacing(v.scale);
  const step = spacing * v.scale;
  const startX =
    Math.floor(-v.tx / v.scale / spacing) * spacing * v.scale + v.tx;
  const startY =
    Math.floor(-v.ty / v.scale / spacing) * spacing * v.scale + v.ty;
  const half = GRID_DOT_PX / 2;
  ctx.fillStyle = CANVAS_THEME.grid;
  ctx.beginPath();
  for (let y = startY; y <= height + step; y += step) {
    const py = Math.round((y - half) * dpr) / dpr;
    for (let x = startX; x <= width + step; x += step) {
      ctx.rect(
        Math.round((x - half) * dpr) / dpr,
        py,
        GRID_DOT_PX,
        GRID_DOT_PX,
      );
    }
  }
  ctx.fill();
}

const control: Point = { x: 0, y: 0 };
const loop = emptyLoop();
const tip = { x: 0, y: 0, angle: 0 };

/** Computes the screen geometry of every edge inside the viewport into `scene.hits`. */
function collectEdges(scene: SceneInput): number {
  const { width, height, hits } = scene;
  const { sx, sy, radius } = scene.scratch;
  const edges = scene.layout.edges;
  hits.reset(edges.length);
  for (let i = 0; i < edges.length; i++) {
    const e = edges[i];
    const x1 = sx[e.source];
    const y1 = sy[e.source];
    if (e.loop >= 0) {
      loopGeometry(x1, y1, radius[e.source], e.loop, loop);
      if (loop.cx + loop.radius < 0 || loop.cx - loop.radius > width) continue;
      if (loop.cy + loop.radius < 0 || loop.cy - loop.radius > height) continue;
      hits.pushLoop(i, loop.cx, loop.cy, loop.radius);
      continue;
    }
    const x2 = sx[e.target];
    const y2 = sy[e.target];
    let minX = Math.min(x1, x2);
    let maxX = Math.max(x1, x2);
    let minY = Math.min(y1, y2);
    let maxY = Math.max(y1, y2);
    if (e.bend !== 0) {
      curveControl(x1, y1, x2, y2, e.bend, control);
      minX = Math.min(minX, control.x);
      maxX = Math.max(maxX, control.x);
      minY = Math.min(minY, control.y);
      maxY = Math.max(maxY, control.y);
    }
    if (maxX < 0 || minX > width || maxY < 0 || minY > height) continue;
    if (e.bend !== 0) hits.pushCurve(i, x1, y1, control.x, control.y, x2, y2);
    else hits.pushLine(i, x1, y1, x2, y2);
  }
  return hits.count;
}

const BASE = 0;
const DIM = 1;
const HOVER = 2;
const HIGHLIGHT = 3;
const HOVERED = 4;
/** More highlighted edges than this are drawn softer. */
const HIGHLIGHT_CROWD = 60;

/**
 * Drawn bottom to top: [style, line width, stroke, arrow fill]. Read per
 * frame: the theme follows the page's light or dark tokens.
 */
const edgePasses = (): ReadonlyArray<[number, number, string, string]> => [
  [DIM, 1, CANVAS_THEME.edgeDim, CANVAS_THEME.edgeDim],
  [BASE, 1, CANVAS_THEME.edge, CANVAS_THEME.edgeArrow],
  [HOVER, 1.25, CANVAS_THEME.edgeHover, CANVAS_THEME.edgeHover],
  [HIGHLIGHT, 1.6, CANVAS_THEME.edgeHighlight, CANVAS_THEME.edgeHighlight],
];

function drawEdges(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
  r: number,
  arrows: boolean,
) {
  const {
    hits,
    emphasis,
    selectedNode,
    selectedEdge,
    hoveredNode,
    hoveredEdge,
  } = scene;
  const edges = scene.layout.edges;
  scene.scratch.ensureEdges(hits.count);
  const styles = scene.scratch.edgeStyle;
  const arrowLength = arrowLengthPx(r);
  const emphasized = (i: number) =>
    i === selectedNode || (emphasis !== null && emphasis[i] === 1);
  const highlighted = (index: number) => {
    const e = edges[index];
    return (
      index === selectedEdge ||
      e.source === selectedNode ||
      e.target === selectedNode
    );
  };
  let hoveredEntry = -1;
  let highlights = 0;

  for (let j = 0; j < hits.count; j++) {
    const index = hits.edgeAt(j);
    const e = edges[index];
    let style = BASE;
    if (index === hoveredEdge) {
      style = HOVERED;
      hoveredEntry = j;
    } else if (highlighted(index)) {
      style = HIGHLIGHT;
      highlights++;
    } else if (
      hoveredNode >= 0 &&
      (e.source === hoveredNode || e.target === hoveredNode)
    )
      style = HOVER;
    else if (
      emphasis !== null &&
      !(emphasized(e.source) && emphasized(e.target))
    )
      style = DIM;
    styles[j] = style;
  }

  // A selected hub lights up hundreds of edges; soften them so the rest of
  // the graph stays readable underneath.
  const highlightAlpha = highlights > HIGHLIGHT_CROWD ? 0.45 : 1;
  for (const [style, width, stroke, fill] of edgePasses()) {
    ctx.globalAlpha = style === HIGHLIGHT ? highlightAlpha : 1;
    ctx.beginPath();
    let drawn = 0;
    for (let j = 0; j < hits.count; j++) {
      if (styles[j] !== style) continue;
      traceLine(ctx, scene, j);
      drawn++;
    }
    if (drawn === 0) continue;
    ctx.lineWidth = width;
    ctx.strokeStyle = stroke;
    ctx.stroke();
    if (!arrows) continue;
    ctx.beginPath();
    for (let j = 0; j < hits.count; j++) {
      if (styles[j] === style) traceHead(ctx, scene, j, arrowLength);
    }
    ctx.fillStyle = fill;
    ctx.fill();
  }
  ctx.globalAlpha = 1;

  if (hoveredEntry >= 0) {
    const color = highlighted(hits.edgeAt(hoveredEntry))
      ? CANVAS_THEME.edgeHighlight
      : CANVAS_THEME.captionStrong;
    ctx.beginPath();
    traceLine(ctx, scene, hoveredEntry);
    ctx.lineWidth = 6;
    ctx.strokeStyle = withAlpha(color, 0.14);
    ctx.stroke();
    ctx.lineWidth = 2;
    ctx.strokeStyle = withAlpha(color, 0.85);
    ctx.stroke();
    ctx.beginPath();
    traceHead(ctx, scene, hoveredEntry, arrowLength + 1);
    ctx.fillStyle = color;
    ctx.fill();
  }
}

/** Adds the edge drawn at hit-buffer `entry` to the current path. */
function traceLine(path: CanvasPath, scene: SceneInput, entry: number) {
  const { hits } = scene;
  const a = hits.coordAt(entry, 0);
  const b = hits.coordAt(entry, 1);
  switch (hits.kindAt(entry)) {
    case EDGE_LOOP: {
      const loopRadius = hits.coordAt(entry, 2);
      path.moveTo(a + loopRadius, b);
      path.arc(a, b, loopRadius, 0, TAU);
      return;
    }
    case EDGE_CURVE:
      path.moveTo(a, b);
      path.quadraticCurveTo(
        hits.coordAt(entry, 2),
        hits.coordAt(entry, 3),
        hits.coordAt(entry, 4),
        hits.coordAt(entry, 5),
      );
      return;
    default:
      path.moveTo(a, b);
      path.lineTo(hits.coordAt(entry, 4), hits.coordAt(entry, 5));
  }
}

/** Adds the arrowhead of the edge at `entry`, at its target's rim. */
function traceHead(
  path: CanvasPath,
  scene: SceneInput,
  entry: number,
  length: number,
) {
  const { hits } = scene;
  const { sx, sy, radius } = scene.scratch;
  const e = scene.layout.edges[hits.edgeAt(entry)];
  const kind = hits.kindAt(entry);
  if (kind === EDGE_LOOP) {
    loopGeometry(sx[e.source], sy[e.source], radius[e.source], e.loop, loop);
    traceArrow(path, loop.tipX, loop.tipY, loop.angle, length);
    return;
  }
  // A straight edge points from its source; a curve along its end tangent.
  const fromX =
    kind === EDGE_CURVE ? hits.coordAt(entry, 2) : hits.coordAt(entry, 0);
  const fromY =
    kind === EDGE_CURVE ? hits.coordAt(entry, 3) : hits.coordAt(entry, 1);
  if (
    arrowTip(
      fromX,
      fromY,
      hits.coordAt(entry, 4),
      hits.coordAt(entry, 5),
      radius[e.target],
      tip,
    )
  ) {
    traceArrow(path, tip.x, tip.y, tip.angle, length);
  }
}

function traceArrow(
  path: CanvasPath,
  x: number,
  y: number,
  angle: number,
  length: number,
) {
  const cos = Math.cos(angle);
  const sin = Math.sin(angle);
  const half = length * 0.55;
  const bx = x - cos * length;
  const by = y - sin * length;
  path.moveTo(x, y);
  path.lineTo(bx - sin * half, by + cos * half);
  path.lineTo(bx + sin * half, by - cos * half);
  path.closePath();
}

function drawNodes(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
  r: number,
) {
  const {
    palette,
    colorIds,
    emphasis,
    selectedNode,
    hoveredNode,
    selectedEdge,
  } = scene;
  const { sx, sy, radius, visible, order } = scene.scratch;
  const nodes = scene.layout.nodes;
  const fast = fastNodeShapes(r);
  const dimmed = (i: number) =>
    emphasis !== null && emphasis[i] !== 1 && i !== selectedNode;

  // Counting sort of the plain nodes into buckets: dimmed colors first (drawn
  // underneath), then full colors.
  const colorCount = palette.length;
  const bucketOf = (i: number) => (dimmed(i) ? 0 : colorCount) + colorIds[i];
  const starts = new Int32Array(colorCount * 2 + 1);
  for (let i = 0; i < nodes.length; i++) {
    if (visible[i] && i !== hoveredNode && i !== selectedNode)
      starts[bucketOf(i) + 1]++;
  }
  for (let b = 1; b < starts.length; b++) starts[b] += starts[b - 1];
  const cursor = starts.slice(0, -1);
  for (let i = 0; i < nodes.length; i++) {
    if (visible[i] && i !== hoveredNode && i !== selectedNode)
      order[cursor[bucketOf(i)]++] = i;
  }

  ctx.lineWidth = 1;
  ctx.strokeStyle = CANVAS_THEME.nodeOutline;
  for (let b = 0; b < colorCount * 2; b++) {
    if (starts[b] === starts[b + 1]) continue;
    const faint = b < colorCount;
    ctx.beginPath();
    for (let k = starts[b]; k < starts[b + 1]; k++) {
      const i = order[k];
      const ri = radius[i];
      if (fast) {
        ctx.rect(sx[i] - ri, sy[i] - ri, ri * 2, ri * 2);
      } else {
        ctx.moveTo(sx[i] + ri, sy[i]);
        ctx.arc(sx[i], sy[i], ri, 0, TAU);
      }
    }
    ctx.globalAlpha = faint ? CANVAS_THEME.dimAlpha : 1;
    ctx.fillStyle = palette[b % colorCount];
    ctx.fill();
    if (!fast && !faint) ctx.stroke();
  }
  ctx.globalAlpha = 1;

  drawPinnedMarkers(ctx, scene, dimmed, false);
  drawOutsideRings(ctx, scene, dimmed);

  if (selectedEdge >= 0) {
    const e = scene.layout.edges[selectedEdge];
    if (e) {
      ctx.lineWidth = 1.5;
      ctx.strokeStyle = withAlpha(CANVAS_THEME.selection, 0.6);
      ctx.beginPath();
      for (const i of [e.source, e.target]) {
        if (!visible[i]) continue;
        ctx.moveTo(sx[i] + radius[i] + 3, sy[i]);
        ctx.arc(sx[i], sy[i], radius[i] + 3, 0, TAU);
      }
      ctx.stroke();
    }
  }

  if (
    hoveredNode >= 0 &&
    hoveredNode !== selectedNode &&
    visible[hoveredNode]
  ) {
    const color = palette[colorIds[hoveredNode]];
    const x = sx[hoveredNode];
    const y = sy[hoveredNode];
    const ri = radius[hoveredNode];
    ctx.globalAlpha = dimmed(hoveredNode) ? 0.5 : 1;
    ctx.lineWidth = 3;
    ctx.strokeStyle = withAlpha(color, CANVAS_THEME.hoverRingAlpha);
    ctx.beginPath();
    ctx.arc(x, y, ri + 4.5, 0, TAU);
    ctx.stroke();
    ctx.fillStyle = color;
    ctx.beginPath();
    ctx.arc(x, y, ri + 1.5, 0, TAU);
    ctx.fill();
    ctx.globalAlpha = 1;
  }

  if (selectedNode >= 0 && visible[selectedNode]) {
    const x = sx[selectedNode];
    const y = sy[selectedNode];
    const ri =
      radius[selectedNode] + (selectedNode === hoveredNode ? 1.5 : 0.75);
    ctx.fillStyle = palette[colorIds[selectedNode]];
    ctx.beginPath();
    ctx.arc(x, y, ri, 0, TAU);
    ctx.fill();
    ctx.lineWidth = 1;
    ctx.strokeStyle = CANVAS_THEME.nodeOutline;
    ctx.stroke();
    ctx.lineWidth = 2;
    ctx.strokeStyle = CANVAS_THEME.selection;
    ctx.beginPath();
    ctx.arc(x, y, ri + 3.5, 0, TAU);
    ctx.stroke();
  }

  // The hovered and selected nodes were just redrawn larger, with rings: their
  // notches go on top, or the enlarged disc and the rings would cover them.
  drawPinnedMarkers(ctx, scene, dimmed, true);
}

/**
 * Nodes outside the explorer's load (another load's, or data no load wrote)
 * carry a dashed ring, so they read as reached, not part of the load.
 */
function drawOutsideRings(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
  dimmed: (i: number) => boolean,
) {
  if (scene.outside.size === 0) return;
  const { sx, sy, radius, visible } = scene.scratch;
  ctx.save();
  ctx.lineWidth = 1.5;
  ctx.setLineDash([3, 2]);
  ctx.strokeStyle = CANVAS_THEME.nodeOutline;
  for (const faint of [true, false]) {
    ctx.beginPath();
    let drawn = 0;
    for (const id of scene.outside) {
      const i = scene.layout.indexById.get(id);
      if (i === undefined || !visible[i] || dimmed(i) !== faint) continue;
      ctx.moveTo(sx[i] + radius[i] + 2.5, sy[i]);
      ctx.arc(sx[i], sy[i], radius[i] + 2.5, 0, TAU);
      drawn++;
    }
    if (drawn === 0) continue;
    ctx.globalAlpha = faint ? CANVAS_THEME.dimAlpha : 1;
    ctx.stroke();
  }
  ctx.restore();
}

/**
 * Pinned nodes carry a small square notch at their upper right. `highlighted`
 * picks the pass: the hovered and selected nodes, or every other node.
 */
function drawPinnedMarkers(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
  dimmed: (i: number) => boolean,
  highlighted: boolean,
) {
  if (scene.pinned.size === 0) return;
  const { sx, sy, radius, visible } = scene.scratch;
  ctx.lineWidth = 1;
  ctx.strokeStyle = CANVAS_THEME.nodeOutline;
  ctx.fillStyle = CANVAS_THEME.pinned;
  for (const faint of [true, false]) {
    ctx.beginPath();
    let drawn = 0;
    for (const id of scene.pinned) {
      const i = scene.layout.indexById.get(id);
      if (i === undefined || !visible[i] || dimmed(i) !== faint) continue;
      const isHighlighted = i === scene.hoveredNode || i === scene.selectedNode;
      if (isHighlighted !== highlighted) continue;
      const grow = isHighlighted ? 1.5 : 0;
      const size = Math.max(3, radius[i] * 0.6);
      const offset = (radius[i] + grow) * 0.72;
      ctx.rect(
        sx[i] + offset - size / 2,
        sy[i] - offset - size / 2,
        size,
        size,
      );
      drawn++;
    }
    if (drawn === 0) continue;
    ctx.globalAlpha = faint ? CANVAS_THEME.dimAlpha : 1;
    ctx.fill();
    if (!faint) ctx.stroke();
  }
  ctx.globalAlpha = 1;
}

function drawCaptions(
  ctx: CanvasRenderingContext2D,
  scene: SceneInput,
  all: boolean,
) {
  const { emphasis, selectedNode, hoveredNode, measure } = scene;
  const { sx, sy, radius, visible } = scene.scratch;
  const nodes = scene.layout.nodes;

  const candidates: number[] = [];
  if (all) {
    for (let i = 0; i < nodes.length; i++) {
      if (!visible[i] || i === selectedNode) continue;
      if (emphasis !== null && emphasis[i] !== 1) continue;
      candidates.push(i);
    }
    // Hubs first: they are the landmarks.
    candidates.sort((a, b) => nodes[b].degree - nodes[a].degree);
    if (hoveredNode >= 0) {
      const at = candidates.indexOf(hoveredNode);
      if (at > 0) {
        candidates.splice(at, 1);
        candidates.unshift(hoveredNode);
      }
    }
  }
  // The selected node is always captioned, at any zoom.
  if (selectedNode >= 0 && visible[selectedNode])
    candidates.unshift(selectedNode);
  if (candidates.length === 0) return;

  ctx.font = captionFont(scene.fontFamily);
  const texts: string[] = [];
  const boxes: Box[] = [];
  for (const i of candidates) {
    const text = scene.captionAt(i);
    let w = measure.get(text);
    if (w === undefined) {
      w = ctx.measureText(text).width;
      if (measure.size > 8000) measure.clear();
      measure.set(text, w);
    }
    texts.push(text);
    const grow = i === selectedNode || i === hoveredNode ? 5 : 3;
    boxes.push({
      x: sx[i] - w / 2 - 2,
      y: sy[i] + radius[i] + grow,
      w: w + 4,
      h: CAPTION_HEIGHT,
    });
  }
  const placed = placeCaptions(boxes);

  ctx.textAlign = "center";
  ctx.textBaseline = "top";
  ctx.lineJoin = "round";
  ctx.lineWidth = 3;
  ctx.strokeStyle = CANVAS_THEME.captionHalo;
  for (let k = 0; k < candidates.length; k++) {
    if (!placed[k] || !texts[k]) continue;
    ctx.strokeText(texts[k], boxes[k].x + boxes[k].w / 2, boxes[k].y + 1);
  }
  for (let k = 0; k < candidates.length; k++) {
    if (!placed[k] || !texts[k]) continue;
    ctx.fillStyle =
      candidates[k] === selectedNode
        ? CANVAS_THEME.captionStrong
        : CANVAS_THEME.caption;
    ctx.fillText(texts[k], boxes[k].x + boxes[k].w / 2, boxes[k].y + 1);
  }
  ctx.textAlign = "start";
  ctx.textBaseline = "alphabetic";
  ctx.lineJoin = "miter";
}
