import { useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";
import { useQueries, useQuery } from "@tanstack/react-query";
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
    [page, setPage] = useState<{ versionId: string; offset: number }>({
      versionId: d.data.activeVersion,
      offset: 0,
    }),
    [selectedVersion, setSelectedVersion] = useState<string>();
  const versionId = selectedVersion || d.data.activeVersion;
  // A page belongs to the snapshot it was turned on. When a refresh replaces
  // the current snapshot, its rows start again from the first page; a chosen
  // historical snapshot keeps its page.
  const offset = page.versionId === versionId ? page.offset : 0;
  const preview = useQuery({
    queryKey: [id, "preview", d.id, versionId, offset],
    queryFn: () =>
      api(`/workspaces/${id}/datasets/${d.id}/versions/${versionId}/preview`, {
        offset,
        limit: 50,
      }),
  });
  const pending = useQuery({
    queryKey: [id, "version", d.data.pendingVersion],
    enabled: !!d.data.pendingVersion,
    queryFn: () =>
      api(`/workspaces/${id}/resources/version/${d.data.pendingVersion}`),
  });
  // Every refresh that changes the data adds a snapshot, newest first: offer
  // the current and pending ones, then the latest 30 others (and the one on
  // display, should newer refreshes push it out).
  const historical: string[] = d.data.versions.filter(
      (v: string) => v !== d.data.activeVersion && v !== d.data.pendingVersion,
    ),
    listed = historical.filter((v, i) => i < 30 || v === versionId),
    unlisted = historical.length - listed.length,
    choices: string[] = [
      d.data.activeVersion,
      d.data.pendingVersion,
      ...listed,
    ].filter(Boolean);
  const choiceVersions = useQueries({
    queries: choices.map((v) => ({
      queryKey: [id, "version", v],
      staleTime: Infinity,
      queryFn: () => api<Resource>(`/workspaces/${id}/resources/version/${v}`),
    })),
  });
  const snapshot = preview.data?.snapshot;
  const profile = snapshot?.profile;
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
      <label>
        Snapshot
        <select
          value={versionId}
          onChange={(e) => {
            setSelectedVersion(
              e.target.value === d.data.activeVersion
                ? undefined
                : e.target.value,
            );
            setPage({ versionId: e.target.value, offset: 0 });
          }}
        >
          {choices.map((v, i) => {
            const at = choiceVersions[i].data?.createdAt;
            return (
              <option key={v} value={v}>
                {(v === d.data.activeVersion
                  ? "Current"
                  : v === d.data.pendingVersion
                    ? "Needs review"
                    : "Historical") +
                  (at ? ` · ${new Date(at).toLocaleString()}` : "")}
              </option>
            );
          })}
          {unlisted > 0 && (
            <option disabled>
              {`${number(unlisted)} older ${unlisted === 1 ? "snapshot" : "snapshots"} not listed`}
            </option>
          )}
        </select>
      </label>
      {snapshot && (
        <div className="dataset-meta">
          <span>
            {source?.name || "Imported data"} · {number(profile.rows)} rows ·
            {snapshot.status === "current"
              ? "Current snapshot"
              : snapshot.status === "pending"
                ? "Snapshot awaiting review"
                : "Historical snapshot"}
            {" · "}
            <time dateTime={snapshot.createdAt} title={snapshot.createdAt}>
              {new Date(snapshot.createdAt).toLocaleString()} (
              {relative(snapshot.createdAt)})
            </time>
          </span>
        </div>
      )}
      {preview.isPending ? (
        <Loading />
      ) : preview.error ? (
        <p role="alert">{preview.error.message}</p>
      ) : (
        <>
          {profile.rows === 0 ? (
            <p role="status">
              This snapshot has no rows. Schema:{" "}
              {profile.columns
                .map((c: any) => `${c.name} (${c.type})`)
                .join(", ")}
            </p>
          ) : (
            <DataTable rows={preview.data.rows} />
          )}
          <div className="pagination">
            <span>
              Sample · {Math.min(offset + 1, profile.rows)}–
              {Math.min(offset + 50, profile.rows)}
            </span>
            <div className="buttons">
              <button
                disabled={!offset}
                onClick={() =>
                  setPage({ versionId, offset: Math.max(0, offset - 50) })
                }
              >
                Previous
              </button>
              <button
                disabled={offset + 50 >= profile.rows}
                onClick={() => setPage({ versionId, offset: offset + 50 })}
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
          rows={(profile?.columns || []).map((c: any) => ({
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
        <DatasetPrivacy key={JSON.stringify(d.data.privacy)} dataset={d} />
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
        {versionId !== d.data.activeVersion && (
          <p className="muted">
            Exports and queries use the current snapshot. Selecting another
            snapshot here only changes this preview.
          </p>
        )}
        <div className="buttons">
          <a
            className="button"
            href={`/api/v1/workspaces/${id}/datasets/${d.id}/export`}
          >
            {versionId === d.data.activeVersion
              ? "Export Parquet"
              : "Export current Parquet"}
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

function DatasetPrivacy({ dataset }: { dataset: Resource }) {
  const { id, role } = useWorkspace(),
    action = useAction();
  const saved: string[] = dataset.data.privacy?.additionalColumns || [];
  const detected = new Set<string>(dataset.data.privacy?.detectedColumns || []);
  const [additional, setAdditional] = useState(saved);
  const names: string[] = [
    ...new Set<string>([
      ...dataset.data.profile.columns.map((c: any) => c.name),
      ...saved,
    ]),
  ];
  return (
    <section className="dataset-privacy" aria-label="Hosted AI privacy">
      <h3>Hosted AI privacy</h3>
      <p className="muted">
        Columns you mark are masked in sample rows sent to hosted AI, unless raw
        data is allowed. Home leaves them out of its key metrics with any AI,
        including Local AI, even when raw data is allowed. Hands-free voice, if
        you use it, still speaks answers, which can include values from these
        columns, because it sends answers to OpenAI.
      </p>
      <fieldset
        className="plain-fieldset"
        disabled={role !== "owner" || action.pending}
      >
        <legend>Columns to mask</legend>
        {names.map((name) => (
          <label key={name}>
            <input
              type="checkbox"
              aria-label={"Mask " + name}
              disabled={detected.has(name)}
              checked={detected.has(name) || additional.includes(name)}
              onChange={(e) =>
                setAdditional((values) =>
                  e.target.checked
                    ? [...values, name]
                    : values.filter((n) => n !== name),
                )
              }
            />
            <span>
              {name}
              {detected.has(name)
                ? " (detected sensitive)"
                : !dataset.data.profile.columns.some(
                      (c: any) => c.name === name,
                    )
                  ? " (not in current snapshot)"
                  : ""}
            </span>
          </label>
        ))}
      </fieldset>
      {role === "owner" && (
        <button
          className="button"
          disabled={
            action.pending ||
            JSON.stringify([...additional].sort()) ===
              JSON.stringify([...saved].sort())
          }
          onClick={() =>
            action.run(
              () =>
                api(
                  `/workspaces/${id}/datasets/${dataset.id}/privacy`,
                  { additionalColumns: additional },
                  "PUT",
                ),
              "Privacy saved",
            )
          }
        >
          Save privacy
        </button>
      )}
    </section>
  );
}
