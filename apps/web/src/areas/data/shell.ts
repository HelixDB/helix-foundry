import { Cable, Database } from "lucide-react";
import type { AreaShell } from "../../shell/types";
import { paths } from "../../paths";
import { relative } from "../../api";

// Sidebar children and command palette groups for Data Connection.
export const dataShell: AreaShell = {
  // The Data page (connections and their tables), plus Graph compute.
  subnav: {
    data: [{ to: paths.graphCompute(), label: "Graph compute", match: ["/data/graph"] }],
  },
  palette: [
    {
      id: "datasets",
      label: "Datasets",
      kind: "dataset",
      icon: Database,
      href: (r) => paths.dataset(r.id),
      subtitle: (r) =>
        [
          r.data.generation ? "Pipeline output" : "Dataset",
          r.data.profile?.rows !== undefined
            ? `${r.data.profile.rows} rows`
            : "",
          r.data.snapshotAt ? relative(r.data.snapshotAt) : "",
        ]
          .filter(Boolean)
          .join(" · "),
    },
    {
      id: "sources",
      label: "Connections",
      kind: "source",
      icon: Cable,
      href: (r) => paths.connection(r.id),
      subtitle: (r) =>
        [r.data.connectionName, r.data.hostedProvider || r.data.kind]
          .filter(Boolean)
          .join(" · "),
    },
  ],
};
