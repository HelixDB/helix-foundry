import { AppError, assert } from "./security.js";
import { checkedURL, type ConnectorConfig } from "./connectors.js";
import { retryingFetch } from "./retrying-fetch.js";

const hosts: Record<string, string> = {
  us: "https://us.posthog.com",
  eu: "https://eu.posthog.com",
};
// Queries must finish within PostHog's 10 second limit, so keep pages modest.
const PAGE = 10_000;
const MAX_PAGES = 20_000;
const DAY = 86_400_000;

type Page = (after: unknown[] | null) => string;
type PosthogObject = {
  name: string;
  // Columns selected by every page; the last `keys` columns order the pages.
  keys: number;
  daily?: boolean;
  sql: (window: string) => Page;
  row: (values: any[], groupTypes: Map<number, string>) => object;
};

const literal = (value: unknown) =>
  typeof value === "number" && Number.isFinite(value)
    ? String(value)
    : "'" + String(value).replaceAll("\\", "\\\\").replaceAll("'", "\\'") + "'";
// Keyset condition for `(a, b) > (x, y)` that ClickHouse can use per column.
function after(columns: string[], values: unknown[] | null) {
  if (!values) return "1 = 1";
  return columns
    .map((column, i) =>
      [
        ...columns.slice(0, i).map((c, j) => `${c} = ${literal(values[j])}`),
        `${column} > ${literal(values[i])}`,
      ].join(" AND "),
    )
    .map((clause) => `(${clause})`)
    .join(" OR ");
}
const keyset =
  (select: string, from: string, where: string, columns: string[]): Page =>
  (last) =>
    `SELECT ${select} FROM ${from} WHERE ${where} AND (${after(columns, last)}) ORDER BY ${columns.join(", ")} LIMIT ${PAGE}`;

const json = (value: unknown) => {
  if (typeof value !== "string") return value ?? {};
  try {
    return JSON.parse(value);
  } catch {
    return {};
  }
};

export const posthogObjects: PosthogObject[] = [
  {
    name: "persons",
    keys: 1,
    sql: () =>
      keyset(
        "created_at, is_identified, properties, toString(id)",
        "persons",
        "1 = 1",
        ["toString(id)"],
      ),
    row: ([created_at, is_identified, properties, id]) => {
      const p = json(properties);
      return {
        person_id: id,
        created_at,
        is_identified,
        email: p.email ?? null,
        name: p.name ?? null,
        properties: p,
      };
    },
  },
  {
    // Identified users carry your application's user ID as a distinct ID.
    name: "person_distinct_ids",
    keys: 1,
    sql: () =>
      keyset(
        "toString(person_id), distinct_id",
        "person_distinct_ids",
        "1 = 1",
        ["distinct_id"],
      ),
    row: ([person_id, distinct_id]) => ({ distinct_id, person_id }),
  },
  {
    name: "groups",
    keys: 2,
    sql: () =>
      keyset("created_at, properties, index, key", "groups", "1 = 1", [
        "index",
        "key",
      ]),
    row: ([created_at, properties, index, key], types) => {
      const p = json(properties);
      return {
        group_type: types.get(Number(index)) ?? "group_" + index,
        group_key: key,
        name: p.name ?? null,
        created_at,
        properties: p,
      };
    },
  },
  {
    name: "events",
    keys: 1,
    daily: true,
    sql: (window) =>
      keyset(
        "event, timestamp, distinct_id, toString(person_id), properties, toString(uuid)",
        "events",
        window,
        ["toString(uuid)"],
      ),
    row: ([event, timestamp, distinct_id, person_id, properties, id]) => {
      const p = json(properties);
      return {
        event_id: id,
        event,
        timestamp,
        distinct_id,
        person_id,
        session_id: p.$session_id ?? null,
        current_url: p.$current_url ?? null,
        properties: p,
      };
    },
  },
  {
    name: "daily_event_counts",
    keys: 2,
    daily: true,
    sql: (window) => (last) =>
      `SELECT toString(toDate(timestamp)), count(), event, toString(person_id) FROM events WHERE ${window} AND (${after(["event", "toString(person_id)"], last)}) GROUP BY toDate(timestamp), event, toString(person_id) ORDER BY event, toString(person_id) LIMIT ${PAGE}`,
    row: ([day, events, event, person_id]) => ({
      day,
      event,
      person_id,
      events,
    }),
  },
];

const relationshipColumns: Record<string, [string, string, string][]> = {
  person_distinct_ids: [["person_id", "persons", "person_id"]],
  events: [
    ["person_id", "persons", "person_id"],
    ["distinct_id", "person_distinct_ids", "distinct_id"],
  ],
  daily_event_counts: [["person_id", "persons", "person_id"]],
};
export const posthogRelationships = () =>
  Object.entries(relationshipColumns).flatMap(([tableName, refs]) =>
    refs.map(([columnName, referencedTable, referencedColumn]) => ({
      tableName,
      columnName,
      referencedTable,
      referencedColumn,
    })),
  );

function connection(c: ConnectorConfig) {
  const key = String(c.apiKey || "").trim();
  assert(
    /^phx_[A-Za-z0-9_]+$/.test(key),
    "Enter a PostHog personal API key (phx_…)",
  );
  const host = checkedURL(
    c.region === "custom" ? String(c.host || "") : hosts[c.region || "us"],
  );
  return { key, host: host.origin };
}

