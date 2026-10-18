// Downloads the published graph in an interchange format from the
// /graph/export routes. Shown next to the view switch in the Explorer.
import { useState } from "react";
import { useAction, useWorkspace } from "../../ui";

const FORMATS = [
  { id: "graphml", name: "GraphML", file: "graph.graphml", note: "Gephi, yEd, Neo4j" },
  { id: "jsonld", name: "JSON-LD", file: "graph.jsonld", note: "Linked data" },
  { id: "dot", name: "Graphviz DOT", file: "graph.dot", note: "Graphviz, docs" },
];
type Format = (typeof FORMATS)[number];

export default function ExportMenu() {
  const { id } = useWorkspace(),
    action = useAction(),
    [open, setOpen] = useState(false);
  const save = (f: Format) =>
    action.run(async () => {
      const res = await fetch(`/api/v1/workspaces/${id}/graph/export/${f.id}`, {
        credentials: "same-origin",
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data.error || "Export failed");
      }
      const url = URL.createObjectURL(await res.blob()),
        a = document.createElement("a");
      a.href = url;
      a.download = f.file;
      a.click();
      URL.revokeObjectURL(url);
    }, `Downloaded ${f.file}`);
  return (
    <div className="export-menu">
      <button
        className="button"
        aria-expanded={open}
        aria-haspopup="menu"
        onClick={() => setOpen((o) => !o)}
      >
        Export
      </button>
      {open && (
        <div className="export-menu-list" role="menu">
          {FORMATS.map((f) => (
            <button
              key={f.id}
              role="menuitem"
              className="export-menu-item"
              disabled={action.pending}
              onClick={() => {
                setOpen(false);
                save(f);
              }}
            >
              <strong>{f.name}</strong>
              <span>{f.note}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
