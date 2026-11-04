// Documents area: an ontology's documents, lazy-loaded by the Ontology
// area's route table under /ontology/:ontologyId/documents.
import "./documents.css";
import { useParams } from "react-router-dom";
import { Page, PageHeader } from "../../kit";
import { useReadAccess } from "../explore/graph-reader";
import { NotShared } from "../ontology/load-selection";
import { DocumentPage } from "./document";
import { DocumentsPage } from "./documents";
import { SharedDocumentPage } from "./shared-document";

export { DocumentsPage };

// /ontology/:ontologyId/documents/:documentId: the document as the
// ontology read it, or, read through source grants, as uploaded.
export function OntologyDocumentPage() {
  const { documentId = "" } = useParams();
  const access = useReadAccess();
  if (access === "none")
    return (
      <Page>
        <PageHeader title="Document" />
        <NotShared />
      </Page>
    );
  return access === "grants" ? (
    <SharedDocumentPage key={documentId} documentId={documentId} />
  ) : (
    <DocumentPage key={documentId} documentId={documentId} />
  );
}
