import "./pipeline-jobs.css";
import { useAction, useWorkspace } from "./ui";
import { api, relative } from "./api";
import { useLiveActivity, isActive } from "./shell/live";
import { Button } from "./kit";

export function PipelineJobs() {
  const { id, role } = useWorkspace(),
    action = useAction(),
    live = useLiveActivity();
  const jobs = live.jobs.filter((job) => job.data.type === "pipeline");
  if (!jobs.length) return null;
  return (
    <section aria-label="Pipeline jobs" className="wk-pipeline-jobs">
      <h2>Pipeline jobs</h2>
      <p className="muted">
        Cancel a queued or running build to retain its previous snapshot.
        Completed builds remain published.
      </p>
      <ul>
        {jobs.map((job) => (
          <li key={job.id}>
            <span>
              <strong>{job.name}</strong>
              <small>
                {job.data.status} · {relative(job.updatedAt)}
              </small>
            </span>
            {role !== "viewer" && isActive(job) && (
              <Button
                size="sm"
                pending={action.pending}
                onClick={() =>
                  action.run(
                    () =>
                      api(
                        `/workspaces/${id}/pipeline-jobs/${job.id}/cancel`,
                        {},
                      ),
                    "Pipeline canceled",
                  )
                }
              >
                Cancel {job.name}
              </Button>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}
