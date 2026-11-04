import { randomBytes } from "node:crypto";
import type {
  CreateServiceCredentialRequest,
  CreateServiceCredentialResponse,
  FoundryAccessChange,
  FoundryAccessGrant,
  FoundryAccessTarget,
  FoundryLoadAccessInputs,
  FoundryOntologyAccess,
  FoundryPrincipal,
  FoundryPrincipalKind,
  FoundryTeam,
  FoundryTeamRequest,
  FoundryTeamResponse,
  GetMyFoundryAccessResponse,
  ListFoundryAccessResponse,
  ListServiceCredentialsResponse,
  PreviewFoundryAccessChangeResponse,
  ServiceCredential,
  SetFoundryAccessGrantsRequest,
  SetFoundryAccessGrantsResponse,
  SetFoundryMemberRestrictedResponse,
} from "../../src/shared/wfe";
import { parseAgentEnrichments } from "../../src/web/areas/ontology/model";
import type { Visibility } from "../graph/view";
import {
  fail,
  id,
  now,
  ok,
  type MockGrantee,
  type MockGranteeLoad,
  type MockReply,
  type MockRequest,
  type MockRoute,
} from "../http";
import { db, isReply, membership, onReset, type Membership } from "../state";
import { workspaceConnectors } from "./data";
import {
  ACME_GENERAL,
  documentSet,
  documentSets,
  loadDocumentIds,
  setConnector,
} from "./documents";
import { loadPropDecisions, servableLoad } from "./loads";
import { ontologyById, workspaceOntologies } from "./ontologies";
import { sourceId } from "./ontology-agent";
import { ontologyVersion, versionInputs, versionScope } from "./ontology";
import { manages, queryReads, rawAccess } from "./workspaces";

// Foundry's source grants (backend/access, backend/api/access*.go) and the
// WFE's agents' keys (service credentials), with the backend's rules and
// messages.
//
// Owners and admins grant a principal (a member, a team, or an agent's key)
// sources: a connector (its tables now and later, and the sets it owns), one
// table, a document set, or People enrichment. A load holds its version's
// generation's tables and table files, the sets its documents and table
// files are in now, and People enrichment when it applied profiles; one
// whose version has no generation holds what nothing covers (uncoverable).
// The mock's review decisions come from the load's own documents, so they
// add no set. A grantee reads the part of an ontology's current load its
// grants (its own and its teams') hold, all of it while they cover all of it,
// and nothing of a load whose inputs can't be told.
//
// Who is a grantee (`guarded`, the backend's authorize, run on every
// Foundry route): a member restricted in the request's workspace, on every
// Foundry call while their role is member; and a member without database
// query access (./workspaces.ts queryReads) on the routes the WFE gates on
// it (reading a load's graph, questions, review decisions), which the WFE
// sends on as a grantee. A grantee gets PERMISSION_DENIED from every route
// the rules don't admit, NOT_FOUND for an ontology its grants hold nothing of
// (a member who is only missing query access gets the WFE's own 403, since it
// may see the ontology), and handlers answer only what it holds (MockRequest.grantee): its
// ontologies, their current loads and versions (as ../graph/view.ts projects
// them, of a load it holds only some of), its own questions, and the
// documents those loads hold in sets it holds (their text, never their
// bytes). The mock's agents' keys authenticate nothing, so no call comes as
// one.
//
// Sources are the limit (the store and SetFoundryAccessGrants): a member given
// any, or on a team given any, is restricted to them and stays so once they
// go, until someone lifts it, which holding any refuses. An owner, an admin or
// a member with raw access is given none, nor can one with raw access join a
// team holding some.
//
// Acme starts with Eve restricted, holding the General set, and one agent's
// key holding nothing. A key's token is shown once, when it is made, and
// accepted nowhere; revoking a key deletes its grants, deleting a team its
// grants, and a grant of a connector or set that is gone is dropped.

type Principal = { kind: FoundryPrincipalKind; id: string };
type StoredGrant = {
  id: string;
  workspaceId: string;
  principal: Principal;
  target: FoundryAccessTarget;
  createdByUserId: string;
  createdAt: string;
};
type StoredTeam = {
  id: string;
  workspaceId: string;
  name: string;
  memberUserIds: string[];
  createdByUserId: string;
  createdAt: string;
};
type StoredCredential = ServiceCredential & { workspaceId: string };
/** What a load holds, as far as grants go (access.Inputs). */
type Inputs = {
  tables: { connectorId: string; table: string }[];
  sets: { setId: string; connectorId: string }[];
  enrichment: boolean;
  uncoverable: boolean;
};

const state = {
  grants: [] as StoredGrant[],
  teams: [] as StoredTeam[],
  // Restricted members, as `${workspaceId}\u001f${userId}`.
  restricted: new Set<string>(),
  credentials: [] as StoredCredential[],
};
const restrictedKey = (ws: string, userId: string) => `${ws}\u001f${userId}`;
const restrict = (ws: string, userIds: string[]) =>
  userIds.forEach((u) => state.restricted.add(restrictedKey(ws, u)));
const RAW_ACCESS =
  "this member can write a project or query its databases outside Foundry; take that access away in the Helix dashboard first";
