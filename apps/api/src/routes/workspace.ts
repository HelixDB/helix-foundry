import { randomUUID } from "node:crypto";
import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { Readable } from "node:stream";
import {
  BundleSchema,
  ProviderSchema,
  ActivityKindSchema,
  collapseActivity,
  describeActivity,
  type ActivityItem,
  type HealthStatus,
  type HomeSummary,
  type Resource,
} from "../../../../packages/shared/src/index.js";
import { config } from "../config.js";
import { proposalComparison } from "../proposal-comparison.js";
import { assert, digest, token, seal } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import {
  saveProposal,
  publishProposal,
  providerSettings,
} from "../assistant.js";
import { testProvider, canReuseProviderKey } from "../providers.js";
import { localAiMemory } from "../memory.js";
import { homeMetrics, regenerateMetrics } from "../metrics.js";
import {
  generationKinds,
  type RequestSchemas,
  type RouteDeps,
} from "./deps.js";

export const workspaceSchemas: RequestSchemas = {
  "PUT /api/v1/workspaces/:w/provider": ProviderSchema,
  "PUT /api/v1/workspaces/:w/proposals/:id": BundleSchema,
  "POST /api/v1/workspaces/:w/proposals/:id/publish": z.object({
    hash: z.string(),
  }),
  "POST /api/v1/workspaces/:w/definitions/import": z.object({
    version: z.literal(1),
    proposals: z.array(BundleSchema),
  }),
  "POST /api/v1/workspaces/:w/tokens": z.object({
    name: z.string(),
    readOnly: z.boolean(),
  }),
};

const byNewest = (x: { at: string }, y: { at: string }) =>
  y.at.localeCompare(x.at);
// Jobs, runs and audits as one feed, newest first.
function activityFeed(jobs: Resource[], runs: Resource[], audits: Resource[]) {
  return [...jobs, ...runs, ...audits].map(describeActivity).sort(byNewest);
}
// What the live tray needs from a job or run; the full resources stay private.
const slim = (r: Resource) => ({
  id: r.id,
  kind: r.kind,
  name: r.name,
  createdAt: r.createdAt,
  updatedAt: r.updatedAt,
  data: {
    status: r.data.status,
    type: r.data.type,
    task: r.data.task,
    goal: r.data.goal,
    error: r.data.error,
    sourceId: r.data.sourceId,
    pipelineId: r.data.pipelineId,
    datasetId: r.data.datasetId,
    settings: r.data.settings && { provider: r.data.settings.provider },
  },
});
const DAY = 86400000;

