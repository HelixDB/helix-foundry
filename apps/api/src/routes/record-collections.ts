import type { FastifyInstance } from "fastify";
import {
  CreateRecordCollectionSchema,
  UpdateRecordCollectionSchema,
  DeleteRecordCollectionSchema,
  type Resource,
  type RecordCollection,
} from "../../../../packages/shared/src/index.js";
import { assert } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import { effectiveObject } from "../context.js";
import { recordLabel } from "./ontology.js";
import type { RouteDeps, RequestSchemas } from "./deps.js";

const kind = "recordCollection";
const summary = (r: Resource): RecordCollection => ({
  id: r.id,
  name: r.name,
  revision: r.revision,
  updatedAt: r.updatedAt,
  logicalIds: r.data.logicalIds,
});
export const recordCollectionSchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/record-collections": CreateRecordCollectionSchema,
  "PUT /api/v1/workspaces/:w/record-collections/:id":
    UpdateRecordCollectionSchema,
  "DELETE /api/v1/workspaces/:w/record-collections/:id":
    DeleteRecordCollectionSchema,
};
export function registerRecordCollectionRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  app.get("/api/v1/workspaces/:w/record-collections", async (req) => {
    const { scope } = await deps.access(req);
    return {
      items: (await store.list(scope, kind))
        .map(summary)
        .sort(
          (a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id),
        ),
    };
  });
  app.post("/api/v1/workspaces/:w/record-collections", async (req) => {
    const { scope } = await deps.access(req, "editor"),
      body = CreateRecordCollectionSchema.parse(req.body);
    const r = resource(scope, kind, body.name, { logicalIds: [] });
    return transaction(store, scope, async (tx) => {
      assert(
        (await tx.list(kind)).length < 40,
        "A workspace can have up to 40 record collections",
        409,
      );
      tx.put(r);
      return summary(r);
    });
  });
  app.get("/api/v1/workspaces/:w/record-collections/:id", async (req: any) => {
    const { scope } = await deps.access(req),
      r = await store.get(scope, req.params.id);
    assert(r?.kind === kind, "Record collection unavailable", 404);
    const generation =
      (await store.get(scope, "workspace"))?.data.activeGeneration ?? null;
    const members = await Promise.all(
      r.data.logicalIds.map(async (logicalId: string) => {
        const object = generation
          ? await store.get(scope, `${generation}_${logicalId}`)
          : undefined;
        if (object?.kind !== "object" || object.data.generation !== generation)
          return { logicalId, available: false, name: null, type: null };
        const current = await effectiveObject(store, scope, object);
        return {
          logicalId,
          available: true,
          name: recordLabel(
            current.name,
            current.data.type,
            current.data.effective,
          ),
          type: current.data.type,
        };
      }),
    );
    return { ...summary(r), generation, members };
  });
  app.put("/api/v1/workspaces/:w/record-collections/:id", async (req: any) => {
    const { scope } = await deps.access(req, "editor"),
      body = UpdateRecordCollectionSchema.parse(req.body);
    assert(
      new Set(body.logicalIds).size === body.logicalIds.length,
      "A record can only appear once in a collection",
      400,
    );
    return transaction(store, scope, async (tx) => {
      const r = await tx.get(req.params.id);
      assert(r?.kind === kind, "Record collection unavailable", 404);
      assert(
        r.revision === body.revision,
        "Collection changed. Reopen it before saving.",
        409,
      );
      const generation = (await tx.get("workspace"))?.data.activeGeneration;
      const existing = new Set(r.data.logicalIds);
      for (const logicalId of body.logicalIds) {
        if (existing.has(logicalId)) continue;
        const object = generation
          ? await tx.get(`${generation}_${logicalId}`)
          : undefined;
        assert(
          object?.kind === "object" && object.data.generation === generation,
          "Added record is unavailable in the current workspace",
          409,
        );
      }
      const next = {
        ...r,
        name: body.name,
        revision: r.revision + 1,
        updatedAt: new Date().toISOString(),
        data: { logicalIds: body.logicalIds },
      };
      tx.put(next);
      return summary(next);
    });
  });
  app.delete(
    "/api/v1/workspaces/:w/record-collections/:id",
    async (req: any) => {
      const { scope } = await deps.access(req, "editor"),
        body = DeleteRecordCollectionSchema.parse(req.body);
      return transaction(store, scope, async (tx) => {
        const r = await tx.get(req.params.id);
        assert(r?.kind === kind, "Record collection unavailable", 404);
        assert(
          r.revision === body.revision,
          "Collection changed. Reopen it before deleting.",
          409,
        );
        tx.delete(r.id);
        return { deleted: true };
      });
    },
  );
}
