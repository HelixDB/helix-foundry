import { lazy } from "react";
import { Shapes } from "lucide-react";
import { FOUNDER } from "../../../shared/deployment";
import type { AreaShell } from "../../shell/types";
import { ontologyPaths as paths } from "./paths";
import { ontologyQueries } from "./queries";

// The Ontologies and Explorer panels, Ask's threads and command palette
// groups for the Ontology area. Each panel sits beside the sidebar, loaded
// with its section: the workspace's ontologies, the open one's pages under
// its row (its Explorer among them); the Explorer's pages, which have no
// sidebar link (../../shell/nav.ts), fill theirs. Ask's threads sit in the
// sidebar itself, under the sections: Founder's (with the Analyst's older
// threads under them) where the build has Founder, a grantee's included,
// else the Analyst's (founder-threads.tsx AskSidebar decides). The sample
// company's banner sits across the top.

const OntologiesPanel = lazy(() =>
  import("./ontologies-panel").then((m) => ({ default: m.OntologiesPanel })),
);
const AskSidebar = lazy(() =>
  import("./founder-threads").then((m) => ({ default: m.AskSidebar })),
);
// The Explorer's pages fill its panel (the graph its labels, Records its
// types); this shows the ontology and load meanwhile.
const ExplorerContext = lazy(() =>
  import("../explore/panel").then((m) => ({ default: m.ExplorerContext })),
);
const SampleBanner = lazy(() =>
  import("./sample-banner").then((m) => ({ default: m.SampleBanner })),
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
  // The label is fixed while the threads it names depend on the user.
  sidebar: {
    ask: {
      label: FOUNDER ? "Threads" : "Analyst threads",
      component: AskSidebar,
    },
  },
  banner: SampleBanner,
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
