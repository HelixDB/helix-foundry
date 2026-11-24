// The schema canvas's view (suggested-ontology.tsx): where the drawn schema
// sits in its frame and how large. Pure, so the pan and zoom rules are
// tested without a browser.

/** A translation (px, in the frame) and a scale, applied in that order. */
export type View = { x: number; y: number; k: number };
export type Size = { w: number; h: number };
export type Point = [number, number];

/** The scales the reader may zoom between. */
export const ZOOM = { min: 0.2, max: 2 } as const;
/** Room kept between the schema and the frame's edges. */
export const PAD = 24;

const clampK = (k: number) => Math.min(ZOOM.max, Math.max(ZOOM.min, k));

/**
 * The whole schema filling the frame, centred: as large as the frame's
 * tighter side allows, within ZOOM.
 *
 * @example
 * fit({ w: 1000, h: 500 }, { w: 524, h: 400 }) // { x: 24, y: 81, k: 0.476 }
 */
export function fit(content: Size, frame: Size): View {
  const k = clampK(
    Math.min((frame.w - 2 * PAD) / content.w, (frame.h - 2 * PAD) / content.h),
  );
  return clamp({ x: PAD, y: PAD, k }, content, frame);
}

/**
 * Keeps the schema where it can be read: along an axis it fits, centred;
 * along one it overflows, its edges no further in than PAD, as a scrolled
 * box's are. A pan that clamp() undoes is one the frame can't make, so the
 * page may scroll instead.
 *
 * @example
 * clamp({ x: 500, y: 0, k: 1 }, { w: 1000, h: 100 }, { w: 400, h: 300 })
 * // { x: 24, y: 100, k: 1 }: the left edge PAD in, the height centred
 */
export function clamp(v: View, content: Size, frame: Size): View {
  const axis = (at: number, size: number, room: number) => {
    const drawn = size * v.k;
    if (drawn + 2 * PAD <= room) return (room - drawn) / 2;
    return Math.min(PAD, Math.max(room - drawn - PAD, at));
  };
  return {
    x: axis(v.x, content.w, frame.w),
    y: axis(v.y, content.h, frame.h),
    k: v.k,
  };
}

/**
 * Zooms to `k` (within ZOOM) about `at`, a point in the frame, which stays
 * under the pointer or between the fingers.
 *
 * @example
 * zoomAt({ x: 0, y: 0, k: 1 }, 2, [100, 50]) // { x: -100, y: -50, k: 2 }
 */
export function zoomAt(v: View, k: number, at: Point): View {
  const next = clampK(k),
    r = next / v.k;
  return {
    x: at[0] - (at[0] - v.x) * r,
    y: at[1] - (at[1] - v.y) * r,
    k: next,
  };
}

/**
 * The view that shows `box` (in the schema's units) whole, moving as little
 * as it can, or `v` itself when it already does: a card the keyboard
 * reaches comes into view.
 *
 * @example
 * reveal({ x: 0, y: 0, k: 1 }, { x: 900, y: 0, w: 200, h: 100 }, { w: 1000, h: 500 })
 * // { x: -124, y: 24, k: 1 }
 */
export function reveal(
  v: View,
  box: { x: number; y: number; w: number; h: number },
  frame: Size,
): View {
  const axis = (at: number, start: number, size: number, room: number) => {
    const lo = at + start * v.k,
      hi = lo + size * v.k;
    if (lo >= PAD && hi <= room - PAD) return at;
    // Too large to show whole: its start comes into view.
    if (hi - lo > room - 2 * PAD || lo < PAD) return at + PAD - lo;
    return at - (hi - (room - PAD));
  };
  return {
    x: axis(v.x, box.x, box.w, frame.w),
    y: axis(v.y, box.y, box.h, frame.h),
    k: v.k,
  };
}
