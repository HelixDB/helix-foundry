import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import {
  ArrowUpRight,
  Braces,
  ChevronRight,
  Copy,
  EyeOff,
  ImageDown,
  Loader2,
  LocateFixed,
  Pin,
  PinOff,
  RotateCcw,
  Scan,
  SquareArrowOutUpRight,
  Waypoints,
  type LucideIcon,
} from "lucide-react";

import { cn } from "./ui";

import type { ExpandOptions } from "./useExplorerGraph";
import { menuRelationships } from "./lib/graph-store";
import type {
  DegreeInfo,
  DegreeProbe,
  GraphEdge,
  GraphNode,
  Selection,
} from "./lib/types";
import { formatCount, KBD, MENU_ITEM, MENU_SURFACE, MONO } from "./ui";

export interface ContextMenuState {
  target: Selection | null;
  x: number;
  y: number;
}

export interface ContextMenuProps {
  menu: ContextMenuState;
  node: GraphNode | null;
  edge: GraphEdge | null;
  /** For an edge: its endpoints, when loaded. */
  source: GraphNode | null;
  target: GraphNode | null;
  degree: DegreeInfo | undefined;
  /** The cheap size probe (neighbors, no edges read): what opening the menu fetches while every edge label shows. */
  probe?: DegreeProbe;
  /**
   * Edge labels are hidden (Foundry's internals): the probe counts their edges, so the menu goes
   * by the degree alone, which opening it then reads (see menuRelationships).
   */
  edgesHidden: boolean;
  degreeLoading: boolean;
  pinned: boolean;
  captionOf: (node: GraphNode) => string;
  onClose: () => void;
  onExpand: (nodeId: string, options?: ExpandOptions) => void;
  onFocusNode: (nodeId: string) => void;
  onHide: (nodeId: string) => void;
  onTogglePin: (nodeId: string) => void;
  onCopy: (label: string, text: string) => void;
  onCopyJson: (selection: Selection) => void;
  /** Opens a node's page in Foundry, for an ontology's entities; unset for other nodes. */
  onOpenPage?: (node: GraphNode) => void;
  onSelect: (selection: Selection) => void;
  onFit: () => void;
  onRelayout: () => void;
  onResetView: () => void;
  onExportPng: () => void;
  /**
   * Where focus returns when the menu closes, if the element focused before it opened is gone
   * (the canvas). Without one, focus goes back to that element only.
   */
  fallbackFocus?: () => HTMLElement | null;
}

const GUTTER = 8;
const SUB_GAP = 4;
const ITEMS = '[role="menuitem"]:not([disabled])';

/** A menu's own items: the Expand submenu sits inside the main menu, but its items aren't the menu's. */
function menuItems(container: HTMLElement | null): HTMLElement[] {
  if (!container) return [];
  return Array.from(container.querySelectorAll<HTMLElement>(ITEMS)).filter(
    (item) => item.parentElement?.closest('[role="menu"]') === container,
  );
}

function focusItem(
  container: HTMLElement | null,
  index: number | "first" | "last",
) {
  const items = menuItems(container);
  if (items.length === 0) return;
  const target =
    index === "first"
      ? 0
      : index === "last"
        ? items.length - 1
        : (index + items.length) % items.length;
  items[target].focus();
}

function moveFocus(
  container: HTMLElement | null,
  event: KeyboardEvent,
): boolean {
  const items = menuItems(container);
  const index = items.indexOf(document.activeElement as HTMLElement);
  if (event.key === "ArrowDown")
    focusItem(container, index < 0 ? 0 : index + 1);
  else if (event.key === "ArrowUp")
    focusItem(container, index <= 0 ? -1 : index - 1);
  else if (event.key === "Home") focusItem(container, "first");
  else if (event.key === "End") focusItem(container, "last");
  else return false;
  event.preventDefault();
  return true;
}

/**
 * The canvas context menu: actions on the node or edge under the pointer, or
 * view actions on empty space. Rendered in place (not portaled) so it stays
 * inside the explorer in fullscreen.
 */
