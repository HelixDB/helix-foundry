import { useEffect, useRef, useState } from "react";
import { Link, Navigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowRight } from "lucide-react";
import { api } from "./api";
import { paths } from "./paths";
import { useWorkspace, Loading } from "./ui";
import {
  ProviderSetup,
  OperationProgress,
  useSetup,
  useSetupAction,
} from "./setup-components";
import { ConnectData } from "./connect-data";
import { OntologyStep, datasetSource, entityNames } from "./ontology-step";
import { AiWorkingScene } from "./ontology-scene";
import type {
  Resource,
  OnboardingUpdate,
  OnboardingState,
  ProviderSettings,
} from "../../../packages/shared/src";
import { sampleSources } from "../../../packages/shared/src";
const steps = [
  ["ai", "AI"],
  ["data", "Data"],
  ["outcome", "Ontology"],
] as const;
// Plain words for the build's stages: a short label and what is happening.
const buildStage = (stage = ""): [string, string] | undefined =>
  stage.startsWith("dashboards_repair")
    ? ["Fixing analytics", "AI is fixing charts whose queries failed"]
    : (
        {
          queued: ["Waiting to start", "Your build starts in a moment"],
          ontology: [
            "Building records",
            "Turning the confirmed ontology into records and relationships",
          ],
          dashboards: [
            "Preparing analytics",
            "AI is choosing charts for your data",
          ],
          validation: [
            "Checking queries",
            "Running every query against your data",
          ],
          publishing: ["Publishing", "Saving records and relationships"],
          failed: ["Couldn’t finish", ""],
        } as Record<string, [string, string]>
      )[stage];
