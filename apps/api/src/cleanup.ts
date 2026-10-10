import type { Resource } from "../../../packages/shared/src/index.js";
import { resource, transaction, type Store } from "./store.js";

// Every publication stages a new generation of records and links. The cleaner
// removes the generations nothing will read again: the one a publication
// replaced, those of attempts that failed or were overtaken, and leftovers
// from releases that kept them all. It works in bounded batches as a
// background job, and hands the worker back to other jobs every few seconds.
const BATCH = 1000,
  // Work per turn before the job queues itself again.
  SLICE = 15_000,
  // A staging generation nothing has written to for this long is abandoned.
  STALE = 6 * 3600_000,
  // A replaced generation stays this long for requests still reading it.
  GRACE = 10 * 60_000;

// Queues one cleaner for the workspace unless one is already waiting.
export async function queueCleanup(store: Store, scope: string) {
  await transaction(store, scope, async (tx) => {
    const pending = (await tx.list("job")).some(
      (j) =>
        j.data.type === "cleanup" &&
        ["queued", "running"].includes(j.data.status),
    );
    if (!pending)
      tx.put(
        resource(scope, "job", "Remove old workspace builds", {
          type: "cleanup",
          status: "queued",
          background: true,
        }),
      );
  });
}

// The generations to keep: the active one, the one it replaced while
// requests may still read it, and staging ones a retry may resume.
async function retained(store: Store, scope: string) {
  const workspace = await store.get(scope, "workspace"),
    active: string | null = workspace?.data.activeGeneration || null,
    keep = new Set<string>(active ? [active] : []);
  const latest = (await store.list(scope, "publication")).sort((a, b) =>
    b.createdAt.localeCompare(a.createdAt),
  )[0];
  // When the replaced generation may go.
  let graceUntil = 0;
  if (
    latest?.data.previousGeneration &&
    Date.now() - Date.parse(latest.createdAt) < GRACE
  ) {
    keep.add(latest.data.previousGeneration);
    graceUntil = Date.parse(latest.createdAt) + GRACE;
  }
  const staging = await store.list(scope, "staging");
  for (const s of staging)
    if (
      s.data.status === "staging" &&
      (s.data.baseGeneration || null) === active &&
      Date.now() - s.data.touchedAt < STALE
    )
      keep.add(s.data.generation);
  return { keep, staging, latest, graceUntil };
}

// A staging generation is marked abandoned before its records go, so a
// retry cannot resume it while it is being removed.
async function abandon(store: Store, scope: string, ids: string[]) {
  if (ids.length)
    await transaction(store, scope, async (tx) => {
      const { keep } = await retained(store, scope);
      for (const id of ids) {
        const s = await tx.get(id);
        if (s?.data.status === "staging" && !keep.has(s.data.generation))
          tx.update(s, { ...s.data, status: "abandoned" });
      }
    });
}

export async function runCleanupJob(
  store: Store,
  scope: string,
  job: Resource,
): Promise<{ again: boolean; retryIn?: number }> {
  const started = Date.now(),
    state = await store.get(scope, "cleanup_state");
  const { keep, staging, latest, graceUntil } = await retained(store, scope);
  await abandon(
    store,
    scope,
    staging
      .filter(
        (s) => s.data.status === "staging" && !keep.has(s.data.generation),
      )
      .map((s) => s.id),
  );
  // The first run after an upgrade finds every generation with records,
  // including failed attempts that no resource names. Later runs only look
  // at the generations publications and attempts recorded.
  const swept = !!state?.data.swept;
  const candidates = swept
    ? [
        ...staging
          .filter((s) => s.data.status !== "removed")
          .map((s) => s.data.generation as string),
        ...(await store.list(scope, "publication"))
          .map((p) => p.data.previousGeneration as string | null)
          .filter((g): g is string => !!g),
      ]
    : await store.generations(scope);
  const done = new Set<string>(state?.data.removed || []);
  let removed = 0;
  for (const generation of [...new Set(candidates)]) {
    if (keep.has(generation) || done.has(generation)) continue;
    for (;;) {
      if (Date.now() - started > SLICE) {
        await save(true);
        return { again: true };
      }
      // Checked again before each batch: a publication may have started
      // resuming it, or activated it, since the job began.
      if ((await retained(store, scope)).keep.has(generation)) break;
      const n = await store.dropGeneration(scope, generation, BATCH);
      removed += n;
      if (!n) {
        done.add(generation);
        break;
      }
      await new Promise((r) => setTimeout(r, 100));
    }
  }
  await save(false);
  // The generation a publication just replaced goes once its grace ends.
  return graceUntil
    ? { again: true, retryIn: graceUntil - Date.now() + 1000 }
    : { again: false };

  async function save(more: boolean) {
    await transaction(store, scope, async (tx) => {
      const s = await tx.get("cleanup_state");
      tx.put({
        ...(s ||
          resource(scope, "cleanupState", "Cleanup", {}, "cleanup_state")),
        revision: (s?.revision || 0) + 1,
        data: {
          // Only a sweep that finished covers everything.
          swept: swept || !more,
          // Bounded: older generations were checked long ago.
          removed: [...done].slice(-200),
          lastRunAt: Date.now(),
          lastPublication: latest?.id || null,
        },
      });
      for (const r of await Promise.all(staging.map((x) => tx.get(x.id))))
        if (r?.data.status === "abandoned" && done.has(r.data.generation))
          tx.update(r, { ...r.data, status: "removed" });
      const j = await tx.get(job.id);
      if (j)
        tx.update(j, {
          ...j.data,
          progress: {
            message: more
              ? `Removed ${removed.toLocaleString("en-US")} old records and links so far`
              : `Removed ${removed.toLocaleString("en-US")} old records and links`,
          },
        });
    });
  }
}
