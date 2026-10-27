// Data area: route components lazy-loaded by main.tsx. Documents share
// their cards, text and lists with the Ontology area's views of them, and
// the callouts and stacks of the Ontology area's pages.
import "../ontology/ontology.css";
import "../documents/documents.css";
import "./data.css";
import { Route, Routes, useParams } from "react-router-dom";
import { NotFound } from "../../shell/ErrorBoundary";
import { ConnectionDetail } from "./connection";
import { DataDocumentPage } from "./document";
import { DataOverview } from "./overview";
import { SyncedTablePage } from "./table";

// /data (?add=1 opens the connector picker): the workspace's data at a
// glance
export function DataHome() {
  return <DataOverview />;
}
// /data/connections/:connectorId
export function ConnectionPage() {
  const { connectorId = "" } = useParams();
  return <ConnectionDetail key={connectorId} connectorId={connectorId} />;
}
// /data/connections/:connectorId/tables/:table
export function TablePage() {
  const { connectorId = "", table = "" } = useParams();
  return (
    <SyncedTablePage
      key={connectorId + "/" + table}
      connectorId={connectorId}
      table={table}
    />
  );
}
// /data/documents/:documentId
export function DocumentPage() {
  const { documentId = "" } = useParams();
  return <DataDocumentPage key={documentId} documentId={documentId} />;
}
// /data/*: the Data area's route table (main.tsx delegates the whole
// prefix). The Data explorer is the section's panel beside the sidebar
// (shell.ts), outside these routes.
export function DataRoutes() {
  return (
    <Routes>
      <Route index element={<DataHome />} />
      <Route path="connections/:connectorId" element={<ConnectionPage />} />
      <Route
        path="connections/:connectorId/tables/:table"
        element={<TablePage />}
      />
      <Route path="documents/:documentId" element={<DocumentPage />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
