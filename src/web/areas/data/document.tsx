import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  ExternalLink,
  FileText,
  FolderInput,
  FolderOpen,
  LoaderCircle,
  Shapes,
  Table2,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import type {
  DocumentResponse,
  FoundryDocument,
  FoundryOntology,
  ListOntologiesResponse,
} from "../../../shared/wfe";
import { ApiError } from "../../api";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  Button,
  ButtonAnchor,
  ButtonLink,
  ConfirmDialog,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  Segmented,
  SkeletonText,
  StatusBadge,
  Tag,
  connectorLabel,
  formatDate,
  formatRelative,
} from "../../kit";
import { DocumentView, PdfReadCallout } from "../documents/document-view";
import {
  deleteDocument,
  documentQueries,
  documentSetQueries,
  documentsChanged,
} from "../documents/documents-api";
import { useFileMappings, type FileMappingRow } from "../documents/mappings";
import {
  andList,
  documentViewer,
  extractionBadge,
  fileRead,
  formatBytes,
  ontologyReadText,
  readKindOf,
  renderedLabel,
} from "../documents/model";
import { MoveDialog, useOntologyNames } from "../documents/sets";
import { Callout } from "../ontology/parts";
import { POLL_MS, ontologyQueries } from "../ontology/queries";
import { connectorsQuery } from "./data-api";
import { canManage } from "./model";

// /data/documents/:documentId: one document of the workspace: what it
// holds (as uploaded, from the WFE: the bucket lets a browser PUT, not GET),
// its set, size and upload, and the ontologies associated with it, each with
// whether its routes read the set, what its current load made of the
// document, and a link to that ontology's view of it (highlights, facts,
// reviews); it polls while an ontology that hasn't read it yet has a job
// running. A table file (CSV, TSV, JSON, JSONL, Parquet) has its Rows
// panel instead: how many rows Foundry read from it, or why it couldn't,
// and which ontologies' current versions map it (useFileMappings). A PDF
// shows the text Foundry read from it, with its pages (the PDF itself) a
// click away (?view=pages); while it is read, or when it couldn't be (a
// scanned PDF has no text), only its pages, under why. Owners and admins
// move it to another set or delete it; for a mapped file, both say first
// that the version's next load fails without it. Its media type picks how
// it shows (../documents/document-view.tsx): Markdown rendered, JSON as a
// tree, a table's rows, a PDF's text or pages; the text as uploaded with
// ?view=source.
//
// A connection's document (`source`, a Granola note) says which connection
// and set it is from and its time at the source, and opens at the source
// when that is an https page. Only the connection's syncs change it, so it
// has no Move or Delete; deleted, it was replaced or removed at the source.

