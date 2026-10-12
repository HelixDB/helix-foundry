import { it, expect, beforeAll, afterAll } from "vitest";
import { createServer, type Server } from "node:http";
import { once } from "node:events";
import { readFile, rm } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";
import { discover, importSource } from "../apps/api/src/connectors.js";
import { sourceSyncDefaults } from "../apps/api/src/source-sync.js";
import { config } from "../apps/api/src/config.js";
import { SourceSchema } from "../packages/shared/src/index.js";
import { importPosthog } from "../apps/api/src/posthog.js";

const persons = Array.from({ length: 23_456 }, (_, i) => [
  "2025-01-01T00:00:00Z",
  i % 2 === 0,
  JSON.stringify({ email: `u${i}@example.com` }),
  "p" + String(i).padStart(6, "0"),
]);
const today = new Date().toISOString().slice(0, 10);
const events = [
  { uuid: "e2", event: "signup", ts: today + "T01:00:00Z", person: "p000001" },
  {
    uuid: "e1",
    event: "$pageview",
    ts: today + "T02:00:00Z",
    person: "p000001",
  },
  {
    uuid: "e3",
    event: "$pageview",
    ts: today + "T03:00:00Z",
    person: "p000002",
  },
  { uuid: "e0", event: "$pageview", ts: "2000-01-01T00:00:00Z", person: "p1" },
];
const queries: string[] = [];
let server: Server, base: string;

// Interprets just enough of the connector's generated HogQL.
function run(sql: string) {
  const limit = Number(/LIMIT (\d+)$/.exec(sql)![1]);
  const since = /toString\(\w+\) > '([^']*)'\)*$/.exec(
    sql.replace(/ ORDER BY.*/, ""),
  )?.[1];
  const [from, to] = [...sql.matchAll(/toDateTime\('([^']+)', 'UTC'\)/g)].map(
    (m) => new Date(m[1].replace(" ", "T") + "Z").getTime(),
  );
  const inWindow = events.filter(
    (e) => +new Date(e.ts) >= from && +new Date(e.ts) < to,
  );
  if (/^SELECT 1 FROM/.test(sql)) return [[1]];
  if (sql.includes("FROM persons"))
    return persons.filter((p) => !since || p[3] > since).slice(0, limit);
  if (sql.includes("FROM person_distinct_ids")) return [["p000001", "user_1"]];
  if (sql.includes("FROM groups"))
    return [["2025-01-01T00:00:00Z", '{"name":"Acme"}', 0, "org_1"]];
  if (sql.includes("GROUP BY")) {
    const counts = new Map<string, number>();
    for (const e of inWindow)
      counts.set(
        e.event + "|" + e.person,
        (counts.get(e.event + "|" + e.person) || 0) + 1,
      );
    return [...counts].map(([k, n]) => [today, n, ...k.split("|")]);
  }
  return inWindow
    .sort((a, b) => a.uuid.localeCompare(b.uuid))
    .map((e) => [
      e.event,
      e.ts,
      "d_" + e.person,
      e.person,
      JSON.stringify({ $session_id: "s1" }),
      e.uuid,
    ]);
}