const TEAM_RAW_ACCESS =
  "a member of this team can write a project or query its databases outside Foundry; take that access away in the Helix dashboard, or take them off the team, first";
/** A member's refusal to be confined to grants (restrictable), if any. */
const unconfinable = (ws: string, userId: string) => {
  const member = db.memberships.find(
    (x) => x.workspaceId === ws && x.userId === userId,
  );
  if (!member) return fail("NOT_FOUND", "member not found");
  if (member.role !== "WORKSPACE_ROLE_MEMBER")
    return fail(
      "FAILED_PRECONDITION",
      "only a member can be restricted: owners and admins see everything",
    );
  return rawAccess(member)
    ? fail("FAILED_PRECONDITION", RAW_ACCESS)
    : undefined;
};
/** Whether any of these team members has raw access (teamRestrictable). */
const anyRaw = (ws: string, userIds: string[]) =>
  db.memberships.some(
    (x) =>
      x.workspaceId === ws &&
      userIds.includes(x.userId) &&
      x.role === "WORKSPACE_ROLE_MEMBER" &&
      rawAccess(x),
  );
export const ACME_AGENT_KEY = "sc_acme_support";
onReset(() => {
  const at = "2026-10-03T10:00:00.000Z";
  state.teams = [];
  state.restricted = new Set([restrictedKey("ws_acme", "usr_eve")]);
  state.grants = [
    {
      id: "grant_acme_eve_general",
      workspaceId: "ws_acme",
      principal: { kind: "KIND_USER", id: "usr_eve" },
      target: { kind: "KIND_DOCUMENT_SET", documentSetId: ACME_GENERAL },
      createdByUserId: "usr_ada",
      createdAt: at,
    },
  ];
  state.credentials = [
    {
      id: ACME_AGENT_KEY,
      workspaceId: "ws_acme",
      name: "Support agent",
      tokenPrefix: "hlxs_7f3a9c",
      workspaceOwner: { workspaceId: "ws_acme" },
      grants: [],
      createdBy: "usr_ada",
      expiresAt: null,
      revokedAt: null,
      lastUsedAt: null,
      timestamps: { createdAt: at, updatedAt: at },
    },
  ];
});

const principalKey = (p: Principal) => `${p.kind}:${p.id}`;
const NOT_GRANTED = "source grants don't allow this call";

// ---------------------------------------------------------------- coverage

/** A target as a key, to tell two apart. */
const targetKey = (t: FoundryAccessTarget) =>
  [t.kind, t.connectorId || "", t.table || "", t.documentSetId || ""].join(
    "\u001f",
  );

/** Whether targets cover a document in `set` (Grants.CoversSet). */
function coversSet(
  targets: readonly FoundryAccessTarget[],
  set: { setId: string; connectorId: string },
) {
  return (
    !!set.setId &&
    targets.some(
      (t) =>
        (t.kind === "KIND_DOCUMENT_SET" && t.documentSetId === set.setId) ||
        (t.kind === "KIND_CONNECTOR" &&
          !!set.connectorId &&
          t.connectorId === set.connectorId),
    )
  );
}

/** Whether targets cover a table (Grants.CoversTable). */
const coversTable = (
  targets: readonly FoundryAccessTarget[],
  x: { connectorId: string; table: string },
) =>
  targets.some(
    (t) =>
      (t.kind === "KIND_CONNECTOR" && t.connectorId === x.connectorId) ||
      (t.kind === "KIND_TABLE" &&
        t.connectorId === x.connectorId &&
        t.table === x.table),
  );

/** Whether targets cover all a load holds (Grants.Covers). */
function covers(targets: readonly FoundryAccessTarget[], inputs: Inputs) {
  return (
    !inputs.uncoverable &&
    (!inputs.enrichment || targets.some((t) => t.kind === "KIND_ENRICHMENT")) &&
    inputs.tables.every((x) => coversTable(targets, x)) &&
    inputs.sets.every((s) => coversSet(targets, s))
  );
}

/**
 * Whether targets hold anything of what a load holds (Grants.Reaches): a load
 * whose inputs can't be told isn't read at all.
 */
function reaches(targets: readonly FoundryAccessTarget[], inputs: Inputs) {
  return (
    !inputs.uncoverable &&
    ((inputs.enrichment && targets.some((t) => t.kind === "KIND_ENRICHMENT")) ||
      inputs.tables.some((x) => coversTable(targets, x)) ||
      inputs.sets.some((s) => coversSet(targets, s)))
  );
}

const withSet = (
  sets: Inputs["sets"],
  set: { setId: string; connectorId: string },
) =>
  sets.some((s) => s.setId === set.setId && s.connectorId === set.connectorId)
    ? sets
    : [...sets, set];

/**
 * What each live ontology's servable current load holds, by ontology id
 * (ListFoundryCurrentLoadInputs).
 */
