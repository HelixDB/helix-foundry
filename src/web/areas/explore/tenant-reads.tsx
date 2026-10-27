import { createContext, useContext, useMemo, type ReactNode } from "react";
import {
  createQueryExecutor,
  type DatabaseQueryResult,
  type DatabaseQueryTarget,
  type QueryExecutor,
} from "./database-query";
import {
  createRequestLimiter,
  delay,
  RATE_LIMIT_RETRY_DELAYS_MS,
  type RequestLimiter,
} from "./explorer/lib/request-limiter";

// The gateway allows a database one read a second (a burst of two), whoever
// sends it. So every page of an ontology reads its database through one
// limiter: both Explorer canvases (compare draws two), Records, an entity's
// and a document's view, the compare diff and the Loads page's counts. The
// ontology's scope (../ontology/scope.tsx) provides it per database, built
// on the database's target with its creation time, so a read of a database
// still starting fails as "starting" (./database-query.ts).

export type TenantReads = {
  /** The database's limiter; the Explorer paces its own requests through it. */
  limiter: RequestLimiter;
  /**
   * A read for a page (Records, an entity, a document, counts): it goes at
   * once and takes a token, so the Explorer's own requests that follow wait
   * their turn; a read the database rate-limits is tried again after 1, 2
   * and 4 s.
   */
  read: QueryExecutor;
  /** The same read waiting its turn, for background work (counts, diffs). */
  readLater: QueryExecutor;
};

const rateLimited = (r: DatabaseQueryResult) =>
  !r.ok &&
  r.kind === "database" &&
  (r.code === "rate_limited" || (r.status === 429 && r.code === null));

const cancelled = (): DatabaseQueryResult => ({
  ok: false,
  kind: "aborted",
  status: 0,
  code: null,
  message: "The query was cancelled.",
  elapsedMs: 0,
});

/**
 * Reads through `limiter` with `execute`, retrying a rate-limited read.
 * `automatic` reads wait for a slot and a token; others go at once.
 */
export function pacedReads(
  limiter: RequestLimiter,
  execute: QueryExecutor,
  automatic: boolean,
  retryDelaysMs: readonly number[] = RATE_LIMIT_RETRY_DELAYS_MS,
): QueryExecutor {
  return async (request, options = {}) => {
    const { signal } = options;
    for (let retry = 0; ; retry += 1) {
      const release = await limiter.acquire({ automatic, signal });
      if (!release) return cancelled();
      let result: DatabaseQueryResult;
      try {
        result = await execute(request, options);
      } finally {
        release();
      }
      if (
        !rateLimited(result) ||
        retry >= retryDelaysMs.length ||
        signal?.aborted
      )
        return result;
      if (!(await delay(retryDelaysMs[retry]!, signal))) return cancelled();
    }
  };
}

/** One database's reads, built once per database. */
export function tenantReads(
  database: DatabaseQueryTarget,
  execute: QueryExecutor = createQueryExecutor(database),
): TenantReads {
  const limiter = createRequestLimiter();
  return {
    limiter,
    read: pacedReads(limiter, execute, false),
    readLater: pacedReads(limiter, execute, true),
  };
}

const TenantReadsContext = createContext<TenantReads | null>(null);

/** Gives the pages under it one limiter for `database`. */
export function TenantReadsProvider({
  database: { tenantId, createdAt },
  children,
}: {
  database: DatabaseQueryTarget;
  children: ReactNode;
}) {
  const value = useMemo(
    () => tenantReads({ kind: "tenant", tenantId, createdAt }),
    [tenantId, createdAt],
  );
  return (
    <TenantReadsContext.Provider value={value}>
      {children}
    </TenantReadsContext.Provider>
  );
}

/** The ontology's database's reads. Only valid inside <TenantReadsProvider>. */
export function useTenantReads(): TenantReads {
  const reads = useContext(TenantReadsContext);
  if (!reads) throw new Error("useTenantReads outside an ontology's pages");
  return reads;
}
