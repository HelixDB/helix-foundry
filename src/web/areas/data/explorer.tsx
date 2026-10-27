// The Data explorer, the Data section's panel beside the sidebar. The shell
// loads it apart from the Data pages, so it brings their styles itself.
import "../ontology/ontology.css";
import "../documents/documents.css";
import "./data.css";
import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
} from "react";
import { useLocation } from "react-router-dom";
import { useIsFetching, useQuery, useQueryClient } from "@tanstack/react-query";
import { FolderPlus, Plus, Upload } from "lucide-react";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  IconButton,
  SearchInput,
  Skeleton,
  SkeletonText,
  useStoredState,
} from "../../kit";
import { documentQueries } from "../documents/documents-api";
import { ExplorerContext, rowLevel } from "./arrange";
import { DocumentsFolder } from "./documents-tree";
import {
  EMPTY_LAYOUT,
  arrange,
  explorerRows,
  rowKeyAction,
  selectedRow,
  selectionOf,
  type DocumentsNode,
  type RootKey,
} from "./layout";
import { canManage } from "./model";
import { DataTree, NewFolderDialog } from "./tree";
import { useDataUpload } from "./upload";
import { useDataTree } from "./use-tree";

// The Data explorer: the workspace's connections and documents as a tree,
// as in an IDE, arranged as the workspace's layout says (./layout.ts): a
// folder per connector kind holding its connections, the workspace's own
// folders of connections, and documents/ with a folder per document set
// people made. A connection opens on its tables and its own sets, whose
// documents its syncs write.
// A connection or a document opens its page, and the row of the page open
// is marked (aria-current), its folders opened when the URL changes. A
// folder opens and closes with a click. The tree is one Tab stop: the
// arrow keys move between rows and open and close folders, Enter opens a
// row and F2 renames it; Tab goes on to that row's buttons. Owners and
// admins rename rows in place, sync a connection from its row, rearrange
// the tree (./tree.tsx) and upload documents with Upload, a set's button,
// or by dropping files on a set or anywhere else in the explorer
// (./upload.tsx). A filter shows what matches, and while it is on, nothing
// moves.
//
// It reads everything from the URL and the query cache, so it can sit
// outside the page's routes. `onNavigate` runs on every link it follows
// (a phone's drawer closes on it).

