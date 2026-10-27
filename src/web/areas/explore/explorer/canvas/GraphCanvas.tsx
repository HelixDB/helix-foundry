import {
  useCallback,
  useEffect,
  useId,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  type Ref,
} from "react";

import { cn } from "../ui";

import { subscribeTheme } from "../../../../shell/theme";
import { CANVAS_THEME } from "../lib/theme";
import type { GraphData, GraphNode, Selection } from "../lib/types";
import { drawScene, FrameScratch, type SceneInput } from "./draw";
import {
  EdgeHitBuffer,
  nearestInDirection,
  nearestTo,
  SpatialGrid,
} from "./hitTest";
import {
  degreeScale,
  edgeHitDistancePx,
  MAX_DEGREE_SCALE,
  nodeHitRadiusPx,
  nodeRadiusPx,
  truncateCaption,
} from "./lod";
import { boundsOf } from "./placement";
import { placeTooltip } from "./tooltip";
import {
  useGraphSimulation,
  type SimNode,
  type SyncResult,
} from "./useGraphSimulation";
import {
  centerOn,
  FIT_MAX_SCALE,
  fitTransform,
  prefersReducedMotion,
  screenToWorld,
  usePanZoom,
  worldToScreen,
  zoomAround,
} from "./usePanZoom";

export interface GraphCanvasHandle {
  fitToView(opts?: { nodeIds?: string[]; animate?: boolean }): void;
  focusNode(id: string, opts?: { animate?: boolean; zoom?: number }): void;
  zoomBy(factor: number): void;
  resetView(): void;
  /** Reheats the simulation; pinned nodes stay put. */
  relayout(): void;
  setPinned(id: string, pinned: boolean): void;
  exportPng(): Promise<Blob | null>;
  /** Client coordinates of a node, for anchoring menus. */
  screenPositionOf(id: string): { x: number; y: number } | null;
}

export interface GraphCanvasProps {
  /** The visible graph (the orchestrator has already filtered hidden labels). */
  graph: GraphData;
  colorForLabel: (label: string) => string;
  captionFor: (node: GraphNode) => string;
  selected: Selection | null;
  /** Nodes to emphasize; everything else dims. null = no emphasis. */
  emphasis: Set<string> | null;
  pinned: ReadonlySet<string>;
  /** Nodes outside what the explorer reads (another load's, or no load's): drawn with a dashed ring. */
  outside?: ReadonlySet<string>;
  onSelect: (selection: Selection | null) => void;
  /** Double-click / double-tap / Enter on a node. */
  onExpand: (nodeId: string) => void;
  /** Right-click, touch long-press, or the context-menu key. */
  onContextMenu: (
    target: Selection | null,
    point: { clientX: number; clientY: number },
  ) => void;
  /** Dragging a node pins it; the orchestrator owns the pinned set. */
  onPinnedChange: (nodeId: string, pinned: boolean) => void;
  ariaLabel: string;
  className?: string;
  handleRef?: Ref<GraphCanvasHandle>;
}

const DRAG_THRESHOLD_PX = { mouse: 3, touch: 6 };
const DOUBLE_TAP_MS = 320;
const DOUBLE_TAP_PX = 12;
const LONG_PRESS_MS = 500;
/** The first layout counts as settled (and is fitted) below this alpha, or after this many ticks. */
const SETTLE_ALPHA = 0.05;
const SETTLE_MAX_TICKS = 220;
/** While the layout moves, the hover grid is rebuilt at most this often. */
const GRID_REBUILD_MS = 80;
const KEY_ZOOM = 1.3;
/** A contextmenu event this soon after the context-menu key (or Shift+F10) came from the keyboard. */
const MENU_KEY_MS = 1000;

interface Press {
  kind: "press";
  pointerId: number;
  pointerType: string;
  startX: number;
  startY: number;
  lastX: number;
  lastY: number;
  /** The node under the pointer at press time; null for empty space (or a middle-button pan). */
  nodeId: string | null;
  /** Node position minus pointer position, in world units, so a drag never snaps the node's center. */
  grabX: number;
  grabY: number;
  mode: "pending" | "pan" | "drag";
  longPress: ReturnType<typeof setTimeout> | null;
  /** A long-press opened the context menu; the release does nothing. */
  consumed: boolean;
}

interface Pinch {
  kind: "pinch";
  distance: number;
  midX: number;
  midY: number;
}

type Gesture = { kind: "idle" } | Press | Pinch;

interface Hover {
  inside: boolean;
  x: number;
  y: number;
  pointerType: string;
  nodeId: string | null;
  edgeId: string | null;
  /** The pointer moved since the last hit test. */
  dirty: boolean;
  positions: number;
  view: number;
  /**
   * The last edge test ran against hit geometry drawn under another view or
   * layout (the previous frame's); test again once this frame's is drawn.
   */
  recheck: boolean;
}

/** A camera move asked for while the canvas had no size; replayed on the first real resize. */
type CameraOp =
  | { kind: "fit"; nodeIds?: string[]; animate: boolean }
  | { kind: "focus"; id: string; zoom?: number; animate: boolean }
  | { kind: "zoom"; factor: number }
  | { kind: "reset" };

/**
 * The explorer's graph view: a 2D canvas over a d3-force layout. It owns the
 * simulation and every node position; the orchestrator owns the data,
 * selection, emphasis and pins and hears about interactions through callbacks.
 */
const NONE: ReadonlySet<string> = new Set();

