import { it, expect, vi, afterEach } from "vitest";
import { randomUUID } from "node:crypto";
import { readFile, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { discover, importSource } from "../apps/api/src/connectors.js";
import { neo4jPaging } from "../apps/api/src/neo4j.js";
import { config } from "../apps/api/src/config.js";
import { sourceSyncDefaults } from "../apps/api/src/source-sync.js";
afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  neo4jPaging.size = 1000;
});
const T = (type: string, value: unknown) => ({ $type: type, _value: value });
const rowsOf = async (scope: string, file: string) =>
  (await readFile(resolve(config.dataDir, "incoming", scope, file), "utf8"))
    .trim()
    .split("\n")
    .map((l) => JSON.parse(l));

// A Neo4j Query API stand-in with two Customer nodes, one Order and a PLACED
// relationship between them.
function fakeNeo4j() {
  const customers = ["4:db:0", "4:db:1"];
  const node = (i: number) => [
    T("String", customers[i]),
    T("Map", {
      customer_id: T("String", "natural-" + i),
      name: T("String", i === 0 ? "東京 café" : "Second"),
      big: T("Integer", "9007199254740993"),
      since: T("Date", "2024-01-02"),
    }),
  ];
  const calls: { url: string; headers: any; body: any }[] = [];
  const fetchMock = vi.fn(async (url: string, init: any) => {
    const body = JSON.parse(init.body),
      s: string = body.statement,
      p = body.parameters || {};
    calls.push({ url, headers: init.headers, body });
    if (init.headers.authorization !== "Basic " + btoa("neo4j:secret"))
      return Response.json(
        {
          errors: [
            {
              code: "Neo.ClientError.Security.Unauthorized",
              message: "Invalid credential.",
            },
          ],
        },
        { status: 401 },
      );
    if (url.includes("/db/missing/")) return new Response("", { status: 404 });
    const reply = (fields: string[], values: unknown[][]) =>
      Response.json({ data: { fields, values } });
    if (s.includes("db.labels"))
      return reply(["label"], [["Customer"], ["Order"]]);
    if (s.includes("db.relationshipTypes"))
      return reply(["type"], [["PLACED"]]);
    if (s.includes("db.schema.visualization"))
      return reply(
        ["nodes", "relationships"],
        [
          [
            [
              {
                elementId: "-1",
                labels: ["Customer"],
                properties: { name: "Customer" },
              },
              {
                elementId: "-2",
                labels: ["Order"],
                properties: { name: "Order" },
              },
            ],
            [
              {
                elementId: "-3",
                startNodeElementId: "-1",
                endNodeElementId: "-2",
                type: "PLACED",
              },
            ],
          ],
        ],
      );
    if (s.includes("nodeTypeProperties"))
      return reply(
        ["nodeLabels", "propertyName", "propertyTypes"],
        [
          [["Customer"], "customer_id", ["String"]],
          [["Customer"], "name", ["String"]],
          [["Customer"], "big", ["Long"]],
          [["Customer"], "since", ["Date"]],
          [["Order"], "amount", ["Double"]],
        ],
      );
    if (s.includes("relTypeProperties"))
      return reply(
        ["relType", "propertyName", "propertyTypes"],
        [[":`PLACED`", "at", ["DateTime"]]],
      );
    if (/RETURN count\(n\)/.test(s))
      return reply(["n"], [[s.includes("`Customer`") ? 2 : 1]]);
    if (/RETURN count\(r\)/.test(s)) return reply(["n"], [[1]]);
    if (s.startsWith("MATCH (n:`Customer`) WHERE elementId(n) > $after")) {
      const rows = customers
        .map((id, i) => [id, node(i)] as const)
        .filter(([id]) => id > p.after)
        .slice(0, p.limit)
        .map(([, r]) => r);
      return reply(["id", "props"], rows);
    }
    if (s.startsWith("MATCH (a)-[r:`PLACED`]->(b)"))
      return reply(
        ["id", "from", "to", "props"],
        p.after
          ? []
          : [
              [
                T("String", "5:db:0"),
                T("String", "4:db:0"),
                T("String", "4:db:9"),
                T("Map", { at: T("OffsetDateTime", "2026-01-01T10:00:00Z") }),
              ],
            ],
      );
    return reply([], []);
  });
  vi.stubGlobal("fetch", fetchMock);
  return { calls, fetchMock };
}
const connection = {
  url: "http://neo4j.test:7474/",
  database: "neo4j",
  user: "neo4j",
  password: "secret",
};

