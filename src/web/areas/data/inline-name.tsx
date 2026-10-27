import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { Pencil } from "lucide-react";
import { useWorkspace } from "../../ui";

// A name in the Data explorer that owners and admins rename in place, as
// helix-foundry's suggested ontology renames a type: the pencil (or F2 on
// its row) swaps the name for a field, Enter or leaving it saves, Escape
// cancels, and the focus goes back to its row. While the new name saves it
// shows, so the old one doesn't flash back. Folders, connections, document
// sets and documents all use it. A name that links covers its row, and
// takes no Tab stop of its own: its row's treeitem does, and Enter on it
// follows the link.

export function InlineName({
  name,
  label = name,
  to,
  hint,
  manage,
  maxLength,
  check,
  save,
}: {
  name: string;
  // What its buttons call it: "Rename <label>". The name, unless a folder
  // shares it with what it holds.
  label?: string;
  // Where the name links; a folder's name links nowhere.
  to?: string;
  // More about it, in its tooltip under the name.
  hint?: string;
  manage: boolean;
  maxLength: number;
  // Why a new name can't be used, said in a toast instead of saving it.
  check?: (value: string) => string | undefined;
  save: (value: string) => Promise<unknown>;
}) {
  const { notify } = useWorkspace();
  const [editing, setEditing] = useState(false),
    [draft, setDraft] = useState(""),
    [saving, setSaving] = useState<string | null>(null);
  // Enter's or Escape's blur must not finish a second time.
  const editingRef = useRef(false);
  editingRef.current = editing;
  const pencil = useRef<HTMLButtonElement>(null),
    refocus = useRef(false);
  useEffect(() => {
    if (editing || !refocus.current) return;
    refocus.current = false;
    (
      pencil.current?.closest<HTMLElement>("[role=treeitem]") ?? pencil.current
    )?.focus();
  }, [editing]);
  // Enter and Escape give the focus back to the row; leaving the field
  // leaves it where it went.
  const finish = (keep: boolean, back: boolean) => {
    if (!editingRef.current) return;
    editingRef.current = false;
    const value = draft.trim(),
      problem = keep && value && value !== name ? check?.(value) : undefined;
    if (problem) notify(problem, true);
    else if (keep && value && value !== name) {
      setSaving(value);
      void save(value).finally(() => setSaving(null));
    }
    refocus.current = back;
    setEditing(false);
  };
  const shown = saving ?? name,
    title = hint ? `${shown}\n${hint}` : shown;
  if (editing)
    return (
      <input
        className="dt-name-input"
        value={draft}
        maxLength={maxLength}
        autoFocus
        aria-label={`Name for ${label}`}
        onFocus={(e) => e.currentTarget.select()}
        onChange={(e) => setDraft(e.target.value)}
        onBlur={() => finish(true, false)}
        onKeyDown={(e) => {
          if (e.key !== "Enter" && e.key !== "Escape") return;
          // Focus returns to the pencil before keyup, which would otherwise
          // press it.
          e.preventDefault();
          e.stopPropagation();
          finish(e.key === "Enter", true);
        }}
      />
    );
  return (
    <span className="dt-name-line">
      {to ? (
        <Link to={to} className="dt-link" title={title} tabIndex={-1}>
          {shown}
        </Link>
      ) : (
        <span className="dt-name" title={title}>
          {shown}
        </span>
      )}
      {manage && (
        <button
          ref={pencil}
          type="button"
          className="dt-rename"
          aria-label={`Rename ${label}`}
          disabled={saving !== null}
          onClick={() => {
            setDraft(name);
            setEditing(true);
          }}
        >
          <Pencil size={12} aria-hidden />
        </button>
      )}
    </span>
  );
}
