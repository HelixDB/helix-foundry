import type {
  Connector,
  FoundryAccessGrant,
  FoundryAccessLoss,
  FoundryAccessTarget,
  FoundryDocumentSet,
  FoundryLoadAccessInputs,
  FoundryPrincipal,
  FoundryPrincipalKind,
  ListFoundryAccessResponse,
  ResourceScope,
  WorkspaceMember,
} from "../../../shared/wfe";

// Source grants as Settings › Access and restricted mode read them, pure:
// what a grant gives (Grant, from and to the WFE's targets), the workspace's
// sources as the grants editor lists them (sourceTree), what a load holds
// that grants don't cover (missing, the backend's Grants.Missing), who reads
// what, whether a member can be restricted, and where a grantee may go.

/**
 * What one grant gives, as the editor holds it: a connector (its tables now
 * and later, and the sets it owns), one table of a connector, a document
 * set, or People enrichment. Each kind names exactly what it needs.
 */
export type Grant =
  | { kind: "connector"; connectorId: string }
  | { kind: "table"; connectorId: string; table: string }
  | { kind: "set"; setId: string }
  | { kind: "enrichment" };

/**
 * A grant as a key, unique per grant.
 *
 *     grantKey({ kind: "table", connectorId: "c", table: "t" }) // "table:c:t"
 */
export const grantKey = (g: Grant) =>
  g.kind === "connector"
    ? `connector:${g.connectorId}`
    : g.kind === "table"
      ? `table:${g.connectorId}:${g.table}`
      : g.kind === "set"
        ? `set:${g.setId}`
        : "enrichment";

/** A WFE target as a Grant; undefined for a kind this build doesn't know, or one missing what its kind names. */
export function grantOf(t: FoundryAccessTarget): Grant | undefined {
  const c = t.connectorId || "",
    table = t.table || "",
    set = t.documentSetId || "";
  switch (t.kind) {
    case "KIND_CONNECTOR":
      return c ? { kind: "connector", connectorId: c } : undefined;
    case "KIND_TABLE":
      return c && table ? { kind: "table", connectorId: c, table } : undefined;
    case "KIND_DOCUMENT_SET":
      return set ? { kind: "set", setId: set } : undefined;
    case "KIND_ENRICHMENT":
      return { kind: "enrichment" };
    default:
      return undefined;
  }
}

/** A Grant as the WFE's target. */
export function targetOf(g: Grant): FoundryAccessTarget {
  switch (g.kind) {
    case "connector":
      return { kind: "KIND_CONNECTOR", connectorId: g.connectorId };
    case "table":
      return { kind: "KIND_TABLE", connectorId: g.connectorId, table: g.table };
    case "set":
      return { kind: "KIND_DOCUMENT_SET", documentSetId: g.setId };
    case "enrichment":
      return { kind: "KIND_ENRICHMENT" };
  }
}

/** A principal as a key: "KIND_USER:usr_1". */
export const principalKey = (p: FoundryPrincipal) =>
  `${p.kind || "KIND_UNSPECIFIED"}:${p.id || ""}`;

/** The principals grants are for, as the editor names them. */
export type Principal = {
  kind: Exclude<FoundryPrincipalKind, "KIND_UNSPECIFIED">;
  id: string;
};

/** One principal's own grants, in the order they were made, once each. */
export function grantsOf(
  grants: readonly FoundryAccessGrant[],
  principal: Principal,
): Grant[] {
  const key = principalKey(principal);
  const out = new Map<string, Grant>();
  for (const g of grants) {
    if (principalKey(g.principal || {}) !== key) continue;
    const grant = g.target && grantOf(g.target);
    if (grant) out.set(grantKey(grant), grant);
  }
  return [...out.values()];
}

/**
 * The grants a principal reads with: its own, and a member's teams' too
 * (a team's members hold its grants).
 */
