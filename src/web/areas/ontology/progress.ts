import type {
  GraphJob,
  JobPhase,
  OntologyGeneration,
} from "../../../shared/wfe";
import { phaseLabel } from "./model";

// How far a generation or a load has got, as a bar and the time left: one
// view (`JobProgress`) that the live callouts, the generation's and the
// load's pages and the job dock (./job-dock.tsx) all draw.
//
// What each job tells:
// - a load's job says its phase, the chunks it has loaded (no total) and the
//   documents it has read of those it knows of (a total that grows set by
//   set), and nothing of when a phase began. Its bar moves by phase, each a
//   share of the whole (`PHASE_SHARE`), and through the documents phase by
//   the documents read. Its time left is the documents left at the rate
//   they were read since this browser first saw them being read
//   (`DocumentSample`), so it shows once two reads of the job are apart by a
//   document.
// - a generation says only queued, running and when it started. Its bar is
//   its time running against how long this ontology's drafts have taken
//   (`expectedDuration`: the median of its last succeeded ones of the same
//   kind), else the usual time, and holds short of the end until it ends.
//   Past that, it says it's taking longer than usual.

/** What a bar shows of a job. */
export type JobProgress = {
  /** The share done, 0 to 1; null while nothing measures it. */
  fraction: number | null;
  /** The time left, in ms; null while nothing estimates it. */
  remainingMs: number | null;
  /** Running past the time it usually takes. */
  overdue: boolean;
  /** The time since it started running, in ms (0 while queued). */
  elapsedMs: number;
  /** What it is doing, in a few words. */
  step: string;
};

/** The bar never reaches the end before the job does. */
const MAX_LIVE_FRACTION = 0.95;

/**
 * A load job's phases' shares of its bar. A load writes its tables, then
 * reads its documents with a model (most of the time); an extract or a
 * resend only reads documents.
 */
export const PHASE_SHARE: Record<
  "load" | "documents",
  Partial<Record<JobPhase, number>>
> = {
  load: { provisioning: 0.05, ingest: 0.3, documents: 0.55, finishing: 0.1 },
  documents: { provisioning: 0.05, documents: 0.85, finishing: 0.1 },
};
const ORDER: JobPhase[] = ["provisioning", "ingest", "documents", "finishing"];

/** The first time this browser saw a job reading documents, and how many were read. */
export type DocumentSample = { at: number; done: number };

const msSince = (iso: string | undefined, now: number) =>
  iso ? Math.max(0, now - Date.parse(iso)) : 0;

/**
 * A load job's bar: its phase's place, the documents read through the
 * documents phase, and the time left at the rate documents were read since
 * `first`.
 *
 * @example
 * // Half the documents read, 10 of them a minute since first seen.
 * loadProgress(
 *   { id: "j", kind: "load", status: "running", phase: "documents",
 *     progress: { documentsDone: 20, documentsTotal: 40 } },
 *   { at: 0, done: 10 },
 *   60_000,
 * ) // fraction 0.625, remainingMs 120000
 */
export function loadProgress(
  job: GraphJob,
  first: DocumentSample | undefined,
  now: number,
): JobProgress {
  const elapsedMs = msSince(job.startedAt, now),
    step = phaseLabel(job);
  if (job.status === "succeeded")
    return { fraction: 1, remainingMs: 0, overdue: false, elapsedMs, step };
  if (job.status !== "running")
    return {
      fraction: job.status === "queued" ? 0 : null,
      remainingMs: null,
      overdue: false,
      elapsedMs,
      step: job.status === "queued" ? "Waiting to start" : step,
    };
  const shares =
      PHASE_SHARE[job.kind === "load" || !job.kind ? "load" : "documents"],
    phase = job.phase || "provisioning",
    before = ORDER.slice(0, ORDER.indexOf(phase)).reduce(
      (n, p) => n + (shares[p] || 0),
      0,
    ),
    p = job.progress || {},
    done = p.documentsDone || 0,
    total = p.documentsTotal || 0,
    reading = phase === "documents" && total > 0,
    within = reading ? Math.min(1, done / total) : 0;
  const fraction = Math.min(
    MAX_LIVE_FRACTION,
    before + (shares[phase] || 0) * within,
  );
  // Documents a minute since first seen: the rest at that rate.
  const rate =
      reading && first && done > first.done && now > first.at
        ? (done - first.done) / (now - first.at)
        : 0,
    remainingMs = rate ? Math.round(Math.max(0, total - done) / rate) : null;
  return {
    fraction,
    remainingMs,
    overdue: false,
    elapsedMs,
    step: reading ? `Reading ${done} of ${total} documents` : step,
  };
}