it("discovers labels and relationship types with keys that link them", async () => {
  const { calls } = fakeNeo4j();
  const d: any = await discover("neo4j", {
    ...connection,
    table: "Customer",
    schema: "node",
  });
  expect(calls[0].url).toBe("http://neo4j.test:7474/db/neo4j/query/v2");
  expect(calls[0].headers.accept).toBe("application/vnd.neo4j.query");
  expect(calls.map((c) => c.body.accessMode)).toEqual(calls.map(() => "Read"));
  expect(d.tables).toEqual([
    { name: "Customer", schema: "node", estimatedRows: 2 },
    { name: "Order", schema: "node", estimatedRows: 1 },
    {
      name: "PLACED",
      schema: "relationship",
      estimatedRows: 1,
      from: "Customer",
      to: "Order",
    },
  ]);
  expect(d.foreignKeys).toEqual([
    expect.objectContaining({
      tableName: "PLACED",
      columnName: "customer_id",
      referencedTable: "Customer",
      referencedColumn: "customer_id",
    }),
    expect.objectContaining({
      tableName: "PLACED",
      columnName: "order_id",
      referencedTable: "Order",
      referencedColumn: "order_id",
    }),
  ]);
  expect(d.keys).toContainEqual(
    expect.objectContaining({
      tableName: "Customer",
      columnName: "customer_id",
    }),
  );
  expect(
    d.columns
      .filter((c: any) => c.tableName === "PLACED")
      .map((c: any) => c.name),
  ).toEqual(["placed_id", "customer_id", "order_id", "at"]);
  // The sample has the element id first and keeps the clashing property.
  expect(d.sample[0]).toEqual({
    customer_id: "4:db:0",
    customer_id_property: "natural-0",
    name: "東京 café",
    big: "9007199254740993",
    since: "2024-01-02",
  });
});

