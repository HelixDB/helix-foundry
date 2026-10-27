import { useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { FileText, FolderOpen, LoaderCircle } from "lucide-react";
import type {
  FoundryDocument,
  OntologyDocumentStatus,
} from "../../../shared/wfe";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  FilterBar,
  FilterSelect,
  Page,
  PageHeader,
  SourceLogo,
  StatusBadge,
  formatDate,
  formatRelative,
  type Status,
} from "../../kit";
import { Callout } from "../ontology/parts";
import { POLL_MS, ontologyQueries } from "../ontology/queries";
import { ontologyName, useOntology } from "../ontology/scope";
import { documentQueries, documentSetQueries } from "./documents-api";
import { documentBadge, formatBytes, liveRead } from "./model";

// /ontology/:ontologyId/documents (?status= filters it): what the ontology
// reads of the workspace's documents, read only: its current load's
// documents and the uploaded ones in the sets its reading version reads,
// each with what the current load made of it. Documents are uploaded,
// moved and deleted in Data. A load reads them into the ontology's database
// by their set's routes; until the first one they wait.

const filters: { value: "" | OntologyDocumentStatus; label: string }[] = [
  { value: "", label: "All" },
  { value: "waiting", label: "Waiting" },
  { value: "extracted", label: "Read" },
  { value: "review", label: "Needs review" },
  { value: "duplicate", label: "Duplicate" },
  { value: "failed", label: "Couldn’t be read" },
  { value: "out_of_scope", label: "Not read by its version" },
];

