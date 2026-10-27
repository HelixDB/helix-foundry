import type {
  CreateOntologyRequest,
  FoundryOntology,
  ListOntologiesResponse,
  OntologyResponse,
} from "../../src/shared/wfe";
import {
  fail,
  id,
  now,
  ok,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import {
  config,
  isReply,
  membership,
  onReset,
  type Membership,
} from "../state";
import { liveTenant } from "./databases";
import { documentCount } from "./documents";
import { ontologyDeleted, ontologyLoads } from "./loads";
import { currentVersionOf } from "./ontology";
import { manages } from "./workspaces";

// A workspace's ontologies, as the backend's foundry_ontologies table
// holds them: each bound to one live database (a tenant) of the workspace,
// one ontology per database. Deleting one soft-deletes Foundry's rows and
// deletes its loads from the database (./loads.ts); the database's other
// data stays. Acme starts with "Customer contracts" on its Production
// project's contracts database, "Sales graph" (its people, enriched) on
// sales, and "Renewals" on its subscriptions database, older, whose current
// version reads the General document set too.

export type StoredOntology = {
  id: string;
  workspaceId: string;
  name: string;
  projectId: string;
  tenantId: string;
  databaseName: string;
  clientToken: string;
  createdAt: string;
  deletedAt: string | null;
};

const state = { ontologies: [] as StoredOntology[] };
// `{"ontologyDatabaseHidden": true}` answers as the WFE does a member who
// can't read the database's project: no project, tenant or database name.
const knobs = config as typeof config & { ontologyDatabaseHidden?: boolean };
export const SEED_ONTOLOGY = "ont_acme_contracts",
  SALES_ONTOLOGY = "ont_acme_sales",
  RENEWALS = "ont_acme_renewals";
onReset(() => {
  delete knobs.ontologyDatabaseHidden;
  state.ontologies = [
    {
      id: SEED_ONTOLOGY,
      workspaceId: "ws_acme",
      name: "Customer contracts",
      projectId: "prj_acme_prod",
      tenantId: "ten_acme_contracts",
      databaseName: "contracts",
      clientToken: "seed",
      createdAt: "2026-10-02T09:30:00.000Z",
      deletedAt: null,
    },
    {
      id: SALES_ONTOLOGY,
      workspaceId: "ws_acme",
      name: "Sales graph",
      projectId: "prj_acme_prod",
      tenantId: "ten_acme_sales",
      databaseName: "sales",
      clientToken: "seed-sales",
      createdAt: "2026-10-01T09:30:00.000Z",
      deletedAt: null,
    },
    {
      id: RENEWALS,
      workspaceId: "ws_acme",
      name: "Renewals",
      projectId: "prj_acme_prod",
      tenantId: "ten_acme_subscriptions",
      databaseName: "subscriptions",
      clientToken: "seed-renewals",
      createdAt: "2026-09-28T09:30:00.000Z",
      deletedAt: null,
    },
  ];
});

/** A live ontology by id, for other mocks. */
export const ontologyById = (ontologyId: string) =>
  state.ontologies.find((o) => o.id === ontologyId && !o.deletedAt);
/** A workspace's live ontologies, newest first, for other mocks. */
export const workspaceOntologies = (workspaceId: string) =>
  state.ontologies
    .filter((o) => o.workspaceId === workspaceId && !o.deletedAt)
    .sort(
      (a, b) =>
        b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id),
    );

/**
 * The caller's membership and the live ontology a request under
 * .../ontologies/{ontologyId} names, or the WFE's refusal. `manage` asks for
 * owner or admin (workspace.ontology.manage).
 */
