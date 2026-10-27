import { useRef, useState, type DragEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { FoundryDocumentSet } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { useWorkspace } from "../../ui";
import {
  createDocumentSet,
  documentSetQueries,
} from "../documents/documents-api";
import {
  DEFAULT_SET_NAME,
  DOCUMENT_FORMATS,
  defaultSetId,
} from "../documents/model";
import { useOntologyNames } from "../documents/sets";
import { UploadCard, useUploads } from "../documents/uploads";
import { canManage } from "./model";

// Uploading documents from a Data surface (the explorer or the Data page):
// with its Upload documents button or a set's, through a hidden file
// chooser, or by dropping files on a set or anywhere else on the surface.
// Files go into the set chosen, else the default one (General, created on
// the first upload of a workspace with no set). A connector's sets take no
// uploads: none is a target, and a workspace with only those uploads as one
// with no set (defaultSetId). Each surface lists the
// uploads it started. Members upload nothing, and a surface takes no drop
// from them. `onUpload` hears which set each batch went into.

/** Whether a drag carries files (not text or a link). */
export const carriesFiles = (e: DragEvent) =>
  [...(e.dataTransfer?.types || [])].includes("Files");

export function useDataUpload(onUpload?: (setId: string) => void) {
  const { id, role, notify } = useWorkspace(),
    manage = canManage(role),
    qc = useQueryClient(),
    name = useOntologyNames();
  const sets = useQuery(documentSetQueries.list(id)),
    setList = sets.data || [];
  const { uploads, takes, gate, add, retry, dismiss } = useUploads();
  const input = useRef<HTMLInputElement>(null),
    choosing = useRef<string | undefined>(undefined);
  // The folder a drag of files would land in: a set's id, or "documents"
  // while there is no set.
  const [over, setOver] = useState<string | null>(null);
  // The default set is chosen from the loaded list only: one not loaded yet
  // would make a second General in a workspace whose sets have no General.
  const into = async (
    setId?: string,
  ): Promise<FoundryDocumentSet | undefined> => {
    if (!sets.data) {
      notify("The document sets are still loading. Try again in a moment.");
      return undefined;
    }
    const chosen = setList.find(
      (x) => x.id === (setId || defaultSetId(setList)),
    );
    if (chosen) return chosen;
    if (setId) {
      notify("That set was deleted.", true);
      return undefined;
    }
    try {
      return (await createDocumentSet(id, { name: DEFAULT_SET_NAME })).set;
    } catch (e) {
      // Another tab made it first: read the sets afresh.
      if (e instanceof ApiError && e.status === 409) {
        const fresh = await qc
          .fetchQuery({ ...documentSetQueries.list(id), staleTime: 0 })
          .catch(() => []);
        const found = fresh.find((x) => x.id === defaultSetId(fresh));
        if (!found) notify("Couldn’t read the document sets. Try again.", true);
        return found;
      }
      notify((e as Error).message, true);
      return undefined;
    } finally {
      void qc.invalidateQueries({ queryKey: [id, "document-sets"] });
    }
  };
  const upload = async (files: File[], setId?: string) => {
    // Refused files make no default set either.
    if (!files.length || !manage || !takes()) return;
    const target = await into(setId);
    if (!target) return;
    add(files, { id: target.id, name: target.name || "" });
    onUpload?.(target.id);
  };
  const dropped = (e: DragEvent, setId?: string) => {
    if (!carriesFiles(e)) return;
    e.preventDefault();
    e.stopPropagation();
    setOver(null);
    void upload([...(e.dataTransfer?.files || [])], setId);
  };
  return {
    // The sets are read, so an upload knows where it goes.
    ready: !!sets.data,
    over,
    /** Opens the file chooser for `setId`, else the default set. */
    choose: (setId?: string) => {
      choosing.current = setId;
      input.current?.click();
    },
    /**
     * Drops anywhere on the surface: a drag of files lights the default set
     * (or "documents" while there is none). Until the sets load it takes no
     * drop, and keeps the browser from opening the file.
     */
    surface: manage
      ? {
          onDragOver: (e: DragEvent) => {
            if (!carriesFiles(e)) return;
            e.preventDefault();
            if (!sets.data) {
              e.dataTransfer.dropEffect = "none";
              return;
            }
            e.dataTransfer.dropEffect = "copy";
            const target = defaultSetId(setList) || "documents";
            if (over !== target) setOver(target);
          },
          onDragLeave: (e: DragEvent) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node | null))
              setOver(null);
          },
          onDrop: (e: DragEvent) => dropped(e),
        }
      : {},
    /** Drops on the set `setId`: into it, not the default set. */
    onto: (setId: string) =>
      manage
        ? {
            onDragOver: (e: DragEvent) => {
              if (!carriesFiles(e)) return;
              e.preventDefault();
              e.stopPropagation();
              e.dataTransfer.dropEffect = "copy";
              if (over !== setId) setOver(setId);
            },
            onDrop: (e: DragEvent) => dropped(e, setId),
          }
        : {},
    // The hidden file chooser and the surface's uploads, for it to place.
    chooser: manage && (
      <input
        ref={input}
        type="file"
        multiple
        hidden
        accept={DOCUMENT_FORMATS.flatMap((f) => f.extensions).join(",")}
        aria-label="Choose documents"
        onChange={(e) => {
          void upload([...(e.target.files || [])], choosing.current);
          e.target.value = "";
        }}
      />
    ),
    list: (gate || uploads.length > 0) && (
      <>
        {gate}
        {uploads.length > 0 && (
          <ul className="dc-uploads dt-uploads" aria-label="Uploads">
            {uploads.map((u) => (
              <UploadCard
                key={u.key}
                upload={u}
                sets={setList}
                name={name}
                onRetry={() => retry(u)}
                onDismiss={() => dismiss(u.key)}
              />
            ))}
          </ul>
        )}
      </>
    ),
  };
}
