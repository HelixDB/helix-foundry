// Data Connection area: route components lazy-loaded by main.tsx.
import "./data.css";
import {
  Navigate,
  Route,
  Routes,
  useLocation,
  useParams,
} from "react-router-dom";
import DataPage from "../../data";
import { Page } from "../../kit";
import { NotFound } from "../../shell/ErrorBoundary";
import { paths } from "../../paths";
import { ConnectionDetail } from "./connection";
import { DataOverview } from "./connections";
import { QueryWorkspace } from "./queries";

// /data (?add=1 opens the connector picker): connections and their tables
export function DataHome() {
  return <DataOverview />;
}
// /data/connections: the connections list is the Data page now.
export function ConnectionsPage() {
  const { search } = useLocation();
  return <Navigate to={paths.data() + search} replace />;
}
// /data/connections/:sourceId
export function ConnectionPage() {
  const { sourceId = "" } = useParams();
  return <ConnectionDetail key={sourceId} sourceId={sourceId} />;
}
// /data/datasets/:datasetId
export function DatasetPage() {
  return (
    <Page>
      <DataPage />
    </Page>
  );
}
// /data/*: the Data area's route table (main.tsx delegates the whole prefix).
// Old /data/:datasetId links redirect.
export function DataRoutes() {
  return (
    <Routes>
      <Route index element={<DataHome />} />
      <Route path="connections" element={<ConnectionsPage />} />
      <Route path="connections/:sourceId" element={<ConnectionPage />} />
      <Route path="datasets/:datasetId" element={<DatasetPage />} />
      <Route path="queries" element={<QueryWorkspace />} />
      <Route path="queries/:queryId" element={<QueryWorkspace />} />
      <Route path=":datasetId" element={<LegacyDataset />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  );
}
function LegacyDataset() {
  const { datasetId = "" } = useParams();
  return <Navigate to={paths.dataset(datasetId)} replace />;
}