export function heldGrants(
  access: ListFoundryAccessResponse,
  principal: Principal,
): Grant[] {
  const teams =
    principal.kind === "KIND_USER"
      ? (access.teams || []).filter((t) =>
          t.memberUserIds?.includes(principal.id),
        )
      : [];
  const all = [
    principal,
    ...teams.map((t): Principal => ({ kind: "KIND_TEAM", id: t.id || "" })),
  ].flatMap((p) => grantsOf(access.grants || [], p));
  return [...new Map(all.map((g) => [grantKey(g), g])).values()];
}

/**
 * The targets to save for `grants`: once each, without a table or a set
 * its connector's grant already covers (`owners` gives the connector that
 * owns a set, if one does).
 */
export function targetsOf(
  grants: readonly Grant[],
  owners: ReadonlyMap<string, string>,
): FoundryAccessTarget[] {
  const connectors = new Set(
    grants.flatMap((g) => (g.kind === "connector" ? [g.connectorId] : [])),
  );
  const kept = grants.filter((g) =>
    g.kind === "table"
      ? !connectors.has(g.connectorId)
      : g.kind === "set"
        ? !connectors.has(owners.get(g.setId) || "")
        : true,
  );
  return [...new Map(kept.map((g) => [grantKey(g), g])).values()].map(targetOf);
}

// ---------------------------------------------------------------- the sources

/** A connection of the grants editor: its tables and the sets it owns. */
export type SourceConnection = {
  id: string;
  name: string;
  kind: string;
  tables: string[];
  sets: { id: string; name: string }[];
};
/**
 * The workspace's sources as the grants editor lists them: each connection
 * with its synced tables and the sets its syncs write, then the sets people
 * made. A set whose connection is gone is listed with the others.
 */
export type SourceTree = {
  connections: SourceConnection[];
  sets: { id: string; name: string }[];
};

export function sourceTree(
  connectors: readonly Connector[],
  sets: readonly FoundryDocumentSet[],
): SourceTree {
  const ids = new Set(connectors.map((c) => c.id));
  const owned = (id: string) =>
    sets
      .filter((s) => s.connector?.connectorId === id)
      .map((s) => ({ id: s.id, name: s.name || s.id }));
  return {
    connections: connectors.map((c) => ({
      id: c.id,
      name: c.name || c.kind,
      kind: c.kind,
      tables: (c.tables || []).map((t) => t.name),
      sets: owned(c.id),
    })),
    sets: sets
      .filter((s) => !ids.has(s.connector?.connectorId || ""))
      .map((s) => ({ id: s.id, name: s.name || s.id })),
  };
}

/** Each connector-owned set's connector, by set id. */
export const setOwners = (sets: readonly FoundryDocumentSet[]) =>
  new Map(
    sets.flatMap((s) =>
      s.connector?.connectorId ? [[s.id, s.connector.connectorId]] : [],
    ),
  );

/**
 * How the editor shows one source given the grants chosen: chosen, or
 * covered by its connection's grant (shown chosen, and not to be changed
 * on its own), or neither.
 */
export type SourceState = "chosen" | "covered" | "off";
export function sourceState(
  chosen: readonly Grant[],
  source: Grant,
  owner = "",
): SourceState {
  const keys = new Set(chosen.map(grantKey));
  const connector =
    source.kind === "table"
      ? source.connectorId
      : source.kind === "set"
        ? owner
        : "";
  if (
    connector &&
    keys.has(grantKey({ kind: "connector", connectorId: connector }))
  )
    return "covered";
  return keys.has(grantKey(source)) ? "chosen" : "off";
}

/** `chosen` with `source` turned on or off. */
export function toggle(chosen: readonly Grant[], source: Grant): Grant[] {
  const key = grantKey(source);
  return chosen.some((g) => grantKey(g) === key)
    ? chosen.filter((g) => grantKey(g) !== key)
    : [...chosen, source];
}

// ---------------------------------------------------------------- coverage

