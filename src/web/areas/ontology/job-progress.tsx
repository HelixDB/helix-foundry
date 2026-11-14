import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { LOCAL } from "../../../shared/deployment";
import type { GraphJob, OntologyGeneration } from "../../../shared/wfe";
import { cx, useNow } from "../../kit";
import { ontologyQueries } from "./queries";
import {
  expectedDuration,
  generationProgress,
  loadProgress,
  progressLine,
  type DocumentSample,
  type JobProgress,
} from "./progress";

// A job's bar (./progress.ts says what moves it): the step it is at, a
// bar, and the time left or so far. Drawn by the live callouts, the
// generation's and the load's pages and the job dock.

// When this browser first saw each job reading documents, for the rate
// its time left goes by. Kept for the visit, so every view of a job agrees.
const samples = new Map<string, DocumentSample>();

// How often a live bar moves: its words change by the minute at most.
const TICK_MS = 5000;

/** A load job's bar, moving while there is a job; null without one. */
export function useLoadJobProgress(job: GraphJob | null | undefined) {
  const now = useNow(job ? TICK_MS : 60_000),
    p = job?.progress,
    reading =
      job?.status === "running" &&
      job.phase === "documents" &&
      !!p?.documentsTotal;
  useEffect(() => {
    if (job && reading && !samples.has(job.id))
      samples.set(job.id, { at: Date.now(), done: p?.documentsDone || 0 });
  }, [job?.id, reading]);
  return job ? loadProgress(job, samples.get(job.id), now) : null;
}

/**
 * A generation's bar, against how long the ontology's earlier generations
 * of its kind took (its generations list, which pages poll while one is
 * live); null without a generation.
 */
export function useGenerationProgress(
  ws: string,
  ontologyId: string,
  g: OntologyGeneration | null | undefined,
) {
  const now = useNow(g ? TICK_MS : 60_000),
    history = useQuery({
      ...ontologyQueries.generations(ws, ontologyId),
      enabled: !!g,
    });
  return g
    ? generationProgress(
        g,
        expectedDuration(g, history.data?.generations || [], LOCAL),
        now,
      )
    : null;
}

/** The bar: its step, then the time left or so far. */
export function ProgressBar({
  progress,
  label,
  className,
}: {
  progress: JobProgress;
  // What is in progress, for screen readers ("Drafting Sales graph").
  label: string;
  className?: string;
}) {
  const pct =
      progress.fraction === null
        ? undefined
        : Math.round(progress.fraction * 100),
    line = progressLine(progress);
  return (
    <div className={cx("on-progress", className)}>
      <div
        className="on-progress-track"
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
        aria-valuetext={[progress.step, line].filter(Boolean).join(". ")}
      >
        <div
          className={cx(
            "on-progress-fill",
            pct === undefined && "is-indeterminate",
          )}
          style={
            pct === undefined ? undefined : { width: `${Math.max(pct, 2)}%` }
          }
        />
      </div>
      <p className="on-progress-line">
        <span>{progress.step}</span>
        {line && <span className="on-progress-time">{line}</span>}
      </p>
    </div>
  );
}
