import { beforeAll, afterAll, it, expect } from "vitest";
import Fastify from "fastify";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { FoundryClient } from "@helix-foundry/sdk";

let app: Awaited<ReturnType<typeof createApp>>;
const connector = Fastify({ logger: false });
let connectorOrigin: string, origin: string, scope: string;
let writerToken: string, readerToken: string;
let restRequests = 0;
const calls: { statement: string; accessMode: string; accept: string }[] = [];
const typed = (type: string, value: unknown) => ({ $type: type, _value: value });

beforeAll(async () => {
  // Protocol stand-in only: no real Neo4j, provider, user keys or database.
  connector.post("/db/neo4j/query/v2", async (req, reply) => {
    if (req.headers.authorization !== "Basic " + Buffer.from("synthetic:synthetic-only").toString("base64"))
      return reply.code(401).send({ errors: [{ message: "Synthetic authentication rejected" }] });
    const body = req.body as { statement: string; accessMode: string };
    calls.push({ statement: body.statement, accessMode: body.accessMode, accept: String(req.headers.accept) });
    const result = (fields: string[], values: unknown[][]) => ({ data: { fields, values } });
    if (body.statement.includes("db.labels")) return result(["label"], [[typed("String", "Customer")]]);
    if (body.statement.includes("db.relationshipTypes")) return result(["type"], []);
    if (body.statement.includes("db.schema.visualization")) return result(["nodes", "relationships"], [[[{ elementId: "-1", labels: ["Customer"], properties: { name: "Customer" } }], []]]);
    if (body.statement.includes("db.schema.nodeTypeProperties")) return result(["nodeLabels", "propertyName", "propertyTypes"], [[typed("List", [typed("String", "Customer")]), typed("String", "name"), typed("List", [typed("String", "String")])]]);
    if (body.statement.includes("db.schema.relTypeProperties")) return result(["relType", "propertyName", "propertyTypes"], []);
    if (body.statement.includes("RETURN count(n)")) return result(["n"], [[typed("Integer", "1")]]);
    if (body.statement.startsWith("MATCH (n:`Customer`) WHERE elementId(n) > $after")) return result(["id", "props"], [[typed("String", "4:standin:0"), typed("Map", { name: typed("String", "Local fixture") })]]);
    return reply.code(400).send({ errors: [{ message: "Unexpected fixture query" }] });
  });
  connector.get("/records", async () => { restRequests++; return [{ id: 1, category: "Local fixture" }]; });
  connectorOrigin = await connector.listen({ host: "127.0.0.1", port: 0 });
  app = await createApp(new MemoryStore());
  origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const me = await app.inject("/api/v1/me");
  expect(me.statusCode).toBe(200);
  scope = me.json().workspaces[0].id;
  for (const readOnly of [false, true]) {
    const r = await app.inject({ method: "POST", url: `/api/v1/workspaces/${scope}/tokens`, payload: { name: "SDK source fixture", readOnly } });
    expect(r.statusCode).toBe(200);
    if (readOnly) readerToken = r.json().token;
    else writerToken = r.json().token;
  }
});
afterAll(async () => {
  await app?.close();
  await connector.close();
});
const client = (token = writerToken) => new FoundryClient({ baseUrl: origin, workspaceId: scope, token });

it("creates the already supported Neo4j source through the real public SDK and native HTTP route", async () => {
  const source = await client().createSource({
    name: "Typed Neo4j source", kind: "neo4j", schedule: null,
    config: { url: connectorOrigin, database: "neo4j", user: "synthetic", password: "synthetic-only", table: "Customer", schema: "node" },
  });
  expect(source.kind).toBe("source");
  expect(source.data.kind).toBe("neo4j");
  expect(source.data.schedule).toBeNull();
  expect(source.data).not.toHaveProperty("secret");
  expect(source.data.discovery.keys).toEqual([expect.objectContaining({ tableName: "Customer", columnName: "customer_id" })]);
  expect(calls.length).toBeGreaterThan(0);
  expect(calls.every(c => c.accessMode === "Read")).toBe(true);
  expect(calls.some(c => c.accept === "application/vnd.neo4j.query")).toBe(true);
  expect(calls.some(c => c.statement.includes("RETURN count(n)"))).toBe(true);
  expect((await client().get("source", source.id)).id).toBe(source.id);
});

it("keeps omitted and null Automatic cadence and explicit cron through the existing REST source route", async () => {
  const omitted = await client().createSource({ name: "Omitted cadence", kind: "rest", config: { url: connectorOrigin + "/records" } });
  const automatic = await client().createSource({ name: "Automatic cadence", kind: "rest", config: { url: connectorOrigin + "/records" }, schedule: null });
  const scheduled = await client().createSource({ name: "Explicit cadence", kind: "rest", config: { url: connectorOrigin + "/records" }, schedule: "0 * * * *" });
  expect(omitted.data.schedule).toBeNull();
  expect(automatic.data.schedule).toBeNull();
  expect(scheduled.data.schedule).toBe("0 * * * *");
  for (const source of [omitted, automatic, scheduled]) {
    expect(source.data.sync.mode).toBe("snapshot");
    expect(source.data.nextRun).toBeTruthy();
    expect(source.data).not.toHaveProperty("secret");
  }
});

it("preserves authorization and server-side unsupported kind validation", async () => {
  const before = calls.length, beforeRest = restRequests;
  await expect(client(readerToken).createSource({ name: "Read-only denied", kind: "rest", config: { url: connectorOrigin + "/records" }, schedule: null })).rejects.toMatchObject({ status: 403 });
  expect(calls).toHaveLength(before);
  expect(restRequests).toBe(beforeRest);
  const invalid = await app.inject({ method: "POST", url: `/api/v1/workspaces/${scope}/sources`, payload: { name: "Unsupported connector", kind: "unsupported", config: {} }, headers: { authorization: "Bearer " + writerToken } });
  expect(invalid.statusCode).toBe(400);
});