export function ContextMenu(props: ContextMenuProps) {
  const { menu, node, edge, onClose } = props;
  const menuRef = useRef<HTMLDivElement>(null);
  const subRef = useRef<HTMLDivElement>(null);
  const expandRef = useRef<HTMLButtonElement>(null);
  const [position, setPosition] = useState({
    left: menu.x,
    top: menu.y,
    ready: false,
  });
  const [subOpen, setSubOpen] = useState(false);
  const [subPosition, setSubPosition] = useState<{
    left: number;
    top: number;
    maxHeight: number;
  } | null>(null);
  /** False when the menu closes because focus is going elsewhere anyway (a click outside, the window). */
  const restoreFocus = useRef(true);
  const fallbackFocus = useRef(props.fallbackFocus);
  useLayoutEffect(() => {
    fallbackFocus.current = props.fallbackFocus;
  });

  // Keep the menu on screen: flip it to the other side of the pointer when it would overflow.
  useLayoutEffect(() => {
    const element = menuRef.current;
    if (!element) return;
    const { width, height } = element.getBoundingClientRect();
    const viewportWidth = window.innerWidth;
    const viewportHeight = window.innerHeight;
    let left = menu.x;
    let top = menu.y;
    if (left + width > viewportWidth - GUTTER)
      left = Math.max(GUTTER, menu.x - width);
    if (top + height > viewportHeight - GUTTER)
      top = Math.max(GUTTER, viewportHeight - height - GUTTER);
    setPosition({ left, top, ready: true });
  }, [menu.x, menu.y]);

  useEffect(() => {
    if (position.ready) focusItem(menuRef.current, "first");
  }, [position.ready]);

  // Closing hands focus back to where it was (the canvas, for Shift+F10 or a right-click), unless an
  // action already moved it somewhere else. A layout cleanup runs while the menu is still in the DOM.
  useLayoutEffect(() => {
    const previous =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    const element = menuRef.current;
    return () => {
      if (!restoreFocus.current) return;
      const active = document.activeElement;
      if (active && active !== document.body && !element?.contains(active))
        return;
      const back =
        previous &&
        previous !== document.body &&
        previous.isConnected &&
        !element?.contains(previous)
          ? previous
          : (fallbackFocus.current?.() ?? null);
      back?.focus({ preventScroll: true });
    };
  }, []);

  useEffect(() => {
    const onPointerDown = (event: PointerEvent) => {
      const target = event.target as Node;
      if (menuRef.current?.contains(target) || subRef.current?.contains(target))
        return;
      restoreFocus.current = false;
      onClose();
    };
    const onResize = () => onClose();
    const onBlur = () => {
      restoreFocus.current = false;
      onClose();
    };
    document.addEventListener("pointerdown", onPointerDown, true);
    window.addEventListener("resize", onResize);
    window.addEventListener("blur", onBlur);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      window.removeEventListener("resize", onResize);
      window.removeEventListener("blur", onBlur);
    };
  }, [onClose]);

  const act = (action: () => void) => () => {
    onClose();
    action();
  };

  // The keys the menu shows, acting on the menu's own target (not on whatever is selected).
  const accelerators: Record<string, () => void> = node
    ? {
        e: () => props.onExpand(node.id),
        h: () => props.onHide(node.id),
        p: () => props.onTogglePin(node.id),
      }
    : edge
      ? {}
      : { f: props.onFit, r: props.onRelayout, "0": props.onResetView };

  /** Runs a hinted key; other plain keys stop here so canvas shortcuts don't act behind the menu. */
  const onAccelerator = (event: KeyboardEvent<HTMLDivElement>): boolean => {
    if (event.metaKey || event.ctrlKey || event.altKey) return false;
    event.stopPropagation();
    const action = accelerators[event.key.toLowerCase()];
    if (!action) return false;
    event.preventDefault();
    act(action)();
    return true;
  };

  const relationships = menuRelationships(
    props.degree,
    props.probe,
    props.edgesHidden,
  );

  const edgeLabels = (() => {
    const labels = new Map<string, { out: number; in: number }>();
    for (const { label, count } of props.degree?.outByLabel ?? [])
      labels.set(label, { out: count ?? 0, in: 0 });
    for (const { label, count } of props.degree?.inByLabel ?? [])
      labels.set(label, { out: labels.get(label)?.out ?? 0, in: count ?? 0 });
    return [...labels.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  })();

  // The Expand submenu goes beside the menu where it fits, else over it below the Expand row (phones),
  // and is clamped to the viewport's height (it scrolls when there are many edge labels).
  useLayoutEffect(() => {
    if (!subOpen) return;
    const sub = subRef.current;
    const element = menuRef.current;
    const anchor = expandRef.current;
    if (!sub || !element || !anchor) return;
    const viewportWidth = window.innerWidth;
    const viewportHeight = window.innerHeight;
    const menuRect = element.getBoundingClientRect();
    const anchorRect = anchor.getBoundingClientRect();
    const width = sub.offsetWidth;
    const maxHeight = Math.max(0, viewportHeight - GUTTER * 2);
    const height = Math.min(
      Math.max(sub.offsetHeight, sub.scrollHeight),
      maxHeight,
    );
    let left: number;
    let top = anchorRect.top - SUB_GAP;
    if (menuRect.right + SUB_GAP + width <= viewportWidth - GUTTER)
      left = menuRect.right + SUB_GAP;
    else if (menuRect.left - SUB_GAP - width >= GUTTER)
      left = menuRect.left - SUB_GAP - width;
    else {
      left = Math.max(
        GUTTER,
        Math.min(menuRect.left + 16, viewportWidth - GUTTER - width),
      );
      top = anchorRect.bottom;
    }
    top = Math.max(GUTTER, Math.min(top, viewportHeight - GUTTER - height));
    setSubPosition((current) =>
      current &&
      current.left === left &&
      current.top === top &&
      current.maxHeight === maxHeight
        ? current
        : { left, top, maxHeight },
    );
  }, [
    subOpen,
    position.left,
    position.top,
    edgeLabels.length,
    props.degreeLoading,
  ]);

  const onMenuKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (moveFocus(menuRef.current, event)) return;
    if (onAccelerator(event)) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      onClose();
    } else if (event.key === "Tab") {
      event.preventDefault();
      onClose();
    } else if (
      event.key === "ArrowRight" &&
      document.activeElement === expandRef.current
    ) {
      event.preventDefault();
      setSubOpen(true);
      requestAnimationFrame(() => focusItem(subRef.current, "first"));
    }
  };

  const onSubKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    event.stopPropagation();
    if (moveFocus(subRef.current, event)) return;
    if (onAccelerator(event)) return;
    if (event.key === "ArrowLeft" || event.key === "Escape") {
      event.preventDefault();
      setSubOpen(false);
      expandRef.current?.focus();
    } else if (event.key === "Tab") {
      event.preventDefault();
      onClose();
    }
  };

  let title: string;
  let body: ReactNode;
  if (node) {
    title = `${node.label}: ${props.captionOf(node)}`;
    body = (
      <>
        <button
          ref={expandRef}
          type="button"
          role="menuitem"
          tabIndex={-1}
          aria-haspopup="menu"
          aria-expanded={subOpen}
          onClick={() => {
            setSubOpen(true);
            requestAnimationFrame(() => focusItem(subRef.current, "first"));
          }}
          onPointerEnter={(event) => {
            event.currentTarget.focus();
            setSubOpen(true);
          }}
          className={cn(MENU_ITEM, subOpen && "is-open")}
        >
          <Waypoints aria-hidden="true" size={16} className="ex-dim" />
          Expand
          <ChevronRight aria-hidden="true" size={14} className="ex-icon-end" />
        </button>
        <Item
          icon={LocateFixed}
          onClick={act(() => props.onFocusNode(node.id))}
          onEnter={() => setSubOpen(false)}
        >
          Focus
        </Item>
        <Item
          icon={EyeOff}
          hint="H"
          onClick={act(() => props.onHide(node.id))}
          onEnter={() => setSubOpen(false)}
        >
          Hide
        </Item>
        <Item
          icon={props.pinned ? PinOff : Pin}
          hint="P"
          onClick={act(() => props.onTogglePin(node.id))}
          onEnter={() => setSubOpen(false)}
        >
          {props.pinned ? "Unpin" : "Pin"}
        </Item>
        <Separator />
        <Item
          icon={Copy}
          onClick={act(() => props.onCopy("id", String(node.dbId)))}
          onEnter={() => setSubOpen(false)}
        >
          Copy id
        </Item>
        <Item
          icon={Braces}
          onClick={act(() => props.onCopyJson({ kind: "node", id: node.id }))}
          onEnter={() => setSubOpen(false)}
        >
          Copy as JSON
        </Item>
        {props.onOpenPage && (
          <Item
            icon={SquareArrowOutUpRight}
            onClick={act(() => props.onOpenPage!(node))}
            onEnter={() => setSubOpen(false)}
          >
            Open its page
          </Item>
        )}
      </>
    );
  } else if (edge) {
    title = `${edge.label} edge`;
    body = (
      <>
        <Item
          icon={ArrowUpRight}
          disabled={!props.source}
          onClick={act(() => props.onSelect({ kind: "node", id: edge.source }))}
        >
          Open source
          {props.source ? (
            <span className="ex-truncate ex-dim">
              {props.captionOf(props.source)}
            </span>
          ) : null}
        </Item>
        <Item
          icon={ArrowUpRight}
          disabled={!props.target}
          onClick={act(() => props.onSelect({ kind: "node", id: edge.target }))}
        >
          Open target
          {props.target ? (
            <span className="ex-truncate ex-dim">
              {props.captionOf(props.target)}
            </span>
          ) : null}
        </Item>
        <Separator />
        <Item
          icon={Copy}
          onClick={act(() => props.onCopy("id", String(edge.dbId)))}
        >
          Copy id
        </Item>
        <Item
          icon={Braces}
          onClick={act(() => props.onCopyJson({ kind: "edge", id: edge.id }))}
        >
          Copy as JSON
        </Item>
      </>
    );
  } else {
    title = "Canvas";
    body = (
      <>
        <Item icon={Scan} hint="F" onClick={act(props.onFit)}>
          Fit to view
        </Item>
        <Item icon={Waypoints} hint="R" onClick={act(props.onRelayout)}>
          Re-run layout
        </Item>
        <Item icon={RotateCcw} hint="0" onClick={act(props.onResetView)}>
          Reset zoom
        </Item>
        <Separator />
        <Item icon={ImageDown} onClick={act(props.onExportPng)}>
          Export PNG
        </Item>
      </>
    );
  }

  return (
    <div
      ref={menuRef}
      role="menu"
      aria-label={title}
      onKeyDown={onMenuKeyDown}
      onContextMenu={(event) => event.preventDefault()}
      className={cn(
        MENU_SURFACE,
        "ex-context-menu",
        !position.ready && "is-measuring",
      )}
      style={{ left: position.left, top: position.top }}
    >
      <div className="ex-menu-title ex-truncate" title={title}>
        {title}
      </div>
      {body}
      {node && subOpen && (
        <div
          ref={subRef}
          role="menu"
          aria-label="Expand"
          onKeyDown={onSubKeyDown}
          className={cn(
            MENU_SURFACE,
            "ex-context-menu is-sub",
            !subPosition && "is-measuring",
          )}
          style={
            subPosition
              ? {
                  left: subPosition.left,
                  top: subPosition.top,
                  maxHeight: subPosition.maxHeight,
                }
              : { left: 0, top: 0 }
          }
        >
          <Item
            icon={Waypoints}
            hint="E"
            onClick={act(() => props.onExpand(node.id))}
            detail={relationships.total}
          >
            All neighbors
          </Item>
          <Item
            disabled={relationships.empty.out}
            onClick={act(() => props.onExpand(node.id, { direction: "out" }))}
            detail={relationships.out}
          >
            Outgoing
          </Item>
          <Item
            disabled={relationships.empty.in}
            onClick={act(() => props.onExpand(node.id, { direction: "in" }))}
            detail={relationships.in}
          >
            Incoming
          </Item>
          {(edgeLabels.length > 0 || props.degreeLoading) && <Separator />}
          {props.degreeLoading && edgeLabels.length === 0 && (
            <div className="ex-menu-note">
              <Loader2 aria-hidden="true" size={14} className="hf-spin" />
              Checking relationships…
            </div>
          )}
          {edgeLabels.map(([label, counts]) => (
            <Item
              key={label}
              onClick={act(() => props.onExpand(node.id, { edgeLabel: label }))}
              detail={formatCount(counts.out + counts.in)}
            >
              <span className={cn(MONO, "ex-truncate")}>{label}</span>
            </Item>
          ))}
        </div>
      )}
    </div>
  );
}

function Item({
  icon: Icon,
  hint,
  detail,
  disabled,
  onClick,
  onEnter,
  children,
}: {
  icon?: LucideIcon;
  hint?: string;
  detail?: string;
  disabled?: boolean;
  onClick: () => void;
  onEnter?: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      tabIndex={-1}
      disabled={disabled}
      onClick={onClick}
      onPointerEnter={(event) => {
        // Pointer and keyboard share one highlight: the focused item.
        if (!disabled) event.currentTarget.focus();
        onEnter?.();
      }}
      onFocus={onEnter}
      className={MENU_ITEM}
    >
      {Icon ? (
        <Icon aria-hidden="true" size={16} className="ex-dim" />
      ) : (
        <span className="ex-menu-icon-gap" />
      )}
      <span className="ex-menu-item-label">{children}</span>
      {detail && <span className="ex-num ex-dim">{detail}</span>}
      {hint && <kbd className={KBD}>{hint}</kbd>}
    </button>
  );
}

function Separator() {
  return <div role="separator" className="ex-menu-sep" />;
}