/** What of a load the grants don't cover (the backend's Grants.Missing). */
export type Missing = {
  tables: { connectorId: string; table: string }[];
  // A set's id "" is a document in no set, which nothing covers.
  sets: { setId: string; connectorId: string }[];
  enrichment: boolean;
  // The load holds what no grant covers: loading again makes it shareable.
  uncoverable: boolean;
};

/**
 * What of `inputs` `grants` leave out: a table neither its connector nor
 * itself is granted, a set neither itself nor its owning connector is,
 * People enrichment, and whether the load holds what nothing covers.
 *
 *     missing([{ kind: "connector", connectorId: "c" }], {
 *       tables: [{ connectorId: "c", table: "t" }],
 *     }) // { tables: [], sets: [], enrichment: false, uncoverable: false }
 */
export function missing(
  grants: readonly Grant[],
  inputs: FoundryLoadAccessInputs,
): Missing {
  const keys = new Set(grants.map(grantKey));
  const connector = (id: string) =>
    !!id && keys.has(grantKey({ kind: "connector", connectorId: id }));
  return {
    tables: (inputs.tables || [])
      .map((t) => ({ connectorId: t.connectorId || "", table: t.table || "" }))
      .filter(
        (t) =>
          !connector(t.connectorId) &&
          !keys.has(grantKey({ kind: "table", ...t })),
      ),
    sets: (inputs.documentSets || [])
      .map((s) => ({
        setId: s.documentSetId || "",
        connectorId: s.connectorId || "",
      }))
      .filter(
        (s) =>
          !s.setId ||
          !(
            keys.has(grantKey({ kind: "set", setId: s.setId })) ||
            connector(s.connectorId)
          ),
      ),
    enrichment: !!inputs.enrichment && !keys.has("enrichment"),
    uncoverable: !!inputs.uncoverable,
  };
}

/** Whether nothing is missing: the grants cover the load. */
export const covered = (m: Missing) =>
  !m.tables.length && !m.sets.length && !m.enrichment && !m.uncoverable;

/**
 * Whether `grants` hold anything of `inputs` (the backend's Grants.Reaches), so
 * whoever holds them reads that part of the load. Nothing of a load that holds
 * what nothing covers: loading it again makes it shareable.
 *
 *     holdsAny([{ kind: "set", setId: "s" }], {
 *       tables: [{ connectorId: "c", table: "t" }],
 *       documentSets: [{ documentSetId: "s" }],
 *     }) // true
 */
export function holdsAny(
  grants: readonly Grant[],
  inputs: FoundryLoadAccessInputs,
): boolean {
  if (inputs.uncoverable) return false;
  const m = missing(grants, inputs);
  return (
    m.tables.length < (inputs.tables || []).length ||
    m.sets.length < (inputs.documentSets || []).length ||
    (!!inputs.enrichment && !m.enrichment)
  );
}

/** Names of what a page lists: connections, their tables' sets, ontologies. */
export type Names = {
  connection: (id: string) => string;
  set: (id: string) => string;
};

/**
 * What is missing, in words, one item a line: "Stripe › invoices", a set's
 * label, "People enrichment", "A document in no set"; a load nothing can
 * cover is "Load again to share" alone.
 */
export function missingText(m: Missing, names: Names): string[] {
  if (m.uncoverable) return ["Load again to share"];
  return [
    ...m.tables.map((t) => `${names.connection(t.connectorId)} › ${t.table}`),
    ...m.sets.map((s) =>
      s.setId ? names.set(s.setId) : "A document in no set",
    ),
    ...(m.enrichment ? ["People enrichment"] : []),
  ];
}

/**
 * What a principal reads of an ontology through its grants: all of it, the
 * part they hold, or nothing; and what they leave out.
 */
export type Reading = {
  ontologyId: string;
  reach: "all" | "part" | "none";
  missing: Missing;
};
/**
 * Each ontology with a current load, as `grants` see it: those they cover
 * first, then those they hold part of, then the rest, each group in the list's
 * order. The WFE's word for `principal` settles what the inputs can't say (a
 * decision whose documents weren't recorded leaves the rest readable).
 */
