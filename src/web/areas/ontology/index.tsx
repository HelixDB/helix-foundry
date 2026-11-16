// Ontology area: route components lazy-loaded by main.tsx.
import "./ontology.css";
import { lazy } from "react";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useParams,
} from "react-router-dom";
import { paths } from "../../paths";
import { NotFound } from "../../shell/ErrorBoundary";
import { AskPage } from "./ask";
import { GeneratePage } from "./generate";
import { GenerationPage } from "./generations";
import { OntologyHome } from "./home";
import { LoadRuleBreaksPage } from "./load-rule-breaks";
import { LoadPage } from "./loads";
import { NewOntologyPage } from "./new-ontology";
import { OpenOntology } from "./ontologies";
import { SampleTour } from "./sample-tour";
import { OntologyScope } from "./scope";
import { VersionPage } from "./version";

export { OntologyHome };

// An ontology's connections and documents, its Review page, its workflows,
// its database's explorer and its records are loaded when first opened (the
// explorer's canvas is large). The Explorer and Ask have route tables of
// their own (ExploreRoutes, AskRoutes), still under the ontology's scope.
const ConnectionsPage = lazy(() =>
    import("./connections").then((m) => ({ default: m.ConnectionsPage })),
  ),
  DocumentPage = lazy(() =>
    import("../documents").then((m) => ({ default: m.OntologyDocumentPage })),
  ),
  ExplorerPage = lazy(() =>
    import("../explore").then((m) => ({ default: m.ExplorerPage })),
  ),
  EntityPage = lazy(() =>
    import("../explore").then((m) => ({ default: m.OntologyEntityPage })),
  ),
  RecordsPage = lazy(() =>
    import("../records").then((m) => ({ default: m.RecordsPage })),
  ),
  ReviewPage = lazy(() =>
    import("../reviews/page").then((m) => ({ default: m.ReviewPage })),
  ),
  WorkflowsPage = lazy(() =>
    import("../workflows").then((m) => ({ default: m.WorkflowsPage })),
  ),
  WorkflowPage = lazy(() =>
    import("../workflows").then((m) => ({ default: m.WorkflowPage })),
  ),
  WorkflowRunPage = lazy(() =>
    import("../workflows").then((m) => ({ default: m.WorkflowRunPage })),
  );

// /ontology/:ontologyId/versions/:versionId
export function OntologyVersionPage() {
  const { versionId = "" } = useParams();
  return <VersionPage key={versionId} versionId={versionId} />;
}
// /ontology/:ontologyId/generations/:generationId
export function OntologyGenerationPage() {
  const { generationId = "" } = useParams();
  return <GenerationPage key={generationId} generationId={generationId} />;
}
// /ontology/:ontologyId/loads/:loadId
export function OntologyLoadPage() {
  const { loadId = "" } = useParams();
  return <LoadPage key={loadId} loadId={loadId} />;
}

// /ontology/:ontologyId/documents, the list before it was Connections:
// old links and emails keep their query (?status=).
function ToConnections() {
  const { ontologyId = "" } = useParams(),
    { search } = useLocation();
  return (
    <Navigate to={paths.ontologyConnections(ontologyId) + search} replace />
  );
}

// /ontology/:ontologyId/suggestions, before it was the Review page's
// Ontology tab: old links keep their query.
function ToReviewOntology() {
  const { ontologyId = "" } = useParams(),
    { search } = useLocation();
  const query = new URLSearchParams(search);
  query.set("tab", "ontology");
  return <Navigate to={`${paths.review(ontologyId)}?${query}`} replace />;
}

// /ontology/:ontologyId/loads/:loadId/rule-breaks
export function OntologyLoadRuleBreaksPage() {
  const { loadId = "" } = useParams();
  return <LoadRuleBreaksPage key={loadId} loadId={loadId} />;
}

// /ontology/*: the Ontology area's route table (main.tsx delegates the whole
// prefix). Every page of an ontology runs inside its scope; the ontologies
// are the section's panel, and /ontology opens one's overview.
export function OntologyRoutes() {
  return (
    <Routes>
      <Route
        index
        element={<OpenOntology title="Ontologies" to={paths.ontology} />}
      />
      <Route path="new" element={<NewOntologyPage />} />
      <Route path="sample" element={<SampleTour />} />
      <Route path=":ontologyId" element={<OntologyScope />}>
        <Route index element={<OntologyHome />} />
        <Route path="versions/:versionId" element={<OntologyVersionPage />} />
        <Route path="generate" element={<GeneratePage />} />
        <Route
          path="generations/:generationId"
          element={<OntologyGenerationPage />}
        />
        <Route path="loads/:loadId" element={<OntologyLoadPage />} />
        <Route
          path="loads/:loadId/rule-breaks"
          element={<OntologyLoadRuleBreaksPage />}
        />
        <Route path="connections" element={<ConnectionsPage />} />
        <Route path="documents" element={<ToConnections />} />
        <Route path="documents/:documentId" element={<DocumentPage />} />
        <Route path="suggestions" element={<ToReviewOntology />} />
        <Route path="review" element={<ReviewPage />} />
        <Route path="workflows" element={<WorkflowsPage />} />
        <Route path="workflows/:workflowId" element={<WorkflowPage />} />
        <Route
          path="workflows/:workflowId/runs/:runId"
          element={<WorkflowRunPage />}
        />
        <Route path="*" element={<NotFound />} />
      </Route>
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}

// /explore/*: the Explorer's route table, opened from an ontology's row in
// the Ontologies panel: its database as a graph, its records and an
// entity's page, inside the ontology's scope. /explore opens an ontology's
// graph.
export function ExploreRoutes() {
  return (
    <Routes>
      <Route
        index
        element={<OpenOntology title="Explorer" to={paths.explore} reads />}
      />
      <Route path=":ontologyId" element={<OntologyScope explores />}>
        <Route index element={<ExplorerPage />} />
        <Route path="records" element={<RecordsPage />} />
        <Route path="entities/:entityId" element={<EntityPage />} />
        <Route path="*" element={<NotFound />} />
      </Route>
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}

// /ask/*: Ask's route table, a section of its own: questions about an
// ontology's load, inside its scope. /ask opens an ontology's.
export function AskRoutes() {
  return (
    <Routes>
      <Route
        index
        element={<OpenOntology title="Analyst" to={paths.ask} reads />}
      />
      <Route path=":ontologyId" element={<OntologyScope explores />}>
        <Route index element={<AskPage />} />
        <Route path="*" element={<NotFound />} />
      </Route>
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
