import { scopeKey, type Scope } from "../explore/load-scope";
import type { Naming, TypeNaming } from "../explore/naming";
import { readOptions, runRead, type GraphReader } from "../explore/reads";
import { countsPlan, runPlan, typedRecordsRead } from "../explore/typed-reads";
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
// (../explore/tenant-reads.tsx), within one load, or through source grants
// as typed reads of the load they cover (../explore/typed-reads.ts). Keys
// sit under the ontology's reads, with the load and the naming version, so
// a job that ends, another load or another version reads again.

export const recordQueries = {
  // Each type's count, from bitmaps.
  counts: (
    ws: string,
    o: string,
    reader: GraphReader,
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
        reader.kind,
      ],
      queryFn: async ({ signal }: { signal?: AbortSignal }) => {
        if (reader.kind === "grants")
          return runPlan(
            ws,
            o,
            scope.kind === "load" ? scope.id : "",
            countsPlan(types),
            signal,
          );
        const read = reader.read;
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
  // Through grants, every row a typed read reaches (its first 200), which
  // the page cuts into pages (typedRecordsPage): the key has no page.
  page: (
    ws: string,
    o: string,
    reader: GraphReader,
    scope: Scope,
    naming: Naming,
    t: TypeNaming,
    view: RecordsView,
  ) => {
    if (reader.kind === "grants") {
      const typed = typedRecordsRead(t, view, naming);
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
          "grants",
        ],
        enabled: typed.kind !== "refused",
        queryFn: async ({
          signal,
        }: {
          signal?: AbortSignal;
        }): Promise<RecordsPage> => {
          if (typed.kind === "refused") throw new Error(typed.message);
          if (typed.kind === "none")
            return {
              rows: [],
              more: false,
              matched: { count: 0, capped: false },
            };
          return {
            ...(await runPlan(
              ws,
              o,
              scope.kind === "load" ? scope.id : "",
              typed.plan,
              signal,
            )),
            more: false,
          };
        },
        ...readOptions,
      };
    }
    const read = reader.read;
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
