// Level-of-detail rules for the canvas: what gets drawn at which zoom level
// and density. Kept pure so the thresholds are easy to test and tune.

/** Node radius in CSS px at scale 1. */
export const NODE_RADIUS = 6;

/** World units between grid dots at the finest level. */
export const GRID_SPACING = 32;
/** Grid dots closer than this on screen switch to the next coarser level. */
const MIN_GRID_SPACING_PX = 18;

const ARROW_MIN_SCALE = 0.6;
const ARROW_MAX_EDGES = 4000;
const CAPTION_MIN_SCALE = 1.1;
const CAPTION_MAX_NODES = 400;
/** A handful of nodes (a small graph, or a small emphasis set) gets captions from further out. */
const CAPTION_FEW_NODES = 60;
const CAPTION_FEW_MIN_SCALE = 0.5;
const CAPTION_MAX_CHARS = 28;

/**
 * Nodes grow with the square root of the zoom: they stay findable when zoomed
 * far out and don't balloon when zoomed in.
 */
export function nodeRadiusPx(scale: number): number {
  return Math.max(2, Math.min(10, NODE_RADIUS * Math.sqrt(Math.max(scale, 0))));
}

/**
 * Hubs are drawn larger so they read as landmarks: nodes with up to 8 edges
 * are normal size, and the size grows with the log of the degree up to 1.9×.
 */
export function degreeScale(degree: number): number {
  return 1 + Math.min(0.9, 0.25 * Math.log2(Math.max(1, degree / 8)));
}

/** The largest {@link degreeScale}. */
export const MAX_DEGREE_SCALE = 1.9;

/** Below this radius nodes are drawn without an outline: cheaper, and indistinguishable at that size. */
export function fastNodeShapes(radiusPx: number): boolean {
  return radiusPx < 2.5;
}

export function showArrows(scale: number, visibleEdges: number): boolean {
  return scale >= ARROW_MIN_SCALE && visibleEdges <= ARROW_MAX_EDGES;
}

export function arrowLengthPx(radiusPx: number): number {
  return Math.max(4, Math.min(7, radiusPx * 0.9));
}

/** `visibleNodes` counts the nodes that would get a caption: on screen and not dimmed. */
export function showCaptions(scale: number, visibleNodes: number): boolean {
  if (visibleNodes <= CAPTION_FEW_NODES) return scale >= CAPTION_FEW_MIN_SCALE;
  return scale >= CAPTION_MIN_SCALE && visibleNodes <= CAPTION_MAX_NODES;
}

/** The world spacing of grid dots: the finest power-of-two multiple that stays readable on screen. */
export function gridSpacing(scale: number): number {
  let spacing = GRID_SPACING;
  while (spacing * scale < MIN_GRID_SPACING_PX && spacing < 1e6) spacing *= 2;
  return spacing;
}

/** How close, in px, a pointer must be to a node's center to hit it. Fingers get more room. */
export function nodeHitRadiusPx(radiusPx: number, pointerType: string): number {
  return pointerType === "touch"
    ? Math.max(radiusPx + 6, 18)
    : Math.max(radiusPx + 4, 9);
}

export function edgeHitDistancePx(pointerType: string): number {
  return pointerType === "touch" ? 10 : 5;
}

export function truncateCaption(text: string, max = CAPTION_MAX_CHARS): string {
  const clean = text.replace(/\s+/g, " ").trim();
  if (clean.length <= max) return clean;
  return `${clean.slice(0, max - 1).trimEnd()}…`;
}

export interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
}

/**
 * Greedy caption placement: boxes are taken in priority order and one that
 * overlaps an already placed box is dropped. Returns which boxes to draw.
 */
export function placeCaptions(boxes: ReadonlyArray<Box>, gap = 2): boolean[] {
  const cell = 64;
  const grid = new Map<number, number[]>();
  const placed: boolean[] = new Array(boxes.length).fill(false);
  const key = (cx: number, cy: number) => (cx + 32768) * 65536 + (cy + 32768);

  for (let i = 0; i < boxes.length; i++) {
    const b = boxes[i];
    const x0 = Math.floor((b.x - gap) / cell);
    const x1 = Math.floor((b.x + b.w + gap) / cell);
    const y0 = Math.floor((b.y - gap) / cell);
    const y1 = Math.floor((b.y + b.h + gap) / cell);
    let blocked = false;
    for (let cx = x0; cx <= x1 && !blocked; cx++) {
      for (let cy = y0; cy <= y1 && !blocked; cy++) {
        const others = grid.get(key(cx, cy));
        if (!others) continue;
        for (const j of others) {
          const o = boxes[j];
          if (
            b.x - gap < o.x + o.w &&
            o.x - gap < b.x + b.w &&
            b.y - gap < o.y + o.h &&
            o.y - gap < b.y + b.h
          ) {
            blocked = true;
            break;
          }
        }
      }
    }
    if (blocked) continue;
    placed[i] = true;
    for (let cx = x0; cx <= x1; cx++) {
      for (let cy = y0; cy <= y1; cy++) {
        const k = key(cx, cy);
        const list = grid.get(k);
        if (list) list.push(i);
        else grid.set(k, [i]);
      }
    }
  }
  return placed;
}