export function readings(
  access: ListFoundryAccessResponse,
  grants: readonly Grant[],
  principal?: Principal,
): Reading[] {
  const all = (access.ontologies || []).map((o): Reading => {
    const lacks = missing(grants, o.inputs || {});
    const told =
      !!principal &&
      !!o.partialReaders?.some(
        (p) => principalKey(p) === principalKey(principal),
      );
    return {
      ontologyId: o.ontologyId || "",
      reach: covered(lacks)
        ? "all"
        : told || holdsAny(grants, o.inputs || {})
          ? "part"
          : "none",
      missing: lacks,
    };
  });
  return (["all", "part", "none"] as const).flatMap((reach) =>
    all.filter((r) => r.reach === reach),
  );
}

/**
 * The line of what a principal reads: all of some ontologies, part of others.
 *
 *     readsText([{ ontologyId: "a", reach: "all", missing }, { ontologyId: "b", reach: "part", missing }], (o) => o) // "Reads a, part of b"
 */
export function readsText(
  reading: readonly Reading[],
  name: (ontologyId: string) => string,
): string {
  const all = reading.filter((r) => r.reach === "all"),
    part = reading.filter((r) => r.reach === "part");
  return all.length || part.length
    ? `Reads ${[
        ...all.map((r) => name(r.ontologyId)),
        ...part.map((r) => `part of ${name(r.ontologyId)}`),
      ].join(", ")}`
    : "Reads nothing yet";
}

// ---------------------------------------------------------------- losses

/**
 * Who stops reading which ontology after a change, one line per principal:
 * "Eve Adler: Customer contracts, Renewals (keeps the part their sources
 * hold)", in the order the WFE lists them.
 */
export function lossLines(
  losses: readonly FoundryAccessLoss[],
  principalName: (p: FoundryPrincipal) => string,
  ontologyName: (id: string) => string,
): string[] {
  const by = new Map<string, { p: FoundryPrincipal; ontologies: string[] }>();
  for (const l of losses) {
    const p = l.principal || {},
      k = principalKey(p),
      entry = by.get(k) || { p, ontologies: [] };
    const name = `${ontologyName(l.ontologyId || "")}${l.keepsPart ? " (keeps the part their sources hold)" : ""}`;
    if (!entry.ontologies.includes(name)) entry.ontologies.push(name);
    by.set(k, entry);
  }
  return [...by.values()].map(
    (e) => `${principalName(e.p)}: ${e.ontologies.join(", ")}`,
  );
}

// ---------------------------------------------------------------- members

const reaches = (s?: ResourceScope) => !!s?.mode && s.mode !== "none";

/**
 * Whether a member writes a project or queries its databases, so reads them
 * outside Foundry, whatever grants say.
 */
export function holdsRawAccess(m: WorkspaceMember) {
  const a = m.projectAccess;
  return reaches(a?.write) || reaches(a?.queryRead) || reaches(a?.queryWrite);
}

/**
 * Whether a member can be restricted to what grants give, else why not:
 * owners and admins see everything, and a member who holds raw access reads
 * the databases outside Foundry.
 */
export function restrictable(
  m: WorkspaceMember,
): { ok: true } | { ok: false; why: string } {
  if (m.role === "WORKSPACE_ROLE_OWNER" || m.role === "WORKSPACE_ROLE_ADMIN")
    return { ok: false, why: "Owners and admins see everything." };
  return holdsRawAccess(m)
    ? {
        ok: false,
        why: "Writes a project or queries its databases outside Foundry. Take that away in the Helix dashboard first.",
      }
    : { ok: true };
}

/**
 * Who saving sources for this principal would restrict, as people: a member
 * given sources, or a team's members, while not restricted yet. An owner or
 * admin on a team stays as they are.
 */
