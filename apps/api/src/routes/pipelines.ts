import type { FastifyInstance } from "fastify";
import { PipelineSchema } from "../../../../packages/shared/src/index.js";
import {
  compilePipeline,
  PipelineValidationError,
} from "../../../../packages/shared/src/pipeline.js";
import { assert, AppError } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import { saveProposal, currentBundle } from "../assistant.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

export const pipelinesSchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/pipelines/draft": PipelineSchema,
};

// Pipeline Builder: drafts (validated as proposals) and batch runs.
export function registerPipelinesRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access } = deps;
  app.post("/api/v1/workspaces/:w/pipelines/draft", async (req) => {
    const a = await access(req, "editor"),
      p = PipelineSchema.parse(req.body);
    if (p.nodes.length) {
      try {
        Object.assign(p, compilePipeline(p.nodes));
      } catch (error) {
        if (error instanceof PipelineValidationError)
          throw new AppError(400, error.message);
        throw error;
      }
    }
    const bundle = await currentBundle(store, a.scope);
    bundle.pipelines = [
      ...bundle.pipelines.filter((x) => x.name !== p.name),
      p,
    ];
    bundle.summary = "Update pipeline: " + p.name;
    return saveProposal(store, a.scope, bundle);
  });
  app.post("/api/v1/workspaces/:w/pipelines/:id/run", async (req: any) => {
    const a = await access(req, "editor"),
      p = await store.get(a.scope, req.params.id);
    assert(p?.kind === "pipeline", "Pipeline unavailable", 404);
    return transaction(store, a.scope, async (tx) =>
      tx.put(
        resource(a.scope, "job", "Run " + p.name, {
          type: "pipeline",
          pipelineId: p.id,
          status: "queued",
        }),
      ),
    );
  });
}
