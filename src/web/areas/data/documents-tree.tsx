import { useState, type DragEvent, type ReactNode } from "react";
import {
  SortableContext,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import {
  FileText,
  FolderInput,
  FolderPlus,
  NotebookPen,
  Trash2,
  Upload,
} from "lucide-react";
import type { FoundryDocument, FoundryDocumentSet } from "../../../shared/wfe";
import { relative } from "../../api";
import { paths } from "../../paths";
import { useAction, useWorkspace } from "../../ui";
import {
  ConfirmDialog,
  IconButton,
  MenuItem,
  StatusDot,
  cx,
  plural,
} from "../../kit";
import {
  deleteDocumentSet,
  renameDocument,
  updateDocumentSet,
} from "../documents/documents-api";
import {
  FORMAT_NAMES,
  MAX_DOCUMENT_NAME,
  MAX_SET_NAME,
  documentNameProblem,
  fileRead,
  fileReadLabel,
  formatBytes,
  readersText,
  setNameProblem,
} from "../documents/model";
import { MoveDialog, SetDialog, useOntologyNames } from "../documents/sets";
import {
  FolderRow,
  RowMenu,
  rowLevel,
  useArranging,
  useRow,
  useTreeItem,
} from "./arrange";
import { InlineName } from "./inline-name";
import { MAX_FOLDER_NAME, type DocumentsNode, type RootKey } from "./layout";
import type { SetFolder } from "./model";

// The Data explorer's documents/ folder: a folder per document set, each
// with its documents, in the layout's order. A document opens its page.
// Owners and admins create a set, rename one or a document in place,
// describe a set, delete an empty one, and upload into a set with its
// button or by dropping files on it (./upload.tsx; files dropped anywhere
// else in the explorer go to the default set, which lights up). They
// rename documents/ in place, and move it, a set among the sets or a
// document within its set by its grip; the grip's menu moves a document to
// another set (the WFE's move). Folders open and close like the kinds'
// (`isOpen`, `toggle`, keyed "documents" and "set:<id>").

/** Drop handlers for a set: files dropped on it go into it. */
type DropOnto = (setId: string) => {
  onDragOver?: (e: DragEvent) => void;
  onDrop?: (e: DragEvent) => void;
};

export function DocumentsFolder({
  node,
  rootKeys,
  rename,
  sets,
  count,
  isOpen,
  toggle,
  manage,
  over,
  onto,
  onChoose,
  status,
}: {
  node: DocumentsNode;
  rootKeys: RootKey[];
  // documents/ can be renamed: an owner or admin, every list read and a
  // layout this site knows.
  rename: boolean;
  // Every set, for names a rename can't take.
  sets: readonly FoundryDocumentSet[];
  // The workspace's documents.
  count: number;
  // Whether a folder (its key) shows what it holds: a filter opens all.
  isOpen: (key: string) => boolean;
  toggle: (key: string) => void;
  manage: boolean;
  // The folder a drag of files is over: a set's id, or "documents".
  over: string | null;
  onto: DropOnto;
  onChoose: (setId?: string) => void;
  // Shown in place of the sets while they load or failed to.
  status?: ReactNode;
}) {
  const { on, change } = useArranging(),
    row = useRow("documents", {
      type: "root",
      key: "documents",
      name: node.name,
    });
  const [creating, setCreating] = useState(false);
  const open = isOpen("documents"),
    item = useTreeItem("documents", 1, open),
    folders = node.sets,
    setIds = folders.map((f) => f.set.id);
  return (
    <li
      ref={row.ref}
      style={row.style}
      className={cx(
        "dt-node",
        over === "documents" && "is-over",
        row.dragging && "is-dragging",
      )}
      aria-label={node.name}
      {...item}
    >
      <FolderRow
        level={1}
        open={open}
        toggle={() => toggle("documents")}
        count={count}
        name={
          <InlineName
            name={node.name}
            label={`${node.name} folder`}
            manage={rename}
            maxLength={MAX_FOLDER_NAME * 2}
            save={async (name) =>
              change({ type: "rename", key: "documents", name })
            }
          />
        }
        menu={
          on && (
            <RowMenu
              rowId="documents"
              label={`${node.name} folder actions`}
              handle={row.handle}
              id="documents"
              siblings={rootKeys}
              move={(before) =>
                change({ type: "move-root", key: "documents", before })
              }
            />
          )
        }
        actions={
          manage && (
            <IconButton
              icon={FolderPlus}
              label="New set"
              size="sm"
              variant="ghost"
              onClick={() => setCreating(true)}
            />
          )
        }
      />
      {open && (
        <SortableContext
          items={setIds.map((id) => `set:${id}`)}
          strategy={verticalListSortingStrategy}
        >
          <ul role="group" className="dt-group">
            {status ??
              (!folders.length ? (
                <li role="none" className="dt-empty" style={rowLevel(2)}>
                  {manage ? (
                    <>
                      No documents yet. Drop {FORMAT_NAMES} files here, or{" "}
                      <button
                        type="button"
                        className="dc-drop-choose"
                        onClick={() => onChoose()}
                      >
                        choose files
                      </button>
                      .
                    </>
                  ) : (
                    "No documents yet. An owner or admin can upload them."
                  )}
                </li>
              ) : (
                folders.map((f) => (
                  <SetItem
                    key={f.set.id}
                    folder={f}
                    siblings={setIds}
                    sets={sets}
                    open={isOpen(`set:${f.set.id}`)}
                    toggle={() => toggle(`set:${f.set.id}`)}
                    manage={manage}
                    over={over === f.set.id}
                    onto={onto}
                    onChoose={onChoose}
                  />
                ))
              ))}
          </ul>
        </SortableContext>
      )}
      {creating && <SetDialog sets={sets} onClose={() => setCreating(false)} />}
    </li>
  );
}

// A set: its name (renamed in place; what it holds and which ontologies
// read it in the name's tooltip), how many documents it holds, and Upload,
// Describe and Delete while hovered or focused.
function SetItem({
  folder: { set: s, documents },
  siblings,
  sets,
  open,
  toggle,
  manage,
  over,
  onto,
  onChoose,
}: {
  folder: SetFolder;
  // The sets' ids in the tree's order.
  siblings: string[];
  sets: readonly FoundryDocumentSet[];
  open: boolean;
  toggle: () => void;
  manage: boolean;
  over: boolean;
  onto: DropOnto;
  onChoose: (setId?: string) => void;
}) {
  const { id } = useWorkspace(),
    action = useAction(),
    name = useOntologyNames(),
    { on, change } = useArranging(),
    row = useRow(`set:${s.id}`, {
      type: "set",
      id: s.id,
      name: s.name || s.id,
    }),
    item = useTreeItem(`set:${s.id}`, 2, open);
  const [editing, setEditing] = useState(false),
    [deleting, setDeleting] = useState(false);
  const label = s.name || s.id,
    held = s.documents ?? documents.length;
  const docIds = documents.map((d) => d.id);
  return (
    <li
      ref={row.ref}
      style={row.style}
      className={cx(
        "dt-node",
        over && "is-over",
        row.dragging && "is-dragging",
      )}
      aria-label={label}
      {...item}
      {...onto(s.id)}
    >
      <FolderRow
        level={2}
        open={open}
        toggle={toggle}
        count={held}
        name={
          <InlineName
            name={label}
            hint={[
              plural(held, "document"),
              readersText(label, s.readBy || [], name),
              s.description,
            ]
              .filter(Boolean)
              .join("\n")}
            manage={manage}
            maxLength={MAX_SET_NAME * 2}
            check={(value) => setNameProblem(value, sets, s.id)}
            save={(value) =>
              action.run(
                () => updateDocumentSet(id, s.id, { name: value }),
                "Set renamed",
              )
            }
          />
        }
        menu={
          on && (
            <RowMenu
              rowId={`set:${s.id}`}
              label={`${label} set actions`}
              handle={row.handle}
              id={s.id}
              siblings={siblings}
              move={(before) => change({ type: "move-set", id: s.id, before })}
            />
          )
        }
        actions={
          manage && (
            <>
              <IconButton
                icon={Upload}
                label={`Upload into ${label}`}
                size="sm"
                variant="ghost"
                onClick={() => onChoose(s.id)}
              />
              <IconButton
                icon={NotebookPen}
                label={`Describe ${label}`}
                size="sm"
                variant="ghost"
                onClick={() => setEditing(true)}
              />
              <IconButton
                icon={Trash2}
                label={`Delete ${label}`}
                title={
                  held
                    ? "Move or delete its documents to delete it"
                    : `Delete ${label}`
                }
                size="sm"
                variant="ghost"
                disabled={held > 0 || action.pending}
                onClick={() => setDeleting(true)}
              />
            </>
          )
        }
      />
      {open && (
        <SortableContext
          items={docIds.map((d) => `d:${d}`)}
          strategy={verticalListSortingStrategy}
        >
          <ul role="group" className="dt-group">
            {documents.length ? (
              documents.map((d) => (
                <DocumentItem
                  key={d.id}
                  document={d}
                  siblings={docIds}
                  manage={manage}
                />
              ))
            ) : (
              <li role="none" className="dt-empty" style={rowLevel(3)}>
                {manage
                  ? "Empty. Drop files here to upload them into it."
                  : "Empty."}
              </li>
            )}
          </ul>
        </SortableContext>
      )}
      {editing && (
        <SetDialog set={s} sets={sets} onClose={() => setEditing(false)} />
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${label}?`}
          description="It holds no documents. Versions that read it read nothing more from it."
          confirmLabel="Delete"
          tone="danger"
          pending={action.pending}
          onConfirm={async () => {
            await action.run(
              () => deleteDocumentSet(id, s.id),
              `Deleted ${label}.`,
            );
            setDeleting(false);
          }}
          onCancel={() => setDeleting(false)}
        />
      )}
    </li>
  );
}

// One document: its name (renamed in place; its size, when it was uploaded
// and, for a table file, its rows in the name's tooltip), and a dot when
// its upload didn't finish or it couldn't be read. It opens its page.
function DocumentItem({
  document: d,
  siblings,
  manage,
}: {
  document: FoundryDocument;
  // Its set's documents' ids in the tree's order.
  siblings: string[];
  manage: boolean;
}) {
  const { id } = useWorkspace(),
    action = useAction(),
    { on, change } = useArranging(),
    row = useRow(`d:${d.id}`, {
      type: "document",
      id: d.id,
      setId: d.setId || "",
      name: d.name || d.id,
    }),
    item = useTreeItem(`d:${d.id}`, 3);
  const [moving, setMoving] = useState(false);
  const name = d.name || d.id,
    unfinished = d.status === "pending_upload",
    read = fileRead(d);
  return (
    <li
      ref={row.ref}
      style={row.style}
      className={cx("dt-node", row.dragging && "is-dragging")}
      aria-label={unfinished ? `${name}, Upload didn’t finish` : name}
      {...item}
    >
      <div className="dt-row" style={rowLevel(3)}>
        {on && (
          <RowMenu
            rowId={`d:${d.id}`}
            label={`${name} actions`}
            handle={row.handle}
            id={d.id}
            siblings={siblings}
            move={(before) =>
              change({ type: "move-document", id: d.id, before })
            }
          >
            <MenuItem
              icon={FolderInput}
              disabled={d.status === "deleted"}
              onSelect={() => setMoving(true)}
            >
              Move to another set…
            </MenuItem>
          </RowMenu>
        )}
        <span className="dt-twist" aria-hidden />
        <FileText size={14} className="dt-icon" aria-hidden />
        <InlineName
          name={name}
          to={paths.dataDocument(d.id)}
          hint={
            unfinished
              ? "Its upload didn’t finish. Upload the file again."
              : [
                  formatBytes(d.sizeBytes),
                  `uploaded ${relative(d.createdAt || "")}`,
                  fileReadLabel(d),
                ]
                  .filter(Boolean)
                  .join(" · ")
          }
          manage={manage && d.status !== "deleted"}
          maxLength={MAX_DOCUMENT_NAME * 2}
          check={documentNameProblem}
          save={(value) =>
            action.run(
              () => renameDocument(id, d.id, value),
              "Document renamed",
            )
          }
        />
        {unfinished && (
          <StatusDot status="stale" label="Upload didn’t finish" />
        )}
        {read.state === "failed" && (
          <StatusDot status="failed" label={read.message} />
        )}
      </div>
      {moving && <MoveDialog document={d} onClose={() => setMoving(false)} />}
    </li>
  );
}
