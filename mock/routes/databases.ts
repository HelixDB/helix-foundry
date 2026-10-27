import type {
  CreateTenantResponse,
  ListProjectsResponse,
  ListTenantsResponse,
  Project,
  ResourceStatus,
  Tenant,
} from "../../src/shared/wfe";
import { runQuery } from "../graph/query";
import { MockGraph } from "../graph/store";
import { fail, id, now, ok, type MockReply, type MockRoute } from "../http";
import { LOCAL_TOKEN, config, isReply, membership, onReset } from "../state";
import { manages } from "./workspaces";

// Helix's own projects, databases (tenants) and query broker, as
// the Helix WFE serves them to the Helix dashboard: GET/POST /v1/projects,
// GET/POST /v1/tenants and POST /v1/databases:query-read. Each tenant holds a
// graph (../graph/store.ts) that query-read runs v3 reads over
// (../graph/query.ts) and that loads write into (./loads.ts).
//
// - Acme has Production (contracts and sales, bound to the seeded
//   ontologies; orders, which already holds the team's own data) and
//   Staging (sandbox, empty; old-orders, deleted). A new workspace has no
//   project.
// - CreateTenant puts the database on the shared cluster, which needs a Helix
//   subscription. Acme has one, and a workspace that has never had one gets
//   its free Helix plan first, as CreateTenant grants a Foundry workspace's
//   (grantFoundryHelixPlan), so every workspace has one. `POST /__mock/config
//   {"helixSubscription": false}` lapses them all: a lapsed plan is a paid
//   decision, never granted again, so CreateTenant is FAILED_PRECONDITION
//   with ErrorInfo NO_WORKSPACE_SUBSCRIPTION (state "expired"), as
//   requireTenantSubscription answers. A local API (mock/state.ts) has no
//   subscriptions and never asks.
// - query-read needs membership of the database's workspace; a cluster
//   reference is refused, as the broker refuses a shared cluster's.
// - `{"startingQueries": {"<slug>": n}}`: that database refuses its next n
//   requests with the gateway's 503 rate_limit_unavailable, as a database
//   does until the gateway has its rate limit: n reads, and an attempt of a load
//   for each it starts (./loads.ts).

type Knobs = {
  helixSubscription?: false;
  startingQueries?: Record<string, number>;
};
const knobs = config as typeof config & Knobs;
const SHARED_CLUSTER = "clu_shared_use2",
  PAGE = 50;

type StoredProject = Required<Pick<Project, "id" | "workspaceId">> & {
  slug: string;
  displayName: string;
  createdAt: string;
};
type StoredTenant = {
  id: string;
  workspaceId: string;
  projectId: string;
  name: string;
  slug: string;
  status: ResourceStatus;
  createdAt: string;
};

const state = {
  projects: [] as StoredProject[],
  tenants: [] as StoredTenant[],
  graphs: new Map<string, MockGraph>(),
};

/** A team's own data, unlike anything Foundry writes: accounts and orders. */
function teamData() {
  const g = new MockGraph();
  const accounts = [
    ["acc_1", "Ada Lovelace", "ada@acme.example"],
    ["acc_2", "Grace Hopper", "grace@acme.example"],
    ["acc_3", "Alan Turing", "alan@acme.example"],
  ].map(([key, name, email]) =>
    g.addNode("Account", { account_id: key!, name: name!, email: email! }),
  );
  [
    ["ord_1001", 0, 120.5],
    ["ord_1002", 0, 42],
    ["ord_1003", 1, 310],
    ["ord_1004", 2, 18.25],
  ].forEach(([key, account, total]) => {
    const o = g.addNode("Order", {
      order_number: key as string,
      total: total as number,
    });
    g.addEdge("PLACED", accounts[account as number]!, o, {
      at: "2026-09-30T12:00:00.000Z",
    });
  });
  return g;
}

function seed() {
  const at = "2026-09-20T09:00:00.000Z",
    later = "2026-10-01T11:00:00.000Z";
  state.projects = [
    {
      id: "prj_acme_prod",
      workspaceId: "ws_acme",
      slug: "production",
      displayName: "Production",
      createdAt: at,
    },
    {
      id: "prj_acme_staging",
      workspaceId: "ws_acme",
      slug: "staging",
      displayName: "Staging",
      createdAt: later,
    },
  ];
  const tenant = (
    id: string,
    projectId: string,
    slug: string,
    status: ResourceStatus = "RESOURCE_STATUS_ACTIVE",
  ): StoredTenant => ({
    id,
    workspaceId: "ws_acme",
    projectId,
    name: slug,
    slug,
    status,
    createdAt: projectId === "prj_acme_prod" ? at : later,
  });
  state.tenants = [
    tenant("ten_acme_contracts", "prj_acme_prod", "contracts"),
    tenant("ten_acme_subscriptions", "prj_acme_prod", "subscriptions"),
    tenant("ten_acme_sales", "prj_acme_prod", "sales"),
    tenant("ten_acme_orders", "prj_acme_prod", "orders"),
    tenant("ten_acme_sandbox", "prj_acme_staging", "sandbox"),
    tenant(
      "ten_acme_old",
      "prj_acme_staging",
      "old-orders",
      "RESOURCE_STATUS_DELETED",
    ),
  ];
  state.graphs = new Map([
    ["ten_acme_contracts", new MockGraph()],
    ["ten_acme_subscriptions", new MockGraph()],
    ["ten_acme_sales", new MockGraph()],
    ["ten_acme_orders", teamData()],
    ["ten_acme_sandbox", new MockGraph()],
  ]);
  delete knobs.helixSubscription;
  delete knobs.startingQueries;
}
onReset(seed);

