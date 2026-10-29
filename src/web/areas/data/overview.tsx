import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Cable, Folder, Plus, Upload } from "lucide-react";
import type { Connector } from "../../../shared/wfe";
import { number } from "../../api";
import { paths } from "../../paths";
import { RefreshFailed, useWorkspace } from "../../ui";
import {
  Button,
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
  cx,
  formatDate,
  formatRelative,
  plural,
} from "../../kit";
import {
  documentQueries,
  documentSetQueries,
} from "../documents/documents-api";
import { FORMAT_NAMES, readersText, setLabel } from "../documents/model";
import { useOntologyNames } from "../documents/sets";
import { ConnectData } from "./connect-data";
import { connectorsQuery } from "./data-api";
import { canManage, lastSyncedAt, rowTotal, syncBadge } from "./model";
import { useDataUpload } from "./upload";

// The Connectors page (/data): what the workspace holds at a glance. Its totals, its
// connections in a grid (a row opens its connection), and its document
// sets with what they hold and which ontologies read them: those people
// made, then the connections' own ("Granola / Notes"), which their syncs
// write. The explorer beside it (./explorer.tsx) opens everything one by
// one. Owners and admins add data (?add=1 opens the connector picker,
// ?add=<kind> that kind's form, as the Helix MCP's links do) and upload
// documents, with Upload documents or by dropping files on a set people
// made or anywhere else on the page (./upload.tsx). One list each, so it
// stays light.

type ConnectionRow = Connector & {
  nameText: string;
  kindText: string;
  statusText: string;
  tableCount: number;
  records: number;
  syncedAt: string;
};

