import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Cable, Database, RefreshCw } from "lucide-react";
import type { Resource } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { useAction, useResources, useWorkspace } from "../../ui";
import {
  Button,
  DataGrid,
  EmptyState,
  KeyValue,
  Page,
  PageHeader,
  Panel,
  Section,
  SkeletonText,
  SourceLogo,
  StatusBadge,
  connectorLabel,
  connectorOf,
  formatDate,
  formatNumber,
  formatRelative,
  toStatus,
} from "../../kit";
import { paths } from "../../paths";

// Interim source page: status, recent syncs and the datasets it produces.
export function ConnectionDetail({ sourceId }: { sourceId: string }) {
  const { id, role } = useWorkspace(),
    action = useAction();
  const source = useQuery({
    queryKey: [id, "source", sourceId],
    queryFn: () =>
      api<Resource>(`/workspaces/${id}/resources/source/${sourceId}`),
  });
  const jobs = useResources("job", { limit: 500 }),
    datasets = useResources("dataset", { limit: 500 });
  const crumbs = [
    { label: "Data", to: paths.data() },
    { label: "Data", to: paths.data() },
  ];
  if (source.isPending)
    return (
      <Page>
        <SkeletonText lines={4} />
      </Page>
    );
  if (source.error || !source.data)
    return (
      <Page>
        <PageHeader breadcrumbs={crumbs} title="Connection unavailable" />
        <EmptyState
          icon={Cable}
          title="This connection no longer exists"
          text={source.error?.message}
        />
      </Page>
    );
  const s = source.data,
    connector = connectorOf(s);
  const runs = ((jobs.data?.items || []) as Resource[])
    .filter((j) => j.data.sourceId === s.id)
    .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  const produced = ((datasets.data?.items || []) as Resource[]).filter(
    (d) => d.data.sourceId === s.id,
  );
  const latest = runs[0];
  return (
    <Page>
      <PageHeader
        breadcrumbs={[...crumbs, { label: s.name }]}
        icon={<SourceLogo source={connector} size={28} />}
        title={s.name}
        subtitle={[s.data.connectionName, connectorLabel(connector)]
          .filter(Boolean)
          .join(" · ")}
        meta={
          <>
            <StatusBadge
              status={
                latest
                  ? toStatus(latest.data.status)
                  : toStatus(s.data.sync?.status)
              }
            />
            {latest && (
              <span>Last activity {formatRelative(latest.updatedAt)}</span>
            )}
          </>
        }
        actions={
          role !== "viewer" && (
            <Button
              icon={RefreshCw}
              pending={action.pending}
              onClick={() =>
                action.run(
                  () => api(`/workspaces/${id}/sources/${s.id}/sync`, {}),
                  "Sync queued",
                )
              }
            >
              Sync now
            </Button>
          )
        }
      />
      <div className="dc-connection-grid">
        <Panel title="Recent syncs" padded={false}>
          <DataGrid
            label="Recent syncs"
            loading={jobs.isPending}
            rows={runs}
            getRowKey={(r) => r.id}
            maxHeight={360}
            emptyText="No syncs yet."
            columns={[
              {
                key: "status",
                label: "Status",
                width: 130,
                render: (r) => (
                  <StatusBadge status={toStatus(r.data.status)} size="sm" />
                ),
                sortValue: (r) => r.data.status,
              },
              { key: "createdAt", label: "Started", type: "timestamp" },
              { key: "updatedAt", label: "Updated", type: "timestamp" },
              {
                key: "message",
                label: "Details",
                width: 240,
                render: (r) =>
                  r.data.error || r.data.progress?.message || r.name,
                sortable: false,
              },
            ]}
          />
        </Panel>
        <Panel title="Details">
          <KeyValue
            items={[
              { label: "Connector", value: connectorLabel(connector) },
              {
                label: "Sync mode",
                value: s.data.sync?.mode,
                hint: s.data.sync?.reason,
              },
              {
                label: "Schedule",
                value: s.data.schedule ? `${s.data.schedule} UTC` : "Automatic",
                mono: !!s.data.schedule,
              },
              {
                label: "Next refresh",
                value: s.data.paused
                  ? "Automatic refresh paused"
                  : s.data.nextRun
                    ? formatDate(s.data.nextRun)
                    : null,
              },
              { label: "Created", value: formatDate(s.createdAt) },
            ]}
          />
        </Panel>
      </div>
      <Section
        title="Datasets"
        description="Tables this connection keeps up to date."
      >
        {produced.length ? (
          <ul className="dc-dataset-list">
            {produced.map((d) => (
              <li key={d.id}>
                <Link to={paths.dataset(d.id)}>
                  <Database size={15} aria-hidden />
                  <strong>{d.name}</strong>
                  <span>
                    {formatNumber(d.data.profile?.rows)} rows ·{" "}
                    {d.data.profile?.columns?.length || 0} columns
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        ) : (
          <EmptyState
            size="sm"
            icon={Database}
            title="No datasets yet"
            text="Datasets appear after the first sync finishes."
          />
        )}
      </Section>
    </Page>
  );
}
