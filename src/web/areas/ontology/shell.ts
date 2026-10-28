import { lazy } from "react";
import { Shapes } from "lucide-react";
import type { AreaShell } from "../../shell/types";
import { ontologyPaths as paths } from "./paths";
import { ontologyQueries } from "./queries";

// The Ontologies and Explorer panels, Ask's threads and command palette
// groups for the Ontology area. Each panel sits beside the sidebar, loaded
// with its section: the workspace's ontologies, the open one's pages under
// its row (on its documents pages, its documents explorer instead); the
// Explorer's pages fill theirs. Ask's threads sit in the sidebar itself,
// under the sections.

const OntologiesPanel = lazy(() =>
  import("./ontologies-panel").then((m) => ({ default: m.OntologiesPanel })),
);
const AskThreads = lazy(() =>
  import("./ask-threads").then((m) => ({ default: m.AskThreads })),
);
// The Explorer's pages fill its panel (the graph its labels, Records its
// types); this shows the ontology and load meanwhile.
const ExplorerContext = lazy(() =>
  import("../explore/panel").then((m) => ({ default: m.ExplorerContext })),
);

export const ontologyShell: AreaShell = {
  panel: {
    ontology: {
      kind: "component",
      label: "Ontologies",
      component: OntologiesPanel,
    },
    explore: {
      kind: "page",
      label: "Explorer panel",
      fallback: ExplorerContext,
    },
  },
  sidebar: { ask: { label: "Analyst threads", component: AskThreads } },
  palette: [
    {
      id: "ontologies",
      label: "Ontologies",
      icon: Shapes,
      load: async (ws) =>
        ((await ontologyQueries.list(ws).queryFn()).ontologies || []).map(
          (o) => ({
            id: o.id,
            title: o.name || o.id,
            subtitle: o.databaseName ? `Database ${o.databaseName}` : undefined,
            href: paths.ontology(o.id),
          }),
        ),
    },
  ],
};
