import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { ChevronDown } from "lucide-react";

import { cn } from "../ui";

// A dropdown rendered in place rather than portaled, so it stays inside the
// explorer (and inside fullscreen). Being in place, anything past an
// overflow-clipping ancestor (the explorer root clips) is cut off and can't
// be scrolled to, so on open the menu measures the room around its trigger:
// it shifts or flips sideways to stay inside, opens upward when that has
// more room, and caps its height so long lists scroll inside the menu.

const ITEM_SELECTOR = '[role="menuitem"]:not([disabled])';
/** The menu's offset from its trigger (mt-1 / mb-1). */
const MENU_GAP = 4;
/** Distance kept from the edges of the visible area. */
const EDGE = 8;
const MIN_MENU_HEIGHT = 96;

interface Rect {
  top: number;
  right: number;
  bottom: number;
  left: number;
}

interface Placement {
  /** Relative to the menu's positioned wrapper. */
  left: number;
  width: number | null;
  maxHeight: number;
  up: boolean;
}

/** The part of the viewport an element can paint in: the viewport cut down by every clipping ancestor. */
export function visibleRect(element: HTMLElement): Rect {
  const rect: Rect = {
    top: 0,
    left: 0,
    right: window.innerWidth,
    bottom: window.innerHeight,
  };
  for (
    let node = element.parentElement;
    node && node !== document.body;
    node = node.parentElement
  ) {
    const style = window.getComputedStyle(node);
    if (style.overflowX === "visible" && style.overflowY === "visible")
      continue;
    const box = node.getBoundingClientRect();
    if (box.width === 0 && box.height === 0) continue;
    rect.top = Math.max(rect.top, box.top);
    rect.left = Math.max(rect.left, box.left);
    rect.right = Math.min(rect.right, box.right);
    rect.bottom = Math.min(rect.bottom, box.bottom);
  }
  return rect;
}

/**
 * Where the menu goes: on `align`'s side of the trigger unless that runs past
 * the visible area, then the other side, then clamped inside; below the
 * trigger unless it doesn't fit there and above has more room.
 */
export function placeMenu(options: {
  bounds: Rect;
  anchor: Rect;
  /** Left edge of the wrapper the menu is positioned in. */
  originLeft: number;
  /** The menu's own width and height (0 when there's no layout). */
  width: number;
  height: number;
  align: "start" | "end";
  maxHeight?: number;
}): Placement {
  const { bounds, anchor, align } = options;
  const room = Math.max(0, bounds.right - bounds.left - 2 * EDGE);
  const width = options.width > 0 ? Math.min(options.width, room) : 0;
  const startLeft = anchor.left;
  const endLeft = anchor.right - width;
  let left = align === "end" ? endLeft : startLeft;
  if (align === "start" && left + width > bounds.right - EDGE) left = endLeft;
  if (align === "end" && left < bounds.left + EDGE) left = startLeft;
  left = Math.max(
    bounds.left + EDGE,
    Math.min(left, bounds.right - EDGE - width),
  );

  const below = bounds.bottom - anchor.bottom - MENU_GAP - EDGE;
  const above = anchor.top - bounds.top - MENU_GAP - EDGE;
  const up = options.height > below && above > below;
  const space = up ? above : below;
  return {
    left: left - options.originLeft,
    width: width > 0 ? width : null,
    maxHeight: Math.max(
      MIN_MENU_HEIGHT,
      Math.min(options.maxHeight ?? Infinity, space),
    ),
    up,
  };
}

