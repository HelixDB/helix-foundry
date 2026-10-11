import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { Resource } from "../../../../../packages/shared/src";
import { datasetTable } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { DataTable, useResources, useWorkspace } from "../../ui";
import { Button, Page, PageHeader, Panel, SkeletonText } from "../../kit";
import { paths } from "../../paths";

export function QueryWorkspace() {
  const { id } = useWorkspace(),
    { queryId } = useParams();
  const datasets = useResources("dataset", { limit: 500 }),
    queries = useResources("query", { limit: 500 });
  const saved = useQuery({
    queryKey: [id, "saved-query", queryId],
    enabled: !!queryId,
    queryFn: () =>
      api<Resource>(`/workspaces/${id}/resources/query/${queryId}`),
  });
  return (
    <Page>
      <PageHeader
        title="SQL queries"
        subtitle="Run read-only SQL against current accepted dataset snapshots. Save a query to revisit it after a refresh."
        breadcrumbs={[
          { label: "Data", to: paths.data() },
          { label: "SQL queries" },
        ]}
      />
      <div className="sq-workspace">
        <Panel title="Saved queries">
          <Link to={paths.queries()}>New query</Link>
          <ul>
            {queries.data?.items.map((q: Resource) => (
              <li key={q.id}>
                <Link
                  to={paths.queries(q.id)}
                  aria-current={q.id === queryId ? "page" : undefined}
                >
                  {q.name}
                </Link>
              </li>
            ))}
          </ul>
          {!queries.isPending && !queries.data?.items.length && (
            <p className="muted">No saved queries yet.</p>
          )}
        </Panel>
        {datasets.isPending || (queryId && saved.isPending) ? (
          <SkeletonText lines={6} />
        ) : saved.error ? (
          <p role="alert">{saved.error.message}</p>
        ) : (
          <QueryEditor
            key={`${id}:${queryId || "new"}`}
            datasets={datasets.data?.items || []}
            saved={saved.data}
          />
        )}
      </div>
    </Page>
  );
}
function QueryEditor({
  datasets,
  saved,
}: {
  datasets: Resource[];
  saved?: Resource;
}) {
  const { id, role } = useWorkspace(),
    navigate = useNavigate(),
    qc = useQueryClient();
  const [name, setName] = useState(saved?.name || ""),
    [inputs, setInputs] = useState<string[]>(saved?.data.inputs || []),
    [sql, setSql] = useState(saved?.data.sql || ""),
    [revision, setRevision] = useState(saved?.revision),
    [result, setResult] = useState<any>(),
    [pending, setPending] = useState(false),
    [error, setError] = useState<string>();
  const same =
    saved &&
    sql === saved.data.sql &&
    JSON.stringify(inputs) === JSON.stringify(saved.data.inputs);
  const run = async (operation: () => Promise<any>) => {
    setPending(true);
    setError(undefined);
    try {
      return await operation();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <div>
      <Panel title="Query">
        <label>
          Query name
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={120}
          />
        </label>
        <fieldset className="sq-inputs">
          <legend>Dataset inputs</legend>
          {datasets.map((d) => (
            <label key={d.id}>
              <input
                type="checkbox"
                checked={inputs.includes(d.id)}
                onChange={(e) => {
                  setInputs(
                    e.target.checked
                      ? [...inputs, d.id]
                      : inputs.filter((x) => x !== d.id),
                  );
                  setResult(undefined);
                }}
              />
              <span>
                {d.name}
                <code>{d.data.table || datasetTable(d.id)}</code>
              </span>
            </label>
          ))}
          {!datasets.length && (
            <p>Connect or upload data before running a query.</p>
          )}
        </fieldset>
        <label>
          SQL
          <textarea
            className="sq-sql"
            rows={7}
            value={sql}
            maxLength={20000}
            spellCheck={false}
            onChange={(e) => {
              setSql(e.target.value);
              setResult(undefined);
            }}
          />
        </label>
        <div className="buttons">
          <Button
            pending={pending}
            disabled={!inputs.length || !sql.trim()}
            onClick={() =>
              run(async () => {
                setResult(undefined);
                const r = await api(
                  `/workspaces/${id}` +
                    (same ? `/queries/${saved.id}/run` : "/query"),
                  same ? {} : { inputs, sql },
                );
                setResult(r);
              })
            }
          >
            Run query
          </Button>
          {role !== "viewer" && (
            <Button
              pending={pending}
              disabled={!name.trim() || !inputs.length || !sql.trim()}
              onClick={() =>
                run(async () => {
                  const q = await api<Resource>(
                    `/workspaces/${id}/queries` + (saved ? `/${saved.id}` : ""),
                    {
                      name,
                      inputs,
                      sql,
                      ...(saved ? { revision } : {}),
                    },
                    saved ? "PUT" : "POST",
                  );
                  setRevision(q.revision);
                  await qc.invalidateQueries();
                  navigate(paths.queries(q.id));
                })
              }
            >
              Save query
            </Button>
          )}
          {saved && role !== "viewer" && (
            <Button
              variant="danger"
              disabled={pending}
              onClick={() => {
                if (window.confirm(`Delete saved query “${saved.name}”?`))
                  void run(async () => {
                    await api(
                      `/workspaces/${id}/queries/${saved.id}`,
                      {},
                      "DELETE",
                    );
                    await qc.invalidateQueries();
                    navigate(paths.queries());
                  });
              }}
            >
              Delete query
            </Button>
          )}
        </div>
        {error && <p role="alert">{error}</p>}
      </Panel>
      {result && (
        <Panel title="Query result">
          <p>
            {result.profile.rows} result rows · showing up to 50 · current
            accepted snapshots
          </p>
          <DataTable rows={result.rows} />
          {!result.rows.length && <p>No matching rows.</p>}
          <details>
            <summary>Snapshot sources</summary>
            {result.citations.map((c: any) => (
              <p key={c.datasetId}>
                <Link to={paths.dataset(c.datasetId)}>
                  {datasets.find((d) => d.id === c.datasetId)?.name ||
                    c.datasetId}
                </Link>{" "}
                · <code>{c.version}</code>
              </p>
            ))}
          </details>
        </Panel>
      )}
    </div>
  );
}