export default function Onboarding() {
  const { id, role } = useWorkspace(),
    q = useSetup(),
    action = useSetupAction(),
    qc = useQueryClient();
  const [sampleLoading, setSampleLoading] = useState(false);
  const [demo, setDemo] = useState({ pending: false, error: "" });
  const demoRequest = useRef(false);
  const state = q.data;
  const [displayStep, setDisplayStep] = useState<
    OnboardingState["step"] | null
  >(null);
  const [providerSaving, setProviderSaving] = useState(false);
  const [providerError, setProviderError] = useState("");
  const providerDraft = useRef<ProviderSettings | undefined>(undefined);
  const providerRequest = useRef(false);
  const step = displayStep || state?.step || "ai";
  // The server's "review" step is the build that follows a confirmed ontology,
  // shown as part of the Ontology step.
  const current = step === "review" ? "outcome" : step;
  // Each step starts at the top; the sample button sits at the end of Data.
  useEffect(() => {
    window.scrollTo(0, 0);
  }, [step]);
  const update = (body: OnboardingUpdate) =>
    api<OnboardingState>(`/workspaces/${id}/onboarding`, body, "PATCH");
  const applyState = async (next: OnboardingState) => {
    await qc.cancelQueries({ queryKey: [id, "onboarding"] });
    qc.setQueryData([id, "onboarding"], next);
    setDisplayStep(null);
  };
  const go = (next: OnboardingState["step"]) => {
    setDisplayStep(next);
    return action.run(async () => {
      try {
        await applyState(await update({ step: next }));
      } catch (error) {
        setDisplayStep(null);
        throw error;
      }
    });
  };
  const finishAI = async (settings?: ProviderSettings) => {
    if (providerRequest.current) return;
    providerRequest.current = true;
    providerDraft.current = settings;
    setProviderSaving(true);
    setProviderError("");
    setDisplayStep("data");
    try {
      if (settings)
        await api(
          `/workspaces/${id}/onboarding/provider`,
          { ...settings, advanceToData: true },
          "POST",
          undefined,
          { keepalive: true },
        );
      else await update({ step: "data" });
      await applyState(
        await api<OnboardingState>(`/workspaces/${id}/onboarding`),
      );
      providerDraft.current = undefined;
      void qc.invalidateQueries({ queryKey: [id, "provider"] });
    } catch (error) {
      setProviderError((error as Error).message);
    } finally {
      providerRequest.current = false;
      setProviderSaving(false);
    }
  };
  if (q.isPending) return <Loading />;
  if (q.error && !state)
    return (
      <div className="setup-page">
        <p role="alert">{q.error.message}</p>
        <button className="button" onClick={() => q.refetch()}>
          Retry
        </button>
      </div>
    );
  if (!state) return null;
  // A finished setup opens Home, where the key metrics are.
  if (state.complete) return <Navigate replace to={paths.home()} />;
  if (role === "viewer")
    return (
      <div className="setup-page">
        <h1>Your workspace is being prepared</h1>
        <p>An editor can finish setup. You can inspect the available data.</p>
        <Link className="button" to="/data">
          Open data
        </Link>
      </div>
    );
  const index = steps.findIndex((s) => s[0] === current),
    run = state.buildRun,
    building = !!run && ["queued", "running"].includes(run.data.status);
  // A build left waiting for the former review page shows the suggestion
  // again; confirming it finishes the build.
  const buildView =
    step === "review" &&
    state.ontologyConfirmed &&
    !!run?.data.ontology &&
    run.data.status !== "awaiting_review";
  const stage = buildStage(run?.data.stage);
  const importAction = (operation: Resource, verb: string) =>
    action.run(() =>
      api(`/workspaces/${id}/onboarding/imports/${operation.id}/${verb}`, {}),
    );
  // Samples load in the background (the server moves setup to Ontology in its
  // first transaction), so the Ontology page can show them arriving.
  const runDemo = () => {
    if (demoRequest.current) return;
    demoRequest.current = true;
    setDemo({ pending: true, error: "" });
    api(`/workspaces/${id}/demo`, { advance: true })
      .then(
        () => "",
        (error: Error) => error.message,
      )
      .then(async (error) => {
        await qc.invalidateQueries({ queryKey: [id, "onboarding"] });
        demoRequest.current = false;
        setDemo({ pending: false, error });
      });
  };
  // Highlight the sources the sample imitates, then continue to the Ontology.
  const continueWithSamples = async () => {
    setSampleLoading(true);
    runDemo();
    await new Promise((resolve) => requestAnimationFrame(resolve));
    // On a phone the button sits below every card, so the highlight would happen
    // off-screen: bring the cards into view and hold it a little longer.
    const first = document.querySelector(".connection-card.highlighted");
    const scrolled = !!first && first.getBoundingClientRect().top < 0;
    if (scrolled) first.scrollIntoView({ behavior: "smooth", block: "start" });
    await new Promise((resolve) => setTimeout(resolve, scrolled ? 1300 : 1000));
    setSampleLoading(false);
    setDisplayStep("outcome");
    void qc.invalidateQueries({ queryKey: [id, "onboarding"] });
  };
  // Samples keep loading if the user comes back to Data before they finish.
  const progress = state.sampleProgress;
  const samplesLoading = sampleLoading || demo.pending || !!progress;
  return (
    <div className="setup-page">
      <div className="setup-header-space" aria-hidden="true" />
      <nav className="setup-steps" aria-label="Setup progress">
        {steps.map(([value, label], i) => (
          <span
            key={value}
            aria-current={current === value ? "step" : undefined}
          >
            <span className="step-number">{i + 1}</span>
            {label}
          </span>
        ))}
      </nav>
      <div className="setup-content">
        {step === "ai" && (
          <>
            <h1>A little help with your data</h1>
            <p className="setup-intro">
              Choose where AI runs. It will help you understand your data and
              build your workspace.
            </p>
            <ProviderSetup onContinue={finishAI} />
          </>
        )}
        {step === "data" && (
          <>
            <h1>Where is your data?</h1>
            <p className="setup-intro">
              Choose a provider to connect your account.
            </p>
            {providerError && (
              <div className="error-box" role="alert">
                <p>
                  We couldn’t save your AI choice. You can keep connecting data.
                </p>
                <p>{providerError}</p>
                <button
                  className="text-button"
                  disabled={providerSaving}
                  onClick={() => finishAI(providerDraft.current)}
                >
                  Retry
                </button>
                <button className="text-button" onClick={() => go("ai")}>
                  Change AI choice
                </button>
              </div>
            )}
            <ConnectData highlight={samplesLoading ? sampleSources : []} />
            <button
              className="text-button sample-option"
              disabled={
                action.pending ||
                samplesLoading ||
                providerSaving ||
                !!providerError
              }
              onClick={continueWithSamples}
            >
              {!samplesLoading
                ? "Continue with sample data"
                : progress && !sampleLoading
                  ? `Loading sample data… (${progress.loaded} of ${progress.total})`
                  : "Loading sample data…"}
            </button>
            <p className="sr-only" role="status">
              {sampleLoading
                ? "Loading sample data from WorkOS, PostHog, PlanetScale, Stripe, and blob storage."
                : ""}
            </p>
            <div className="setup-actions">
              <button
                className="button primary"
                disabled={
                  !state.inputsReady ||
                  action.pending ||
                  providerSaving ||
                  !!providerError
                }
                onClick={() => go("outcome")}
              >
                Continue <ArrowRight size={15} />
              </button>
            </div>
          </>
        )}
        {buildView ? (
          <>
            <h1>
              {run.data.status === "failed"
                ? "We couldn’t build your workspace"
                : run.data.status === "canceled"
                  ? "Build stopped"
                  : "Building your workspace"}
            </h1>
            {building && (
              // Continues the ontology scene: the ontology now feeds the apps.
              <div className="build-scene">
                <AiWorkingScene
                  sources={[
                    ...new Set(
                      state.datasets.map((d) =>
                        datasetSource(d.name, d, state.sources),
                      ),
                    ),
                  ]}
                  entities={entityNames(run.data.ontology)}
                  stage="describing"
                  label="AI is building your workspace: records from the confirmed ontology, then analytics, workflows and integrations."
                />
              </div>
            )}
            <OperationProgress
              operation={{
                ...run,
                name: "Records",
                data: {
                  ...run.data,
                  stage: stage?.[0] || run.data.stage,
                  // Publishing also counts the records and relationships it
                  // has read.
                  progress: stage?.[1]
                    ? {
                        message: stage[1],
                        ...(run.data.stage === "publishing" && {
                          records: run.data.progress?.records,
                          relationships: run.data.progress?.relationships,
                        }),
                      }
                    : undefined,
                },
              }}
              onRetry={() =>
                action.run(() =>
                  api(`/workspaces/${id}/assistant/runs/${run.id}/resume`, {}),
                )
              }
              // Publication finishes once it starts.
              onCancel={
                run.data.stage === "publishing"
                  ? undefined
                  : () =>
                      action.run(() =>
                        api(
                          `/workspaces/${id}/assistant/runs/${run.id}/cancel`,
                          {},
                        ),
                      )
              }
            />
            {!building && (
              <button
                className="text-button setup-back"
                disabled={action.pending}
                onClick={() => go("outcome")}
              >
                ← Back
              </button>
            )}
          </>
        ) : (
          current === "outcome" && (
            <OntologyStep
              workspaceId={id}
              state={state}
              demo={demo}
              onRetryDemo={runDemo}
              onConfirmed={applyState}
              onBack={() => go("data")}
              onReviewData={() => go("data")}
            />
          )
        )}
        {action.error && (
          <p role="alert" className="error-box">
            {action.error}
          </p>
        )}
        {step !== "ai" &&
          !providerError &&
          ["failed", "canceled"].includes(state.providerJob?.data.status) && (
            <div className="notice" role="alert">
              <p>
                {state.providerJob?.data.error || "AI isn’t connected."} Setup
                still works without it; AI only adds descriptions.
              </p>
              {role === "owner" ? (
                <div className="buttons">
                  <button
                    className="text-button"
                    disabled={action.pending}
                    onClick={() => importAction(state.providerJob!, "retry")}
                  >
                    Retry AI connection
                  </button>
                  <button className="text-button" onClick={() => go("ai")}>
                    Change AI choice
                  </button>
                </div>
              ) : (
                <p>A workspace owner can check the AI connection.</p>
              )}
            </div>
          )}
        {index > 0 && current !== "outcome" && (
          <button
            className="text-button setup-back"
            disabled={action.pending || providerSaving}
            onClick={() => go(steps[index - 1][0])}
          >
            ← Back
          </button>
        )}
      </div>
    </div>
  );
}