function currentInputs(workspaceId: string) {
  const out = new Map<
    string,
    { loadId: string; versionId: string; inputs: Inputs }
  >();
  for (const o of workspaceOntologies(workspaceId)) {
    const load = servableLoad(o.id);
    if (!load) continue;
    const drafted = versionInputs(o.id, load.versionId);
    const sets = [
      ...loadDocumentIds(load.loadId),
      ...(drafted?.documents || []),
    ].map(documentSet);
    out.set(o.id, {
      loadId: load.loadId,
      versionId: load.versionId,
      inputs: {
        tables: (drafted?.tables || []).map((t) => ({
          connectorId: t.connectorId || "",
          table: t.table || "",
        })),
        sets: sets.reduce(withSet, [] as Inputs["sets"]),
        enrichment: load.enrichment,
        uncoverable: !drafted,
      },
    });
  }
  return out;
}

/**
 * What a new load of the ontology's version would hold
 * (FoundryVersionInputs): its generation's tables and table files, the sets
 * of its scope, and People enrichment when its mappings enrich (or can't be
 * read: a load fails on them, so say it would).
 */
function versionAccessInputs(
  workspaceId: string,
  ontologyId: string,
  versionId: string,
): Inputs | MockReply {
  const v = ontologyVersion(ontologyId, versionId);
  if (!v) return fail("NOT_FOUND", "version not found");
  const drafted = versionInputs(ontologyId, versionId);
  if (!drafted)
    return { tables: [], sets: [], enrichment: false, uncoverable: true };
  const live = new Set(documentSets(workspaceId).map((s) => s.id));
  const sets = [
    ...versionScope(ontologyId, versionId)
      .filter((setId) => live.has(setId))
      .map((setId) => ({ setId, connectorId: setConnector(setId) })),
    ...drafted.documents.map(documentSet),
  ].reduce(withSet, [] as Inputs["sets"]);
  const enrichments = parseAgentEnrichments(
    String(v.definition.mappings_toml || ""),
  );
  return {
    tables: drafted.tables.map((t) => ({
      connectorId: t.connectorId || "",
      table: t.table || "",
    })),
    sets,
    enrichment: !enrichments.ok || enrichments.value.length > 0,
    uncoverable: false,
  };
}

const inputsOut = (i: Inputs): FoundryLoadAccessInputs => ({
  tables: i.tables,
  documentSets: i.sets.map((s) => ({
    documentSetId: s.setId,
    connectorId: s.connectorId,
  })),
  enrichment: i.enrichment,
  uncoverable: i.uncoverable,
});

/**
 * The workspace's grants, oldest first, without those of a connector or set
 * that is gone (the store deletes them with it).
 */
function grantsOf(workspaceId: string) {
  const connectors = new Set(workspaceConnectors(workspaceId).map((c) => c.id)),
    sets = new Set(documentSets(workspaceId).map((s) => s.id));
  state.grants = state.grants.filter(
    (g) =>
      g.workspaceId !== workspaceId ||
      (g.target.kind === "KIND_DOCUMENT_SET"
        ? sets.has(g.target.documentSetId || "")
        : g.target.kind === "KIND_ENRICHMENT" ||
          connectors.has(g.target.connectorId || "")),
  );
  return state.grants.filter((g) => g.workspaceId === workspaceId);
}

const teamsOf = (workspaceId: string, userId: string) =>
  state.teams
    .filter((t) => t.workspaceId === workspaceId)
    .filter((t) => t.memberUserIds.includes(userId))
    .map((t) => t.id);

/** The targets a user holds in the workspace: their own, then their teams'. */
function userTargets(workspaceId: string, userId: string) {
  const held = new Set([
    principalKey({ kind: "KIND_USER", id: userId }),
    ...teamsOf(workspaceId, userId).map((id) =>
      principalKey({ kind: "KIND_TEAM", id }),
    ),
  ]);
  return grantsOf(workspaceId)
    .filter((g) => held.has(principalKey(g.principal)))
    .map((g) => g.target);
}

/** The current loads targets hold anything of, by ontology (granteeLoads). */
function heldLoads(
  workspaceId: string,
  targets: readonly FoundryAccessTarget[],
) {
  const out = new Map<string, MockGranteeLoad>();
  if (!targets.length) return out;
  for (const [ontologyId, l] of currentInputs(workspaceId))
    if (reaches(targets, l.inputs))
      out.set(ontologyId, {
        loadId: l.loadId,
        versionId: l.versionId,
        partial: !covers(targets, l.inputs),
      });
  return out;
}

/**
 * What targets hold of an ontology's current load, as the agent is told it
 * (Grants.Part and ontologyagent.VisibilityOf): its tables and table files,
 * the documents it holds by the set each is in now, and the properties decided
 * while it doesn't hold them all.
 */
function visibilityOf(
  ontologyId: string,
  targets: readonly FoundryAccessTarget[],
): Visibility {
  const load = servableLoad(ontologyId);
  const drafted = load && versionInputs(ontologyId, load.versionId);
  const documents = load ? loadDocumentIds(load.loadId) : [];
  const held = documents.filter((d) => coversSet(targets, documentSet(d))),
    files = (drafted?.documents || []).filter((d) =>
      coversSet(targets, documentSet(d)),
    );
  const all = held.length === documents.length;
  return {
    sources: [
      ...(drafted?.tables || [])
        .filter((t) =>
          coversTable(targets, {
            connectorId: t.connectorId || "",
            table: t.table || "",
          }),
        )
        .map((t) => sourceId(t.connectorId || "", t.table || "")),
      ...files,
    ],
    documents: all ? "all" : { only: held },
    sets: [...new Set([...held, ...files].map((d) => documentSet(d).setId))],
    enrichment:
      !!load?.enrichment && targets.some((t) => t.kind === "KIND_ENRICHMENT"),
    withheld: all ? [] : load ? loadPropDecisions(load.loadId) : [],
  };
}

