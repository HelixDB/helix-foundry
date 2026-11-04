import { useEffect, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Cable,
  Folder,
  KeyRound,
  MoreHorizontal,
  Pencil,
  RefreshCw,
  Table2,
  Trash2,
} from "lucide-react";
import type { Connector, ConnectorSync } from "../../../shared/wfe";
import { ApiError } from "../../api";
import { paths } from "../../paths";
import { Loading, RefreshFailed, useAction, useWorkspace } from "../../ui";
import {
  Button,
  ButtonLink,
  ConfirmDialog,
  DataGrid,
  DropdownMenu,
  EmptyState,
  KeyValue,
  MenuItem,
  Page,
  PageHeader,
  Panel,
  Section,
  SkeletonText,
  SourceLogo,
  StatusBadge,
  Tag,
  connectorLabel,
  formatDate,
  formatNumber,
  formatRelative,
  plural,
  type KeyValueItem,
} from "../../kit";
import { documentSetQueries } from "../documents/documents-api";
import { readersText } from "../documents/model";
import { useOntologyNames } from "../documents/sets";
import { ProGate } from "../workspace/pro-gate";
import { Modal } from "./connect-data";
import { ConnectionForm } from "./connection-form";
import { EventSourcePage } from "./event-source-page";
import {
  connectorKindsQuery,
  connectorQuery,
  connectorSyncQuery,
  connectorSyncsQuery,
  deleteConnector,
  syncConnector,
} from "./data-api";
import {
  canManage,
  connectorBadge,
  isEventSource,
  isKnownKind,
  isLiveSync,
  keyLabel,
  lastSyncedAt,
  sourceSummary,
  syncBadge,
} from "./model";
import { useAccessCheck } from "../workspace/access-guard";
import {
  AccountNotices,
  useSignInStart,
  type SignedInArrival,
} from "./sign-in";
import { accountLine, needsSignIn } from "./sign-ins";

const crumbs = [{ label: "Connectors", to: paths.data() }];
// The kit's grid widens every column to its widest one, so the sync history
// gives all four the same width: together they fill the panel at 1440px.
const COL = 170;

// How long a finished sync ran: "4s", "2m 5s", "1h 3m".
function duration(s: ConnectorSync) {
  const from = Date.parse(s.startedAt || s.createdAt || ""),
    to = Date.parse(s.finishedAt || "");
  if (Number.isNaN(from) || Number.isNaN(to)) return "—";
  const secs = Math.max(0, Math.round((to - from) / 1000));
  return secs < 60
    ? `${secs}s`
    : secs < 3600
      ? `${Math.floor(secs / 60)}m ${secs % 60}s`
      : `${Math.floor(secs / 3600)}h ${Math.floor((secs % 3600) / 60)}m`;
}

// What a connector's settings say, never its credential: one connected by
// signing in says whose access it reads with instead, and for GitHub where
// Helix Foundry reads (`installUrl`, the app's install page).
function details(c: Connector, installUrl?: string): KeyValueItem[] {
  const config = c.config,
    items: KeyValueItem[] = [
      { label: "Connector", value: connectorLabel(c.kind) },
      c.account
        ? { label: "Access", value: accountLine(c) }
        : { label: "Credential", value: c.secretHint, mono: true },
    ];
  if (c.account && c.kind === "github")
    items.push({
      label: "GitHub App",
      value: installUrl ? (
        <>
          Reads where Helix Foundry is installed.{" "}
          <a href={installUrl} target="_blank" rel="noopener noreferrer">
            Install it on an organization
          </a>
        </>
      ) : (
        "Reads where Helix Foundry is installed"
      ),
    });
  if (config && "posthog" in config) {
    const p = config.posthog;
    items.push(
      {
        label: "Region",
        value:
          p.region === "custom"
            ? p.host
            : p.region === "eu"
              ? "EU Cloud"
              : "US Cloud",
      },
      { label: "Project", value: p.projectId, mono: true },
      { label: "Event history", value: plural(p.eventDays || 30, "day") },
    );
  }
  if (config && "granola" in config)
    items.push({
      label: "Documents",
      value: config.granola.transcripts ? "Notes and transcripts" : "Notes",
    });
  if (config && "slack" in config)
    items.push({
      label: "Message history",
      value: plural(config.slack.historyDays || 90, "day"),
    });
  if (config && "google" in config)
    items.push(
      // Signed in, it reads as the person who signed in (Access).
      ...(c.account
        ? []
        : [{ label: "Acts as", value: config.google.subject, mono: true }]),
      {
        label: "Event history",
        value: plural(config.google.eventDays || 90, "day"),
      },
    );
  if (config && "github" in config) {
    const g = config.github;
    items.push(
      {
        label: "Repositories",
        value: g.repos?.length
          ? g.repos.join(", ")
          : c.account
            ? "Every repository Helix Foundry is installed on that it can see"
            : "Every repository the token reads",
      },
      { label: "Review history", value: plural(g.reviewDays || 90, "day") },
    );
  }
  if (config && "s3" in config) {
    const s = config.s3;
    items.push(
      { label: "Bucket", value: s.bucket, mono: true },
      { label: "Folder", value: s.prefix || "Whole bucket", mono: !!s.prefix },
      { label: "Region", value: s.region, mono: true },
      ...("keys" in s
        ? [
            { label: "Access key ID", value: s.keys.accessKeyId, mono: true },
            ...(s.keys.endpoint
              ? [{ label: "Endpoint", value: s.keys.endpoint, mono: true }]
              : []),
          ]
        : "role" in s
          ? [{ label: "Role ARN", value: s.role?.roleArn, mono: true }]
          : []),
      {
        label: "Tables",
        value: s.tables?.length ? s.tables.join(", ") : "Every table",
      },
    );
  }
  const d =
    config && "postgres" in config
      ? config.postgres
      : config && "mysql" in config
        ? config.mysql
        : undefined;
  if (d)
    items.push(
      {
        label: "Host",
        value: `${d.host}${d.port ? ":" + d.port : ""}`,
        mono: true,
      },
      { label: "Database", value: d.database, mono: true },
      { label: "User", value: d.user, mono: true },
      ...(c.kind === "postgres"
        ? [{ label: "Schema", value: d.schema || "public", mono: true }]
        : []),
      {
        label: "Tables",
        value: d.tables?.length ? d.tables.join(", ") : "Every table",
      },
      {
        label: "Encryption",
        value: d.disableTls
          ? "No TLS"
          : d.ca
            ? "TLS, verified with your CA"
            : "TLS, verified",
      },
    );
  items.push({
    label: "Created",
    value: c.createdAt && formatDate(c.createdAt),
  });
  return items;
}

