import { useLayoutEffect, useRef, useState } from "react";

/**
 * Screen-space translate applied after scale: world point (wx, wy) is drawn at
 * (wx * scale + tx, wy * scale + ty) in CSS pixels.
 */
export interface ViewTransform {
  tx: number;
  ty: number;
  scale: number;
}

export interface Bounds {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
}

export interface Viewport {
  width: number;
  height: number;
}

export const MIN_SCALE = 0.04;
export const MAX_SCALE = 8;
/** Fits never zoom in past this, so a lone node isn't blown up to fill the screen. */
export const FIT_MAX_SCALE = 2.4;
export const FIT_PADDING = 56;
const ANIMATION_MS = 420;

export function clampScale(scale: number): number {
  if (!Number.isFinite(scale)) return 1;
  return Math.max(MIN_SCALE, Math.min(MAX_SCALE, scale));
}

export function screenToWorld(
  v: ViewTransform,
  sx: number,
  sy: number,
): { x: number; y: number } {
  return { x: (sx - v.tx) / v.scale, y: (sy - v.ty) / v.scale };
}

export function worldToScreen(
  v: ViewTransform,
  wx: number,
  wy: number,
): { x: number; y: number } {
  return { x: wx * v.scale + v.tx, y: wy * v.scale + v.ty };
}

/** Zoom by `factor` keeping the world point under (sx, sy) fixed on screen. */
export function zoomAround(
  v: ViewTransform,
  sx: number,
  sy: number,
  factor: number,
): ViewTransform {
  const scale = clampScale(v.scale * factor);
  const k = scale / v.scale;
  return { scale, tx: sx - (sx - v.tx) * k, ty: sy - (sy - v.ty) * k };
}

/** The transform that puts world point `center` in the middle of the viewport at `scale`. */
export function centerOn(
  center: { x: number; y: number },
  viewport: Viewport,
  scale: number,
): ViewTransform {
  const s = clampScale(scale);
  return {
    scale: s,
    tx: viewport.width / 2 - center.x * s,
    ty: viewport.height / 2 - center.y * s,
  };
}

/**
 * The transform that fits `bounds` inside the viewport with `padding` CSS px on
 * each side. Zero-extent bounds (one node, or nodes on a line) fit on the axis
 * that has extent and cap at `maxScale`. Null while the viewport has no area.
 */
export function fitTransform(
  bounds: Bounds,
  viewport: Viewport,
  opts: { padding?: number; maxScale?: number } = {},
): ViewTransform | null {
  const { width, height } = viewport;
  if (!(width > 0 && height > 0)) return null;
  const padding = Math.min(
    opts.padding ?? FIT_PADDING,
    Math.max(8, Math.min(width, height) * 0.15),
  );
  const maxScale = opts.maxScale ?? FIT_MAX_SCALE;
  const w = Math.max(0, bounds.maxX - bounds.minX);
  const h = Math.max(0, bounds.maxY - bounds.minY);
  const sx = w > 1e-6 ? (width - padding * 2) / w : Infinity;
  const sy = h > 1e-6 ? (height - padding * 2) / h : Infinity;
  const scale = clampScale(Math.min(sx, sy, maxScale));
  return centerOn(
    { x: (bounds.minX + bounds.maxX) / 2, y: (bounds.minY + bounds.maxY) / 2 },
    viewport,
    scale,
  );
}

/**
 * A point along the way from `from` to `to`: the scale changes geometrically
 * and the viewport center moves linearly in world space, so zooming far out
 * and back doesn't swoop.
 */
export function interpolateTransform(
  from: ViewTransform,
  to: ViewTransform,
  t: number,
  viewport: Viewport,
): ViewTransform {
  if (t >= 1) return to;
  if (t <= 0) return from;
  const cx = viewport.width / 2;
  const cy = viewport.height / 2;
  const c0 = screenToWorld(from, cx, cy);
  const c1 = screenToWorld(to, cx, cy);
  const scale = from.scale * Math.pow(to.scale / from.scale, t);
  const x = c0.x + (c1.x - c0.x) * t;
  const y = c0.y + (c1.y - c0.y) * t;
  return { scale, tx: cx - x * scale, ty: cy - y * scale };
}

