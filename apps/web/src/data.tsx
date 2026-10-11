import { useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { api, relative, number } from "./api";
import { paths } from "./paths";
import {
  useWorkspace,
  useResources,
  useAction,
  PageHeader,
  Loading,
  AskButton,
  DataTable,
} from "./ui";
import type { Resource } from "../../../packages/shared/src";
export default function DataPage() {
  const { id } = useWorkspace(),
    sources = useResources("source");
  const { datasetId: selectedId } = useParams();
  const detail = useQuery({
    queryKey: [id, "dataset", selectedId],
    enabled: !!selectedId,
    queryFn: () => api(`/workspaces/${id}/resources/dataset/${selectedId}`),
  });
  if (selectedId)
    return detail.isPending ? (
      <Loading />
    ) : detail.error ? (
      <p role="alert">{detail.error.message}</p>
    ) : (
      <DatasetDetail
        key={selectedId}
        dataset={detail.data}
        sources={sources.data?.items || []}
      />
    );
  // The list of data is the Data page (areas/data/connections.tsx).
  return <Navigate to={paths.data()} replace />;
}
function DatasetDetail({
  dataset: d,
  sources,
}: {
  dataset: Resource;
  sources: Resource[];
}) {
  const { id, role } = useWorkspace(),
    action = useAction(),
    [description, setDescription] = useState(d.description),
    [offset, setOffset] = useState(0);
  const preview = useQuery({
    queryKey: [id, "preview", d.id, d.data.activeVersion, offset],
    queryFn: () =>
      api(`/workspaces/${id}/datasets/${d.id}/preview`, { offset, limit: 50 }),
  });
  const pending = useQuery({
    queryKey: [id, "version", d.data.pendingVersion],
    enabled: !!d.data.pendingVersion,
    queryFn: () =>
      api(`/workspaces/${id}/resources/version/${d.data.pendingVersion}`),
  });
  const currentVersion = useQuery({
    queryKey: [id, "version", d.data.activeVersion],
    queryFn: () =>
      api(`/workspaces/${id}/resources/version/${d.data.activeVersion}`),
  });
  const lineage = useResources("lineage"),
    source = sources.find((s) => s.id === d.data.sourceId);
  return (
    <>
      <Link className="back-link" to="/data">
        ← Data
      </Link>
      <PageHeader title={d.name} description={d.description}>
        <AskButton resourceId={d.id} title={d.name} />
      </PageHeader>
      {d.data.pendingVersion && (
        <div className="notice">
          <div>
            <strong>The source schema changed</strong>
            <p>
              Your existing views still use the previous snapshot. Review the
              schema in Details before accepting.
            </p>
          </div>
          {role !== "viewer" && (
            <button
              className="button"
              onClick={() =>
                action.run(() =>
                  api(`/workspaces/${id}/datasets/${d.id}/accept-version`, {}),
                )
              }
            >
              Accept new snapshot
            </button>
          )}
        </div>
      )}
      <div className="dataset-meta">
        <span>
          {source?.name || "Imported data"} · {number(d.data.profile.rows)} rows
          · refreshed {relative(currentVersion.data?.createdAt || d.updatedAt)}
        </span>
      </div>
      {preview.isPending ? (
        <Loading />
      ) : preview.error ? (
        <p role="alert">{preview.error.message}</p>
      ) : (
        <>
          <DataTable rows={preview.data.rows} />
          <div className="pagination">
            <span>
              Sample · {offset + 1}–{Math.min(offset + 50, d.data.profile.rows)}
            </span>
            <div className="buttons">
              <button
                disabled={!offset}
                onClick={() => setOffset((n) => Math.max(0, n - 50))}
              >
                Previous
              </button>
              <button
                disabled={offset + 50 >= d.data.profile.rows}
                onClick={() => setOffset((n) => n + 50)}
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}
      <details className="disclosure dataset-details">
        <summary>Details</summary>
        {pending.data && (
          <section>
            <h3>Proposed schema</h3>
            <DataTable
              rows={pending.data.data.profile.columns.map((c: any) => ({
                column: c.name,
                type: c.type,
                change: d.data.profile.columns.some(
                  (old: any) => old.name === c.name && old.type === c.type,
                )
                  ? "Unchanged"
                  : "Added or type changed",
              }))}
            />
            <p className="muted">
              Removed:{" "}
              {d.data.profile.columns
                .filter(
                  (old: any) =>
                    !pending.data.data.profile.columns.some(
                      (c: any) => c.name === old.name,
                    ),
                )
                .map((c: any) => c.name)
                .join(", ") || "None"}
            </p>
          </section>
        )}
        <h3>Schema and profile</h3>
        <DataTable
          rows={d.data.profile.columns.map((c: any) => ({
            column: c.name,
            type: c.type,
            distinct: c.distinct,
            missing: c.nulls,
            signals: [
              c.primaryKey ? "Candidate key" : "",
              c.sensitive ? "Sensitive" : "",
            ]
              .filter(Boolean)
              .join(", "),
          }))}
        />
        <h3>Description</h3>
        <label>
          Dataset description
          <textarea
            rows={3}
            disabled={role === "viewer"}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </label>
        {role !== "viewer" && (
          <button
            className="button"
            disabled={action.pending || description === d.description}
            onClick={() =>
              action.run(
                () =>
                  api(
                    `/workspaces/${id}/datasets/${d.id}`,
                    { description },
                    "PATCH",
                  ),
                "Description saved",
              )
            }
          >
            Save description
          </button>
        )}
        <details>
          <summary>Snapshot history</summary>
          {d.data.versions.map((v: string) => (
            <p key={v}>
              <code>{v}</code>
              {v === d.data.activeVersion
                ? " · Current"
                : v === d.data.pendingVersion
                  ? " · Needs review"
                  : ""}
            </p>
          ))}
        </details>
        <details>
          <summary>Lineage</summary>
          {lineage.data?.items
            .filter(
              (l: Resource) =>
                l.data.from === d.id ||
                l.data.to === d.id ||
                l.data.input === d.id ||
                l.data.output === d.id,
            )
            .map((l: Resource) => (
              <div key={l.id}>
                {l.name}
                <pre>{JSON.stringify(l.data, null, 2)}</pre>
              </div>
            ))}
          <p className="muted">
            Origin: {source?.name || "File / ingestion API"}
          </p>
        </details>
        <div className="buttons">
          <a
            className="button"
            href={`/api/v1/workspaces/${id}/datasets/${d.id}/export`}
          >
            Export Parquet
          </a>
          <a
            className="button"
            href={`/api/v1/workspaces/${id}/datasets/${d.id}/export?format=csv`}
          >
            Export CSV
          </a>
          {source && (
            <Link className="button" to={paths.connections()}>
              Connection settings
            </Link>
          )}
        </div>
      </details>
    </>
  );
}