export function DataOverview() {
  const { id, role } = useWorkspace(),
    manage = canManage(role),
    navigate = useNavigate(),
    name = useOntologyNames();
  const connectors = useQuery(connectorsQuery(id)),
    documents = useQuery(documentQueries.workspace(id)),
    sets = useQuery(documentSetQueries.list(id));
  const files = useDataUpload();
  const [params, setParams] = useSearchParams();
  const picker = params.has("add") && manage,
    setPicker = (open: boolean) =>
      setParams(
        (p) => {
          const next = new URLSearchParams(p);
          if (open) next.set("add", "1");
          else next.delete("add");
          return next;
        },
        { replace: true },
      );
  const rows = (connectors.data?.connectors || []).map(
    (c): ConnectionRow => ({
      ...c,
      nameText: c.name || connectorLabel(c.kind),
      kindText: connectorLabel(c.kind),
      statusText: syncBadge(c.lastSync).label,
      tableCount: (c.tables || []).length,
      records: rowTotal(c),
      syncedAt: lastSyncedAt(c) || "",
    }),
  );
  // People's sets first; a connection's take no drop.
  const setList = [
      ...(sets.data || []).filter((s) => !s.connector),
      ...(sets.data || []).filter((s) => s.connector),
    ],
    docs = documents.data || [],
    // The documents list is the uploads: the connections' count by set.
    documentCount = setList.reduce(
      (n, s) => n + (s.connector ? s.documents || 0 : 0),
      docs.length,
    );
  // A failed refetch keeps the page it had; the error state is for no data.
  const empty = !!connectors.data && !rows.length,
    failed = !connectors.data && !!connectors.error;
  const addData = manage && (
    <Button variant="primary" icon={Plus} onClick={() => setPicker(true)}>
      Add data
    </Button>
  );
  return (
    <Page>
      <div className="dt-page" {...files.surface}>
        <PageHeader
          title="Connectors"
          subtitle="Where your data comes from, what it holds and how it stays up to date."
          meta={
            connectors.data && (
              <>
                <span>{plural(rows.length, "connection")}</span>
                <span>
                  {plural(
                    rows.reduce((n, c) => n + c.tableCount, 0),
                    "table",
                  )}
                </span>
                <span>
                  {number(rows.reduce((n, c) => n + c.records, 0))} records
                </span>
                {documents.data && sets.data && (
                  <span>{plural(documentCount, "document")}</span>
                )}
                {sets.data && <span>{plural(setList.length, "set")}</span>}
              </>
            )
          }
          actions={
            manage && (
              <>
                <Button
                  icon={Upload}
                  disabled={!files.ready}
                  onClick={() => files.choose()}
                >
                  Upload documents
                </Button>
                {addData}
              </>
            )
          }
        />
        {files.chooser}
        {connectors.data && <RefreshFailed query={connectors} />}
        {files.list}
        <Section title="Connections" id="dt-connections">
          {failed ? (
            <EmptyState
              icon={Cable}
              title="Couldn’t load your connections"
              text={connectors.error?.message}
              action={
                <Button onClick={() => void connectors.refetch()}>Retry</Button>
              }
            />
          ) : empty ? (
            <EmptyState
              title="No data connected"
              text="Connect an app such as Stripe, GitHub, Slack, Notion or Google Workspace, a PostgreSQL or MySQL database, or files in Amazon S3."
              action={addData}
            />
          ) : (
            <DataGrid<ConnectionRow>
              label="Connections"
              loading={connectors.isPending}
              rows={rows}
              getRowKey={(c) => c.id}
              rowHeight={44}
              maxHeight={440}
              onRowClick={(c) => navigate(paths.connection(c.id))}
              columns={[
                {
                  key: "nameText",
                  label: "Name",
                  width: 220,
                  render: (c) => (
                    <span className="dt-grid-name">
                      <SourceLogo source={c.kind} size={18} />
                      <Link
                        to={paths.connection(c.id)}
                        className="on-row-link dc-clip"
                        title={c.nameText}
                        onClick={(e) => e.stopPropagation()}
                      >
                        {c.nameText}
                      </Link>
                    </span>
                  ),
                },
                { key: "kindText", label: "Kind", width: 130 },
                {
                  key: "statusText",
                  label: "Status",
                  width: 130,
                  render: (c) => {
                    const b = syncBadge(c.lastSync);
                    return (
                      <span
                        title={
                          c.lastSync?.status === "CONNECTOR_SYNC_STATUS_FAILED"
                            ? c.lastSync.error
                            : undefined
                        }
                      >
                        <StatusBadge
                          status={b.status}
                          label={b.label}
                          size="sm"
                        />
                      </span>
                    );
                  },
                },
                {
                  key: "tableCount",
                  label: "Tables",
                  width: 90,
                  type: "integer",
                },
                {
                  key: "records",
                  label: "Records",
                  width: 120,
                  type: "integer",
                  render: (c) => number(c.records),
                },
                {
                  key: "syncedAt",
                  label: "Synced",
                  width: 140,
                  type: "timestamp",
                  render: (c) =>
                    c.syncedAt ? (
                      <span title={formatDate(c.syncedAt)}>
                        {formatRelative(c.syncedAt)}
                      </span>
                    ) : (
                      "—"
                    ),
                },
              ]}
            />
          )}
        </Section>
        <Section title="Document sets" id="dt-sets">
          {sets.isPending || documents.isPending ? (
            <SkeletonText lines={2} />
          ) : !sets.data || !documents.data ? (
            <p className="dt-note">
              <span role="alert">
                Couldn’t load the documents:{" "}
                {(sets.error || documents.error)?.message}
              </span>{" "}
              <Button
                size="sm"
                variant="ghost"
                onClick={() => {
                  void sets.refetch();
                  void documents.refetch();
                }}
              >
                Retry
              </Button>
            </p>
          ) : !setList.length ? (
            <EmptyState
              size="sm"
              title="No documents yet"
              text={
                manage
                  ? `Upload ${FORMAT_NAMES} files, or drop them here.`
                  : "An owner or admin can upload them."
              }
              className={cx(files.over === "documents" && "is-over")}
            />
          ) : (
            <ul
              className={cx("dt-sets", files.over === "documents" && "is-over")}
              aria-label="Document sets"
            >
              {setList.map((s) => {
                const label = setLabel(s),
                  inSet = docs.filter((d) => d.setId === s.id),
                  held = s.documents ?? inSet.length,
                  readers = s.readBy || [],
                  kind = rows.find(
                    (c) => c.id === s.connector?.connectorId,
                  )?.kind;
                return (
                  <li
                    key={s.id}
                    aria-label={label}
                    className={cx(files.over === s.id && "is-over")}
                    {...(s.connector ? {} : files.onto(s.id))}
                  >
                    {kind ? (
                      <SourceLogo source={kind} size={16} />
                    ) : (
                      <Folder size={16} className="dt-icon" aria-hidden />
                    )}
                    <span className="dt-set-text">
                      <strong>{label}</strong>
                      <span>
                        {plural(held, "document")} ·{" "}
                        {readersText(label, readers, name)}
                        {s.description ? ` ${s.description}` : ""}
                      </span>
                    </span>
                    <Tag tone={readers.length ? "success" : "neutral"}>
                      {readers.length
                        ? `Read by ${readers.length === 1 ? name(readers[0]!.ontologyId) : plural(readers.length, "ontology", "ontologies")}`
                        : "Not read"}
                    </Tag>
                  </li>
                );
              })}
            </ul>
          )}
        </Section>
        {picker && (
          <ConnectData
            initialKind={params.get("add")}
            onClose={() => setPicker(false)}
          />
        )}
      </div>
    </Page>
  );
}