export function InlineMenu({
  label,
  icon,
  menuLabel,
  align = "start",
  maxHeight,
  disabled,
  title,
  ariaLabel,
  hideChevron,
  triggerClassName,
  menuClassName,
  className,
  children,
}: {
  label: ReactNode;
  icon?: ReactNode;
  /** Accessible name of the menu. */
  menuLabel: string;
  /** Preferred side; the menu switches sides or shifts when it would run past the visible area. */
  align?: "start" | "end";
  /** An extra cap on the menu's height; it always fits the visible area. */
  maxHeight?: number;
  disabled?: boolean;
  title?: string;
  /** Accessible name of the trigger, when its label is only an icon. */
  ariaLabel?: string;
  hideChevron?: boolean;
  triggerClassName?: string;
  /** Overrides the menu's width, e.g. to fit a narrow panel. */
  menuClassName?: string;
  className?: string;
  children: (close: () => void) => ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const [placement, setPlacement] = useState<Placement | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const menuId = useId();

  const close = useCallback((restoreFocus = true) => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  }, []);

  useEffect(() => {
    if (!open) setPlacement(null);
  }, [open]);

  // Measure before paint, with no placement styles applied, then place.
  useLayoutEffect(() => {
    if (!open || placement) return;
    const root = rootRef.current;
    const menu = menuRef.current;
    const trigger = triggerRef.current;
    if (!root || !menu || !trigger) return;
    setPlacement(
      placeMenu({
        bounds: visibleRect(root),
        anchor: trigger.getBoundingClientRect(),
        originLeft: root.getBoundingClientRect().left,
        width: menu.offsetWidth,
        height: menu.scrollHeight,
        align,
        maxHeight,
      }),
    );
  }, [open, placement, align, maxHeight]);

  useEffect(() => {
    if (!open) return;
    const remeasure = () => setPlacement(null);
    window.addEventListener("resize", remeasure);
    return () => window.removeEventListener("resize", remeasure);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    menuRef.current?.querySelector<HTMLElement>(ITEM_SELECTOR)?.focus();
    const onPointerDown = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    return () => document.removeEventListener("pointerdown", onPointerDown);
  }, [open]);

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(
      menuRef.current?.querySelectorAll<HTMLElement>(ITEM_SELECTOR) ?? [],
    );
    const index = items.indexOf(document.activeElement as HTMLElement);
    let next: number | null = null;
    if (event.key === "ArrowDown")
      next = index < 0 ? 0 : (index + 1) % items.length;
    else if (event.key === "ArrowUp")
      next = index <= 0 ? items.length - 1 : index - 1;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = items.length - 1;
    else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      close();
      return;
    } else if (event.key === "Tab") {
      close(false);
      return;
    }
    if (next !== null && items.length > 0) {
      event.preventDefault();
      items[next].focus();
    }
  };

  return (
    <div ref={rootRef} className={cn("ex-menu-anchor", className)}>
      <button
        ref={triggerRef}
        type="button"
        disabled={disabled}
        title={title}
        aria-label={ariaLabel}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpen((current) => !current)}
        onKeyDown={(event) => {
          if (event.key === "ArrowDown" && !open) {
            event.preventDefault();
            setOpen(true);
          }
        }}
        className={cn("ex-btn", open && "is-on", triggerClassName)}
      >
        {icon}
        {label}
        {!hideChevron && (
          <ChevronDown aria-hidden="true" size={12} className="ex-dim" />
        )}
      </button>
      {open && (
        <div
          ref={menuRef}
          id={menuId}
          role="menu"
          aria-label={menuLabel}
          onKeyDown={onMenuKeyDown}
          style={menuStyle(placement, maxHeight)}
          className={cn(
            "ex-menu ex-inline-menu",
            placement?.up ? "is-up" : "is-down",
            !placement && (align === "end" ? "is-end" : "is-start"),
            menuClassName,
          )}
        >
          {children(() => close())}
        </div>
      )}
    </div>
  );
}

function menuStyle(
  placement: Placement | null,
  maxHeight: number | undefined,
): CSSProperties | undefined {
  if (!placement) return maxHeight ? { maxHeight } : undefined;
  return {
    left: placement.left,
    width: placement.width ?? undefined,
    maxHeight: placement.maxHeight,
  };
}

export function MenuItem({
  onSelect,
  disabled,
  title,
  children,
}: {
  onSelect: () => void;
  disabled?: boolean;
  title?: string;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      tabIndex={-1}
      disabled={disabled}
      title={title}
      onClick={onSelect}
      className="ex-menu-item is-stacked"
    >
      {children}
    </button>
  );
}
