// Documents area: an ontology's documents, lazy-loaded by the Ontology
// area's route table under /ontology/:ontologyId/documents.
import "./documents.css";
import { useParams } from "react-router-dom";
import { DocumentPage } from "./document";
import { DocumentsPage } from "./documents";

export { DocumentsPage };

// /ontology/:ontologyId/documents/:documentId
export function OntologyDocumentPage() {
  const { documentId = "" } = useParams();
  return <DocumentPage key={documentId} documentId={documentId} />;
}