// Workspace essentials: home, overview, live events, reviews and publication,
// AI provider, API tokens and definitions.
export function registerWorkspaceRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access, scrub, visibleIn } = deps;
  // Home's growth and analytics metrics: picked once (by the AI when it is set
  // up) and re-run by code when the data changes.
  app.get("/api/v1/workspaces/:w/home/metrics", async (req) => {
    const a = await access(req);
    return homeMetrics(store, a.scope, a.user.id);
  });
  app.post("/api/v1/workspaces/:w/home/metrics/regenerate", async (req) => {
    const a = await access(req, "editor");
    await regenerateMetrics(store, a.scope, a.user.id);
    return homeMetrics(store, a.scope, a.user.id);
  });
  app.get("/api/v1/workspaces/:w/home", async (req) => {
    const a = await access(req);
    const ws = await store.get(a.scope, "workspace");
    const generation: string | null = ws?.data.activeGeneration ?? null,
      excluded: string[] = ws?.data.onboarding?.excludedIds || [];
    const [
      datasets,
      sources,
      jobs,
      runs,
      proposals,
      findings,
      dashboards,
      pipelines,
      objectTypes,
      audits,
      publications,
    ] = await Promise.all(
      [
        "dataset",
        "source",
        "job",
        "run",
        "proposal",
        "finding",
        "dashboard",
        "pipeline",
        "objectType",
        "audit",
        "publication",
      ].map((k) => store.list(a.scope, k)),
    );
    const live = (r: Resource) => visibleIn(r, generation);
    const newest = (x: Resource, y: Resource) =>
      y.updatedAt.localeCompare(x.updatedAt);
    const active = (r?: Resource) =>
      ["queued", "running"].includes(r?.data.status);
    const kept = sources.filter((s) => !excluded.includes(s.id)),
      raw = datasets.filter(
        (d) =>
          !d.data.generation &&
          !excluded.includes(d.id) &&
          !excluded.includes(d.data.sourceId),
      );
    const latest = new Map<string, Resource>();
    for (const j of [...jobs].sort(newest))
      if (j.data.sourceId && !latest.has(j.data.sourceId))
        latest.set(j.data.sourceId, j);
    const rank: Record<HealthStatus, number> = {
      healthy: 0,
      paused: 1,
      stale: 2,
      syncing: 3,
      failed: 4,
    };
    const status = (s: Resource): HealthStatus => {
      const j = latest.get(s.id);
      if (s.data.paused) return "paused";
      if (active(j)) return "syncing";
      if (j?.data.status === "failed" || s.data.sync?.status === "retrying")
        return "failed";
      return j || raw.some((d) => d.data.sourceId === s.id)
        ? "healthy"
        : "syncing";
    };
    const groups = new Map<string, Resource[]>();
    for (const s of kept) {
      const key = s.data.connectionId || s.id;
      groups.set(key, [...(groups.get(key) || []), s]);
    }
    const connections: HomeSummary["connections"] = [...groups].map(
      ([id, members]) => {
        const worst = members.map(status).sort((x, y) => rank[y] - rank[x])[0],
          recent = members
            .map((s) => latest.get(s.id))
            .filter((j): j is Resource => !!j);
        return {
          id,
          name: members[0].data.connectionName || members[0].name,
          connector: members[0].data.hostedProvider || members[0].data.kind,
          status: worst,
          sources: members.length,
          datasets: raw.filter((d) =>
            members.some((s) => s.id === d.data.sourceId),
          ).length,
          lastSyncAt:
            recent
              .filter((j) => j.data.status === "succeeded")
              .map((j) => j.updatedAt)
              .sort()
              .at(-1) ?? null,
          error:
            recent.find((j) => j.data.status === "failed")?.data.error ?? null,
          sourceId: members[0].id,
          sample: false,
        };
      },
    );
    // Sample data and files arrive without a source; each shows as one connection.
    const unsourced = new Map<string, Resource[]>();
    for (const d of raw.filter((d) => !d.data.sourceId)) {
      const key = d.data.sampleSource || "upload";
      unsourced.set(key, [...(unsourced.get(key) || []), d]);
    }
    const uploads = jobs.filter((j) => j.data.type === "upload").sort(newest);
    for (const [key, members] of unsourced) {
      const sample = key !== "upload";
      connections.push({
        id: sample ? "sample:" + key : "files",
        name: sample ? key : "Files and API",
        connector: key,
        status: sample
          ? "healthy"
          : uploads.some(active)
            ? "syncing"
            : uploads[0]?.data.status === "failed"
              ? "failed"
              : "healthy",
        sources: 0,
        datasets: members.length,
        lastSyncAt:
          members
            .map((d) => d.updatedAt)
            .sort()
            .at(-1) ?? null,
        error: sample ? null : (uploads[0]?.data.error ?? null),
        sourceId: null,
        sample,
      });
    }
    connections.sort(
      (x, y) => rank[y.status] - rank[x.status] || x.name.localeCompare(y.name),
    );
    const builds = jobs.filter((j) => j.data.type === "pipeline").sort(newest),
      lastBuild = new Map<string, Resource>();
    for (const j of builds)
      if (!lastBuild.has(j.data.pipelineId))
        lastBuild.set(j.data.pipelineId, j);
    const types = objectTypes.filter(live),
      names = new Set(types.map((t) => t.name));
    const published = proposals.find(
      (p) => generation && p.data.publishedGeneration === generation,
    );
    const open = findings.filter((f) => !f.data.dismissed).sort(newest),
      ready = proposals.filter((p) => p.data.status === "ready").sort(newest);
    const activity = collapseActivity(activityFeed(jobs, runs, audits)).slice(
      0,
      12,
    );
    const livePipelines = new Map(pipelines.filter(live).map((p) => [p.id, p]));
    const failing = [...lastBuild.values()]
      .filter(
        (j) =>
          j.data.status === "failed" && livePipelines.has(j.data.pipelineId),
      )
      .map((j) => ({
        pipelineId: j.data.pipelineId as string,
        name: livePipelines.get(j.data.pipelineId)!.name,
        jobId: j.id,
        at: j.updatedAt,
        error: j.data.error ?? null,
      }));
    const summary: HomeSummary = {
      workspace: {
        id: a.scope,
        name: ws?.name || "",
        activeGeneration: generation,
        publishedAt:
          publications
            .filter((p) => p.data.generation === generation)
            .map((p) => p.createdAt)
            .at(0) ?? null,
      },
      counts: {
        datasets: datasets.filter((d) => live(d) && !excluded.includes(d.id))
          .length,
        sources: kept.length,
        pipelines: pipelines.filter(live).length,
        objectTypes: types.length,
        objects: types.reduce((n, t) => n + (t.data.objectCount || 0), 0),
        dashboards: dashboards.filter(live).length,
      },
      connections,
      pipelines: {
        total: pipelines.filter(live).length,
        running: builds.filter(active).length,
        failed: failing.length,
        failing,
        lastRun: builds[0]
          ? {
              jobId: builds[0].id,
              pipelineId: builds[0].data.pipelineId ?? null,
              name: builds[0].name.replace(/^Run /, ""),
              status: builds[0].data.status,
              at: builds[0].updatedAt,
              error: builds[0].data.error ?? null,
            }
          : null,
      },
      findings: {
        open: open.length,
        items: open.slice(0, 3).map((f) => ({
          id: f.id,
          name: f.name,
          kind: f.data.kind || "finding",
          at: f.updatedAt,
          datasetId: f.data.datasetId ?? f.data.evidence?.datasetId ?? null,
          runId: f.data.runId ?? null,
        })),
      },
      proposals: {
        pending: ready.length,
        items: ready
          .slice(0, 3)
          .map((p) => ({ id: p.id, name: p.name, at: p.updatedAt })),
      },
      ontology: {
        types: types
          .map((t) => ({
            id: t.id,
            typeId: t.data.typeId || t.id,
            name: t.name,
            objects: t.data.objectCount || 0,
            properties: t.data.properties?.length || 0,
            datasetId: t.data.datasetId,
          }))
          .sort(
            (x, y) => y.objects - x.objects || x.name.localeCompare(y.name),
          ),
        links: (published?.data.bundle.relationships || [])
          .filter((r: any) => names.has(r.fromType) && names.has(r.toType))
          .map((r: any) => ({ name: r.name, from: r.fromType, to: r.toType })),
      },
      activity,
      dashboards: dashboards
        .filter(live)
        .sort((x, y) => x.name.localeCompare(y.name))
        .map((d) => ({
          id: d.id,
          name: d.name,
          description: d.description || d.data.description || "",
          widgets: d.data.widgets?.length || 0,
        })),
    };
    return summary;
  });
  app.get("/api/v1/workspaces/:w/overview", async (req) => {
    const a = await access(req);
    const ws = await store.get(a.scope, "workspace");
    const kinds = [
      "dataset",
      "source",
      "run",
      "proposal",
      "finding",
      "dashboard",
      "pipeline",
      "objectType",
    ];
    const result = Object.fromEntries(
      await Promise.all(
        kinds.map(async (k) => [
          k,
          (await store.list(a.scope, k))
            .filter(
              (r) =>
                (!generationKinds.has(k) &&
                  !(k === "dataset" && r.data.generation)) ||
                r.data.generation === ws?.data.activeGeneration,
            )
            .map(scrub),
        ]),
      ),
    );
    return { workspace: ws, ...result };
  });
  app.get("/api/v1/workspaces/:w/provider", async (req) => {
    const a = await access(req);
    const settings = await providerSettings(store, a.scope);
    return {
      ...settings,
      apiKey: undefined,
      hasKey: !!settings.apiKey,
      // Lets setup warn before Local is chosen on a Docker without room for it.
      localMemory: localAiMemory(
        (settings.provider === "local" && settings.baseUrl) || config.ollama,
      ),
    };
  });
  app.put("/api/v1/workspaces/:w/provider", async (req) => {
    const a = await access(req, "owner"),
      s = ProviderSchema.parse(req.body);
    if (!s.apiKey) {
      const old = await providerSettings(store, a.scope);
      if (canReuseProviderKey(old, s)) s.apiKey = old.apiKey;
    }
    await testProvider(s);
    await transaction(store, a.scope, async (tx) =>
      tx.put(
        resource(
          a.scope,
          "provider",
          "AI provider",
          {
            settings: { ...s, apiKey: undefined },
            secret: s.apiKey ? seal(s.apiKey) : null,
          },
          "provider",
        ),
      ),
    );
    return { ok: true };
  });
  app.post("/api/v1/workspaces/:w/provider/pull", async (req, reply) => {
    const a = await access(req, "owner"),
      s = await providerSettings(store, a.scope);
    assert(s.provider === "local", "Select local inference");
    const result = await fetch((s.baseUrl || config.ollama) + "/api/pull", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ model: s.model, stream: true }),
      signal: AbortSignal.timeout(1800000),
    });
    assert(result.ok && result.body, "Model download failed", 502);
    reply.type("application/x-ndjson");
    return reply.send(Readable.fromWeb(result.body as any));
  });
  app.get("/api/v1/workspaces/:w/activity", async (req: any) => {
    const a = await access(req);
    const kinds = String(req.query.kind || "")
      .split(",")
      .filter(Boolean)
      .map((k) => ActivityKindSchema.parse(k));
    const [jobs, runs, audits] = await Promise.all(
      ["job", "run", "audit"].map((k) => store.list(a.scope, k)),
    );
    const items = collapseActivity(
      activityFeed(jobs, runs, audits),
      "adjacent",
    ).filter((i) => !kinds.length || kinds.includes(i.kind));
    const offset = Math.max(Number(req.query.offset) || 0, 0),
      limit = Math.max(1, Math.min(Number(req.query.limit) || 50, 200));
    return {
      items: items.slice(offset, offset + limit) as ActivityItem[],
      total: items.length,
    };
  });
  // Live runs and jobs (active, or updated in the last day) plus the pending
  // review count. Sent when something changes; a comment keeps idle streams open.
  app.get("/api/v1/workspaces/:w/events", async (req: any, reply) => {
    const a = await access(req);
    reply.hijack();
    reply.raw.writeHead(200, {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "keep-alive",
    });
    let busy = false,
      last = "",
      idle = 0;
    const send = async () => {
      if (busy) return;
      busy = true;
      try {
        await access(req);
        const since = new Date(Date.now() - DAY).toISOString();
        const recent = (r: Resource) =>
          ["queued", "running"].includes(r.data.status) || r.updatedAt >= since;
        const [runs, jobs, proposals] = await Promise.all(
          ["run", "job", "proposal"].map((k) => store.list(a.scope, k)),
        );
        const payload = JSON.stringify({
          runs: runs.filter(recent).map((r) => slim(scrub(r))),
          jobs: jobs.filter(recent).map((r) => slim(scrub(r))),
          reviews: proposals.filter((p) => p.data.status === "ready").length,
        });
        if (payload !== last) {
          last = payload;
          idle = 0;
          reply.raw.write(`data: ${payload}\n\n`);
        } else if (++idle % 8 === 0) reply.raw.write(": keep-alive\n\n");
      } catch {
        reply.raw.end();
        clearInterval(timer);
      } finally {
        busy = false;
      }
    };
    const timer = setInterval(send, 2000);
    req.raw.on("close", () => clearInterval(timer));
    void send();
  });
  app.get(
    "/api/v1/workspaces/:w/proposals/:id/comparison",
    async (req: any) => {
      const a = await access(req);
      return proposalComparison(store, a.scope, req.params.id);
    },
  );
  app.put("/api/v1/workspaces/:w/proposals/:id", async (req: any) => {
    const a = await access(req, "editor"),
      old = await store.get(a.scope, req.params.id);
    assert(old?.kind === "proposal", "Proposal unavailable", 404);
    return saveProposal(store, a.scope, BundleSchema.parse(req.body), old.id);
  });
  app.post("/api/v1/workspaces/:w/proposals/:id/validate", async (req: any) => {
    const a = await access(req, "editor"),
      p = await store.get(a.scope, req.params.id);
    assert(p?.kind === "proposal", "Proposal unavailable", 404);
    return saveProposal(
      store,
      a.scope,
      BundleSchema.parse(p.data.bundle),
      p.id,
    );
  });
  app.post("/api/v1/workspaces/:w/proposals/:id/publish", async (req: any) => {
    const a = await access(req, "editor"),
      b = z.object({ hash: z.string() }).parse(req.body);
    return publishProposal(store, a.scope, req.params.id, b.hash, a.user.id);
  });
  app.post("/api/v1/workspaces/:w/proposals/:id/reject", async (req: any) => {
    const a = await access(req, "editor");
    return transaction(store, a.scope, async (tx) => {
      const p = await tx.get(req.params.id);
      assert(
        p?.kind === "proposal" && p.data.status !== "published",
        "Proposal cannot be rejected",
      );
      return tx.update(p, { ...p.data, status: "rejected" });
    });
  });
  app.post("/api/v1/workspaces/:w/rollback", async (req) => {
    const a = await access(req, "editor"),
      ws = await store.get(a.scope, "workspace");
    assert(ws?.data.previousGeneration, "No previous publication");
    const old = (await store.list(a.scope, "proposal")).find(
      (p) => p.data.publishedGeneration === ws.data.previousGeneration,
    );
    assert(old, "Previous definitions unavailable");
    const draft = await saveProposal(
      store,
      a.scope,
      BundleSchema.parse(old.data.bundle),
      old.id,
    );
    assert(
      draft.data.validation.valid,
      "Previous definitions no longer validate against current inputs; review the rollback draft",
      409,
    );
    const result = await publishProposal(
      store,
      a.scope,
      draft.id,
      draft.data.hash,
      a.user.id,
    );
    await transaction(store, a.scope, async (tx) =>
      tx.put(
        resource(a.scope, "audit", "Rolled back definitions", {
          actor: a.user.id,
          previousProposal: old.id,
          proposalId: result.id,
        }),
      ),
    );
    return result;
  });
  app.get("/api/v1/workspaces/:w/tokens", async (req) => {
    const a = await access(req, "owner");
    return transaction(store, "global", async (tx) => {
      const items = (await tx.list("token"))
        .filter((t) => t.data.workspaceId === a.scope)
        .map((t) => {
          // Tokens predating this inventory had only their private authentication
          // digest as an id. Give those tokens a stable, opaque revocation handle.
          const current = t.data.publicId
            ? t
            : tx.update(t, { ...t.data, publicId: randomUUID() });
          return {
            id: current.data.publicId as string,
            name: current.name,
            readOnly: !!current.data.readOnly,
            createdAt: current.createdAt,
          };
        })
        .sort(
          (x, y) =>
            y.createdAt.localeCompare(x.createdAt) || x.id.localeCompare(y.id),
        );
      return { items };
    });
  });
  app.post("/api/v1/workspaces/:w/tokens", async (req) => {
    const a = await access(req, "owner"),
      b = z
        .object({ name: z.string(), readOnly: z.boolean().default(true) })
        .parse(req.body),
      raw = token(),
      publicId = randomUUID();
    await transaction(store, "global", async (tx) =>
      tx.put(
        resource(
          "global",
          "token",
          b.name,
          {
            userId: a.user.id,
            workspaceId: a.scope,
            readOnly: b.readOnly,
            publicId,
          },
          digest(raw),
        ),
      ),
    );
    return { token: raw, id: publicId };
  });
  app.delete("/api/v1/workspaces/:w/tokens/:id", async (req: any) => {
    const a = await access(req, "owner");
    await transaction(store, "global", async (tx) => {
      // Keep ids returned by older versions valid without exposing them in
      // the inventory. New callers use the opaque public handle.
      const t =
        (await tx.get(req.params.id)) ||
        (await tx.list("token")).find(
          (t) =>
            t.data.workspaceId === a.scope && t.data.publicId === req.params.id,
        );
      assert(
        t?.kind === "token" && t.data.workspaceId === a.scope,
        "Token unavailable",
        404,
      );
      tx.delete(t.id);
    });
    return { ok: true };
  });
  app.get("/api/v1/workspaces/:w/definitions/export", async (req) => {
    const a = await access(req);
    const ws = await store.get(a.scope, "workspace");
    return {
      version: 1,
      proposals: (await store.list(a.scope, "proposal"))
        .filter((p) => p.data.publishedGeneration === ws?.data.activeGeneration)
        .map((p) => p.data.bundle),
    };
  });
  app.post("/api/v1/workspaces/:w/definitions/import", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({ version: z.literal(1), proposals: z.array(BundleSchema) })
        .parse(req.body);
    const items = [];
    for (const bundle of b.proposals)
      items.push(await saveProposal(store, a.scope, bundle));
    return { items };
  });
}
