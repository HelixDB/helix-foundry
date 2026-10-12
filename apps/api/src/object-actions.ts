import { z } from "zod";
import type { Resource } from "../../../packages/shared/src/index.js";
import { resource, transaction, type Store, type Tx } from "./store.js";
import { assert, digest } from "./security.js";
export const ObjectActionSchema = z.discriminatedUnion("operation", [
  z.object({
    operation: z.literal("create"),
    objectType: z.string(),
    values: z.record(z.string(), z.unknown()),
  }),
  z.object({
    operation: z.enum(["link", "unlink"]),
    name: z.string().min(1).max(120),
    from: z.string(),
    to: z.string(),
  }),
]);
export type ObjectAction = z.infer<typeof ObjectActionSchema>;
async function resolveAction(
  store: Pick<Store, "get" | "list" | "getMany" | "relationsBetween">,
  scope: string,
  action: ObjectAction,
) {
  const ws = await store.get(scope, "workspace");
  assert(ws?.data.activeGeneration, "Publish an ontology first");
  const generation = ws.data.activeGeneration;
  if (action.operation === "create") {
    const type = (await store.list(scope, "objectType")).find(
      (t) => t.data.generation === generation && t.name === action.objectType,
    );
    assert(type, "Object type unavailable");
    assert(
      Object.keys(action.values).every((k) => type.data.properties.includes(k)),
      "Unknown object property",
    );
    const key = action.values[type.data.primaryKey];
    assert(
      key !== null && key !== undefined && String(key).length > 0,
      "A primary key is required",
    );
    const logicalId = digest(type.data.typeId + ":" + String(key)).slice(0, 32);
    assert(
      !(await store.get(scope, generation + "_" + logicalId)),
      "An object with this key already exists",
      409,
    );
    return {
      generation,
      logicalId,
      typeId: type.data.typeId,
      key: String(key),
      before: null,
      after: action.values,
    };
  }
  const from = await store.get(scope, action.from),
    to = await store.get(scope, action.to);
  assert(
    from?.kind === "object" &&
      to?.kind === "object" &&
      from.data.generation === generation &&
      to.data.generation === generation,
    "Objects unavailable",
    404,
  );
  // Read from the record's own links, not every relation in the workspace.
  const relations = (
    await store.getMany(
      scope,
      await store.relationsBetween(scope, from.id, to.id, action.name),
    )
  ).filter(
    (r): r is Resource =>
      !!r &&
      r.data.generation === generation &&
      r.data.from === from.id &&
      r.data.to === to.id,
  );
  const editId =
    "relationEdit_" +
    digest([from.data.logicalId, to.data.logicalId, action.name].join(":"));
  const prior = await store.get(scope, editId);
  return {
    generation,
    fromName: from.name,
    toName: to.name,
    fromLogicalId: from.data.logicalId,
    toLogicalId: to.data.logicalId,
    editId,
    revision: prior?.revision || 0,
    relations: relations.map((r) => r.id),
    before: relations.length > 0,
    after: action.operation === "link",
  };
}
export async function previewObjectAction(
  store: Store,
  scope: string,
  action: ObjectAction,
) {
  const evidence = await resolveAction(store, scope, action);
  return {
    action,
    evidence,
    hash: digest(JSON.stringify({ action, evidence })),
  };
}
export async function executeObjectAction(
  store: Store,
  scope: string,
  action: ObjectAction,
  hash: string,
  idempotencyKey: string,
  actor: string,
) {
  return transaction(store, scope, async (tx) => {
    const auditId = "action_" + digest(idempotencyKey),
      requestHash = digest(JSON.stringify({ action, hash })),
      prior = await tx.get(auditId);
    if (prior) {
      assert(
        prior.data.requestHash === requestHash,
        "Idempotency key reused",
        409,
      );
      return prior;
    }
    const scoped = {
      get: (_s: string, id: string) => tx.get(id),
      list: (_s: string, kind: string) => tx.list(kind),
      getMany: (_s: string, ids: string[]) =>
        Promise.all(ids.map((id) => tx.get(id))),
      relationsBetween: store.relationsBetween.bind(store),
    };
    const evidence: any = await resolveAction(scoped, scope, action);
    assert(
      hash === digest(JSON.stringify({ action, evidence })),
      "Action preview is stale",
      409,
    );
    if (action.operation === "create") {
      const base = {
        type: action.objectType,
        typeId: evidence.typeId,
        key: evidence.key,
        logicalId: evidence.logicalId,
        base: action.values,
        manual: true,
      };
      const type = (await tx.list("objectType")).find(
        (t) =>
          t.data.generation === evidence.generation &&
          t.name === action.objectType,
      );
      if (type)
        tx.update(type, {
          ...type.data,
          objectCount: (type.data.objectCount || 0) + 1,
        });
      tx.put(
        resource(
          scope,
          "manualObject",
          `${action.objectType} ${evidence.key}`,
          base,
          "manual_" + evidence.logicalId,
        ),
      );
      tx.put(
        resource(
          scope,
          "object",
          `${action.objectType} ${evidence.key}`,
          { ...base, generation: evidence.generation },
          evidence.generation + "_" + evidence.logicalId,
        ),
      );
    } else {
      const old = await tx.get(evidence.editId);
      tx.put({
        ...resource(
          scope,
          "relationEdit",
          action.name,
          {
            fromLogicalId: evidence.fromLogicalId,
            toLogicalId: evidence.toLogicalId,
            enabled: action.operation === "link",
          },
          evidence.editId,
        ),
        revision: (old?.revision || 0) + 1,
      });
      for (const id of evidence.relations) tx.delete(id);
      if (action.operation === "link")
        tx.put(
          resource(
            scope,
            "relation",
            action.name,
            {
              from: action.from,
              to: action.to,
              generation: evidence.generation,
            },
            // As publication names it, so the generation's id prefix finds it.
            `${evidence.generation}_r${digest(
              [action.name, evidence.fromLogicalId, evidence.toLogicalId].join(
                ":",
              ),
            ).slice(0, 32)}`,
          ),
        );
    }
    return tx.put(
      resource(
        scope,
        "audit",
        action.operation === "create"
          ? "Created " + action.objectType
          : "Relationship " + action.operation,
        { actor, action, evidence, requestHash },
        auditId,
      ),
    );
  });
}