/** A first draft's usual time, hosted and on a model on this machine. */
export const USUAL_DRAFT_MS = { hosted: 15 * 60_000, local: 20 * 60_000 };
/** An edit's preview or save's usual time. */
export const USUAL_REVISION_MS = 2 * 60_000;
/** How many of an ontology's latest drafts its median takes. */
const HISTORY = 5;

const kindOf = (g: OntologyGeneration) =>
  g.revision ? (g.revision.dryRun ? "preview" : "save") : "draft";

/**
 * How long a generation like `g` takes: the median of the latest succeeded
 * generations of its kind (a draft, an edit's preview or its save) in
 * `history`, else the usual time.
 *
 * @example
 * expectedDuration(draft, [], false) // USUAL_DRAFT_MS.hosted
 */
export function expectedDuration(
  g: OntologyGeneration,
  history: readonly OntologyGeneration[],
  local: boolean,
): number {
  const kind = kindOf(g),
    took = history
      .filter(
        (h) =>
          h.id !== g.id &&
          h.status === "ONTOLOGY_GENERATION_STATUS_SUCCEEDED" &&
          h.startedAt &&
          h.finishedAt &&
          kindOf(h) === kind,
      )
      .sort((a, b) => Date.parse(b.finishedAt!) - Date.parse(a.finishedAt!))
      .slice(0, HISTORY)
      .map((h) => Date.parse(h.finishedAt!) - Date.parse(h.startedAt!))
      .filter((ms) => ms > 0)
      .sort((a, b) => a - b);
  if (!took.length)
    return kind === "draft"
      ? local
        ? USUAL_DRAFT_MS.local
        : USUAL_DRAFT_MS.hosted
      : USUAL_REVISION_MS;
  const mid = took.length >> 1;
  return took.length % 2 ? took[mid]! : (took[mid - 1]! + took[mid]!) / 2;
}

/**
 * A generation's bar: its time running against `expectedMs`.
 *
 * @example
 * // Five minutes into a draft that takes fifteen.
 * generationProgress(running, 15 * 60_000, start + 5 * 60_000)
 * // fraction 1/3, remainingMs 600000
 */
export function generationProgress(
  g: OntologyGeneration,
  expectedMs: number,
  now: number,
): JobProgress {
  const elapsedMs = msSince(g.startedAt, now);
  switch (g.status) {
    case "ONTOLOGY_GENERATION_STATUS_SUCCEEDED":
      return {
        fraction: 1,
        remainingMs: 0,
        overdue: false,
        elapsedMs: msSince(g.startedAt, Date.parse(g.finishedAt || "") || now),
        step: "Done",
      };
    case "ONTOLOGY_GENERATION_STATUS_RUNNING": {
      const overdue = elapsedMs > expectedMs;
      return {
        fraction: Math.min(MAX_LIVE_FRACTION, elapsedMs / expectedMs),
        remainingMs: overdue ? null : expectedMs - elapsedMs,
        overdue,
        elapsedMs,
        step: g.revision
          ? g.revision.dryRun
            ? "Checking the edits"
            : "Creating the edited version"
          : "Drafting types, links and mappings",
      };
    }
    case "ONTOLOGY_GENERATION_STATUS_QUEUED":
      return {
        fraction: 0,
        remainingMs: null,
        overdue: false,
        elapsedMs: 0,
        step: (g.attempts || 0) > 0 ? "Waiting to retry" : "Waiting to start",
      };
    default:
      return {
        fraction: null,
        remainingMs: null,
        overdue: false,
        elapsedMs,
        step: "Stopped",
      };
  }
}

/**
 * A time left in words, rounded up to the minute past a minute.
 *
 * @example
 * timeLeft(30_000) // "Less than a minute left"
 * timeLeft(61_000) // "About 2 min left"
 * timeLeft(75 * 60_000) // "About 1 h 15 min left"
 */
export function timeLeft(ms: number): string {
  if (ms < 60_000) return "Less than a minute left";
  const min = Math.ceil(ms / 60_000);
  if (min < 60) return `About ${min} min left`;
  const h = Math.floor(min / 60),
    rest = min % 60;
  return `About ${h} h${rest ? ` ${rest} min` : ""} left`;
}

/**
 * A time so far in words.
 *
 * @example
 * timeSoFar(42_000) // "42s so far"
 * timeSoFar(185_000) // "3 min so far"
 */
export function timeSoFar(ms: number): string {
  const s = Math.floor(ms / 1000);
  return s < 60 ? `${s}s so far` : `${Math.floor(s / 60)} min so far`;
}

/** The line under a bar: the time left, else that it's late, else the time so far. */
export function progressLine(p: JobProgress): string {
  if (p.remainingMs !== null && p.fraction !== 1)
    return timeLeft(p.remainingMs);
  if (p.overdue) return `Taking longer than usual · ${timeSoFar(p.elapsedMs)}`;
  return p.elapsedMs ? timeSoFar(p.elapsedMs) : "";
}