beforeAll(async () => {
  server = createServer(async (req, res) => {
    res.setHeader("content-type", "application/json");
    const send = (status: number, body: unknown) => {
      res.statusCode = status;
      res.end(JSON.stringify(body));
    };
    if (req.headers.authorization !== "Bearer phx_good")
      return send(401, { detail: "Invalid key" });
    if (req.url === "/api/projects/")
      return send(200, { results: [{ id: 7, name: "App" }] });
    if (req.url === "/api/projects/7/groups_types/")
      return send(200, [{ group_type: "organization", group_type_index: 0 }]);
    if (req.url === "/api/projects/7/query/" && req.method === "POST") {
      let body = "";
      for await (const chunk of req) body += chunk;
      const sql = JSON.parse(body).query.query;
      queries.push(sql);
      return send(200, { results: run(sql) });
    }
    send(404, { detail: "Not found" });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  base = "http://127.0.0.1:" + (server.address() as any).port;
});
afterAll(async () => {
  server.closeAllConnections();
  await new Promise<void>((r) => server.close(() => r()));
});

const connection = (extra = {}) => ({
  apiKey: "phx_good",
  region: "custom",
  host: base,
  ...extra,
});

async function imported(table: string) {
  const scope = "posthog-" + randomUUID();
  try {
    const result = await importSource(scope, "posthog", connection({ table }));
    const text = await readFile(
      resolve(config.dataDir, "incoming", scope, result.file),
      "utf8",
    );
    return text
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((l) => JSON.parse(l));
  } finally {
    await rm(resolve(config.dataDir, "incoming", scope), {
      recursive: true,
      force: true,
    });
  }
}

it("finds the project and lists queryable objects", async () => {
  const found: any = await discover("posthog", connection());
  expect(found.projectId).toBe("7");
  expect(found.tables.map((t: any) => t.name)).toEqual([
    "persons",
    "person_distinct_ids",
    "groups",
    "events",
    "daily_event_counts",
  ]);
  expect(found.foreignKeys).toContainEqual({
    tableName: "events",
    columnName: "person_id",
    referencedTable: "persons",
    referencedColumn: "person_id",
  });
  await expect(
    discover("posthog", connection({ apiKey: "phx_bad" })),
  ).rejects.toThrow("rejected this key");
  await expect(
    discover("posthog", connection({ apiKey: "phc_project" })),
  ).rejects.toThrow("personal API key");
});

it("pages persons with keyset queries", async () => {
  const rows = await imported("persons");
  expect(rows).toHaveLength(persons.length);
  expect(rows[1]).toMatchObject({
    person_id: "p000001",
    email: "u1@example.com",
    is_identified: false,
  });
  expect(queries.some((q) => q.includes("> 'p009999'"))).toBe(true);
  expect(queries.some((q) => /OFFSET/i.test(q))).toBe(false);
});

it("imports recent events day by day and names group types", async () => {
  const rows = await imported("events");
  expect(rows.map((r) => r.event_id)).toEqual(["e1", "e2", "e3"]);
  expect(rows[0]).toMatchObject({ person_id: "p000001", session_id: "s1" });
  const counts = await imported("daily_event_counts");
  expect(counts).toContainEqual({
    day: today,
    event: "$pageview",
    person_id: "p000001",
    events: 1,
  });
  const [group] = await imported("groups");
  expect(group).toMatchObject({
    group_type: "organization",
    group_key: "org_1",
    name: "Acme",
  });
  const [link] = await imported("person_distinct_ids");
  expect(link).toEqual({ distinct_id: "user_1", person_id: "p000001" });
});

it("refreshes every six hours", () => {
  const next = new Date(sourceSyncDefaults("posthog").nextRun!);
  expect(next.getUTCHours() % 6).toBe(0);
  expect(next.getUTCMinutes()).toBe(0);
});

it.each([
  ["nonnumeric text", "not-a-number", 30],
  ["NaN text", "NaN", 30],
  ["absent", undefined, 30],
  ["null", null, 30],
  ["cleared", "", 30],
  ["numeric zero", 0, 30],
  ["text zero", "0", 1],
  ["negative", -4, 1],
  ["text overrange", "366", 365],
  ["numeric overrange", 366, 365],
  ["positive infinity text", "Infinity", 365],
  ["negative infinity text", "-Infinity", 1],
  ["numeric text", "2", 2],
] as [string, unknown, number][])(
  "preserves PostHog history defaults and clamps: %s",
  async (_label, eventDays, expectedDays) => {
    // The public source routes accept this schema, then pass its config to
    // discovery and to the importer that also serves scheduled refreshes.
    const source = SourceSchema.parse({
      name: "PostHog events",
      kind: "posthog",
      config: connection({ table: "events", projectId: "7", eventDays }),
    });
    const before = queries.length;
    const preview: any = await discover(source.kind, source.config);
    expect(preview.sample.map((row: any) => row.event_id)).toEqual([
      "e1",
      "e2",
      "e3",
    ]);
    const rows: any[] = [];
    await importPosthog(source.config, async (row) => {
      rows.push(row);
    });
    expect(rows.map((row) => row.event_id)).toEqual(["e1", "e2", "e3"]);
    // Preview reads one day; an import reads the configured, clamped window.
    expect(queries.slice(before)).toHaveLength(expectedDays + 1);
  },
);