// /data/connections/:connectorId: a connection's syncs, settings and tables
// (helix-foundry's areas/data/connection.tsx), and the sets of documents its
// syncs write (a Granola note each), with how many each holds, whether it
// is at its cap and which ontologies read it. While a sync is queued or
// running, its status is polled until it ends, then everything refreshes.
// An API source has a page of its own (./event-source-page.tsx).
export function ConnectionDetail({ connectorId }: { connectorId: string }) {
  const { id, role, notify } = useWorkspace(),
    action = useAction(),
    qc = useQueryClient(),
    navigate = useNavigate(),
    arrival = (useLocation().state || {}) as SignedInArrival,
    signIn = useSignInStart(),
    manage = canManage(role);
  const connector = useQuery(connectorQuery(id, connectorId)),
    syncs = useQuery(connectorSyncsQuery(id, connectorId)),
    kinds = useQuery(connectorKindsQuery(id)),
    sets = useQuery(documentSetQueries.list(id)),
    ontologyName = useOntologyNames();
  const last = connector.data?.connector.lastSync;
  const polled = useQuery({
    ...connectorSyncQuery(id, last?.id || ""),
    enabled: isLiveSync(last),
  });
  // The polled sync is newer than the connector's copy of it.
  const current: ConnectorSync | null | undefined =
    polled.data && polled.data.sync.id === last?.id ? polled.data.sync : last;
  const live = isLiveSync(current);
  // Each polled status change refreshes the history; the end of the sync
  // refreshes the connector, its tables and its sets too.
  const polledStatus = polled.data?.sync.status;
  useEffect(() => {
    if (!polledStatus) return;
    const ended = !isLiveSync(polled.data!.sync);
    void qc.invalidateQueries({
      queryKey: ended
        ? [id, "connectors"]
        : [id, "connectors", connectorId, "syncs"],
    });
    if (ended) void qc.invalidateQueries({ queryKey: [id, "document-sets"] });
  }, [polledStatus]);
  const [editing, setEditing] = useState(false),
    [deleting, setDeleting] = useState<"confirm" | "pending" | null>(null);
  // Who would stop reading through source grants, asked before it goes.
  const access = useAccessCheck();
  useEffect(() => {
    if (signIn.error) notify(signIn.error.message, true);
  }, [signIn.error]);
  // Leaves the page before the deleted connection is refetched as missing.
  const remove = async () => {
    setDeleting("pending");
    if (!(await access.check({ deleteConnector: connectorId })))
      return setDeleting("confirm");
    try {
      await deleteConnector(id, connectorId);
      navigate(paths.data());
      qc.removeQueries({ queryKey: [id, "connectors", connectorId] });
      // Its sets and their documents went with it.
      await Promise.all(
        ["connectors", "document-sets", "documents"].map((key) =>
          qc.invalidateQueries({ queryKey: [id, key] }),
        ),
      );
      notify("Connection deleted");
    } catch (e) {
      setDeleting("confirm");
      notify((e as Error).message, true);
    }
  };
  if (connector.isPending)
    return (
      <Page>
        <SkeletonText lines={4} />
      </Page>
    );
  if (!connector.data)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Connection unavailable" />
        {connector.error instanceof ApiError &&
        connector.error.status === 404 ? (
          <EmptyState
            icon={Cable}
            title="This connection no longer exists"
            text={connector.error.message}
            action={<ButtonLink to={paths.data()}>All data</ButtonLink>}
          />
        ) : (
          <EmptyState
            icon={Cable}
            title="Couldn’t load this connection"
            text={connector.error?.message}
            action={
              <Button onClick={() => void connector.refetch()}>Retry</Button>
            }
          />
        )}
      </Page>
    );
  const c = connector.data.connector;
  if (isEventSource(c.kind)) return <EventSourcePage source={c} />;
  const name = c.name || connectorLabel(c.kind),
    // Nothing syncs while its account needs signing in again.
    broken = needsSignIn(c),
    badge = broken ? connectorBadge(c) : syncBadge(current),
    refreshed = lastSyncedAt(c),
    tables = c.tables || [],
    own = (sets.data || []).filter((s) => s.connector?.connectorId === c.id),
    kind = kinds.data?.kinds?.find((k) => k.kind === c.kind),
    // A Pro connection on a workspace that left Pro: the WFE refuses its
    // syncs until it upgrades again.
    gated = kind?.available === false;
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: name }]}
        icon={<SourceLogo source={c.kind} size={42} />}
        title={name}
        subtitle={[connectorLabel(c.kind), sourceSummary(c)]
          .filter(Boolean)
          .join(" · ")}
        meta={
          <>
            <StatusBadge status={badge.status} label={badge.label} />
            {refreshed && <span>Synced {formatRelative(refreshed)}</span>}
            {current?.status === "CONNECTOR_SYNC_STATUS_FAILED" &&
              current.finishedAt && (
                <span>Failed {formatRelative(current.finishedAt)}</span>
              )}
          </>
        }
        actions={
          manage && (
            <>
              <Button
                icon={RefreshCw}
                pending={action.pending}
                disabled={live || gated || broken}
                onClick={() =>
                  action.run(
                    () => syncConnector(id, c.id, "connection"),
                    "Sync queued",
                  )
                }
              >
                {live ? "Syncing…" : "Sync now"}
              </Button>
              <DropdownMenu
                label="Connection actions"
                align="end"
                trigger={<MoreHorizontal size={16} aria-hidden />}
              >
                {isKnownKind(c.kind) && !isEventSource(c.kind) && (
                  <MenuItem icon={Pencil} onSelect={() => setEditing(true)}>
                    Edit connection
                  </MenuItem>
                )}
                {/* Signed in: anyone who manages it may sign it in again
                    as themselves. */}
                {c.account && (
                  <MenuItem
                    icon={KeyRound}
                    disabled={signIn.pending}
                    onSelect={() =>
                      void signIn.go({ kind: c.kind, connectorId: c.id })
                    }
                  >
                    Sign in again
                  </MenuItem>
                )}
                <MenuItem
                  icon={Trash2}
                  danger
                  onSelect={() => setDeleting("confirm")}
                >
                  Delete connection
                </MenuItem>
              </DropdownMenu>
            </>
          )
        }
      />
      <RefreshFailed query={connector} />
      <AccountNotices
        connector={c}
        arrival={arrival}
        installUrl={kind?.github?.installUrl}
      />
      {gated && (
        <ProGate
          feature={`${connectorLabel(c.kind)} connections`}
          className="dc-pro-gate"
        />
      )}
      {current?.status === "CONNECTOR_SYNC_STATUS_FAILED" &&
        current.error &&
        !broken && (
          <p role="alert" className="error-box dc-sync-error">
            <strong>The last sync failed.</strong> {current.error}
          </p>
        )}
      <div className="dc-connection-grid">
        <Panel title="Recent syncs" padded={false}>
          <DataGrid
            label="Recent syncs"
            loading={syncs.isPending}
            rows={syncs.data?.syncs || []}
            getRowKey={(r) => r.id}
            maxHeight={360}
            emptyText="No syncs yet."
            columns={[
              {
                key: "status",
                label: "Status",
                width: COL,
                render: (r) => {
                  const b = syncBadge(r.id === current?.id ? current : r);
                  return (
                    <StatusBadge status={b.status} label={b.label} size="sm" />
                  );
                },
                sortValue: (r) => r.status,
              },
              {
                key: "createdAt",
                label: "Started",
                type: "timestamp",
                width: COL,
                render: (r) => (
                  <span className="dc-clip" title={formatDate(r.createdAt)}>
                    {r.createdAt ? formatRelative(r.createdAt) : "—"}
                  </span>
                ),
              },
              {
                key: "rowsSynced",
                label: "Rows",
                type: "integer",
                width: COL,
                render: (r) =>
                  r.status === "CONNECTOR_SYNC_STATUS_SUCCEEDED"
                    ? formatNumber(r.rowsSynced)
                    : "—",
              },
              {
                key: "error",
                label: "Details",
                width: COL,
                sortable: false,
                render: (r) => {
                  const text =
                    r.error ||
                    [
                      r.finishedAt && `Took ${duration(r)}`,
                      (r.attempts || 0) > 1 && `${r.attempts} attempts`,
                    ]
                      .filter(Boolean)
                      .join(" · ") ||
                    "—";
                  return (
                    <span className="dc-clip" title={text}>
                      {text}
                    </span>
                  );
                },
              },
            ]}
          />
        </Panel>
        <Panel title="Details">
          <KeyValue items={details(c, kind?.github?.installUrl)} />
        </Panel>
      </div>
      <Section
        title="Tables"
        description="Each table’s newest snapshot, with its columns and keys."
      >
        {tables.length ? (
          <ul className="dc-dataset-list" aria-label="Synced tables">
            {tables.map((t) => {
              const key = keyLabel(t),
                links = (t.foreignKeys || []).length;
              return (
                <li key={t.name}>
                  <Link to={paths.syncedTable(c.id, t.name)}>
                    <Table2 size={15} aria-hidden />
                    <strong className="ds-name" title={t.name}>
                      {t.name}
                    </strong>
                    <span className="dc-dataset-meta">
                      {formatNumber(t.rowCount)} rows ·{" "}
                      {plural((t.columns || []).length, "column")}
                    </span>
                    <span className="dc-dataset-meta dc-dataset-keys">
                      <span className="ds-name" title={key}>
                        {key}
                      </span>
                      {links > 0 && (
                        <span className="ds-tag">{plural(links, "link")}</span>
                      )}
                    </span>
                  </Link>
                </li>
              );
            })}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Table2}
            title="No tables yet"
            text={
              live
                ? "Tables appear when this sync finishes."
                : "Tables appear after the first sync finishes."
            }
          />
        )}
      </Section>
      {own.length > 0 && (
        <Section
          title="Documents"
          id="dc-documents"
          description="What its syncs write as documents, a set each, newest first at the source. A generation reads them as it reads any set, and extracting them draws on the month’s budget."
        >
          <ul className="dt-sets" aria-label="Document sets">
            {own.map((s) => {
              const label = s.name || s.id,
                held = s.documents || 0,
                max = s.connector?.maxDocuments || 0,
                readers = s.readBy || [];
              return (
                <li key={s.id} aria-label={label}>
                  <Folder size={16} className="dt-icon" aria-hidden />
                  <span className="dt-set-text">
                    <strong>{label}</strong>
                    <span>
                      {[
                        plural(held, "document"),
                        // At its cap, older ones at the source aren't here.
                        max && held >= max
                          ? `Keeps the newest ${formatNumber(max)}`
                          : "",
                        readersText(label, readers, ontologyName),
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </span>
                  </span>
                  <Tag tone={readers.length ? "success" : "neutral"}>
                    {readers.length
                      ? `Read by ${readers.length === 1 ? ontologyName(readers[0]!.ontologyId) : plural(readers.length, "ontology", "ontologies")}`
                      : "Not read"}
                  </Tag>
                </li>
              );
            })}
          </ul>
        </Section>
      )}
      {editing && isKnownKind(c.kind) && !isEventSource(c.kind) && (
        <Modal title={`Edit ${name}`} onClose={() => setEditing(false)}>
          <div className="pad connection-dialog">
            {/* The kind's S3 setup decides the form's access choices, so
                an S3 form waits for the kinds. */}
            {c.kind !== "s3" || kinds.data ? (
              <ConnectionForm
                kind={c.kind}
                connector={c}
                setup={kind?.s3}
                onDone={async (_, message) => {
                  await qc.invalidateQueries({ queryKey: [id, "connectors"] });
                  setEditing(false);
                  notify(message);
                }}
              />
            ) : kinds.error ? (
              <p role="alert" className="error-box">
                {kinds.error.message}
              </p>
            ) : (
              <Loading />
            )}
          </div>
        </Modal>
      )}
      {deleting && (
        <ConfirmDialog
          title={`Delete ${name}?`}
          description={`Foundry stops syncing it, and its tables${own.length ? " and documents" : ""} leave this workspace’s data. The source itself is not changed.`}
          confirmLabel="Delete connection"
          tone="danger"
          pending={deleting === "pending"}
          onCancel={() => setDeleting(null)}
          onConfirm={remove}
        />
      )}
      {access.dialog}
    </Page>
  );
}