/**
 * Whether an ontology's load holds a document: as one of its documents, or
 * a table file its version drafted from (ListFoundryOntologiesHoldingDocument).
 */
const holds = (
  ontologyId: string,
  loadId: string,
  versionId: string,
  documentId: string,
) =>
  loadDocumentIds(loadId).includes(documentId) ||
  !!versionInputs(ontologyId, versionId)?.documents.includes(documentId);

// ---------------------------------------------------------------- the check

type Rule = "workspace" | "ontology";
const W = "/v1/workspaces/{workspaceId}/foundry",
  O = `${W}/ontologies/{ontologyId}`;
/**
 * What each of Foundry's routes lets a grantee do (access.Rules); a route
 * not named refuses it. A grantee only reads, and asks and cancels its own
 * questions.
 */
const RULES: Record<string, Rule> = {
  [`GET ${W}/access/me`]: "workspace",
  [`GET ${W}/ontologies`]: "workspace",
  [`GET ${O}`]: "ontology",
  [`GET ${O}/versions/{id}`]: "ontology",
  [`GET ${O}/loads`]: "ontology",
  [`GET ${O}/loads/{id}`]: "ontology",
  [`POST ${O}/loads/{loadId}:read`]: "ontology",
  [`POST ${O}/questions`]: "ontology",
  [`GET ${O}/questions`]: "ontology",
  [`GET ${O}/questions/{id}`]: "ontology",
  [`GET ${O}/questions/{id}/events`]: "ontology",
  [`POST ${O}/questions/{id}:cancel`]: "ontology",
  [`GET ${W}/documents`]: "workspace",
  [`GET ${W}/documents/{id}:content`]: "workspace",
  [`GET ${O}/documents`]: "ontology",
  [`GET ${O}/documents/{id}`]: "ontology",
};
/**
 * The Foundry routes the WFE gates on reading the ontology's database: a
 * member without query access calls them as a grantee.
 */
const QUERY_GATED = new Set([
  `POST ${O}/loads/{loadId}:read`,
  `POST ${O}/questions`,
  `GET ${O}/questions`,
  `GET ${O}/questions/{id}`,
  `GET ${O}/questions/{id}/events`,
  `POST ${O}/questions/{id}:cancel`,
  `GET ${O}/review-decisions`,
]);
const isFoundry = (path: string) =>
  /^\/v1\/(foundry(\/|$|:)|workspaces\/\{workspaceId\}\/foundry(\/|$|:))/.test(
    path,
  );

/** The workspace a call is for: its path's, query's or body's. */
const workspaceOf = (req: MockRequest): string =>
  req.params.workspaceId ||
  req.query.get("workspaceId") ||
  (typeof req.body?.workspaceId === "string" ? req.body.workspaceId : "");

/**
 * The member a call comes from as a grantee, if it does (granteeOf): one
 * restricted in the request's workspace while their role is member, or a
 * member without query access on a route the WFE gates on it.
 */
function granteeMember(req: MockRequest, route: string) {
  const ws = workspaceOf(req),
    user = req.principal.user.id;
  const m = db.memberships.find(
    (x) => x.workspaceId === ws && x.userId === user,
  );
  if (!m || m.role !== "WORKSPACE_ROLE_MEMBER") return undefined;
  return state.restricted.has(restrictedKey(ws, user)) ||
    (QUERY_GATED.has(route) && !queryReads(m))
    ? m
    : undefined;
}

/**
 * `route` with the backend's access check run first: a grantee is refused
 * a route its rule doesn't admit (PERMISSION_DENIED), an ontology its grants
 * don't cover (NOT_FOUND), and is handed on as MockRequest.grantee.
 */
