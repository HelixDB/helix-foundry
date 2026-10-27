// An ontology's documents as a file system, nested under its Documents in
// the Ontologies panel. The shell loads it apart from the ontology's pages,
// so it brings the Data explorer's styles, whose rows it shows.
import "../ontology/ontology.css";
import "../data/data.css";
import {
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { FileText } from "lucide-react";
import type { FoundryDocument } from "../../../shared/wfe";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import { Button, Skeleton, StatusDot, plural, useStoredState } from "../../kit";
import {
  ExplorerContext,
  FolderRow,
  rowLevel,
  useTreeItem,
} from "../data/arrange";
import { rowKeyAction, type ExplorerRow } from "../data/layout";
import { documentQueries, documentSetQueries } from "./documents-api";
import {
  documentDot,
  ontologySetFolders,
  type OntologySetFolder,
} from "./model";

// The open ontology's documents, on every page of it, under its Documents
// link: the sets the ontology reads as folders, each with what it reads of
// them (`ontologySetFolders`), as the Data explorer's documents/ shows them
// (../data/documents-tree.tsx), read only. A document's dot says what the
// ontology's current load made of it (`documentDot`), in the row's name too;
// a set counts its documents that need review, when there are any. A
// document opens its page; its row is marked while it is open, and its set
// opened each time the URL changes. A set opens and closes with a click,
// remembered in this browser. The tree is one Tab stop, and the arrow keys,
// Home, End and Enter move as in the Data explorer.
//
// It reads everything from the URL and the query cache (the documents page
// shares its list), so it can sit outside the page's routes. `onNavigate`
// runs on every link it follows (a phone's drawer closes on it).

export function OntologyDocumentsTree({
  ontologyId: o,
  documentId,
  onNavigate,
}: {
  ontologyId: string;
  // The document open, if one is.
  documentId?: string;
  onNavigate?: () => void;
}) {
  const { id: ws } = useWorkspace();
  const documents = useQuery(documentQueries.list(ws, o)),
    sets = useQuery(documentSetQueries.list(ws));
  // The sets closed in this browser, as "set:<id>".
  const [closedText, setClosedText] = useStoredState(
      "foundry.ontology.documents.closed",
      "",
    ),
    closed = new Set(closedText.split(",").filter(Boolean)),
    isOpen = (key: string) => !closed.has(key),
    toggle = (key: string) => {
      const next = new Set(closed);
      if (!next.delete(key)) next.add(key);
      setClosedText([...next].join(","));
    };
  const list = documents.data?.documents || [],
    folders = ontologySetFolders(o, sets.data || [], list);
  const failed =
    (!documents.data && !!documents.error) || (!sets.data && !!sets.error);

  // The open document's row, and its set, opened once each time the
  // document changes (once the lists show it), so a set closed afterwards
  // stays closed.
  const current = documentId ? `d:${documentId}` : null,
    holding = documentId
      ? folders.find((f) => f.documents.some((d) => d.id === documentId))
      : undefined,
    revealed = useRef({ documentId: "", done: false });
  useEffect(() => {
    if (!documentId) return;
    if (revealed.current.documentId !== documentId)
      revealed.current = { documentId, done: false };
    if (revealed.current.done || !holding) return;
    revealed.current.done = true;
    const key = `set:${holding.set.id}`;
    if (closed.has(key))
      setClosedText([...closed].filter((k) => k !== key).join(","));
  }, [documentId, holding?.set.id]);

  // The rows as shown, for the keyboard. Tab lands on the row last focused,
  // else the open document's, else the first.
  const rows = folders.flatMap((f): ExplorerRow[] => {
      const key = `set:${f.set.id}`;
      return isOpen(key)
        ? [
            { key, level: 1, state: "open" },
            ...f.documents.map(
              (d): ExplorerRow => ({
                key: `d:${d.id}`,
                level: 2,
                state: "leaf",
              }),
            ),
          ]
        : [{ key, level: 1, state: "closed" }];
    }),
    shown = (key: string | null) =>
      key !== null && rows.some((r) => r.key === key);
  const [active, setActive] = useState<string | null>(null),
    tabStop = shown(active)
      ? active
      : shown(current)
        ? current
        : (rows[0]?.key ?? null);
  const body = useRef<HTMLDivElement>(null);
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
    // Nothing here is renamed.
    if (!action || action.type === "rename") return;
    e.preventDefault();
    if (action.type === "focus")
      return body.current
        ?.querySelector<HTMLElement>(`[data-row="${CSS.escape(action.key)}"]`)
        ?.focus();
    const link = item.querySelector<HTMLElement>(":scope > .dt-row .dt-link");
    if (action.type === "open" && link) return link.click();
    toggle(action.key);
  };

  let tree: ReactNode;
  if (failed)
    tree = (
      <p className="dx-none">
        <span role="alert">
          Couldn’t load the documents:{" "}
          {(documents.error || sets.error)?.message}
        </span>{" "}
        <Button
          size="sm"
          variant="ghost"
          onClick={() => {
            void documents.refetch();
            void sets.refetch();
          }}
        >
          Retry
        </Button>
      </p>
    );
  else if (documents.isPending || sets.isPending)
    tree = (
      <div className="dx-loading" aria-busy="true">
        {Array.from({ length: 3 }, (_, i) => (
          <Skeleton key={i} height={22} />
        ))}
      </div>
    );
  else if (!folders.length)
    tree = (
      <p className="dx-none">No documents yet: upload them in Connectors.</p>
    );
  else
    tree = (
      <ExplorerContext.Provider value={{ current, tabStop }}>
        <ul role="tree" className="dt-tree on-docs-tree" aria-label="Documents">
          {folders.map((f) => (
            <SetItem
              key={f.set.id}
              ontologyId={o}
              folder={f}
              open={isOpen(`set:${f.set.id}`)}
              toggle={() => toggle(`set:${f.set.id}`)}
            />
          ))}
        </ul>
      </ExplorerContext.Provider>
    );

  return (
    <div
      ref={body}
      className="on-docs-nested"
      onKeyDown={onKeyDown}
      onFocus={(e) => {
        const row = (e.target as Element).closest<HTMLElement>(
          "[role=treeitem]",
        );
        if (row?.dataset.row) setActive(row.dataset.row);
      }}
      onClick={(e) => {
        const target = e.target as Element;
        if (target.closest("a")) onNavigate?.();
        // A click on a row (its link included) leaves the focus on it, for
        // the arrow keys.
        if (target.closest("button, input")) return;
        target
          .closest<HTMLElement>("[role=treeitem]")
          ?.focus({ preventScroll: true });
      }}
    >
      {documents.data && <RefreshFailed query={documents} />}
      {tree}
    </div>
  );
}

