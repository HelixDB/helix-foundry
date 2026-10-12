import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { Resource } from "../../../../../packages/shared/src";
import { api } from "../../api";
import { paths } from "../../paths";
import { useResources, useWorkspace } from "../../ui";
import { Page, PageHeader } from "../../kit";

export function PipelineWorkspace() {
  const { id } = useWorkspace(),
    { pipelineId } = useParams();
  const pipelines = useResources("pipeline", { limit: 500 });
  const selected = pipelines.data?.items.find(
    (p: Resource) => p.id === pipelineId,
  );
  return (
    <Page>
      <PageHeader
        title="Pipelines"
        meta={<Link to={paths.data()}>Data</Link>}
      />
      <p className="muted">
        Create SQL transforms as tested proposals. Publishing creates new
        immutable pipeline definitions; run an accepted pipeline to refresh its
        output.
      </p>
      <div className="pipeline-workspace">
        <nav aria-label="Accepted pipelines">
          <Link className="button" to={paths.pipelines()}>
            New pipeline
          </Link>
          {pipelines.error && <p role="alert">{pipelines.error.message}</p>}
          {pipelines.data?.items.map((p: Resource) => (
            <p key={p.id}>
              <Link to={paths.pipelines(p.id)}>{p.name}</Link>
            </p>
          ))}
        </nav>
        {pipelineId && pipelines.isPending ? (
          <p>Loading pipeline…</p>
        ) : pipelineId && !selected ? (
          <p role="alert">Accepted pipeline unavailable.</p>
        ) : (
          <PipelineEditor
            key={id + ":" + (pipelineId || "new")}
            pipeline={selected}
          />
        )}
      </div>
    </Page>
  );
}

function PipelineEditor({ pipeline }: { pipeline?: Resource }) {
  const { id, role } = useWorkspace(),
    navigate = useNavigate();
  const datasets = useResources("dataset", { limit: 500 });
  const [name, setName] = useState(pipeline?.name || ""),
    [description, setDescription] = useState(pipeline?.data.description || ""),
    [inputs, setInputs] = useState<string[]>(pipeline?.data.inputs || []),
    [sql, setSql] = useState(pipeline?.data.sql || ""),
    [pending, setPending] = useState(false),
    [error, setError] = useState(""),
    [jobId, setJobId] = useState<string>();
  const job = useQuery({
    queryKey: [id, "pipeline-job", jobId],
    enabled: !!jobId,
    queryFn: () => api(`/workspaces/${id}/resources/job/${jobId}`),
    refetchInterval: (q) =>
      ["queued", "running"].includes(q.state.data?.data.status) ? 1000 : false,
  });
  const currentOutput = useQuery({
    queryKey: [id, "pipeline-output", pipeline?.id, job.data?.data.status],
    enabled: !!pipeline,
    queryFn: () =>
      api(
        `/workspaces/${id}/resources/dataset/${pipeline!.data.outputDatasetId}`,
      ),
  });
  const perform = async (run: () => Promise<void>) => {
    setPending(true);
    setError("");
    try {
      await run();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setPending(false);
    }
  };
  return (
    <section aria-label="Pipeline editor">
      <label>
        Pipeline name
        <input
          value={name}
          disabled={role === "viewer"}
          onChange={(e) => setName(e.target.value)}
        />
      </label>
      <label>
        Description
        <input
          value={description}
          disabled={role === "viewer"}
          onChange={(e) => setDescription(e.target.value)}
        />
      </label>
      <fieldset>
        <legend>Dataset inputs</legend>
        {datasets.error && <p role="alert">{datasets.error.message}</p>}
        {datasets.data?.items.map((d: Resource) => (
          <label key={d.id} className="pipeline-input">
            <input
              type="checkbox"
              checked={inputs.includes(d.id)}
              disabled={role === "viewer"}
              onChange={(e) =>
                setInputs(
                  e.target.checked
                    ? [...inputs, d.id]
                    : inputs.filter((x) => x !== d.id),
                )
              }
            />
            {d.name}
            <code>{d.data.table}</code>
          </label>
        ))}
      </fieldset>
      <label>
        SQL
        <textarea
          rows={10}
          value={sql}
          disabled={role === "viewer"}
          onChange={(e) => setSql(e.target.value)}
          spellCheck={false}
        />
      </label>
      {pipeline?.data.nodes?.length > 0 && (
        <p className="muted">
          This review uses the SQL shown here. Saving a review replaces the node
          graph with this explicit SQL definition.
        </p>
      )}
      {role !== "viewer" && (
        <button
          disabled={pending || !name.trim() || !sql.trim() || !inputs.length}
          onClick={() =>
            perform(async () => {
              const proposal = await api(`/workspaces/${id}/pipelines/draft`, {
                name: name.trim(),
                description,
                inputs,
                sql,
                nodes: [],
                checks: pipeline?.data.checks || [],
              });
              navigate(paths.proposals(proposal.id));
            })
          }
        >
          Create review
        </button>
      )}
      {error && <p role="alert">{error}</p>}
      {pipeline && (
        <section aria-label="Accepted pipeline output">
          <h3>Accepted output</h3>
          <p>
            Run uses the accepted definition, including when the editor above
            has unsaved changes.
          </p>
          {role !== "viewer" && (
            <button
              disabled={
                pending || ["queued", "running"].includes(job.data?.data.status)
              }
              onClick={() =>
                perform(async () => {
                  const next = await api(
                    `/workspaces/${id}/pipelines/${pipeline.id}/run`,
                    {},
                  );
                  setJobId(next.id);
                })
              }
            >
              Run accepted pipeline
            </button>
          )}
          {job.error && <p role="alert">{job.error.message}</p>}
          {job.data && (
            <p role="status">
              Run {job.data.id}: {job.data.data.status}
              {job.data.data.error ? " · " + job.data.data.error : ""}
            </p>
          )}
          {currentOutput.error && (
            <p role="alert">{currentOutput.error.message}</p>
          )}
          {currentOutput.data && (
            <>
              <p>
                {currentOutput.data.data.profile.rows} output rows · snapshot{" "}
                <code>{currentOutput.data.data.activeVersion}</code>
              </p>
              <Link to={paths.dataset(currentOutput.data.id)}>
                Inspect output dataset
              </Link>
            </>
          )}
        </section>
      )}
    </section>
  );
}
