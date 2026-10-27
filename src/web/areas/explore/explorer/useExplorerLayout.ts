import {
  useCallback,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type KeyboardEvent,
  type PointerEvent,
} from "react";

import {
  asPane,
  readStored,
  writeStored,
  type StoredPane,
} from "./lib/storage";

/**
 * desktop ≥1280: rail, canvas and inspector side by side.
 * laptop 1024–1279: the rail folds into an icon strip with a flyout; inspector 320px.
 * tablet 768–1023: the rail is a left sheet and the inspector overlays the canvas.
 * mobile <768: full canvas with a bottom tab bar opening bottom sheets.
 */
export type ViewportMode = "desktop" | "laptop" | "tablet" | "mobile";

const QUERIES: [ViewportMode, string][] = [
  ["desktop", "(min-width: 1280px)"],
  ["laptop", "(min-width: 1024px)"],
  ["tablet", "(min-width: 768px)"],
];

function currentMode(): ViewportMode {
  for (const [mode, query] of QUERIES)
    if (window.matchMedia(query).matches) return mode;
  return "mobile";
}

function subscribe(onChange: () => void): () => void {
  const lists = QUERIES.map(([, query]) => window.matchMedia(query));
  for (const list of lists) list.addEventListener("change", onChange);
  return () => {
    for (const list of lists) list.removeEventListener("change", onChange);
  };
}

export function useViewportMode(): ViewportMode {
  return useSyncExternalStore(subscribe, currentMode, () => "desktop");
}

/** Whether the primary pointer can't hover (touch): row actions stay visible. */
export function useCoarsePointer(): boolean {
  return useSyncExternalStore(
    (onChange) => {
      const list = window.matchMedia("(hover: none)");
      list.addEventListener("change", onChange);
      return () => list.removeEventListener("change", onChange);
    },
    () => window.matchMedia("(hover: none)").matches,
    () => false,
  );
}

export interface PaneState extends StoredPane {
  setOpen: (open: boolean | ((open: boolean) => boolean)) => void;
  setWidth: (width: number) => void;
}

/** A side panel's open state and width, remembered per database. */
export function usePane(
  storageKey: string,
  defaults: StoredPane,
  bounds: { min: number; max: number },
): PaneState {
  const [pane, setPane] = useState<StoredPane>(defaults);
  // Which key's stored value has been applied (rendered). State, not a ref: the write below must not
  // run until the restored value has rendered, or it would store the defaults over it. Under
  // StrictMode both effects run twice on mount, before any queued update renders.
  const [loadedKey, setLoadedKey] = useState<string | null>(null);
  const [fallback] = useState(defaults);
  const restoredOnce = useRef(false);

  const { min, max } = bounds;
  useEffect(() => {
    const stored = readStored(storageKey, asPane);
    if (stored)
      setPane({ open: stored.open, width: clamp(stored.width, min, max) });
    // Switching to a database with nothing remembered starts from the defaults, not the last one's panes.
    else if (restoredOnce.current) setPane(fallback);
    restoredOnce.current = true;
    setLoadedKey(storageKey);
  }, [storageKey, min, max, fallback]);

  useEffect(() => {
    if (loadedKey !== storageKey) return;
    writeStored(storageKey, pane);
  }, [pane, storageKey, loadedKey]);

  const setOpen = useCallback(
    (open: boolean | ((open: boolean) => boolean)) => {
      setPane((current) => {
        const next = typeof open === "function" ? open(current.open) : open;
        return next === current.open ? current : { ...current, open: next };
      });
    },
    [],
  );
  const setWidth = useCallback(
    (width: number) =>
      setPane((current) => ({ ...current, width: clamp(width, min, max) })),
    [min, max],
  );
  return { ...pane, setOpen, setWidth };
}

function clamp(value: number, min: number, max: number): number {
  return Math.round(Math.min(max, Math.max(min, value)));
}

const KEY_STEP = 16;

/**
 * Drag and keyboard handlers for a vertical splitter. `edge` is the side of
 * the panel the handle sits on: a handle on a right-hand panel's left edge
 * grows the panel when dragged left.
 */
export function useSplitter({
  width,
  min,
  max,
  edge,
  onResize,
}: {
  width: number;
  min: number;
  max: number;
  edge: "left" | "right";
  onResize: (width: number) => void;
}) {
  const drag = useRef<{ startX: number; startWidth: number } | null>(null);
  const [dragging, setDragging] = useState(false);
  const sign = edge === "left" ? -1 : 1;

  useEffect(() => {
    if (!dragging) return;
    const previous = document.body.style.cursor;
    document.body.style.cursor = "col-resize";
    return () => {
      document.body.style.cursor = previous;
    };
  }, [dragging]);

  return {
    dragging,
    props: {
      role: "separator" as const,
      "aria-orientation": "vertical" as const,
      "aria-valuemin": min,
      "aria-valuemax": max,
      "aria-valuenow": Math.round(width),
      tabIndex: 0,
      onPointerDown: (event: PointerEvent<HTMLElement>) => {
        if (event.button !== 0) return;
        event.preventDefault();
        event.currentTarget.setPointerCapture(event.pointerId);
        drag.current = { startX: event.clientX, startWidth: width };
        setDragging(true);
      },
      onPointerMove: (event: PointerEvent<HTMLElement>) => {
        const current = drag.current;
        if (!current) return;
        onResize(current.startWidth + sign * (event.clientX - current.startX));
      },
      onPointerUp: (event: PointerEvent<HTMLElement>) => {
        if (!drag.current) return;
        drag.current = null;
        setDragging(false);
        if (event.currentTarget.hasPointerCapture(event.pointerId))
          event.currentTarget.releasePointerCapture(event.pointerId);
      },
      onPointerCancel: () => {
        drag.current = null;
        setDragging(false);
      },
      onKeyDown: (event: KeyboardEvent<HTMLElement>) => {
        const step = event.shiftKey ? KEY_STEP * 4 : KEY_STEP;
        let next: number | null = null;
        if (event.key === "ArrowLeft") next = width - sign * step;
        else if (event.key === "ArrowRight") next = width + sign * step;
        else if (event.key === "Home") next = min;
        else if (event.key === "End") next = max;
        if (next === null) return;
        event.preventDefault();
        onResize(next);
      },
    },
  };
}
