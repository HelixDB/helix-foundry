import { randomUUID } from "node:crypto";
import {
  readBatch,
  writeBatch,
  g,
  SourcePredicate,
  Predicate,
  NodeRef,
  EdgeRef,
  BatchCondition,
  IndexSpec,
  HelixError,
  parseJson,
} from "@helix-db/helix-db";
import type { Resource } from "../../../packages/shared/src/index.js";
import { config } from "./config.js";
import { AppError } from "./security.js";
import { oomWatcher } from "./memory.js";
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
// Records within some links of one record, and the links between them.
export type Neighborhood = {
  nodes: Resource[];
  edges: { from: string; to: string; name: string }[];
  truncated: boolean;
};
export interface Store {
  init(): Promise<void>;
  list(scope: string, kind?: string): Promise<Resource[]>;
  get(scope: string, id: string): Promise<Resource | undefined>;
  getMany(scope: string, ids: string[]): Promise<(Resource | undefined)[]>;
  commit(scope: string, version: number, changes: Change[]): Promise<boolean>;
  version(scope: string): Promise<number>;
  neighbors(scope: string, id: string): Promise<string[]>;
  // A publication's records are written outside the workspace's revision
  // guard: nothing reads a generation until a guarded transaction activates
  // it. Objects return the handles that their relations use as endpoints.
  // With `resume`, records an earlier attempt already wrote are skipped.
  stageObjects(
    scope: string,
    objects: Resource[],
    resume: boolean,
  ): Promise<Map<string, string>>;
  stageRelations(
    scope: string,
    relations: { relation: Resource; from: string; to: string }[],
    resume: boolean,
  ): Promise<number>;
  // Bounded reads: they cost what they return, not what the workspace holds.
  neighborhood(
    scope: string,
    ids: string[],
    depth: number,
    limit: number,
  ): Promise<Neighborhood>;
  linkCounts(scope: string, ids: string[]): Promise<Map<string, number>>;
  relationsBetween(
    scope: string,
    from: string,
    to: string,
    name: string,
  ): Promise<string[]>;
  // Records whose ids start with `prefix`, in id order, a page at a time.
  page(
    scope: string,
    prefix: string,
    offset: number,
    limit: number,
  ): Promise<{ items: Resource[]; total: number }>;
  // Removes up to `limit` of a generation's links, then its records; returns
  // how many it removed, 0 once none remain.
  dropGeneration(
    scope: string,
    generation: string,
    limit: number,
  ): Promise<number>;
  // Generations that have records, including ones never published.
  generations(scope: string): Promise<string[]>;
  // Whether the store's indexes are built. HelixDB builds an index in the
  // background: until it is active, lookups scan, and ranges of keys may see
  // only part of the data.
  indexesReady(): Promise<boolean>;
}
const anchor = (key: string) =>
  g().nWithLabelWhere("FoundryRecord", SourcePredicate.eq("key", key));
// Metadata schema version. scripts/backup.sh records it from this line.
export const SCHEMA_VERSION = 1;
export const databaseOutOfMemory =
  "The database stopped because Docker ran out of memory, and is restarting. Retry in a minute. To prevent this, give Docker more memory, or use Claude or OpenAI instead of Local AI.";
// A HelixDB request that failed for a reason worth saying in words: the
// database stopped, refused or dropped the connection, or did not answer in
// time. `transient` failures can be retried; a write may still have
// committed before its connection failed.
export class DatabaseError extends AppError {
  constructor(
    message: string,
    public code: string,
    public transient: boolean,
    cause?: unknown,
  ) {
    super(503, message);
    this.cause = cause;
  }
}
// Time limits for one request. A write waits for durability, and a
// publication batch writes 500 records.
const READ_TIMEOUT = 120_000,
  WRITE_TIMEOUT = 300_000;
