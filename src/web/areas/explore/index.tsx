// Explore area: an ontology's database, lazy-loaded by the Ontology area's
// Explorer route table: the graph explorer (/explore/:ontologyId) and an
// entity's page (/explore/:ontologyId/entities/:entityId).
import "./explore.css";
import { useParams } from "react-router-dom";
import { EntityPage } from "./entity";
import { ExplorerPage } from "./explorer-page";

export { ExplorerPage };

export function OntologyEntityPage() {
  const { entityId = "" } = useParams();
  return <EntityPage key={entityId} entityId={entityId} />;
}
