import { useEffect, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { api } from "./api";
import { useWorkspace } from "./ui";
import { useSetupAction } from "./setup-components";
import { OntologyStep } from "./ontology-step";
import { BuildProgress } from "./build-progress";
import type { OnboardingState, Resource } from "../../../packages/shared/src";

const working = (r?: Resource) =>
  !!r && ["queued", "running"].includes(r.data.status);

// Datasets added after setup are not in the published ontology until an editor
// reviews the suggestion recomputed from all current data and confirms it.
// The build is code from the confirmed ontology, as in setup, and publishing it
// replaces the ontology.
export function OntologyUpdate({
  published,
  header,
  children,
}: {
  // The proposal the published ontology came from.
  published?: Resource;
  header: ReactNode;
  children: ReactNode;
}) {
  const { id, role } = useWorkspace(),
    qc = useQueryClient(),
    action = useSetupAction(),
    navigate = useNavigate();
  const [reviewing, setReviewing] = useState(false);
  const editor = role !== "viewer";
  const setup = useQuery({
    queryKey: [id, "onboarding"],
    queryFn: () => api<OnboardingState>(`/workspaces/${id}/onboarding`),
    enabled: editor,
    refetchInterval: (q) =>
      reviewing || working(q.state.data?.buildRun) ? 1500 : 15000,
  });
  const state = setup.data;
  const publishedRunId = published?.data.onboardingRunId as string | undefined;
  // The datasets the published build was given, including any whose entities
  // were left out on review; those are not new.
  const publishedRun = useQuery({
    queryKey: [id, "run", publishedRunId],
    queryFn: () =>
      api<Resource>(`/workspaces/${id}/resources/run/${publishedRunId}`),
    enabled: editor && !!publishedRunId,
  });
  const run = state?.buildRun;
  const update =
    !!run?.data.ontology && !!published && run.id !== publishedRunId;
  const updating =
    update &&
    (working(run) || ["failed", "canceled"].includes(run.data.status));
  const finished = update && run.data.status === "succeeded";
  // Once the update is published, the page shows the new ontology.
  useEffect(() => {
    if (finished) void qc.invalidateQueries({ queryKey: [id] });
  }, [finished, run?.id, id, qc]);

  const given: string[] | undefined = publishedRunId
    ? publishedRun.data?.data.datasetIds
    : published && Object.keys(published.data.validation?.inputVersions || {});
  const fresh =
    state && given ? state.datasets.filter((d) => !given.includes(d.id)) : [];

  if (!editor || !state?.complete || !published)
    return (
      <>
        {header}
        {children}
      </>
    );
  if (reviewing)
    return (
      <div className="ontology-update-review">
        <OntologyStep
          workspaceId={id}
          state={state}
          demo={{ pending: false, error: "" }}
          onRetryDemo={() => undefined}
          onConfirmed={async (next) => {
            await qc.cancelQueries({ queryKey: [id, "onboarding"] });
            qc.setQueryData([id, "onboarding"], next);
            setReviewing(false);
          }}
          onBack={() => setReviewing(false)}
          onReviewData={() => navigate("/data")}
          confirmLabel="Looks right — update ontology"
          backLabel="Back to ontology"
          fit
        />
      </div>
    );
  return (
    <>
      {header}
      {updating ? (
        <section className="panel ontology-update" aria-label="Ontology update">
          <h2>
            {working(run)
              ? "Updating your ontology"
              : run.data.status === "failed"
                ? "We couldn’t update your ontology"
                : "Ontology update stopped"}
          </h2>
          <BuildProgress workspaceId={id} run={run} action={action} />
          {!working(run) && (
            <button
              className="text-button"
              disabled={action.pending}
              onClick={() => setReviewing(true)}
            >
              Review the ontology again
            </button>
          )}
          {action.error && (
            <p role="alert" className="error-box">
              {action.error}
            </p>
          )}
        </section>
      ) : (
        fresh.length > 0 && (
          <div className="notice ontology-update" role="status">
            <p>
              {fresh.length === 1
                ? "1 new dataset isn’t in your ontology yet."
                : `${fresh.length} new datasets aren’t in your ontology yet.`}
            </p>
            <button className="button" onClick={() => setReviewing(true)}>
              <RefreshCw size={15} aria-hidden /> Update ontology
            </button>
          </div>
        )
      )}
      {children}
    </>
  );
}
