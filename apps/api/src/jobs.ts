import type { Resource } from "../../../packages/shared/src/index.js";
import { prepareProvider } from "./setup-jobs.js";
import {
  startChangeStreams,
  scheduledRefresh,
  needsChangeRefresh,
  nextRefresh,
} from "./source-sync.js";
import { transaction, resource, type Store } from "./store.js";
import { buildWorkspace, providerSettings, enqueue } from "./assistant.js";
import { assert } from "./security.js";
import { runSyncJob, runUploadJob } from "./data-jobs.js";
import { runPipelineJob } from "./pipeline-jobs.js";
// Queue, leases and retries live here; each job type's work lives with its area.
const jobHandlers: Record<
  string,
  (store: Store, scope: string, job: Resource) => Promise<unknown>
> = {
  provider_prepare: prepareProvider,
  upload: runUploadJob,
  sync: runSyncJob,
  pipeline: runPipelineJob,
};
export function startWorker(store: Store) {
  const stopStreams = startChangeStreams(store);
  let busy = false,
    stopped = false;
  const tick = async () => {
    if (busy || stopped) return;
    busy = true;
    try {
      const workspaces = await store.list("global", "workspaceRef");
      for (const workspace of workspaces) {
        const scope = workspace.id;
        for (const source of await store.list(scope, "source"))
          if (scheduledRefresh(source) || needsChangeRefresh(source)) {
            await transaction(store, scope, async (tx) => {
              const s = await tx.get(source.id);
              if (!s || (!scheduledRefresh(s) && !needsChangeRefresh(s)))
                return;
              const pending = (await tx.list("job")).find(
                (j) =>
                  j.data.sourceId === s.id &&
                  ["queued", "running"].includes(j.data.status),
              );
              if (pending) {
                tx.update(s, {
                  ...s.data,
                  nextRun: s.data.sync?.mode === "cdc" ? null : nextRefresh(s),
                });
                return;
              }
              tx.put(
                resource(scope, "job", "Sync " + s.name, {
                  type: "sync",
                  sourceId: s.id,
                  status: "queued",
                  background: true,
                  trigger: s.data.sync?.mode === "cdc" ? "cdc" : "schedule",
                }),
              );
              tx.update(s, {
                ...s.data,
                nextRun: s.data.sync?.mode === "cdc" ? null : nextRefresh(s),
              });
            });
          }
        const runs = await store.list(scope, "run"),
          jobs = await store.list(scope, "job");
        const candidate = [...runs, ...jobs]
          .filter(
            (r) =>
              (r.data.status === "queued" &&
                (!r.data.retryAt || r.data.retryAt <= Date.now())) ||
              (r.data.status === "running" && r.data.leaseUntil < Date.now()),
          )
          .sort(
            (a, b) =>
              Number(a.data.background || false) -
                Number(b.data.background || false) ||
              a.createdAt.localeCompare(b.createdAt),
          )[0];
        if (candidate) {
          const claimed = await transaction(store, scope, async (tx) => {
            const r = await tx.get(candidate.id);
            if (
              !r ||
              !["queued", "running"].includes(r.data.status) ||
              (r.data.status === "running" && r.data.leaseUntil > Date.now())
            )
              return false;
            tx.update(r, {
              ...r.data,
              status: "running",
              leaseUntil: Date.now() + 180000,
              attempts: (r.data.attempts || 0) + 1,
            });
            return true;
          });
          if (!claimed) continue;
          const heartbeat = setInterval(
            () =>
              void transaction(store, scope, async (tx) => {
                const r = await tx.get(candidate.id);
                if (r?.data.status === "running")
                  tx.update(r, { ...r.data, leaseUntil: Date.now() + 180000 });
              }).catch(console.error),
            30000,
          );
          try {
            if (candidate.kind === "run")
              await buildWorkspace(store, scope, candidate.id);
            else {
              if (candidate.data.actor) {
                const member = await store.get(
                  "global",
                  `${candidate.data.actor}:${scope}`,
                );
                assert(
                  member && ["owner", "editor"].includes(member.data.role),
                  "Import permission is no longer available",
                  403,
                );
              }
              const handler = Object.hasOwn(jobHandlers, candidate.data.type)
                ? jobHandlers[candidate.data.type]
                : undefined;
              if (!handler)
                throw new Error("Unsupported job type: " + candidate.data.type);
              await handler(store, scope, candidate);
              await transaction(store, scope, async (tx) => {
                const r = await tx.get(candidate.id);
                if (r && r.data.status !== "canceled")
                  tx.update(r, {
                    ...r.data,
                    status: "succeeded",
                    error: null,
                    progress: { message: "Complete snapshot ready" },
                  });
              });
            }
          } catch (e) {
            await transaction(store, scope, async (tx) => {
              const r = await tx.get(candidate.id);
              if (r && r.data.status !== "canceled") {
                tx.update(r, {
                  ...r.data,
                  status:
                    r.kind === "job" && (r.data.attempts || 0) < 3
                      ? "queued"
                      : "failed",
                  error: e instanceof Error ? e.message : "Job failed",
                  retryAt:
                    Date.now() +
                    Math.min(60000, 1000 * 2 ** (r.data.attempts || 1)),
                });
                if (r.kind === "job" && (r.data.attempts || 0) >= 3)
                  tx.put(
                    resource(
                      scope,
                      "finding",
                      "Failed " + r.name,
                      {
                        kind: "failure",
                        jobId: r.id,
                        dismissed: false,
                        evidence: {
                          error: e instanceof Error ? e.message : "Job failed",
                          pipelineId: r.data.pipelineId,
                          sourceId: r.data.sourceId,
                        },
                      },
                      "failure_" + r.id,
                    ),
                  );
              }
            });
          } finally {
            clearInterval(heartbeat);
          }
          break;
        }
        const settings = await providerSettings(store, scope);
        if (
          settings.continuous &&
          !runs.some((r) => ["queued", "running"].includes(r.data.status))
        ) {
          const findings = (await store.list(scope, "finding")).filter(
            (f) => !f.data.dismissed && !f.data.runId,
          );
          if (findings.length) {
            const generation = (await store.get(scope, "workspace"))?.data
              .activeGeneration;
            const published = (await store.list(scope, "proposal")).find(
              (p) => p.data.publishedGeneration === generation,
            );
            if (published) {
              const run = await enqueue(
                store,
                scope,
                "Review changed data and repair affected definitions. Preserve accepted business meaning. Findings: " +
                  findings.map((f) => f.name).join("; "),
                published.id,
                undefined,
                true,
              );
              await transaction(store, scope, async (tx) => {
                for (const f of findings)
                  tx.update(f, { ...f.data, runId: run.id });
              });
            }
          }
        }
      }
    } catch (e) {
      console.error("Worker:", e instanceof Error ? e.message : e);
    } finally {
      busy = false;
    }
  };
  let running = tick();
  const timer = setInterval(() => {
    if (!busy) running = tick();
  }, 2000);
  return async () => {
    stopped = true;
    clearInterval(timer);
    await stopStreams();
    await running;
  };
}