/** A live database of the workspace, or undefined. */
export function liveTenant(workspaceId: string, tenantId: string) {
  return state.tenants.find(
    (t) =>
      t.id === tenantId &&
      t.workspaceId === workspaceId &&
      t.status === "RESOURCE_STATUS_ACTIVE",
  );
}
/** The graph a database holds (empty until something writes it). */
export function tenantGraph(tenantId: string): MockGraph {
  let g = state.graphs.get(tenantId);
  if (!g) state.graphs.set(tenantId, (g = new MockGraph()));
  return g;
}
/**
 * Whether a database still starting refuses this request: one of the n
 * `startingQueries` gives its slug, which it counts down.
 */
export function startingRefuses(tenantId: string): boolean {
  const slug = state.tenants.find((t) => t.id === tenantId)?.slug || "",
    left = knobs.startingQueries?.[slug] || 0;
  if (left <= 0) return false;
  knobs.startingQueries = { ...knobs.startingQueries, [slug]: left - 1 };
  return true;
}
/** The gateway's answer while it has no rate limit for a database. */
export const RATE_LIMIT_UNAVAILABLE = {
  error: "rate_limit_unavailable",
  msg: "tenant rate limit is unavailable",
};

/** Hooks the loads run before a database is read: they write as they go. */
export const beforeRead: ((tenantId: string) => void)[] = [];

const toProject = (p: StoredProject): Project => ({
  id: p.id,
  workspaceId: p.workspaceId,
  slug: p.slug,
  displayName: p.displayName,
  timestamps: { createdAt: p.createdAt, updatedAt: p.createdAt },
});
const toTenant = (t: StoredTenant): Tenant => ({
  id: t.id,
  clusterId: SHARED_CLUSTER,
  workspaceId: t.workspaceId,
  projectId: t.projectId,
  name: t.name,
  slug: t.slug,
  status: t.status,
  timestamps: { createdAt: t.createdAt, updatedAt: t.createdAt },
  queryState: "TENANT_QUERY_STATE_ENABLED",
  gateReason: "WORKSPACE_GATE_REASON_UNSPECIFIED",
});
// ListProjects and ListTenants page with page.size (default 50) and an
// opaque page.token, newest first.
function paged<T>(items: T[], query: URLSearchParams) {
  const size = Math.min(Number(query.get("page.size")) || PAGE, 100),
    from = Number(query.get("page.token") || 0) || 0;
  return {
    items: items.slice(from, from + size),
    nextPageToken: from + size < items.length ? String(from + size) : "",
  };
}
const newest = <T extends { createdAt: string; id: string }>(a: T, b: T) =>
  b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id);
const SLUG = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

function noSubscription(workspaceId: string): MockReply {
  const reply = fail(
    "FAILED_PRECONDITION",
    "this workspace has no active subscription",
  );
  return {
    ...reply,
    body: {
      ...(reply.body as object),
      details: [
        {
          "@type": "type.googleapis.com/google.rpc.ErrorInfo",
          reason: "NO_WORKSPACE_SUBSCRIPTION",
          domain: "billing.helix-db.com",
          metadata: { workspace_id: workspaceId, state: "expired" },
        },
      ],
    },
  };
}

const decode = (b64: string) => {
  const text = Buffer.from(b64, "base64").toString("utf8");
  return JSON.parse(text) as unknown;
};

