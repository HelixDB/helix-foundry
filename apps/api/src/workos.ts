import { AppError, assert } from "./security.js";
import type { ConnectorConfig } from "./connectors.js";
import { retryingFetch } from "./retrying-fetch.js";

// Tests point this at a local fixture; the key is never sent anywhere else.
export const workosApi = { base: "https://api.workos.com" };
const MAX_PAGES = 20_000;

type WorkosObject = {
  name: string;
  path: string;
  key: string;
  // Lists that must be filtered by a parent, fetched once per parent row.
  parent?: { path: string; param: string; column: string };
  // Rows that record membership rather than carry their own ID.
  link?: string;
};
const byOrganization = {
  path: "/organizations",
  param: "organization_id",
  column: "organization_id",
};
const byDirectory = {
  path: "/directories",
  param: "directory",
  column: "directory_id",
};
export const workosObjects: WorkosObject[] = [
  { name: "organizations", path: "/organizations", key: "organization_id" },
  { name: "users", path: "/user_management/users", key: "user_id" },
  {
    name: "organization_memberships",
    path: "/user_management/organization_memberships",
    key: "organization_membership_id",
    parent: byOrganization,
  },
  {
    name: "invitations",
    path: "/user_management/invitations",
    key: "invitation_id",
  },
  { name: "sso_connections", path: "/connections", key: "connection_id" },
  { name: "directories", path: "/directories", key: "directory_id" },
  {
    name: "directory_users",
    path: "/directory_users",
    key: "directory_user_id",
    parent: byDirectory,
  },
  {
    name: "directory_groups",
    path: "/directory_groups",
    key: "directory_group_id",
    parent: byDirectory,
  },
  {
    name: "directory_group_members",
    path: "/directory_users",
    key: "directory_user_id",
    parent: {
      path: "/directory_groups",
      param: "group",
      column: "directory_group_id",
    },
    link: "directory_user_id",
  },
];

const relationshipColumns: Record<string, string[]> = {
  organization_memberships: ["organization_id", "user_id"],
  invitations: ["organization_id"],
  sso_connections: ["organization_id"],
  directories: ["organization_id"],
  directory_users: ["directory_id", "organization_id"],
  directory_groups: ["directory_id", "organization_id"],
  directory_group_members: ["directory_group_id", "directory_user_id"],
};

export function workosRelationships() {
  const primary = new Map(
    workosObjects.filter((o) => !o.link).map((o) => [o.key, o.name]),
  );
  return Object.entries(relationshipColumns).flatMap(([table, columns]) =>
    columns.map((column) => ({
      tableName: table,
      columnName: column,
      referencedTable: primary.get(column)!,
      referencedColumn: column,
    })),
  );
}

export function normalizeWorkos(
  object: WorkosObject,
  row: any,
  parentId?: string,
) {
  if (object.link)
    return { [object.parent!.column]: parentId, [object.link]: row.id };
  const out: Record<string, unknown> = {};
  for (const [field, value] of Object.entries(row)) {
    if (field === "object") continue;
    // `groups` on directory users is deprecated; use directory_group_members.
    if (object.name === "directory_users" && field === "groups") continue;
    out[field === "id" ? object.key : field] = value;
  }
  if (object.parent && parentId !== undefined)
    out[object.parent.column] = parentId;
  return out;
}

function workosKey(c: ConnectorConfig) {
  const key = String(c.apiKey || "").trim();
  assert(/^sk_[A-Za-z0-9_]+$/.test(key), "Enter a WorkOS API key (sk_…)");
  return key;
}

async function request(key: string, path: string, params = {}) {
  const url = new URL(path, workosApi.base);
  for (const [k, v] of Object.entries(params))
    url.searchParams.set(k, String(v));
  const response = await retryingFetch(url, {
    Authorization: "Bearer " + key,
  });
  const body: any = await response.json().catch(() => ({}));
  if (response.status === 401)
    throw new AppError(
      400,
      "WorkOS rejected this key. Check it and test again.",
    );
  if (response.status === 403 || response.status === 404)
    throw new AppError(
      403,
      "This key cannot read that object. Check the WorkOS environment.",
    );
  if (!response.ok)
    throw new Error(
      `WorkOS returned HTTP ${response.status}: ${body.message || ""}`,
    );
  return body;
}

async function* list(
  key: string,
  path: string,
  query: Record<string, string> = {},
) {
  let after: string | undefined;
  for (let page = 0; page < MAX_PAGES; page++) {
    const body = await request(key, path, {
      ...query,
      limit: 100,
      order: "asc",
      ...(after ? { after } : {}),
    });
    assert(Array.isArray(body.data), "Unexpected WorkOS list response");
    for (const row of body.data) yield row;
    after = body.list_metadata?.after || undefined;
    if (!after || !body.data.length) return;
  }
  throw new Error(`WorkOS pagination exceeded ${MAX_PAGES} pages`);
}

async function* rows(key: string, object: WorkosObject) {
  if (!object.parent) {
    for await (const row of list(key, object.path))
      yield normalizeWorkos(object, row);
    return;
  }
  for await (const parent of list(key, object.parent.path))
    for await (const row of list(key, object.path, {
      [object.parent.param]: parent.id,
    }))
      yield normalizeWorkos(object, row, parent.id);
}

function lookup(name: unknown) {
  const object = workosObjects.find((o) => o.name === name);
  assert(object, "Choose a WorkOS object");
  return object;
}

export async function discoverWorkos(c: ConnectorConfig) {
  const key = workosKey(c);
  if (c.table) {
    const sample: Record<string, unknown>[] = [];
    for await (const row of rows(key, lookup(c.table))) {
      sample.push(row);
      if (sample.length === 10) break;
    }
    const names = [...new Set(sample.flatMap((r) => Object.keys(r)))];
    return { sample, columns: names.map((name) => ({ name })) };
  }
  // Filtered lists are readable whenever their parent list is.
  const paths = [
    ...new Set(workosObjects.map((o) => o.parent?.path || o.path)),
  ];
  const readable = new Map(
    await Promise.all(
      paths.map(async (path) => {
        try {
          await request(key, path, { limit: 1 });
          return [path, true] as const;
        } catch (e) {
          if (e instanceof AppError && e.statusCode === 403)
            return [path, false] as const;
          throw e;
        }
      }),
    ),
  );
  const available = workosObjects.filter((o) =>
    readable.get(o.parent?.path || o.path),
  );
  assert(
    available.length,
    "This key cannot read any supported WorkOS objects. Check the key and test again.",
  );
  const names = new Set(available.map((o) => o.name));
  return {
    tables: available.map((o) => ({ name: o.name })),
    unavailable: workosObjects
      .filter((o) => !names.has(o.name))
      .map((o) => o.name),
    foreignKeys: workosRelationships().filter(
      (k) => names.has(k.tableName) && names.has(k.referencedTable),
    ),
  };
}

export async function importWorkos(
  c: ConnectorConfig,
  write: (row: unknown) => Promise<void>,
) {
  const key = workosKey(c),
    object = lookup(c.table);
  for await (const row of rows(key, object)) await write(row);
  return object.link
    ? [
        { name: object.parent!.column, type: "varchar" },
        { name: object.link, type: "varchar" },
      ]
    : [
        { name: object.key, type: "varchar" },
        { name: "created_at", type: "timestamp" },
      ];
}
