// The generations and loads this browser started, for the job dock
// (./job-dock.tsx) to follow from any page until each ends and is seen:
// started by queries.ts (generateOntology, a revision that saves, createLoad),
// kept per workspace in localStorage (`foundry.jobs.watched.v1:<workspace>`),
// so another tab and a reload follow them too. A job a person dismissed, or
// whose page they opened once it ended, leaves the list; one a day old leaves
// it whatever happened. Storage that's blocked keeps nothing, and the dock
// shows nothing.

export type WatchedJob = {
  kind: "generation" | "load";
  ontologyId: string;
  // The generation's id, or the load's.
  id: string;
  // When this browser started it (ms).
  at: number;
};

/** The most a workspace follows at once: the newest. */
export const MAX_WATCHED = 3;
/** How long a job is followed at most. */
export const WATCH_MS = 24 * 3_600_000;

const key = (ws: string) => "foundry.jobs.watched.v1:" + ws;

/**
 * The jobs stored, newest first, from anything: what isn't a job, or is a
 * day old, is dropped.
 *
 * @example
 * parseWatched('[{"kind":"load","ontologyId":"o","id":"l","at":5}]', 10)
 * // [{ kind: "load", ontologyId: "o", id: "l", at: 5 }]
 * parseWatched("nope", 10) // []
 */
export function parseWatched(raw: string | null, now: number): WatchedJob[] {
  let value: unknown;
  try {
    value = JSON.parse(raw || "[]");
  } catch {
    return [];
  }
  if (!Array.isArray(value)) return [];
  return value
    .filter(
      (j): j is WatchedJob =>
        !!j &&
        typeof j === "object" &&
        (j.kind === "generation" || j.kind === "load") &&
        typeof j.ontologyId === "string" &&
        !!j.ontologyId &&
        typeof j.id === "string" &&
        !!j.id &&
        typeof j.at === "number" &&
        now - j.at < WATCH_MS,
    )
    .map(({ kind, ontologyId, id, at }) => ({ kind, ontologyId, id, at }))
    .sort((a, b) => b.at - a.at)
    .slice(0, MAX_WATCHED);
}

/**
 * The list with `job` first (once), and no more than MAX_WATCHED.
 *
 * @example
 * withJob([], job) // [job]
 */
export const withJob = (list: readonly WatchedJob[], job: WatchedJob) =>
  [job, ...list.filter((j) => !(j.kind === job.kind && j.id === job.id))].slice(
    0,
    MAX_WATCHED,
  );

/** The list without the job `kind` `id`. */
export const withoutJob = (
  list: readonly WatchedJob[],
  kind: WatchedJob["kind"],
  id: string,
) => list.filter((j) => !(j.kind === kind && j.id === id));

const listeners = new Set<() => void>();
// The last read of each workspace's list, so a snapshot is stable until it changes.
const cache = new Map<string, { raw: string | null; jobs: WatchedJob[] }>();

/** The jobs a workspace follows, newest first, as last stored. */
export function readWatched(ws: string): WatchedJob[] {
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(key(ws));
  } catch {
    // Blocked: nothing is followed.
  }
  const hit = cache.get(ws);
  if (hit && hit.raw === raw) return hit.jobs;
  const jobs = parseWatched(raw, Date.now());
  cache.set(ws, { raw, jobs });
  return jobs;
}

function write(ws: string, jobs: readonly WatchedJob[]) {
  try {
    if (jobs.length) localStorage.setItem(key(ws), JSON.stringify(jobs));
    else localStorage.removeItem(key(ws));
  } catch {
    // Blocked: nothing is kept.
  }
  for (const listener of listeners) listener();
}

/** Follows a job this browser just started. */
export const watchJob = (ws: string, job: Omit<WatchedJob, "at">) =>
  write(ws, withJob(readWatched(ws), { ...job, at: Date.now() }));

/** Stops following a job: dismissed, or seen once it ended. */
export const unwatchJob = (ws: string, kind: WatchedJob["kind"], id: string) =>
  write(ws, withoutJob(readWatched(ws), kind, id));

/**
 * Follows `next` in the place of `job`, keeping when it started: a first
 * draft's load, which its generation queued as it was stored.
 */
export const followJob = (
  ws: string,
  job: WatchedJob,
  next: Omit<WatchedJob, "at" | "ontologyId">,
) =>
  write(
    ws,
    readWatched(ws).map((j) =>
      j.kind === job.kind && j.id === job.id ? { ...j, ...next } : j,
    ),
  );

/** Calls `listener` whenever a list changes, in this tab or another. */
export function subscribeWatched(listener: () => void) {
  listeners.add(listener);
  window.addEventListener("storage", listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", listener);
  };
}

/**
 * Whether the page at `pathname` already shows `job` as it goes: the
 * ontology's overview (its setup's steps and live callouts), the job's own
 * page, and, for the sample's ontology (`sampleId`), the sample's tour. The
 * dock leaves those to the page.
 *
 * @example
 * shownOn({ kind: "load", ontologyId: "o", id: "l", at: 0 }, "/ontology/o/loads/l") // true
 * shownOn({ kind: "load", ontologyId: "o", id: "l", at: 0 }, "/data") // false
 */
export function shownOn(
  job: WatchedJob,
  pathname: string,
  sampleId?: string,
): boolean {
  const at = "/ontology/" + encodeURIComponent(job.ontologyId),
    own = encodeURIComponent(job.id);
  return (
    pathname === at ||
    (pathname === "/ontology/sample" && job.ontologyId === sampleId) ||
    pathname ===
      (job.kind === "generation"
        ? `${at}/generations/${own}`
        : `${at}/loads/${own}`)
  );
}
