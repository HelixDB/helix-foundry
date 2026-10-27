// Where the canvas's hover tooltip goes. Pure, so the placement rules are easy
// to test.

export interface TooltipPlacement {
  /** False while the anchor is off the canvas: the tooltip must be hidden. */
  visible: boolean;
  /** Top-left corner, in canvas CSS px. */
  left: number;
  top: number;
}

const EDGE_MARGIN = 8;

/**
 * Above the anchor, or below it when there's no room, and always inside the
 * canvas. A hovered node anchors the tooltip at its center, clear of its
 * radius, and hides it while that center is off the canvas; anything else (an
 * edge) anchors it at the pointer.
 */
export function placeTooltip(opts: {
  pointer: { x: number; y: number };
  node: { x: number; y: number; radius: number } | null;
  viewport: { width: number; height: number };
  size: { w: number; h: number };
}): TooltipPlacement {
  const { pointer, node, viewport, size } = opts;
  const { width, height } = viewport;
  let ax = pointer.x;
  let ay = pointer.y;
  let above = 14;
  let below = 22;
  if (node) {
    if (node.x < 0 || node.y < 0 || node.x > width || node.y > height)
      return { visible: false, left: 0, top: 0 };
    ax = node.x;
    ay = node.y;
    above = node.radius + 10;
    below = node.radius + 22;
  }
  const left = Math.max(
    EDGE_MARGIN,
    Math.min(ax - size.w / 2, width - size.w - EDGE_MARGIN),
  );
  let top = ay - above - size.h;
  if (top < EDGE_MARGIN) top = ay + below;
  top = Math.max(EDGE_MARGIN, Math.min(top, height - size.h - EDGE_MARGIN));
  return { visible: true, left, top };
}