export function DataExplorer({ onNavigate }: { onNavigate?: () => void }) {
  const { id, role } = useWorkspace(),
    manage = canManage(role),
    { pathname } = useLocation();
  const tree = useDataTree(id),
    { connectors, documents, sets } = tree;
  const files = useDataUpload();
  const [search, setSearch] = useState(""),
    [creating, setCreating] = useState(false),
    term = search.trim(),
    searching = !!term;
  // The folders closed in this browser: a kind, "documents" or "set:<id>";
  // and those opened of the ones that start closed: a connection ("c:<id>")
  // and its own sets, which read their documents once opened. A filter
  // shows every folder open, a connection on what it was found by, and
  // closes none; a connection's sets still open and close.
  const [closedText, setClosedText] = useStoredState("foundry.data.closed", ""),
    [openedText, setOpenedText] = useStoredState("foundry.data.opened", ""),
    closed = new Set(closedText.split(",").filter(Boolean)),
    opened = new Set(openedText.split(",").filter(Boolean)),
    owned = new Set(
      (sets.data || []).flatMap((s) => (s.connector ? [`set:${s.id}`] : [])),
    ),
    startsClosed = (key: string) => key.startsWith("c:") || owned.has(key),
    isOpen = (key: string) =>
      startsClosed(key)
        ? opened.has(key) || (searching && !owned.has(key))
        : searching || !closed.has(key),
    toggle = (key: string) => {
      if (searching && !owned.has(key)) return;
      const [set, save] = startsClosed(key)
        ? [opened, setOpenedText]
        : [closed, setClosedText];
      const next = new Set(set);
      if (!next.delete(key)) next.add(key);
      save([...next].join(","));
    };
  // A layout of a version this site doesn't know shows the default order.
  const layout = tree.unknown ? EMPTY_LAYOUT : tree.layout,
    all = arrange(layout, tree.data),
    nodes = term ? arrange(layout, tree.data, term) : all;
  const editable = manage && tree.editable,
    arranging = editable && !term;
  // A failed refetch keeps the tree it had; the error is for no data.
  const failed = !connectors.data && !!connectors.error;

  // The page open: its row, and the folders that hold it, opened once each
  // time the URL changes (once the lists show it), so a folder closed
  // afterwards stays closed. A connection's document is in none of the
  // lists: the document, which its page reads too, says its set.
  const selection = selectionOf(pathname),
    selected = useQuery({
      ...documentQueries.workspaceDocument(
        id,
        selection?.type === "document" ? selection.id : "",
      ),
      enabled: selection?.type === "document",
    }),
    setId = selected.data?.document.setId,
    current = selectedRow(nodes, selection, setId)?.key ?? null,
    holding = selectedRow(all, selection, setId)?.folders,
    revealed = useRef({ pathname: "", done: false });
  useEffect(() => {
    if (revealed.current.pathname !== pathname)
      revealed.current = { pathname, done: false };
    if (revealed.current.done || !holding) return;
    revealed.current.done = true;
    if (holding.some((key) => !startsClosed(key) && closed.has(key)))
      setClosedText(
        [...closed].filter((key) => !holding.includes(key)).join(","),
      );
    if (holding.some((key) => startsClosed(key) && !opened.has(key)))
      setOpenedText(
        [...new Set([...opened, ...holding.filter(startsClosed)])].join(","),
      );
  }, [pathname, holding?.join(",")]);

  // The rows as shown, for the keyboard: a connection's set's documents as
  // far as they are read. Reading a page re-renders the explorer, so its
  // rows hold the page. Tab lands on the row last focused, else the page's,
  // else the first.
  const qc = useQueryClient();
  useIsFetching({ queryKey: [id, "document-sets"] });
  const rows = explorerRows(nodes, isOpen, (setId) =>
      (
        qc.getQueryData(documentQueries.set(id, setId).queryKey)?.pages || []
      ).flatMap((p) => p.documents || []),
    ),
    shown = (key: string | null) =>
      key !== null && rows.some((r) => r.key === key);
  const [active, setActive] = useState<string | null>(null),
    tabStop = shown(active)
      ? active
      : shown(current)
        ? current
        : (rows[0]?.key ?? null);
  const body = useRef<HTMLDivElement>(null);
  // Tab reaches the buttons of that row only: the others' leave the Tab
  // order. The kit's buttons and menus take no tabIndex, so it is set here,
  // after each render.
  useLayoutEffect(() => {
    for (const button of body.current?.querySelectorAll<HTMLElement>(
      "[role=treeitem] > .dt-row button",
    ) || [])
      button.tabIndex =
        button.closest<HTMLElement>("[role=treeitem]")?.dataset.row === tabStop
          ? 0
          : -1;
  });
  const onKeyDown = (e: KeyboardEvent) => {
    const item = e.target as HTMLElement;
    if (
      item.getAttribute("role") !== "treeitem" ||
      e.altKey ||
      e.ctrlKey ||
      e.metaKey
    )
      return;
    const action = rowKeyAction(rows, item.dataset.row || "", e.key);
    if (!action) return;
    e.preventDefault();
    const own = (selector: string) =>
      item.querySelector<HTMLElement>(`:scope > .dt-row ${selector}`);
    switch (action.type) {
      case "focus":
        return body.current
          ?.querySelector<HTMLElement>(`[data-row="${CSS.escape(action.key)}"]`)
          ?.focus();
      case "toggle":
        return toggle(action.key);
      case "open": {
        const link = own(".dt-link");
        return link ? link.click() : toggle(action.key);
      }
      case "rename":
        return own(".dt-rename")?.click();
    }
  };

  const documentsFolder = (node: DocumentsNode, rootKeys: RootKey[]) => (
    <DocumentsFolder
      node={node}
      rootKeys={rootKeys}
      rename={editable}
      sets={(sets.data || []).filter((s) => !s.connector)}
      count={documents.data?.length || 0}
      isOpen={isOpen}
      toggle={toggle}
      manage={manage}
      over={files.over}
      onto={files.onto}
      onChoose={files.choose}
      status={
        sets.isPending || documents.isPending ? (
          <li role="none" className="dt-empty" style={rowLevel(2)}>
            <SkeletonText lines={2} />
          </li>
        ) : !sets.data || !documents.data ? (
          <li role="none" className="dt-empty" style={rowLevel(2)}>
            <span role="alert">
              Couldn’t load the documents:{" "}
              {(sets.error || documents.error)?.message}
            </span>{" "}
            <Button
              size="sm"
              variant="ghost"
              onClick={() => {
                void sets.refetch();
                void documents.refetch();
              }}
            >
              Retry
            </Button>
          </li>
        ) : undefined
      }
    />
  );
  return (
    <div
      className="dx"
      {...files.surface}
      onClick={(e) => {
        if ((e.target as Element).closest("a")) onNavigate?.();
      }}
    >
      <div className="dx-head">
        <h2>Connectors</h2>
        {manage && (
          <span className="dx-actions">
            <ButtonLink
              to={paths.connections(true)}
              variant="ghost"
              size="sm"
              icon={Plus}
              aria-label="Add data"
              title="Add data"
            />
            <IconButton
              icon={Upload}
              label="Upload documents"
              size="sm"
              disabled={!files.ready}
              onClick={() => files.choose()}
            />
            {editable && (
              <IconButton
                icon={FolderPlus}
                label="New folder"
                size="sm"
                onClick={() => setCreating(true)}
              />
            )}
          </span>
        )}
      </div>
      {!failed && (
        <SearchInput
          className="dx-filter"
          value={search}
          onChange={setSearch}
          label="Filter data"
          placeholder="Filter"
        />
      )}
      {files.chooser}
      {files.list}
      {connectors.data && <RefreshFailed query={connectors} />}
      {tree.unknown && (
        <p role="status" className="dt-note">
          A newer version of Foundry arranged this tree, so it shows in its
          usual order and can’t be rearranged here. Reload the page to get the
          newest version.
        </p>
      )}
      {failed && (
        <p className="dt-note">
          <span role="alert">
            Couldn’t load your connections: {connectors.error?.message}
          </span>{" "}
          <Button
            size="sm"
            variant="ghost"
            onClick={() => void connectors.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      <div
        ref={body}
        className="dx-body"
        onKeyDown={onKeyDown}
        onFocus={(e) => {
          const row = (e.target as Element).closest<HTMLElement>(
            "[role=treeitem]",
          );
          if (row?.dataset.row) setActive(row.dataset.row);
        }}
        onClick={(e) => {
          // A click on a row (its link included) leaves the focus on it, for
          // the arrow keys.
          const target = e.target as Element;
          if (target.closest("button, input, dialog")) return;
          target
            .closest<HTMLElement>("[role=treeitem]")
            ?.focus({ preventScroll: true });
        }}
      >
        {connectors.isPending ? (
          <div className="dx-loading" aria-busy="true">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} height={22} />
            ))}
          </div>
        ) : !nodes.length ? (
          <p className="dx-none">
            No sources, tables or documents match “{term}”.
          </p>
        ) : (
          <ExplorerContext.Provider value={{ current, tabStop }}>
            <DataTree
              nodes={nodes}
              layout={layout}
              data={tree.data}
              arranging={arranging}
              editable={editable}
              isOpen={isOpen}
              toggle={toggle}
              change={tree.change}
              documents={documentsFolder}
            />
          </ExplorerContext.Provider>
        )}
      </div>
      {creating && (
        <NewFolderDialog
          nodes={all}
          onCreate={(name) =>
            tree.change({
              type: "create-folder",
              folder: {
                id: crypto.randomUUID().replace(/-/g, "").slice(0, 12),
                name,
              },
            })
          }
          onClose={() => setCreating(false)}
        />
      )}
    </div>
  );
}
