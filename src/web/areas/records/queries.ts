import type { QueryExecutor } from "../explore/database-query";
import { scopeKey, type Scope } from "../explore/load-scope";
import type { Naming, TypeNaming } from "../explore/naming";
import { readOptions, runRead } from "../explore/reads";
import {
  activeFilter,
  pageRead,
  parsePage,
  parseTypeCounts,
  recordTypes,
  typeCountsRequests,
  type RecordsPage,
  type RecordsView,
} from "./model";

// Records' reads (./model.ts) as TanStack Query options, run against the
// ontology's database with the user's session through its shared limiter
// (../explore/tenant-reads.tsx), within one load. Keys sit under the
// ontology's reads, with the load and the naming version, so a job that
// ends, another load or another version reads again.

export const recordQueries = {
  // Each type's count, from bitmaps.
  counts: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    naming: Naming,
  ) => {
    const types = recordTypes(naming).map((t) => t.name);
    return {
      queryKey: [
        ws,
        "ontology",
        o,
        "read",
        scopeKey(scope),
        "records",
        naming.version,
        types,
      ],
      queryFn: async ({ signal }: { signal?: AbortSignal }) => {
        const batches = typeCountsRequests(scope, types);
        // One request after another: they share the database's limiter.
        const bodies: unknown[] = [];
        for (const b of batches)
          bodies.push(await runRead(read, b.request, signal));
        return parseTypeCounts(bodies, scope, batches);
      },
      ...readOptions,
    };
  },
  // One page of a type's records, as the view asks: none for a search
  // nothing of the type can match, and off for a filter that isn't one.
  page: (
    ws: string,
    o: string,
    read: QueryExecutor,
    scope: Scope,
    naming: Naming,
    t: TypeNaming,
    view: RecordsView,
  ) => {
    const plan = pageRead(scope, t, view);
    return {
      queryKey: [
        ws,
        "ontology",
        o,
        "read",
        scopeKey(scope),
        "records",
        naming.version,
        t.name,
        view.search.trim(),
        activeFilter(view),
        view.sort,
        view.page,
      ],
      enabled: plan.kind !== "refused",
      queryFn: async ({
        signal,
      }: {
        signal?: AbortSignal;
      }): Promise<RecordsPage> => {
        if (plan.kind === "refused") throw new Error(plan.message);
        if (plan.kind === "none")
          return {
            rows: [],
            more: false,
            matched: { count: 0, capped: false },
          };
        return parsePage(await runRead(read, plan.request, signal), naming, t);
      },
      ...readOptions,
    };
  },
};