export const databaseRoutes: MockRoute[] = [
  {
    method: "GET",
    path: "/v1/projects",
    handle: ({ query, principal: { user } }) => {
      const ws = query.get("workspaceId") || "";
      const m = membership(ws, user.id);
      if (isReply(m)) return m;
      const page = paged(
        state.projects.filter((p) => p.workspaceId === ws).sort(newest),
        query,
      );
      return ok({
        projects: page.items.map(toProject),
        nextPageToken: page.nextPageToken,
      } satisfies ListProjectsResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/projects",
    handle: ({ body, principal: { user } }) => {
      const b = (body || {}) as Record<string, string>;
      const ws = b.workspaceId || b.workspace_id || "";
      const m = membership(ws, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      let slug = b.slug || "",
        displayName = (b.displayName || b.display_name || "").trim();
      if (!SLUG.test(slug))
        return fail(
          "INVALID_ARGUMENT",
          "slug must be lowercase letters, digits and dashes",
        );
      if (!displayName)
        return fail("INVALID_ARGUMENT", "display_name is required");
      // A slug the workspace has already takes the next free number, name
      // and slug alike ("Foundry 2", foundry-2), as CreateProject allocates.
      const taken = (s: string) =>
        state.projects.some((p) => p.workspaceId === ws && p.slug === s);
      if (taken(slug)) {
        let n = 2;
        while (taken(`${slug}-${n}`)) n += 1;
        slug = `${slug}-${n}`;
        displayName = `${displayName} ${n}`;
      }
      const p: StoredProject = {
        id: id("prj"),
        workspaceId: ws,
        slug,
        displayName,
        createdAt: now(),
      };
      state.projects.push(p);
      return ok(toProject(p));
    },
  },
  {
    method: "GET",
    path: "/v1/tenants",
    handle: ({ query, principal: { user } }) => {
      const p = state.projects.find((x) => x.id === query.get("projectId"));
      if (!p) return fail("NOT_FOUND", "project not found");
      const m = membership(p.workspaceId, user.id);
      if (isReply(m)) return m;
      // Deleted tenants are listed too, as the WFE lists them.
      const page = paged(
        state.tenants.filter((t) => t.projectId === p.id).sort(newest),
        query,
      );
      return ok({
        tenants: page.items.map(toTenant),
        nextPageToken: page.nextPageToken,
      } satisfies ListTenantsResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/tenants",
    handle: ({ body, principal: { user } }) => {
      const b = (body || {}) as Record<string, string>;
      const projectId = b.projectId || b.project_id || "",
        name = (b.name || "").trim(),
        slug = b.slug || "";
      if (!projectId) return fail("INVALID_ARGUMENT", "project_id is required");
      if (!name) return fail("INVALID_ARGUMENT", "name is required");
      if (!slug) return fail("INVALID_ARGUMENT", "slug is required");
      if (b.clusterId || b.cluster_id)
        return fail(
          "INVALID_ARGUMENT",
          "the mock only places databases on the shared cluster",
        );
      const p = state.projects.find((x) => x.id === projectId);
      if (!p) return fail("NOT_FOUND", "project not found");
      const m = membership(p.workspaceId, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      if (LOCAL_TOKEN === null && knobs.helixSubscription === false)
        return noSubscription(p.workspaceId);
      if (!SLUG.test(slug))
        return fail(
          "INVALID_ARGUMENT",
          "slug must be lowercase letters, digits and dashes",
        );
      if (
        state.tenants.some(
          (t) => t.slug === slug && t.status !== "RESOURCE_STATUS_DELETED",
        )
      )
        return fail(
          "ALREADY_EXISTS",
          `tenant "${slug}" already exists in cluster`,
        );
      const t: StoredTenant = {
        id: id("ten"),
        workspaceId: p.workspaceId,
        projectId: p.id,
        name,
        slug,
        status: "RESOURCE_STATUS_ACTIVE",
        createdAt: now(),
      };
      state.tenants.push(t);
      state.graphs.set(t.id, new MockGraph());
      // The first key's raw token, which Foundry never reads.
      return ok({
        tenant: toTenant(t),
        token: "hx_mock_never_shown",
      } satisfies CreateTenantResponse & { token: string });
    },
  },
  {
    method: "POST",
    path: "/v1/databases:query-read",
    handle: ({ body, principal: { user } }) => {
      const b = (body || {}) as {
        database?: { tenantId?: string; clusterId?: string };
        queryJson?: string;
      };
      if (b.database?.clusterId)
        return fail(
          "INVALID_ARGUMENT",
          "shared clusters are not database references",
        );
      const t = state.tenants.find(
        (x) =>
          x.id === b.database?.tenantId &&
          x.status !== "RESOURCE_STATUS_DELETED",
      );
      if (!t) return fail("NOT_FOUND", "database not found");
      const m = membership(t.workspaceId, user.id);
      if (isReply(m)) return m;
      let request: unknown;
      try {
        request = decode(b.queryJson || "");
      } catch {
        return fail("INVALID_ARGUMENT", "query_json is not a JSON request");
      }
      beforeRead.forEach((hook) => hook(t.id));
      const answer = startingRefuses(t.id)
        ? { status: 503, body: RATE_LIMIT_UNAVAILABLE }
        : runQuery(tenantGraph(t.id), request);
      return ok({
        responseJson: Buffer.from(JSON.stringify(answer.body), "utf8").toString(
          "base64",
        ),
        statusCode: answer.status,
        contentType: "application/json",
      });
    },
  },
];
