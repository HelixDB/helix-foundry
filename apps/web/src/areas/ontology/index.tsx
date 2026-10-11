// Ontology area: route components lazy-loaded by main.tsx.
import "./ontology.css";
import { useState } from "react";
import { Route, Routes, useParams } from "react-router-dom";
import { OntologyPage } from "../../graphs";
import RecordsPage, { RecordDetail } from "../../records";
import { RecordCollectionsPage } from "./collections";
import { paths } from "../../paths";
import { Page, PageHeader, ButtonLink } from "../../kit";
import { useResources } from "../../ui";
import { ObjectGraph } from "./graph";
import { ExplorerSwitch, useExplorerView } from "./views";
import { NotFound } from "../../shell/ErrorBoundary";
import { ObjectTypeDetail } from "./object-type";

// /ontology
export function OntologyHome() {
  return (
    <Page>
      <OntologyPage />
    </Page>
  );
}
// /ontology/types/:typeId (typeId is objectType.data.typeId, stable across publications)
export function ObjectTypePage() {
  const { typeId = "" } = useParams();
  return <ObjectTypeDetail key={typeId} typeId={typeId} />;
}
// /ontology/explore: the record graph; ?view=table (&type=<name>&search=)
// lists one type's records instead.
export function ObjectExplorer() {
  return useExplorerView() === "table" ? (
    <Page>
      <RecordsPage />
    </Page>
  ) : (
    <Page width="wide">
      <GraphExplorer />
    </Page>
  );
}
function GraphExplorer() {
  const types = useResources("objectType"),
    [open, setOpen] = useState<string | null>(null);
  return (
    <>
      <PageHeader
        title="Explorer"
        subtitle="Your records and how they connect."
        actions={
          <>
            <ExplorerSwitch />
            <ButtonLink to={paths.recordCollections()}>Collections</ButtonLink>
          </>
        }
      />
      <ObjectGraph onOpen={setOpen} />
      {open && (
        <RecordDetail
          key={open}
          logicalId={open}
          types={types.data?.items || []}
          onClose={() => setOpen(null)}
        />
      )}
    </>
  );
}
// /records/:logicalId (?type=<name>): object view over the explorer
export function ObjectView() {
  return (
    <Page>
      <RecordsPage />
    </Page>
  );
}
// /ontology/*: the Ontology area's route table (main.tsx delegates the whole
// prefix). /records/:logicalId stays in main.tsx as a legacy path.
export function OntologyRoutes() {
  return (
    <Routes>
      <Route index element={<OntologyHome />} />
      <Route path="types/:typeId" element={<ObjectTypePage />} />
      <Route path="collections" element={<RecordCollectionsPage />} />
      <Route path="explore" element={<ObjectExplorer />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
