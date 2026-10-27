import {
  createContext,
  useContext,
  useEffect,
  useRef,
  type CSSProperties,
  type PointerEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { useSortable } from "@dnd-kit/sortable";
import {
  ArrowDown,
  ArrowUp,
  ChevronRight,
  Folder,
  FolderOpen,
  GripVertical,
} from "lucide-react";
import { DropdownMenu, MenuItem } from "../../kit";
import {
  EMPTY_LAYOUT,
  stepBefore,
  type FolderKey,
  type KindKey,
  type Layout,
  type LayoutChange,
  type RootKey,
  type TreeData,
} from "./layout";

// What the Data explorer's rows share. Each is a treeitem (`useTreeItem`):
// the row the URL opens is marked, one row takes the tree's Tab stop, and
// the explorer moves the focus between them with the arrow keys
// (./explorer.tsx). To be rearranged, owners and admins drag a row by its
// grip, or open the grip's menu (a click, or Enter from the keyboard) to
// move it a place up or down, or elsewhere. The grip shows while the tree
// can be arranged (see `Arranging.on`). Each row is a dnd-kit sortable item
// in its container's list; tree.tsx's DndContext decides where a drop
// lands.

/** The explorer's rows: the one the URL opens, and the one Tab lands on. */
export const ExplorerContext = createContext<{
  current: string | null;
  tabStop: string | null;
}>({ current: null, tabStop: null });

/**
 * A row's treeitem attributes, for the row `key` (an ExplorerRow's) at
 * `level`: whether it is open (folders only), whether the URL opens it,
 * and whether Tab lands on it.
 */
export function useTreeItem(key: string, level: number, open?: boolean) {
  const { current, tabStop } = useContext(ExplorerContext);
  return {
    role: "treeitem",
    "data-row": key,
    "aria-level": level,
    "aria-expanded": open,
    "aria-current": current === key ? ("page" as const) : undefined,
    tabIndex: tabStop === key ? 0 : -1,
  };
}

/** Indents a row (or a note in a folder) to its level, with its guides. */
export const rowLevel = (level: number) =>
  ({ "--dt-level": level }) as CSSProperties;

/**
 * What a dragged or hovered row is, for the tree to decide a drop, and its
 * name for the copy of it that follows the pointer.
 */
export type DragData = { name: string } & (
  | { type: "root"; key: RootKey }
  | {
      type: "connector";
      id: string;
      kind: string;
      container: KindKey | FolderKey;
    }
  | { type: "set"; id: string }
  | { type: "document"; id: string; setId: string }
);

export type Arranging = {
  // Rows can move: an owner or admin, every list read, a layout this site
  // knows, and no search.
  on: boolean;
  change: (change: LayoutChange) => void;
  // The layout as shown, and what it arranges.
  layout: Layout;
  data: TreeData;
  // The folder a drag would drop a connector into, lit up.
  into: string | null;
  // The row a menu moved, whose grip takes the focus back once it is drawn
  // in its new place.
  focus: RefObject<string | null>;
};

export const ArrangeContext = createContext<Arranging>({
  on: false,
  change: () => {},
  layout: EMPTY_LAYOUT,
  data: { kinds: [], connectors: [], sets: [], documents: [] },
  into: null,
  focus: { current: null },
});
export const useArranging = () => useContext(ArrangeContext);

/** A row of the tree as a sortable item: its node, place and grip. */
export function useRow(id: string, data: DragData) {
  const { on } = useArranging();
  const s = useSortable({ id, data, disabled: !on });
  // Up and down only, never stretched to another row's size.
  const style: CSSProperties = {
    transform: s.transform
      ? `translate3d(0, ${Math.round(s.transform.y)}px, 0)`
      : undefined,
    transition: s.transition,
  };
  return {
    ref: s.setNodeRef,
    style,
    dragging: s.isDragging,
    handle: {
      ref: s.setActivatorNodeRef,
      onPointerDown: s.listeners?.onPointerDown as
        | ((e: PointerEvent) => void)
        | undefined,
    },
  };
}
export type RowHandle = ReturnType<typeof useRow>["handle"];

/**
 * A row's grip: dragged, it moves the row; clicked, it opens the row's
 * menu: Move up and Move down among `siblings` (ids as `move` takes them),
 * then `children`.
 */
export function RowMenu<T extends string>({
  rowId,
  label,
  handle,
  id,
  siblings,
  move,
  children,
}: {
  // The row's sortable id.
  rowId: string;
  label: string;
  handle: RowHandle;
  id: T;
  siblings: readonly T[];
  move: (before: T | null) => void;
  children?: ReactNode;
}) {
  const { focus } = useArranging();
  const grip = useRef<HTMLSpanElement | null>(null);
  useEffect(() => {
    if (focus.current !== rowId) return;
    focus.current = null;
    grip.current?.querySelector("button")?.focus();
  });
  const up = stepBefore(siblings, id, -1),
    down = stepBefore(siblings, id, 1);
  const step = (before: T | null | undefined) => {
    if (before === undefined) return;
    focus.current = rowId;
    move(before);
  };
  return (
    <span
      ref={(el) => {
        grip.current = el;
        handle.ref(el);
      }}
      className="dt-grip"
      // Only the grip drags, not a press inside its open menu.
      onPointerDown={(e) => {
        if ((e.target as Element).closest(".hf-menu-trigger"))
          handle.onPointerDown?.(e);
      }}
    >
      <DropdownMenu
        label={label}
        triggerClassName="dt-grip-button"
        trigger={
          <span
            title="Drag to move, or click for more"
            className="dt-grip-icon"
          >
            <GripVertical size={14} aria-hidden />
          </span>
        }
      >
        <MenuItem
          icon={ArrowUp}
          disabled={up === undefined}
          onSelect={() => step(up)}
        >
          Move up
        </MenuItem>
        <MenuItem
          icon={ArrowDown}
          disabled={down === undefined}
          onSelect={() => step(down)}
        >
          Move down
        </MenuItem>
        {children}
      </DropdownMenu>
    </span>
  );
}

/**
 * A folder's row: a root folder's or a set's. A click anywhere on it but
 * its buttons and its name's field opens or closes it (as Enter does on
 * its treeitem); it shows its grip and menu, its `name` (an InlineName),
 * a `count` (how many it holds), and `actions`, which show while the row is
 * hovered or has the focus.
 */
export function FolderRow({
  level,
  open,
  toggle,
  menu,
  name,
  count,
  actions,
}: {
  level: number;
  open: boolean;
  toggle: () => void;
  menu?: ReactNode;
  name: ReactNode;
  count?: number;
  actions?: ReactNode;
}) {
  const Icon = open ? FolderOpen : Folder;
  return (
    <div
      className="dt-row dt-folder-row"
      style={rowLevel(level)}
      onClick={(e) => {
        if (!(e.target as Element).closest("button, input, a")) toggle();
      }}
    >
      {menu}
      <ChevronRight size={14} className="dt-chevron" aria-hidden />
      <Icon size={15} className="dt-icon" aria-hidden />
      {name}
      {count !== undefined && <span className="dt-count">{count}</span>}
      {actions && <span className="dt-row-actions">{actions}</span>}
    </div>
  );
}
