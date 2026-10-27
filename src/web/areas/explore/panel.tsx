import "./explore.css";
import {
  Link,
  matchPath,
  useLocation,
  useSearchParams,
} from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Database, History, Network, Table2 } from "lucide-react";
import { Skeleton, cx } from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { chooseLoad, loadOptionLabel, withLoad } from "../ontology/model";
import { ontologyQueries } from "../ontology/queries";
import { ontologyName } from "../ontology/scope";

// The Explorer section's panel while its page puts nothing there: the page
// loading, a compare (each side keeps its labels), an entity's page, or a
// database that can't be read. It names the ontology and the load the
// Explorer reads, and opens its views. The shell renders it outside the
// ontology's scope, so it reads the ontology from the path and the rest
// from the query cache the pages share; its links keep the page's `?load=`.

export function ExplorerContext({ onNavigate }: { onNavigate?: () => void }) {
  const { pathname } = useLocation();
  const at = matchPath({ path: "/explore/:ontologyId", end: false }, pathname),
    o = at?.params.ontologyId;
  if (!o)
    return (
      <p className="hf-section-panel-note">
        The Explorer reads an ontology’s database. Its labels and types show
        here once one is open.
      </p>
    );
  const view = matchPath("/explore/:o/records", pathname)
    ? "records"
    : matchPath("/explore/:o", pathname)
      ? "graph"
      : "other";
  return <Context key={o} ontologyId={o} view={view} onNavigate={onNavigate} />;
}

function Context({
  ontologyId: o,
  view,
  onNavigate,
}: {
  ontologyId: string;
  view: "graph" | "records" | "other";
  onNavigate?: () => void;
}) {
  const { id: ws } = useWorkspace(),
    [params] = useSearchParams();
  const ontology = useQuery(ontologyQueries.ontology(ws, o)).data?.ontology;
  const loads = useQuery(ontologyQueries.loads(ws, o)),
    list = loads.data?.loads ?? [];
  const param = params.get("load"),
    choice = chooseLoad(
      list,
      list.find((l) => l.current)?.id ?? ontology?.currentLoad?.id,
      param,
    ),
    compare = list.find((l) => l.id === params.get("compare"));
  const views = [
    { id: "graph", label: "Graph", icon: Network, to: paths.explore(o) },
    { id: "records", label: "Records", icon: Table2, to: paths.records(o) },
  ] as const;
  return (
    <div className="ex-context">
      <div className="ex-context-head">
        <h2>{ontology ? ontologyName(ontology) : <Skeleton width={140} />}</h2>
        {ontology?.databaseName && (
          <p title={`Database ${ontology.databaseName}`}>
            <Database size={13} aria-hidden />
            <span>{ontology.databaseName}</span>
          </p>
        )}
      </div>
      <nav className="ex-context-views" aria-label="Explorer views">
        {views.map((v) => (
          <Link
            key={v.id}
            to={withLoad(v.to, param)}
            onClick={onNavigate}
            className={cx("ex-context-link", view === v.id && "is-active")}
            aria-current={view === v.id ? "page" : undefined}
          >
            <v.icon size={16} aria-hidden />
            {v.label}
          </Link>
        ))}
      </nav>
      <section className="ex-context-load" aria-label="Load">
        <h3 className="ex-section-title">Load</h3>
        {loads.isPending ? (
          <Skeleton width={120} />
        ) : (
          <p>
            {choice.kind === "load"
              ? loadOptionLabel(choice.load)
              : choice.kind === "missing"
                ? "A deleted load"
                : choice.kind === "other"
                  ? "Other data: what no load wrote"
                  : "Nothing loaded yet"}
          </p>
        )}
        {compare && <p>Compared with {loadOptionLabel(compare)}</p>}
        <Link
          to={paths.loads(o)}
          onClick={onNavigate}
          className="ex-context-link"
        >
          <History size={16} aria-hidden />
          Data loads
        </Link>
      </section>
    </div>
  );
}