export function DocumentsPage() {
  const { id } = useWorkspace(),
    { id: o, ontology } = useOntology(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  // The status filter lives in the URL, so a link can open it (a load's
  // "Review documents").
  const status =
    filters.find((f) => f.value && f.value === params.get("status"))?.value ||
    undefined;
  const setStatus = (value: string) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (value) next.set("status", value);
        else next.delete("status");
        return next;
      },
      { replace: true },
    );
  const loads = useQuery(ontologyQueries.loads(id, o));
  const running = liveRead(loads.data?.loads),
    live = running?.job,
    reading = !!live;
  const loaded = !!ontology.currentLoad;
  const documents = useQuery({
    ...documentQueries.list(id, o, status),
    refetchInterval: reading ? POLL_MS : false,
  });
  const sets = useQuery(documentSetQueries.list(id));
  const setList = sets.data || [];
  const setName = (setId?: string) =>
    setList.find((x) => x.id === setId)?.name || "";
  // A load that just ended changed what the documents say.
  const wasReading = useRef(false);
  useEffect(() => {
    if (reading) wasReading.current = true;
    else if (wasReading.current) {
      wasReading.current = false;
      void qc.invalidateQueries({ queryKey: [id, "ontology", o] });
    }
  }, [reading]);
  const [search, setSearch] = useState(""),
    [inSet, setInSet] = useState("");
  // Drop a filter whose set was deleted, including by another tab.
  if (inSet && sets.isSuccess && !setList.some((x) => x.id === inSet)) {
    setInSet("");
  }
  const list = documents.data?.documents || [];
  const rows = list
    .map((d) => ({ ...d, badge: documentBadge(d, live) }))
    .filter((d) =>
      (d.name || "").toLowerCase().includes(search.trim().toLowerCase()),
    )
    .filter((d) => !inSet || d.setId === inSet);
  const openInData = (
    <ButtonLink icon={FolderOpen} to={paths.data()}>
      Open in Connectors
    </ButtonLink>
  );
  return (
    <Page>
      <PageHeader
        breadcrumbs={[
          { label: "Ontologies", to: paths.ontologies() },
          { label: ontologyName(ontology), to: paths.ontology(o) },
        ]}
        title="Documents"
        subtitle="What this ontology reads of your documents: those in the sets its version reads, and those its current load holds. A load reads them for the customers, companies and facts they name. Upload and organise them in Connectors."
        actions={openInData}
      />
      <div className="on-stack">
        {reading ? (
          <Callout
            icon={LoaderCircle}
            live
            title={
              live!.phase === "documents"
                ? "Reading documents"
                : "A load is running"
            }
            text="New documents are read before it finishes."
            action={
              <ButtonLink size="sm" to={paths.load(o, running!.load.id)}>
                View progress
              </ButtonLink>
            }
          />
        ) : (
          loads.isSuccess &&
          !loaded && (
            <Callout
              icon={FileText}
              tone="muted"
              title="Documents are read when you load the ontology"
              text="Until then they wait. A load writes the current version’s tables into the database, then reads the documents of its sets; after that, new documents are read as they arrive."
              action={
                <ButtonLink size="sm" to={paths.ontology(o)}>
                  Go to the overview
                </ButtonLink>
              }
            />
          )
        )}
        {documents.data && <RefreshFailed query={documents} />}
        {!documents.data && documents.error ? (
          <EmptyState
            icon={FileText}
            title="Couldn’t load the documents"
            text={documents.error.message}
            action={
              <Button onClick={() => void documents.refetch()}>Retry</Button>
            }
          />
        ) : !documents.isPending && !list.length && !status ? (
          <EmptyState
            icon={FileText}
            title="No documents yet"
            text="This ontology reads the documents of the sets its version picked. Upload them in Connectors, and pick their sets when you generate a version."
          />
        ) : (
          <>
            <FilterBar
              search={{
                value: search,
                onChange: setSearch,
                placeholder: "Search documents",
              }}
              count={
                documents.data ? `${rows.length} of ${list.length}` : undefined
              }
            >
              <FilterSelect
                label="Status"
                value={status || ""}
                onChange={setStatus}
                options={filters}
              />
              {setList.length > 1 && (
                <FilterSelect
                  label="Set"
                  value={inSet}
                  onChange={setInSet}
                  options={[
                    { value: "", label: "All sets" },
                    ...setList.map((x) => ({
                      value: x.id,
                      label: x.name || x.id,
                    })),
                  ]}
                />
              )}
            </FilterBar>
            <DataGrid<
              FoundryDocument & { badge: { status: Status; label: string } }
            >
              label="Documents"
              loading={documents.isPending}
              rows={rows}
              getRowKey={(d) => d.id}
              rowHeight={44}
              onRowClick={(d) => navigate(paths.document(o, d.id))}
              emptyText="No documents match."
              columns={[
                {
                  key: "name",
                  label: "Name",
                  width: 175,
                  render: (d) => (
                    <span className="dc-doc-name">
                      <SourceLogo source="upload" size={18} />
                      <Link
                        to={paths.document(o, d.id)}
                        className="on-row-link on-cell-text"
                        onClick={(e) => e.stopPropagation()}
                      >
                        {d.name}
                      </Link>
                    </span>
                  ),
                },
                {
                  key: "set",
                  label: "Set",
                  width: 140,
                  sortValue: (d) => setName(d.setId),
                  render: (d) => (
                    <span className="on-cell-text">
                      {setName(d.setId) || "—"}
                    </span>
                  ),
                },
                {
                  key: "status",
                  label: "Status",
                  width: 175,
                  sortValue: (d) => d.badge.label,
                  render: (d) => (
                    <StatusBadge
                      status={d.badge.status}
                      label={d.badge.label}
                      size="sm"
                    />
                  ),
                },
                {
                  key: "entities",
                  label: "Entities",
                  type: "integer",
                  width: 100,
                  sortValue: (d) => d.extraction?.entities ?? -1,
                  render: (d) =>
                    d.extraction ? d.extraction.entities || 0 : "—",
                },
                {
                  key: "facts",
                  label: "Facts",
                  type: "integer",
                  width: 90,
                  sortValue: (d) => d.extraction?.facts ?? -1,
                  render: (d) => (d.extraction ? d.extraction.facts || 0 : "—"),
                },
                {
                  key: "sizeBytes",
                  label: "Size",
                  type: "integer",
                  width: 100,
                  sortValue: (d) => Number(d.sizeBytes || 0),
                  render: (d) => formatBytes(d.sizeBytes),
                },
                {
                  key: "createdAt",
                  label: "Uploaded",
                  type: "timestamp",
                  width: 150,
                  render: (d) => (
                    <span title={formatDate(d.createdAt)}>
                      {formatRelative(d.createdAt || "")}
                    </span>
                  ),
                },
              ]}
            />
          </>
        )}
      </div>
    </Page>
  );
}