const dropped = new Set([
  "ECONNRESET",
  "EPIPE",
  "UND_ERR_SOCKET",
  "UND_ERR_CLOSED",
]);
// Names the cause of a failed request. HelixDB is the largest process in
// Docker, so when memory runs out the kernel often stops it: that is said
// instead of a connection error.
export function databaseError(
  e: any,
  op: "read" | "write",
  ms: number,
  memoryKilled: () => boolean,
): unknown {
  if (e instanceof DatabaseError) return e;
  if (e?.name === "TimeoutError" || e?.name === "AbortError")
    return new DatabaseError(
      `HelixDB did not answer a ${op} within ${Math.round(ms / 1000)} seconds. It may be overloaded. Try again.`,
      "timeout",
      true,
      e,
    );
  const status = e instanceof HelixError ? e.statusCode : undefined;
  if (status && [502, 503, 504].includes(status))
    return new DatabaseError(
      `HelixDB is unavailable (HTTP ${status}). Try again in a minute.`,
      "unavailable",
      true,
      e,
    );
  const network =
    (e instanceof HelixError && e.kind === "Network") ||
    e?.kind === "Network" ||
    (e instanceof TypeError && /fetch failed/i.test(e.message));
  if (!network) return e;
  if (memoryKilled())
    return new DatabaseError(databaseOutOfMemory, "oom", true, e);
  const code = String(e?.cause?.code || e?.cause?.cause?.code || "");
  if (code === "ECONNREFUSED")
    return new DatabaseError(
      "HelixDB is not accepting connections: it is stopped or still starting. Try again in a minute.",
      code,
      true,
      e,
    );
  if (dropped.has(code))
    return new DatabaseError(
      `The connection to HelixDB was dropped during a ${op}. Try again.`,
      code,
      true,
      e,
    );
  return new DatabaseError(
    `Cannot reach HelixDB at ${config.helix}${code ? ` (${code})` : ""}.`,
    code || "network",
    true,
    e,
  );
}
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
// HelixDB's 64-bit node and edge ids, as decimal strings.
const idOf = (row: any) => String(row?.["$id"] ?? "");
const nodeIds = (ids: string[]) => NodeRef.ids(ids.map((id) => BigInt(id)));
const chunks = <T>(items: T[], size: number) =>
  Array.from({ length: Math.ceil(items.length / size) }, (_, i) =>
    items.slice(i * size, (i + 1) * size),
  );
