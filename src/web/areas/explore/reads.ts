import type { EntityRef } from "../../../shared/foundry-read";
import type { FoundryOntology } from "../../../shared/wfe";
import {
  queriesPaused,
  type DatabaseQueryFailure,
  type DatabaseQueryTarget,
  type DynamicQueryRequest,
  type QueryExecutor,
} from "./database-query";
import { scopeKey, type Scope } from "./load-scope";
import {
  documentFactsRequest,
  documentRequest,
  entityRequest,
  factEvidenceRequest,
  parseDocument,
  parseDocumentFacts,
  parseEntity,
  parseFactEvidence,
  type DocumentGraph,
  type EntityGraph,
  type EntityIdentity,
  type EntityRow,
  type Fact,
} from "./model";
import { NO_NAMING, namingKeys, readable, type Naming } from "./naming";
import { entityNamesRequest, parseEntityNames, refKey } from "./rule-breaks";

// The reads of ./model.ts as TanStack Query options, run against an
// ontology's database (its tenant) with the user's session, through the
// database's shared limiter (./tenant-reads.tsx), within one load (or Other
// data). Keys sit under the ontology's and carry the load, so a job that
// ends refreshes them. A failed read throws a ReadFailed carrying the
// transport's description. Entities come back named by the load's version
// (./naming.ts), whose id is in the key.

export class ReadFailed extends Error {
  constructor(readonly failure: DatabaseQueryFailure) {
    super(failure.message);
  }
}

/** The database an ontology reads: its tenant, starting from its creation. */
export const ontologyDatabase = (
  ontology: FoundryOntology,
): DatabaseQueryTarget => ({
  kind: "tenant",
  tenantId: ontology.tenantId || "",
  createdAt: ontology.createdAt,
});

/**
 * A query error's starting failure: a new database not answering yet. A
 * page passes `error ?? failureReason`: while the query retries (readOptions)
 * it is pending, and the failure is its failureReason.
 */
export function startingOf(error: unknown) {
  return error instanceof ReadFailed && error.failure.kind === "starting"
    ? error.failure
    : null;
}

/**
 * A query error's paused failure: the workspace's Helix billing pauses the
 * database's queries (queriesPaused), so its page says why instead of
 * offering to read again.
 */
export function pausedOf(error: unknown) {
  return error instanceof ReadFailed && queriesPaused(error.failure)
    ? error.failure
    : null;
}

/**
 * What every read of an ontology's database shares: a failed read isn't
 * retried behind a skeleton (its page says why, with a Retry), except that
 * one of a database still starting is retried after the wait the transport
 * gives (startingRetryMs), until it answers or the starting window passes.
 * The query retries rather than refetches, so between attempts it stays
 * pending with the failure as its failureReason: its page keeps saying the
 * database is starting, never flashing back to its skeleton.
 */
export const readOptions = {
  retry: (_: number, error: Error) => !!startingOf(error),
  retryDelay: (_: number, error: Error) => startingOf(error)?.retryInMs ?? 0,
};

/**
 * Runs one read through `read` (the database's, from useTenantReads(),
 * built on ontologyDatabase so a starting database fails as "starting"),
 * throwing ReadFailed when it fails.
 */
export async function runRead(
  read: QueryExecutor,
  request: DynamicQueryRequest,
  signal?: AbortSignal,
) {
  const result = await read(request, { signal });
  if (!result.ok) throw new ReadFailed(result);
  return result.body;
}

// An entity of a type the version knows takes its readable name; any other
// keeps what the read found (its stored name, key or normalized name).
function named<T extends EntityRow>(naming: Naming, e: T): T {
  const r = readable(naming, e.type, e.props, e.dbId);
  return r.known ? { ...e, name: r.name } : e;
}

export const readQueries = {
  // Everything a load stored from one document.
  document: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    documentId: string,
    naming: Naming = NO_NAMING,
  ) => ({
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      scopeKey(scope),
      "document",
      documentId,
      naming.version,
    ],
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const doc = parseDocument(
        await runRead(
          read,
          documentRequest(scope, documentId, namingKeys(naming)),
          signal,
        ),
      );
      return {
        ...doc,
        entities: doc.entities.map((e) => named(naming, e)),
        mentions: doc.mentions.map((m) => ({
          ...m,
          entity: named(naming, m.entity),
        })),
        reviews: doc.reviews.map((r) => ({
          ...r,
          candidates: r.candidates.map((c) =>
            c.entity ? { ...c, entity: named(naming, c.entity) } : c,
          ),
        })),
      };
    },
    ...readOptions,
  }),
  // The facts behind its quotes.
  documentFacts: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    documentId: string,
    doc: DocumentGraph | undefined,
    naming: Naming = NO_NAMING,
  ) => ({
    // The facts' ends are the document's entities, named with it.
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      scopeKey(scope),
      "document",
      documentId,
      naming.version,
      "facts",
    ],
    enabled: !!doc?.evidence.length,
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const req = documentFactsRequest(scope, doc!.evidence);
      return req
        ? parseDocumentFacts(await runRead(read, req, signal), doc!)
        : [];
    },
    ...readOptions,
  }),
  // One entity's page.
  entity: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    identity: EntityIdentity,
    naming: Naming = NO_NAMING,
  ) => ({
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      scopeKey(scope),
      "entity",
      identity,
      naming.version,
    ],
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const page: EntityGraph = parseEntity(
        await runRead(
          read,
          entityRequest(scope, identity, namingKeys(naming)),
          signal,
        ),
      );
      return {
        ...page,
        entity: page.entity && named(naming, page.entity),
        facts: page.facts.map((f) =>
          f.other ? { ...f, other: named(naming, f.other) } : f,
        ),
        reviews: page.reviews.map((r) => ({
          ...r,
          candidates: r.candidates.map((c) =>
            c.entity ? { ...c, entity: named(naming, c.entity) } : c,
          ),
        })),
      };
    },
    ...readOptions,
  }),
  // What states its facts, by fact key.
  evidence: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    identity: EntityIdentity,
    facts: Fact[] | undefined,
  ) => ({
    queryKey: [
      ws,
      "ontology",
      o,
      "read",
      scopeKey(scope),
      "entity",
      identity,
      "evidence",
    ],
    enabled: !!facts?.some((f) => f.factKey),
    queryFn: async ({ signal }: { signal?: AbortSignal }) => {
      const req = factEvidenceRequest(scope, facts!);
      return req
        ? parseFactEvidence(await runRead(read, req, signal))
        : new Map();
    },
    ...readOptions,
  }),
  // The names of entities a page knows only by type and id (a rules' read,
  // an answer's citations), by refKey, as their pages name them: one lookup
  // in the load (./rule-breaks.ts). Disabled without a load, or when none
  // is of a type the version knows.
  names: (
    ws: string,
    o: string,
    read: QueryExecutor,
    loadId: string,
    refs: readonly EntityRef[],
    naming: Naming,
  ) => {
    const unique = [...new Map(refs.map((r) => [refKey(r), r])).values()];
    const request = entityNamesRequest(loadId, unique, naming);
    return {
      queryKey: [
        ws,
        "ontology",
        o,
        "read",
        loadId,
        "names",
        unique.map(refKey).sort(),
        naming.version,
      ],
      enabled: !!loadId && !!request,
      queryFn: async ({ signal }: { signal?: AbortSignal }) =>
        parseEntityNames(await runRead(read, request!, signal), naming),
      staleTime: 60_000,
      ...readOptions,
    };
  },
};