it("imports nodes page by page and relationships with their end node ids", async () => {
  const { fetchMock } = fakeNeo4j();
  neo4jPaging.size = 1;
  const scope = "neo4j-" + randomUUID();
  try {
    const nodes = await importSource(scope, "neo4j", {
      ...connection,
      table: "Customer",
      schema: "node",
    });
    expect(nodes.records).toBe(2);
    const rows = await rowsOf(scope, nodes.file);
    expect(rows.map((r) => r.customer_id)).toEqual(["4:db:0", "4:db:1"]);
    expect(rows[1].big).toBe("9007199254740993");
    const pages = fetchMock.mock.calls.filter(([, init]: any) =>
      JSON.parse(init.body).statement.startsWith("MATCH (n:`Customer`)"),
    );
    expect(
      pages.map(([, init]: any) => JSON.parse(init.body).parameters.after),
    ).toEqual(["", "4:db:0", "4:db:1"]);
    // Rows were written, so the graph-wide schema scan is skipped.
    expect(
      fetchMock.mock.calls.some(([, init]: any) =>
        JSON.parse(init.body).statement.includes("TypeProperties"),
      ),
    ).toBe(false);

    const rels = await importSource(scope, "neo4j", {
      ...connection,
      table: "PLACED",
      schema: "relationship",
    });
    expect(await rowsOf(scope, rels.file)).toEqual([
      {
        placed_id: "5:db:0",
        customer_id: "4:db:0",
        order_id: "4:db:9",
        at: "2026-01-01T10:00:00Z",
      },
    ]);
    const empty = await importSource(scope, "neo4j", {
      ...connection,
      table: "Order",
      schema: "node",
    });
    expect(empty.records).toBe(0);
    expect(empty.schema).toEqual([
      { name: "order_id", type: "String" },
      { name: "amount", type: "Double" },
    ]);
  } finally {
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
});

it("names wrong credentials and a missing database", async () => {
  fakeNeo4j();
  await expect(
    discover("neo4j", { ...connection, password: "wrong" }),
  ).rejects.toThrow("Authentication failed");
  await expect(
    discover("neo4j", { ...connection, database: "missing" }),
  ).rejects.toThrow('Database "missing" was not found');
  await expect(
    discover("neo4j", { ...connection, url: "bolt://neo4j.test:7687" }),
  ).rejects.toThrow("HTTP(S) URL");
});

// A real Neo4j 5: docker run -d --name helix-foundry-neo4j-test -p 127.0.0.1:37474:7474 \
//   -e NEO4J_AUTH=neo4j/foundry-test-only neo4j:5-community
it.skipIf(process.env.TEST_NEO4J !== "1")(
  "discovers and imports from a real Neo4j",
  async () => {
    const c = {
      url: process.env.NEO4J_TEST_URL || "http://127.0.0.1:37474",
      database: "neo4j",
      user: "neo4j",
      password: process.env.NEO4J_TEST_PASSWORD || "foundry-test-only",
    };
    // The connector sends read-only queries, so the fixture is written directly.
    const run = async (statement: string) => {
      const r = await fetch(`${c.url}/db/neo4j/query/v2`, {
        method: "POST",
        headers: {
          "content-type": "application/json",
          authorization:
            "Basic " +
            Buffer.from(`${c.user}:${c.password}`).toString("base64"),
        },
        body: JSON.stringify({ statement }),
      });
      expect(r.ok, await r.text()).toBe(true);
    };
    const scope = "neo4j-" + randomUUID();
    try {
      await run(
        "MATCH (n) WHERE n:FoundryCustomer OR n:FoundryOrder DETACH DELETE n",
      );
      await run(
        'CREATE (a:FoundryCustomer {id: 1, name: "東京 café", big: 9007199254740993, since: date("2024-01-02")}) CREATE (b:FoundryOrder {id: 10, amount: 12.5}) CREATE (a)-[:FOUNDRY_PLACED {at: datetime("2026-01-01T10:00:00Z")}]->(b)',
      );
      const d: any = await discover("neo4j", c);
      expect(d.tables).toContainEqual({
        name: "FoundryCustomer",
        schema: "node",
        estimatedRows: 1,
      });
      expect(d.tables).toContainEqual({
        name: "FOUNDRY_PLACED",
        schema: "relationship",
        estimatedRows: 1,
        from: "FoundryCustomer",
        to: "FoundryOrder",
      });
      const nodes = await importSource(scope, "neo4j", {
        ...c,
        table: "FoundryCustomer",
        schema: "node",
      });
      const [row] = await rowsOf(scope, nodes.file);
      expect(row).toMatchObject({
        id: 1,
        name: "東京 café",
        big: "9007199254740993",
        since: "2024-01-02",
      });
      expect(typeof row.foundry_customer_id).toBe("string");
      const rels = await importSource(scope, "neo4j", {
        ...c,
        table: "FOUNDRY_PLACED",
        schema: "relationship",
      });
      const [rel] = await rowsOf(scope, rels.file);
      expect(rel).toMatchObject({
        foundry_customer_id: row.foundry_customer_id,
        at: "2026-01-01T10:00:00Z",
      });
      expect(typeof rel.foundry_order_id).toBe("string");
      await expect(
        discover("neo4j", { ...c, password: "wrong" }),
      ).rejects.toThrow("Authentication failed");
    } finally {
      await run(
        "MATCH (n) WHERE n:FoundryCustomer OR n:FoundryOrder DETACH DELETE n",
      );
      await rm(resolve(config.dataDir, "incoming", scope), {
        recursive: true,
        force: true,
      });
    }
  },
);

it("asks for the URL before trying to connect", async () => {
  const fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  await expect(discover("neo4j", { ...connection, url: "" })).rejects.toThrow(
    "Enter the HTTP URL",
  );
  await expect(
    discover("neo4j", { ...connection, url: "localhost:7474" }),
  ).rejects.toThrow("Use an HTTP(S) URL");
  await expect(
    discover("neo4j", { ...connection, url: "not a url" }),
  ).rejects.toThrow("is not a valid URL");
  expect(fetchMock).not.toHaveBeenCalled();
});

it("refreshes hourly, because each refresh re-reads a whole label", () => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-01-01T10:07:00Z"));
  expect(new Date(sourceSyncDefaults("neo4j").nextRun!).toISOString()).toBe(
    "2026-01-01T11:00:00.000Z",
  );
});
