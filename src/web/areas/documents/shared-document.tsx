import { useQuery } from "@tanstack/react-query";
import { FileText } from "lucide-react";
import { ApiError } from "../../api";
import {
  ButtonLink,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  formatDate,
  formatRelative,
} from "../../kit";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import { PartNote } from "../explore/part-note";
import { ontologyName, useOntology } from "../ontology/scope";
import { DocumentView, PdfReadCallout } from "./document-view";
import { documentQueries } from "./documents-api";
import { formatBytes } from "./model";

// /ontology/:ontologyId/documents/:documentId for someone reading through
// source grants (../explore/graph-reader.ts): a document the shared load
// holds, as uploaded, its text (GET .../documents/{id}:content) rendered
// by its media type (./document-view.tsx), and nothing that needs its
// bytes. What the load found in it (its names, quotes and reviews, marked
// on the text by ./document.tsx) is read from the database, so it isn't
// shown; a link to one of its spans (a citation's quote) opens it at the
// top.

export function SharedDocumentPage({ documentId }: { documentId: string }) {
  const { id: ws } = useWorkspace(),
    { id: o, ontology } = useOntology();
  const doc = useQuery(documentQueries.document(ws, o, documentId));
  const d = doc.data?.document;
  const crumbs = [
    { label: "Ontologies", to: paths.ontologies() },
    { label: ontologyName(ontology), to: paths.ontology(o) },
  ];
  if (doc.isPending)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Document" />
        <SkeletonText lines={6} />
      </Page>
    );
  if (!d)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Document unavailable" />
        <EmptyState
          icon={FileText}
          title={
            doc.error instanceof ApiError && doc.error.status === 404
              ? "This document isn’t shared with you"
              : "Couldn’t load this document"
          }
          text={
            doc.error instanceof ApiError && doc.error.status === 404
              ? "The load shared with you doesn’t hold it."
              : doc.error?.message
          }
          action={<ButtonLink to={paths.ask(o)}>Ask</ButtonLink>}
        />
      </Page>
    );
  return (
    <Page width="wide">
      <PageHeader
        breadcrumbs={[...crumbs, { label: d.name || d.id }]}
        icon={
          <span className="dc-table-icon">
            <FileText size={18} aria-hidden />
          </span>
        }
        eyebrow="Document"
        title={d.name || d.id}
        meta={
          <>
            <span>{formatBytes(d.sizeBytes)}</span>
            <span title={formatDate(d.createdAt)}>
              Uploaded {formatRelative(d.createdAt || "")}
            </span>
          </>
        }
      />
      <div className="on-stack">
        <PartNote />
        <PdfReadCallout document={d} />
        <Panel
          title="Text"
          subtitle="As uploaded. What the load found in it isn’t shared with you."
        >
          {d.status === "uploaded" ? (
            <DocumentView
              workspaceId={ws}
              document={d}
              source={false}
              textOnly
            />
          ) : (
            <EmptyState
              size="sm"
              icon={FileText}
              title="Nothing to show"
              text={
                d.status === "deleted"
                  ? "It was deleted since the load read it."
                  : "Only an uploaded document shows what it holds."
              }
            />
          )}
        </Panel>
      </div>
    </Page>
  );
}
