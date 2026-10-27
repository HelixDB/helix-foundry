import { lazy } from "react";
import { Cable, FileText, Table2 } from "lucide-react";
import { connectorLabel, formatNumber } from "../../kit";
import type { AreaShell } from "../../shell/types";
import { paths } from "../../paths";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { formatBytes } from "../documents/model";
import { connectorsQuery } from "./data-api";

// The Connectors explorer panel and command palette groups for the Data
// area (Connectors in the sidebar). The
// explorer lists connections and documents beside the sidebar; the palette
// finds connections and synced tables from one connector list each, and
// documents from the documents and sets lists.
const connectors = (workspaceId: string) =>
  connectorsQuery(workspaceId)
    .queryFn()
    .then((r) => r.connectors || []);

// The Connectors explorer: the section's panel beside the sidebar, loaded with Data.
const DataExplorer = lazy(() =>
  import("./explorer").then((m) => ({ default: m.DataExplorer })),
);

export const dataShell: AreaShell = {
  panel: {
    data: {
      kind: "component",
      label: "Connectors explorer",
      component: DataExplorer,
    },
  },
  palette: [
    {
      id: "connections",
      label: "Connections",
      icon: Cable,
      load: async (workspaceId) =>
        (await connectors(workspaceId)).map((c) => ({
          id: c.id,
          title: c.name || connectorLabel(c.kind),
          subtitle: [
            connectorLabel(c.kind),
            `${(c.tables || []).length} tables`,
          ].join(" · "),
          href: paths.connection(c.id),
        })),
    },
    {
      id: "tables",
      label: "Tables",
      icon: Table2,
      load: async (workspaceId) =>
        (await connectors(workspaceId)).flatMap((c) =>
          (c.tables || []).map((t) => ({
            id: c.id + "/" + t.name,
            title: t.name,
            subtitle: [
              c.name || connectorLabel(c.kind),
              `${formatNumber(t.rowCount)} rows`,
            ].join(" · "),
            description: (t.columns || []).map((col) => col.name).join(" "),
            href: paths.syncedTable(c.id, t.name),
          })),
        ),
    },
    {
      id: "documents",
      label: "Documents",
      icon: FileText,
      load: async (workspaceId) => {
        const [documents, sets] = await Promise.all([
          documentQueries.workspace(workspaceId).queryFn(),
          documentSetQueries.list(workspaceId).queryFn(),
        ]);
        const setName = new Map(sets.map((s) => [s.id, s.name || s.id]));
        return documents.map((d) => ({
          id: d.id,
          title: d.name || d.id,
          subtitle: [setName.get(d.setId || ""), formatBytes(d.sizeBytes)]
            .filter(Boolean)
            .join(" · "),
          href: paths.dataDocument(d.id),
        }));
      },
    },
  ],
};