export function guarded(route: MockRoute): MockRoute {
  if (route.public || !isFoundry(route.path)) return route;
  const key = `${route.method} ${route.path}`,
    rule = RULES[key];
  return {
    ...route,
    handle: (req) => {
      const m = granteeMember(req, key);
      if (!m) return route.handle(req);
      // The WFE's own refusal: this member isn't restricted, and sees the
      // ontology. It sends the call on only with grants that cover it.
      const denied = state.restricted.has(
        restrictedKey(m.workspaceId, m.userId),
      )
        ? null
        : fail("PERMISSION_DENIED", "permission denied");
      if (!rule) return denied || fail("PERMISSION_DENIED", NOT_GRANTED);
      const targets = userTargets(m.workspaceId, m.userId);
      const grantee: MockGrantee = {
        principal: principalKey({ kind: "KIND_USER", id: m.userId }),
        covered: heldLoads(m.workspaceId, targets),
        coversSet: (setId) =>
          coversSet(targets, { setId, connectorId: setConnector(setId) }),
        holds: (documentId, ontologyId) =>
          coversSet(targets, documentSet(documentId)) &&
          [...grantee.covered].some(
            ([o, l]) =>
              (!ontologyId || o === ontologyId) &&
              holds(o, l.loadId, l.versionId, documentId),
          ),
        visibility: (ontologyId) => visibilityOf(ontologyId, targets),
        seen: (ontologyId) => {
          const l = currentInputs(m.workspaceId).get(ontologyId)!.inputs;
          return {
            tables: l.tables.filter((t) => coversTable(targets, t)),
            sets: l.sets.filter((s) => coversSet(targets, s)),
            enrichment:
              l.enrichment && targets.some((t) => t.kind === "KIND_ENRICHMENT"),
            uncoverable: false,
          };
        },
        stillSees: (seen) => covers(targets, seen),
      };
      if (rule === "ontology") {
        grantee.load = grantee.covered.get(req.params.ontologyId || "");
        if (!grantee.load)
          return denied || fail("NOT_FOUND", "ontology not found");
      }
      return route.handle({ ...req, grantee });
    },
  };
}

// ---------------------------------------------------------------- routes

/** The caller's membership when it manages the workspace's members. */
function manager(req: MockRequest): Membership | MockReply {
  const m = membership(req.params.workspaceId!, req.principal.user.id);
  if (isReply(m)) return m;
  return manages(m) ? m : fail("PERMISSION_DENIED", "permission denied");
}

const grantOut = (g: StoredGrant): FoundryAccessGrant => ({
  id: g.id,
  principal: g.principal,
  target: {
    kind: g.target.kind,
    connectorId: g.target.connectorId || "",
    table: g.target.table || "",
    documentSetId: g.target.documentSetId || "",
  },
  createdByUserId: g.createdByUserId,
  createdAt: g.createdAt,
});
const teamOut = (t: StoredTeam): FoundryTeam => ({
  id: t.id,
  name: t.name,
  memberUserIds: [...t.memberUserIds],
  createdByUserId: t.createdByUserId,
  createdAt: t.createdAt,
});
const credentialOut = ({
  workspaceId: _,
  ...c
}: StoredCredential): ServiceCredential => structuredClone(c);

const PRINCIPAL_KINDS = new Set<FoundryPrincipalKind>([
  "KIND_USER",
  "KIND_SERVICE_CREDENTIAL",
  "KIND_TEAM",
]);
const KIND_NAMES: Partial<Record<string, string>> = {
  KIND_CONNECTOR: "connector",
  KIND_TABLE: "table",
  KIND_DOCUMENT_SET: "document_set",
  KIND_ENRICHMENT: "enrichment",
};

/** A target as ParseTarget reads it: each kind names exactly what it needs. */
function parseTarget(t: FoundryAccessTarget): FoundryAccessTarget | string {
  const kind = t.kind || "",
    c = t.connectorId || "",
    table = t.table || "",
    set = t.documentSetId || "";
  const name = KIND_NAMES[kind];
  if (!name) return `access: unknown grant target kind ""`;
  const fits =
    kind === "KIND_CONNECTOR"
      ? !!c && !table && !set
      : kind === "KIND_TABLE"
        ? !!c && !!table && !set
        : kind === "KIND_DOCUMENT_SET"
          ? !c && !table && !!set
          : !c && !table && !set;
  return fits
    ? {
        kind: t.kind,
        ...(c ? { connectorId: c } : {}),
        ...(table ? { table } : {}),
        ...(set ? { documentSetId: set } : {}),
      }
    : `access: a ${name} grant can't name connector "${c}", table "${table}" and set "${set}"`;
}

const MAX_GRANTS = 1000,
  MAX_TEAMS = 100,
  MAX_TEAM_MEMBERS = 1000,
  MAX_TEAM_NAME = 120;

/** A team's trimmed name and its members, once each, or the WFE's refusal. */
function teamOf(
  workspaceId: string,
  body: Partial<FoundryTeamRequest> | undefined,
): { name: string; members: string[] } | MockReply {
  const name = (body?.name || "").trim(),
    members = body?.memberUserIds || [];
  if (!name || [...name].length > MAX_TEAM_NAME)
    return fail(
      "INVALID_ARGUMENT",
      `a team's name is 1 to ${MAX_TEAM_NAME} characters`,
    );
  if (members.length > MAX_TEAM_MEMBERS)
    return fail(
      "INVALID_ARGUMENT",
      `a team has at most ${MAX_TEAM_MEMBERS} members`,
    );
  for (const userId of members)
    if (
      !db.memberships.some(
        (m) => m.workspaceId === workspaceId && m.userId === userId,
      )
    )
      return fail(
        "INVALID_ARGUMENT",
        `${userId} is not a member of the workspace`,
      );
  return { name, members: [...new Set(members)] };
}
const nameTaken = (workspaceId: string, name: string, except = "") =>
  state.teams.some(
    (t) =>
      t.workspaceId === workspaceId &&
      t.id !== except &&
      t.name.toLowerCase() === name.toLowerCase(),
  );

