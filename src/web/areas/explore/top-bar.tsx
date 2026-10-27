import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { Segmented, useTitle } from "../../kit";
import { paths } from "../../paths";
import {
  ComparePicker,
  LoadPicker,
  type LoadSelection,
} from "../ontology/load-selection";
import { OntologyPicker, useOntology } from "../ontology/scope";

/**
 * The Explorer's slim bar, shared by its two views of the ontology's
 * database so switching between them moves nothing: the title (for screen
 * readers and the route announcer; the rail shows the section), the view
 * (Records or Graph, keeping the page's load), the ontology, the load and,
 * on the graph, a second load to compare with; then the view's own tools.
 * Without a selection (a database the user can't read) only the ontology
 * changes.
 */
export function ExplorerTop({
  view,
  selection,
  children,
}: {
  view: "graph" | "records";
  selection?: LoadSelection;
  children?: ReactNode;
}) {
  useTitle("Explorer");
  const { id: o } = useOntology(),
    navigate = useNavigate();
  return (
    <header className="ex-top">
      <div className="ex-top-lead">
        <h1 className="hf-sr-only">Explorer</h1>
        {selection && (
          <Segmented
            label="View"
            value={view}
            options={[
              { value: "records", label: "Records" },
              { value: "graph", label: "Graph" },
            ]}
            onChange={(v) =>
              navigate(
                selection.link(
                  v === "records" ? paths.records(o) : paths.explore(o),
                ),
              )
            }
          />
        )}
        <OntologyPicker
          ontologyId={o}
          to={view === "records" ? paths.records : paths.explore}
        />
        {selection && <LoadPicker selection={selection} />}
        {selection && view === "graph" && (
          <ComparePicker selection={selection} />
        )}
      </div>
      {children && <div className="ex-top-tools">{children}</div>}
    </header>
  );
}