const escapeRegExp = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
// The indexes lookups depend on. Keys in order (a range index; an equality
// index does not serve ranges) find a generation's records by their key
// prefix.
const indexes = () => [
  IndexSpec.nodeUniqueEquality("FoundryRecord", "key"),
  IndexSpec.nodeEquality("FoundryRecord", "scope"),
  IndexSpec.nodeEquality("FoundryRecord", "scopeKind"),
  IndexSpec.nodeRange("FoundryRecord", "key"),
];
export class HelixStore implements Store {
  constructor(private memoryKilled = oomWatcher()) {}
  private built = false;
  // Asking to create an index that exists returns its state: "already_active"
  // once its build is done.
  async indexesReady() {
    if (this.built) return true;
    for (const index of indexes()) {
      const r = await this.write(
        writeBatch()
          .varAs("index", g().createIndexIfNotExists(index))
          .returning(["index"]),
      );
      const receipt = Array.isArray(r?.index) ? r.index[0] : r?.index;
      if (receipt?.kind !== "already_active") return false;
    }
    return (this.built = true);
  }
  async read(q: any): Promise<any> {
    return this.send(q, "read");
  }
  async write(q: any): Promise<any> {
    return this.send(q, "write");
  }
  // One request with a time limit and the cause of any failure kept. Reads
  // are repeated while HelixDB restarts or drops the connection, for up to
  // about half a minute; writes never are, since one may already have
  // committed.
  async send(q: any, op: "read" | "write"): Promise<any> {
    const body = q.toQueryRequest().toJsonString(),
      url = new URL("/v2/query", config.helix).toString();
    for (let attempt = 1; ; attempt++) {
      const started = Date.now();
      try {
        const response = await fetch(url, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            ...(op === "write" ? { "x-helix-await-durable": "true" } : {}),
          },
          body,
          signal: AbortSignal.timeout(
            op === "write" ? WRITE_TIMEOUT : READ_TIMEOUT,
          ),
        });
        if (response.status === 204) return undefined;
        const text = await response.text();
        if (response.status !== 200)
          throw HelixError.remote(
            response.status,
            text,
            response.statusText || `HTTP ${response.status}`,
          );
        return parseJson(text);
      } catch (e) {
        const ms = Date.now() - started,
          error = databaseError(e, op, ms, this.memoryKilled);
        if (error instanceof DatabaseError)
          console.error(
            JSON.stringify({
              level: "warn",
              msg: "HelixDB request failed",
              op,
              code: error.code,
              ms,
              attempt,
            }),
          );
        if (
          op === "write" ||
          !(error instanceof DatabaseError) ||
          !error.transient ||
          error.code === "timeout" ||
          attempt >= 6
        )
          throw error;
        await sleep(Math.min(15_000, 1000 * 2 ** (attempt - 1)));
      }
    }
  }
  async init() {
    // HelixDB builds an index in the background, and while writes keep
    // arriving the build keeps catching up: until it is active, lookups scan.
    for (const index of indexes())
      await this.write(
        writeBatch()
          .varAs("index", g().createIndexIfNotExists(index))
          .returning(["index"]),
      );
    await transaction(this, "global", async (tx) => {
      const current = await tx.get("schema");
      if (current && current.data.version > SCHEMA_VERSION)
        throw new Error(
          "Database schema is newer than this application; restore the matching release",
        );
      if (!current)
        tx.put(
          resource(
            "global",
            "migration",
            "Initial workspace schema",
            { version: SCHEMA_VERSION, appliedAt: now() },
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
  async getMany(scope: string, ids: string[]) {
    const found = new Map<string, Resource>();
    for (const part of chunks([...new Set(ids)], 200)) {
      let q = readBatch();
      part.forEach((id, i) => {
        q = q.varAs("r" + i, anchor(scope + ":" + id).valueMap(["doc"]));
      });
      const r = await this.read(q.returning(part.map((_, i) => "r" + i)));
      part.forEach((id, i) => {
        const doc = r["r" + i]?.[0]?.doc;
        if (doc) found.set(id, JSON.parse(doc));
      });
    }
    return ids.map((id) => found.get(id));
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
  private props(scope: string, r: Resource) {
    return {
      key: scope + ":" + r.id,
      scope,
      scopeKind: scope + ":" + r.kind,
      doc: JSON.stringify(r),
    };
  }
  // Node handles of the records that exist, by record id.
  private async handles(scope: string, ids: string[]) {
    const found = new Map<string, string>();
    for (const part of chunks(ids, 500)) {
      let q = readBatch();
      part.forEach((id, i) => {
        q = q.varAs("h" + i, anchor(scope + ":" + id).valueMap(["$id"]));
      });
      const r = await this.read(q.returning(part.map((_, i) => "h" + i)));
      part.forEach((id, i) => {
        const row = r["h" + i]?.[0];
        if (row) found.set(id, idOf(row));
      });
    }
    return found;
  }
  async stageObjects(scope: string, objects: Resource[], resume: boolean) {
    const handles = resume
      ? await this.handles(
          scope,
          objects.map((o) => o.id),
        )
      : new Map<string, string>();
    const todo = objects.filter((o) => !handles.has(o.id));
    await this.inParts(todo, async (part) => {
      let q = writeBatch();
      part.forEach((o, i) => {
        q = q.varAs("n" + i, g().addN("FoundryRecord", this.props(scope, o)));
      });
      const r = await this.write(q.returning(part.map((_, i) => "n" + i)));
      part.forEach((o, i) => handles.set(o.id, idOf(r?.["n" + i]?.[0])));
      return part;
    });
    return handles;
  }
  async stageRelations(
    scope: string,
    relations: { relation: Resource; from: string; to: string }[],
    resume: boolean,
  ) {
    const present = resume
      ? await this.handles(
          scope,
          relations.map((r) => r.relation.id),
        )
      : new Map<string, string>();
    const todo = relations.filter((r) => !present.has(r.relation.id));
    await this.inParts(todo, async (part) => {
      let q = writeBatch();
      part.forEach(({ relation, from, to }, i) => {
        q = q
          .varAs(
            "r" + i,
            g().addN("FoundryRecord", this.props(scope, relation)),
          )
          .varAs(
            "e" + i,
            g()
              .n(NodeRef.id(BigInt(from)))
              .addE("FoundryRelationship", NodeRef.id(BigInt(to)), {
                scope,
                generation: relation.data.generation,
                name: relation.name,
                relationId: relation.id,
              }),
          );
      });
      await this.write(q.returning(["r0"]));
      return part;
    });
    return todo.length;
  }
  async neighborhood(
    scope: string,
    ids: string[],
    depth: number,
    limit: number,
  ) {
    let q = readBatch();
    ids.forEach((id, i) => {
      q = q.varAs("s" + i, anchor(scope + ":" + id).valueMap(["$id", "doc"]));
    });
    const start = await this.read(q.returning(ids.map((_, i) => "s" + i)));
    const docs = new Map<string, Resource>();
    for (let i = 0; i < ids.length && docs.size < limit; i++) {
      const row = start["s" + i]?.[0];
      if (row) docs.set(idOf(row), JSON.parse(row.doc));
    }
    if (!docs.size) return { nodes: [], edges: [], truncated: false };
    const links = new Map<string, { from: string; to: string; name: string }>();
    let frontier = [...docs.keys()],
      truncated = false;
    for (let d = 0; d < depth && frontier.length; d++) {
      // Each level reads at most a few links per record it may still add.
      const budget = Math.max(50, (limit - docs.size) * 2);
      const r = await this.read(
        readBatch()
          .varAs(
            "e",
            g()
              .n(nodeIds(frontier))
              .bothE("FoundryRelationship")
              .limit(budget)
              .valueMap(["$id", "$from", "$to", "name"]),
          )
          .returning(["e"]),
      );
      const edges: any[] = r.e || [];
      if (edges.length >= budget) truncated = true;
      const next: string[] = [];
      for (const e of edges) {
        const from = String(e["$from"]),
          to = String(e["$to"]);
        links.set(idOf(e), { from, to, name: String(e.name) });
        for (const n of [from, to])
          if (!docs.has(n) && !next.includes(n)) {
            if (docs.size + next.length < limit) next.push(n);
            else truncated = true;
          }
      }
      if (!next.length) break;
      const n = await this.read(
        readBatch()
          .varAs("n", g().n(nodeIds(next)).valueMap(["$id", "doc"]))
          .returning(["n"]),
      );
      for (const row of n.n || []) docs.set(idOf(row), JSON.parse(row.doc));
      frontier = next;
    }
    return {
      nodes: [...docs.values()],
      edges: [...links.values()]
        .filter((e) => docs.has(e.from) && docs.has(e.to))
        .map((e) => ({
          from: docs.get(e.from)!.id,
          to: docs.get(e.to)!.id,
          name: e.name,
        })),
      truncated,
    };
  }
  async linkCounts(scope: string, ids: string[]) {
    const counts = new Map<string, number>();
    for (const part of chunks(ids, 200)) {
      let q = readBatch();
      part.forEach((id, i) => {
        q = q.varAs(
          "c" + i,
          anchor(scope + ":" + id)
            .bothE("FoundryRelationship")
            .count(),
        );
      });
      const r = await this.read(q.returning(part.map((_, i) => "c" + i)));
      part.forEach((id, i) => {
        let v = r["c" + i];
        if (Array.isArray(v)) v = v[0];
        if (v && typeof v === "object") v = Object.values(v)[0];
        counts.set(id, Number(v) || 0);
      });
    }
    return counts;
  }
  async relationsBetween(
    scope: string,
    from: string,
    to: string,
    name: string,
  ) {
    const r = await this.read(
      readBatch()
        .varAs("to", anchor(scope + ":" + to).valueMap(["$id"]))
        .varAs(
          "e",
          anchor(scope + ":" + from)
            .outE("FoundryRelationship")
            .where(Predicate.eq("name", name))
            .valueMap(["$to", "relationId"]),
        )
        .returning(["to", "e"]),
    );
    const target = idOf(r.to?.[0]);
    return (r.e || [])
      .filter((e: any) => target && String(e["$to"]) === target)
      .map((e: any) => String(e.relationId));
  }
  async page(scope: string, prefix: string, offset: number, limit: number) {
    const from = `${scope}:${prefix}`,
      range = () =>
        g().nWithLabelWhere(
          "FoundryRecord",
          SourcePredicate.between("key", from, from + "\uffff"),
        );
    const r = await this.read(
      readBatch()
        .varAs(
          "items",
          // A range index scan reads keys in order.
          range()
            .range(offset, offset + limit)
            .valueMap(["doc"]),
        )
        .varAs("total", range().count())
        .returning(["items", "total"]),
    );
    let total = r.total;
    if (Array.isArray(total)) total = total[0];
    if (total && typeof total === "object") total = Object.values(total)[0];
    return {
      items: (r.items || []).map((x: any) => JSON.parse(x.doc) as Resource),
      total: Number(total) || 0,
    };
  }
  async dropGeneration(scope: string, generation: string, limit: number) {
    // A generation's records all have keys starting with it. The links of
    // the first of them go first, since the pinned server keeps a link whose
    // record is dropped, with the relation records of links from before
    // relation ids carried their generation; then those records.
    const prefix = `${scope}:${generation}_`,
      first = () =>
        g()
          .nWithLabelWhere(
            "FoundryRecord",
            SourcePredicate.between("key", prefix, prefix + "\uffff"),
          )
          .limit(limit);
    const r = await this.read(
      readBatch()
        .varAs("n", first().valueMap(["$id"]))
        .varAs(
          "e",
          first()
            .bothE("FoundryRelationship")
            .limit(limit)
            .valueMap(["$id", "relationId"]),
        )
        .returning(["n", "e"]),
    );
    const links: [string, string][] = [
      ...new Map<string, string>(
        (r.e || []).map((x: any) => [idOf(x), String(x.relationId)]),
      ).entries(),
    ];
    if (links.length) {
      await this.inParts(links, async (part) => {
        let q = writeBatch().varAs(
          "links",
          g().dropEdgeById(EdgeRef.ids(part.map(([id]) => BigInt(id)))),
        );
        [...new Set(part.map(([, relation]) => relation))].forEach((id, i) => {
          q = q.varAs("r" + i, anchor(scope + ":" + id).drop());
        });
        await this.write(q.returning(["links"]));
        return part;
      });
      return links.length;
    }
    const nodes: string[] = (r.n || []).map(idOf);
    if (!nodes.length) return 0;
    await this.inParts(nodes, async (part) => {
      await this.write(
        writeBatch()
          .varAs("records", g().n(nodeIds(part)).drop())
          .returning(["records"]),
      );
      return part;
    });
    return nodes.length;
  }
  // HelixDB's planner sometimes fails to plan a large write
  // (internal_planner_error, "unsupported cascades plan"). Nothing was
  // written then, and the same request would fail again, so it is written in
  // halves.
  private async inParts<T, R>(
    items: T[],
    write: (part: T[]) => Promise<R[]>,
  ): Promise<R[]> {
    // HelixDB refuses a batch with no entries.
    if (!items.length) return [];
    try {
      return await write(items);
    } catch (e) {
      const planner =
        e instanceof HelixError &&
        (e.code === "internal_planner_error" ||
          /unsupported cascades plan/i.test(e.serverMessage || e.message));
      if (items.length < 2 || !planner) throw e;
      const half = Math.ceil(items.length / 2);
      return [
        ...(await this.inParts(items.slice(0, half), write)),
        ...(await this.inParts(items.slice(half), write)),
      ];
    }
  }
  // Walks the workspace's keys in order, reading keys only, and skips each
  // generation's records once its first one is found. Relations from before
  // relation ids carried their generation are stepped through.
  async generations(scope: string) {
    const found = new Set<string>(),
      staged = new RegExp(
        `^${escapeRegExp(scope)}:([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})_`,
      ),
      end = scope + ":\uffff",
      page = 1000;
    let cursor = scope + ":";
    for (;;) {
      const r = await this.read(
        readBatch()
          .varAs(
            "k",
            g()
              .nWithLabelWhere(
                "FoundryRecord",
                SourcePredicate.between("key", cursor, end),
              )
              .limit(page)
              .valueMap(["key"]),
          )
          .returning(["k"]),
      );
      const keys: string[] = (r.k || []).map((x: any) => String(x.key)).sort();
      for (const key of keys) {
        const m = staged.exec(key);
        if (m) found.add(m[1]);
      }
      if (keys.length < page) break;
      const last = keys.at(-1)!,
        m = staged.exec(last);
      cursor = m ? `${scope}:${m[1]}_\uffff` : last + "\u0000";
    }
    return [...found];
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
  async getMany(s: string, ids: string[]) {
    return Promise.all(ids.map((id) => this.get(s, id)));
  }
  // Staged records skip the revision counter, as in HelixDB. A handle is the
  // record's id.
  async stageObjects(s: string, objects: Resource[], resume: boolean) {
    const handles = new Map<string, string>();
    for (const o of objects) {
      if (!(resume && this.rows.has(s + ":" + o.id)))
        this.rows.set(s + ":" + o.id, structuredClone(o));
      handles.set(o.id, o.id);
    }
    return handles;
  }
  async stageRelations(
    s: string,
    relations: { relation: Resource; from: string; to: string }[],
    resume: boolean,
  ) {
    let written = 0;
    for (const { relation } of relations)
      if (!(resume && this.rows.has(s + ":" + relation.id))) {
        this.rows.set(s + ":" + relation.id, structuredClone(relation));
        written++;
      }
    return written;
  }
  private links(s: string) {
    return [...this.rows.values()].filter(
      (r) => r.workspaceId === s && r.kind === "relation",
    );
  }
  async neighborhood(s: string, ids: string[], depth: number, limit: number) {
    const kept = new Map<string, Resource>();
    for (const id of ids) {
      const o = await this.get(s, id);
      if (o && kept.size < limit) kept.set(id, o);
    }
    if (!kept.size) return { nodes: [], edges: [], truncated: false };
    const relations = this.links(s);
    let frontier = [...kept.keys()],
      truncated = false;
    for (let d = 0; d < depth && frontier.length; d++) {
      const next: string[] = [];
      for (const r of relations)
        for (const [a, b] of [
          [r.data.from, r.data.to],
          [r.data.to, r.data.from],
        ])
          if (frontier.includes(a) && !kept.has(b)) {
            const o = await this.get(s, b);
            if (!o) continue;
            if (kept.size < limit) {
              kept.set(b, o);
              next.push(b);
            } else truncated = true;
          }
      frontier = next;
    }
    return {
      nodes: [...kept.values()],
      edges: relations
        .filter((r) => kept.has(r.data.from) && kept.has(r.data.to))
        .map((r) => ({ from: r.data.from, to: r.data.to, name: r.name })),
      truncated,
    };
  }
  async linkCounts(s: string, ids: string[]) {
    const counts = new Map(ids.map((id) => [id, 0]));
    for (const r of this.links(s))
      for (const end of [r.data.from, r.data.to])
        if (counts.has(end)) counts.set(end, counts.get(end)! + 1);
    return counts;
  }
  async relationsBetween(s: string, from: string, to: string, name: string) {
    return this.links(s)
      .filter(
        (r) => r.data.from === from && r.data.to === to && r.name === name,
      )
      .map((r) => r.id);
  }
  async page(s: string, prefix: string, offset: number, limit: number) {
    const rows = [...this.rows.values()]
      .filter((r) => r.workspaceId === s && r.id.startsWith(prefix))
      .sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
    return {
      items: structuredClone(rows.slice(offset, offset + limit)),
      total: rows.length,
    };
  }
  async dropGeneration(s: string, generation: string, limit: number) {
    const staged = [...this.rows.entries()]
      .filter(
        ([, r]) =>
          r.workspaceId === s &&
          ["object", "relation"].includes(r.kind) &&
          r.data.generation === generation,
      )
      // Links first, as in HelixDB.
      .sort(
        ([, a], [, b]) =>
          Number(b.kind === "relation") - Number(a.kind === "relation"),
      )
      .slice(0, limit);
    for (const [key] of staged) this.rows.delete(key);
    return staged.length;
  }
  async indexesReady() {
    return true;
  }
  async generations(s: string) {
    return [
      ...new Set(
        [...this.rows.values()]
          .filter(
            (r) =>
              r.workspaceId === s &&
              ["object", "relation"].includes(r.kind) &&
              r.data.generation,
          )
          .map((r) => r.data.generation as string),
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