export function easeInOutCubic(t: number): number {
  return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
}

let reducedMotion: MediaQueryList | null = null;

/** Live: the query list is created once and its `matches` follows the setting. */
export function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function")
    return false;
  reducedMotion ??= window.matchMedia("(prefers-reduced-motion: reduce)");
  return reducedMotion.matches;
}

interface Animation {
  from: ViewTransform;
  to: ViewTransform;
  viewport: Viewport;
  duration: number;
  start: number | null;
}

export interface PanZoom {
  /** The live transform. Read it; change it through the methods below. */
  readonly transformRef: { current: ViewTransform };
  /** Bumps on every change, so a frame loop can tell whether it must redraw. */
  version(): number;
  set(next: ViewTransform): void;
  panBy(dx: number, dy: number): void;
  zoomAt(sx: number, sy: number, factor: number): void;
  /** Moves to `target`, animated unless `animate` is false or the user prefers reduced motion. Replaces any running animation. */
  animateTo(
    target: ViewTransform,
    opts: { viewport: Viewport; animate?: boolean; duration?: number },
  ): void;
  cancelAnimation(): void;
  /** Advances the running animation to `now`. True while it needs more frames. */
  step(now: number): boolean;
  isAnimating(): boolean;
  /** Where the view is headed: the running animation's target, else the current transform. */
  destination(): ViewTransform;
}

/**
 * The view transform lives outside React state (every drag frame would
 * re-render the tree otherwise); `notify` asks the canvas for a frame.
 */
export function createPanZoom(
  notify: () => void,
  initial?: Partial<ViewTransform>,
): PanZoom {
  const transformRef = {
    current: {
      tx: initial?.tx ?? 0,
      ty: initial?.ty ?? 0,
      scale: clampScale(initial?.scale ?? 1),
    },
  };
  let version = 0;
  let animation: Animation | null = null;

  const apply = (next: ViewTransform) => {
    transformRef.current = {
      tx: next.tx,
      ty: next.ty,
      scale: clampScale(next.scale),
    };
    version++;
    notify();
  };

  return {
    transformRef,
    version: () => version,
    set(next) {
      animation = null;
      apply(next);
    },
    panBy(dx, dy) {
      animation = null;
      const cur = transformRef.current;
      apply({ ...cur, tx: cur.tx + dx, ty: cur.ty + dy });
    },
    zoomAt(sx, sy, factor) {
      animation = null;
      apply(zoomAround(transformRef.current, sx, sy, factor));
    },
    animateTo(target, { viewport, animate = true, duration = ANIMATION_MS }) {
      const to = { ...target, scale: clampScale(target.scale) };
      if (!animate || prefersReducedMotion() || duration <= 0) {
        animation = null;
        apply(to);
        return;
      }
      animation = {
        from: transformRef.current,
        to,
        viewport,
        duration,
        start: null,
      };
      notify();
    },
    cancelAnimation() {
      animation = null;
    },
    step(now) {
      const a = animation;
      if (!a) return false;
      if (a.start === null) a.start = now;
      const t = Math.min(1, (now - a.start) / a.duration);
      const next = interpolateTransform(
        a.from,
        a.to,
        easeInOutCubic(t),
        a.viewport,
      );
      transformRef.current = next;
      version++;
      if (t >= 1) {
        animation = null;
        return false;
      }
      return true;
    },
    isAnimating: () => animation !== null,
    destination: () => animation?.to ?? transformRef.current,
  };
}

/** A stable {@link PanZoom} for the component's lifetime; `onChange` may change between renders. */
export function usePanZoom(onChange: () => void): PanZoom {
  const onChangeRef = useRef(onChange);
  useLayoutEffect(() => {
    onChangeRef.current = onChange;
  });
  const [panZoom] = useState(() => createPanZoom(() => onChangeRef.current()));
  return panZoom;
}
