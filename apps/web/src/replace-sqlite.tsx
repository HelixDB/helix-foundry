import { useRef, useState, type ReactNode } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import { useWorkspace } from "./ui";
import type { Resource } from "../../../packages/shared/src";

// A SQLite connection is refreshed by replacing its file. The new copy is
// uploaded and read first, so a copy without one of the connection's tables
// changes nothing; then each table's source is pointed at the new file and
// re-imported into the dataset it already has. Choosing the file again after
// a failure repeats every step safely: the upload is named by its content.
export function useReplaceSqliteFile(sources: Resource[], notifyErrors = true) {
  const { id, notify } = useWorkspace(),
    qc = useQueryClient(),
    input = useRef<HTMLInputElement>(null),
    [pending, setPending] = useState(false),
    [error, setError] = useState<string | null>(null);
  const replace = async (chosen: File) => {
    setPending(true);
    setError(null);
    try {
      const body = new FormData();
      body.set("file", chosen);
      const { file } = await api<{ file: string }>(
        `/workspaces/${id}/sources/sqlite`,
        body,
        "POST",
      );
      const read = await api<{ tables?: { name: string }[] }>(
        `/workspaces/${id}/sources/test`,
        { kind: "sqlite", name: chosen.name, config: { file } },
      );
      // A SQLite source is named after the table it imports.
      const missing = sources
        .map((s) => s.name)
        .filter((table) => !read.tables?.some((t) => t.name === table));
      if (missing.length)
        throw new Error(
          `${chosen.name} has no ${missing.length === 1 ? "table" : "tables"} named ${missing.join(", ")}. Choose a copy of the same database.`,
        );
      for (const source of sources)
        await api(
          `/workspaces/${id}/onboarding/sources/${source.id}`,
          { kind: "sqlite", name: source.name, config: { file } },
          "PUT",
        );
      notify("File replaced. Importing its tables…");
    } catch (e) {
      const message =
        e instanceof Error ? e.message : "The file could not be read.";
      setError(message);
      if (notifyErrors) notify(message, true);
    } finally {
      await qc.invalidateQueries({ queryKey: [id] });
      setPending(false);
    }
  };
  return {
    pending,
    error,
    choose: () => input.current?.click(),
    input: (
      <input
        ref={input}
        type="file"
        hidden
        accept=".db,.sqlite,.sqlite3,.db3"
        onChange={(e) => {
          const chosen = e.target.files?.[0];
          e.target.value = "";
          if (chosen) replace(chosen);
        }}
      />
    ),
  };
}

// The action for one connection, drawn by the caller in its own style. In a
// dialog its error shows below what the caller draws, since a notification
// would be hidden behind the dialog; elsewhere it is a notification.
export function ReplaceSqliteFile({
  sources,
  inDialog = false,
  children,
}: {
  sources: Resource[];
  inDialog?: boolean;
  children: (choose: () => void, pending: boolean) => ReactNode;
}) {
  const replace = useReplaceSqliteFile(sources, !inDialog);
  return (
    <>
      {replace.input}
      {children(replace.choose, replace.pending)}
      {inDialog && replace.error && (
        <p className="error-text small" role="alert">
          {replace.error}
        </p>
      )}
    </>
  );
}