// A set: its name (how many documents it shows and its description in the
// name's tooltip) and how many of them need review.
function SetItem({
  ontologyId,
  folder: { set: s, documents, review },
  open,
  toggle,
}: {
  ontologyId: string;
  folder: OntologySetFolder;
  open: boolean;
  toggle: () => void;
}) {
  const item = useTreeItem(`set:${s.id}`, 1, open);
  const label = s.name || s.id;
  return (
    <li
      className="dt-node"
      aria-label={review ? `${label}, ${review} to review` : label}
      {...item}
    >
      <FolderRow
        level={1}
        open={open}
        toggle={toggle}
        count={review || undefined}
        name={
          <span
            className="dt-name"
            title={[label, plural(documents.length, "document"), s.description]
              .filter(Boolean)
              .join("\n")}
          >
            {label}
          </span>
        }
      />
      {open && (
        <ul role="group" className="dt-group">
          {documents.length ? (
            documents.map((d) => (
              <DocumentItem key={d.id} ontologyId={ontologyId} document={d} />
            ))
          ) : (
            <li role="none" className="dt-empty" style={rowLevel(2)}>
              Empty.
            </li>
          )}
        </ul>
      )}
    </li>
  );
}

// A document: its name, opening its page in the ontology, and its dot.
function DocumentItem({
  ontologyId,
  document: d,
}: {
  ontologyId: string;
  document: FoundryDocument;
}) {
  const item = useTreeItem(`d:${d.id}`, 2);
  const name = d.name || d.id,
    dot = documentDot(d);
  return (
    <li className="dt-node" aria-label={`${name}, ${dot.label}`} {...item}>
      <div className="dt-row" style={rowLevel(2)}>
        <span className="dt-twist" aria-hidden />
        <FileText size={14} className="dt-icon" aria-hidden />
        <Link
          to={paths.document(ontologyId, d.id)}
          className="dt-link"
          title={`${name}\n${dot.label}`}
          tabIndex={-1}
        >
          {name}
        </Link>
        <StatusDot status={dot.status} label={dot.label} />
      </div>
    </li>
  );
}
