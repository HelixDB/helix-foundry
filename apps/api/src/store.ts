import { randomUUID } from "node:crypto";
import {
  Client,
  readBatch,
  writeBatch,
  g,
  SourcePredicate,
  Predicate,
  NodeRef,
  EdgeRef,
  BatchCondition,
  IndexSpec,
} from "@helix-db/helix-db";
import type { Resource } from "../../../packages/shared/src/index.js";
import { config } from "./config.js";
import { AppError } from "./security.js";
export const now = () => new Date().toISOString();
export function resource(
  scope: string,
  kind: string,
  name: string,
  data: Record<string, any> = {},
  id: string = randomUUID(),
): Resource {
  return {
    id,
    workspaceId: scope,
    kind,
    name,
    description: "",
    revision: 1,
    createdAt: now(),
    updatedAt: now(),
    data,
  };
}
export type Change = { put: Resource } | { delete: string };
export interface Store {
  init(): Promise<void>;
  list(scope: string, kind?: string): Promise<Resource[]>;
  get(scope: string, id: string): Promise<Resource | undefined>;
  commit(scope: string, version: number, changes: Change[]): Promise<boolean>;
  version(scope: string): Promise<number>;
  neighbors(scope: string, id: string): Promise<string[]>;
}
const anchor = (key: string) =>
  g().nWithLabelWhere("FoundryRecord", SourcePredicate.eq("key", key));