async function request(
  c: { key: string; host: string },
  path: string,
  body?: unknown,
) {
  const url = new URL(path, c.host);
  const response = await retryingFetch(
    url,
    { Authorization: "Bearer " + c.key, "Content-Type": "application/json" },
    body === undefined
      ? undefined
      : { method: "POST", body: JSON.stringify(body) },
  );
  const data: any = await response.json().catch(() => ({}));
  if (response.status === 401)
    throw new AppError(
      400,
      "PostHog rejected this key. Check it and the region, then test again.",
    );
  if (response.status === 403)
    throw new AppError(
      403,
      "This key is missing a permission. Grant Query Read and Project Read.",
    );
  if (!response.ok)
    throw new Error(
      `PostHog returned HTTP ${response.status}: ${data.detail || ""}`,
    );
  return data;
}

async function project(
  c: { key: string; host: string },
  config: ConnectorConfig,
) {
  if (config.projectId) {
    const id = String(config.projectId).trim();
    assert(
      /^\d+$/.test(id),
      "Project ID is a number, found in project settings",
    );
    return id;
  }
  let projects: { id: number; name: string }[];
  try {
    projects = (await request(c, "/api/projects/")).results || [];
  } catch (e) {
    if (e instanceof AppError && e.statusCode === 403)
      throw new AppError(
        400,
        "Enter the project ID from PostHog project settings",
      );
    throw e;
  }
  assert(projects.length, "This key cannot see any PostHog projects");
  assert(
    projects.length === 1,
    "Enter a project ID: " +
      projects.map((p) => `${p.name} (${p.id})`).join(", "),
  );
  return String(projects[0].id);
}

async function groupTypes(c: { key: string; host: string }, id: string) {
  try {
    const types = await request(c, `/api/projects/${id}/groups_types/`);
    return new Map<number, string>(
      (Array.isArray(types) ? types : types.results || []).map((t: any) => [
        Number(t.group_type_index),
        String(t.group_type),
      ]),
    );
  } catch (e) {
    if (e instanceof AppError) return new Map<number, string>();
    throw e;
  }
}

const query = (c: { key: string; host: string }, id: string, sql: string) =>
  request(c, `/api/projects/${id}/query/`, {
    query: { kind: "HogQLQuery", query: sql },
    name: "foundry import",
  }).then((r) => (r.results || []) as any[][]);

const stamp = (t: number) =>
  `toDateTime(${literal(new Date(t).toISOString().slice(0, 19).replace("T", " "))}, 'UTC')`;
// Events are read one UTC day at a time so each query scans a single partition.
function windows(object: PosthogObject, days: number, now = Date.now()) {
  if (!object.daily) return ["1 = 1"];
  const today = Math.floor(now / DAY) * DAY;
  return Array.from({ length: days }, (_, i) => {
    const start = today - (days - 1 - i) * DAY;
    return `timestamp >= ${stamp(start)} AND timestamp < ${stamp(start + DAY)}`;
  });
}

async function* rows(config: ConnectorConfig, limit = Infinity) {
  const c = connection(config),
    id = await project(c, config),
    object = posthogObjects.find((o) => o.name === config.table);
  assert(object, "Choose a PostHog object");
  const configuredDays = Number(config.eventDays || 30);
  const days = Math.min(
    Math.max(Number.isNaN(configuredDays) ? 30 : configuredDays, 1),
    365,
  );
  const types = object.name === "groups" ? await groupTypes(c, id) : new Map();
  let pages = 0,
    count = 0;
  // Previews read only the most recent day of events.
  const all = windows(object, days);
  for (const window of limit < Infinity ? all.slice(-1) : all) {
    const page = object.sql(window);
    let last: unknown[] | null = null;
    while (true) {
      assert(++pages <= MAX_PAGES, "PostHog import exceeded the page limit");
      const result = await query(c, id, page(last));
      for (const values of result) {
        yield object.row(values, types);
        if (++count >= limit) return;
      }
      if (result.length < PAGE) break;
      last = result.at(-1)!.slice(-object.keys);
    }
  }
}

export async function discoverPosthog(c: ConnectorConfig) {
  if (c.table) {
    const sample = [];
    for await (const row of rows(c, 10)) sample.push(row);
    const names = [...new Set(sample.flatMap((r) => Object.keys(r)))];
    return { sample, columns: names.map((name) => ({ name })) };
  }
  const conn = connection(c),
    id = await project(conn, c);
  // A one-row probe per table confirms the key can query it.
  const probes = await Promise.all(
    [...new Set(["persons", "person_distinct_ids", "groups", "events"])].map(
      async (table) => {
        try {
          await query(conn, id, `SELECT 1 FROM ${table} LIMIT 1`);
          return table;
        } catch (e) {
          if (e instanceof AppError && e.statusCode !== 403) throw e;
          return null;
        }
      },
    ),
  );
  const readable = new Set(probes.filter(Boolean));
  const available = posthogObjects.filter((o) =>
    readable.has(o.daily ? "events" : o.name),
  );
  assert(
    available.length,
    "This key cannot query PostHog data. Grant Query Read and test again.",
  );
  const names = new Set(available.map((o) => o.name));
  return {
    projectId: id,
    tables: available.map((o) => ({ name: o.name })),
    unavailable: posthogObjects
      .filter((o) => !names.has(o.name))
      .map((o) => o.name),
    foreignKeys: posthogRelationships().filter(
      (k) => names.has(k.tableName) && names.has(k.referencedTable),
    ),
  };
}

export async function importPosthog(
  c: ConnectorConfig,
  write: (row: unknown) => Promise<void>,
) {
  for await (const row of rows(c)) await write(row);
  const key = {
    persons: "person_id",
    person_distinct_ids: "distinct_id",
    groups: "group_key",
    events: "event_id",
    daily_event_counts: "day",
  }[String(c.table)];
  return [{ name: key || "id", type: "varchar" }];
}
