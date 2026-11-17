import "../data/data.css";
import "../documents/documents.css";
import { useMemo } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Cable, Folder, FolderOpen, Plus, Sparkles } from "lucide-react";
import {
  Button,
  ButtonLink,
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Section,
  SkeletonText,
  SourceLogo,
  StatusBadge,
  Tag,
  connectorLabel,
  plural,
} from "../../kit";
import { paths } from "../../paths";
import { canManage, useWorkspace } from "../../ui";
import { connectorBadge } from "../data/model";
import { documentSetQueries } from "../documents/documents-api";
import { OntologyDocuments } from "../documents/documents";
import { setLabel } from "../documents/model";
import {
  ontologyConnections,
  versionTables,
  type OntologyConnection,
} from "./model";
import { ontologyQueries } from "./queries";
import { ontologyName, useOntology } from "./scope";
import { useSources } from "./sources";

// /ontology/:ontologyId/connections (?status= filters its documents): what
// the ontology reads, read only. First the connections its current version
// maps (the connectors of its synced tables, and those whose own sets it
// reads), each opening its page in Connectors; then the document sets it
// reads (their `readBy` names it), each read by a route or only stored;
// then their documents (../documents/documents.tsx). Data is connected,
// uploaded and arranged in Connectors. Owners and admins get the action an
// empty section needs (Generate, Add data); members the words only.

export function ConnectionsPage() {
  const { id, role } = useWorkspace(),
    manage = canManage(role),
    { id: o, ontology } = useOntology();
  const versionId = ontology.currentVersionId || "";
  const version = useQuery({
    ...ontologyQueries.version(id, o, versionId),
    enabled: !!versionId,
  });
  const v = version.data?.version;
  const generation = useQuery({
    ...ontologyQueries.generation(id, o, v?.generationId || ""),
    enabled: !!v?.generationId,
  });
  const sources = useSources(generation.data?.generation?.tables);
  const sets = useQuery(documentSetQueries.list(id));
  const reads = (sets.data || []).filter((s) =>
    s.readBy?.some((r) => r.ontologyId === o),
  );
  const tables = useMemo(() => (v ? versionTables(v) : []), [v]);
  const { connections, unknown } = ontologyConnections(
    sources.connectors,
    tables,
    (ref) => {
      const info = sources.resolve(ref);
      if (!info) return undefined;
      return info.source === "connector"
        ? { connectorId: info.connectorId, table: info.table }
        : null;
    },
    reads,
  );
  return (
    <Page>
      <PageHeader
        breadcrumbs={[
          { label: "Ontologies", to: paths.ontologies() },
          { label: ontologyName(ontology), to: paths.ontology(o) },
        ]}
        title="Connections"
        subtitle="The connections and documents this ontology reads. Connect data and upload documents in Connectors."
        actions={
          <ButtonLink icon={FolderOpen} to={paths.data()}>
            Open in Connectors
          </ButtonLink>
        }
      />
      <div className="on-stack">
        <Section title="Connections" id="on-connections">
          {!versionId ? (
            <EmptyState
              icon={Cable}
              title="No version yet"
              text={
                manage
                  ? "An ontology reads the connections its current version maps. Generate a version to choose them."
                  : "An ontology reads the connections its current version maps. An owner or admin generates one."
              }
              action={
                manage && (
                  <ButtonLink icon={Sparkles} to={paths.generate(o)}>
                    Generate
                  </ButtonLink>
                )
              }
            />
          ) : version.isError || sources.error ? (
            <EmptyState
              icon={Cable}
              title="Couldn’t load its connections"
              text={(version.error || sources.error)?.message}
              action={
                <Button
                  onClick={() => {
                    void version.refetch();
                    sources.retry();
                  }}
                >
                  Retry
                </Button>
              }
            />
          ) : version.isPending || sources.pending || sets.isPending ? (
            <SkeletonText lines={3} />
          ) : !connections.length ? (
            <EmptyState
              icon={Cable}
              title="It reads no connections"
              text={
                manage
                  ? "Its current version maps no synced tables. Connect data, then generate a version that reads it."
                  : "Its current version maps no synced tables."
              }
              action={
                manage && (
                  <>
                    <ButtonLink icon={Plus} to={paths.connections(true)}>
                      Add data
                    </ButtonLink>
                    <ButtonLink
                      variant="secondary"
                      icon={Sparkles}
                      to={paths.generate(o)}
                    >
                      Generate
                    </ButtonLink>
                  </>
                )
              }
            />
          ) : (
            <ConnectionsGrid connections={connections} />
          )}
          {!!unknown && (
            <p className="dt-note">
              {plural(unknown, "table")} its version maps{" "}
              {unknown === 1 ? "comes" : "come"} from a connection deleted
              since.
            </p>
          )}
        </Section>
        <Section title="Document sets" id="on-sets">
          {sets.isPending ? (
            <SkeletonText lines={2} />
          ) : !sets.data ? (
            <p className="dt-note">
              <span role="alert">
                Couldn’t load the document sets: {sets.error?.message}
              </span>{" "}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => void sets.refetch()}
              >
                Retry
              </Button>
            </p>
          ) : !reads.length ? (
            <EmptyState
              size="sm"
              title="It reads no document sets"
              text="A version reads the sets picked when it was generated."
            />
          ) : (
            <ul className="dt-sets" aria-label="Document sets">
              {reads.map((s) => {
                const label = setLabel(s),
                  routed = !!s.readBy?.find((r) => r.ontologyId === o)?.routed,
                  kind = sources.connectors.find(
                    (c) => c.id === s.connector?.connectorId,
                  )?.kind;
                return (
                  <li key={s.id} aria-label={label}>
                    {kind ? (
                      <SourceLogo source={kind} size={16} />
                    ) : (
                      <Folder size={16} className="dt-icon" aria-hidden />
                    )}
                    <span className="dt-set-text">
                      <strong>{label}</strong>
                      <span>
                        {plural(s.documents || 0, "document")} ·{" "}
                        {routed
                          ? "Its routes read what they hold."
                          : "No route reads them: loads store them without reading them."}
                      </span>
                    </span>
                    <Tag tone={routed ? "success" : "neutral"}>
                      {routed ? "Reads" : "Stores only"}
                    </Tag>
                  </li>
                );
              })}
            </ul>
          )}
        </Section>
        <OntologyDocuments />
      </div>
    </Page>
  );
}

