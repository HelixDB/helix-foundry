import type { FastifyInstance } from "fastify";
import { z } from "zod";
import type { Resource } from "../../../../packages/shared/src/index.js";
import { listRecords, recordLabel, snapshotNeighbours } from "../records.js";
import { assert, digest } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import { effectiveObject } from "../context.js";
import {
  ObjectActionSchema,
  previewObjectAction,
  executeObjectAction,
} from "../object-actions.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

export const ontologySchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/actions/change/preview": ObjectActionSchema,
  "POST /api/v1/workspaces/:w/actions/change/execute": z.object({
    action: ObjectActionSchema,
    hash: z.string(),
    idempotencyKey: z.string(),
  }),
  "POST /api/v1/workspaces/:w/actions/preview": z.object({
    objectId: z.string(),
    values: z.record(z.string(), z.unknown()),
  }),
  "POST /api/v1/workspaces/:w/actions/execute": z.object({
    objectId: z.string(),
    values: z.record(z.string(), z.unknown()),
    revision: z.number().int(),
    idempotencyKey: z.string(),
    revert: z.boolean().optional(),
  }),
};

// Ontology: object views, neighbors and object actions (edits, create, link).
export function registerOntologyRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access } = deps;
  app.get("/api/v1/workspaces/:w/records/:logicalId", async (req: any) => {
    const a = await access(req),
      ws = await store.get(a.scope, "workspace");
    const r = await store.get(
      a.scope,
      `${ws?.data.activeGeneration}_${req.params.logicalId}`,
    );
    assert(
      r?.kind === "object" && r.data.generation === ws?.data.activeGeneration,
      "Record unavailable",
      404,
    );
    const record = await effectiveObject(store, a.scope, r);
    const neighbors = await store.neighbors(a.scope, r.id);
    const related = (
      await Promise.all(neighbors.map((x) => store.get(a.scope, x)))
    ).filter(
      (x) =>
        x?.kind === "object" && x.data.generation === ws?.data.activeGeneration,
    );
    return {
      record,
      history: (await store.list(a.scope, "audit"))
        .filter(
          (x) =>
            x.data.objectId === r.id ||
            x.data.objectId?.endsWith("_" + r.data.logicalId) ||
            x.data.evidence?.logicalId === r.data.logicalId ||
            x.data.evidence?.fromLogicalId === r.data.logicalId ||
            x.data.evidence?.toLogicalId === r.data.logicalId,
        )
        .sort((a, b) => b.createdAt.localeCompare(a.createdAt))
        .slice(0, 30),
      related: await Promise.all(
        related.map((x) => effectiveObject(store, a.scope, x)),
      ),
    };
  });
  app.get("/api/v1/workspaces/:w/objects/:id/neighbors", async (req: any) => {
    const a = await access(req),
      o = await store.get(a.scope, req.params.id);
    assert(
      o?.kind === "object" &&
        o.data.generation ===
          (await store.get(a.scope, "workspace"))?.data.activeGeneration,
      "Object unavailable",
      404,
    );
    return { ids: await store.neighbors(a.scope, o.id) };
  });
  // The record graph: records as nodes and their links as edges. With a
  // focus, the records within `depth` links of it; without one, the records
  // around the best-connected ones. Only those records are read, and
  // `truncated` says when the limit left some out.
  app.get("/api/v1/workspaces/:w/graph", async (req: any) => {
    const a = await access(req),
      q = z
        .object({
          focus: z.string().max(300).optional(),
          depth: z.coerce.number().int().min(1).max(3).default(1),
          limit: z.coerce.number().int().min(1).max(3000).default(1500),
        })
        .parse(req.query),
      generation = (await store.get(a.scope, "workspace"))?.data
        .activeGeneration;
    const total = (await store.list(a.scope, "objectType"))
      .filter((t) => t.data.generation === generation)
      .reduce((n, t) => n + (t.data.objectCount || 0), 0);
    if (!generation || !total)
      return { total: 0, nodes: [], edges: [], truncated: false };
    let seeds: string[];
    // Links already counted for the seeds, which are the best-connected.
    let counted = new Map<string, number>(),
      best: Resource[] = [];
    if (q.focus) {
      const start = await store.get(a.scope, `${generation}_${q.focus}`);
      assert(
        start?.kind === "object" && start.data.generation === generation,
        "Record unavailable",
        404,
      );
      seeds = [start.id];
    } else {
      best = (
        await listRecords(store, a.scope, {
          sort: "links",
          dir: "desc",
          offset: 0,
          limit: Math.min(20, q.limit),
        })
      ).items;
      seeds = best.map((o) => o.id);
      counted = new Map(best.map((o) => [o.id, o.data.links as number]));
    }
    const hood = q.focus
      ? await store.neighborhood(a.scope, seeds, q.depth, q.limit)
      : await bestConnectedNeighbourhood(
          store,
          a.scope,
          best,
          counted,
          q.limit,
        );
    const nodes = hood.nodes.filter(
        (o) => o.kind === "object" && o.data.generation === generation,
      ),
      kept = new Map(nodes.map((o) => [o.id, o])),
      degree = new Map([
        ...counted,
        ...(await store.linkCounts(
          a.scope,
          nodes.map((o) => o.id).filter((id) => !counted.has(id)),
        )),
      ]);
    return {
      total,
      truncated: hood.truncated || nodes.length < total,
      nodes: nodes.map((o) => ({
        id: o.data.logicalId,
        type: o.data.type,
        name: o.name,
        label: recordLabel(o.name, o.data.type, o.data.base || {}),
        degree: degree.get(o.id) || 0,
      })),
      edges: hood.edges
        .filter((e) => kept.has(e.from) && kept.has(e.to))
        .map((e) => ({
          from: kept.get(e.from)!.data.logicalId,
          to: kept.get(e.to)!.data.logicalId,
          name: e.name,
        })),
    };
  });
  app.post("/api/v1/workspaces/:w/actions/change/preview", async (req) => {
    const a = await access(req, "editor");
    return previewObjectAction(
      store,
      a.scope,
      ObjectActionSchema.parse(req.body),
    );
  });
  app.post("/api/v1/workspaces/:w/actions/change/execute", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          action: ObjectActionSchema,
          hash: z.string(),
          idempotencyKey: z.string().min(8),
        })
        .parse(req.body);
    return executeObjectAction(
      store,
      a.scope,
      b.action,
      b.hash,
      b.idempotencyKey,
      a.user.id,
    );
  });
  app.post("/api/v1/workspaces/:w/actions/preview", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          objectId: z.string(),
          values: z.record(z.string(), z.unknown()),
        })
        .parse(req.body),
      o = await store.get(a.scope, b.objectId);
    assert(
      o?.kind === "object" &&
        o.data.generation ===
          (await store.get(a.scope, "workspace"))?.data.activeGeneration,
      "Object unavailable",
      404,
    );
    const type = (await store.list(a.scope, "objectType")).find(
      (t) => t.data.generation === o.data.generation && t.name === o.data.type,
    );
    assert(type, "Object type missing");
    assert(
      Object.keys(b.values).every(
        (k) => type.data.properties.includes(k) && k !== type.data.primaryKey,
      ),
      "Invalid editable fields",
    );
    const overlay = await store.get(a.scope, "overlay_" + o.data.logicalId);
    return {
      objectId: o.id,
      before: { ...o.data.base, ...overlay?.data.values },
      after: { ...o.data.base, ...overlay?.data.values, ...b.values },
      revision: overlay?.revision || 0,
    };
  });
  app.post("/api/v1/workspaces/:w/actions/execute", async (req) => {
    const a = await access(req, "editor"),
      b = z
        .object({
          objectId: z.string(),
          values: z.record(z.string(), z.unknown()),
          revision: z.number().int(),
          idempotencyKey: z.string().min(8),
          revert: z.boolean().default(false),
        })
        .parse(req.body);
    return transaction(store, a.scope, async (tx) => {
      const prior = await tx.get("action_" + digest(b.idempotencyKey));
      if (prior) {
        assert(
          prior.data.requestHash === digest(JSON.stringify(b)),
          "Idempotency key reused for different action",
          409,
        );
        return prior;
      }
      const o = await tx.get(b.objectId),
        ws = await tx.get("workspace");
      assert(
        o?.kind === "object" && o.data.generation === ws?.data.activeGeneration,
        "Object unavailable",
        404,
      );
      const type = (await tx.list("objectType")).find(
        (t) =>
          t.data.generation === o.data.generation && t.name === o.data.type,
      );
      assert(type, "Object type missing");
      for (const [key, value] of Object.entries(b.values)) {
        assert(
          type.data.properties.includes(key) && key !== type.data.primaryKey,
          "Invalid editable field",
        );
        const original = o.data.base[key];
        assert(
          value === null ||
            original === null ||
            typeof value === typeof original,
          `Invalid type for ${key}`,
        );
      }
      const overlay = await tx.get("overlay_" + o.data.logicalId);
      assert(
        (overlay?.revision || 0) === b.revision,
        "Object was edited; refresh the preview",
        409,
      );
      const updated =
        overlay ||
        resource(
          a.scope,
          "overlay",
          o.name,
          { values: {} },
          "overlay_" + o.data.logicalId,
        );
      tx.put({
        ...updated,
        revision: b.revision + 1,
        data: {
          values: b.revert ? {} : { ...updated.data.values, ...b.values },
        },
      });
      return tx.put(
        resource(
          a.scope,
          "audit",
          "Updated " + o.name,
          {
            actor: a.user.id,
            objectId: o.id,
            before: { ...o.data.base, ...overlay?.data.values },
            values: b.values,
            requestHash: digest(JSON.stringify(b)),
          },
          "action_" + digest(b.idempotencyKey),
        ),
      );
    });
  });
}