const A = `${W}/access` as const;
/** A change's oneof as optional fields, as a request may carry any of them. */
type UnionToFields<U> = {
  [K in U extends unknown ? keyof U : never]: U extends Record<K, infer V>
    ? V
    : never;
};

export const accessRoutes: MockRoute[] = [
  {
    method: "GET",
    path: A,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const grants = grantsOf(m.workspaceId);
      // Each principal's targets, in the order its first grant was made.
      const held = new Map<
        string,
        Principal & { targets: FoundryAccessTarget[] }
      >();
      for (const g of grants) {
        const k = principalKey(g.principal);
        held.set(k, held.get(k) || { ...g.principal, targets: [] });
        held.get(k)!.targets.push(g.target);
      }
      const ontologies: FoundryOntologyAccess[] = [
        ...currentInputs(m.workspaceId),
      ]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([ontologyId, l]) => ({
          ontologyId,
          loadId: l.loadId,
          inputs: inputsOut(l.inputs),
          readers: [...held.values()]
            .filter((p) => covers(p.targets, l.inputs))
            .map(({ kind, id }): FoundryPrincipal => ({ kind, id })),
          partialReaders: [...held.values()]
            .filter(
              (p) =>
                !covers(p.targets, l.inputs) && reaches(p.targets, l.inputs),
            )
            .map(({ kind, id }): FoundryPrincipal => ({ kind, id })),
        }));
      return ok({
        grants: grants.map(grantOut),
        ontologies,
        teams: state.teams
          .filter((t) => t.workspaceId === m.workspaceId)
          .sort((a, b) => a.name.localeCompare(b.name))
          .map(teamOut),
        restrictedUserIds: [...state.restricted]
          .map((k) => k.split("\u001f"))
          .filter(([ws]) => ws === m.workspaceId)
          .map(([, userId]) => userId!),
      } satisfies ListFoundryAccessResponse);
    },
  },
  {
    method: "PUT",
    path: `${A}/grants`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const body = (req.body || {}) as Partial<SetFoundryAccessGrantsRequest>;
      const p = body.principal;
      if (!p?.kind || !PRINCIPAL_KINDS.has(p.kind) || !p.id)
        return fail(
          "INVALID_ARGUMENT",
          "name a principal: a user, a service credential or a team, and its id",
        );
      const principal: Principal = { kind: p.kind, id: p.id };
      if ((body.targets || []).length > MAX_GRANTS)
        return fail(
          "INVALID_ARGUMENT",
          `a principal holds at most ${MAX_GRANTS} grants`,
        );
      const targets: FoundryAccessTarget[] = [];
      for (const t of body.targets || []) {
        const parsed = parseTarget(t);
        if (typeof parsed === "string") return fail("INVALID_ARGUMENT", parsed);
        if (!targets.some((x) => targetKey(x) === targetKey(parsed)))
          targets.push(parsed);
      }
      // Sources restrict whoever they reach, so they go only to whom they can confine.
      const team =
        principal.kind === "KIND_TEAM"
          ? state.teams.find(
              (t) => t.workspaceId === m.workspaceId && t.id === principal.id,
            )
          : undefined;
      // Only what's new has to confine them: taking some away is always allowed.
      const holding = state.grants.filter(
        (g) =>
          g.workspaceId === m.workspaceId &&
          principalKey(g.principal) === principalKey(principal),
      );
      if (
        targets.some(
          (t) =>
            !holding.some(
              (g) =>
                targetKey(g.target) === targetKey(t) ||
                (t.kind === "KIND_TABLE" &&
                  g.target.kind === "KIND_CONNECTOR" &&
                  g.target.connectorId === t.connectorId),
            ),
        )
      ) {
        if (principal.kind === "KIND_USER") {
          const refused = unconfinable(m.workspaceId, principal.id);
          if (refused) return refused;
        }
        if (principal.kind === "KIND_TEAM") {
          if (!team) return fail("NOT_FOUND", "the workspace has no such team");
          if (anyRaw(m.workspaceId, team.memberUserIds))
            return fail("FAILED_PRECONDITION", TEAM_RAW_ACCESS);
        }
      }
      const connectors = new Set(
          workspaceConnectors(m.workspaceId).map((c) => c.id),
        ),
        sets = new Set(documentSets(m.workspaceId).map((s) => s.id));
      if (
        targets.some((t) =>
          t.kind === "KIND_DOCUMENT_SET"
            ? !sets.has(t.documentSetId!)
            : t.kind !== "KIND_ENRICHMENT" && !connectors.has(t.connectorId!),
        )
      )
        return fail(
          "NOT_FOUND",
          "a connector or document set to grant is not in the workspace",
        );
      // Grants kept keep their id and creation.
      const k = principalKey(principal),
        mine = (g: StoredGrant) =>
          g.workspaceId === m.workspaceId && principalKey(g.principal) === k;
      const kept = state.grants.filter(
        (g) =>
          mine(g) && targets.some((t) => targetKey(t) === targetKey(g.target)),
      );
      const added = targets
        .filter((t) => !kept.some((g) => targetKey(g.target) === targetKey(t)))
        .map(
          (target): StoredGrant => ({
            id: id("grant"),
            workspaceId: m.workspaceId,
            principal,
            target,
            createdByUserId: req.principal.user.id,
            createdAt: now(),
          }),
        );
      state.grants = [
        ...state.grants.filter((g) => !mine(g) || kept.includes(g)),
        ...added,
      ];
      if (targets.length > 0) {
        if (principal.kind === "KIND_USER")
          restrict(m.workspaceId, [principal.id]);
        if (team) restrict(m.workspaceId, team.memberUserIds);
      }
      return ok({
        grants: grantsOf(m.workspaceId).filter(mine).map(grantOut),
      } satisfies SetFoundryAccessGrantsResponse);
    },
  },
  {
    method: "POST",
    path: `${A}:preview`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const ws = m.workspaceId,
        change = (req.body?.change || {}) as Partial<
          UnionToFields<FoundryAccessChange>
        >;
      const grants = grantsOf(ws);
      if (!grants.length)
        return ok({} satisfies PreviewFoundryAccessChangeResponse);
      const before = currentInputs(ws);
      const after = new Map<string, Inputs>();
      let keep = (_: StoredGrant) => true;
      if (change.loadVersion) {
        const { ontologyId, versionId } = change.loadVersion;
        const o = ontologyById(ontologyId);
        if (!o || o.workspaceId !== ws)
          return fail("NOT_FOUND", "ontology not found");
        const inputs = versionAccessInputs(ws, o.id, versionId);
        if ("status" in inputs) return inputs;
        after.set(o.id, inputs);
      } else if (change.deleteConnector) {
        const connectorId = change.deleteConnector;
        if (!workspaceConnectors(ws).some((c) => c.id === connectorId))
          return fail("NOT_FOUND", "connector not found");
        const owned = documentSets(ws)
          .filter((s) => s.connector?.connectorId === connectorId)
          .map((s) => s.id);
        // Deleting a connector deletes its grants and its sets' grants.
        keep = (g) =>
          g.target.connectorId !== connectorId &&
          !owned.includes(g.target.documentSetId || "");
        for (const [o, l] of before) after.set(o, l.inputs);
      } else if (change.moveDocument) {
        const { documentId, documentSetId } = change.moveDocument;
        if (!documentSets(ws).some((s) => s.id === documentSetId))
          return fail("NOT_FOUND", "document set not found");
        // The document's old set may stay for its others; only the new one
        // can take a load away.
        const moved = {
          setId: documentSetId,
          connectorId: setConnector(documentSetId),
        };
        for (const [o, l] of before)
          if (holds(o, l.loadId, l.versionId, documentId))
            after.set(o, { ...l.inputs, sets: [...l.inputs.sets, moved] });
      } else return fail("INVALID_ARGUMENT", "name a change");
      const held = new Map<
        string,
        {
          p: Principal;
          now: FoundryAccessTarget[];
          then: FoundryAccessTarget[];
        }
      >();
      for (const g of grants) {
        const k = principalKey(g.principal),
          h = held.get(k) || { p: g.principal, now: [], then: [] };
        h.now.push(g.target);
        if (keep(g)) h.then.push(g.target);
        held.set(k, h);
      }
      const losses = [...after.keys()].sort().flatMap((ontologyId) => {
        const was = before.get(ontologyId);
        return [...held.values()]
          .filter(
            (h) =>
              was &&
              covers(h.now, was.inputs) &&
              !covers(h.then, after.get(ontologyId)!),
          )
          .map((h) => ({
            principal: h.p,
            ontologyId,
            keepsPart: reaches(h.then, after.get(ontologyId)!),
          }));
      });
      return ok({ losses } satisfies PreviewFoundryAccessChangeResponse);
    },
  },
  {
    method: "GET",
    path: `${A}/me`,
    handle: (req) => {
      const m = membership(req.params.workspaceId!, req.principal.user.id);
      if (isReply(m)) return m;
      const targets = userTargets(m.workspaceId, m.userId),
        loads = heldLoads(m.workspaceId, targets);
      return ok({
        grantee: !!req.grantee,
        hasGrants: targets.length > 0,
        ontologyIds: [...loads.keys()].sort(),
        partialOntologyIds: [...loads]
          .filter(([, l]) => l.partial)
          .map(([o]) => o)
          .sort(),
      } satisfies GetMyFoundryAccessResponse);
    },
  },
  {
    method: "PUT",
    path: `${A}/members/{userId}`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const restricted = !!req.body?.restricted,
        userId = req.params.userId!;
      if (restricted) {
        const refused = unconfinable(m.workspaceId, userId);
        if (refused) return refused;
        state.restricted.add(restrictedKey(m.workspaceId, userId));
      } else {
        const principals = new Set([
          principalKey({ kind: "KIND_USER", id: userId }),
          ...state.teams
            .filter(
              (t) =>
                t.workspaceId === m.workspaceId &&
                t.memberUserIds.includes(userId),
            )
            .map((t) => principalKey({ kind: "KIND_TEAM", id: t.id })),
        ]);
        if (
          state.grants.some(
            (g) =>
              g.workspaceId === m.workspaceId &&
              principals.has(principalKey(g.principal)),
          )
        )
          return fail(
            "FAILED_PRECONDITION",
            "this member holds sources, their own or a team's; take those away first",
          );
        state.restricted.delete(restrictedKey(m.workspaceId, userId));
      }
      return ok({ restricted } satisfies SetFoundryMemberRestrictedResponse);
    },
  },
  {
    method: "POST",
    path: `${A}/teams`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const team = teamOf(m.workspaceId, req.body);
      if ("status" in team) return team;
      if (
        state.teams.filter((t) => t.workspaceId === m.workspaceId).length >=
        MAX_TEAMS
      )
        return fail(
          "FAILED_PRECONDITION",
          `a workspace has at most ${MAX_TEAMS} teams`,
        );
      if (nameTaken(m.workspaceId, team.name))
        return fail("ALREADY_EXISTS", "the workspace has a team of that name");
      const t: StoredTeam = {
        id: id("team"),
        workspaceId: m.workspaceId,
        name: team.name,
        memberUserIds: team.members,
        createdByUserId: req.principal.user.id,
        createdAt: now(),
      };
      state.teams.push(t);
      return ok({ team: teamOut(t) } satisfies FoundryTeamResponse);
    },
  },
  {
    method: "PUT",
    path: `${A}/teams/{teamId}`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const team = teamOf(m.workspaceId, req.body);
      if ("status" in team) return team;
      const t = state.teams.find(
        (x) => x.workspaceId === m.workspaceId && x.id === req.params.teamId,
      );
      if (!t) return fail("NOT_FOUND", "team not found");
      if (nameTaken(m.workspaceId, team.name, t.id))
        return fail("ALREADY_EXISTS", "the workspace has a team of that name");
      const holds = state.grants.some(
        (g) => g.principal.kind === "KIND_TEAM" && g.principal.id === t.id,
      );
      // Only who joins: someone given raw access since doesn't stop a rename.
      if (
        holds &&
        anyRaw(
          m.workspaceId,
          team.members.filter((u) => !t.memberUserIds.includes(u)),
        )
      )
        return fail("FAILED_PRECONDITION", TEAM_RAW_ACCESS);
      Object.assign(t, { name: team.name, memberUserIds: team.members });
      if (holds) restrict(m.workspaceId, t.memberUserIds);
      return ok({ team: teamOut(t) } satisfies FoundryTeamResponse);
    },
  },
  {
    method: "DELETE",
    path: `${A}/teams/{teamId}`,
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const teamId = req.params.teamId!;
      if (
        !state.teams.some(
          (t) => t.workspaceId === m.workspaceId && t.id === teamId,
        )
      )
        return fail("NOT_FOUND", "team not found");
      state.teams = state.teams.filter((t) => t.id !== teamId);
      state.grants = state.grants.filter(
        (g) => !(g.principal.kind === "KIND_TEAM" && g.principal.id === teamId),
      );
      return ok({});
    },
  },

  // The WFE's agents' keys: listed (revoked ones too, with revokedAt), made
  // with their token shown once, and revoked, by owners and admins.
  {
    method: "GET",
    path: "/v1/workspaces/{workspaceId}/service-credentials",
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      return ok({
        credentials: state.credentials
          .filter((c) => c.workspaceId === m.workspaceId)
          .map(credentialOut),
      } satisfies ListServiceCredentialsResponse);
    },
  },
  {
    method: "POST",
    path: "/v1/workspaces/{workspaceId}/service-credentials",
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const body = (req.body || {}) as Partial<CreateServiceCredentialRequest>;
      const name = (body.name || "").trim();
      if (!name || [...name].length > 100)
        return fail("INVALID_ARGUMENT", "name must be 1 to 100 characters");
      const token = `hlxs_${randomBytes(24).toString("hex")}`,
        at = now();
      const c: StoredCredential = {
        id: id("sc"),
        workspaceId: m.workspaceId,
        name,
        tokenPrefix: token.slice(0, 11),
        workspaceOwner: { workspaceId: m.workspaceId },
        grants: structuredClone(body.grants || []),
        createdBy: req.principal.user.id,
        expiresAt: null,
        revokedAt: null,
        lastUsedAt: null,
        timestamps: { createdAt: at, updatedAt: at },
      };
      state.credentials.push(c);
      return ok({
        credential: credentialOut(c),
        token,
      } satisfies CreateServiceCredentialResponse);
    },
  },
  {
    method: "DELETE",
    path: "/v1/workspaces/{workspaceId}/service-credentials/{id}",
    handle: (req) => {
      const m = manager(req);
      if (isReply(m)) return m;
      const c = state.credentials.find(
        (x) =>
          x.workspaceId === m.workspaceId &&
          x.id === req.params.id &&
          !x.revokedAt,
      );
      if (!c) return fail("NOT_FOUND", "service credential not found");
      c.revokedAt = now();
      // The host deletes a revoked key's grants (DeleteFoundryAccessPrincipal).
      state.grants = state.grants.filter(
        (g) =>
          !(
            g.principal.kind === "KIND_SERVICE_CREDENTIAL" &&
            g.principal.id === c.id
          ),
      );
      return ok({});
    },
  },
];