export function newlyRestricted(
  access: ListFoundryAccessResponse,
  people: readonly WorkspaceMember[],
  principal: Principal,
): WorkspaceMember[] {
  const team =
      principal.kind === "KIND_TEAM"
        ? (access.teams || []).find((t) => t.id === principal.id)
        : undefined,
    reached = (id: string) =>
      principal.kind === "KIND_USER"
        ? id === principal.id
        : !!team?.memberUserIds?.includes(id);
  return people.filter(
    (m) =>
      m.role === "WORKSPACE_ROLE_MEMBER" &&
      reached(m.user?.id || "") &&
      !access.restrictedUserIds?.includes(m.user?.id || ""),
  );
}

// ---------------------------------------------------------------- restricted mode

/**
 * Where a grantee (a member restricted to what grants give) goes instead
 * of `pathname`, or undefined where it may stay: Ask, its ontologies'
 * overviews and the read-only pages of their shared loads (Records, an
 * entity's page and a document's view). Connectors, Settings and making an
 * ontology open Ask; the Explorer's graph opens Records, and an ontology's
 * other pages its overview.
 *
 *     granteeRedirect("/data/connections/c") // "/ask"
 *     granteeRedirect("/explore/o") // "/explore/o/records"
 *     granteeRedirect("/ontology/o/loads/l") // "/ontology/o"
 */
export function granteeRedirect(pathname: string): string | undefined {
  const [first = "", second, third, fourth] = pathname
    .split("/")
    .filter(Boolean);
  switch (first) {
    case "":
    case "ask":
      return undefined;
    case "ontology":
      if (second === "new" || second === "sample") return "/ask";
      return third === undefined || (third === "documents" && fourth)
        ? undefined
        : `/ontology/${second}`;
    case "explore":
      return second === undefined ||
        third === "records" ||
        (third === "entities" && fourth)
        ? undefined
        : `/explore/${second}/records`;
    case "data":
    case "settings":
      return "/ask";
    default:
      return undefined;
  }
}

/**
 * How the user reads an ontology's loads: "database", through its database
 * with their session (the host's callers, and anyone whose query access
 * isn't known); "grants", through the WFE's typed reads of the current
 * load their source grants hold (a grantee, or a member who can't query
 * the ontology's project, on an ontology `granted` names); or "none".
 */
export type ReadAccess = "database" | "grants" | "none";

/**
 * The user's ReadAccess to an ontology: `granted` are the ontologies whose
 * current loads their grants hold any of (.../foundry/access/me), `queryRead`
 * the projects whose databases they query (whoami's projectAccess),
 * undefined when that isn't known.
 *
 *     readAccess({ grantee: true, granted: ["o"] }, { id: "o" }) // "grants"
 *     readAccess({ grantee: false, granted: [], queryRead: { mode: "all" } }, { id: "o" }) // "database"
 *     readAccess({ grantee: false, granted: [], queryRead: {} }, { id: "o" }) // "none"
 */
export function readAccess(
  me: {
    grantee: boolean;
    granted: readonly string[];
    queryRead?: ResourceScope;
  },
  o: { id: string; projectId?: string },
): ReadAccess {
  const shared = me.granted.includes(o.id) ? "grants" : "none";
  if (me.grantee) return shared;
  const q = me.queryRead;
  if (!q) return "database";
  return q.mode === "all" ||
    (q.mode === "selected" &&
      !!o.projectId &&
      !!q.projectIds?.includes(o.projectId))
    ? "database"
    : shared;
}

/**
 * Whether the user reads only part of an ontology: they read it through source
 * grants that hold only some of its current load (`partial`,
 * .../foundry/access/me). What a read asks of what is partly held may be
 * refused (`not_visible`), and rows come without totals.
 *
 *     readsPart({ grantee: true, granted: ["o"], partial: ["o"] }, { id: "o" }) // true
 *     readsPart({ grantee: false, granted: ["o"], partial: ["o"], queryRead: { mode: "all" } }, { id: "o" }) // false
 */
export const readsPart = (
  me: Parameters<typeof readAccess>[0] & { partial: readonly string[] },
  o: { id: string; projectId?: string },
) => readAccess(me, o) === "grants" && me.partial.includes(o.id);
