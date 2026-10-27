import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ArrowRight, KeyRound, Table2 } from "lucide-react";
import type { ConnectorColumn } from "../../../shared/wfe";
import { paths } from "../../paths";
import { useWorkspace } from "../../ui";
import {
  DataGrid,
  EmptyState,
  Page,
  PageHeader,
  Panel,
  SkeletonText,
  Tag,
  connectorLabel,
  formatNumber,
  formatRelative,
  plural,
} from "../../kit";
import { connectorQuery } from "./data-api";
import { hasTableChoice, referencesTo } from "./model";

// /data/connections/:connectorId/tables/:table: one synced table's newest
// snapshot: its columns with types, its primary key, the foreign keys it
// holds and the ones that point at it. Linked tables of the same connection
// open in place.
export function SyncedTablePage({
  connectorId,
  table,
}: {
  connectorId: string;
  table: string;
}) {
  const { id } = useWorkspace();
  const connector = useQuery(connectorQuery(id, connectorId));
  if (connector.isPending)
    return (
      <Page>
        <SkeletonText lines={4} />
      </Page>
    );
  const c = connector.data?.connector,
    name = c ? c.name || connectorLabel(c.kind) : "",
    tables = c?.tables || [],
    t = tables.find((x) => x.name === table);
  const crumbs = [
    { label: "Connectors", to: paths.data() },
    ...(c ? [{ label: name, to: paths.connection(c.id) }] : []),
    { label: table },
  ];
  if (!c || !t)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Table unavailable" />
        <EmptyState
          icon={Table2}
          title="This table is not synced"
          text={
            connector.error?.message ||
            "It may have been removed from the connection’s tables, or its first sync has not finished."
          }
        />
      </Page>
    );
  const key = t.primaryKey || [],
    links = t.foreignKeys || [],
    incoming = referencesTo(tables, t.name),
    synced = new Set(tables.map((x) => x.name)),
    columns = t.columns || [],
    typed = columns.some((col) => col.type);
  const linkTo = (target: string, column: string) =>
    synced.has(target) ? (
      <Link to={paths.syncedTable(c.id, target)}>
        {target}.{column}
      </Link>
    ) : (
      `${target}.${column}`
    );
  return (
    <Page>
      <PageHeader
        breadcrumbs={crumbs}
        icon={
          <span className="dc-table-icon">
            <Table2 size={18} aria-hidden />
          </span>
        }
        title={t.name}
        subtitle={
          name === connectorLabel(c.kind)
            ? name
            : `${name} · ${connectorLabel(c.kind)}`
        }
        meta={
          <>
            <span>{plural(Number(t.rowCount || 0), "row")}</span>
            <span>{plural(columns.length, "column")}</span>
            {t.syncedAt && <span>Synced {formatRelative(t.syncedAt)}</span>}
          </>
        }
      />
      <div className="dc-table-grid">
        <Panel title="Columns" padded={false}>
          <DataGrid<ConnectorColumn>
            label="Columns"
            rows={columns}
            getRowKey={(r) => r.name}
            maxHeight={560}
            emptyText="No columns recorded."
            columns={[
              {
                key: "name",
                label: "Column",
                width: typed ? 226 : 340,
                render: (r) => (
                  <span className="dc-clip hf-mono" title={r.name}>
                    {r.name}
                  </span>
                ),
              },
              ...(typed
                ? [
                    {
                      key: "type",
                      label: "Type",
                      width: 226,
                      render: (r: ConnectorColumn) => {
                        const text =
                          (r.type || "—") + (r.nullable ? " · nullable" : "");
                        return (
                          <span className="dc-clip hf-mono" title={text}>
                            {r.type || "—"}
                            {r.nullable && <small> · nullable</small>}
                          </span>
                        );
                      },
                    },
                  ]
                : []),
              {
                key: "key",
                label: "Key",
                width: typed ? 226 : 340,
                sortable: false,
                render: (r) => {
                  const fk = links.find((l) => l.column === r.name);
                  return (
                    <span className="dc-key-cell">
                      {key.includes(r.name) && (
                        <Tag tone="accent" icon={KeyRound}>
                          Primary
                        </Tag>
                      )}
                      {fk && (
                        <span
                          className="dc-fk dc-clip"
                          title={`${fk.referencedTable}.${fk.referencedColumn}`}
                        >
                          <ArrowRight size={12} aria-hidden />
                          {linkTo(fk.referencedTable, fk.referencedColumn)}
                        </span>
                      )}
                    </span>
                  );
                },
              },
            ]}
          />
          {!typed && columns.length > 0 && !hasTableChoice(c.kind) && (
            <p className="muted small dc-note">
              {connectorLabel(c.kind)} is an API source: its columns are the
              fields found in the synced rows, without types.
            </p>
          )}
          {c.kind === "s3" && columns.length > 0 && (
            <p className="muted small dc-note">
              Column types are inferred from the files.
            </p>
          )}
        </Panel>
        <div className="dc-key-panels">
          <Panel title="Primary key">
            {key.length ? (
              <p className="hf-mono">{key.join(", ")}</p>
            ) : (
              <p className="muted">
                The source declares no primary key for this table.
              </p>
            )}
          </Panel>
          <Panel
            title="Foreign keys"
            subtitle="Columns that point at other tables"
          >
            {links.length ? (
              <ul className="dc-key-list" aria-label="Foreign keys">
                {links.map((fk) => (
                  <li key={fk.column}>
                    <span className="hf-mono">{fk.column}</span>
                    <ArrowRight size={12} aria-hidden />
                    {linkTo(fk.referencedTable, fk.referencedColumn)}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">None.</p>
            )}
          </Panel>
          <Panel
            title="Referenced by"
            subtitle="Columns in other tables that point here"
          >
            {incoming.length ? (
              <ul className="dc-key-list" aria-label="Referenced by">
                {incoming.map((fk) => (
                  <li key={fk.table + "." + fk.column}>
                    {linkTo(fk.table, fk.column)}
                    <ArrowRight size={12} aria-hidden />
                    <span className="hf-mono">{fk.referencedColumn}</span>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="muted">None.</p>
            )}
          </Panel>
          <Panel title="Snapshot">
            <p className="muted small">
              {formatNumber(t.rowCount)} rows
              {t.syncedAt && `, synced ${formatRelative(t.syncedAt)}`}.{" "}
              <Link to={paths.connection(c.id)}>See recent syncs</Link>
            </p>
          </Panel>
        </div>
      </div>
    </Page>
  );
}
