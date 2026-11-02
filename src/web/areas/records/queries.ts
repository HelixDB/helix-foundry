import type { Scope } from "../explore/load-scope";
import type { Naming, TypeNaming } from "../explore/naming";
import { readOptions, runRead, type GraphReader } from "../explore/reads";
import {
  countsPlan,
  runPlan,
  typedOffset,
  typedRecordsRead,
  type TypedRecords,
} from "../explore/typed-reads";
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
        scope.id,
        "records",
        naming.version,
        types,
        reader.kind,
      ],
      queryFn: async ({ signal }: { signal?: AbortSignal }) => {
        if (reader.kind === "grants")
          return runPlan(ws, o, scope.id, countsPlan(types), {
            signal,
            createdAt: reader.createdAt,
          });
        const read = reader.read;
        const batches = typeCountsRequests(scope, types);
        // One request after another: they share the database's limiter.
        const bodies: unknown[] = [];
        for (const b of batches)
          bodies.push(await runRead(read, b.request, signal));
        return parseTypeCounts(bodies, batches);
      },
      ...readOptions,
    };
  },
  // One page of a type's records, as the view asks: none for a search
  // nothing of the type can match, and off for a filter that isn't one.
  // Through grants, the typed read that page's rows come from, which the page
  // cuts into pages (typedRecordsPage): the key has its offset, not the page;
  // a narrowed read (`narrowed`, typedNarrows) is the one read there is. A sort
  // it refuses is read again without, saying so; a later window refused for
  // being narrowed reads the first rows.
  page: (
    ws: string,
    o: string,
    reader: GraphReader,
    scope: Scope,
    naming: Naming,
    t: TypeNaming,
    view: RecordsView,
    narrowed = false,
  ) => {
    if (reader.kind === "grants") {
      const typed = typedRecordsRead(t, view, naming, narrowed);
      const loadId = scope.id,
        createdAt = reader.createdAt;
      return {
        queryKey: [
          ws,
          "ontology",
          o,
          "read",
          scope.id,
          "records",
          naming.version,
          t.name,
          view.search.trim(),
          activeFilter(view),
          view.sort,
          "grants",
          typedOffset(view.page, narrowed),
          narrowed,
        ],
        enabled: typed.kind !== "refused",
        queryFn: async ({
          signal,
        }: {
          signal?: AbortSignal;
        }): Promise<TypedRecords> => {
          const none: TypedRecords = {
            rows: [],
            more: false,
            offset: 0,
            total: 0,
            matched: { count: 0, capped: false },
          };
          if (typed.kind === "refused") throw new Error(typed.message);
          if (typed.kind === "none") return none;
          const options = { signal, createdAt };
          const read = await runPlan(ws, o, loadId, typed.plan, options);
          if (read.refusedBy !== "sort" && read.refusedBy !== "paging")
            return read;
          const paging = read.refusedBy === "paging";
          const again = typedRecordsRead(
            t,
            paging ? view : { ...view, sort: null },
            naming,
            paging || narrowed,
          );
          return again.kind === "read"
            ? {
                ...(await runPlan(ws, o, loadId, again.plan, options)),
                ...(paging ? {} : { notice: read.refused }),
              }
            : none;
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
        scope.id,
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
