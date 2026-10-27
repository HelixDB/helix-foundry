import { Fragment, useRef, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useInfiniteQuery } from "@tanstack/react-query";
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  pointerWithin,
  useSensor,
  useSensors,
  type CollisionDetection,
  type DragEndEvent,
  type DragOverEvent,
  type Modifier,
} from "@dnd-kit/core";
import {
  SortableContext,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import {
  ChevronRight,
  FileText,
  Folder,
  FolderInput,
  RefreshCw,
  Table2,
  Trash2,
} from "lucide-react";
import type {
  FoundryDocument,
  FoundryDocumentSet,
  SyncedTable,
} from "../../../shared/wfe";
import { number, relative } from "../../api";
import { paths } from "../../paths";
import { useAction, useWorkspace } from "../../ui";
import {
  Button,
  ConfirmDialog,
  Dialog,
  IconButton,
  MenuItem,
  MenuSeparator,
  Select,
  SkeletonText,
  SourceLogo,
  StatusDot,
  connectorLabel,
  cx,
  formatDate,
  plural,
} from "../../kit";
import { documentQueries } from "../documents/documents-api";
import { formatBytes, readersText } from "../documents/model";
import { useOntologyNames } from "../documents/sets";
import {
  ArrangeContext,
  FolderRow,
  RowMenu,
  rowLevel,
  useArranging,
  useRow,
  useTreeItem,
  type DragData,
} from "./arrange";
import { syncConnector, updateConnector } from "./data-api";
import { InlineName } from "./inline-name";
import {
  MAX_FOLDER_NAME,
  destinations,
  dropBefore,
  folderNameProblem,
  tableKey,
  type FolderKey,
  type DocumentsNode,
  type KindKey,
  type Layout,
  type LayoutChange,
  type RootKey,
  type TreeData,
  type TreeNode,
} from "./layout";
import {
  canManage,
  isEventSource,
  isLiveSync,
  lastSyncedAt,
  rowTotal,
  syncBadge,
  type TreeItem,
} from "./model";

// The Data explorer's tree as the layout arranges it: folders at the root
// (a kind's, the workspace's own, and documents/), connections in the
// first two, each opening (its chevron) on what it lists: its tables, then
// its own document sets, a set paging in its documents newest first, 50 at
// a time. Those sets and documents are the connection's, so nothing here
// changes or moves them. Owners and admins
// drag a row by its grip within its list, a connection into another custom
// folder or back to its kind's, or move one from the grip's menu (Move up,
// Move down, Move to…); they create, rename and delete custom folders and
// rename a kind's folder in place. A drop on a folder's row goes last in
// it; on a row of another folder, before or after that row, whichever half
// it lands on.

// Where a dragged row may drop: in its own list, and a connection into any
// custom folder or its kind's.
function accepts(active: DragData, target: DragData) {
  switch (active.type) {
    case "root":
      return target.type === "root";
    case "connector": {
      const home = (key: string) =>
        key.startsWith("folder:") || key === `kind:${active.kind}`;
      return target.type === "connector"
        ? home(target.container)
        : target.type === "root" && home(target.key);
    }
    case "set":
      return target.type === "set";
    case "document":
      return target.type === "document" && target.setId === active.setId;
  }
}
const collide: CollisionDetection = (args) => {
  const active = args.active.data.current as DragData;
  return pointerWithin({
    ...args,
    droppableContainers: args.droppableContainers.filter((c) =>
      accepts(active, c.data.current as DragData),
    ),
  });
};
// The copy of a dragged row moves up and down only, as the rows do.
const upAndDown: Modifier = ({ transform }) => ({ ...transform, x: 0 });
const folderOf = (key: KindKey | FolderKey) =>
  key.startsWith("folder:") ? key.slice("folder:".length) : null;

export function DataTree({
  nodes,
  layout,
  data,
  arranging,
  editable,
  isOpen,
  toggle,
  change,
  documents,
}: {
  nodes: TreeNode[];
  layout: Layout;
  data: TreeData;
  arranging: boolean;
  // Folders can be renamed: an owner or admin, every list read and a layout
  // this site knows.
  editable: boolean;
  // Whether a folder (its key) shows what it holds: a filter opens all.
  isOpen: (key: string) => boolean;
  toggle: (key: string) => void;
  change: (change: LayoutChange) => void;
  documents: (node: DocumentsNode, rootKeys: RootKey[]) => ReactNode;
}) {
  const [into, setInto] = useState<string | null>(null),
    [dragged, setDragged] = useState<DragData | null>(null),
    focus = useRef<string | null>(null);
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
  );
  const rootKeys = nodes.map((n) => n.key);
  const connectorIds = (key: string) =>
    nodes.flatMap((n) =>
      n.key === key && n.type !== "documents"
        ? n.items.map((i) => i.connector.id)
        : [],
    );
  const sets = nodes.flatMap((n) => (n.type === "documents" ? n.sets : []));
  // A connection dropped on another folder: that folder's key.
  const target = (e: DragOverEvent | DragEndEvent) => {
    const a = e.active.data.current as DragData,
      o = e.over?.data.current as DragData | undefined;
    if (a.type !== "connector" || !o) return null;
    const key =
      o.type === "root" ? o.key : o.type === "connector" ? o.container : null;
    return key && key !== a.container ? key : null;
  };
  const drop = (e: DragEndEvent) => {
    setInto(null);
    setDragged(null);
    const a = e.active.data.current as DragData,
      over = e.over,
      o = over?.data.current as DragData | undefined;
    if (!over || !o) return;
    // `accepts` let only these through: the same kind of row, and for a
    // connection a folder it may go in.
    if (a.type === "root" && o.type === "root") {
      const before = dropBefore(rootKeys, a.key, o.key);
      if (before !== undefined)
        change({ type: "move-root", key: a.key, before });
    } else if (a.type === "connector" && o.type === "root") {
      // On a folder's head: last in it.
      if (o.key !== "documents")
        change({
          type: "move-connector",
          id: a.id,
          folder: folderOf(o.key),
          before: null,
        });
    } else if (a.type === "connector" && o.type === "connector") {
      const ids = connectorIds(o.container),
        pointer = (e.activatorEvent as PointerEvent).clientY + e.delta.y,
        below = pointer > over.rect.top + over.rect.height / 2;
      // In its own list as the list shows it; in another, before or after
      // the row the pointer is on, by which half.
      const before =
        o.container === a.container
          ? dropBefore(ids, a.id, o.id)
          : below
            ? (ids[ids.indexOf(o.id) + 1] ?? null)
            : o.id;
      if (before !== undefined)
        change({
          type: "move-connector",
          id: a.id,
          folder: folderOf(o.container),
          before,
        });
    } else if (a.type === "set" && o.type === "set") {
      const before = dropBefore(
        sets.map((f) => f.set.id),
        a.id,
        o.id,
      );
      if (before !== undefined) change({ type: "move-set", id: a.id, before });
    } else if (a.type === "document" && o.type === "document") {
      const held = sets.find((f) => f.set.id === a.setId)?.documents || [],
        before = dropBefore(
          held.map((d) => d.id),
          a.id,
          o.id,
        );
      if (before !== undefined)
        change({ type: "move-document", id: a.id, before });
    }
  };
  return (
    <ArrangeContext.Provider
      value={{ on: arranging, change, layout, data, into, focus }}
    >
      <DndContext
        sensors={sensors}
        collisionDetection={collide}
        onDragStart={(e) => setDragged(e.active.data.current as DragData)}
        onDragOver={(e) => setInto(target(e))}
        onDragEnd={drop}
        onDragCancel={() => {
          setInto(null);
          setDragged(null);
        }}
      >
        <SortableContext
          items={rootKeys}
          strategy={verticalListSortingStrategy}
        >
          <ul
            role="tree"
            className={cx("dt-tree", arranging && "is-arranging")}
            aria-label="Connectors"
          >
            {nodes.map((n) =>
              n.type === "documents" ? (
                <Fragment key={n.key}>{documents(n, rootKeys)}</Fragment>
              ) : (
                <ConnectorFolder
                  key={n.key}
                  node={n}
                  rootKeys={rootKeys}
                  isOpen={isOpen}
                  toggle={toggle}
                  rename={editable}
                />
              ),
            )}
          </ul>
        </SortableContext>
        <DragOverlay dropAnimation={null} modifiers={[upAndDown]}>
          {dragged && (
            <div className="dt-drag">
              {dragged.type === "connector" ? (
                <SourceLogo source={dragged.kind} size={16} />
              ) : dragged.type === "document" ? (
                <FileText size={15} className="dt-icon" aria-hidden />
              ) : (
                <Folder size={15} className="dt-icon" aria-hidden />
              )}
              <span className="dt-name">{dragged.name}</span>
            </div>
          )}
        </DragOverlay>
      </DndContext>
    </ArrangeContext.Provider>
  );
}