export function DataDocumentPage({ documentId }: { documentId: string }) {
  const { id, role, notify } = useWorkspace(),
    manage = canManage(role),
    navigate = useNavigate(),
    qc = useQueryClient(),
    name = useOntologyNames();
  // While an ontology that hasn't read it has a job live, both poll: each
  // reads the other's answer from the cache.
  const docQuery = documentQueries.workspaceDocument(id, documentId),
    listQuery = ontologyQueries.list(id);
  const reading = (
    document?: FoundryDocument,
    ontologies?: FoundryOntology[],
  ) =>
    (document?.ontologies || []).some(
      (o) =>
        !o.extraction &&
        !!ontologies?.find((x) => x.id === o.ontologyId)?.activeJob,
    );
  const poll = () =>
    reading(
      qc.getQueryData<DocumentResponse>(docQuery.queryKey)?.document,
      qc.getQueryData<ListOntologiesResponse>(listQuery.queryKey)?.ontologies,
    )
      ? POLL_MS
      : false;
  // Its own read too, while Foundry reads its rows.
  const doc = useQuery({
      ...docQuery,
      refetchInterval: (q) => poll() || docQuery.refetchInterval(q),
    }),
    sets = useQuery(documentSetQueries.list(id)),
    connectors = useQuery(connectorsQuery(id)),
    list = useQuery({ ...listQuery, refetchInterval: poll });
  const d = doc.data?.document;
  // The job that ended read it: read the document once more.
  const busy = reading(d, list.data?.ontologies),
    wasBusy = useRef(false);
  useEffect(() => {
    if (busy) wasBusy.current = true;
    else if (wasBusy.current) {
      wasBusy.current = false;
      void qc.invalidateQueries({ queryKey: docQuery.queryKey });
    }
  }, [busy]);
  const [params, setParams] = useSearchParams(),
    view = params.get("view") || "";
  const [moving, setMoving] = useState(false),
    [deleting, setDeleting] = useState(false),
    [pending, setPending] = useState(false);
  // How each ontology's current version reads it, when it is a table file.
  const mappings = useFileMappings(d ?? { id: documentId }),
    mappedBy = mappings.filter((m) => m.mapping?.kind === "mapped");
  const crumbs = [{ label: "Connectors", to: paths.data() }];
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
              ? "This document doesn’t exist in this workspace"
              : "Couldn’t load this document"
          }
          text={doc.error?.message}
          action={<ButtonLink to={paths.data()}>Back to Connectors</ButtonLink>}
        />
      </Page>
    );
  const set = sets.data?.find((s) => s.id === d.setId),
    setName = set?.name || "its set";
  // A connection's document: its connection (once the lists say), and its
  // page at the source, an https URL only.
  const source = d.source,
    connectorId = set?.connector?.connectorId || "",
    connection = connectors.data?.connectors?.find((c) => c.id === connectorId),
    connectionName =
      connection?.name ||
      set?.connector?.connectorName ||
      (connection ? connectorLabel(connection.kind) : "its connection"),
    sourceUrl = /^https:\/\//i.test(source?.url || "") ? source!.url : "";
  const ontologies = d.ontologies || [],
    viewer = documentViewer(d.mediaType),
    rendered = renderedLabel(viewer),
    table = readKindOf(d) === "table",
    pdf = viewer.kind === "pdf";
  // A PDF's text, once read, and its pages; any other's view and its source.
  const textRead = pdf && fileRead(d).state === "ready",
    pages = pdf && (!textRead || view === "pages"),
    toggle = textRead
      ? {
          value: pages ? "pages" : "text",
          options: [
            { value: "text", label: "Text" },
            { value: "pages", label: "Pages" },
          ],
        }
      : rendered && {
          value: view === "source" ? "source" : "rendered",
          options: [
            { value: "rendered", label: rendered },
            { value: "source", label: "Source" },
          ],
        };
  const remove = async () => {
    setPending(true);
    try {
      await deleteDocument(id, d.id);
      await documentsChanged(qc, id);
      notify(`Deleted ${d.name}.`);
      navigate(paths.data());
    } catch (e) {
      notify((e as Error).message, true);
      setDeleting(false);
    } finally {
      setPending(false);
    }
  };
  return (
    <Page width="wide">
      <PageHeader
        breadcrumbs={[
          ...crumbs,
          ...(connectorId
            ? [{ label: connectionName, to: paths.connection(connectorId) }]
            : []),
          ...(set ? [{ label: set.name || set.id }] : []),
          { label: d.name || d.id },
        ]}
        icon={
          <span className="dc-table-icon">
            <FileText size={18} aria-hidden />
          </span>
        }
        eyebrow="Document"
        title={d.name || d.id}
        meta={
          source ? (
            <>
              {connectorId && (
                <span>
                  From{" "}
                  <Link to={paths.connection(connectorId)}>
                    {connectionName}
                  </Link>{" "}
                  · {set?.name || d.setId}
                </span>
              )}
              {source.at && <span>{formatDate(source.at)}</span>}
              <span>{formatBytes(d.sizeBytes)}</span>
            </>
          ) : (
            <>
              {set && <Tag icon={FolderOpen}>{set.name}</Tag>}
              <span>{formatBytes(d.sizeBytes)}</span>
              <span title={formatDate(d.createdAt || "")}>
                Uploaded {formatRelative(d.createdAt || "")}
              </span>
            </>
          )
        }
        actions={
          source
            ? sourceUrl &&
              d.status !== "deleted" && (
                <ButtonAnchor
                  icon={ExternalLink}
                  href={sourceUrl}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {connection
                    ? `Open in ${connectorLabel(connection.kind)}`
                    : "Open at the source"}
                </ButtonAnchor>
              )
            : manage &&
              d.status !== "deleted" && (
                <>
                  <Button icon={FolderInput} onClick={() => setMoving(true)}>
                    Move
                  </Button>
                  <Button icon={Trash2} onClick={() => setDeleting(true)}>
                    Delete
                  </Button>
                </>
              )
        }
      />
      <div className="on-stack">
        {d.status === "deleted" ? (
          source ? (
            <Callout
              icon={Trash2}
              tone="muted"
              title="Replaced or removed at the source"
              text={`${connectorId ? `A sync of ${connectionName} found new text for it, or no longer found it.` : "Its connection was deleted, or a sync of it found new text for it or no longer found it."} What loads read from it stays in their ontologies’ databases until a load without it is current.`}
            />
          ) : (
            <Callout
              icon={Trash2}
              tone="muted"
              title="This document was deleted"
              text="What loads read from it stays in their ontologies’ databases."
            />
          )
        ) : (
          d.status === "pending_upload" && (
            <Callout
              icon={FileText}
              tone="muted"
              title="Its upload didn’t finish"
              text="Upload the file again from Connectors."
            />
          )
        )}
        <div className="dc-doc-grid">
          <Panel
            title={table ? "Preview" : pages ? "Pages" : "Text"}
            actions={
              toggle &&
              d.status === "uploaded" && (
                <Segmented
                  label="View"
                  value={toggle.value}
                  onChange={(value) =>
                    setParams(
                      (p) => {
                        const next = new URLSearchParams(p);
                        // The first view is the page's own.
                        if (value === toggle.options[0]!.value)
                          next.delete("view");
                        else next.set("view", value);
                        return next;
                      },
                      { replace: true },
                    )
                  }
                  options={toggle.options}
                />
              )
            }
          >
            {d.status !== "uploaded" ? (
              <EmptyState
                size="sm"
                icon={FileText}
                title="Nothing to show"
                text="Only an uploaded document shows what it holds."
              />
            ) : (
              <div className="on-stack">
                <PdfReadCallout document={d} />
                <DocumentView
                  workspaceId={id}
                  document={d}
                  source={view === "source"}
                  pages={pages}
                />
              </div>
            )}
          </Panel>
          <div className="on-stack">
            {table ? (
              <RowsPanel document={d} setName={setName} mappings={mappings} />
            ) : (
              <Panel
                title="Ontologies"
                subtitle={`Those that read ${setName}, or whose current load holds it, and what each made of it.`}
              >
                {!ontologies.length ? (
                  <EmptyState
                    size="sm"
                    icon={Shapes}
                    title={`No ontology reads ${setName}`}
                    text="A generation that picks the set reads its documents."
                    action={
                      <ButtonLink size="sm" to={paths.ontologies()}>
                        Go to Ontologies
                      </ButtonLink>
                    }
                  />
                ) : (
                  <ul className="dc-doc-list" aria-label="Ontologies">
                    {ontologies.map((o) => {
                      const ontology = name(o.ontologyId),
                        badge = o.extraction
                          ? extractionBadge(o.extraction)
                          : {
                              status: "queued" as const,
                              label: "Not read yet",
                            };
                      return (
                        <li key={o.ontologyId} aria-label={ontology}>
                          <p className="dc-fact">
                            <Link to={paths.ontology(o.ontologyId || "")}>
                              {ontology}
                            </Link>
                            <StatusBadge
                              status={badge.status}
                              label={badge.label}
                              size="sm"
                            />
                          </p>
                          <small>
                            {ontologyReadText(
                              setName,
                              // Until the sets load, the document's own word.
                              set
                                ? set.readBy?.find(
                                    (r) => r.ontologyId === o.ontologyId,
                                  )
                                : {
                                    ontologyId: o.ontologyId,
                                    routed: o.routed,
                                  },
                            )}
                          </small>
                          <Link
                            className="dc-doc-open"
                            to={paths.document(o.ontologyId || "", d.id)}
                          >
                            Open in {ontology}
                            <ArrowRight size={12} aria-hidden />
                          </Link>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </Panel>
            )}
            <Panel title="Details">
              <KeyValue
                items={[
                  ...(source
                    ? [
                        {
                          label: "Connection",
                          value: connectorId && connectionName,
                        },
                        { label: "Set", value: set?.name },
                        {
                          label: "At the source",
                          value: source.at && formatDate(source.at),
                        },
                        {
                          label: "Source ID",
                          value: source.externalId,
                          mono: true,
                        },
                        {
                          label: "Synced",
                          value: d.createdAt && formatDate(d.createdAt),
                        },
                      ]
                    : [
                        { label: "Set", value: set?.name },
                        {
                          label: "Uploaded",
                          value: d.createdAt && formatDate(d.createdAt),
                        },
                      ]),
                  { label: "Media type", value: d.mediaType, mono: true },
                  { label: "Size", value: formatBytes(d.sizeBytes) },
                  { label: "SHA-256", value: d.sha256, mono: true },
                  { label: "Document ID", value: d.id, mono: true },
                ]}
              />
            </Panel>
          </div>
        </div>
      </div>
      {moving && <MoveDialog document={d} onClose={() => setMoving(false)} />}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${d.name}?`}
          description={`It’s removed from Foundry now. What loads read from it stays in their ontologies’ databases.${mappedBy.length ? ` ${mapsText(mappedBy, name)}: the next load of ${mappedBy.length === 1 ? "it" : "each"} fails until the version is revised to stop mapping it, or the file is uploaded again.` : ""}`}
          confirmLabel="Delete"
          tone="danger"
          pending={pending}
          onConfirm={remove}
          onCancel={() => setDeleting(false)}
        />
      )}
    </Page>
  );
}

/**
 * "Renewals’s version 3 maps it", "Renewals’s version 3 and Billing’s
 * version 1 map it": the ontologies whose current versions map a file.
 */
const mapsText = (
  rows: readonly FileMappingRow[],
  name: (ontologyId: string) => string,
) =>
  `${andList(rows.map((r) => `${name(r.ontology.id)}’s version ${r.version?.version ?? ""}`))} ${rows.length === 1 ? "maps" : "map"} it`;

// A table file's rows as ontologies read them: how many Foundry read, or
// that it is being read or couldn't be (why, and what to fix), and, for
// each ontology whose current version reads its set, whether that version
// maps it: mapped, or not mapped with the way to map it (a revision, when
// the version's generation drafted from the file, else a generation). Text
// in its cells is never read for entities.
function RowsPanel({
  document: d,
  setName,
  mappings,
}: {
  document: FoundryDocument;
  setName: string;
  mappings: FileMappingRow[];
}) {
  const name = useOntologyNames(),
    read = fileRead(d);
  const shown = mappings.filter((m) => m.mapping?.kind !== "none"),
    loading = mappings.some((m) => !m.mapping);
  return (
    <Panel
      title="Rows"
      subtitle="Ontologies load a table file as a table of its own, through a version’s mappings. Text in its cells isn’t read for entities."
    >
      <div className="on-stack">
        {read.state === "failed" ? (
          <Callout
            icon={TriangleAlert}
            tone="danger"
            title="It couldn’t be read"
            text={read.message}
          />
        ) : read.state === "reading" ? (
          <Callout
            icon={LoaderCircle}
            live
            title={
              read.retrying ? "Waiting to be read again" : "Reading its rows"
            }
            text={
              read.retrying
                ? "Reading it failed for a reason that isn’t the file’s. Foundry tries again on its own."
                : "A generation started now leaves it out. It shows here once its rows are read."
            }
          />
        ) : (
          read.state === "ready" && (
            <p>
              <Table2 size={14} aria-hidden />{" "}
              {read.rows === undefined
                ? "Its rows were read."
                : `${read.rows.toLocaleString("en-US")} ${read.rows === 1 ? "row" : "rows"} read.`}
            </p>
          )
        )}
        {loading ? (
          <SkeletonText lines={2} />
        ) : !shown.length ? (
          <p className="on-muted">
            No ontology’s current version reads {setName}. A generation that
            picks the set maps its files.{" "}
            <Link to={paths.ontologies()}>Go to Ontologies</Link>
          </p>
        ) : (
          <ul className="dc-doc-list" aria-label="Ontologies">
            {shown.map(({ ontology: o, version: v, mapping }) => {
              const label = name(o.id);
              return (
                <li key={o.id} aria-label={label}>
                  <p className="dc-fact">
                    <Link to={paths.ontology(o.id)}>{label}</Link>
                    <StatusBadge
                      status={
                        mapping?.kind === "mapped" ? "succeeded" : "draft"
                      }
                      label={
                        mapping?.kind === "mapped" ? "Mapped" : "Not mapped"
                      }
                      size="sm"
                    />
                  </p>
                  <small>
                    {mapping?.kind === "mapped" ? (
                      <>
                        Mapped by{" "}
                        <Link to={paths.version(o.id, v!.id)}>
                          version {v!.version}
                        </Link>
                        : its loads load these rows.
                      </>
                    ) : mapping?.kind === "unmapped" && mapping.revise ? (
                      <>
                        Not mapped by version {v!.version}, which read the file
                        when it was drafted.{" "}
                        <Link to={paths.version(o.id, v!.id, "edit")}>
                          Revise
                        </Link>
                      </>
                    ) : (
                      <>
                        Not mapped by version {v!.version}: it was drafted
                        before the file was read.{" "}
                        <Link to={paths.generate(o.id)}>Generate</Link>
                      </>
                    )}
                  </small>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </Panel>
  );
}