// The best-connected records and those linked to them. Their neighbours come
// from the snapshot, since walking the links of a record with tens of
// thousands of them takes HelixDB a second; records are then read by id. A
// seed the snapshot cannot place, or a failed query, falls back to the walk.
async function bestConnectedNeighbourhood(
  store: Store,
  scope: string,
  best: Resource[],
  counted: Map<string, number>,
  limit: number,
) {
  const seeds = best.map((o) => o.id);
  let near: Awaited<ReturnType<typeof snapshotNeighbours>>;
  try {
    near = await snapshotNeighbours(
      store,
      scope,
      best.map((record) => ({ record, links: counted.get(record.id) || 0 })),
      limit - best.length,
    );
  } catch (e) {
    console.error(
      "Graph: snapshot neighbours:",
      e instanceof Error ? e.message : e,
    );
    return store.neighborhood(scope, seeds, 1, limit);
  }
  const walked = near.unmatched.length
    ? await store.neighborhood(scope, near.unmatched, 1, limit)
    : { nodes: [], edges: [], truncated: false };
  const ids = [...new Set([...seeds, ...near.ids])].slice(0, limit),
    nodes = new Map<string, Resource>();
  for (const o of [...(await store.getMany(scope, ids)), ...walked.nodes])
    if (o && nodes.size < limit) nodes.set(o.id, o);
  return {
    nodes: [...nodes.values()],
    edges: [...near.links, ...walked.edges].filter(
      (e) => nodes.has(e.from) && nodes.has(e.to),
    ),
    truncated:
      near.truncated ||
      walked.truncated ||
      seeds.length + near.ids.length > limit,
  };
}