function ConnectionsGrid({
  connections,
}: {
  connections: OntologyConnection[];
}) {
  const navigate = useNavigate();
  const rows = connections.map((c) => ({
    ...c,
    id: c.connector.id,
    name: c.connector.name || connectorLabel(c.connector.kind),
    kind: connectorLabel(c.connector.kind),
    status: connectorBadge(c.connector),
    reads: [...c.tables, ...c.sets].join(", "),
  }));
  return (
    <DataGrid<(typeof rows)[number]>
      label="Connections"
      rows={rows}
      getRowKey={(c) => c.id}
      rowHeight={44}
      onRowClick={(c) => navigate(paths.connection(c.id))}
      columns={[
        {
          key: "name",
          label: "Name",
          width: 220,
          render: (c) => (
            <span className="dt-grid-name">
              <SourceLogo source={c.connector.kind} size={18} />
              <Link
                to={paths.connection(c.id)}
                className="on-row-link dc-clip"
                title={c.name}
                onClick={(e) => e.stopPropagation()}
              >
                {c.name}
              </Link>
            </span>
          ),
        },
        { key: "kind", label: "Kind", width: 130 },
        {
          key: "status",
          label: "Status",
          width: 170,
          sortValue: (c) => c.status.label,
          render: (c) => (
            <StatusBadge
              status={c.status.status}
              label={c.status.label}
              size="sm"
            />
          ),
        },
        {
          // Its tables the version maps, then its own sets the ontology
          // reads.
          key: "reads",
          label: "Reads",
          width: 280,
          render: (c) => (
            <span className="on-cell-text" title={c.reads}>
              {c.reads}
            </span>
          ),
        },
      ]}
    />
  );
}