// A kind's folder or a custom one, with its connections.
function ConnectorFolder({
  node,
  rootKeys,
  isOpen,
  toggle,
  rename,
}: {
  node: Exclude<TreeNode, DocumentsNode>;
  rootKeys: RootKey[];
  isOpen: (key: string) => boolean;
  toggle: (key: string) => void;
  rename: boolean;
}) {
  const open = isOpen(node.key);
  const { on, change, into } = useArranging(),
    row = useRow(node.key, { type: "root", key: node.key, name: node.name }),
    item = useTreeItem(node.key, 1, open);
  const [deleting, setDeleting] = useState(false);
  const ids = node.items.map((i) => i.connector.id);
  const remove = () =>
    node.type === "folder" && change({ type: "delete-folder", id: node.id });
  return (
    <li
      ref={row.ref}
      style={row.style}
      className={cx(
        "dt-node",
        into === node.key && "is-over",
        row.dragging && "is-dragging",
      )}
      aria-label={node.name}
      {...item}
    >
      <FolderRow
        level={1}
        open={open}
        toggle={() => toggle(node.key)}
        count={node.items.length}
        name={
          <InlineName
            name={node.name}
            label={`${node.name} folder`}
            manage={rename}
            maxLength={MAX_FOLDER_NAME * 2}
            save={async (name) =>
              change(
                node.type === "folder"
                  ? { type: "rename-folder", id: node.id, name }
                  : { type: "rename", key: node.key, name },
              )
            }
          />
        }
        menu={
          on && (
            <RowMenu
              rowId={node.key}
              label={`${node.name} folder actions`}
              handle={row.handle}
              id={node.key}
              siblings={rootKeys}
              move={(before) =>
                change({ type: "move-root", key: node.key, before })
              }
            >
              {node.type === "folder" && (
                <>
                  <MenuSeparator />
                  <MenuItem
                    icon={Trash2}
                    danger
                    onSelect={() => (ids.length ? setDeleting(true) : remove())}
                  >
                    Delete folder
                  </MenuItem>
                </>
              )}
            </RowMenu>
          )
        }
      />
      {open && (
        <SortableContext
          items={ids.map((id) => `c:${id}`)}
          strategy={verticalListSortingStrategy}
        >
          <ul role="group" className="dt-group">
            {node.items.length ? (
              node.items.map((item) => (
                <ConnectorItem
                  key={item.connector.id}
                  item={item}
                  container={node.key}
                  siblings={ids}
                  isOpen={isOpen}
                  toggle={toggle}
                />
              ))
            ) : (
              <li role="none" className="dt-empty" style={rowLevel(2)}>
                Empty. Drag a connection here, or move one from its menu.
              </li>
            )}
          </ul>
        </SortableContext>
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${node.name}?`}
          description={
            ids.length === 1
              ? "Its connection goes back to its kind’s folder. Nothing else changes."
              : `Its ${ids.length} connections go back to their kinds’ folders. Nothing else changes.`
          }
          confirmLabel="Delete folder"
          tone="danger"
          onConfirm={() => {
            setDeleting(false);
            remove();
          }}
          onCancel={() => setDeleting(false)}
        />
      )}
    </li>
  );
}

// One connection: its grip, its chevron while it lists something, its
// kind's logo, its name (renamed in place, as helix-foundry's suggested
// ontology renames a type; what it holds in the name's tooltip), its
// sync's status as a dot (said in the row's name), Sync while hovered or
// focused, and, open, its tables and its own sets (those a search found it
// by, while one is on). The name opens its page; the chevron (or the arrow
// keys) opens it here. An API source has no Sync: its pieces sync as they
// arrive.
function ConnectorItem({
  item,
  container,
  siblings,
  isOpen,
  toggle,
}: {
  item: TreeItem;
  container: KindKey | FolderKey;
  siblings: string[];
  isOpen: (key: string) => boolean;
  toggle: (key: string) => void;
}) {
  const { id, role } = useWorkspace(),
    manage = canManage(role),
    action = useAction(),
    { on, change, layout, data, focus } = useArranging();
  const { connector: c, tables, sets } = item;
  const name = c.name || connectorLabel(c.kind),
    lists = tables.length + sets.length > 0,
    open = lists && isOpen(`c:${c.id}`);
  const row = useRow(`c:${c.id}`, {
    type: "connector",
    id: c.id,
    kind: c.kind,
    container,
    name,
  });
  const treeItem = useTreeItem(`c:${c.id}`, 2, lists ? open : undefined);
  const [moving, setMoving] = useState(false);
  const badge = syncBadge(c.lastSync),
    live = isLiveSync(c.lastSync),
    pushed = isEventSource(c.kind),
    all = c.tables || [],
    refreshed = lastSyncedAt(c),
    error =
      c.lastSync?.status === "CONNECTOR_SYNC_STATUS_FAILED" && c.lastSync.error;
  const move = (folder: string | null, before: string | null) =>
    change({ type: "move-connector", id: c.id, folder, before });
  return (
    <li
      ref={row.ref}
      style={row.style}
      className={cx("dt-node", row.dragging && "is-dragging")}
      aria-label={`${name}, ${badge.label}`}
      {...treeItem}
    >
      <div className="dt-row" style={rowLevel(2)}>
        {on && (
          <RowMenu
            rowId={`c:${c.id}`}
            label={`${name} actions`}
            handle={row.handle}
            id={c.id}
            siblings={siblings}
            move={(before) => move(folderOf(container), before)}
          >
            <MenuItem
              icon={FolderInput}
              disabled={!layout.folders.length}
              hint={layout.folders.length ? undefined : "No folders yet"}
              onSelect={() => setMoving(true)}
            >
              Move to…
            </MenuItem>
          </RowMenu>
        )}
        {lists ? (
          // The pointer's way to open it here; the keyboard's is the row's
          // arrow keys, as on any folder.
          <span
            className="dt-toggle"
            aria-hidden
            onClick={() => toggle(`c:${c.id}`)}
          >
            <ChevronRight size={14} className="dt-chevron" />
          </span>
        ) : (
          <span className="dt-twist" aria-hidden />
        )}
        <SourceLogo source={c.kind} size={16} />
        <InlineName
          name={name}
          to={paths.connection(c.id)}
          hint={
            error
              ? `Last sync failed: ${error}`
              : pushed
                ? all.length
                  ? `${plural(rowTotal(c), "piece")}${refreshed ? ` · synced ${relative(refreshed)}` : ""}`
                  : "Pieces appear after their first sync"
                : all.length
                  ? `${plural(all.length, "table")} · ${number(rowTotal(c))} records${refreshed ? ` · synced ${relative(refreshed)}` : ""}`
                  : live
                    ? "Copying tables…"
                    : "Tables appear after the first sync"
          }
          manage={manage}
          maxLength={120}
          save={(value) =>
            action.run(
              () => updateConnector(id, c.id, { name: value }),
              "Connection renamed",
            )
          }
        />
        <StatusDot status={badge.status} label={badge.label} />
        {manage && !pushed && (
          <span className="dt-row-actions">
            <IconButton
              icon={RefreshCw}
              label={`Sync ${name}`}
              size="sm"
              variant="ghost"
              disabled={action.pending || live}
              onClick={() =>
                void action.run(() => syncConnector(id, c.id), "Sync queued")
              }
            />
          </span>
        )}
      </div>
      {open && (
        <ul role="group" className="dt-group">
          {tables.map((t) => (
            <TableItem key={t.name} connectorId={c.id} table={t} />
          ))}
          {sets.map((s) => (
            <ConnectorSetItem
              key={s.id}
              set={s}
              open={isOpen(`set:${s.id}`)}
              toggle={() => toggle(`set:${s.id}`)}
            />
          ))}
        </ul>
      )}
      {moving && (
        <MoveToDialog
          name={name}
          options={destinations(layout, data, c.id)}
          onMove={(folder) => {
            // Its grip takes the focus in its new folder, if that is open.
            focus.current = `c:${c.id}`;
            move(folder, null);
            setMoving(false);
          }}
          onClose={() => setMoving(false)}
        />
      )}
    </li>
  );
}

// A table of a connection: it opens the table's page.
function TableItem({
  connectorId,
  table: t,
}: {
  connectorId: string;
  table: SyncedTable;
}) {
  const item = useTreeItem(tableKey(connectorId, t.name), 3);
  const rows = number(Number(t.rowCount || 0));
  return (
    <li className="dt-node" aria-label={t.name} {...item}>
      <div className="dt-row" style={rowLevel(3)}>
        <span className="dt-twist" aria-hidden />
        <Table2 size={14} className="dt-icon" aria-hidden />
        <Link
          to={paths.syncedTable(connectorId, t.name)}
          className="dt-link"
          title={`${t.name}\n${rows} rows`}
          tabIndex={-1}
        >
          {t.name}
        </Link>
        <span className="dt-count">{rows}</span>
      </div>
    </li>
  );
}

// A connection's own set: its name (what it holds and which ontologies read
// it in the name's tooltip) and how many documents it holds. Open, it reads
// its documents newest first, 50 at a time, and Show more reads the next 50.
function ConnectorSetItem({
  set: s,
  open,
  toggle,
}: {
  set: FoundryDocumentSet;
  open: boolean;
  toggle: () => void;
}) {
  const { id } = useWorkspace(),
    ontologyName = useOntologyNames(),
    item = useTreeItem(`set:${s.id}`, 3, open);
  const documents = useInfiniteQuery({
    ...documentQueries.set(id, s.id),
    enabled: open,
  });
  const label = s.name || s.id,
    held = s.documents || 0,
    max = s.connector?.maxDocuments,
    shown = documents.data?.pages.flatMap((p) => p.documents || []) || [];
  return (
    <li className="dt-node" aria-label={label} {...item}>
      <FolderRow
        level={3}
        open={open}
        toggle={toggle}
        count={held}
        name={
          <span
            className="dt-name"
            title={[
              label,
              plural(held, "document"),
              // At its cap, older ones at the source aren't here.
              max && held >= max ? `Keeps the newest ${number(max)}` : "",
              readersText(label, s.readBy || [], ontologyName),
              s.description,
            ]
              .filter(Boolean)
              .join("\n")}
          >
            {label}
          </span>
        }
      />
      {open && (
        <ul role="group" className="dt-group">
          {documents.isPending ? (
            <li role="none" className="dt-empty" style={rowLevel(4)}>
              <SkeletonText lines={2} />
            </li>
          ) : documents.isError && !shown.length ? (
            <li role="none" className="dt-empty" style={rowLevel(4)}>
              <span role="alert">
                Couldn’t load its documents: {documents.error.message}
              </span>{" "}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void documents.refetch()}
              >
                Retry
              </Button>
            </li>
          ) : !shown.length ? (
            <li role="none" className="dt-empty" style={rowLevel(4)}>
              Empty.
            </li>
          ) : (
            shown.map((d) => <ConnectorDocumentItem key={d.id} document={d} />)
          )}
          {documents.hasNextPage && (
            <li role="none" className="dt-empty" style={rowLevel(4)}>
              <Button
                size="sm"
                variant="ghost"
                disabled={documents.isFetchingNextPage}
                onClick={() => void documents.fetchNextPage()}
              >
                {documents.isFetchingNextPage ? "Loading…" : "Show more"}
              </Button>
            </li>
          )}
        </ul>
      )}
    </li>
  );
}

// A document of a connection's set: its name (its size and its time at the
// source in the tooltip). It opens its page; the connection's syncs alone
// rename, move or delete it.
function ConnectorDocumentItem({ document: d }: { document: FoundryDocument }) {
  const item = useTreeItem(`d:${d.id}`, 4);
  const name = d.name || d.id;
  return (
    <li className="dt-node" aria-label={name} {...item}>
      <div className="dt-row" style={rowLevel(4)}>
        <span className="dt-twist" aria-hidden />
        <FileText size={14} className="dt-icon" aria-hidden />
        <Link
          to={paths.dataDocument(d.id)}
          className="dt-link"
          title={[
            name,
            [formatBytes(d.sizeBytes), d.source?.at && formatDate(d.source.at)]
              .filter(Boolean)
              .join(" · "),
          ].join("\n")}
          tabIndex={-1}
        >
          {name}
        </Link>
      </div>
    </li>
  );
}

// Moves a connection into a custom folder, or back to its kind's ("" in the
// select: a folder's id is never empty).
function MoveToDialog({
  name,
  options,
  onMove,
  onClose,
}: {
  name: string;
  options: { folder: string | null; name: string }[];
  onMove: (folder: string | null) => void;
  onClose: () => void;
}) {
  const [to, setTo] = useState(options[0]?.folder ?? "");
  return (
    <Dialog title={`Move ${name}`} size="sm" onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!options.length) return;
          onMove(to || null);
        }}
      >
        <label className="hf-field">
          <span>Folder</span>
          <Select
            label="Folder"
            autoFocus
            value={to}
            onChange={setTo}
            options={options.map((o) => ({
              value: o.folder ?? "",
              label: o.name,
            }))}
          />
        </label>
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={!options.length}>
            Move
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

/** Names a new custom folder at the tree's root; it holds connections. */
export function NewFolderDialog({
  nodes,
  onCreate,
  onClose,
}: {
  // The tree without a search, for names already taken.
  nodes: readonly TreeNode[];
  onCreate: (name: string) => void;
  onClose: () => void;
}) {
  const [name, setName] = useState("");
  const problem = folderNameProblem(name, nodes);
  return (
    <Dialog
      title="New folder"
      description="A folder of connections, shared with the workspace. Drag connections into it, or move them from their menus."
      size="sm"
      onClose={onClose}
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (problem) return;
          onCreate(name);
          onClose();
        }}
      >
        <label className="hf-field">
          <span>Name</span>
          <input
            required
            data-autofocus
            maxLength={MAX_FOLDER_NAME * 2}
            placeholder="e.g. Finance"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {name.trim() && problem && <small>{problem}</small>}
        </label>
        <div className="hf-form-actions">
          <Button onClick={onClose}>Cancel</Button>
          <Button type="submit" variant="primary" disabled={!!problem}>
            Create folder
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
