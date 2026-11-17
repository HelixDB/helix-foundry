// Documents area: a document's page, lazy-loaded by the Ontology area's
// route table under /ontology/:ontologyId/documents. An ontology's list of
// them is its Connections page's (./documents.tsx).
import "./documents.css";
import { useParams } from "react-router-dom";
import { Page, PageHeader } from "../../kit";
import { useReadAccess } from "../explore/graph-reader";
import { NotShared } from "../ontology/load-selection";
import { DocumentPage } from "./document";
import { SharedDocumentPage } from "./shared-document";

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
