import type { FastifyInstance } from "fastify";
import { z } from "zod";
import { assert, digest } from "../security.js";
import { resource, transaction, type Store } from "../store.js";
import { effectiveObject } from "../context.js";
import { BundleSchema } from "../../../../packages/shared/src/index.js";
import { saveProposal } from "../assistant.js";

const OntologyDraftSchema = z.object({
  expectedGeneration: z.string().min(1).nullable(),
  ontology: BundleSchema.shape.ontology,
  relationships: BundleSchema.shape.relationships,
});
async function acceptedDefinitions(store: Store, scope: string) {
  const activeGeneration =
    (await store.get(scope, "workspace"))?.data.activeGeneration || null;
  const published = activeGeneration
    ? (await store.list(scope, "proposal")).find(
        (p) => p.data.publishedGeneration === activeGeneration,
      )
    : undefined;
  assert(
    !activeGeneration || published,
    "Published definitions unavailable",
    409,
  );
  const bundle = BundleSchema.parse(
    published?.data.bundle || {
      summary: "Workspace definitions",
      assumptions: [],
      catalog: [],
      pipelines: [],
      ontology: [],
      relationships: [],
      dashboards: [],
      actions: [],
    },
  );
  return { activeGeneration, bundle };
}
import {
  ObjectActionSchema,
  previewObjectAction,
  executeObjectAction,
} from "../object-actions.js";
import type { RequestSchemas, RouteDeps } from "./deps.js";

export const ontologySchemas: RequestSchemas = {
  "POST /api/v1/workspaces/:w/ontology/draft": OntologyDraftSchema,
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

// A readable name for a record: a person's name, a name or title, an email;
// otherwise its type and a shortened key.
const blank = (v: unknown) =>
  v == null || ["", "None", "null", "NaN"].includes(String(v).trim());
export function recordLabel(name: string, type: string, values: object) {
  const entries = Object.entries(values).filter(([, v]) => !blank(v));
  const find = (test: RegExp) => entries.find(([k]) => test.test(k))?.[1];
  const first = find(/(^|_)first_?name$/i),
    last = find(/(^|_)last_?name$/i);
  if (first || last) return [first, last].filter(Boolean).join(" ");
  const named =
    find(/^(name|full_?name|display_?name|title|subject|company(_?name)?)$/i) ??
    find(/(^|_)(name|title)$/i) ??
    // Only the record's own email: requester_email names someone else.
    find(/^e-?mail$/i);
  if (named) return String(named).slice(0, 80);
  const key = name.startsWith(type + " ") ? name.slice(type.length + 1) : name;
  return `${type} ${key.length > 14 ? key.slice(0, 12) + "…" : key}`;
}

// Ontology: object views, neighbors and object actions (edits, create, link).
export function registerOntologyRoutes(
  app: FastifyInstance,
  store: Store,
  deps: RouteDeps,
) {
  const { access } = deps;
  app.get("/api/v1/workspaces/:w/ontology/definitions", async (req) => {
    const a = await access(req);
    return acceptedDefinitions(store, a.scope);
  });
  app.post("/api/v1/workspaces/:w/ontology/draft", async (req) => {
    const a = await access(req, "editor"),
      b = OntologyDraftSchema.parse(req.body),
      current = await acceptedDefinitions(store, a.scope);
    assert(
      current.activeGeneration === b.expectedGeneration,
      "Published definitions changed. Reopen the designer.",
      409,
    );
    return saveProposal(
      store,
      a.scope,
      {
        ...current.bundle,
        summary: "Update ontology mappings",
        ontology: b.ontology,
        relationships: b.relationships,
      },
      undefined,
      {
        expectedGeneration: b.expectedGeneration,
        preserveOntologyMappings: true,
      },
    );
  });
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
  // The record graph: records as nodes and their links as edges. Without a
  // focus it is the whole graph, or its best-connected records when that is
  // too big to draw; with a focus, the records within `depth` links of it.
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
    const objects = new Map(
      (await store.list(a.scope, "object"))
        .filter((o) => o.data.generation === generation)
        .map((o) => [o.id, o]),
    );
    const relations = (await store.list(a.scope, "relation")).filter(
      (r) =>
        r.data.generation === generation &&
        objects.has(r.data.from) &&
        objects.has(r.data.to),
    );
    const adjacent = new Map<string, string[]>();
    for (const r of relations)
      for (const [x, y] of [
        [r.data.from, r.data.to],
        [r.data.to, r.data.from],
      ]) {
        if (!adjacent.has(x)) adjacent.set(x, []);
        adjacent.get(x)!.push(y);
      }
    const degree = (id: string) => adjacent.get(id)?.length || 0;
    let keep: string[];
    if (q.focus) {
      const start = `${generation}_${q.focus}`;
      assert(objects.has(start), "Record unavailable", 404);
      const seen = new Set([start]);
      let frontier = [start];
      for (let d = 0; d < q.depth && seen.size < q.limit; d++) {
        const next: string[] = [];
        for (const id of frontier)
          for (const n of adjacent.get(id) || [])
            if (!seen.has(n) && seen.size < q.limit) {
              seen.add(n);
              next.push(n);
            }
        frontier = next;
      }
      keep = [...seen];
    } else
      keep = [...objects.keys()]
        .sort((x, y) => degree(y) - degree(x) || x.localeCompare(y))
        .slice(0, q.limit);
    const kept = new Set(keep);
    const node = (id: string) => objects.get(id)!;
    return {
      total: objects.size,
      nodes: keep.map((id) => {
        const o = node(id);
        return {
          id: o.data.logicalId,
          type: o.data.type,
          name: o.name,
          label: recordLabel(o.name, o.data.type, o.data.base || {}),
          degree: degree(id),
        };
      }),
      edges: relations
        .filter((r) => kept.has(r.data.from) && kept.has(r.data.to))
        .map((r) => ({
          from: node(r.data.from).data.logicalId,
          to: node(r.data.to).data.logicalId,
          name: r.name,
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