export function ontologyOf(
  req: MockRequest,
  manage = false,
): { m: Membership; o: StoredOntology } | MockReply {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  if (manage && !manages(m))
    return fail("PERMISSION_DENIED", "permission denied");
  const o = state.ontologies.find(
    (x) =>
      x.id === req.params.ontologyId &&
      x.workspaceId === m.workspaceId &&
      !x.deletedAt,
  );
  return o ? { m, o } : fail("NOT_FOUND", "ontology not found");
}
export const isScope = (
  x: { m: Membership; o: StoredOntology } | MockReply,
): x is { m: Membership; o: StoredOntology } => "o" in x;

const toOntology = (o: StoredOntology): FoundryOntology => ({
  id: o.id,
  name: o.name,
  ...(knobs.ontologyDatabaseHidden
    ? { projectId: "", tenantId: "", databaseName: "" }
    : {
        projectId: o.projectId,
        tenantId: o.tenantId,
        databaseName: o.databaseName,
      }),
  currentVersionId: currentVersionOf(o.id) || "",
  createdAt: o.createdAt,
  ...(o.deletedAt ? {} : ontologyLoads(o.id)),
  counts: { documents: documentCount(o.workspaceId, o.id) },
});

const BASE = "/v1/workspaces/{workspaceId}/foundry/ontologies" as const;
const PAGE = 50;

export const ontologiesRoutes: MockRoute[] = [
  {
    method: "GET",
    path: BASE,
    handle: ({ params, query, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      const all = workspaceOntologies(m.workspaceId);
      const from = Number(query.get("pageToken") || 0) || 0;
      return ok({
        ontologies: all.slice(from, from + PAGE).map(toOntology),
        nextPageToken: from + PAGE < all.length ? String(from + PAGE) : "",
      } satisfies ListOntologiesResponse);
    },
  },
  {
    method: "POST",
    path: BASE,
    handle: ({ params, body, principal: { user } }) => {
      const m = membership(params.workspaceId!, user.id);
      if (isReply(m)) return m;
      if (!manages(m)) return fail("PERMISSION_DENIED", "permission denied");
      const req = (body || {}) as Partial<CreateOntologyRequest>;
      const name = (req.name || "").trim(),
        clientToken = req.clientToken || "";
      if (!name || [...name].length > 100)
        return fail("INVALID_ARGUMENT", "name must be 1 to 100 characters");
      if (!clientToken || clientToken.length > 128)
        return fail(
          "INVALID_ARGUMENT",
          "clientToken must be 1 to 128 characters",
        );
      const again = state.ontologies.find(
        (o) => o.workspaceId === m.workspaceId && o.clientToken === clientToken,
      );
      if (again) return ok({ ontology: toOntology(again) });
      const t = liveTenant(m.workspaceId, req.tenantId || "");
      if (!t) return fail("NOT_FOUND", "tenant not found");
      const bound = state.ontologies.find(
        (o) => o.tenantId === t.id && !o.deletedAt,
      );
      if (bound)
        return fail(
          "ALREADY_EXISTS",
          "another ontology already uses this database; choose or create another",
        );
      const o: StoredOntology = {
        id: id("ont"),
        workspaceId: m.workspaceId,
        name,
        projectId: t.projectId,
        tenantId: t.id,
        databaseName: t.name,
        clientToken,
        createdAt: now(),
        deletedAt: null,
      };
      state.ontologies.push(o);
      return ok({ ontology: toOntology(o) } satisfies OntologyResponse);
    },
  },
  {
    method: "GET",
    path: `${BASE}/{ontologyId}`,
    handle: (req) => {
      const s = ontologyOf(req);
      return isScope(s)
        ? ok({ ontology: toOntology(s.o) } satisfies OntologyResponse)
        : s;
    },
  },
  {
    method: "DELETE",
    path: `${BASE}/{ontologyId}`,
    handle: (req) => {
      const s = ontologyOf(req, true);
      if (!isScope(s)) return s;
      // Its live job stops and its loads leave the database; the
      // database's other data stays.
      ontologyDeleted(s.o.id);
      s.o.deletedAt = now();
      return ok({ ontology: toOntology(s.o) } satisfies OntologyResponse);
    },
  },
];
