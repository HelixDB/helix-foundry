import { useEffect, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import {
  AiWorkingScene,
  DataFeed,
  type FeedItem,
  type FeedStep,
  type SceneStage,
} from "./ontology-scene";
import { SuggestedOntologyView } from "./suggested-ontology";
import type {
  OnboardingState,
  Resource,
  SuggestedOntology,
} from "../../../packages/shared/src";
import { sampleSources } from "../../../packages/shared/src";

// Sample sources in the order their tables load, a few tables at a time.
const sampleOrder = ["planetscale", "workos", "posthog", "stripe", "s3"];
const SAMPLE_LOADS = 3;
// Discovery takes well under a second; each stage is held long enough to read.
const READ = 1000,
  KEYS = 700,
  MATCH = 800,
  PROPOSE = 1200,
  SETTLE = 700,
  MIN_TOTAL = 3500,
  DESCRIBE_WAIT = 20000,
  // Longer than any one sample takes, so a resume never races a live load.
  STALLED = 12000;

const working = (r?: Resource) =>
  !!r && ["queued", "running"].includes(r.data.status);

// Sample tables are only tagged with their source after they load, so the
// "<System> · table" name stands in until then.
export function datasetSource(
  name: string,
  d?: Resource,
  sources: Resource[] = [],
) {
  const prefix = name.includes(" · ") ? name.split(" · ")[0].toLowerCase() : "";
  const source = sources.find((s) => s.id === d?.data.sourceId);
  return (
    d?.data.sampleSource ||
    ((sampleSources as readonly string[]).includes(prefix) && prefix) ||
    source?.data.hostedProvider ||
    source?.data.kind ||
    "upload"
  );
}

// Most connected first: the scene puts the first entity at its hub.
export const entityNames = (o: SuggestedOntology) =>
  o.entities
    .map((e) => ({
      name: e.name,
      degree: o.links.filter((l) => l.from === e.id || l.to === e.id).length,
    }))
    .sort((a, b) => b.degree - a.degree)
    .map((e) => e.name);

export function OntologyStep({
  workspaceId: id,
  state,
  demo,
  onRetryDemo,
  onConfirmed,
  onBack,
  onReviewData,
  confirmLabel,
  backLabel,
  fit,
}: {
  workspaceId: string;
  state: OnboardingState;
  demo: { pending: boolean; error: string };
  onRetryDemo: () => void;
  onConfirmed: (next: OnboardingState) => Promise<void>;
  onBack: () => void;
  onReviewData: () => void;
  confirmLabel?: string;
  backLabel?: string;
  fit?: boolean;
}) {
  const qc = useQueryClient();
  const { ontology, sampleProgress, signature } = state;
  const samplesLoading = !!sampleProgress || demo.pending;
  const readReady = state.inputsReady && !samplesLoading;
  const run =
    state.ontologyRun?.data.onboardingSignature === signature
      ? state.ontologyRun
      : undefined;
  const describing = !!ontology && !ontology.described && working(run);
  // Returning to this step, or reloading it, shows an existing suggestion at once.
  const [revealed, setRevealed] = useState(() => !!ontology);
  const [arrived, setArrived] = useState(false);
  const [now, setNow] = useState(() => Date.now());
  const marks = useRef<{
    start: number;
    read?: number;
    found?: number;
    done?: number;
  }>({ start: Date.now() });
  const [suggestError, setSuggestError] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [confirmError, setConfirmError] = useState("");

  const showing = revealed && !!ontology;
  useEffect(() => {
    if (showing) return;
    const timer = setInterval(() => setNow(Date.now()), 200);
    return () => clearInterval(timer);
  }, [showing]);

  const requested = useRef("");
  const wantsDescription =
    !!ontology && state.providerReady && !ontology.described && !run;
  useEffect(() => {
    if (!readReady || suggestError || (ontology && !wantsDescription)) return;
    // Asked again once AI is ready, so the suggestion gets its descriptions.
    const key = `${signature}:${state.providerReady}`;
    if (requested.current === key) return;
    requested.current = key;
    api<OnboardingState>(`/workspaces/${id}/onboarding/ontology`, {})
      .then(async (next) => {
        await qc.cancelQueries({ queryKey: [id, "onboarding"] });
        qc.setQueryData([id, "onboarding"], next);
      })
      .catch((error: Error) => {
        requested.current = "";
        setSuggestError(error.message);
      });
  }, [
    id,
    qc,
    readReady,
    ontology,
    wantsDescription,
    signature,
    state.providerReady,
    suggestError,
  ]);

  // A load that stops advancing (e.g. the server restarted mid-way) resumes;
  // /demo is idempotent, but a second request racing a live one doubles the work.
  const retryDemo = useRef(onRetryDemo);
  retryDemo.current = onRetryDemo;
  const resumed = useRef(false);
  const loaded = sampleProgress?.loaded;
  useEffect(() => {
    if (loaded === undefined || demo.pending || demo.error || resumed.current)
      return;
    const timer = setTimeout(() => {
      resumed.current = true;
      retryDemo.current();
    }, STALLED);
    return () => clearTimeout(timer);
  }, [loaded, demo.pending, demo.error]);

  const m = marks.current;
  if (!readReady) m.read = undefined;
  else m.read ??= now;
  if (!ontology) m.found = undefined;
  else m.found ??= now;
  const past = (t?: number) => t !== undefined && now >= t;
  const readAt =
      m.read === undefined ? undefined : Math.max(m.read, m.start + READ),
    keysAt = readAt === undefined ? undefined : readAt + KEYS,
    matchAt =
      keysAt === undefined || m.found === undefined
        ? undefined
        : Math.max(keysAt + MATCH, m.found),
    proposeAt = matchAt === undefined ? undefined : matchAt + PROPOSE;
  const stage: SceneStage = !past(readAt)
    ? "reading"
    : !past(matchAt)
      ? "matching"
      : !past(proposeAt)
        ? "proposing"
        : describing && now < m.found! + DESCRIBE_WAIT
          ? "describing"
          : "done";
  if (stage !== "done") m.done = undefined;
  else m.done ??= now;
  const ready =
    !!ontology &&
    m.done !== undefined &&
    now >= Math.max(m.done + SETTLE, m.start + MIN_TOTAL);
  // The suggestion replaces the scene: start it from the top, where its heading is.
  useEffect(() => {
    if (!ready || revealed) return;
    setRevealed(true);
    setArrived(true);
    if (window.scrollY > 0) {
      window.scrollTo({ top: 0, behavior: "smooth" });
    }
  }, [ready, revealed]);

  // Building from a confirmed ontology is code, so it doesn't wait for AI.
  const confirm = async (choice: {
    excluded: string[];
    names: Record<string, string>;
  }) => {
    if (!ontology) return;
    setConfirming(true);
    setConfirmError("");
    try {
      await onConfirmed(
        await api<OnboardingState>(
          `/workspaces/${id}/onboarding/ontology/confirm`,
          { hash: ontology.hash, ...choice },
        ),
      );
    } catch (error) {
      setConfirmError((error as Error).message);
      void qc.invalidateQueries({ queryKey: [id, "onboarding"] });
    } finally {
      setConfirming(false);
    }
  };

  if (showing)
    return (
      <SuggestedOntologyView
        key={ontology.hash}
        ontology={ontology}
        datasets={state.datasets}
        describing={describing}
        busy={confirming}
        error={confirmError}
        edits={state.ontologyEdits}
        focusHeading={arrived}
        onConfirm={confirm}
        onBack={onBack}
        confirmLabel={confirmLabel}
        backLabel={backLabel}
        fit={fit}
      />
    );

  const pending = sampleProgress?.pending || [];
  const loading = new Set(pending.slice(0, SAMPLE_LOADS)),
    pendingSet = new Set(pending);
  const status = (name: string): FeedItem["status"] =>
    loading.has(name) ? "loading" : pendingSet.has(name) ? "pending" : "ready";
  const known = new Set(state.datasets.map((d) => d.name));
  const rank = (source: string) => {
    const i = sampleOrder.indexOf(source);
    return i < 0 ? sampleOrder.length : i;
  };
  const items: FeedItem[] = [
    ...[...state.datasets]
      .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
      .map((d) => ({
        id: d.id,
        name: d.name,
        source: datasetSource(d.name, d, state.sources),
        rows: d.data.profile?.rows,
        status: status(d.name),
      })),
    ...pending
      .filter((name) => !known.has(name))
      .map((name) => ({
        id: "pending:" + name,
        name,
        source: datasetSource(name),
        status: status(name),
      })),
  ].sort((a, b) => rank(a.source) - rank(b.source));
  // The whole sample set is shown from the start so the scene does not re-lay out.
  const sources = [
    ...new Set([
      ...(samplesLoading ? sampleOrder : []),
      ...items.map((i) => i.source),
    ]),
  ];
  const read = !items.length
    ? "Reading your tables"
    : `Reading ${items.length} ${items.length === 1 ? "table" : "tables"} from ${sources.length} ${sources.length === 1 ? "source" : "sources"}`;
  const entities =
    ontology && ["proposing", "describing", "done"].includes(stage)
      ? entityNames(ontology)
      : [];
  const step = (label: string, from?: number, to?: number): FeedStep => ({
    label,
    status: past(to) ? "done" : past(from) ? "active" : "pending",
  });
  const steps: FeedStep[] = [
    step(read, m.start, readAt),
    step("Finding keys", readAt, keysAt),
    step("Matching records across sources", keysAt, matchAt),
    step("Proposing entities", matchAt, proposeAt),
    ...(state.providerReady || run
      ? [
          {
            label: "AI is writing descriptions",
            status: !past(proposeAt)
              ? ("pending" as const)
              : describing
                ? ("active" as const)
                : ("done" as const),
          },
        ]
      : []),
  ];
  return (
    <>
      <h1>Understanding your data</h1>
      {demo.error && (
        <div className="error-box setup-alert" role="alert">
          <p>We couldn’t finish loading the sample data.</p>
          <p>{demo.error}</p>
          <button className="text-button" onClick={onRetryDemo}>
            Retry
          </button>
        </div>
      )}
      {suggestError && (
        <div className="error-box setup-alert" role="alert">
          <p>We couldn’t work out how your data connects.</p>
          <p>{suggestError}</p>
          <button className="text-button" onClick={() => setSuggestError("")}>
            Retry
          </button>
        </div>
      )}
      {!state.inputsReady && !samplesLoading && !demo.error && (
        <div className="error-box setup-alert" role="alert">
          {state.blockers.map((b) => (
            <p key={b}>{b}</p>
          ))}
          <button className="text-button" onClick={onReviewData}>
            Review data
          </button>
        </div>
      )}
      <div className="ontology-loading">
        <AiWorkingScene sources={sources} entities={entities} stage={stage} />
        <DataFeed items={items} steps={steps} />
      </div>
      <button className="text-button setup-back" onClick={onBack}>
        ← Back
      </button>
    </>
  );
}
