import type { ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { ArrowLeft } from "lucide-react";
import { ButtonLink, Segmented, useTitle } from "../../kit";
import { paths } from "../../paths";
import {
  ComparePicker,
  LoadPicker,
  type LoadSelection,
} from "../ontology/load-selection";
import { ontologyName, useOntology } from "../ontology/scope";

/**
 * The Explorer's slim bar, shared by its two views of the ontology's
 * database so switching between them moves nothing: the way back to the
 * ontology's overview (its panel holds labels or types, not the Ontologies
 * panel), the title (for screen readers and the route announcer), the view
 * (Records or Graph, keeping the page's load), the load and, on the graph,
 * a second load to compare with; then the view's own tools. The ontology is
 * the one whose Explorer link opened it (the Ontologies panel), so there is
 * no picker of it. Without a selection (a database the user can't read)
 * there are no pickers.
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
  const { id: o, ontology } = useOntology(),
    navigate = useNavigate();
  return (
    <header className="ex-top">
      <div className="ex-top-lead">
        <ButtonLink
          to={paths.ontology(o)}
          variant="ghost"
          size="sm"
          icon={ArrowLeft}
          className="ex-top-back"
          title="Back to the ontology"
        >
          <span>{ontologyName(ontology)}</span>
        </ButtonLink>
        <h1 className="hf-sr-only">Explorer</h1>
        {/* Through source grants there is no graph: Records is the Explorer. */}
        {selection && !selection.shared && (
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
        {selection && <LoadPicker selection={selection} />}
        {selection && view === "graph" && (
          <ComparePicker selection={selection} />
        )}
      </div>
      {children && <div className="ex-top-tools">{children}</div>}
    </header>
  );
}