export class HelixStore implements Store {
  client = Client.server(config.helix);
  async read(q: any): Promise<any> {
    return this.client.query(q.toQueryRequest()).send();
  }
  async write(q: any): Promise<any> {
    return this.client
      .requestBuilder()
      .shouldAwaitDurability(true)
      .query(q.toQueryRequest())
      .send();
  }
  async init() {
    for (const property of ["key", "scope", "scopeKind"])
      await this.write(
        writeBatch()
          .varAs(
            "index",
            g().createIndexIfNotExists(
              property === "key"
                ? IndexSpec.nodeUniqueEquality("FoundryRecord", property)
                : IndexSpec.nodeEquality("FoundryRecord", property),
            ),
          )
          .returning(["index"]),
      );
    await transaction(this, "global", async (tx) => {
      const current = await tx.get("schema");
      if (current && current.data.version > 1)
        throw new Error(
          "Database schema is newer than this application; restore the matching release",
        );
      if (!current)
        tx.put(
          resource(
            "global",
            "migration",
            "Initial workspace schema",
            { version: 1, appliedAt: now() },
            "schema",
          ),
        );
    });
  }
  async list(scope: string, kind?: string) {
    const q = readBatch()
      .varAs(
        "records",
        g()
          .nWithLabelWhere(
            "FoundryRecord",
            SourcePredicate.eq(
              kind ? "scopeKind" : "scope",
              kind ? scope + ":" + kind : scope,
            ),
          )
          .valueMap(["doc"]),
      )
      .returning(["records"]);
    const r = await this.read(q);
    return (r.records || [])
      .filter((x: any) => x.doc)
      .map((x: any) => JSON.parse(x.doc)) as Resource[];
  }
  async get(scope: string, id: string) {
    const r = await this.read(
      readBatch()
        .varAs("records", anchor(scope + ":" + id).valueMap(["doc"]))
        .returning(["records"]),
    );
    return r.records?.[0]?.doc
      ? (JSON.parse(r.records[0].doc) as Resource)
      : undefined;
  }
  async version(scope: string) {
    const r = await this.read(
      readBatch()
        .varAs("v", anchor(scope + ":__revision").valueMap(["version"]))
        .returning(["v"]),
    );
    return Number(r.v?.[0]?.version || 0);
  }
  async commit(scope: string, version: number, changes: Change[]) {
    changes = [
      ...new Map(
        changes.map((c) => ["put" in c ? c.put.id : c.delete, c]),
      ).values(),
    ];
    let existingQuery = readBatch();
    const names: string[] = [];
    for (const [i, c] of changes.entries()) {
      const name = "p" + i;
      names.push(name);
      existingQuery = existingQuery.varAs(
        name,
        anchor(scope + ":" + ("put" in c ? c.put.id : c.delete)).valueMap([
          "key",
        ]),
      );
    }
    const present = names.length
      ? await this.read(existingQuery.returning(names))
      : {};
    let q = writeBatch().varAs("clock", anchor(scope + ":__revision"));
    if (version === 0) {
      q = q.varAsIf(
        "guard",
        BatchCondition.varEmpty("clock"),
        g().addN("FoundryRecord", {
          key: scope + ":__revision",
          scope: "__clocks",
          scopeKind: "__clocks:clock",
          version: 1,
        }),
      );
    } else
      q = q.varAs(
        "guard",
        g()
          .n(NodeRef.var("clock"))
          .where(Predicate.eq("version", version))
          .setProperty("version", version + 1),
      );
    for (let i = 0; i < changes.length; i++) {
      const c = changes[i],
        id = "put" in c ? c.put.id : c.delete;
      if ("delete" in c) {
        q = q.varAsIf(
          "de" + i,
          BatchCondition.varNotEmpty("guard"),
          g()
            .eWithLabelWhere(
              "FoundryRelationship",
              SourcePredicate.eq("relationId", id),
            )
            .where(Predicate.eq("scope", scope)),
        );
        // Explicit edge deletion is required by the pinned server release.
        q = q.varAsIf(
          "dropEdge" + i,
          BatchCondition.varNotEmpty("guard"),
          g().dropEdgeById(EdgeRef.var("de" + i)),
        );
        q = q.varAsIf(
          "d" + i,
          BatchCondition.varNotEmpty("guard"),
          anchor(scope + ":" + id).drop(),
        );
        continue;
      }
      const props = {
        key: scope + ":" + id,
        scope,
        scopeKind: scope + ":" + c.put.kind,
        doc: JSON.stringify(c.put),
      };
      const existing = !!present["p" + i]?.length;
      q = existing
        ? q.varAsIf(
            "u" + i,
            BatchCondition.varNotEmpty("guard"),
            anchor(scope + ":" + id)
              .setProperty("doc", props.doc)
              .setProperty("scopeKind", props.scopeKind),
          )
        : q.varAsIf(
            "new" + i,
            BatchCondition.varNotEmpty("guard"),
            g().addN("FoundryRecord", props),
          );
      if (!existing && c.put.kind === "relation") {
        q = q
          .varAsIf(
            "from" + i,
            BatchCondition.varNotEmpty("guard"),
            anchor(scope + ":" + c.put.data.from),
          )
          .varAsIf(
            "to" + i,
            BatchCondition.varNotEmpty("guard"),
            anchor(scope + ":" + c.put.data.to),
          )
          .varAsIf(
            "edge" + i,
            BatchCondition.varNotEmpty("guard"),
            g()
              .n(NodeRef.var("from" + i))
              .addE("FoundryRelationship", NodeRef.var("to" + i), {
                scope,
                generation: c.put.data.generation,
                name: c.put.name,
                relationId: c.put.id,
              }),
          );
      }
    }
    q = q.returning(["guard"]);
    try {
      const r = await this.write(q);
      return !!r.guard?.length;
    } catch (e) {
      if (/conflict|unique/i.test(String(e))) return false;
      throw e;
    }
  }
  async neighbors(scope: string, id: string) {
    const r = await this.read(
      readBatch()
        .varAs(
          "neighbors",
          anchor(scope + ":" + id)
            .both("FoundryRelationship")
            .where(Predicate.eq("scope", scope))
            .dedup()
            .limit(500)
            .valueMap(["doc"]),
        )
        .returning(["neighbors"]),
    );
    return (r.neighbors || []).map((n: any) => JSON.parse(n.doc).id);
  }
}
export class MemoryStore implements Store {
  private rows = new Map<string, Resource>();
  private versions = new Map<string, number>();
  async init() {}
  async list(s: string, k?: string) {
    return structuredClone(
      [...this.rows.values()].filter(
        (r) => r.workspaceId === s && (!k || r.kind === k),
      ),
    );
  }
  async get(s: string, id: string) {
    return structuredClone(this.rows.get(s + ":" + id));
  }
  async version(s: string) {
    return this.versions.get(s) || 0;
  }
  async commit(s: string, v: number, cs: Change[]) {
    if ((this.versions.get(s) || 0) !== v) return false;
    this.versions.set(s, v + 1);
    for (const c of cs) {
      if ("put" in c) this.rows.set(s + ":" + c.put.id, structuredClone(c.put));
      else this.rows.delete(s + ":" + c.delete);
    }
    return true;
  }
  async neighbors(s: string, id: string) {
    return [
      ...new Set(
        (await this.list(s, "relation"))
          .filter((r) => r.data.from === id || r.data.to === id)
          .map((r) => (r.data.from === id ? r.data.to : r.data.from)),
      ),
    ];
  }
}
export class Tx {
  changes: Change[] = [];
  constructor(
    public store: Store,
    public scope: string,
  ) {}
  async get(id: string) {
    const pending = this.changes.findLast(
      (c) => ("put" in c ? c.put.id : c.delete) === id,
    );
    return pending
      ? "put" in pending
        ? pending.put
        : undefined
      : this.store.get(this.scope, id);
  }
  async list(kind?: string) {
    const rows = new Map(
      (await this.store.list(this.scope, kind)).map((r) => [r.id, r]),
    );
    for (const c of this.changes) {
      if ("delete" in c) rows.delete(c.delete);
      else if (!kind || c.put.kind === kind) rows.set(c.put.id, c.put);
      else rows.delete(c.put.id);
    }
    return [...rows.values()];
  }
  put(r: Resource) {
    if (r.workspaceId !== this.scope) throw new Error("Scope mismatch");
    this.changes.push({ put: r });
    return r;
  }
  delete(id: string) {
    this.changes.push({ delete: id });
  }
  update(r: Resource, data: Record<string, any>) {
    return this.put({ ...r, data, revision: r.revision + 1, updatedAt: now() });
  }
}
export async function transaction<T>(
  store: Store,
  scope: string,
  fn: (tx: Tx) => Promise<T>,
) {
  for (let i = 0; i < 5; i++) {
    const v = await store.version(scope),
      tx = new Tx(store, scope),
      result = await fn(tx);
    if (!tx.changes.length || (await store.commit(scope, v, tx.changes)))
      return result;
    await new Promise((r) => setTimeout(r, 20 * (i + 1)));
  }
  throw new AppError(409, "Concurrent change. Please retry.");
}
export const store = new HelixStore();
