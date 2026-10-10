import type { FastifyInstance } from "fastify";
import { z } from "zod";
import {
  BundleSchema,
  GoalSchema,
  type Resource,
} from "../../../../packages/shared/src/index.js";
import { assert } from "../security.js";
import { transaction, type Store } from "../store.js";
import {
  enqueue,
  saveProposal,
  currentBundle,
  retried,
  assistantResource,
  threadOf,
} from "../assistant.js";
import { createLiveBridge } from "../live.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

const LiveSessionSchema = z.object({
  // The browser's WebRTC offer.
  sdp: z.string().min(1).max(100_000),
  // The chat to continue; omitted on Home, where the first question starts one.
  chatId: z.string().min(1).max(200).nullish(),
});
const LiveContextSchema = z.object({
  chatId: z.string().min(1).max(200).optional(),
  typed: z.string().min(1).max(4000).optional(),
});
export const analystSchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/assistant/runs": GoalSchema,
  "POST /api/v1/workspaces/:w/live/sessions": LiveSessionSchema,
  "POST /api/v1/workspaces/:w/live/sessions/:sessionId/context":
    LiveContextSchema,
};

// AIP Analyst: assistant runs and their controls.
export function registerAnalystRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access, scrub } = deps;
  // Chats: a question and its follow-ups. Setup and other task runs aren't.
  const chats = async (scope: string) => {
    const runs = new Map(
      (await store.list(scope, "run"))
        .filter((r) => !r.data.task)
        .map((r) => [r.id, r]),
    );
    const groups = new Map<string, Resource[]>();
    for (const r of runs.values()) {
      const t = threadOf(r, runs);
      if (!groups.has(t)) groups.set(t, []);
      groups.get(t)!.push(r);
    }
    for (const g of groups.values())
      g.sort((a, b) => a.createdAt.localeCompare(b.createdAt));
    return groups;
  };
  app.get("/api/v1/workspaces/:w/threads", async (req) => {
    const a = await access(req);
    return {
      items: [...(await chats(a.scope)).entries()]
        .map(([id, runs]) => ({
          id,
          title: runs[0].data.goal || runs[0].name,
          createdAt: runs[0].createdAt,
          updatedAt: runs
            .map((r) => r.updatedAt)
            .sort()
            .at(-1)!,
          turns: runs.length,
          status: runs.at(-1)!.data.status,
        }))
        .sort((x, y) => y.updatedAt.localeCompare(x.updatedAt)),
    };
  });
  app.get("/api/v1/workspaces/:w/threads/:threadId", async (req: any) => {
    const a = await access(req),
      runs = (await chats(a.scope)).get(req.params.threadId);
    assert(runs, "Chat not found", 404);
    return {
      id: req.params.threadId,
      title: runs[0].data.goal || runs[0].name,
      runs: runs.map(scrub),
    };
  });
  app.delete("/api/v1/workspaces/:w/threads/:threadId", async (req: any) => {
    const a = await access(req, "editor"),
      runs = (await chats(a.scope)).get(req.params.threadId);
    assert(runs, "Chat not found", 404);
    assert(
      !runs.some((r) => ["queued", "running"].includes(r.data.status)),
      "Stop the question that is still running first.",
      409,
    );
    await transaction(store, a.scope, async (tx) => {
      for (const r of runs) tx.delete(r.id);
    });
    return { ok: true };
  });
  app.post("/api/v1/workspaces/:w/assistant/runs", async (req) => {
    const b = GoalSchema.parse(req.body),
      a = await access(req, b.intent === "build" ? "editor" : "viewer");
    if (b.context?.resourceId) {
      const r = await assistantResource(store, a.scope, b.context.resourceId);
      if (r.kind === "proposal") b.parentId ||= r.id;
    }
    if (b.parentId)
      assert(
        (await store.get(a.scope, b.parentId))?.kind === "proposal",
        "Proposal unavailable",
        404,
      );
    return enqueue(
      store,
      a.scope,
      b.goal,
      b.parentId,
      b.context,
      false,
      b.intent,
      {
        userId: a.user.id,
        readOnly: a.role === "viewer" || !!a.session.data.readOnly,
      },
    );
  });
  app.post(
    "/api/v1/workspaces/:w/assistant/runs/:id/cancel",
    async (req: any) => {
      const a = await access(req);
      return transaction(store, a.scope, async (tx) => {
        const r = await tx.get(req.params.id);
        assert(
          (a.role !== "viewer" && !a.session.data.readOnly) ||
            (r?.data.authority?.userId === a.user.id &&
              r.data.intent !== "build" &&
              r.data.resolvedIntent !== "build"),
          "Insufficient workspace permissions",
          403,
        );
        assert(r?.kind === "run", "Run unavailable", 404);
        // Publication commits as a unit, so it finishes once started.
        assert(
          r.data.stage !== "publishing",
          "Your workspace is already being published.",
          409,
        );
        return tx.update(r, { ...r.data, status: "canceled" });
      });
    },
  );
  app.post(
    "/api/v1/workspaces/:w/assistant/runs/:id/resume",
    async (req: any) => {
      const a = await access(req);
      return transaction(store, a.scope, async (tx) => {
        const r = await tx.get(req.params.id);
        assert(
          (a.role !== "viewer" && !a.session.data.readOnly) ||
            (r?.data.authority?.userId === a.user.id &&
              r.data.intent !== "build" &&
              r.data.resolvedIntent !== "build"),
          "Insufficient workspace permissions",
          403,
        );
        assert(
          r?.kind === "run" && ["failed", "canceled"].includes(r.data.status),
          "Run cannot be resumed",
        );
        return tx.update(r, {
          ...retried(r.data, "Trying again"),
          // A setup build publishes as whoever retries it.
          ...(r.data.task === "onboarding_build"
            ? { authority: { userId: a.user.id, readOnly: false } }
            : {}),
        });
      });
    },
  );
  app.post(
    "/api/v1/workspaces/:w/assistant/runs/:id/save-analysis",
    async (req: any) => {
      const a = await access(req, "editor"),
        r = await store.get(a.scope, req.params.id);
      assert(r?.kind === "run" && r.data.answer, "Analysis unavailable", 404);
      const v = r.data.answer,
        existing = await currentBundle(store, a.scope);
      return saveProposal(
        store,
        a.scope,
        BundleSchema.parse({
          summary: v.title,
          assumptions: v.assumptions,
          catalog: existing.catalog,
          pipelines: existing.pipelines,
          ontology: existing.ontology,
          relationships: existing.relationships,
          actions: existing.actions,
          dashboards: [
            ...existing.dashboards.filter((d) => d.name !== v.title),
            {
              name: v.title,
              description: "Saved analysis of dataset snapshots",
              widgets: [
                { title: v.title, type: "table", sql: v.sql, inputs: v.inputs },
              ],
            },
          ],
        }),
      );
    },
  );

  // Hands-free voice (GPT-Live): see ../live.ts.
  const live = createLiveBridge(store);
  app.addHook("onClose", async () => live.close());
  app.get("/api/v1/workspaces/:w/live", async (req) => {
    const a = await access(req);
    return live.availability(a.scope);
  });
  app.post("/api/v1/workspaces/:w/live/sessions", async (req) => {
    const a = await access(req),
      b = LiveSessionSchema.parse(req.body);
    return live.create({
      scope: a.scope,
      userId: a.user.id,
      readOnly: a.role === "viewer" || !!a.session.data.readOnly,
      sdp: b.sdp,
      chatId: b.chatId,
    });
  });
  app.get(
    "/api/v1/workspaces/:w/live/sessions/:sessionId",
    async (req: any) => {
      const a = await access(req);
      return live.state(req.params.sessionId, a.scope, a.user.id);
    },
  );
  app.post(
    "/api/v1/workspaces/:w/live/sessions/:sessionId/delegations/:delegationId",
    async (req: any) => {
      const a = await access(req);
      return live.claim(
        req.params.sessionId,
        a.scope,
        a.user.id,
        String(req.params.delegationId).slice(0, 200),
      );
    },
  );
  app.post(
    "/api/v1/workspaces/:w/live/sessions/:sessionId/context",
    async (req: any) => {
      const a = await access(req);
      return live.context(
        req.params.sessionId,
        a.scope,
        a.user.id,
        LiveContextSchema.parse(req.body),
      );
    },
  );
  app.delete(
    "/api/v1/workspaces/:w/live/sessions/:sessionId",
    async (req: any) => {
      const a = await access(req);
      return live.end(req.params.sessionId, a.scope, a.user.id);
    },
  );
}
