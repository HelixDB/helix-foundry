import { useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { RotateCcw, X } from "lucide-react";
import type { FoundryDocumentSet } from "../../../shared/wfe";
import { trackEvent } from "../../analytics";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { Button, IconButton, SourceLogo, cx } from "../../kit";
import { AiGate, SettingsLinked, useAiConfigured } from "../workspace/ai-gate";
import { documentsChanged, uploadDocument } from "./documents-api";
import {
  formatBytes,
  formatOf,
  kindText,
  readsText,
  uploadText,
  type UploadStep,
} from "./model";

// Uploads in this tab, one card each: hashed here, the bytes PUT straight
// to storage with a progress bar, then completed. A file the workspace has
// already says which set it is in; a finished upload says what the
// ontologies that read its set do with it, or that they don't read its
// format yet. Locally, completing an upload needs an AI provider (any
// document may be read by a model): until an owner chooses one, `takes()`
// refuses files before anything is sent, and `gate` says why.

export type Upload = {
  key: string;
  file: File;
  // The set it goes in, and its name for the card.
  setId: string;
  setName: string;
  clientToken: string;
  state: UploadStep;
};

/** The upload cards and what runs them. */
export function useUploads() {
  const { id } = useWorkspace(),
    qc = useQueryClient();
  const [uploads, setUploads] = useState<Upload[]>([]),
    [refused, setRefused] = useState(false),
    aiReady = useAiConfigured();
  const set = (key: string, state: UploadStep) =>
    setUploads((list) =>
      list.map((u) => (u.key === key ? { ...u, state } : u)),
    );
  const run = async (u: Upload, retry: boolean) => {
    const end = await uploadDocument(
      id,
      u.file,
      u.setId,
      u.clientToken,
      (s) => set(u.key, s),
      retry,
    );
    // The same bytes already in the workspace are no new document.
    const format =
      end.step === "done" &&
      end.result.kind !== "existing" &&
      formatOf(end.document.mediaType)?.mediaType;
    if (format) trackEvent("document_uploaded", { format });
    return end;
  };
  // Uploads may start reads in any ontology: once a batch settles, what
  // they changed is read again, once.
  const settle = async (ends: Promise<UploadStep>[]) => {
    if ((await Promise.all(ends)).some((e) => e.step === "done"))
      await documentsChanged(qc, id);
  };
  return {
    uploads,
    /** Whether files may be uploaded now; when not, `gate` says why. */
    takes: () => {
      setRefused(aiReady === false);
      return aiReady !== false;
    },
    // Why files chosen weren't sent: no AI provider is set up.
    gate: refused && aiReady === false && (
      <AiGate action="Uploading documents" />
    ),
    add: (files: File[], into: { id: string; name: string }) => {
      const added = files.map((file) => ({
        key: crypto.randomUUID(),
        file,
        setId: into.id,
        setName: into.name,
        // One per file chosen: a retry is the same upload.
        clientToken: crypto.randomUUID(),
        state: { step: "checking" } as UploadStep,
      }));
      setUploads((list) => [...added, ...list]);
      void settle(added.map((u) => run(u, false)));
    },
    retry: (u: Upload) => {
      set(u.key, { step: "checking" });
      void settle([run(u, true)]);
    },
    dismiss: (key: string) =>
      setUploads((list) => list.filter((u) => u.key !== key)),
  };
}

export function UploadCard({
  upload: u,
  sets,
  name,
  onRetry,
  onDismiss,
}: {
  upload: Upload;
  sets: readonly FoundryDocumentSet[];
  // An ontology's name by id.
  name: (ontologyId: string) => string;
  onRetry: () => void;
  onDismiss: () => void;
}) {
  const s = u.state,
    done = s.step === "done",
    failed = s.step === "failed";
  // The document may be the workspace's already, in another set.
  const set = done ? sets.find((x) => x.id === s.document.setId) : undefined,
    where = set?.name || u.setName;
  return (
    <li
      className={cx("dc-upload", failed && "is-failed")}
      aria-label={u.file.name}
    >
      <SourceLogo source="upload" size={28} />
      <div className="dc-upload-body">
        <strong>
          {done ? (
            <Link to={paths.dataDocument(s.document.id)}>{u.file.name}</Link>
          ) : (
            u.file.name
          )}
        </strong>
        <span role={failed ? "alert" : "status"}>
          {formatBytes(u.file.size)} · <SettingsLinked text={uploadText(s)} />
          {done &&
            (s.result.kind === "existing"
              ? `: it’s in ${where}${s.document.name !== u.file.name.trim() ? ` as ${s.document.name}` : ""}`
              : ` into ${where}`)}
        </span>
        {done && s.result.kind === "uploaded" && (
          <small className="dc-upload-reads">
            {kindText(s.document, where) ??
              readsText(s.result.reads, {
                name,
                setName: where,
                readers: set?.readBy || [],
              })}
          </small>
        )}
        {!done && !failed && (
          <progress
            aria-label={`Uploading ${u.file.name}`}
            max={s.step === "uploading" ? s.total || 1 : 1}
            value={
              s.step === "uploading"
                ? s.loaded
                : s.step === "completing"
                  ? 1
                  : undefined
            }
          />
        )}
      </div>
      {failed && s.retry && (
        <Button size="sm" icon={RotateCcw} onClick={onRetry}>
          Retry
        </Button>
      )}
      {(done || failed) && (
        <IconButton
          icon={X}
          label={`Dismiss ${u.file.name}`}
          size="sm"
          variant="ghost"
          onClick={onDismiss}
        />
      )}
    </li>
  );
}