export function GraphCanvas(props: GraphCanvasProps) {
  const {
    graph,
    selected,
    emphasis,
    pinned,
    colorForLabel,
    captionFor,
    ariaLabel,
    className,
    handleRef,
  } = props;

  const containerRef = useRef<HTMLDivElement>(null);
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const tooltipRef = useRef<HTMLDivElement>(null);
  const tooltipSwatchRef = useRef<HTMLSpanElement>(null);
  const tooltipLabelRef = useRef<HTMLSpanElement>(null);
  const tooltipCaptionRef = useRef<HTMLDivElement>(null);
  const helpId = useId();

  const propsRef = useRef(props);
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null);
  const sizeRef = useRef({ width: 0, height: 0, dpr: 1 });
  const rafRef = useRef(0);
  const frameRef = useRef<(now: number) => void>(() => {});
  const fontRef = useRef("ui-sans-serif, system-ui, sans-serif");
  const measureRef = useRef(new Map<string, number>());
  const scratchRef = useRef(new FrameScratch());
  const hitsRef = useRef(new EdgeHitBuffer());
  /**
   * The versions the hit buffer was filled under. Edge indices are only valid
   * while `structure` matches; the geometry is current while `view` and
   * `positions` do.
   */
  const hitsKeyRef = useRef({ structure: -1, view: -1, positions: -1 });
  const gridRef = useRef({
    index: new SpatialGrid<SimNode>(),
    positions: -1,
    structure: -1,
    builtAt: 0,
  });
  const gestureRef = useRef<Gesture>({ kind: "idle" });
  const pointersRef = useRef(
    new Map<number, { x: number; y: number; type: string }>(),
  );
  const hoverRef = useRef<Hover>({
    inside: false,
    x: 0,
    y: 0,
    pointerType: "mouse",
    nodeId: null,
    edgeId: null,
    dirty: false,
    positions: -1,
    view: -1,
    recheck: false,
  });
  const tooltipSizeRef = useRef({ w: 0, h: 0 });
  const lastTapRef = useRef<{
    nodeId: string;
    at: number;
    x: number;
    y: number;
  } | null>(null);
  const longPressAtRef = useRef(-Infinity);
  /** When the context-menu key (or Shift+F10) last went down on the canvas. */
  const menuKeyAtRef = useRef(-Infinity);
  /** Auto-fit of the first graph: armed while empty, waits for the layout to settle, then done. */
  const fitRef = useRef<{ state: "idle" | "settling" | "done"; since: number }>(
    { state: "idle", since: 0 },
  );
  /** Camera moves asked for while the canvas had no size; they run, in order, on the first real resize. */
  const pendingCameraRef = useRef<CameraOp[]>([]);
  /**
   * The last size with area, so hiding the canvas (display: none) and showing
   * it again keeps the view. A ref, not an effect local: StrictMode re-runs
   * the sizing effect, and the re-run must not forget it.
   */
  const shownRef = useRef<{ width: number; height: number } | null>(null);
  const derivedRef = useRef({
    data: -1,
    colorFn: null as GraphCanvasProps["colorForLabel"] | null,
    palette: [] as string[],
    colorIds: new Uint16Array(0),
    sizes: new Float32Array(0),
    captionFn: null as GraphCanvasProps["captionFor"] | null,
    captions: [] as (string | undefined)[],
    emphasisSet: undefined as Set<string> | null | undefined,
    emphasis: null as Uint8Array | null,
  });

  const requestFrame = useCallback(() => {
    if (
      rafRef.current ||
      typeof window === "undefined" ||
      !window.requestAnimationFrame
    )
      return;
    rafRef.current = window.requestAnimationFrame((now) => {
      rafRef.current = 0;
      frameRef.current(now);
    });
  }, []);

  const view = usePanZoom(requestFrame);

  useLayoutEffect(() => {
    propsRef.current = props;
  });

  // Sizing runs before the first graph sync, so placement and the first fit
  // see the real viewport.
  useLayoutEffect(() => {
    const canvas = canvasRef.current;
    const container = containerRef.current;
    if (!canvas || !container) return;
    ctxRef.current = canvas.getContext("2d");

    const apply = () => {
      const rect = container.getBoundingClientRect();
      const dpr = Math.max(1, window.devicePixelRatio || 1);
      const prev = sizeRef.current;
      const { width, height } = rect;
      if (width === prev.width && height === prev.height && dpr === prev.dpr)
        return;
      sizeRef.current = { width, height, dpr };
      canvas.width = Math.max(1, Math.round(width * dpr));
      canvas.height = Math.max(1, Math.round(height * dpr));
      if (!(width > 0 && height > 0)) return;
      const shown = shownRef.current;
      if (shown && (width !== shown.width || height !== shown.height)) {
        // Keep the middle of the view in the middle when a panel opens or closes.
        const cur = view.transformRef.current;
        view.set({
          ...cur,
          tx: cur.tx + (width - shown.width) / 2,
          ty: cur.ty + (height - shown.height) / 2,
        });
      }
      const pending = pendingCameraRef.current;
      pendingCameraRef.current = [];
      // The first size: start with the world origin in the middle, unless a
      // queued move sets the whole view anyway.
      if (!shown && (pending.length === 0 || pending[0].kind === "zoom")) {
        view.set(
          centerOn(
            { x: 0, y: 0 },
            { width, height },
            view.transformRef.current.scale,
          ),
        );
      }
      shownRef.current = { width, height };
      // Moves queued while hidden only animate if the canvas was seen before.
      for (const op of pending)
        handlersRef.current.runCamera(op, shown !== null);
      requestFrame();
    };

    // devicePixelRatio changes (moving between monitors, browser zoom) don't
    // fire a resize on the container; a resolution query does.
    let resolution: MediaQueryList | null = null;
    const onResolution = () => {
      apply();
      watchResolution();
    };
    const watchResolution = () => {
      resolution?.removeEventListener("change", onResolution);
      if (typeof window.matchMedia !== "function") return;
      resolution = window.matchMedia(
        `(resolution: ${window.devicePixelRatio || 1}dppx)`,
      );
      resolution.addEventListener("change", onResolution);
    };

    apply();
    watchResolution();
    const observer =
      typeof ResizeObserver !== "undefined" ? new ResizeObserver(apply) : null;
    observer?.observe(container);
    return () => {
      observer?.disconnect();
      resolution?.removeEventListener("change", onResolution);
    };
  }, [requestFrame, view]);

  const layout = useGraphSimulation(graph, {
    pinned,
    spawnCenter: () => {
      const { width, height } = sizeRef.current;
      return screenToWorld(view.transformRef.current, width / 2, height / 2);
    },
    onSync: (result) => handlersRef.current.afterSync(result),
  });

  // ---- Derived per-graph data -------------------------------------------

  /**
   * Colors, sizes, captions and the emphasis mask, aligned with layout.nodes.
   * Recomputed when the data or the props they come from change, never per
   * frame.
   */
  function derived() {
    const d = derivedRef.current;
    const p = propsRef.current;
    const dataChanged = d.data !== layout.dataVersion;
    if (dataChanged) {
      d.sizes = Float32Array.from(layout.nodes, (n) => degreeScale(n.degree));
    }
    if (dataChanged || d.colorFn !== p.colorForLabel) {
      // One colorForLabel call per label; nodes keep an index into the palette.
      const idByLabel = new Map<string, number>();
      const idByColor = new Map<string, number>();
      const palette: string[] = [];
      d.colorIds = Uint16Array.from(layout.nodes, (n) => {
        let id = idByLabel.get(n.label);
        if (id === undefined) {
          const color = p.colorForLabel(n.label);
          id = idByColor.get(color);
          if (id === undefined) {
            id = palette.length;
            palette.push(color);
            idByColor.set(color, id);
          }
          idByLabel.set(n.label, id);
        }
        return id;
      });
      d.palette = palette;
      d.colorFn = p.colorForLabel;
    }
    if (dataChanged || d.captionFn !== p.captionFor) {
      d.captions = new Array(layout.nodes.length);
      d.captionFn = p.captionFor;
    }
    if (dataChanged || d.emphasisSet !== p.emphasis) {
      if (p.emphasis) {
        const mask = new Uint8Array(layout.nodes.length);
        for (const id of p.emphasis) {
          const i = layout.indexById.get(id);
          if (i !== undefined) mask[i] = 1;
        }
        d.emphasis = mask;
      } else {
        d.emphasis = null;
      }
      d.emphasisSet = p.emphasis;
    }
    d.data = layout.dataVersion;
    return d;
  }

  function captionAt(i: number): string {
    const d = derivedRef.current;
    let caption = d.captions[i];
    if (caption === undefined) {
      const node = layout.graphNodes[i];
      caption = node ? truncateCaption(propsRef.current.captionFor(node)) : "";
      d.captions[i] = caption;
    }
    return caption;
  }

  function sceneFor(live: boolean): SceneInput {
    const d = derived();
    const p = propsRef.current;
    const sel = p.selected;
    const hover = hoverRef.current;
    return {
      layout,
      palette: d.palette,
      colorIds: d.colorIds,
      sizes: d.sizes,
      captionAt,
      view: view.transformRef.current,
      ...sizeRef.current,
      selectedNode:
        sel?.kind === "node" ? (layout.indexById.get(sel.id) ?? -1) : -1,
      selectedEdge:
        sel?.kind === "edge" ? (layout.edgeIndexById.get(sel.id) ?? -1) : -1,
      hoveredNode:
        live && hover.nodeId ? (layout.indexById.get(hover.nodeId) ?? -1) : -1,
      hoveredEdge:
        live && hover.edgeId
          ? (layout.edgeIndexById.get(hover.edgeId) ?? -1)
          : -1,
      emphasis: d.emphasis,
      pinned: p.pinned,
      outside: p.outside ?? NONE,
      fontFamily: fontRef.current,
      measure: measureRef.current,
      hits: live ? hitsRef.current : new EdgeHitBuffer(),
      scratch: live ? scratchRef.current : new FrameScratch(),
    };
  }

  // ---- Frame loop --------------------------------------------------------

  /**
   * One frame: advance the layout and any camera animation, draw, and ask for
   * another frame only while something still moves. Input events request
   * frames themselves, so an idle canvas costs nothing.
   */
  function frame(now: number) {
    const ctx = ctxRef.current;
    if (!ctx) return;
    if (layout.isActive()) {
      layout.tick();
      // Reduced motion: settle in a few frames instead of a few seconds.
      if (prefersReducedMotion()) {
        const until = performance.now() + 10;
        for (
          let i = 0;
          i < 8 && layout.isActive() && performance.now() < until;
          i++
        )
          layout.tick();
      }
    }
    view.step(now);
    checkSettled();
    updateHover();
    drawScene(ctx, sceneFor(true));
    const hits = hitsKeyRef.current;
    hits.structure = layout.structureVersion;
    hits.view = view.version();
    hits.positions = layout.positionsVersion;
    positionTooltip();
    const hover = hoverRef.current;
    if (hover.recheck) {
      // The edge hover above used the last frame's geometry; the camera or the
      // layout has moved since. Test once more against what was just drawn.
      hover.recheck = false;
      hover.dirty = true;
      requestFrame();
    }
    if (layout.isActive() || view.isAnimating()) requestFrame();
  }

  function checkSettled() {
    const fit = fitRef.current;
    if (fit.state !== "settling") return;
    const settled =
      !layout.isActive() ||
      layout.alpha() < SETTLE_ALPHA ||
      layout.ticks - fit.since >= SETTLE_MAX_TICKS;
    if (!settled) return;
    fit.state = "done";
    fitNodes(undefined, true);
  }

  function afterSync(result: SyncResult) {
    if (layout.nodes.length === 0) {
      fitRef.current = { state: "idle", since: 0 };
    } else if (result.firstNodes && fitRef.current.state === "idle") {
      // A few synchronous ticks so the first frame isn't a single blob, then
      // a guess at the settled size so the graph grows into view; the real
      // fit follows once the layout settles.
      const reduced = prefersReducedMotion();
      layout.warmUp(reduced ? 300 : 40, reduced ? 200 : 32);
      fitRef.current = { state: "settling", since: layout.ticks };
      guessInitialView();
    }
    const hover = hoverRef.current;
    if (hover.nodeId && !layout.indexById.has(hover.nodeId))
      hover.nodeId = null;
    if (hover.edgeId && !layout.edgeIndexById.has(hover.edgeId))
      hover.edgeId = null;
    hover.dirty = true;
    renderTooltipContent();
    requestFrame();
  }

  function guessInitialView() {
    const { width, height } = sizeRef.current;
    const bounds = boundsOf(layout.nodes);
    if (!bounds) return;
    if (!(width > 0 && height > 0)) {
      deferCamera({ kind: "fit", animate: false });
      return;
    }
    // A settled force layout covers roughly 15·√n world units of radius.
    const radius = 15 * Math.sqrt(layout.nodes.length) + 40;
    const scale = Math.min(
      FIT_MAX_SCALE,
      Math.min(width, height) / (2 * radius),
    );
    const center = {
      x: (bounds.minX + bounds.maxX) / 2,
      y: (bounds.minY + bounds.maxY) / 2,
    };
    view.set(centerOn(center, { width, height }, scale));
  }

  /** Stops the first-graph auto-fit from moving the camera once the user has. */
  function markInteracted() {
    if (fitRef.current.state === "settling") fitRef.current.state = "done";
  }

  function viewport() {
    const { width, height } = sizeRef.current;
    return { width, height };
  }

  /** False while the canvas is hidden or not laid out yet: camera targets would be computed for a 0×0 view. */
  function hasArea() {
    const { width, height } = sizeRef.current;
    return width > 0 && height > 0;
  }

  function deferCamera(op: CameraOp) {
    const queue = pendingCameraRef.current;
    if (op.kind === "zoom") {
      const last = queue[queue.length - 1];
      if (last?.kind === "zoom") {
        last.factor *= op.factor;
        return;
      }
    } else {
      // A fit, focus or reset sets the whole view: nothing queued before it matters.
      queue.length = 0;
    }
    queue.push(op);
  }

  /** Replays a queued camera move. `mayAnimate` is false when the canvas is being shown for the first time. */
  function runCamera(op: CameraOp, mayAnimate: boolean) {
    switch (op.kind) {
      case "fit":
        fitNodes(op.nodeIds, op.animate && mayAnimate);
        break;
      case "focus":
        focusNode(op.id, { zoom: op.zoom, animate: op.animate && mayAnimate });
        break;
      case "zoom":
        zoomBy(op.factor, mayAnimate);
        break;
      case "reset":
        resetView(mayAnimate);
        break;
    }
  }

  function fitNodes(nodeIds: string[] | undefined, animate: boolean): boolean {
    const points: SimNode[] = [];
    if (nodeIds) {
      for (const id of nodeIds) {
        const i = layout.indexById.get(id);
        if (i !== undefined) points.push(layout.nodes[i]);
      }
    }
    const bounds = boundsOf(nodeIds ? points : layout.nodes);
    if (!bounds) return false;
    const target = fitTransform(bounds, viewport());
    if (!target) {
      deferCamera({ kind: "fit", nodeIds, animate });
      return false;
    }
    view.animateTo(target, { viewport: viewport(), animate });
    return true;
  }

  // ---- Hit testing -------------------------------------------------------

  function local(e: { clientX: number; clientY: number }) {
    const rect = canvasRef.current!.getBoundingClientRect();
    return { x: e.clientX - rect.left, y: e.clientY - rect.top };
  }

  function nodeAt(
    x: number,
    y: number,
    pointerType: string,
    fresh: boolean,
  ): SimNode | null {
    const grid = gridRef.current;
    const stale = grid.positions !== layout.positionsVersion;
    const now = performance.now();
    if (
      grid.structure !== layout.structureVersion ||
      (stale &&
        (fresh || !layout.isActive() || now - grid.builtAt > GRID_REBUILD_MS))
    ) {
      grid.index.rebuild(layout.nodes);
      grid.positions = layout.positionsVersion;
      grid.structure = layout.structureVersion;
      grid.builtAt = now;
    }
    const v = view.transformRef.current;
    const w = screenToWorld(v, x, y);
    const r = nodeRadiusPx(v.scale);
    const sizes = derived().sizes;
    return grid.index.nearest(
      w.x,
      w.y,
      nodeHitRadiusPx(r * MAX_DEGREE_SCALE, pointerType) / v.scale,
      (node) =>
        nodeHitRadiusPx(
          r * (sizes[layout.indexById.get(node.id) ?? -1] ?? 1),
          pointerType,
        ) / v.scale,
    );
  }

  function edgeAt(x: number, y: number, pointerType: string): string | null {
    if (hitsKeyRef.current.structure !== layout.structureVersion) return null;
    const i = hitsRef.current.nearest(x, y, edgeHitDistancePx(pointerType));
    return i >= 0 ? (layout.edges[i]?.id ?? null) : null;
  }

  function targetAt(
    x: number,
    y: number,
    pointerType: string,
  ): Selection | null {
    const node = nodeAt(x, y, pointerType, true);
    if (node) return { kind: "node", id: node.id };
    const edge = edgeAt(x, y, pointerType);
    return edge ? { kind: "edge", id: edge } : null;
  }

  function setCursor(cursor: string) {
    const canvas = canvasRef.current;
    if (canvas && canvas.style.cursor !== cursor) canvas.style.cursor = cursor;
  }

  // ---- Hover + tooltip ---------------------------------------------------

  function updateHover() {
    const hover = hoverRef.current;
    const idle = gestureRef.current.kind === "idle";
    if (!hover.inside || !idle || hover.pointerType === "touch") {
      if (hover.nodeId || hover.edgeId) {
        hover.nodeId = null;
        hover.edgeId = null;
        renderTooltipContent();
      }
      return;
    }
    if (
      !hover.dirty &&
      hover.positions === layout.positionsVersion &&
      hover.view === view.version()
    )
      return;
    hover.dirty = false;
    hover.positions = layout.positionsVersion;
    hover.view = view.version();
    const node = nodeAt(hover.x, hover.y, hover.pointerType, false);
    const nodeId = node?.id ?? null;
    const edgeId = nodeId ? null : edgeAt(hover.x, hover.y, hover.pointerType);
    // Nodes are tested against live positions, edges against the geometry the
    // last frame drew; when that is out of date, the frame tests again.
    const hits = hitsKeyRef.current;
    hover.recheck =
      !nodeId &&
      (hits.view !== hover.view || hits.positions !== hover.positions);
    if (nodeId !== hover.nodeId || edgeId !== hover.edgeId) {
      hover.nodeId = nodeId;
      hover.edgeId = edgeId;
      renderTooltipContent();
    }
    setCursor(nodeId || edgeId ? "pointer" : "grab");
  }

  function renderTooltipContent() {
    const el = tooltipRef.current;
    const swatch = tooltipSwatchRef.current;
    const label = tooltipLabelRef.current;
    const caption = tooltipCaptionRef.current;
    if (!el || !swatch || !label || !caption) return;
    const hover = hoverRef.current;
    const p = propsRef.current;
    const nodeIndex = hover.nodeId
      ? layout.indexById.get(hover.nodeId)
      : undefined;
    const edgeIndex = hover.edgeId
      ? layout.edgeIndexById.get(hover.edgeId)
      : undefined;
    if (nodeIndex !== undefined) {
      const node = layout.graphNodes[nodeIndex];
      swatch.style.display = "";
      const d = derived();
      swatch.style.background =
        d.palette[d.colorIds[nodeIndex]] ?? CANVAS_THEME.caption;
      label.textContent = node.label;
      caption.textContent = truncateCaption(p.captionFor(node), 80);
    } else if (edgeIndex !== undefined) {
      const edge = layout.graphEdges[edgeIndex];
      const source = layout.graphNodes[layout.edges[edgeIndex].source];
      const target = layout.graphNodes[layout.edges[edgeIndex].target];
      swatch.style.display = "none";
      label.textContent = edge.label;
      caption.textContent = `${truncateCaption(p.captionFor(source), 32)} → ${truncateCaption(p.captionFor(target), 32)}`;
    } else {
      el.style.display = "none";
      return;
    }
    el.style.display = "block";
    tooltipSizeRef.current = { w: el.offsetWidth, h: el.offsetHeight };
  }

  /** Keeps the tooltip on its node (which may be moving) and inside the canvas. */
  function positionTooltip() {
    const el = tooltipRef.current;
    if (!el || el.style.display !== "block") return;
    const hover = hoverRef.current;
    const v = view.transformRef.current;
    const i = hover.nodeId ? layout.indexById.get(hover.nodeId) : undefined;
    let node: { x: number; y: number; radius: number } | null = null;
    if (i !== undefined) {
      const n = layout.nodes[i];
      const p = worldToScreen(v, n.x ?? 0, n.y ?? 0);
      node = {
        x: p.x,
        y: p.y,
        radius: nodeRadiusPx(v.scale) * (derived().sizes[i] ?? 1),
      };
    }
    const place = placeTooltip({
      pointer: hover,
      node,
      viewport: sizeRef.current,
      size: tooltipSizeRef.current,
    });
    // Written on every placement: a node that hid the tooltip must not keep
    // the next (edge) tooltip hidden.
    el.style.visibility = place.visible ? "visible" : "hidden";
    if (!place.visible) return;
    el.style.transform = `translate(${Math.round(place.left)}px, ${Math.round(place.top)}px)`;
  }

  // ---- Pointer input -----------------------------------------------------

  function onPointerDown(e: PointerEvent) {
    const canvas = canvasRef.current;
    if (!canvas) return;
    // A pointer is in play: the next context menu isn't the keyboard's.
    menuKeyAtRef.current = -Infinity;
    if (e.pointerType === "mouse" && e.button !== 0 && e.button !== 1) return;
    const { x, y } = local(e);
    pointersRef.current.set(e.pointerId, { x, y, type: e.pointerType });
    try {
      canvas.setPointerCapture(e.pointerId);
    } catch {
      // The pointer may already be gone (fast taps on some browsers).
    }
    view.cancelAnimation();

    const touches = [...pointersRef.current.values()].filter(
      (p) => p.type === "touch",
    );
    if (e.pointerType === "touch" && touches.length === 2) {
      // A second finger turns the gesture into a pinch. A node being dragged
      // stays where it was dropped, pinned, like any other drag.
      endPress();
      const [a, b] = touches;
      gestureRef.current = {
        kind: "pinch",
        distance: Math.hypot(a.x - b.x, a.y - b.y),
        midX: (a.x + b.x) / 2,
        midY: (a.y + b.y) / 2,
      };
      markInteracted();
      return;
    }
    if (gestureRef.current.kind !== "idle") return;

    const node = e.button === 1 ? null : nodeAt(x, y, e.pointerType, true);
    const world = screenToWorld(view.transformRef.current, x, y);
    const press: Press = {
      kind: "press",
      pointerId: e.pointerId,
      pointerType: e.pointerType,
      startX: x,
      startY: y,
      lastX: x,
      lastY: y,
      nodeId: node?.id ?? null,
      grabX: node ? (node.x ?? 0) - world.x : 0,
      grabY: node ? (node.y ?? 0) - world.y : 0,
      mode: e.button === 1 ? "pan" : "pending",
      longPress: null,
      consumed: false,
    };
    if (e.pointerType === "touch") {
      press.longPress = setTimeout(() => {
        press.longPress = null;
        if (gestureRef.current !== press || press.mode !== "pending") return;
        press.consumed = true;
        longPressAtRef.current = performance.now();
        const rect = canvas.getBoundingClientRect();
        propsRef.current.onContextMenu(
          targetAt(press.startX, press.startY, "touch"),
          {
            clientX: rect.left + press.startX,
            clientY: rect.top + press.startY,
          },
        );
      }, LONG_PRESS_MS);
    }
    gestureRef.current = press;
    hoverRef.current.dirty = true;
    setCursor(node ? "pointer" : "grabbing");
    requestFrame();
  }

  function onPointerMove(e: PointerEvent) {
    const { x, y } = local(e);
    const tracked = pointersRef.current.get(e.pointerId);
    if (tracked) {
      tracked.x = x;
      tracked.y = y;
    }
    const gesture = gestureRef.current;

    if (gesture.kind === "pinch") {
      const touches = [...pointersRef.current.values()].filter(
        (p) => p.type === "touch",
      );
      if (touches.length < 2) return;
      const [a, b] = touches;
      const distance = Math.hypot(a.x - b.x, a.y - b.y);
      const midX = (a.x + b.x) / 2;
      const midY = (a.y + b.y) / 2;
      view.panBy(midX - gesture.midX, midY - gesture.midY);
      if (gesture.distance > 0 && distance > 0)
        view.zoomAt(midX, midY, distance / gesture.distance);
      gesture.distance = distance;
      gesture.midX = midX;
      gesture.midY = midY;
      return;
    }

    if (gesture.kind === "press" && gesture.pointerId === e.pointerId) {
      if (gesture.consumed) return;
      if (gesture.mode === "pending") {
        const threshold =
          e.pointerType === "touch"
            ? DRAG_THRESHOLD_PX.touch
            : DRAG_THRESHOLD_PX.mouse;
        if (Math.hypot(x - gesture.startX, y - gesture.startY) < threshold)
          return;
        if (gesture.longPress) clearTimeout(gesture.longPress);
        gesture.longPress = null;
        markInteracted();
        if (gesture.nodeId && layout.indexById.has(gesture.nodeId)) {
          gesture.mode = "drag";
          layout.startDrag(gesture.nodeId);
        } else {
          gesture.mode = "pan";
        }
        setCursor("grabbing");
      }
      if (gesture.mode === "pan") {
        view.panBy(x - gesture.lastX, y - gesture.lastY);
      } else if (gesture.mode === "drag" && gesture.nodeId) {
        const world = screenToWorld(view.transformRef.current, x, y);
        layout.dragTo(
          gesture.nodeId,
          world.x + gesture.grabX,
          world.y + gesture.grabY,
        );
        requestFrame();
      }
      gesture.lastX = x;
      gesture.lastY = y;
      return;
    }

    if (gesture.kind === "idle" && e.pointerType !== "touch") {
      const hover = hoverRef.current;
      hover.inside = true;
      hover.x = x;
      hover.y = y;
      hover.pointerType = e.pointerType;
      hover.dirty = true;
      requestFrame();
    }
  }

  function onPointerUp(e: PointerEvent) {
    const wasTracked = pointersRef.current.delete(e.pointerId);
    const canvas = canvasRef.current;
    try {
      if (canvas?.hasPointerCapture(e.pointerId))
        canvas.releasePointerCapture(e.pointerId);
    } catch {
      // Already released.
    }
    const gesture = gestureRef.current;

    if (gesture.kind === "pinch") {
      // Lifting one finger hands the pan to the one left down.
      const rest = [...pointersRef.current.entries()].find(
        ([, p]) => p.type === "touch",
      );
      if (rest) {
        const [pointerId, p] = rest;
        gestureRef.current = {
          kind: "press",
          pointerId,
          pointerType: "touch",
          startX: p.x,
          startY: p.y,
          lastX: p.x,
          lastY: p.y,
          nodeId: null,
          grabX: 0,
          grabY: 0,
          mode: "pan",
          longPress: null,
          consumed: false,
        };
      } else {
        gestureRef.current = { kind: "idle" };
      }
      return;
    }

    if (
      gesture.kind !== "press" ||
      gesture.pointerId !== e.pointerId ||
      !wasTracked
    )
      return;
    const { x, y } = local(e);
    const cancelled = e.type === "pointercancel";
    const tap = gesture.mode === "pending" && !gesture.consumed && !cancelled;
    const nodeId = gesture.nodeId;
    endPress();
    if (tap) handleTap(nodeId, x, y, e.pointerType);
    hoverRef.current.dirty = true;
    hoverRef.current.x = x;
    hoverRef.current.y = y;
    setCursor("grab");
    requestFrame();
  }

  /**
   * Finishes the current press. A drag leaves its node pinned where it was
   * dropped, and the orchestrator hears about it, so the layout and the pinned
   * set always agree.
   */
  function endPress() {
    const gesture = gestureRef.current;
    if (gesture.kind !== "press") return;
    if (gesture.longPress) clearTimeout(gesture.longPress);
    gestureRef.current = { kind: "idle" };
    if (gesture.mode !== "drag" || !gesture.nodeId) return;
    const id = gesture.nodeId;
    // False when the node left the graph mid-drag: nothing was left fixed.
    const live = layout.draggingId === id;
    layout.endDrag(id, true);
    if (live && layout.indexById.has(id) && !propsRef.current.pinned.has(id)) {
      propsRef.current.onPinnedChange(id, true);
    }
  }

  function handleTap(
    nodeId: string | null,
    x: number,
    y: number,
    pointerType: string,
  ) {
    const p = propsRef.current;
    if (nodeId && layout.indexById.has(nodeId)) {
      const now = performance.now();
      const last = lastTapRef.current;
      if (
        last &&
        last.nodeId === nodeId &&
        now - last.at < DOUBLE_TAP_MS &&
        Math.hypot(x - last.x, y - last.y) < DOUBLE_TAP_PX
      ) {
        lastTapRef.current = null;
        p.onExpand(nodeId);
        return;
      }
      lastTapRef.current = { nodeId, at: now, x, y };
      p.onSelect({ kind: "node", id: nodeId });
      return;
    }
    lastTapRef.current = null;
    const edgeId = edgeAt(x, y, pointerType);
    p.onSelect(edgeId ? { kind: "edge", id: edgeId } : null);
  }

  function onPointerLeave(e: PointerEvent) {
    if (e.pointerType === "touch") return;
    const hover = hoverRef.current;
    hover.inside = false;
    hover.dirty = true;
    requestFrame();
  }

  function onWheel(e: WheelEvent) {
    e.preventDefault();
    const { x, y } = local(e);
    const unit =
      e.deltaMode === 1
        ? 16
        : e.deltaMode === 2
          ? sizeRef.current.height || 800
          : 1;
    // Trackpad pinches arrive as ctrlKey wheels with small deltas.
    const k = e.ctrlKey ? 0.01 : 0.0018;
    const factor = Math.min(2, Math.max(0.5, Math.exp(-e.deltaY * unit * k)));
    markInteracted();
    view.zoomAt(x, y, factor);
    hoverRef.current.dirty = true;
  }

  function onContextMenu(e: MouseEvent) {
    e.preventDefault();
    const p = propsRef.current;
    const canvas = canvasRef.current;
    if (!canvas) return;
    // A touch long-press already opened the menu; Android also fires this.
    if (performance.now() - longPressAtRef.current < 800) return;
    const menuKey = performance.now() - menuKeyAtRef.current < MENU_KEY_MS;
    menuKeyAtRef.current = -Infinity;
    const gesture = gestureRef.current;
    if (
      gesture.kind === "press" &&
      (gesture.pointerType === "touch" || gesture.mode === "pending")
    ) {
      // The menu takes the press: its release is not a tap. That covers a
      // touch long-press and macOS Ctrl+click, which arrives as a primary
      // button press followed by this event.
      if (gesture.longPress) clearTimeout(gesture.longPress);
      gesture.longPress = null;
      gesture.consumed = true;
      if (gesture.pointerType === "touch")
        longPressAtRef.current = performance.now();
    }
    const rect = canvas.getBoundingClientRect();
    // The context-menu key: Chromium puts the event at the canvas center, so
    // the key press itself is the signal; the coordinate checks catch
    // browsers that send (0, 0) or the focused element's corner.
    const fromKeyboard =
      menuKey ||
      (gesture.kind !== "press" &&
        ((e.clientX === 0 && e.clientY === 0) ||
          e.clientX < rect.left ||
          e.clientX > rect.right ||
          e.clientY < rect.top ||
          e.clientY > rect.bottom));
    if (fromKeyboard) {
      // The context-menu key: open it on the selection.
      const sel = p.selected;
      const at = sel ? selectionPoint(sel) : null;
      p.onContextMenu(sel, {
        clientX: at?.x ?? rect.left + rect.width / 2,
        clientY: at?.y ?? rect.top + rect.height / 2,
      });
      return;
    }
    const { x, y } = local(e);
    p.onContextMenu(
      targetAt(x, y, gesture.kind === "press" ? gesture.pointerType : "mouse"),
      {
        clientX: e.clientX,
        clientY: e.clientY,
      },
    );
  }

  // ---- Keyboard ----------------------------------------------------------

  function onKeyDown(e: KeyboardEvent) {
    if (
      e.key === "ContextMenu" ||
      (e.key === "F10" && e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey)
    ) {
      // Not prevented: the browser follows with the contextmenu event, which
      // then opens the menu on the selection.
      menuKeyAtRef.current = performance.now();
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    const p = propsRef.current;
    const sel = p.selected;
    switch (e.key) {
      case "ArrowUp":
        moveSelection(0, -1);
        break;
      case "ArrowDown":
        moveSelection(0, 1);
        break;
      case "ArrowLeft":
        moveSelection(-1, 0);
        break;
      case "ArrowRight":
        moveSelection(1, 0);
        break;
      case "Enter":
        if (sel?.kind !== "node" || !layout.indexById.has(sel.id)) return;
        p.onExpand(sel.id);
        break;
      case "Escape":
        if (!sel) return;
        p.onSelect(null);
        break;
      case "+":
      case "=":
        zoomBy(KEY_ZOOM);
        break;
      case "-":
      case "_":
        zoomBy(1 / KEY_ZOOM);
        break;
      case "0":
        resetView();
        break;
      case "f":
      case "F":
        markInteracted();
        fitNodes(undefined, true);
        break;
      default:
        return;
    }
    e.preventDefault();
    e.stopPropagation();
  }

  /** Arrow keys walk the graph: select the nearest node that way and keep it in view. */
  function moveSelection(dx: number, dy: number) {
    if (layout.nodes.length === 0) return;
    const p = propsRef.current;
    const from = selectionWorldPoint(p.selected);
    let next: number;
    if (from) {
      const skip =
        p.selected?.kind === "node"
          ? (layout.indexById.get(p.selected.id) ?? -1)
          : -1;
      next = nearestInDirection(layout.nodes, from, dx, dy, skip);
    } else {
      const { width, height } = sizeRef.current;
      next = nearestTo(
        layout.nodes,
        screenToWorld(view.transformRef.current, width / 2, height / 2),
      );
    }
    if (next < 0) return;
    const node = layout.nodes[next];
    p.onSelect({ kind: "node", id: node.id });
    // Pan (never zoom) when the new selection is near the edge or off screen.
    // Without a size there is no edge to be near.
    if (!hasArea()) return;
    const { width, height } = sizeRef.current;
    const v = view.destination();
    const s = worldToScreen(v, node.x ?? 0, node.y ?? 0);
    const margin = Math.min(80, Math.min(width, height) * 0.15);
    if (
      s.x < margin ||
      s.y < margin ||
      s.x > width - margin ||
      s.y > height - margin
    ) {
      markInteracted();
      view.animateTo(
        centerOn({ x: node.x ?? 0, y: node.y ?? 0 }, viewport(), v.scale),
        {
          viewport: viewport(),
          duration: 260,
        },
      );
    }
  }

  function selectionWorldPoint(
    sel: Selection | null,
  ): { x: number; y: number } | null {
    if (!sel) return null;
    if (sel.kind === "node") {
      const i = layout.indexById.get(sel.id);
      return i === undefined
        ? null
        : { x: layout.nodes[i].x ?? 0, y: layout.nodes[i].y ?? 0 };
    }
    const i = layout.edgeIndexById.get(sel.id);
    if (i === undefined) return null;
    const s = layout.nodes[layout.edges[i].source];
    const t = layout.nodes[layout.edges[i].target];
    return {
      x: ((s.x ?? 0) + (t.x ?? 0)) / 2,
      y: ((s.y ?? 0) + (t.y ?? 0)) / 2,
    };
  }

  /** Client coordinates of a selection (node center, or edge midpoint). */
  function selectionPoint(sel: Selection): { x: number; y: number } | null {
    const canvas = canvasRef.current;
    const w = selectionWorldPoint(sel);
    if (!canvas || !w) return null;
    const rect = canvas.getBoundingClientRect();
    const s = worldToScreen(view.transformRef.current, w.x, w.y);
    return { x: rect.left + s.x, y: rect.top + s.y };
  }

  // ---- Handle --------------------------------------------------------------

  function zoomBy(factor: number, animate = true) {
    markInteracted();
    if (!hasArea()) {
      deferCamera({ kind: "zoom", factor });
      return;
    }
    const { width, height } = sizeRef.current;
    view.animateTo(
      zoomAround(view.destination(), width / 2, height / 2, factor),
      {
        viewport: viewport(),
        duration: 200,
        animate,
      },
    );
  }

  function resetView(animate = true) {
    markInteracted();
    if (!hasArea()) {
      deferCamera({ kind: "reset" });
      return;
    }
    const bounds = boundsOf(layout.nodes);
    const center = bounds
      ? {
          x: (bounds.minX + bounds.maxX) / 2,
          y: (bounds.minY + bounds.maxY) / 2,
        }
      : { x: 0, y: 0 };
    view.animateTo(centerOn(center, viewport(), 1), {
      viewport: viewport(),
      animate,
    });
  }

  function focusNode(
    id: string,
    opts: { animate?: boolean; zoom?: number } = {},
  ) {
    const i = layout.indexById.get(id);
    if (i === undefined) return;
    markInteracted();
    if (!hasArea()) {
      // Hidden (say, behind the query dock on a phone) or not measured yet:
      // centering on a 0×0 view would put the node in the corner.
      deferCamera({
        kind: "focus",
        id,
        zoom: opts.zoom,
        animate: opts.animate ?? true,
      });
      return;
    }
    const n = layout.nodes[i];
    const scale = opts.zoom ?? Math.max(view.destination().scale, 1.4);
    view.animateTo(centerOn({ x: n.x ?? 0, y: n.y ?? 0 }, viewport(), scale), {
      viewport: viewport(),
      animate: opts.animate ?? true,
    });
  }

  async function exportPng(): Promise<Blob | null> {
    const { width, height, dpr } = sizeRef.current;
    if (!(width > 0 && height > 0) || typeof document === "undefined")
      return null;
    const canvas = document.createElement("canvas");
    canvas.width = Math.round(width * dpr);
    canvas.height = Math.round(height * dpr);
    const ctx = canvas.getContext("2d");
    if (!ctx) return null;
    drawScene(ctx, sceneFor(false));
    return new Promise((resolve) =>
      canvas.toBlob((blob) => resolve(blob), "image/png"),
    );
  }

  function screenPositionOf(id: string): { x: number; y: number } | null {
    const canvas = canvasRef.current;
    const i = layout.indexById.get(id);
    if (!canvas || i === undefined) return null;
    const rect = canvas.getBoundingClientRect();
    const n = layout.nodes[i];
    const s = worldToScreen(view.transformRef.current, n.x ?? 0, n.y ?? 0);
    return { x: rect.left + s.x, y: rect.top + s.y };
  }

  // Native listeners and the imperative handle are bound once and call
  // through this ref, so parent re-renders never tear them down mid-gesture.
  const handlers = {
    frame,
    afterSync,
    fitNodes,
    onPointerDown,
    onPointerMove,
    onPointerUp,
    onPointerLeave,
    onWheel,
    onContextMenu,
    onKeyDown,
    zoomBy,
    resetView,
    focusNode,
    runCamera,
    exportPng,
    screenPositionOf,
    markInteracted,
  };
  const handlersRef = useRef(handlers);
  useLayoutEffect(() => {
    handlersRef.current = handlers;
    frameRef.current = handlers.frame;
  });

  useImperativeHandle(
    handleRef,
    (): GraphCanvasHandle => ({
      fitToView: (opts) => {
        handlersRef.current.markInteracted();
        handlersRef.current.fitNodes(opts?.nodeIds, opts?.animate ?? true);
      },
      focusNode: (id, opts) => handlersRef.current.focusNode(id, opts),
      zoomBy: (factor) => handlersRef.current.zoomBy(factor),
      resetView: () => handlersRef.current.resetView(),
      relayout: () => {
        layout.relayout();
        requestFrame();
      },
      setPinned: (id, value) => {
        layout.setPinned(id, value);
        requestFrame();
      },
      exportPng: () => handlersRef.current.exportPng(),
      screenPositionOf: (id) => handlersRef.current.screenPositionOf(id),
    }),
    [layout, requestFrame],
  );

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const h = handlersRef;
    const down = (e: PointerEvent) => h.current.onPointerDown(e);
    const move = (e: PointerEvent) => h.current.onPointerMove(e);
    const up = (e: PointerEvent) => h.current.onPointerUp(e);
    const leave = (e: PointerEvent) => h.current.onPointerLeave(e);
    const wheel = (e: WheelEvent) => h.current.onWheel(e);
    const menu = (e: MouseEvent) => h.current.onContextMenu(e);
    const key = (e: KeyboardEvent) => h.current.onKeyDown(e);
    canvas.addEventListener("pointerdown", down);
    canvas.addEventListener("pointermove", move);
    canvas.addEventListener("pointerup", up);
    canvas.addEventListener("pointercancel", up);
    canvas.addEventListener("pointerleave", leave);
    canvas.addEventListener("wheel", wheel, { passive: false });
    canvas.addEventListener("contextmenu", menu);
    canvas.addEventListener("keydown", key);
    const pointers = pointersRef.current;
    return () => {
      canvas.removeEventListener("pointerdown", down);
      canvas.removeEventListener("pointermove", move);
      canvas.removeEventListener("pointerup", up);
      canvas.removeEventListener("pointercancel", up);
      canvas.removeEventListener("pointerleave", leave);
      canvas.removeEventListener("wheel", wheel);
      canvas.removeEventListener("contextmenu", menu);
      canvas.removeEventListener("keydown", key);
      // Unmounting mid-gesture: drop the gesture, but leave a dragged node
      // where it is (and the layout able to cool).
      const gesture = gestureRef.current;
      if (gesture.kind === "press") {
        if (gesture.longPress) clearTimeout(gesture.longPress);
        if (gesture.mode === "drag" && gesture.nodeId)
          layout.endDrag(gesture.nodeId, true);
      }
      gestureRef.current = { kind: "idle" };
      pointers.clear();
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
      rafRef.current = 0;
    };
  }, [layout]);

  // Canvas text uses the dashboard's Geist, not a generic face.
  useEffect(() => {
    const read = () => {
      const geist = getComputedStyle(document.body)
        .getPropertyValue("--font-geist")
        .trim();
      fontRef.current = geist
        ? `${geist}, ui-sans-serif, system-ui, sans-serif`
        : getComputedStyle(document.body).fontFamily ||
          "ui-sans-serif, system-ui, sans-serif";
      measureRef.current.clear();
      requestFrame();
    };
    read();
    const fonts = typeof document !== "undefined" ? document.fonts : undefined;
    if (!fonts) return;
    let active = true;
    fonts.ready.then(() => active && read()).catch(() => {});
    fonts.addEventListener?.("loadingdone", read);
    return () => {
      active = false;
      fonts.removeEventListener?.("loadingdone", read);
    };
  }, [requestFrame]);

  // Every frame reads CANVAS_THEME, so a theme switch needs only a new frame.
  useEffect(() => subscribeTheme(requestFrame), [requestFrame]);

  useEffect(() => {
    requestFrame();
  }, [
    requestFrame,
    graph,
    selected,
    emphasis,
    pinned,
    props.outside,
    colorForLabel,
    captionFor,
  ]);

  const announcement = useSelectionAnnouncement(graph, selected, captionFor);

  return (
    <div ref={containerRef} className={cn("ex-canvas", className)}>
      <canvas
        ref={canvasRef}
        role="application"
        aria-label={ariaLabel}
        aria-describedby={helpId}
        tabIndex={0}
        className="ex-canvas-surface"
      />
      {/* The keyboard focus ring. A ring on the canvas itself never shows: an
          inset shadow paints under the canvas bitmap, and an outer one is
          clipped by this container. So a sibling draws it over the canvas. */}
      <div
        data-canvas-focus-ring=""
        aria-hidden="true"
        className="ex-canvas-ring"
      />
      <div
        ref={tooltipRef}
        aria-hidden="true"
        className="ex-tooltip"
        style={{ display: "none" }}
      >
        <div className="ex-tooltip-meta">
          <span ref={tooltipSwatchRef} className="ex-swatch" />
          <span ref={tooltipLabelRef} className="ex-truncate" />
        </div>
        <div
          ref={tooltipCaptionRef}
          className="ex-tooltip-caption ex-truncate"
        />
      </div>
      <p id={helpId} className="hf-sr-only">
        Arrow keys move the selection between nodes. Enter expands the selected
        node, Escape clears the selection. Plus and minus zoom, 0 resets the
        zoom, F fits the graph.
      </p>
      <div className="hf-sr-only" aria-live="polite">
        {announcement}
      </div>
    </div>
  );
}

function useSelectionAnnouncement(
  graph: GraphData,
  selected: Selection | null,
  captionFor: (node: GraphNode) => string,
): string {
  const nodesById = useMemo(
    () => new Map(graph.nodes.map((n) => [n.id, n])),
    [graph.nodes],
  );
  const edgesById = useMemo(
    () => new Map(graph.edges.map((e) => [e.id, e])),
    [graph.edges],
  );
  return useMemo(() => {
    if (!selected) return "";
    if (selected.kind === "node") {
      const node = nodesById.get(selected.id);
      return node ? `Selected ${node.label}: ${captionFor(node)}` : "";
    }
    const edge = edgesById.get(selected.id);
    if (!edge) return "";
    const source = nodesById.get(edge.source);
    const target = nodesById.get(edge.target);
    const from = source ? captionFor(source) : edge.source;
    const to = target ? captionFor(target) : edge.target;
    return `Selected ${edge.label} edge from ${from} to ${to}`;
  }, [selected, nodesById, edgesById, captionFor]);
}
